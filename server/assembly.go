package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync/atomic"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/intercept"
	"github.com/Autumn-27/artex/llmrec"
	"github.com/Autumn-27/artex/traffic"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/skill"
	actool "github.com/Autumn-27/norma/tool"
)

// installQuotaGate 把群组预算闸门装到 provider 链上。
//
// 链的形状是 Recorder(QuotaProvider(raw)) —— 闸门在 **recorder 之内**：
// 闸门拒绝时 recorder 仍会看到这次调用（记成一次**失败**调用，llm_usage 里
// 留痕），而拒绝本身另有 group_quota_refusals 留痕。两条证据各说一件事：
// 「调用发生过但被拒」与「这次调用没有发出去」。
//
// 为什么装在这里而不是「配了预算才装」：预算就是花钱的上限。上限不接进真实
// 调用路径，它就只是一张表。没有预算行的群组按 fail-closed 拒绝
// （db.ErrQuotaNoBudget）—— 那是 db 层既定的口径，不是这里新增的规则。
//
// 闸门按 ctx 里的 task id 解析群组：不属于任何群组的调用（平台对话路径）
// 一律不拦截，所以装在共享 provider 链上不会波及非大厅流量。
//
// gate 由 Server 单例持有（C2）：阈值预警的已发记录必须跨 provider 重建存活，
// 每条链各建一个 gate 会把「已预警」的记忆打散。
func installQuotaGate(prov llm.Provider, gate *llmrec.QuotaGate) llm.Provider {
	if prov == nil || gate == nil {
		return prov
	}
	return llmrec.NewQuotaProvider(prov, gate)
}

// wireToolDenialRecorder 把工具层的拒绝接进审计链。
//
// 工具层（norma 的 permission 判定 Stage 1）早于 PreToolUse hook 求值，hook 不会
// 被调用，于是短路表那条留痕路径拿不到 —— 由这里补一条。用 interceptor 写入，
// payload 的规范化与串行化与其他判定动作一致，链不会因此断掉。
func wireToolDenialRecorder(ic *intercept.Interceptor) {
	agent.ToolDenialRecorder = func(ctx context.Context, toolName, reason string, input []byte) {
		ic.AppendDecisionEvent(ctx, intercept.Decision{
			Action:   "deny",
			RuleName: "[代码常量] 工具层秘密判定",
			Message:  reason,
		}, toolName, input, "denied")
	}
}

// wireAgentAugment connects the PG agent_visibility table into the agent runtime:
// an agent's visible skills are loaded from the filesystem and packed into one
// Skill meta-tool; its visible stdio MCP servers are spawned and expanded to
// mcp__server__tool. skillDir is the root directory of all skill subdirectories.
// hostTools, if set, returns runtime host tools (currently the traffic tools when
// capture is on) to add to EVERY agent's base list — the DB tools table then
// filters them per-agent binding. Empty/nil → no host tools this run (capture off).
//
// G1（方案 v10 §15）装配缓存：昂贵的部分（重读解析全部 SKILL.md、spawn 每个 stdio
// MCP 进程 + tools/list）不再每 run 重跑 —— 按 agentKey 缓存「连接与解析产物」，
// 每 run 只用缓存产物重建轻包装（新 UnlockSet / OnInvoke / Skill 工具 / host 工具，
// 这些是 per-run 状态，不能跨 run 复用）。失效走两条通道：配置写入 handler 调
// agent.InvalidateAssembly() 直接翻代；命中时 fresh() 用几条廉价查询复核装配输入
// —— 给绕过 handler 的直写 DB（测试、种子代码）兜底。MCP 连接跨 run 复用不关闭，
// 由失效/重建替换时与进程退出统一收。
func wireAgentAugment(pg *db.DB, skillDir string, hostTools func() ([]actool.CoreTool, map[string][]string)) {
	agent.ToolAugment = func(ctx context.Context, agentKey string) ([]actool.CoreTool, agent.DeferredInfo, func()) {
		value, release, err := agent.GetOrBuildAssembly(agentKey,
			func(v any) bool {
				asm, ok := v.(*agentAssembly)
				return ok && asm.fresh(pg)
			},
			func() (agent.AssemblyBuild, error) {
				return buildAgentAssembly(pg, skillDir, agentKey, ctx)
			})
		if err != nil || value == nil {
			return nil, agent.DeferredInfo{}, nil
		}
		return value.(*agentAssembly).rebuild(ctx, pg, skillDir, hostTools, release)
	}
}

// agentAssembly 是一个 agent 的【可缓存装配产物】：连接与解析产物（skill 文档、活的
// MCP 客户端连接、工具 schema 名单）。字段在建完后只读、跨 run 共享：
//   - mcpTools：CoreTool 实例只捕获连接与工具元数据，Call 是 per-call ctx + 并发安全
//     （norma mcp conn.call / mcphttp.Client 都有锁、无 per-run 状态）⇒ 多 run 并发复用；
//   - serverTools：OnInvoke/unlockSkill 闭包只读它；
//   - skills：skill.Skill 是值结构，每 run Add 进新 Registry（OnInvoke 是 per-run 的）。
//
// per-run 可变状态（Unlock、globalSet 的 host 追加、name 名单的 host 追加）不在这里，
// rebuild 里每次拷贝重建。
type agentAssembly struct {
	agentID  int64
	agentKey string
	skills   []skill.Skill     // 该 agent 可见的 skills（按目录名过滤后的解析产物）；空 = 无 Skill 工具
	mcpTools []actool.CoreTool // 活连接上的 MCP 工具（服务端遍历序，与全量装配一致）
	// server 名 → 工具名；unlockSkill / OnInvoke 按它解锁与揭示
	serverTools map[string][]string
	allNames    []string        // deferred 名单的 MCP 部分（host 部分每 run 追加）
	globalNames []string        // 全局块名单的 MCP 部分（host 部分每 run 追加）
	globalSet   map[string]bool // globalNames 的集合形态（OnInvoke 的 reveal 判定用）

	// --- 装配输入快照：fresh() 探针比对这些，任一变化 ⇒ 产物过期 ---
	skillNames []string       // AgentSkillNames（该查询带 ORDER BY，直接 DeepEqual）
	mcpVis     []int64        // AgentVisible("mcp")（无序，按集合比对）
	mcpSig     []mcpServerSig // ListMCP 里影响装配的字段投影（ORDER BY id，直接 DeepEqual）
}

// mcpServerSig 是装配真正消费的 MCPServer 字段（连接参数 + enabled）。Tools 缓存列
// 不进装配决策，不参与比对。Args/Env 归一成 string，nil 与空切片等价。
type mcpServerSig struct {
	ID        int64
	Name      string
	Transport string
	Command   string
	Args      string
	Env       string
	URL       string
	Enabled   bool
	Insecure  bool
}

func mcpSignature(all []*db.MCPServer) []mcpServerSig {
	out := make([]mcpServerSig, 0, len(all))
	for _, m := range all {
		out = append(out, mcpServerSig{
			ID: m.ID, Name: m.Name, Transport: m.Transport, Command: m.Command,
			Args: string(m.Args), Env: string(m.Env), URL: m.URL,
			Enabled: m.Enabled, Insecure: m.Insecure,
		})
	}
	return out
}

// fresh 是缓存命中时的装配输入探针（G1）：agent 行、skill 可见性、MCP 可见性、MCP
// 配置四条廉价索引查询（毫秒级）。任何一条读失败或与快照不符 ⇒ 过期，走重建 ——
// 语义与「每 run 全量装配」对齐，包括绕过 handler 的直写 DB。
func (asm *agentAssembly) fresh(pg *db.DB) bool {
	a, err := pg.GetAgentByKey(asm.agentKey)
	if err != nil || a == nil || a.ID != asm.agentID {
		return false
	}
	names, err := pg.AgentSkillNames(a.ID)
	if err != nil || !reflect.DeepEqual(names, asm.skillNames) {
		return false
	}
	vis, err := pg.AgentVisible(a.ID, "mcp")
	if err != nil || !equalInt64Set(vis, asm.mcpVis) {
		return false
	}
	all, err := pg.ListMCP()
	if err != nil || !reflect.DeepEqual(mcpSignature(all), asm.mcpSig) {
		return false
	}
	return true
}

func equalInt64Set(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	set := make(map[int64]bool, len(b))
	for _, v := range b {
		set[v] = true
	}
	for _, v := range a {
		if !set[v] {
			return false
		}
	}
	return true
}

// buildAgentAssembly 是全量装配路径（缓存未命中/过期时跑）：读 agent 行、解析可见
// skills、连接可见 MCP 并 tools/list、拼 deferred/global 名单。任何一处【非确定性】
// 失败（DB 读错、LoadDir 失败、MCP 拨号/tools-list 失败）都把 Cacheable 置 false：
// 本 run 照旧返回部分装配（与缓存前逐字等价），下轮重试 —— 失败状态不进缓存。
// 成功且输入干净的产物才允许缓存（Cacheable=true，连接随之跨 run 存活）。
func buildAgentAssembly(pg *db.DB, skillDir, agentKey string, ctx context.Context) (agent.AssemblyBuild, error) {
	cacheable := true // G1：装配输入全程读干净才缓存；任何降级都退回每 run 重试
	a, err := pg.GetAgentByKey(agentKey)
	if err != nil {
		return agent.AssemblyBuild{}, err
	}
	if a == nil {
		return agent.AssemblyBuild{}, fmt.Errorf("agent %q 不存在", agentKey)
	}
	art := &agentAssembly{agentID: a.ID, agentKey: a.Key}

	// --- skills: 解析可见 skills（原每 run 路径的 LoadDir + 过滤），建产物时存
	// 列表；reg 与 OnInvoke 每 run 重建。 ---
	names, err := pg.AgentSkillNames(a.ID)
	if err != nil {
		cacheable = false // 读失败按空处理（同旧行为），但不缓存这份结果
		names = nil
	}
	art.skillNames = append([]string(nil), names...)
	if len(names) > 0 {
		nameSet := make(map[string]bool, len(names))
		for _, n := range names {
			nameSet[n] = true
		}
		if allReg, err := skill.LoadDir(skillDir); err == nil && allReg != nil {
			for _, s := range allReg.List() {
				// match by directory name (Base of Dir), not by skill display Name
				if s.Dir != "" && nameSet[filepath.Base(s.Dir)] {
					art.skills = append(art.skills, s)
				}
			}
		} else if err != nil {
			cacheable = false // LoadDir 失败：本轮无 Skill 工具（同旧），下轮重试
		}
	}
	// A server named by any visible skill's `mcps:` is skill-gated: its tools
	// are deferred + locked (not in the global block) until that skill loads.
	gated := map[string]bool{}
	for _, s := range art.skills {
		for _, srv := range s.MCPs {
			gated[srv] = true
		}
	}

	// --- mcp: connect enabled servers that are directly visible to this agent
	// OR skill-gated (named in a visible skill's mcps field). Directly-visible
	// and NOT gated → global (unlocked from session start). Skill-gated →
	// deferred until that skill is invoked (regardless of direct visibility).
	// G1：连接建立后跨 run 复用（进 build.Closers，由缓存统一收），不再每 run 关。
	mcpIDs, err := pg.AgentVisible(a.ID, "mcp")
	if err != nil {
		cacheable = false
		mcpIDs = nil
	}
	art.mcpVis = append([]int64(nil), mcpIDs...)
	sort.Slice(art.mcpVis, func(i, j int) bool { return art.mcpVis[i] < art.mcpVis[j] }) // 无序查询 → 探针按集合比对
	all, err := pg.ListMCP()
	if err != nil {
		cacheable = false
		all = nil
	}
	art.mcpSig = mcpSignature(all)

	var closers []io.Closer
	serverTools := map[string][]string{} // server name → its tool names
	var allNames, globalNames []string
	globalSet := map[string]bool{}
	{
		want := idSet(mcpIDs)
		for _, m := range all {
			if !m.Enabled {
				continue
			}
			directVisible := want[m.ID]
			skillGated := gated[m.Name]
			if !directVisible && !skillGated {
				continue // neither directly visible nor referenced by a visible skill
			}
			// F5（方案 v9.2 §3.5）：http 传输但 URL 没配（如 schema 播种的
			// ScopeSentry 占位行）⇒ 连接必然失败，跳过发现并只告警一次——
			// 否则每个 agent 会话装配都刷一条「连接失败」（周期性噪音）。
			// 补上 URL 后此处不再命中，发现自动恢复。
			if m.Transport == "http" && m.URL == "" && warnMCPMissingURLOnce(m.Name) {
				continue
			}
			cl, err := connectMCP(ctx, m)
			if err != nil {
				log.Printf("[mcp] %s 连接失败: %v", m.Name, err)
				cacheable = false // 拨号失败照旧每 run 重试重报错，不缓存
				continue
			}
			closers = append(closers, cl)
			ts, err := cl.Tools(ctx)
			if err != nil {
				log.Printf("[mcp] %s tools/list 失败: %v", m.Name, err)
				cacheable = false
				continue
			}
			for _, t := range ts {
				art.mcpTools = append(art.mcpTools, t)
				allNames = append(allNames, t.Name())
				serverTools[m.Name] = append(serverTools[m.Name], t.Name())
				if directVisible && !skillGated {
					// directly visible and not gated → available from session start
					globalNames = append(globalNames, t.Name())
					globalSet[t.Name()] = true
				}
				// skill-gated tools stay out of globalNames; unlocked via unlockSkill()
			}
		}
	}
	art.serverTools = serverTools
	art.allNames = allNames
	art.globalNames = globalNames
	art.globalSet = globalSet
	return agent.AssemblyBuild{Value: art, Closers: closers, Cacheable: cacheable}, nil
}

// rebuild 用缓存产物重建本 run 的轻包装（G1）：新 UnlockSet / OnInvoke / Skill 工具 /
// host 工具 + per-run 名单拷贝。工具追加顺序与全量装配逐字一致：
// [mcp 工具..., Skill 元工具, host 工具...]。
// cleanup：缓存命中的 run 是 no-op（连接跨 run 复用）；降级产物（不可缓存的 build）
// 的 release 会关掉本 run 新建的连接 —— 与缓存前的 cleanup 语义一致。
func (asm *agentAssembly) rebuild(ctx context.Context, pg *db.DB, skillDir string, hostTools func() ([]actool.CoreTool, map[string][]string), cleanup func()) ([]actool.CoreTool, agent.DeferredInfo, func()) {
	var extra []actool.CoreTool
	extra = append(extra, asm.mcpTools...)
	// 名单/集合按 run 拷贝：host deferred 工具会往里追加，缓存产物本体保持只读。
	allNames := append([]string(nil), asm.allNames...)
	globalNames := append([]string(nil), asm.globalNames...)
	globalSet := make(map[string]bool, len(asm.globalSet))
	for k, v := range asm.globalSet {
		globalSet[k] = v
	}

	// Skill 元工具用的 reg 每 run 重建 —— OnInvoke 捕获本 run 的 unlock，绝不能共享。
	var reg *skill.Registry
	if len(asm.skills) > 0 {
		reg = skill.NewRegistry()
		for _, s := range asm.skills {
			reg.Add(s)
		}
	}

	// Shared call-gate: global MCP tools are unlocked from the start; skill-gated
	// ones are unlocked when their skill loads (or replayed from history — C2).
	unlock := actool.NewUnlockSet(globalNames...)
	unlockSkill := func(skillName string) {
		if reg == nil {
			return
		}
		if s, ok := reg.Get(skillName); ok {
			for _, srv := range s.MCPs {
				unlock.Add(asm.serverTools[srv]...)
			}
		}
	}

	if reg != nil {
		reg.OnInvoke = func(s skill.Skill) string {
			unlockSkill(s.Name)
			var reveal []string
			for _, srv := range s.MCPs {
				for _, n := range asm.serverTools[srv] {
					if !globalSet[n] {
						reveal = append(reveal, n)
					}
				}
			}
			return actool.RenderDeferredToolsBlock(reveal)
		}
		// Attribution for the usage ledger: ToolAugment only gets (ctx, agentKey),
		// so the run's task/session ids ride in on the ctx (agent.RunInfo). Read it
		// once here — this closure is rebuilt per run, so the captured value always
		// belongs to this run.
		extra = append(extra, meterSkillTool(reg.Tool(), pg, reg, skillDir, asm.agentKey, agent.RunInfoFrom(ctx)))
	}

	// host tools (traffic / orchestration / custom) — 每跑现取：capture 开关、自定义
	// 工具增删都实时生效（不进缓存）。Custom tools flagged deferred contribute their
	// names to the deferred wiring so their schema is withheld
	// (SearchExtraTools/ExecuteExtraTool), same as MCP.
	if hostTools != nil {
		ht, deferredBinds := hostTools()
		extra = append(extra, ht...)
		for name, boundAgents := range deferredBinds {
			if !contains(boundAgents, asm.agentKey) {
				continue // only defer names this agent is actually bound to
			}
			allNames = append(allNames, name)       // schema withheld from the prompt
			globalNames = append(globalNames, name) // advertised in the deferred block
			globalSet[name] = true
			if unlock != nil {
				unlock.Add(name) // global deferred → callable from the start
			}
		}
	}

	def := agent.DeferredInfo{
		Deferred:    allNames,
		GlobalNames: globalNames,
		Unlock:      unlock,
		UnlockSkill: unlockSkill,
	}
	return extra, def, cleanup
}

func idSet(ids []int64) map[int64]bool {
	m := make(map[int64]bool, len(ids))
	for _, id := range ids {
		m[id] = true
	}
	return m
}

// seedPrompts writes each built-in agent's code-default prompt body into
// agent_prompts on startup — first-insert only (SeedPromptIfEmpty is a no-op once
// any version exists), so the DB becomes the authoritative editable source while
// user edits survive restarts. Runs after seedBuiltins has created the agent rows.
func seedPrompts(pg *db.DB) {
	for key, tmpl := range agent.BuiltinPromptSeeds() {
		a, err := pg.GetAgentByKey(key)
		if err != nil || a == nil {
			log.Printf("[prompts] seed %s 跳过: agent 不存在 (%v)", key, err)
			continue
		}
		if err := pg.SeedPromptIfEmpty(a.ID, tmpl); err != nil {
			log.Printf("[prompts] seed %s 失败: %v", key, err)
		}
	}
}

// wireTools seeds the built-in tool catalog (idempotent, first-insert only so page
// edits survive restart) and wires the DB tools table into the agent runtime: at
// tool-assembly time each built-in tool is filtered by its agent binding / enabled
// flag and, if kept, wrapped so the model sees the DB-overridden description/schema
// and缺省入参 get injected. MCP/skill/host tools have no row and pass through.
func wireTools(pg *db.DB, domainReg map[string]actool.CoreTool) {
	agent.FindingTrafficBindingEnabled = func() bool { return pg.GetBool(settingAgentTrafficBinding, false) }
	// 工具表不可读且无快照时，拒绝创建新 run（V2 G-04④）。
	//
	// 降级为最小工具面只是「少给几个工具」；但冷启动那一次恰恰是无人值守的时刻，
	// 白名单未知时开工等于无边界放行。这道闸由 db 层在所有任务创建入口的汇合点执行。
	//
	// 只注册「有无快照」这个判定，工具表本身由 db 层按**当前这个库**读取 ——
	// 若把 pg 捕获进钩子，测试里前一个用例关库后钩子就会指向已关闭的库。
	pg.SetToolTableFallback(func() bool {
		cached, _ := lastGoodToolRows.Load().([]*db.Tool)
		return cached != nil
	})
	// Seed the built-in domain tools (first-insert only; DO NOTHING preserves edits).
	// No startup prune: rows we didn't seed are left alone so future user-defined
	// custom tools (system=false, added via the UI) survive restarts.
	for _, s := range agent.BuiltinToolSeeds() {
		schema, _ := json.Marshal(s.Schema)
		agents, _ := json.Marshal(s.Agents)
		if err := pg.SeedTool(s.Key, s.Desc, schema, agents); err != nil {
			log.Printf("[tools] seed %s 失败: %v", s.Key, err)
		}
	}
	// Seed the traffic host tools so they're bindable per-agent like built-ins.
	// Default binding = worker (preserves prior behavior). Their runtime availability
	// is still gated by the global capture switch (hostTools() returns them only when
	// capture is on), so an off-capture binding simply never surfaces the tool.
	trafficAgents, _ := json.Marshal([]string{"worker"})
	for _, t := range traffic.SeedToolMetas() {
		schema, _ := json.Marshal(t.InputSchema())
		if err := pg.SeedTool(t.Name(), t.Description(), schema, trafficAgents); err != nil {
			log.Printf("[tools] seed %s 失败: %v", t.Name(), err)
		}
	}
	// bashInteractiveShellNote is appended to Bash's description ONLY for agents whose
	// interactive_shell is on, so Bash points at shell_open for interactive programs
	// without ever referencing a tool that isn't injected (§14.1/§14.2).
	const bashInteractiveShellNote = "\n\n需要【交互输入】的程序（msfconsole / ssh 交互登录 / mysql、psql、python 等 REPL / 密码或 yes/no 提示 / nc 反弹 shell）不要用 Bash（它没有 stdin、会卡住），改用 shell_open 开交互会话（用完 shell_close）。一次性、非交互命令仍用 Bash。"
	agent.ToolResolve = func(ctx context.Context, agentKey string, tools []actool.CoreTool) []actool.CoreTool {
		rows, err := pg.ListTools()
		degraded := false
		if err != nil {
			cached, _ := lastGoodToolRows.Load().([]*db.Tool)
			if cached == nil {
				degraded = true
				log.Printf("[tools] 读取工具表失败且无可用快照（冷启动），降级为最小工具面: %v", err)
			} else {
				log.Printf("[tools] 读取工具表失败，沿用上一份已验证快照（%d 行）: %v", len(cached), err)
				rows = cached
			}
		} else {
			lastGoodToolRows.Store(rows)
		}
		if degraded {
			// rows 为空则 byKey 空、全部工具走 !known 分支原样放行，故此处不能 return tools。
			out := make([]actool.CoreTool, 0, len(tools))
			for _, t := range tools {
				if degradedToolAllow[t.Name()] {
					out = append(out, t)
				}
			}
			return out
		}
		byKey := make(map[string]*db.Tool, len(rows))
		for _, t := range rows {
			byKey[t.Key] = t
		}
		runInfo := agent.RunInfoFrom(ctx)
		resolve := func(t actool.CoreTool, row *db.Tool) actool.CoreTool {
			var schema map[string]any
			if len(row.Schema) > 0 {
				_ = json.Unmarshal(row.Schema, &schema)
			}
			return meterTool(agent.DecorateTool(t, row.Description, schema), pg, row.Key, agentKey, runInfo)
		}
		out := tools[:0:0]
		for _, t := range tools {
			row, known := byKey[t.Name()]
			if !known { // MCP/skill/host tool: no row → untouched
				out = append(out, t)
				continue
			}
			if !row.Enabled || !contains(row.Agents, agentKey) {
				continue // disabled globally or not bound to this agent → drop
			}
			out = append(out, resolve(t, row))
		}
		// inject: domain tools bound to this agent in the DB but absent from the
		// incoming list. Covers agents (Auto, custom) whose base only has DefaultTools
		// and therefore never includes ToolSet-backed domain tools. Per-task instances
		// in the base always win: inList is built from the original incoming list so a
		// worker's own upsert_asset is never shadowed by the server-level registry copy.
		if len(domainReg) > 0 {
			inList := make(map[string]bool, len(tools))
			for _, t := range tools {
				inList[t.Name()] = true
			}
			for _, row := range rows {
				if row.Kind == "shell" || !row.Enabled || !contains(row.Agents, agentKey) || inList[row.Key] {
					continue
				}
				inst, ok := domainReg[row.Key]
				if !ok {
					continue // not a domain tool; custom/host tools are injected via hostTools()
				}
				out = append(out, resolve(inst, row))
			}
		}
		// shell hints: user-defined kind="shell" tools are not callable — they are
		// environment declarations that tell the model which command-line tools are
		// installed. Collect the ones bound to this agent and append to Bash's description.
		var shellHints []string
		for _, row := range rows {
			if row.Kind == "shell" && row.Enabled && contains(row.Agents, agentKey) {
				shellHints = append(shellHints, "- "+row.Key+": "+row.Description)
			}
		}
		if len(shellHints) > 0 {
			note := "\n\n以下工具已安装在此 bash 环境中，可直接通过 Bash 调用：\n" + strings.Join(shellHints, "\n")
			for i, t := range out {
				if t.Name() == "Bash" {
					out[i] = agent.DecorateTool(t, t.Description()+note, t.InputSchema())
					break
				}
			}
		}
		// interactive shell: gated purely by the agent's interactive_shell flag (like
		// web_search), NOT by tools-table binding. When on, inject the 5 shell_* tools
		// and COUPLE the Bash description addendum so it points at shell_open — and never
		// dangles when off. See docs/交互式shell设计.md §14.2.
		if !actool.InteractiveShellDisabled() {
			if a, err := pg.GetAgentByKey(agentKey); err == nil && a != nil && a.InteractiveShell {
				out = append(out, actool.ShellSessionTools()...)
				for i, t := range out {
					if t.Name() == "Bash" {
						out[i] = agent.DecorateTool(t, t.Description()+bashInteractiveShellNote, t.InputSchema())
						break
					}
				}
			}
		}
		return out
	}
}

func contains(ss []string, v string) bool {
	for _, s := range ss {
		if s == v {
			return true
		}
	}
	return false
}

// buildDomainReg builds a name→CoreTool registry from a server-level ToolSet
// (real AssetStore, nil ExplorationStore, taskID=0). Used by ToolResolve to inject
// domain tools into agents (Auto, custom) that don't own a per-task ToolSet.
// nil as → returns nil (no injection, graceful degradation).
//
// The nil ExplorationStore is deliberate — these instances are task-less by
// construction — so every tool here must tolerate it. Asset/company tools do
// (they only need the AssetStore); the exploration-graph tools refuse with a
// clear message via ToolSet.needExploration. Binding one of them to a task-less
// agent in the tools table is therefore a useless tool, not a crash.
func buildDomainReg(as *db.AssetStore) map[string]actool.CoreTool {
	if as == nil {
		return nil
	}
	serverTS := agent.NewToolSet(nil, "")
	serverTS.SetAssetStore(as, as.Companies())
	reg := make(map[string]actool.CoreTool)
	for _, t := range serverTS.AllDomainTools() {
		reg[t.Name()] = t
	}
	return reg
}

func jsonStrSlice(raw json.RawMessage) []string {
	var out []string
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &out)
	}
	return out
}

func jsonStrMap(raw json.RawMessage) map[string]string {
	out := map[string]string{}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &out)
	}
	return out
}

// lastGoodToolRows 保存最后一次成功读取的工具表。ListTools 失败时沿用它而非
// 全量放行：放行意味着 DB 一抖动，所有 agent 就拿到完整工具面（含本该被
// 白名单挡住的），且无任何告警可见性。
var lastGoodToolRows atomic.Value

// degradedToolAllow 是工具表不可读且无快照时仅放行的 SDK 基础件。
// 平台/MCP/领域/自定义工具一律不在此列：它们的效果依赖 DB 绑定关系，
// 而此刻 DB 恰好不可读，无从判断该不该给。
var degradedToolAllow = map[string]bool{
	"Bash": true, "Read": true, "Write": true, "Edit": true,
	"MultiEdit": true, "LS": true, "Glob": true, "Grep": true,
}

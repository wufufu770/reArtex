package agent

import (
	"context"
	"encoding/json"

	"github.com/Autumn-27/artex/db"
	actool "github.com/Autumn-27/norma/tool"
)

// 本文件把「内置工具」从纯代码变成可枚举、可被 DB 覆盖的目录：
//   - BuiltinToolSeeds()：把三个执行 agent 的内置工具集展开成 seed 记录（key +
//     描述 + 参数 schema + 默认绑定的 agent），供服务端开机幂等播种进 tools 表。
//   - ToolResolve 钩子：运行时按 DB 里的 tools 行对已装配的工具做「按 agent 过滤 +
//     覆盖描述/schema + 注入参数默认值」。key/handler 仍在代码层，DB 只改「散文与默认值」。
// handler（Call 行为）永远来自代码——DB 改不了它，只能改模型看到的说明与缺省入参。

// ToolSeed 是一个内置工具的可播种快照：key 即 CoreTool.Name()（与 handler 死绑，
// UI 只读），Desc/Schema 取自代码里的工具定义，Agents 是代码默认把它给了哪些 agent。
type ToolSeed struct {
	Key    string         // = CoreTool.Name()，主键，不可改
	Desc   string         // 顶层描述（可在 UI 覆盖）
	Schema map[string]any // 参数 JSON-Schema（结构只读，description/default 可在 UI 改）
	Agents []string       // 默认绑定的 agent key（worker/planner/mainagent）
}

// builtinToolsByAgent 用一个「只读空壳」ToolSet（nil stores）构造每个执行 agent 的
// 领域工具集。工具构造函数只把闭包塞进 Spec、构造期不解引用 store，所以 nil 安全——
// 这些工具在这里只用来读 Name()/Description()/InputSchema()，绝不 Call。
//
// 刻意【不含】SDK 通用工具 actool.DefaultTools()（Read/Write/Edit/MultiEdit/LS/Glob/
// Grep/Bash）：它们每个 agent 都固定拥有、没有「绑定到谁」的取舍，且说明大多在 Prompt()
// 里（本表只覆盖 Description()，会造成半覆盖误导）。不 seed → 无 DB 行 → ToolResolve
// 原样放行、不覆盖，行为与从前一致。只有 artex 自己的领域工具入表可管。
// 顾问型五件套（curator/verifier/redteam/creative/intent-normalizer）不在这张表里：
// 它们的工具不是「代码里写死给它的一套」，而是按 db 层的绑定策略逐个工具判定
// （白名单 + 拒绝优先），见 applyAdvisoryAgentBindings。
func builtinToolsByAgent() map[string][]actool.CoreTool {
	ts := NewToolSet(nil, "")
	return map[string][]actool.CoreTool{
		"mainagent": ts.MainAgentTools(),
		"planner":   ts.PlannerTools(),
		"worker":    ts.WorkerTools(),
		// goals（目标拆解器）默认绑 set_goals + set_constraints：靠它们把拆出的目标、
		// 抽出的操作约束写进库。与 mainagent 共用同一受管工具，web 端可改描述/schema、按 agent 勾选。
		"goals": {ts.setGoals(), ts.setConstraints()},
		// auto 默认绑漏洞上报 + 资产管理工具，其他域工具可在 UI 按需勾选。
		// 新库由此 seed 写入；老库由 seedAutoDefaultBindings 迁移。
		"auto": {ts.addFinding(), ts.insertAssets(), ts.addCompanyScope(), ts.listAssets(), ts.listCompanies()},
		// pentest（独立渗透 agent）默认绑：查资产 / 插资产 / 报漏洞 / 查漏洞 / 查企业。
		// 新库由此 seed 写入；老库由 seedPentestDefaultBindings 迁移。
		"pentest": {ts.listAssets(), ts.insertAssets(), ts.addFinding(), ts.listFindings(), ts.listCompanies()},
	}
}

// defaultUnbound：这些 system 工具会照常入目录（web 端可见、可手动按 agent 勾选），但
// 默认【不绑任何 agent】——ToolResolve 对空绑定的工具对所有 agent 一律丢弃,须显式 opt-in。
// 之所以仍留在某个 agent 的 base 工具集里(如 goal_met 在 PlannerTools):一是让 seed 能
// 构造它拿到 desc/schema,二是用户手动绑回后运行时 base 里有它、ToolResolve 才留得住。
//
// goal_met：绕过逐个 prove_goal、直接从全局宣布【整个任务完成】,权重大且有误判风险,又与
// 「prove_goal 标记最后一个目标 → 自动收官」重复,故默认不给任何 agent,需要时再手动绑。
var defaultUnbound = map[string]bool{"goal_met": true}

// BuiltinToolSeeds 把各 agent 的内置工具集去重合并成 seed 列表：同名工具（如 list_assets
// 多个 agent 都有）合成一条，Agents 取并集；defaultUnbound 里的工具则强制绑定为空。
func BuiltinToolSeeds() []ToolSeed {
	byAgent := builtinToolsByAgent()
	order := []string{"mainagent", "goals", "planner", "worker", "auto", "pentest"}

	type acc struct {
		tool   actool.CoreTool
		agents []string
	}
	m := map[string]*acc{}
	var keys []string
	for _, ak := range order {
		for _, t := range byAgent[ak] {
			a, ok := m[t.Name()]
			if !ok {
				a = &acc{tool: t}
				m[t.Name()] = a
				keys = append(keys, t.Name())
			}
			a.agents = append(a.agents, ak)
		}
	}

	out := make([]ToolSeed, 0, len(keys))
	for _, k := range keys {
		a := m[k]
		agents := a.agents
		if defaultUnbound[k] {
			agents = []string{} // 入目录、可手动绑，但默认不给任何 agent（存 [] 而非 null，与其它工具一致）
		}
		out = append(out, ToolSeed{
			Key:    k,
			Desc:   a.tool.Description(),
			Schema: a.tool.InputSchema(),
			Agents: agents,
		})
	}
	out = append(out, emitStructuredIntentSeed)
	return applyAdvisoryAgentBindings(out)
}

// emitStructuredIntentSeed 是「输出结构化意图」工具的目录条目。
//
// 它是 intent-normalizer 的唯一输出通道，必须在工具目录里占位：tools 表的行全部
// 来自 BuiltinToolSeeds()，白名单里的名字若没有对应行，协调时只能跳过并记录，
// agent 就永远拿不到工具。绑定不硬编码在这里——它由 db 层策略
// （AdvisoryAgentToolPolicies）经 applyAdvisoryAgentBindings 追加，与另外几个
// 顾问 agent 同一套机制。
//
// handler 属于执行侧接线（Phase 6）：目录占位只负责「绑得上、看得见」，不负责
// 「调得动」——ToolResolve 的注入路径按 domainReg 查 handler，查不到就不注入，
// 所以占位期间不会有任何 agent 拿到一个调不动的工具。
var emitStructuredIntentSeed = ToolSeed{
	Key:  "emit_structured_intent",
	Desc: "把原始动作（shell 命令 / HTTP 请求 / 工具参数）转成结构化意图并提交。只做结构化拆分与标记，不执行、不改写语义、不判断 scope。",
	Schema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"structured_intent": map[string]any{
				"type":        "object",
				"description": "结构化意图本体",
				"properties": map[string]any{
					"action_type":        map[string]any{"type": "string", "description": "command_execution | http_request | tool_invocation | unparseable"},
					"executable":         map[string]any{"type": "string", "description": "shell 主体 / http method / tool_key"},
					"arguments":          map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "原样参数"},
					"target_hint":        map[string]any{"type": "string", "description": "候选目标（不判断是否在 scope 内）"},
					"shell_features":     map[string]any{"type": "object", "description": "管道/重定向/命令替换/后台/混淆的布尔标记"},
					"body_digest":        map[string]any{"type": "string", "description": "http body 的哈希（仅 http 类型）"},
					"raw_payload_digest": map[string]any{"type": "string", "description": "原始动作的哈希，用于审计"},
				},
				"required": []string{"action_type", "raw_payload_digest"},
			},
			"anomaly_flag":   map[string]any{"type": "boolean", "description": "是否发现异常（只标记，不拦截）"},
			"anomaly_reason": map[string]any{"type": "string", "description": "异常原因（一句话，不含主观判断）"},
			"source_agent":   map[string]any{"type": "string", "description": "发起该动作的 agent_key"},
			"task_id":        map[string]any{"type": "string", "description": "任务 id"},
		},
		"required": []string{"structured_intent", "anomaly_flag", "source_agent"},
	},
}

// applyAdvisoryAgentBindings 把顾问型 agent（curator/verifier/redteam/creative/
// intent-normalizer）的
// 工具绑定按策略追加进各工具的 agents 数组。
//
// 为什么在这里算：tools 表的 agents 列就是「这个工具给了谁」，而 BuiltinToolSeeds 是
// 内置工具播种的唯一入口——绑定在这里算，才会在播种时随行落库。既有 agent 的绑定一个
// 字节都不动（纯追加，既有条目顺序不变）。
//
// 为什么是「按名字」而不是「按工具对象」：策略库里那些工具（read_strategy_library、
// submit_verdict…）此刻还没注册，没有工具对象可指认。等它们注册进目录，同一份策略
// 会自动把它们绑上，不需要在这里再改一次。
//
// 策略本身在 db 层（db/config.go 的 AdvisoryAgentToolPolicies）：agent → db 是单向依赖，
// 两边各留一份策略迟早漂移。拒绝优先由策略保证（Allows 内部先过 Denies）。
func applyAdvisoryAgentBindings(seeds []ToolSeed) []ToolSeed {
	for _, p := range db.AdvisoryAgentToolPolicies() {
		for i := range seeds {
			if !p.Allows(seeds[i].Key, "builtin") {
				continue
			}
			if containsAgentKey(seeds[i].Agents, p.Agent) {
				continue
			}
			seeds[i].Agents = append(seeds[i].Agents, p.Agent)
		}
	}
	return seeds
}

// containsAgentKey 判断 agent key 数组里是否含某个 key。
func containsAgentKey(keys []string, want string) bool {
	for _, k := range keys {
		if k == want {
			return true
		}
	}
	return false
}

// ToolResolve, if set, post-processes an agent's fully-assembled tool list against
// the DB tools table: it drops tools not bound to this agent (or globally disabled)
// and wraps the rest so the model sees the DB-overridden description/schema and
// 缺省入参 get injected. Tools with no matching DB row (MCP/skill/host tools like
// traffic) pass through untouched. nil = tools unchanged. Wired in server/assembly.go.
var ToolResolve func(ctx context.Context, agentKey string, tools []actool.CoreTool) []actool.CoreTool

// DecorateTool wraps t so Description()/InputSchema() report the DB overrides and
// Call() injects scalar parameter defaults (from schema's "default" props) whenever
// the model omitted them. Name/Prompt/permission/scheduler flags delegate to t, so
// the tool's identity and handler are unchanged. Empty desc/schema fall back to t's.
func DecorateTool(t actool.CoreTool, desc string, schema map[string]any) actool.CoreTool {
	if desc == "" {
		desc = t.Description()
	}
	if len(schema) == 0 {
		schema = t.InputSchema()
	}
	return &overriddenTool{CoreTool: t, desc: desc, schema: schema}
}

// overriddenTool is a CoreTool decorator: it embeds the original (so all behavioral
// methods — Prompt/IsReadOnly/IsConcurrencySafe/CheckPermissions/Name — delegate)
// and overrides only the model-facing description/schema plus default injection.
type overriddenTool struct {
	actool.CoreTool
	desc   string
	schema map[string]any
}

func (o *overriddenTool) Description() string         { return o.desc }
func (o *overriddenTool) InputSchema() map[string]any { return o.schema }

func (o *overriddenTool) Call(ctx context.Context, in json.RawMessage, tc *actool.ToolContext) (actool.Result, error) {
	return o.CoreTool.Call(ctx, injectDefaults(in, o.schema), tc)
}

// injectDefaults fills scalar parameter defaults declared in the (possibly edited)
// schema into the input JSON whenever the model omitted the field or left it empty/
// null. Structure (names/types/required) is untouched — only缺省值 are merged in.
func injectDefaults(in json.RawMessage, schema map[string]any) json.RawMessage {
	defs := scalarDefaults(schema)
	if len(defs) == 0 {
		return in
	}
	m := map[string]json.RawMessage{}
	if len(in) > 0 {
		if err := json.Unmarshal(in, &m); err != nil {
			return in // non-object input: don't touch it
		}
	}
	changed := false
	for k, dv := range defs {
		if cur, ok := m[k]; !ok || isEmptyJSON(cur) {
			m[k] = dv
			changed = true
		}
	}
	if !changed {
		return in
	}
	b, err := json.Marshal(m)
	if err != nil {
		return in
	}
	return b
}

// scalarDefaults extracts properties[k]["default"] for scalar params (string/
// integer/number/boolean). Array/object defaults are skipped: merging them is
// ambiguous and not worth the surprise.
func scalarDefaults(schema map[string]any) map[string]json.RawMessage {
	props, _ := schema["properties"].(map[string]any)
	if len(props) == 0 {
		return nil
	}
	out := map[string]json.RawMessage{}
	for name, raw := range props {
		p, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		dv, ok := p["default"]
		if !ok || dv == nil {
			continue
		}
		switch p["type"] {
		case "string", "integer", "number", "boolean":
			if b, err := json.Marshal(dv); err == nil {
				out[name] = b
			}
		}
	}
	return out
}

func isEmptyJSON(raw json.RawMessage) bool {
	s := string(raw)
	return s == "null" || s == `""`
}

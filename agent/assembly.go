package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"runtime/debug"

	actool "github.com/Autumn-27/norma/tool"

	"github.com/Autumn-27/artex/internal/sod"
)

// DeferredInfo carries the deferred-tools wiring an agent needs to build its
// Options: the MCP tool names whose schemas are withheld, the subset listed in the
// global system-prompt block (non-skill-gated), and the shared session unlock set.
// UnlockSkill unlocks a named skill's MCPs — hosts call it to rebuild the unlock set
// from history on a resumed session (design doc C2).
type DeferredInfo struct {
	FindingGuidance string            // derived from the final permitted tools, including DB overrides
	Deferred        []string          // all MCP tool names (schema withheld)
	GlobalNames     []string          // MCP names to list in the system-prompt block
	Unlock          *actool.UnlockSet // shared call-gate; nil when no MCP tools
	UnlockSkill     func(skillName string)
}

// ToolAugment, if set, returns the EXTRA tools an agent should see beyond its
// built-in base set — the agent's visible skills (packed into one Skill meta-tool)
// and visible MCP servers (expanded to mcp__server__tool). It also returns the
// DeferredInfo describing how those MCP tools are deferred/gated. The server wires
// it to the PG agent_visibility table. cleanup releases any spawned MCP clients —
// G1（方案 v10 §15）后：装配缓存命中的 run 它是 no-op（连接跨 run 复用、进程退出统一
// 收），仅降级路径按 run 关闭本 run 新建的连接（见 assemblycache.go）。
//
// When nil, agents run with only their built-in tools — behavior is unchanged
// until a user assigns a skill/MCP to the agent in the UI.
var ToolAugment func(ctx context.Context, agentKey string) (extra []actool.CoreTool, def DeferredInfo, cleanup func())

// ToolDenialRecorder 把工具层的拒绝决定记进审计链。nil 时不记录。
//
// 为什么需要它：工具层（norma 的 permission 判定 Stage 1）早于 PreToolUse hook
// 被求值 —— hook 不会被调用，于是短路表那条路径上的留痕（intercept_pending +
// audit_events）拿不到。工具层是覆盖面最广的一层（所有 agent 的所有工具都经
// AugmentTools 包装），不能为了留痕把它降级；但留痕同样不能丢：被拦下的
// 「谁在何时想干什么」必须可追责。
//
// server 侧用 interceptor 实现（它负责审计链的规范化 payload 与串行化写入）。
var ToolDenialRecorder func(ctx context.Context, toolName, reason string, input []byte)

// AugmentTools returns base plus the agent's visible skill/MCP tools, the
// DeferredInfo, and a cleanup func the caller must defer (closes MCP clients).
// Built-in base tools are kept as-is — never filtered (内置工具留代码层，不做可见性过滤).
func AugmentTools(ctx context.Context, agentKey string, base []actool.CoreTool) ([]actool.CoreTool, DeferredInfo, func()) {
	var (
		def     DeferredInfo
		cleanup = func() {}
		out     = base
	)
	if ToolAugment != nil {
		var extra []actool.CoreTool
		var cl func()
		extra, def, cl = ToolAugment(ctx, agentKey)
		if cl != nil {
			cleanup = cl
		}
		if len(extra) > 0 {
			out = append(append([]actool.CoreTool{}, base...), extra...)
		}
	}
	// DB tools table has the final say on the built-in tools: drop the ones this
	// agent isn't bound to (or that are disabled) and swap in overridden
	// descriptions/schemas + default injection. MCP/skill/host tools have no row
	// and pass through untouched, so deferred/unlock wiring stays consistent.
	if ToolResolve != nil {
		out = ToolResolve(ctx, agentKey, out)
	}
	out, def.FindingGuidance = findingWorkflowTools(agentKey, out)
	// P1 角色工具纪律（agent/roledispatch.go）：装配期主闸——白名单外工具在此
	// 消失（模型不可见、系统提示不带其文档）。编译期常量，DB/settings 改不动。
	out, def = filterToolsForRole(agentKey, out, def)
	// 装饰顺序（由外到内）是刻意定的，不是随手写的：
	//
	//   enforceToolPolicy → guardPanic → 执行器 → 真实工具
	//
	// 为什么 enforceToolPolicy 在最外层：工具层（Stage-1 权限）的绝对拒绝权必须
	// 完整保留 —— 策略拒了就不该再问链「批不批」，更不该出现「链批了所以放行」。
	//
	// 为什么执行器在最内层：它是**最后一道**闸门，位置贴着真实调用 ⇒ 凭证检查
	// 发生在「真的要执行了」的那一刻；上面任何一层拒绝都在它之前发生，不会出现
	// 「工具已被策略拒绝却还去消费凭证」的副作用。
	//
	// 为什么摘要口径不会因顺序漂移：链在 PreToolUse hook 里对入参字节算摘要，
	// 执行器对同一份入参字节算摘要 ⇒ 两边算的是**同一个** CanonicalDigest。装饰器
	// 只透传入参，不改字节 ⇒ 顺序怎么排都不影响摘要一致。
	//
	// 为什么用 GatedTool(t.Name()) 而不是一律套上：放行清单（本地只读 / 平台只读 /
	// 纯记账）里的工具不进链 —— 它们不改状态、不外发、不派生进程，裁决它们只会
	// 给每次调用凭空加 1~4 秒的规整延迟。清单外的**全部**工具都套上 ⇒ 未登记的
	// 新工具（MCP 任意名、自定义工具任意 key）默认被管住。
	//
	// 为什么执行器装在这里（所有 agent 共用的 AugmentTools）而不是只装 worker：
	// AugmentTools 是工具装配的唯一收口 ⇒ 装在这里才能保证「模型提议 ⇒ 确定性层
	// 裁决 ⇒ 执行器验凭证」这条链没有旁路。受管与否在**运行期**由 ctx 判定
	// （internal/sod.ManagedScopeOf），不在装配期 ⇒ 非 worker 路径（planner /
	// mainagent / chat）不会去索要一把没人铸的凭证。
	for i, t := range out {
		// ⚠️ 凭证表传 nil：执行器因此在【调用时】现读进程级单例，而不是在装配时
		// 把它快照下来。
		//
		// 为什么不能快照：工具装配与装链的先后顺序不是承诺。若装配早于装链，
		// 快照到的就是 nil ⇒ 那个执行器永久握着一张空表 ⇒ 受管路径上的每一次
		// 调用都被拒（fail-closed，不漏放行，但整条链静默瘫掉，日志上看不出是
		// 「表没装」而不是「链没批」）。链重建（换新表）也是同一个病。
		//
		// 现读单例把「装配顺序」这个变量彻底去掉：执行器与链永远读同一张表。
		out[i] = enforceToolPolicy(guardPanic(sod.WrapExecutor(t, nil, sod.GatedTool(t.Name()))))
	}
	return out, def, cleanup
}

// guardPanic turns a panicking tool handler into an ordinary tool error. The
// harness runs each tool on its own goroutine, so a panic inside a handler can't
// be recovered by the caller that started the run — it takes the whole process
// down, and on restart the agent replays the same call and crashes again. Wrapped
// per tool in AugmentTools, so it covers every tool the agent can reach: domain,
// SDK, MCP and skill. It sits inside enforceToolPolicy — policy decides first,
// panic recovery only matters once a call is allowed through.
func guardPanic(t actool.CoreTool) actool.CoreTool { return &guardedTool{CoreTool: t} }

type guardedTool struct{ actool.CoreTool }

func (g *guardedTool) Call(ctx context.Context, in json.RawMessage, tc *actool.ToolContext) (res actool.Result, err error) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[tools] %s panic: %v\n%s", g.Name(), r, debug.Stack())
			res, err = actool.Errorf(fmt.Sprintf("工具 %s 内部错误：%v（本次调用已失败，可换个参数或改用别的工具）", g.Name(), r)), nil
		}
	}()
	return g.CoreTool.Call(ctx, in, tc)
}

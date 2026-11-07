package agent

// ---------- 角色工具纪律（P1，方案 v3.1 §5）----------
//
// 编译期角色→工具白名单：**DB/settings 任何 PUT 都改不动**（公理 1：约束 agent
// 行为的边界挂源码）。本期只钉 planner 一个角色（改动最小、收益最大，为 P2 角色
// 定型铺路）；其余角色不收敛（nil 语义 = 维持现状全量）。
//
// 双闸设计（两闸共用同一份常量表，行为不一致即 bug——由单测钉死）：
//   装配期主闸（AugmentTools 末尾过滤）：白名单外的工具根本不进 registry——
//     模型看不见、系统提示不带其文档（上下文净省，P0 基线可量化）。
//   调用期副闸（toolpolicy.CheckPermissions 最前端）：防 deferred unlock / MCP
//     注入等装配期旁路；拒绝走 ToolDenialRecorder 审计链。
//
// 威胁模型（如实版，v2 §5.1）：planner 被注入后仍能写坏意图/误派 worker——爆炸
// 半径从"任意命令执行"缩到"浪费若干 worker 周期 + 图上坏意图（kill_work 可清）"，
// 可观测可恢复。**Bash 与一切写宿主 FS 工具已移出**（红队 P0-3：tier_judge 无命令
// 级判定、会话跑 ModeBypass，planner+Bash=任意命令执行，一行注释带不过关）。
// 残余面：Read/LS/Grep 仍读宿主 FS（tier3LocalFileTools 口径）——本期 planner
// 白名单不含它们（见下），planner 的宿主接触面收敛为**零**。

import (
	"context"
	"strings"

	actool "github.com/Autumn-27/norma/tool"
)

// plannerToolAllow 是 planner 的允许集（v2 §5.1 终版清单 + 回归修订）。
// 相对 PlannerTools+DefaultTools 现状的显式变更（全部刻意，红队 P1-1 声明）：
//
//	移除 Bash/Write/Edit/MultiEdit/Glob/Sleep —— 宿主 FS 与进程接触面清零；
//	移除 add_task_scope/add_company_scope/list_untested_assets —— 范围自主扩权
//	  收归人工/主 agent；
//	移除 strategy_get/strategy_search/strategy_propose —— 维持 strategy_get 的
//	  defaultUnbound 既有安全决策，不静默放宽。
//
// 回归修订（原能力优先，finding_workflow/assembly 回归抓出来的）：
//
//	补回 report_finding —— 原版 PlannerTools 注释即写明「规划态势研判时若自身
//	  已确证漏洞，可直接登记（与 worker 同工具）」，这是 finding 工作流的 planner
//	  合法路径，不是旁路；它有 evidence 强校验 + 落 findings 表审计，不是执行面。
//	补回 Skill 元工具 —— 技能可见性是 per-agent 机制，只是加载技能说明文本；
//	  不给 planner 收敛它等于砍掉 planner 的技能能力。
var plannerToolAllow = map[string]bool{
	"graph_overview":           true,
	"list_facts":               true,
	"node_detail":              true,
	"list_findings":            true,
	"list_goals":               true,
	"expand_digest":            true,
	"get_worker_output":        true,
	"get_worker_trace":         true,
	"search_all_worker_traces": true,
	"list_assets":              true,
	"list_companies":           true,
	"add_intent":               true,
	"propose_intent":           true,
	"kill_work":                true,
	"steer_work":               true,
	"prove_goal":               true,
	"goal_met":                 true,
	"report_finding":           true,
	"Skill":                    true,
}

// roleToolAllow 是角色→允许集的总表；值 nil = 该角色不收敛（维持现状）。
// 新增受钉角色时在这里加一行——不要在调用点写 if。
var roleToolAllow = map[string]map[string]bool{
	"planner": plannerToolAllow,
	// "worker": nil, "mainagent": nil, "goals": nil, chat/*: 不收敛
}

// RoleConstrained 报一个 agent key 是否被白名单收敛。
// 未登记的 agent key（自定义 agent / chat 类）一律不收敛——显式登记才收紧。
func RoleConstrained(agentKey string) bool {
	_, ok := roleToolAllow[strings.TrimSpace(agentKey)]
	return ok
}

// RoleAllowsTool 报某角色是否允许某工具；constrained=false 表示该角色不受白名单
// 管辖（调用方应放行并跳过判定）。
func RoleAllowsTool(agentKey, toolName string) (allowed, constrained bool) {
	allow, ok := roleToolAllow[strings.TrimSpace(agentKey)]
	if !ok {
		return true, false
	}
	return allow[strings.TrimSpace(toolName)], true
}

// filterToolsForRole 是装配期主闸：把允许集外的工具从最终 registry 与 deferred
// 名单里剔除（模型看不见 ⇒ 系统提示不携带其文档）。在 AugmentTools 的全部装配
// （base/ToolAugment/ToolResolve/finding 工作流）完成之后、装饰之前调用。
func filterToolsForRole(agentKey string, out []actool.CoreTool, def DeferredInfo) ([]actool.CoreTool, DeferredInfo) {
	if !RoleConstrained(agentKey) {
		return out, def
	}
	kept := out[:0:0]
	for _, t := range out {
		if t == nil {
			continue
		}
		if allowed, _ := RoleAllowsTool(agentKey, t.Name()); allowed {
			kept = append(kept, t)
		}
	}
	if len(def.Deferred) > 0 {
		keptDeferred := def.Deferred[:0:0]
		for _, name := range def.Deferred {
			if allowed, _ := RoleAllowsTool(agentKey, name); allowed {
				keptDeferred = append(keptDeferred, name)
			}
		}
		def.Deferred = keptDeferred
	}
	if len(def.GlobalNames) > 0 {
		keptGlobal := def.GlobalNames[:0:0]
		for _, name := range def.GlobalNames {
			if allowed, _ := RoleAllowsTool(agentKey, name); allowed {
				keptGlobal = append(keptGlobal, name)
			}
		}
		def.GlobalNames = keptGlobal
	}
	return kept, def
}

// ---------- agent key 的 ctx 传递（调用期副闸的身份来源）----------

type agentKeyCtxKey struct{}

// WithAgentKey 把当前运行的 agent key 挂上 ctx。四个任务 lane（planner/worker/
// mainagent/chat）在装配 ctx 时各打一行；toolpolicy.CheckPermissions 读它做
// 调用期兜底判定。未打 key 的 ctx = 未收敛（fail-open 到现状，不误伤）。
func WithAgentKey(ctx context.Context, agentKey string) context.Context {
	return context.WithValue(ctx, agentKeyCtxKey{}, strings.TrimSpace(agentKey))
}

// AgentKeyFrom 读 ctx 上的 agent key；没有 = ""。
func AgentKeyFrom(ctx context.Context) string {
	if v, ok := ctx.Value(agentKeyCtxKey{}).(string); ok {
		return v
	}
	return ""
}

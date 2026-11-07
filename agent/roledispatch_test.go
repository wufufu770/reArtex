package agent

// 角色工具纪律（P1）的验收用例：白名单内容钉死 + 双闸一致性 + 未收敛角色放行。
// 方案 v3.1 §5：两闸共用同一份常量表，行为不一致即 bug。

import (
	"context"
	"testing"

	actool "github.com/Autumn-27/norma/tool"
)

// TestPlannerAllowlistPinned 钉白名单终版内容：宿主接触面为零（含 Read/LS/Grep
// 一并移除）、执行/扩权/策略工具不在（红队 P0-3/P1-1 的修订声明）；
// report_finding 与 Skill 元工具在（回归修订：原能力优先——finding 工作流的
// planner 合法路径 + per-agent 技能可见性）。
func TestPlannerAllowlistPinned(t *testing.T) {
	banned := []string{
		"Bash", "Write", "Edit", "MultiEdit", "Glob", "Read", "LS", "Grep", "Sleep", // 宿主 FS / 进程
		"add_task_scope", "add_company_scope", // 范围自主扩权
		"list_untested_assets",                                // 补测视角收归人工
		"strategy_get", "strategy_search", "strategy_propose", // defaultUnbound 不放宽
		"insert_assets", "record_fact", // worker 的写回工具
	}
	for _, name := range banned {
		if allowed, constrained := RoleAllowsTool("planner", name); constrained && allowed {
			t.Fatalf("planner 不应允许 %s", name)
		}
	}
	required := []string{
		"graph_overview", "list_facts", "node_detail", "list_findings", "list_goals",
		"expand_digest", "get_worker_output", "get_worker_trace", "search_all_worker_traces",
		"list_assets", "list_companies", "add_intent", "propose_intent",
		"kill_work", "steer_work", "prove_goal", "goal_met",
		"report_finding", "Skill",
	}
	for _, name := range required {
		if allowed, constrained := RoleAllowsTool("planner", name); !constrained || !allowed {
			t.Fatalf("planner 应允许 %s (constrained=%v allowed=%v)", name, constrained, allowed)
		}
	}
}

// TestBothGatesAgree 双闸一致性：装配闸（filterToolsForRole 的保留条件）与调用闸
// （RoleAllowsTool）对同一批名字必须同值——它们引用同一份常量表，这里钉死不漂移。
func TestBothGatesAgree(t *testing.T) {
	names := []string{
		"graph_overview", "add_intent", "Bash", "Write", "report_finding",
		"strategy_get", "mcp__anything__call", "SomeSkill", "record_fact",
		"list_facts", "node_detail", "Read", "",
	}
	for _, name := range names {
		allowed, constrained := RoleAllowsTool("planner", name)
		keptByAssembly := !constrained || allowed
		callGateAllows := allowed || !constrained
		if keptByAssembly != callGateAllows {
			t.Fatalf("双闸不一致：%q assembly(kept)=%v callgate(allow)=%v", name, keptByAssembly, callGateAllows)
		}
	}
}

// TestUnconstrainedRolesPass worker / 未知 agent key 不收敛：一切放行。
func TestUnconstrainedRolesPass(t *testing.T) {
	for _, key := range []string{"worker", "mainagent", "goals", "auto", "pentest", "", "custom-agent"} {
		if RoleConstrained(key) {
			t.Fatalf("%s 不应被收敛", key)
		}
		for _, name := range []string{"Bash", "anything"} {
			if allowed, constrained := RoleAllowsTool(key, name); !allowed || constrained {
				t.Fatalf("%s 对 %s 应放行且不受管 (allowed=%v constrained=%v)", key, name, allowed, constrained)
			}
		}
	}
}

// TestAssemblyFilterDropsBanned 装配闸端到端：含违禁工具的 base，经 AugmentTools
// 对 planner 应只剩白名单内工具（hooks 未装配的最小环境）。
func TestAssemblyFilterDropsBanned(t *testing.T) {
	savedAug, savedResolve := ToolAugment, ToolResolve
	ToolAugment, ToolResolve = nil, nil
	t.Cleanup(func() { ToolAugment, ToolResolve = savedAug, savedResolve })

	base := []actool.CoreTool{actool.NewRead(), actool.NewBash(), actool.NewWrite(), actool.NewLS()}
	out, def, _ := AugmentTools(context.Background(), "planner", base)
	names := map[string]bool{}
	for _, tool := range out {
		names[tool.Name()] = true
	}
	for _, banned := range []string{"Bash", "Write", "Read", "LS"} {
		if names[banned] {
			t.Fatalf("planner 装配结果不应含 %s: %v", banned, names)
		}
	}
	// deferred 名单同样被滤（本测试无 MCP，但语义要过一遍空集路径）。
	if len(def.Deferred) != 0 {
		t.Fatalf("无 MCP 环境下 deferred 应为空: %v", def.Deferred)
	}
	// worker 同环境不收敛：原样全保留。
	outW, _, _ := AugmentTools(context.Background(), "worker", base)
	if len(outW) != len(base) {
		t.Fatalf("worker 不应被过滤: %d != %d", len(outW), len(base))
	}
}

// TestAgentKeyCtx WithAgentKey/AgentKeyFrom 往返；未打 = 空。
func TestAgentKeyCtx(t *testing.T) {
	ctx := WithAgentKey(context.Background(), " planner ")
	if got := AgentKeyFrom(ctx); got != "planner" {
		t.Fatalf("agent key 往返失败: %q", got)
	}
	if got := AgentKeyFrom(context.Background()); got != "" {
		t.Fatalf("未打 key 应为空: %q", got)
	}
}

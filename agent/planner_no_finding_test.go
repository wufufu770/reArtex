package agent

import (
	"testing"

	actool "github.com/Autumn-27/norma/tool"
)

// v0.11.1 P0 回归钉：planner 工具面不得包含漏洞登记工具（report_finding）。
// 实证根因：planner 无 SoD 闸、此前也未接 findingValidator ⇒ LLM 可在零执行的情况下
// 幻觉登记漏洞并宣布完成。发现一律走 add_hint 交 worker 验证登记。
// worker 面仍必须包含（执行侧登记是正当产出）——双向钉死，防单向回退。
func TestPlannerToolsNoFindingRegistration(t *testing.T) {
	has := func(tools []actool.CoreTool, name string) bool {
		for _, tl := range tools {
			if tl.Name() == name {
				return true
			}
		}
		return false
	}

	plannerTools := NewToolSet(nil, "planner").PlannerTools()
	if has(plannerTools, "report_finding") {
		t.Fatal("planner 工具面不应包含 report_finding（登记必须有真实执行支撑）")
	}
	for _, banned := range []string{"record_fact"} {
		if has(plannerTools, banned) {
			t.Fatalf("planner 工具面不应包含登记类工具 %s（规划者只规划）", banned)
		}
	}
	if !has(plannerTools, "add_intent") {
		t.Fatal("planner 的产出通道 add_intent 必须仍在")
	}

	if !has(NewToolSet(nil, "work#1").WorkerTools(), "report_finding") {
		t.Fatal("worker 工具面必须保留 report_finding（执行侧登记）")
	}
}

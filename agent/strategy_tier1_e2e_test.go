// 本文件钉住「策略工具不进 Tier 1」这条门禁，并且按 **Tier 1 清单的实际维护方式**
// 来断言 —— 而不是抄一份清单。
//
// 【Tier 1 清单是怎么被维护的】（见 agent/approval_policy.go 的文件头注释
// 与 agent/tier1_scope_gate_test.go）：
//
//	agent.Tier1ReadOnlyTools 是 Tier 1 的**唯一真相源**；Phase 0 的验收测试
//	TestTier1ReadOnlyToolsNoScopeLeak **从它派生**自己的用例表，两边必须一一对应：
//	  ① 清单里有、而那张表里没有 ⇒ 验收测试失败（有人加了工具却没做范围隔离）；
//	  ② 那张表里有、而清单里没有 ⇒ 验收测试失败（用例表退化成了第二份清单）。
//
// ⇒ 因此「把一个工具加进 Tier 1」的**唯一**合法路径是先补上它的范围隔离用例。
// 策略库是跨任务、跨租户的知识，读它不是「任务范围内的只读」⇒ 策略工具
// **不应有**范围隔离用例 ⇒ 策略工具进不了 Tier 1。
//
// 本文件断言的就是这条推理链的每一环，而不是「某个列表里没有某个名字」。
package agent_test

import (
	"testing"

	"github.com/Autumn-27/artex/agent"
)

// strategyLibraryToolKeys 是策略库（L4）面向 agent 的三个工具 key。
//
// 为什么在这里再写一遍而不是从 agent 包导出：agent 包刻意不导出这张表
// （它只在 approval_policy.go 的 init 守卫里内部使用）⇒ 这里是一份
// **断言用的清单**，不是第二份真相源 —— 下面的用例会把它与 Tier 1 清单对照，
// 任何一方多出名字都会红。
var strategyLibraryToolKeys = []string{"strategy_search", "strategy_get", "strategy_propose"}

// TestStrategyLibraryToolsAreNotTier1 钉住「策略工具不进 Tier 1」。
//
// 三层断言：
//  1. 三个 key 都不在 agent.Tier1ReadOnlyTools 里，IsTier1ReadOnlyTool 均为 false；
//  2. Tier 1 的范围隔离用例表里**没有**这三个 key —— 这正是「加进 Tier 1 就会
//     让 Phase 0 验收测试变红」的机制本身；
//  3. 用例表与清单仍然一一对应（验收契约本身没被绕开）。
func TestStrategyLibraryToolsAreNotTier1(t *testing.T) {
	// ① 清单层面。
	for _, key := range strategyLibraryToolKeys {
		if agent.IsTier1ReadOnlyTool(key) {
			t.Fatalf("策略工具 %q 进了 Tier 1：策略库跨任务、跨租户，读它不是「任务范围内的只读」", key)
		}
		for _, tier1 := range agent.Tier1ReadOnlyTools {
			if tier1 == key {
				t.Fatalf("策略工具 %q 出现在 Tier 1 清单里", key)
			}
		}
	}

	// ② 验收契约层面：范围隔离用例表里没有策略工具。
	//
	// 只需要用例表里的**键**（用来对照清单），不需要真跑那些用例 ⇒ 这里手工
	// 造一个只带标记字段的夹具，不起服务、不碰数据库。
	fx := &tier1Fixture{
		inHost:       "t1in-marker",
		outHost:      "t1out-marker",
		graphInHost:  "t1gin-marker",
		graphOutHost: "t1gout-marker",
		inFindingID:  1,
	}
	probes := tier1ScopeProbes(fx)
	for _, key := range strategyLibraryToolKeys {
		if _, ok := probes[key]; ok {
			t.Fatalf("策略工具 %q 有范围隔离用例 ⇒ 它可以被加进 Tier 1，"+
				"但策略库是跨任务知识，读它不是「任务范围内的只读」", key)
		}
	}

	// ③ 验收契约本身仍然成立（清单与用例表一一对应）。
	for key := range probes {
		if !agent.IsTier1ReadOnlyTool(key) {
			t.Fatalf("范围隔离用例 %q 不在 Tier 1 清单里：清单只有一处真相，用例表不能变成第二份", key)
		}
	}
	for _, key := range agent.Tier1ReadOnlyTools {
		if _, ok := probes[key]; !ok {
			t.Fatalf("Tier 1 工具 %q 没有范围隔离用例：加进 Tier 1 之前必须先证明它的读面范围隔离", key)
		}
	}
}

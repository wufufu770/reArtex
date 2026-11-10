package agent

import (
	"encoding/json"
	"testing"

	"github.com/Autumn-27/artex/db"
)

// advisoryAgentKeys 是五个顾问型 agent 的 key。
var advisoryAgentKeys = []string{"curator", "verifier", "redteam", "creative", "intent-normalizer"}

// seedAgentsByTool 把 seed 列表转成「工具 → 绑定它的 agent」的反查表。
func seedAgentsByTool(seeds []ToolSeed) map[string][]string {
	out := map[string][]string{}
	for _, s := range seeds {
		out[s.Key] = append([]string{}, s.Agents...)
	}
	return out
}

// TestAdvisoryAgentsHoldNoDeniedTool 保证四个顾问 agent 手里没有任何被策略硬拒绝
// 的工具。
//
// 这条不变量是「模型只提议，确定性层裁决」在工具层的落点：一个能调
// report_finding 的 curator 就是写入者，不是仲裁者。断言放在绑定路径的产出上，
// 谁改了策略让越权工具漏进来，测试立刻炸。
func TestAdvisoryAgentsHoldNoDeniedTool(t *testing.T) {
	seeds := BuiltinToolSeeds()
	for _, p := range db.AdvisoryAgentToolPolicies() {
		for _, s := range seeds {
			if !containsAgentKey(s.Agents, p.Agent) {
				continue
			}
			if p.Denies(s.Key, "builtin") {
				t.Errorf("顾问 agent %s 被绑上了被拒绝的工具 %s", p.Agent, s.Key)
			}
		}
	}
}

// TestVerifierHoldsNoWriteOrExecTool 保证 verifier 只读。
//
// 「写」在这里指改写平台共享状态机（finding / intent / scope / 流量绑定 / 目标 /
// 资产 / 任务 / 平台配置）。verifier 唯一允许写的通道是它自己的建议通道
// submit_verdict（写 verification_verdicts），那不是共享状态——判定仍由确定性层做。
func TestVerifierHoldsNoWriteOrExecTool(t *testing.T) {
	if _, ok := db.AdvisoryAgentToolPolicyFor("verifier"); !ok {
		t.Fatal("找不到 verifier 的工具绑定策略")
	}
	for _, s := range BuiltinToolSeeds() {
		if !containsAgentKey(s.Agents, "verifier") {
			continue
		}
		switch db.AdvisoryToolCapability(s.Key, "builtin") {
		case db.AdvisoryCapGraphWrite:
			t.Errorf("verifier 绑上了改写共享状态的工具 %s", s.Key)
		case db.AdvisoryCapExec:
			t.Errorf("verifier 绑上了可执行工具 %s", s.Key)
		}
	}
	// 除建议通道外，verifier 只该拿只读工具。
	for _, s := range BuiltinToolSeeds() {
		if !containsAgentKey(s.Agents, "verifier") {
			continue
		}
		if db.AdvisoryToolCapability(s.Key, "builtin") == db.AdvisoryCapAdvisoryWrite && s.Key != "submit_verdict" {
			t.Errorf("verifier 绑上了非自己的建议通道工具 %s", s.Key)
		}
	}
}

// TestAdvisoryAgentsCannotWriteSharedGraph 保证四个顾问 agent 碰不到「写进共享图」
// 的工具——也就是既有 agent 用来写 finding / intent / scope / 流量绑定的那几个。
//
// 这条断言是读侧隔离（Phase 0 的 scope 隔离）之外的写侧隔离：新 agent 不能通过
// 工具去改既有 agent 已经写好的图。
func TestAdvisoryAgentsCannotWriteSharedGraph(t *testing.T) {
	sharedGraphWriters := []string{
		"report_finding", "add_intent", "insert_assets", "record_fact",
		"add_task_scope", "add_company_scope", "bind_finding_traffic",
		"update_finding_report", "add_hint", "add_task_hint",
		"set_goals", "set_constraints", "kill_work", "steer_work",
		"spawn_task", "pause_task", "delete_assets_by_host",
	}
	byTool := seedAgentsByTool(BuiltinToolSeeds())
	for _, key := range sharedGraphWriters {
		for _, agent := range byTool[key] {
			for _, advisory := range advisoryAgentKeys {
				if agent == advisory {
					t.Errorf("顾问 agent %s 绑上了写共享图的工具 %s", advisory, key)
				}
			}
		}
	}
}

// TestExistingAgentToolBindingsUnchanged 钉住既有 agent 的工具绑定。
//
// 顾问 agent 的绑定是【纯追加】：既有 agent 在同一个 agents 数组里的条目一个都
// 不能动。钉住代表性工具的完整数组，任何对既有绑动的改动都会在这里暴露。
//
// recon（资产收集 agent，方案 v4 §3-C）是有意识新增的内置 agent：它只在
// list_findings / insert_assets / list_assets / record_fact / list_companies 的数组
// 尾部追加了自己，既有 agent 的条目一个没动。
func TestExistingAgentToolBindingsUnchanged(t *testing.T) {
	want := map[string][]string{
		"graph_overview":           {"mainagent", "planner", "creative"},
		"list_findings":            {"mainagent", "planner", "worker", "pentest", "recon", "verifier", "redteam"},
		"list_facts":               {"mainagent", "planner"},
		"node_detail":              {"mainagent", "planner", "worker"},
		"expand_digest":            {"mainagent", "planner"},
		"get_worker_output":        {"mainagent", "planner"},
		"get_worker_trace":         {"mainagent", "planner", "worker"},
		"search_all_worker_traces": {"mainagent", "planner", "worker"},
		"add_hint":                 {"mainagent"},
		"add_intent":               {"mainagent", "planner"},
		"steer_work":               {"mainagent", "planner"},
		"set_goals":                {"mainagent", "goals"},
		"set_constraints":          {"mainagent", "goals"},
		"insert_assets":            {"mainagent", "worker", "auto", "pentest", "recon"},
		"add_company_scope":        {"mainagent", "planner", "auto"},
		"list_assets":              {"mainagent", "planner", "worker", "auto", "pentest", "recon", "creative"},
		"report_finding":           {"mainagent", "planner", "worker", "auto", "pentest"},
		"record_fact":              {"mainagent", "worker", "recon"},
		"add_task_scope":           {"mainagent", "planner", "worker"}, // E1-5：恢复 worker 横向扩权执行端（仍走 ask+预算）
		"list_untested_assets":     {"mainagent", "planner"},
		"list_goals":               {"planner"},
		"prove_goal":               {"planner"},
		"goal_met":                 {},
		"kill_work":                {"planner"},
		"list_companies":           {"planner", "auto", "pentest", "recon"},
	}
	byTool := seedAgentsByTool(BuiltinToolSeeds())
	for key, agents := range want {
		got, ok := byTool[key]
		if !ok {
			t.Errorf("工具 %s 不在 seed 目录里", key)
			continue
		}
		if !sameAgentSet(got, agents) {
			t.Errorf("工具 %s 的绑定变了：got %v want %v", key, got, agents)
		}
	}
}

// TestAdvisoryBindingsAreAdditive 保证顾问 agent 的绑定是【追加】在既有条目之后：
// 数组里既有 agent 的相对顺序不变，顾问 agent 只出现在尾部。
func TestAdvisoryBindingsAreAdditive(t *testing.T) {
	legacy := map[string]bool{"mainagent": true, "goals": true, "planner": true, "worker": true, "auto": true, "pentest": true, "recon": true}
	for _, s := range BuiltinToolSeeds() {
		lastLegacy, firstAdvisory := -1, -1
		for i, a := range s.Agents {
			if legacy[a] {
				lastLegacy = i
				continue
			}
			if firstAdvisory < 0 {
				firstAdvisory = i
			}
		}
		if lastLegacy >= 0 && firstAdvisory >= 0 && lastLegacy > firstAdvisory {
			t.Errorf("工具 %s：顾问 agent 插到了既有 agent 前面（%v）", s.Key, s.Agents)
		}
	}
}

// TestAdvisorySeedsAreJSONSerializable 保证 seed 记录能原样落进 tools 表的
// jsonb 列（agents 是字符串数组）。
func TestAdvisorySeedsAreJSONSerializable(t *testing.T) {
	for _, s := range BuiltinToolSeeds() {
		b, err := json.Marshal(s.Agents)
		if err != nil {
			t.Fatalf("工具 %s 的 agents 序列化失败: %v", s.Key, err)
		}
		var out []string
		if err := json.Unmarshal(b, &out); err != nil {
			t.Fatalf("工具 %s 的 agents 反序列化失败: %v", s.Key, err)
		}
		if len(out) != len(s.Agents) {
			t.Fatalf("工具 %s 的 agents 往返后长度变了", s.Key)
		}
	}
}

// sameAgentSet 比较两个 agent key 数组（顺序无关，重复即不等）。
func sameAgentSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := map[string]int{}
	for _, k := range a {
		seen[k]++
	}
	for _, k := range b {
		seen[k]--
		if seen[k] < 0 {
			return false
		}
	}
	return true
}

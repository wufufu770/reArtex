package agent

// 本文件是【自动权限分层】里 Tier 1 判定的唯一真相源。
//
// 分层模型（见 /home/analyst/.omo/plans/2026-10-29-auto-permission-tiers.md §1）：
//
//	Tier 1 —— 安全只读工具，自动通过（不消耗分类器调用）
//	Tier 2 —— 范围内的结构化写，条件自动通过
//	Tier 3 —— 高风险 / 范围外 / 裁决写
//
// 为什么 Tier 1 的清单必须是【代码常量】而不是 DB 行或配置：DB 行可以被删、
// settings 可以被持有 JWT 的人热改 —— 与 guard/shortcircuit.go 对拒绝名单的同一条
// 理由。清单一旦可被数据改写，「自动通过」就变成了「谁能把一个工具写进清单，谁就能
// 拿到免审」。
//
// 为什么必须【单一】：后面阶段的分层判定（TierOf / 准入校验）要读同一份清单。
// 清单若在「判定处」与「验收处」各存一份，两者就会漂移 —— 清单说安全、读面其实没
// 隔离时，自动通过就等于自动放行泄露。
//
// 硬规则：把一个工具加进这份清单之前，它的读面范围隔离必须已经被
// TestTier1ReadOnlyToolsNoScopeLeak 证明（agent/tier1_scope_gate_test.go）。
// 那道验收测试的清单【从本常量派生】，所以这里多写一个工具、而读面还没隔离，测试立刻
// 变红 ⇒ Tier 1 无法被启用。这正是「Tier 1 的启用绑定读面隔离验收测试」这条硬要求的
// 落地方式。
//
// 工具 key 与平台注册处一一对应（key 即 CoreTool.Name()，与 handler 死绑，DB 改不了）：
//
//	list_assets           agent/tools_insert.go   (*ToolSet).listAssets
//	list_untested_assets  agent/tools_insert.go   (*ToolSet).listUntestedAssets
//	list_findings         agent/tools.go           (*ToolSet).listFindings
//	graph_overview        agent/tools.go           (*ToolSet).graphOverview
//	get_finding_traffic   server/finding_traffic.go (*Server).toolGetFindingTraffic
//
// 注意 get_finding_traffic 由 server 包注册（它是宿主侧工具，不在 agent 的 ToolSet
// 里），但它同样受 agent 的分层判定约束 —— 所以清单放在 agent 包：server → agent 是
// 既有依赖方向，反过来会成导入环。
var Tier1ReadOnlyTools = []string{
	"list_assets",
	"list_findings",
	"get_finding_traffic",
	"graph_overview",
	"list_untested_assets",
}

// IsTier1ReadOnlyTool 报告某个工具 key 是否属于 Tier 1（安全只读，自动通过）。
//
// 分层判定代码读的是这一份清单，不是另抄一份 —— 见本文件的「为什么必须单一」。
func IsTier1ReadOnlyTool(key string) bool {
	for _, k := range Tier1ReadOnlyTools {
		if k == key {
			return true
		}
	}
	return false
}

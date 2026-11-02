package agent

import (
	"encoding/json"
	"testing"

	"github.com/Autumn-27/artex/mcphttp"
	actool "github.com/Autumn-27/norma/tool"
)

// 本文件钉住【自动权限分层判定器】的行为。
//
// 钉的是设计文档 §1 的逐行映射（.omo/plans/2026-10-29-auto-permission-tiers.md）：
// 每个动作都必须落在文档说的那一层，【包括文档点名的「明确不含」】——
// 改 intent owner 的意图写不是 Tier 2，状态迁出 open 的意图写也不是 Tier 2。
//
// 这些用例【不需要数据库】：判定器是纯函数（不查库、不调模型、不出网），
// 依赖库才有资格被穷举单测 —— 一旦判定里混进一次查库，「几百个组合一次钉死」就
// 变成「几百个组合每次都可能不同」。

// tierInScope 是「调用方判定：引用的资产都在有效 scope 内」的结论。
func tierInScope() ScopeVerdict { return ScopeVerdict{AssetInScope: true, Assessed: true} }

// tierOutOfScope 是「调用方判定：引用的资产不在有效 scope 内」的结论。
func tierOutOfScope() ScopeVerdict { return ScopeVerdict{AssetInScope: false, Assessed: true} }

// wantTier 断言一次判定的层级。
func wantTier(t *testing.T, label string, got TierVerdict, want Tier) {
	t.Helper()
	if got.Tier != want {
		t.Fatalf("%s：判成 %v want %v（%s）", label, got.Tier, want, got.Reason)
	}
}

// wantCategory 断言一次判定的类别。
func wantCategory(t *testing.T, label string, got TierVerdict, want TierCategory) {
	t.Helper()
	if got.Category != want {
		t.Fatalf("%s：类别是 %q want %q（%s）", label, got.Category, want, got.Reason)
	}
}

// TestTierOfTier1ReadOnlyToolsAreTier1 pins that every key on the Tier 1 list is
// answered as Tier 1. The list is read from the single source (approval_policy.go),
// so this test follows the list instead of copying it.
func TestTierOfTier1ReadOnlyToolsAreTier1(t *testing.T) {
	for _, key := range Tier1ReadOnlyTools {
		got := TierOf(key, nil)
		wantTier(t, "Tier 1 工具 "+key, got, Tier1)
		wantCategory(t, "Tier 1 工具 "+key, got, CategorySafeRead)
		if !got.AutoPass() {
			t.Fatalf("Tier 1 工具 %q 必须可自动放行", key)
		}
	}
}

// TestTierOfReadsTheSameTier1List proves the judge reads THAT list instead of a
// second copy: appending a key to the list must change the verdict immediately.
// A copied list would keep answering from the stale copy and this test would go red.
func TestTierOfReadsTheSameTier1List(t *testing.T) {
	original := Tier1ReadOnlyTools
	t.Cleanup(func() { Tier1ReadOnlyTools = original })
	Tier1ReadOnlyTools = append(append([]string{}, original...), "tier_probe_tool")

	wantTier(t, "临时加进 Tier 1 清单的工具", TierOf("tier_probe_tool", nil), Tier1)
}

// TestTierOfStrategyLibraryToolsAreNotTier1 pins that the strategy-library surface
// is never answered as Tier 1. approval_policy.go's init() panic guard covers the
// "someone put it in the list" failure mode; this pins the judge's own answer.
func TestTierOfStrategyLibraryToolsAreNotTier1(t *testing.T) {
	for _, key := range strategyLibraryToolKeys {
		got := TierOf(key, nil)
		if got.Tier == Tier1 {
			t.Fatalf("策略库工具 %q 被判成 Tier 1 —— 策略库跨任务、跨租户，自动放行等于放行范围外读取", key)
		}
		wantTier(t, "策略库工具 "+key, got, Tier3)
		if got.AutoPass() {
			t.Fatalf("策略库工具 %q 必须不可自动放行", key)
		}
	}
}

// TestTierOfReportFindingAnchoredInScopeIsTier2 covers design §1 Tier 2 ① and the
// §2.4 "先取证后报告" distinction: the tier depends on whether the anchored asset is
// IN scope, so the scope verdict has to come in as an argument.
func TestTierOfReportFindingAnchoredInScopeIsTier2(t *testing.T) {
	args := json.RawMessage(`{"vulnclass":"sqli","asset_ids":[42]}`)
	got := TierOfAction(TierAction{ToolKey: "report_finding", Args: args, Scope: tierInScope()})
	wantTier(t, "锚定 scope 内资产的 report_finding", got, Tier2)
	wantCategory(t, "锚定 scope 内资产的 report_finding", got, CategoryScopedWrite)
}

// TestTierOfReportFindingOutOfScopeAssetIsTier3 covers design §1 Tier 3 ①: the same
// tool with an out-of-scope asset id is Tier 3, not Tier 2.
func TestTierOfReportFindingOutOfScopeAssetIsTier3(t *testing.T) {
	args := json.RawMessage(`{"vulnclass":"sqli","asset_ids":[42]}`)
	got := TierOfAction(TierAction{ToolKey: "report_finding", Args: args, Scope: tierOutOfScope()})
	wantTier(t, "锚定 scope 外资产的 report_finding", got, Tier3)
	wantCategory(t, "锚定 scope 外资产的 report_finding", got, CategoryOutOfScope)
	if got.AutoPass() {
		t.Fatal("锚定 scope 外资产的漏洞登记不得自动放行")
	}
}

// TestTierOfReportFindingWithoutScopeVerdictIsTier3 pins fail-closed: "not judged"
// is not "in scope" (design §5 不制造虚假安全感).
func TestTierOfReportFindingWithoutScopeVerdictIsTier3(t *testing.T) {
	args := json.RawMessage(`{"asset_ids":[42]}`)
	wantTier(t, "没有 scope 判定结论的 report_finding", TierOfAction(TierAction{ToolKey: "report_finding", Args: args}), Tier3)
}

// TestTierOfReportFindingWithoutAssetIsTier2 covers the finding that anchors no asset:
// there is no out-of-scope asset to violate.
func TestTierOfReportFindingWithoutAssetIsTier2(t *testing.T) {
	wantTier(t, "未锚定资产的 report_finding", TierOf("report_finding", json.RawMessage(`{"vulnclass":"sqli"}`)), Tier2)
}

// TestTierOfIntentWriteIsTier2 covers design §1 Tier 2 ②.
func TestTierOfIntentWriteIsTier2(t *testing.T) {
	for _, key := range []string{"add_intent", "propose_intent"} {
		wantTier(t, key, TierOf(key, json.RawMessage(`{"summary":"挖登录"}`)), Tier2)
	}
}

// TestTierOfIntentWriteChangingOwnerIsTier3 covers design §1 Tier 2「明确不含：改变
// intent owner」. Both spellings are pinned: the explicit write semantics and the
// owner field carried in the arguments.
func TestTierOfIntentWriteChangingOwnerIsTier3(t *testing.T) {
	bySemantics := TierOfAction(TierAction{
		ToolKey: "add_intent",
		Args:    json.RawMessage(`{"summary":"挖登录"}`),
		Write:   WriteSemantics{IntentOwner: "worker-2"},
	})
	wantTier(t, "改归属的意图写（写语义）", bySemantics, Tier3)
	wantCategory(t, "改归属的意图写（写语义）", bySemantics, CategoryAdjudication)
	if !bySemantics.HostOnly {
		t.Fatalf("改归属必须走 host-only 写面（%s）", bySemantics.Reason)
	}
	byArgs := TierOf("add_intent", json.RawMessage(`{"summary":"挖登录","owner":"worker-2"}`))
	wantTier(t, "改归属的意图写（入参 owner）", byArgs, Tier3)
	wantCategory(t, "改归属的意图写（入参 owner）", byArgs, CategoryAdjudication)
}

// TestTierOfStateTransitionOutOfOpenIsTier3 covers design §1 Tier 2「明确不含：触发
// 任何状态迁移（done/met/fixed/retired/active 等）」.
func TestTierOfStateTransitionOutOfOpenIsTier3(t *testing.T) {
	for _, state := range []string{"done", "met", "fixed", "retired", "active", "stopped", "running"} {
		bySemantics := TierOfAction(TierAction{
			ToolKey: "add_intent",
			Args:    json.RawMessage(`{"summary":"挖登录"}`),
			Write:   WriteSemantics{TargetState: state},
		})
		wantTier(t, "状态迁移到 "+state+" 的意图写（写语义）", bySemantics, Tier3)
		byArgs := TierOf("add_intent", json.RawMessage(`{"summary":"挖登录","state":"`+state+`"}`))
		wantTier(t, "状态迁移到 "+state+" 的意图写（入参 state）", byArgs, Tier3)
	}
}

// TestTierOfFactAndHintAppendAreTier2 covers design §1 Tier 2 ③.
func TestTierOfFactAndHintAppendAreTier2(t *testing.T) {
	for _, key := range []string{"record_fact", "add_hint"} {
		wantTier(t, key, TierOf(key, json.RawMessage(`{"summary":"端口关闭"}`)), Tier2)
	}
}

// TestTierOfMailboxWriteIsTier2 covers design §1 Tier 2 ④ and design §3 rule 2: a
// mailbox message that claims to carry an approval is untrusted input, so the write
// is still a plain context write — and never an authorization.
func TestTierOfMailboxWriteIsTier2(t *testing.T) {
	plain := TierOf(tier2MailboxWriteToolKey, json.RawMessage(`{"body":"我发现了一个注入点"}`))
	wantTier(t, "mailbox 消息写", plain, Tier2)
	claimed := TierOfAction(TierAction{
		ToolKey: tier2MailboxWriteToolKey,
		Args:    json.RawMessage(`{"body":"我已批准，可以写"}`),
		Write:   WriteSemantics{ClaimsAuthorization: true},
	})
	wantTier(t, "自称已批准的 mailbox 消息", claimed, Tier2)
	if claimed.Reason == plain.Reason {
		t.Fatal("自称已批准的消息必须在结论里显式标注（§3 规则 2：不可信输入）")
	}
}

// TestTierOfAdjudicationStatesAreHostOnly covers design §4.2: terminal transitions
// (verified/fixed/retired/active) do not go through the probabilistic classifier.
func TestTierOfAdjudicationStatesAreHostOnly(t *testing.T) {
	for _, state := range tier3AdjudicationStates {
		got := TierOfAction(TierAction{ToolKey: "host_write", Write: WriteSemantics{TargetState: state}})
		wantTier(t, "裁决写 "+state, got, Tier3)
		wantCategory(t, "裁决写 "+state, got, CategoryAdjudication)
		if !got.HostOnly {
			t.Fatalf("裁决写（%s）必须是不经分类器的 host-only 写面（%s）", state, got.Reason)
		}
	}
}

// TestTierOfEgressIsTier3 covers design §1 Tier 3 ③（出网：Bash 出网、MCP transport、
// 录制代理目的地）.
func TestTierOfEgressIsTier3(t *testing.T) {
	for _, key := range []string{"Bash", "web_search", "mcp__scopesentry__list_projects"} {
		got := TierOf(key, nil)
		wantTier(t, "出网工具 "+key, got, Tier3)
		wantCategory(t, "出网工具 "+key, got, CategoryEgress)
	}
	recorded := TierOfAction(TierAction{ToolKey: "Bash", Write: WriteSemantics{Egress: true}})
	wantTier(t, "录制代理目的地", recorded, Tier3)
	wantCategory(t, "录制代理目的地", recorded, CategoryEgress)
}

// TestTierOfScopeExpansionIsTier3 covers design §1 Tier 3 ④（扩权请求本身）and
// §2.4 hard constraint 2.
func TestTierOfScopeExpansionIsTier3(t *testing.T) {
	for _, key := range []string{"add_task_scope", "add_company_scope", "insert_assets"} {
		got := TierOf(key, nil)
		wantTier(t, "扩权工具 "+key, got, Tier3)
		wantCategory(t, "扩权工具 "+key, got, CategoryScopeExpansion)
	}
	flagged := TierOfAction(TierAction{ToolKey: "host_write", Write: WriteSemantics{ScopeExpansion: true}})
	wantTier(t, "显式标记的扩权写", flagged, Tier3)
	wantCategory(t, "显式标记的扩权写", flagged, CategoryScopeExpansion)
}

// TestTierOfUnlistedToolFailsClosed pins the default: only proven-safe actions
// auto-pass (design §5).
func TestTierOfUnlistedToolFailsClosed(t *testing.T) {
	got := TierOf("some_tool_nobody_classified", nil)
	wantTier(t, "未分类的工具", got, Tier3)
	if got.AutoPass() {
		t.Fatal("未分类的工具不得自动放行")
	}
}

// TestTierOfIsDeterministicAndDependencyFree pins that the judge is a pure
// function: same input → same output, and no database is involved.
func TestTierOfIsDeterministicAndDependencyFree(t *testing.T) {
	action := TierAction{
		ToolKey: "report_finding",
		Args:    json.RawMessage(`{"asset_ids":[7]}`),
		Scope:   tierInScope(),
	}
	want := TierOfAction(action)
	wantTier(t, "基准动作", want, Tier2)
	for i := 0; i < 100; i++ {
		if got := TierOfAction(action); got != want {
			t.Fatalf("第 %d 次判定不一致：got %+v want %+v", i, got, want)
		}
	}
	// 判定不得依赖数据库：把 DSN 指到不可达地址，结论必须一模一样。
	t.Setenv("ARTEX_PG_DSN", "postgres://nobody:nobody@127.0.0.1:1/none?sslmode=disable")
	if got := TierOfAction(action); got != want {
		t.Fatalf("换掉 DSN 后判定变了：got %+v want %+v", got, want)
	}
	// 两个入口在同一入参下必须同判。
	if got, plain := TierOf(action.ToolKey, action.Args), TierOfAction(TierAction{ToolKey: action.ToolKey, Args: action.Args}); got != plain {
		t.Fatalf("TierOf 与 TierOfAction 不一致：got %+v want %+v", got, plain)
	}
	// TierOf 没有 scope 判定这一路输入 ⇒ 条件型动作落 Tier 3（fail closed）。
	wantTier(t, "没有 scope 判定的入口", TierOf(action.ToolKey, action.Args), Tier3)
}

// TestTierOfMatchesDesignDocSection1 is the exhaustive mapping of design §1: every
// row, including the two rows the doc lists as「明确不含」.
func TestTierOfMatchesDesignDocSection1(t *testing.T) {
	cases := []struct {
		docRow string
		action TierAction
		want   Tier
	}{
		// 字段顺序：设计 §1 的原文行 / 动作 / 期望层级。
		{"Tier 1 动作：list_assets / list_findings / get_finding_traffic / graph_overview / list_untested_assets", TierAction{ToolKey: "list_assets"}, Tier1},
		{"Tier 2 ① report_finding（锚定 scope 内资产）", TierAction{ToolKey: "report_finding", Args: json.RawMessage(`{"asset_ids":[1]}`), Scope: tierInScope()}, Tier2},
		{"Tier 2 ② 写 open 状态的 intent", TierAction{ToolKey: "add_intent", Args: json.RawMessage(`{"summary":"挖登录"}`)}, Tier2},
		{"Tier 2 ③ 追加 fact / hint", TierAction{ToolKey: "record_fact", Args: json.RawMessage(`{"summary":"端口关闭"}`)}, Tier2},
		{"Tier 2 ④ 写 mailbox 消息", TierAction{ToolKey: tier2MailboxWriteToolKey, Args: json.RawMessage(`{"body":"发现注入点"}`)}, Tier2},
		{"Tier 2 明确不含：改变 intent owner", TierAction{ToolKey: "add_intent", Write: WriteSemantics{IntentOwner: "worker-2"}}, Tier3},
		{"Tier 2 明确不含：触发任何状态迁移（done/met/fixed/retired/active 等）", TierAction{ToolKey: "add_intent", Write: WriteSemantics{TargetState: "met"}}, Tier3},
		{"Tier 3 ① 写范围外资产", TierAction{ToolKey: "report_finding", Args: json.RawMessage(`{"asset_ids":[1]}`), Scope: tierOutOfScope()}, Tier3},
		{"Tier 3 ② 改变 intent owner（裁决写）", TierAction{ToolKey: "add_intent", Write: WriteSemantics{IntentOwner: "worker-2"}}, Tier3},
		{"Tier 3 ② 把策略标 active（裁决写）", TierAction{ToolKey: "strategy_activate", Write: WriteSemantics{TargetState: "active"}}, Tier3},
		{"Tier 3 ② 把 finding 标 verified（裁决写）", TierAction{ToolKey: "finding_verify", Write: WriteSemantics{TargetState: "verified"}}, Tier3},
		{"Tier 3 ③ 出网操作（Bash 出网、MCP transport、录制代理目的地）", TierAction{ToolKey: "Bash"}, Tier3},
		{"Tier 3 ④ AddAgentScope / AddAutoScope 扩权请求本身", TierAction{ToolKey: "add_task_scope"}, Tier3},
	}
	for _, c := range cases {
		wantTier(t, "设计 §1「"+c.docRow+"」", TierOfAction(c.action), c.want)
	}
}

// TestTier2CandidateListMatchesDesignDocSection1 pins that the Tier 2 candidate
// list is exactly the four actions of design §1 — no more, no less.
func TestTier2CandidateListMatchesDesignDocSection1(t *testing.T) {
	want := []string{"report_finding", "add_intent", "propose_intent", "record_fact", "add_hint", "message_group"}
	if len(tier2WriteKinds) != len(want) {
		t.Fatalf("Tier 2 候选清单有 %d 条 want %d（%v）", len(tier2WriteKinds), len(want), tier2WriteKinds)
	}
	for _, key := range want {
		if _, ok := tier2WriteKinds[key]; !ok {
			t.Fatalf("Tier 2 候选清单缺 %q", key)
		}
		wantTier(t, "Tier 2 候选 "+key, TierOfAction(TierAction{ToolKey: key, Scope: tierInScope()}), Tier2)
	}
}

// TestTierOfMCPAndBashToolNamesMatchSDK pins the tool-key constants against the
// code that really builds those names: the MCP prefix and the SDK's Bash key.
func TestTierOfMCPAndBashToolNamesMatchSDK(t *testing.T) {
	if name := mcphttp.ToolName("scopesentry", "list_projects"); name != tier3MCPToolPrefix+"scopesentry__list_projects" {
		t.Fatalf("MCP 工具名 %q 与判定器的前缀 %q 对不上", name, tier3MCPToolPrefix)
	}
	wantTier(t, "SDK 拼出来的 MCP 工具名", TierOf(mcphttp.ToolName("scopesentry", "list_projects"), nil), Tier3)
	if name := actool.NewBash().Name(); name != "Bash" {
		t.Fatalf("SDK 的 Bash 工具名是 %q，判定清单里的 %q 漂移了", name, "Bash")
	}
	wantTier(t, "SDK 的 Bash 工具名", TierOf(actool.NewBash().Name(), nil), Tier3)
}

// TestTierOfEveryTier3CategoryIsReachable pins that every Tier 3 category the
// verdict can carry is actually producible — a category nobody can reach is a
// category that will never be reported.
func TestTierOfEveryTier3CategoryIsReachable(t *testing.T) {
	reachable := map[TierCategory]bool{}
	for _, action := range []TierAction{
		{ToolKey: "report_finding", Args: json.RawMessage(`{"asset_ids":[1]}`), Scope: tierOutOfScope()},
		{ToolKey: "add_intent", Write: WriteSemantics{IntentOwner: "worker-2"}},
		{ToolKey: "add_intent", Write: WriteSemantics{TargetState: "stopped"}},
		{ToolKey: "Bash"},
		{ToolKey: "add_task_scope"},
		{ToolKey: "some_tool_nobody_classified"},
	} {
		got := TierOfAction(action)
		wantTier(t, "Tier 3 取样 "+string(got.Category), got, Tier3)
		reachable[got.Category] = true
	}
	for _, cat := range []TierCategory{
		CategoryOutOfScope, CategoryAdjudication, CategoryStateChange,
		CategoryEgress, CategoryScopeExpansion, CategoryUnlisted,
	} {
		if !reachable[cat] {
			t.Fatalf("Tier 3 类别 %q 没有任何动作能产生它", cat)
		}
	}
}

// TestTierOfScopeInIsStampedUniformly 钉住结论里的 ScopeIn 口径。
//
// 计划（Phase 3 Task 6）的 TierOf 签名要求结论带 scopeIn ⇒ 审计留痕时能记下
// 「Tier 2 且在范围内」，而不必回头查调用方传进来的那个 scope 判定。
//
// 口径只有一条：调用方**真的判过**（Assessed）且**全部引用的资产都在范围内**
// （AssetInScope）⇒ true。「没判定过」⇒ false —— 把「不知道」当成「在范围内」
// 正是范围旁路本身。
func TestTierOfScopeInIsStampedUniformly(t *testing.T) {
	cases := []struct {
		name  string
		scope ScopeVerdict
		want  bool
	}{
		{"判过且全部在范围内", ScopeVerdict{Assessed: true, AssetInScope: true}, true},
		{"判过但不在范围内", ScopeVerdict{Assessed: true, AssetInScope: false}, false},
		{"没判定过", ScopeVerdict{Assessed: false, AssetInScope: false}, false},
		{"没判定过但 AssetInScope 为真（自相矛盾 ⇒ 按没判过处理）", ScopeVerdict{Assessed: false, AssetInScope: true}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := TierOfAction(TierAction{
				ToolKey: "report_finding",
				Args:    json.RawMessage(`{"asset_ids":[1],"summary":"x"}`),
				Scope:   c.scope,
			})
			if got.ScopeIn != c.want {
				t.Fatalf("ScopeIn = %v，期望 %v（结论 %+v）", got.ScopeIn, c.want, got)
			}
		})
	}
}

// TestTierOfScopeInOnTier2FindingAnchoredInScope 钉住「锚定 scope 内资产的漏洞
// 登记 ⇒ Tier 2 且 ScopeIn=true」这条最典型的路径。
func TestTierOfScopeInOnTier2FindingAnchoredInScope(t *testing.T) {
	got := TierOfAction(TierAction{
		ToolKey: "report_finding",
		Args:    json.RawMessage(`{"asset_ids":[1],"summary":"x"}`),
		Scope:   ScopeVerdict{Assessed: true, AssetInScope: true},
	})
	if got.Tier != Tier2 {
		t.Fatalf("层级 = %v，期望 Tier 2（结论 %+v）", got.Tier, got)
	}
	if !got.ScopeIn {
		t.Fatalf("ScopeIn = false，期望 true（结论 %+v）", got)
	}
}

// TestTierOfScopeInFalseWhenUnassessed 钉住「没做 scope 判定 ⇒ 不许报在范围内」。
//
// 这是本条最容易被误改的地方：一旦把口径改成「调用方说在就在」，没判定过的写就会
// 被记成「在范围内」⇒ 审计留痕失真。
func TestTierOfScopeInFalseWhenUnassessed(t *testing.T) {
	got := TierOf("report_finding", json.RawMessage(`{"asset_ids":[1],"summary":"x"}`))
	if got.ScopeIn {
		t.Fatalf("没做 scope 判定却报了 ScopeIn=true（结论 %+v）", got)
	}
}

package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// 本文件钉住【提议级的审查成本严格低于直接写入级】这条规则的**机制部分**。
//
// 这条规则此前只活在两句注释里：
//   - agent/approval_policy.go 的 StrategyProposalTools 注释（"审查成本必须严格低于"）；
//   - agent/strategy_tools.go 的文件头注释（"提议写不出 active"）。
// ⇒ 没有任何代码表达它，也没有任何测试钉住它 ⇒ 它随时可以被人改坏而无人变红。
//
// 本文件把它变成三件可执行、可断言的东西：
//  1. **分类**：分层判定器里「提议级」是一个可判定的类别（CategoryProposal），
//     而不是「Tier 2 里的一坨」；
//  2. **序**：结论上带一个【审查成本】序数（ReviewCost），「更轻」这句话因此
//     可以被断言，而不是靠嘴说；
//  3. **守卫**：策略提议的写入路径上真的有一道门（guardStrategyProposal），
//     它把 IsStrategyProposalTool 从「清单里的一个名字」变成真正被调用的判定。
//
// 口径来源：
//   - .omo/plans/2026-10-29-auto-permission-tiers.md §1（三层定义）、
//     §4.2（裁决写走 host-only 写面）；
//   - .omo/plans/2026-10-29-task-hall-phase6-implementation.md Task 6
//     （"把「提议级比直接写入级更轻」这件事在 strategy_propose 上落实 + 钉住"）。
//
// 【fail closed 的口径】提议级是【被证明】才成立的：证明不了 ⇒ Tier 3。
// 分层判定器上那个「只有 key 与入参」的入口（TierOf）拿不到写语义 ⇒ 它对
// strategy_propose 仍然给 Tier 3 —— 这不是漏洞，是「没证明就不轻判」。

// TestStrategyProposalGuardAcceptsPlainProposal 钉住守卫的正例。
//
// 一次干净的提议（只有名字、摘要、理由）⇒ 提议级结论：Tier 2、不带 host-only、
// 带 Proposal 标记。
func TestStrategyProposalGuardAcceptsPlainProposal(t *testing.T) {
	got := guardStrategyProposal(strategyProposalToolKey,
		json.RawMessage(`{"name":"n","summary":"s","reason":"r"}`))

	wantTier(t, "干净的策略提议", got, Tier2)
	wantCategory(t, "干净的策略提议", got, CategoryProposal)
	if !got.IsProposal() {
		t.Fatalf("提议级结论必须带 Proposal 标记：%+v", got)
	}
	if got.HostOnly {
		t.Fatalf("提议级不是裁决写，不得标记 host-only：%+v", got)
	}
	if got.ReviewCost() != ReviewProposal {
		t.Fatalf("提议级的审查成本 = %d，期望 %d：%+v", got.ReviewCost(), ReviewProposal, got)
	}
}

// TestStrategyProposalGuardRejectsAdjudicationState 钉住「提议写不出裁决态」。
//
// 裁决态（active / verified / fixed / retired）是裁决写 ⇒ 不经分类器、走
// host-only 写面（设计 §4.2）。提议一旦携带裁决态，它就不是提议级了。
func TestStrategyProposalGuardRejectsAdjudicationState(t *testing.T) {
	for _, args := range []string{
		`{"name":"n","summary":"s","state":"active"}`,
		`{"name":"n","summary":"s","new_state":"active"}`,
		`{"name":"n","summary":"s","target_state":"active"}`,
		`{"name":"n","summary":"s","state":"verified"}`,
	} {
		got := guardStrategyProposal(strategyProposalToolKey, json.RawMessage(args))
		wantTier(t, "试图写裁决态的提议 "+args, got, Tier3)
		if !got.HostOnly {
			t.Fatalf("写裁决态必须走 host-only 写面（%s）", got.Reason)
		}
	}
}

// TestStrategyProposalGuardRejectsStateFieldByPresence 钉住「字段存在即拒绝」。
//
// 判据是【字段存在】，不是「值是不是合法状态」：提议的语义就是「不声明任何
// 状态」—— 入参里出现一个状态字段（哪怕值是 null）就说明调用方在试图声明
// 状态，而不是在提议。
func TestStrategyProposalGuardRejectsStateFieldByPresence(t *testing.T) {
	got := guardStrategyProposal(strategyProposalToolKey, json.RawMessage(`{"name":"n","summary":"s","state":null}`))
	wantTier(t, "带空状态字段的提议", got, Tier3)
	if !strings.Contains(got.Reason, "状态") {
		t.Fatalf("拒绝理由必须点明是状态问题：%s", got.Reason)
	}
}

// TestStrategyProposalGuardRejectsOwnershipChange 钉住「提议不改归属」。
//
// 归属变更永远是裁决写（设计 §1 Tier 2 明确不含 / §4.2）。
func TestStrategyProposalGuardRejectsOwnershipChange(t *testing.T) {
	for _, args := range []string{
		`{"name":"n","summary":"s","owner":"someone"}`,
		`{"name":"n","summary":"s","intent_owner":"someone"}`,
		`{"name":"n","summary":"s","created_by":"someone"}`,
		`{"name":"n","summary":"s","decided_by":"someone"}`,
		`{"name":"n","summary":"s","promoted_by":"someone"}`,
	} {
		got := guardStrategyProposal(strategyProposalToolKey, json.RawMessage(args))
		wantTier(t, "试图指定归属的提议 "+args, got, Tier3)
		if !got.HostOnly {
			t.Fatalf("改归属必须走 host-only 写面（%s）", got.Reason)
		}
	}
}

// TestStrategyProposalGuardRejectsSelfApproval 钉住「agent 不能自证已裁决」。
//
// mailbox/提议这一层是【协商层】，不是【授权层】（设计 §3）：agent 说
// 「已批准」没有意义 —— 裁决权在 curator/operator 手里。
func TestStrategyProposalGuardRejectsSelfApproval(t *testing.T) {
	for _, args := range []string{
		`{"name":"n","summary":"s","status":"approved"}`,
		`{"name":"n","summary":"s","proposal_status":"approved"}`,
		`{"name":"n","summary":"s","decided_reason":"看着不错"}`,
	} {
		got := guardStrategyProposal(strategyProposalToolKey, json.RawMessage(args))
		wantTier(t, "自证裁决的提议 "+args, got, Tier3)
		if !got.HostOnly {
			t.Fatalf("自证裁决必须走 host-only 写面（%s）", got.Reason)
		}
	}
}

// TestStrategyProposalGuardRejectsNonProposalTool 钉住「守卫只对提议级工具生效」。
//
// 这条同时是 IsStrategyProposalTool 的**真实调用点**：不是提议级工具走这道门
// ⇒ 立刻被拒，而不是被当成一个普通的 Tier 2 写放过去。
func TestStrategyProposalGuardRejectsNonProposalTool(t *testing.T) {
	for _, key := range []string{"add_intent", "propose_intent", "strategy_search", "strategy_get"} {
		got := guardStrategyProposal(key, json.RawMessage(`{"name":"n","summary":"s"}`))
		wantTier(t, "非提议级工具 "+key, got, Tier3)
		if got.Category != CategoryUnlisted {
			t.Fatalf("非提议级工具的拒绝类别 = %q，期望 %q", got.Category, CategoryUnlisted)
		}
	}
}

// TestStrategyProposalGuardNeverLighterThanDirectWrite 钉住守卫的**序**：
// 提议级的审查成本严格低于同一工具的直接写路径。
//
// 直接写路径 = 同一个工具试图写裁决态（state='active'）⇒ Tier 3 裁决写。
func TestStrategyProposalGuardNeverLighterThanDirectWrite(t *testing.T) {
	proposal := guardStrategyProposal(strategyProposalToolKey, json.RawMessage(`{"name":"n","summary":"s"}`))
	direct := guardStrategyProposal(strategyProposalToolKey, json.RawMessage(`{"name":"n","summary":"s","state":"active"}`))

	if proposal.ReviewCost() >= direct.ReviewCost() {
		t.Fatalf("提议级的审查成本必须严格低于直接写：proposal=%d direct=%d",
			proposal.ReviewCost(), direct.ReviewCost())
	}
}

// TestStrategyProposalTierOfKeyOnlyEntryStaysTier3 钉住 fail closed 的那一半：
// **只拿到 key 与入参**时（TierOf），判定器拿不到写语义 ⇒ 证明不了这是隔离
// 提议 ⇒ 仍然 Tier 3。
//
// 为什么这条重要：TierOf 是「只有 key 与入参」的场景的入口（文件头注释），
// 而写语义只有副作用发生点才给得起 ⇒ 没给就是没证明 ⇒ 不轻判。
func TestStrategyProposalTierOfKeyOnlyEntryStaysTier3(t *testing.T) {
	got := TierOf(strategyProposalToolKey, json.RawMessage(`{"name":"n","summary":"s"}`))
	wantTier(t, "只有 key 与入参的策略提议", got, Tier3)
	if got.AutoPass() {
		t.Fatal("证明不了是隔离提议时不得自动放行")
	}
}

// TestStrategyProposalNonProposalToolCannotClaimProposal 钉住「任何工具都不能
// 靠自称提议拿到轻判」。
//
// 轻判只能由【提议级工具 + 隔离提议语义】共同换到；别的工具带上
// Write.Proposal=true ⇒ 不认。
func TestStrategyProposalNonProposalToolCannotClaimProposal(t *testing.T) {
	got := TierOfAction(TierAction{
		ToolKey: "add_intent",
		Args:    json.RawMessage(`{"summary":"挖登录"}`),
		Write:   WriteSemantics{Proposal: true},
	})
	wantTier(t, "自称提议的 add_intent", got, Tier3)
	if got.IsProposal() {
		t.Fatal("非提议级工具不得拿到提议级结论")
	}
}

// TestStrategyProposeToolRefusesStateArgument 钉住守卫真的接在写入路径上。
//
// 这条是端到端的：在守卫接上之前，strategy_propose 会**静默忽略**入参里的
// "state" 字段（它的入参结构里没有这个字段）⇒ 调用方以为自己在写状态、
// 实际上写的是一条隔离态记录 —— 一次「以为成功其实没生效」的静默降级。
// 守卫接上之后 ⇒ 明确拒绝，并且**一个字节都不写**。
//
// 断言落在【机器可判的信号】上：工具返回错误 + 库里没有记录。拒绝理由的措辞
// 不做断言（措辞会变，行为不会）。
func TestStrategyProposeToolRefusesStateArgument(t *testing.T) {
	fx := newStrategyToolFixture(t)
	sfx := time.Now().UnixNano()
	name := fmt.Sprintf("e2e-守卫-提议-%d", sfx)

	raw, err := json.Marshal(map[string]any{
		"name":    name,
		"summary": "e2e 守卫提议摘要",
		"state":   "active",
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := strategyToolByName(t, fx.tools, "strategy_propose").Call(context.Background(), raw, nil)
	if err != nil {
		t.Fatalf("strategy_propose 调用失败: %v", err)
	}
	if !res.IsError {
		t.Fatalf("带状态字段的提议必须被拒，实际返回: %s", res.Flatten())
	}
	if res.Flatten() == "" {
		t.Fatal("拒绝必须给出理由")
	}

	var n int
	if err := fx.db.QueryRow(`SELECT count(*) FROM strategies WHERE name=$1`, name).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("被拒的提议不得留下策略记录（实际 %d 条）", n)
	}
}

// TestStrategyProposeToolAcceptsPlainProposal 钉住守卫的正例端到端：
// 干净的提议照常通过（守卫不能把门焊死）。
func TestStrategyProposeToolAcceptsPlainProposal(t *testing.T) {
	fx := newStrategyToolFixture(t)
	sfx := time.Now().UnixNano()
	name := fmt.Sprintf("e2e-守卫-放行-%d", sfx)

	raw, err := json.Marshal(map[string]any{
		"name":    name,
		"summary": "e2e 守卫放行摘要",
		"reason":  "e2e 这条值得沉淀",
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := strategyToolByName(t, fx.tools, "strategy_propose").Call(context.Background(), raw, nil)
	if err != nil {
		t.Fatalf("strategy_propose 调用失败: %v", err)
	}
	if res.IsError {
		t.Fatalf("干净的提议不该被拒: %s", res.Flatten())
	}
}

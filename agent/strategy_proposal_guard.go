package agent

import (
	"encoding/json"
	"strconv"
	"strings"
)

// 本文件是【策略提议级】写入的守卫：把「提议级的审查成本严格低于直接写入级」
// 这条规则从两句注释变成【可执行、可断言】的机制。
//
// 为什么守卫必须落在【副作用发生点】（设计 §2.2）：分层判定器是纯函数，它看不到
// 这次写到底写了什么 —— 它只认调用方声明的写语义。「这次写真的只落在隔离态」
// 只有在写入路径上才判得了 ⇒ 守卫的职责是：
//  1. 把「这是提议级」这个声明连同入参一起送进判定器；
//  2. 把 Tier 3 的结论翻译成一句中文拒绝理由，并**挡住这次写**。
//
// 守卫顺带把 IsStrategyProposalTool 从「清单里的一个名字」变成【真正被调用的
// 判定】：非提议级工具走这道门 ⇒ 立刻被拒，而不是被当成一个普通的写放过去。

// strategyProposalToolKey 是策略提议工具的 key（= CoreTool.Name()，与 handler 死绑）。
const strategyProposalToolKey = "strategy_propose"

// guardStrategyProposal 校验一次策略提议写入满足「提议级」的全部不变量。
//
// 返回值是这次写入的【分层结论】：
//   - Tier 2 + CategoryProposal ⇒ 允许写入（提议级：不严于 Tier 2，且比同族的
//     直接写轻）；
//   - Tier 3 ⇒ 必须拒绝这次写入，Reason 是中文拒绝理由。
//
// 判据只有一条：**这次写是不是一个隔离提议**。是 ⇒ 轻判；不是 ⇒ Tier 3
// （fail closed：判不出来就不是提议级）。
//
// 提议级的不变量（缺一不可）：
//  1. 写入者是【提议级工具】（IsStrategyProposalTool）；
//  2. 只写【隔离态】记录：入参不得携带任何状态字段（state / new_state /
//     target_state）—— 提议写不出裁决态（设计 §4.2）；
//  3. 不改【归属】：入参不得携带 owner / created_by / decided_by / promoted_by；
//  4. 不自证【已裁决】：入参不得携带 status / proposal_status —— agent 说
//     「已批准」没有意义，裁决权在 curator / operator 手里（设计 §3 / §4.2）。
func guardStrategyProposal(toolKey string, args json.RawMessage) TierVerdict {
	if !IsStrategyProposalTool(toolKey) {
		return tier3Verdict(CategoryUnlisted,
			"工具 "+toolKey+" 不是提议级工具：提议级的不变量只对提议级工具成立")
	}
	return TierOfAction(TierAction{
		ToolKey: toolKey,
		Args:    args,
		Write:   WriteSemantics{Proposal: true},
	})
}

// judgeProposalWrite 判定一次【提议级】写入。
//
// 提议级的定义（approval_policy.go 的 StrategyProposalTools + 设计 §1）：提议只写
// **隔离态**记录 —— 不改任何 owner、不触发任何状态迁移、被裁决之前不影响任何
// agent 的行为。
//
// 为什么它可以比直接写更轻（这是 ReviewCost 序数的根据，不是修辞）：提议的写面
// **不落进共享图**，被裁决之前没有任何 agent 会读它 ⇒ 它的一致性校验面严格更小：
// 不需要 scope 判定（提议不写任何资产）、不需要归属判定（提议结构上不改归属）、
// 不需要状态判定（提议结构上不触发迁移）。直接写（add_intent）落进共享探索图、
// 立刻被规划者看见 ⇒ 必须过这三道判定。
//
// fail closed 的三处（判不出来就不是提议级）：
//  1. 不是提议级工具却自称提议 ⇒ 不认；
//  2. 提议携带归属变更 ⇒ 裁决写（host-only）；
//  3. 提议携带任何状态（裁决态或其它迁移）⇒ 状态迁移 / 裁决写。
func judgeProposalWrite(a TierAction) TierVerdict {
	// ① 轻判是【资格】，资格由工具清单授予，不由入参自称。
	if !IsStrategyProposalTool(a.ToolKey) {
		return tier3Verdict(CategoryUnlisted,
			"工具 "+a.ToolKey+" 不是提议级工具：提议级不变量只对提议级工具成立（fail closed）")
	}
	// ② 写语义：归属 / 状态 / 出网 / 扩权，逐条与直接写路径同款 —— 提议不能靠
	//    「我是提议」绕过任何一条硬 Tier 3 规则。
	if a.Write.IntentOwner != "" {
		return tier3HostOnlyVerdict("提议级写入顺带改归属（owner=" + a.Write.IntentOwner +
			"）⇒ 裁决写，走 host-only 写面（设计 §1 Tier 2 明确不含 / §4.2）")
	}
	if st := a.Write.TargetState; st != "" {
		return tier3StateVerdict(st)
	}
	if a.Write.Egress {
		return tier3Verdict(CategoryEgress, "出网（数据外带）⇒ Tier 3（设计 §4.1：出网治理不完整，必须显式标注）")
	}
	if a.Write.ScopeExpansion {
		return tier3Verdict(CategoryScopeExpansion,
			"扩权请求本身 ⇒ Tier 3，分类器审 + 留痕（设计 §1 Tier 3 ④ / §2.4 硬约束 2）")
	}
	// ③ 入参层：同一组不变量。字段【存在】即拒绝（值是不是合法状态不重要 ——
	//    提议的入参里根本不该有这些字段）。
	if owners := argsPresentKeys(a.Args, proposalOwnershipArgs...); len(owners) > 0 {
		return tier3HostOnlyVerdict("提议级写入试图指定归属（" + strings.Join(owners, "、") +
			"）⇒ 裁决写，走 host-only 写面（设计 §1 Tier 2 明确不含 / §4.2）")
	}
	if statuses := argsPresentKeys(a.Args, proposalAdjudicationArgs...); len(statuses) > 0 {
		return tier3HostOnlyVerdict("提议级写入试图自证裁决（" + strings.Join(statuses, "、") +
			"）⇒ 裁决权在宿主侧（curator / operator），agent 说「已批准」没有意义（设计 §4.2）")
	}
	if states := argsPresentKeys(a.Args, proposalStateArgs...); len(states) > 0 {
		st := argsString(a.Args, proposalStateArgs...)
		if st != "" && containsString(tier3AdjudicationStates, st) {
			return tier3HostOnlyVerdict("提议级写入试图指定裁决态（" + strings.Join(states, "、") + "=" +
				strconv.Quote(st) + "）⇒ 裁决写，走 host-only 写面（设计 §1 Tier 2 明确不含 / §4.2）")
		}
		return tier3Verdict(CategoryStateChange,
			"提议级写入试图指定状态（"+strings.Join(states, "、")+
				"）⇒ 提议只写隔离态记录，不触发任何状态迁移（设计 §1 Tier 2 明确不含）")
	}
	// ④ 提议级结论：不严于 Tier 2，且比同族的直接写轻。
	return TierVerdict{Tier: Tier2, Category: CategoryProposal,
		Reason: "提议级写入：只写隔离态记录，不改归属、不触发状态迁移、被裁决前不影响任何 agent ⇒ " +
			"审查成本严格低于同族的直接写（设计 §1 Tier 2 / §4.2）"}
}

package agent

import (
	"encoding/json"
	"strconv"
	"strings"
)

// 本文件是【自动权限分层】的判定器：给定「一次动作」，回答它落在哪一层。
//
// 真相源（只读，本文件不改它）：
//   - .omo/plans/2026-10-29-auto-permission-tiers.md §1（三层定义）、§2（Tier 2
//     的一致性校验）、§4（出网与裁决写）
//   - agent/approval_policy.go —— 分层清单的【唯一】真相源（Tier 1 + Tier 2/3 清单都在
//     那里）。本文件【读它】，不另抄一份：抄一份就会出现「清单说安全、判定读另一份」
//     这种无人变红的漂移。
//
// 本文件【只判定，不放行】：放行由宿主侧（server/db 的副作用发生点）消费。
// 判定必须发生在副作用发生点（设计 §2.2/§2.3）：只在工具面判，Bash curl 调平台 API、
// MCP 工具、DB 直连这三条旁路就成立。
//
// 【纯函数】：不查库、不调模型、不出网、不读任何可变全局状态。三个理由：
//  1. 同一个动作在工具面与平台 API 面必须【同判】（设计 §2.3 的跨入口一致性要求）；
//     两处各查一次库就有两次不一致的机会。
//  2. 分类器【不能】读 DB 里 agent 可写的 task_scope 行 —— agent 自己写一行就能让校验
//     通过（自证合法），强来源校验形同虚设（设计 §2.4 实现红线）。所以「这个资产在不在
//     有效 scope 内」必须由调用方作为【入参】给，而不是分类器自己去查。
//  3. 纯函数才能被穷举单测：几百个组合一次性钉死，不依赖库、不依赖模型、不依赖网。
//
// allow: SIZE_OK —— 分层模型（Tier / TierCategory / ScopeVerdict / WriteSemantics /
// TierAction / TierVerdict）与它的判定分支必须待在一起：把结论类型与判定它的
// 判定器拆开，会让「结论长什么样」与「结论怎么来的」分居两个文件 ⇒ 读判定器
// 的人拿不到完整口径。与 db/quota.go 同一条理由（状态常量与预算/周期/预扣/
// 结算放在一个文件里）。

// Tier 是自动权限分层的层级。
//
// 语义见设计 §1：
//
//	Tier 1 —— 安全只读，自动通过（不消耗分类器调用）
//	Tier 2 —— scope 内结构化写，条件自动通过
//	Tier 3 —— 高风险 / scope 外 / 裁决写，分类器审查（建议 + 阻断）或直接拒绝
type Tier int

const (
	// TierUnknown 是判定不出来的占位值。按 fail-closed 处理（既不是 Tier1 也不是 Tier2）。
	TierUnknown Tier = 0
	// Tier1 是安全只读层：自动通过，不消耗分类器调用。
	Tier1 Tier = 1
	// Tier2 是 scope 内结构化写层：条件自动通过。
	Tier2 Tier = 2
	// Tier3 是高风险 / scope 外 / 裁决写层：分类器审查或直接拒绝。
	Tier3 Tier = 3
)

// String 给出层级的稳定短名（审计日志与 UI 直接用）。
func (t Tier) String() string {
	switch t {
	case Tier1:
		return "Tier1"
	case Tier2:
		return "Tier2"
	case Tier3:
		return "Tier3"
	default:
		return "TierUnknown"
	}
}

// AutoPassable 报告该层级是否可能走自动放行。
//
// 只有 Tier 1 / Tier 2 可以；Tier 3 一律不行 —— 裁决写连分类器都不该接手（设计 §4.2）。
func (t Tier) AutoPassable() bool { return t == Tier1 || t == Tier2 }

// TierCategory 是判定结论的【类别】：同一层里也分轻重。
//
// 为什么层级之外还要一个类别：Tier 3 里既有「出网（数据外带）」也有「没拿到自动放行
// 白名单」—— 两者的处置完全不同（前者必须有人看，后者只是没进白名单）。只回一个层号，
// 宿主侧只能一律当「高风险」处理，日志也没法区分「出网被拦」与「写了个新工具」。
type TierCategory string

const (
	// CategorySafeRead 是 Tier 1：安全只读。
	CategorySafeRead TierCategory = "safe_read"
	// CategoryScopedWrite 是 Tier 2：scope 内结构化写。
	CategoryScopedWrite TierCategory = "scoped_write"
	// CategoryOutOfScope 是 Tier 3：写范围外的资产。
	CategoryOutOfScope TierCategory = "out_of_scope"
	// CategoryAdjudication 是 Tier 3：裁决写（host-only 写面，不经分类器）。
	CategoryAdjudication TierCategory = "adjudication"
	// CategoryStateChange 是 Tier 3：状态迁移（不是终态，但仍不属于 Tier 2）。
	CategoryStateChange TierCategory = "state_change"
	// CategoryEgress 是 Tier 3：出网（数据外带）。
	CategoryEgress TierCategory = "egress"
	// CategoryScopeExpansion 是 Tier 3：扩权请求本身。
	CategoryScopeExpansion TierCategory = "scope_expansion"
	// CategoryProposal 是 Tier 2 的一种：提议级写入（隔离态记录，不影响他人行为）。
	CategoryProposal TierCategory = "proposal"
	// CategoryUnlisted 是 Tier 3：不在自动放行清单里（fail closed）。
	CategoryUnlisted TierCategory = "unlisted"
)

// ReviewCost 是这次动作的【审查成本】序数。数字越小越轻。
//
// 为什么层级之外还要它：Tier 只说「哪一层」，不说「这一层里谁更轻」。而
// 「提议级的审查成本严格低于直接写入级」这条规则要求的正是【层内的轻重序】
// —— 提议与直接写可以落在同一层（都是 Tier 2），但它们的审查成本不同。
type ReviewCost int

const (
	// ReviewAutoPass：自动通过，不调分类器（Tier 1）。
	ReviewAutoPass ReviewCost = 1
	// ReviewProposal：提议级的一致性校验（Tier 2 的提议类）。
	ReviewProposal ReviewCost = 2
	// ReviewScopedWrite：scope 内结构化写的一致性校验（Tier 2 的直接写类）。
	ReviewScopedWrite ReviewCost = 3
	// ReviewClassifier：分类器审查（Tier 3 的非裁决类）。
	ReviewClassifier ReviewCost = 4
	// ReviewAdjudication：裁决写，不经分类器，走 host-only 写面（Tier 3 的裁决类）。
	ReviewAdjudication ReviewCost = 5
)

// ReviewCost 返回这次动作的审查成本。
//
// 口径（从轻到重）：
//   - 谁都不用看 ⇒ ReviewAutoPass；
//   - 一个确定性守卫 + 写面落在隔离记录里（被裁决前没人读它）⇒ ReviewProposal；
//   - 一个确定性守卫 + 写面落在共享图里（别的 agent 立刻会看见）⇒ ReviewScopedWrite；
//   - 交给概率性分类器 ⇒ ReviewClassifier；
//   - 交给宿主侧写面 / 人 ⇒ ReviewAdjudication（最重：设计 §4.2 说分类器在
//     Tier 3 仍有 70.3% 假阴性率 ⇒ 高后果决策不能只靠它）。
//
// 判不出来（TierUnknown）落在 ReviewClassifier —— 宁可算重，不许算轻。
func (v TierVerdict) ReviewCost() ReviewCost {
	switch {
	case v.HostOnly:
		return ReviewAdjudication
	case v.Tier == Tier1:
		return ReviewAutoPass
	case v.Tier == Tier2 && v.Category == CategoryProposal:
		return ReviewProposal
	case v.Tier == Tier2:
		return ReviewScopedWrite
	default:
		return ReviewClassifier
	}
}

// IsProposal 报告这次动作是不是【提议级】写入。
//
// 判据是结论的类别（单一真相源）而不是另开一个布尔位 —— 两处判据迟早会漂移。
func (v TierVerdict) IsProposal() bool { return v.Category == CategoryProposal }

// ScopeVerdict 是【调用方】给出的 scope 判定结论。
//
// 为什么它是入参而不是分类器自己查（设计 §2.2 第 3 条 / §2.4 实现红线）：
//   - 判定发生在副作用发生点时，那里已经有「这个资产在不在本任务有效 scope 内」的结论；
//     分类器再查一次 = 两处判定 = 两处可能不一致。
//   - 分类器若去读 DB 里 agent【可写】的 task_scope 行，agent 自己写一行就能让校验通过
//     （自证合法），强来源校验形同虚设。真正的有效视图必须由访问层强制注入 scope 谓词。
type ScopeVerdict struct {
	// AssetInScope：这次写引用的【全部】资产是否都落在本任务的有效 scope 内。
	// 多个资产时，调用方必须对【每一个】都判过 —— 「有一个不在」就是不在。
	AssetInScope bool
	// Assessed：调用方是否真的做过这次判定。false = 没判定过。
	//
	// 为什么要有这个位：判定「不在 scope 内」与「没判定过」不是一回事，但两者的处置
	// 一样 —— 都不能自动放行（fail closed，设计 §5 不制造虚假安全感）。
	Assessed bool
}

// WriteSemantics 描述这次写【将会产生什么语义效果】。
//
// 为什么由调用方填而不是分类器猜：归属变更与状态迁移的判据在【副作用发生点】最准确
// （那里才知道这次写到底动了 owner 还是动了 state）。让分类器从入参里「猜」这是不是
// 一次裁决写 = 把语义判断塞进一个本该只做查表的函数。
type WriteSemantics struct {
	// IntentOwner 非空 = 这次写会改变 intent 的归属（ClaimIntent 的 owner）。
	IntentOwner string
	// TargetState 非空 = 这次写会把节点迁到某个状态。
	// 'open' 表示「新建一条开放的意图」（Tier 2 允许）；其它值都是状态迁移。
	TargetState string
	// ScopeExpansion = 这次写是一次扩权请求（AddAgentScope / AddAutoScope）。
	ScopeExpansion bool
	// Egress = 这次动作会出网（数据外带）。录制代理目的地、MCP transport 走这个位。
	Egress bool
	// ClaimsAuthorization = 这次 mailbox 消息自称携带「已批准」。
	ClaimsAuthorization bool
	// Proposal = 这次写是一条【提议级】记录：隔离态、不改 owner、不触发任何状态
	// 迁移、被裁决之前不影响任何 agent 的行为。
	//
	// 为什么由调用方声明而不是判定器猜：判定器是纯函数，它看不到这次写到底写了
	// 什么（那是副作用发生点才知道的事）—— 「这次写真的是个隔离提议」只能由
	// 副作用发生点声明。没声明 ⇒ 不是提议级 ⇒ Tier 3（fail closed）。
	Proposal bool
}

// TierAction 是一次待分层动作的【全部输入】。
type TierAction struct {
	// ToolKey 是工具 key（= CoreTool.Name()，与 handler 死绑，DB 改不了）。
	ToolKey string
	// Args 是工具入参的原始 JSON（可空）。分类器只读【判定所需的那几个字段】。
	Args json.RawMessage
	// Scope 是调用方给出的 scope 判定结论。
	Scope ScopeVerdict
	// Write 是这次写的语义效果。
	Write WriteSemantics
}

// TierVerdict 是一次分层的结论。
type TierVerdict struct {
	// Tier 是落在哪一层。
	Tier Tier
	// Category 是结论类别。
	Category TierCategory
	// HostOnly 为真时：这次写【不经分类器】，只允许发生在宿主侧写面（设计 §4.2：
	// 分类器不能作为高后果决策的唯一防线）。
	HostOnly bool
	// ScopeIn：这次写引用的资产**全部**落在本任务有效 scope 内。
	//
	// 为什么结论里要带它（计划的 TierOf 签名也要求这个量）：调用方拿到的结论
	// 必须能回答「这次写是不是落在授权范围内」，而不必回头去查它自己传进来的那个
	// scope 判定 —— 尤其在审计留痕时：只记「Tier 2」不够，得记「Tier 2 且在范围内」。
	//
	// 口径：**只有真的判过（Assessed）且全部在范围内（AssetInScope）才为 true**。
	// 「没判定过」⇒ false（fail closed）—— 不许把「不知道」当成「在范围内」。
	ScopeIn bool
	// Reason 是一句话理由（中文），审计与 UI 直接用。
	Reason string
}

// AutoPass 报告这次动作是否可以走自动放行。
func (v TierVerdict) AutoPass() bool { return v.Tier.AutoPassable() }

// tier3Verdict 造一个 Tier 3 结论（非裁决写）。
func tier3Verdict(cat TierCategory, reason string) TierVerdict {
	return TierVerdict{Tier: Tier3, Category: cat, Reason: reason}
}

// tier3HostOnlyVerdict 造一个【裁决写】结论：不经分类器，只走宿主侧写面（设计 §4.2）。
func tier3HostOnlyVerdict(reason string) TierVerdict {
	return TierVerdict{Tier: Tier3, Category: CategoryAdjudication, HostOnly: true, Reason: reason}
}

// tier2Verdict 造一个 Tier 2 结论。
func tier2Verdict(reason string) TierVerdict {
	return TierVerdict{Tier: Tier2, Category: CategoryScopedWrite, Reason: reason}
}

// ---------------------------------------------------------------------------
// 判定器
// ---------------------------------------------------------------------------

// TierOf 按工具 key + 入参判定一次动作的层级。
//
// 这是「只有 key 与入参」的场景的入口。语义最全的入口是 TierOfAction。
//
// 条件没被证明满足就落到 Tier 3（fail closed）：report_finding 没有 scope 判定结论时
// 【不能】判 Tier 2 —— 「不在有效 scope 内」与「没判定过」都按不放行处理。
//
// ⚠️ 返回类型与计划原文（Phase 3 Task 6：`func TierOf(toolKey string, args []byte)
// (tier int, scopeIn bool)`）**刻意不同**，不是随手写的：
//
// 计划的签名只带「层级 + scope 判定结论」两项 ⇒ 它**无法表达「裁决写」**（改
// intent owner、把策略标 active、把 finding 标 verified）。而 auto-permission-tiers
// §4.2 对裁决写的处置是「**不经分类器**，走 host-only 写面」⇒ 结论里必须能带上
// 「这是裁决写、必须走 host-only」这层信息。
//
// 若按计划签名实现 ⇒ 裁决写会被压成「Tier 3 + scope 判定结论」⇒ 调用方无从
// 区分「普通 Tier 3 出网操作」（走分类器审查）与「裁决写」（**绝不能**走分类器，
// 因为 Anthropic 生产流量 FNR 17%、独立压力测试 Tier 3 仍 70.3% FNR ⇒ 分类器不能
// 作为高后果决策的唯一防线）⇒ 裁决写会被送进概率性分类器 ⇒ 正是设计要防的事。
//
// ⇒ 这里返回 TierVerdict（多带 Category / HostOnly / Reason），并保留
// TierOfAction 作为最全入口（可显式传入写语义）。需要计划那两个标量时用
// TierVerdict.Tier 与 TierVerdict.ScopeIn。
func TierOf(toolKey string, args []byte) TierVerdict {
	return TierOfAction(TierAction{ToolKey: toolKey, Args: args})
}

// TierOfAction 判定一次动作的层级。
//
// 纯函数：同一个入参永远得到同一个结论。
func TierOfAction(a TierAction) TierVerdict {
	v := tierOfActionDecide(a)
	// ScopeIn 在这里【统一盖章】，而不是逐分支各填各的。
	//
	// 为什么统一：逐分支填 ⇒ 哪个分支忘了填，它那儿的 ScopeIn 就是 false ⇒ 调用方
	// 分不清「真不在 scope 内」与「这项信息没填」⇒ 审计里两者长得一样 ⇒ 无法排查。
	// 统一口径是「调用方判过且全部在范围内」⇒ 每一处的含义完全一致。
	v.ScopeIn = a.Scope.Assessed && a.Scope.AssetInScope
	return v
}

// tierOfActionDecide 是判定本体（不含 ScopeIn 盖章）。
func tierOfActionDecide(a TierAction) TierVerdict {
	// ① 提议级写走自己的判定分支（见 judgeProposalWrite）。
	//
	// 为什么放在最前面：提议级是【被证明才成立】的资格，而资格成立时它要争取的
	// 正是「比直接写更轻」的结论 ⇒ 它的判据必须自己闭合，不能被直接写的判据
	// 截胡。fail closed 由 judgeProposalWrite 内部逐条保证。
	if a.Write.Proposal {
		return judgeProposalWrite(a)
	}

	// ② 写语义【优先于】工具 key：裁决写 / 状态迁移 / 出网 / 扩权与「这个 key 属于哪张
	//    清单」无关。顺序反了就会出现「一个 Tier 2 工具顺带改了归属 ⇒ 被当成普通结构化
	//    写放行」—— 正是设计 §1 想堵的那类绕过。
	if a.Write.IntentOwner != "" {
		return tier3HostOnlyVerdict("裁决写：改变 intent 的归属（owner=" + a.Write.IntentOwner +
			"）⇒ 不经分类器，走 host-only 写面（设计 §4.2：分类器不能作为高后果决策的唯一防线）")
	}
	if st := a.Write.TargetState; st != "" && st != intentStateOpen {
		return tier3StateVerdict(st)
	}
	if a.Write.Egress {
		return tier3Verdict(CategoryEgress, "出网（数据外带）⇒ Tier 3（设计 §4.1：出网治理不完整，必须显式标注）")
	}
	if a.Write.ScopeExpansion {
		return tier3Verdict(CategoryScopeExpansion,
			"扩权请求本身 ⇒ Tier 3，分类器审 + 留痕（设计 §1 Tier 3 ④ / §2.4 硬约束 2）")
	}

	// ③ Tier 1：读 approval_policy.go 的【唯一一份】清单（不另抄一份）。
	if IsTier1ReadOnlyTool(a.ToolKey) {
		return TierVerdict{Tier: Tier1, Category: CategorySafeRead,
			Reason: "Tier 1 安全只读工具：自动通过，不消耗分类器调用（设计 §1）"}
	}

	// ④ Tier 2 候选 + 条件。
	if kind, ok := tier2WriteKinds[a.ToolKey]; ok {
		return judgeTier2(a, kind)
	}

	// ⑤ Tier 3 的显式名单。
	if v, ok := judgeTier3ByKey(a.ToolKey); ok {
		return v
	}

	// ⑥ 默认 Tier 3（fail closed）：只有被证明安全的工具/动作才自动放行
	//    （设计 §5：不制造虚假安全感）。
	return tier3Verdict(CategoryUnlisted,
		"不在自动放行清单里：只有被证明安全的工具/动作才自动放行（设计 §5 不制造虚假安全感）")
}

// 提议级分支 judgeProposalWrite 的实现在 agent/strategy_proposal_guard.go ——
// 它是本文件里**唯一**一条依赖具体面（策略库面）的分支，其余分支（事实 / 意图 /
// 邮箱 / 出网 / 扩权）都是与面无关的通用分层规则。把面相关的分支搬到面自己的
// 文件里，通用判定器就重新变成「与面无关」的了。

// judgeTier2 判定一个 Tier 2 候选动作的【条件】是否满足。
//
// 条件不满足 ⇒ Tier 3（不是「Tier 2 但打个折」）。Tier 2 的语义是「条件自动通过」，
// 条件没被证明满足就不能自动通过。
func judgeTier2(a TierAction, kind tier2WriteKind) TierVerdict {
	switch kind {
	case tier2Finding:
		return judgeFindingWrite(a)
	case tier2Intent:
		return judgeIntentWrite(a)
	case tier2Fact:
		return tier2Verdict("追加 fact（设计 §1 ③）")
	case tier2Hint:
		return tier2Verdict("追加 hint（设计 §1 ③）")
	case tier2Mailbox:
		return judgeMailboxWrite(a)
	default:
		return tier3Verdict(CategoryUnlisted, "未知的 Tier 2 动作种类（fail closed）")
	}
}

// judgeFindingWrite 判定「锚定 scope 内资产的漏洞登记」（设计 §1 ①）。
//
// §2.4 的硬要求：校验对象是「该 asset_id 落在本任务的有效 scope 内」，【不是】
// 「本次 list_assets 返回里出现过」—— 后者会误杀「先取证后报告」：用 curl 手工确认
// 目标时目标还没进资产库，list_assets 必然查不到，而这正是渗透流程的常态。
//
// 判定依据由调用方给（ScopeVerdict），分类器不查库：查库就等于读 agent 可写的
// task_scope 行 = 自证合法（§2.4 实现红线）。
func judgeFindingWrite(a TierAction) TierVerdict {
	if len(referencedAssetIDs(a.Args)) == 0 {
		// 没锚定任何资产 ⇒ 没有「写范围外资产」这个失败面。
		return tier2Verdict("漏洞登记（未锚定资产，设计 §1 ①）")
	}
	if !a.Scope.Assessed {
		return tier3Verdict(CategoryOutOfScope, "漏洞登记锚定了资产但没有 scope 判定结论 ⇒ 未判定 ≠ 在 scope 内（fail closed）")
	}
	if !a.Scope.AssetInScope {
		return tier3Verdict(CategoryOutOfScope,
			"漏洞登记锚定的资产不在本任务有效 scope 内 ⇒ 写范围外资产（设计 §1 Tier 3 ① / §2.4）")
	}
	return tier2Verdict("漏洞登记（锚定 scope 内资产，设计 §1 ①）")
}

// judgeIntentWrite 判定「写 open 状态的 intent」（设计 §1 ②）。
//
// §1 明确不含两件事：改变 intent owner、触发任何状态迁移（done/met/fixed/retired/active
// 等）。上面 Write 语义分支已经兜住调用方填的语义；这里再兜一层【从入参里读到的】
// owner/state，防止调用方漏填语义时把裁决写当成普通意图写放行。
//
// 为什么「写意图是 Tier 2」而「改归属是 Tier 3」：意图的创建是【开放探索方向】的登记，
// 它不改任何既有归属、不推进任何状态，代价可回滚；而归属是【裁决权】本身 ——
// 归属一改，「这条意图归谁做」就由一次写入单方面决定，而归属权威唯一 =
// exploration_nodes.owner（由 ClaimIntent 的 CAS 写入）。两者不是同一类副作用，
// 不能因为都写 exploration_nodes 就同层。
func judgeIntentWrite(a TierAction) TierVerdict {
	if owner := argsString(a.Args, "owner", "intent_owner"); owner != "" {
		return tier3HostOnlyVerdict("写意图顺带改归属（owner=" + owner +
			"）⇒ 裁决写，走 host-only 写面（设计 §1 Tier 2 明确不含 / §4.2）")
	}
	if st := argsString(a.Args, "state", "new_state", "target_state"); st != "" && st != intentStateOpen {
		return tier3StateVerdict(st)
	}
	return tier2Verdict("写 open 状态的 intent（设计 §1 ②）")
}

// judgeMailboxWrite 判定「写 mailbox 消息」（设计 §1 ④）。
//
// mailbox 是【协商层】，不是【授权层】（设计 §3）：消息只提供上下文，不改变 owner、
// 不推进状态、不构成任何写面授权。归属变更/状态迁移已由 Write 语义拦在 Tier 3；
// 消息自称「已批准」也只是不可信输入（§3 规则 2）—— 本判定【永远不】把它当成授权。
func judgeMailboxWrite(a TierAction) TierVerdict {
	reason := "写 mailbox 消息（设计 §1 ④）：只提供上下文，不构成授权"
	if a.Write.ClaimsAuthorization {
		reason += "；⚠️ 该消息自称携带「已批准」：按设计 §3 规则 2 属不可信输入，不触发任何权限门放行"
	}
	return TierVerdict{Tier: Tier2, Category: CategoryScopedWrite, Reason: reason}
}

// tier3StateVerdict 判一次状态迁移的层级。
//
// 终态迁移（verified/fixed/retired/active）是【裁决写】⇒ host-only 写面，不经分类器
// （设计 §4.2）；其余状态迁移（met/done/stopped/running…）是 Tier 3 的普通状态变更，
// 交分类器审查 —— 但都不是 Tier 2（设计 §1 明确不含任何状态迁移）。
func tier3StateVerdict(state string) TierVerdict {
	if containsString(tier3AdjudicationStates, state) {
		return tier3HostOnlyVerdict("裁决写：终态迁移到 " + strconv.Quote(state) + " ⇒ 不经分类器，走 host-only 写面（设计 §4.2）")
	}
	return tier3Verdict(CategoryStateChange, "状态迁移：目标状态 "+strconv.Quote(state)+
		" ⇒ Tier 2 明确不含任何状态迁移（设计 §1）")
}

// judgeTier3ByKey 按工具 key 判 Tier 3 的四类（设计 §1 Tier 3 / §4.1）。
func judgeTier3ByKey(key string) (TierVerdict, bool) {
	switch {
	case strings.HasPrefix(key, tier3MCPToolPrefix):
		return tier3Verdict(CategoryEgress, "MCP 工具："+key+
			" ⇒ transport 不受 netguard 约束（出网），且描述是运行期抓取 ⇒ 分类器只能给「建议」（设计 §4.1.1）"), true
	case containsString(tier3EgressTools, key):
		return tier3Verdict(CategoryEgress, "出网工具："+key+" ⇒ Tier 3（设计 §4.1：出网治理不完整，必须显式标注）"), true
	case containsString(tier3ScopeExpansionTools, key):
		return tier3Verdict(CategoryScopeExpansion, "扩权请求工具："+key+
			" ⇒ 扩权请求本身在 Tier 3，分类器审 + 留痕（设计 §1 Tier 3 ④ / §2.4 硬约束 2）"), true
	case containsString(tier3StateChangeTools, key):
		return tier3Verdict(CategoryStateChange, "状态迁移工具："+key+
			" ⇒ Tier 2 明确不含任何状态迁移（设计 §1）"), true
	case containsString(tier3CrossTaskWriteTools, key):
		return tier3Verdict(CategoryUnlisted, "跨任务写工具："+key+
			" ⇒ 写的是别的任务的图，不是「本任务范围内的结构化写」（设计 §1）"), true
	case containsString(strategyLibraryToolKeys, key) || containsString(tier3StrategyLibraryAdvisoryTools, key):
		return tier3Verdict(CategoryUnlisted, "策略库面工具："+key+
			" ⇒ 策略库跨任务、跨租户，读它不是「任务范围内的只读」，写它进的是 curator 的全局队列（设计 §1 Tier 2 四类动作里没有它）"), true
	case containsString(tier3LocalFileTools, key):
		return tier3Verdict(CategoryUnlisted, "宿主文件系统工具："+key+
			" ⇒ reArtex 的 Tier 2 是「scope 内结构化写」，不含文件编辑；读面隔离证明也不适用于它（设计 §1 / §0）"), true
	}
	return TierVerdict{}, false
}

// ---------------------------------------------------------------------------
// 入参读取（只读判定所需的几个字段）
// ---------------------------------------------------------------------------

// argsObject 把入参解成顶层对象。解不出（非 JSON / 非对象）时返回 nil。
func argsObject(args json.RawMessage) map[string]json.RawMessage {
	if len(args) == 0 {
		return nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(args, &m); err != nil {
		return nil
	}
	return m
}

// argsString 依次尝试几个字段名，取第一个非空字符串值。
func argsString(args json.RawMessage, keys ...string) string {
	m := argsObject(args)
	for _, k := range keys {
		raw, ok := m[k]
		if !ok {
			continue
		}
		var s string
		if err := json.Unmarshal(raw, &s); err == nil && strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

// argsPresentKeys 报告入参里出现了哪些给定的顶层字段名。
//
// 只看【存在性】、不看值：提议级的门是「这个字段根本不该出现在提议的入参里」
// —— 出现一个状态字段（哪怕它的值是 null）就说明调用方在试图声明状态，而不是
// 在提议。值语义由 argsString 另判。
func argsPresentKeys(args json.RawMessage, keys ...string) []string {
	m := argsObject(args)
	if m == nil {
		return nil
	}
	var hit []string
	for _, k := range keys {
		if _, ok := m[k]; ok {
			hit = append(hit, k)
		}
	}
	return hit
}

// referencedAssetIDs 取入参里引用的资产 id（asset_ids 数组 + asset_id 单值）。
//
// 只看顶层：漏洞登记（report_finding）的入参是平的，没有批量形态。
func referencedAssetIDs(args json.RawMessage) []int64 {
	m := argsObject(args)
	var ids []int64
	if raw, ok := m["asset_ids"]; ok {
		var list []json.RawMessage
		if err := json.Unmarshal(raw, &list); err == nil {
			for _, item := range list {
				var id int64
				if err := json.Unmarshal(item, &id); err == nil && id > 0 {
					ids = append(ids, id)
				}
			}
		}
	}
	if raw, ok := m["asset_id"]; ok {
		var id int64
		if err := json.Unmarshal(raw, &id); err == nil && id > 0 {
			ids = append(ids, id)
		}
	}
	return ids
}

// containsString 报告列表里是否有该值。
func containsString(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

package agent

// 本文件是【自动权限分层】里 Tier 1 判定的唯一真相源。
//
// 分层模型（见 .omo/plans/2026-10-29-auto-permission-tiers.md §1）：
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

// ---------------------------------------------------------------------------
// Tier 2 / Tier 3 清单（设计 §1 的逐行映射）
// ---------------------------------------------------------------------------
//
// 下面这些清单与上面那份 Tier 1 清单是同一件事的三个面：**分层判定读的就是它们**。
// 判定器在 agent/tier_judge.go，它【读】本文件的清单，不另抄一份 —— 抄一份就会出现
// 「清单说安全、判定读另一份」这种无人变红的漂移。
//
// 设计 §1 的逐行映射（原文照抄，便于审计）：
//
//	Tier 2 —— scope 内结构化写，条件自动通过
//	  动作 | ① report_finding（锚定 scope 内资产）；② 写 open 状态的 intent；
//	        ③ 追加 fact / hint；④ 写 mailbox 消息
//	  明确不含 | 改变 intent owner、触发任何状态迁移（done/met/fixed/retired/active 等）
//
//	Tier 3 —— 高风险 / scope 外 / 裁决写
//	  动作 | ① 写范围外资产；② 改变 intent owner、把策略标 active、把 finding 标
//	        verified 等裁决写；③ 出网操作（Bash 出网、MCP transport、录制代理目的地）；
//	        ④ AddAgentScope / AddAutoScope 扩权请求本身
//
// 「进清单 ≠ 自动放行」：Tier 2 是【条件】自动通过，条件不满足就是 Tier 3；
// Tier 3 之外的工具一律按 fail closed 落到 Tier 3（不在自动放行清单里）。

// intentStateOpen 是「开放的意图」状态。Tier 2 只认它（设计 §1 Tier 2 ②）。
const intentStateOpen = "open"

// tier2WriteKind 是 Tier 2 候选动作的种类。
type tier2WriteKind int

const (
	// tier2Finding 是「锚定 scope 内资产的漏洞登记」（设计 §1 ①）。
	tier2Finding tier2WriteKind = iota
	// tier2Intent 是「写 open 状态的 intent」（设计 §1 ②）。
	tier2Intent
	// tier2Fact 是「追加 fact」（设计 §1 ③）。
	tier2Fact
	// tier2Hint 是「追加 hint」（设计 §1 ③）。
	tier2Hint
	// tier2Mailbox 是「写 mailbox 消息」（设计 §1 ④）。
	tier2Mailbox
)

// tier2MailboxWriteToolKey 是 mailbox 写工具的 key（设计 §1 Tier 2 ④）。
//
// 该工具由 Phase 3 Task 2 落地（message_group，见
// .omo/plans/2026-10-28-task-hall-phase3-implementation.md Task 2）；
// 本清单先按计划把 key 钉住 —— 工具落地后同名，不会漂移。
const tier2MailboxWriteToolKey = "message_group"

// tier2WriteKinds 是 Tier 2 的候选动作清单（设计 §1 Tier 2 行的四个动作）。
//
//	① report_finding  —— 漏洞登记（锚定 scope 内资产）
//	② add_intent      —— 写 open 状态的 intent
//	  propose_intent  —— 同上（群成员的隔离提议：写的也是一条 open 的 intent）
//	③ record_fact     —— 追加 fact
//	  add_hint        —— 追加 hint
//	④ message_group   —— 写 mailbox 消息
//
// 「候选」二字是认真的：进了这张清单【不等于】自动放行 —— Tier 2 是【条件】自动通过，
// 条件不满足就是 Tier 3（见 agent/tier_judge.go 的 judgeTier2）。
var tier2WriteKinds = map[string]tier2WriteKind{
	"report_finding":         tier2Finding,
	"add_intent":             tier2Intent,
	"propose_intent":         tier2Intent,
	"record_fact":            tier2Fact,
	"add_hint":               tier2Hint,
	tier2MailboxWriteToolKey: tier2Mailbox,
}

// tier3EgressTools 是【出网工具】清单（设计 §4.1：出网一律 Tier 3）。
//
//	Bash       —— 可执行任意命令（含 curl 出网、直调平台 API 的写入口）。netguard 只
//	              覆盖自定义工具，Bash 不在其中（§4.1「出网治理不完整」），而且 §2.1(a)
//	              的绕过路径正是「不走 report_finding，用 Bash curl 调平台 API 写 finding」。
//	web_search —— 搜索后端直连（§5 列出的未覆盖通道之一）。
//
// MCP 工具不在这张清单里：它们的 key 是运行期拼出来的（mcp__<server>__<tool>），
// 按前缀判才追得上注册进来的 MCP —— 见 tier3MCPToolPrefix。
var tier3EgressTools = []string{"Bash", "web_search"}

// tier3MCPToolPrefix 是 MCP 工具 key 的前缀。
//
// MCP 工具的 key 由 mcphttp.ToolName(server, name) 拼成（mcphttp/client.go:82；
// SDK 侧同名函数 third_party/norma/mcp/mcp.go:21）—— 这里钉的是同一个前缀。
//
// 为什么 MCP 一律 Tier 3（设计 §4.1.1）：
//   - transport 是裸 http.Client + InsecureSkipVerify（mcphttp/client.go:104,129），
//     不受 netguard 约束 ⇒ 它就是出网；
//   - 工具描述是运行期外部抓取、不在 ARTex DB 里 ⇒ 分类器看到的只是【当次抓到的描述】，
//     对这条路径的判断只能算「建议」。
var tier3MCPToolPrefix = "mcp__"

// tier3ScopeExpansionTools 是【扩权请求】工具（设计 §1 Tier 3 ④ / §2.4 硬约束 2）。
//
//	add_task_scope    —— 直接改本任务的授权边界（db/task_scope.go 的 AddAgentScope）。
//	add_company_scope —— 改公司级资产范围（会把整个公司名下的资产树拉进来）。
//	insert_assets     —— 登记资产会触发自动扩 scope（db/task_scope.go 的 AddAutoScope）。
//
// 为什么 insert_assets 也在里面：设计 §2.4 的「先取证后报告」合法路径 = 先登记再报告，
// 而登记动作【本身】就是一次扩权请求 —— 「扩权请求本身在 Tier 3 → 分类器审 + 留痕」
// （§2.4 硬约束 2）。它的扩张幅度已被 ScopePolicy.MaxAgentAddedScope /
// MaxAgentAddedKinds 限死（db/task_scope.go 的 checkAgentScopeBudget / checkAgentScopeKind），
// 所以「合法路径存在，但扩张幅度有上限，且每次扩张可审计」。
//
// 当前口径是【一律 Tier 3】（fail closed）：宁可多审一次扩权，也不给「先把范围扩出去
// 再回头把范围外资产当合法」的绕行留缝。
var tier3ScopeExpansionTools = []string{"add_task_scope", "add_company_scope", "insert_assets"}

// tier3StateChangeTools 是【会触发状态迁移】的工具（设计 §1 Tier 2 明确不含任何状态迁移）。
//
//	prove_goal —— 把目标标 met（§1 明确点名 'met'）。
//	kill_work  —— 把意图标 stopped（§1 的「等」覆盖它）。
//	goal_met   —— 直接宣布整个任务完成（且本来就默认不绑定：toolcatalog.go 的 defaultUnbound）。
var tier3StateChangeTools = []string{"prove_goal", "kill_work", "goal_met"}

// tier3CrossTaskWriteTools 是【跨任务写】的工具。
//
//	add_task_hint —— 把提示写进【别的任务】的图（server/orchestration.go 注册）。
//	                 那不是「本任务范围内的结构化写」，而是往别人家的图里写 ⇒ 不在 Tier 2。
var tier3CrossTaskWriteTools = []string{"add_task_hint"}

// tier3LocalFileTools 是 SDK 的本地文件工具（norma 的 actool.DefaultTools）。
//
// Claude Code 的口径是「项目内文件编辑 = Tier 2」（设计 §0），但 reArtex 的 §1 把 Tier 2
// 重新定义成【scope 内结构化写】，那四类动作里没有文件编辑 ⇒ 文件工具不自动放行。
//
// 为什么 Read/LS/Glob/Grep 也不在 Tier 1：Tier 1 的准入条件是「读面范围隔离已被
// TestTier1ReadOnlyToolsNoScopeLeak 证明」，而这些工具读的是【宿主文件系统】，不是
// 任务范围内的平台数据 —— 那道证明对它们不成立。
var tier3LocalFileTools = []string{"Read", "Write", "Edit", "MultiEdit", "LS", "Glob", "Grep", "Sleep"}

// tier3StrategyLibraryAdvisoryTools 是策略库的顾问型占位工具（toolcatalog.go 的
// advisoryStrategyToolSeeds 里的那批）。
//
// 它们是策略库【面】上的工具：读的是跨任务、跨租户的知识，写的是 curator 的全局队列。
// 策略库面一律 Tier 3 —— approval_policy.go 的 init 守卫只挡「策略工具进 Tier 1」，
// 这张清单回答的是「它们落在哪一层」。
var tier3StrategyLibraryAdvisoryTools = []string{
	"read_strategy_library",
	"read_pending_contradictions",
	"run_replay_eval",
	"read_finding",
	"read_strategy_posterior",
	"read_traffic_evidence",
	"read_asset_graph",
	"submit_decision",
	"submit_verdict",
	"submit_contradiction",
	"propose_strategy",
	"propose_scenario_feature",
}

// tier3AdjudicationStates 是【裁决写】的目标状态（设计 §4.2：终态迁移）。
//
//	verified —— finding 标 verified
//	fixed    —— finding 标 fixed
//	retired  —— 策略标 retired
//	active   —— 策略标 active
//
// 这些写【不经分类器】，只走宿主侧写面（curator 进程或 DB role）：Anthropic 生产流量
// 的 FNR 是 17%，独立压力测试在 Tier 3 仍有 70.3% FNR ⇒ 分类器不能作为高后果决策的
// 唯一防线（设计 §4.2）。
var tier3AdjudicationStates = []string{"verified", "fixed", "retired", "active"}

// StrategyProposalTools 是【提议级】工具清单。
//
// 它们的审查成本必须**严格低于**直接写入类工具（如 add_intent）：提议只写隔离态
// 策略 + 待裁决提议，不改任何 owner、不触发任何状态迁移。与 Phase 5 的
// propose_intent 同一处置（design §2 Step 4）。
//
// 这条「严格低于」不再是注释里的承诺，而是【可断言的关系】：分层判定器给提议级
// 动作一个独立类别（CategoryProposal）与一个更低的审查成本序数
// （TierVerdict.ReviewCost），守卫（agent/strategy_proposal_guard.go）在真正
// 的写入路径上把 IsStrategyProposalTool 接上 ⇒ 轻判只给被证明的隔离提议。
//
// ⚠️ 策略工具**不得**进 Tier1ReadOnlyTools：策略库跨任务、跨租户，读它不是
// 「任务范围内的只读」⇒ 进 Tier 1 等于自动放行范围外读取。这条由本文件末尾的
// init 守卫结构性拦住，不是靠约定。
var StrategyProposalTools = []string{"strategy_propose"}

// IsStrategyProposalTool 报告某个工具 key 是否属于提议级。
//
// 调用点：分层判定器的提议级分支（agent/tier_judge.go 的 judgeProposalWrite）
// 与策略提议守卫（agent/strategy_proposal_guard.go）—— 两处都在**副作用发生
// 点**上，不是只在清单里躺着一个名字。
func IsStrategyProposalTool(key string) bool {
	for _, k := range StrategyProposalTools {
		if k == key {
			return true
		}
	}
	return false
}

// 提议级入参里**不该出现**的字段清单（判据见 agent/tier_judge.go 的
// judgeProposalWrite 与 agent/strategy_proposal_guard.go 的守卫）。
//
// 为什么按「字段存在」判而不是「值非空」：提议的语义就是「不声明任何状态、
// 不指定任何归属」—— 入参里出现这些字段（哪怕值是 null）就说明调用方在试图
// 声明状态 / 归属，而不是在提议。
//
// 为什么放在本文件：判定器与守卫读的是同一份清单。抄一份就会出现「判定读一份、
// 守卫读另一份」的漂移（与 Tier 1 清单同一条理由）。
var (
	// proposalStateArgs 是提议级入参里不该出现的【状态】字段。
	proposalStateArgs = []string{"state", "new_state", "target_state"}
	// proposalAdjudicationArgs 是提议级入参里不该出现的【裁决面】字段
	//（裁决状态 + 裁决理由）。
	proposalAdjudicationArgs = []string{"status", "proposal_status", "decided_reason"}
	// proposalOwnershipArgs 是提议级入参里不该出现的【归属】字段。
	proposalOwnershipArgs = []string{"owner", "intent_owner", "created_by", "decided_by", "promoted_by"}
)

// strategyLibraryToolKeys 是策略库（L4）面向 agent 的工具 key 全集。
//
// 三个都要守住「不进 Tier 1」：它们读的是**跨任务、跨租户**的知识，
// 自动放行等于让任何 agent 无需申请就把范围外的知识读走。
var strategyLibraryToolKeys = []string{"strategy_search", "strategy_get", "strategy_propose"}

// init 是「策略工具不入 Tier 1」的**结构性**守卫。
//
// 为什么是 panic 而不是一条测试：Tier 1 的语义是「自动通过、不消耗分类器调用」，
// 一旦策略工具被写进那张清单，泄露就是【默认发生】而不是「可能发生」。用 panic
// 把这条变成「写错就起不来」，比留一条会被人绕过的测试更硬。
func init() {
	for _, k := range strategyLibraryToolKeys {
		if IsTier1ReadOnlyTool(k) {
			panic("策略工具 " + k + " 不允许进 Tier1ReadOnlyTools：策略库跨任务、跨租户，读它不是「任务范围内的只读」")
		}
	}
}

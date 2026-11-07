// Package sodpolicy 是 SoD（职责分离）链的【代码常量】单一真相源。
//
// 为什么是代码常量而不是配置：DB 里的行可以被 DELETE，settings 可以被持有
// JWT 的人热改 —— 与 internal/scopepolicy、guard/shortcircuit.go 同一条理由。
// 熔断阈值 / 熔断时长 / 规整器超时 / 凭证 TTL 这些数字一旦能被外部数据改写，
// 闸门就等于「由被它约束的人来调阈值」：把熔断阈值调到 100，连续失败 99 次
// 的工具照样放行，SoD 链就成了摆设。
//
// 为什么是叶子包：db / agent / server 三方都要 import 它。放在 agent 里会成环
// （agent → db，db 不能 → agent），编译不过。
package sodpolicy

// 裁决三态 —— 闸门只能产出这三种结果。
//
// 为什么没有第四种「先放行、事后再说」：approve 就是放行；needs_review 是
// 「不执行、交给对抗者证伪」，它**不是**放行的同义词 —— needs_review 的工具
// 调用同样被 block。闸门不产出第四种结果，也就不会有「忘了处理的那一类」。
const (
	// VerdictApprove 放行：五道检查全过。
	VerdictApprove = "approve"
	// VerdictDeny 拒绝：越界 / 熔断 / 额度 / tier2 不一致 / 各类 fail-closed。
	VerdictDeny = "deny"
	// VerdictNeedsReview 复核：证据不足以拒，但值得让对抗者证伪。
	VerdictNeedsReview = "needs_review"
)

// 原因码全集（机器可读，不含数字）。
//
// 为什么要有原因码：拒绝消息会给人看，但「拒绝的原因」必须能被机器唯一定位
// —— 审计里的原因码要能把一条裁决指回它落在哪道检查上。含数字是刻意的
// 禁止：数字会随阈值调整而漂移，历史裁决会失去可比性。
const (
	// ---- 通过 ----
	// ReasonScopeOK scope 检查通过（单独使用时的通过码）。
	ReasonScopeOK = "scope_ok"
	// ReasonAllChecksPassed 全部检查通过时的裁决原因码。
	ReasonAllChecksPassed = "all_checks_passed"
	// ReasonTier2NotApplicable 本次调用不关联 finding ⇒ tier2 检查不适用（视为通过）。
	ReasonTier2NotApplicable = "tier2_not_applicable"

	// ---- 拒绝 ----
	// ReasonOutOfScope 目标不在任务范围内（越权，最高等级的越权）。
	ReasonOutOfScope = "out_of_scope"
	// ReasonToolCircuitOpen 目标工具处于 open 熔断（工具不可用）。
	ReasonToolCircuitOpen = "tool_circuit_open"
	// ReasonQuotaExhausted 群组额度处于 draining / parked。
	ReasonQuotaExhausted = "quota_exhausted"
	// ReasonTier2Inconsistent 关联 finding 的重放证据为空或资产归属不一致。
	ReasonTier2Inconsistent = "tier2_inconsistent"

	// ---- 复核 ----
	// ReasonAnomalyFlagged 规整器标记了异常（是复核，不是拒绝）。
	ReasonAnomalyFlagged = "anomaly_flagged"

	// ---- fail-closed：查不出来 ⇒ 拒绝，不是放行 ----
	// ReasonScopeUnavailable scope 查询出错。
	ReasonScopeUnavailable = "scope_check_unavailable"
	// ReasonToolHealthUnavailable 熔断状态查询出错。
	ReasonToolHealthUnavailable = "tool_health_unavailable"
	// ReasonQuotaUnavailable 额度状态查询出错。
	ReasonQuotaUnavailable = "quota_check_unavailable"
	// ReasonTier2Unavailable finding / 证据查询出错。
	ReasonTier2Unavailable = "tier2_check_unavailable"
	// ReasonNormalizerUnavailable 规整器超时 / 报错 / 不可用。
	ReasonNormalizerUnavailable = "normalizer_unavailable"
	// ReasonNormalizerMalformed 规整器输出无法解析或结构不合法。
	ReasonNormalizerMalformed = "normalizer_malformed"
	// ReasonTokenInvalid 执行器侧凭证校验失败（无凭证 / 摘要不匹配 / 已用过 / 越权转让）。
	ReasonTokenInvalid = "execution_token_invalid"
)

// 熔断状态。
//
// 为什么有第三态 half_open：open 是「一律拒绝」，closed 是「一律放行」，
// 两者之间需要一格「允许一次试探」—— 坏掉的工具要能自己恢复，否则一次熔断
// 就把工具在整场任务里判了死刑。
const (
	// CircuitClosed 正常态。
	CircuitClosed = "closed"
	// CircuitOpen 开路态：拒绝一切调用。
	CircuitOpen = "open"
	// CircuitHalfOpen 半开态：允许一次试探（成功即恢复，失败即重新开路）。
	CircuitHalfOpen = "half_open"
)

// 阈值（全部是代码常量；DB 里不允许出现任何阈值列）。
//
// ⚠️ 不许提供「可配置」的影子：将来要调，走代码评审改常量 —— 与
// guard/shortcircuit.go 的纪律一致。一个能被外部数据改写的阈值等于没有阈值。
const (
	// NormalizerTimeoutSeconds 是规整器（LLM）的同步调用上限。
	//
	// 一次规整往返的典型耗时是 1–4 秒（短提示 + 短 JSON 输出 + temperature=0）。
	// 原值 20s 按「典型 1-4s × 5 倍余量」推导，但实测（v0.4.11 联动测试，
	// mimo 网关 hy3）约一半规整调用超过 20s（llm_records 连续 context deadline
	// exceeded，此前实测均值 17.8s）——网关延迟分布远比「典型」宽。超时是
	// fail-closed 拒绝 ⇒ 每次超时都把一个正常动作打成拒绝并逼 worker 重试整轮。
	// 提到 45s：仍远小于 worker 单轮预算，排队等待已被信号量隔离在超时之外，
	// 这里只管模型往返本身。
	NormalizerTimeoutSeconds = 45

	// NormalizerMaxTokens 是规整器输出的 token 上限。
	//
	// 规整器的产出是一份结构化意图（几百 token 量级），不是散文。上限压低是
	// 刻意的：它同时限制了「模型开始长篇大论」与单次调用的成本。
	NormalizerMaxTokens = 2048

	// NormalizerCacheMaxEntries 是规整结果缓存的条目上限（进程内 LRU）。
	//
	// 缓存里存的是**规整结果**，不是裁决 —— 裁决依赖实时状态（额度、熔断、
	// scope 行），缓存它等于拿过期状态放行。上限防的是缓存被撑爆。
	NormalizerCacheMaxEntries = 512

	// CircuitFailThreshold 是工具连续失败多少次后熔断开路。
	//
	// 5 次：单次失败（网络抖动、参数写错）不该把工具拉黑；连续 5 次说明这个
	// 工具在当前上下文里确实不可用，继续放行只会反复浪费动作。
	CircuitFailThreshold = 5

	// CircuitOpenDurationSeconds 是熔断开路后多久转入半开（允许一次试探）。
	//
	// 300 秒（5 分钟）：覆盖一次「修好再试」的典型间隔，同时不至于让一个坏掉
	// 的工具在整场任务里永久不可用。
	CircuitOpenDurationSeconds = 300

	// TokenTTLSeconds 是一次性执行凭证的寿命（秒）。
	//
	// 它存在的唯一目的是**兜住「批准了但没执行」的窗口**（工具被上层取消、
	// 进程被打断），不是为了让凭证可以事后重放 —— 单次有效由「消费即删除」
	// 保证，不由 TTL 保证。
	TokenTTLSeconds = 60

	// TokenBytes 是凭证本体的字节数（取随机数后 base64url 编码）。
	//
	// 32 字节 = 256 bit 熵：凭证是一次性的、秒级寿命的、且与动作绑定，
	// 猜中的代价是一次动作被拒；熵不需要更大，也不能更小（更小 ⇒ 可枚举）。
	TokenBytes = 32

	// PayloadPreviewMaxBytes 是审计里留存的脱敏载荷预览长度上限（字节）。
	//
	// 原始载荷**只**以「摘要 + 脱敏预览」入库，绝不存原文（原文里可能有
	// Authorization / 口令 / 目标地址）。预览只是给人看的线索，够定位即可。
	PayloadPreviewMaxBytes = 512
)

// NotGatedTools 是【不进入 SoD 链】的工具：本地只读 / 平台只读 / 纯记账。
//
// 为什么是【放行清单】而不是【拦截清单】：清单以外的**全部**工具都进链 ⇒
// 未登记的新工具（MCP 的任意名、自定义工具的任意 key）默认被管住
// （classification 上的 fail-closed）。用拦截清单的话，每加一个新工具就
// 等于给它开了一扇默认放行的门 —— 那是把安全决定交给「记得更新清单的人」。
//
// ⚠️ 平台只读那 5 个必须是 agent.Tier1ReadOnlyTools 的【子集】
// （agent/approval_policy.go:37-43）—— 由 agent/sod_worker_prompt_test.go 断言。
//
// 方案 v3 缺陷 B1 扩容：清单原来只放行 5 个平台只读工具，于是
// get_worker_trace / list_progress_receipts 这类【平台自己的只读/记账工具】
// 也进链 → 被判 Tier3「未登记工具」→ 依赖规整器 → 规整器一抖全被拒
// （远程实测一次任务 16 次 get_worker_trace 全 deny）。扩容只加两类：
// 平台只读（图/链路/目标的读侧）与平台结构化记账（写的事实在任务图内，
// 由 gate 的任务目录/写宿主检查兜底）。Bash/Write/MCP/自定义工具一律不进
// 这份清单——它们仍走完整链。
var NotGatedTools = []string{
	// 本地只读：不派生进程、不外发、不改平台状态。
	"Read", "LS", "Glob",
	// 平台只读（Tier 1）。
	"list_assets", "list_findings", "get_finding_traffic",
	"graph_overview", "list_untested_assets",
	// 纯记账。
	"TodoWrite", "Sleep",
	// 平台只读扩容（B1）：探索图/执行链路的读侧与检索类。
	// ⚠️ 交互 shell 全家族（shell_open/shell_send/shell_read/shell_close…）**不**
	// 放行——internal/sod/sod_safety_pins_test.go 把「命令执行类必须进链」钉成
	// 生死线，shell_read 虽只读输出缓冲，也随家族一起保持受管。
	"get_worker_trace", "get_worker_output", "search_all_worker_traces",
	"node_detail", "expand_digest", "list_goals", "recall_memory",
	"list_companies",
	// 平台结构化记账（B1）：写侧目标固定在任务图/回执表，不触宿主 FS、不出网。
	"record_fact", "record_progress", "list_progress_receipts",
}

// NormalizerAgentKey 是意图规整 agent 的规范 key。
//
// 规整器必须有独立的 agent 身份：它只提议不裁决，它的产出在审计里要能被
// 单独指认（「这条线索来自规整器」），不能与发起动作的 agent 混在一起。
const NormalizerAgentKey = "intent_normalizer"

// RedteamAgentKey 是消费 needs_review 队列的 agent key。
//
// 对抗证伪必须由**对抗者**做：needs_review 交回给提出者复核等于自证。
const RedteamAgentKey = "redteam"

// AuditorAgentKey 是合并闸裁决岗（auditor）的规范 agent key（方案 v4.1 §3）：
// 它只提议不执行——裁决是写一条记录，不是替跑命令；它的每次裁决在审计里
// 能被单独指认。
const AuditorAgentKey = "auditor"

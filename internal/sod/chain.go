package sod

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/internal/sodpolicy"
	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/llm"
)

// reentrancyKey 是重入标记的 ctx 键（私有类型 ⇒ 外部无法伪造）。
type reentrancyKey struct{}

// withReentrancy 给 ctx 打上「正在规整器内部」的标记。
//
// 为什么需要：规整器自己会发起一次模型调用；若那次调用触发了工具执行，工具
// 又会走执行器装饰器 → 执行器又要查凭证 → 没有凭证就拒绝 → 拒绝又可能再触发
// 规整 ⇒ 无限递归。标记让执行器看到「这是链自己发起的调用」就直连。
func withReentrancy(ctx context.Context) context.Context {
	return WithReentrancy(ctx)
}

// WithReentrancy 是 withReentrancy 的导出形态：auditor 裁决岗（server 侧）
// 发起的模型调用同样不得再进链（否则递归）。语义与私有版一致。
func WithReentrancy(ctx context.Context) context.Context {
	return context.WithValue(ctx, reentrancyKey{}, true)
}

// InReentrancy 报告 ctx 是否来自规整器自身的模型调用。
func InReentrancy(ctx context.Context) bool {
	v, _ := ctx.Value(reentrancyKey{}).(bool)
	return v
}

// RunContext 是链与执行器从 ctx 里读到的运行上下文。
//
// 为什么放在 internal/sod 而不是直接 import agent：internal/sod 是叶子包，
// 而 agent 的工具装配（agent/assembly.go）将来要装执行器装饰器 ⇒ agent →
// internal/sod。若 internal/sod 再 import agent 就是循环依赖。
type RunContext struct {
	TaskID         int64
	ConversationID int64
	AgentKey       string
}

// runContextKey 是内置运行上下文的 ctx 键（私有类型 ⇒ 外部无法伪造）。
type runContextKey struct{}

// WithRunContext 把运行上下文挂进 ctx。
//
// 宿主若已经用别的约定携带了任务上下文，可以不调它，改用 SetContextReader
// 绑定读法；两者取其一即可。
func WithRunContext(ctx context.Context, rc RunContext) context.Context {
	return context.WithValue(ctx, runContextKey{}, rc)
}

// ContextReader 从 ctx 里读运行上下文的读法。
type ContextReader func(ctx context.Context) RunContext

var (
	contextReaderMu sync.RWMutex
	contextReader   ContextReader
)

// SetContextReader 绑定运行上下文的读法（宿主装配时调用；线程安全）。
//
// ⚠️ 链与执行器**必须**用同一套读法：链铸的凭证绑定 (载荷摘要, task_id,
// source_agent, tool_name)，执行器用同一组四元组查表 —— 两边读法不一致 ⇒
// 执行器查不到凭证 ⇒ 一律拒绝（fail-closed，不会放行）。
func SetContextReader(r ContextReader) {
	contextReaderMu.Lock()
	defer contextReaderMu.Unlock()
	contextReader = r
}

// currentRunContext 读运行上下文：优先用宿主绑定的读法，否则读内置 ctx 键。
func currentRunContext(ctx context.Context) RunContext {
	contextReaderMu.RLock()
	r := contextReader
	contextReaderMu.RUnlock()
	if r != nil {
		return r(ctx)
	}
	if v, ok := ctx.Value(runContextKey{}).(RunContext); ok {
		return v
	}
	return RunContext{}
}

// AgentKeyFromContext 从运行上下文里取当前 agent key。
//
// **为什么它是单一真相源**：执行凭证绑的是四元组 (载荷摘要, task_id,
// source_agent, tool_name)，链用它铸凭证、执行器用它查表。两边只要有一边
// 读法不同（一个从运行上下文读、另一个从别处读），凭证就永远查不到 ⇒ 每一个
// 被批准的动作都被执行器拒（fail-closed，但链等于瘫了）。
//
// 所以宿主装配时必须写 `AgentKeyOf: sod.AgentKeyFromContext` —— 链与执行器
// 于是从**同一个** ctx 里取**同一个**原值。
//
// ⚠️ 返回的是**原值**（不修剪空白）：执行器那边就是拿这个原值查表的，
// 链这边若擅自修剪，两端就会差一个空白 ⇒ 一样查不到。
func AgentKeyFromContext(ctx context.Context) string {
	return currentRunContext(ctx).AgentKey
}

// degradeThreshold 是进入降级所需的连续规整器 unavailable 次数（方案 v3 缺陷
// B4，代码常量——安全语义的阈值不进可热改面）。
const degradeThreshold = 5

// degradeTracker 跟踪规整器连续不可用次数与降级态（进程内；重启即回 fail-closed
// —— 降级是应急档，重启重置是保守方向）。
//
// 状态机：
//
//	开关关闭 ⇒ 永不进降级；在降级中关掉 ⇒ 立即退出（下一次读取即生效）。
//	连续 unavailable ≥ degradeThreshold ⇒ 进降级（广播一次）。
//	任一次规整成功 ⇒ 清零并退出降级（广播一次）。
type degradeTracker struct {
	mu          sync.Mutex
	consecutive int
	active      bool
}

var degrade degradeTracker

// isActive 报告当前是否处于降级态（开关关闭 ⇒ 恒 false）。
func (t *degradeTracker) isActive(enabled func() bool) bool {
	if enabled == nil || !enabled() {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.active
}

// recordUnavailable 记一次规整失败；达到阈值进降级。
func (t *degradeTracker) recordUnavailable(enabled func() bool, onChange func(bool)) {
	if enabled == nil || !enabled() {
		return
	}
	t.mu.Lock()
	t.consecutive++
	if t.consecutive >= degradeThreshold && !t.active {
		t.active = true
		t.mu.Unlock()
		log.Printf("[sod] 规整器连续 %d 次不可用，进入降级档（Tier1/2 确定性放行，Tier3 照拒）", degradeThreshold)
		if onChange != nil {
			onChange(true)
		}
		return
	}
	t.mu.Unlock()
}

// recordAvailable 记一次规整成功；退出降级并清零。
func (t *degradeTracker) recordAvailable(enabled func() bool, onChange func(bool)) {
	t.mu.Lock()
	wasActive := t.active
	t.consecutive = 0
	t.active = false
	t.mu.Unlock()
	if wasActive && enabled != nil && enabled() {
		log.Printf("[sod] 规整器恢复，退出降级档")
		if onChange != nil {
			onChange(false)
		}
	}
}

// Chain 是 SoD 链本体：实现 harness.HookRunner。
//
// 它是【装饰器】：先问 inner（guard），inner 不 block 才跑链。
// ⇒ 链看到的是 inner 可能改写后的【最终】入参字节 ⇒
// 凭证绑定的字节 == 真正会执行的字节（见「口径统一」第 3 条的 TOCTOU 说明）。
//
// 顺序：inner → （inner block 则直接返回）→ 规整 → 闸门 → 铸凭证。
type Chain struct {
	inner harness.HookRunner
	deps  ChainDeps
	// recentDenied 记录「刚被本链拒绝的 (tool|digest)」→ 过期时刻。
	//
	// 为什么要有：治理拒绝不是工具故障。PostToolUse 只看 isErr，链 deny 后 SDK
	// 回传的错误也会 isErr=true ⇒ 拒绝被喂进熔断计数 ⇒ normalizer 故障 ⇒ 全 deny
	// ⇒ deny 又加速熔断 ⇒ 正常工具被整只拉黑（v0.11.0 E2E 实证的恶性循环）。
	// 治理拒绝只审计、不计熔断；真正执行了且执行层出错才计数。
	recentDenied sync.Map // map[string]time.Time
}

// noteDeniedByName 标记「这次调用被链拒绝了」（2 分钟窗口，足够覆盖 SDK 回调）。
func (c *Chain) noteDeniedByName(tool, payloadDigest string) {
	if payloadDigest == "" {
		return
	}
	c.recentDenied.Store(tool+"|"+payloadDigest, time.Now().Add(2*time.Minute))
}

// deniedRecently 报告这次调用是否刚被链拒绝（是 ⇒ 不喂熔断）。
func (c *Chain) deniedRecently(tool string, input []byte) bool {
	digest := CanonicalDigest(input)
	if digest == "" {
		return false
	}
	key := tool + "|" + digest
	v, ok := c.recentDenied.Load(key)
	if !ok {
		return false
	}
	if ts, ok := v.(time.Time); ok && time.Now().Before(ts) {
		return true
	}
	c.recentDenied.Delete(key)
	return false
}

// ChainDeps 是链的依赖装配。
type ChainDeps struct {
	Normalizer *Normalizer
	Gate       *Gate
	Tokens     *TokenStore
	Auditor    *Auditor
	Cache      *Cache
	Assets     *db.AssetStore
	// DB 用于熔断计数（PostToolUse）与对抗复核队列的读取。
	DB *db.DB
	// NotifyReview 通知 redteam 取走 needs_review（可为 nil）。
	NotifyReview func(ctx context.Context, item *db.AdversarialReviewItem)
	// AgentKeyOf 从 ctx 里取当前 agent key。
	//
	// ⚠️ 它为 nil ⇒ 这条链**没有接到受管路径上** ⇒ 不 gate（放行）。这不是
	// 「查不出来所以放行」，而是「这条链根本没被装到 worker 的工具入口上」：
	// 宿主（server/sod_chain.go）必须保证受管路径上它非 nil，装配测试要钉住
	// 这一条。反过来，链一旦被装上（AgentKeyOf 非 nil），其余任何部件缺失都
	// 走 fail-closed 拒绝。
	AgentKeyOf func(ctx context.Context) string

	// DegradeEnabled 读降级开关（方案 v3 缺陷 B4，settings sod.degrade_enabled，
	// 默认 false）。nil = 永不降级（fail-closed 原样）。
	DegradeEnabled func() bool
	// DegradedAllow 判定降级期哪些工具可以确定性放行。宿主注入分级器：
	// 只有 Tier1/2 类（平台只读 + 结构化写）返回 true；Tier3（Bash/出网/
	// MCP/扩权/状态迁移）必须 false —— 降级**永不**豁免 Tier3（§4 不变式 1）。
	DegradedAllow func(toolName string) bool
	// OnDegradeChange 降级态变化回调（进/出降级时各一次，宿主发通知用）。可为 nil。
	OnDegradeChange func(active bool)

	// ---- v4.1 合并闸（docs/方案-审计裁决与职责分离合并-v4.1.md §1）----
	// MergedGateMode 读三态开关（off/shadow/enforce，settings sod.merged_gate）。
	// nil = off（旧链原样——装配未完成时的安全默认）。
	MergedGateMode func() string
	// IntelSource 情报源白名单（settings sod.intel_sources，默认 17 源）。
	// nil = 情报源放行不可用（crt.sh 等照旧进范围判定——方向安全）。
	IntelSource func(host string) bool
	// Categories 返回种类表快照。nil = 空表，全部走 auditor 默认裁量岗。
	Categories func() ([]sodpolicy.SodCategory, uint64)
	// Adjudicator 是 auditor 合并闸裁决岗（server 装配：并发闸+缓存+LLM+建议
	// 队列）。与上面的 Auditor（审计账本）是两回事。nil = auditor 类动作走
	// 种类 fallback。
	Adjudicator func(ctx context.Context, req AuditorRequest) (AuditorVerdict, error)
	// ElevateAsk 人工 ask 通路（server 装配：interceptor.HandleAsk + 群聊卡）。
	// nil = ask 一律视为未批准（fail-safe 拒绝）。
	ElevateAsk func(ctx context.Context, req AskRequest) (approved bool, by string, err error)
	// CategoryUsed 查询 (task, category) 的已用次数（账本计数，重启不丢）。
	// nil = cap 不生效。
	CategoryUsed func(ctx context.Context, taskID int64, category string) (int, error)
	// RecordShadowDiff 影子模式对照账本。nil = 影子模式不记差异。
	RecordShadowDiff func(ShadowDiff)
	// OnBaselineDestructive 是锁定基线命中（删除/破坏）的宿主回调（方案 v6
	// C1，用户拍板「超出界限外的删除动作就停」）：宿主据此自动暂停任务+播报。
	// nil = 只拦动作不停任务。guard 基线预检命中由宿主侧独立接同一语义。
	OnBaselineDestructive func(taskID int64, toolName, hit string)

	// OnCapHit 是种类配额触发的宿主回调（方案 v9 批次1⑥，业内对齐 Claude
	// Code hooks 的事件驱动思想）：cap 命中即刻通知宿主直触一次监督轮，不再
	// 等 3min 轮询空窗。nil = 只走裁决不通知。
	OnCapHit func(taskID int64, category string)

	// OnNormalizerEvent 是规整器失败事件回调（方案 v9.2 §3.5-F3）：宿主据此把
	// normalizer 失败补记进 llm_records（worker=normalizer，与 auditor 治理线
	// 同口径——此前该通道只落 reason_code，llm_records 黑盒）。nil = 只走日志
	// 采样不落账。
	OnNormalizerEvent func(ev NormalizerEvent)
}

// NewChain 构造链。inner 为 nil 时链独立工作（测试用）。
func NewChain(inner harness.HookRunner, deps ChainDeps) *Chain {
	return &Chain{inner: inner, deps: deps}
}

// Wrap 返回一个绑定了指定 inner 的链视图：deps 共享（同一份凭证表/规整器/闸门），
// inner 独立。
//
// 为什么需要它：链是**进程级单例**，但 inner（宿主的 guard）是**每任务**的 ——
// 每个任务有自己的 guard。宿主于是持有唯一一条链，按任务派生视图：视图共享
// 同一份 deps（同一张凭证表、同一份规整器、同一道闸门、同一份缓存、同一个
// 审计器），只换一个 inner。
//
// ⚠️ 共享必须是**同一张凭证表**（同一指针），不是副本：链铸凭证、执行器消费
// 凭证，两边必须落在同一张表里，否则被批准的动作在执行器侧查不到凭证 ⇒
// 全部被拒（fail-closed，但链等于瘫了）。
//
// Wrap(nil) 是合法的：返回的视图 inner 为空，等价于 NewChain(nil, c.deps)。
func (c *Chain) Wrap(inner harness.HookRunner) *Chain {
	if c == nil {
		return NewChain(inner, ChainDeps{})
	}
	return &Chain{inner: inner, deps: c.deps}
}

// chainCall 是一次工具调用在链内的上下文（把一串参数收成一个值对象）。
type chainCall struct {
	toolName       string
	sourceAgent    string
	taskID         int64
	conversationID int64
	payload        []byte
	payloadDigest  string
	scopeDigest    string
	started        time.Time
}

// PreToolUse 是链的入口。
//
// 顺序：inner → （inner block 则直接返回）→ 规整 → 闸门 → 铸凭证。
//
// 返回契约：(block, message, updated)。approve 时返回 updated（inner 改写后
// 的字节）—— 必须返回它：harness 是靠 updated 把改写后的入参喂给工具的
// （third_party/norma/harness/tools.go:78-84），不返回就会让工具跑**旧**
// 字节，而凭证绑的是**新**字节 ⇒ 摘要不匹配 ⇒ 执行器拒绝（虽然 fail-closed，
// 但每次调用都会被误拒，链等于瘫了）。
func (c *Chain) PreToolUse(ctx context.Context, name string, input []byte) (bool, string, []byte) {
	payload := input
	var updated []byte

	// ① inner 优先（guard 的短路表 / 拦截规则 / 人工 ask）。
	if c.inner != nil {
		block, msg, u := c.inner.PreToolUse(ctx, name, input)
		if block {
			return true, msg, nil
		}
		if len(u) > 0 {
			updated = u
			payload = u
		}
	}

	// ② 链没被装到受管路径上 ⇒ 不 gate。
	if c.deps.AgentKeyOf == nil {
		return false, "", updated
	}

	// ③ 放行清单里的工具不进链（本地只读 / 平台只读 / 纯记账）。
	if !GatedTool(name) {
		return false, "", updated
	}

	run := currentRunContext(ctx)
	// agent 分量必须与执行器查表时用的**逐字节相同**：执行器取的是运行上下文
	// 里的原值，所以这里也不修剪、不改写 —— 修剪会让两端差一个空白 ⇒
	// 凭证查不到 ⇒ 被批准的动作全被拒。
	agentKey := c.deps.AgentKeyOf(ctx)
	if strings.TrimSpace(agentKey) == "" {
		agentKey = run.AgentKey
	}
	call := chainCall{
		toolName:       name,
		sourceAgent:    agentKey,
		taskID:         run.TaskID,
		conversationID: run.ConversationID,
		payload:        payload,
		payloadDigest:  CanonicalDigest(payload),
		started:        time.Now(),
	}

	// ④ 任务上下文缺失 ⇒ 无法判定授权范围 ⇒ 拒绝（fail-closed）。
	// 查不出来不等于没有限制 —— 与 db/quota.go 的既有契约一字不差。
	if call.taskID <= 0 {
		return c.deny(ctx, call, sodpolicy.ReasonScopeUnavailable,
			"任务上下文缺失，无法判定授权范围，按失败关闭处理")
	}

	// ⑤ 范围摘要（缓存键的一个分量）：范围一变 ⇒ 缓存必失效。
	scopeDigest, err := c.scopeDigest(call.taskID)
	if err != nil {
		return c.deny(ctx, call, sodpolicy.ReasonScopeUnavailable,
			"范围摘要不可用，按失败关闭处理")
	}
	call.scopeDigest = scopeDigest

	// ⑤.5 人工复核改判放行（方案 v3 缺陷 F3）：sod_manual_overrides 里的凭据
	// 四元组 (tool, task, agent, payload_digest) 全等命中且未过期 ⇒ 跳过规整与
	// 裁决，直接铸执行凭证放行。call-precision：换一个字节就不命中；once 型
	// 用后即焚；DB 持久，重启不丢。审计如实记 override 放行（不变式 4）。
	// Tier3 永不进这张表（review API 在写入侧拒绝）——链侧不做二次判断，表的
	// 内容即人工意愿的完整边界。
	if c.deps.DB != nil && call.taskID > 0 {
		ovr, oerr := c.deps.DB.LookupSodManualOverride(call.toolName, call.taskID, call.sourceAgent, call.payloadDigest)
		if oerr != nil {
			log.Printf("[sod] 查人工改判凭据失败（按无凭据继续）: %v", oerr)
		}
		if oerr == nil && ovr != nil {
			if c.deps.Tokens == nil {
				return c.deny(ctx, call, sodpolicy.ReasonTokenInvalid, "执行凭证存储未装配，按失败关闭处理")
			}
			_, digest, terr := c.deps.Tokens.Mint(call.payloadDigest, call.taskID, call.sourceAgent, call.toolName)
			if terr != nil {
				return c.deny(ctx, call, sodpolicy.ReasonTokenInvalid, "无法生成执行凭证，按失败关闭处理")
			}
			if ovr.Scope == "once" {
				_ = c.deps.DB.ConsumeSodManualOverride(ovr.ID) // 用后即焚（焚不掉最多多放一次，凭证本身仍是一次性）
			}
			rec := &DecisionRecord{
				TaskID:         call.taskID,
				ConversationID: call.conversationID,
				SourceAgent:    call.sourceAgent,
				ToolName:       call.toolName,
				Verdict:        sodpolicy.VerdictApprove,
				ReasonCode:     sodpolicy.ReasonAllChecksPassed,
				Note:           fmt.Sprintf("人工复核改判放行（override #%d，scope=%s，by=%s）", ovr.ID, ovr.Scope, ovr.CreatedBy),
				PayloadDigest:  call.payloadDigest,
				PayloadPreview: RedactPreview(call.payload, sodpolicy.PayloadPreviewMaxBytes),
				Checks:         map[string]string{"manual_override": "true"},
				ScopeDigest:    call.scopeDigest,
				TokenDigest:    digest,
				LatencyMs:      int(time.Since(call.started) / time.Millisecond),
			}
			c.record(ctx, rec)
			return false, "", updated
		}
	}

	// ⑤.8 锁定基线（v4.1 §10-F1 防御性复查）：guard 预检已在一切 allow 短路
	// 之前拦过一次；这里兜「guard 未装配 / 被 env 停用」的路径，并让基线拒绝
	// 也留 sod 账本行（可回放、可统计）。
	if bReason, bHit := sodpolicy.BaselineHit(call.toolName, call.payload); bReason != "" {
		if bReason == sodpolicy.ReasonDestructiveAction && c.deps.OnBaselineDestructive != nil {
			c.deps.OnBaselineDestructive(call.taskID, call.toolName, bHit)
		}
		return c.deny(ctx, call, bReason, "锁定基线命中（"+bHit+"）：删除/破坏、平台凭据与平台自身配置不可被任何机制放行")
	}

	// ⑥ 合并闸三态（v4.1 §1）。off=旧链；shadow=旧链权威+新链对照（§11-G1）；
	// enforce=新链裁决（LLM 规整器退役，管线见 mergedgate.go）。
	mode := sodpolicy.MergedGateOff
	if c.deps.MergedGateMode != nil {
		mode = c.deps.MergedGateMode()
	}
	if mode == sodpolicy.MergedGateEnforce {
		return c.enforceGate(ctx, call, name)
	}

	// ---- 旧链（off / shadow 的权威路径）：规整 → 闸门 → 裁决 ----
	// 规整（失败 ⇒ fail-closed）。四类确定性豁免跳过规整：
	//   - 任务内结构化写（B2，见 taskInternalWriteTools）；
	//   - 本地任务目录 Bash（B5）：命令引用的全部绝对路径都在任务工作区内——
	//     出界路径只要有一个就走完整流程，绝不放宽；
	//   - 确定性快路径（方案 v10 §14 R1）：形态上完全确定的安全只读出网
	//     （curl 纯 GET/HEAD、nmap -sn/-p、dig/nslookup/whois/host、WebFetch）。
	//     豁免的是 LLM 规整不是闸门裁决 —— 五道检查照跑，scope 用快路径提取的
	//     TargetHint 判定；审计带 fast_path 独立标记（§4 不变式 4）。
	//   - 宿主本地桌面动作（0.12.1 A4）：工具属桌面控制面且任务的宿主授权集合
	//     非空（tasks.host_targets）——桌面载荷只有 ref，规整器产不出目标线索
	//     （编造目标反而制造不稳定放行），豁免规整、scope 走第五分支。集合未
	//     配置的桌面动作在规整之前就以独立理由拒绝（与「无可判定目标」区分）。
	localBash := name == "Bash" && BashLocalTaskDir(call.taskID, payload)
	taskInternal := TaskInternalWrite(call.toolName)
	fastPath, fastHit := DeterministicFastPath(name, payload)
	// 桌面识别按【解包后的内层名】——延迟桌面工具的外层名是 ExecuteExtraTool，
	// 载荷 tool_name 才是实际执行的工具（与 guard 侧 intercept.UnwrapDeferredName
	// 同一语义；一致性由 sod 测试与 intercept 包互验钉死）。
	effName := deferredInnerName(name, payload)
	desktopHost := db.IsDesktopToolName(effName)
	hostTargets, htErr := c.desktopHostTargets(call.taskID)
	if desktopHost && (htErr != nil || len(hostTargets) == 0) {
		// 独立拒绝路径：区分「动作作用于未授权/未配置的宿主清单」与「无可判定
		// 目标」——前者是授权管理问题（先配 host_targets），后者是载荷形态问题。
		return c.deny(ctx, call, sodpolicy.ReasonOutOfScope,
			"宿主桌面动作未在本任务授权清单内（host_targets 未配置）——先在任务上配置授权窗口/应用，或改走会话路径")
	}
	desktopLocal := desktopHost && len(hostTargets) > 0
	var (
		normalized NormalizerResult
		cacheHit   bool
		patternHit bool
	)
	degradeOn := false
	if fastHit && !taskInternal && !localBash {
		normalized, cacheHit = fastPath, false
	}
	if !taskInternal && !localBash && !fastHit && !desktopLocal {
		normalized, cacheHit, patternHit, err = c.normalize(ctx, call)
		if err != nil {
			if errors.Is(err, ErrNormalizerUnavailable) {
				// B4 降级档：开关开启且连续不可用达阈值 ⇒ Tier1/2 类动作确定性
				// 放行（闸门其余检查照跑）；Tier3 照旧 fail-closed 拒绝。
				degrade.recordUnavailable(c.deps.DegradeEnabled, c.deps.OnDegradeChange)
				if degrade.isActive(c.deps.DegradeEnabled) && c.deps.DegradedAllow != nil && c.deps.DegradedAllow(call.toolName) {
					degradeOn = true
					normalized = NormalizerResult{}
					cacheHit = false
				} else {
					c.shadowCompare(call, GateOutput{Verdict: sodpolicy.VerdictDeny, ReasonCode: sodpolicy.ReasonNormalizerUnavailable})
					return c.deny(ctx, call, sodpolicy.ReasonNormalizerUnavailable, "意图规整失败，按失败关闭处理")
				}
			} else {
				if errors.Is(err, ErrNormalizerMalformed) {
					c.shadowCompare(call, GateOutput{Verdict: sodpolicy.VerdictDeny, ReasonCode: sodpolicy.ReasonNormalizerMalformed})
					return c.deny(ctx, call, sodpolicy.ReasonNormalizerMalformed, "意图规整失败，按失败关闭处理")
				}
				degrade.recordUnavailable(c.deps.DegradeEnabled, c.deps.OnDegradeChange)
				c.shadowCompare(call, GateOutput{Verdict: sodpolicy.VerdictDeny, ReasonCode: sodpolicy.ReasonNormalizerUnavailable})
				return c.deny(ctx, call, sodpolicy.ReasonNormalizerUnavailable, "意图规整失败，按失败关闭处理")
			}
		} else {
			degrade.recordAvailable(c.deps.DegradeEnabled, c.deps.OnDegradeChange)
		}
	}

	// ⑦ 确定性闸门。**每次调用都重跑全部五道检查** —— 缓存里只有规整结果，
	// 裁决依赖实时状态，缓存它等于用过期状态放行。
	out := c.evaluate(ctx, call, normalized, degradeOn)
	if mode == sodpolicy.MergedGateShadow {
		// 影子对照（§11-G1）：新链只记差异，最终动作仍由旧链给出。
		c.shadowCompare(call, out)
	}

	// §4 不变式 4：所有放行旁路带独立审计标记。任务内结构化写跳过了 LLM 规整
	// —— 这件事必须能从审计里单独回放出来，而不是混在「全部通过」里。
	markers := map[string]string{}
	if taskInternal || localBash || desktopLocal {
		markers["bypassed_normalizer"] = "true"
		if localBash {
			markers["bypass_reason"] = "local_task_dir"
		}
		if desktopLocal {
			markers["bypass_reason"] = "host_local_action"
		}
	}
	if fastHit && !taskInternal && !localBash {
		markers["fast_path"] = "true"
	}
	if patternHit {
		markers["fast_cache"] = "true"
	}
	if degradeOn {
		markers["degraded"] = "true"
	}
	return c.finalize(ctx, call, out, normalized, cacheHit, degradeOn, markers, updated)
}

// finalize 构建审计记录并执行裁决副作用（铸凭证 / 对抗复核 / 拒绝）。
// 旧链（off/shadow）与合并闸（enforce）共用这一个出口——账本与凭证语义只有
// 一份，两条管线不允许各自为政。
func (c *Chain) finalize(ctx context.Context, call chainCall, out GateOutput, normalized NormalizerResult, cacheHit, degradeOn bool, extraChecks map[string]string, updated []byte) (bool, string, []byte) {
	intentJSON, err := json.Marshal(normalized.StructuredIntent)
	if err != nil {
		intentJSON = []byte("{}")
	}
	rec := &DecisionRecord{
		TaskID:           call.taskID,
		ConversationID:   call.conversationID,
		SourceAgent:      call.sourceAgent,
		ToolName:         call.toolName,
		Verdict:          out.Verdict,
		ReasonCode:       out.ReasonCode,
		Note:             out.Note,
		PayloadDigest:    call.payloadDigest,
		PayloadPreview:   RedactPreview(call.payload, sodpolicy.PayloadPreviewMaxBytes),
		IntentDigest:     CanonicalDigest(intentJSON),
		AnomalyFlag:      normalized.AnomalyFlag,
		AnomalyReason:    normalized.AnomalyReason,
		Checks:           out.Checks,
		ScopeDigest:      call.scopeDigest,
		CacheHit:         cacheHit,
		LatencyMs:        int(time.Since(call.started) / time.Millisecond),
		StructuredIntent: intentJSON,
	}
	if len(extraChecks) > 0 && rec.Checks != nil {
		for k, v := range extraChecks {
			rec.Checks[k] = v
		}
	}

	switch out.Verdict {
	case sodpolicy.VerdictApprove:
		// ⑧ 铸一次性执行凭证（绑定**将要执行的那份字节**的摘要）。
		//
		// token 本体只在这一行存在过，之后**不再被任何人持有** —— 执行器靠
		// 四元组索引取用它（TokenStore.Take）。token 本体永不落库、永不进
		// 日志、永不进模型上下文。
		if c.deps.Tokens == nil {
			rec.Verdict = sodpolicy.VerdictDeny
			rec.ReasonCode = sodpolicy.ReasonTokenInvalid
			rec.Note = "执行凭证存储未装配，按失败关闭处理"
			c.record(ctx, rec)
			return true, blockMessage(rec), nil
		}
		_, digest, err := c.deps.Tokens.Mint(call.payloadDigest, call.taskID, call.sourceAgent, call.toolName)
		if err != nil {
			rec.Verdict = sodpolicy.VerdictDeny
			rec.ReasonCode = sodpolicy.ReasonTokenInvalid
			rec.Note = "无法生成执行凭证，按失败关闭处理"
			c.record(ctx, rec)
			return true, blockMessage(rec), nil
		}
		rec.TokenDigest = digest
		c.record(ctx, rec)
		return false, "", updated
	case sodpolicy.VerdictNeedsReview:
		// ⑨ needs_review：**不执行**、**不进人工队列**，落对抗复核队列
		// （审计器在 Record 里入队），并即时通知 redteam。
		c.record(ctx, rec)
		c.notifyReview(ctx, rec, intentJSON)
		c.noteDeniedByName(call.toolName, call.payloadDigest)
		return true, blockMessage(rec), nil
	default: // deny
		c.record(ctx, rec)
		c.noteDeniedByName(call.toolName, call.payloadDigest)
		return true, blockMessage(rec), nil
	}
}

// PostToolUse 记工具成败（喂熔断计数），并转发给 inner。
func (c *Chain) PostToolUse(ctx context.Context, name string, input, result []byte, isErr bool) {
	if c.deps.DB != nil && GatedTool(name) {
		// 治理拒绝（本次调用根本没执行）不喂熔断：只有真执行了的失败才计数。
		if c.deniedRecently(name, input) {
			c.recentDenied.Delete(name + "|" + CanonicalDigest(input))
		} else if err := c.deps.DB.RecordToolCallOutcome(name, isErr); err != nil {
			log.Printf("[sod] 记录工具调用结果失败: tool=%s err=%v", name, err)
		}
	}
	if c.inner != nil {
		c.inner.PostToolUse(ctx, name, input, result, isErr)
	}
}

// Stop 转发给 inner（链不改变收场语义）。
func (c *Chain) Stop(ctx context.Context, messages []llm.Message) (bool, []string, string) {
	if c.inner == nil {
		return false, nil, ""
	}
	return c.inner.Stop(ctx, messages)
}

// scopeDigest 取任务的有效范围摘要。
func (c *Chain) scopeDigest(taskID int64) (string, error) {
	if c.deps.Assets == nil {
		return "", errors.New("资产库未装配，范围摘要不可用")
	}
	return c.deps.Assets.ScopeDigestForTask(taskID)
}

// normalize 取规整结果：命中缓存就直接用，未命中才调模型。
//
// 返回的第二个值是「是否命中缓存」—— 它进审计（cache_hit），让人能看出
// 这次裁决是省了一次模型调用还是真跑了模型。
// 返回的第三个值是「是否命中模式缓存」（方案 v10 §14 R2）—— 调用方据此落
// fast_cache 审计标记，让人能看出这次是精确复用还是模式复用。
//
// 查序：精确键 → 模式键（同工具 + 同 host + 同方法 ⇒ 同规整形态）→ 模型。
// 规整成功后精确键与模式键同落。
//
// malformed 重试一次（方案 v3 缺陷 B3③）：temperature=0 的输出仍可能偶发
// 不合规（截断/漏字段）；同输入重试一次成本可控，能把一次抖动从
// normalizer_malformed 拒绝里救回来。unavailable 不重试——那是模型侧故障，
// 立即交给调用方的失败路径（信号量+独立档已在 B3①② 缓解它本身）。
func (c *Chain) normalize(ctx context.Context, call chainCall) (NormalizerResult, bool, bool, error) {
	if c.deps.Cache == nil || c.deps.Normalizer == nil {
		return NormalizerResult{}, false, false, fmt.Errorf("%w: 规整器未装配", ErrNormalizerUnavailable)
	}
	key := c.deps.Cache.Key(call.toolName, call.payloadDigest, call.taskID, call.scopeDigest, call.sourceAgent)
	if cached, ok := c.deps.Cache.Get(key); ok {
		return cached, true, false, nil
	}
	// R2 模式键：精确键未命中 → 查模式键。键里的 host 出自与 scope 判定同一
	// 套确定性提取 ⇒ 模式命中的意图所指向的目标 == 本次载荷词法指向的目标，
	// 闸门照常对它判实时范围（缓存只有线索、没有裁决，见 cache.go PatternKey）。
	patternKey, hasPattern := c.deps.Cache.PatternKey(call.toolName, call.payload)
	if hasPattern {
		if cached, ok := c.deps.Cache.Get(patternKey); ok {
			return cached, true, true, nil
		}
	}
	doNormalize := func() (NormalizerResult, error) {
		return c.deps.Normalizer.Normalize(ctx, NormalizeInput{
			ToolName:       call.toolName,
			RawPayload:     call.payload,
			SourceAgent:    call.sourceAgent,
			TaskID:         call.taskID,
			ConversationID: call.conversationID,
		})
	}
	normalizeStarted := time.Now()
	normalized, err := doNormalize()
	if errors.Is(err, ErrNormalizerMalformed) {
		normalized, err = doNormalize()
	}
	if err != nil {
		// F3：失败事件回传宿主（llm_records 补记；日志采样在 normalizer 内）。
		if c.deps.OnNormalizerEvent != nil {
			kind := "unavailable"
			if errors.Is(err, ErrNormalizerMalformed) {
				kind = "malformed"
			}
			detail := err.Error()
			if len(detail) > 200 {
				detail = detail[:200]
			}
			c.deps.OnNormalizerEvent(NormalizerEvent{
				Kind:      kind,
				ToolName:  call.toolName,
				TaskID:    call.taskID,
				Detail:    detail,
				LatencyMs: time.Since(normalizeStarted).Milliseconds(),
			})
		}
		return NormalizerResult{}, false, false, err
	}
	c.deps.Cache.Put(key, normalized)
	// R2：规整成功 ⇒ 精确键与模式键同落（不同载荷、同 host/方法 ⇒ 下次直接
	// 模式命中，再省一次模型调用）。
	if hasPattern {
		c.deps.Cache.Put(patternKey, normalized)
	}
	return normalized, false, false, nil
}

// evaluate 跑确定性闸门。degraded 由调用方传入（B4 降级档放行的调用）。
func (c *Chain) evaluate(ctx context.Context, call chainCall, normalized NormalizerResult, degraded bool) GateOutput {
	if c.deps.Gate == nil {
		return GateOutput{
			Verdict:    sodpolicy.VerdictDeny,
			ReasonCode: sodpolicy.ReasonScopeUnavailable,
			Note:       "闸门未装配，按失败关闭处理",
			Checks:     map[string]string{},
		}
	}
	return c.deps.Gate.Evaluate(ctx, GateInput{
		ToolName:          call.toolName,
		SourceAgent:       call.sourceAgent,
		TaskID:            call.taskID,
		ConversationID:    call.conversationID,
		PayloadDigest:     call.payloadDigest,
		ScopeDigest:       call.scopeDigest,
		Normalized:        normalized,
		FindingID:         findingIDFromInput(call.payload),
		TaskInternalWrite: TaskInternalWrite(call.toolName),
		LocalTaskDir:      call.toolName == "Bash" && BashLocalTaskDir(call.taskID, call.payload),
		HostLocalAction:   c.hostLocalAction(call.taskID, call.toolName, call.payload),
		Degraded:          degraded,
	})
}

// deferredInnerName 从 ExecuteExtraTool 载荷取内层工具名（非转发器调用回退原名）。
// 语义与 intercept.UnwrapDeferredName 一致——两包各持一份实现（internal/sod 不
// import intercept 以保持依赖纪律），一致性由 sod 测试与 intercept 包互验钉死。
func deferredInnerName(tool string, payload []byte) string {
	if tool != "ExecuteExtraTool" {
		return tool
	}
	var probe struct {
		ToolName string `json:"tool_name"`
	}
	if err := json.Unmarshal(payload, &probe); err != nil || probe.ToolName == "" {
		return tool
	}
	return probe.ToolName
}

// desktopHostTargets 读任务级宿主授权集合（tasks.host_targets）。deps.DB 未装配
// 或读失败按「未授权」处理（fail-closed：授权读不出来 ⇒ 不放行桌面动作）。
func (c *Chain) desktopHostTargets(taskID int64) ([]string, error) {
	if c.deps.DB == nil {
		return nil, fmt.Errorf("依赖库未装配")
	}
	targets, ok, err := c.deps.DB.TaskHostTargets(taskID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}
	return targets, nil
}

// hostLocalAction 判定本次调用是否走宿主本地动作分支（闸门第五类）：工具属桌面
// 控制面（解包后的内层名）且任务授权集合非空。读失败一律 false（fail-closed）。
func (c *Chain) hostLocalAction(taskID int64, tool string, payload []byte) bool {
	if !db.IsDesktopToolName(deferredInnerName(tool, payload)) {
		return false
	}
	targets, err := c.desktopHostTargets(taskID)
	if err != nil || len(targets) == 0 {
		return false
	}
	return true
}

// deny 记一次拒绝并返回拒绝消息。
func (c *Chain) deny(ctx context.Context, call chainCall, reasonCode, note string) (bool, string, []byte) {
	rec := &DecisionRecord{
		TaskID:         call.taskID,
		ConversationID: call.conversationID,
		SourceAgent:    call.sourceAgent,
		ToolName:       call.toolName,
		Verdict:        sodpolicy.VerdictDeny,
		ReasonCode:     reasonCode,
		Note:           note,
		PayloadDigest:  call.payloadDigest,
		PayloadPreview: RedactPreview(call.payload, sodpolicy.PayloadPreviewMaxBytes),
		ScopeDigest:    call.scopeDigest,
		LatencyMs:      int(time.Since(call.started) / time.Millisecond),
	}
	c.record(ctx, rec)
	// 治理拒绝不计熔断（见 Chain.recentDenied）。
	c.noteDeniedByName(call.toolName, call.payloadDigest)
	return true, blockMessage(rec), nil
}

// record 写审计留痕（best-effort：留痕失败不改变已经发生的裁决）。
func (c *Chain) record(ctx context.Context, rec *DecisionRecord) {
	if c.deps.Auditor == nil {
		log.Printf("[sod] 审计器未装配，裁决未留痕: verdict=%s reason=%s", rec.Verdict, rec.ReasonCode)
		return
	}
	c.deps.Auditor.Record(ctx, rec)
}

// notifyReview 通知 redteam 去证伪。
//
// 入队由审计器完成（Auditor.Record → EnqueueReview ⇒ sod_adversarial_review_pending）；
// 这里只负责「即时触发」那条腿：把复核材料交给宿主，由宿主拉起 redteam。
func (c *Chain) notifyReview(ctx context.Context, rec *DecisionRecord, intentJSON []byte) {
	if c.deps.NotifyReview == nil {
		return
	}
	c.deps.NotifyReview(ctx, &db.AdversarialReviewItem{
		TaskID:           nilInt64(rec.TaskID),
		ConversationID:   nilInt64(rec.ConversationID),
		SourceAgent:      rec.SourceAgent,
		ToolName:         rec.ToolName,
		PayloadDigest:    rec.PayloadDigest,
		PayloadPreview:   rec.PayloadPreview,
		AnomalyFlag:      rec.AnomalyFlag,
		AnomalyReason:    rec.AnomalyReason,
		ReasonCode:       rec.ReasonCode,
		StructuredIntent: intentJSON,
	})
}

// taskInternalWriteTools 是「写目标固定在本任务图/回执/群信箱」的结构化写工具
// （方案 v3 缺陷 B2）：record_fact / record_progress 写任务探索图与回执表；
// add_hint 追加任务提示；message_group 写群信箱。
//
// 为什么它们跳过 LLM 规整：规整器的职责是把「自由文本动作」转成带外部目标的
// 结构化意图；这些动作没有外部目标（写进本任务自己的账本），LLM 在这里提供的
// 唯一「价值」是把基础设施故障变成安全拒绝——实测一次任务 113 次 record_fact
// 全走规整，规整器一抖（37/54 unavailable）整条写回链路瘫痪。
//
// ⚠️ 放行的是「跳过规整」，不是「跳过闸门」：熔断/额度/tier2 检查照跑，
// scope 检查按构造通过（理由码 pass(task_internal_write)），审计带
// bypassed_normalizer 标记（§4 不变式 4：所有放行旁路带独立审计标记）。
// insert_assets / add_task_scope 等扩权工具**不在**本清单——它们照旧进规整。
var taskInternalWriteTools = map[string]bool{
	"record_fact":     true,
	"record_progress": true,
	"add_hint":        true,
	"message_group":   true,
}

// TaskInternalWrite 报告该工具是否为任务内结构化写（跳过规整、scope 按构造通过）。
func TaskInternalWrite(name string) bool { return taskInternalWriteTools[name] }

// localPathTokenRe 抽取载荷里的绝对路径 token。前界排除 [A-Za-z0-9+:._\-/]：
// 'http://host/x' 的 '/' 前是 ':' 或 '/' ⇒ 不算路径；'-o /tmp/x' 的 '/' 前是
// 空白 ⇒ 算。B5 的判定输入。
var localPathTokenRe = regexp.MustCompile(`(?:^|[^A-Za-z0-9+:._\-/])(/[A-Za-z0-9._\-]+(?:/[A-Za-z0-9._\-]+)*)`)

// networkIndicatorRe 是「这条命令在碰网络」的确定性指示符（B5 的反向守卫）：
// URL 方案 / IP 字面量 / host:port / 裸域名。命中任何一个 ⇒ 不豁免（照旧走
// 完整 scope 判定——`curl http://x --output /app/data/tasks/10/x` 这种把出网
// 藏在本地产物后面的写法必须被拦住）。裸域名的误伤（bare 文件名如 flag.txt
// 被当域名）只影响「少一条豁免」，方向安全。
var networkIndicatorRe = regexp.MustCompile(`(?i)(?:https?|ftp|wss?)://|\b\d{1,3}(?:\.\d{1,3}){3}\b|\b[a-z0-9.-]+\.[a-z]{2,}:\d{2,5}\b|(?:^|[^/A-Za-z0-9._\-])[a-z0-9-]+(?:\.[a-z0-9-]+)+\.[a-z]{2,}\b`)

// BashLocalTaskDir 判定一次 Bash 调用是否【只】引用本任务工作区内的绝对路径
// （方案 v3 缺陷 B5）：载荷里抽出的每个绝对路径 token 都必须落在
// /app/data/tasks/<taskID>/ 前缀下；一个都没有（纯相对路径/纯 flag 命令）或
// 任何一个出界 ⇒ false（照旧走完整 scope 判定，宁多审不放松）。
//
// 出界判定示例：/app/data/tasks/10 不放行任务 11（前缀必须整段命中，后随字符
// 是串尾或 '/'）——防 /app/data/tasks/10-evil 这类前缀混淆。
func BashLocalTaskDir(taskID int64, payload []byte) bool {
	if taskID <= 0 || len(payload) == 0 {
		return false
	}
	// 反向守卫先判：任何网络指示符 ⇒ 不豁免（宁多审不放松）。
	if networkIndicatorRe.Match(payload) {
		return false
	}
	prefix := "/app/data/tasks/" + strconv.FormatInt(taskID, 10)
	found := false
	for _, m := range localPathTokenRe.FindAllStringSubmatch(string(payload), -1) {
		tok := m[1]
		if harmlessSystemPath(tok) {
			continue // stderr/stdout 重定向等 shell 常态不计入（v6.1 修复）
		}
		if tok != prefix && !strings.HasPrefix(tok, prefix+"/") {
			return false // 任何一个出界路径 ⇒ 整体不豁免
		}
		found = true
	}
	return found
}

// GatedTool 报告该工具是否要进 SoD 链。
//
// 用的是【放行清单】（sodpolicy.NotGatedTools）：清单以外的**全部**工具都进链
// ⇒ 未登记的新工具（MCP 的任意名、自定义工具的任意 key）默认被管住。用拦截
// 清单的话，每加一个新工具就等于给它开了一扇默认放行的门。
func GatedTool(name string) bool {
	for _, t := range sodpolicy.NotGatedTools {
		if t == name {
			return false
		}
	}
	return true
}

// blockMessage 生成给模型/人看的拒绝消息。
//
// 消息里带 reason_code：它必须能被机器唯一定位到落在哪道检查上。
func blockMessage(rec *DecisionRecord) string {
	return fmt.Sprintf("[SoD] 拒绝执行：reason_code=%s；%s（工具=%s）", rec.ReasonCode, rec.Note, rec.ToolName)
}

// findingIDKeys 是「本次调用点名了哪个 finding」的判据（代码常量）。
//
// 口径来自「口径统一」第 6 条：「意图 → finding」在数据模型里没有直接外键，
// 唯一可实现且可测的口径是「本次工具入参里点名了哪个 finding」。
var findingIDKeys = []string{"finding_id", "findingId", "node_id", "nodeId"}

// findingIDFromInput 从工具入参里取本次调用关联的 finding id（0 = 不关联）。
//
// 只看顶层键、只认数值：入参结构千差万别，深层遍历既容易误取（某个嵌套的
// id 恰好叫 node_id），又难以解释。
func findingIDFromInput(input []byte) int64 {
	if len(input) == 0 {
		return 0
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(input, &obj); err != nil {
		return 0
	}
	for _, key := range findingIDKeys {
		raw, ok := obj[key]
		if !ok {
			continue
		}
		id, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
		if err != nil || id <= 0 {
			continue
		}
		return id
	}
	return 0
}

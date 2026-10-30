package sod

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
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
}

// NewChain 构造链。inner 为 nil 时链独立工作（测试用）。
func NewChain(inner harness.HookRunner, deps ChainDeps) *Chain {
	return &Chain{inner: inner, deps: deps}
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
	agentKey := strings.TrimSpace(c.deps.AgentKeyOf(ctx))
	if agentKey == "" {
		agentKey = strings.TrimSpace(run.AgentKey)
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

	// ⑥ 规整（失败 ⇒ fail-closed）。
	normalized, cacheHit, err := c.normalize(ctx, call)
	if err != nil {
		reason := sodpolicy.ReasonNormalizerUnavailable
		if errors.Is(err, ErrNormalizerMalformed) {
			reason = sodpolicy.ReasonNormalizerMalformed
		}
		return c.deny(ctx, call, reason, "意图规整失败，按失败关闭处理")
	}

	// ⑦ 确定性闸门。**每次调用都重跑全部五道检查** —— 缓存里只有规整结果，
	// 裁决依赖实时状态，缓存它等于用过期状态放行。
	out := c.evaluate(ctx, call, normalized)

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
		return true, blockMessage(rec), nil
	default: // deny
		c.record(ctx, rec)
		return true, blockMessage(rec), nil
	}
}

// PostToolUse 记工具成败（喂熔断计数），并转发给 inner。
func (c *Chain) PostToolUse(ctx context.Context, name string, input, result []byte, isErr bool) {
	if c.deps.DB != nil && GatedTool(name) {
		if err := c.deps.DB.RecordToolCallOutcome(name, isErr); err != nil {
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
func (c *Chain) normalize(ctx context.Context, call chainCall) (NormalizerResult, bool, error) {
	if c.deps.Cache == nil || c.deps.Normalizer == nil {
		return NormalizerResult{}, false, fmt.Errorf("%w: 规整器未装配", ErrNormalizerUnavailable)
	}
	key := c.deps.Cache.Key(call.toolName, call.payloadDigest, call.taskID, call.scopeDigest, call.sourceAgent)
	if cached, ok := c.deps.Cache.Get(key); ok {
		return cached, true, nil
	}
	normalized, err := c.deps.Normalizer.Normalize(ctx, NormalizeInput{
		ToolName:       call.toolName,
		RawPayload:     call.payload,
		SourceAgent:    call.sourceAgent,
		TaskID:         call.taskID,
		ConversationID: call.conversationID,
	})
	if err != nil {
		return NormalizerResult{}, false, err
	}
	c.deps.Cache.Put(key, normalized)
	return normalized, false, nil
}

// evaluate 跑确定性闸门。
func (c *Chain) evaluate(ctx context.Context, call chainCall, normalized NormalizerResult) GateOutput {
	if c.deps.Gate == nil {
		return GateOutput{
			Verdict:    sodpolicy.VerdictDeny,
			ReasonCode: sodpolicy.ReasonScopeUnavailable,
			Note:       "闸门未装配，按失败关闭处理",
			Checks:     map[string]string{},
		}
	}
	return c.deps.Gate.Evaluate(ctx, GateInput{
		ToolName:       call.toolName,
		SourceAgent:    call.sourceAgent,
		TaskID:         call.taskID,
		ConversationID: call.conversationID,
		PayloadDigest:  call.payloadDigest,
		ScopeDigest:    call.scopeDigest,
		Normalized:     normalized,
		FindingID:      findingIDFromInput(call.payload),
	})
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

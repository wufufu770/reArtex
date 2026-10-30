// 群组预算在计量层的接入：**发请求前预扣（fail-closed），拿到真实 usage 后结算**。
//
// 三段式（LiteLLM 的 reserve → settle → release）：
//
//	预扣 —— 发请求前按**估算**扣住预算。扣不上就**不发请求**（fail closed）。
//	结算 —— 请求返回后按**真实用量**结算：多退少补（多出来的照记，不裁剪）。
//	释放 —— 确定没发出去（或已结清）时把预扣整块还回去。
//
// 关键口径（与 db/llm_usage.go 一致，不另立一套）：
//
//	token 数 = input + output，缓存**已经**含在 input 里，不再另加。
//
// 拒绝路径：预扣失败 ⇒ 构造 llmrec.GroupQuotaRefusal —— 它带 GroupQuotaMarker，
// 已提交的 agent/provider.go 认这个标记并中止请求，用户看到 GroupQuotaRefusalCopy。
// 拒绝消息里**不带任何额度数字**：额度只留在账上（group_quotas /
// group_quota_periods / group_quota_refusals）。
package llmrec

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/llm"
)

// asciiCharsPerToken 是 ASCII 文本"字符数/token"的经验值（4）。
const asciiCharsPerToken = 4

// defaultOutputTokenEstimate 是请求未指定 max_tokens 时的输出侧估算。
//
// 取值偏保守：预扣宁多勿少 —— 少扣会让并发调用一起越过预算线，多扣只是暂时
// 占住额度（结算时按真实用量退回）。
const defaultOutputTokenEstimate = 4096

// QuotaEstimate 是一次模型调用的 token 估算（输入侧估算 + 输出侧上限）。
type QuotaEstimate struct {
	TotalTokens  int64
	InputTokens  int64
	OutputTokens int64
}

// QuotaResolver 把一次模型调用解析到它所属的群组。
//
// ok = false：这次调用**不属于任何群组**（平台里绝大多数调用不在大厅里）——
// 不是错误，不拦截。err != nil：解析失败 —— 调用方必须按拒绝处理（fail closed）。
type QuotaResolver func(ctx context.Context, taskID string) (groupID int64, ok bool, err error)

// QuotaGate 是群组预算的预扣闸门。
type QuotaGate struct {
	pg      *db.DB
	resolve QuotaResolver
	seq     atomic.Uint64
}

// NewQuotaGate 建一个预算闸门。resolve 为 nil 时用 TaskGroupResolver（按 task
// 解析群组）。pg 为 nil 时闸门不生效（未接入预算的部署不受影响）。
func NewQuotaGate(pg *db.DB, resolve QuotaResolver) *QuotaGate {
	if resolve == nil {
		resolve = TaskGroupResolver(pg)
	}
	return &QuotaGate{pg: pg, resolve: resolve}
}

// TaskGroupResolver 按 task registry id 解析群组（默认解析器）。
//
// task id 为空 ⇒ 不属于任何群组（平台对话路径没有 task id）⇒ 不受限。
// task id 解析不出数字 ⇒ **当作解析失败**（fail closed）：宁可不放行，也不
// 因为"看不懂"就跳过预算闸门。
func TaskGroupResolver(pg *db.DB) QuotaResolver {
	return func(ctx context.Context, taskID string) (int64, bool, error) {
		taskID = strings.TrimSpace(taskID)
		if taskID == "" {
			return 0, false, nil
		}
		if pg == nil {
			return 0, false, errors.New("预算闸门没有数据库句柄")
		}
		id, err := strconv.ParseInt(taskID, 10, 64)
		if err != nil || id <= 0 {
			return 0, false, fmt.Errorf("任务 id 不是合法的数字 id")
		}
		gid, err := pg.GroupIDForScope(id, 0)
		if err != nil {
			return 0, false, err
		}
		if gid == 0 {
			return 0, false, nil
		}
		return gid, true, nil
	}
}

// Reserve 为一次模型调用预扣预算，返回预扣 id。
//
// 返回 (0, nil) 表示这次调用不受预算约束（没有闸门，或不属于任何群组）。
// 任何失败都返回错误 —— 调用方据此**拒绝发请求**。
func (g *QuotaGate) Reserve(ctx context.Context, taskID string, est QuotaEstimate) (int64, error) {
	if g == nil || g.pg == nil {
		return 0, nil
	}
	groupID, ok, err := g.resolve(ctx, taskID)
	if err != nil {
		g.pg.RecordQuotaRefusal(0, "", db.QuotaRefusalCheckFailed, taskID)
		return 0, fmt.Errorf("群组预算检查失败: %w", err)
	}
	if !ok {
		return 0, nil
	}
	requestID := g.newRequestID(taskID)
	reservationID, err := g.pg.ReserveGroupQuota(groupID, db.QuotaReservation{
		RequestID:    requestID,
		Tokens:       est.TotalTokens,
		InputTokens:  est.InputTokens,
		OutputTokens: est.OutputTokens,
		TaskID:       taskID,
	})
	if err != nil {
		g.pg.RecordQuotaRefusal(groupID, requestID, quotaRefusalReason(err), taskID)
		return 0, err
	}
	return reservationID, nil
}

// newRequestID 生成一次模型调用的唯一 id（同一个 id 只能预扣一次）。
func (g *QuotaGate) newRequestID(taskID string) string {
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		taskID = "none"
	}
	return fmt.Sprintf("q-%s-%d-%d", taskID, time.Now().UnixNano(), g.seq.Add(1))
}

// Settle 按真实 usage 结算一次预扣。
//
// 口径：total = input + output（缓存已含在 input 里，不重复计）。
func (g *QuotaGate) Settle(reservationID int64, usage llm.Usage) {
	if g == nil || g.pg == nil || reservationID == 0 {
		return
	}
	in, out := int64(usage.InputTokens), int64(usage.OutputTokens)
	_ = g.pg.SettleGroupQuota(reservationID, db.QuotaUsage{
		InputTokens:  in,
		OutputTokens: out,
		CacheRead:    int64(usage.CacheReadTokens),
		CacheWrite:   int64(usage.CacheWriteTokens),
		TotalTokens:  in + out,
	})
}

// SettleDispatchedFailure 结算"请求已发出但没有 usage"的调用。
//
// 按**输入估算**结算而不是记 0：请求确实发出去了，供应商已经计了输入侧，记 0
// 等于白嫖计费窗口。估算优先从预扣行反查（以库里的为准），读不到才用调用点
// 带来的估算。
func (g *QuotaGate) SettleDispatchedFailure(reservationID int64, estimate QuotaEstimate) {
	if g == nil || g.pg == nil || reservationID == 0 {
		return
	}
	input, _, err := g.pg.QuotaReservationEstimate(reservationID)
	if err != nil || input <= 0 {
		input = estimate.InputTokens
	}
	_ = g.pg.SettleGroupQuota(reservationID, db.QuotaUsage{
		InputTokens: input,
		TotalTokens: input,
	})
}

// Release 释放一次预扣（确定没有花出去）。
func (g *QuotaGate) Release(reservationID int64) {
	if g == nil || g.pg == nil || reservationID == 0 {
		return
	}
	_ = g.pg.ReleaseGroupQuota(reservationID)
}

// quotaRefusalReason 把预扣失败映射成机器可读的原因码（不含额度数字）。
func quotaRefusalReason(err error) string {
	switch {
	case errors.Is(err, db.ErrQuotaBudgetExceeded):
		return db.QuotaRefusalBudgetExceeded
	case errors.Is(err, db.ErrQuotaStateNotActive):
		return db.QuotaRefusalStateNotActive
	case errors.Is(err, db.ErrQuotaNoBudget):
		return db.QuotaRefusalNoBudget
	case errors.Is(err, db.ErrQuotaDuplicateRequest):
		return db.QuotaRefusalDuplicate
	default:
		return db.QuotaRefusalCheckFailed
	}
}

// quotaRefusalCause 把预扣失败映射成**可以出现在用户可见文案里**的原因。
//
// 只透传哨兵错误；数据库原始错误（可能含 SQL 文本与数字）不透传 —— 拒绝消息
// 里既不该出现额度数字，也不该出现内部细节。原始错误由账与日志承载。
func quotaRefusalCause(err error) error {
	switch {
	case errors.Is(err, db.ErrQuotaBudgetExceeded):
		return db.ErrQuotaBudgetExceeded
	case errors.Is(err, db.ErrQuotaStateNotActive):
		return db.ErrQuotaStateNotActive
	case errors.Is(err, db.ErrQuotaNoBudget):
		return db.ErrQuotaNoBudget
	case errors.Is(err, db.ErrQuotaDuplicateRequest):
		return db.ErrQuotaDuplicateRequest
	default:
		return errors.New("群组预算检查未通过")
	}
}

// QuotaProvider 在模型调用的前后挂上预算闸门。
//
// 它包在 provider 链里、**recorder 之内**：拒绝发生时 recorder 仍会把它记成
// 一次失败调用（llm_usage 里留痕），而拒绝本身另有 group_quota_refusals 留痕。
type QuotaProvider struct {
	inner llm.Provider
	gate  *QuotaGate
}

// NewQuotaProvider 把预算闸门装到 inner 前面。
func NewQuotaProvider(inner llm.Provider, gate *QuotaGate) *QuotaProvider {
	return &QuotaProvider{inner: inner, gate: gate}
}

// Stream 实现 llm.Provider：迭代前预扣，迭代结束后结算。
func (p *QuotaProvider) Stream(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		reservationID, estimate, err := p.reserve(ctx, req)
		if err != nil {
			yield(llm.StreamEvent{}, err)
			return
		}
		var usage llm.Usage
		dispatched := false
		defer func() { p.settle(reservationID, estimate, dispatched, usage) }()

		for ev, err := range p.inner.Stream(ctx, req) {
			// 请求已经发出（哪怕紧接着就报错）：失败时按输入估算结算，不记 0。
			dispatched = true
			// 用量折叠遵循上游契约（norma/llm/accumulate.go）：message_start 走
			// llm.Usage.Add，message_delta 走 llm.Usage.MergeEcho —— 后者对**回显**
			// 字段取大而不是求和（现役 MiniMax 端点的全部用量只在 message_delta，
			// 见 research-notes §27）。
			switch ev.Type {
			case llm.SEMessageStart:
				usage.Add(ev.Usage)
			case llm.SEMessageDelta:
				usage.MergeEcho(ev.Usage)
			}
			if err != nil {
				yield(ev, err)
				return
			}
			if !yield(ev, nil) {
				return
			}
		}
	}
}

// Complete 实现 llm.Provider：调用前预扣，返回后结算。
func (p *QuotaProvider) Complete(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	reservationID, estimate, err := p.reserve(ctx, req)
	if err != nil {
		return llm.Message{}, "", llm.Usage{}, err
	}
	msg, stopReason, usage, err := p.inner.Complete(ctx, req)
	p.settle(reservationID, estimate, true, usage)
	return msg, stopReason, usage, err
}

// reserve 预扣；失败时返回**带 GroupQuotaMarker 的拒绝错误**。
func (p *QuotaProvider) reserve(ctx context.Context, req llm.CompletionRequest) (int64, QuotaEstimate, error) {
	if p.gate == nil {
		return 0, QuotaEstimate{}, nil
	}
	estimate, err := EstimateTokens(req)
	if err != nil {
		// 估算不出来 ⇒ fail closed（拒绝调用）。**禁止**退回"发出去再查额度"：
		// 那正是估算失败时预算闸门被整条跳过、预算形同虚设的漏洞。
		return 0, QuotaEstimate{}, GroupQuotaRefusal(errors.New("模型请求估算失败"))
	}
	taskID := TaskIDFrom(ctx)
	reservationID, err := p.gate.Reserve(ctx, taskID, estimate)
	if err != nil {
		return 0, QuotaEstimate{}, GroupQuotaRefusal(quotaRefusalCause(err))
	}
	return reservationID, estimate, nil
}

// settle 按三种结局收尾：真实 usage / 输入估算（已发出但没 usage）/ 释放。
func (p *QuotaProvider) settle(reservationID int64, estimate QuotaEstimate, dispatched bool, usage llm.Usage) {
	if reservationID == 0 || p.gate == nil {
		return
	}
	switch {
	case dispatched && (usage.InputTokens > 0 || usage.OutputTokens > 0):
		p.gate.Settle(reservationID, usage)
	case dispatched:
		p.gate.SettleDispatchedFailure(reservationID, estimate)
	default:
		p.gate.Release(reservationID)
	}
}

// EstimateTokens 估算一次模型调用要花的 token：输入侧估算 + 输出侧上限。
//
// 估算**必然**能给出一个值（估不出来就报错 → 调用方按 fail closed 拒绝），
// 但它只是估算：预扣按它扣，结算按真实 usage 调整，差额在结算时结清 ——
// 估算值永远不会被当成最终用量。
func EstimateTokens(req llm.CompletionRequest) (QuotaEstimate, error) {
	input := estimateInputTokens(req)
	output := int64(req.MaxTokens)
	if output <= 0 {
		output = defaultOutputTokenEstimate
	}
	total := input + output
	if total <= 0 {
		return QuotaEstimate{}, errors.New("无法估算本次模型请求的 token")
	}
	return QuotaEstimate{TotalTokens: total, InputTokens: input, OutputTokens: output}, nil
}

// estimateInputTokens 估算输入侧 token：system + 历史消息 + 工具定义。
func estimateInputTokens(req llm.CompletionRequest) int64 {
	var total int64
	add := func(s string) { total += countTokens(s) }
	for _, seg := range req.System {
		add(seg)
	}
	for _, msg := range req.Messages {
		for _, block := range msg.Content {
			switch block.Type {
			case llm.BlockText:
				add(block.Text)
			case llm.BlockThinking:
				add(block.Thinking)
			case llm.BlockToolUse:
				add(block.Name)
				add(string(block.Input))
			case llm.BlockToolResult:
				add(block.ToolUseID)
				for _, sub := range block.Content {
					add(sub.Text)
					add(sub.Thinking)
				}
			}
		}
	}
	for _, tool := range req.Tools {
		add(tool.Name)
		add(tool.Description)
		if raw, err := json.Marshal(tool.InputSchema); err == nil {
			add(string(raw))
		}
	}
	return total
}

// countTokens 估算一段文本的 token 数：ASCII 每 4 个字符算 1 个 token，
// 非 ASCII（中文等）每个字符约 1 个 token。
func countTokens(s string) int64 {
	var ascii, wide int64
	for _, r := range s {
		if r < 128 {
			ascii++
		} else {
			wide++
		}
	}
	return (ascii+asciiCharsPerToken-1)/asciiCharsPerToken + wide
}

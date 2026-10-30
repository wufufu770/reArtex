package sod

import (
	"context"
	"encoding/json"
	"errors"
	"log"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/internal/sodpolicy"
)

// DecisionRecord 是一次裁决的完整记录（哈希链与决策账本**同源**）。
//
// 同一个结构体喂给两个落点 ⇒ 两处写出的字段不会漂移。
//
// 原始载荷只以**摘要 + 脱敏预览**出现在这里：PayloadDigest 是将要执行的那份
// 字节的规范化摘要，PayloadPreview 是脱敏后的前 sodpolicy.PayloadPreviewMaxBytes
// 字节。**载荷原文永远不进这个结构体** —— 一旦进来，就有可能被写进库或日志。
type DecisionRecord struct {
	TaskID         int64
	ConversationID int64
	SourceAgent    string
	ToolName       string
	Verdict        string
	ReasonCode     string
	Note           string
	PayloadDigest  string
	PayloadPreview string
	IntentDigest   string
	AnomalyFlag    bool
	AnomalyReason  string
	Checks         map[string]string
	TokenDigest    string
	ScopeDigest    string
	CacheHit       bool
	LatencyMs      int
	// StructuredIntent 是规整器产出的结构化意图（JSON）。
	//
	// 为什么要有这一项：needs_review 的复核材料就是这份意图 —— 对抗者要证伪的
	// 正是「这个意图为什么异常」。只留摘要的话，redteam 拿到手只有一个 digest，
	// 什么都证伪不了。
	StructuredIntent json.RawMessage
}

// Auditor 写 SoD 的裁决留痕。
type Auditor struct {
	pg *db.DB
}

// NewAuditor 构造审计器。
func NewAuditor(pg *db.DB) *Auditor {
	return &Auditor{pg: pg}
}

// Record 写一次裁决。
//
// 落点两处，字段同源：
//   - 决策账本（sod_decisions）：**每一次**裁决都写（approve 也在内）。
//   - 哈希链（audit_events）：**deny / needs_review / fail-closed** 才写
//     （哈希链只收「判定动作」—— db/audit_chain.go 的设计如此：放行量极大，
//     而每条链记录都依赖上一条，approve 全进链会把 appendAuditMu 变成全局
//     串行瓶颈）。
//
// needs_review 另外落**对抗复核队列**（sod_adversarial_review_pending）——
// 那是它的唯一去处，**绝不**进人工队列（intercept_pending）。
//
// best-effort：留痕失败**不能**改变工具调用的结果（裁决已经发生了），所以这里
// 只记日志不返回错误 —— 但写失败必须能被看见（log），否则「没留痕」和「留了痕」
// 在事后追责时长得一模一样。
//
// ctx 目前不参与写入：db 访问层的签名不带 ctx（既有契约，本单元不改）。
func (a *Auditor) Record(ctx context.Context, rec *DecisionRecord) {
	if rec == nil {
		return
	}
	checks, err := json.Marshal(rec.Checks)
	if err != nil {
		checks = []byte("{}")
	}
	if a.pg == nil {
		log.Printf("[sod] 数据库未装配，裁决未留痕: verdict=%s reason=%s", rec.Verdict, rec.ReasonCode)
		return
	}
	if _, err := a.pg.RecordSoDDecision(&db.SoDDecision{
		TaskID:         nilInt64(rec.TaskID),
		ConversationID: nilInt64(rec.ConversationID),
		SourceAgent:    rec.SourceAgent,
		ToolName:       rec.ToolName,
		Verdict:        rec.Verdict,
		ReasonCode:     rec.ReasonCode,
		PayloadDigest:  rec.PayloadDigest,
		PayloadPreview: rec.PayloadPreview,
		IntentDigest:   rec.IntentDigest,
		AnomalyFlag:    rec.AnomalyFlag,
		AnomalyReason:  rec.AnomalyReason,
		Checks:         checks,
		TokenDigest:    rec.TokenDigest,
		ScopeDigest:    rec.ScopeDigest,
		CacheHit:       rec.CacheHit,
		LatencyMs:      rec.LatencyMs,
	}); err != nil {
		log.Printf("[sod] 写决策账本失败: %v", err)
	}
	if rec.Verdict == sodpolicy.VerdictApprove {
		return // approve 不进哈希链 —— 见上
	}
	payload, err := json.Marshal(map[string]any{
		"task_id":                  rec.TaskID,
		"conversation_id":          rec.ConversationID,
		"source_agent":             rec.SourceAgent,
		"tool_name":                rec.ToolName,
		"reason_code":              rec.ReasonCode,
		"payload_digest":           rec.PayloadDigest,
		"payload_preview":          rec.PayloadPreview,
		"normalizer_intent_digest": rec.IntentDigest,
		"anomaly_flag":             rec.AnomalyFlag,
		"anomaly_reason":           rec.AnomalyReason,
		"checks":                   rec.Checks,
		"verdict":                  rec.Verdict,
		"token_digest":             rec.TokenDigest,
		"scope_digest":             rec.ScopeDigest,
		"cache_hit":                rec.CacheHit,
		"latency_ms":               rec.LatencyMs,
	})
	if err != nil {
		payload = []byte("{}")
	}
	if _, err := a.pg.AppendAuditEvent(db.AuditEvent{
		Actor:    rec.SourceAgent,
		ToolName: rec.ToolName,
		Action:   "sod_" + rec.Verdict,
		Reason:   rec.ReasonCode + ": " + rec.Note,
		Payload:  payload,
	}); err != nil {
		log.Printf("[sod] 追加审计链失败: %v", err)
	}
	if rec.Verdict == sodpolicy.VerdictNeedsReview {
		if _, err := EnqueueReview(a.pg, rec); err != nil {
			log.Printf("[sod] needs_review 入队失败: %v", err)
		}
	}
}

// EnqueueReview 把一条 needs_review 裁决放进对抗复核队列（redteam 取走证伪）。
//
// ⚠️ 这是 needs_review 的**唯一**去处：绝不写 intercept_pending（人工审批表）——
// needs_review 是「交给对抗者证伪」，不是「请人看一眼」。
//
// 去重：同一个 (任务, agent, 工具, 载荷摘要) 已经在队列里 ⇒ 不再插第二条。
// 为什么去重：同一个动作重复入队会让 redteam 白证伪一遍；而同一个四元组就是
// 同一个动作，重复批准不产生新的待复核对象。
func EnqueueReview(pg *db.DB, rec *DecisionRecord) (int64, error) {
	if pg == nil {
		return 0, errors.New("数据库未装配，无法入队对抗复核")
	}
	if rec == nil {
		return 0, errors.New("裁决记录为空，无法入队对抗复核")
	}
	existing, err := findPendingReview(pg, rec)
	if err != nil {
		return 0, err
	}
	if existing > 0 {
		return existing, nil
	}
	intent := rec.StructuredIntent
	if len(intent) == 0 {
		intent = json.RawMessage(`{}`)
	}
	return pg.EnqueueAdversarialReview(&db.AdversarialReviewItem{
		TaskID:           nilInt64(rec.TaskID),
		ConversationID:   nilInt64(rec.ConversationID),
		SourceAgent:      rec.SourceAgent,
		ToolName:         rec.ToolName,
		PayloadDigest:    rec.PayloadDigest,
		PayloadPreview:   rec.PayloadPreview,
		AnomalyFlag:      rec.AnomalyFlag,
		AnomalyReason:    rec.AnomalyReason,
		ReasonCode:       rec.ReasonCode,
		StructuredIntent: intent,
	})
}

// findPendingReview 找出队列里与这条裁决同一个动作的待复核项（0 = 没有）。
func findPendingReview(pg *db.DB, rec *DecisionRecord) (int64, error) {
	items, err := pg.ListAdversarialReviewPending(rec.TaskID, 200)
	if err != nil {
		return 0, err
	}
	for _, it := range items {
		if it.SourceAgent == rec.SourceAgent && it.ToolName == rec.ToolName &&
			it.PayloadDigest == rec.PayloadDigest {
			return it.ID, nil
		}
	}
	return 0, nil
}

// nilInt64 把 0 变成 NULL。
//
// 为什么：外键列不接受 0 —— 0 不是任何一行的 id，硬塞进去会撞外键约束。缺任务
// 上下文时应当是「没有这一列的值」，不是「指向 id=0 的那一行」。
func nilInt64(v int64) *int64 {
	if v <= 0 {
		return nil
	}
	return &v
}

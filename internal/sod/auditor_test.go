package sod

import (
	"context"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/internal/sodpolicy"
)

// TestSoDAuditorRecordsDenials 钉住审计的两条硬要求：
//
//  1. **每一次裁决都写**（拒绝也写）；
//  2. 原始载荷只以「摘要 + 脱敏预览」入库 —— 原文与秘密一个字节都不许出现。
//
// 用例里的载荷刻意包含两种最常见的泄密形态：Authorization 头与口令参数。
func TestSoDAuditorRecordsDenials(t *testing.T) {
	d := auditorTestDB(t)
	payload := []byte(`{"command":"curl -H 'Authorization: Bearer supersecret' 'https://target.test/?password=hunter2'"}`)
	digest := CanonicalDigest(payload)
	rec := &DecisionRecord{
		SourceAgent:    "worker",
		ToolName:       "Bash",
		Verdict:        sodpolicy.VerdictDeny,
		ReasonCode:     sodpolicy.ReasonOutOfScope,
		Note:           "目标不在本任务的授权范围内",
		PayloadDigest:  digest,
		PayloadPreview: RedactPreview(payload, sodpolicy.PayloadPreviewMaxBytes),
		Checks:         map[string]string{"scope_check": "fail"},
	}
	NewAuditor(d).Record(context.Background(), rec)

	var (
		preview, reasonCode, verdict, storedDigest string
	)
	err := d.QueryRow(`SELECT payload_preview, reason_code, verdict, payload_digest
FROM sod_decisions WHERE payload_digest=$1 ORDER BY id DESC LIMIT 1`, digest).
		Scan(&preview, &reasonCode, &verdict, &storedDigest)
	if err != nil {
		t.Fatalf("读决策账本失败: %v", err)
	}
	if verdict != sodpolicy.VerdictDeny {
		t.Fatalf("verdict=%q want %q", verdict, sodpolicy.VerdictDeny)
	}
	if reasonCode != sodpolicy.ReasonOutOfScope {
		t.Fatalf("reason_code=%q want %q", reasonCode, sodpolicy.ReasonOutOfScope)
	}
	if storedDigest != digest {
		t.Fatalf("payload_digest=%q want %q", storedDigest, digest)
	}
	if !strings.Contains(preview, "«redacted»") {
		t.Fatalf("预览里必须出现 «redacted»: %s", preview)
	}
	for _, secret := range []string{"supersecret", "hunter2"} {
		if strings.Contains(preview, secret) {
			t.Fatalf("预览里泄漏了原文 %q: %s", secret, preview)
		}
	}

	// 哈希链：拒绝必须进链（链只收「被拦下的那些」）。
	var action, reason, chainPayload string
	err = d.QueryRow(`SELECT action, reason, payload FROM audit_events
WHERE payload LIKE $1 ORDER BY seq DESC LIMIT 1`, "%"+digest+"%").
		Scan(&action, &reason, &chainPayload)
	if err != nil {
		t.Fatalf("读审计链失败: %v", err)
	}
	if action != "sod_"+sodpolicy.VerdictDeny {
		t.Fatalf("action=%q want %q", action, "sod_"+sodpolicy.VerdictDeny)
	}
	if !strings.Contains(reason, sodpolicy.ReasonOutOfScope) {
		t.Fatalf("reason=%q 必须含原因码 %q", reason, sodpolicy.ReasonOutOfScope)
	}
	for _, secret := range []string{"supersecret", "hunter2"} {
		if strings.Contains(chainPayload, secret) {
			t.Fatalf("审计链里泄漏了原文 %q: %s", secret, chainPayload)
		}
	}
}

// TestSoDAuditorRecordsApproveInLedgerOnly 钉住哈希链的边界：approve **只**进
// 决策账本，不进哈希链（理由见 db/audit_chain.go：放行量极大，链上每条都依赖
// 上一条）。
func TestSoDAuditorRecordsApproveInLedgerOnly(t *testing.T) {
	d := auditorTestDB(t)
	payload := []byte(`{"command":"curl https://ok.test/"}`)
	digest := CanonicalDigest(payload)
	rec := &DecisionRecord{
		SourceAgent:    "worker",
		ToolName:       "Bash",
		Verdict:        sodpolicy.VerdictApprove,
		ReasonCode:     sodpolicy.ReasonAllChecksPassed,
		Note:           "全部确定性检查通过",
		PayloadDigest:  digest,
		PayloadPreview: RedactPreview(payload, sodpolicy.PayloadPreviewMaxBytes),
		Checks:         map[string]string{"scope_check": "pass"},
	}
	NewAuditor(d).Record(context.Background(), rec)

	var verdict string
	if err := d.QueryRow(`SELECT verdict FROM sod_decisions
WHERE payload_digest=$1 ORDER BY id DESC LIMIT 1`, digest).Scan(&verdict); err != nil {
		t.Fatalf("approve 必须进决策账本: %v", err)
	}
	if verdict != sodpolicy.VerdictApprove {
		t.Fatalf("verdict=%q want %q", verdict, sodpolicy.VerdictApprove)
	}

	var chainRows int
	if err := d.QueryRow(`SELECT count(*) FROM audit_events WHERE payload LIKE $1`,
		"%"+digest+"%").Scan(&chainRows); err != nil {
		t.Fatalf("统计审计链失败: %v", err)
	}
	if chainRows != 0 {
		t.Fatalf("approve 不该进哈希链，实际进了 %d 条", chainRows)
	}
}

// TestSoDAuditorRecordsNeedsReviewInAdversarialQueue 钉住 needs_review 的**唯一
// 去处**：对抗复核队列（redteam 取走证伪），**不是**人工审批队列。
func TestSoDAuditorRecordsNeedsReviewInAdversarialQueue(t *testing.T) {
	d := auditorTestDB(t)
	payload := []byte(`{"command":"curl https://odd.test/"}`)
	digest := CanonicalDigest(payload)
	rec := &DecisionRecord{
		SourceAgent:      "worker",
		ToolName:         "Bash",
		Verdict:          sodpolicy.VerdictNeedsReview,
		ReasonCode:       sodpolicy.ReasonAnomalyFlagged,
		Note:             "动作存在异常特征，转入对抗复核",
		PayloadDigest:    digest,
		PayloadPreview:   RedactPreview(payload, sodpolicy.PayloadPreviewMaxBytes),
		AnomalyFlag:      true,
		AnomalyReason:    "命令里出现编码后的载荷",
		Checks:           map[string]string{"anomaly_check": "flagged"},
		StructuredIntent: []byte(`{"action_type":"http_request"}`),
	}
	NewAuditor(d).Record(context.Background(), rec)

	var reasonCode, status, queuedDigest, intent string
	err := d.QueryRow(`SELECT reason_code, status, payload_digest, structured_intent
FROM sod_adversarial_review_pending WHERE payload_digest=$1 ORDER BY id DESC LIMIT 1`, digest).
		Scan(&reasonCode, &status, &queuedDigest, &intent)
	if err != nil {
		t.Fatalf("needs_review 必须进对抗复核队列: %v", err)
	}
	if reasonCode != sodpolicy.ReasonAnomalyFlagged {
		t.Fatalf("reason_code=%q want %q", reasonCode, sodpolicy.ReasonAnomalyFlagged)
	}
	if status != "pending" {
		t.Fatalf("status=%q want pending", status)
	}
	if queuedDigest != digest {
		t.Fatalf("payload_digest=%q want %q", queuedDigest, digest)
	}
	if !strings.Contains(intent, "http_request") {
		t.Fatalf("队列项必须带结构化意图（对抗者要证伪的就是它）: %s", intent)
	}

	var humanQueue int
	if err := d.QueryRow(`SELECT count(*) FROM intercept_pending`).Scan(&humanQueue); err != nil {
		t.Fatalf("统计人工队列失败: %v", err)
	}
	if humanQueue != 0 {
		t.Fatalf("needs_review 绝不许进人工队列（intercept_pending），实际有 %d 行", humanQueue)
	}
}

// TestSoDAuditorDeduplicatesReviewQueueItem 钉住队列去重：同一个
// (任务, agent, 工具, 载荷摘要) 只应有一条待复核项 —— 否则 redteam 要把同一
// 件事证伪两遍。
func TestSoDAuditorDeduplicatesReviewQueueItem(t *testing.T) {
	d := auditorTestDB(t)
	payload := []byte(`{"command":"curl https://dup.test/"}`)
	digest := CanonicalDigest(payload)
	rec := &DecisionRecord{
		SourceAgent:    "worker",
		ToolName:       "Bash",
		Verdict:        sodpolicy.VerdictNeedsReview,
		ReasonCode:     sodpolicy.ReasonAnomalyFlagged,
		Note:           "动作存在异常特征，转入对抗复核",
		PayloadDigest:  digest,
		PayloadPreview: RedactPreview(payload, sodpolicy.PayloadPreviewMaxBytes),
		AnomalyFlag:    true,
	}
	auditor := NewAuditor(d)
	auditor.Record(context.Background(), rec)
	auditor.Record(context.Background(), rec)

	var rows int
	if err := d.QueryRow(`SELECT count(*) FROM sod_adversarial_review_pending
WHERE payload_digest=$1`, digest).Scan(&rows); err != nil {
		t.Fatalf("统计对抗复核队列失败: %v", err)
	}
	if rows != 1 {
		t.Fatalf("同一动作只应有一条待复核项，实际 %d 条", rows)
	}
}

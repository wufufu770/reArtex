package server

// 本文件是【审计链校验端点】的验收用例。
//
// 审计链（db/audit_chain.go）每条记录的 digest 都依赖上一条 ⇒ 链被改过就一定
// 能验出来。追加有入口（db.AppendAuditEvent），校验却没有对外通道 —— 操作者
// 没法自查「这条链还完整吗」。
//
// ⚠️ 路由可达性用**真实路由表**（s.Handler()）判定：ServeMux 未匹配时的默认
// 响应体是纯文本 "404 page not found"，与 handler 自己的 404（JSON 体）分得开。

import (
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
)

// TestAuditChainVerifyRouteRegistered 钉住校验端点真的接上了。
func TestAuditChainVerifyRouteRegistered(t *testing.T) {
	_, _, h, tok := sodAPIServer(t)
	code, body := sodAPIRaw(t, h, tok, "GET", "/api/audit/chain")
	if containsNotFound(body) {
		t.Fatalf("路由未注册: GET /api/audit/chain → %d %s", code, body)
	}
}

// appendAuditEventRetry 追加一条审计事件，撞上 seq 冲突时重试。
//
// 为什么需要重试：审计链的 seq 是「链尾 + 1」（db/audit_chain.go 的
// auditChainTail），进程内有互斥锁但**跨进程没有** ⇒ 同一库里另一个测试进程
// 正好追加时，本进程会撞上 idx_audit_events_seq 的唯一约束。那是并发环境的
// 竞态，不是本用例的失败 ⇒ 有界重试把它挡在外面。
func appendAuditEventRetry(t *testing.T, pg *db.DB, ev db.AuditEvent) string {
	t.Helper()
	var digest string
	var err error
	for i := 0; i < 3; i++ {
		digest, err = pg.AppendAuditEvent(ev)
		if err == nil {
			return digest
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("追加审计事件: %v", err)
	return ""
}

// TestAuditChainVerifyReportsIntact 钉住链完整时报「完整」。
func TestAuditChainVerifyReportsIntact(t *testing.T) {
	pg, _, h, tok := sodAPIServer(t)
	appendAuditEventRetry(t, pg, db.AuditEvent{
		Actor:    "operator",
		ToolName: "audit_chain_probe",
		Action:   "probe",
		Reason:   "链路校验用例",
		Payload:  []byte(`{"probe":true}`),
	})

	code, out := sodAPIDo(t, h, tok, "GET", "/api/audit/chain", nil)
	if code != 200 {
		t.Fatalf("校验审计链应当成功，实际 %d %v", code, out)
	}
	if out["intact"] != true {
		t.Fatalf("审计链应当是完整的: %v", out)
	}
	if v, ok := out["broken_at_seq"].(float64); ok && v != 0 {
		t.Fatalf("完整的链不该报断裂位置: %v", out)
	}
}

// TestAuditChainVerifyDetectsTampering 钉住篡改能被抓出来。
//
// 这是这条端点存在的全部理由：链的价值在于「事后能证明没被改过」。篡改一条
// 记录后，校验必须报出断裂位置 —— 报「完整」等于把校验变成摆设。
func TestAuditChainVerifyDetectsTampering(t *testing.T) {
	pg, _, h, tok := sodAPIServer(t)
	digest := appendAuditEventRetry(t, pg, db.AuditEvent{
		Actor:    "operator",
		ToolName: "audit_chain_tamper",
		Action:   "tamper",
		Reason:   "改之前的原因",
		Payload:  []byte(`{"tamper":true}`),
	})
	var seq int64
	if err := pg.QueryRow(`SELECT seq FROM audit_events WHERE digest=$1`, digest).Scan(&seq); err != nil {
		t.Fatalf("读审计事件 seq: %v", err)
	}
	if _, err := pg.Exec(`UPDATE audit_events SET reason='改之后的原因' WHERE seq=$1`, seq); err != nil {
		t.Fatalf("篡改审计记录: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pg.Exec(`UPDATE audit_events SET reason='改之前的原因' WHERE seq=$1`, seq)
	})

	code, out := sodAPIDo(t, h, tok, "GET", "/api/audit/chain", nil)
	if code != 200 {
		t.Fatalf("校验审计链应当成功（校验本身没坏），实际 %d %v", code, out)
	}
	if out["intact"] == true {
		t.Fatalf("被篡改的链不许报完整: %v", out)
	}
	broken, ok := out["broken_at_seq"].(float64)
	if !ok || int64(broken) != seq {
		t.Fatalf("断裂位置不对：期望 seq=%d，实际 %v", seq, out["broken_at_seq"])
	}
}

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

// TestAuditAnchorRouteRegistered 钉住锚点端点真的接上了（A-05）。
func TestAuditAnchorRouteRegistered(t *testing.T) {
	_, _, h, tok := sodAPIServer(t)
	code, body := sodAPIRaw(t, h, tok, "GET", "/api/audit/anchor")
	if containsNotFound(body) {
		t.Fatalf("路由未注册: GET /api/audit/anchor → %d %s", code, body)
	}
}

// TestAuditAnchorEndpoint 钉住锚点对账端点的三态（A-05）：
//
//   - 无锚点 ⇒ anchor=null 且 consistent=true（部署尚未配置锚点脚本的正常过渡态，
//     不算不一致，但部署应尽快配置——这正是「诚实回报」而不是「报错吓人」）；
//   - 锚点与链一致 ⇒ consistent=true，锚点位 seq/digest 带回；
//   - 锚点位 digest 被改 ⇒ consistent=false 且给出可读 error。
//
// 【只读】用例全程只写锚点表（RecordAuditAnchor/UPDATE），绝不通过端点写——
// GET 写状态的接口本身就是漏洞。
func TestAuditAnchorEndpoint(t *testing.T) {
	pg, _, h, tok := sodAPIServer(t)
	// 锚点表是全局的：起手清空，保证「无锚点」分支可判定（派生库每轮新建，
	// 这里只可能清掉本轮其它用例的锚点）。
	if _, err := pg.Exec(`DELETE FROM audit_anchors`); err != nil {
		t.Fatalf("清空锚点表: %v", err)
	}

	// ① 无锚点。
	code, out := sodAPIDo(t, h, tok, "GET", "/api/audit/anchor", nil)
	if code != 200 {
		t.Fatalf("锚点对账应当成功，实际 %d %v", code, out)
	}
	if out["anchor"] != nil {
		t.Fatalf("无锚点时 anchor 应为 null: %v", out)
	}
	if out["consistent"] != true {
		t.Fatalf("无锚点不算不一致: %v", out)
	}

	// ② 落一个真实锚点（记在当前链尾）：consistent=true。
	a, err := pg.RecordAuditAnchor("test-anchor-endpoint")
	if err != nil {
		t.Fatalf("记录锚点: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pg.Exec(`DELETE FROM audit_anchors WHERE note='test-anchor-endpoint'`)
	})
	code, out = sodAPIDo(t, h, tok, "GET", "/api/audit/anchor", nil)
	if code != 200 {
		t.Fatalf("锚点对账应当成功，实际 %d %v", code, out)
	}
	if out["consistent"] != true {
		t.Fatalf("链尾未动时锚点应当一致: %v", out)
	}
	am, ok := out["anchor"].(map[string]any)
	if !ok {
		t.Fatalf("锚点应当带明细返回: %v", out)
	}
	if seqv, ok := am["seq"].(float64); !ok || int64(seqv) != a.Seq {
		t.Fatalf("锚点 seq 不对：期望 %d，实际 %v", a.Seq, am["seq"])
	}
	if am["digest"] != a.Digest {
		t.Fatalf("锚点 digest 不对：期望 %s，实际 %v", a.Digest, am["digest"])
	}

	// ③ 锚点被改（外部介质/表被篡改的对立面：锚点说的和链上长的不一样）。
	if _, err := pg.Exec(`UPDATE audit_anchors SET digest='forged' WHERE note='test-anchor-endpoint'`); err != nil {
		t.Fatal(err)
	}
	code, out = sodAPIDo(t, h, tok, "GET", "/api/audit/anchor", nil)
	if code != 200 {
		t.Fatalf("锚点不一致也是 200（结论而非故障），实际 %d %v", code, out)
	}
	if out["consistent"] != false {
		t.Fatalf("锚点位 digest 被改必须报不一致: %v", out)
	}
	if msg, ok := out["error"].(string); !ok || msg == "" {
		t.Fatalf("不一致时必须给出可读 error: %v", out)
	}
}

package db

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// 审计链：连续追加若干条后应验通；改动中间任意一条都必须被验出来，
// 且报出断在第几条。
func TestAuditChainDetectsTampering(t *testing.T) {
	dsn := testDSN(t)
	d, err := Open(dsn)
	if err != nil {
		t.Skipf("postgres unavailable (%v) - skipping", err)
	}
	defer d.Close()

	suffix := time.Now().UnixNano()
	// 链是全局的：只对本次追加的尾部做校验，避免受同库其它用例影响。
	base, err := d.auditChainTail()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM audit_events WHERE seq > $1`, base) })

	for i := 0; i < 3; i++ {
		if _, err := d.AppendAuditEvent(AuditEvent{
			Actor:    "test-agent",
			ToolName: "Bash",
			Action:   "denied",
			Reason:   fmt.Sprintf("chain-probe-%d-%d", suffix, i),
			Payload:  json.RawMessage(fmt.Sprintf(`{"i":%d,"tag":"%d"}`, i, suffix)),
		}); err != nil {
			t.Fatal(err)
		}
	}

	// 链应完整
	if seq, err := d.VerifyAuditChain(); err != nil {
		t.Fatalf("追加后链应完整，却在 seq=%d 处报断: %v", seq, err)
	}

	// 改动中间那条的内容 → 必须被验出来
	firstSeq := base + 1
	if _, err := d.Exec(`UPDATE audit_events SET reason='tampered' WHERE seq=$1`, firstSeq); err != nil {
		t.Fatal(err)
	}
	seq, err := d.VerifyAuditChain()
	if err == nil {
		t.Fatal("改动链上的一条后，校验应当报出断裂")
	}
	if seq != firstSeq {
		t.Fatalf("应报出断裂位置 seq=%d，得到 %d", firstSeq, seq)
	}
}

// 链为空时校验也算通过 —— 没有记录就没有可被改的痕迹。
func TestAuditChainEmptyIsIntact(t *testing.T) {
	dsn := testDSN(t)
	d, err := Open(dsn)
	if err != nil {
		t.Skipf("postgres unavailable (%v) - skipping", err)
	}
	defer d.Close()

	base, err := d.auditChainTail()
	if err != nil {
		t.Fatal(err)
	}
	if base != 0 {
		t.Skip("链上已有其它用例的记录，跳过空链断言")
	}
	if seq, err := d.VerifyAuditChain(); err != nil {
		t.Fatalf("空链应视为完整，却报 seq=%d: %v", seq, err)
	}
}

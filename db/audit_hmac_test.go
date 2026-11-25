package db

import (
	"bytes"
	"fmt"
	"testing"
	"time"
)

// hmacTestKey 是 A-05 测试用的固定审计密钥（派生值形态，32 字节）。
var hmacTestKey = DeriveAuditKey([]byte("test-master-key-for-audit-hmac"))

// cleanupAuditTail 把测试追加的链行与锚点行清掉（链是全局共享的：同库的其它
// 用例不该看到本用例的行，锚点表同理）。Close 也折进 cleanup：t.Cleanup 在用例
// 函数的 defer 之后才跑，若用 `defer d.Close()`，清理的 DELETE 会跑在已关闭的
// 池上被 `_, _ =` 吞掉 ⇒ 行残留，同轮进程里后面的整链校验用例全部误报断裂
// （实测钉子：TestAuditChainDetectsTampering 的 tampered 行残留曾让本文件三个
// 用例齐报「断裂在 seq=1」）。
func cleanupAuditTail(t *testing.T, d *DB, base int64) {
	t.Cleanup(func() {
		_, _ = d.Exec(`DELETE FROM audit_events WHERE seq > $1`, base)
		_, _ = d.Exec(`DELETE FROM audit_anchors WHERE note LIKE 'test-%' OR note LIKE 'genesis: legacy sha256 tail%'`)
		_ = d.Close()
	})
}

// TestDeriveAuditKeyDomainSeparated pins the derivation：确定性（同主密钥同派生
// 值）、域分离（不同主密钥/不同用途不得撞钥）、长度合规。
func TestDeriveAuditKeyDomainSeparated(t *testing.T) {
	k1 := DeriveAuditKey([]byte("master-one"))
	k2 := DeriveAuditKey([]byte("master-one"))
	if !bytes.Equal(k1, k2) {
		t.Fatal("同主密钥必须派生出同一把审计钥（确定性）")
	}
	if len(k1) != 32 {
		t.Fatalf("派生钥应为 32 字节，得到 %d", len(k1))
	}
	if bytes.Equal(k1, DeriveAuditKey([]byte("master-two"))) {
		t.Fatal("不同主密钥不得派生出同一把审计钥")
	}
	// 域分离的最低要求：派生值不得就是主密钥本身（一把钥开两把锁不是加固）。
	if bytes.Equal(k1, []byte("master-one")) {
		t.Fatal("派生值不得等于主密钥原文")
	}
}

// TestAuditChainHMACMixedVerify pins the A-05 核心：密钥行（source 纳入摘要）与
// 存量 SHA-256 行在同一条链上自然衔接、按行 key_id 选口径复算；改 HMAC 行的
// source 也必须现形。
func TestAuditChainHMACMixedVerify(t *testing.T) {
	dsn := testDSN(t)
	dLegacy, err := Open(dsn) // 未配钥实例：写旧口径行、模拟存量链
	if err != nil {
		t.Skipf("postgres unavailable (%v) - skipping", err)
	}
	defer dLegacy.Close()
	dKeyed, err := Open(dsn)
	if err != nil {
		t.Skipf("postgres unavailable (%v) - skipping", err)
	}
	dKeyed = dKeyed.WithAuditKey(hmacTestKey)

	base, err := dKeyed.AuditChainTail()
	if err != nil {
		t.Fatal(err)
	}
	cleanupAuditTail(t, dKeyed, base)

	// 旧口径一行 + HMAC 一行：边界处 prev 自然衔接。
	tag := fmt.Sprintf("hmac-mixed-%d", time.Now().UnixNano())
	if _, err := dLegacy.AppendAuditEvent(AuditEvent{Actor: "legacy", ToolName: "probe", Action: "denied", Reason: tag + "-old", Source: "legacy"}); err != nil {
		t.Fatal(err)
	}
	hmacDigest, err := dKeyed.AppendAuditEvent(AuditEvent{Actor: "hmac", ToolName: "probe", Action: "denied", Reason: tag + "-new", Source: "sod"})
	if err != nil {
		t.Fatal(err)
	}
	var keyID, storedSource string
	if err := dKeyed.QueryRow(`SELECT key_id, source FROM audit_events WHERE digest=$1`, hmacDigest).Scan(&keyID, &storedSource); err != nil {
		t.Fatal(err)
	}
	if keyID != auditKeyID {
		t.Fatalf("配钥实例写出的行应为 %s，实际 %q", auditKeyID, keyID)
	}
	if storedSource != "sod" {
		t.Fatalf("HMAC 行必须落 source，实际 %q", storedSource)
	}

	// 配钥实例：混合链完整。
	if seq, err := dKeyed.VerifyAuditChain(); err != nil {
		t.Fatalf("新旧口径混合链应验通，却报 seq=%d: %v", seq, err)
	}
	// 未配钥实例：链上有 HMAC 行 ⇒ 如实报「无法复算」（不是跳过）。
	if seq, err := dLegacy.VerifyAuditChain(); err == nil {
		t.Fatalf("未配钥实例验 HMAC 行必须报无法复算，却通过了（seq=%d）", seq)
	}
	// source 纳入 HMAC：改 HMAC 行的 source ⇒ 现形，报断裂在该行。
	if _, err := dKeyed.Exec(`UPDATE audit_events SET source='rewritten' WHERE digest=$1`, hmacDigest); err != nil {
		t.Fatal(err)
	}
	seq, err := dKeyed.VerifyAuditChain()
	if err == nil {
		t.Fatal("改 HMAC 行的 source 必须被验出来（source 纳入摘要）")
	}
	if seq == 0 {
		t.Fatalf("应报出断裂位置，得到 0: %v", err)
	}
}

// TestAuditAnchorLifecycle pins the anchor contract：Record → Latest → Verify
// 闭环；锚点 digest 被改 ⇒ 不一致；锚点位之后整段截断 ⇒ VerifyAuditChain 报
// 「锚点与链不一致」且指到锚点 seq（截断的链逐行仍验得通——这正是锚点存在的
// 理由：只靠逐行复算看不出「尾部被删」）。
func TestAuditAnchorLifecycle(t *testing.T) {
	dsn := testDSN(t)
	d, err := Open(dsn)
	if err != nil {
		t.Skipf("postgres unavailable (%v) - skipping", err)
	}
	d.WithAuditKey(hmacTestKey)

	base, err := d.AuditChainTail()
	if err != nil {
		t.Fatal(err)
	}
	cleanupAuditTail(t, d, base)

	tag := fmt.Sprintf("anchor-life-%d", time.Now().UnixNano())
	for i := 0; i < 3; i++ {
		if _, err := d.AppendAuditEvent(AuditEvent{Actor: "t", ToolName: "probe", Action: "denied", Reason: fmt.Sprintf("%s-%d", tag, i)}); err != nil {
			t.Fatal(err)
		}
	}
	a, err := d.RecordAuditAnchor("test-anchor-lifecycle")
	if err != nil {
		t.Fatal(err)
	}
	got, ok, err := d.LatestAuditAnchor()
	if err != nil || !ok {
		t.Fatalf("应能读到最近锚点: ok=%v err=%v", ok, err)
	}
	if got.Seq != a.Seq || got.Digest != a.Digest {
		t.Fatalf("读回的锚点不对: %+v vs %+v", got, a)
	}
	if err := d.VerifyAuditAnchor(got); err != nil {
		t.Fatalf("锚点应与链一致: %v", err)
	}
	if seq, err := d.VerifyAuditChain(); err != nil {
		t.Fatalf("锚定后链应完整且锚点一致，却报 seq=%d: %v", seq, err)
	}

	// 锚点被改（外部介质/表被篡改）⇒ 报不一致，位置=锚点 seq。
	if _, err := d.Exec(`UPDATE audit_anchors SET digest='forged' WHERE note='test-anchor-lifecycle'`); err != nil {
		t.Fatal(err)
	}
	seq, err := d.VerifyAuditChain()
	if err == nil || seq != a.Seq {
		t.Fatalf("锚点被改必须报不一致（seq=%d），实际 seq=%d err=%v", a.Seq, seq, err)
	}
	if _, err := d.Exec(`UPDATE audit_anchors SET digest=$1 WHERE note='test-anchor-lifecycle'`, a.Digest); err != nil {
		t.Fatal(err)
	}

	// 链尾截断：删掉锚点位之后的行（含锚点位）——逐行验通但链尾早于锚点。
	if _, err := d.Exec(`DELETE FROM audit_events WHERE seq > $1`, a.Seq-1); err != nil {
		t.Fatal(err)
	}
	seq, err = d.VerifyAuditChain()
	if err == nil || seq != a.Seq {
		t.Fatalf("链尾截断必须报锚点不一致（seq=%d），实际 seq=%d err=%v", a.Seq, seq, err)
	}
}

// TestMigrateAuditChainToHMANGenesis pins the migration：存量（SHA-256）链验证
// 通过 ⇒ 写创世锚点（seq=链尾）；幂等（重复调用不再新增）；旧链已断 ⇒ 拒绝写
// 锚点并返回断裂位置。
func TestMigrateAuditChainToHMANGenesis(t *testing.T) {
	dsn := testDSN(t)
	d, err := Open(dsn)
	if err != nil {
		t.Skipf("postgres unavailable (%v) - skipping", err)
	}
	d.WithAuditKey(hmacTestKey)

	base, err := d.AuditChainTail()
	if err != nil {
		t.Fatal(err)
	}
	cleanupAuditTail(t, d, base)

	// 存量链：旧口径两行。
	tag := fmt.Sprintf("migrate-%d", time.Now().UnixNano())
	for i := 0; i < 2; i++ {
		if _, err := d.AppendAuditEvent(AuditEvent{Actor: "legacy", ToolName: "probe", Action: "denied", Reason: fmt.Sprintf("%s-%d", tag, i)}); err != nil {
			t.Fatal(err)
		}
	}
	// 迁移前提：库里此刻没有别的锚点（锚点表是全局的，清掉历史测试锚点）。
	if _, err := d.Exec(`DELETE FROM audit_anchors`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM audit_anchors`) })

	if err := d.MigrateAuditChainToHMAC(); err != nil {
		t.Fatalf("存量链完整时迁移应成功: %v", err)
	}
	a, ok, err := d.LatestAuditAnchor()
	if err != nil || !ok {
		t.Fatalf("迁移后应有创世锚点: ok=%v err=%v", ok, err)
	}
	tail, err := d.AuditChainTail()
	if err != nil {
		t.Fatal(err)
	}
	if a.Seq != tail {
		t.Fatalf("创世锚点应落在链尾 seq=%d，实际 %d", tail, a.Seq)
	}
	// 幂等：第二次调用不新增锚点。
	if err := d.MigrateAuditChainToHMAC(); err != nil {
		t.Fatalf("重复迁移应幂等 no-op: %v", err)
	}
	var n int
	if err := d.QueryRow(`SELECT COUNT(*) FROM audit_anchors`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("重复迁移后锚点应仍为 1 条，实际 %d", n)
	}
	// 首条 HMAC 行的 prev = 创世锚点位的 digest（链自然衔接的直接证据）。
	hmacDigest, err := d.AppendAuditEvent(AuditEvent{Actor: "hmac", ToolName: "probe", Action: "denied", Reason: tag + "-first-hmac", Source: "sod"})
	if err != nil {
		t.Fatal(err)
	}
	var prevDigest string
	if err := d.QueryRow(`SELECT prev_digest FROM audit_events WHERE digest=$1`, hmacDigest).Scan(&prevDigest); err != nil {
		t.Fatal(err)
	}
	if prevDigest != a.Digest {
		t.Fatalf("首条 HMAC 行应引用创世锚点位的 digest，实际 %q ≠ %q", prevDigest, a.Digest)
	}
	// 迁移后整链（混合口径）验通。
	if seq, err := d.VerifyAuditChain(); err != nil {
		t.Fatalf("迁移+首条 HMAC 行后整链应验通，却报 seq=%d: %v", seq, err)
	}

	// 断链场景：改掉一行 ⇒ 迁移拒绝写锚点并返回断裂位置。
	if _, err := d.Exec(`UPDATE audit_events SET reason='broken' WHERE seq=$1`, base+1); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`DELETE FROM audit_anchors`); err != nil {
		t.Fatal(err)
	}
	migErr := d.MigrateAuditChainToHMAC()
	if migErr == nil {
		t.Fatal("存量链已断时迁移必须拒绝写锚点")
	}
}

package db

// 方案 v3 缺陷 D2/D4 的库层单测：
//
//	D2 —— 传承图评审转移矩阵放开（retired→draft 恢复、accepted→retired 退役）、
//	      删除闸（accepted 拒绝）、删除前审计快照；
//	D4 —— 对抗复核队列的过期自动结案（pending 超 24h ⇒ dismissed + 留痕）。

import (
	"strings"
	"testing"
	"time"
)

func legacyTestNode(t *testing.T, d *DB, title string) int64 {
	t.Helper()
	id, err := d.CreateLegacyNode(&LegacyNode{
		Kind: "experience", Title: title, Summary: "测试经验正文", State: "draft",
	})
	if err != nil {
		t.Fatalf("建经验节点失败: %v", err)
	}
	return id
}

func TestLegacyReviewTransitions(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v)", err)
	}
	defer d.Close()

	id := legacyTestNode(t, d, "D2-转移矩阵")
	t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM legacy_nodes WHERE id=$1`, id) })

	// draft → accepted（首评）。
	if err := d.ReviewLegacyNode(id, "accepted", "tester"); err != nil {
		t.Fatalf("draft→accepted: %v", err)
	}
	// accepted → retired（在用经验退役，进注入池立即失效）。
	if err := d.ReviewLegacyNode(id, "retired", "tester"); err != nil {
		t.Fatalf("accepted→retired: %v", err)
	}
	// retired → draft（恢复待评审：D2 的核心诉求——退役可撤销）。
	if err := d.ReviewLegacyNode(id, "draft", "tester"); err != nil {
		t.Fatalf("retired→draft: %v", err)
	}
	// 非法转移：draft → draft（同态幂等算成功）。
	if err := d.ReviewLegacyNode(id, "draft", "tester"); err != nil {
		t.Fatalf("draft→draft 应幂等成功: %v", err)
	}
	// 非法转移：accepted 之外的目标态拒绝在 retired 态直接 accepted？——retired→accepted
	// 不在矩阵里（必须先恢复 draft 再评 accepted）。
	if err := d.ReviewLegacyNode(id, "accepted", "tester"); err == nil {
		// 当前是 draft 态，draft→accepted 合法 —— 先退到 retired 再试。
		if err := d.ReviewLegacyNode(id, "retired", "tester"); err != nil {
			t.Fatalf("draft→retired: %v", err)
		}
		if err := d.ReviewLegacyNode(id, "accepted", "tester"); err == nil {
			t.Fatal("retired→accepted 必须被拒（不在转移矩阵内）")
		}
	}
}

func TestLegacyDeleteGuardAndAudit(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v)", err)
	}
	defer d.Close()

	accepted := legacyTestNode(t, d, "D2-删除守卫-accepted")
	t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM legacy_nodes WHERE id IN ($1,$2)`, accepted, accepted+1) })
	if err := d.ReviewLegacyNode(accepted, "accepted", "tester"); err != nil {
		t.Fatalf("置 accepted: %v", err)
	}
	// accepted 直删必须被拒（ErrLegacyAcceptedPin）。
	if err := d.DeleteLegacyNode(accepted, "tester"); err == nil {
		t.Fatal("accepted 经验不可直接删除")
	}

	retired := legacyTestNode(t, d, "D2-删除- retired")
	if err := d.ReviewLegacyNode(retired, "retired", "tester"); err != nil {
		t.Fatalf("置 retired: %v", err)
	}
	// 采纳留痕一行，验证快照把它带上。
	d.RecordLegacyUsage(retired, 999001)
	if err := d.DeleteLegacyNode(retired, "tester"); err != nil {
		t.Fatalf("retired 删除失败: %v", err)
	}
	// 审计快照必须落了（含节点与 usage）。
	var payload []byte
	if err := d.QueryRow(`SELECT payload FROM legacy_audit WHERE node_id=$1 AND action='delete' ORDER BY id DESC LIMIT 1`, retired).Scan(&payload); err != nil {
		t.Fatalf("删除审计快照缺失: %v", err)
	}
	snap := string(payload)
	if !strings.Contains(snap, "D2-删除- retired") || !strings.Contains(snap, "999001") {
		t.Fatalf("审计快照应含节点与 usage 行，got: %.200s", snap)
	}
	// 节点本体已删。
	if _, err := d.GetLegacyNode(retired); err == nil {
		t.Fatal("删除后节点不应可读")
	}
}

func TestDismissStaleAdversarialReviews(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v)", err)
	}
	defer d.Close()

	// 造两条 pending：一条 25h 前（应被结案），一条刚刚（应保留）。
	stamp := time.Now().UnixNano()
	old, err := d.EnqueueAdversarialReview(&AdversarialReviewItem{
		SourceAgent: "worker", ToolName: "Bash", ReasonCode: "anomaly_flagged",
		PayloadDigest: "d-old-" + time.Unix(0, stamp).String(), PayloadPreview: "old",
	})
	if err != nil {
		t.Fatalf("入队 old: %v", err)
	}
	fresh, err := d.EnqueueAdversarialReview(&AdversarialReviewItem{
		SourceAgent: "worker", ToolName: "Bash", ReasonCode: "anomaly_flagged",
		PayloadDigest: "d-fresh", PayloadPreview: "fresh",
	})
	if err != nil {
		t.Fatalf("入队 fresh: %v", err)
	}
	t.Cleanup(func() {
		_, _ = d.Exec(`DELETE FROM sod_adversarial_review_pending WHERE id IN ($1,$2)`, old, fresh)
	})
	if _, err := d.Exec(`UPDATE sod_adversarial_review_pending SET created_at = now() - interval '25 hours' WHERE id=$1`, old); err != nil {
		t.Fatalf("做旧: %v", err)
	}

	n, err := d.DismissStaleAdversarialReviews(24 * time.Hour)
	if err != nil {
		t.Fatalf("过期结案: %v", err)
	}
	if n < 1 {
		t.Fatalf("至少应结案 1 条（做旧的这条），got %d", n)
	}
	var oldStatus, freshStatus string
	if err := d.QueryRow(`SELECT status FROM sod_adversarial_review_pending WHERE id=$1`, old).Scan(&oldStatus); err != nil {
		t.Fatal(err)
	}
	if err := d.QueryRow(`SELECT status FROM sod_adversarial_review_pending WHERE id=$1`, fresh).Scan(&freshStatus); err != nil {
		t.Fatal(err)
	}
	if oldStatus != "dismissed" {
		t.Fatalf("25h 前的 pending 应被自动结案，got %q", oldStatus)
	}
	if freshStatus != "pending" {
		t.Fatalf("新 pending 不应被误伤，got %q", freshStatus)
	}
	// 留痕：resolution 里有自动结案说明。
	var resolution string
	if err := d.QueryRow(`SELECT resolution FROM sod_adversarial_review_pending WHERE id=$1`, old).Scan(&resolution); err == nil && resolution == "" {
		t.Fatal("自动结案必须留痕（resolution 非空）")
	}
}

func TestSodManualOverrideLifecycle(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v)", err)
	}
	defer d.Close()

	ovr := &SodManualOverride{
		DecisionID: 1, PayloadDigest: "digest-abc", ToolKey: "record_fact",
		TaskID: 7, AgentKey: "work#1", Scope: "once", Reason: "误拒", CreatedBy: "tester",
	}
	id, err := d.CreateSodManualOverride(ovr)
	if err != nil {
		t.Fatalf("建 override: %v", err)
	}
	t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM sod_manual_overrides WHERE id=$1`, id) })

	// 四元组命中。
	got, err := d.LookupSodManualOverride("record_fact", 7, "work#1", "digest-abc")
	if err != nil || got == nil {
		t.Fatalf("命中失败: %v, %v", got, err)
	}
	// 摘要不匹配 ⇒ 不命中。
	miss, err := d.LookupSodManualOverride("record_fact", 7, "work#1", "digest-OTHER")
	if err != nil || miss != nil {
		t.Fatalf("摘要不同不应命中: %v, %v", miss, err)
	}
	// once 用后即焚。
	if err := d.ConsumeSodManualOverride(id); err != nil {
		t.Fatalf("消费: %v", err)
	}
	gone, err := d.LookupSodManualOverride("record_fact", 7, "work#1", "digest-abc")
	if err != nil || gone != nil {
		t.Fatalf("once 消费后不应再命中: %v, %v", gone, err)
	}

	// task 级：终态过期。
	tid, err := d.CreateSodManualOverride(&SodManualOverride{
		DecisionID: 2, PayloadDigest: "d2", ToolKey: "add_hint", TaskID: 7,
		AgentKey: "planner", Scope: "task", CreatedBy: "tester",
	})
	if err != nil {
		t.Fatalf("建 task override: %v", err)
	}
	t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM sod_manual_overrides WHERE id=$1`, tid) })
	if err := d.ExpireTaskSodOverrides(7); err != nil {
		t.Fatalf("任务终态过期: %v", err)
	}
	expired, err := d.LookupSodManualOverride("add_hint", 7, "planner", "d2")
	if err != nil || expired != nil {
		t.Fatalf("task 级过期后不应命中: %v, %v", expired, err)
	}
}

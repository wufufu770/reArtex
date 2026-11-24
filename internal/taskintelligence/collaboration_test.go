package taskintelligence

import (
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------- 偏锋 → 业务建模 ----------

func TestEdgeNeedsBusinessModel(t *testing.T) {
	// 快照不可用 → 建议（宁补勿猜）。
	if ok, _ := EdgeNeedsBusinessModel(nil); !ok {
		t.Fatal("nil 快照应建议补建模")
	}
	// 无模型 → 建议。
	ok, reason := EdgeNeedsBusinessModel(&TaskContextSnapshot{
		BizModel: BizModelSummary{Version: 0},
	})
	if !ok || !strings.Contains(reason, "尚无业务建模快照") {
		t.Fatalf("无模型应建议补建模, ok=%v reason=%q", ok, reason)
	}
	// 过期模型 → 建议。
	stale := &TaskContextSnapshot{
		BizModel: BizModelSummary{Version: 3, UpdatedAt: time.Now().Add(-25 * time.Hour), LayerDiffs: 1},
	}
	if ok, reason := EdgeNeedsBusinessModel(stale); !ok || !strings.Contains(reason, "过期") {
		t.Fatalf("过期模型应建议补建模, ok=%v reason=%q", ok, reason)
	}
	// 差异积压 → 建议。
	backlog := &TaskContextSnapshot{
		BizModel: BizModelSummary{Version: 3, UpdatedAt: time.Now(), LayerDiffs: 21},
	}
	if ok, _ := EdgeNeedsBusinessModel(backlog); !ok {
		t.Fatal("差异积压超限应建议收敛")
	}
	// 新鲜且覆盖 → 不建议。
	fresh := &TaskContextSnapshot{
		BizModel: BizModelSummary{Version: 3, UpdatedAt: time.Now(), LayerDiffs: 2},
	}
	if ok, reason := EdgeNeedsBusinessModel(fresh); ok {
		t.Fatalf("新鲜模型不应建议补建模, reason=%q", reason)
	}
	// 分节失败 → 建议（显式失败不冒充"没有数据"）。
	errSnap := &TaskContextSnapshot{}
	errSnap.BizModel.Err = &testError{msg: "db down"}
	if ok, reason := EdgeNeedsBusinessModel(errSnap); !ok || !strings.Contains(reason, "db down") {
		t.Fatalf("分节失败应建议并带原因, ok=%v reason=%q", ok, reason)
	}
}

// ---------- 业务建模 → 偏锋 ----------

func TestDeltaShouldBecomeHypothesis(t *testing.T) {
	// 达标：范围内 high 差异 → 建议。
	ok, why := DeltaShouldBecomeHypothesis(ModelDeltaRef{ID: 12, TaskID: 1, DiffType: "missing_role", Severity: "high", InScope: true})
	if !ok || !strings.Contains(why, "待验证") {
		t.Fatalf("范围内 high 差异应建议转假设, ok=%v why=%q", ok, why)
	}
	// 越权锚定 → 拒绝（INV-1）。
	ok, why = DeltaShouldBecomeHypothesis(ModelDeltaRef{ID: 13, Severity: "critical", InScope: false})
	if ok || !strings.Contains(why, "不在任务范围内") {
		t.Fatalf("范围外差异应拒绝, ok=%v why=%q", ok, why)
	}
	// 低严重度 → 观察。
	ok, why = DeltaShouldBecomeHypothesis(ModelDeltaRef{ID: 14, Severity: "medium", InScope: true})
	if ok || !strings.Contains(why, "严重度未达 high") {
		t.Fatalf("medium 差异应留观察, ok=%v why=%q", ok, why)
	}
	// 无 id → 拒绝。
	if ok, _ := DeltaShouldBecomeHypothesis(ModelDeltaRef{Severity: "critical", InScope: true}); ok {
		t.Fatal("缺 id 应拒绝")
	}
}

// ---------- 指纹去重 ----------

func TestHypothesisFingerprint(t *testing.T) {
	a := HypothesisFingerprint(7, "普通用户可能执行越权状态转换。")
	b := HypothesisFingerprint(7, "普通  用户可能执行越权状态转换！")
	c := HypothesisFingerprint(7, "完全不同的另一条假设")
	d := HypothesisFingerprint(8, "普通用户可能执行越权状态转换")
	if a != b {
		t.Fatal("空白/标点差异不应改变指纹")
	}
	if a == c {
		t.Fatal("不同语句指纹应不同")
	}
	if a == d {
		t.Fatal("不同任务的指纹应隔离")
	}
}

// ---------- 防循环台账 ----------

func TestCollabLedgerLoopGuard(t *testing.T) {
	l := NewCollabLedger()
	// 同一 causation 对同一目标：前 MaxAutoFeedbackPerKey 次放行，之后拒绝。
	for i := 0; i < MaxAutoFeedbackPerKey; i++ {
		_, ok, why := l.AllowAutoFeedback("bizdiff#12", "exploration.edge")
		if !ok {
			t.Fatalf("第 %d 次应放行: %s", i+1, why)
		}
	}
	_, ok, why := l.AllowAutoFeedback("bizdiff#12", "exploration.edge")
	if ok || !strings.Contains(why, "已达上限") {
		t.Fatalf("超限应拒绝且带原因, ok=%v why=%q", ok, why)
	}
	// 对象更新出新 causation → 新预算。
	if _, ok, _ = l.AllowAutoFeedback("bizdiff#13", "exploration.edge"); !ok {
		t.Fatal("新 causation 应获得新预算")
	}
	// 拒绝时不占预算：拒绝后再查仍拒绝，计数不涨。
	if _, ok, _ = l.AllowAutoFeedback("bizdiff#12", "exploration.edge"); ok {
		t.Fatal("超限后应持续拒绝")
	}
	// 不同目标能力互不影响。
	if _, ok, _ = l.AllowAutoFeedback("bizdiff#12", "modeling.business"); !ok {
		t.Fatal("不同目标能力应有独立预算")
	}
	// 空参数拒绝。
	if _, ok, why := l.AllowAutoFeedback("", "exploration.edge"); ok || why == "" {
		t.Fatal("空 causation 应拒绝并给理由")
	}
}

func TestCollabLedgerRecordAndRecent(t *testing.T) {
	l := NewCollabLedger()
	for i := 0; i < 300; i++ { // 超容量，验证环形丢弃
		l.Record(CollabRecord{CausationID: "c", Status: "suggested"})
	}
	recs := l.Recent()
	if len(recs) != 256 {
		t.Fatalf("台账应封顶 256 条, got %d", len(recs))
	}
}

// 并发安全冒烟：并行 Allow/Record 不数据竞争（go test -race 下运行）。
func TestCollabLedgerConcurrent(t *testing.T) {
	l := NewCollabLedger()
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			l.AllowAutoFeedback("x#1", "exploration.edge")
			l.Record(CollabRecord{CausationID: "x#1"})
		}(i)
	}
	wg.Wait()
}

type testError struct{ msg string }

func (e *testError) Error() string { return e.msg }

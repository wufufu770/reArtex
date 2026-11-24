package db

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// jsonUnmarshalForTest 包装 json.Unmarshal（测试内统一错误处理）。
func jsonUnmarshalForTest(b []byte, v any) error { return json.Unmarshal(b, v) }

// TI-06 outbox SQL 语义测试（§0.6）：event_id 唯一去重、到期认领、状态迁移、
// 消费幂等台账。派发器逻辑测试在 events/dispatcher_test.go（内存 fake）。

func TestOutboxRoundTrip(t *testing.T) {
	d := tiSetup(t)

	// 入队：新事件 deduped=false；同 event_id 重投是 no-op（生产端幂等键）。
	rec := &DomainEventRecord{EventID: "evt-rt-1", Type: "hypothesis.proposed", TaskID: 0,
		CompanyID: 0, ExecutionID: "exec-1", CorrelationID: "corr-1", CausationID: "cause-1",
		ScopeVersion: 7, Payload: []byte(`{"statement":"s"}`), OccurredAt: time.Now()}
	if _, dedup, err := d.EnqueueEvent(rec); err != nil || dedup {
		t.Fatalf("首次入队: dedup=%v err=%v", dedup, err)
	}
	if _, dedup, err := d.EnqueueEvent(rec); err != nil || !dedup {
		t.Fatalf("重投应去重: dedup=%v err=%v", dedup, err)
	}
	var n int
	if err := d.QueryRow(`SELECT COUNT(*) FROM domain_events`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("去重后应只有 1 行: n=%d err=%v", n, err)
	}

	// 认领：到期事件被取回且字段完整。
	ctx := context.Background()
	claimed, err := d.ClaimDueEvents(ctx, 10)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("认领: n=%d err=%v", len(claimed), err)
	}
	got := claimed[0]
	if got.EventID != "evt-rt-1" || got.Type != "hypothesis.proposed" || got.ScopeVersion != 7 ||
		got.ExecutionID != "exec-1" {
		t.Fatalf("认领字段不符: %+v", got)
	}
	// payload 被 jsonb 规范化（空白/键序），按解析后语义比较。
	var wantPayload, gotPayload map[string]any
	if err := jsonUnmarshalForTest(rec.Payload, &wantPayload); err != nil {
		t.Fatal(err)
	}
	if err := jsonUnmarshalForTest(got.Payload, &gotPayload); err != nil {
		t.Fatal(err)
	}
	if wantPayload["statement"] != gotPayload["statement"] {
		t.Fatalf("payload 语义不符: want %v got %v", wantPayload, gotPayload)
	}

	// 认领不改变状态：手动收尾前不会被再次认领吗？——会被（at-least-once：
	// 状态仍是 pending）。这里验证的是收尾语义而不是认领锁。
	if err := d.MarkEventDispatched("evt-rt-1"); err != nil {
		t.Fatal(err)
	}
	claimed, err = d.ClaimDueEvents(ctx, 10)
	if err != nil || len(claimed) != 0 {
		t.Fatalf("dispatched 不应再被认领: n=%d err=%v", len(claimed), err)
	}

	// 失败退避：置 failed + 未来 next_attempt_at → 未到期不可认领；
	// dead 直接入死信不再认领。
	if err := d.MarkEventFailed("evt-rt-1", "boom", time.Now().Add(time.Hour), false); err != nil {
		t.Fatal(err)
	}
	if claimed, _ := d.ClaimDueEvents(ctx, 10); len(claimed) != 0 {
		t.Fatalf("退避期内不应认领: %d", len(claimed))
	}
	if err := d.MarkEventFailed("evt-rt-1", "boom×5", time.Time{}, true); err != nil {
		t.Fatal(err)
	}
	if claimed, _ := d.ClaimDueEvents(ctx, 10); len(claimed) != 0 {
		t.Fatalf("dead 不应被认领: %d", len(claimed))
	}
}

func TestOutboxConsumerIdempotency(t *testing.T) {
	d := tiSetup(t)

	done, err := d.HasConsumerProcessed("evt-c-1", "queue.hypothesis")
	if err != nil || done {
		t.Fatalf("初始应未处理: done=%v err=%v", done, err)
	}
	if err := d.MarkConsumerProcessed("evt-c-1", "queue.hypothesis"); err != nil {
		t.Fatal(err)
	}
	// 重复记账是 no-op；查询为真；不同消费者互不影响。
	if err := d.MarkConsumerProcessed("evt-c-1", "queue.hypothesis"); err != nil {
		t.Fatalf("重复记账应 no-op: %v", err)
	}
	if done, _ := d.HasConsumerProcessed("evt-c-1", "queue.hypothesis"); !done {
		t.Fatal("记账后应查到已处理")
	}
	if done, _ := d.HasConsumerProcessed("evt-c-1", "queue.evidence"); done {
		t.Fatal("不同消费者不应被连带标记")
	}
}

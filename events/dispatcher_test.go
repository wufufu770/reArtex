package events

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
)

// fakeOutbox 是派发器逻辑测试的内存 outbox（SQL 语义由 db/events_test.go 兜）。
type fakeOutbox struct {
	events    []*db.DomainEventRecord
	processed map[string]bool // event_id|consumer
	marks     []string        // 状态迁移轨迹（event_id:dispatched/failed/dead）

	claimErr error
}

func (f *fakeOutbox) EnqueueEvent(ev *db.DomainEventRecord) (bool, error) {
	for _, e := range f.events {
		if e.EventID == ev.EventID {
			return true, nil
		}
	}
	c := *ev
	if c.Status == "" {
		c.Status = "pending" // 与库层 DEFAULT 'pending' 同口径
	}
	f.events = append(f.events, &c)
	return false, nil
}

func (f *fakeOutbox) ClaimDueEvents(_ context.Context, limit int) ([]*db.DomainEventRecord, error) {
	if f.claimErr != nil {
		return nil, f.claimErr
	}
	out := []*db.DomainEventRecord{}
	for _, e := range f.events {
		if (e.Status == "pending" || e.Status == "failed") && len(out) < limit {
			out = append(out, e)
		}
	}
	return out, nil
}

func (f *fakeOutbox) MarkEventDispatched(eventID string) error {
	f.marks = append(f.marks, eventID+":dispatched")
	for _, e := range f.events {
		if e.EventID == eventID {
			e.Status = "dispatched"
		}
	}
	return nil
}

func (f *fakeOutbox) MarkEventFailed(eventID, lastErr string, next time.Time, dead bool) error {
	s := eventID + ":failed"
	for _, e := range f.events {
		if e.EventID == eventID {
			e.Attempts++ // dead 分支也计一次（与库层 attempts=attempts+1 同口径）
			if dead {
				e.Status = "dead"
				s = eventID + ":dead"
			} else {
				e.Status = "failed"
			}
		}
	}
	f.marks = append(f.marks, s)
	return nil
}

func (f *fakeOutbox) MarkConsumerProcessed(eventID, consumer string) error {
	f.processed[eventID+"|"+consumer] = true
	return nil
}

func (f *fakeOutbox) HasConsumerProcessed(eventID, consumer string) (bool, error) {
	return f.processed[eventID+"|"+consumer], nil
}

func newTestEvent(id string) *db.DomainEventRecord {
	return ToRecord(DomainEvent{ID: id, Type: EventHypothesisProposed, SchemaVersion: EventSchemaV1,
		TaskID: 1, Payload: []byte(`{"k":"v"}`), OccurredAt: time.Now()})
}

// 主成功路径：匹配类型的消费者收到事件、台账落账、事件标 dispatched；
// 不匹配类型的消费者不被调用。
func TestPollOnceHappyPath(t *testing.T) {
	fo := &fakeOutbox{processed: map[string]bool{}}
	_, err := fo.EnqueueEvent(newTestEvent("ev-1"))
	if err != nil {
		t.Fatal(err)
	}
	d := NewDispatcher(fo, DispatcherOptions{})
	var hits []string
	if err := d.RegisterConsumer("alpha", []string{EventHypothesisProposed}, func(_ context.Context, _ DomainEvent) error {
		hits = append(hits, "alpha")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := d.RegisterConsumer("beta", []string{EventEvidenceRecorded}, func(_ context.Context, _ DomainEvent) error {
		hits = append(hits, "beta")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	n, err := d.PollOnce(context.Background())
	if err != nil || n != 1 {
		t.Fatalf("PollOnce: n=%d err=%v", n, err)
	}
	if len(hits) != 1 || hits[0] != "alpha" {
		t.Fatalf("类型过滤失效: %v", hits)
	}
	if !fo.processed["ev-1|alpha"] {
		t.Fatal("消费台账应落账")
	}
	if len(fo.marks) != 1 || fo.marks[0] != "ev-1:dispatched" {
		t.Fatalf("事件应标 dispatched: %v", fo.marks)
	}
}

// 幂等路径：同一事件重投，已处理的消费者被台账跳过、不再执行。
func TestPollOnceIdempotentRedelivery(t *testing.T) {
	fo := &fakeOutbox{processed: map[string]bool{}}
	_, _ = fo.EnqueueEvent(newTestEvent("ev-r"))
	d := NewDispatcher(fo, DispatcherOptions{})
	calls := 0
	if err := d.RegisterConsumer("c1", nil, func(_ context.Context, _ DomainEvent) error {
		calls++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.PollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	// 模拟崩溃遗留：事件回到 pending 再投一次。
	fo.events[0].Status = "pending"
	if _, err := d.PollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("重投不应重复执行消费者, calls=%d", calls)
	}
}

// 失败路径：handler 失败 → 退避重试 → 超 MaxEventAttempts 入 dead；
// panic 与错误同样按失败处理，不拖死循环。
func TestPollOnceRetryThenDead(t *testing.T) {
	fo := &fakeOutbox{processed: map[string]bool{}}
	_, _ = fo.EnqueueEvent(newTestEvent("ev-f"))
	d := NewDispatcher(fo, DispatcherOptions{})
	if err := d.RegisterConsumer("bad", nil, func(_ context.Context, _ DomainEvent) error {
		return errors.New("下游故障")
	}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < MaxEventAttempts; i++ {
		fo.events[0].Status = "pending" // 每轮手动到期
		if _, err := d.PollOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if fo.events[0].Status != "dead" {
		t.Fatalf("超限应入 dead, got %s", fo.events[0].Status)
	}
	if fo.events[0].Attempts != MaxEventAttempts {
		t.Fatalf("attempts 应=%d, got %d", MaxEventAttempts, fo.events[0].Attempts)
	}
}

func TestPollOncePanicIsolated(t *testing.T) {
	fo := &fakeOutbox{processed: map[string]bool{}}
	_, _ = fo.EnqueueEvent(newTestEvent("ev-p"))
	d := NewDispatcher(fo, DispatcherOptions{})
	if err := d.RegisterConsumer("panicky", nil, func(_ context.Context, _ DomainEvent) error {
		panic("boom")
	}); err != nil {
		t.Fatal(err)
	}
	n, err := d.PollOnce(context.Background())
	if err != nil || n != 1 {
		t.Fatalf("panic 应被隔离不外抛: n=%d err=%v", n, err)
	}
	if len(fo.marks) == 0 || fo.marks[len(fo.marks)-1] != "ev-p:failed" {
		t.Fatalf("panic 应按失败收尾: %v", fo.marks)
	}
}

// 注册约束：同名消费者拒绝（幂等台账键唯一）；空参数拒绝；无消费者时事件
// 也正常标 dispatched（事件先落库，消费方可以后接）。
func TestRegisterConsumerRules(t *testing.T) {
	fo := &fakeOutbox{processed: map[string]bool{}}
	d := NewDispatcher(fo, DispatcherOptions{})
	fn := func(_ context.Context, _ DomainEvent) error { return nil }
	if err := d.RegisterConsumer("", nil, fn); err == nil {
		t.Fatal("空 name 应拒绝")
	}
	if err := d.RegisterConsumer("x", nil, nil); err == nil {
		t.Fatal("nil handler 应拒绝")
	}
	if err := d.RegisterConsumer("x", nil, fn); err != nil {
		t.Fatalf("首次注册应成功: %v", err)
	}
	if err := d.RegisterConsumer("x", nil, fn); err == nil {
		t.Fatal("同名重复注册应拒绝")
	}
	// 无消费者：事件仍然收尾（先落库后派发，消费方允许缺席）。
	_, _ = fo.EnqueueEvent(newTestEvent("ev-none"))
	if _, err := d.PollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(fo.marks) != 1 || fo.marks[0] != "ev-none:dispatched" {
		t.Fatalf("无消费者事件应标 dispatched: %v", fo.marks)
	}
}

// envelope ↔ 库行互转：水位字符串与 bigint 两个方向一致，0/空串语义对齐。
func TestToRecordRoundTrip(t *testing.T) {
	ev := DomainEvent{ID: "ev-x", Type: EventTaskGoalEvaluated, SchemaVersion: EventSchemaV1,
		TaskID: 3, TenantID: "9", ExecutionID: "exec-1", CorrelationID: "corr-1", CausationID: "cause-1",
		ScopeVersion: "42", Payload: []byte(`{}`)}
	rec := ToRecord(ev)
	if rec.CompanyID != 9 || rec.ScopeVersion != 42 || rec.SchemaVersion != EventSchemaV1 {
		t.Fatalf("ToRecord 投影不符: %+v", rec)
	}
	back := envelope(rec)
	if back.TenantID != "9" || back.ScopeVersion != "42" || back.ID != "ev-x" {
		t.Fatalf("envelope 回投影不符: %+v", back)
	}
	// 空串 ↔ 0 对齐；非法值安全落 0。
	if v := idStringToInt(""); v != 0 {
		t.Fatalf("空串应落 0, got %d", v)
	}
	if s := intToIDString(0); s != "" {
		t.Fatalf("0 应投影空串, got %q", s)
	}
	if v := idStringToInt("not-a-number"); v != 0 {
		t.Fatalf("非法值应安全落 0, got %d", v)
	}
}

// ValidateEvent 契约门：缺 ID/Type、版本不符、nil payload 在入口拒绝。
func TestValidateEvent(t *testing.T) {
	ok := DomainEvent{ID: "e1", Type: EventEvidenceRecorded, SchemaVersion: EventSchemaV1, Payload: []byte(`{}`)}
	if err := ValidateEvent(ok); err != nil {
		t.Fatalf("合法事件不应报错: %v", err)
	}
	noID := ok
	noID.ID = ""
	if err := ValidateEvent(noID); err == nil {
		t.Fatal("缺 ID 应拒绝")
	}
	badVer := ok
	badVer.SchemaVersion = 99
	if err := ValidateEvent(badVer); err == nil {
		t.Fatal("版本不符应拒绝")
	}
	noPayload := ok
	noPayload.Payload = nil
	if err := ValidateEvent(noPayload); err == nil {
		t.Fatal("nil payload 应拒绝（无载荷显式 {}）")
	}
}

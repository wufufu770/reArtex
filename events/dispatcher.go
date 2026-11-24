package events

// dispatcher.go —— §0.6 事件派发控制面（0.15.64，批次 TI-06）。
//
// 职责：轮询 outbox（db.ClaimDueEvents，FOR UPDATE SKIP LOCKED）→ 按事件类型
// 分发给已注册的消费者 → 消费幂等（domain_event_consumers 台账先查后记）→
// 失败指数退避重试、超限入 dead。派发语义是 at-least-once：崩溃在「handler
// 已执行、台账未记」之间会重投，handler 自身必须幂等。
//
// 接线边界：本派发器不在 server 启动序列装配（归 TI-11 及后续）；测试用
// 内存 fake store 驱动（dispatcher_test.go），SQL 语义由 db/events_test.go 兜。

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/Autumn-27/artex/db"
)

// 重试策略（可观测面与 db/domain_events 列一一对应）：
//
//	attempts 达 MaxEventAttempts → status='dead'，不再重试（人工排查面）；
//	否则 next_attempt_at = now() + base*2^(attempts-1)，封顶 RetryBackoffCap。
const (
	MaxEventAttempts    = 5
	RetryBackoffBase    = 30 * time.Second
	RetryBackoffCap     = 10 * time.Minute
	DefaultPollInterval = 2 * time.Second
	DefaultBatchSize    = 32
)

// OutboxStore 是派发器对 outbox 的窄依赖（*db.DB 结构性满足；测试用 fake）。
type OutboxStore interface {
	EnqueueEvent(ev *db.DomainEventRecord) (deduped bool, err error)
	ClaimDueEvents(ctx context.Context, limit int) ([]*db.DomainEventRecord, error)
	MarkEventDispatched(eventID string) error
	MarkEventFailed(eventID, lastErr string, nextAttemptAt time.Time, dead bool) error
	MarkConsumerProcessed(eventID, consumer string) error
	HasConsumerProcessed(eventID, consumer string) (bool, error)
}

// EventHandler 是消费者回调。返回错误 = 本消费者本轮失败（事件整体重试）；
// panic 被派发器拦下按失败处理，不允许一个消费者的崩溃拖死轮询循环。
type EventHandler func(ctx context.Context, ev DomainEvent) error

// consumer 一条订阅：名称（幂等台账键）+ 订阅的事件类型（nil = 全类型）。
type consumer struct {
	name  string
	types map[string]bool
	fn    EventHandler
}

// Dispatcher 是单体内的 outbox 派发器。
type Dispatcher struct {
	store    OutboxStore
	interval time.Duration
	batch    int

	mu        sync.RWMutex
	consumers []*consumer
}

// DispatcherOptions 派发器参数（零值取缺省）。
type DispatcherOptions struct {
	PollInterval time.Duration
	BatchSize    int
}

// NewDispatcher 产出一个派发器。
func NewDispatcher(store OutboxStore, opts DispatcherOptions) *Dispatcher {
	if opts.PollInterval <= 0 {
		opts.PollInterval = DefaultPollInterval
	}
	if opts.BatchSize <= 0 {
		opts.BatchSize = DefaultBatchSize
	}
	return &Dispatcher{store: store, interval: opts.PollInterval, batch: opts.BatchSize}
}

// RegisterConsumer 注册消费者。name 必须全局唯一（它是幂等台账的键，重复
// 注册同名的第二个 handler 不会生效）；types 为 nil 订阅全部事件类型。
func (d *Dispatcher) RegisterConsumer(name string, types []string, fn EventHandler) error {
	if name == "" || fn == nil {
		return fmt.Errorf("消费者缺 name 或 handler")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, c := range d.consumers {
		if c.name == name {
			return fmt.Errorf("消费者 %q 已注册——同名同幂等键，不允许第二个 handler", name)
		}
	}
	tm := map[string]bool{}
	for _, t := range types {
		tm[t] = true
	}
	d.consumers = append(d.consumers, &consumer{name: name, types: tm, fn: fn})
	// 稳定顺序：同事件多消费者的执行顺序确定（可复现，便于测试与排查）。
	sort.Slice(d.consumers, func(i, j int) bool { return d.consumers[i].name < d.consumers[j].name })
	return nil
}

// Start 阻塞式轮询循环（ctx 取消即退出；server 装配方负责放 goroutine）。
func (d *Dispatcher) Start(ctx context.Context) {
	ticker := time.NewTicker(d.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// 轮询失败不终止循环：outbox 短暂不可用只推迟本轮，可观测靠日志。
			if _, err := d.PollOnce(ctx); err != nil {
				log.Printf("[events] 轮询失败（下一轮重试）: %v", err)
			}
		}
	}
}

// PollOnce 派发一批到期事件，返回本批处理条数。
func (d *Dispatcher) PollOnce(ctx context.Context) (int, error) {
	events, err := d.store.ClaimDueEvents(ctx, d.batch)
	if err != nil {
		return 0, err
	}
	for _, rec := range events {
		d.dispatch(ctx, rec)
	}
	return len(events), nil
}

// dispatch 处理单条事件：消费者逐个跑（幂等台账跳过已处理）→ 全部成功标
// dispatched；任一失败标 failed/dead 并给退避。单条事件的失败不影响同批其他条。
func (d *Dispatcher) dispatch(ctx context.Context, rec *db.DomainEventRecord) {
	ev := envelope(rec)
	d.mu.RLock()
	consumers := d.consumers
	d.mu.RUnlock()

	var firstErr error
	for _, c := range consumers {
		if len(c.types) > 0 && !c.types[rec.Type] {
			continue
		}
		done, err := d.store.HasConsumerProcessed(rec.EventID, c.name)
		if err != nil {
			firstErr = fmt.Errorf("查消费台账失败（%s）: %w", c.name, err)
			break
		}
		if done {
			continue
		}
		if err := runHandler(ctx, c.fn, ev); err != nil {
			firstErr = fmt.Errorf("消费者 %s 处理失败: %w", c.name, err)
			break
		}
		if err := d.store.MarkConsumerProcessed(rec.EventID, c.name); err != nil {
			firstErr = fmt.Errorf("记消费台账失败（%s）: %w", c.name, err)
			break
		}
	}

	if firstErr == nil {
		if err := d.store.MarkEventDispatched(rec.EventID); err != nil {
			// 事件已全部消费成功、只是状态列没写成功：按失败重投处理，
			// 重投后消费者被台账跳过，幂等性保证不产生二次副作用。
			log.Printf("[events] 事件 %s 标记 dispatched 失败（将重投，消费幂等兜底）: %v", rec.EventID, err)
			_ = d.fail(ctx, rec, "mark dispatched 失败")
		}
		return
	}
	log.Printf("[events] 事件 %s 第 %d 次处理失败: %v", rec.EventID, rec.Attempts+1, firstErr)
	_ = d.fail(ctx, rec, firstErr.Error())
}

// fail 计算退避并落失败态；超 MaxEventAttempts 入 dead。
func (d *Dispatcher) fail(_ context.Context, rec *db.DomainEventRecord, msg string) error {
	attempts := rec.Attempts + 1
	if attempts >= MaxEventAttempts {
		return d.store.MarkEventFailed(rec.EventID, msg, time.Time{}, true)
	}
	backoff := RetryBackoffBase << (attempts - 1) // 30s, 60s, 120s, 240s
	if backoff > RetryBackoffCap {
		backoff = RetryBackoffCap
	}
	return d.store.MarkEventFailed(rec.EventID, msg, time.Now().Add(backoff), false)
}

// runHandler 带 panic 隔离地执行一个消费者回调。
func runHandler(ctx context.Context, fn EventHandler, ev DomainEvent) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("消费者 panic（已隔离）: %v", r)
		}
	}()
	return fn(ctx, ev)
}

// envelope 把库行投影回 §0.6 契约 envelope（bigint→string 的方向）。
func envelope(rec *db.DomainEventRecord) DomainEvent {
	return DomainEvent{
		ID:            rec.EventID,
		Type:          rec.Type,
		SchemaVersion: rec.SchemaVersion,
		TaskID:        rec.TaskID,
		TenantID:      intToIDString(rec.CompanyID),
		ExecutionID:   rec.ExecutionID,
		CorrelationID: rec.CorrelationID,
		CausationID:   rec.CausationID,
		ScopeVersion:  intToIDString(rec.ScopeVersion),
		OccurredAt:    rec.OccurredAt,
		Payload:       rec.Payload,
	}
}

// ToRecord 把 §0.6 契约 envelope 投影为 outbox 库行（生产方入队用；TenantID /
// ScopeVersion 的字符串→水位 bigint 方向，空串/非法值落 0 = 未记录）。
// SchemaVersion 为 0 时补当前契约版本；payload 为空补 {}。
func ToRecord(ev DomainEvent) *db.DomainEventRecord {
	sv := ev.SchemaVersion
	if sv == 0 {
		sv = EventSchemaV1
	}
	payload := ev.Payload
	if len(payload) == 0 {
		payload = json.RawMessage("{}")
	}
	return &db.DomainEventRecord{
		EventID:       ev.ID,
		Type:          ev.Type,
		SchemaVersion: sv,
		TaskID:        ev.TaskID,
		CompanyID:     idStringToInt(ev.TenantID),
		ExecutionID:   ev.ExecutionID,
		CorrelationID: ev.CorrelationID,
		CausationID:   ev.CausationID,
		ScopeVersion:  idStringToInt(ev.ScopeVersion),
		Payload:       payload,
		OccurredAt:    ev.OccurredAt,
	}
}

// idStringToInt envelope 字符串 → 水位/企业 bigint（空串/非法 = 0，与库层同语义）。
func idStringToInt(s string) int64 {
	if s == "" {
		return 0
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0
	}
	return v
}

// intToIDString 水位/企业 id → envelope 字符串（0 = 空串，方案语义「未记录」）。
func intToIDString(v int64) string {
	if v <= 0 {
		return ""
	}
	return fmt.Sprintf("%d", v)
}

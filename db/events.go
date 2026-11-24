package db

// 统一事件 outbox 访问层（v2.1 统一架构优化增补 §0.6，0.15.64，批次 TI-06）。
//
// 分工与纪律：
//   - DDL 在 db/schema_task_intelligence.sql（domain_events /
//     domain_event_consumers 两表），随 EnsureTaskIntelligenceSchema 启动期应用；
//   - 本层只做存储：入队（event_id 唯一 = 生产端幂等）、认领（FOR UPDATE SKIP
//     LOCKED，多实例安全）、状态迁移、消费幂等台账。重试/退避/dead 判定等
//     **策略在 events.Dispatcher**，不在这里；
//   - 派发语义 at-least-once：崩溃在任意两步之间会重投，幂等靠消费台账 +
//     handler 自身幂等（events 包文档有完整口径）。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// DomainEventRecord 是 domain_events 表的一行（存储投影；§0.6 envelope 与本
// 结构的互转在 events 包：ToRecord / envelope）。
type DomainEventRecord struct {
	ID            int64           `json:"id"`
	EventID       string          `json:"event_id"`
	Type          string          `json:"type"`
	SchemaVersion int             `json:"schema_version"`
	TaskID        int64           `json:"task_id"`
	CompanyID     int64           `json:"company_id"` // envelope.TenantID 投影（0=无企业维度）
	ExecutionID   string          `json:"execution_id"`
	CorrelationID string          `json:"correlation_id"`
	CausationID   string          `json:"causation_id"`
	ScopeVersion  int64           `json:"scope_version"` // envelope.ScopeVersion 投影（0=未记录）
	Payload       json.RawMessage `json:"payload"`
	Status        string          `json:"status"`
	Attempts      int             `json:"attempts"`
	NextAttemptAt time.Time       `json:"next_attempt_at"`
	LastError     string          `json:"last_error"`
	OccurredAt    time.Time       `json:"occurred_at"`
	CreatedAt     time.Time       `json:"created_at"`
}

// idStringToInt envelope 字符串 → 水位/企业 bigint（空串/非法 = 0）。
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

// EnqueueEvent 把一条事件写入 outbox（status=pending）。event_id 唯一索引兜底：
// 同一事件重复入队是 no-op，返回 deduped=true 与既有行 id——生产端重试安全。
func (d *DB) EnqueueEvent(ev *DomainEventRecord) (id int64, deduped bool, err error) {
	if err := d.ensureTaskIntelligenceSchema(); err != nil {
		return 0, false, err
	}
	if ev.EventID == "" || ev.Type == "" {
		return 0, false, fmt.Errorf("事件缺 event_id/type")
	}
	if len(ev.Payload) == 0 {
		ev.Payload = json.RawMessage("{}")
	}
	// §0.12 消毒门（TI-12，0.15.70）：存储单点强制，生产方无法绕过。
	// 凭据类键值改写占位符；超限/非 JSON 整体拒绝。
	clean, redacted, err := SanitizeEventPayload(ev.Payload)
	if err != nil {
		return 0, false, fmt.Errorf("事件 %s 消毒拒绝: %w", ev.EventID, err)
	}
	ev.Payload = clean
	_ = redacted // 键路径已记录进 payload 的 _sanitized 标记块（可观测面）
	if ev.SchemaVersion == 0 {
		ev.SchemaVersion = 1
	}
	if ev.OccurredAt.IsZero() {
		ev.OccurredAt = time.Now()
	}
	row := d.QueryRow(`
INSERT INTO domain_events(event_id, type, schema_version, task_id, company_id,
                          execution_id, correlation_id, causation_id, scope_version,
                          payload, occurred_at)
VALUES ($1,$2,$3,NULLIF($4,0),NULLIF($5,0),$6,$7,$8,$9,$10::jsonb,$11)
ON CONFLICT (event_id) DO NOTHING
RETURNING id`,
		ev.EventID, ev.Type, ev.SchemaVersion, ev.TaskID, ev.CompanyID,
		ev.ExecutionID, ev.CorrelationID, ev.CausationID, ev.ScopeVersion,
		string(ev.Payload), ev.OccurredAt)
	if err := row.Scan(&id); err == nil {
		return id, false, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return 0, false, fmt.Errorf("enqueue event: %w", err)
	}
	// ON CONFLICT 命中：取既有行（幂等重投面）。
	err = d.QueryRow(`SELECT id FROM domain_events WHERE event_id=$1`, ev.EventID).Scan(&id)
	if err != nil {
		return 0, false, err
	}
	return id, true, nil
}

// ClaimDueEvents 认领一批到期事件（status pending/failed 且 next_attempt_at
// 已到）。FOR UPDATE SKIP LOCKED：多实例/多轮询并发认领互不重复。返回行保持
// 原状态不动——派发器处理完后用 MarkEventDispatched / MarkEventFailed 收尾；
// 崩溃遗留的行为保持到期可再认领（at-least-once 的来源）。
func (d *DB) ClaimDueEvents(ctx context.Context, limit int) ([]*DomainEventRecord, error) {
	if err := d.ensureTaskIntelligenceSchema(); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 500 {
		limit = 32
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck
	rows, err := tx.QueryContext(ctx, `
SELECT id, event_id, type, schema_version, COALESCE(task_id,0), COALESCE(company_id,0),
       execution_id, correlation_id, causation_id, scope_version, payload,
       status, attempts, next_attempt_at, last_error, occurred_at, created_at
FROM domain_events
WHERE status IN ('pending','failed') AND next_attempt_at <= now()
ORDER BY id
LIMIT $1
FOR UPDATE SKIP LOCKED`, limit)
	if err != nil {
		return nil, err
	}
	var out []*DomainEventRecord
	for rows.Next() {
		r := &DomainEventRecord{}
		if err := rows.Scan(&r.ID, &r.EventID, &r.Type, &r.SchemaVersion, &r.TaskID, &r.CompanyID,
			&r.ExecutionID, &r.CorrelationID, &r.CausationID, &r.ScopeVersion, &r.Payload,
			&r.Status, &r.Attempts, &r.NextAttemptAt, &r.LastError, &r.OccurredAt, &r.CreatedAt); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, r)
	}
	rows.Close()
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}

// MarkEventDispatched 派发成功收尾。
func (d *DB) MarkEventDispatched(eventID string) error {
	if err := d.ensureTaskIntelligenceSchema(); err != nil {
		return err
	}
	_, err := d.Exec(`UPDATE domain_events SET status='dispatched', last_error='', next_attempt_at=now() WHERE event_id=$1`,
		eventID)
	return err
}

// MarkEventFailed 失败收尾：dead=true 直接入死信（不再重试）；否则置 failed 并
// 写派发器算好的 next_attempt_at（退避策略在 events.Dispatcher，存储层不算策略）。
func (d *DB) MarkEventFailed(eventID, lastErr string, nextAttemptAt time.Time, dead bool) error {
	if err := d.ensureTaskIntelligenceSchema(); err != nil {
		return err
	}
	if dead {
		_, err := d.Exec(`UPDATE domain_events SET status='dead', attempts=attempts+1, last_error=$2 WHERE event_id=$1`,
			eventID, lastErr)
		return err
	}
	_, err := d.Exec(`UPDATE domain_events SET status='failed', attempts=attempts+1, last_error=$2, next_attempt_at=$3 WHERE event_id=$1`,
		eventID, lastErr, nextAttemptAt)
	return err
}

// MarkConsumerProcessed 记消费幂等台账（主键 event_id+consumer；重复记是 no-op）。
func (d *DB) MarkConsumerProcessed(eventID, consumer string) error {
	if err := d.ensureTaskIntelligenceSchema(); err != nil {
		return err
	}
	_, err := d.Exec(`
INSERT INTO domain_event_consumers(event_id, consumer) VALUES ($1,$2)
ON CONFLICT (event_id, consumer) DO NOTHING`, eventID, consumer)
	return err
}

// HasConsumerProcessed 查消费幂等台账。
func (d *DB) HasConsumerProcessed(eventID, consumer string) (bool, error) {
	if err := d.ensureTaskIntelligenceSchema(); err != nil {
		return false, err
	}
	var one int
	err := d.QueryRow(`SELECT 1 FROM domain_event_consumers WHERE event_id=$1 AND consumer=$2`,
		eventID, consumer).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

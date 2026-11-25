package db

// A-13（0.15.82）：凭据熔断器状态的持久化（profile + host 维度）。
//
// cred_breaker_state 是新表（非基线列演进），但仍走独立幂等 DDL 文件 +
// EnsureCredBreakerSchema + 启动迁移块 + markRuntimeSchemasReady 的既定管线，
// 与 llm_profile_health 的生命周期管理同款（受限角色无 DDL 权限，运行期懒执行
// 变空操作——v0.9.16 事故的既定修法）。

import (
	_ "embed"
	"fmt"
	"sync"
	"time"
)

//go:embed schema_credbreaker.sql
var credBreakerSchemaSQL string

// credBreakerSchemaReady 记录某个连接已经应用过 cred_breaker_state DDL。
var credBreakerSchemaReady sync.Map

// EnsureCredBreakerSchema 应用凭据熔断状态表 DDL（幂等）。
func (d *DB) EnsureCredBreakerSchema() error {
	if _, ok := credBreakerSchemaReady.Load(d.DB); ok {
		return nil
	}
	if _, err := d.Exec(credBreakerSchemaSQL); err != nil {
		return fmt.Errorf("应用凭据熔断状态 schema 失败: %w", err)
	}
	credBreakerSchemaReady.Store(d.DB, struct{}{})
	return nil
}

// CredBreakerState 是一行熔断状态的持久化形态。
type CredBreakerState struct {
	ProfileID   int64      `json:"profile_id"`
	Host        string     `json:"host"`
	Consecutive int        `json:"consecutive"`
	OpenUntil   *time.Time `json:"open_until"` // nil/过去 = 未断路
	LastReason  string     `json:"last_reason"`
	LastAt      time.Time  `json:"last_at"`
}

// SaveCredBreakerState upsert 一个键的熔断状态（persist 回调的落库侧）。
func (d *DB) SaveCredBreakerState(s CredBreakerState) error {
	_, err := d.Exec(`
INSERT INTO cred_breaker_state(profile_id,host,consecutive,open_until,last_reason,last_at)
VALUES ($1,$2,$3,$4,$5,now())
ON CONFLICT (profile_id,host) DO UPDATE SET
  consecutive=EXCLUDED.consecutive, open_until=EXCLUDED.open_until,
  last_reason=EXCLUDED.last_reason, last_at=now()`,
		s.ProfileID, s.Host, s.Consecutive, s.OpenUntil, s.LastReason)
	return err
}

// ClearCredBreakerState 删除一个键的状态（成功复位 / 手动恢复的 forget 侧）。
func (d *DB) ClearCredBreakerState(profileID int64, host string) error {
	_, err := d.Exec(`DELETE FROM cred_breaker_state WHERE profile_id=$1 AND host=$2`, profileID, host)
	return err
}

// LoadCredBreakerState 返回仍处于未过期断路窗口内的行。已自然到期的行刻意不取：
// 重启后冷却完毕的凭据视为健康，由下一次调用重新学习（与 LoadLLMHealth 同口径）。
func (d *DB) LoadCredBreakerState() ([]CredBreakerState, error) {
	rows, err := d.Query(`SELECT profile_id,host,consecutive,open_until,COALESCE(last_reason,''),last_at
FROM cred_breaker_state WHERE open_until IS NOT NULL AND open_until > now()`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CredBreakerState
	for rows.Next() {
		var s CredBreakerState
		if err := rows.Scan(&s.ProfileID, &s.Host, &s.Consecutive, &s.OpenUntil, &s.LastReason, &s.LastAt); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

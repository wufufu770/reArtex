package db

// 意图冲突仲裁数据层（0.14.1，批次二 A）。
//
// 纪律（施工方案 v2）：
//   - 归一对（存储时按 node id 排序）在 state=open 时唯一——同一冲突对不重复登记；
//   - verdict 枚举 proceed_a/proceed_b/defer_both，宿主写函数白名单校验；
//   - 裁决审计在 server 侧写 audit_events（source=intent-arbitration），本层不管审计；
//   - DDL 独立幂等文件 db/schema_intent2.sql（schema.sql 是 RESERVED），
//     EnsureIntent2Schema 启动期接线。

import (
	_ "embed"
	"fmt"
	"sync"
	"time"
)

//go:embed schema_intent2.sql
var intent2SchemaSQL string

var intent2SchemaReady sync.Map

// EnsureIntent2Schema 应用意图冲突表的幂等 DDL。
func (d *DB) EnsureIntent2Schema() error {
	if _, ok := intent2SchemaReady.Load(d.DB); ok {
		return nil
	}
	if _, err := d.Exec(intent2SchemaSQL); err != nil {
		return fmt.Errorf("应用意图冲突 schema 失败: %w", err)
	}
	intent2SchemaReady.Store(d.DB, struct{}{})
	return nil
}

// IntentConflict 是一行冲突记录（清单面字段全量）。
type IntentConflict struct {
	ID            int64      `json:"id"`
	ExplorationID int64      `json:"exploration_id"`
	ANodeID       int64      `json:"a_node_id"`
	BNodeID       int64      `json:"b_node_id"`
	DetectRule    string     `json:"detect_rule"`
	Basis         []byte     `json:"basis"`
	State         string     `json:"state"`
	Verdict       *string    `json:"verdict"`
	VerdictReason string     `json:"verdict_reason"`
	ResolvedBy    string     `json:"resolved_by"`
	ReopenCount   int        `json:"reopen_count"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

// conflictPair 归一冲突对的存储方向（小 id 在前——与对端无关的稳定序）。
func conflictPair(a, b int64) (int64, int64) {
	if a <= b {
		return a, b
	}
	return b, a
}

// RecordConflict 登记一条冲突（open 态；归一对已 open 时幂等返回既有 id）。
// 返回 (conflictID, created, err)。
func (d *DB) RecordConflict(explorationID, nodeA, nodeB int64, rule string, basis []byte) (int64, bool, error) {
	if err := d.EnsureIntent2Schema(); err != nil {
		return 0, false, err
	}
	if explorationID <= 0 || nodeA <= 0 || nodeB <= 0 || nodeA == nodeB {
		return 0, false, fmt.Errorf("冲突对参数非法：exploration=%d a=%d b=%d", explorationID, nodeA, nodeB)
	}
	if rule == "" {
		return 0, false, fmt.Errorf("detect_rule 必填")
	}
	a, b := conflictPair(nodeA, nodeB)
	var id int64
	var created bool
	err := d.QueryRow(`
INSERT INTO intent_conflicts (exploration_id, a_node_id, b_node_id, detect_rule, basis)
VALUES ($1,$2,$3,$4,$5)
ON CONFLICT (exploration_id, a_node_id, b_node_id) WHERE state='open'
DO UPDATE SET updated_at = now()
RETURNING id, (xmax = 0) AS created`, explorationID, a, b, rule, basis).Scan(&id, &created)
	if err != nil {
		return 0, false, fmt.Errorf("record conflict: %w", err)
	}
	return id, created, nil
}

// ResolveConflict 宿主落笔：open/frozen → adjudicated，写 verdict 与理由。
// resolvedBy 白名单 arbiter|host|host-rule（agent 只能经宿主函数以 arbiter 身份落笔，
// 没有任何直接改库路径）。verdict 枚举校验在这里。
func (d *DB) ResolveConflict(id int64, verdict, reason, resolvedBy string) error {
	if err := d.EnsureIntent2Schema(); err != nil {
		return err
	}
	switch verdict {
	case "proceed_a", "proceed_b", "defer_both":
	default:
		return fmt.Errorf("verdict 只接受 proceed_a|proceed_b|defer_both")
	}
	switch resolvedBy {
	case "arbiter", "host", "host-rule":
	default:
		return fmt.Errorf("resolved_by 只接受 arbiter|host|host-rule")
	}
	if reason == "" {
		return fmt.Errorf("裁决理由必填（写审计链）")
	}
	res, err := d.Exec(`
UPDATE intent_conflicts
SET state='adjudicated', verdict=$2, verdict_reason=$3, resolved_by=$4, updated_at=now()
WHERE id=$1 AND state IN ('open','frozen')`, id, verdict, reason, resolvedBy)
	if err != nil {
		return fmt.Errorf("resolve conflict: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("冲突 #%d 不存在或不在可裁决态", id)
	}
	return nil
}

// FreezeConflict 仲裁熔断：open → frozen（连续 defer，等人兜底）。
func (d *DB) FreezeConflict(id int64) error {
	if err := d.EnsureIntent2Schema(); err != nil {
		return err
	}
	res, err := d.Exec(`UPDATE intent_conflicts SET state='frozen', updated_at=now() WHERE id=$1 AND state='open'`, id)
	if err != nil {
		return fmt.Errorf("freeze conflict: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("冲突 #%d 不存在或不在 open 态", id)
	}
	return nil
}

// ReopenConflict 重开：adjudicated/frozen → open，reopen_count+1，清空裁决。
// 新事实出现（程序性）或人工覆盖（verdict 由下一轮仲裁/人工重下）时调。
func (d *DB) ReopenConflict(id int64) error {
	if err := d.EnsureIntent2Schema(); err != nil {
		return err
	}
	res, err := d.Exec(`
UPDATE intent_conflicts
SET state='open', verdict=NULL, verdict_reason='', resolved_by='', reopen_count=reopen_count+1, updated_at=now()
WHERE id=$1 AND state IN ('adjudicated','frozen')`, id)
	if err != nil {
		return fmt.Errorf("reopen conflict: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("冲突 #%d 不存在或不在可重开态", id)
	}
	return nil
}

// ListConflictsByExploration 列一个探索的冲突（state 过滤，空=全部）。
func (d *DB) ListConflictsByExploration(explorationID int64, state string, limit int) ([]*IntentConflict, error) {
	if err := d.EnsureIntent2Schema(); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 100
	}
	q := `SELECT id, exploration_id, a_node_id, b_node_id, detect_rule, basis, state,
	     verdict, verdict_reason, resolved_by, reopen_count, created_at, updated_at
FROM intent_conflicts WHERE exploration_id=$1`
	args := []any{explorationID}
	if state != "" {
		q += fmt.Sprintf(` AND state=$%d`, len(args)+1)
		args = append(args, state)
	}
	q += fmt.Sprintf(` ORDER BY id DESC LIMIT %d`, limit)
	rows, err := d.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*IntentConflict{}
	for rows.Next() {
		c := &IntentConflict{}
		if err := rows.Scan(&c.ID, &c.ExplorationID, &c.ANodeID, &c.BNodeID, &c.DetectRule,
			&c.Basis, &c.State, &c.Verdict, &c.VerdictReason, &c.ResolvedBy,
			&c.ReopenCount, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

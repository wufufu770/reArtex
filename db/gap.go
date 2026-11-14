package db

// 覆盖缺口数据层（0.14.3，批次二 F4）。
//
// 纪律：缺口判定是结构化确定性的（链接到任务且无 open 意图锚的资产）；
// 状态机 open→targeted→closed 原位演进（task×asset 唯一）；closed 不被重扫
// 复活；派生决策在 planner/bizmodeler（LLM 提议），平台不自动派生。

import (
	"database/sql"
	_ "embed"
	"fmt"
	"sync"
	"time"
)

//go:embed schema_gap.sql
var gapSchemaSQL string

var gapSchemaReady sync.Map

// EnsureGapSchema 应用覆盖缺口表的幂等 DDL。
func (d *DB) EnsureGapSchema() error {
	if _, ok := gapSchemaReady.Load(d.DB); ok {
		return nil
	}
	if _, err := d.Exec(gapSchemaSQL); err != nil {
		return fmt.Errorf("应用覆盖缺口 schema 失败: %w", err)
	}
	gapSchemaReady.Store(d.DB, struct{}{})
	return nil
}

// CoverageGap 是一行缺口账。
type CoverageGap struct {
	ID         int64     `json:"id"`
	TaskID     int64     `json:"task_id"`
	AssetID    int64     `json:"asset_id"`
	Reason     string    `json:"reason"`
	State      string    `json:"state"`
	TargetedBy *int64    `json:"targeted_by"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// ComputeCoverageGaps 对一个任务做缺口对账（确定性、幂等）：
//   - 链接到任务且无 open 意图锚的资产 → upsert open（closed 不复活）；
//   - 已登记缺口且资产获得了 open 意图锚 → open → targeted。
// 返回本轮 upsert 影响的行数（仅观测用）。
func (d *DB) ComputeCoverageGaps(taskID int64) (int, error) {
	if err := d.EnsureGapSchema(); err != nil {
		return 0, err
	}
	res, err := d.Exec(`
WITH scoped AS (
  SELECT DISTINCT l.asset_id
  FROM task_asset_links l
  JOIN assets a ON a.id = l.asset_id
  WHERE l.task_id=$1
),
covered AS (
  SELECT DISTINCT an.asset_id
  FROM exploration_anchors an
  JOIN exploration_nodes n ON n.id = an.node_id
  WHERE n.kind='intent' AND n.state='open'
    AND n.exploration_id = (SELECT exploration_id FROM tasks WHERE id=$1)
),
gaps AS (SELECT asset_id FROM scoped EXCEPT SELECT asset_id FROM covered)
INSERT INTO coverage_gaps (task_id, asset_id, reason)
SELECT $1, g.asset_id, '链接到任务但无 open 意图锚定'
FROM gaps g
ON CONFLICT (task_id, asset_id) DO UPDATE
  SET state = CASE WHEN coverage_gaps.state = 'closed' THEN 'closed' ELSE 'open' END,
      updated_at = now()
WHERE coverage_gaps.state IN ('open','targeted')
RETURNING 1`, taskID)
	if err != nil {
		return 0, fmt.Errorf("compute gaps (open): %w", err)
	}
	n1, _ := res.RowsAffected()

	// 获得锚的资产：open → targeted。
	res2, err := d.Exec(`
UPDATE coverage_gaps g SET state='targeted', updated_at=now()
WHERE g.task_id=$1 AND g.state='open'
  AND EXISTS (
    SELECT 1 FROM exploration_anchors an
    JOIN exploration_nodes n ON n.id = an.node_id
    WHERE an.asset_id = g.asset_id AND n.kind='intent' AND n.state='open'
      AND n.exploration_id = (SELECT exploration_id FROM tasks WHERE id=$1)
  )`, taskID)
	if err != nil {
		return int(n1), fmt.Errorf("compute gaps (targeted): %w", err)
	}
	n2, _ := res2.RowsAffected()
	return int(n1 + n2), nil
}

// CloseCoverageGap 人工闭合（宿主唯一写路径；server 侧写审计）。
func (d *DB) CloseCoverageGap(id int64) error {
	if err := d.EnsureGapSchema(); err != nil {
		return err
	}
	res, err := d.Exec(`UPDATE coverage_gaps SET state='closed', updated_at=now() WHERE id=$1 AND state IN ('open','targeted')`, id)
	if err != nil {
		return fmt.Errorf("close gap: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("缺口 #%d 不存在或已闭合", id)
	}
	return nil
}

// ListCoverageGaps 列任务缺口（state 过滤，空=全部）。
func (d *DB) ListCoverageGaps(taskID int64, state string, limit int) ([]*CoverageGap, error) {
	if err := d.EnsureGapSchema(); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 200
	}
	q := `SELECT id, task_id, asset_id, reason, state, targeted_by, created_at, updated_at
FROM coverage_gaps WHERE task_id=$1`
	args := []any{taskID}
	if state != "" {
		q += fmt.Sprintf(` AND state=$%d`, len(args)+1)
		args = append(args, state)
	}
	q += fmt.Sprintf(` ORDER BY id LIMIT %d`, limit)
	rows, err := d.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*CoverageGap{}
	for rows.Next() {
		g := &CoverageGap{}
		var targetedBy sql.NullInt64
		if err := rows.Scan(&g.ID, &g.TaskID, &g.AssetID, &g.Reason, &g.State, &targetedBy, &g.CreatedAt, &g.UpdatedAt); err != nil {
			return nil, err
		}
		if targetedBy.Valid {
			v := targetedBy.Int64
			g.TargetedBy = &v
		}
		out = append(out, g)
	}
	return out, nil
}

package db

// 方法论配置持久层（0.14.7，批次六 F1）。
//
// 纪律：内置方法论【本体】与判据在代码常量（internal/edge 注册表，编译期钉死）；
// 本层只持久化两类数据——① 内置条目的启停/排序覆盖；② 用户自定义方法论全量
// （读取时由 server 层过 internal/edge 同一套注册期校验，校验规则是代码常量）。

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

//go:embed schema_edge2.sql
var edge2SchemaSQL string

var edge2SchemaReady sync.Map

// EnsureEdge2Schema 应用方法论配置表的幂等 DDL。
func (d *DB) EnsureEdge2Schema() error {
	if _, ok := edge2SchemaReady.Load(d.DB); ok {
		return nil
	}
	if _, err := d.Exec(edge2SchemaSQL); err != nil {
		return fmt.Errorf("应用方法论配置 schema 失败: %w", err)
	}
	edge2SchemaReady.Store(d.DB, struct{}{})
	return nil
}

// EdgeMethodologyRow 是配置表的一行。
type EdgeMethodologyRow struct {
	ID            string          `json:"id"`
	Source        string          `json:"source"`
	Name          string          `json:"name"`
	Tier          int             `json:"tier"`
	EvidenceGrade string          `json:"evidence_grade"`
	Summary       string          `json:"summary"`
	Criterion     json.RawMessage `json:"criterion"`
	HeuristicTags []string        `json:"heuristic_tags"`
	Enabled       bool            `json:"enabled"`
	Priority      int             `json:"priority"`
	CreatedAt     time.Time       `json:"created_at"`
	UpdatedAt     time.Time       `json:"updated_at"`
}

// UpsertEdgeMethodology 写一条配置（custom 全量或 builtin-override 的启停/排序）。
func (d *DB) UpsertEdgeMethodology(r EdgeMethodologyRow) error {
	if err := d.EnsureEdge2Schema(); err != nil {
		return err
	}
	if r.ID == "" {
		return fmt.Errorf("id 必填")
	}
	tags, _ := json.Marshal(r.HeuristicTags)
	_, err := d.Exec(`
INSERT INTO edge_methodologies (id, source, name, tier, evidence_grade, summary, criterion, heuristic_tags, enabled, priority)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
ON CONFLICT (id) DO UPDATE SET
  source=EXCLUDED.source, name=EXCLUDED.name, tier=EXCLUDED.tier,
  evidence_grade=EXCLUDED.evidence_grade, summary=EXCLUDED.summary,
  criterion=EXCLUDED.criterion, heuristic_tags=EXCLUDED.heuristic_tags,
  enabled=EXCLUDED.enabled, priority=EXCLUDED.priority, updated_at=now()`,
		r.ID, r.Source, r.Name, r.Tier, r.EvidenceGrade, r.Summary, r.Criterion, tags, r.Enabled, r.Priority)
	return err
}

// DeleteEdgeMethodology 删除一条自定义方法论（内置覆盖行用恢复默认替代）。
func (d *DB) DeleteEdgeMethodology(id string) error {
	if err := d.EnsureEdge2Schema(); err != nil {
		return err
	}
	res, err := d.Exec(`DELETE FROM edge_methodologies WHERE id=$1 AND source='custom'`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("自定义方法论 %q 不存在（内置覆盖行请用恢复默认）", id)
	}
	return nil
}

// DeleteEdgeMethodologyBuiltinOverride 删除内置条目的覆盖行（恢复默认的
// 启停/排序——内置本体在代码常量）。
func (d *DB) DeleteEdgeMethodologyBuiltinOverride(id string) error {
	if err := d.EnsureEdge2Schema(); err != nil {
		return err
	}
	res, err := d.Exec(`DELETE FROM edge_methodologies WHERE id=$1 AND source='builtin-override'`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("内置方法论 %q 无覆盖行（已是默认值）", id)
	}
	return nil
}

// ListEdgeMethodologies 列配置表全部行。
func (d *DB) ListEdgeMethodologies() ([]EdgeMethodologyRow, error) {
	if err := d.EnsureEdge2Schema(); err != nil {
		return nil, err
	}
	rows, err := d.Query(`
SELECT id, source, name, tier, evidence_grade, summary, criterion, heuristic_tags, enabled, priority, created_at, updated_at
FROM edge_methodologies ORDER BY priority DESC, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []EdgeMethodologyRow{}
	for rows.Next() {
		r := &EdgeMethodologyRow{}
		var tags json.RawMessage
		if err := rows.Scan(&r.ID, &r.Source, &r.Name, &r.Tier, &r.EvidenceGrade, &r.Summary,
			&r.Criterion, &tags, &r.Enabled, &r.Priority, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, err
		}
		// jsonb 列（pgx 返回 []uint8）不能直接 Scan 进 []string——RawMessage
		// 中转再 Unmarshal（空数组/NULL 都落 []）。
		r.HeuristicTags = []string{}
		if len(tags) > 0 {
			_ = json.Unmarshal(tags, &r.HeuristicTags)
		}
		out = append(out, *r)
	}
	return out, nil
}

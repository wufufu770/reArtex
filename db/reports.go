package db

import (
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

//go:embed schema_reports.sql
var reportsSchemaSQL string

// reportsSchemaReady 记录某个连接已经应用过报告中心 DDL。
var reportsSchemaReady sync.Map

// EnsureReportsSchema 应用报告中心 DDL（幂等）。
func (d *DB) EnsureReportsSchema() error {
	if _, ok := reportsSchemaReady.Load(d.DB); ok {
		return nil
	}
	if _, err := d.Exec(reportsSchemaSQL); err != nil {
		return fmt.Errorf("应用报告中心 schema 失败: %w", err)
	}
	reportsSchemaReady.Store(d.DB, struct{}{})
	return nil
}

func (d *DB) ensureReportsSchema() error { return d.EnsureReportsSchema() }

// Report 是报告中心一行：最终报告的正文整篇存库（见 schema_reports.sql 头注释）。
type Report struct {
	ID         int64     `json:"id"`
	Title      string    `json:"title"`
	TaskID     *int64    `json:"task_id,omitempty"`
	SourcePath string    `json:"source_path,omitempty"`
	Content    string    `json:"content"`
	CreatedBy  string    `json:"created_by"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// ReportMeta 是列表行（不带正文，列表不拖整篇内容）。
type ReportMeta struct {
	ID         int64     `json:"id"`
	Title      string    `json:"title"`
	TaskID     *int64    `json:"task_id,omitempty"`
	SourcePath string    `json:"source_path,omitempty"`
	Size       int       `json:"size"`
	CreatedBy  string    `json:"created_by"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// CreateReport 新建一份报告；content 为空时给标题做占位正文。
func (d *DB) CreateReport(title, content, createdBy string, taskID *int64, sourcePath string) (int64, error) {
	if err := d.ensureReportsSchema(); err != nil {
		return 0, err
	}
	title = strings.TrimSpace(title)
	if title == "" {
		return 0, fmt.Errorf("报告标题不能为空")
	}
	if strings.TrimSpace(content) == "" {
		content = "# " + title + "\n"
	}
	var id int64
	err := d.QueryRow(`
INSERT INTO reports (title, task_id, source_path, content, created_by)
VALUES ($1, $2, NULLIF($3,''), $4, $5) RETURNING id`,
		title, taskID, sourcePath, content, createdBy).Scan(&id)
	return id, err
}

// ListReports 列报告中心（元信息，按更新时间倒序）。
func (d *DB) ListReports(limit int) ([]ReportMeta, error) {
	if err := d.ensureReportsSchema(); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	rows, err := d.Query(`
SELECT id, title, task_id, COALESCE(source_path,''), LENGTH(content), created_by, created_at, updated_at
FROM reports ORDER BY updated_at DESC, id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ReportMeta{}
	for rows.Next() {
		var m ReportMeta
		if err := rows.Scan(&m.ID, &m.Title, &m.TaskID, &m.SourcePath, &m.Size, &m.CreatedBy, &m.CreatedAt, &m.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// GetReport 读整篇（编辑器/下载用）。不存在 ⇒ (nil, nil)。
func (d *DB) GetReport(id int64) (*Report, error) {
	if err := d.ensureReportsSchema(); err != nil {
		return nil, err
	}
	var r Report
	err := d.QueryRow(`
SELECT id, title, task_id, COALESCE(source_path,''), content, created_by, created_at, updated_at
FROM reports WHERE id=$1`, id).
		Scan(&r.ID, &r.Title, &r.TaskID, &r.SourcePath, &r.Content, &r.CreatedBy, &r.CreatedAt, &r.UpdatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &r, nil
}

// UpdateReport 改标题/正文（可空 = 不改）；updated_at 刷新。
func (d *DB) UpdateReport(id int64, title, content *string) (int64, error) {
	if err := d.ensureReportsSchema(); err != nil {
		return 0, err
	}
	if title != nil {
		*title = strings.TrimSpace(*title)
		if *title == "" {
			return 0, fmt.Errorf("报告标题不能为空")
		}
	}
	var n int64
	err := d.QueryRow(`
UPDATE reports SET
  title = COALESCE(NULLIF($2,''), title),
  content = COALESCE($3, content),
  updated_at = now()
WHERE id=$1 RETURNING id`,
		id, derefOrEmpty(title), content).Scan(&n)
	return n, err
}

// DeleteReport 删除一份报告；返回是否真的删了。
func (d *DB) DeleteReport(id int64) (bool, error) {
	if err := d.ensureReportsSchema(); err != nil {
		return false, err
	}
	tag, err := d.Exec(`DELETE FROM reports WHERE id=$1`, id)
	if err != nil {
		return false, err
	}
	n, _ := tag.RowsAffected()
	return n > 0, nil
}

func derefOrEmpty(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

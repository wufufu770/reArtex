package db

import (
	"database/sql"
	_ "embed"
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

// 偏锋探索（方法论驱动探索线）访问层：edge_cells / edge_runs。
//
// 分工与纪律：
//   - DDL 走独立幂等文件 db/schema_edge.sql（db/schema.sql 是 RESERVED），由
//     EnsureEdgeSchema 在启动期应用——与 EnsureLoopSchema 同款模式。
//   - 格点/运行是循环的【观测记账面】：判据进度必须落在格点行上（可审计性的
//     前提——结束后要能回答「哪些控制点乘哪些 UCA 型已被覆盖、哪些还没有」）；
//     只在运行表留一个最终布尔值则事后无法复算。
//   - stop_reason 取值固定七类（StopReason* 常量）：主路径（判据成立）与兜底
//     （三熔断）、边界（operator_guard）、异常（cancelled/failed）必须在统计上
//     可区分——判据触发率（doctrine_complete 占比）是自主终止是否真实发生的
//     唯一直接度量。
//   - edge_cells.attempts 是【格点尝试数】，与 exploration_nodes.attempts
//     （意图认领计数）语义无关——两处 DDL 注释都写死了这条边界。

//go:embed schema_edge.sql
var edgeSchemaSQL string

// edgeSchemaReady 记录某个连接已经应用过偏锋 DDL（与 loopTrackSchemaReady 同款）。
var edgeSchemaReady sync.Map

// EnsureEdgeSchema 应用偏锋探索的幂等 DDL。
func (d *DB) EnsureEdgeSchema() error {
	if _, ok := edgeSchemaReady.Load(d.DB); ok {
		return nil
	}
	if _, err := d.Exec(edgeSchemaSQL); err != nil {
		return fmt.Errorf("应用偏锋探索 schema 失败: %w", err)
	}
	edgeSchemaReady.Store(d.DB, struct{}{})
	return nil
}

// ensureEdgeSchema 是访问层的幂等入口。
func (d *DB) ensureEdgeSchema() error { return d.EnsureEdgeSchema() }

// ---------- stop_reason 七值（统计口径上必须可区分）----------

const (
	// StopReasonDoctrine 主路径：方法论判据成立（四门收尾的第四门过了）。
	StopReasonDoctrine = "doctrine_complete"
	// StopReasonDegenerate 兜底①：连续 N 条假设未通过溯因门禁。
	StopReasonDegenerate = "degenerate_hypothesis"
	// StopReasonStalled 兜底②：判据状态连续 M 轮无推进（规范化哈希不变）。
	StopReasonStalled = "criterion_stalled"
	// StopReasonCellExhausted 兜底③：格点封顶（attempts 达上限强制封格）。
	StopReasonCellExhausted = "cell_exhausted"
	// StopReasonOperatorGuard 边界层：人设最坏情况兜底（墙钟/轮数上限）。
	StopReasonOperatorGuard = "operator_guard"
	// StopReasonCancelled 运行异常：人工取消。
	StopReasonCancelled = "cancelled"
	// StopReasonFailed 运行异常：出错终止。
	StopReasonFailed = "failed"
)

// edge_cells.state 白名单常量。
const (
	EdgeCellOpen      = "open"
	EdgeCellDispatched = "dispatched"
	EdgeCellConfirmed  = "confirmed"
	EdgeCellDisproven  = "disproven"
	EdgeCellFolded     = "folded"
)

// EdgeCellMaxAttempts 是格点封顶阈值（熔断③）：同一格点下发满 N 次仍未确证
// ⇒ 强制封为 disproven 并退出候选集。代码常量不进 settings（与 guard 短路表
// 同理：熔断参数是安全属性，不是调优项）。
const EdgeCellMaxAttempts = 2

// EdgeCell 是 edge_cells 的一行。
type EdgeCell struct {
	ID             int64           `json:"id"`
	TaskID         int64           `json:"task_id"`
	CellID         string          `json:"cell_id"`
	MethodologyID  string          `json:"methodology_id"`
	State          string          `json:"state"`
	Attempts       int             `json:"attempts"`
	CriterionState json.RawMessage `json:"criterion_state"`
	CreatedAt      time.Time       `json:"created_at"`
	UpdatedAt      time.Time       `json:"updated_at"`
}

// EdgeRun 是 edge_runs 的一行。
type EdgeRun struct {
	ID              int64           `json:"id"`
	TaskID          int64           `json:"task_id"`
	MethodologyID   string          `json:"methodology_id"`
	CriterionTrace  json.RawMessage `json:"criterion_trace"`
	StopReason      string          `json:"stop_reason"`
	StopDetail      json.RawMessage `json:"stop_detail"`
	CreatedAt       time.Time       `json:"created_at"`
	UpdatedAt       time.Time       `json:"updated_at"`
}

// StartEdgeRun 开一次偏锋运行（edge_run 工具建任务后调用；edge_activate 在
// 任务内模式时也可开运行行以记录判据轨迹）。
func (d *DB) StartEdgeRun(taskID int64, methodologyID string) (int64, error) {
	if err := d.ensureEdgeSchema(); err != nil {
		return 0, err
	}
	var id int64
	err := d.QueryRow(`
INSERT INTO edge_runs(task_id, methodology_id) VALUES ($1,$2) RETURNING id`,
		taskID, methodologyID).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("start edge run: %w", err)
	}
	return id, nil
}

// UpsertEdgeCell 幂等登记一个格点（初始化坐标系时批量调用；重复调用是 no-op）。
func (d *DB) UpsertEdgeCell(taskID int64, cellID, methodologyID string, criterionState map[string]any) error {
	if err := d.ensureEdgeSchema(); err != nil {
		return err
	}
	cs, err := json.Marshal(criterionState)
	if err != nil {
		return err
	}
	_, err = d.Exec(`
INSERT INTO edge_cells(task_id, cell_id, methodology_id, criterion_state)
VALUES ($1,$2,$3,$4)
ON CONFLICT (task_id, cell_id) DO NOTHING`, taskID, cellID, methodologyID, cs)
	return err
}

// RecordEdgeDispatch 记一次格点下发：attempts +1、state → dispatched。
// 返回更新后的 attempts；达到 EdgeCellMaxAttempts 时调用方应触发封格熔断。
func (d *DB) RecordEdgeDispatch(taskID int64, cellID string) (int, error) {
	if err := d.ensureEdgeSchema(); err != nil {
		return 0, err
	}
	var attempts int
	err := d.QueryRow(`
UPDATE edge_cells SET attempts = attempts + 1, state = $3
WHERE task_id=$1 AND cell_id=$2
RETURNING attempts`, taskID, cellID, EdgeCellDispatched).Scan(&attempts)
	if err != nil {
		return 0, fmt.Errorf("record edge dispatch: %w", err)
	}
	return attempts, nil
}

// FoldEdgeCell 封格（熔断③或判据否定）：state → folded/disproven。
func (d *DB) FoldEdgeCell(taskID int64, cellID, state string) error {
	if err := d.ensureEdgeSchema(); err != nil {
		return err
	}
	if state != EdgeCellFolded && state != EdgeCellDisproven && state != EdgeCellConfirmed {
		return fmt.Errorf("非法格点终态 %q", state)
	}
	_, err := d.Exec(`UPDATE edge_cells SET state=$3 WHERE task_id=$1 AND cell_id=$2`, taskID, cellID, state)
	return err
}

// UpdateEdgeCellCriterion 更新格点判据进度（edge_exec 每轮对账后写）。
func (d *DB) UpdateEdgeCellCriterion(taskID int64, cellID string, criterionState map[string]any) error {
	if err := d.ensureEdgeSchema(); err != nil {
		return err
	}
	cs, err := json.Marshal(criterionState)
	if err != nil {
		return err
	}
	_, err = d.Exec(`UPDATE edge_cells SET criterion_state=$3 WHERE task_id=$1 AND cell_id=$2`,
		taskID, cellID, cs)
	return err
}

// ListEdgeCells 列任务的格点（判据进度查询面）。
func (d *DB) ListEdgeCells(taskID int64) ([]*EdgeCell, error) {
	if err := d.ensureEdgeSchema(); err != nil {
		return nil, err
	}
	rows, err := d.Query(`
SELECT id, task_id, cell_id, methodology_id, state, attempts, criterion_state, created_at, updated_at
FROM edge_cells WHERE task_id=$1 ORDER BY cell_id`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*EdgeCell
	for rows.Next() {
		c := &EdgeCell{}
		if err := rows.Scan(&c.ID, &c.TaskID, &c.CellID, &c.MethodologyID, &c.State,
			&c.Attempts, &c.CriterionState, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	if out == nil {
		out = []*EdgeCell{}
	}
	return out, nil
}

// OpenEdgeRun 取任务上未收尾的运行（stop_reason 为空）；没有返回 (nil, nil)。
func (d *DB) OpenEdgeRun(taskID int64) (*EdgeRun, error) {
	if err := d.ensureEdgeSchema(); err != nil {
		return nil, err
	}
	run := &EdgeRun{}
	err := d.QueryRow(`
SELECT id, task_id, methodology_id, criterion_trace, stop_reason, stop_detail, created_at, updated_at
FROM edge_runs WHERE task_id=$1 AND stop_reason='' ORDER BY id DESC LIMIT 1`, taskID).
		Scan(&run.ID, &run.TaskID, &run.MethodologyID, &run.CriterionTrace, &run.StopReason,
			&run.StopDetail, &run.CreatedAt, &run.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return run, nil
}

// AppendEdgeCriterionTrace 往运行轨迹追加一条判据对账记录（edge_exec 每轮一条）。
func (d *DB) AppendEdgeCriterionTrace(runID int64, entry map[string]any) error {
	if err := d.ensureEdgeSchema(); err != nil {
		return err
	}
	b, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	_, err = d.Exec(`
UPDATE edge_runs SET criterion_trace = criterion_trace || $2::jsonb WHERE id=$1`, runID, b)
	return err
}

// ConcludeEdgeRun 收尾一次运行：写 stop_reason 与 detail（唯一写入口——
// edge_conclude 工具与熔断路径都走这里；调用方负责审计与人工确认分支）。
func (d *DB) ConcludeEdgeRun(runID int64, stopReason string, stopDetail map[string]any) error {
	if err := d.ensureEdgeSchema(); err != nil {
		return err
	}
	switch stopReason {
	case StopReasonDoctrine, StopReasonDegenerate, StopReasonStalled,
		StopReasonCellExhausted, StopReasonOperatorGuard, StopReasonCancelled, StopReasonFailed:
	default:
		return fmt.Errorf("非法 stop_reason %q（必须是七类之一）", stopReason)
	}
	b, err := json.Marshal(stopDetail)
	if err != nil {
		return err
	}
	_, err = d.Exec(`UPDATE edge_runs SET stop_reason=$2, stop_detail=$3 WHERE id=$1`,
		runID, stopReason, b)
	return err
}

// ---------- tasks 表的 edge 字段（methodology_id / edge_mode）----------

// SetTaskEdgeMode 写任务的偏锋模式字段（edge_run 建任务时 / edge_activate 热切时）。
// methodologyID 为空串 = 清除方法论（关模式时传空 + off）。
func (d *DB) SetTaskEdgeMode(taskID int64, on bool, methodologyID string) error {
	_, err := d.Exec(`UPDATE tasks SET edge_mode=$2, methodology_id=$3 WHERE id=$1`,
		taskID, on, methodologyID)
	return err
}

// TaskEdgeFields 读任务的偏锋模式字段；任务不存在返回 (false, "", false)。
func (d *DB) TaskEdgeFields(taskID int64) (edgeMode bool, methodologyID string, ok bool) {
	err := d.QueryRow(`SELECT edge_mode, methodology_id FROM tasks WHERE id=$1`, taskID).
		Scan(&edgeMode, &methodologyID)
	if err != nil {
		return false, "", false
	}
	return edgeMode, methodologyID, true
}

// ListOpenEdgeRuns 列全部未收尾的偏锋运行（fireEdgeWake 的候选流；与 fireGoals
// 同款：调度器按 scheduler_state 水位去重，这里只给「还没停」的行）。
func (d *DB) ListOpenEdgeRuns() ([]*EdgeRun, error) {
	if err := d.ensureEdgeSchema(); err != nil {
		return nil, err
	}
	rows, err := d.Query(`
SELECT id, task_id, methodology_id, criterion_trace, stop_reason, stop_detail, created_at, updated_at
FROM edge_runs WHERE stop_reason='' ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*EdgeRun
	for rows.Next() {
		r := &EdgeRun{}
		if err := rows.Scan(&r.ID, &r.TaskID, &r.MethodologyID, &r.CriterionTrace, &r.StopReason,
			&r.StopDetail, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

// ---------- 偏锋总览（前端总览页数据源）----------

// EdgeTaskOverview 是「偏锋总览」一行：一个 edge_mode 任务 + 它的判据进度。
type EdgeTaskOverview struct {
	TaskID      int64          `json:"task_id"`
	TaskDesc    string         `json:"task_desc"`
	TaskGoal    string         `json:"task_goal"`
	Status      string         `json:"status"`
	Methodology string         `json:"methodology_id"`
	CellsTotal  int            `json:"cells_total"`
	CellsDone   int            `json:"cells_terminal"`
	Progress    float64        `json:"criterion_progress"`
	OpenRun     *EdgeRun       `json:"open_run"`
	StopStats   map[string]int `json:"stop_stats"`
}

// ListEdgeTasksOverview 列全部 edge_mode 的未删任务及其判据进度
//（偏锋总览页的数据源；零 LLM，纯机械汇总）。判据进度 = 终态格点 / 总格点。
func (d *DB) ListEdgeTasksOverview() ([]*EdgeTaskOverview, error) {
	rows, err := d.Query(`
SELECT t.id, t.description, t.goal, t.status, COALESCE(t.methodology_id,'')
FROM tasks t
WHERE t.edge_mode=true AND t.deleted_at IS NULL
ORDER BY t.id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*EdgeTaskOverview
	for rows.Next() {
		o := &EdgeTaskOverview{StopStats: map[string]int{}}
		if err := rows.Scan(&o.TaskID, &o.TaskDesc, &o.TaskGoal, &o.Status, &o.Methodology); err != nil {
			return nil, err
		}
		cells, err := d.ListEdgeCells(o.TaskID)
		if err != nil {
			return nil, err
		}
		o.CellsTotal = len(cells)
		for _, c := range cells {
			if c.State == EdgeCellConfirmed || c.State == EdgeCellDisproven || c.State == EdgeCellFolded {
				o.CellsDone++
			}
		}
		if o.CellsTotal > 0 {
			o.Progress = float64(o.CellsDone) / float64(o.CellsTotal)
		}
		if o.OpenRun, err = d.OpenEdgeRun(o.TaskID); err != nil {
			return nil, err
		}
		stopRows, err := d.Query(`SELECT stop_reason, COUNT(*) FROM edge_runs WHERE task_id=$1 GROUP BY stop_reason`, o.TaskID)
		if err != nil {
			return nil, err
		}
		for stopRows.Next() {
			var reason string
			var n int
			if err := stopRows.Scan(&reason, &n); err == nil {
				o.StopStats[reason] = n
			}
		}
		stopRows.Close()
		out = append(out, o)
	}
	return out, nil
}

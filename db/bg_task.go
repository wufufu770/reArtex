package db

// 后台命令的账本（bg_task）—— 方案书 §11.7 P0-④b 的「真相来源」。
//
// 为什么要有一张专门的表（而不是复用 activity 的 kind 列）：
// 见 db/schema_bg_task.sql 顶部的说明。一句话：activity 是事件流，后台命令的
// 生命周期是可变状态，而 lost 与 failed 的区别正是本设计要保住的东西。
//
// 这张表同时承担两个角色：
//   - **任务表的一行**：每个后台命令占一行，状态 running/done/failed/lost；
//   - **跨 session 注册表**：owner_task_id + owner_key 让后来的 run 能找到
//     先前 run 起的后台命令（输出路径就在这一行里）。
//
// 为什么 db/schema.sql 没动（它被别的单元并行开发、属主保留）：走独立的幂等
// schema 文件 + EnsureBgTaskSchema，和 db/sod.go 的做法完全一致。

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"
	"sync"
	"time"
)

//go:embed schema_bg_task.sql
var bgTaskSchemaSQL string

// bgTaskSchemaReady 记「这个连接已经建过表」，键是底层 *sql.DB 指针
// （与 db/sod.go 的 sodSchemaReady 同范式：每个测试实例自带一条连接）。
var bgTaskSchemaReady sync.Map

// BgTaskState 是后台命令在账本里的状态。
//
// lost 与 failed 必须分开：failed 是"这条命令自己挂了"，lost 是"它被回收了
// —— 没人再要它的输出了"。agent 读到这两个字时的下一步动作完全不同。
const (
	BgTaskStateRunning = "running"
	BgTaskStateDone    = "done"
	BgTaskStateFailed  = "failed"
	BgTaskStateLost    = "lost"
)

// BgTask 是账本里的一行：一条后台命令。
type BgTask struct {
	ID          int64
	OwnerTaskID int64
	OwnerKey    string
	SessionID   string
	SdkTaskID   string
	Command     string
	OutputPath  string
	PID         int
	State       string
	ExitCode    *int
	Notice      string
	// DeadlineUnix 是拥有者任务的绝对 deadline（unix 秒）；0 = 没有 deadline。
	DeadlineUnix int64
	// TTLSeconds 是这条命令自己的存活上限（秒）；0 = 不设 TTL。
	TTLSeconds int
	StartedAt  time.Time
	EndedAt    *time.Time
	NotifiedAt *time.Time
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// EnsureBgTaskSchema 建出 bg_task 表（幂等）。
//
// 为什么单独一个 Ensure 而不是并进 db.Open：db/db.go 由别的单元拥有（并行开发），
// 本单元不改它；于是本单元在第一次用到这张表时自己补齐 DDL。语义与 db/sod.go 的
// EnsureSoDSchema 完全一致 —— 进程内按连接记一次，SQL 本身幂等（IF NOT EXISTS），
// 失败不写进 memo（下次会重试）。
func (d *DB) EnsureBgTaskSchema() error {
	if _, ok := bgTaskSchemaReady.Load(d.DB); ok {
		return nil
	}
	if _, err := d.Exec(bgTaskSchemaSQL); err != nil {
		return fmt.Errorf("应用后台任务 schema 失败: %w", err)
	}
	bgTaskSchemaReady.Store(d.DB, struct{}{})
	return nil
}

// ensureBgTaskSchema 是访问层的幂等入口。
func (d *DB) ensureBgTaskSchema() error { return d.EnsureBgTaskSchema() }

const bgTaskCols = `id, owner_task_id, owner_key, session_id, sdk_task_id, command,
	output_path, pid, state, exit_code, notice, deadline_unix, ttl_seconds,
	started_at, ended_at, notified_at, created_at, updated_at`

// scanBgTask 读一行账本。
func scanBgTask(row interface{ Scan(...any) error }) (*BgTask, error) {
	var t BgTask
	var exitCode sql.NullInt64
	var endedAt, notifiedAt sql.NullTime
	err := row.Scan(&t.ID, &t.OwnerTaskID, &t.OwnerKey, &t.SessionID, &t.SdkTaskID,
		&t.Command, &t.OutputPath, &t.PID, &t.State, &exitCode, &t.Notice,
		&t.DeadlineUnix, &t.TTLSeconds, &t.StartedAt, &endedAt, &notifiedAt,
		&t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		return nil, err
	}
	if exitCode.Valid {
		code := int(exitCode.Int64)
		t.ExitCode = &code
	}
	if endedAt.Valid {
		v := endedAt.Time
		t.EndedAt = &v
	}
	if notifiedAt.Valid {
		v := notifiedAt.Time
		t.NotifiedAt = &v
	}
	return &t, nil
}

// InsertBgTask 登记一条后台命令（state=running）。
//
// 幂等：同一 (session_id, sdk_task_id) 重复登记时更新原行而不是插第二行 ——
// 登记发生在 SDK 把任务标成后台的那一刻，重复登记只可能是重试。
func (d *DB) InsertBgTask(ctx context.Context, t BgTask) (int64, error) {
	if err := d.ensureBgTaskSchema(); err != nil {
		return 0, err
	}
	if t.State == "" {
		t.State = BgTaskStateRunning
	}
	var id int64
	err := d.QueryRowContext(ctx, `
INSERT INTO bg_task(owner_task_id, owner_key, session_id, sdk_task_id, command,
	output_path, pid, state, notice, deadline_unix, ttl_seconds, started_at, updated_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,COALESCE($12, now()), now())
ON CONFLICT (session_id, sdk_task_id) DO UPDATE
   SET owner_task_id = EXCLUDED.owner_task_id,
       owner_key = EXCLUDED.owner_key,
       command = EXCLUDED.command,
       output_path = EXCLUDED.output_path,
       pid = EXCLUDED.pid,
       state = EXCLUDED.state,
       notice = EXCLUDED.notice,
       deadline_unix = EXCLUDED.deadline_unix,
       ttl_seconds = EXCLUDED.ttl_seconds,
       updated_at = now()
RETURNING id`,
		t.OwnerTaskID, t.OwnerKey, t.SessionID, t.SdkTaskID, t.Command,
		t.OutputPath, t.PID, t.State, t.Notice, t.DeadlineUnix, t.TTLSeconds,
		nullableTime(t.StartedAt)).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("登记后台任务失败: %w", err)
	}
	return id, nil
}

// UpdateBgTaskState 把一行账本收尾成终态（done / failed / lost）。
//
// notified 决定要不要同时盖上提醒投递时间（完成提醒投出去了没有）。
func (d *DB) UpdateBgTaskState(ctx context.Context, id int64, state string, exitCode *int, notice string, endedAt time.Time, notified bool) error {
	if err := d.ensureBgTaskSchema(); err != nil {
		return err
	}
	var code any
	if exitCode != nil {
		code = *exitCode
	}
	var ended any
	if !endedAt.IsZero() {
		ended = endedAt
	}
	_, err := d.ExecContext(ctx, `
UPDATE bg_task
   SET state = $2,
       exit_code = $3,
       notice = $4,
       ended_at = COALESCE($5, ended_at, now()),
       notified_at = CASE WHEN $6 THEN now() ELSE notified_at END,
       updated_at = now()
 WHERE id = $1`,
		id, state, code, notice, ended, notified)
	if err != nil {
		return fmt.Errorf("更新后台任务状态失败: %w", err)
	}
	return nil
}

// GetBgTask 按 id 读一行账本。
func (d *DB) GetBgTask(ctx context.Context, id int64) (*BgTask, error) {
	if err := d.ensureBgTaskSchema(); err != nil {
		return nil, err
	}
	row := d.QueryRowContext(ctx, `SELECT `+bgTaskCols+` FROM bg_task WHERE id = $1`, id)
	t, err := scanBgTask(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("查询后台任务失败: %w", err)
	}
	return t, nil
}

// GetBgTaskBySessionTask 按 (session_id, sdk_task_id) 读一行账本。
//
// 这是 SDK 回调 OnFinished 时唯一的定位手段：SDK 只知道 session id 和它自己发的
// task id。
func (d *DB) GetBgTaskBySessionTask(ctx context.Context, sessionID, sdkTaskID string) (*BgTask, error) {
	if err := d.ensureBgTaskSchema(); err != nil {
		return nil, err
	}
	row := d.QueryRowContext(ctx, `SELECT `+bgTaskCols+` FROM bg_task WHERE session_id = $1 AND sdk_task_id = $2`,
		sessionID, sdkTaskID)
	t, err := scanBgTask(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("查询后台任务失败: %w", err)
	}
	return t, nil
}

// ListBgTasksByOwner 列出某个 ARTEX 任务名下的全部后台命令（跨 session）。
//
// 这就是跨 session 注册表的读面：后来的 run 拿 task id 就能问出"我（或我之前的
// run）起过哪些后台命令、它们的输出在哪儿、还活着没有"。
func (d *DB) ListBgTasksByOwner(ctx context.Context, ownerTaskID int64) ([]BgTask, error) {
	if err := d.ensureBgTaskSchema(); err != nil {
		return nil, err
	}
	rows, err := d.QueryContext(ctx, `SELECT `+bgTaskCols+` FROM bg_task WHERE owner_task_id = $1 ORDER BY started_at DESC, id DESC`,
		ownerTaskID)
	if err != nil {
		return nil, fmt.Errorf("查询后台任务列表失败: %w", err)
	}
	return collectBgTasks(rows)
}

// ListRunningBgTasks 列出所有还在 running 的后台命令（回收者的输入集）。
func (d *DB) ListRunningBgTasks(ctx context.Context) ([]BgTask, error) {
	if err := d.ensureBgTaskSchema(); err != nil {
		return nil, err
	}
	rows, err := d.QueryContext(ctx, `SELECT `+bgTaskCols+` FROM bg_task WHERE state = 'running' ORDER BY started_at ASC, id ASC`)
	if err != nil {
		return nil, fmt.Errorf("查询在跑的后台任务失败: %w", err)
	}
	return collectBgTasks(rows)
}

// collectBgTasks 把结果集收成切片。
func collectBgTasks(rows *sql.Rows) ([]BgTask, error) {
	defer rows.Close()
	out := []BgTask{}
	for rows.Next() {
		t, err := scanBgTask(rows)
		if err != nil {
			return nil, fmt.Errorf("读取后台任务行失败: %w", err)
		}
		out = append(out, *t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("读取后台任务行失败: %w", err)
	}
	return out, nil
}

// nullableTime 把零值时间变成 SQL NULL。
func nullableTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}

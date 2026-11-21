package db

// 升级提议 + 会话单循环硬锁（0.15.44，#12 single_loop 裁决落地）。
//
// 语义（方案 A/C-T1 的本仓落法——SoD：LLM 只提议，确定性代码裁决）：
//   - conversations.single_loop = 'on' ⇒ 该会话的工具面确定性剥离 spawn_task
//     （server/assembly.go ToolResolve）——agent 物理不可建任务；
//   - 替代出口 propose_task_escalation：LLM 只能写一张「升级提议」
//     （task_escalation_proposals，pending），**不执行**；
//   - 人工在任务页批准 → 后端用提议 payload 确定性建任务（spawnTaskCore）。
//
// 既有设计不破坏：未开启 single_loop 的会话 spawn_task 照旧（recon 派任务
// 是本仓设计能力）；锁是会话粒度 opt-in。

import (
	"database/sql"
	_ "embed"
	"fmt"
	"strings"
	"sync"
	"time"
)

//go:embed schema_selfloop.sql
var selfLoopSchemaSQL string

// selfLoopSchemaReady 记录某个连接已应用过本 DDL。
var selfLoopSchemaReady sync.Map

// EnsureSelfLoopSchema 应用单循环/升级提议 DDL（幂等）。
func (d *DB) EnsureSelfLoopSchema() error {
	if _, ok := selfLoopSchemaReady.Load(d.DB); ok {
		return nil
	}
	if _, err := d.Exec(selfLoopSchemaSQL); err != nil {
		return fmt.Errorf("应用单循环 schema 失败: %w", err)
	}
	selfLoopSchemaReady.Store(d.DB, struct{}{})
	return nil
}

// ConversationSingleLoop 读会话单循环硬锁。查不到/非法一律回落 off
// （读侧不收紧不放宽——engagement 同款纪律）。
func (d *DB) ConversationSingleLoop(convID int64) bool {
	if d == nil {
		return false
	}
	_ = d.EnsureSelfLoopSchema()
	var v string
	if err := d.QueryRow(`SELECT single_loop FROM conversations WHERE id=$1`, convID).Scan(&v); err != nil {
		return false
	}
	return v == "on"
}

// SetConversationSingleLoop 置/清会话单循环硬锁。
func (d *DB) SetConversationSingleLoop(convID int64, on bool) error {
	if err := d.EnsureSelfLoopSchema(); err != nil {
		return err
	}
	v := "off"
	if on {
		v = "on"
	}
	tag, err := d.Exec(`UPDATE conversations SET single_loop=$2 WHERE id=$1`, convID, v)
	if err != nil {
		return err
	}
	if n, _ := tag.RowsAffected(); n == 0 {
		return fmt.Errorf("会话不存在")
	}
	return nil
}

// TaskEscalationProposal 是一张升级提议（LLM 只写不执行；人工批准才建任务）。
type TaskEscalationProposal struct {
	ID         int64          `json:"id"`
	SessionID  int64          `json:"session_id,omitempty"` // 发起会话（chat 提议时回填）
	AgentKey   string         `json:"agent_key"`
	TaskRef    string         `json:"task_ref,omitempty"` // 提议挂靠的任务（可选）
	Title      string         `json:"title"`
	Goal       string         `json:"goal"`
	Rationale  string         `json:"rationale,omitempty"`
	Payload    []byte         `json:"-"` // 原始 spawn 参数 JSON（批准时确定性重放）
	Status     string         `json:"status"`
	ProposedBy string         `json:"proposed_by,omitempty"`
	CreatedAt  time.Time      `json:"created_at"`
	DecidedAt  sql.NullTime   `json:"decided_at,omitempty"`
	TaskID     sql.NullString `json:"task_id,omitempty"` // 批准后落成的任务号
}

// CreateTaskEscalationProposal 写一张待审提议（payload = 原始 spawn 参数 JSON）。
func (d *DB) CreateTaskEscalationProposal(sessionID int64, agentKey, taskRef, title, goal, rationale, proposedBy string, payload []byte) (int64, error) {
	if err := d.EnsureSelfLoopSchema(); err != nil {
		return 0, err
	}
	title = strings.TrimSpace(title)
	if title == "" {
		return 0, fmt.Errorf("提议标题不能为空")
	}
	if strings.TrimSpace(goal) == "" {
		return 0, fmt.Errorf("提议目标不能为空")
	}
	var id int64
	err := d.QueryRow(`
INSERT INTO task_escalation_proposals (session_id, agent_key, task_ref, title, goal, rationale, payload, status, proposed_by)
VALUES ($1, $2, NULLIF($3,''), $4, $5, $6, $7, 'pending', $8) RETURNING id`,
		sessionID, agentKey, taskRef, title, goal, rationale, payload, proposedBy).Scan(&id)
	return id, err
}

// ListTaskEscalationProposals 列提议（status 空 = 全部；pending|approved|rejected|converted）。
func (d *DB) ListTaskEscalationProposals(status string, limit int) ([]*TaskEscalationProposal, error) {
	if err := d.EnsureSelfLoopSchema(); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	q := `SELECT id, COALESCE(session_id,0), COALESCE(agent_key,''), COALESCE(task_ref,''),
		title, goal, COALESCE(rationale,''), payload, status, COALESCE(proposed_by,''),
		created_at, decided_at, task_id
	FROM task_escalation_proposals`
	args := []any{limit}
	if s := strings.TrimSpace(status); s != "" {
		q += ` WHERE status=$2`
		args = append(args, s)
	}
	q += ` ORDER BY created_at DESC, id DESC LIMIT $1`
	rows, err := d.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*TaskEscalationProposal{}
	for rows.Next() {
		p := &TaskEscalationProposal{}
		if err := rows.Scan(&p.ID, &p.SessionID, &p.AgentKey, &p.TaskRef, &p.Title, &p.Goal,
			&p.Rationale, &p.Payload, &p.Status, &p.ProposedBy,
			&p.CreatedAt, &p.DecidedAt, &p.TaskID); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// GetTaskEscalationProposal 读一张提议（不存在 ⇒ nil, nil）。
func (d *DB) GetTaskEscalationProposal(id int64) (*TaskEscalationProposal, error) {
	if err := d.EnsureSelfLoopSchema(); err != nil {
		return nil, err
	}
	p := &TaskEscalationProposal{}
	err := d.QueryRow(`
SELECT id, COALESCE(session_id,0), COALESCE(agent_key,''), COALESCE(task_ref,''),
	title, goal, COALESCE(rationale,''), payload, status, COALESCE(proposed_by,''),
	created_at, decided_at, task_id
FROM task_escalation_proposals WHERE id=$1`, id).
		Scan(&p.ID, &p.SessionID, &p.AgentKey, &p.TaskRef, &p.Title, &p.Goal,
			&p.Rationale, &p.Payload, &p.Status, &p.ProposedBy,
			&p.CreatedAt, &p.DecidedAt, &p.TaskID)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return p, nil
}

// DecideTaskEscalationProposal 人工裁决一张提议（CAS：pending → decided 状态；
// 返回是否真的改了——并发/重复裁决不二写）。
func (d *DB) DecideTaskEscalationProposal(id int64, status, taskID string) (bool, error) {
	if err := d.EnsureSelfLoopSchema(); err != nil {
		return false, err
	}
	switch status {
	case "approved", "rejected":
	default:
		return false, fmt.Errorf("裁决状态只允许 approved|rejected")
	}
	tag, err := d.Exec(`
UPDATE task_escalation_proposals
SET status=$2, decided_at=now(), task_id=NULLIF($3,'')
WHERE id=$1 AND status='pending'`, id, status, taskID)
	if err != nil {
		return false, err
	}
	n, _ := tag.RowsAffected()
	return n > 0, nil
}

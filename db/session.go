// 会话（sessions）访问层：一个 session = 一群聊（人 + N 个 agent 同一条消息流）。
//
// 形状（session-groupchat-proposal.md §4.1 / §4.7）：conversations.agent_key 是
// **单值列** ⇒ 一个会话只能跑一个 agent ⇒ 「一个 session 就是一群聊」在数据层
// 无处安放。本文件把 agent 从「会话的属性」升格为「会话成员的属性」：
//
//	sessions            会话本体（容器；kind = chat | task_hall | side_question）
//	session_members     agent 在会话里的席位（热加入 / 退出 / presence）
//	session_attachments 会话↔工件（typed，正向加、反查）
//	session_messages    人 / agent 同一条流 + 收据条（receipt）+ 线程（parent）
//
// 三条被写死的边界：
//  1. 归属权威唯一 = exploration_nodes.owner。消息表**没有** owner/assignee 一类的
//     列，kind='request' 且带 intent_id 的消息只是【提议】。
//  2. claims_authorization 永远是不可信输入：没有任何权限门读这一列。
//  3. 收据条（kind='receipt'）必须带 receipt_of（DDL 的 CHECK 兜底）：没有它就
//     分不清「一条普通消息」和「贴在某条消息下的收据」。
//
// 命名三标签（proposal §4.2）：name = 用户显式名（唯一句柄，永不被覆盖）、
// title = 生成标题（可搜）、name_source = 当前显示名取自哪一层
// （user > generated > default）。
//
// allow: SIZE_OK —— 本单元的文件域被任务书钉死为 db/session.go 一个文件；下面
// 四组访问器共用同一条约束（「会话是容器、agent 是成员、消息带准入判定、工件
// 可反查」）。拆成四个文件会把这条约束切成四份副本，而四份副本正是漂移的开始。
package db

import (
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Autumn-27/artex/internal/sessionwake"
)

//go:embed schema_session.sql
var sessionSchemaSQL string

// sessionSchemaReady 记录某个连接已经应用过会话 DDL。
//
// 与 db/sod.go 的 sodSchemaReady 同理：用包级 sync.Map 而不是 DB 上的字段
// （db/db.go 不是本单元的文件域）。键是底层 *sql.DB 指针 —— 每个测试包/实例
// 各连各的库，互不干扰。
var sessionSchemaReady sync.Map

// ---------- 会话的状态机常量（与 db/schema_session.sql 的 CHECK 镜像）----------

// 会话类型。
// kind='master' 是总控会话：群聊全部能力（成员/唤醒/会话流）+ 绑定任务（创建即编排
// 或 spawn_task 自动绑回）+ 引擎进展播报进会话流。与 task_hall 的区别：task_hall 是
// 「发布任务时生成的任务大厅」，master 是「人主动开的指挥室」——任务可由 goal 在
// 创建时一并起，也可由成员在会话里 spawn_task 后自动绑定。
const (
	SessionKindChat         = "chat"
	SessionKindMaster       = "master"
	SessionKindTaskHall     = "task_hall"
	SessionKindSideQuestion = "side_question"
)

// 显示名来源。优先级：user > generated > default（proposal §4.2）。
const (
	SessionNameSourceUser      = "user"
	SessionNameSourceGenerated = "generated"
	SessionNameSourceDefault   = "default"
)

// 成员分工。**不表达权限**（与 chat_group_members.role 同款纪律）。
const (
	SessionRoleDispatcher = "dispatcher"
	SessionRoleMember     = "member"
	SessionRoleObserver   = "observer"
)

// 成员在场状态。proposal §4.3 的三态是 listening / working / blocked；
// 'idle'（在席但没在听）与 'left'（已退出，left_at 有值）是另外两个态。
const (
	SessionPresenceIdle      = "idle"
	SessionPresenceListening = "listening"
	SessionPresenceWorking   = "working"
	SessionPresenceBlocked   = "blocked"
	SessionPresenceLeft      = "left"
)

// 消息类型。'receipt' 是收据条（不进消息气泡）。
const (
	SessionMessageKindNote       = "note"
	SessionMessageKindRequest    = "request"
	SessionMessageKindAnswer     = "answer"
	SessionMessageKindFinding    = "finding"
	SessionMessageKindEscalation = "escalation"
	SessionMessageKindReceipt    = "receipt"
)

// 消息状态。blocked = **投递前**拦截的结果（落库留痕，但不进入接收方上下文）。
const (
	SessionMessageUnread  = "unread"
	SessionMessageRead    = "read"
	SessionMessageAcked   = "acked"
	SessionMessageBlocked = "blocked"
)

// 收据动作。
const (
	SessionReceiptTaken    = "taken"
	SessionReceiptNoted    = "noted"
	SessionReceiptDeferred = "deferred"
)

// sessionAttachmentTypes 是内置工件类型白名单（proposal §4.4）。
//
// 为什么是**代码常量**而不是 DB 规则行：工件类型决定「会话能挂什么」，是可被
// 热改的存储 ⇒ 类型白名单必须是代码里的确定性裁决（与 db/chat_group.go 的
// 禁列表纪律同源）。
var sessionAttachmentTypes = map[string]bool{
	"task": true, "task_hall": true, "strategy": true, "asset": true,
	"finding": true, "pr": true, "external": true,
}

// sessionMessageKinds / sessionReceiptActions 是消息类型与收据动作白名单。
var sessionMessageKinds = map[string]bool{
	SessionMessageKindNote: true, SessionMessageKindRequest: true,
	SessionMessageKindAnswer: true, SessionMessageKindFinding: true,
	SessionMessageKindEscalation: true, SessionMessageKindReceipt: true,
}

var sessionReceiptActions = map[string]bool{
	SessionReceiptTaken: true, SessionReceiptNoted: true, SessionReceiptDeferred: true,
}

// sessionRoles / sessionPresences 是分工与在场状态白名单。
var sessionRoles = map[string]bool{
	SessionRoleDispatcher: true, SessionRoleMember: true, SessionRoleObserver: true,
}

var sessionPresences = map[string]bool{
	SessionPresenceIdle: true, SessionPresenceListening: true,
	SessionPresenceWorking: true, SessionPresenceBlocked: true, SessionPresenceLeft: true,
}

var sessionKinds = map[string]bool{
	SessionKindChat: true, SessionKindMaster: true, SessionKindTaskHall: true, SessionKindSideQuestion: true,
}

// ---------- 结构体 ----------

// Session 是一个会话（群聊容器）。
type Session struct {
	ID         int64      `json:"id"`
	Name       string     `json:"name"`
	Title      string     `json:"title"`
	NameSource string     `json:"name_source"`
	Kind       string     `json:"kind"`
	TaskID     *int64     `json:"task_id,omitempty"`
	StrategyID *int64     `json:"strategy_id,omitempty"`
	AssetID    *int64     `json:"asset_id,omitempty"`
	PinnedAt   *time.Time `json:"pinned_at,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
}

// SessionMember 是 agent 在会话里的席位。
//
// Role 只表达分工（dispatcher/member/observer），**不表达权限**；
// Presence 表达在场状态（listening/working/blocked/left/…）。
type SessionMember struct {
	SessionID   int64      `json:"session_id"`
	AgentKey    string     `json:"agent_key"`
	Role        string     `json:"role"`
	Presence    string     `json:"presence"`
	ETAMinutes  *int       `json:"eta_minutes,omitempty"`
	BlockedOn   string     `json:"blocked_on,omitempty"`
	JoinedAt    time.Time  `json:"joined_at"`
	LeftAt      *time.Time `json:"left_at,omitempty"`
	HasLeft     bool       `json:"has_left,omitempty"`
	DisplayName string     `json:"display_name,omitempty"`
}

// SessionAttachment 是会话与一个工件的类型化关联。
type SessionAttachment struct {
	ID             int64           `json:"id"`
	SessionID      int64           `json:"session_id"`
	AttachmentType string          `json:"attachment_type"`
	IdentityKey    string          `json:"identity_key"`
	Payload        json.RawMessage `json:"payload,omitempty"`
	CreatedAt      time.Time       `json:"created_at"`
}

// SessionAttachmentSpec 是「加一个关联」的入参（幂等：同类型同 key 覆盖）。
type SessionAttachmentSpec struct {
	Type    string          `json:"type"`
	Key     string          `json:"key"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// SessionMessage 是会话消息流里的一条。
//
// ReceiptOf 非空 ⇒ 这是一条**收据条**（贴在 ReceiptOf 那条消息下面，不占气泡）。
// ParentMessageID 非空 ⇒ 这是一条**线程回复**（补充意见折叠在原消息下面）。
type SessionMessage struct {
	ID                  int64      `json:"id"`
	SessionID           int64      `json:"session_id"`
	ParentMessageID     *int64     `json:"parent_message_id,omitempty"`
	FromAgent           string     `json:"from_agent"`
	ToAgent             string     `json:"to_agent"`
	Kind                string     `json:"kind"`
	Body                string     `json:"body"`
	TaskID              *int64     `json:"task_id,omitempty"`
	IntentID            *int64     `json:"intent_id,omitempty"`
	FindingID           *int64     `json:"finding_id,omitempty"`
	Status              string     `json:"status"`
	BlockedReason       string     `json:"blocked_reason,omitempty"`
	ClaimsAuthorization bool       `json:"claims_authorization,omitempty"`
	ReceiptOf           *int64     `json:"receipt_of,omitempty"`
	ReceiptAction       string     `json:"receipt_action,omitempty"`
	CreatedAt           time.Time  `json:"created_at"`
	ReadAt              *time.Time `json:"read_at,omitempty"`
}

// SessionPatch 是会话的部分更新（只有非 nil 的字段生效）。
type SessionPatch struct {
	// Name 是用户显式名：设置后 name_source='user'，且**永不被生成标题覆盖**。
	Name *string
	// Title 是生成标题：只有 name_source 不是 'user' 时才会改变 name_source。
	Title *string
	// Pinned 是钉住开关（nil = 不改）。
	Pinned *bool
	// AddAttachments / RemoveAttachments 是关联的增删（逐条幂等）。
	AddAttachments    []SessionAttachmentSpec
	RemoveAttachments []SessionAttachmentSpec
}

// ---------- schema ----------

// EnsureSessionSchema 应用会话 DDL（幂等）**并**跑一次幂等回填。
//
// db/schema.sql 是 RESERVED 文件，本单元的 DDL 放在 db/schema_session.sql，由这里
// 在首次访问时幂等补齐（与 db/sod.go 的 EnsureSoDSchema 同款）。
//
// ⚠️ 依赖顺序：sessions 有两处跨文件外键（strategies / chat_groups），所以这里
// 先补齐它们 —— 与 db/quota.go 的 EnsureQuotaSchema → EnsureChatGroupSchema
// 同款纪律（外键指向的表必须先在）。
//
// 回填（proposal §4.7 第 2/3 条）也在这里跑，是为了让 db.Open 只需要**一行**
// Ensure*Schema() 调用就能完成「加列 + 加行」的迁移。
func (d *DB) EnsureSessionSchema() error {
	if _, ok := sessionSchemaReady.Load(d.DB); ok {
		return nil
	}
	if err := d.EnsureChatGroupSchema(); err != nil {
		return fmt.Errorf("应用群聊 schema 失败: %w", err)
	}
	if err := d.EnsureStrategySchema(); err != nil {
		return fmt.Errorf("应用策略库 schema 失败: %w", err)
	}
	if _, err := d.Exec(sessionSchemaSQL); err != nil {
		return fmt.Errorf("应用会话 schema 失败: %w", err)
	}
	if err := d.backfillSessions(); err != nil {
		return fmt.Errorf("回填会话失败: %w", err)
	}
	sessionSchemaReady.Store(d.DB, struct{}{})
	return nil
}

// ensureSessionSchema 是访问层的幂等入口：首次调用时建表，之后只是一次 map 查表。
func (d *DB) ensureSessionSchema() error { return d.EnsureSessionSchema() }


// backfillSessions 只做回填（DDL 由调用方保证已应用）。
func (d *DB) backfillSessions() error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// A1 存量迁移：系统对话（升级前没有 origin 列）按标题前缀补 origin——
	// 一次性、幂等（WHERE origin='human' 只补一次）。
	for _, m := range []struct{ prefix, origin string }{
		{"合并触发", "trigger"},
		{"工具触发", "trigger"},
		{"定时触发", "trigger"},
		{"finding 触发", "trigger"},
		{"目标触发", "trigger"},
		{"超时触发", "trigger"},
		{"任务创建触发", "trigger"},
		{"SoD 复核", "sod_review"},
	} {
		_, _ = tx.Exec(`UPDATE conversations SET origin=$2
WHERE origin='human' AND title LIKE $1||'%'`, m.prefix, m.origin)
	}

	// A1 回填收紧：只有人发起的对话才回填成正式会话。系统对话（trigger/
	// sod_review/wake/hall）留在 conversations 表里（数据无损），不再刷屏侧边栏。
	rows, err := tx.Query(`SELECT id, agent_key, title FROM conversations
WHERE session_id IS NULL AND origin='human' ORDER BY id`)
	if err != nil {
		return err
	}
	type convRow struct {
		id       int64
		agentKey string
		title    string
	}
	var convs []convRow
	for rows.Next() {
		var c convRow
		if err := rows.Scan(&c.id, &c.agentKey, &c.title); err != nil {
			rows.Close()
			return err
		}
		convs = append(convs, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	for _, c := range convs {
		var sessionID int64
		role := SessionRoleMember
		if c.agentKey == "auto" {
			role = SessionRoleDispatcher
		}
		// title 作为【生成标题】以外的默认显示名来源：回填出来的会话还没有
		// 用户显式名 ⇒ name_source='default'（§4.2 的第三档）。
		err := tx.QueryRow(`INSERT INTO sessions(kind, name, title, name_source)
VALUES ($1, '', $2, $3) RETURNING id`,
			SessionKindChat, c.title, SessionNameSourceDefault).Scan(&sessionID)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE conversations SET session_id=$2 WHERE id=$1`,
			c.id, sessionID); err != nil {
			return err
		}
		// 成员行的外键指向 agents(key)；会话的 agent_key 没有外键约束（历史数据
		// 里可能有已下线的 agent）⇒ 不存在的 agent 跳过，不让回填失败。
		var exists bool
		if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM agents WHERE key=$1)`,
			c.agentKey).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			continue
		}
		if _, err := tx.Exec(`INSERT INTO session_members(session_id, agent_key, role, presence)
VALUES ($1,$2,$3,$4) ON CONFLICT (session_id, agent_key) DO NOTHING`,
			sessionID, c.agentKey, role, SessionPresenceIdle); err != nil {
			return err
		}
	}

	groupRows, err := tx.Query(`SELECT id, name, task_id FROM chat_groups
WHERE session_id IS NULL ORDER BY id`)
	if err != nil {
		return err
	}
	type groupRow struct {
		id     int64
		name   string
		taskID *int64
	}
	var groups []groupRow
	for groupRows.Next() {
		var g groupRow
		if err := groupRows.Scan(&g.id, &g.name, &g.taskID); err != nil {
			groupRows.Close()
			return err
		}
		groups = append(groups, g)
	}
	groupRows.Close()
	if err := groupRows.Err(); err != nil {
		return err
	}

	for _, g := range groups {
		var sessionID int64
		err := tx.QueryRow(`INSERT INTO sessions(kind, name, title, name_source, task_id)
VALUES ($1, $2, $2, $3, $4) RETURNING id`,
			SessionKindTaskHall, g.name, SessionNameSourceDefault, g.taskID).Scan(&sessionID)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE chat_groups SET session_id=$2 WHERE id=$1`,
			g.id, sessionID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ensureSessionSchemaTables 保证回填依赖的表存在（回填被单独调用时的入口）。
func (d *DB) ensureSessionSchemaTables() error {
	if err := d.EnsureChatGroupSchema(); err != nil {
		return fmt.Errorf("应用群聊 schema 失败: %w", err)
	}
	if _, err := d.Exec(sessionSchemaSQL); err != nil {
		return fmt.Errorf("应用会话 schema 失败: %w", err)
	}
	return nil
}

// ---------- sessions ----------

const sessionCols = `id, name, title, name_source, kind, task_id, strategy_id, asset_id,
	pinned_at, created_at, updated_at`

type sessionScanner interface{ Scan(...any) error }

func scanSession(row sessionScanner) (*Session, error) {
	var s Session
	err := row.Scan(&s.ID, &s.Name, &s.Title, &s.NameSource, &s.Kind, &s.TaskID,
		&s.StrategyID, &s.AssetID, &s.PinnedAt, &s.CreatedAt, &s.UpdatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &s, nil
}

// CreateSession 建一个会话。
//
// Name 非空 ⇒ name_source='user'（用户显式名，唯一句柄）；否则 'default'。
// Title 只是【生成标题】：它不改变 name_source 的 user 档（§4.2：生成标题永不
// 覆盖用户显式名）。
func (d *DB) CreateSession(s *Session) (int64, error) {
	if err := d.ensureSessionSchema(); err != nil {
		return 0, err
	}
	if s == nil {
		return 0, errors.New("会话参数为空")
	}
	s.Name = strings.TrimSpace(s.Name)
	s.Title = strings.TrimSpace(s.Title)
	s.Kind = strings.TrimSpace(s.Kind)
	if s.Kind == "" {
		s.Kind = SessionKindChat
	}
	if !sessionKinds[s.Kind] {
		return 0, fmt.Errorf("会话类型只能是 chat/master/task_hall/side_question，收到 %q", s.Kind)
	}
	s.NameSource = SessionNameSourceDefault
	if s.Name != "" {
		s.NameSource = SessionNameSourceUser
	} else if s.Title != "" {
		s.NameSource = SessionNameSourceGenerated
	}
	var id int64
	err := d.QueryRow(`INSERT INTO sessions(name, title, name_source, kind, task_id, strategy_id, asset_id)
VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING id`,
		s.Name, s.Title, s.NameSource, s.Kind, s.TaskID, s.StrategyID, s.AssetID).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("建会话失败: %w", err)
	}
	s.ID = id
	return id, nil
}

// GetSession 按 id 读会话（不存在时返回 nil, nil）。
func (d *DB) GetSession(id int64) (*Session, error) {
	if err := d.ensureSessionSchema(); err != nil {
		return nil, err
	}
	return scanSession(d.QueryRow(`SELECT `+sessionCols+` FROM sessions WHERE id=$1`, id))
}

// ListSessions 列会话（扁平列表，按最近更新排序；置顶的排在前面）。
//
// 排序口径与 conversations 列表一致（钉住优先、然后 updated_at DESC），
// 与 proposal §4.6 侧边栏的「置顶 / 最近」两段对应。
func (d *DB) ListSessions(kind string) ([]Session, error) {
	if err := d.ensureSessionSchema(); err != nil {
		return nil, err
	}
	q := `SELECT ` + sessionCols + ` FROM sessions`
	args := []any{}
	if k := strings.TrimSpace(kind); k != "" {
		q += ` WHERE kind = $1`
		args = append(args, k)
	}
	q += ` ORDER BY (pinned_at IS NOT NULL) DESC, pinned_at DESC NULLS LAST, updated_at DESC, id DESC`
	rows, err := d.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Session{}
	for rows.Next() {
		s, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		if s != nil {
			out = append(out, *s)
		}
	}
	return out, rows.Err()
}

// UpdateSession 应用部分更新并返回更新后的行。
//
// 命名规则（§4.2）：用户显式名一旦设置，name_source 永远停在 'user' ⇒ 生成标题
// 只能填 title 列，不能把显示名抢回去。
func (d *DB) UpdateSession(id int64, patch SessionPatch) (*Session, error) {
	if err := d.ensureSessionSchema(); err != nil {
		return nil, err
	}
	name := ""
	if patch.Name != nil {
		name = strings.TrimSpace(*patch.Name)
	}
	title := ""
	if patch.Title != nil {
		title = strings.TrimSpace(*patch.Title)
	}
	row := d.QueryRow(`UPDATE sessions SET
  name = CASE WHEN $2::boolean THEN $3 ELSE name END,
  title = CASE WHEN $4::boolean THEN $5 ELSE title END,
  name_source = CASE
    WHEN $2::boolean AND $3 <> '' THEN 'user'
    WHEN name_source = 'user' THEN 'user'
    WHEN $4::boolean AND $5 <> '' THEN 'generated'
    ELSE name_source END,
  pinned_at = CASE
    WHEN $6::boolean IS NULL THEN pinned_at
    WHEN $6::boolean THEN COALESCE(pinned_at, now())
    ELSE NULL END
WHERE id=$1 RETURNING `+sessionCols,
		id, patch.Name != nil, name, patch.Title != nil, title, patch.Pinned)
	s, err := scanSession(row)
	if err != nil || s == nil {
		return nil, err
	}
	for _, spec := range patch.AddAttachments {
		if err := d.AddSessionAttachment(id, spec); err != nil {
			return nil, err
		}
	}
	for _, spec := range patch.RemoveAttachments {
		if err := d.RemoveSessionAttachment(id, spec.Type, spec.Key); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// DeleteSession 删一个会话（成员、关联、消息随外键级联删除）。
func (d *DB) DeleteSession(id int64) error {
	if err := d.ensureSessionSchema(); err != nil {
		return err
	}
	if _, err := d.Exec(`DELETE FROM sessions WHERE id=$1`, id); err != nil {
		return fmt.Errorf("删会话失败: %w", err)
	}
	return nil
}

// ---------- session_members ----------

const sessionMemberCols = `session_id, agent_key, role, presence, eta_minutes, blocked_on,
	joined_at, left_at`

// AddSessionMember 加一个成员（热加入）。
//
// 幂等：同 (session_id, agent_key) 覆盖分工与在场状态；重新加入会把 left_at 清空
// （退出过的 agent 再被 @ 到 ⇒ 重新入席，§4.3 的热加入路径）。
func (d *DB) AddSessionMember(sessionID int64, agentKey, role, presence string) error {
	if err := d.ensureSessionSchema(); err != nil {
		return err
	}
	agentKey = strings.TrimSpace(agentKey)
	if agentKey == "" {
		return errors.New("成员 agent key 不能为空")
	}
	if role == "" {
		role = SessionRoleMember
	}
	if !sessionRoles[role] {
		return fmt.Errorf("成员分工只能是 dispatcher/member/observer，收到 %q", role)
	}
	if presence == "" {
		presence = SessionPresenceListening
	}
	if !sessionPresences[presence] {
		return fmt.Errorf("在场状态只能是 idle/listening/working/blocked/left，收到 %q", presence)
	}
	if presence == SessionPresenceLeft {
		return errors.New("离席请用退出会话（写 left_at），不能用 presence='left' 新建席位")
	}
	_, err := d.Exec(`INSERT INTO session_members(session_id, agent_key, role, presence, joined_at, left_at)
VALUES ($1,$2,$3,$4, now(), NULL)
ON CONFLICT (session_id, agent_key) DO UPDATE
  SET role = EXCLUDED.role, presence = EXCLUDED.presence,
      left_at = NULL, joined_at = now()`, sessionID, agentKey, role, presence)
	if err != nil {
		return fmt.Errorf("加会话成员失败: %w", err)
	}
	return nil
}

// SessionMembers 列会话成员（在席优先，按加入时间）。
func (d *DB) SessionMembers(sessionID int64) ([]SessionMember, error) {
	if err := d.ensureSessionSchema(); err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT `+sessionMemberCols+` FROM session_members
WHERE session_id=$1 ORDER BY (left_at IS NULL) DESC, joined_at ASC, agent_key ASC`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SessionMember{}
	for rows.Next() {
		var m SessionMember
		// blocked_on 可空 ⇒ 用 sql.NullString 接。直接扫进 string 会在遇到
		// NULL 时报「converting NULL to string is unsupported」—— 这正是
		// 「成员默认没有阻塞项」时的常态。
		var blockedOn sql.NullString
		if err := rows.Scan(&m.SessionID, &m.AgentKey, &m.Role, &m.Presence,
			&m.ETAMinutes, &blockedOn, &m.JoinedAt, &m.LeftAt); err != nil {
			return nil, err
		}
		m.BlockedOn = blockedOn.String
		m.HasLeft = m.LeftAt != nil
		out = append(out, m)
	}
	return out, rows.Err()
}

// SessionRoster 批量读多个会话的成员名册（列表页用，避免 N+1）。
func (d *DB) SessionRoster(sessionIDs []int64) (map[int64][]SessionMember, error) {
	out := map[int64][]SessionMember{}
	if len(sessionIDs) == 0 {
		return out, nil
	}
	if err := d.ensureSessionSchema(); err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT `+sessionMemberCols+` FROM session_members
WHERE session_id = ANY($1) ORDER BY (left_at IS NULL) DESC, joined_at ASC, agent_key ASC`,
		sessionIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var m SessionMember
		// blocked_on 可空 ⇒ 用 sql.NullString 接。直接扫进 string 会在遇到
		// NULL 时报「converting NULL to string is unsupported」—— 这正是
		// 「成员默认没有阻塞项」时的常态。
		var blockedOn sql.NullString
		if err := rows.Scan(&m.SessionID, &m.AgentKey, &m.Role, &m.Presence,
			&m.ETAMinutes, &blockedOn, &m.JoinedAt, &m.LeftAt); err != nil {
			return nil, err
		}
		m.BlockedOn = blockedOn.String
		m.HasLeft = m.LeftAt != nil
		out[m.SessionID] = append(out[m.SessionID], m)
	}
	return out, rows.Err()
}

// RemoveSessionMember 退席：写 presence='left' + left_at=now()。
//
// **不改历史消息的归属**（§9 约束③）：退席 agent 之前发的消息仍然留在流里。
func (d *DB) RemoveSessionMember(sessionID int64, agentKey string) error {
	if err := d.ensureSessionSchema(); err != nil {
		return err
	}
	agentKey = strings.TrimSpace(agentKey)
	if agentKey == "" {
		return errors.New("成员 agent key 不能为空")
	}
	res, err := d.Exec(`UPDATE session_members SET presence='left', left_at=now()
WHERE session_id=$1 AND agent_key=$2 AND left_at IS NULL`, sessionID, agentKey)
	if err != nil {
		return fmt.Errorf("退席失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("成员 %q 不在会话里", agentKey)
	}
	return nil
}

// SetSessionMemberPresence 更新在场状态（working / blocked / listening …）。
//
// blocked 允许带 eta_minutes 与 blocked_on（DotCollab 式的 presence 三态）；
// left_at 语义由 RemoveSessionMember 独占，这里不接受 presence='left'。
func (d *DB) SetSessionMemberPresence(sessionID int64, agentKey, presence string, etaMinutes *int, blockedOn string) error {
	if err := d.ensureSessionSchema(); err != nil {
		return err
	}
	agentKey = strings.TrimSpace(agentKey)
	if agentKey == "" {
		return errors.New("成员 agent key 不能为空")
	}
	if !sessionPresences[presence] {
		return fmt.Errorf("在场状态只能是 idle/listening/working/blocked/left，收到 %q", presence)
	}
	if presence == SessionPresenceLeft {
		return d.RemoveSessionMember(sessionID, agentKey)
	}
	res, err := d.Exec(`UPDATE session_members
SET presence=$3, eta_minutes=$4, blocked_on=$5
WHERE session_id=$1 AND agent_key=$2`, sessionID, agentKey, presence, etaMinutes, blockedOn)
	if err != nil {
		return fmt.Errorf("更新在场状态失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("成员 %q 不在会话里", agentKey)
	}
	return nil
}

// ---------- session_attachments ----------

// AddSessionAttachment 加一条关联（幂等：同类型同 key 覆盖 payload）。
func (d *DB) AddSessionAttachment(sessionID int64, spec SessionAttachmentSpec) error {
	if err := d.ensureSessionSchema(); err != nil {
		return err
	}
	typ := strings.TrimSpace(spec.Type)
	key := strings.TrimSpace(spec.Key)
	if typ == "" || key == "" {
		return errors.New("关联类型与标识都不能为空")
	}
	if !sessionAttachmentTypes[typ] {
		return fmt.Errorf("不支持的关联类型 %q（内置类型：task/task_hall/strategy/asset/finding/pr/external）", typ)
	}
	payload := spec.Payload
	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	}
	if !json.Valid(payload) {
		return errors.New("关联载荷不是合法 JSON")
	}
	_, err := d.Exec(`INSERT INTO session_attachments(session_id, attachment_type, identity_key, payload)
VALUES ($1,$2,$3,$4)
ON CONFLICT (session_id, attachment_type, identity_key) DO UPDATE SET payload = EXCLUDED.payload`,
		sessionID, typ, key, payload)
	if err != nil {
		return fmt.Errorf("加会话关联失败: %w", err)
	}
	return nil
}

// RemoveSessionAttachment 删一条关联。
func (d *DB) RemoveSessionAttachment(sessionID int64, typ, key string) error {
	if err := d.ensureSessionSchema(); err != nil {
		return err
	}
	typ = strings.TrimSpace(typ)
	key = strings.TrimSpace(key)
	if typ == "" || key == "" {
		return errors.New("关联类型与标识都不能为空")
	}
	if _, err := d.Exec(`DELETE FROM session_attachments
WHERE session_id=$1 AND attachment_type=$2 AND identity_key=$3`, sessionID, typ, key); err != nil {
		return fmt.Errorf("删会话关联失败: %w", err)
	}
	return nil
}

// SessionAttachments 列会话的全部关联。
func (d *DB) SessionAttachments(sessionID int64) ([]SessionAttachment, error) {
	if err := d.ensureSessionSchema(); err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT id, session_id, attachment_type, identity_key, payload, created_at
FROM session_attachments WHERE session_id=$1 ORDER BY attachment_type ASC, identity_key ASC`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SessionAttachment{}
	for rows.Next() {
		var a SessionAttachment
		var payload []byte
		if err := rows.Scan(&a.ID, &a.SessionID, &a.AttachmentType, &a.IdentityKey,
			&payload, &a.CreatedAt); err != nil {
			return nil, err
		}
		a.Payload = json.RawMessage(payload)
		out = append(out, a)
	}
	return out, rows.Err()
}

// SessionAttachmentsBySession 批量读多个会话的关联（列表页用，避免 N+1）。
func (d *DB) SessionAttachmentsBySession(sessionIDs []int64) (map[int64][]SessionAttachment, error) {
	out := map[int64][]SessionAttachment{}
	if len(sessionIDs) == 0 {
		return out, nil
	}
	if err := d.ensureSessionSchema(); err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT id, session_id, attachment_type, identity_key, payload, created_at
FROM session_attachments WHERE session_id = ANY($1)
ORDER BY session_id ASC, attachment_type ASC, identity_key ASC`, sessionIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var a SessionAttachment
		var payload []byte
		if err := rows.Scan(&a.ID, &a.SessionID, &a.AttachmentType, &a.IdentityKey,
			&payload, &a.CreatedAt); err != nil {
			return nil, err
		}
		a.Payload = json.RawMessage(payload)
		out[a.SessionID] = append(out[a.SessionID], a)
	}
	return out, rows.Err()
}

// SessionsByAttachment 反查：某个工件关联了哪些会话（proposal §4.4 的反向查询）。
func (d *DB) SessionsByAttachment(typ, key string) ([]Session, error) {
	if err := d.ensureSessionSchema(); err != nil {
		return nil, err
	}
	typ = strings.TrimSpace(typ)
	key = strings.TrimSpace(key)
	if typ == "" || key == "" {
		return nil, errors.New("关联类型与标识都不能为空")
	}
	rows, err := d.Query(`SELECT `+sessionCols+` FROM sessions s
WHERE EXISTS (SELECT 1 FROM session_attachments a
              WHERE a.session_id = s.id AND a.attachment_type = $1 AND a.identity_key = $2)
ORDER BY s.updated_at DESC, s.id DESC`, typ, key)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Session{}
	for rows.Next() {
		s, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		if s != nil {
			out = append(out, *s)
		}
	}
	return out, rows.Err()
}

// ---------- session_messages ----------

const sessionMessageCols = `id, session_id, parent_message_id, from_agent, to_agent, kind, body,
	task_id, intent_id, finding_id, status, blocked_reason, claims_authorization,
	receipt_of, receipt_action, created_at, read_at`

// AddSessionMessage 往会话消息流写一条消息，并施加**投递前**的准入判定。
//
// 准入判定（被拦的消息落库标 blocked，但不进入接收方上下文 —— 与
// db/chat_group.go 的 chatGroupAdmissionReason 同款纪律）：
//   - 定向消息的收件人必须是本会话成员；
//   - 会话绑了 task 时，消息携带的 task / intent / finding 必须属于它；
//   - 线程回复的 parent 必须是本会话的消息。
//
// 收据条（kind='receipt'）是**硬约束**（不是准入判定）：必须带 receipt_of 且
// receipt_action 取值合法，receipt_of 必须指向本会话的一条消息 ⇒ 不满足直接
// 报错，不落库（DDL 的 CHECK 是最后一道兜底）。
func (d *DB) AddSessionMessage(m *SessionMessage) (int64, error) {
	if err := d.ensureSessionSchema(); err != nil {
		return 0, err
	}
	if m == nil {
		return 0, errors.New("消息参数为空")
	}
	m.Kind = strings.TrimSpace(m.Kind)
	if m.Kind == "" {
		m.Kind = SessionMessageKindNote
	}
	if !sessionMessageKinds[m.Kind] {
		return 0, fmt.Errorf("消息类型只能是 note/request/answer/finding/escalation/receipt，收到 %q", m.Kind)
	}
	m.FromAgent = strings.TrimSpace(m.FromAgent)
	m.ToAgent = strings.TrimSpace(m.ToAgent)
	m.Body = strings.TrimSpace(m.Body)
	m.ReceiptAction = strings.TrimSpace(m.ReceiptAction)
	if m.Kind == SessionMessageKindReceipt {
		if m.ReceiptOf == nil {
			return 0, errors.New("收据条必须挂在一条原消息上（receipt_of 必填）")
		}
		if !sessionReceiptActions[m.ReceiptAction] {
			return 0, fmt.Errorf("收据动作只能是 taken/noted/deferred，收到 %q", m.ReceiptAction)
		}
	} else if m.ReceiptAction != "" {
		return 0, errors.New("只有收据条（kind='receipt'）能带 receipt_action")
	}
	if m.Kind != SessionMessageKindReceipt && m.Body == "" {
		return 0, errors.New("消息正文不能为空")
	}

	sess, err := d.GetSession(m.SessionID)
	if err != nil {
		return 0, fmt.Errorf("查询会话失败: %w", err)
	}
	if sess == nil {
		return 0, errors.New("会话不存在")
	}

	reason, err := d.sessionMessageAdmissionReason(sess, m)
	if err != nil {
		return 0, err
	}
	m.Status = SessionMessageUnread
	m.BlockedReason = ""
	if reason != "" {
		m.Status = SessionMessageBlocked
		m.BlockedReason = reason
	}

	var id int64
	err = d.QueryRow(`INSERT INTO session_messages(session_id, parent_message_id, from_agent, to_agent,
  kind, body, task_id, intent_id, finding_id, status, blocked_reason, claims_authorization,
  receipt_of, receipt_action)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14) RETURNING id`,
		m.SessionID, m.ParentMessageID, m.FromAgent, m.ToAgent, m.Kind, m.Body,
		m.TaskID, m.IntentID, m.FindingID, m.Status, m.BlockedReason, m.ClaimsAuthorization,
		m.ReceiptOf, m.ReceiptAction).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("写会话消息失败: %w", err)
	}
	m.ID = id
	return id, nil
}

// sessionMessageAdmissionReason 判定一条消息能不能投递；返回非空原因即被拦截。
//
// 引用了不存在或跨范围的 intent/finding 时会把对应外键置空（否则外键约束会让
// 整条 INSERT 失败），原始 id 写进拦截原因里留痕。
func (d *DB) sessionMessageAdmissionReason(sess *Session, m *SessionMessage) (string, error) {
	if m.TaskID != nil && sess.TaskID != nil && *m.TaskID != *sess.TaskID {
		id := *m.TaskID
		var exists bool
		if err := d.QueryRow(`SELECT EXISTS(SELECT 1 FROM tasks WHERE id=$1)`, id).Scan(&exists); err != nil {
			return "", fmt.Errorf("查询任务失败: %w", err)
		}
		if !exists {
			m.TaskID = nil
		}
		return fmt.Sprintf("消息携带的任务 %d 与会话绑定的任务 %d 不一致", id, *sess.TaskID), nil
	}
	if m.IntentID != nil && sess.TaskID != nil {
		id := *m.IntentID
		var expID int64
		err := d.QueryRow(`SELECT exploration_id FROM exploration_nodes WHERE id=$1`, id).Scan(&expID)
		if err != nil {
			if !errors.Is(err, sql.ErrNoRows) {
				return "", fmt.Errorf("查询意图节点失败: %w", err)
			}
			m.IntentID = nil
			return fmt.Sprintf("意图节点 %d 不存在", id), nil
		}
		var taskExp int64
		if err := d.QueryRow(`SELECT exploration_id FROM tasks WHERE id=$1`, *sess.TaskID).Scan(&taskExp); err != nil {
			if !errors.Is(err, sql.ErrNoRows) {
				return "", fmt.Errorf("查询任务失败: %w", err)
			}
			m.IntentID = nil
			return fmt.Sprintf("会话绑定的任务 %d 不存在", *sess.TaskID), nil
		}
		if expID != taskExp {
			m.IntentID = nil
			return fmt.Sprintf("意图节点 %d 不属于本会话任务的探索图", id), nil
		}
	}
	if m.FindingID != nil && sess.TaskID != nil {
		id := *m.FindingID
		var taskID *int64
		err := d.QueryRow(`SELECT task_id FROM findings WHERE id=$1`, id).Scan(&taskID)
		if err != nil {
			if !errors.Is(err, sql.ErrNoRows) {
				return "", fmt.Errorf("查询漏洞记录失败: %w", err)
			}
			m.FindingID = nil
			return fmt.Sprintf("漏洞记录 %d 不存在", id), nil
		}
		if taskID == nil || *taskID != *sess.TaskID {
			m.FindingID = nil
			return fmt.Sprintf("漏洞记录 %d 不属于本会话任务", id), nil
		}
	}
	if m.ParentMessageID != nil {
		id := *m.ParentMessageID
		var same bool
		if err := d.QueryRow(`SELECT EXISTS(SELECT 1 FROM session_messages
WHERE id=$1 AND session_id=$2)`, id, m.SessionID).Scan(&same); err != nil {
			return "", fmt.Errorf("查询原消息失败: %w", err)
		}
		if !same {
			m.ParentMessageID = nil
			return fmt.Sprintf("原消息 %d 不在本会话里", id), nil
		}
	}
	if m.ReceiptOf != nil {
		id := *m.ReceiptOf
		var same bool
		if err := d.QueryRow(`SELECT EXISTS(SELECT 1 FROM session_messages
WHERE id=$1 AND session_id=$2)`, id, m.SessionID).Scan(&same); err != nil {
			return "", fmt.Errorf("查询原消息失败: %w", err)
		}
		if !same {
			return "", fmt.Errorf("收据条挂的原消息 %d 不在本会话里", id)
		}
	}
	if m.ToAgent != "" {
		var member bool
		if err := d.QueryRow(`SELECT EXISTS(SELECT 1 FROM session_members
WHERE session_id=$1 AND agent_key=$2)`, m.SessionID, m.ToAgent).Scan(&member); err != nil {
			return "", fmt.Errorf("查询会话成员失败: %w", err)
		}
		if !member {
			return fmt.Sprintf("收件 agent %q 不是本会话成员", m.ToAgent), nil
		}
	}
	return "", nil
}

// SessionMessages 读会话消息流（含 agent 消息、收据条与线程回复）。
//
// 排序按 id 升序：id 是本会话内的单调序号，线程与收据条靠 parent_message_id /
// receipt_of 表达归属，不靠时间戳猜。
func (d *DB) SessionMessages(sessionID int64, limit int) ([]SessionMessage, error) {
	return d.SessionMessagesSince(sessionID, 0, limit)
}

// SessionMessagesSince 读会话里 id 更大的消息（增量拉；sinceID=0 ⇒ 全量）。
//
// 水位线用**消息 id**而不是时间戳：id 是本会话内的单调序号 ⇒ 增量拉既不会漏
// 也不会重（同毫秒写入的两条消息，用时间戳会漏掉一条）。
func (d *DB) SessionMessagesSince(sessionID int64, sinceID int64, limit int) ([]SessionMessage, error) {
	if err := d.ensureSessionSchema(); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 200
	}
	rows, err := d.Query(`SELECT `+sessionMessageCols+` FROM session_messages
WHERE session_id=$1 AND id > $2 ORDER BY id ASC LIMIT $3`, sessionID, sinceID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SessionMessage{}
	for rows.Next() {
		var m SessionMessage
		if err := rows.Scan(&m.ID, &m.SessionID, &m.ParentMessageID, &m.FromAgent, &m.ToAgent,
			&m.Kind, &m.Body, &m.TaskID, &m.IntentID, &m.FindingID, &m.Status,
			&m.BlockedReason, &m.ClaimsAuthorization, &m.ReceiptOf, &m.ReceiptAction,
			&m.CreatedAt, &m.ReadAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// SessionMessageByID 读会话流里的一条消息（不存在时返回 nil, nil）。
//
// 用途：唤醒器展开「内容与 #N 相同、已转交」这类**指针式转交说明**时，按 id 取
// 被引用消息的原文内联进 wake 输入 —— 接单方拿到的是内容，不是指针。
func (d *DB) SessionMessageByID(sessionID, id int64) (*SessionMessage, error) {
	if err := d.ensureSessionSchema(); err != nil {
		return nil, err
	}
	var m SessionMessage
	err := d.QueryRow(`SELECT `+sessionMessageCols+` FROM session_messages
WHERE session_id=$1 AND id=$2`, sessionID, id).Scan(&m.ID, &m.SessionID, &m.ParentMessageID, &m.FromAgent, &m.ToAgent,
		&m.Kind, &m.Body, &m.TaskID, &m.IntentID, &m.FindingID, &m.Status,
		&m.BlockedReason, &m.ClaimsAuthorization, &m.ReceiptOf, &m.ReceiptAction,
		&m.CreatedAt, &m.ReadAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读会话消息失败: %w", err)
	}
	return &m, nil
}

// SessionConversationIDs 列这个会话背后的旧对话 id（迁移回填后，一个会话可以
// 挂着多个对话 —— 每个成员 agent 一个）。
//
// 用途：判定「这个会话有没有正在跑的对话」（服务端拿它查 in-flight 状态）。
func (d *DB) SessionConversationIDs(sessionID int64) ([]int64, error) {
	if err := d.ensureSessionSchema(); err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT id FROM conversations WHERE session_id=$1 ORDER BY id`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("查会话的对话失败: %w", err)
	}
	defer rows.Close()
	out := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// BindSessionTask 把任务绑定到会话（sessions.task_id）。幂等且「首个为准」：
// 已绑定别的任务时不覆盖（返回 ok=false），绑定同一条任务视为成功。
//
// 用途：总控会话创建时带 goal 一并起任务；成员在会话里 spawn_task 后把任务绑回
// 会话（server/session_task_mirror.go 的播报路由按绑定走）。UPDATE 只碰 task_id
// 一列，updated_at 由 trg_sessions_upd 维护。
func (d *DB) BindSessionTask(sessionID, taskID int64) (bool, error) {
	if err := d.ensureSessionSchema(); err != nil {
		return false, err
	}
	if sessionID <= 0 || taskID <= 0 {
		return false, errors.New("绑定参数不合法（session/task id 必须为正）")
	}
	var current *int64
	err := d.QueryRow(`SELECT task_id FROM sessions WHERE id=$1`, sessionID).Scan(&current)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, fmt.Errorf("会话 %d 不存在", sessionID)
		}
		return false, fmt.Errorf("读会话失败: %w", err)
	}
	if current != nil {
		if *current == taskID {
			return true, nil
		}
		return false, nil // 首个为准：一个会话一个主任务，后续 spawn 的任务是它的子任务
	}
	if _, err := d.Exec(`UPDATE sessions SET task_id=$2 WHERE id=$1 AND task_id IS NULL`, sessionID, taskID); err != nil {
		return false, fmt.Errorf("绑定任务失败: %w", err)
	}
	// 并发下可能被别处抢先绑定 ⇒ 复核一次（失败只影响播报路由，不致命）。
	err = d.QueryRow(`SELECT task_id FROM sessions WHERE id=$1`, sessionID).Scan(&current)
	if err != nil {
		return false, fmt.Errorf("复核绑定失败: %w", err)
	}
	return current != nil && *current == taskID, nil
}

// SessionByTask 查绑定到某任务的会话（总控/群聊的进展播报路由）。
// 一个任务至多被一个会话持有主绑定（首个为准）⇒ 取 task_id 命中的第一条；
// 没有绑定 ⇒ nil。
func (d *DB) SessionByTask(taskID int64) (*Session, error) {
	if err := d.ensureSessionSchema(); err != nil {
		return nil, err
	}
	var id int64
	err := d.QueryRow(`SELECT id FROM sessions WHERE task_id=$1 ORDER BY id LIMIT 1`, taskID).Scan(&id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("查任务会话失败: %w", err)
	}
	sess, err := d.GetSession(id)
	if err != nil {
		return nil, fmt.Errorf("读任务会话失败: %w", err)
	}
	return sess, nil
}

// SessionConversationFor 返回会话里某个成员的专属对话线程；没有就建一个。
//
// 为什么需要它：conversations.agent_key 是单值列 ⇒ 一个成员一个线程；会话流
// （session_messages）只记消息，「成员跑一个 turn」要落在它的对话线程上
// （transcript / 活动 / in-flight 判定都以 conversations 为主体）。
// 迁移回填出来的会话每个成员自带一个对话（session_id 回填）；新会话（HTTP 建的）
// 成员入席时**不**建对话 ⇒ 唤醒器在第一次唤醒该成员时按需补建。
//
// 建线程的插入失败回表重读一次（同一成员的线程在并发唤醒下只建一条）：对话线程
// 重复的代价（transcript 分裂）比少一条难补救，宁可重读。
func (d *DB) SessionConversationFor(sessionID int64, agentKey string) (*Conversation, error) {
	if err := d.ensureSessionSchema(); err != nil {
		return nil, err
	}
	agentKey = strings.TrimSpace(agentKey)
	if agentKey == "" {
		return nil, errors.New("agent key 不能为空")
	}
	var id int64
	err := d.QueryRow(`SELECT id FROM conversations WHERE session_id=$1 AND agent_key=$2
ORDER BY id LIMIT 1`, sessionID, agentKey).Scan(&id)
	if err == nil {
		c, err := scanConv(d.QueryRow(`SELECT ` + convCols + ` FROM conversations WHERE id=$1`, id))
		if err != nil {
			return nil, fmt.Errorf("读会话对话线程失败: %w", err)
		}
		return &c, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("查会话对话线程失败: %w", err)
	}
	// 没有线程 ⇒ 建一个。标题取「会话显示名/成员 key」（与任务大厅建成员会话的
	// 命名一致）；会话名缺失时退到 id，保证标题非空。
	var sessName string
	_ = d.QueryRow(`SELECT COALESCE(NULLIF(name,''), NULLIF(title,'')) FROM sessions WHERE id=$1`,
		sessionID).Scan(&sessName)
	if sessName == "" {
		sessName = fmt.Sprintf("会话-%d", sessionID)
	}
	// A1：唤醒线程标记 origin='wake'——它是会话的子记录，不是一级对话。
	c, err := scanConv(d.QueryRow(`INSERT INTO conversations(agent_key, title, session_id, origin)
VALUES ($1, $2, $3, $4) RETURNING `+convCols, agentKey, sessName+"/"+agentKey, sessionID, OriginWake))
	if err != nil {
		// 并发建线程没有 DB 约束兜底（session_id+agent_key 无 UNIQUE）⇒ 插入失败
		// 回表重读一次；仍读不到就如实报错。
		if inner := d.QueryRow(`SELECT id FROM conversations WHERE session_id=$1 AND agent_key=$2
ORDER BY id LIMIT 1`, sessionID, agentKey).Scan(&id); inner == nil {
			c, err = scanConv(d.QueryRow(`SELECT ` + convCols + ` FROM conversations WHERE id=$1`, id))
			if err == nil {
				return &c, nil
			}
		}
		return nil, fmt.Errorf("建会话对话线程失败: %w", err)
	}
	return &c, nil
}

// ChatGroupIDBySession 查这个会话对应的大厅 id（没有则 nil）。
//
// 一个群（chat_groups）回填出的会话带 kind='task_hall' ⇒ 前端能拿它读额度与
// 并发状态（会话页右侧的「关联与状态」面板）。
func (d *DB) ChatGroupIDBySession(sessionID int64) (*int64, error) {
	if err := d.ensureSessionSchema(); err != nil {
		return nil, err
	}
	var id int64
	err := d.QueryRow(`SELECT id FROM chat_groups WHERE session_id=$1 ORDER BY id LIMIT 1`,
		sessionID).Scan(&id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("查会话对应的大厅失败: %w", err)
	}
	return &id, nil
}

// ChatGroupIDsBySession 批量查多个会话各自对应的大厅 id（列表页免 N+1）。
func (d *DB) ChatGroupIDsBySession(sessionIDs []int64) (map[int64]int64, error) {
	out := map[int64]int64{}
	if len(sessionIDs) == 0 {
		return out, nil
	}
	if err := d.ensureSessionSchema(); err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT session_id, id FROM chat_groups
WHERE session_id = ANY($1) ORDER BY id`, sessionIDs)
	if err != nil {
		return nil, fmt.Errorf("查会话对应的大厅失败: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var sessionID, groupID int64
		if err := rows.Scan(&sessionID, &groupID); err != nil {
			return nil, err
		}
		if _, dup := out[sessionID]; !dup {
			out[sessionID] = groupID
		}
	}
	return out, rows.Err()
}

// MarkSessionMessagesRead 把给定消息标为已读（只影响仍是 unread 的）。
func (d *DB) MarkSessionMessagesRead(ids []int64) error {
	if err := d.ensureSessionSchema(); err != nil {
		return err
	}
	if len(ids) == 0 {
		return nil
	}
	if _, err := d.Exec(`UPDATE session_messages SET status='read', read_at=now()
WHERE id = ANY($1) AND status='unread'`, ids); err != nil {
		return fmt.Errorf("标记会话消息已读失败: %w", err)
	}
	return nil
}

// ---------- @提及 ----------

// ParseSessionAgentMentions 从消息正文里解析 @提及 的 agent key（去重、保序）。
//
// '@everyone' 是广播关键词，不作为 agent key 返回。
//
// ⚠️ 解析规则只有**一份**：真正的实现在 internal/sessionwake（唤醒裁决层），
// 这里只是转发 —— 唤醒裁决与消息写入必须用同一套提及口径，否则「@ 到的人」
// 与「被叫醒的人」会对不上（两份副本就是漂移的开始）。
func ParseSessionAgentMentions(body string) []string {
	return sessionwake.ParseMentions(body).Agents
}

// ---------- 会话 ↔ 旧对话 / 成员信箱 ----------

// SessionByConversation 按旧对话 id 反查它所属的会话。
//
// 为什么需要它：迁移回填之后，**每个旧对话背后是一个会话**
// （conversations.session_id，proposal §4.7 第 2 条）⇒ agent 侧只知道自己
// 在哪个对话里跑，要参与群聊就得先反查自己所在的会话。
//
// 没有绑定（session_id 为 NULL）或对话不存在时返回 nil, nil（调用方按「这个
// 对话还没进会话体系」处理，不是错误）。
func (d *DB) SessionByConversation(conversationID int64) (*Session, error) {
	if err := d.ensureSessionSchema(); err != nil {
		return nil, err
	}
	var sessionID *int64
	if err := d.QueryRow(`SELECT session_id FROM conversations WHERE id=$1`,
		conversationID).Scan(&sessionID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("查对话的会话失败: %w", err)
	}
	if sessionID == nil {
		return nil, nil
	}
	return d.GetSession(*sessionID)
}

// SessionMessagesForAgent 读一个 agent 在会话里该看的消息（成员信箱的取件面）。
//
// 过滤口径（四条，每条都有理由）：
//
//  1. **定向给它的 + 广播给全会话的**：别的成员之间的私聊不该进它的上下文
//     （to_agent 指向第三个人时它不是收件人）。
//  2. **排除被拦下的**（status='blocked'）：拦截发生在投递前，被拦的消息不进
//     接收方上下文（与 agent/group_mailbox.go 的同款纪律）。
//  3. **排除收据条**（kind='receipt'）：收据是给界面看的确认噪音（「收到 / 好
//     的」），不是内容（proposal §4.3 的噪音控制）。
//  4. **排除它自己发的**：自己的话不需要再读一遍。
//
// ⚠️ 为什么**不**按 status='unread' 过滤：会话消息的 status / read_at 是**消息
// 级**的，不是成员级的 ⇒ 一个成员读了就会把这条消息对**所有**成员标成已读 ⇒
// 群聊会退化成单播（第二个成员就看不见第一条了）。所以这里用**窗口**（最近
// limit 条）代替「未读」语义。成员级的已读水位线需要一张新表（成员 × 消息），
// 那是 P5 噪音治理的活，不在本单元里偷偷加。
//
// 排序按 id 升序（id 是本会话内的单调序号）⇒ 同一份数据永远得到同一份窗口。
func (d *DB) SessionMessagesForAgent(sessionID int64, toAgent string, limit int) ([]SessionMessage, error) {
	if err := d.ensureSessionSchema(); err != nil {
		return nil, err
	}
	toAgent = strings.TrimSpace(toAgent)
	if toAgent == "" {
		return nil, errors.New("收件 agent key 不能为空")
	}
	if limit <= 0 {
		limit = 50
	}
	// 先取窗口内最新的 limit 条，再按 id 升序排回去。
	rows, err := d.Query(`SELECT `+sessionMessageCols+` FROM session_messages
WHERE session_id=$1
  AND status <> 'blocked'
  AND kind <> 'receipt'
  AND (to_agent = '' OR to_agent = $2)
  AND (from_agent = '' OR from_agent <> $2)
ORDER BY id DESC LIMIT $3`, sessionID, toAgent, limit)
	if err != nil {
		return nil, fmt.Errorf("读会话成员信箱失败: %w", err)
	}
	defer rows.Close()
	out := []SessionMessage{}
	for rows.Next() {
		var m SessionMessage
		if err := rows.Scan(&m.ID, &m.SessionID, &m.ParentMessageID, &m.FromAgent, &m.ToAgent,
			&m.Kind, &m.Body, &m.TaskID, &m.IntentID, &m.FindingID, &m.Status,
			&m.BlockedReason, &m.ClaimsAuthorization, &m.ReceiptOf, &m.ReceiptAction,
			&m.CreatedAt, &m.ReadAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// 反转成升序（窗口是倒着取的）。
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// ConversationsForSession 列一个会话名下的全部对话线程（A2 删除守卫用）。
func (d *DB) ConversationsForSession(sessionID int64) ([]*Conversation, error) {
	rows, err := d.Query(`SELECT `+convCols+` FROM conversations WHERE session_id=$1 ORDER BY id`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Conversation{}
	for rows.Next() {
		c, err := scanConv(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, &c)
	}
	return out, rows.Err()
}

// 群聊（任务大厅）访问层：一个大厅会话 + N 个成员会话 + 共享的一个 task。
//
// 形状（Phase 3 计划 §0 的落地澄清）：conversations.agent_key 是单值列 ⇒ 一个会话
// 只能跑一个 agent ⇒ 群聊落地为「1 大厅会话 + N 成员会话」：
//   - 大厅会话（chat_groups.conversation_id）：agent_key='auto'，人在此布置任务、看全局；
//   - 成员会话（chat_group_members.conversation_id）：每个成员 agent 一个，独立 transcript，
//     共享同一个 task（也就是同一张探索图）。
//
// 两条被写死的边界：
//  1. 归属权威唯一 = exploration_nodes.owner。消息表**没有** owner/assignee 一类的列，
//     kind='request' 且带 intent_id 的消息只是【提议】，不转移归属、不授予完成权。
//  2. 群内工具清单是**禁列表**（chat_group_disabled_tools），表里没有 enabled 列 ⇒
//     结构上无法表达"放开"，群内只能让权限更严。
//
// allow: SIZE_OK —— 四张表的读/写集中在一个文件里是任务书给定的文件域（不允许再拆）；
// 拆开的收益（按表分文件）不足以抵过"群聊语义被切散"的代价。
package db

import (
	_ "embed"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

//go:embed schema_chat_group.sql
var chatGroupSchemaSQL string

// chatGroupSchemaReady 记录某个连接已经应用过群聊 DDL。
//
// 为什么用包级 sync.Map 而不是 DB 上的字段：DB 结构体在 db/db.go 里，本单元不改它。
// 键是底层 *sql.DB 指针 —— 每个测试包/实例各连各的库，互不干扰。
var chatGroupSchemaReady sync.Map

// 消息状态。blocked 是**投递前**拦截的结果：消息落库留痕，但不进入接收方上下文。
const (
	ChatGroupMessageUnread  = "unread"
	ChatGroupMessageRead    = "read"
	ChatGroupMessageAcked   = "acked"
	ChatGroupMessageBlocked = "blocked"
)

// chatGroupMessageKinds 是消息类型白名单。
//
// request 是【协商请求】不是【指派】：它不改变 intent 归属（归属只看
// exploration_nodes.owner），也不授予任何写面操作。
var chatGroupMessageKinds = map[string]bool{
	"note": true, "request": true, "answer": true, "finding": true, "escalation": true,
}

// ChatGroup 是一次群聊。
type ChatGroup struct {
	ID     int64  `json:"id"`
	TaskID int64  `json:"task_id"`
	// TaskExpID 是该群 task 的 exploration_id：成员会话注入任务上下文时要用。
	TaskExpID        int64      `json:"task_exploration_id"`
	ConversationID   int64      `json:"conversation_id"`
	Name             string     `json:"name"`
	Status           string     `json:"status"`
	ConcurrencyLimit int        `json:"concurrency_limit"`
	PlanID           *int64     `json:"plan_id,omitempty"`
	BudgetTokens     *int64     `json:"budget_tokens,omitempty"`
	DrainedAt        *time.Time `json:"drained_at,omitempty"`
	ParkedAt         *time.Time `json:"parked_at,omitempty"`
	ResumedAt        *time.Time `json:"resumed_at,omitempty"`
	CreatedAt        time.Time  `json:"created_at"`
	UpdatedAt        time.Time  `json:"updated_at"`
}

// ChatGroupMember 是群成员：agent key + 它自己的会话 + 大厅内的分工。
// role 只表达分工（dispatcher/member/observer），**不表达权限**。
type ChatGroupMember struct {
	GroupID        int64     `json:"group_id"`
	AgentKey       string    `json:"agent_key"`
	ConversationID *int64    `json:"conversation_id,omitempty"`
	Role           string    `json:"role"`
	JoinedAt       time.Time `json:"joined_at"`
}

// ChatGroupMessage 是群内消息箱里的一条消息。
//
// 注意这里**没有** owner/assignee 字段：意图归属的唯一权威是 exploration_nodes.owner。
// ClaimsAuthorization 只表示"发送方声称本消息携带某个已批准"，它永远是不可信输入，
// 没有任何权限门读这一列（2026-10-29-auto-permission-tiers.md §3 第 2 条）。
type ChatGroupMessage struct {
	ID        int64  `json:"id"`
	GroupID   int64  `json:"group_id"`
	FromAgent string `json:"from_agent"`
	ToAgent   string `json:"to_agent"` // "" = 广播给全群
	Kind      string `json:"kind"`
	Body      string `json:"body"`
	TaskID    *int64 `json:"task_id,omitempty"`
	IntentID  *int64 `json:"intent_id,omitempty"`
	FindingID *int64 `json:"finding_id,omitempty"`
	Status    string `json:"status"`
	// BlockedReason 非空表示这条消息在投递前被拦下（Status=blocked）。
	BlockedReason string `json:"blocked_reason,omitempty"`
	// ClaimsAuthorization 标记"发送方声称本消息携带已批准"——只留痕，不授权。
	ClaimsAuthorization bool      `json:"claims_authorization,omitempty"`
	CreatedAt           time.Time `json:"created_at"`
	ReadAt              *time.Time `json:"read_at,omitempty"`
}

// chatGroupCols 是群组列清单。exploration_id 从 tasks JOIN 出来：成员的会话要靠它
// 拿到任务上下文（否则群聊里的 agent 拿不到探索图）。
const chatGroupCols = `g.id, g.task_id, t.exploration_id, g.conversation_id, g.name, g.status,
 g.concurrency_limit, g.plan_id, g.budget_tokens, g.drained_at, g.parked_at, g.resumed_at,
 g.created_at, g.updated_at`

type chatGroupScanner interface{ Scan(...any) error }

func scanChatGroup(row chatGroupScanner) (*ChatGroup, error) {
	var g ChatGroup
	err := row.Scan(&g.ID, &g.TaskID, &g.TaskExpID, &g.ConversationID, &g.Name, &g.Status,
		&g.ConcurrencyLimit, &g.PlanID, &g.BudgetTokens, &g.DrainedAt, &g.ParkedAt, &g.ResumedAt,
		&g.CreatedAt, &g.UpdatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &g, nil
}

// EnsureChatGroupSchema 应用群聊 DDL（幂等）。
//
// db/schema.sql 由别的单元持有，本单元的 DDL 放在 db/schema_chat_group.sql，
// 由这里在 db.Open 之后补齐（见 db/chat_group.go 的 ensureChatGroupSchema 惰性调用）。
// schema 属主把内容并进 db/schema.sql 之后，这里重复应用也无副作用。
func (d *DB) EnsureChatGroupSchema() error {
	if _, ok := chatGroupSchemaReady.Load(d.DB); ok {
		return nil
	}
	if _, err := d.Exec(chatGroupSchemaSQL); err != nil {
		return fmt.Errorf("应用群聊 schema 失败: %w", err)
	}
	chatGroupSchemaReady.Store(d.DB, struct{}{})
	return nil
}

// ensureChatGroupSchema 是访问层的幂等入口：首次调用时建表，之后只是一次 map 查表。
func (d *DB) ensureChatGroupSchema() error { return d.EnsureChatGroupSchema() }

// CreateChatGroup 建一个群组：绑定大厅会话 + 一个 task。
func (d *DB) CreateChatGroup(g *ChatGroup) (int64, error) {
	if err := d.ensureChatGroupSchema(); err != nil {
		return 0, err
	}
	if g == nil {
		return 0, errors.New("群组参数为空")
	}
	if g.ConversationID <= 0 {
		return 0, errors.New("群组必须绑定大厅会话")
	}
	if g.TaskID <= 0 {
		return 0, errors.New("群组必须绑定一个任务")
	}
	if g.Status == "" {
		g.Status = "active"
	}
	if g.ConcurrencyLimit <= 0 {
		g.ConcurrencyLimit = 3
	}
	var id int64
	err := d.QueryRow(`
INSERT INTO chat_groups(conversation_id, task_id, name, status, concurrency_limit, plan_id, budget_tokens)
VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING id`,
		g.ConversationID, g.TaskID, g.Name, g.Status, g.ConcurrencyLimit, g.PlanID, g.BudgetTokens).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("建群失败: %w", err)
	}
	g.ID = id
	return id, nil
}

// GetChatGroup 按 id 读群组。
func (d *DB) GetChatGroup(id int64) (*ChatGroup, error) {
	if err := d.ensureChatGroupSchema(); err != nil {
		return nil, err
	}
	row := d.QueryRow(`SELECT `+chatGroupCols+`
FROM chat_groups g JOIN tasks t ON t.id = g.task_id WHERE g.id=$1`, id)
	return scanChatGroup(row)
}

// GroupByTask 按 task 反查群组：一个 task 至多一个大厅（uq_chat_groups_task）。
func (d *DB) GroupByTask(taskID int64) (*ChatGroup, error) {
	if err := d.ensureChatGroupSchema(); err != nil {
		return nil, err
	}
	row := d.QueryRow(`SELECT `+chatGroupCols+`
FROM chat_groups g JOIN tasks t ON t.id = g.task_id WHERE g.task_id=$1`, taskID)
	return scanChatGroup(row)
}

// GroupByConversation 按会话反查群组：大厅会话或任一成员会话都命中同一个群。
func (d *DB) GroupByConversation(conversationID int64) (*ChatGroup, error) {
	if err := d.ensureChatGroupSchema(); err != nil {
		return nil, err
	}
	row := d.QueryRow(`SELECT `+chatGroupCols+`
FROM chat_groups g JOIN tasks t ON t.id = g.task_id
WHERE g.conversation_id = $1
   OR EXISTS (SELECT 1 FROM chat_group_members m WHERE m.group_id = g.id AND m.conversation_id = $1)
LIMIT 1`, conversationID)
	return scanChatGroup(row)
}

// ListChatGroups 列群组；status 为空则列全部。
func (d *DB) ListChatGroups(status string) ([]ChatGroup, error) {
	if err := d.ensureChatGroupSchema(); err != nil {
		return nil, err
	}
	q := `SELECT ` + chatGroupCols + ` FROM chat_groups g JOIN tasks t ON t.id = g.task_id`
	args := []any{}
	if s := strings.TrimSpace(status); s != "" {
		q += ` WHERE g.status = $1`
		args = append(args, s)
	}
	q += ` ORDER BY g.id ASC`
	rows, err := d.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ChatGroup{}
	for rows.Next() {
		var g ChatGroup
		if err := rows.Scan(&g.ID, &g.TaskID, &g.TaskExpID, &g.ConversationID, &g.Name, &g.Status,
			&g.ConcurrencyLimit, &g.PlanID, &g.BudgetTokens, &g.DrainedAt, &g.ParkedAt, &g.ResumedAt,
			&g.CreatedAt, &g.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// DeleteChatGroup 删一个群组（成员、禁列表、消息箱随外键级联删除）。
func (d *DB) DeleteChatGroup(id int64) error {
	if err := d.ensureChatGroupSchema(); err != nil {
		return err
	}
	if _, err := d.Exec(`DELETE FROM chat_groups WHERE id=$1`, id); err != nil {
		return fmt.Errorf("删群失败: %w", err)
	}
	return nil
}

// AddChatGroupMember 加一个群成员（幂等：同组同 agent 覆盖会话与分工）。
func (d *DB) AddChatGroupMember(groupID int64, agentKey string, conversationID *int64, role string) error {
	if err := d.ensureChatGroupSchema(); err != nil {
		return err
	}
	agentKey = strings.TrimSpace(agentKey)
	if agentKey == "" {
		return errors.New("成员 agent key 不能为空")
	}
	if role == "" {
		role = "member"
	}
	switch role {
	case "dispatcher", "member", "observer":
	default:
		return fmt.Errorf("成员分工只能是 dispatcher/member/observer，收到 %q", role)
	}
	_, err := d.Exec(`
INSERT INTO chat_group_members(group_id, agent_key, conversation_id, role)
VALUES ($1,$2,$3,$4)
ON CONFLICT (group_id, agent_key) DO UPDATE
  SET conversation_id = EXCLUDED.conversation_id, role = EXCLUDED.role`,
		groupID, agentKey, conversationID, role)
	if err != nil {
		return fmt.Errorf("加群成员失败: %w", err)
	}
	return nil
}

// ChatGroupMembers 列群成员。
func (d *DB) ChatGroupMembers(groupID int64) ([]ChatGroupMember, error) {
	if err := d.ensureChatGroupSchema(); err != nil {
		return nil, err
	}
	rows, err := d.Query(`
SELECT group_id, agent_key, conversation_id, role, joined_at
FROM chat_group_members WHERE group_id=$1 ORDER BY joined_at ASC, agent_key ASC`, groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ChatGroupMember{}
	for rows.Next() {
		var m ChatGroupMember
		if err := rows.Scan(&m.GroupID, &m.AgentKey, &m.ConversationID, &m.Role, &m.JoinedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ChatGroupMemberConversations 读成员 agent key → 成员会话 id 的映射。
// 成员会话各自独立（互不污染 transcript），但共享群组的那一个 task。
func (d *DB) ChatGroupMemberConversations(groupID int64) (map[string]int64, error) {
	if err := d.ensureChatGroupSchema(); err != nil {
		return nil, err
	}
	rows, err := d.Query(`
SELECT agent_key, conversation_id FROM chat_group_members
WHERE group_id=$1 AND conversation_id IS NOT NULL`, groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var key string
		var conv int64
		if err := rows.Scan(&key, &conv); err != nil {
			return nil, err
		}
		out[key] = conv
	}
	return out, rows.Err()
}

// AddChatGroupMessage 往群消息箱写一条消息，并施加**投递前**的准入判定。
//
// 准入判定（被拦的消息落库标 blocked，但不进入接收方上下文 —— 2026-10-29 §3 第 3 条）：
//   - 消息携带的 task 必须就是群组绑定的那个 task（群只绑一个 task）；
//   - 消息引用的 intent 必须属于群组 task 的探索图（跨任务的意图不许在群里流转）；
//   - 消息引用的 finding 必须属于群组 task；
//   - 定向消息的收件人必须是本群成员。
//
// 归属语义：带 intent_id 的消息只是**提议**。本方法不写任何 owner/assignee 列 ——
// 归属权威唯一 = exploration_nodes.owner。
func (d *DB) AddChatGroupMessage(m *ChatGroupMessage) (int64, error) {
	if err := d.ensureChatGroupSchema(); err != nil {
		return 0, err
	}
	if m == nil {
		return 0, errors.New("消息参数为空")
	}
	m.Kind = strings.TrimSpace(m.Kind)
	if m.Kind == "" {
		m.Kind = "note"
	}
	if !chatGroupMessageKinds[m.Kind] {
		return 0, errors.New("消息类型只能是 note/request/answer/finding/escalation")
	}
	m.Body = strings.TrimSpace(m.Body)
	if m.Body == "" {
		return 0, errors.New("消息正文不能为空")
	}
	m.ToAgent = strings.TrimSpace(m.ToAgent)
	m.FromAgent = strings.TrimSpace(m.FromAgent)

	g, err := d.GetChatGroup(m.GroupID)
	if err != nil {
		return 0, fmt.Errorf("查询群组失败: %w", err)
	}
	if g == nil {
		return 0, errors.New("群组不存在")
	}

	reason, err := d.chatGroupAdmissionReason(g, m)
	if err != nil {
		return 0, err
	}
	m.Status = ChatGroupMessageUnread
	m.BlockedReason = ""
	if reason != "" {
		m.Status = ChatGroupMessageBlocked
		m.BlockedReason = reason
	}

	var id int64
	err = d.QueryRow(`
INSERT INTO chat_group_messages(group_id, from_agent, to_agent, kind, body,
  task_id, intent_id, finding_id, status, blocked_reason, claims_authorization)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11) RETURNING id`,
		m.GroupID, m.FromAgent, m.ToAgent, m.Kind, m.Body,
		m.TaskID, m.IntentID, m.FindingID, m.Status, m.BlockedReason, m.ClaimsAuthorization).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("写群消息失败: %w", err)
	}
	m.ID = id
	return id, nil
}

// chatGroupAdmissionReason 判定一条消息能不能投递；返回非空原因即被拦截。
//
// 引用了不存在或跨任务的 intent/finding 时会把对应外键置空（否则外键约束会让
// 整条 INSERT 失败），原始 id 写进拦截原因里留痕。
func (d *DB) chatGroupAdmissionReason(g *ChatGroup, m *ChatGroupMessage) (string, error) {
	if m.TaskID != nil && *m.TaskID != g.TaskID {
		id := *m.TaskID
		// 保留 task_id 需要它真实存在（外键），否则整条 INSERT 写不进去 ——
		// 拦截不能变成"写库报错"，被拦的消息也要留痕。
		var exists bool
		if err := d.QueryRow(`SELECT EXISTS(SELECT 1 FROM tasks WHERE id=$1)`, id).Scan(&exists); err != nil {
			return "", fmt.Errorf("查询任务失败: %w", err)
		}
		if !exists {
			m.TaskID = nil
		}
		return fmt.Sprintf("消息携带的任务 %d 与群组绑定的任务 %d 不一致", id, g.TaskID), nil
	}
	if m.IntentID != nil {
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
		if expID != g.TaskExpID {
			m.IntentID = nil
			return fmt.Sprintf("意图节点 %d 不属于本群任务的探索图", id), nil
		}
	}
	if m.FindingID != nil {
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
		if taskID == nil || *taskID != g.TaskID {
			m.FindingID = nil
			return fmt.Sprintf("漏洞记录 %d 不属于本群任务", id), nil
		}
	}
	if m.ToAgent != "" {
		var member bool
		if err := d.QueryRow(`SELECT EXISTS(
  SELECT 1 FROM chat_group_members WHERE group_id=$1 AND agent_key=$2)`, g.ID, m.ToAgent).Scan(&member); err != nil {
			return "", fmt.Errorf("查询群成员失败: %w", err)
		}
		if !member {
			return fmt.Sprintf("收件 agent %q 不是本群成员", m.ToAgent), nil
		}
	}
	return "", nil
}

// UnreadChatGroupMessages 读一个成员还没读过的消息：给它的定向消息 + 全群广播。
//
// 拆成两条查询是刻意的：收取条件 (to_agent='' OR to_agent=<me>) 会让
// (group_id,to_agent,status) 索引失效，退化成扫全组消息再过滤。拆开后分别命中
// idx_cgm_broadcast_unread 与 idx_cgm_direct_unread 两条部分索引。
// status='unread' 天然排除被拦截（blocked）的消息 —— 它们不进入接收方上下文。
func (d *DB) UnreadChatGroupMessages(groupID int64, toAgent string, limit int) ([]ChatGroupMessage, error) {
	if err := d.ensureChatGroupSchema(); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 50
	}
	cols := `id, group_id, from_agent, to_agent, kind, body, task_id, intent_id, finding_id,
 status, blocked_reason, claims_authorization, created_at, read_at`
	broadcast, err := d.queryChatGroupMessages(
		`SELECT `+cols+` FROM chat_group_messages
 WHERE group_id=$1 AND to_agent='' AND status='unread' ORDER BY created_at ASC, id ASC LIMIT $2`,
		groupID, limit)
	if err != nil {
		return nil, err
	}
	direct, err := d.queryChatGroupMessages(
		`SELECT `+cols+` FROM chat_group_messages
 WHERE group_id=$1 AND to_agent=$3 AND status='unread' ORDER BY created_at ASC, id ASC LIMIT $2`,
		groupID, limit, toAgent)
	if err != nil {
		return nil, err
	}
	merged := make([]ChatGroupMessage, 0, len(broadcast)+len(direct))
	merged = append(merged, broadcast...)
	merged = append(merged, direct...)
	sortChatGroupMessages(merged)
	if len(merged) > limit {
		merged = merged[:limit]
	}
	return merged, nil
}

func (d *DB) queryChatGroupMessages(q string, args ...any) ([]ChatGroupMessage, error) {
	rows, err := d.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ChatGroupMessage{}
	for rows.Next() {
		var m ChatGroupMessage
		if err := rows.Scan(&m.ID, &m.GroupID, &m.FromAgent, &m.ToAgent, &m.Kind, &m.Body,
			&m.TaskID, &m.IntentID, &m.FindingID, &m.Status, &m.BlockedReason,
			&m.ClaimsAuthorization, &m.CreatedAt, &m.ReadAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// sortChatGroupMessages 按 (created_at, id) 升序 —— 消息箱按时间线读。
func sortChatGroupMessages(msgs []ChatGroupMessage) {
	sort.SliceStable(msgs, func(i, j int) bool {
		if msgs[i].CreatedAt.Equal(msgs[j].CreatedAt) {
			return msgs[i].ID < msgs[j].ID
		}
		return msgs[i].CreatedAt.Before(msgs[j].CreatedAt)
	})
}

// ListChatGroupMessages 读群消息箱的最近若干条（含已读/被拦截的），供管理端查看。
// 收取面（UnreadChatGroupMessages）只给成员看未读且未被拦截的。
func (d *DB) ListChatGroupMessages(groupID int64, limit int) ([]ChatGroupMessage, error) {
	if err := d.ensureChatGroupSchema(); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 100
	}
	return d.queryChatGroupMessages(`
SELECT id, group_id, from_agent, to_agent, kind, body, task_id, intent_id, finding_id,
 status, blocked_reason, claims_authorization, created_at, read_at
FROM chat_group_messages WHERE group_id=$1 ORDER BY created_at DESC, id DESC LIMIT $2`,
		groupID, limit)
}

// CountChatGroupMessages 数群消息箱里的消息条数（含被拦截的 —— 拦截也要留痕）。
func (d *DB) CountChatGroupMessages(groupID int64) (int, error) {
	if err := d.ensureChatGroupSchema(); err != nil {
		return 0, err
	}
	var n int
	if err := d.QueryRow(`SELECT count(*) FROM chat_group_messages WHERE group_id=$1`, groupID).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// MarkChatGroupMessagesRead 把给定消息标为已读（只影响仍是 unread 的）。
func (d *DB) MarkChatGroupMessagesRead(ids []int64) error {
	if err := d.ensureChatGroupSchema(); err != nil {
		return err
	}
	if len(ids) == 0 {
		return nil
	}
	if _, err := d.Exec(`UPDATE chat_group_messages SET status='read', read_at=now()
WHERE id = ANY($1) AND status='unread'`, ids); err != nil {
		return fmt.Errorf("标记群消息已读失败: %w", err)
	}
	return nil
}

// GroupDisabledTools 读群内工具**禁列表**。
//
// 返回值是"被禁用的工具 key → true"。这是**禁**语义：出现在列表里就是禁用。
// 表里没有 enabled 列 ⇒ 群内配置**不可能**放宽任何 agent 的工具集。
func (d *DB) GroupDisabledTools(groupID int64) (map[string]bool, error) {
	if err := d.ensureChatGroupSchema(); err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT tool_key FROM chat_group_disabled_tools WHERE group_id=$1`, groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		out[k] = true
	}
	return out, rows.Err()
}

// DisableChatGroupTool 登记一个群内禁用的工具（幂等）。
func (d *DB) DisableChatGroupTool(groupID int64, toolKey string) error {
	if err := d.ensureChatGroupSchema(); err != nil {
		return err
	}
	toolKey = strings.TrimSpace(toolKey)
	if toolKey == "" {
		return errors.New("工具 key 不能为空")
	}
	if _, err := d.Exec(`INSERT INTO chat_group_disabled_tools(group_id, tool_key)
VALUES ($1,$2) ON CONFLICT (group_id, tool_key) DO NOTHING`, groupID, toolKey); err != nil {
		return fmt.Errorf("登记群内禁用工具失败: %w", err)
	}
	return nil
}

// EnableChatGroupTool 撤销一条群内禁用（只是把 key 从禁列表里拿掉，不动全局绑定）。
func (d *DB) EnableChatGroupTool(groupID int64, toolKey string) error {
	if err := d.ensureChatGroupSchema(); err != nil {
		return err
	}
	if _, err := d.Exec(`DELETE FROM chat_group_disabled_tools WHERE group_id=$1 AND tool_key=$2`,
		groupID, strings.TrimSpace(toolKey)); err != nil {
		return fmt.Errorf("撤销群内禁用工具失败: %w", err)
	}
	return nil
}

// GroupDisabledTools 是 AssetStore 上的同义入口：工具装配侧拿的是资产存储句柄。
func (s *AssetStore) GroupDisabledTools(groupID int64) (map[string]bool, error) {
	return s.db.GroupDisabledTools(groupID)
}

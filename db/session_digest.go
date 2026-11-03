package db

// 会话收件条（inbox/digest）的持久化层：水位线 + 折叠时间线 + 摘要的读取。
//
// 依据：session-groupchat-proposal.md §7 的 P5 行「收件条/线程折叠等噪音治理」。
//
// 职责切分（与 db/session_thread.go 的分工）：
//
//	db/session_thread.go   纯函数：怎么折叠、怎么挑条目、怎么算气泡数
//	db/session_digest.go   本文件：状态存哪、怎么读出来、怎么把「读掉了」记下来
//
// 三条纪律：
//  1. **判定在确定性层**：摘要内容由 Go 算，不由模型总结（同款：模型只提议）。
//  2. **不删数据**：折叠只是呈现；消息原文一条不少地留在 session_messages。
//  3. **DDL 幂等**：db/schema.sql 是 RESERVED 文件，本单元的 DDL 放在
//     db/schema_session_digest.sql，由 EnsureSessionDigestSchema 幂等补齐
//     （与 db/session.go 的 EnsureSessionSchema 同款）。

import (
	"database/sql"
	_ "embed"
	"fmt"
	"sync"
	"time"
)

//go:embed schema_session_digest.sql
var sessionDigestSchemaSQL string

// sessionDigestSchemaReady 记录某个连接已经应用过收件条 DDL。
//
// 与 db/sod.go 的 sodSchemaReady 同款：用包级 sync.Map，键是底层 *sql.DB 指针。
var sessionDigestSchemaReady sync.Map

// EnsureSessionDigestSchema 应用收件条 DDL（幂等）。
func (d *DB) EnsureSessionDigestSchema() error {
	if _, ok := sessionDigestSchemaReady.Load(d.DB); ok {
		return nil
	}
	// 依赖 sessions 表（外键目标）⇒ 先把它建出来。
	if err := d.EnsureSessionSchema(); err != nil {
		return fmt.Errorf("应用会话 schema 失败: %w", err)
	}
	if _, err := d.Exec(sessionDigestSchemaSQL); err != nil {
		return fmt.Errorf("应用收件条 schema 失败: %w", err)
	}
	sessionDigestSchemaReady.Store(d.DB, struct{}{})
	return nil
}

// ensureSessionDigestSchema 是访问层的幂等入口。
func (d *DB) ensureSessionDigestSchema() error { return d.EnsureSessionDigestSchema() }

// SessionDigestState 是一个会话的收件条水位线。
type SessionDigestState struct {
	SessionID         int64     `json:"session_id"`
	LastReadMessageID int64     `json:"last_read_message_id"`
	LastSeenAt        time.Time `json:"last_seen_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

// SessionDigestState 读收件条水位线（没有记录时返回零值，不报错）。
//
// 「没有记录」= 用户还没读过任何消息 = 水位线 0 ⇒ 摘要窗口是整个会话。
// 这不是错误状态，因此不返回 error 之外的信号。
func (d *DB) SessionDigestState(sessionID int64) (*SessionDigestState, error) {
	if err := d.ensureSessionDigestSchema(); err != nil {
		return nil, err
	}
	var s SessionDigestState
	err := d.QueryRow(`SELECT session_id, last_read_message_id, last_seen_at, updated_at
FROM session_digest_state WHERE session_id=$1`, sessionID).
		Scan(&s.SessionID, &s.LastReadMessageID, &s.LastSeenAt, &s.UpdatedAt)
	if err == sql.ErrNoRows {
		return &SessionDigestState{SessionID: sessionID}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读收件条水位线失败: %w", err)
	}
	return &s, nil
}

// MarkSessionDigestRead 把水位线推进到 messageID（只前进，不后退）。
//
// 「只前进」是硬语义：重复提交旧水位线不会把收件条退回旧窗口（否则用户会
// 反复看到同一段摘要）。水位线也不会超过该会话当前最大的消息 id —— 越界
// 说明调用方算错了，宁可少收也不给出空摘要。
func (d *DB) MarkSessionDigestRead(sessionID, messageID int64) (*SessionDigestState, error) {
	if err := d.ensureSessionDigestSchema(); err != nil {
		return nil, err
	}
	if messageID < 0 {
		messageID = 0
	}
	var latest int64
	if err := d.QueryRow(`SELECT COALESCE(MAX(id),0) FROM session_messages WHERE session_id=$1`,
		sessionID).Scan(&latest); err != nil {
		return nil, fmt.Errorf("查询会话最新消息失败: %w", err)
	}
	if messageID > latest {
		messageID = latest
	}
	_, err := d.Exec(`INSERT INTO session_digest_state(session_id, last_read_message_id, last_seen_at, updated_at)
VALUES ($1,$2, now(), now())
ON CONFLICT (session_id) DO UPDATE
  SET last_read_message_id = GREATEST(session_digest_state.last_read_message_id, EXCLUDED.last_read_message_id),
      last_seen_at = now(),
      updated_at = now()`, sessionID, messageID)
	if err != nil {
		return nil, fmt.Errorf("推进收件条水位线失败: %w", err)
	}
	return d.SessionDigestState(sessionID)
}

// SessionTimeline 读一个会话的**折叠时间线**（线程 + 收据条）。
//
// sinceMessageID <= 0 ⇒ 取全部消息（上限 sessionTimelineMaxMessages）。
// 这是「用户在线时看的那条流」—— 与收件条（用户不在时的那段）互补。
func (d *DB) SessionTimeline(sessionID, sinceMessageID int64, limit int) ([]SessionThread, *SessionDigest, error) {
	if err := d.ensureSessionDigestSchema(); err != nil {
		return nil, nil, err
	}
	if limit <= 0 || limit > sessionTimelineMaxMessages {
		limit = sessionTimelineMaxMessages
	}
	msgs, err := d.sessionMessagesSince(sessionID, sinceMessageID, limit)
	if err != nil {
		return nil, nil, err
	}
	threads := GroupSessionMessages(msgs)
	digest := BuildSessionDigest(sessionID, msgs, sinceMessageID)
	return threads, &digest, nil
}

// sessionTimelineMaxMessages 是折叠时间线单次读取的消息条数上限。
//
// 上限存在的原因是老问题：会话消息体是自由文本，没有上限的读取就是内存放大器。
const sessionTimelineMaxMessages = 500

// sessionMessagesSince 读会话消息（可按 id 下限过滤）。
//
// 与 db/session.go 的 SessionMessages 的区别：这里要能读「大于某个 id 的部分」
// 且能放宽条数上限（时间线要能看到折叠后的全部线程）。不新增导出 API，
// 免得两个几乎一样的读取器漂移。
func (d *DB) sessionMessagesSince(sessionID, sinceMessageID int64, limit int) ([]SessionMessage, error) {
	if limit <= 0 {
		limit = sessionTimelineMaxMessages
	}
	rows, err := d.Query(`SELECT `+sessionMessageCols+` FROM session_messages
WHERE session_id=$1 AND id > $2 ORDER BY id ASC LIMIT $3`, sessionID, sinceMessageID, limit)
	if err != nil {
		return nil, fmt.Errorf("读会话消息失败: %w", err)
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

// SessionDigestFor 读某个会话的收件条摘要。
//
// sinceMessageID < 0 ⇒ 用持久化的水位线（用户上次看到的地方）；
// sinceMessageID >= 0 ⇒ 显式指定（前端切换会话时按自己的游标请求）。
func (d *DB) SessionDigestFor(sessionID, sinceMessageID int64) (*SessionDigest, error) {
	if err := d.ensureSessionDigestSchema(); err != nil {
		return nil, err
	}
	since := sinceMessageID
	if since < 0 {
		st, err := d.SessionDigestState(sessionID)
		if err != nil {
			return nil, err
		}
		since = st.LastReadMessageID
	}
	msgs, err := d.sessionMessagesSince(sessionID, 0, sessionTimelineMaxMessages)
	if err != nil {
		return nil, err
	}
	digest := BuildSessionDigest(sessionID, msgs, since)
	return &digest, nil
}

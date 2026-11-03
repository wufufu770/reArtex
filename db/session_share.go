package db

// 跨会话共享上下文（cross-session shared context）。
//
// 依据：session-groupchat-proposal.md §9.1 第 10 条（定稿：跨 session 共享落在
// P5）+ §7 的 P5 行「收件条/线程折叠等噪音治理 + 跨 session 共享上下文」。
//
// ## 这一层到底允许什么（**窄范围**，逐条对应方案书的定稿）
//
// 方案书 §9.1 第 10 条的原话是：「跨 session 共享会同时复杂化三件事：权限边界、
// 额度归属、审计留痕 ⇒ 先把同 session 内的共享语义做干净，再谈跨 session」。
// 也就是说：P5 要做跨 session 共享，但**必须同时把三件事做对**。本文件把
// 「做对」落成三条可验证的约束：
//
//	① 权限边界：共享内容是**不可信输入**，不携带任何授权；能跨界的消息类型是
//	  **代码常量**白名单，DB 里的值只能收窄不能放宽。共享内容进不了任何权限门。
//	② 额度归属：**消费方付费**（内容进的是消费方的上下文窗口）；供给方既不被
//	  扣也不被加（额度不能跨会话洗）；消费方的群组预算处于冻结态时直接拒绝
//	  （fail-closed，见 db/quota.go 的 GroupQuotaState 契约）。
//	③ 审计留痕：授予、撤销、每次读取都进**审计哈希链**（db/audit_chain.go 的
//	  AppendAuditEvent），读取另外落一张账（session_context_reads）⇒ 「谁借了
//	  谁什么、借了多少 token」有确定答案。
//
// ## 与记忆层同款纪律（§8 红线）
//
// 「记忆层不是安全旁路：从记忆读入的内容一律当不可信输入 ⇒ 记忆不能扩大工具
// 权限，不能绕过 SoD 裁决」。跨会话共享内容是**另一个会话的产物**，同样是
// 不可信输入（可能来自工具输出、可能是被污染的经验）⇒ 同一套纪律：
// claims_authorization 在投影里被强制置 false；没有任何权限门读这份内容。
//
// 允许：SIZE_OK —— 白名单、准入、额度口径、审计写在一个文件里才看得清「谁能
// 看到什么、谁付钱、谁留痕」这三条是不是同一件事。

import (
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

//go:embed schema_session_share.sql
var sessionShareSchemaSQL string

// sessionShareSchemaReady 记录某个连接已经应用过共享上下文 DDL。
var sessionShareSchemaReady sync.Map

// EnsureSessionShareSchema 应用共享上下文 DDL（幂等）。
func (d *DB) EnsureSessionShareSchema() error {
	if _, ok := sessionShareSchemaReady.Load(d.DB); ok {
		return nil
	}
	if err := d.EnsureSessionSchema(); err != nil {
		return fmt.Errorf("应用会话 schema 失败: %w", err)
	}
	if _, err := d.Exec(sessionShareSchemaSQL); err != nil {
		return fmt.Errorf("应用共享上下文 schema 失败: %w", err)
	}
	sessionShareSchemaReady.Store(d.DB, struct{}{})
	return nil
}

func (d *DB) ensureSessionShareSchema() error { return d.EnsureSessionShareSchema() }

// ---------- 代码常量（安全边界不进 DB / settings）----------

// sessionShareableKinds 是**允许跨会话共享**的消息类型白名单。
//
// 为什么只有这三种：
//
//	note / answer / finding —— 事实陈述。它们不指名「谁该做什么」，跨界之后
//	  仍然只是一段背景，不会变成对消费方成员的指令。
//	request / escalation —— **行动契约**：它们在说「谁该去做」。跨会话之后
//	  「A 你去看一下」会被读成「消费方这边去看一下」⇒ 不跨界。
//	receipt —— 确认噪音，跨界没有任何信息价值。
//
// 这张表是**代码常量**：改 DB 里的 share_kinds 只能收窄它、放宽不了它
// （与 db/session.go 的 sessionAttachmentTypes 同款纪律）。
var sessionShareableKinds = map[string]bool{
	SessionMessageKindNote:    true,
	SessionMessageKindAnswer:  true,
	SessionMessageKindFinding: true,
}

// sessionShareMaxMessages 是单次共享读取的条数硬上限。
//
// 共享内容会进消费方的上下文窗口 ⇒ 条数就是 token 成本的直接放大器。上限写死
// 在代码里（不是 DB 可改值）⇒ 单次拉取的最坏成本有界。
const sessionShareMaxMessages = 50

// sessionShareMinMessages 是单次共享读取的条数下限（低于它没有意义）。
const sessionShareMinMessages = 1

// sessionShareCharsPerToken 是 token 估算口径：每 4 个字符约 1 个 token。
//
// 与 third_party/norma/compaction 的 chars/4 估算同源（方案书 §11.2 记的
// 「chars/4*4/3」是 SDK 的输入侧口径；这里只做**预算口径**的粗估，不需要
// 与 SDK 逐 token 精确对齐 —— 它只用来给额度归属记账）。
const sessionShareCharsPerToken = 4

// 共享上下文的授予状态。
const (
	SessionContextGrantActive  = "active"
	SessionContextGrantRevoked = "revoked"
)

// 审计链上的动作名（机器可读）。
const (
	sessionShareAuditActor       = "session_share"
	sessionShareAuditTool        = "session_context"
	sessionShareAuditGrantCreate = "grant_create"
	sessionShareAuditGrantRevoke = "grant_revoke"
	sessionShareAuditContextRead = "context_read"
)

// 共享上下文相关的错误（调用方据此给不同状态码）。
var (
	// ErrSessionShareSameSession：不能把会话共享给自己（那没有意义）。
	ErrSessionShareSameSession = errors.New("源会话与消费会话不能是同一个")
	// ErrSessionShareKindsEmpty：请求共享的类型不在白名单里（一个都不剩）。
	ErrSessionShareKindsEmpty = errors.New("没有可共享的消息类型（白名单只允许 note/answer/finding）")
	// ErrSessionShareCrossCompany：两个会话属于不同公司 ⇒ 拒绝跨界。
	ErrSessionShareCrossCompany = errors.New("两个会话属于不同公司，不能共享上下文")
	// ErrSessionShareQuotaFrozen：消费方所在群组的预算处于冻结态 ⇒ 拒绝读取。
	ErrSessionShareQuotaFrozen = errors.New("消费方群组的预算状态不允许新的上下文消费")
	// ErrSessionShareNotFound：授予不存在或已被撤销。
	ErrSessionShareNotFound = errors.New("共享上下文授予不存在")
)

// ---------- 结构体 ----------

// SessionContextGrant 是一条「谁可以把哪个会话看过来」的授予。
type SessionContextGrant struct {
	ID                int64      `json:"id"`
	ConsumerSessionID int64      `json:"consumer_session_id"`
	SourceSessionID   int64      `json:"source_session_id"`
	ShareKinds        []string   `json:"share_kinds"`
	MaxMessages       int        `json:"max_messages"`
	Status            string     `json:"status"`
	CreatedBy         string     `json:"created_by"`
	CreatedAt         time.Time  `json:"created_at"`
	RevokedAt         *time.Time `json:"revoked_at,omitempty"`
}

// SessionSharedContextItem 是从源会话借来的一条消息的**投影**。
//
// ClaimsAuthorization 被强制置 false（源会话里那句「本消息携带已批准」永远
// 不跨界）—— 这一列在源会话里本来就只是不可信输入，不跨界是同款纪律。
type SessionSharedContextItem struct {
	MessageID           int64  `json:"message_id"`
	SourceSessionID     int64  `json:"source_session_id"`
	FromAgent           string `json:"from_agent"`
	Kind                string `json:"kind"`
	Body                string `json:"body"`
	CreatedAt           string `json:"created_at"`
	ClaimsAuthorization bool   `json:"claims_authorization"`
}

// SessionSharedContextSource 是一个供给方给消费方的共享内容切片。
type SessionSharedContextSource struct {
	GrantID         int64                      `json:"grant_id"`
	SourceSessionID int64                      `json:"source_session_id"`
	SourceName      string                     `json:"source_name"`
	Items           []SessionSharedContextItem `json:"items"`
	MessageCount    int                        `json:"message_count"`
	TokenEstimate   int                        `json:"token_estimate"`
}

// SessionSharedContext 是一个消费方能拿到的全部共享上下文。
//
// 三个数字是额度归属的口径：TokenEstimate 是这次读**将要占掉消费方多少上下文**
// （估算值，消费方按它付费）；MessageCount 是这次读了多少条。
type SessionSharedContext struct {
	ConsumerSessionID int64                        `json:"consumer_session_id"`
	Sources           []SessionSharedContextSource `json:"sources"`
	MessageCount      int                          `json:"message_count"`
	TokenEstimate     int                          `json:"token_estimate"`
}

// ---------- 授予 ----------

// CreateSessionContextGrant 授予「消费方可以看供给方的上下文」。
//
// 准入（fail-closed，任意一条不满足就报错、不落库）：
//   - 消费方与供给方都存在、且不是同一个会话；
//   - 至少一种请求的类型落在代码白名单里（否则没有任何东西可共享）；
//   - 两个会话若都能解析出公司，则公司必须相同（跨公司拒绝）。
//
// 额度归属在这一层就定死：**消费方付费**。供给方不被扣也不被加。
func (d *DB) CreateSessionContextGrant(consumerID, sourceID int64, kinds []string, maxMessages int, createdBy string) (*SessionContextGrant, error) {
	if err := d.ensureSessionShareSchema(); err != nil {
		return nil, err
	}
	if consumerID <= 0 || sourceID <= 0 {
		return nil, errors.New("会话 id 不合法")
	}
	if consumerID == sourceID {
		return nil, ErrSessionShareSameSession
	}
	effective, err := EffectiveSessionShareKinds(kinds)
	if err != nil {
		return nil, err
	}
	if maxMessages <= 0 {
		maxMessages = sessionShareMaxMessages
	}
	if maxMessages > sessionShareMaxMessages {
		maxMessages = sessionShareMaxMessages
	}
	if maxMessages < sessionShareMinMessages {
		maxMessages = sessionShareMinMessages
	}

	consumer, err := d.GetSession(consumerID)
	if err != nil {
		return nil, fmt.Errorf("查询消费会话失败: %w", err)
	}
	if consumer == nil {
		return nil, errors.New("消费会话不存在")
	}
	source, err := d.GetSession(sourceID)
	if err != nil {
		return nil, fmt.Errorf("查询源会话失败: %w", err)
	}
	if source == nil {
		return nil, errors.New("源会话不存在")
	}
	ok, err := d.sessionsShareableAcrossCompany(consumerID, sourceID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrSessionShareCrossCompany
	}

	g := &SessionContextGrant{
		ConsumerSessionID: consumerID,
		SourceSessionID:   sourceID,
		ShareKinds:        effective,
		MaxMessages:       maxMessages,
		Status:            SessionContextGrantActive,
		CreatedBy:         strings.TrimSpace(createdBy),
	}
	var id int64
	err = d.QueryRow(`INSERT INTO session_context_grants(consumer_session_id, source_session_id,
  share_kinds, max_messages, status, created_by)
VALUES ($1,$2,$3,$4,$5,$6)
ON CONFLICT (consumer_session_id, source_session_id) DO UPDATE
  SET share_kinds = EXCLUDED.share_kinds,
      max_messages = EXCLUDED.max_messages,
      status = 'active',
      created_by = EXCLUDED.created_by,
      created_at = now(),
      revoked_at = NULL
RETURNING id`, consumerID, sourceID, strings.Join(effective, ","), maxMessages,
		SessionContextGrantActive, g.CreatedBy).Scan(&id)
	if err != nil {
		return nil, fmt.Errorf("写共享上下文授予失败: %w", err)
	}
	g.ID = id

	if err := d.appendSessionShareAudit(sessionShareAuditGrantCreate, g, nil); err != nil {
		return nil, err
	}
	return g, nil
}

// RevokeSessionContextGrant 撤销一条授予（不删行 —— 审计要看得见它曾经存在）。
func (d *DB) RevokeSessionContextGrant(consumerID, sourceID int64) error {
	if err := d.ensureSessionShareSchema(); err != nil {
		return err
	}
	res, err := d.Exec(`UPDATE session_context_grants SET status='revoked', revoked_at=now()
WHERE consumer_session_id=$1 AND source_session_id=$2 AND status='active'`, consumerID, sourceID)
	if err != nil {
		return fmt.Errorf("撤销共享上下文授予失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrSessionShareNotFound
	}
	g, err := d.SessionContextGrant(consumerID, sourceID)
	if err != nil {
		return err
	}
	return d.appendSessionShareAudit(sessionShareAuditGrantRevoke, g, nil)
}

// SessionContextGrant 读一条授予（不存在时返回 nil, nil）。
func (d *DB) SessionContextGrant(consumerID, sourceID int64) (*SessionContextGrant, error) {
	if err := d.ensureSessionShareSchema(); err != nil {
		return nil, err
	}
	row := d.QueryRow(`SELECT id, consumer_session_id, source_session_id, share_kinds, max_messages,
  status, created_by, created_at, revoked_at
FROM session_context_grants WHERE consumer_session_id=$1 AND source_session_id=$2`, consumerID, sourceID)
	g, err := scanSessionContextGrant(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读共享上下文授予失败: %w", err)
	}
	return g, nil
}

// ListSessionContextGrants 列一个消费方的全部授予（含已撤销的）。
func (d *DB) ListSessionContextGrants(consumerID int64) ([]SessionContextGrant, error) {
	if err := d.ensureSessionShareSchema(); err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT id, consumer_session_id, source_session_id, share_kinds, max_messages,
  status, created_by, created_at, revoked_at
FROM session_context_grants WHERE consumer_session_id=$1 ORDER BY created_at ASC, id ASC`, consumerID)
	if err != nil {
		return nil, fmt.Errorf("列共享上下文授予失败: %w", err)
	}
	defer rows.Close()
	out := []SessionContextGrant{}
	for rows.Next() {
		g, err := scanSessionContextGrant(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *g)
	}
	return out, rows.Err()
}

// scanSessionContextGrant 扫一行授予。
func scanSessionContextGrant(row interface{ Scan(...any) error }) (*SessionContextGrant, error) {
	var g SessionContextGrant
	var kinds string
	if err := row.Scan(&g.ID, &g.ConsumerSessionID, &g.SourceSessionID, &kinds, &g.MaxMessages,
		&g.Status, &g.CreatedBy, &g.CreatedAt, &g.RevokedAt); err != nil {
		return nil, err
	}
	g.ShareKinds = splitSessionShareKinds(kinds)
	return &g, nil
}

// EffectiveSessionShareKinds 算出真正生效的可共享类型。
//
// 口径：**请求 ∩ 代码白名单**。请求里写了白名单之外的类型不会报错（那只是
// 「没申请到」），但一个都没剩下 ⇒ 报错（fail-closed：不能建一条永远读不到
// 东西的授予）。
func EffectiveSessionShareKinds(requested []string) ([]string, error) {
	want := map[string]bool{}
	for _, k := range requested {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		want[k] = true
	}
	out := []string{}
	for _, k := range []string{
		SessionMessageKindNote, SessionMessageKindAnswer, SessionMessageKindFinding,
	} {
		if want[k] && sessionShareableKinds[k] {
			out = append(out, k)
		}
	}
	if len(out) == 0 {
		return nil, ErrSessionShareKindsEmpty
	}
	return out, nil
}

// splitSessionShareKinds 解析存库的逗号分隔类型列表。
func splitSessionShareKinds(raw string) []string {
	out := []string{}
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		out = append(out, part)
	}
	return out
}

// ---------- 读取 ----------

// SessionSharedContextFor 取一个消费方能读到的全部共享上下文，并**记账**。
//
// 三件事在这里同时发生（顺序不能换）：
//  1. 额度归属：先查消费方所在群组的预算状态；处于冻结态（draining/parked）
//     或状态读不出来 ⇒ 拒绝（fail-closed，见 db/quota.go 的 GroupQuotaState 契约）。
//  2. 权限边界：只取白名单类型、跳过被拦截（blocked）的消息、投影里把
//     claims_authorization 置 false。
//  3. 审计留痕：写 session_context_reads 一行 + 审计哈希链一条。
//
// 只读一层：供给方的内容**不再展开**它自己的授予 ⇒ 不会出现「A→B→C」的
// 链式扩散（那会让一条共享把整个会话图的内容拖进来）。
func (d *DB) SessionSharedContextFor(consumerID int64) (*SessionSharedContext, error) {
	return d.sessionSharedContext(consumerID, true)
}

// PreviewSessionSharedContext 预览共享上下文，**不记账**。
//
// 给界面用：看一眼不该算一次消费（否则额度归属与审计会被噪声淹没）。真正的
// 消费发生在 agent 读取时（SessionSharedContextFor），那里才记账。
func (d *DB) PreviewSessionSharedContext(consumerID int64) (*SessionSharedContext, error) {
	return d.sessionSharedContext(consumerID, false)
}

// sessionSharedContext 是共享上下文的统一实现；record 决定要不要记账。
func (d *DB) sessionSharedContext(consumerID int64, record bool) (*SessionSharedContext, error) {
	if err := d.ensureSessionShareSchema(); err != nil {
		return nil, err
	}
	grants, err := d.ListSessionContextGrants(consumerID)
	if err != nil {
		return nil, err
	}
	out := &SessionSharedContext{ConsumerSessionID: consumerID, Sources: []SessionSharedContextSource{}}

	active := []SessionContextGrant{}
	for _, g := range grants {
		if g.Status == SessionContextGrantActive {
			active = append(active, g)
		}
	}
	if len(active) == 0 {
		return out, nil
	}

	// ① 额度归属：消费方所在群组的预算状态闸门。
	if err := d.checkSessionShareQuotaGate(consumerID); err != nil {
		return nil, err
	}

	for _, g := range active {
		items, err := d.readSharedContextItems(g)
		if err != nil {
			return nil, err
		}
		if len(items) == 0 {
			continue
		}
		source := SessionSharedContextSource{
			GrantID:         g.ID,
			SourceSessionID: g.SourceSessionID,
			Items:           items,
			MessageCount:    len(items),
			TokenEstimate:   sessionShareTokenEstimate(items),
		}
		if s, err := d.GetSession(g.SourceSessionID); err == nil && s != nil {
			source.SourceName = sessionDisplayName(s)
		}
		out.Sources = append(out.Sources, source)
		out.MessageCount += source.MessageCount
		out.TokenEstimate += source.TokenEstimate
		if !record {
			continue
		}

		ids := make([]int64, 0, len(items))
		for _, it := range items {
			ids = append(ids, it.MessageID)
		}
		raw, err := json.Marshal(ids)
		if err != nil {
			return nil, fmt.Errorf("序列化共享消息 id 失败: %w", err)
		}
		if _, err := d.Exec(`INSERT INTO session_context_reads(grant_id, consumer_session_id,
  source_session_id, message_count, token_estimate, message_ids)
VALUES ($1,$2,$3,$4,$5,$6)`, g.ID, consumerID, g.SourceSessionID, len(items), source.TokenEstimate, raw); err != nil {
			return nil, fmt.Errorf("写共享上下文读取账失败: %w", err)
		}
		// ③ 审计留痕：每次读取都进哈希链。
		if err := d.appendSessionShareAudit(sessionShareAuditContextRead, &g, map[string]any{
			"message_count":  len(items),
			"token_estimate": source.TokenEstimate,
			"message_ids":    ids,
		}); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// readSharedContextItems 取一个授予下真正能跨界的那批消息。
func (d *DB) readSharedContextItems(g SessionContextGrant) ([]SessionSharedContextItem, error) {
	limit := g.MaxMessages
	if limit <= 0 {
		limit = sessionShareMaxMessages
	}
	if limit > sessionShareMaxMessages {
		limit = sessionShareMaxMessages
	}
	kinds := map[string]bool{}
	for _, k := range g.ShareKinds {
		if sessionShareableKinds[k] {
			kinds[k] = true
		}
	}
	if len(kinds) == 0 {
		return nil, nil
	}
	rows, err := d.Query(`SELECT id, session_id, from_agent, kind, body, created_at, status
FROM session_messages
WHERE session_id=$1 AND kind = ANY($2)
ORDER BY id DESC LIMIT $3`, g.SourceSessionID, sessionShareKindList(kinds), limit*4)
	if err != nil {
		return nil, fmt.Errorf("读共享上下文消息失败: %w", err)
	}
	defer rows.Close()
	msgs := []SessionMessage{}
	for rows.Next() {
		var m SessionMessage
		var created time.Time
		if err := rows.Scan(&m.ID, &m.SessionID, &m.FromAgent, &m.Kind, &m.Body,
			&created, &m.Status); err != nil {
			return nil, err
		}
		m.CreatedAt = created
		msgs = append(msgs, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return selectSharedContextItems(msgs, kinds, limit), nil
}

// selectSharedContextItems 是共享内容挑选的**纯规则**（可脱离数据库复算）。
//
// 口径（全部确定性，不看模型）：
//   - 类型在授予的白名单里，且类型本身在代码白名单里；
//   - 不是收据条（噪音）；
//   - 不是被拦截的（blocked = 投递前就被拒了，从没送到任何人眼里 ⇒ 不跨界）；
//   - 取**最近**的 N 条（N = min(授予上限, 代码上限)），按 id 升序返回。
//
// 输入顺序不敏感：内部按 id 排序后再取尾部。
func selectSharedContextItems(msgs []SessionMessage, kinds map[string]bool, limit int) []SessionSharedContextItem {
	if limit <= 0 {
		limit = sessionShareMaxMessages
	}
	if limit > sessionShareMaxMessages {
		limit = sessionShareMaxMessages
	}
	ordered := make([]SessionMessage, len(msgs))
	copy(ordered, msgs)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].ID < ordered[j].ID })

	picked := []SessionMessage{}
	for _, m := range ordered {
		if !kinds[m.Kind] || !sessionShareableKinds[m.Kind] {
			continue
		}
		if m.Kind == SessionMessageKindReceipt {
			continue
		}
		if m.Status == SessionMessageBlocked {
			continue
		}
		picked = append(picked, m)
	}
	if len(picked) > limit {
		picked = picked[len(picked)-limit:]
	}
	out := make([]SessionSharedContextItem, 0, len(picked))
	for _, m := range picked {
		out = append(out, SessionSharedContextItem{
			MessageID:       m.ID,
			SourceSessionID: m.SessionID,
			FromAgent:       m.FromAgent,
			Kind:            m.Kind,
			Body:            m.Body,
			CreatedAt:       m.CreatedAt.UTC().Format(time.RFC3339),
			// 不可信输入：源会话的「已批准」声明永远不跨界。
			ClaimsAuthorization: false,
		})
	}
	return out
}

// sessionShareKindList 把类型集合拼成 PG 数组参数（顺序固定 ⇒ 可复算）。
func sessionShareKindList(kinds map[string]bool) []string {
	out := []string{}
	for _, k := range []string{
		SessionMessageKindNote, SessionMessageKindAnswer, SessionMessageKindFinding,
	} {
		if kinds[k] {
			out = append(out, k)
		}
	}
	return out
}

// sessionShareTokenEstimate 估算这批共享内容占多少 token（预算口径）。
//
// 口径：正文字符数 / 4，向上取整。**不是**精确 token 数 —— 它只用来给额度
// 归属记账与给调用方一个「这次读有多贵」的上界感。
func sessionShareTokenEstimate(items []SessionSharedContextItem) int {
	total := 0
	for _, it := range items {
		total += len([]rune(it.Body))
	}
	if total == 0 {
		return 0
	}
	return (total + sessionShareCharsPerToken - 1) / sessionShareCharsPerToken
}

// checkSessionShareQuotaGate 是额度归属那一侧的闸门。
//
// 口径：消费方所在群组的预算状态处于 draining/parked ⇒ 拒绝；状态读不出来 ⇒
// 也拒绝（fail-closed，见 db/quota.go 的 GroupQuotaState 的契约注释）。
//
// 消费方不挂任何群组（普通聊天会话）⇒ 没有额度边界 ⇒ 放行。
func (d *DB) checkSessionShareQuotaGate(consumerID int64) error {
	groupID, err := d.sessionChatGroupID(consumerID)
	if err != nil {
		return err
	}
	if groupID <= 0 {
		return nil
	}
	state, err := d.GroupQuotaState(groupID)
	if err != nil {
		return fmt.Errorf("查询消费方群组预算状态失败（按拒绝处理）: %w", err)
	}
	if QuotaStateDeniesNewCalls(state) {
		return ErrSessionShareQuotaFrozen
	}
	return nil
}

// sessionChatGroupID 解析会话所属的群组 id（没有则 0）。
//
// 会话 ⇒ 群组的映射走 chat_groups.session_id（db/schema_session.sql 加的列）。
func (d *DB) sessionChatGroupID(sessionID int64) (int64, error) {
	var id int64
	err := d.QueryRow(`SELECT id FROM chat_groups WHERE session_id=$1 ORDER BY id LIMIT 1`,
		sessionID).Scan(&id)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("查询会话所属群组失败: %w", err)
	}
	return id, nil
}

// sessionsShareableAcrossCompany 判定两个会话能不能共享上下文（公司维度）。
//
// 口径：**能解析出公司时才判**。两个会话都挂任务 ⇒ 任务的公司必须相同；
// 有一个没挂任务 ⇒ 它没有公司归属 ⇒ 只能按「未归属」处理。
//
// ⚠️ 这个口径是**已知的不完备**：sessions 表本身没有公司列（公司维度在 tasks /
// assets 上），所以「不挂任务的纯聊天会话」没有租户身份可言。平台是单租户
// 部署，跨公司共享不是现实威胁；但这一条会写进审计载荷（tenant_check 字段）
// ⇒ 事后能看出哪些共享发生在「未归属」判定上。
func (d *DB) sessionsShareableAcrossCompany(consumerID, sourceID int64) (bool, error) {
	c, err := d.sessionCompanyID(consumerID)
	if err != nil {
		return false, err
	}
	s, err := d.sessionCompanyID(sourceID)
	if err != nil {
		return false, err
	}
	if c == nil || s == nil {
		return true, nil
	}
	return *c == *s, nil
}

// sessionCompanyID 解析会话所属的公司（挂任务 ⇒ 任务的公司；否则 nil）。
func (d *DB) sessionCompanyID(sessionID int64) (*int64, error) {
	var taskID *int64
	if err := d.QueryRow(`SELECT task_id FROM sessions WHERE id=$1`, sessionID).Scan(&taskID); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("查询会话任务失败: %w", err)
	}
	if taskID == nil {
		return nil, nil
	}
	var companyID *int64
	if err := d.QueryRow(`SELECT company_id FROM tasks WHERE id=$1`, *taskID).Scan(&companyID); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("查询任务公司失败: %w", err)
	}
	return companyID, nil
}

// sessionDisplayName 给会话一个展示名（用户显式名 > 生成标题 > 空）。
func sessionDisplayName(s *Session) string {
	if s == nil {
		return ""
	}
	if strings.TrimSpace(s.Name) != "" {
		return s.Name
	}
	return strings.TrimSpace(s.Title)
}

// ---------- 审计 ----------

// appendSessionShareAudit 把共享上下文的一次动作写进审计哈希链。
//
// 用 db/audit_chain.go 的 AppendAuditEvent（平台自己的链）而不是另起一张表：
// 共享上下文的授予/撤销/读取都要经得起「事后有没有被动过手脚」的追问。
func (d *DB) appendSessionShareAudit(action string, g *SessionContextGrant, extra map[string]any) error {
	payload := map[string]any{}
	if g != nil {
		payload["grant_id"] = g.ID
		payload["consumer_session_id"] = g.ConsumerSessionID
		payload["source_session_id"] = g.SourceSessionID
		payload["share_kinds"] = g.ShareKinds
		payload["max_messages"] = g.MaxMessages
		payload["created_by"] = g.CreatedBy
	}
	for k, v := range extra {
		payload[k] = v
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("序列化共享上下文审计载荷失败: %w", err)
	}
	if _, err := d.AppendAuditEvent(AuditEvent{
		Actor:    sessionShareAuditActor,
		ToolName: sessionShareAuditTool,
		Action:   action,
		Reason:   "跨会话共享上下文",
		Payload:  raw,
	}); err != nil {
		return fmt.Errorf("写共享上下文审计失败: %w", err)
	}
	return nil
}

// SessionIDForConversation 解析旧对话 id 对应的会话 id。
//
// 迁移回填后每个旧对话背后各有一个会话（db/session.go 的 backfillSessions），
// 而 agent 侧手上只有**对话 id**（RunInfo.SessionID 是对话 id 的字符串）⇒
// 这一层是「对话 id ⇒ 会话 id」的桥。没有对应会话时返回 0（不是错误：
// 还没迁移的老对话就是这样）。
func (d *DB) SessionIDForConversation(conversationID int64) (int64, error) {
	if conversationID <= 0 {
		return 0, nil
	}
	var sessionID *int64
	err := d.QueryRow(`SELECT session_id FROM conversations WHERE id=$1`, conversationID).Scan(&sessionID)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("查询对话所属会话失败: %w", err)
	}
	if sessionID == nil {
		return 0, nil
	}
	return *sessionID, nil
}

// SharedContextForConversation 按**对话 id**取共享上下文（agent 侧的入口）。
//
// agent 手上只有对话 id（RunInfo.SessionID）⇒ 这一层把它解析成会话 id 再读。
// 没有对应会话 ⇒ 返回空上下文（不是错误）。
func (d *DB) SharedContextForConversation(conversationID int64) (*SessionSharedContext, error) {
	sessionID, err := d.SessionIDForConversation(conversationID)
	if err != nil {
		return nil, err
	}
	if sessionID <= 0 {
		return &SessionSharedContext{Sources: []SessionSharedContextSource{}}, nil
	}
	return d.SessionSharedContextFor(sessionID)
}

// ---------- 读取账 ----------
// SessionContextRead 是共享上下文读取账的一行（额度归属与审计留痕的落点）。
type SessionContextRead struct {
	ID                int64     `json:"id"`
	GrantID           int64     `json:"grant_id"`
	ConsumerSessionID int64     `json:"consumer_session_id"`
	SourceSessionID   int64     `json:"source_session_id"`
	MessageCount      int       `json:"message_count"`
	TokenEstimate     int       `json:"token_estimate"`
	MessageIDs        []int64   `json:"message_ids"`
	ReadAt            time.Time `json:"read_at"`
}

// ListSessionContextReads 列读取账（可按消费方或供给方过滤）。
//
// 这个查询就是额度归属问题的答案：「B 从 A 借了多少 token」。
func (d *DB) ListSessionContextReads(consumerID, sourceID int64, limit int) ([]SessionContextRead, error) {
	if err := d.ensureSessionShareSchema(); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	q := `SELECT id, grant_id, consumer_session_id, source_session_id, message_count, token_estimate,
  message_ids, read_at FROM session_context_reads WHERE 1=1`
	args := []any{}
	if consumerID > 0 {
		args = append(args, consumerID)
		q += fmt.Sprintf(" AND consumer_session_id=$%d", len(args))
	}
	if sourceID > 0 {
		args = append(args, sourceID)
		q += fmt.Sprintf(" AND source_session_id=$%d", len(args))
	}
	args = append(args, limit)
	q += fmt.Sprintf(" ORDER BY read_at DESC, id DESC LIMIT $%d", len(args))
	rows, err := d.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("读共享上下文读取账失败: %w", err)
	}
	defer rows.Close()
	out := []SessionContextRead{}
	for rows.Next() {
		var r SessionContextRead
		var ids []byte
		if err := rows.Scan(&r.ID, &r.GrantID, &r.ConsumerSessionID, &r.SourceSessionID,
			&r.MessageCount, &r.TokenEstimate, &ids, &r.ReadAt); err != nil {
			return nil, err
		}
		r.MessageIDs = []int64{}
		if len(ids) > 0 {
			_ = json.Unmarshal(ids, &r.MessageIDs)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SessionContextTokenTotals 汇总「消费方从供给方借了多少 token」。
//
// 返回的 map 键是 "consumer:source" ⇒ 额度归属与「谁影响了谁」一次问完。
func (d *DB) SessionContextTokenTotals() (map[string]int, error) {
	if err := d.ensureSessionShareSchema(); err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT consumer_session_id, source_session_id, SUM(token_estimate)
FROM session_context_reads GROUP BY consumer_session_id, source_session_id`)
	if err != nil {
		return nil, fmt.Errorf("汇总共享上下文用量失败: %w", err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var c, s int64
		var total int64
		if err := rows.Scan(&c, &s, &total); err != nil {
			return nil, err
		}
		out[fmt.Sprintf("%d:%d", c, s)] = int(total)
	}
	return out, rows.Err()
}

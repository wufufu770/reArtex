package server

// 会话（sessions）HTTP 出口 —— proposal §4.5「API 变更」。
//
// 会话是新的第一类对象：一个 session = 一群聊（人 + N 个 agent 同一条消息流）。
// 数据层在 db/session.go（sessions / session_members / session_attachments /
// session_messages 四张表）；本文件只做三件事：解析入参、调数据层、把结果
// 落成前端能直接吃的形状。**所有判定都在数据层**（准入判定、白名单、幂等）
// —— 本文件不复制任何一条业务规则，只负责把「客户端给错了」判成 400、
// 「东西不存在」判成 404。
//
// 路由注册不在本文件：server/server.go 的 mux 注册表是别的单元的文件域（同一
// worktree 内并行开发）。本单元只提供 handler 与这个一次性注册入口 —— 属主在
// 注册区调一次 s.RegisterSessionRoutes(mux) 即可（与 server/task_hall.go、
// server/chat_group.go 同款纪律）。

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/Autumn-27/artex/db"
)

const (
	// maxSessionRequestBytes 是会话接口的请求体上限。
	//
	// 为什么限长：会话消息正文与关联载荷都是自由文本 ⇒ 没有上限的请求体就是
	// 一个内存放大器（与 server/task_metadata.go 同款纪律）。
	maxSessionRequestBytes = 64 << 10
	// sessionMessageListLimit 是会话消息单页的缺省条数（与 db 层的默认一致）。
	sessionMessageListLimit = 200
)

// errSessionNotFound 是「会话不存在」的内部标记（对外是 404）。
var errSessionNotFound = errors.New("会话不存在")

// sessionKinds 是会话类型的白名单。
//
// 镜像 db/session.go 的同名私有表，但**用导出的常量拼**⇒ 两边不可能漂移
// （db 那份也是用同一组常量拼的）。
var sessionKinds = map[string]bool{
	db.SessionKindChat:         true,
	db.SessionKindTaskHall:     true,
	db.SessionKindSideQuestion: true,
}

// sessionRoles 是成员分工的白名单（同样用导出的常量拼）。
//
// 分工**不表达权限**（与 db/session.go 同款纪律）：role 只说明「这个 agent 在
// 会话里干什么」，不是「它能做什么」。
var sessionRoles = map[string]bool{
	db.SessionRoleDispatcher: true,
	db.SessionRoleMember:     true,
	db.SessionRoleObserver:   true,
}

// sessionPresences 是在场状态的白名单（用导出的常量拼）。
var sessionPresences = map[string]bool{
	db.SessionPresenceIdle:      true,
	db.SessionPresenceListening: true,
	db.SessionPresenceWorking:   true,
	db.SessionPresenceBlocked:   true,
	db.SessionPresenceLeft:      true,
}

// sessionMessageKinds 是消息类型的白名单（用导出的常量拼）。
var sessionMessageKinds = map[string]bool{
	db.SessionMessageKindNote:       true,
	db.SessionMessageKindRequest:    true,
	db.SessionMessageKindAnswer:     true,
	db.SessionMessageKindFinding:    true,
	db.SessionMessageKindEscalation: true,
	db.SessionMessageKindReceipt:    true,
}

// sessionReceiptActions 是收据动作的白名单（用导出的常量拼）。
var sessionReceiptActions = map[string]bool{
	db.SessionReceiptTaken:    true,
	db.SessionReceiptNoted:    true,
	db.SessionReceiptDeferred: true,
}

// sessionView 是会话的对外视图：本体 + 在席名册 + 关联工件。
//
// 为什么把名册和关联**内联**在会话视图里（而不是让前端二次请求）：proposal
// §4.6 的侧边栏每行都要显示「agent 名册 + 状态点 + 关联工件小图标」，会话页
// 也要显示参与 agent 的 presence ⇒ 一次拿全，前端不再 N+1。
//
// 也正因为名册在列表项里，§9 约束④的「按 agent 过滤」可以纯前端做（共用同一
// 份列表数据，不额外请求）⇒ 本文件**不**提供按 agent 过滤的查询参数。
type sessionView struct {
	*db.Session
	Members     []db.SessionMember     `json:"members"`
	Attachments []db.SessionAttachment `json:"attachments"`
	// AgentCount 是在席成员数（退席的不算）。
	AgentCount int `json:"agent_count"`
}

// sessionPresenceItem 是「成员在场状态」的一条对外形状。
type sessionPresenceItem struct {
	AgentKey   string `json:"agent_key"`
	Role       string `json:"role"`
	Presence   string `json:"presence"`
	ETAMinutes *int   `json:"eta_minutes,omitempty"`
	BlockedOn  string `json:"blocked_on,omitempty"`
	HasLeft    bool   `json:"has_left"`
	JoinedAt   string `json:"joined_at"`
	LeftAt     string `json:"left_at,omitempty"`
}

// RegisterSessionRoutes 挂上会话路由（proposal §4.5）。
//
// 为什么要单独一个注册函数而不是把 mux.HandleFunc 写在 server.go 里：路由表是
// 别的单元的文件域（同一 worktree 内并行开发），本单元只能提供 handler 与这个
// 一次性注册入口 —— 属主在注册区调一次即可。
func (s *Server) RegisterSessionRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/sessions", s.listSessions)
	mux.HandleFunc("POST /api/sessions", s.createSession)
	mux.HandleFunc("GET /api/sessions/{id}", s.getSession)
	mux.HandleFunc("PATCH /api/sessions/{id}", s.patchSession)
	mux.HandleFunc("DELETE /api/sessions/{id}", s.deleteSession)
	mux.HandleFunc("GET /api/sessions/{id}/messages", s.listSessionMessages)
	mux.HandleFunc("POST /api/sessions/{id}/messages", s.postSessionMessage)
	mux.HandleFunc("POST /api/sessions/{id}/members", s.addSessionMember)
	mux.HandleFunc("PATCH /api/sessions/{id}/members/{agent}", s.patchSessionMember)
	mux.HandleFunc("DELETE /api/sessions/{id}/members/{agent}", s.deleteSessionMember)
	mux.HandleFunc("GET /api/sessions/{id}/presence", s.getSessionPresence)
	mux.HandleFunc("GET /api/sessions/attachments/{type}/{key}/sessions", s.listSessionsByAttachment)
}

// decodeSessionRequest 读请求体（限长）。
//
// 复用 server/server_mgmt.go 的 decode，只在外面加一层体积上限。
func decodeSessionRequest(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxSessionRequestBytes)
	if err := decode(r, v); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeErr(w, http.StatusRequestEntityTooLarge, "请求正文过大")
		} else {
			writeErr(w, http.StatusBadRequest, "请求体解析失败: "+err.Error())
		}
		return false
	}
	return true
}

// sessionViewOf 组装一个会话视图。
//
// 空切片统一成 [] 而不是 null：前端不必为「没有成员」写一套判空分支。
func sessionViewOf(sess *db.Session, members []db.SessionMember, attachments []db.SessionAttachment) sessionView {
	if members == nil {
		members = []db.SessionMember{}
	}
	if attachments == nil {
		attachments = []db.SessionAttachment{}
	}
	inSeat := 0
	for _, m := range members {
		if !m.HasLeft {
			inSeat++
		}
	}
	return sessionView{Session: sess, Members: members, Attachments: attachments, AgentCount: inSeat}
}

// sessionViewOfID 读一个会话的完整视图（本体 + 名册 + 关联）。
func sessionViewOfID(pg *db.DB, id int64) (sessionView, error) {
	sess, err := pg.GetSession(id)
	if err != nil {
		return sessionView{}, err
	}
	if sess == nil {
		return sessionView{}, errSessionNotFound
	}
	members, err := pg.SessionMembers(id)
	if err != nil {
		return sessionView{}, err
	}
	attachments, err := pg.SessionAttachments(id)
	if err != nil {
		return sessionView{}, err
	}
	return sessionViewOf(sess, members, attachments), nil
}

// listSessions 列会话（扁平列表：按 updated_at，附 agent 名册、状态、关联摘要）。
//
// ?kind=chat|task_hall|side_question 可按类型筛（空 = 全部）。
func (s *Server) listSessions(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	sessions, err := pg.ListSessions(strings.TrimSpace(r.URL.Query().Get("kind")))
	if err != nil {
		writeErr(w, 500, "读会话列表失败: "+err.Error())
		return
	}
	ids := make([]int64, 0, len(sessions))
	for _, sess := range sessions {
		ids = append(ids, sess.ID)
	}
	// 名册与关联各一次批量读 ⇒ 不按会话逐个查（避免 N+1）。
	roster, err := pg.SessionRoster(ids)
	if err != nil {
		writeErr(w, 500, "读会话名册失败: "+err.Error())
		return
	}
	attachments, err := pg.SessionAttachmentsBySession(ids)
	if err != nil {
		writeErr(w, 500, "读会话关联失败: "+err.Error())
		return
	}
	views := make([]sessionView, 0, len(sessions))
	for i := range sessions {
		id := sessions[i].ID
		views = append(views, sessionViewOf(&sessions[i], roster[id], attachments[id]))
	}
	writeJSON(w, 200, map[string]any{"sessions": views, "count": len(views)})
}

// createSession 建会话。
//
// 请求体：name? / title? / kind? / task_id? / strategy_id? / asset_id? /
// agents[]? / dispatcher? / attachments[]?
//
// 默认只带 auto 一个成员（dispatcher）（§9 决策②：单 agent 起步最省额度）。
//
// 失败语义（先校验、后落库）：agent 是否存在、类型是否合法都在**建会话之前**
// 查清 ⇒ 任何一个不过 ⇒ 什么都不创建。建完会话之后如果成员或关联没落全 ⇒ 把
// 这个会话删掉（半个会话比没有会话更糟：它占住名额，重试时会撞冲突）。
func (s *Server) createSession(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	var req struct {
		Name        string                     `json:"name"`
		Title       string                     `json:"title"`
		Kind        string                     `json:"kind"`
		TaskID      *int64                     `json:"task_id"`
		StrategyID  *int64                     `json:"strategy_id"`
		AssetID     *int64                     `json:"asset_id"`
		Agents      []string                   `json:"agents"`
		Dispatcher  string                     `json:"dispatcher"`
		Attachments []db.SessionAttachmentSpec `json:"attachments"`
	}
	if !decodeSessionRequest(w, r, &req) {
		return
	}
	kind := strings.TrimSpace(req.Kind)
	if kind == "" {
		kind = db.SessionKindChat
	}
	if !sessionKinds[kind] {
		writeErr(w, 400, "会话类型只能是 chat/task_hall/side_question")
		return
	}
	members, err := sessionMembersFromRequest(pg, req.Agents, req.Dispatcher)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	sess := &db.Session{
		Name:       strings.TrimSpace(req.Name),
		Title:      strings.TrimSpace(req.Title),
		Kind:       kind,
		TaskID:     req.TaskID,
		StrategyID: req.StrategyID,
		AssetID:    req.AssetID,
	}
	id, err := pg.CreateSession(sess)
	if err != nil {
		writeErr(w, 500, "建会话失败: "+err.Error())
		return
	}
	rollback := true
	defer func() {
		if rollback {
			_ = pg.DeleteSession(id)
		}
	}()
	for _, m := range members {
		if err := pg.AddSessionMember(id, m.AgentKey, m.Role, m.Presence); err != nil {
			writeErr(w, 400, "加会话成员失败: "+err.Error())
			return
		}
	}
	for _, spec := range req.Attachments {
		if err := pg.AddSessionAttachment(id, spec); err != nil {
			writeErr(w, 400, "加会话关联失败: "+err.Error())
			return
		}
	}
	rollback = false
	view, err := sessionViewOfID(pg, id)
	if err != nil {
		writeErr(w, 500, "读会话失败: "+err.Error())
		return
	}
	writeJSON(w, 201, map[string]any{"session": view})
}

// sessionMemberSpec 是一个待入席成员的形状。
type sessionMemberSpec struct {
	AgentKey string `json:"agent_key"`
	Role     string `json:"role"`
	Presence string `json:"presence"`
}

// sessionMembersFromRequest 归一「建会话时带哪些 agent」。
//
// 规则（§9 决策② + 约束②）：
//   - dispatcher 指定分发者（缺省 auto），它拿 role='dispatcher'；
//   - 其余成员拿 role='member'；
//   - 每个 agent 必须已登记（session_members.agent_key 有外键）⇒ 不存在的
//     agent 在**建会话之前**就报错，不会留下半个会话。
func sessionMembersFromRequest(pg *db.DB, rawAgents []string, rawDispatcher string) ([]sessionMemberSpec, error) {
	dispatcher := strings.TrimSpace(rawDispatcher)
	if dispatcher == "" {
		dispatcher = "auto"
	}
	keys := []string{dispatcher}
	seen := map[string]bool{dispatcher: true}
	for _, raw := range rawAgents {
		key := strings.TrimSpace(raw)
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		keys = append(keys, key)
	}
	out := make([]sessionMemberSpec, 0, len(keys))
	for _, key := range keys {
		a, err := pg.GetAgentByKey(key)
		if err != nil {
			return nil, fmt.Errorf("查 agent 失败: %w", err)
		}
		if a == nil {
			return nil, fmt.Errorf("agent 不存在: %s", key)
		}
		role := db.SessionRoleMember
		if key == dispatcher {
			role = db.SessionRoleDispatcher
		}
		out = append(out, sessionMemberSpec{AgentKey: key, Role: role})
	}
	return out, nil
}

// getSession 读会话详情（成员、presence、关联、状态）。
func (s *Server) getSession(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, ok := pathInt(r, "id")
	if !ok {
		writeErr(w, 400, "会话 id 不合法")
		return
	}
	view, err := sessionViewOfID(pg, id)
	if errors.Is(err, errSessionNotFound) {
		writeErr(w, 404, "会话不存在")
		return
	}
	if err != nil {
		writeErr(w, 500, "读会话失败: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"session": view})
}

// patchSession 改会话（改名 / 改生成标题 / 钉住 / 关联增删）。
//
// 请求体里没给的字段一律不动（指针为 nil = 不改）。pinned 显式给 false = 取消
// 钉住（不传 = 不动）⇒ 这里必须用 *bool 而不是 bool。
func (s *Server) patchSession(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, ok := pathInt(r, "id")
	if !ok {
		writeErr(w, 400, "会话 id 不合法")
		return
	}
	var req struct {
		Name              *string                    `json:"name"`
		Title             *string                    `json:"title"`
		Pinned            *bool                      `json:"pinned"`
		AddAttachments    []db.SessionAttachmentSpec `json:"add_attachments"`
		RemoveAttachments []db.SessionAttachmentSpec `json:"remove_attachments"`
	}
	if !decodeSessionRequest(w, r, &req) {
		return
	}
	updated, err := pg.UpdateSession(id, db.SessionPatch{
		Name:              req.Name,
		Title:             req.Title,
		Pinned:            req.Pinned,
		AddAttachments:    req.AddAttachments,
		RemoveAttachments: req.RemoveAttachments,
	})
	if err != nil {
		writeErr(w, 400, "改会话失败: "+err.Error())
		return
	}
	if updated == nil {
		writeErr(w, 404, "会话不存在")
		return
	}
	writeJSON(w, 200, map[string]any{"session": updated})
}

// deleteSession 删会话（成员、关联、消息随外键级联删除）。
func (s *Server) deleteSession(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, ok := pathInt(r, "id")
	if !ok {
		writeErr(w, 400, "会话 id 不合法")
		return
	}
	sess, err := pg.GetSession(id)
	if err != nil {
		writeErr(w, 500, "读会话失败: "+err.Error())
		return
	}
	if sess == nil {
		writeErr(w, 404, "会话不存在")
		return
	}
	if err := pg.DeleteSession(id); err != nil {
		writeErr(w, 500, "删会话失败: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"deleted": true, "id": id})
}

// listSessionMessages 读会话消息流（含 agent 消息、收据条、线程回复）。
//
// ?limit= 限单页条数（缺省 200，超出按缺省截）。
func (s *Server) listSessionMessages(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, ok := pathInt(r, "id")
	if !ok {
		writeErr(w, 400, "会话 id 不合法")
		return
	}
	sess, err := pg.GetSession(id)
	if err != nil {
		writeErr(w, 500, "读会话失败: "+err.Error())
		return
	}
	if sess == nil {
		writeErr(w, 404, "会话不存在")
		return
	}
	limit := atoiDefault(r.URL.Query().Get("limit"), sessionMessageListLimit)
	if limit <= 0 || limit > sessionMessageListLimit {
		limit = sessionMessageListLimit
	}
	msgs, err := pg.SessionMessages(id, limit)
	if err != nil {
		writeErr(w, 500, "读会话消息失败: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"messages": msgs, "count": len(msgs)})
}

// postSessionMessage 往会话消息流写一条消息。
//
// 请求体：from_agent?（空 = 人）/ to_agent?（空 = 广播给全会话）/ kind? / body /
// parent_message_id? / task_id? / intent_id? / finding_id? / receipt_of? /
// receipt_action?
//
// 【投递前的准入判定在存储层】（db.AddSessionMessage）：收件人不是成员、消息
// 携带的 task/intent/finding 不属于本会话、原消息不在本会话 ⇒ 消息**照落库**
// 但标成 blocked 并带 blocked_reason，不进入接收方上下文。响应里带 status /
// blocked_reason，让调用方看得见（与 server/chat_group.go 同款处置）。
//
// 【唤醒与自取必须分开】（§9 约束①）：本出口只**报告**唤醒集合（wake_agents =
// 正文里被 @ 到、且仍在席的成员），**不**代为唤醒 —— 唤醒路径与「自取」路径
// 要分开实现、分开计量。
//
// 【claims_authorization 不开放】：那一列只表示「发送方声称本消息携带某个已
// 批准」，永远是不可信输入，没有任何权限门读它（db/session.go 同款纪律）⇒
// HTTP 出口不提供这个入参：写不出，就没人能依赖它。
func (s *Server) postSessionMessage(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, ok := pathInt(r, "id")
	if !ok {
		writeErr(w, 400, "会话 id 不合法")
		return
	}
	var req struct {
		FromAgent       string `json:"from_agent"`
		ToAgent         string `json:"to_agent"`
		Kind            string `json:"kind"`
		Body            string `json:"body"`
		ParentMessageID *int64 `json:"parent_message_id"`
		TaskID          *int64 `json:"task_id"`
		IntentID        *int64 `json:"intent_id"`
		FindingID       *int64 `json:"finding_id"`
		ReceiptOf       *int64 `json:"receipt_of"`
		ReceiptAction   string `json:"receipt_action"`
	}
	if !decodeSessionRequest(w, r, &req) {
		return
	}
	kind := strings.TrimSpace(req.Kind)
	if kind == "" {
		kind = db.SessionMessageKindNote
	}
	if !sessionMessageKinds[kind] {
		writeErr(w, 400, "消息类型只能是 note/request/answer/finding/escalation/receipt")
		return
	}
	action := strings.TrimSpace(req.ReceiptAction)
	if action != "" && !sessionReceiptActions[action] {
		writeErr(w, 400, "收据动作只能是 taken/noted/deferred")
		return
	}
	sess, err := pg.GetSession(id)
	if err != nil {
		writeErr(w, 500, "读会话失败: "+err.Error())
		return
	}
	if sess == nil {
		writeErr(w, 404, "会话不存在")
		return
	}
	// 名册先读：唤醒集合要在写消息**之前**算好（读不到名册就不写消息 ——
	// 否则会出现「消息写进去了但不知道该叫醒谁」的半截状态）。
	members, err := pg.SessionMembers(id)
	if err != nil {
		writeErr(w, 500, "读会话成员失败: "+err.Error())
		return
	}
	msg := &db.SessionMessage{
		SessionID:       id,
		FromAgent:       strings.TrimSpace(req.FromAgent),
		ToAgent:         strings.TrimSpace(req.ToAgent),
		Kind:            kind,
		Body:            strings.TrimSpace(req.Body),
		ParentMessageID: req.ParentMessageID,
		TaskID:          req.TaskID,
		IntentID:        req.IntentID,
		FindingID:       req.FindingID,
		ReceiptOf:       req.ReceiptOf,
		ReceiptAction:   action,
	}
	if _, err := pg.AddSessionMessage(msg); err != nil {
		writeErr(w, 400, "写会话消息失败: "+err.Error())
		return
	}
	mentions := db.ParseSessionAgentMentions(msg.Body)
	writeJSON(w, 201, map[string]any{
		"message":     msg,
		"mentions":    mentions,
		"wake_agents": sessionWakeAgents(mentions, members),
	})
}

// sessionWakeAgents 算出唤醒集合：被 @ 到、且**仍在席**的成员。
//
// 退席的成员（left_at 有值）不在集合里 —— 叫醒一个已经走了的 agent 是白烧
// 额度（§9 决策①：唤醒器只对「显式 @」负责，且额度是硬约束）。
func sessionWakeAgents(mentions []string, members []db.SessionMember) []string {
	inSeat := map[string]bool{}
	for _, m := range members {
		if !m.HasLeft {
			inSeat[m.AgentKey] = true
		}
	}
	out := []string{}
	for _, key := range mentions {
		if inSeat[key] {
			out = append(out, key)
		}
	}
	return out
}

// addSessionMember 热加入一个 agent（proposal §4.3）。
//
// 请求体：agent_key / role? / presence?
//
// role 缺省：auto ⇒ dispatcher，其余 ⇒ member（§9 约束②：auto 在 session 内
// 是 dispatcher 角色，不与成员抢活）。
func (s *Server) addSessionMember(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, ok := pathInt(r, "id")
	if !ok {
		writeErr(w, 400, "会话 id 不合法")
		return
	}
	var req struct {
		AgentKey string `json:"agent_key"`
		Role     string `json:"role"`
		Presence string `json:"presence"`
	}
	if !decodeSessionRequest(w, r, &req) {
		return
	}
	key := strings.TrimSpace(req.AgentKey)
	if key == "" {
		writeErr(w, 400, "agent_key 不能为空")
		return
	}
	sess, err := pg.GetSession(id)
	if err != nil {
		writeErr(w, 500, "读会话失败: "+err.Error())
		return
	}
	if sess == nil {
		writeErr(w, 404, "会话不存在")
		return
	}
	a, err := pg.GetAgentByKey(key)
	if err != nil {
		writeErr(w, 500, "查 agent 失败: "+err.Error())
		return
	}
	if a == nil {
		writeErr(w, 404, "agent 不存在: "+key)
		return
	}
	role := strings.TrimSpace(req.Role)
	if role == "" {
		role = db.SessionRoleMember
		if key == "auto" {
			role = db.SessionRoleDispatcher
		}
	}
	if !sessionRoles[role] {
		writeErr(w, 400, "成员分工只能是 dispatcher/member/observer")
		return
	}
	presence := strings.TrimSpace(req.Presence)
	if presence != "" && !sessionPresences[presence] {
		writeErr(w, 400, "在场状态只能是 idle/listening/working/blocked/left")
		return
	}
	if err := pg.AddSessionMember(id, key, role, presence); err != nil {
		writeErr(w, 400, "加会话成员失败: "+err.Error())
		return
	}
	member, ok := sessionMemberByKey(pg, id, key)
	if !ok {
		writeErr(w, 500, "加会话成员失败: 写入后读不到该成员")
		return
	}
	writeJSON(w, 201, map[string]any{"member": member})
}

// patchSessionMember 更新成员的在场状态（working / blocked(+eta,blocked_on) /
// listening …）。
//
// presence='left' 在数据层等价于退席（写 left_at）；其余状态只改在场字段。
//
// 为什么需要这个出口：§4.3 的 presence 三态（listening / working / blocked）
// 是会话页右上角的状态点来源，而 §4.5 只列了 GET …/presence（读）—— 没有写入
// 通道，三态永远是 idle，读接口就没有意义。
func (s *Server) patchSessionMember(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, ok := pathInt(r, "id")
	if !ok {
		writeErr(w, 400, "会话 id 不合法")
		return
	}
	key := strings.TrimSpace(r.PathValue("agent"))
	if key == "" {
		writeErr(w, 400, "agent key 不能为空")
		return
	}
	var req struct {
		Presence   string `json:"presence"`
		ETAMinutes *int   `json:"eta_minutes"`
		BlockedOn  string `json:"blocked_on"`
	}
	if !decodeSessionRequest(w, r, &req) {
		return
	}
	presence := strings.TrimSpace(req.Presence)
	if presence == "" {
		writeErr(w, 400, "presence 不能为空")
		return
	}
	if !sessionPresences[presence] {
		writeErr(w, 400, "在场状态只能是 idle/listening/working/blocked/left")
		return
	}
	if err := pg.SetSessionMemberPresence(id, key, presence, req.ETAMinutes, strings.TrimSpace(req.BlockedOn)); err != nil {
		writeErr(w, 400, "更新在场状态失败: "+err.Error())
		return
	}
	member, ok := sessionMemberByKey(pg, id, key)
	if !ok {
		writeErr(w, 500, "更新在场状态失败: 写入后读不到该成员")
		return
	}
	writeJSON(w, 200, map[string]any{"member": member})
}

// deleteSessionMember 退席（写 left_at + presence='left'）。
//
// 历史消息的归属**不改**（§9 约束③）：退席 agent 之前发的消息仍留在流里。
func (s *Server) deleteSessionMember(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, ok := pathInt(r, "id")
	if !ok {
		writeErr(w, 400, "会话 id 不合法")
		return
	}
	key := strings.TrimSpace(r.PathValue("agent"))
	if key == "" {
		writeErr(w, 400, "agent key 不能为空")
		return
	}
	member, found := sessionMemberByKey(pg, id, key)
	if !found {
		writeErr(w, 404, "成员不在会话里: "+key)
		return
	}
	if member.HasLeft {
		writeErr(w, 404, "成员不在会话里: "+key)
		return
	}
	if err := pg.RemoveSessionMember(id, key); err != nil {
		writeErr(w, 500, "退席失败: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"removed": true, "session_id": id, "agent_key": key})
}

// getSessionPresence 读会话成员的到场状态。
func (s *Server) getSessionPresence(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, ok := pathInt(r, "id")
	if !ok {
		writeErr(w, 400, "会话 id 不合法")
		return
	}
	sess, err := pg.GetSession(id)
	if err != nil {
		writeErr(w, 500, "读会话失败: "+err.Error())
		return
	}
	if sess == nil {
		writeErr(w, 404, "会话不存在")
		return
	}
	members, err := pg.SessionMembers(id)
	if err != nil {
		writeErr(w, 500, "读会话成员失败: "+err.Error())
		return
	}
	out := make([]sessionPresenceItem, 0, len(members))
	for _, m := range members {
		item := sessionPresenceItem{
			AgentKey:   m.AgentKey,
			Role:       m.Role,
			Presence:   m.Presence,
			ETAMinutes: m.ETAMinutes,
			BlockedOn:  m.BlockedOn,
			HasLeft:    m.HasLeft,
			JoinedAt:   rfc3339(m.JoinedAt),
		}
		if m.LeftAt != nil {
			item.LeftAt = rfc3339(*m.LeftAt)
		}
		out = append(out, item)
	}
	writeJSON(w, 200, map[string]any{"presence": out, "count": len(out)})
}

// listSessionsByAttachment 反查：某个工件关联了哪些会话（proposal §4.4）。
//
// identity_key 里可能有斜杠（PR 用 'owner/repo#12'）⇒ 客户端必须把它百分号
// 编码（%2F / %23）；Go 的 ServeMux 会把通配段解码回原值。
func (s *Server) listSessionsByAttachment(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	typ := strings.TrimSpace(r.PathValue("type"))
	key := strings.TrimSpace(r.PathValue("key"))
	if typ == "" || key == "" {
		writeErr(w, 400, "关联类型与标识都不能为空")
		return
	}
	sessions, err := pg.SessionsByAttachment(typ, key)
	if err != nil {
		writeErr(w, 500, "反查会话失败: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"sessions": sessions, "count": len(sessions)})
}

// sessionMemberByKey 在会话成员里找一个 agent。
//
// 第二个返回值 false = 该 agent 不在这个会话里（或会话本身不存在）⇒ 调用方
// 据此判 404。
func sessionMemberByKey(pg *db.DB, sessionID int64, agentKey string) (db.SessionMember, bool) {
	members, err := pg.SessionMembers(sessionID)
	if err != nil {
		return db.SessionMember{}, false
	}
	for _, m := range members {
		if m.AgentKey == agentKey {
			return m, true
		}
	}
	return db.SessionMember{}, false
}

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
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/internal/sessionwake"
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
	db.SessionKindMaster:       true,
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
//
// ⚠️ 视图**直接就是响应体**（db.Session 内嵌 ⇒ 它的字段被摊平到顶层），
// 不是 {"session": {...}} 的信封。理由：前端（web/src/lib/api.ts 的 sessions
// 段）按「响应体本身就是 Session」来读；信封会让前端拿到一个没有 id 的对象。
type sessionView struct {
	*db.Session
	Members     []db.SessionMember     `json:"members"`
	Attachments []db.SessionAttachment `json:"attachments"`
	// AgentCount 是在席成员数（退席的不算）。
	AgentCount int `json:"agent_count"`
	// GroupID 是这个会话对应的大厅 id（kind='task_hall' 时才有；老群回填出的
	// session 会带上它 ⇒ 前端能读到额度与并发状态）。
	GroupID *int64 `json:"group_id,omitempty"`
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

// sessionViewOfID 读一个会话的完整视图（本体 + 名册 + 关联 + 大厅 id）。
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
	view := sessionViewOf(sess, members, attachments)
	view.GroupID, err = pg.ChatGroupIDBySession(id)
	if err != nil {
		return sessionView{}, err
	}
	return view, nil
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
	groups, err := pg.ChatGroupIDsBySession(ids)
	if err != nil {
		writeErr(w, 500, "读会话对应的大厅失败: "+err.Error())
		return
	}
	views := make([]sessionView, 0, len(sessions))
	for i := range sessions {
		id := sessions[i].ID
		view := sessionViewOf(&sessions[i], roster[id], attachments[id])
		if gid, ok := groups[id]; ok {
			view.GroupID = &gid
		}
		views = append(views, view)
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
		// Goal 只对 kind='master'（总控）有意义：非空 ⇒ 创建会话的同时按这个目标
		// 起一个任务并绑定（launchTask 共享路径），后续规划/执行进展直接播报进会话流。
		Goal string `json:"goal"`
	}
	if !decodeSessionRequest(w, r, &req) {
		return
	}
	kind := strings.TrimSpace(req.Kind)
	if kind == "" {
		kind = db.SessionKindChat
	}
	if !sessionKinds[kind] {
		writeErr(w, 400, "会话类型只能是 chat/master/task_hall/side_question")
		return
	}
	goal := strings.TrimSpace(req.Goal)
	if goal != "" && kind != db.SessionKindMaster {
		writeErr(w, 400, "只有总控会话（kind='master'）支持创建时附带任务目标（goal）")
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
	// 总控创建即编排：goal 非空 ⇒ 建任务 + 绑定 + 播报，再走 launchTask 共享路径。
	// 失败回滚要连带删掉刚建的任务（半个总控比没有总控更糟，与 PublishTask 同款纪律）。
	spawnedTask := int64(0)
	defer func() {
		if rollback && spawnedTask > 0 {
			_ = pg.DeleteTask(spawnedTask)
		}
	}()
	if kind == db.SessionKindMaster && goal != "" {
		t, err := s.m.CreateTaskWithOptions("总控会话目标："+firstLine(goal, 40), goal, db.TaskCreateOptions{
			Name: strings.TrimSpace(req.Name),
		})
		if err != nil {
			writeErr(w, 500, "创建总控任务失败: "+err.Error())
			return
		}
		taskID, _ := strconv.ParseInt(t.ID, 10, 64)
		spawnedTask = taskID
		ok, err := pg.BindSessionTask(id, taskID)
		if err != nil {
			writeErr(w, 500, "绑定总控任务失败: "+err.Error())
			return
		}
		if !ok {
			writeErr(w, 500, "绑定总控任务失败：会话已绑定其他任务")
			return
		}
		announce := fmt.Sprintf("【总控】已创建任务 #%d 并绑定本会话。\n目标：%s\n后续的规划、认领与执行进展会直接播报在这里。",
			taskID, goal)
		if _, err := pg.AddSessionMessage(&db.SessionMessage{SessionID: id, FromAgent: members[0].AgentKey,
			Kind: db.SessionMessageKindNote, Body: announce}); err != nil {
			log.Printf("[session] 总控播报写入失败（session %d）: %v", id, err)
		}
		s.launchTask(t, goal, false)
		s.registerTaskSessionBinding(taskID, id)
	}
	rollback = false
	view, err := sessionViewOfID(pg, id)
	if err != nil {
		writeErr(w, 500, "读会话失败: "+err.Error())
		return
	}
	writeJSON(w, 201, view)
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
	writeJSON(w, 200, view)
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
	view, err := sessionViewOfID(pg, id)
	if err != nil {
		writeErr(w, 500, "读会话失败: "+err.Error())
		return
	}
	writeJSON(w, 200, view)
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
	writeJSON(w, 200, map[string]any{"ok": true, "deleted": id, "id": id})
}

// listSessionMessages 读会话消息流（含 agent 消息、收据条、线程回复）。
//
// ?since=<id> 增量拉（只回 id 更大的消息）；?limit= 限单页条数（缺省 200）。
//
// 响应：items（消息数组）/ cursor（下次增量用的水位线 = 本次最大 id）/ running
// （会话里有没有正在跑的对话）/ count。
//
// cursor 用**消息 id**而不是时间戳：id 是本会话内的单调序号 ⇒ 增量拉不会漏
// 也不会重（时间戳会因同毫秒写入而漏消息）。
//
// running 的口径是「这个会话背后的对话有没有在跑」—— 不是 presence 的推论：
// presence 是持久化的状态字段，没人去改它 ⇒ 用它当「正在跑」会永远返回真。
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
	since := int64(0)
	if sv := strings.TrimSpace(r.URL.Query().Get("since")); sv != "" {
		since, _ = strconv.ParseInt(sv, 10, 64)
		if since < 0 {
			since = 0
		}
	}
	msgs, err := pg.SessionMessagesSince(id, since, limit)
	if err != nil {
		writeErr(w, 500, "读会话消息失败: "+err.Error())
		return
	}
	cursor := since
	if n := len(msgs); n > 0 {
		cursor = msgs[n-1].ID
	}
	writeJSON(w, 200, map[string]any{
		"items":   msgs,
		"cursor":  cursor,
		"running": s.sessionRunning(pg, id),
		"count":   len(msgs),
	})
}

// sessionRunning 判定这个会话背后有没有正在跑的对话（前端据此决定要不要继续轮询）。
func (s *Server) sessionRunning(pg *db.DB, sessionID int64) bool {
	convIDs, err := pg.SessionConversationIDs(sessionID)
	if err != nil || len(convIDs) == 0 {
		return false
	}
	s.chatMu.Lock()
	defer s.chatMu.Unlock()
	for _, cid := range convIDs {
		if s.chatBusy[s.convBusyKey(cid)] {
			return true
		}
	}
	return false
}

// postSessionMessage 往会话消息流写一条消息，并回报**唤醒集合**。
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
// 【唤醒与自取必须分开】（§9 约束①）：本出口只**报告**两个集合，**不**代为
// 唤醒 —— 唤醒路径与「自取」路径要分开实现、分开计量：
//
//	wake_agents       定向必醒（@ 到了 / 显式 to_agent，且仍在席、不是发起者）
//	self_serve_agents 非 @ 按需自取（空闲的普通成员；不因此起任何 turn）
//
// 裁决本身在 internal/sessionwake（纯函数、无库无模型）⇒ 同一份输入永远得到
// 同一份唤醒集合。
//
// 【@ 到不在席的 agent ⇒ 先加入再被定向】（§4.3 的热加入路径②）：本出口在写
// 消息前把被 @ 的 agent 热加入（presence='listening'），否则「@ 必醒」在成员
// 还没入席时就落空了。
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
	sender := strings.TrimSpace(req.FromAgent)
	mentions := db.ParseSessionAgentMentions(strings.TrimSpace(req.Body))
	// 热加入：@ 到不在席的 agent ⇒ 先入席再被定向（§4.3）。
	members = hotJoinMentioned(pg, id, mentions, members, sender)

	msg := &db.SessionMessage{
		SessionID:       id,
		FromAgent:       sender,
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
	decision := sessionwake.Decide(sessionwake.Message{
		Body:      msg.Body,
		FromAgent: msg.FromAgent,
		ToAgent:   msg.ToAgent,
		Kind:      msg.Kind,
		Blocked:   msg.Status == db.SessionMessageBlocked,
	}, sessionWakeMembers(members))
	// 会话级唤醒器（server/session_waker.go）：本出口只报告 wake 集合；真正起 turn
	// 在这里 —— @ 定向必醒（人发消息接力深度 0）。异步、尽力而为：起不起得动
	// 不影响本响应。
	s.wakeSessionAgents(id, decision.MustWake, msg, 0)
	writeJSON(w, 201, map[string]any{
		"status":            "ok",
		"message":           msg,
		"mentions":          decision.Mentions,
		"everyone":          decision.Everyone,
		"designated":        decision.Designated,
		"wake_agents":       decision.MustWake,
		"wake_states":       decision.WakeStates,
		"self_serve_agents": decision.SelfServe,
	})
}

// hotJoinMentioned 把正文里被 @ 到的、不在席的 agent 热加入会话。
//
// §4.3 的加入方式②：「在会话里 @Agent名 ⇒ 若该 agent 尚未在席，先加入再被
// 定向」。没有这一步，「@ 必醒」在成员还没入席时就落空了（裁决层只对在席的
// 人负责 —— 叫醒一个不在席的 agent 是白烧额度）。
//
// 不在 agents 表里的 agent 跳过（session_members.agent_key 有外键，硬写会
// 失败）；退席过的 agent 重新入席（AddSessionMember 会把 left_at 清空）。
//
// 返回更新后的名册（就地追加，不重新查库）。
func hotJoinMentioned(pg *db.DB, sessionID int64, mentions []string, members []db.SessionMember, sender string) []db.SessionMember {
	inSeat := map[string]bool{}
	for _, m := range members {
		if !m.HasLeft {
			inSeat[m.AgentKey] = true
		}
	}
	for _, key := range mentions {
		key = strings.TrimSpace(key)
		if key == "" || key == sender || inSeat[key] {
			continue
		}
		a, err := pg.GetAgentByKey(key)
		if err != nil || a == nil {
			continue
		}
		role := db.SessionRoleMember
		if key == "auto" {
			role = db.SessionRoleDispatcher
		}
		if err := pg.AddSessionMember(sessionID, key, role, db.SessionPresenceListening); err != nil {
			continue
		}
		members = append(members, db.SessionMember{
			SessionID: sessionID,
			AgentKey:  key,
			Role:      role,
			Presence:  db.SessionPresenceListening,
		})
		inSeat[key] = true
	}
	return members
}

// sessionWakeMembers 把名册转成唤醒裁决层的输入（裁决层不认 db 的类型 ⇒ 转换
// 发生在这里，裁决层保持零依赖、可脱离数据库单测）。
func sessionWakeMembers(members []db.SessionMember) []sessionwake.Member {
	out := make([]sessionwake.Member, 0, len(members))
	for _, m := range members {
		out = append(out, sessionwake.Member{
			AgentKey: m.AgentKey,
			Role:     sessionwake.Role(m.Role),
			Presence: sessionwake.Presence(m.Presence),
			Left:     m.HasLeft,
		})
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
	writeJSON(w, 201, map[string]any{"ok": true, "member": member})
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
	writeJSON(w, 200, map[string]any{"ok": true, "member": member})
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
	writeJSON(w, 200, map[string]any{"ok": true, "removed": true, "session_id": id, "agent_key": key})
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

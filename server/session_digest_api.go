package server

// 会话噪音治理与跨会话共享上下文的 HTTP 出口 —— 方案书 §7 的 P5 行。
//
// 三组出口：
//
//	收件条   GET  /api/sessions/{id}/digest          你不在时发生了什么（折叠摘要）
//	         POST /api/sessions/{id}/digest/read     把这条收件条标记成已读
//	折叠时间线 GET /api/sessions/{id}/timeline        线程折叠后的消息流
//	跨会话共享 POST /api/sessions/{id}/context-grants 授予「看另一个会话」的权限
//	         GET  /api/sessions/{id}/context-grants  列授予
//	         DELETE /api/sessions/{id}/context-grants/{source}  撤销授予
//	         GET  /api/sessions/{id}/context          预览共享内容（不记账）
//
// **所有判定都在数据层**（db/session_thread.go 的折叠与摘要规则、
// db/session_share.go 的白名单与额度口径、db/session_digest.go 的水位线）——
// 本文件不复制任何一条业务规则，只负责把「客户端给错了」判成 400、
// 「东西不存在」判成 404、「被安全边界挡住」判成 409/403。
//
// 路由注册不在本文件：server/server.go 的 mux 注册表是别的单元的文件域（同一
// worktree 内并行开发）。本单元只提供 handler 与这个一次性注册入口 —— 属主在
// 注册区调一次 s.RegisterSessionDigestRoutes(mux) 即可（与 server/session_api.go
// 的 RegisterSessionRoutes 同款纪律）。

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
)

// maxSessionDigestRequestBytes 是本组出口的请求体上限。
//
// 与 server/session_api.go 的 maxSessionRequestBytes 同款纪律：共享授予的请求体
// 里有自由文本（share_kinds 之类），没有上限就是内存放大器。
const maxSessionDigestRequestBytes = 16 << 10

// RegisterSessionDigestRoutes 挂上噪音治理与跨会话共享的路由。
func (s *Server) RegisterSessionDigestRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/sessions/{id}/timeline", s.getSessionTimeline)
	mux.HandleFunc("GET /api/sessions/{id}/digest", s.getSessionDigest)
	mux.HandleFunc("POST /api/sessions/{id}/digest/read", s.postSessionDigestRead)
	mux.HandleFunc("GET /api/sessions/{id}/context-grants", s.listSessionContextGrants)
	mux.HandleFunc("POST /api/sessions/{id}/context-grants", s.createSessionContextGrant)
	mux.HandleFunc("DELETE /api/sessions/{id}/context-grants/{source}", s.revokeSessionContextGrant)
	mux.HandleFunc("GET /api/sessions/{id}/context", s.getSessionSharedContext)
}

// WireSessionContext 把数据层接成 agent 侧的共享上下文来源。
//
// 装配期调一次即可（与 agent/group_mailbox.go 的 WireGroupMailbox 同款纪律）。
func WireSessionContext(pg *db.DB) {
	if pg == nil {
		return
	}
	agent.DefaultSessionContextProvider = pg
}

// ---------- 折叠时间线 ----------

// getSessionTimeline 返回折叠后的会话消息流。
//
// 查询参数：
//   - since：只取 id 大于它的消息（0 = 全部）
//   - limit：消息条数上限（缺省与上限都是 db 层的 sessionTimelineMaxMessages）
//
// 响应里的 threads 每条是「一条根消息 + 它的补充意见 + 它的收据条」⇒ 前端一个
// 气泡只渲染一条线程，补充意见折叠在下面（§2.3 第 7 条）。
func (s *Server) getSessionTimeline(w http.ResponseWriter, r *http.Request) {
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
	since := atoiDefault(r.URL.Query().Get("since"), 0)
	limit := atoiDefault(r.URL.Query().Get("limit"), 0)
	threads, digest, err := pg.SessionTimeline(id, int64(since), limit)
	if err != nil {
		writeErr(w, 500, "读会话时间线失败: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{
		"session_id":   id,
		"threads":      threads,
		"thread_count": len(threads),
		"digest":       digest,
	})
}

// ---------- 收件条 ----------

// getSessionDigest 返回「你不在时发生了什么」。
//
// 查询参数：
//   - since：水位线（消息 id）。**不给就用持久化的水位线**（用户上次看到的地方）。
//
// 响应里的 bubbles_* 四个数字是验收口径（方案书 §7 的 P5 行「群消息气泡数下降
// 可量化」）：raw_bubbles 是没有任何折叠时的气泡数，digest_bubbles 是收件条展开
// 后真正要读的条数，bubbles_saved 是省下来的。
func (s *Server) getSessionDigest(w http.ResponseWriter, r *http.Request) {
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
	// since 缺省是 -1 ⇒ 用持久化水位线；给了就用显式值。
	since := int64(-1)
	if raw := strings.TrimSpace(r.URL.Query().Get("since")); raw != "" {
		n, parseErr := strconv.ParseInt(raw, 10, 64)
		if parseErr != nil {
			writeErr(w, 400, "since 不合法")
			return
		}
		since = n
	}
	digest, err := pg.SessionDigestFor(id, since)
	if err != nil {
		writeErr(w, 500, "读会话收件条失败: "+err.Error())
		return
	}
	writeJSON(w, 200, digest)
}

// postSessionDigestRead 把收件条标记成已读（水位线推进到该会话最新消息）。
//
// 请求体可选：{"message_id": 123}（推进到指定消息）。不给就推进到最新。
// 水位线**只前进不后退**（db.MarkSessionDigestRead 的硬语义）。
func (s *Server) postSessionDigestRead(w http.ResponseWriter, r *http.Request) {
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
	var body struct {
		MessageID int64 `json:"message_id"`
	}
	if r.ContentLength > 0 {
		if !decodeSessionDigestRequest(w, r, &body) {
			return
		}
	}
	state, err := pg.MarkSessionDigestRead(id, body.MessageID)
	if err != nil {
		writeErr(w, 500, "推进收件条水位线失败: "+err.Error())
		return
	}
	writeJSON(w, 200, state)
}

// ---------- 跨会话共享上下文 ----------

// listSessionContextGrants 列一个消费方的共享授予（含已撤销的）。
func (s *Server) listSessionContextGrants(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, ok := pathInt(r, "id")
	if !ok {
		writeErr(w, 400, "会话 id 不合法")
		return
	}
	grants, err := pg.ListSessionContextGrants(id)
	if err != nil {
		writeErr(w, 500, "读共享上下文授予失败: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"grants": grants, "count": len(grants)})
}

// createSessionContextGrant 授予「消费会话可以看源会话的上下文」。
//
// 请求体：{"source_session_id": 9, "share_kinds": ["note","answer"],
// "max_messages": 20, "created_by": "operator"}
//
// 判定全在数据层：可共享类型是**代码常量**白名单（DB 里的值只能收窄）、
// 跨公司拒绝、同会话拒绝、一个白名单类型都不剩时拒绝。
func (s *Server) createSessionContextGrant(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, ok := pathInt(r, "id")
	if !ok {
		writeErr(w, 400, "会话 id 不合法")
		return
	}
	var body struct {
		SourceSessionID int64    `json:"source_session_id"`
		ShareKinds      []string `json:"share_kinds"`
		MaxMessages     int      `json:"max_messages"`
		CreatedBy       string   `json:"created_by"`
	}
	if !decodeSessionDigestRequest(w, r, &body) {
		return
	}
	if body.SourceSessionID <= 0 {
		writeErr(w, 400, "source_session_id 不合法")
		return
	}
	grant, err := pg.CreateSessionContextGrant(id, body.SourceSessionID, body.ShareKinds,
		body.MaxMessages, body.CreatedBy)
	if err != nil {
		writeSessionShareError(w, err)
		return
	}
	writeJSON(w, 200, grant)
}

// revokeSessionContextGrant 撤销一条授予（不删行 —— 审计要看得见它曾经存在）。
func (s *Server) revokeSessionContextGrant(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, ok := pathInt(r, "id")
	if !ok {
		writeErr(w, 400, "会话 id 不合法")
		return
	}
	source, ok := pathInt(r, "source")
	if !ok {
		writeErr(w, 400, "源会话 id 不合法")
		return
	}
	if err := pg.RevokeSessionContextGrant(id, source); err != nil {
		if errors.Is(err, db.ErrSessionShareNotFound) {
			writeErr(w, 404, "共享上下文授予不存在")
			return
		}
		writeErr(w, 500, "撤销共享上下文授予失败: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// getSessionSharedContext 预览消费方能读到的跨会话背景。
//
// **不记账**：这个出口是给界面看的，每次调用都往账上写一笔会把「看一眼」变成
// 「一次消费」（额度归属与审计会被噪声淹没）。真正的消费发生在 agent 读取时
// （db.SessionSharedContextFor），那里才记账。
func (s *Server) getSessionSharedContext(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, ok := pathInt(r, "id")
	if !ok {
		writeErr(w, 400, "会话 id 不合法")
		return
	}
	sc, err := pg.PreviewSessionSharedContext(id)
	if err != nil {
		writeSessionShareError(w, err)
		return
	}
	writeJSON(w, 200, sc)
}

// writeSessionShareError 把数据层的共享上下文错误翻译成 HTTP 状态码。
//
// 口径：被安全边界挡住 ⇒ 403（客户端改请求也没用，是策略问题）；额度冻结 ⇒
// 409（状态问题，过一会儿可能恢复）；其余是 400。
func writeSessionShareError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, db.ErrSessionShareCrossCompany):
		writeErr(w, 403, "两个会话属于不同公司，不能共享上下文")
	case errors.Is(err, db.ErrSessionShareQuotaFrozen):
		writeErr(w, 409, "消费方群组的预算状态不允许新的上下文消费")
	case errors.Is(err, db.ErrSessionShareKindsEmpty):
		writeErr(w, 400, "没有可共享的消息类型（白名单只允许 note/answer/finding）")
	case errors.Is(err, db.ErrSessionShareSameSession):
		writeErr(w, 400, "源会话与消费会话不能是同一个")
	default:
		writeErr(w, 500, err.Error())
	}
}

// decodeSessionDigestRequest 读请求体（限长）。
func decodeSessionDigestRequest(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxSessionDigestRequestBytes)
	if err := decode(r, v); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeErr(w, 413, "请求体过大")
			return false
		}
		writeErr(w, 400, "请求体不是合法 JSON")
		return false
	}
	return true
}

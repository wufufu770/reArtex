package server

import (
	"net/http"
	"strings"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
)

// ---------- 群聊（任务大厅）API ----------
//
// 群聊 = 1 个大厅会话（agent_key='auto'，分发者）+ N 个成员会话（每个成员一个，
// 独立 transcript）+ 共享的**一个** task（同一张探索图 ⇒ 群用量记在同一张 task 上）。
//
// 路由注册不在本文件：server/server.go 的 mux 注册表是别的单元的文件域。属主在
// 注册区加一行 s.RegisterTaskHallGroupRoutes(mux) 即可（见下面的注册函数）。

// taskHallGroupCreateReq 建群请求。
type taskHallGroupCreateReq struct {
	TaskID           int64    `json:"task_id"`
	Name             string   `json:"name"`
	Members          []string `json:"members"`
	ConcurrencyLimit int      `json:"concurrency_limit"`
}

// taskHallGroupDetail 群详情：群本身 + 成员 + 群内禁列表 + 消息箱计数。
type taskHallGroupDetail struct {
	*db.ChatGroup
	Members       []db.ChatGroupMember `json:"members"`
	DisabledTools map[string]bool      `json:"disabled_tools"`
	MessageCount  int                  `json:"message_count"`
}

// RegisterTaskHallGroupRoutes 挂上群聊路由。
//
// 为什么要单独一个注册函数而不是把 mux.HandleFunc 写在 server.go 里：路由表是
// 别的单元的文件域（同一 worktree 内并行开发），本单元只能提供 handler 与这个
// 一次性注册入口 —— 属主在注册区调一次即可。
func (s *Server) RegisterTaskHallGroupRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/task-hall/groups", s.createTaskHallGroup)
	mux.HandleFunc("GET /api/task-hall/groups", s.listTaskHallGroups)
	mux.HandleFunc("GET /api/task-hall/groups/{id}", s.getTaskHallGroup)
	mux.HandleFunc("POST /api/task-hall/groups/{id}/messages", s.postTaskHallGroupMessage)
	mux.HandleFunc("GET /api/task-hall/groups/{id}/messages", s.getTaskHallGroupMessages)
	mux.HandleFunc("GET /api/task-hall/groups/{id}/disabled-tools", s.getTaskHallGroupDisabledTools)
	mux.HandleFunc("POST /api/task-hall/groups/{id}/disabled-tools", s.postTaskHallGroupDisabledTool)
	mux.HandleFunc("DELETE /api/task-hall/groups/{id}/disabled-tools/{tool}", s.deleteTaskHallGroupDisabledTool)
}

// WireGroupMailbox 把群消息箱句柄接到 agent 侧：群成员会话每轮收取未读群消息。
//
// 装配处调一次即可（ChatAgent 的构造处）。不调不影响其它功能，只是群消息不会被收取。
func WireGroupMailbox(pg *db.DB) { agent.DefaultGroupMailbox = pg }

// createTaskHallGroup 建群聊：大厅会话 + 群组 + 每个成员一个会话。
//
// 一个 task 至多一个大厅（大厅是任务的"大厅"，不是一个独立实体）⇒ 重复建群返回 409。
func (s *Server) createTaskHallGroup(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	var req taskHallGroupCreateReq
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if req.TaskID <= 0 {
		writeErr(w, 400, "task_id 不能为空")
		return
	}
	task, err := pg.GetTask(req.TaskID)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if task == nil {
		writeErr(w, 404, "任务不存在")
		return
	}

	members := []string{}
	seen := map[string]bool{}
	for _, raw := range req.Members {
		key := strings.TrimSpace(raw)
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		members = append(members, key)
	}
	if len(members) == 0 {
		writeErr(w, 400, "members 不能为空")
		return
	}
	for _, key := range members {
		a, err := pg.GetAgentByKey(key)
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		if a == nil {
			writeErr(w, 400, "agent 不存在: "+key)
			return
		}
	}

	existing, err := pg.GroupByTask(req.TaskID)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if existing != nil {
		writeErr(w, 409, "该任务已有群聊")
		return
	}

	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = strings.TrimSpace(task.Name)
	}
	if name == "" {
		name = "任务大厅"
	}
	limit := req.ConcurrencyLimit
	if limit <= 0 {
		limit = len(members)
	}

	// 大厅会话：分发者 auto 在这里接待人、布置任务、看全局。
	hall, err := pg.CreateConversation("auto", name, nil)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	gid, err := pg.CreateChatGroup(&db.ChatGroup{
		ConversationID:   hall.ID,
		TaskID:           req.TaskID,
		Name:             name,
		Status:           "active",
		ConcurrencyLimit: limit,
	})
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}

	// auto 永远是分发者，绑定大厅会话（它不需要第二个会话）。
	if err := pg.AddChatGroupMember(gid, "auto", &hall.ID, "dispatcher"); err != nil {
		_ = pg.DeleteChatGroup(gid)
		writeErr(w, 500, err.Error())
		return
	}
	for _, key := range members {
		if key == "auto" {
			continue
		}
		conv, err := pg.CreateConversation(key, name+"/"+key, nil)
		if err != nil {
			_ = pg.DeleteChatGroup(gid)
			writeErr(w, 500, err.Error())
			return
		}
		if err := pg.AddChatGroupMember(gid, key, &conv.ID, "member"); err != nil {
			// 半个群比没有群更糟：它会占住"一个 task 至多一个大厅"的名额，
			// 让重试变成 409。
			_ = pg.DeleteChatGroup(gid)
			writeErr(w, 500, err.Error())
			return
		}
	}

	detail, err := s.taskHallGroupDetail(pg, gid)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 201, detail)
}

// listTaskHallGroups 列群组；?status= 过滤。
func (s *Server) listTaskHallGroups(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	groups, err := pg.ListChatGroups(r.URL.Query().Get("status"))
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"groups": groups})
}

// getTaskHallGroup 群详情（含成员与消息箱计数）。
func (s *Server) getTaskHallGroup(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, ok := pathInt(r, "id")
	if !ok {
		writeErr(w, 400, "群组 id 不合法")
		return
	}
	detail, err := s.taskHallGroupDetail(pg, id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if detail == nil {
		writeErr(w, 404, "群组不存在")
		return
	}
	writeJSON(w, 200, detail)
}

// postTaskHallGroupMessage 人往群消息箱写一条消息（from_agent 记 human）。
//
// 准入判定在存储层（db.AddChatGroupMessage）：被拦的消息落库标 blocked，
// 但不进入任何成员的上下文。响应里带 status / blocked_reason，让调用方看得见。
func (s *Server) postTaskHallGroupMessage(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, ok := pathInt(r, "id")
	if !ok {
		writeErr(w, 400, "群组 id 不合法")
		return
	}
	group, err := pg.GetChatGroup(id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if group == nil {
		writeErr(w, 404, "群组不存在")
		return
	}
	var req struct {
		ToAgent  string `json:"to_agent"`
		Body     string `json:"body"`
		Kind     string `json:"kind"`
		IntentID int64  `json:"intent_id"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	msg := &db.ChatGroupMessage{
		GroupID:   id,
		FromAgent: "human",
		ToAgent:   strings.TrimSpace(req.ToAgent),
		Kind:      strings.TrimSpace(req.Kind),
		Body:      strings.TrimSpace(req.Body),
		TaskID:    &group.TaskID,
	}
	if req.IntentID > 0 {
		msg.IntentID = &req.IntentID
	}
	if _, err := pg.AddChatGroupMessage(msg); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 201, msg)
}

// getTaskHallGroupMessages 读群消息箱（管理视角：含已读与被拦截的）。
func (s *Server) getTaskHallGroupMessages(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, ok := pathInt(r, "id")
	if !ok {
		writeErr(w, 400, "群组 id 不合法")
		return
	}
	msgs, err := pg.ListChatGroupMessages(id, 200)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"messages": msgs})
}

// getTaskHallGroupDisabledTools 读群内工具禁列表。
//
// 它是**禁**列表：出现的 key 就是被禁用的（值为 true）。表里没有 enabled 列 ⇒
// 群内配置不可能放宽任何 agent 的工具集。
func (s *Server) getTaskHallGroupDisabledTools(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, ok := pathInt(r, "id")
	if !ok {
		writeErr(w, 400, "群组 id 不合法")
		return
	}
	disabled, err := pg.GroupDisabledTools(id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"disabled_tools": disabled})
}

// postTaskHallGroupDisabledTool 登记一个群内禁用的工具。
func (s *Server) postTaskHallGroupDisabledTool(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, ok := pathInt(r, "id")
	if !ok {
		writeErr(w, 400, "群组 id 不合法")
		return
	}
	var req struct {
		ToolKey string `json:"tool_key"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if err := pg.DisableChatGroupTool(id, req.ToolKey); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	disabled, err := pg.GroupDisabledTools(id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "disabled_tools": disabled})
}

// deleteTaskHallGroupDisabledTool 撤销一条群内禁用（只把 key 从禁列表拿掉，
// 不动全局绑定）。
func (s *Server) deleteTaskHallGroupDisabledTool(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, ok := pathInt(r, "id")
	if !ok {
		writeErr(w, 400, "群组 id 不合法")
		return
	}
	if err := pg.EnableChatGroupTool(id, r.PathValue("tool")); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	disabled, err := pg.GroupDisabledTools(id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "disabled_tools": disabled})
}

// taskHallGroupDetail 组装群详情（群 + 成员 + 禁列表 + 消息箱计数）。
func (s *Server) taskHallGroupDetail(pg *db.DB, groupID int64) (*taskHallGroupDetail, error) {
	g, err := pg.GetChatGroup(groupID)
	if err != nil || g == nil {
		return nil, err
	}
	members, err := pg.ChatGroupMembers(groupID)
	if err != nil {
		return nil, err
	}
	disabled, err := pg.GroupDisabledTools(groupID)
	if err != nil {
		return nil, err
	}
	count, err := pg.CountChatGroupMessages(groupID)
	if err != nil {
		return nil, err
	}
	return &taskHallGroupDetail{
		ChatGroup:     g,
		Members:       members,
		DisabledTools: disabled,
		MessageCount:  count,
	}, nil
}

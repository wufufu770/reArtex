package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

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
	// HallStatus 是语义化整体状态（db.TaskHallStatusByGroup 纯函数算出）。此前该
	// 函数 0 生产调用 ⇒ 大厅只有原始 group_status 没有人话档位。best-effort：算不出
	// 不给（详情不被状态计算拖垮）。JSON 键叫 hall_status：嵌入了 ChatGroup，它的
	// status（群原始状态字符串）不能被这个新字段遮蔽（撞名会让老字段消失）。
	HallStatus *db.TaskHallStatus `json:"hall_status,omitempty"`
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
	mux.HandleFunc("PUT /api/task-hall/groups/{id}/status", s.putChatGroupState)
}

// chatGroupStateTransitions 是群组状态的迁移表（**代码常量**，不读 DB 规则行）。
//
// 为什么是代码常量：状态机是安全边界（它决定一个群还能不能领新活），DB 规则行
// 可被改可被删 ⇒ 边界必须留在代码里（同 db/quota.go 的额度状态机同款纪律）。
//
// 迁移语义取自既有的两处口径，不是这里新发明的：
//   - server/manager.go：只有 active / resuming 的群还有活可派 ⇒ 其余状态都
//     是「不开新活」；
//   - db/schema_chat_group.sql 的注释：draining / parked / resuming 这套状态机
//     由「额度 / 排空」单元实现 ⇒ 它与 db/quota.go 的额度状态机是同一套。
var chatGroupStateTransitions = map[string][]string{
	"draft":    {"active", "closed", "failed"},
	"active":   {"draining", "parked", "resuming", "closed", "failed"},
	"resuming": {"active", "draining", "parked", "closed", "failed"},
	"draining": {"active", "resuming", "parked", "closed", "failed"},
	"parked":   {"active", "resuming", "draining", "closed", "failed"},
	// closed / failed 是终态：回退等于让操作者绕过「这个群到此为止」的决定。
	"closed": {},
	"failed": {},
}

// chatGroupStateQuota 把群组状态映射成对应的额度状态。
//
// 为什么必须一一对应：群组状态与额度状态是**两道独立的闸门** —— 认领新意图看
// 前者，发起模型调用看后者（server/manager.go 的 groupUsableMemberDecision 两
// 道都查）。两道闸门给出的答案必须一致：群说「在跑」而额度说「冻结」就是自相
// 矛盾（认领了却调不动模型）。
//
// closed / failed 映射到 parked（冻结）是本单元的推断：死掉的群不该再花钱。
// 方向是安全的（只收紧，不放宽）。
func chatGroupStateQuota(status string) (db.QuotaState, bool) {
	switch status {
	case "active", "resuming", "draft":
		return db.QuotaStateActive, true
	case "draining":
		return db.QuotaStateDraining, true
	case "parked", "closed", "failed":
		return db.QuotaStateParked, true
	default:
		return "", false
	}
}

// setChatGroupStateReq 是改群状态的请求体。
type setChatGroupStateReq struct {
	// Status 是目标状态：draft/active/draining/parked/resuming/closed/failed。
	Status string `json:"status"`
	// Reason 是变更理由。必填 —— 群的开关是授权决定，没有理由的变更事后追不回来。
	Reason string `json:"reason"`
}

// putChatGroupState 改群组状态（暂停 / 恢复 / 排空 / 停放 / 关闭）。
//
// 【为什么群状态与额度状态要一起改】
//
//	两道闸门独立生效：认领新意图看群状态，发起模型调用看额度状态。只改一道
//	就会出现「群说在跑、额度说冻结」这种自相矛盾的状态（表现是认领了却调不动
//	模型）。所以每次迁移都把额度状态对齐到映射表里的那一档。
//
// 【顺序：先群状态、后额度】
//
//	万一额度同步失败，留在「群已冻结、额度还 active」—— 这是安全方向（群不再
//	开新活 ⇒ 没有新的模型调用），而反过来（额度冻了、群还在跑）会让人以为群
//	还活着。失败会如实报 500，不假装成功。
func (s *Server) putChatGroupState(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	groupID, ok := pathInt(r, "id")
	if !ok || groupID <= 0 {
		writeErr(w, 400, "群组 id 不合法")
		return
	}
	var req setChatGroupStateReq
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, "请求体解析失败")
		return
	}
	status := strings.TrimSpace(req.Status)
	if _, known := chatGroupStateTransitions[status]; !known {
		writeErr(w, 400, "群组状态只能是 draft/active/draining/parked/resuming/closed/failed")
		return
	}
	reason := strings.TrimSpace(req.Reason)
	if reason == "" {
		writeErr(w, 400, "必须给出变更理由")
		return
	}
	group, err := pg.GetChatGroup(groupID)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if group == nil {
		writeErr(w, 404, "群组不存在")
		return
	}
	if !chatGroupStateCanMove(group.Status, status) {
		writeErr(w, 409, "群组当前状态是 "+group.Status+"，不能迁移到 "+status)
		return
	}
	if err := setChatGroupStatus(pg, groupID, status); err != nil {
		writeErr(w, 500, "改群组状态失败: "+err.Error())
		return
	}
	quotaSynced := true
	if quotaState, has := chatGroupStateQuota(status); has {
		// 没有额度行时额度闸门不参与判定（db.GroupQuotaState 的契约：零值 =
		// 未设 = 不拒绝）⇒ 不必建行 —— 建一个 0 额度的行反而会把所有模型调用
		// 都拒掉。
		if err := pg.SetGroupQuotaState(groupID, quotaState); err != nil {
			if !errors.Is(err, db.ErrQuotaNoBudget) {
				writeErr(w, 500, "同步群组额度状态失败: "+err.Error())
				return
			}
			quotaSynced = false
		}
	}
	payload, _ := json.Marshal(map[string]any{
		"group_id":    groupID,
		"from":        group.Status,
		"to":          status,
		"remote_addr": r.RemoteAddr,
	})
	// 先落变更再追审计：追审计失败时变更已经发生，硬要回滚会与「预算正在被
	// 并发预扣」打架 ⇒ 如实回报 audited=false（与额度接口同款处置）。
	audited := true
	if _, err := pg.AppendAuditEvent(db.AuditEvent{
		Actor:    "operator",
		ToolName: "group_state",
		Action:   "group_state_set",
		Reason:   reason,
		Payload:  payload,
	}); err != nil {
		audited = false
		log.Printf("[audit] 追加群组状态变更审计失败 group=%d status=%s: %v", groupID, status, err)
	}
	detail, err := s.taskHallGroupDetail(pg, groupID)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{
		"group":        detail,
		"status":       status,
		"audited":      audited,
		"quota_synced": quotaSynced,
	})
}

// chatGroupStateCanMove 报告这条迁移是否被允许。
func chatGroupStateCanMove(from, to string) bool {
	for _, allowed := range chatGroupStateTransitions[from] {
		if allowed == to {
			return true
		}
	}
	return false
}

// setChatGroupStatus 写群组状态与对应的时间戳列。
//
// 为什么在 server 层直接写 SQL：db 层没有这个写入原语，而 db/chat_group.go 是
// 别的单元的文件域。状态机本身是**代码常量**（见 chatGroupStateTransitions），
// 不读 DB 规则行 ⇒ 在哪一层执行都是同一张表、同一套规则。
func setChatGroupStatus(pg *db.DB, groupID int64, status string) error {
	if err := pg.EnsureChatGroupSchema(); err != nil {
		return fmt.Errorf("应用群聊 schema 失败: %w", err)
	}
	now := time.Now()
	var drainedAt, parkedAt, resumedAt *time.Time
	switch status {
	case "draining":
		drainedAt = &now
	case "parked":
		parkedAt = &now
	case "active", "resuming":
		resumedAt = &now
	}
	if _, err := pg.Exec(`UPDATE chat_groups
SET status=$2,
    drained_at=COALESCE($3, drained_at),
    parked_at=COALESCE($4, parked_at),
    resumed_at=COALESCE($5, resumed_at)
WHERE id=$1`, groupID, status, drainedAt, parkedAt, resumedAt); err != nil {
		return fmt.Errorf("写群组状态失败: %w", err)
	}
	return nil
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
	hall, err := pg.CreateConversationOf("auto", name, nil, db.OriginHall)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	// A1（方案 v10 §15）：失败回滚除了删组，还要删 CreateConversationOf 建出的会话
	// （大厅 + 成员会话）——孤儿会话占住"一个 task 至多一个大厅"的名额，重试恒 409。
	createdConvs := []int64{hall.ID}
	delConvs := func() {
		for _, id := range createdConvs {
			_ = pg.DeleteConversation(id)
		}
	}
	gid, err := pg.CreateChatGroup(&db.ChatGroup{
		ConversationID:   hall.ID,
		TaskID:           req.TaskID,
		Name:             name,
		Status:           "active",
		ConcurrencyLimit: limit,
	})
	if err != nil {
		delConvs()
		writeErr(w, 500, err.Error())
		return
	}

	// auto 永远是分发者，绑定大厅会话（它不需要第二个会话）。
	if err := pg.AddChatGroupMember(gid, "auto", &hall.ID, "dispatcher"); err != nil {
		_ = pg.DeleteChatGroup(gid)
		delConvs()
		writeErr(w, 500, err.Error())
		return
	}
	for _, key := range members {
		if key == "auto" {
			continue
		}
		conv, err := pg.CreateConversationOf(key, name+"/"+key, nil, db.OriginHall)
		if err != nil {
			_ = pg.DeleteChatGroup(gid)
			delConvs()
			writeErr(w, 500, err.Error())
			return
		}
		createdConvs = append(createdConvs, conv.ID)
		if err := pg.AddChatGroupMember(gid, key, &conv.ID, "member"); err != nil {
			// 半个群比没有群更糟：它会占住"一个 task 至多一个大厅"的名额，
			// 让重试变成 409。
			_ = pg.DeleteChatGroup(gid)
			delConvs()
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
	// v0.11.2（用户报障「群聊发命令没人认领」）：此前消息入库后【零唤醒】——命令
	// 只躺在信箱里等成员"下一轮"，而没有人给成员起轮 ⇒ 永远无人认领。现在入库即
	// 驱动：定向 → 唤醒该成员的会话跑一轮（经 collectGroupMailbox 收到命令，带任务
	// 上下文执行/回答）；广播/未定向 → 唤醒 auto 分发者（hall 会话）派活。
	// 双方回复镜像回群信箱，大厅消息流可见。busy 冲突则放弃本轮触发：消息已在
	// 信箱，上一轮结束后任何新消息都会再触发，不丢。
	if pg != nil && group != nil {
		trigger := "【群信箱】有新的人类命令，请收取群信箱（collectGroupMailbox 通道已注入内容）并处理：确认要做什么，动手执行或在群里回答。"
		if to := strings.TrimSpace(req.ToAgent); to != "" {
			convs, _ := pg.ChatGroupMemberConversations(id)
			if convID, ok := convs[to]; ok && convID > 0 {
				s.wakeHallAgent(group, convID, to, trigger)
			}
		} else if group.ConversationID > 0 {
			s.wakeHallAgent(group, group.ConversationID, "auto", trigger)
		}
	}
	writeJSON(w, 201, msg)
}

// wakeHallAgent 对大厅相关的某个会话跑一轮 agent（异步），并把最终回复镜像回群
// 信箱（from_agent=该 agent，大厅消息流可见）。busy 冲突静默放弃。
func (s *Server) wakeHallAgent(group *db.ChatGroup, convID int64, agentKey, trigger string) {
	conv, err := s.m.pg.GetConversation(convID)
	if err != nil || conv == nil {
		return
	}
	busyKey := s.convBusyKey(convID)
	s.chatMu.Lock()
	if s.chatBusy[busyKey] {
		s.chatMu.Unlock() // 上一轮未完：消息已在信箱，不丢
		return
	}
	s.chatBusy[busyKey] = true
	s.chatMu.Unlock()
	go func() {
		text, rerr := s.runConversationSync(conv, trigger, busyKey)
		if rerr != nil {
			log.Printf("[task-hall] 群 %d 唤醒 %s 失败: %v", group.ID, agentKey, rerr)
			return
		}
		text = strings.TrimSpace(text)
		if text == "" {
			return
		}
		if _, err := s.m.pg.AddChatGroupMessage(&db.ChatGroupMessage{
			GroupID:   group.ID,
			FromAgent: agentKey,
			Kind:      "note",
			Body:      text,
			TaskID:    &group.TaskID,
		}); err != nil {
			log.Printf("[task-hall] 群 %d 镜像 %s 回复失败: %v", group.ID, agentKey, err)
		}
	}()
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
	var status *db.TaskHallStatus
	if st, serr := pg.TaskHallStatusByGroup(groupID, time.Now()); serr == nil {
		status = st
	} else {
		log.Printf("[task-hall] 群 %d 状态计算失败（详情照常返回）: %v", groupID, serr)
	}
	return &taskHallGroupDetail{
		ChatGroup:     g,
		Members:       members,
		DisabledTools: disabled,
		MessageCount:  count,
		HallStatus:    status,
	}, nil
}

// hallTaskForConversation 反查一个会话作为群成员所绑定的任务（v0.11.2）：
// conv → chat_group_members → chat_groups → tasks。查不到（独立聊天/无绑定）⇒ ok=false。
// agent_key='auto' 的 hall 会话同样命中（它是 dispatcher 成员）。
func (s *Server) hallTaskForConversation(convID int64) (taskID, expID int64, ok bool) {
	if s.m == nil || s.m.pg == nil {
		return 0, 0, false
	}
	err := s.m.pg.QueryRow(`
SELECT g.task_id, t.exploration_id
FROM chat_group_members m
JOIN chat_groups g ON g.id = m.group_id
JOIN tasks t ON t.id = g.task_id AND t.deleted_at IS NULL
WHERE m.conversation_id = $1
ORDER BY m.group_id DESC LIMIT 1`, convID).Scan(&taskID, &expID)
	return taskID, expID, err == nil && taskID > 0
}

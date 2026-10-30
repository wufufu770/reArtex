package server

import (
	"encoding/json"
	"log"
	"net/http"

	"github.com/Autumn-27/artex/db"
)

// 群组预算的 operator 接口。
//
// 为什么走 HTTP 而不是 agent 工具：预算是**授权与成本**的决定，属于人（operator）。
// 把它做成 agent 可调用的工具，agent 就能给自己抬预算、自己把自己从 draining
// 放回 active —— 那道闸门就成了摆设。所以本文件里的接口不注册进 tools 表。
//
// 变更必须留痕：预算与状态都是「谁在什么时候把哪一组的额度改成多少」，
// 要能事后追责，所以每次变更都追加一条审计事件（db/audit_chain.go 的哈希链）。

// RegisterTaskHallQuotaRoutes 注册群组预算的 operator 接口。
//
// 为什么要单独一个注册函数而不是把 mux.HandleFunc 写在 server.go 里：路由表是
// 别的单元的文件域（同一 worktree 内并行开发），本单元只能提供 handler 与这个
// 一次性注册入口 —— 属主在注册区调一次即可。
func (s *Server) RegisterTaskHallQuotaRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/task-hall/groups/{id}/quota", s.getTaskHallGroupQuota)
	mux.HandleFunc("PUT /api/task-hall/groups/{id}/quota", s.putTaskHallGroupQuota)
}

// setGroupQuotaRequest 是改预算/状态的请求体。
//
// 两个字段都是可选的：只给 budget_tokens 是改额度，只给 state 是改状态。
type setGroupQuotaRequest struct {
	// BudgetTokens 是本周期的 token 预算。nil 表示不动它。
	BudgetTokens *int64 `json:"budget_tokens"`
	// State 是预算状态：active / draining / parked。空串表示不动它。
	State string `json:"state"`
	// Reason 是变更理由。必填 —— 没有理由的额度变更事后无法追责。
	Reason string `json:"reason"`
}

// getTaskHallGroupQuota 读群组预算：状态、预算、本周期已结算/在途预扣/剩余。
func (s *Server) getTaskHallGroupQuota(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	groupID, ok := pathInt(r, "id")
	if !ok {
		writeErr(w, 400, "群组 id 不合法")
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
	quota, err := pg.GetGroupQuota(groupID)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if quota == nil {
		writeErr(w, 404, "群组未配置预算")
		return
	}
	writeJSON(w, 200, quota)
}

// putTaskHallGroupQuota 改群组预算与/或预算状态。
//
// 理由必填：额度与状态都是授权决定，没有理由的变更事后追不回来。
func (s *Server) putTaskHallGroupQuota(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	groupID, ok := pathInt(r, "id")
	if !ok {
		writeErr(w, 400, "群组 id 不合法")
		return
	}
	var req setGroupQuotaRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "请求体解析失败")
		return
	}
	if req.BudgetTokens == nil && req.State == "" {
		writeErr(w, 400, "预算与状态至少给一个")
		return
	}
	if req.Reason == "" {
		writeErr(w, 400, "必须给出变更理由")
		return
	}
	if req.State != "" && req.State != string(db.QuotaStateActive) &&
		req.State != string(db.QuotaStateDraining) && req.State != string(db.QuotaStateParked) {
		writeErr(w, 400, "预算状态只能是 active / draining / parked")
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

	action := "set_budget"
	if req.State != "" {
		action = "set_state"
	}
	if req.BudgetTokens != nil && req.State != "" {
		action = "set_budget_and_state"
	}
	payload, _ := json.Marshal(map[string]any{
		"group_id":      groupID,
		"budget_tokens": req.BudgetTokens,
		"state":         req.State,
		"remote_addr":   r.RemoteAddr,
	})

	// 变更与留痕的先后：先落变更再追审计。追审计失败时变更已经发生，
	// 硬要回滚会与「预算正在被并发预扣」打架；因此改为如实回报 audited=false，
	// 让操作者知道这一笔没有进哈希链（而不是假装留了痕）。
	if req.BudgetTokens != nil {
		if err := pg.SetGroupQuotaBudget(groupID, *req.BudgetTokens); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
	}
	if req.State != "" {
		if err := pg.SetGroupQuotaState(groupID, db.QuotaState(req.State)); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
	}
	audited := true
	if _, err := pg.AppendAuditEvent(db.AuditEvent{
		Actor:    "operator",
		ToolName: "group_quota",
		Action:   action,
		Reason:   req.Reason,
		Payload:  payload,
	}); err != nil {
		audited = false
		log.Printf("[audit] 追加群组预算变更审计失败 group=%d action=%s: %v", groupID, action, err)
	}

	quota, err := pg.GetGroupQuota(groupID)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"quota": quota, "audited": audited})
}

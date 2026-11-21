package server

// 升级提议 REST 面（0.15.44 #12）：GET 列表（?status=pending）+ POST 裁决。
//
// 裁决语义（SoD：人工批准才建任务）：
//   - approve → 用提议 payload 确定性重放 spawnTaskCore（与 spawn_task 工具
//     同一套校验/建任务/绑会话/launch）→ 状态 converted、落 task_id；
//   - reject → 状态 rejected。CAS：pending 才可裁决，重复裁决不二写。
//
// 批准失败（类别不存在/关联任务无效等）→ 提议保持 pending，错误原样返回——
// 人工可修正参数后由 LLM 重新提议或直接手建任务。

import (
	"encoding/json"
	"net/http"
	"strconv"
)

// registerTaskEscalationRoutes 注册升级提议面（0.15.44）。
func (s *Server) registerTaskEscalationRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/task-escalations", s.taskEscalationsList)
	mux.HandleFunc("POST /api/task-escalations/{id}/decide", s.taskEscalationDecide)
}

// taskEscalationsList GET /api/task-escalations?status=pending
func (s *Server) taskEscalationsList(w http.ResponseWriter, r *http.Request) {
	if s.m == nil || s.m.pg == nil {
		writeErr(w, 500, "存储未就绪")
		return
	}
	items, err := s.m.pg.ListTaskEscalationProposals(r.URL.Query().Get("status"), 100)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"proposals": items, "count": len(items)})
}

// taskEscalationDecide POST /api/task-escalations/{id}/decide
// body {"action":"approve"|"reject"}
func (s *Server) taskEscalationDecide(w http.ResponseWriter, r *http.Request) {
	if s.m == nil || s.m.pg == nil {
		writeErr(w, 500, "存储未就绪")
		return
	}
	id, ok := pathInt(r, "id")
	if !ok {
		writeErr(w, 400, "bad id")
		return
	}
	var req struct {
		Action string `json:"action"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || (req.Action != "approve" && req.Action != "reject") {
		writeErr(w, 400, "action 必填（approve|reject）")
		return
	}
	p, err := s.m.pg.GetTaskEscalationProposal(id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if p == nil {
		writeErr(w, 404, "提议不存在")
		return
	}
	if p.Status != "pending" {
		writeErr(w, 409, "提议已裁决（"+p.Status+"），不重复裁决")
		return
	}

	switch req.Action {
	case "reject":
		changed, err := s.m.pg.DecideTaskEscalationProposal(id, "rejected", "")
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		if !changed {
			writeErr(w, 409, "提议已被并发裁决")
			return
		}
		s.govRecord(p.TaskRef, "task_escalation_decide", "人工驳回升级提议 #"+strconv.FormatInt(id, 10))
		writeJSON(w, 200, map[string]any{"ok": true, "status": "rejected"})
	case "approve":
		var a spawnArgs
		if len(p.Payload) > 0 {
			if jerr := json.Unmarshal(p.Payload, &a); jerr != nil {
				writeErr(w, 500, "提议参数损坏: "+jerr.Error())
				return
			}
		}
		if a.Description == "" {
			a.Description = p.Title
		}
		if a.Goal == "" {
			a.Goal = p.Goal
		}
		t, err := s.spawnTaskCore(r.Context(), a)
		if err != nil {
			// 建任务失败 → 提议保持 pending（人工看到原因后可再批/驳回）。
			writeErr(w, 400, "批准执行失败（提议保持待审）: "+err.Error())
			return
		}
		changed, err := s.m.pg.DecideTaskEscalationProposal(id, "approved", t.ID)
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		if !changed {
			writeErr(w, 409, "提议已被并发裁决（任务 "+t.ID+" 已建立，请核对）")
			return
		}
		s.govRecord(p.TaskRef, "task_escalation_decide",
			"人工批准升级提议 #"+strconv.FormatInt(id, 10)+" → 任务 "+t.ID)
		writeJSON(w, 200, map[string]any{"ok": true, "status": "converted", "task_id": t.ID})
	}
}

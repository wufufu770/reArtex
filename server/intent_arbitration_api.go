package server

// 意图冲突 REST（0.14.1 批次二 E 的后端半边）：
//
//	GET  /api/intents/conflicts?task_id=N[&state=]   冲突清单（含裁决/理由/reopen 数）
//	POST /api/intents/conflicts/{id}/reopen          重开（人工覆盖或新事实；写审计链）
//
// 全部挂 requireAuth（bizRoutes 同层）。前端冲突块在 0.14.2 接入。

import (
	"encoding/json"
	"net/http"
	"strconv"
)

func (s *Server) registerIntentConflictRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/intents/conflicts", s.intentConflictList)
	mux.HandleFunc("POST /api/intents/conflicts/{id}/reopen", s.intentConflictReopen)
}

func (s *Server) intentConflictList(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	taskID, _ := strconv.ParseInt(r.URL.Query().Get("task_id"), 10, 64)
	if taskID <= 0 {
		writeErr(w, 400, "task_id 为必填")
		return
	}
	var eid int64
	if err := pg.QueryRow(`SELECT exploration_id FROM tasks WHERE id=$1`, taskID).Scan(&eid); err != nil || eid <= 0 {
		writeErr(w, 404, "任务不存在")
		return
	}
	conflicts, err := pg.ListConflictsByExploration(eid, r.URL.Query().Get("state"), 200)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"conflicts": conflicts, "total": len(conflicts)})
}

func (s *Server) intentConflictReopen(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, 400, "id 非法")
		return
	}
	var req struct {
		Reason string `json:"reason"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&req)
	}
	if req.Reason == "" {
		writeErr(w, 400, "重开理由必填（写审计链）")
		return
	}
	if err := pg.ReopenConflict(id); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	// 反向联动（0.14.2）：被本仲裁压制的意图回 open（只解 arbiter_deferred）。
	var aNode, bNode int64
	if err := pg.QueryRow(`SELECT a_node_id, b_node_id FROM intent_conflicts WHERE id=$1`, id).Scan(&aNode, &bNode); err == nil {
		_ = pg.ReleaseIntentForArbitration(aNode)
		_ = pg.ReleaseIntentForArbitration(bNode)
	}
	s.appendArbitrationAudit("intent-arbitration", "conflict-reopen",
		"conflict="+strconv.FormatInt(id, 10)+" 人工重开",
		map[string]any{"conflict_id": id, "reason": req.Reason, "resolved_by": "host"})
	writeJSON(w, 200, map[string]any{"ok": true})
}

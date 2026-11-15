package server

// 偏锋运行的 REST 化（0.15.2，按用户裁定：偏锋页要能直接布置任务、可交互）：
//
//	POST /api/edge/run                 发起一次偏锋探索（goal/description/方法论可选）
//	POST /api/edge/tasks/{id}/stop     人工停止（open run → cancelled，写审计）
//
// 发起路径与 edge_run 工具共用宿主函数 StartEdgeExploration——同一条
// CreateTask→edge_mode→StartEdgeRun→launchTask→执行体排队链，不另造第二条。
// REST 与工具同权限面（requireAuth 之内，操作者身份）。

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
)

func (s *Server) registerEdgeRunRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/edge/run", s.edgeRunStart)
	mux.HandleFunc("POST /api/edge/tasks/{id}/stop", s.edgeRunStop)
}

// edgeRunStart POST /api/edge/run。
func (s *Server) edgeRunStart(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Goal          string  `json:"goal"`
		Description   string  `json:"description"`
		MethodologyID string  `json:"methodology_id"`
		CompanyIDs    []int64 `json:"company_ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "请求体非法")
		return
	}
	if req.Goal == "" || req.Description == "" {
		writeErr(w, 400, "goal / description 必填")
		return
	}
	taskID, runID, err := s.StartEdgeExploration(req.Goal, req.Description, req.MethodologyID, req.CompanyIDs, nil)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "task_id": taskID, "run_id": runID})
}

// edgeRunStop POST /api/edge/tasks/{id}/stop：人工停止（cancelled）。
func (s *Server) edgeRunStop(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	taskID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || taskID <= 0 {
		writeErr(w, 400, "task id 非法")
		return
	}
	runID, err := pg.StopOpenEdgeRun(taskID, "cancelled")
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	s.appendEdgeAudit("edge-run", "manual-stop",
		fmt.Sprintf("task=%d run=%d 人工停止", taskID, runID),
		map[string]any{"task_id": taskID, "run_id": runID, "stop_reason": "cancelled"})
	writeJSON(w, 200, map[string]any{"ok": true, "run_id": runID})
}

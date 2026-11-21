package server

// 任务软截止的人工直连面（0.15.42，交接文档 A2a）。
//
// 背景（v0.15.11 远程实锤）：task_drain 工具只绑 auto agent，监督环又对
// 软截止任务永久跳过（旧版）——一旦 drain，人工除重启外无解。本端点给
// 人工一个确定性解除入口：POST /api/tasks/{id}/drain body {"drain":bool}。
//
//   - drain=true：停派新意图、在飞收尾（仅非终态任务；终态 409）
//   - drain=false：恢复派发（幂等，任何时候都允许）
//
// 两条路径都走 engine.Drain（emitActivity【治理】留痕）+ govRecord 审计
// （source 标注人工）。前端「恢复派发」按钮与本端点同源（A3）。

import (
	"encoding/json"
	"net/http"
)

// registerTaskDrainRoutes 注册人工软截止面（0.15.42）。
func (s *Server) registerTaskDrainRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/tasks/{id}/drain", s.taskDrainPost)
}

// taskDrainPost 人工置/清软截止。
func (s *Server) taskDrainPost(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "task not found")
		return
	}
	var req struct {
		Drain  *bool  `json:"drain"`
		Reason string `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Drain == nil {
		writeErr(w, 400, "drain 必填（true=软截止，false=恢复派发）")
		return
	}
	if *req.Drain && isTerminalStatus(t.lifecycleSnapshot().Status) {
		writeErr(w, 409, "任务已终态，无派发面可软截止")
		return
	}
	s.engine.Drain(t.ID, *req.Drain)
	verb := "人工软截止"
	if !*req.Drain {
		verb = "人工恢复派发"
	}
	if reason := trimGov(req.Reason); reason != "" {
		s.govRecord(t.ID, "task_drain", verb+"（"+reason+"）")
	} else {
		s.govRecord(t.ID, "task_drain", verb)
	}
	writeJSON(w, 200, map[string]any{"ok": true, "task_id": t.ID, "drained": s.engine.Drained(t.ID)})
}

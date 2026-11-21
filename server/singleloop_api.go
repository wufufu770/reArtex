package server

// 会话单循环硬锁开关（0.15.44 #12）：POST /api/conversations/{id}/single-loop
// body {"on":true|false}。开启后该会话工具面确定性剥离 spawn_task（物理不可
// 建任务）；升级走 propose_task_escalation 提议卡（人工批准才建任务）。
// 关闭恢复原样（recon 派任务等既有设计能力不受影响）。

import (
	"encoding/json"
	"net/http"
	"strconv"
)

// registerSingleLoopRoutes 注册会话单循环硬锁开关（0.15.44）。
func (s *Server) registerSingleLoopRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/conversations/{id}/single-loop", s.conversationSingleLoopSet)
}

// conversationSingleLoopSet 置/清会话单循环锁。
func (s *Server) conversationSingleLoopSet(w http.ResponseWriter, r *http.Request) {
	if s.m == nil || s.m.pg == nil {
		writeErr(w, 500, "存储未就绪")
		return
	}
	convID, ok := pathInt(r, "id")
	if !ok {
		writeErr(w, 400, "bad conversation id")
		return
	}
	var req struct {
		On *bool `json:"on"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.On == nil {
		writeErr(w, 400, "on 必填（true=锁（剥 spawn_task），false=恢复）")
		return
	}
	if err := s.m.pg.SetConversationSingleLoop(convID, *req.On); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	singleLoopCache.Delete(convID) // 工具面缓存即时失效
	verb := "已开启单循环硬锁（本会话不可直接建任务；升级请走 propose_task_escalation 提议）"
	if !*req.On {
		verb = "已解除单循环硬锁（恢复 spawn_task）"
	}
	writeJSON(w, 200, map[string]any{
		"ok": true, "conversation_id": strconv.FormatInt(convID, 10),
		"single_loop": *req.On, "message": verb,
	})
}

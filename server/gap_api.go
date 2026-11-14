package server

// 覆盖缺口 REST（0.14.3 批次二 F4）：
//
//	GET  /api/tasks/{id}/gaps[&state=]        缺口清单
//	POST /api/tasks/{id}/gaps/{gid}/close     人工闭合（理由必填，写审计链）
//
// 计算面在调度器重扫轮（ComputeCoverageGaps，60s 水位），REST 只读账 + 人工闭合。

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"

	"github.com/Autumn-27/artex/db"
)

func (s *Server) registerGapRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/tasks/{id}/gaps", s.gapList)
	mux.HandleFunc("POST /api/tasks/{id}/gaps/{gid}/close", s.gapClose)
}

func (s *Server) gapList(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	taskID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || taskID <= 0 {
		writeErr(w, 400, "task id 非法")
		return
	}
	gaps, err := pg.ListCoverageGaps(taskID, r.URL.Query().Get("state"), 0)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"gaps": gaps, "total": len(gaps)})
}

func (s *Server) gapClose(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	gid, err := strconv.ParseInt(r.PathValue("gid"), 10, 64)
	if err != nil || gid <= 0 {
		writeErr(w, 400, "gid 非法")
		return
	}
	var req struct {
		Reason string `json:"reason"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&req)
	}
	if req.Reason == "" {
		writeErr(w, 400, "闭合理由必填（写审计链）")
		return
	}
	if err := pg.CloseCoverageGap(gid); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	b, _ := json.Marshal(map[string]any{"gap_id": gid, "reason": req.Reason})
	if _, err := pg.AppendAuditEvent(db.AuditEvent{
		Actor:    "operator",
		ToolName: "gap",
		Action:   "gap-close",
		Reason:   "gap=" + strconv.FormatInt(gid, 10) + " 人工闭合",
		Payload:  b,
		Source:   "coverage-gap",
	}); err != nil {
		log.Printf("[gap] 审计链写入失败（忽略）: %v", err)
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

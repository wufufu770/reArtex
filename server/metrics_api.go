package server

// ---------- 运行度量 API（P0 评估基线）----------
//
// GET /api/metrics/run?task_id=N   —— run 快照 JSON（tokens by lane / $ / judge 单列 /
//                                     blocked 数 / planner 扫描构成）
// GET /api/metrics/prices          —— 读单价表
// PUT /api/metrics/prices          —— 覆盖一个模型档的单价（经济配置，可运维编辑）
//
// 前端暂无页面（F 线后续）；接口供脚本与 curl 使用，与 v3.1 §4「快照 JSON 齐全」对齐。

import (
	"net/http"
	"strconv"

	"github.com/Autumn-27/artex/db"
)

func (s *Server) registerMetricsRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/metrics/run", s.metricsRun)
	mux.HandleFunc("GET /api/metrics/prices", s.metricsPricesGet)
	mux.HandleFunc("PUT /api/metrics/prices", s.metricsPricesPut)
}

func (s *Server) metricsRun(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	taskID, err := strconv.ParseInt(r.URL.Query().Get("task_id"), 10, 64)
	if err != nil || taskID <= 0 {
		writeErr(w, 400, "task_id 必须为正整数")
		return
	}
	snap, err := pg.RunMetricsSnapshot(taskID)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, snap)
}

func (s *Server) metricsPricesGet(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	prices, err := pg.ProfilePrices()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"prices": prices})
}

func (s *Server) metricsPricesPut(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	var req struct {
		ProfileName string  `json:"profile_name"`
		Currency    string  `json:"currency"`
		PerMInput   float64 `json:"per_m_input"`
		PerMOutput  float64 `json:"per_m_output"`
		PerMCacheRd float64 `json:"per_m_cache_read"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if req.ProfileName == "" {
		writeErr(w, 400, "profile_name 必填")
		return
	}
	if req.PerMInput < 0 || req.PerMOutput < 0 || req.PerMCacheRd < 0 {
		writeErr(w, 400, "单价不能为负")
		return
	}
	if err := pg.SaveProfilePrice(db.ProfilePrice{
		ProfileName:   req.ProfileName,
		Currency:      req.Currency,
		PerMInput:     req.PerMInput,
		PerMOutput:    req.PerMOutput,
		PerMCacheRead: req.PerMCacheRd,
	}); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

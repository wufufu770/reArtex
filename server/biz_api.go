package server

// 业务建模 × 偏锋探索的 REST 通道（合并施工 M1/M7/M8）。
//
// 全部只读（除人工裁决一个 POST）。范围语义：
//   - 业务图读：task_id 参数可选——传了按任务范围读（agent/任务详情 tab 用），
//     不传按平台范围读（scopepolicy 默认 ScopeEmpty = 什么都看不到，不回退
//     全局——与 ListByKindScoped 同一口径）。宿主/API 面是「操作者可信」面，
//     但范围读的授权边照锚点语义执行：这是给前端业务图页与任务详情 tab 的
//     数据源。
//   - 差异读：按公司/任务列（差异不锚资产，授权边界是建模任务本身）。
//   - 人工裁决 POST：宿主侧裁决写 + 审计链（与 biz_adjudicate 工具同一条
//     db 层唯一写函数；操作者改判与 agent 建议裁决走同一落笔点，审计口径
//     只有一份）。
//   - edge-coverage 读：格点覆盖与判据进度 + stop_reason 统计（M8 评测口径：
//     doctrine_complete 与兜底类在统计上可区分）。

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/Autumn-27/artex/db"
)

// bizGraphHandler 处理 GET /api/business-graph。
func (s *Server) bizGraphHandler(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	q := r.URL.Query()
	companyID, _ := strconv.ParseInt(q.Get("company_id"), 10, 64)
	taskID, _ := strconv.ParseInt(q.Get("task_id"), 10, 64)
	if companyID <= 0 {
		writeErr(w, 400, "company_id 为必填")
		return
	}
	g, err := pg.ListBizGraphScoped(taskID, companyID, q.Get("layer"), q.Get("node_type"), 2000)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, g)
}

// bizTaskGraphHandler 处理 GET /api/tasks/{id}/biz-graph（任务详情 tab 用）。
// 从任务行取 company_id + taskID 双条件，范围按任务。
func (s *Server) bizTaskGraphHandler(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	taskID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || taskID <= 0 {
		writeErr(w, 400, "task id 非法")
		return
	}
	var companyID int64
	if err := pg.QueryRow(`SELECT COALESCE(company_id,0) FROM tasks WHERE id=$1 AND deleted_at IS NULL`, taskID).Scan(&companyID); err != nil {
		writeErr(w, 404, "task 不存在")
		return
	}
	g, err := pg.ListBizGraphScoped(taskID, companyID, r.URL.Query().Get("layer"), "", 2000)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	designN, behaviorN, openDiffs, _ := pg.BizGraphSummary(taskID)
	writeJSON(w, 200, map[string]any{
		"nodes": g.Nodes, "edges": g.Edges,
		"summary": map[string]any{
			"design_nodes": designN, "behavior_nodes": behaviorN, "open_diffs": openDiffs,
		},
	})
}

// bizDiffsHandler 处理 GET /api/business-graph/diffs。
func (s *Server) bizDiffsHandler(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	q := r.URL.Query()
	companyID, _ := strconv.ParseInt(q.Get("company_id"), 10, 64)
	taskID, _ := strconv.ParseInt(q.Get("task_id"), 10, 64)
	limit, _ := strconv.Atoi(q.Get("limit"))
	if companyID <= 0 && taskID <= 0 {
		writeErr(w, 400, "company_id 或 task_id 至少传一个")
		return
	}
	// 只传 task_id 时从任务行补 company_id：ListBizDiffs 的 WHERE company_id=$1
	// 在 companyID=0 时永远空（运行验证抓到的第一个 API bug——集成测试用的是
	// (company>0, task) 组合没暴露它）。
	if companyID <= 0 && taskID > 0 {
		_ = pg.QueryRow(`SELECT COALESCE(company_id,0) FROM tasks WHERE id=$1`, taskID).Scan(&companyID)
	}
	diffs, err := pg.ListBizDiffs(companyID, taskID, q.Get("status"), q.Get("severity"), limit)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"diffs": diffs})
}

// bizDiffDecisionHandler 处理 POST /api/business-graph/diffs/{id}/decision：
// 人工裁决（confirmed / false_positive / open 重开）。宿主侧裁决写 + 审计链。
func (s *Server) bizDiffDecisionHandler(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	diffID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || diffID <= 0 {
		writeErr(w, 400, "diff id 非法")
		return
	}
	var req struct {
		Status string `json:"status"`
		Reason string `json:"reason"`
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err := json.Unmarshal(body, &req); err != nil {
		writeErr(w, 400, "请求体非法")
		return
	}
	switch req.Status {
	case "confirmed", "false_positive", "open":
	default:
		writeErr(w, 400, "status 只接受 confirmed|false_positive|open")
		return
	}
	if strings.TrimSpace(req.Reason) == "" {
		writeErr(w, 400, "reason 为必填：裁决必须可追溯")
		return
	}
	if err := pg.UpdateBizDiffStatus(diffID, req.Status, nil); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	b, _ := json.Marshal(map[string]any{"diff_id": diffID, "status": req.Status})
	// 审计失败只记日志（链是增值层不回滚业务写，与 appendBizAudit 同款纪律）。
	if _, err := pg.AppendAuditEvent(db.AuditEvent{
		Actor:    "operator",
		ToolName: "biz",
		Action:   "biz_diff." + req.Status,
		Reason:   req.Reason,
		Payload:  b,
		Source:   "biz-diff-decision",
	}); err != nil {
		log.Printf("[biz] 人工裁决审计链写入失败（忽略）: %v", err)
	}
	writeJSON(w, 200, map[string]any{"ok": true, "diff_id": diffID, "status": req.Status})
}

// taskEdgeCoverageHandler 处理 GET /api/tasks/{id}/edge-coverage（M8 评测口径的数据源）。
func (s *Server) taskEdgeCoverageHandler(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	taskID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || taskID <= 0 {
		writeErr(w, 400, "task id 非法")
		return
	}
	cells, err := pg.ListEdgeCells(taskID)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	run, err := pg.OpenEdgeRun(taskID)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	edgeMode, methodologyID, _ := pg.TaskEdgeFields(taskID)
	// stop 统计（M8）：本任务全部运行的 stop_reason 分布——判据触发率
	//（doctrine_complete 占比）是自主终止是否真实发生的唯一直接度量。
	stopStats := map[string]int{}
	rows, err := pg.Query(`SELECT stop_reason, COUNT(*) FROM edge_runs WHERE task_id=$1 GROUP BY stop_reason`, taskID)
	if err == nil {
		for rows.Next() {
			var reason string
			var n int
			if err := rows.Scan(&reason, &n); err == nil {
				stopStats[reason] = n
			}
		}
		rows.Close()
	}
	writeJSON(w, 200, map[string]any{
		"task_id": taskID, "edge_mode": edgeMode, "methodology_id": methodologyID,
		"cells": cells, "open_run": run, "stop_stats": stopStats,
	})
}

// bizRoutes 注册业务建模/偏锋探索的路由（在 requireAuth 之内）。
func (s *Server) bizRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/business-graph", s.bizGraphHandler)
	mux.HandleFunc("GET /api/business-graph/diffs", s.bizDiffsHandler)
	mux.HandleFunc("POST /api/business-graph/diffs/{id}/decision", s.bizDiffDecisionHandler)
	mux.HandleFunc("GET /api/tasks/{id}/biz-graph", s.bizTaskGraphHandler)
	mux.HandleFunc("GET /api/tasks/{id}/edge-coverage", s.taskEdgeCoverageHandler)
}

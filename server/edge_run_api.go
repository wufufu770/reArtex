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
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
)

func (s *Server) registerEdgeRunRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/edge/run", s.edgeRunStart)
	mux.HandleFunc("POST /api/edge/tasks/{id}/stop", s.edgeRunStop)
}

// edgeRunStart POST /api/edge/run。
// 0.15.13 硬化：MaxBytesReader 限请求体、goal/description 修剪空白。
func (s *Server) edgeRunStart(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
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
	req.Goal = strings.TrimSpace(req.Goal)
	req.Description = strings.TrimSpace(req.Description)
	req.MethodologyID = strings.TrimSpace(req.MethodologyID)
	if req.Goal == "" || req.Description == "" {
		writeErr(w, 400, "goal / description 必填")
		return
	}
	taskID, runID, err := s.StartEdgeExploration(req.Goal, req.Description, req.MethodologyID, req.CompanyIDs, nil)
	if err != nil {
		// 入参校验类失败（方法论未注册/类别种子缺失等）回 400；原始错误进日志。
		log.Printf("[edge] run start failed: %v", err)
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "task_id": taskID, "run_id": runID})
}

// edgeRunStop POST /api/edge/tasks/{id}/stop：人工停止（cancelled）。
//
// 0.15.13 修 zombie run：此前只把 edge_runs 行翻成 cancelled，底层任务原样在跑
// （worker 继续领意图烧 token——外部审查实锤）。现在联动正常任务控制链暂停任务
// （取消在飞运行 + paused 落库，与人工暂停同一条链）；edge_runs 行照旧翻
// cancelled 供偏锋 UI 判态。错误分类：无 open run=404（语义性），后端错误=500
// 不裸泄 err.Error()。
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
		if errors.Is(err, db.ErrNoOpenEdgeRun) {
			writeErr(w, 404, err.Error())
		} else {
			log.Printf("[edge] task %d stop open run 失败: %v", taskID, err)
			writeErr(w, 500, "停止运行失败")
		}
		return
	}
	// 底层任务联动暂停：engine 取消在飞 worker/planner，任务进 paused。
	// 任务可能已终态/已暂停/不在管理器（刚建未入册）——那种情况幂等无关，
	// 失败只记日志；edge_runs 翻行已完成，不回滚。
	if t, ok := s.m.Task(strconv.FormatInt(taskID, 10)); ok && t != nil {
		if _, cerr := s.applyTaskControlWithCause(t, "pause", agent.AbortPausedByUser); cerr != nil {
			log.Printf("[edge] task %d 联动暂停未执行（任务可能已终态/已暂停）: %v", taskID, cerr)
		}
	}
	s.appendEdgeAudit("edge-run", "manual-stop",
		fmt.Sprintf("task=%d run=%d 人工停止（任务联动暂停）", taskID, runID),
		map[string]any{"task_id": taskID, "run_id": runID, "stop_reason": "cancelled"})
	writeJSON(w, 200, map[string]any{"ok": true, "run_id": runID})
}

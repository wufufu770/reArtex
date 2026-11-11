package server

// ---------- 传承图 API（P5）----------
// GET    /api/legacy/nodes[?state=draft|accepted|retired]  列表
// POST   /api/legacy/nodes/{id}/review                     人工评审（accepted/retired/恢复 draft）
// DELETE /api/legacy/nodes/{id}                            删除（draft/retired；accepted 409；审计快照先行）
// 页面（web/src/app/(main)/legacy）消费；评审是人在环路的确定性闸。

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/Autumn-27/artex/db"
)

func (s *Server) registerLegacyRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/legacy/nodes", s.legacyList)
	mux.HandleFunc("POST /api/legacy/nodes/{id}/review", s.legacyReview)
	mux.HandleFunc("DELETE /api/legacy/nodes/{id}", s.legacyDelete)
	// 手动重蒸馒头出口（联动测试发现的缺陷修复）：蒸馏在任务 done 后异步跑，
	// 进程重启会把进行中的蒸馏杀掉且永不重试（内存一次性闸重启即清，但触发点
	// 已经走过）——人工补一次蒸馏是唯一的补救路径。
	mux.HandleFunc("POST /api/legacy/distill/{taskID}", s.legacyDistill)
}

func (s *Server) legacyList(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	nodes, err := pg.ListLegacyNodes(strings.TrimSpace(r.URL.Query().Get("state")), 100)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"nodes": nodes})
}

func (s *Server) legacyReview(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, 400, "id 不合法")
		return
	}
	var req struct {
		State    string `json:"state"`
		Reviewer string `json:"reviewer"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, "参数解析失败")
		return
	}
	state := strings.TrimSpace(req.State)
	// 三门晋级（X2，方案 v10 §6 T3）：经验线的「晋级」= draft → accepted 转移，
	// 同样过三门评估（结构完整 / 不撞证伪池 / 实战证据 wins）。不过门返回
	// 409 + 缺哪门；无 --force（退役/恢复 draft 不评估 —— 那两个方向只收缩
	// 可见性，不产生新信任）。
	if state == "accepted" {
		pass, missing := EvaluateExperiencePromotion(pg, id)
		if !pass {
			writeJSON(w, 409, map[string]any{
				"ok":            false,
				"error":         "经验晋级未过三门评估",
				"missing_gates": missing,
				"node_id":       id,
			})
			return
		}
	}
	if err := pg.ReviewLegacyNode(id, state, strings.TrimSpace(req.Reviewer)); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// legacyDistill 手动触发一次任务的终局蒸馏（重启杀掉异步蒸馏后的补救入口）。
// 只对非删除的任务有效；重复调用由内存一次性闸挡住（同进程内不重复蒸馏）。
func (s *Server) legacyDistill(w http.ResponseWriter, r *http.Request) {
	if s.m == nil || s.m.pg == nil {
		writeErr(w, 503, "数据库未连接")
		return
	}
	taskID, ok := pathInt(r, "taskID")
	if !ok || taskID <= 0 {
		writeErr(w, 400, "taskID 不合法")
		return
	}
	t, exists := s.m.Task(strconv.FormatInt(taskID, 10))
	if !exists || t == nil {
		writeErr(w, 404, "任务不存在或已删除")
		return
	}
	if !db.IsTerminal(t.Status) {
		writeErr(w, 409, "任务尚未进入终态，不能蒸馏")
		return
	}
	s.DistillLegacyExperience(t)
	writeJSON(w, 202, map[string]any{"ok": true, "task_id": t.ID})
}

// legacyDelete 删除一条经验（缺陷 D2）。DELETE 带 body 在部分客户端不友好，
// 操作者身份走 query 参数（审计要留人）。
func (s *Server) legacyDelete(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, 400, "id 不合法")
		return
	}
	operator := strings.TrimSpace(r.URL.Query().Get("operator"))
	if operator == "" {
		writeErr(w, 400, "删除必须留操作者身份（?operator=）")
		return
	}
	if err := pg.DeleteLegacyNode(id, operator); err != nil {
		if errors.Is(err, db.ErrLegacyAcceptedPin) {
			writeErr(w, 409, err.Error())
			return
		}
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

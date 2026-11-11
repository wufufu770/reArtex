package server

// ---------- 传承图 API（P5）----------
// GET    /api/legacy/nodes[?state=draft|accepted|retired]  列表
// POST   /api/legacy/nodes/{id}/review                     人工评审（accepted/retired/恢复 draft）
// DELETE /api/legacy/nodes/{id}                            删除（draft/retired；accepted 409；审计快照先行）
// 页面（web/src/app/(main)/legacy）消费；评审是人在环路的确定性闸。

import (
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/Autumn-27/artex/db"
)

func (s *Server) registerLegacyRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/legacy/nodes", s.legacyList)
	mux.HandleFunc("POST /api/legacy/nodes/{id}/review", s.legacyReview)
	mux.HandleFunc("DELETE /api/legacy/nodes/{id}", s.legacyDelete)
	// v0.10.1 三门自动评估出口：评审页对 draft 卡直接展示各门 pass/fail 明细
	// （用户要求「自动走流程，显示在这里」），而不是等点了接受才弹一个含糊的
	// 「未过三门」。
	mux.HandleFunc("GET /api/legacy/nodes/{id}/gates", s.legacyGates)
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

// legacyGates 返回一条经验的三门评估明细（结构完整 / 历史复盘 / 实战证据）。
// 只读：不判状态、不写任何东西——评审页拿它在 draft 卡上直接画三门徽章。
func (s *Server) legacyGates(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, 400, "id 不合法")
		return
	}
	pass, missing := EvaluateExperiencePromotion(pg, id)
	writeJSON(w, 200, map[string]any{
		"pass":          pass,
		"missing_gates": missing,
		// 死锁说明要写给看的人：门3（实战证据）的 wins 只在接受注入后产生——
		// 新经验 wins=0 是常态，人工接受即授权（越级留痕），实战表现由权重卡
		// 后续跟踪。这不是评审人的失误，是门的次序。
		"note": "门3（实战证据）的 wins 来自「接受注入后」的命中与因果计胜——新经验恒为 0。" +
			"人工接受即授权进池（缺门以越级留痕记录），实战表现由经验权重卡后续跟踪。",
	})
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
		// v0.10.1 以人为本：三门在这条线上是「建议 + 留痕」，不是硬拒——
		// 门3 的实战 wins 只在接受注入后产生，新经验恒为 0，硬拒会让
		// draft→accepted 整条路死锁（评审人永远点不进池）。人工接受可越级，
		// 但必须带理由并记录在案（legacy_audit）。
		Override bool   `json:"override"`
		Reason   string `json:"reason"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, "参数解析失败")
		return
	}
	state := strings.TrimSpace(req.State)
	// 三门晋级（X2，方案 v10 §6 T3）：经验线的「晋级」= draft → accepted 转移。
	// v0.10.1 口径修正（以人为本，与 E1 改判同一哲学）：三门在这条线上是
	// 「自动评估 + 明细展示 + 越级留痕」，不是硬拒——门3 的实战 wins 只在
	// 接受注入后才产生，新经验恒为 0，硬拒 = draft→accepted 整条路死锁。
	// 结构门（标题/摘要非空）仍硬拦：空壳没有可接受的东西。
	// 退役/恢复 draft 不评估——那两个方向只收缩可见性，不产生新信任。
	if state == "accepted" {
		pass, missing := EvaluateExperiencePromotion(pg, id)
		if !pass && GateMissing(missing, PromotionGateStructure) {
			writeJSON(w, 409, map[string]any{
				"ok":            false,
				"error":         "结构门不过：标题与摘要必须非空（空壳不可接受）",
				"missing_gates": missing,
				"node_id":       id,
			})
			return
		}
		if !pass {
			if !req.Override || strings.TrimSpace(req.Reason) == "" {
				writeJSON(w, 409, map[string]any{
					"ok":                false,
					"error":             "三门评估未全过——可带理由越级接受（记录在案）",
					"missing_gates":     missing,
					"override_required": true,
					"node_id":           id,
				})
				return
			}
			// 越级接受：缺门清单 + 理由 + 操作人全量落审计。
			if aerr := pg.AppendLegacyAudit("override_accept", id, map[string]any{
				"missing_gates": missing,
				"reason":        strings.TrimSpace(req.Reason),
			}, strings.TrimSpace(req.Reviewer)); aerr != nil {
				// 审计写失败不阻塞接受，但必须喊出来（fail-loud，不留无痕越级）。
				log.Printf("[legacy] 经验 #%d 越级接受审计写失败: %v", id, aerr)
			}
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

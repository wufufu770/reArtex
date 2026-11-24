package server

// ti_api.go —— Task Intelligence task-scoped API（v2.1 统一架构优化增补 §0.11
// 「API 与工具契约建议」的第一阶段，0.15.69，批次 TI-11）。
//
// §0.11 契约要求与本实现的对应：
//   - API 以任务为主资源：全部端点挂在 /api/tasks/{id}/ 前缀下，task identity
//     来自路径并由服务器端解析（写操作不信任模型/请求体传入的 scope）；
//   - 写操作服务器端重载校验：POST hypotheses 先 GetTask 验任务存在且未删除，
//     指纹由服务器算（HypothesisFingerprint），scope 水位由服务器取——请求体
//     只提供 statement 与来源描述；
//   - capability_run 等执行类工具端点**本阶段不开放**：能力执行必须走统一
//     工具安全管道（ActionGate/SoD/审计），其执行 envelope 接线是下一阶段
//     （TI-11 第二阶段），宁缺毋假；
//   - 删除类动作不进本 API（§0.11：删除不属于普通 capability 自动权限）。
//
// Agent 工具契约（task_context_get / capability_list / capability_run /
// hypothesis_create / hypothesis_update / business_model_get /
// business_model_update / evidence_link / finding_validate）：名称与语义以本
// 文件 + §0.11 表格为契约源；工具表种子与装配归第二阶段（与 provider 接线
// 同批），本阶段先以 REST 面暴露同一份数据。
//
// 现状边界（如实）：快照端点当前返回的分节全部显式 unavailable——Provider
// 注入（scope/assets/exploration/biz/facts/evidence/findings/history 的
// server 侧实现）是第二阶段工作；本端点先钉住契约形状（ti.context.v1），
// 不伪造空数据。

import (
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/internal/taskintelligence"
)

// tiRoutes 注册 Task Intelligence 的 task-scoped 路由（在 requireAuth 之内）。
func (s *Server) tiRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/tasks/{id}/intelligence", s.taskIntelligenceHandler)
	mux.HandleFunc("GET /api/tasks/{id}/hypotheses", s.taskHypothesesListHandler)
	mux.HandleFunc("POST /api/tasks/{id}/hypotheses", s.taskHypothesisCreateHandler)
	mux.HandleFunc("GET /api/tasks/{id}/model-deltas", s.taskModelDeltasHandler)
	mux.HandleFunc("GET /api/tasks/{id}/capability-runs", s.taskCapabilityRunsHandler)
	mux.HandleFunc("GET /api/tasks/{id}/timeline", s.taskTimelineHandler)
}

// parseTaskPathValue 解析并校验路径中的任务 id（task identity 的唯一来源）。
func parseTaskPathValue(r *http.Request) (int64, bool, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		return 0, false, errInvalidTaskID
	}
	return id, true, nil
}

type tiErr string

const errInvalidTaskID tiErr = "task id 非法"

func (e tiErr) Error() string { return string(e) }

// requireTask 要求任务存在且未删除（写操作的服务器端重载校验；§0.11）。
func (s *Server) requireTask(w http.ResponseWriter, r *http.Request) (int64, *db.Task, bool) {
	pg := s.pg(w)
	if pg == nil {
		return 0, nil, false
	}
	taskID, ok, err := parseTaskPathValue(r)
	if err != nil || !ok {
		writeErr(w, 400, string(errInvalidTaskID))
		return 0, nil, false
	}
	tk, err := pg.GetTask(taskID) // GetTask 已过滤 deleted_at IS NULL
	if err != nil {
		writeErr(w, 500, err.Error())
		return 0, nil, false
	}
	if tk == nil {
		writeErr(w, 404, "任务不存在或已删除")
		return 0, nil, false
	}
	return taskID, tk, true
}

// taskIntelligenceHandler 处理 GET /api/tasks/{id}/intelligence：
// §0.11 建议端点。返回 ti.context.v1 契约形状的快照（Provider 未接线时各分节
// 显式 unavailable）+ 领域对象计数。
func (s *Server) taskIntelligenceHandler(w http.ResponseWriter, r *http.Request) {
	taskID, _, ok := s.requireTask(w, r)
	if !ok {
		return
	}
	pg := s.pg(w)
	// Provider 未接线：Builder 各字段为 nil → 分节显式 Unavailable（不伪造空数据）。
	b := taskintelligence.SnapshotBuilder{}
	snap, err := b.Build(r.Context(), taskID)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	hyps, _ := pg.ListHypothesesByTask(taskID, 200)
	runs, _ := pg.ListCapabilityExecutionsByTask(taskID, 200)
	facts, _ := pg.ListFactsByTask(taskID, 200)
	evs, _ := pg.ListEvidenceByTask(taskID, 200)
	writeJSON(w, 200, map[string]any{
		"contract_version": snap.ContractVersion,
		"task_id":          taskID,
		"generated_at":     snap.GeneratedAt,
		"unavailable":      snap.Unavailable(),
		"snapshot":         snap,
		"counts": map[string]int{
			"hypotheses": len(hyps), "capability_runs": len(runs),
			"facts": len(facts), "evidence": len(evs),
		},
		"providers_wired": false, // 第二阶段接线后翻真——前端据此区分空态与未接线
	})
}

// taskHypothesesListHandler 处理 GET /api/tasks/{id}/hypotheses。
func (s *Server) taskHypothesesListHandler(w http.ResponseWriter, r *http.Request) {
	taskID, _, ok := s.requireTask(w, r)
	if !ok {
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	hs, err := s.pg(w).ListHypothesesByTask(taskID, limit)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"hypotheses": hs, "total": len(hs)})
}

// taskHypothesisCreateHandler 处理 POST /api/tasks/{id}/hypotheses（§0.11 建议端点）。
// 服务器端职责：重载任务 → 计算指纹（HypothesisFingerprint）→ 取 scope 水位 →
// 去重插入（指纹命中返回既有 id）。请求体不带也不信任任何 scope/scope_version 字段。
func (s *Server) taskHypothesisCreateHandler(w http.ResponseWriter, r *http.Request) {
	taskID, _, ok := s.requireTask(w, r)
	if !ok {
		return
	}
	pg := s.pg(w)
	var req struct {
		Statement string `json:"statement"`
		Source    string `json:"source"`
		// source_type/capability_id 由调用方声明来源；缺省 manual（人工/主 Agent 直接提出）。
		SourceType   string `json:"source_type"`
		CapabilityID string `json:"capability_id"`
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err := json.Unmarshal(body, &req); err != nil {
		writeErr(w, 400, "请求体非法 JSON")
		return
	}
	if req.Statement == "" {
		writeErr(w, 400, "statement 为必填")
		return
	}
	if req.SourceType == "" {
		req.SourceType = "manual"
	}
	scopeVersion, err := pg.ScopeVersionForTask(taskID)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	h := &db.Hypothesis{
		TaskID:       taskID,
		Fingerprint:  taskintelligence.HypothesisFingerprint(taskID, req.Statement),
		Statement:    req.Statement,
		Source:       req.Source,
		SourceType:   req.SourceType,
		CapabilityID: req.CapabilityID,
		Status:       db.HypothesisProposed,
		ScopeVersion: scopeVersion,
	}
	id, deduped, err := pg.InsertHypothesis(h)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"id": id, "deduped": deduped, "fingerprint": h.Fingerprint})
}

// taskModelDeltasHandler 处理 GET /api/tasks/{id}/model-deltas（§0.11 建议端点）。
// ModelDelta 不另建库——直接复用 biz_layer_diffs 的任务维度查询（TI-05 口径）。
func (s *Server) taskModelDeltasHandler(w http.ResponseWriter, r *http.Request) {
	taskID, _, ok := s.requireTask(w, r)
	if !ok {
		return
	}
	diffs, err := s.pg(w).ListBizDiffsP(db.ListBizDiffsOpts{TaskID: taskID, Limit: 200})
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"model_deltas": diffs, "total": len(diffs)})
}

// taskCapabilityRunsHandler 处理 GET /api/tasks/{id}/capability-runs。
func (s *Server) taskCapabilityRunsHandler(w http.ResponseWriter, r *http.Request) {
	taskID, _, ok := s.requireTask(w, r)
	if !ok {
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	runs, err := s.pg(w).ListCapabilityExecutionsByTask(taskID, limit)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"capability_runs": runs, "total": len(runs)})
}

// timelineEntry 是时间线的统一条目（type 区分对象来源，ts 排序键）。
type timelineEntry struct {
	Type    string          `json:"type"` // hypothesis / capability_run / evidence / fact / event
	ID      int64           `json:"id"`
	Ref     string          `json:"ref"` // 语义引用（执行 envelope / 事件 id / 指纹）
	Summary string          `json:"summary"`
	TS      time.Time       `json:"ts"`
	Extra   json.RawMessage `json:"extra,omitempty"`
}

// taskTimelineHandler 处理 GET /api/tasks/{id}/timeline（§0.11 建议端点）：
// 五类领域对象按时间合并（新→旧，封顶 200 条）——「能从任务时间线追踪完整链路」
// 的只读数据面（TI-13 验收 TI-4 阶段的 API 侧前置）。
func (s *Server) taskTimelineHandler(w http.ResponseWriter, r *http.Request) {
	taskID, _, ok := s.requireTask(w, r)
	if !ok {
		return
	}
	pg := s.pg(w)
	entries := []timelineEntry{}
	if hs, err := pg.ListHypothesesByTask(taskID, 100); err == nil {
		for _, h := range hs {
			entries = append(entries, timelineEntry{Type: "hypothesis", ID: h.ID, Ref: h.Fingerprint,
				Summary: h.Statement, TS: h.CreatedAt})
		}
	}
	if runs, err := pg.ListCapabilityExecutionsByTask(taskID, 100); err == nil {
		for _, e := range runs {
			entries = append(entries, timelineEntry{Type: "capability_run", ID: e.ID, Ref: e.ExecutionID,
				Summary: e.CapabilityID + " " + e.Status, TS: e.CreatedAt})
		}
	}
	if evs, err := pg.ListEvidenceByTask(taskID, 100); err == nil {
		for _, e := range evs {
			entries = append(entries, timelineEntry{Type: "evidence", ID: e.ID, Ref: e.ContentRef,
				Summary: e.Kind + " " + e.Support, TS: e.ObservedAt})
		}
	}
	if fs, err := pg.ListFactsByTask(taskID, 100); err == nil {
		for _, f := range fs {
			entries = append(entries, timelineEntry{Type: "fact", ID: f.ID,
				Summary: f.Subject + " " + f.Predicate + " " + f.Object, TS: f.ObservedAt})
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].TS.After(entries[j].TS) })
	if len(entries) > 200 {
		entries = entries[:200]
	}
	writeJSON(w, 200, map[string]any{"timeline": entries, "total": len(entries)})
}

package server

// ---------- 拦截种类表 / 建议队列 / 影子对照 的 HTTP API（方案 v4.1 §2）----------

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/Autumn-27/artex/internal/sodpolicy"
)

func (s *Server) registerSodCategoryRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/sod/categories", s.sodCategoryList)
	mux.HandleFunc("POST /api/sod/categories", s.sodCategoryCreate)
	mux.HandleFunc("POST /api/sod/categories/{id}", s.sodCategoryUpdate)
	mux.HandleFunc("DELETE /api/sod/categories/{id}", s.sodCategoryDelete)
	mux.HandleFunc("GET /api/sod/category-suggestions", s.sodSuggestionList)
	mux.HandleFunc("POST /api/sod/category-suggestions/{id}/decide", s.sodSuggestionDecide)
	mux.HandleFunc("GET /api/sod/shadow", s.sodShadowList)
	mux.HandleFunc("GET /api/sod/merged-gate", s.sodMergedGateState)
	mux.HandleFunc("POST /api/sod/merged-gate", s.sodMergedGateSet)
}

// sodCategoryList 列全部种类（含停用与锁定行）。
func (s *Server) sodCategoryList(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	rows, err := pg.ListSodCategories()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"categories": rows, "version": pg.SodCategoriesVersion()})
}

// sodCategoryCreate 新建种类（custom 正则或引用内建识别器）。
func (s *Server) sodCategoryCreate(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	var req struct {
		Name       string `json:"name"`
		Kind       string `json:"kind"`
		Pattern    string `json:"pattern"`
		Action     string `json:"action"`
		CapPerTask int    `json:"cap_per_task"`
		Fallback   string `json:"fallback"`
		Priority   int    `json:"priority"`
		Reason     string `json:"reason"`
		CreatedBy  string `json:"created_by"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, "参数解析失败")
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		writeErr(w, 400, "name 必填")
		return
	}
	if req.Kind == "" {
		req.Kind = "custom"
	}
	if req.Kind != "custom" && req.Kind != "builtin" {
		writeErr(w, 400, "kind 只能是 custom|builtin")
		return
	}
	if strings.TrimSpace(req.CreatedBy) == "" {
		writeErr(w, 400, "必须留操作者身份（created_by）")
		return
	}
	if req.Action == "" {
		req.Action = sodpolicy.CategoryActionAuditor
	}
	if req.Action != sodpolicy.CategoryActionHardDeny && req.Action != sodpolicy.CategoryActionAsk &&
		req.Action != sodpolicy.CategoryActionAuditor && req.Action != sodpolicy.CategoryActionPass {
		writeErr(w, 400, "action 只能是 hard_deny|ask|auditor|pass")
		return
	}
	if req.Fallback == "" {
		req.Fallback = sodpolicy.CategoryFallbackAsk
	}
	if req.Kind == "custom" {
		if strings.TrimSpace(req.Pattern) == "" {
			writeErr(w, 400, "custom 种类必须给 pattern")
			return
		}
		// 破坏特征不可经 custom 正则沉淀为 pass——那是对锁定基线的绕道。
		if req.Action == sodpolicy.CategoryActionPass {
			if reason, hit := sodpolicy.BaselineHit("Bash", []byte(req.Pattern)); reason != "" {
				writeErr(w, 409, "pattern 含锁定基线特征（"+hit+"）不可配置为放行")
				return
			}
		}
	}
	if req.Priority == 0 {
		req.Priority = 40
	}
	cat := sodpolicy.SodCategory{
		Name: req.Name, Kind: req.Kind, Pattern: req.Pattern, Action: req.Action,
		CapPerTask: req.CapPerTask, Fallback: req.Fallback, Locked: false,
		Enabled: true, Priority: req.Priority, Reason: strings.TrimSpace(req.Reason),
		CreatedBy: strings.TrimSpace(req.CreatedBy),
	}
	row, err := pg.CreateSodCategory(cat)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "category": row})
}

// sodCategoryUpdate 改动作/上限/兜底/启停/优先级；锁定行拒绝改动作。
func (s *Server) sodCategoryUpdate(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, ok := pathInt(r, "id")
	if !ok || id <= 0 {
		writeErr(w, 400, "id 不合法")
		return
	}
	var req struct {
		Action     string `json:"action"`
		CapPerTask *int   `json:"cap_per_task"`
		Fallback   string `json:"fallback"`
		Enabled    *bool  `json:"enabled"`
		Priority   *int   `json:"priority"`
		Reason     *string `json:"reason"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, "参数解析失败")
		return
	}
	cur, err := pg.GetSodCategory(id)
	if err != nil {
		writeErr(w, 404, err.Error())
		return
	}
	action := req.Action
	if action == "" {
		action = cur.Action
	}
	cap := cur.CapPerTask
	if req.CapPerTask != nil {
		cap = *req.CapPerTask
	}
	fallback := req.Fallback
	if fallback == "" {
		fallback = cur.Fallback
	}
	if action != cur.Action && cur.Locked {
		writeErr(w, 409, "种类「"+cur.Name+"」是锁定基线，动作不可修改")
		return
	}
	row, err := pg.UpdateSodCategory(id, action, cap, fallback, req.Enabled, req.Priority, req.Reason)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "category": row})
}

// sodCategoryDelete 删除种类（锁定行拒绝）。
func (s *Server) sodCategoryDelete(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, ok := pathInt(r, "id")
	if !ok || id <= 0 {
		writeErr(w, 400, "id 不合法")
		return
	}
	if err := pg.DeleteSodCategory(id); err != nil {
		writeErr(w, 409, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// sodSuggestionList 列 auditor 的种类建议。
func (s *Server) sodSuggestionList(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	status := strings.TrimSpace(r.URL.Query().Get("status"))
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	rows, err := pg.ListSodCategorySuggestions(status, limit)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"suggestions": rows})
}

// sodSuggestionDecide 采纳（落成种类行）/忽略一条建议。
func (s *Server) sodSuggestionDecide(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, ok := pathInt(r, "id")
	if !ok || id <= 0 {
		writeErr(w, 400, "id 不合法")
		return
	}
	var req struct {
		Decision string `json:"decision"` // adopted | dismissed
		Operator string `json:"operator"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, "参数解析失败")
		return
	}
	if strings.TrimSpace(req.Operator) == "" {
		writeErr(w, 400, "必须留操作者身份（operator）")
		return
	}
	sugs, err := pg.ListSodCategorySuggestions("", 200)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	var target *struct {
		ID      int64
		Name    string
		Kind    string
		Pattern string
		Action  string
		Reason  string
	}
	for i := range sugs {
		if sugs[i].ID == id {
			target = &struct {
				ID      int64
				Name    string
				Kind    string
				Pattern string
				Action  string
				Reason  string
			}{sugs[i].ID, sugs[i].Name, sugs[i].Kind, sugs[i].Pattern, sugs[i].Action, sugs[i].Reason}
			break
		}
	}
	if target == nil {
		writeErr(w, 404, "建议不存在")
		return
	}
	if req.Decision == "adopted" {
		if _, err := pg.CreateSodCategory(sodpolicy.SodCategory{
			Name: target.Name, Kind: target.Kind, Pattern: target.Pattern,
			Action: target.Action, Fallback: sodpolicy.CategoryFallbackAsk,
			Enabled: true, Priority: 40, Reason: "auditor 建议采纳：" + target.Reason,
			CreatedBy: strings.TrimSpace(req.Operator),
		}); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
	}
	if err := pg.DecideSodCategorySuggestion(id, req.Decision, strings.TrimSpace(req.Operator)); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "adopted": req.Decision == "adopted"})
}

// sodShadowList 近期影子对照差异。
func (s *Server) sodShadowList(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	rows, err := pg.ListSodShadowDiffs(limit)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"diffs": rows, "mode": s.mergedGateMode()})
}

// sodMergedGateState 读三态开关当前值。
func (s *Server) sodMergedGateState(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	writeJSON(w, 200, map[string]any{"mode": s.mergedGateMode()})
}

// sodMergedGateSet 推进/回退三态开关（off→shadow→enforce；一键回退 off）。
func (s *Server) sodMergedGateSet(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	var req struct {
		Mode     string `json:"mode"`
		Operator string `json:"operator"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, "参数解析失败")
		return
	}
	if strings.TrimSpace(req.Operator) == "" {
		writeErr(w, 400, "必须留操作者身份（operator）")
		return
	}
	switch strings.TrimSpace(req.Mode) {
	case sodpolicy.MergedGateOff, sodpolicy.MergedGateShadow, sodpolicy.MergedGateEnforce:
	default:
		writeErr(w, 400, "mode 只能是 off|shadow|enforce")
		return
	}
	if err := pg.SetSetting(sodpolicy.SettingMergedGate, strings.TrimSpace(req.Mode)); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "mode": s.mergedGateMode()})
}

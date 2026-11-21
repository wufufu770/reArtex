package server

// 任务执行档位 REST（0.15.17）：
//
//	GET   /api/tasks/{id}/execution-profile  当前档位（未切过=默认档）+ 可选项
//	PATCH /api/tasks/{id}/execution-profile  {difficulty?, mode?} 切档
//
// 生效纪律：只动请求级参数（ReasoningEffort/压缩窗口帽），system 与 tools 零改动
// ——切档零缓存代价（见 agent/execution_profile.go）。difficulty/mode 传 "auto"
// 时按任务书文本自动判定（AssessAutoProfile）。档位不改变任何 lane 的 LLM profile
// 身份（那由 agents.llm_profile_id 绑定裁定）——监督角色锁定天然成立。

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
)

func (s *Server) registerExecutionProfileRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/tasks/{id}/execution-profile", s.executionProfileGet)
	mux.HandleFunc("PATCH /api/tasks/{id}/execution-profile", s.executionProfilePatch)
}

func (s *Server) executionProfileGet(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "task not found")
		return
	}
	taskIDInt, _ := strconv.ParseInt(t.ID, 10, 64)
	p, ok := s.engine.ExecutionProfileFor(taskIDInt)
	if !ok {
		p = agent.ResolveExecutionProfile(agent.DefaultDifficulty, agent.DefaultMode)
	}
	autoOn := false
	if s.m.pg != nil {
		autoOn = s.m.pg.GetBool(settingAutoScaling, false)
	}
	// 任务授权面（EngagementPolicy，0.15.26）：与执行档位同卡展示，可选项随档返回。
	engagement := db.EngagementAuthorizedActive
	if s.m.pg != nil {
		engagement = s.m.pg.TaskEngagementProfile(taskIDInt)
	}
	writeJSON(w, 200, map[string]any{
		"profile":      p,
		"auto_scaling": autoOn,
		"engagement_profile": engagement,
		"options": map[string]any{
			"difficulties":       []string{"auto", "simple", "medium", "hard"},
			"modes":              []string{"auto", "fast", "balanced", "super_heavy"},
			"engagement_profiles": []string{db.EngagementAuthorizedActive, db.EngagementPassiveRecon, db.EngagementStrictManual},
		},
	})
}

func (s *Server) executionProfilePatch(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "task not found")
		return
	}
	if s.engine.IsDeleting(t.ID) {
		writeErr(w, 409, "任务正在删除，无法切换执行档位")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	var req struct {
		Difficulty        *string `json:"difficulty,omitempty"`
		Mode              *string `json:"mode,omitempty"`
		EngagementProfile *string `json:"engagement_profile,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeErr(w, http.StatusRequestEntityTooLarge, "请求体过大")
			return
		}
		writeErr(w, 400, "bad json: "+err.Error())
		return
	}
	taskIDInt, _ := strconv.ParseInt(t.ID, 10, 64)
	// 当前档位为基线（缺省字段保持不变）。
	cur, ok := s.engine.ExecutionProfileFor(taskIDInt)
	if !ok {
		cur = agent.ResolveExecutionProfile(agent.DefaultDifficulty, agent.DefaultMode)
	}
	diff, mode := cur.Difficulty, cur.Mode
	if req.Difficulty != nil {
		diff = agent.TaskDifficulty(*req.Difficulty)
	}
	if req.Mode != nil {
		mode = agent.ExecutionMode(*req.Mode)
	}
	// auto → 文本启发式判定（目标数/资产图规模创建时点通常未知，按 0 供给——
	// 文本判据兜底，宁去中间档）。
	diff, mode = agent.AssessAutoProfile(t.Goal, t.Description, 0, 0, diff, mode)
	// 显式非法值在 Resolve 里回落默认（档位是运营旋钮，坏输入不炸任务）。
	p := agent.ResolveExecutionProfile(diff, mode)
	s.engine.SetExecutionProfile(taskIDInt, p)
	// 任务授权面（0.15.26）：独立于算力档，只传了 engagement 时算力档保持不变。
	// 非法值 400（授权面坏值不静默回落，与 createTask 同口径）。
	engagementChanged := ""
	if req.EngagementProfile != nil {
		normalized, err := db.NormalizeEngagementProfile(*req.EngagementProfile)
		if err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		if s.m.pg == nil {
			writeErr(w, 500, "数据库未装配")
			return
		}
		if err := s.m.pg.SetTaskEngagementProfile(taskIDInt, normalized); err != nil {
			writeErr(w, 500, "授权面写入失败: "+err.Error())
			return
		}
		engagementChanged = normalized
		s.engine.emitActivity(t, db.Activity{Worker: "system", Kind: "text",
			Summary: "任务授权面切换：" + engagementLabel(normalized) +
				"（合并闸自动放行出口按档收紧；审计与审批页可查每次确认）"})
	}
	s.engine.emitActivity(t, db.Activity{Worker: "system", Kind: "text",
		Summary: "执行档位切换：难度=" + string(p.Difficulty) + " 模式=" + string(p.Mode) +
			"（推理强度 " + effortLabel(p.ReasoningEffort) + "；只调请求级参数，上下文与工具面不变）"})
	writeJSON(w, 200, map[string]any{"ok": true, "task_id": t.ID, "profile": p, "engagement_profile": engagementChanged})
}

// engagementLabel 把授权面枚举渲染成人话（活动播报/UI 共用）。
func engagementLabel(p string) string {
	switch p {
	case db.EngagementPassiveRecon:
		return "passive_recon（对目标的出网动作先经人工确认）"
	case db.EngagementStrictManual:
		return "strict_manual（除平台内安全读写外一律先经人工确认）"
	}
	return "authorized_active（默认：范围内动作自动进行）"
}

func effortLabel(e string) string {
	switch e {
	case "low":
		return "低"
	case "medium":
		return "中"
	case "high":
		return "高"
	}
	return e
}

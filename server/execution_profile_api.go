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
	writeJSON(w, 200, map[string]any{
		"profile":      p,
		"auto_scaling": autoOn,
		"options": map[string]any{
			"difficulties": []string{"auto", "simple", "medium", "hard"},
			"modes":        []string{"auto", "fast", "balanced", "super_heavy"},
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
		Difficulty *string `json:"difficulty,omitempty"`
		Mode       *string `json:"mode,omitempty"`
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
	s.engine.emitActivity(t, db.Activity{Worker: "system", Kind: "text",
		Summary: "执行档位切换：难度=" + string(p.Difficulty) + " 模式=" + string(p.Mode) +
			"（推理强度 " + effortLabel(p.ReasoningEffort) + "；只调请求级参数，上下文与工具面不变）"})
	writeJSON(w, 200, map[string]any{"ok": true, "task_id": t.ID, "profile": p})
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

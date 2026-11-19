package server

// 0.15.17 执行档位 REST 回归：GET 默认档 / PATCH 切档与持久化水合 / auto 判定 /
// 非法值回落（运营旋钮不炸任务）。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/agent"
)

func TestExecutionProfileAPI(t *testing.T) {
	s, _ := taskHallTestServer(t)
	task := killTestTask(t, s, "执行档位 REST 测试")
	taskIDInt, _ := strconv.ParseInt(task.ID, 10, 64)

	mux := http.NewServeMux()
	s.registerExecutionProfileRoutes(mux)
	do := func(method, body string) map[string]any {
		req := httptest.NewRequest(method, "/api/tasks/"+task.ID+"/execution-profile", strings.NewReader(body))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("%s %s → %d %s", method, body, rec.Code, rec.Body.String())
		}
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	// GET 未切档：默认档（medium/balanced）。
	get1 := do(http.MethodGet, "")
	p1 := get1["profile"].(map[string]any)
	if p1["difficulty"] != "medium" || p1["mode"] != "balanced" {
		t.Fatalf("默认档不符: %v", p1)
	}

	// PATCH 切 super_heavy：effort=high + 熔断预算；持久化（新 Engine 水合可读）。
	patch := do(http.MethodPatch, `{"mode":"super_heavy"}`)
	p2 := patch["profile"].(map[string]any)
	if p2["mode"] != "super_heavy" || p2["reasoning_effort"] != "high" {
		t.Fatalf("切档结果不符: %v", p2)
	}
	restarted := NewEngine(s.m)
	p, ok := restarted.ExecutionProfileFor(taskIDInt)
	if !ok || p.Mode != agent.ModeSuperHeavy || p.ReasoningEffort != "high" {
		t.Fatalf("跨 Engine 水合失败: %+v ok=%v", p, ok)
	}

	// PATCH auto：按任务书文本判定（killTestTask 描述中性 → 默认档兜底）。
	patch2 := do(http.MethodPatch, `{"difficulty":"auto","mode":"auto"}`)
	p3 := patch2["profile"].(map[string]any)
	if p3["difficulty"] == "" || p3["mode"] == "" {
		t.Fatalf("auto 判定无产出: %v", p3)
	}

	// 非法值整体回落默认档（运营旋钮不炸任务）。
	patch3 := do(http.MethodPatch, `{"difficulty":"bogus","mode":"bogus"}`)
	p4 := patch3["profile"].(map[string]any)
	if p4["difficulty"] != string(agent.DefaultDifficulty) || p4["mode"] != string(agent.DefaultMode) {
		t.Fatalf("非法值应回落默认: %v", p4)
	}

	// 需求体超限/坏 JSON → 400。
	req := httptest.NewRequest(http.MethodPatch, "/api/tasks/"+task.ID+"/execution-profile", strings.NewReader("{bad"))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Fatalf("坏 JSON 应 400，got %d", rec.Code)
	}
}

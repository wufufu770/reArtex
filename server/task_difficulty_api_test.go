package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
)

// TestTaskDifficultyAPI 钉住 C-01B 的创建/修改契约，并确认执行档字段 difficulty 原样可用。
func TestTaskDifficultyAPI(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer m.Close()
	td := t.TempDir()
	s := New(testServerCtx(t), m, td, td, td)
	h := s.Handler()
	tok, err := signJWT(s.jwtKey)
	if err != nil {
		t.Fatal(err)
	}
	do := func(method, path, body string) (int, map[string]any) {
		t.Helper()
		var r *http.Request
		if body != "" {
			r = httptest.NewRequest(method, path, bytes.NewReader([]byte(body)))
			r.Header.Set("Content-Type", "application/json")
		} else {
			r = httptest.NewRequest(method, path, nil)
		}
		r.Header.Set("Authorization", "Bearer "+tok)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	stored := func(id string) (string, string) {
		t.Helper()
		n, err := strconv.ParseInt(id, 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		row, err := m.pg.GetTask(n)
		if err != nil || row == nil {
			t.Fatalf("GetTask %s: %+v err=%v", id, row, err)
		}
		return row.Difficulty, row.DifficultyReason
	}
	audits := func(id string) int {
		t.Helper()
		var n int
		if err := m.pg.QueryRow(`SELECT count(*) FROM audit_events WHERE action='task.difficulty' AND payload::jsonb->>'task_id'=$1`, id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// 不传难度、也不传执行档：201，任务难度 medium，不设执行档。
	code, out := do(http.MethodPost, "/api/tasks", `{"description":"c01b-plain","goal":"看看这个站点"}`)
	if code != 201 {
		t.Fatalf("plain create: %d %v", code, out)
	}
	plainID, _ := out["id"].(string)
	if plainID == "" {
		t.Fatalf("plain create 缺少 id: %v", out)
	}
	if _, ok := out["difficulty"]; ok {
		t.Fatalf("响应不应出现 difficulty 键（那是执行档请求字段）: %v", out["difficulty"])
	}
	if out["task_difficulty"] != db.TaskDifficultyMedium || out["suggested_difficulty"] != agent.LevelMedium {
		t.Fatalf("plain 建议/落库: %v", out)
	}
	if reason, _ := out["suggested_difficulty_reason"].(string); reason == "" {
		t.Fatal("建议理由应非空")
	}
	if got, why := stored(plainID); got != db.TaskDifficultyMedium || why != "" {
		t.Fatalf("plain 库内 %q / %q", got, why)
	}
	if n, err := strconv.ParseInt(plainID, 10, 64); err != nil {
		t.Fatal(err)
	} else if _, ok := s.engine.ExecutionProfileFor(n); ok {
		t.Fatal("未传执行档不应写入执行档")
	}

	// 现有创建路径：difficulty=auto 仍是执行档。目标含「支付」时执行档启发式仍升到 hard，
	// 任务难度列保持默认 medium，建议值另给 hard。
	code, out = do(http.MethodPost, "/api/tasks", `{"description":"c01b-exec-auto","goal":"测试支付接口","difficulty":"auto","mode":"auto"}`)
	if code != 201 {
		t.Fatalf("exec auto create: %d %v", code, out)
	}
	autoID, _ := out["id"].(string)
	if out["task_difficulty"] != db.TaskDifficultyMedium || out["suggested_difficulty"] != agent.LevelHard {
		t.Fatalf("auto 执行档不应写成任务难度: %v", out)
	}
	if reason, _ := out["suggested_difficulty_reason"].(string); reason == "" {
		t.Fatal("支付接口的建议理由应非空")
	}
	if got, _ := stored(autoID); got != db.TaskDifficultyMedium {
		t.Fatalf("auto 请求的任务难度应仍是 medium，得到 %q", got)
	}
	autoNum, _ := strconv.ParseInt(autoID, 10, 64)
	profile, ok := s.engine.ExecutionProfileFor(autoNum)
	if !ok || profile.Difficulty != agent.DifficultyHard || profile.Mode != agent.ModeBalanced {
		t.Fatalf("执行档启发式应仍是 hard/balanced，得到 ok=%v %+v", ok, profile)
	}

	// 显式执行档 hard，不带 task_difficulty：任务难度仍是 medium。
	code, out = do(http.MethodPost, "/api/tasks", `{"description":"c01b-exec-hard","goal":"常规检查","difficulty":"hard","mode":"balanced"}`)
	if code != 201 {
		t.Fatalf("exec hard create: %d %v", code, out)
	}
	hardID, _ := out["id"].(string)
	if got, _ := stored(hardID); got != db.TaskDifficultyMedium {
		t.Fatalf("执行档 hard 不应写入任务难度，得到 %q", got)
	}
	hardNum, _ := strconv.ParseInt(hardID, 10, 64)
	profile, ok = s.engine.ExecutionProfileFor(hardNum)
	if !ok || profile.Difficulty != agent.DifficultyHard || profile.Mode != agent.ModeBalanced {
		t.Fatalf("执行档应保持 hard/balanced，得到 ok=%v %+v", ok, profile)
	}

	// 旧字段传 expert：执行档不认识它，按原逻辑回落默认，不能 400，也不能当成任务难度落库。
	code, out = do(http.MethodPost, "/api/tasks", `{"description":"c01b-exec-expert","goal":"常规检查","difficulty":"expert"}`)
	if code != 201 {
		t.Fatalf("exec expert 应仍 201，得到 %d %v", code, out)
	}
	legacyID, _ := out["id"].(string)
	if got, _ := stored(legacyID); got != db.TaskDifficultyMedium {
		t.Fatalf("difficulty=expert 不是任务难度，库内得到 %q", got)
	}
	legacyNum, _ := strconv.ParseInt(legacyID, 10, 64)
	profile, ok = s.engine.ExecutionProfileFor(legacyNum)
	if !ok || profile.Difficulty != agent.DifficultyMedium {
		t.Fatalf("非法执行档应回落 medium，得到 ok=%v %+v", ok, profile)
	}

	// 用户用 task_difficulty 覆盖建议：建议仍是 hard，落库是 easy。
	code, out = do(http.MethodPost, "/api/tasks", `{"description":"c01b-override","goal":"测试支付接口","task_difficulty":"easy","difficulty_reason":"  用户只要摸底  "}`)
	if code != 201 {
		t.Fatalf("override create: %d %v", code, out)
	}
	overID, _ := out["id"].(string)
	if out["task_difficulty"] != agent.LevelEasy || out["suggested_difficulty"] != agent.LevelHard || out["difficulty_reason"] != "用户只要摸底" {
		t.Fatalf("覆盖结果: %v", out)
	}
	if got, why := stored(overID); got != db.TaskDifficultyEasy || why != "用户只要摸底" {
		t.Fatalf("覆盖落库 %q / %q", got, why)
	}

	code, out = do(http.MethodPost, "/api/tasks", `{"description":"c01b-insane","goal":"x","task_difficulty":"insane"}`)
	if code != 400 {
		t.Fatalf("insane 应 400，得到 %d %v", code, out)
	}
	var leaked int
	if err := m.pg.QueryRow(`SELECT count(*) FROM tasks WHERE description='c01b-insane'`).Scan(&leaked); err != nil {
		t.Fatal(err)
	}
	if leaked != 0 {
		t.Fatalf("非法难度不应留下任务，实际 %d", leaked)
	}

	if audits(overID) != 0 {
		t.Fatal("创建本身不应写 task.difficulty 审计")
	}
	code, out = do(http.MethodPatch, "/api/tasks/"+overID, `{"task_difficulty":"expert","difficulty_reason":"改为专家档"}`)
	if code != 200 {
		t.Fatalf("patch: %d %v", code, out)
	}
	if out["task_difficulty"] != agent.LevelExpert || out["difficulty_reason"] != "改为专家档" {
		t.Fatalf("patch 响应: %v", out)
	}
	if out["suggested_difficulty"] != agent.LevelHard {
		t.Fatalf("改难度不应改掉按原文计算的建议: %v", out["suggested_difficulty"])
	}
	if got, why := stored(overID); got != db.TaskDifficultyExpert || why != "改为专家档" {
		t.Fatalf("patch 落库 %q / %q", got, why)
	}
	if audits(overID) != 1 {
		t.Fatalf("难度变更应有 1 条审计，得到 %d", audits(overID))
	}

	code, out = do(http.MethodPatch, "/api/tasks/"+overID, `{"task_difficulty":"insane"}`)
	if code != 400 {
		t.Fatalf("patch insane 应 400，得到 %d %v", code, out)
	}
	if got, _ := stored(overID); got != db.TaskDifficultyExpert {
		t.Fatalf("非法 patch 不应改库，得到 %q", got)
	}
	if audits(overID) != 1 {
		t.Fatalf("非法 patch 不应再写审计，得到 %d", audits(overID))
	}

	code, out = do(http.MethodPatch, "/api/tasks/"+overID, `{"name":"只改名称"}`)
	if code != 200 {
		t.Fatalf("rename: %d %v", code, out)
	}
	if audits(overID) != 1 {
		t.Fatalf("只改名称不应写难度审计，得到 %d", audits(overID))
	}
	if got, _ := stored(overID); got != db.TaskDifficultyExpert {
		t.Fatalf("改名称不应动难度，得到 %q", got)
	}
}

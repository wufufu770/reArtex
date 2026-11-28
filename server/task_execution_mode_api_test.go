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

// TestTaskExecutionModeAPI 钉住 C-02C 的创建/修改契约。
// 资源模式走 task_execution_mode，不占用执行档的 difficulty / mode。
func TestTaskExecutionModeAPI(t *testing.T) {
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
		r := httptest.NewRequest(method, path, bytes.NewReader([]byte(body)))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Authorization", "Bearer "+tok)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	rowOf := func(id string) *db.Task {
		t.Helper()
		n, err := strconv.ParseInt(id, 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		row, err := m.pg.GetTask(n)
		if err != nil || row == nil {
			t.Fatalf("GetTask %s: %+v err=%v", id, row, err)
		}
		return row
	}
	modeAudits := func(id string) int {
		t.Helper()
		var n int
		if err := m.pg.QueryRow(`SELECT count(*) FROM audit_events WHERE action='task.execution_mode' AND payload::jsonb->>'task_id'=$1`, id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// 不传资源模式：空串跟随难度。普通目标落 medium → 默认和生效都是 balanced。
	code, out := do(http.MethodPost, "/api/tasks", `{"description":"c02c-plain","goal":"看看这个站点"}`)
	if code != 201 {
		t.Fatalf("plain create: %d %v", code, out)
	}
	plainID, _ := out["id"].(string)
	if _, ok := out["difficulty"]; ok {
		t.Fatalf("响应不应出现 difficulty 键: %v", out["difficulty"])
	}
	if _, ok := out["mode"]; ok {
		t.Fatalf("响应不应出现 mode 键（那是执行档请求字段）: %v", out["mode"])
	}
	if out["task_execution_mode"] != "" || out["default_execution_mode"] != string(agent.ModeBalanced) || out["effective_execution_mode"] != string(agent.ModeBalanced) {
		t.Fatalf("plain 模式: %v", out)
	}
	if out["retest_scope"] != agent.RetestScopeHighPlus || out["require_oracle"] != false {
		t.Fatalf("plain 复测建议: %v", out)
	}
	if rowOf(plainID).ExecutionMode != "" {
		t.Fatalf("plain 库内应为空串，得到 %q", rowOf(plainID).ExecutionMode)
	}
	if n, _ := strconv.ParseInt(plainID, 10, 64); func() bool { _, ok := s.engine.ExecutionProfileFor(n); return ok }() {
		t.Fatal("未传执行档不应写入 0.15.17 执行档")
	}
	code, out = do(http.MethodGet, "/api/tasks/"+plainID, "")
	if code != 200 {
		t.Fatalf("get task: %d %v", code, out)
	}
	if out["task_execution_mode"] != "" || out["default_execution_mode"] != string(agent.ModeBalanced) || out["effective_execution_mode"] != string(agent.ModeBalanced) {
		t.Fatalf("详情应带回资源模式三键: %v", out)
	}

	// 难度 easy、不覆盖：默认 fast，生效 fast，库内仍是空串。
	code, out = do(http.MethodPost, "/api/tasks", `{"description":"c02c-easy","goal":"收集子域","task_difficulty":"easy"}`)
	if code != 201 {
		t.Fatalf("easy create: %d %v", code, out)
	}
	easyID, _ := out["id"].(string)
	if out["task_difficulty"] != db.TaskDifficultyEasy || out["default_execution_mode"] != string(agent.ModeFast) || out["effective_execution_mode"] != string(agent.ModeFast) || out["task_execution_mode"] != "" {
		t.Fatalf("easy 默认模式: %v", out)
	}
	if rowOf(easyID).ExecutionMode != "" {
		t.Fatal("不覆盖不应把 fast 烘焙进列")
	}

	// 手动把 easy 改成 super_heavy：默认仍是 fast，生效是 super_heavy。
	code, out = do(http.MethodPost, "/api/tasks", `{"description":"c02c-override","goal":"收集子域","task_difficulty":"easy","task_execution_mode":"  super_heavy  "}`)
	if code != 201 {
		t.Fatalf("override create: %d %v", code, out)
	}
	overID, _ := out["id"].(string)
	if out["task_execution_mode"] != string(agent.ModeSuperHeavy) || out["default_execution_mode"] != string(agent.ModeFast) || out["effective_execution_mode"] != string(agent.ModeSuperHeavy) {
		t.Fatalf("覆盖响应: %v", out)
	}
	if out["retest_scope"] != agent.RetestScopeAll || out["require_oracle"] != true {
		t.Fatalf("super_heavy 应标注全量复测和 oracle，且不因此拒绝创建: %v", out)
	}
	if rowOf(overID).ExecutionMode != db.TaskExecutionModeSuperHeavy || rowOf(overID).Difficulty != db.TaskDifficultyEasy {
		t.Fatalf("覆盖落库 mode=%q difficulty=%q", rowOf(overID).ExecutionMode, rowOf(overID).Difficulty)
	}
	if n, _ := strconv.ParseInt(overID, 10, 64); func() bool { _, ok := s.engine.ExecutionProfileFor(n); return ok }() {
		t.Fatal("task_execution_mode 不应写入 0.15.17 执行档")
	}
	if modeAudits(overID) != 0 {
		t.Fatal("创建本身不应写 task.execution_mode 审计")
	}

	// 旧创建请求：difficulty + mode 仍是执行档，不写资源模式列。
	code, out = do(http.MethodPost, "/api/tasks", `{"description":"c02c-legacy","goal":"常规检查","difficulty":"hard","mode":"fast"}`)
	if code != 201 {
		t.Fatalf("legacy create: %d %v", code, out)
	}
	legacyID, _ := out["id"].(string)
	if rowOf(legacyID).ExecutionMode != "" || rowOf(legacyID).Difficulty != db.TaskDifficultyMedium {
		t.Fatalf("执行档不应写入资源模式或任务难度: mode=%q difficulty=%q", rowOf(legacyID).ExecutionMode, rowOf(legacyID).Difficulty)
	}
	if out["task_execution_mode"] != "" || out["effective_execution_mode"] != string(agent.ModeBalanced) {
		t.Fatalf("执行档 fast 不应变成生效资源模式: %v", out)
	}
	legacyNum, _ := strconv.ParseInt(legacyID, 10, 64)
	profile, ok := s.engine.ExecutionProfileFor(legacyNum)
	if !ok || profile.Difficulty != agent.DifficultyHard || profile.Mode != agent.ModeFast {
		t.Fatalf("执行档应仍是 hard/fast，得到 ok=%v %+v", ok, profile)
	}

	for _, bad := range []string{"auto", "simple", "medium", "hard", "FAST", "expert", "insane"} {
		desc := "c02c-bad-" + bad
		code, out = do(http.MethodPost, "/api/tasks", `{"description":"`+desc+`","goal":"x","task_execution_mode":"`+bad+`"}`)
		if code != 400 {
			t.Fatalf("模式 %q 应 400，得到 %d %v", bad, code, out)
		}
		var leaked int
		if err := m.pg.QueryRow(`SELECT count(*) FROM tasks WHERE description=$1`, desc).Scan(&leaked); err != nil {
			t.Fatal(err)
		}
		if leaked != 0 {
			t.Fatalf("非法模式 %q 不应留下任务，实际 %d", bad, leaked)
		}
	}

	code, out = do(http.MethodPatch, "/api/tasks/"+easyID, `{"task_execution_mode":"super_heavy"}`)
	if code != 200 {
		t.Fatalf("patch: %d %v", code, out)
	}
	if out["task_execution_mode"] != string(agent.ModeSuperHeavy) || out["default_execution_mode"] != string(agent.ModeFast) || out["effective_execution_mode"] != string(agent.ModeSuperHeavy) {
		t.Fatalf("patch 响应: %v", out)
	}
	if rowOf(easyID).ExecutionMode != db.TaskExecutionModeSuperHeavy || rowOf(easyID).Difficulty != db.TaskDifficultyEasy {
		t.Fatalf("patch 不应改难度: %+v", rowOf(easyID))
	}
	if modeAudits(easyID) != 1 {
		t.Fatalf("模式变更应有 1 条审计，得到 %d", modeAudits(easyID))
	}
	var oldMode, newMode string
	if err := m.pg.QueryRow(`SELECT payload::jsonb->>'old_execution_mode', payload::jsonb->>'new_execution_mode' FROM audit_events WHERE action='task.execution_mode' AND payload::jsonb->>'task_id'=$1`, easyID).Scan(&oldMode, &newMode); err != nil {
		t.Fatal(err)
	}
	if oldMode != "" || newMode != string(agent.ModeSuperHeavy) {
		t.Fatalf("审计 payload %q → %q", oldMode, newMode)
	}

	code, out = do(http.MethodPatch, "/api/tasks/"+easyID, `{"task_execution_mode":"super_heavy"}`)
	if code != 200 {
		t.Fatalf("重复 patch: %d %v", code, out)
	}
	if modeAudits(easyID) != 1 {
		t.Fatalf("相同值不应再写审计，得到 %d", modeAudits(easyID))
	}

	code, out = do(http.MethodPatch, "/api/tasks/"+easyID, `{"task_execution_mode":"auto"}`)
	if code != 400 {
		t.Fatalf("非法 patch 应 400，得到 %d %v", code, out)
	}
	if rowOf(easyID).ExecutionMode != db.TaskExecutionModeSuperHeavy {
		t.Fatalf("非法 patch 不应改库，得到 %q", rowOf(easyID).ExecutionMode)
	}
	if modeAudits(easyID) != 1 {
		t.Fatalf("非法 patch 不应写审计，得到 %d", modeAudits(easyID))
	}

	code, out = do(http.MethodPatch, "/api/tasks/"+easyID, `{"task_execution_mode":"  "}`)
	if code != 200 {
		t.Fatalf("清空: %d %v", code, out)
	}
	if out["task_execution_mode"] != "" || out["effective_execution_mode"] != string(agent.ModeFast) || rowOf(easyID).ExecutionMode != "" {
		t.Fatalf("清空应回到跟随难度: 响应 %v 库 %q", out, rowOf(easyID).ExecutionMode)
	}
	if modeAudits(easyID) != 2 {
		t.Fatalf("清空应再写 1 条审计，得到 %d", modeAudits(easyID))
	}

	// 只改难度：资源模式列不动，生效模式跟着新难度走，不写资源模式审计。
	before := modeAudits(easyID)
	code, out = do(http.MethodPatch, "/api/tasks/"+easyID, `{"task_difficulty":"expert"}`)
	if code != 200 {
		t.Fatalf("改难度: %d %v", code, out)
	}
	if out["default_execution_mode"] != string(agent.ModeSuperHeavy) || out["effective_execution_mode"] != string(agent.ModeSuperHeavy) || out["task_execution_mode"] != "" {
		t.Fatalf("难度变化应改变生效模式: %v", out)
	}
	if modeAudits(easyID) != before {
		t.Fatal("只改难度不应写资源模式审计")
	}

	code, out = do(http.MethodPatch, "/api/tasks/"+easyID, `{"name":"只改名称"}`)
	if code != 200 {
		t.Fatalf("rename: %d %v", code, out)
	}
	if modeAudits(easyID) != before || rowOf(easyID).ExecutionMode != "" {
		t.Fatal("只改名称不应动资源模式")
	}
}

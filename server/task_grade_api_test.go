package server

import (
	"bytes"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
)

// TestTaskGradeAPI 钉住 C-03C：终态后 GET 带回评级，两次一致；
// 改 finding 不改评级；重新评级才变，并写审计。非终态拒绝。
func TestTaskGradeAPI(t *testing.T) {
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
	gradeAudits := func(id string) int {
		t.Helper()
		var n int
		if err := m.pg.QueryRow(`SELECT count(*) FROM audit_events WHERE action='task.grade' AND payload::jsonb->>'task_id'=$1`, id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	code, out := do(http.MethodPost, "/api/tasks", `{"description":"c03c-grade-http","goal":"看看评级"}`)
	if code != 201 {
		t.Fatalf("create: %d %v", code, out)
	}
	httpID, _ := out["id"].(string)
	if out["grade"] != "" || out["grade_summary"] != "" || out["grade_score"] != nil {
		t.Fatalf("新建应未评：%v", out)
	}
	if gradeAudits(httpID) != 0 {
		t.Fatal("创建不应写 task.grade 审计")
	}
	code, out = do(http.MethodPost, "/api/tasks/"+httpID+"/regrade", "")
	if code != http.StatusConflict {
		t.Fatalf("非终态重新评级应 409，得到 %d %v", code, out)
	}

	// 不走 HTTP 创建：那条路径会后台拆目标，目标数会漂。
	// 这里直接登记一行，终态只由 SetTaskStatus 触发评级。
	created, err := m.CreateTaskWithOptions("c03c-grade", "看看评级", db.TaskCreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	id := created.ID
	n, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	row, err := m.pg.GetTask(n)
	if err != nil || row == nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := m.pg.Exec(`INSERT INTO exploration_nodes (exploration_id, kind, payload, state) VALUES ($1,'goal','{"text":"g"}','met')`, row.ExplorationID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := m.pg.Exec(`INSERT INTO exploration_nodes (exploration_id, kind, payload, state) VALUES ($1,'goal','{"text":"g"}','open')`, row.ExplorationID); err != nil {
		t.Fatal(err)
	}
	good := "GET /api?id=1 HTTP/1.1\n200 OK"
	for i := 0; i < 9; i++ {
		if _, err := m.pg.Exec(`INSERT INTO findings (task_id, vulnclass, severity, summary, evidence, status) VALUES ($1,'sqli','high','SQL 注入可读取他人数据',$2,'confirmed')`, n, good); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 3; i++ {
		if _, err := m.pg.Exec(`INSERT INTO findings (task_id, vulnclass, severity, summary, evidence, status) VALUES ($1,'sqli','low','待补证据','','pending')`, n); err != nil {
			t.Fatal(err)
		}
	}

	if err := m.SetTaskStatus(id, "done"); err != nil {
		t.Fatal(err)
	}
	if gradeAudits(id) != 0 {
		t.Fatal("自动终态评级不应写 task.grade 审计")
	}

	want := agent.ComputeGrade(agent.GradeInput{
		GoalsMet: 3, GoalsTotal: 4,
		VerifiedFindings: 9, TotalFindings: 12,
		EvidenceCompleteFindings: 9,
	})
	code, first := do(http.MethodGet, "/api/tasks/"+id, "")
	if code != 200 {
		t.Fatalf("GET: %d %v", code, first)
	}
	code, second := do(http.MethodGet, "/api/tasks/"+id, "")
	if code != 200 {
		t.Fatalf("GET2: %d %v", code, second)
	}
	assertGrade := func(body map[string]any) {
		t.Helper()
		if body["grade"] != want.Grade || body["grade_summary"] != want.Summary {
			t.Fatalf("评级\n got %v / %v\nwant %s / %s", body["grade"], body["grade_summary"], want.Grade, want.Summary)
		}
		score, _ := body["grade_score"].(float64)
		if math.Abs(score-want.Score) > 0.05 {
			t.Fatalf("分数 got %v want %v", score, want.Score)
		}
	}
	assertGrade(first)
	assertGrade(second)
	if first["grade"] != second["grade"] || first["grade_summary"] != second["grade_summary"] || first["grade_score"] != second["grade_score"] {
		t.Fatalf("两次 GET 不一致：%v vs %v", first["grade_summary"], second["grade_summary"])
	}

	// 事后把空证据的发现改成已确认。不重新评级时，GET 仍是原来的数字。
	if _, err := m.pg.Exec(`UPDATE findings SET status='confirmed', evidence=$2, summary='SQL 注入可读取他人数据' WHERE task_id=$1 AND status='pending'`, n, good); err != nil {
		t.Fatal(err)
	}
	code, held := do(http.MethodGet, "/api/tasks/"+id, "")
	if code != 200 {
		t.Fatal(code)
	}
	assertGrade(held)

	code, again := do(http.MethodPost, "/api/tasks/"+id+"/regrade", "")
	if code != 200 {
		t.Fatalf("重新评级: %d %v", code, again)
	}
	if again["grade_summary"] == want.Summary {
		t.Fatalf("重新评级后摘要应变化，仍是 %v", again["grade_summary"])
	}
	if again["grade"] != agent.GradeS && again["grade"] != agent.GradeA && again["grade"] != agent.GradeB {
		t.Fatalf("重新评级后的等级异常：%v", again)
	}
	// 12/12 已确认、12/12 过门、3/4 目标、覆盖 0 → 0.75×40+30+10 = 70 → B
	if again["grade"] != agent.GradeB {
		t.Fatalf("补齐发现后应是 B，得到 %v %v", again["grade"], again["grade_summary"])
	}
	if gradeAudits(id) != 1 {
		t.Fatalf("重新评级应有 1 条审计，得到 %d", gradeAudits(id))
	}
	var actor, source string
	if err := m.pg.QueryRow(`SELECT actor, source FROM audit_events WHERE action='task.grade' AND payload::jsonb->>'task_id'=$1`, id).Scan(&actor, &source); err != nil {
		t.Fatal(err)
	}
	if actor != "operator" || source != "task-api" {
		t.Fatalf("审计 actor/source=%s/%s", actor, source)
	}

	code, third := do(http.MethodGet, "/api/tasks/"+id, "")
	if code != 200 || third["grade"] != again["grade"] || third["grade_summary"] != again["grade_summary"] {
		t.Fatalf("重新评级后的 GET 应与响应一致：%v vs %v", third["grade_summary"], again["grade_summary"])
	}
}

func TestTaskGradeSourceDoesNotCallLLM(t *testing.T) {
	src, err := os.ReadFile("task_grade.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(src)
	for _, bad := range []string{"llm.", ".Complete(", "openai", "anthropic"} {
		if strings.Contains(text, bad) {
			t.Fatalf("task_grade.go 不应出现 %q", bad)
		}
	}
}

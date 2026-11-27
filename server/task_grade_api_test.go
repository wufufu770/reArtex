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

// TestTaskGradeEvidenceLevelRule 钉住 C-03D：
// 存量发现仍过门；自报不是默认 L1 或写了降级原因的发现，按证据等级 ≥ 3。
// 已经落库的等级不因发现改动重写；显式重新评级才用新口径。
func TestTaskGradeEvidenceLevelRule(t *testing.T) {
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
	good := "GET /api?id=1 HTTP/1.1\n200 OK"
	seed := func(n int64) {
		t.Helper()
		// 存量：过门。省略分级列，落默认 L1 / 自报 1 / 空原因。
		if _, err := m.pg.Exec(`INSERT INTO findings (task_id, vulnclass, severity, summary, evidence, status) VALUES ($1,'sqli','high','SQL 注入可读取他人数据',$2,'confirmed')`, n, good); err != nil {
			t.Fatal(err)
		}
		// 存量：证据为空，门不通过。
		if _, err := m.pg.Exec(`INSERT INTO findings (task_id, vulnclass, severity, summary, evidence, status) VALUES ($1,'sqli','low','待补证据','','pending')`, n); err != nil {
			t.Fatal(err)
		}
		// 新发现：自报 L3，结论自否。旧门会排除，新口径按等级算完整。
		if _, err := m.pg.Exec(`INSERT INTO findings (task_id, vulnclass, severity, summary, evidence, status, evidence_level, evidence_level_requested, evidence_level_reason)
VALUES ($1,'sqli','critical','这不是漏洞',$2,'confirmed',3,3,'')`, n, good); err != nil {
			t.Fatal(err)
		}
		// 新发现：自报 L4 降到 L2。旧门会放行，新口径不算完整。两条，让新旧计数不一样。
		for i := 0; i < 2; i++ {
			if _, err := m.pg.Exec(`INSERT INTO findings (task_id, vulnclass, severity, summary, evidence, status, evidence_level, evidence_level_requested, evidence_level_reason)
VALUES ($1,'sqli','high','SQL 注入可读取他人数据',$2,'confirmed',2,4,'自报证据等级 L4，可验证上限为 L2')`, n, good); err != nil {
				t.Fatal(err)
			}
		}
		// 新发现：落到 L4。两种口径都算完整。
		if _, err := m.pg.Exec(`INSERT INTO findings (task_id, vulnclass, severity, summary, evidence, status, evidence_level, evidence_level_requested, evidence_level_reason)
VALUES ($1,'sqli','high','未授权访问可导出用户列表',$2,'confirmed',4,4,'')`, n, good); err != nil {
			t.Fatal(err)
		}
	}
	// 新口径完整 3/6（存量过门 + L3 + L4）。旧门是 4/6（两条 L2 也过门，自否的 L3 不过）。
	// 已验证 5/6。无目标、无范围资产。
	wantNew := agent.ComputeGrade(agent.GradeInput{
		VerifiedFindings: 5, TotalFindings: 6,
		EvidenceCompleteFindings: 3,
	})
	if !strings.Contains(wantNew.Summary, "证据完整率 50%") || strings.Contains(wantNew.Summary, "证据完整率 67%") {
		t.Fatalf("期望摘要能把 3/6 和 4/6 分开：%s", wantNew.Summary)
	}

	// 已经写上的等级，再进一次终态也不覆盖。重新评级才换成新口径。
	held, err := m.CreateTaskWithOptions("c03d-held", "已有评级不动", db.TaskCreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	heldN, err := strconv.ParseInt(held.ID, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	seed(heldN)
	if err := m.pg.SetStatus(heldN, "done"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.pg.SaveTaskGrade(heldN, "C", "旧口径保持", 1, true); err != nil {
		t.Fatal(err)
	}
	if err := m.SetTaskStatus(held.ID, "done"); err != nil {
		t.Fatal(err)
	}
	code, body := do(http.MethodGet, "/api/tasks/"+held.ID, "")
	if code != 200 || body["grade_summary"] != "旧口径保持" || body["grade"] != "C" {
		t.Fatalf("已有评级不应被终态重写：%d %v", code, body)
	}
	code, body = do(http.MethodPost, "/api/tasks/"+held.ID+"/regrade", "")
	if code != 200 || body["grade_summary"] != wantNew.Summary || body["grade"] != wantNew.Grade {
		t.Fatalf("重新评级应换成新口径\n got %v / %v\nwant %s / %s", body["grade"], body["grade_summary"], wantNew.Grade, wantNew.Summary)
	}

	// 还没评过的任务，第一次进入终态就用新口径。之后改等级不自动重算。
	fresh, err := m.CreateTaskWithOptions("c03d-fresh", "终态用新口径", db.TaskCreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	freshN, err := strconv.ParseInt(fresh.ID, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	seed(freshN)
	if err := m.SetTaskStatus(fresh.ID, "done"); err != nil {
		t.Fatal(err)
	}
	code, body = do(http.MethodGet, "/api/tasks/"+fresh.ID, "")
	if code != 200 || body["grade_summary"] != wantNew.Summary {
		t.Fatalf("新终态应是新口径\n got %v\nwant %s", body["grade_summary"], wantNew.Summary)
	}
	score, _ := body["grade_score"].(float64)
	if math.Abs(score-wantNew.Score) > 0.05 {
		t.Fatalf("分数 got %v want %v", score, wantNew.Score)
	}
	if _, err := m.pg.Exec(`UPDATE findings SET evidence_level=3 WHERE task_id=$1 AND evidence_level=2`, freshN); err != nil {
		t.Fatal(err)
	}
	code, heldBody := do(http.MethodGet, "/api/tasks/"+fresh.ID, "")
	if code != 200 || heldBody["grade_summary"] != wantNew.Summary {
		t.Fatalf("改发现后不应自动重算：%v", heldBody["grade_summary"])
	}
	wantRaised := agent.ComputeGrade(agent.GradeInput{
		VerifiedFindings: 5, TotalFindings: 6,
		EvidenceCompleteFindings: 5,
	})
	if !strings.Contains(wantRaised.Summary, "证据完整率 83%") {
		t.Fatalf("抬到 L3 后应是 5/6：%s", wantRaised.Summary)
	}
	code, body = do(http.MethodPost, "/api/tasks/"+fresh.ID+"/regrade", "")
	if code != 200 || body["grade_summary"] != wantRaised.Summary || body["grade"] != wantRaised.Grade {
		t.Fatalf("重新评级应看到抬升后的完整率\n got %v / %v\nwant %s / %s", body["grade"], body["grade_summary"], wantRaised.Grade, wantRaised.Summary)
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

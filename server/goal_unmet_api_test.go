package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/db"
)

func TestUnmetGoalsOnTaskAndReport(t *testing.T) {
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

	created, err := m.CreateTaskWithOptions("c05e-unmet", "四个目标", db.TaskCreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	n, err := strconv.ParseInt(created.ID, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	row, err := m.pg.GetTask(n)
	if err != nil || row == nil {
		t.Fatal(err)
	}
	for _, state := range []string{"met", "met", "met"} {
		if _, err := m.pg.Exec(`INSERT INTO exploration_nodes (exploration_id, kind, payload, state) VALUES ($1,'goal','{"text":"已达成","criteria":"有证据"}',$2)`, row.ExplorationID, state); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := m.pg.Exec(`INSERT INTO exploration_nodes (exploration_id, kind, payload, state) VALUES ($1,'goal','{"text":"还没达成","criteria":"读到 flag"}','open')`, row.ExplorationID); err != nil {
		t.Fatal(err)
	}
	if err := m.SetTaskStatus(created.ID, "done"); err != nil {
		t.Fatal(err)
	}

	code, body := do(http.MethodGet, "/api/tasks/"+created.ID, "")
	if code != 200 {
		t.Fatalf("GET: %d %v", code, body)
	}
	if body["status"] != "timeout" {
		t.Fatalf("详情状态应是 timeout，得到 %v", body["status"])
	}
	raw, _ := json.Marshal(body["unmet_goals"])
	var goals []db.UnmetGoal
	if err := json.Unmarshal(raw, &goals); err != nil {
		t.Fatalf("unmet_goals 不是清单：%v raw=%s", err, raw)
	}
	if len(goals) != 1 || goals[0].Text != "还没达成" || goals[0].Criteria != "读到 flag" || goals[0].Reason == "" {
		t.Fatalf("未达成清单 = %+v", goals)
	}

	code, gen := do(http.MethodPost, "/api/reports/generate", `{"task_id":`+strconv.FormatInt(n, 10)+`}`)
	if code != 200 {
		t.Fatalf("生成报告: %d %v", code, gen)
	}
	var content string
	if err := m.pg.QueryRow(`SELECT content FROM reports WHERE task_id=$1 ORDER BY id DESC LIMIT 1`, n).Scan(&content); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"## 未达成目标", "还没达成", "读到 flag", "未达成原因"} {
		if !strings.Contains(content, want) {
			t.Fatalf("报告缺少 %q\n%s", want, content)
		}
	}

	done, err := m.CreateTaskWithOptions("c05e-all", "都达成", db.TaskCreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	dn, _ := strconv.ParseInt(done.ID, 10, 64)
	drow, err := m.pg.GetTask(dn)
	if err != nil || drow == nil {
		t.Fatal(err)
	}
	if _, err := m.pg.Exec(`INSERT INTO exploration_nodes (exploration_id, kind, payload, state) VALUES ($1,'goal','{"text":"达成","criteria":"有证据"}','met')`, drow.ExplorationID); err != nil {
		t.Fatal(err)
	}
	if err := m.SetTaskStatus(done.ID, "done"); err != nil {
		t.Fatal(err)
	}
	code, all := do(http.MethodGet, "/api/tasks/"+done.ID, "")
	if code != 200 || all["status"] != "done" {
		t.Fatalf("全达成应是 done：%d %v", code, all["status"])
	}
	raw, _ = json.Marshal(all["unmet_goals"])
	if string(raw) != "[]" {
		t.Fatalf("全达成的未达成清单应是 []，得到 %s", raw)
	}
	code, gen = do(http.MethodPost, "/api/reports/generate", `{"task_id":`+strconv.FormatInt(dn, 10)+`}`)
	if code != 200 {
		t.Fatalf("全达成报告: %d %v", code, gen)
	}
	if err := m.pg.QueryRow(`SELECT content FROM reports WHERE task_id=$1 ORDER BY id DESC LIMIT 1`, dn).Scan(&content); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(content, "## 未达成目标") || !strings.Contains(content, "无未达成目标") {
		t.Fatalf("全达成报告仍应列出这一节：\n%s", content)
	}
}

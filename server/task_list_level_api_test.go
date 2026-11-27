package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"

	"github.com/Autumn-27/artex/db"
)

func TestParseTaskListLevelFilter(t *testing.T) {
	d, g, errMsg := parseTaskListLevelFilter(url.Values{})
	if d != "" || g != "" || errMsg != "" {
		t.Fatalf("空查询应不过滤：%q %q %q", d, g, errMsg)
	}
	q := url.Values{"difficulty": {"auto"}, "grade": {"S"}}
	if _, _, errMsg = parseTaskListLevelFilter(q); errMsg == "" {
		t.Fatal("执行档 auto 不能当作任务难度筛选项")
	}
	q.Set("difficulty", "hard")
	q.Set("grade", "s")
	if _, _, errMsg = parseTaskListLevelFilter(q); errMsg == "" {
		t.Fatal("小写 s 应拒绝")
	}
	q.Set("grade", "ungraded")
	d, g, errMsg = parseTaskListLevelFilter(q)
	if errMsg != "" || d != "hard" || g != db.TaskGradeUngraded {
		t.Fatalf("hard + ungraded：%q %q %q", d, g, errMsg)
	}
}

func TestTaskListLevelFilter(t *testing.T) {
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
		r.Header.Set("Authorization", "Bearer "+tok)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	doRaw := func(path string) (int, []byte) {
		t.Helper()
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.Header.Set("Authorization", "Bearer "+tok)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec.Code, rec.Body.Bytes()
	}
	tasksOf := func(body map[string]any) []map[string]any {
		t.Helper()
		raw, _ := body["tasks"].([]any)
		out := make([]map[string]any, 0, len(raw))
		for _, item := range raw {
			row, _ := item.(map[string]any)
			out = append(out, row)
		}
		return out
	}
	find := func(rows []map[string]any, id string) map[string]any {
		for _, row := range rows {
			if row["id"] == id {
				return row
			}
		}
		return nil
	}

	mk := func(name, goal, level string) (string, int64) {
		t.Helper()
		created, err := m.CreateTaskWithOptions(name, goal, db.TaskCreateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		n, err := strconv.ParseInt(created.ID, 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := m.pg.UpdateTask(n, db.TaskPatch{Difficulty: &level}); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = m.pg.DeleteTask(n) })
		return created.ID, n
	}
	grade := func(n int64, g string, score float64) {
		t.Helper()
		if err := m.pg.SetStatus(n, "done"); err != nil {
			t.Fatal(err)
		}
		if _, err := m.pg.SaveTaskGrade(n, g, g+" 摘要", score, true); err != nil {
			t.Fatal(err)
		}
	}

	hardSID, hardSN := mk("c06a-hard-s", "看看支付", db.TaskDifficultyHard)
	hardOpenID, hardOpenN := mk("c06a-hard-open", "收集子域", db.TaskDifficultyHard)
	easySID, easySN := mk("c06a-easy-s", "看看这个站点", db.TaskDifficultyEasy)
	expertCID, expertCN := mk("c06a-expert-c", "内网横向", db.TaskDifficultyExpert)
	grade(hardSN, db.TaskGradeS, 95)
	grade(easySN, db.TaskGradeS, 92)
	grade(expertCN, db.TaskGradeC, 40)

	if _, err := m.pg.Exec(`INSERT INTO findings (task_id, vulnclass, severity, summary, evidence, impact_level) VALUES ($1,'sqli','high','另一处','evidence',2)`, hardOpenN); err != nil {
		t.Fatal(err)
	}
	row, err := m.pg.GetTask(hardSN)
	if err != nil || row == nil {
		t.Fatal(err)
	}
	rec, err := m.pg.Exploration(row.ExplorationID).RecordFinding(context.Background(), db.RecordFindingInput{
		TaskID: hardSN, VulnClass: "sqli", Name: "注入", Severity: db.SeverityCritical,
		Summary: "SQL 注入可读取他人数据", Evidence: "只有一段说明，没有工具输出。",
		Worker: "test", ImpactLevel: 4, EvidenceLevel: 2,
	})
	if err != nil || rec == nil {
		t.Fatal(err)
	}

	code, body := do(http.MethodGet, "/api/tasks?difficulty=hard", "")
	if code != 200 {
		t.Fatalf("hard: %d %v", code, body)
	}
	hardRows := tasksOf(body)
	if find(hardRows, hardSID) == nil || find(hardRows, hardOpenID) == nil || find(hardRows, easySID) != nil || find(hardRows, expertCID) != nil {
		t.Fatalf("难度 hard 应只含两条 hard：%v", idsOf(hardRows))
	}
	for _, item := range hardRows {
		if item["task_difficulty"] != db.TaskDifficultyHard {
			t.Fatalf("筛 hard 后出现 %v", item["task_difficulty"])
		}
	}

	code, body = do(http.MethodGet, "/api/tasks?grade=S", "")
	if code != 200 {
		t.Fatalf("grade S: %d %v", code, body)
	}
	sRows := tasksOf(body)
	if find(sRows, hardSID) == nil || find(sRows, easySID) == nil || find(sRows, hardOpenID) != nil || find(sRows, expertCID) != nil {
		t.Fatalf("评级 S 的成员不对：%v", idsOf(sRows))
	}
	for _, item := range sRows {
		if item["grade"] != db.TaskGradeS {
			t.Fatalf("筛 S 后出现 %v", item["grade"])
		}
	}

	code, body = do(http.MethodGet, "/api/tasks?difficulty=hard&grade=S", "")
	if code != 200 {
		t.Fatalf("交集: %d %v", code, body)
	}
	both := tasksOf(body)
	if find(both, hardSID) == nil || find(both, hardOpenID) != nil || find(both, easySID) != nil {
		t.Fatalf("hard∩S 应只剩 hard-s：%v", idsOf(both))
	}
	for _, item := range both {
		if item["task_difficulty"] != db.TaskDifficultyHard || item["grade"] != db.TaskGradeS {
			t.Fatalf("交集里有 %v / %v", item["task_difficulty"], item["grade"])
		}
	}
	got := find(both, hardSID)
	if got["max_impact_level"] != float64(4) || got["grade_summary"] != "S 摘要" {
		t.Fatalf("列表应带最高影响层级和摘要：%v", got)
	}
	if got["suggested_difficulty"] == "" {
		t.Fatal("列表应带建议难度")
	}

	code, body = do(http.MethodGet, "/api/tasks?grade=ungraded", "")
	if code != 200 {
		t.Fatalf("ungraded: %d %v", code, body)
	}
	openRows := tasksOf(body)
	if find(openRows, hardOpenID) == nil || find(openRows, hardSID) != nil {
		t.Fatalf("未评级应含 open、不含已评：%v", idsOf(openRows))
	}
	for _, item := range openRows {
		if item["grade"] != "" {
			t.Fatalf("未评级筛选出现等级 %v", item["grade"])
		}
	}
	if open := find(openRows, hardOpenID); open["max_impact_level"] != float64(2) {
		t.Fatalf("open 的最高影响应为 2：%v", open["max_impact_level"])
	}

	code, body = do(http.MethodGet, "/api/tasks?difficulty=auto", "")
	if code != 400 {
		t.Fatalf("执行档 auto 应 400，得到 %d %v", code, body)
	}
	code, body = do(http.MethodGet, "/api/tasks?grade=Z", "")
	if code != 400 {
		t.Fatalf("非法评级应 400，得到 %d %v", code, body)
	}
	code, plain := do(http.MethodGet, "/api/tasks", "")
	if code != 200 || find(tasksOf(plain), hardSID) == nil || find(tasksOf(plain), easySID) == nil {
		t.Fatalf("不筛选应仍能看到这些任务：%d", code)
	}

	code, raw := doRaw("/api/exploration/findings?task=" + hardSID)
	if code != 200 {
		t.Fatalf("任务发现列表: %d %s", code, raw)
	}
	var findings []map[string]any
	if err := json.Unmarshal(raw, &findings); err != nil {
		t.Fatalf("发现列表不是数组：%v %s", err, raw)
	}
	var matched map[string]any
	for _, f := range findings {
		if f["finding_id"] == strconv.FormatInt(rec.FindingID, 10) {
			matched = f
			break
		}
	}
	if matched == nil || matched["impact_level"] != float64(4) || matched["evidence_level"] != float64(2) || matched["evidence_insufficient"] != true || matched["severity"] != db.SeverityCritical {
		t.Fatalf("任务内发现应带三维分级和证据不足：%v", findings)
	}
}

func idsOf(rows []map[string]any) []any {
	out := make([]any, 0, len(rows))
	for _, row := range rows {
		out = append(out, row["id"])
	}
	return out
}

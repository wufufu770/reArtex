package server

// monitor_findings_test.go：B-09G Monitor findings 人工处置 API 验收。
//
// 卡验收逐项（live PG + 真实路由表 + 真实 JWT 鉴权链 + fake LLM/fake 拉取面
// 零真实出网）：
//   - 权限/租户隔离：四路由无 token 全 401（401 而非 404=注册在 requireAuth 内）；
//   - 复现：只创建待验证任务（现有建任务入口 CreateTaskWithOptions——带仓库/
//     commit/候选上下文，不自动执行）；状态落 reproduced；重复操作幂等拒绝
//     （不产重复任务不写重复审计）；
//   - 忽略：理由必填（缺失/空白 400）、理由进审计载荷、原始行绝不删除、
//     重复忽略幂等拒绝、reproduced→ignored 合法路径；
//   - 重判：安全更新→new、非安全更新→行保留状态不动、材料拉取失败/LLM 未
//     配置/判定失败→pending_review（fail-closed）、已处置态 409 不覆写人工
//     决定；
//   - 列表：白名单过滤+limit 界+404 面。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
)

const monitorFindingTestPrefix = "https://github.com/b09gfind/"

func monitorFindingSeed(t *testing.T, pg *db.DB, repoURL string, status string) (int64, int64, string) {
	t.Helper()
	repoID, err := pg.UpsertMonitorRepo(repoURL, "main", "", "", "tester", true)
	if err != nil {
		t.Fatal(err)
	}
	sha := fmt.Sprintf("%040x", time.Now().UnixNano())
	inserted, err := pg.RecordMonitorFinding(repoID, sha, "fix: CVE-2024-9999 测试候选 "+repoURL, "CVE-2024-9999", "", status)
	if err != nil || !inserted {
		t.Fatalf("seed finding: inserted=%v err=%v", inserted, err)
	}
	rows, err := pg.ListMonitorFindings(status, 500)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range rows {
		if f.SHA == sha {
			return repoID, f.ID, sha
		}
	}
	t.Fatal("seed finding 未找到")
	return 0, 0, ""
}

func monitorFindingAuditRows(t *testing.T, pg *db.DB, action string) []map[string]any {
	t.Helper()
	rows, err := pg.Query(`SELECT payload FROM audit_events WHERE tool_name='monitor' AND action=$1 ORDER BY id ASC`, action)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(payload), &m); err != nil {
			t.Fatalf("audit payload 非法 JSON: %v", err)
		}
		out = append(out, m)
	}
	return out
}

func TestMonitorFindingRoutesAuth(t *testing.T) {
	s, h, _ := monitorAPITestServer(t)
	_ = s
	for _, path := range []string{
		"/api/monitor/findings",
		"/api/monitor/findings/1/reproduce",
		"/api/monitor/findings/1/rejudge",
		"/api/monitor/findings/1/ignore",
	} {
		rec := monitorAPIReq(h, "", "GET", path, "")
		if rec.Code != 401 {
			t.Fatalf("%s 无 token 应 401（注册在 requireAuth 内）: %d", path, rec.Code)
		}
	}
}

func TestMonitorFindingReproduceCreatesPendingTask(t *testing.T) {
	s, h, tok := monitorAPITestServer(t)
	pg := s.m.pg
	repoID, fid, sha := monitorFindingSeed(t, pg, monitorFindingTestPrefix+"repro", db.MonitorStatusNew)

	rec := monitorAPIReq(h, tok, "POST", fmt.Sprintf("/api/monitor/findings/%d/reproduce", fid), "")
	if rec.Code != 200 {
		t.Fatalf("复现应 200: %d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Finding map[string]any `json:"finding"`
		TaskID  string         `json:"task_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Finding["status"] != db.MonitorStatusReproduced {
		t.Fatalf("复现后应 reproduced: %v", resp.Finding["status"])
	}
	if resp.TaskID == "" {
		t.Fatal("应返回 task_id")
	}
	// 任务经现有人工建任务入口落库：goal 带仓库/commit/候选上下文+不自动执行
	// 声明；验证任务以待验证形态创建（本 API 不 launch 不 seed scope）。
	var goal, description string
	if err := pg.QueryRow(`SELECT goal, description FROM tasks WHERE id=$1`, resp.TaskID).Scan(&goal, &description); err != nil {
		t.Fatalf("验证任务应经现有建任务入口落库: %v", err)
	}
	for _, want := range []string{sha, monitorFindingTestPrefix + "repro", "CVE-2024-9999", "不自动执行任何测试或利用"} {
		if !strings.Contains(goal, want) {
			t.Fatalf("goal 缺上下文 %q: %s", want, goal)
		}
	}
	if !strings.Contains(description, "Monitor 候选验证") {
		t.Fatalf("description 应可追踪: %s", description)
	}

	// 审计链：恰一行动作行，payload 键恰白名单。
	rows := monitorFindingAuditRows(t, pg, "monitor-finding-reproduce")
	if len(rows) != 1 {
		t.Fatalf("复现审计应恰一行: %d", len(rows))
	}
	allowed := map[string]bool{"id": true, "repo_id": true, "sha": true, "task_id": true, "via": true}
	for k := range rows[0] {
		if !allowed[k] {
			t.Fatalf("复现审计载荷自由键 %v", k)
		}
	}
	if rows[0]["task_id"].(float64) <= 0 || rows[0]["sha"] != sha {
		t.Fatalf("复现审计载荷不符: %v", rows[0])
	}

	// 重复操作幂等拒绝：不产重复任务、不写重复审计。
	rec = monitorAPIReq(h, tok, "POST", fmt.Sprintf("/api/monitor/findings/%d/reproduce", fid), "")
	if rec.Code != 409 {
		t.Fatalf("重复复现应 409: %d", rec.Code)
	}
	var taskCount int
	if err := pg.QueryRow(`SELECT count(*) FROM tasks WHERE description=$1`, description).Scan(&taskCount); err != nil {
		t.Fatal(err)
	}
	if taskCount != 1 {
		t.Fatalf("重复复现不得产重复任务: %d", taskCount)
	}
	if rows2 := monitorFindingAuditRows(t, pg, "monitor-finding-reproduce"); len(rows2) != 1 {
		t.Fatalf("重复复现不得写重复审计: %d", len(rows2))
	}

	// 忽略后的发现不可复现（显式合法路径外全拒）。
	if err := pg.SetMonitorFindingStatus(fid, db.MonitorStatusIgnored); err != nil {
		t.Fatal(err)
	}
	rec = monitorAPIReq(h, tok, "POST", fmt.Sprintf("/api/monitor/findings/%d/reproduce", fid), "")
	if rec.Code != 409 {
		t.Fatalf("ignored 不可复现: %d", rec.Code)
	}
	_ = repoID
}

func TestMonitorFindingIgnoreReasonAndAudit(t *testing.T) {
	s, h, tok := monitorAPITestServer(t)
	pg := s.m.pg
	_, fid, sha := monitorFindingSeed(t, pg, monitorFindingTestPrefix+"ignore", db.MonitorStatusPendingReview)
	path := fmt.Sprintf("/api/monitor/findings/%d/ignore", fid)

	// 理由必填：缺体/缺键/空白全 400。
	if rec := monitorAPIReq(h, tok, "POST", path, ""); rec.Code != 400 {
		t.Fatalf("缺体应 400: %d", rec.Code)
	}
	if rec := monitorAPIReq(h, tok, "POST", path, `{}`); rec.Code != 400 {
		t.Fatalf("缺 reason 应 400: %d", rec.Code)
	}
	if rec := monitorAPIReq(h, tok, "POST", path, `{"reason":"   "}`); rec.Code != 400 {
		t.Fatalf("空白 reason 应 400: %d", rec.Code)
	}

	// 正常忽略：理由进审计，原始行保留（绝不删除）。
	reason := "误报：仅文档提及 CVE，非安全更新"
	rec := monitorAPIReq(h, tok, "POST", path, fmt.Sprintf(`{"reason":%q}`, reason))
	if rec.Code != 200 {
		t.Fatalf("忽略应 200: %d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Finding map[string]any `json:"finding"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Finding["status"] != db.MonitorStatusIgnored {
		t.Fatalf("应 ignored: %v", resp.Finding["status"])
	}
	var title, shaDB, statusDB string
	if err := pg.QueryRow(`SELECT title, sha, status FROM monitor_findings WHERE id=$1`, fid).Scan(&title, &shaDB, &statusDB); err != nil {
		t.Fatal(err)
	}
	if shaDB != sha || statusDB != db.MonitorStatusIgnored || title == "" {
		t.Fatalf("原始行必须保留（只改状态）: sha=%s status=%s", shaDB, statusDB)
	}
	rows := monitorFindingAuditRows(t, pg, "monitor-finding-ignore")
	if len(rows) != 1 || rows[0]["reason"] != reason {
		t.Fatalf("忽略审计应带理由: %v", rows)
	}

	// 重复忽略幂等拒绝。
	if rec := monitorAPIReq(h, tok, "POST", path, fmt.Sprintf(`{"reason":%q}`, reason)); rec.Code != 409 {
		t.Fatalf("重复忽略应 409: %d", rec.Code)
	}

	// reproduced→ignored 是合法路径（复现已发起后仍可改判忽略）。
	_, fid2, _ := monitorFindingSeed(t, pg, monitorFindingTestPrefix+"ignore2", db.MonitorStatusNew)
	if err := pg.SetMonitorFindingStatus(fid2, db.MonitorStatusReproduced); err != nil {
		t.Fatal(err)
	}
	if rec := monitorAPIReq(h, tok, "POST", fmt.Sprintf("/api/monitor/findings/%d/ignore", fid2), `{"reason":"复现后确认无关"}`); rec.Code != 200 {
		t.Fatalf("reproduced→ignored 应 200: %d", rec.Code)
	}
}

func TestMonitorFindingRejudgeOutcomes(t *testing.T) {
	s, h, tok := monitorAPITestServer(t)
	pg := s.m.pg
	realFetcher := monitorFindingCommitFetcher
	t.Cleanup(func() { monitorFindingCommitFetcher = realFetcher; s.llmOn = false; s.llmProv = nil })
	commitOf := func(sha string) *MonitorCommit {
		return &MonitorCommit{SHA: sha, Message: "fix: CVE-2024-9999 sanitize input\n\nbackport"}
	}

	path := func(fid int64) string { return fmt.Sprintf("/api/monitor/findings/%d/rejudge", fid) }
	post := func(fid int64) (int, map[string]any) {
		rec := monitorAPIReq(h, tok, "POST", path(fid), "")
		var m map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &m)
		return rec.Code, m
	}
	expect := func(t *testing.T, code int, m map[string]any, wantCode int, outcome, status string) {
		t.Helper()
		if code != wantCode {
			t.Fatalf("code=%d want=%d body=%v", code, wantCode, m)
		}
		if m["outcome"] != outcome {
			t.Fatalf("outcome=%v want=%s", m["outcome"], outcome)
		}
		f := m["finding"].(map[string]any)
		if f["status"] != status {
			t.Fatalf("status=%v want=%s", f["status"], status)
		}
	}

	// ① 判定安全更新：pending_review → new（回到候选）。
	monitorFindingCommitFetcher = func(ctx context.Context, repo *db.MonitorRepo, sha string) (*MonitorCommit, error) {
		return commitOf(sha), nil
	}
	s.llmOn = true
	s.llmProv = &monitorMockLLM{resp: `{"verdict":true,"reason":"上游安全修复"}`}
	_, fid, _ := monitorFindingSeed(t, pg, monitorFindingTestPrefix+"rjnew", db.MonitorStatusPendingReview)
	code, m := post(fid)
	expect(t, code, m, 200, "new", db.MonitorStatusNew)

	// ② 判定非安全更新：行保留、状态不动、审计留痕（处置权留给操作者）。
	s.llmProv = &monitorMockLLM{resp: `{"verdict":false,"reason":"纯文档更新"}`}
	_, fid2, _ := monitorFindingSeed(t, pg, monitorFindingTestPrefix+"rjnot", db.MonitorStatusNew)
	code, m = post(fid2)
	expect(t, code, m, 200, "not_security", db.MonitorStatusNew)

	// ③ 判定失败（无效 JSON）→ pending_review（fail-closed，卡要求 2）。
	s.llmProv = &monitorMockLLM{resp: "这不是 JSON"}
	_, fid3, _ := monitorFindingSeed(t, pg, monitorFindingTestPrefix+"rjfail", db.MonitorStatusNew)
	code, m = post(fid3)
	expect(t, code, m, 200, "undecided", db.MonitorStatusPendingReview)

	// ④ 材料拉取失败 → pending_review。
	monitorFindingCommitFetcher = func(ctx context.Context, repo *db.MonitorRepo, sha string) (*MonitorCommit, error) {
		return nil, errors.New("github 500")
	}
	_, fid4, _ := monitorFindingSeed(t, pg, monitorFindingTestPrefix+"rjfetch", db.MonitorStatusNew)
	code, m = post(fid4)
	expect(t, code, m, 200, "undecided", db.MonitorStatusPendingReview)

	// ⑤ LLM 未配置 → pending_review（绝不半装配硬判）。
	monitorFindingCommitFetcher = func(ctx context.Context, repo *db.MonitorRepo, sha string) (*MonitorCommit, error) {
		return commitOf(sha), nil
	}
	s.llmOn = false
	s.llmProv = nil
	_, fid5, _ := monitorFindingSeed(t, pg, monitorFindingTestPrefix+"rjnofllm", db.MonitorStatusNew)
	code, m = post(fid5)
	expect(t, code, m, 200, "undecided", db.MonitorStatusPendingReview)

	// ⑥ 已处置态不可重判（不覆写人工决定）。
	_, fid6, _ := monitorFindingSeed(t, pg, monitorFindingTestPrefix+"rjdisp", db.MonitorStatusNew)
	if err := pg.SetMonitorFindingStatus(fid6, db.MonitorStatusReproduced); err != nil {
		t.Fatal(err)
	}
	if code, _ := post(fid6); code != 409 {
		t.Fatalf("reproduced 重判应 409: %d", code)
	}

	// 审计链：恰 5 行重判动作，载荷键恰白名单 {id,sha,outcome,via}。
	rows := monitorFindingAuditRows(t, pg, "monitor-finding-rejudge")
	if len(rows) != 5 {
		t.Fatalf("重判审计应恰 5 行: %d", len(rows))
	}
	allowed := map[string]bool{"id": true, "sha": true, "outcome": true, "via": true}
	for _, r := range rows {
		for k := range r {
			if !allowed[k] {
				t.Fatalf("重判审计载荷自由键 %v", k)
			}
		}
	}
}

func TestMonitorFindingListAnd404(t *testing.T) {
	s, h, tok := monitorAPITestServer(t)
	pg := s.m.pg
	repoURL := monitorFindingTestPrefix + "list"
	repoID, err := pg.UpsertMonitorRepo(repoURL, "main", "", "", "tester", true)
	if err != nil {
		t.Fatal(err)
	}
	shaNew := fmt.Sprintf("%040x", time.Now().UnixNano())
	shaPend := fmt.Sprintf("%040x", time.Now().UnixNano()+1)
	if _, err := pg.RecordMonitorFinding(repoID, shaNew, "fix: CVE-2024-8888 thing", "CVE-2024-8888", "", db.MonitorStatusNew); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.RecordMonitorFinding(repoID, shaPend, "待审：判定未决", "", "GHSA-xxxx-xxxx-xxxx", db.MonitorStatusPendingReview); err != nil {
		t.Fatal(err)
	}

	// 全列表：带仓库上下文投影（repo_url/branch），无凭据面。
	rec := monitorAPIReq(h, tok, "GET", "/api/monitor/findings?limit=500", "")
	if rec.Code != 200 {
		t.Fatalf("列表应 200: %d", rec.Code)
	}
	var resp struct {
		Findings []map[string]any `json:"findings"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	foundNew, foundPend := false, false
	for _, f := range resp.Findings {
		if f["repo_url"] != repoURL {
			continue
		}
		switch f["sha"] {
		case shaNew:
			foundNew = f["status"] == db.MonitorStatusNew && f["cve_id"] == "CVE-2024-8888"
		case shaPend:
			foundPend = f["status"] == db.MonitorStatusPendingReview && f["ghsa_id"] == "GHSA-xxxx-xxxx-xxxx"
		}
	}
	if !foundNew || !foundPend {
		t.Fatalf("列表投影不符（含 repo_url 上下文）: foundNew=%v foundPend=%v", foundNew, foundPend)
	}

	// status 白名单过滤。
	rec = monitorAPIReq(h, tok, "GET", "/api/monitor/findings?status=pending_review&limit=500", "")
	if rec.Code != 200 {
		t.Fatalf("过滤列表应 200: %d", rec.Code)
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	for _, f := range resp.Findings {
		if f["status"] != db.MonitorStatusPendingReview {
			t.Fatalf("过滤应只含 pending_review: %v", f["status"])
		}
	}
	if rec := monitorAPIReq(h, tok, "GET", "/api/monitor/findings?status=bogus", ""); rec.Code != 400 {
		t.Fatalf("非法 status 应 400: %d", rec.Code)
	}
	if rec := monitorAPIReq(h, tok, "GET", "/api/monitor/findings?limit=0", ""); rec.Code != 400 {
		t.Fatalf("limit=0 应 400: %d", rec.Code)
	}
	if rec := monitorAPIReq(h, tok, "GET", "/api/monitor/findings?limit=501", ""); rec.Code != 400 {
		t.Fatalf("limit>500 应 400: %d", rec.Code)
	}

	// 不存在的发现：三操作全 404（不静默）。
	for _, p := range []string{"/reproduce", "/rejudge", "/ignore"} {
		rec := monitorAPIReq(h, tok, "POST", fmt.Sprintf("/api/monitor/findings/99999999%s", p), `{"reason":"r"}`)
		if rec.Code != 404 {
			t.Fatalf("%s 不存在应 404: %d", p, rec.Code)
		}
	}
}

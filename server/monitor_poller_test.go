package server

// monitor_poller_test.go：B-09F Monitor Poller 增量轮询与游标验收。
//
// 卡验收逐项（fake GitHub + fake LLM + live PG）：
//   - 新提交：时间序逐提交处理，安全更新落 new 候选、非安全更新不落行、
//     未判定落 pending_review，游标逐提交推进到最新已处理 sha；
//   - 无更新：零判定零新行（重复 poll 幂等）；
//   - GitHub 接口失败：整仓跳过，游标不动，零落行；
//   - Judge 超时：pending_review 可追踪（标题带「待审」+理由），游标照常推进
//     （明确入人工队列后才推进——卡要求 2 逐字）；
//   - 存储失败：游标不动（不丢 commit），恢复后重拉同窗幂等兜底；
//   - shutdown：Stop() 取消在途轮次即刻退出（goroutine 不悬挂）；
//   - 节流闸：sweep 水位未到点零开销（sc.now 可注入时钟）+ 过点触发
//     （fake GitHub 在场零真实出网——init 水位重种使测试生命周期内绝不误触发，
//     本测试显式操纵水位验证触发面）。

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/llm"
)

const monitorPollTestPrefix = "https://github.com/b09fpoll/"

// monitorFakeGH 是 fake GitHub（MonitorCommitSource 注入面）。
type monitorFakeGH struct {
	page *MonitorCommitPage
	err  error
	seen []string // 请求参数记录（repoURL|branch|lastSeen）
}

func (f *monitorFakeGH) ListNewCommits(ctx context.Context, repoURL, branch, lastSeenSHA, authRef string) (*MonitorCommitPage, error) {
	f.seen = append(f.seen, repoURL+"|"+branch+"|"+lastSeenSHA)
	if f.err != nil {
		return nil, f.err
	}
	return f.page, nil
}

// monitorSeqLLM 按序返回响应的 fake 判定调用面（MonitorLLMCaller 面——
// 判定只走 Complete；超出序列长度夹取末项）。
type monitorSeqLLM struct {
	resp []string
	at   int
}

func (m *monitorSeqLLM) Complete(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	i := m.at
	if i > len(m.resp)-1 {
		i = len(m.resp) - 1
	}
	m.at++
	return llm.UserText(m.resp[i]), "", llm.Usage{}, nil
}

func monitorPollTestServer(t *testing.T) (*Server, *db.DB) {
	t.Helper()
	s, _, _ := monitorAPITestServer(t)
	pg := s.m.pg
	// 干净状态板：禁用测试库里全部残留 enabled 仓库（共享 PG 里先前用例/中断
	// 运行留下的仓库会被 ListMonitorRepos(true) 拉进本轮，污染轮级计数断言——
	// 断言面必须只含本用例自建的仓库）。只动 monitor_repos 测试面，零生产影响。
	if repos, err := pg.ListMonitorRepos(false); err == nil {
		for _, r := range repos {
			if r.Enabled {
				_ = pg.SetMonitorRepoEnabled(r.ID, false)
			}
		}
	}
	return s, pg
}

func TestMonitorPollerNewCommitsAndCursor(t *testing.T) {
	_, pg := monitorPollTestServer(t)
	repoURL := monitorPollTestPrefix + "newcommits"
	repoID, err := pg.UpsertMonitorRepo(repoURL, "main", "", "", "tester", true)
	if err != nil {
		t.Fatal(err)
	}

	// fake GitHub：3 个新提交（新→旧返回——客户端契约），无 lastSeen。
	newest := "cccccccccccccccccccccccccccccccccccccccc"
	mid := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	oldest := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	gh := &monitorFakeGH{page: &MonitorCommitPage{Commits: []MonitorCommit{
		{SHA: newest, Message: "fix: GHSA-xxxx-xxxx-xxxx harden parser", Date: time.Now()},
		{SHA: mid, Message: "docs: update readme with CVE-2024-3094 mention", Date: time.Now().Add(-time.Hour)},
		{SHA: oldest, Message: "fix: CVE-2024-0001 sanitize input path", Date: time.Now().Add(-2 * time.Hour)},
	}}}
	// fake LLM 按处理序（时间序=最旧先）：最旧=safe、中=not security、最新=unsure。
	seq := &monitorSeqLLM{resp: []string{
		`{"verdict":true,"reason":"修复 CVE-2024-0001 输入清洗"}`,
		`{"verdict":false,"reason":"文档更新，CVE 字样只是提及"}`,
		`{"verdict":false,"reason":"材料不足","unsure":true}`,
	}}
	p := NewMonitorPoller(pg, NewMonitorJudge(seq))
	p.source = func(repo *db.MonitorRepo) (MonitorCommitSource, error) { return gh, nil }

	stats, err := p.pollRound(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Repos != 1 || stats.Commits != 3 {
		t.Fatalf("轮次计数不符: %+v", stats)
	}
	// 处理顺序实证：时间序（最旧先）——seq[0]（safe）必须落在最旧提交上。
	rows, err := pg.ListMonitorFindings("", 100)
	if err != nil {
		t.Fatal(err)
	}
	var newCount, pendingCount int
	var pendingRow *db.MonitorFinding
	for _, f := range rows {
		if f.RepoID != repoID {
			continue
		}
		switch f.Status {
		case db.MonitorStatusNew:
			newCount++
			if f.SHA != oldest || !strings.HasPrefix(f.Title, "安全更新候选：") || f.CVEID != "CVE-2024-0001" || f.GHSAID != "" {
				t.Fatalf("new 行不符（时间序/标题/ID 提取）: %+v", f)
			}
		case db.MonitorStatusPendingReview:
			pendingCount++
			pendingRow = f
		}
	}
	if newCount != 1 || pendingCount != 1 || stats.FindingsNew != 1 || stats.PendingReview != 1 || stats.NotSecurity != 1 {
		t.Fatalf("落点统计不符: new=%d pending=%d stats=%+v", newCount, pendingCount, stats)
	}
	if pendingRow == nil || !strings.HasPrefix(pendingRow.Title, "待审：") || pendingRow.SHA != newest {
		t.Fatalf("pending_review 应可追踪（待审前缀+对应最新提交）: %+v", pendingRow)
	}
	// 游标推进到最新已处理 sha（时间序最后一个——即便它只是 pending_review）。
	rp, _ := pg.GetMonitorRepo(repoID)
	if rp == nil || rp.LastSeenSHA != newest {
		t.Fatalf("游标应=最新已处理 sha: %+v", rp)
	}

	// ② 重复 poll 幂等：无更新（空页返回）→ 零判定零新行零游标变化。
	gh.page = &MonitorCommitPage{Commits: []MonitorCommit{}, StoppedAtSeen: true}
	stats, err = p.pollRound(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Commits != 0 || stats.FindingsNew != 0 || stats.PendingReview != 0 || stats.CursorAdvanced != 0 {
		t.Fatalf("无更新轮应为零: %+v", stats)
	}
	rows2, _ := pg.ListMonitorFindings("", 100)
	if len(rows2) != len(rows) {
		t.Fatalf("重复 poll 幂等（行数不变）: %d → %d", len(rows), len(rows2))
	}
}

func TestMonitorPollerFetchFailureKeepsCursor(t *testing.T) {
	_, pg := monitorPollTestServer(t)
	repoID, err := pg.UpsertMonitorRepo(monitorPollTestPrefix+"fetchfail", "main", "", "", "tester", true)
	if err != nil {
		t.Fatal(err)
	}
	seen := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if err := pg.UpdateMonitorRepoCursor(repoID, seen); err != nil {
		t.Fatal(err)
	}

	gh := &monitorFakeGH{err: errors.New("github 500")}
	p := NewMonitorPoller(pg, NewMonitorJudge(&monitorMockLLM{resp: `{"verdict":true,"reason":"r"}`}))
	p.source = func(repo *db.MonitorRepo) (MonitorCommitSource, error) { return gh, nil }

	stats, err := p.pollRound(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.FetchFailures != 1 || stats.Commits != 0 || stats.CursorAdvanced != 0 {
		t.Fatalf("拉取失败应整仓跳过: %+v", stats)
	}
	rows, _ := pg.ListMonitorFindings("", 100)
	for _, f := range rows {
		if f.RepoID == repoID {
			t.Fatalf("拉取失败不得落任何行: %+v", f)
		}
	}
	rp, _ := pg.GetMonitorRepo(repoID)
	if rp == nil || rp.LastSeenSHA != seen {
		t.Fatalf("拉取失败游标绝不动: %+v", rp)
	}
}

func TestMonitorPollerJudgeTimeoutPendingReview(t *testing.T) {
	_, pg := monitorPollTestServer(t)
	repoID, err := pg.UpsertMonitorRepo(monitorPollTestPrefix+"timeout", "main", "", "", "tester", true)
	if err != nil {
		t.Fatal(err)
	}
	sha := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	gh := &monitorFakeGH{page: &MonitorCommitPage{Commits: []MonitorCommit{
		{SHA: sha, Message: "fix: something security-ish", Date: time.Now()},
	}}}
	slow := &monitorMockLLM{sleep: 300 * time.Millisecond, resp: `{"verdict":true,"reason":"r"}`}
	judge := NewMonitorJudge(slow)
	judge.Timeout = 50 * time.Millisecond
	p := NewMonitorPoller(pg, judge)
	p.source = func(repo *db.MonitorRepo) (MonitorCommitSource, error) { return gh, nil }

	stats, err := p.pollRound(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.PendingReview != 1 || stats.JudgeUndecided != 1 || stats.CursorAdvanced != 1 {
		t.Fatalf("Judge 超时应入 pending_review 且游标推进: %+v", stats)
	}
	rows, _ := pg.ListMonitorFindings(db.MonitorStatusPendingReview, 50)
	found := false
	for _, f := range rows {
		if f.RepoID == repoID && f.SHA == sha && strings.HasPrefix(f.Title, "待审：") {
			found = true
		}
	}
	if !found {
		t.Fatal("超时应落可追踪 pending_review 行")
	}
	rp, _ := pg.GetMonitorRepo(repoID)
	if rp == nil || rp.LastSeenSHA != sha {
		t.Fatalf("明确入人工队列后游标应推进: %+v", rp)
	}
}

func TestMonitorPollerStorageFailureNoAdvance(t *testing.T) {
	_, pg := monitorPollTestServer(t)
	repoID, err := pg.UpsertMonitorRepo(monitorPollTestPrefix+"storefail", "main", "", "", "tester", true)
	if err != nil {
		t.Fatal(err)
	}
	sha := "cccccccccccccccccccccccccccccccccccccccc"
	gh := &monitorFakeGH{page: &MonitorCommitPage{Commits: []MonitorCommit{
		{SHA: sha, Message: "fix: CVE-2024-1111 thing", Date: time.Now()},
	}}}
	p := NewMonitorPoller(pg, NewMonitorJudge(&monitorMockLLM{resp: `{"verdict":true,"reason":"r"}`}))
	p.source = func(repo *db.MonitorRepo) (MonitorCommitSource, error) { return gh, nil }

	// 注入存储失败面（测试缝隙——生产默认 pg.RecordMonitorFinding）。
	calls := 0
	p.store = func(repoID2 int64, sha2, title, cveID, ghsaID, status string) (bool, error) {
		calls++
		return false, errors.New("db down 模拟")
	}
	stats, err := p.pollRound(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.StorageFailures != 1 || stats.CursorAdvanced != 0 || calls != 1 {
		t.Fatalf("存储失败应游标不动: %+v calls=%d", stats, calls)
	}
	rp, _ := pg.GetMonitorRepo(repoID)
	if rp == nil || rp.LastSeenSHA != "" {
		t.Fatalf("存储失败游标绝不动（commit 不丢）: %+v", rp)
	}

	// 恢复存储面后重拉同窗：commit 不丢（findings 幂等闸兜底）。
	p.store = pg.RecordMonitorFinding
	stats, err = p.pollRound(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.FindingsNew != 1 || stats.CursorAdvanced != 1 {
		t.Fatalf("恢复后重拉应成功处理: %+v", stats)
	}
	rows, _ := pg.ListMonitorFindings(db.MonitorStatusNew, 50)
	found := false
	for _, f := range rows {
		if f.RepoID == repoID && f.SHA == sha {
			found = true
		}
	}
	if !found {
		t.Fatal("恢复后 commit 应落候选行")
	}
}

func TestMonitorPollerShutdownAndThrottle(t *testing.T) {
	s, pg := monitorPollTestServer(t)
	repoID, err := pg.UpsertMonitorRepo(monitorPollTestPrefix+"shutdown", "main", "", "", "tester", true)
	if err != nil {
		t.Fatal(err)
	}

	// ① shutdown：在途轮次被 Stop() 取消即刻退出（goroutine 不悬挂）。
	gh1 := &monitorFakeGH{page: &MonitorCommitPage{Commits: []MonitorCommit{
		{SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Message: "m", Date: time.Now()},
	}}}
	slow := &monitorMockLLM{sleep: 2 * time.Second, resp: `{"verdict":true,"reason":"r"}`}
	judge := NewMonitorJudge(slow)
	judge.Timeout = 3 * time.Second
	p1 := NewMonitorPoller(pg, judge)
	p1.source = func(repo *db.MonitorRepo) (MonitorCommitSource, error) { return gh1, nil }
	done := make(chan struct{})
	go func() {
		_, _ = p1.pollRound(p1.baseCtx)
		close(done)
	}()
	time.Sleep(150 * time.Millisecond) // 进入 Judge 慢调用在途窗口
	p1.Stop()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Stop 后轮次应即刻退出")
	}

	// ② 节流闸：sweep 水位未到点零开销（sc.now 注入时钟），不得触发轮次。
	gh2 := &monitorFakeGH{page: &MonitorCommitPage{}}
	p2 := NewMonitorPoller(pg, NewMonitorJudge(&monitorMockLLM{resp: `{"verdict":true,"reason":"r"}`}))
	p2.source = func(repo *db.MonitorRepo) (MonitorCommitSource, error) { return gh2, nil }
	SetDefaultMonitorPoller(p2)
	t.Cleanup(func() { SetDefaultMonitorPoller(nil) })

	sc := newScheduler(s)
	sc.now = func() time.Time { return time.Now() }
	_ = pg.SetSchedState(schedKeyMonitorSweep, fmt.Sprintf("%d", time.Now().Unix())) // 模拟 init 重种
	sc.maybeMonitorPoll()                                                            // 未到点：零触发
	time.Sleep(200 * time.Millisecond)
	if len(gh2.seen) != 0 {
		t.Fatalf("节流未到点不得触发轮次: %v", gh2.seen)
	}

	// ③ 时钟推进过间隔后触发：异步轮次运行（fake GitHub 在场，零真实出网）。
	sc.now = func() time.Time { return time.Now().Add(monitorPollSweepInterval + time.Minute) }
	sc.maybeMonitorPoll()
	deadline := time.Now().Add(3 * time.Second)
	for len(gh2.seen) == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if len(gh2.seen) == 0 {
		t.Fatal("过节流间隔后应触发轮次（fake GitHub 在场）")
	}
	_ = repoID
}

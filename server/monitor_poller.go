package server

// monitor_poller.go：Monitor 增量轮询 poller（B-09F）。
//
// 唯一目标：拉取 enabled 仓库的新提交（B-09D 客户端）→ 逐提交判定（B-09E
// 判定器，经现有 LLM pool）→ 持久化候选结果（B-09B 存储）→ 成功处理/明确
// 入人工队列后推进 last_seen_sha 游标。
//
// 安全与边界纪律（卡要求逐条）：
//   - 注册入口=B-09A 核实的 scheduler step() 前缀区（maybeCVEMatch 同款：
//     15 分钟节流水位 + atomic.Bool 轮次互斥 + sc.now 可注入时钟 + 批次异步
//     goroutine——轮次有界必退出，绝不新起永不关闭 goroutine）；
//   - 游标纪律：GitHub 拉取失败→整仓跳过游标不动；存储失败→中止本仓游标
//     不动（下轮重拉同窗，findings (repo_id,sha) 幂等闸兜底绝不丢 commit
//     绝不重复行）；判定未决（模型错误/超时/unsure）→ pending_review 落行
//     （可追踪）后游标才推进；安全更新→new 候选行后推进；非安全更新→不落
//     行但游标推进（已成功处理）；
//   - 有界面：每轮仓库数（10）、每仓提交数（50，按时间序最旧先处理——游标
//     停在最后一个已处理 sha，下轮 stop-at-seen 续拉绝不跳过）、轮次总超时
//     （5min）、轮次内串行（并发=1，轮间互斥）；
//   - 与 CVE 同步完全解耦：只读 monitor_repos/写 monitor_findings，不触碰
//     cve_hints/cve_match 面；不依赖 B-01/B-02 在线（LLM 未配置时判定器
//     nil→全部 pending_review 可追踪，绝不半装配硬判）。
//
// 提交处理顺序语义（如实申报）：ListNewCommits 返回新→旧；poller 倒序按
// 时间序（最旧先）处理，游标逐提交推进到最后一个已处理 sha。提交数超
// commitsPerRepo 截断时游标停在已处理边界，下轮以该游标 stop-at-seen 续拉
// 中段提交（绝不静默跳过任何 commit——B-09D 分页上限 1000 与本层 50 的组合
// 在极端积压下按轮收敛）。

import (
	"context"
	"fmt"
	"log"
	"regexp"
	"sort"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/Autumn-27/artex/db"
)

// 轮次边界常量（卡要求 4：并发数/每轮仓库数/每仓提交数/执行时间全有界）。
const (
	monitorPollSweepInterval  = 15 * time.Minute // 节流间隔（maybeCVEMatch 同款）
	monitorPollReposPerRound  = 10               // 每轮仓库数上限（id ASC 确定性）
	monitorPollCommitsPerRepo = 50               // 每仓提交数上限（时间序最旧先）
	monitorPollRoundTimeout   = 5 * time.Minute  // 轮次总超时
	monitorPollTitleCap       = 200              // 候选发现标题截断
)

// schedKeyMonitorSweep 是 poller 节流水位键（上次尝试，含失败——失败下轮照常
// 重试，游标语义由 per-repo last_seen_sha 承担，sweep 只管节流）。
const schedKeyMonitorSweep = "last_monitor_poll_sweep"

var monitorCVEIDRe = regexp.MustCompile(`CVE-[0-9]{4}-[0-9]{4,}`)
var monitorGHSAIDRe = regexp.MustCompile(`GHSA(-[23456789cfghjmpqrvwx]{4}){3}`)

// MonitorCommitSource 是 poller 的拉取面（*MonitorGitHubClient 的子集接口
// ——测试注入 fake，生产注入真客户端）。
type MonitorCommitSource interface {
	ListNewCommits(ctx context.Context, repoURL, branch, lastSeenSHA, authRef string) (*MonitorCommitPage, error)
}

// MonitorStoreFn 是持久化面（默认 pg.RecordMonitorFinding；测试注入失败态）。
type MonitorStoreFn func(repoID int64, sha, title, cveID, ghsaID, status string) (bool, error)

// MonitorCursorFn 是游标推进面（默认 pg.UpdateMonitorRepoCursor；测试可观测）。
type MonitorCursorFn func(repoID int64, sha string) error

// MonitorPoller 是轮询器（轮次互斥 + 可取消）。
type MonitorPoller struct {
	pg     *db.DB
	source func(repo *db.MonitorRepo) (MonitorCommitSource, error) // 每仓拉取面工厂
	judge  *MonitorJudge
	store  MonitorStoreFn
	cursor MonitorCursorFn

	// 有界面。
	reposPerRound  int
	commitsPerRepo int
	roundTimeout   time.Duration

	running atomic.Bool     // 轮次互斥（maybeMonitorPoll CAS 闸）
	baseCtx context.Context // poller 生命周期 ctx（Stop 取消在途轮次）
	stop    context.CancelFunc
}

// MonitorRoundStats 是一轮的如实计数。
type MonitorRoundStats struct {
	Repos           int
	Commits         int
	FindingsNew     int
	PendingReview   int
	NotSecurity     int
	CursorAdvanced  int
	FetchFailures   int
	StorageFailures int
	JudgeUndecided  int
	Capped          int // 触顶截断的仓库数（提交数上限）
}

// NewMonitorPoller 构造 poller（生产面；source 工厂默认按仓库 base_url 构建
// B-09D 客户端 + 默认 auth_ref 解析面）。
func NewMonitorPoller(pg *db.DB, judge *MonitorJudge) *MonitorPoller {
	ctx, cancel := context.WithCancel(context.Background())
	p := &MonitorPoller{
		pg:             pg,
		judge:          judge,
		reposPerRound:  monitorPollReposPerRound,
		commitsPerRepo: monitorPollCommitsPerRepo,
		roundTimeout:   monitorPollRoundTimeout,
		baseCtx:        ctx,
		stop:           cancel,
	}
	p.source = func(repo *db.MonitorRepo) (MonitorCommitSource, error) {
		base := repo.GitHubBaseURL
		if base == "" {
			base = "https://api.github.com"
		}
		return monitorGHNew(base, monitorDefaultTokenResolver)
	}
	p.store = pg.RecordMonitorFinding
	p.cursor = pg.UpdateMonitorRepoCursor
	return p
}

// Stop 取消在途轮次（shutdown 退出面——轮次本身还有 roundTimeout 兜底必退出）。
func (p *MonitorPoller) Stop() { p.stop() }

// defaultMonitorPoller 是进程级注册表（B-08E DepAuditRunner 同款：接线点零
// 引用持有，测试可先注入 fake 再接线）。
var defaultMonitorPoller atomic.Pointer[MonitorPoller]

// SetDefaultMonitorPoller / DefaultMonitorPoller 注册与读取。
func SetDefaultMonitorPoller(p *MonitorPoller) { defaultMonitorPoller.Store(p) }
func DefaultMonitorPoller() *MonitorPoller     { return defaultMonitorPoller.Load() }

// maybeMonitorPoll 是 scheduler step() 前缀区的注册入口（maybeCVEMatch 同款
// 形态：节流水位 + 轮次互斥 + 异步 goroutine；与 CVE 同步零耦合）。poller
// 未装配时惰性自建（pg + 全局判定器——LLM 未配置时 judge=nil→全 pending_review）。
func (sc *Scheduler) maybeMonitorPoll() {
	if sc == nil || sc.pg == nil || sc.s == nil {
		return
	}
	last, _ := strconv.ParseInt(sc.mustState(schedKeyMonitorSweep), 10, 64)
	now := sc.now().Unix()
	if now-last < int64(monitorPollSweepInterval/time.Second) {
		return
	}
	_ = sc.pg.SetSchedState(schedKeyMonitorSweep, strconv.FormatInt(now, 10))
	p := DefaultMonitorPoller()
	if p == nil {
		p = NewMonitorPoller(sc.pg, sc.s.MonitorJudgeFromServer())
		SetDefaultMonitorPoller(p)
	}
	if p.running.CompareAndSwap(false, true) {
		go func() {
			defer p.running.Store(false)
			ctx, cancel := context.WithTimeout(p.baseCtx, p.roundTimeout)
			defer cancel()
			stats, err := p.pollRound(ctx)
			if err != nil {
				log.Printf("[monitor-poll] 轮次失败（游标纪律保证不丢不跳）: %v", err)
			}
			log.Printf("[monitor-poll] 轮次完成: %+v", stats)
		}()
	}
}

// pollRound 跑一轮：enabled 仓库 id ASC 确定性取前 N 个，逐仓处理。
func (p *MonitorPoller) pollRound(ctx context.Context) (MonitorRoundStats, error) {
	var stats MonitorRoundStats
	repos, err := p.pg.ListMonitorRepos(true)
	if err != nil {
		return stats, fmt.Errorf("monitor-poll: 列仓库失败: %w", err)
	}
	if len(repos) > p.reposPerRound {
		repos = repos[:p.reposPerRound]
	}
	stats.Repos = len(repos)
	for _, repo := range repos {
		if ctx.Err() != nil {
			return stats, ctx.Err()
		}
		p.pollRepo(ctx, repo, &stats)
	}
	return stats, nil
}

// pollRepo 处理单仓：拉增量→时间序逐提交判定→落行→游标逐提交推进。
// 拉取失败/存储失败都让游标停在安全边界（不丢不跳）。
func (p *MonitorPoller) pollRepo(ctx context.Context, repo *db.MonitorRepo, stats *MonitorRoundStats) {
	src, err := p.source(repo)
	if err != nil {
		stats.FetchFailures++
		log.Printf("[monitor-poll] repo=%s branch=%s 拉取面构建失败（游标不动）: %v", repo.RepoURL, repo.Branch, err)
		return
	}
	page, err := src.ListNewCommits(ctx, repo.RepoURL, repo.Branch, repo.LastSeenSHA, repo.AuthRef)
	if err != nil {
		stats.FetchFailures++
		log.Printf("[monitor-poll] repo=%s branch=%s 拉取失败（游标不动，下轮重试）: %v", repo.RepoURL, repo.Branch, err)
		return
	}
	// 时间序（最旧先）处理——游标逐提交推进，任何中断都停在已处理边界。
	commits := make([]MonitorCommit, len(page.Commits))
	copy(commits, page.Commits)
	sort.Slice(commits, func(i, j int) bool { return commits[i].Date.Before(commits[j].Date) })
	processed := 0
	for _, c := range commits {
		if ctx.Err() != nil {
			return // 游标停在最后已处理 sha（中断不丢已处理，不跳未处理）
		}
		if processed >= p.commitsPerRepo {
			stats.Capped++
			break
		}
		verdict, jerr := (func() (MonitorJudgeVerdict, error) {
			if p.judge == nil {
				return MonitorJudgeVerdict{Decided: false, Reason: "判定器未装配（LLM 未配置）"}, fmt.Errorf("monitor-poll: 判定器未装配")
			}
			return p.judge.Judge(ctx, &c)
		})()
		cve, ghsa := extractMonitorIDs(c.Message)
		title := monitorTruncate(monitorFirstLine(c.Message), monitorPollTitleCap, nil)
		if jerr != nil || !verdict.Decided {
			// 未判定→pending_review（可追踪：标题带「待审」+判定理由）。
			reason := verdict.Reason
			if reason == "" {
				reason = "判定未决"
			}
			title = "待审：" + monitorTruncate(reason, monitorPollTitleCap-len("待审："), nil)
			if _, serr := p.store(repo.ID, c.SHA, title, cve, ghsa, db.MonitorStatusPendingReview); serr != nil {
				stats.StorageFailures++
				log.Printf("[monitor-poll] repo=%s sha=%s 落 pending_review 失败（游标不动，下轮重试不丢）: %v",
					repo.RepoURL, c.SHA[:8], serr)
				return
			}
			stats.PendingReview++
			stats.JudgeUndecided++
		} else if verdict.SecurityUpdate {
			if _, serr := p.store(repo.ID, c.SHA, "安全更新候选："+title, cve, ghsa, db.MonitorStatusNew); serr != nil {
				stats.StorageFailures++
				log.Printf("[monitor-poll] repo=%s sha=%s 落候选失败（游标不动，下轮重试不丢）: %v",
					repo.RepoURL, c.SHA[:8], serr)
				return
			}
			stats.FindingsNew++
		} else {
			stats.NotSecurity++ // 非安全更新：不落行（已成功处理）
		}
		// 游标逐提交推进：成功处理/明确入人工队列后才走到这里（卡要求 2）。
		if cerr := p.cursor(repo.ID, c.SHA); cerr != nil {
			stats.StorageFailures++
			log.Printf("[monitor-poll] repo=%s sha=%s 游标推进失败（下轮同窗重拉，幂等闸兜底）: %v",
				repo.RepoURL, c.SHA[:8], cerr)
			return
		}
		stats.CursorAdvanced++
		processed++
	}
	stats.Commits += processed
}

// extractMonitorIDs 从提交 message 提取 CVE/GHSA 引用（首见；形态闸由存储层
// db.MonitorFinding 校验兜底——提取只做识别不做验证）。
func extractMonitorIDs(message string) (string, string) {
	cve := ""
	if m := monitorCVEIDRe.FindString(message); m != "" {
		cve = m
	}
	ghsa := ""
	if m := monitorGHSAIDRe.FindString(message); m != "" {
		ghsa = m
	}
	return cve, ghsa
}

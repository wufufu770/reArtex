package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Autumn-27/artex/db"
)

// Scheduler drives P3 triggers: on each tick it fires due interval triggers and
// scans for new findings / newly-met goals (any task) to fire event triggers.
// Each fire enqueues a NEW conversation on the agent's per-agent trigger queue
// (StartTriggeredRun): fires for the same agent run one at a time in FIFO order,
// distinct agents still run concurrently. State is persisted (per-trigger last_fire
// + finding watermark + fired-goal set) so a restart resumes without double-firing.
// Triggers only attach to CUSTOM agents.
type Scheduler struct {
	s    *Server
	pg   *db.DB
	tick time.Duration

	// now 是可注入时钟（0.14.0 批次一 A1）：生产用 time.Now；测试用 fake
	// 控制水位/熔断的推进节奏，不再靠真实等待。
	now func() time.Time

	// curatorRunning 保证同一时刻只有一轮 curator 在跑。
	//
	// 为什么需要：curator 的水位在跑**完之后**才写（崩重启要能立刻再跑一轮）
	// ⇒ 5 秒 tick 期间可能重复判定「该跑」。没有这层互斥，两轮 curator 会并发
	// 跑全库淘汰，而裁决写必须串行（auto-permission-tiers §4.2）。
	curatorRunning atomic.Bool

	// cveMatchRunning 保证同一时刻只有一轮 CVE 反向匹配批次在跑（B-04G；
	// curatorRunning 同款互斥语义）。
	cveMatchRunning atomic.Bool
}

const (
	schedKeyLastFinding    = "last_finding_id"    // watermark: max finding node id fired for
	schedKeyFiredGoals     = "fired_goals"        // JSON array of goal node ids already fired
	schedKeyLastTimeout    = "last_timeout_id"    // watermark: max task id fired for task timeout
	schedKeyLastToolCall   = "last_toolcall_id"   // watermark: max activity id fired for tool call
	schedKeyLastTaskCreate = "last_taskcreate_id" // watermark: max task id fired for task create
	schedKeyLastRevSweep   = "last_review_sweep"  // unix sec: last stale needs_review sweep

	// CVE 反向匹配批次水位（B-04G）：sweep 是节流闸（上次尝试，含失败），
	// window 是 published_after 时间窗水位（仅成功推进——失败下轮重试同窗，
	// 绝不丢窗）。首跑种子 window=启动时刻：存量 CVE 不回扫（scheduler 水位
	// 同款纪律，全量回扫属运维动作）。
	schedKeyCVEMatchSweep  = "last_cve_match_sweep"  // unix sec
	schedKeyCVEMatchWindow = "last_cve_match_window" // RFC3339 UTC
)

// cveMatchSweepInterval 批次节流间隔（15 分钟一轮；steady state 下新 CVE/轮
// 远小于单批 100 上限，批量导入场景按 CVSS 优先吃高价值子集）。
const cveMatchSweepInterval = 15 * time.Minute

func newScheduler(s *Server) *Scheduler {
	return &Scheduler{s: s, pg: s.m.pg, tick: 5 * time.Second, now: time.Now}
}

// Run loops until ctx is done, ticking the scheduler. Started once from server New.
func (sc *Scheduler) Run(ctx context.Context) {
	if sc.pg == nil {
		return
	}
	sc.init()
	t := time.NewTicker(sc.tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			sc.s.reconcileConcurrency() // 并发上限:有空位就把排队任务补位启动
			sc.step()
		}
	}
}

// init seeds the watermarks on first run so pre-existing findings/goals don't all
// fire at once — only events created AFTER the scheduler first starts count.
func (sc *Scheduler) init() {
	if sc.mustState(schedKeyLastFinding) == "" {
		var maxID int64
		if evs, err := sc.pg.NewFindingsSince(0); err == nil {
			for _, e := range evs {
				if e.NodeID > maxID {
					maxID = e.NodeID
				}
			}
		}
		_ = sc.pg.SetSchedState(schedKeyLastFinding, strconv.FormatInt(maxID, 10))
	}
	if sc.mustState(schedKeyFiredGoals) == "" {
		set := map[int64]bool{}
		if evs, err := sc.pg.MetGoals(); err == nil {
			for _, e := range evs {
				set[e.NodeID] = true
			}
		}
		sc.saveFiredGoalSet(set)
	}
	if sc.mustState(schedKeyLastTimeout) == "" {
		var maxID int64
		if evs, err := sc.pg.TimedOutTasksSince(0); err == nil {
			for _, e := range evs {
				if e.NodeID > maxID {
					maxID = e.NodeID
				}
			}
		}
		_ = sc.pg.SetSchedState(schedKeyLastTimeout, strconv.FormatInt(maxID, 10))
	}
	if sc.mustState(schedKeyLastToolCall) == "" {
		var maxID int64
		if evs, err := sc.pg.NewToolCallsSince(0); err == nil {
			for _, e := range evs {
				if e.NodeID > maxID {
					maxID = e.NodeID
				}
			}
		}
		_ = sc.pg.SetSchedState(schedKeyLastToolCall, strconv.FormatInt(maxID, 10))
	}
	if sc.mustState(schedKeyLastTaskCreate) == "" {
		var maxID int64
		if evs, err := sc.pg.NewTasksSince(0); err == nil {
			for _, e := range evs {
				if e.NodeID > maxID {
					maxID = e.NodeID
				}
			}
		}
		_ = sc.pg.SetSchedState(schedKeyLastTaskCreate, strconv.FormatInt(maxID, 10))
	}
	// B-09F：Monitor poller 节流水位无条件随每次调度器启动重种——首轮 poll
	// 固定落在启动后 15 分钟（生产：避开启动风暴；测试：Server 生命周期远短
	// 于该间隔，结构性保证测试零真实出网。如实申报：重启会重置首轮延迟）。
	_ = sc.pg.SetSchedState(schedKeyMonitorSweep, strconv.FormatInt(sc.now().Unix(), 10))
}

func (sc *Scheduler) step() {
	// curator 每小时跑一次（小时级水位）：策略晋级 / 淘汰是**重裁决**（刷 utility
	// 物化视图 + 扫一遍全库），不能挂在 5 秒 tick 的同步路径上 ⇒ 异步跑。
	//
	// 放在 step() 最前面：step() 在「一个 trigger 都没有」时会提前返回，curator
	// 不能被这个提前返回挡掉（否则没配 trigger 时策略永远不晋级）。
	sc.dispatchStrategyCurator()
	sc.sweepStaleReviews()
	sc.maybeCVEMatch()
	sc.maybeMonitorPoll() // B-09F：Monitor 增量轮询（节流水位+轮次互斥，形态同 maybeCVEMatch；实现与状态全在 monitor_poller.go）
	// Amber curator 观察模式（0.15.40，批次 J2）：每小时水位闸内扫隔离区产
	// 拟晋升清单——观察态不落写；amber 关闭时函数内直接返回（零开销）。
	go sc.s.AmberCuratorObserve()
	// 偏锋轮转唤醒（EDGE E7）：放在 triggers 提前返回之前——edge_exec 是内置
	// agent，不能配 agent_triggers（HTTP 面对内置 400），它的轮间接力走这里。
	schedCounters.tickTotal.Add(1)
	sc.fireEdgeWake()
	// 意图仲裁唤醒（0.14.1 批次二）：open 冲突按水位唤醒 arbiter 调研判定。
	sc.fireConflictArbitration()
	triggers, err := sc.pg.ListEnabledTriggers()
	if err != nil {
		return
	}
	if len(triggers) == 0 {
		return
	}
	sc.fireIntervals(triggers)
	sc.fireFindings(triggers)
	sc.fireGoals(triggers)
	sc.fireTaskTimeouts(triggers)
	sc.fireToolCalls(triggers)
	sc.fireTaskCreates(triggers)
}

// dispatchStrategyCurator 判定是否该跑 curator，并在**独立 goroutine** 里跑。
//
// 为什么异步：curator 一轮要刷 utility 物化视图 + 扫一遍全库策略。同步挂在
// 5 秒 tick 上会把 tick 拖慢到秒级 ⇒ 触发器派发全部滞后。
//
// 水位判断有两道：这里判一次（省掉绝大多数 tick 的 goroutine 开销），
// runStrategyCurator → RunCuratorIfDue 里再判一次（真正写水位的那道闸）。
func (sc *Scheduler) dispatchStrategyCurator() {
	if sc == nil || sc.pg == nil {
		return
	}
	if !sc.shouldRunCurator() {
		return
	}
	// 互斥：同一时刻只允许一轮 curator 在跑（裁决写必须串行）。
	if !sc.curatorRunning.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer sc.curatorRunning.Store(false)
		sc.runStrategyCurator()
	}()
}

// sweepStaleReviews 每小时跑一次：把挂起超 24h 的 needs_review 项自动结案为
// dismissed（方案 v3 缺陷 D4——对抗复核队列不能无限堆积）。水位未到点时是
// 一次字符串比较，零开销。
func (sc *Scheduler) sweepStaleReviews() {
	if sc.pg == nil {
		return
	}
	last, _ := strconv.ParseInt(sc.mustState(schedKeyLastRevSweep), 10, 64)
	now := time.Now().Unix()
	if now-last < int64(time.Hour/time.Second) {
		return
	}
	_ = sc.pg.SetSchedState(schedKeyLastRevSweep, strconv.FormatInt(now, 10))
	// TTL 可配（settings.sod_review_ttl_hours，小时；默认 24，下限 1）——运维按
	// 积压承受度调（0.15.5 靶场实测一天积 120 条 pending）。扫描间隔仍为 1h。
	ttlHours := sc.pg.GetInt(settingSodReviewTtlHours, 24)
	if ttlHours < 1 {
		ttlHours = 1
	}
	n, err := sc.pg.DismissStaleAdversarialReviews(time.Duration(ttlHours) * time.Hour)
	if err != nil {
		log.Printf("[scheduler] needs_review 过期结案失败: %v", err)
		return
	}
	if n > 0 {
		log.Printf("[scheduler] needs_review 过期闸：自动结案 %d 条挂起超 %dh 的复核项", n, ttlHours)
	}
}

// maybeCVEMatch CVE 反向匹配批次触发（B-04G）：15 分钟节流闸 + 双水位（sweep
// 尝试闸 / window 成功推进）。批次异步跑（sweepStaleReviews 同款：不占 5 秒
// tick 同步路径）；同一时刻只允许一轮（cveMatchRunning 互斥，curatorRunning
// 同款）。CVE_MCP_URL 未配置时批次自身静默降级（B-01E），这里只在水位种子
// 阶段零开销返回。
func (sc *Scheduler) maybeCVEMatch() {
	if sc.pg == nil {
		return
	}
	last, _ := strconv.ParseInt(sc.mustState(schedKeyCVEMatchSweep), 10, 64)
	now := sc.now().Unix()
	if now-last < int64(cveMatchSweepInterval/time.Second) {
		return
	}
	_ = sc.pg.SetSchedState(schedKeyCVEMatchSweep, strconv.FormatInt(now, 10))
	window := sc.mustState(schedKeyCVEMatchWindow)
	if window == "" {
		window = sc.now().UTC().Format(time.RFC3339)
		_ = sc.pg.SetSchedState(schedKeyCVEMatchWindow, window)
	}
	if sc.cveMatchRunning.CompareAndSwap(false, true) {
		go func() {
			defer sc.cveMatchRunning.Store(false)
			w, err := time.Parse(time.RFC3339, window)
			if err != nil {
				log.Printf("[cve-batch] 水位 %q 非法，重置为当前时刻: %v", window, err)
				_ = sc.pg.SetSchedState(schedKeyCVEMatchWindow, time.Now().UTC().Format(time.RFC3339))
				return
			}
			stats, err := sc.s.RunCVEMatchBatch(context.Background(), w)
			if err != nil {
				// 失败不推进 window：下轮重试同一时间窗（绝不丢窗）。
				log.Printf("[cve-batch] 批次失败（window 保持 %s）: %v", window, err)
				return
			}
			if stats.Unavailable {
				// 未配置同样不推进 window：sidecar 恢复后从旧窗继续（台账幂等）。
				log.Printf("[cve-batch] CVE_MCP_URL 未配置，本轮静默跳过（swept=0）")
				return
			}
			_ = sc.pg.SetSchedState(schedKeyCVEMatchWindow, sc.now().UTC().Format(time.RFC3339))
			if stats.Swept > 0 || stats.Emitted > 0 {
				log.Printf("[cve-batch] swept=%d no_tokens=%d matched_assets=%d emitted=%d deduped=%d throttled=%d skipped=%d",
					stats.Swept, stats.NoTokens, stats.MatchedAssets, stats.Emitted, stats.Deduped, stats.Throttled, stats.Skipped)
			}
		}()
	}
}

// fireIntervals fires triggers whose interval has elapsed since last_fire.
func (sc *Scheduler) fireIntervals(triggers []*db.AgentTrigger) {
	now := time.Now()
	for _, tr := range triggers {
		if tr.IntervalSec <= 0 {
			continue
		}
		due := tr.LastFire == nil || now.Sub(*tr.LastFire) >= time.Duration(tr.IntervalSec)*time.Second
		if !due {
			continue
		}
		_ = sc.pg.TouchTriggerFire(tr.ID)
		ctx := "\n\n【本次为定时触发】" + now.Format(" 2006-01-02 15:04:05 MST")
		sc.s.StartTriggeredRun(tr.AgentKey, fmt.Sprintf("定时触发 · %s", now.Format("15:04")), tr.IntervalMessage+ctx, 0, false, "", "")
	}
}

// fireFindings fires on_finding triggers for findings above the persisted
// watermark (monotonic node id → no double-fire across restarts).
func (sc *Scheduler) fireFindings(triggers []*db.AgentTrigger) {
	var want []*db.AgentTrigger
	for _, tr := range triggers {
		if tr.OnFinding {
			want = append(want, tr)
		}
	}
	last, _ := strconv.ParseInt(sc.mustState(schedKeyLastFinding), 10, 64)
	events, err := sc.pg.NewFindingsSince(last)
	if err != nil || len(events) == 0 {
		return
	}
	// Advance the watermark whether or not any on_finding trigger is active: a
	// finding fires only for triggers live at the moment it appears. Otherwise a
	// trigger enabled later would replay the entire historical backlog at once.
	maxID := last
	for _, e := range events {
		if e.NodeID > maxID {
			maxID = e.NodeID
		}
		if len(want) == 0 {
			continue
		}
		msgCtx := fmt.Sprintf("\n\n【本次由任务发现 finding 触发】\n发现: [%s/%s] %s",
			e.VulnClass, e.Severity, e.Summary)
		for _, tr := range want {
			sc.s.StartTriggeredRun(tr.AgentKey, fmt.Sprintf("finding 触发 · task#%d", e.TaskID), tr.FindingMessage+msgCtx, e.TaskID, true, e.TaskDesc, e.TaskGoal)
		}
	}
	_ = sc.pg.SetSchedState(schedKeyLastFinding, strconv.FormatInt(maxID, 10))
}

// fireGoals fires on_goal_met triggers for met goals not yet in the fired set.
//
// G3（方案 v10 §15）水位收窄：候选流改走 MetGoalsActive()（活跃任务 + 刚终态 1h 内
// 的任务），不再每 5s 全表 join。终态已久的任务其 met goal 早已进已触发集合、
// 也不可能再新达标（引擎停了就没有 prove_goal），收窄不影响触发语义；重启播种仍用
// 全表 MetGoals()（见 init）。
//
// 集合写入确认（方案 ②）：saveFiredGoalSet 仅在本次确实【新增】了未触发 id 时调用
// （changed 标记），纯命中的 tick 既不重写 JSON 也不发 UPDATE —— 本来就是增量的，
// 此处只注明不改动。
func (sc *Scheduler) fireGoals(triggers []*db.AgentTrigger) {
	var want []*db.AgentTrigger
	for _, tr := range triggers {
		if tr.OnGoalMet {
			want = append(want, tr)
		}
	}
	events, err := sc.pg.MetGoalsActive()
	if err != nil || len(events) == 0 {
		return
	}
	// Mark goals as consumed whether or not a trigger is active, so enabling an
	// on_goal_met trigger later doesn't replay every already-met goal.
	fired := sc.firedGoalSet()
	changed := false
	for _, e := range events {
		if fired[e.NodeID] {
			continue
		}
		fired[e.NodeID] = true
		changed = true
		if len(want) == 0 {
			continue
		}
		msgCtx := fmt.Sprintf("\n\n【本次由任务完成目标触发】\n达成目标: %s", e.Summary)
		for _, tr := range want {
			sc.s.StartTriggeredRun(tr.AgentKey, fmt.Sprintf("目标触发 · task#%d", e.TaskID), tr.GoalMessage+msgCtx, e.TaskID, true, e.TaskDesc, e.TaskGoal)
		}
	}
	if changed {
		sc.saveFiredGoalSet(fired)
	}
}

// fireTaskTimeouts fires on_task_timeout triggers for tasks that newly reached
// status='timeout' above the persisted watermark (task id → no double-fire).
func (sc *Scheduler) fireTaskTimeouts(triggers []*db.AgentTrigger) {
	var want []*db.AgentTrigger
	for _, tr := range triggers {
		if tr.OnTaskTimeout {
			want = append(want, tr)
		}
	}
	last, _ := strconv.ParseInt(sc.mustState(schedKeyLastTimeout), 10, 64)
	events, err := sc.pg.TimedOutTasksSince(last)
	if err != nil || len(events) == 0 {
		return
	}
	// Advance the watermark even with no active trigger — see fireFindings.
	maxID := last
	for _, e := range events {
		if e.NodeID > maxID {
			maxID = e.NodeID
		}
		if len(want) == 0 {
			continue
		}
		msgCtx := "\n\n【本次由任务超时触发】"
		for _, tr := range want {
			sc.s.StartTriggeredRun(tr.AgentKey, fmt.Sprintf("超时触发 · task#%d", e.TaskID), tr.TaskTimeoutMessage+msgCtx, e.TaskID, true, e.TaskDesc, e.TaskGoal)
		}
	}
	_ = sc.pg.SetSchedState(schedKeyLastTimeout, strconv.FormatInt(maxID, 10))
}

// fireTaskCreates fires on_task_create triggers for tasks newly created above the
// persisted watermark (task id → no double-fire across restarts).
func (sc *Scheduler) fireTaskCreates(triggers []*db.AgentTrigger) {
	var want []*db.AgentTrigger
	for _, tr := range triggers {
		if tr.OnTaskCreate {
			want = append(want, tr)
		}
	}
	last, _ := strconv.ParseInt(sc.mustState(schedKeyLastTaskCreate), 10, 64)
	events, err := sc.pg.NewTasksSince(last)
	if err != nil || len(events) == 0 {
		return
	}
	// Advance the watermark even with no active trigger — see fireFindings.
	maxID := last
	for _, e := range events {
		if e.NodeID > maxID {
			maxID = e.NodeID
		}
		if len(want) == 0 {
			continue
		}
		msgCtx := "\n\n【本次由任务创建触发】"
		for _, tr := range want {
			sc.s.StartTriggeredRun(tr.AgentKey, fmt.Sprintf("任务创建触发 · task#%d", e.TaskID), tr.TaskCreateMessage+msgCtx, e.TaskID, true, e.TaskDesc, e.TaskGoal)
		}
	}
	_ = sc.pg.SetSchedState(schedKeyLastTaskCreate, strconv.FormatInt(maxID, 10))
}

// fireToolCalls fires on_tool_call triggers for tool calls (tool_result rows) above
// the persisted watermark whose tool name is in the trigger's selected set. The fire
// message carries the task id/desc/goal + tool name + (truncated) input & output.
func (sc *Scheduler) fireToolCalls(triggers []*db.AgentTrigger) {
	var want []*db.AgentTrigger
	for _, tr := range triggers {
		if tr.OnToolCall && len(tr.ToolNames) > 0 {
			want = append(want, tr)
		}
	}
	last, _ := strconv.ParseInt(sc.mustState(schedKeyLastToolCall), 10, 64)
	events, err := sc.pg.NewToolCallsSince(last)
	if err != nil || len(events) == 0 {
		return
	}
	// Advance the watermark even with no active trigger — see fireFindings.
	maxID := last
	for _, e := range events {
		if e.NodeID > maxID {
			maxID = e.NodeID
		}
		if len(want) == 0 {
			continue
		}
		// A failed finding write has no committed finding to report. Keep other
		// tool-error triggers available for user-defined automation.
		if e.ToolIsErr && e.Tool == "report_finding" {
			continue
		}
		errTag := ""
		if e.ToolIsErr {
			errTag = "[error] "
		}
		msgCtx := fmt.Sprintf("\n\n【本次由工具调用触发】\n工具: %s\n入参: %s\n返回: %s%s",
			e.Tool, trunc(e.ToolInput, 1500), errTag, trunc(e.ToolOutput, 1500))
		for _, tr := range want {
			if !containsFold(tr.ToolNames, e.Tool) {
				continue
			}
			sc.s.StartTriggeredRun(tr.AgentKey, fmt.Sprintf("工具触发 · %s · task#%d", e.Tool, e.TaskID), tr.ToolCallMessage+msgCtx, e.TaskID, true, e.TaskDesc, e.TaskGoal)
		}
	}
	_ = sc.pg.SetSchedState(schedKeyLastToolCall, strconv.FormatInt(maxID, 10))
}

// containsFold reports whether name is in set (case-insensitive).
func containsFold(set []string, name string) bool {
	for _, s := range set {
		if strings.EqualFold(s, name) {
			return true
		}
	}
	return false
}

// trunc caps s to max runes, appending an ellipsis + original length when cut.
func trunc(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + fmt.Sprintf("…(已截断,共 %d 字)", len(r))
}

func (sc *Scheduler) mustState(key string) string {
	v, _ := sc.pg.GetSchedState(key)
	return v
}

func (sc *Scheduler) firedGoalSet() map[int64]bool {
	out := map[int64]bool{}
	raw := sc.mustState(schedKeyFiredGoals)
	if raw == "" {
		return out
	}
	var ids []int64
	if json.Unmarshal([]byte(raw), &ids) == nil {
		for _, id := range ids {
			out[id] = true
		}
	}
	return out
}

func (sc *Scheduler) saveFiredGoalSet(set map[int64]bool) {
	ids := make([]int64, 0, len(set))
	for id := range set {
		ids = append(ids, id)
	}
	b, _ := json.Marshal(ids)
	if err := sc.pg.SetSchedState(schedKeyFiredGoals, string(b)); err != nil {
		log.Printf("[scheduler] save fired goals failed: %v", err)
	}
}

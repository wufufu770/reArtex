package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/intercept"
	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/llm"
	"github.com/jackc/pgx/v5/pgconn"
)

// isFKViolation reports whether err is a Postgres foreign-key violation (SQLSTATE
// 23503) — e.g. an activity insert whose exploration_id has no parent row.
func isFKViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503"
}

// dropReason classifies why an activity write was dropped, so the log can be
// grouped/analysed by cause rather than by raw error text.
func dropReason(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23503":
			return "fk_violation(23503,父exploration不存在)"
		case "23505":
			return "unique_violation(23505)"
		default:
			return "pg_error(" + pgErr.Code + ")"
		}
	}
	return "write_error"
}

// bumpDrop increments and returns the running count of dropped (unpersistable)
// activity records for a task. Concurrent planner + worker emits race here, so the
// counter is an atomic behind sync.Map. The count in the log shows loss scale at a
// glance instead of forcing a grep-and-count.
func (e *Engine) bumpDrop(taskID string) int64 {
	v, _ := e.dropCnt.LoadOrStore(taskID, new(int64))
	return atomic.AddInt64(v.(*int64), 1)
}

// preview collapses newlines and trims s to a short rune-safe snippet for one-line
// log output (avoids dumping a multi-KB summary/detail into the log).
func preview(s string, n int) string {
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	r := []rune(s)
	if len(r) > n {
		return string(r[:n]) + "…"
	}
	return string(r)
}

// model_error（provider/API 故障：LLM 层瞬时重试耗尽，或流已开始后中途断流）
// 收场的 work 不是「试过没做完」也不是真失败，而是外部抖动。默认把它当永久
// blocked 会白丢一条意图，所以这里对该终态额外重跑几次，每次之间退避一下，给
// provider 恢复的时间；重试期间若被暂停/终止/取消则立即让位给对应分支处理。
const (
	modelErrorRetries      = 2               // model_error 收场后额外重试的次数
	modelErrorRetryBackoff = 3 * time.Second // 每次重试前的退避
	workControlWaitTimeout = 30 * time.Second
	steerMsgMaxRunes       = 2000
	steerQueueMax          = 10
)

// KillSettleGrace is how long a killed work is given to land its conclusions
// before the run is torn down. A hard cancel loses whatever the agent had in
// context but had not yet written back (see the wrap-up semantics in
// agent/wrapup.go): the SDK only enters its settlement phase when a *budget*
// is exhausted — a cancelled parent context is an immediate abort with no
// write-back window at all. So a kill first hands the worker a settle prompt
// and only cancels once this grace has passed.
//
// 宽限期是【有界】的：到期必取消，一次 kill 最多让这条 work 多活这么久。
const KillSettleGrace = 30 * time.Second

// killSettlePrompt is the notice a killed work receives so it writes back what
// it already found instead of dropping it on the floor. 它只对【本次执行】有意
// 义（所以不落库：落库会在下一次重跑时变成一条过期的「即将终止」）。
const killSettlePrompt = "【收尾通知】本次执行即将终止。请立即把已经确认的事实、资产与漏洞写回" +
	"（report_finding / insert_assets / record_fact），不要启动新的探测或新的命令。"

// steerPayloadKind marks the durable nodes that carry steering messages. The
// graph's other hint nodes are human/strategic hints; they must never be
// mistaken for a steering message (and vice versa).
const steerPayloadKind = "steer"

// steerScanLimit bounds how many hint nodes one steering read walks. Hints are
// few (the planner itself reads 50), so the bound keeps a pathological graph
// from turning a single delivery into an unbounded scan.
const steerScanLimit = 200

var errWorkControlConflict = errors.New("work control conflict")

// retryableWorkerModelError excludes errors already handled by the task router.
// In particular, a quota error after partial streaming advances the task cursor
// for the next LLM call but must not replay this whole intent on the backup.
func retryableWorkerModelError(reason harness.TerminalReason, err error) bool {
	return reason == harness.ReasonModelError && !isTaskLLMRuntimeError(err)
}

// Engine drives the event-driven exploration loop with real LLM agents
// (docs §4.3/§4.4): on asset/exploration-graph change (debounced) it wakes the
// planner, which reads the route, queries assets, judges goals and emits intents;
// N concurrent work agents claim intents and execute them. There is no
// simulation mode — an LLM provider is required. The planner/worker can be
// (re)installed at runtime (LLM configured from the UI); the loops always run
// but idle until an LLM is set.
type Engine struct {
	m        *Manager
	debounce time.Duration

	bc *Broadcaster // live activity pub/sub (SSE)

	// ActivityMirror / ClaimMirror 是总控会话播报的注入点（server/session_task_mirror.go）：
	// emitActivity / 认领点在主链路旁 best-effort 调用，nil = 未装配（测试、无会话形态）。
	// 为什么做成字段而不是直接 import：Engine 在 Server 之下装配，回调由 New() 注入，
	// 引擎侧不感知会话形态 —— 与 toolTableFallback（db 层注入回调）同一套手法。
	ActivityMirror func(taskID string, r db.Activity)
	ClaimMirror    func(taskID, workerKey string, intentID int64, summary string)
	// LegacyDistiller 是 P5 传承图的终局蒸馏回调（server/legacy.go）：任务 done 的
	// 三处终局点旁 best-effort 异步调用，nil = 未装配。与 freezeReplayCorpus 同点
	// 同款一次性可重入语义（legacyDistilled 记账在 Server 侧）。
	LegacyDistiller func(t *Task)
	// DenialWatcher 是 B6 升级路径的观察者（server/denial_escalation.go）：
	// 工具调用被拒（「Blocked by hook」）时回调 (taskID, toolName, denialMessage)。
	// nil = 不观察。宿主（NewServer）装配。
	DenialWatcher func(taskID, toolName, denialMessage string)
	// AuditSummary 是 v4.1 收尾审计报告的回调（server/auditor_report.go）：
	// 任务落终态（done/failed/timeout/cancelled）时 best-effort 调用，
	// 产出《本任务操作审计摘要》播报到绑定会话 + 落审计页。nil = 不产出。
	AuditSummary func(t *Task)

	// C4 停滞闸：per-task 的零产出意图计数与收尾评估状态（进程内；重启即清，
	// 计数从新任务的实际行为重新累积）。
	// D2 三门收敛：任务级「最近结论物入账」时刻 + 意图延期计数（软着陆）。
	lastConclusionAt map[string]time.Time
	intentExtends    map[string]int
	stagnationMu sync.Mutex
	stagnation   map[string]*taskStagnation

	started  sync.Map // taskID -> bool, so Run is idempotent per task
	lastAct  sync.Map // taskID -> int64 unix, last planner/worker activity (heartbeat)
	llmCalls sync.Map // taskID -> *int64, actual planner/worker/main-agent LLM calls
	paused   sync.Map // taskID -> bool, user-paused (planner + workers idle but loops alive)
	deleting sync.Map // taskID -> bool, delete barrier (no new task-owned writes)
	dropCnt  sync.Map // taskID -> *int64, running count of dropped (unpersistable) activity records

	// deleteMu makes installing the delete barrier atomic with registering a new
	// task operation. Once BeginDelete returns, every admitted writer is reflected
	// in inflight and every later writer is rejected.
	deleteMu sync.RWMutex

	// Every long-lived task goroutine (planner, workers and deadline coordinator)
	// runs under one task-scoped context. Successful deletion cancels that context,
	// waits for all goroutines, then releases every task-level Engine reference.
	runtimeMu sync.Mutex
	runtimes  map[string]*taskRuntime

	// per-task execution context: each planner.Plan / worker.Execute runs under it,
	// so pausing can CANCEL an in-flight run (not just skip the next one). Recreated
	// on resume since cancelling is one-shot. Every cancellation carries a named
	// cause so the activity trace can identify the initiating control path.
	execMu     sync.Mutex
	execCancel map[string]context.CancelCauseFunc
	execCtx    map[string]context.Context

	// Per-work control lets the planner kill a worker and lets the UI pause/cancel
	// one intent without pausing the whole task. The done channel closes only after
	// runWorkerStep has stopped writing and committed its final state.
	workMu sync.Mutex
	work   map[int64]*workExecution

	// steerBox queues planner course-corrections for a running work (keyed by intent
	// id). The worker's PreToolUse hook drains it before its next tool call and hands
	// the message to the model (blocking that call) so it re-plans — no kill needed.
	steerMu  sync.Mutex
	steerBox map[int64][]string

	// killSettleGrace is the grace a killed work gets to land its conclusions
	// before it is torn down. Defaults to KillSettleGrace; tests shorten it.
	killSettleGrace time.Duration

	plannerRound sync.Map // taskID -> int, planner round counter (for UI round separators)

	// strategyProvider 是「给 planner 注入策略摘要」的宿主侧回调（装配一次，全局共用）。
	//
	// 为什么存成字段而不是每次现造：provider 是无状态闭包，装一次即可；每次现造
	// 会让 planner.strategyContext 每轮被改写一次。
	strategyProvider func(taskID int64) string
	// strategyWired 记住「这个 planner 已经装过策略上下文回调」（planner 指针 -> struct{}）。
	//
	// 为什么记：planner.strategyContext 是普通字段，每写一次就是一次写；与超时
	// 收尾那一轮（另一个 goroutine 读同一字段）并发时构成数据竞争。装一次就够
	// ⇒ 每个 planner 只写一次。
	strategyWired sync.Map

	// replayFrozen 记住「这个任务的重放语料已冻结」（任务 id -> struct{}）。
	//
	// 为什么要有这层记账：任务进终态后 worker 循环仍在空转（每秒一轮），没有它
	// 就会每秒重算一次资产签名 + usage + findings ⇒ 白烧数据库。冻结本身是幂等的
	// （ON CONFLICT DO NOTHING），这里只是省掉重复劳动。
	replayFrozen sync.Map

	// 任务级超时(见 docs/任务级超时与收尾设计.md):
	settling     sync.Map // taskID -> bool, 任务已进入收尾时序(停止派/领新意图)
	deadline     sync.Map // taskID -> int64 unix, 绝对截止时刻(首次运行时盖章;0/缺省=不限)
	stamped      sync.Map // taskID -> bool, first_run_at 是否已盖章(本进程内只盖一次)
	inflight     sync.Map // taskID -> *int64, 在跑的 planner.Plan + worker.Execute 计数(用于 drain)
	coordStarted sync.Map // taskID -> bool, deadline 协调器是否已启动(Run/reload 去重)

	// resolve returns a task's dedicated planner/worker (wired by the server as the
	// authoritative task-router). nil,nil means this task is deliberately unavailable
	// (for example an exhausted failover chain) — there is no global-pair fallback.
	resolve              func(t *Task) (*agent.Planner, *agent.Worker)
	resolveAuthoritative bool
	// readiness reports whether a global LLM provider is configured — the signal behind
	// Ready()/the llm_configured indicator. Wired once at startup; nil → not ready.
	readiness func() bool

	// ---------- 群组闸门（任务大厅）----------
	//
	// 三个回调都由 server 侧注入（默认实现见 NewEngine）：判据（群成员能不能用、
	// 组内上限、额度状态）属于 server 侧的数据层，engine 自己不查群组表，
	// 只问「这次能不能认领」。
	//
	// GroupConcurrencyLimit 报某个 task 所属群组的并发上限；ok=false 表示这个 task
	// 没有群组 —— 平台的绝大多数任务都没有大厅 ⇒ 没有组内闸门 ⇒ 认领路径与从前
	// 一模一样。
	GroupConcurrencyLimit func(taskID int64) (int, bool)
	// GroupRunningIntents 报某个 task 当前有多少个 intent 在执行（state='running'）。
	// 它是组内并发上限的分母：一个群 = 一个 task = 一张探索图。
	GroupRunningIntents func(taskID int64) (int, error)
	// GroupDispatcherClaimable 判定当前 worker 是否应把这次认领让给 auto（分发者）。
	// 返回 true = 让位（本次不认领）；false = 正常参与。
	GroupDispatcherClaimable func(taskID int64, worker string) bool
}

type taskRuntime struct {
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

type workExecution struct {
	cancel context.CancelCauseFunc
	done   chan error
	action string // user action: pause | cancel
	// task is the task this work belongs to. It is nil for a handle that is not
	// backed by a running worker: there is nobody to settle and nowhere to persist.
	task *Task
	// finished is closed once the worker returned. A pending kill grace waits on
	// it, so a work that settled on its own is never cancelled afterwards.
	finished chan struct{}
}

// nextPlannerRound returns the next planner round number for a task (1-based).
func (e *Engine) nextPlannerRound(taskID string) int {
	v, _ := e.plannerRound.LoadOrStore(taskID, 0)
	n := v.(int) + 1
	e.plannerRound.Store(taskID, n)
	return n
}

// Pause stops a task: marks it paused AND cancels any in-flight planner/worker run
// for it (a long worker.Execute would otherwise keep going until it finishes).
func (e *Engine) Pause(taskID string, cause error) {
	e.paused.Store(taskID, true)
	e.cancelExec(taskID, cause)
}

// BeginDelete installs an execution barrier before task data/files are removed.
// The temporary pause is not a user pause. The server serializes this transition
// with lifecycle admission and tells AbortDelete whether the persisted task is
// paused/queued if cleanup fails.
func (e *Engine) BeginDelete(taskID string) bool {
	e.deleteMu.Lock()
	if _, loaded := e.deleting.LoadOrStore(taskID, true); loaded {
		e.deleteMu.Unlock()
		return false
	}
	e.paused.Store(taskID, true)
	e.deleteMu.Unlock()
	e.cancelExec(taskID, agent.AbortTaskDeleted)
	return true
}

func (e *Engine) AbortDelete(taskID string, keepPaused bool) {
	e.deleteMu.Lock()
	if !e.IsDeleting(taskID) {
		e.deleteMu.Unlock()
		return
	}
	e.deleting.Delete(taskID)
	if !keepPaused {
		e.paused.Delete(taskID)
	}
	e.deleteMu.Unlock()
	if !keepPaused && e.m != nil {
		if t, ok := e.m.Task(taskID); ok {
			t.Notify()
		}
	}
}

func (e *Engine) IsDeleting(taskID string) bool {
	_, ok := e.deleting.Load(taskID)
	return ok
}

// registerTaskRoutines reserves count goroutines in the task runtime. Callers
// hold deleteMu for reading so StopTask cannot race WaitGroup.Add with Wait.
func (e *Engine) registerTaskRoutines(parent context.Context, taskID string, count int) *taskRuntime {
	e.runtimeMu.Lock()
	defer e.runtimeMu.Unlock()
	rt := e.runtimes[taskID]
	if rt == nil {
		ctx, cancel := context.WithCancel(parent)
		rt = &taskRuntime{ctx: ctx, cancel: cancel}
		e.runtimes[taskID] = rt
	}
	rt.wg.Add(count)
	return rt
}

func runTaskRoutine(rt *taskRuntime, fn func(context.Context)) {
	go func() {
		defer rt.wg.Done()
		fn(rt.ctx)
	}()
}

// StopTask permanently stops every long-lived goroutine and removes all Engine
// state for a successfully deleted task. The delete barrier remains installed
// until cleanup finishes, so no new task operation can race the teardown.
func (e *Engine) StopTask(taskID string) {
	e.deleteMu.Lock()
	e.deleting.Store(taskID, true)
	e.deleteMu.Unlock()

	e.cancelExec(taskID, agent.AbortTaskDeleted)
	e.runtimeMu.Lock()
	rt := e.runtimes[taskID]
	if rt != nil {
		rt.cancel()
	}
	e.runtimeMu.Unlock()
	if rt != nil {
		rt.wg.Wait()
	}

	e.execMu.Lock()
	if cancel := e.execCancel[taskID]; cancel != nil {
		cancel(agent.AbortTaskDeleted)
	}
	delete(e.execCancel, taskID)
	delete(e.execCtx, taskID)
	e.execMu.Unlock()

	e.runtimeMu.Lock()
	if e.runtimes[taskID] == rt {
		delete(e.runtimes, taskID)
	}
	e.runtimeMu.Unlock()

	e.started.Delete(taskID)
	e.lastAct.Delete(taskID)
	e.llmCalls.Delete(taskID)
	e.paused.Delete(taskID)
	e.dropCnt.Delete(taskID)
	e.plannerRound.Delete(taskID)
	e.settling.Delete(taskID)
	e.deadline.Delete(taskID)
	e.stamped.Delete(taskID)
	e.inflight.Delete(taskID)
	e.coordStarted.Delete(taskID)
	e.deleteMu.Lock()
	e.deleting.Delete(taskID)
	e.deleteMu.Unlock()
}

// cancelExec cancels a task's current per-task exec context (any in-flight
// planner.Plan / worker.Execute), if present. Shared by Pause and the settle
// sequence's hard-drain backstop.
func (e *Engine) cancelExec(taskID string, cause error) {
	e.execMu.Lock()
	if cancel := e.execCancel[taskID]; cancel != nil {
		cancel(cause)
	}
	e.execMu.Unlock()
}

// Resume un-pauses a task and nudges a fresh planning round. The next exec under
// it gets a fresh (uncancelled) context.
func (e *Engine) Resume(t *Task) {
	// BeginDelete owns the pause barrier once deletion starts. A concurrent
	// resume must never clear it and let a planner/worker re-enter while cleanup
	// is waiting for task operations to drain.
	if t == nil {
		return
	}
	e.deleteMu.RLock()
	defer e.deleteMu.RUnlock()
	if e.IsDeleting(t.ID) {
		return
	}
	e.paused.Delete(t.ID)
	t.Notify()
}

// execContextFor returns a live per-task context derived from parent, recreating
// it if a prior pause cancelled it.
func (e *Engine) execContextFor(parent context.Context, taskID string) context.Context {
	e.execMu.Lock()
	defer e.execMu.Unlock()
	if e.IsPaused(taskID) {
		// never hand out a live context while paused (guards the claim→Execute race)
		c, cancel := context.WithCancelCause(parent)
		cancel(agent.AbortPausedRaceGuard)
		return c
	}
	if c := e.execCtx[taskID]; c != nil && c.Err() == nil {
		return c
	}
	c, cancel := context.WithCancelCause(parent)
	e.execCtx[taskID] = c
	e.execCancel[taskID] = cancel
	return c
}

// IsPaused reports whether a task is user-paused.
func (e *Engine) IsPaused(taskID string) bool {
	v, ok := e.paused.Load(taskID)
	return ok && v.(bool)
}

// Started reports whether the engine loops are running for a task.
func (e *Engine) Started(taskID string) bool {
	_, ok := e.started.Load(taskID)
	return ok
}

// LastActivity returns the unix time of the last planner/worker activity for a
// task (0 if none yet).
func (e *Engine) LastActivity(taskID string) int64 {
	if v, ok := e.lastAct.Load(taskID); ok {
		return v.(int64)
	}
	return 0
}

// BeginLLMCall/EndLLMCall track actual provider calls separately from the
// scheduler's task-operation counter. A task can have live loops while all of
// them are waiting for a trigger; that state must remain idle in the UI.
func (e *Engine) BeginLLMCall(taskID string) {
	v, _ := e.llmCalls.LoadOrStore(taskID, new(int64))
	atomic.AddInt64(v.(*int64), 1)
}

func (e *Engine) EndLLMCall(taskID string) {
	if v, ok := e.llmCalls.Load(taskID); ok {
		p := v.(*int64)
		if atomic.AddInt64(p, -1) <= 0 {
			atomic.StoreInt64(p, 0)
		}
	}
}

func (e *Engine) ActiveLLMCalls(taskID string) int64 {
	if v, ok := e.llmCalls.Load(taskID); ok {
		return atomic.LoadInt64(v.(*int64))
	}
	return 0
}

func (e *Engine) touch(taskID string) { e.lastAct.Store(taskID, time.Now().Unix()) }

func NewEngine(m *Manager) *Engine {
	e := &Engine{m: m, debounce: 800 * time.Millisecond, bc: NewBroadcaster(),
		execCancel: map[string]context.CancelCauseFunc{}, execCtx: map[string]context.Context{},
		work: map[int64]*workExecution{}, steerBox: map[int64][]string{},
		runtimes: map[string]*taskRuntime{}, killSettleGrace: KillSettleGrace}
	// 群组闸门的默认实现装在 manager 侧（manager.go）：判据（群成员能不能用、组内
	// 上限、额度状态）都是 server 侧的数据层口径，engine 不查群组表。装配区
	// （server.go 的 New）属别的单元的文件域，所以这里先按 manager 侧回调装上，
	// server 侧随时可以用 SetGroupXxx 覆盖。
	e.GroupConcurrencyLimit = m.GroupConcurrencyLimit
	e.GroupRunningIntents = m.GroupRunningIntents
	e.GroupDispatcherClaimable = m.groupDispatcherYields
	// 策略上下文注入的宿主侧回调：装一次，之后每个 planner 都拿同一个 provider
	// （组内同版本快照 ⇒ 同一份摘要）。
	e.strategyProvider = e.strategyContextProvider()
	return e
}

// registerWork records the cancel for the work currently running intentID.
func (e *Engine) registerWork(intentID int64, cancel context.CancelCauseFunc) {
	e.workMu.Lock()
	e.work[intentID] = &workExecution{cancel: cancel, done: make(chan error, 1), finished: make(chan struct{})}
	e.workMu.Unlock()
}

// registerWorkForTask is registerWork plus the task binding: the kill path needs
// the task to know there is a worker worth settling, so the binding is installed
// together with the handle — a kill landing in between would find no task and
// skip the settle window.
func (e *Engine) registerWorkForTask(intentID int64, t *Task, cancel context.CancelCauseFunc) {
	e.workMu.Lock()
	e.work[intentID] = &workExecution{cancel: cancel, done: make(chan error, 1), finished: make(chan struct{}), task: t}
	e.workMu.Unlock()
}

// markWorkSettled flags the live work as finished: the worker returned, so a
// pending kill grace must not cancel it any more (and the grace wait can end
// early instead of burning the whole window).
func (e *Engine) markWorkSettled(intentID int64) {
	e.workMu.Lock()
	defer e.workMu.Unlock()
	run := e.work[intentID]
	if run == nil {
		return
	}
	select {
	case <-run.finished:
	default:
		close(run.finished)
	}
}

// detachWork removes the live control handle once Execute has returned. complete
// must be called after the final intent state write so a waiting cancel handler can
// safely delete the worker's blackboard output without racing a late write.
func (e *Engine) detachWork(intentID int64) (action string, complete func(error)) {
	e.workMu.Lock()
	run := e.work[intentID]
	if run != nil {
		delete(e.work, intentID)
		action = run.action
		run.cancel(agent.AbortWorkFinished) // release resources (no-op if already cancelled)
	}
	e.workMu.Unlock()
	e.steerMu.Lock()
	delete(e.steerBox, intentID) // drop any undelivered steering for a finished work
	e.steerMu.Unlock()
	if run == nil {
		return action, func(error) {}
	}
	return action, func(err error) { run.done <- err }
}

// ControlWork requests a user-visible pause or cancellation and waits until the
// worker has fully stopped writing. Cancellation cleanup is performed by the API
// handler after this returns; pause state is committed by runWorkerStep itself.
func (e *Engine) ControlWork(ctx context.Context, intentID int64, action string) error {
	if action != "pause" && action != "cancel" {
		return fmt.Errorf("unsupported work action %q", action)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	e.workMu.Lock()
	run := e.work[intentID]
	if run == nil {
		e.workMu.Unlock()
		return fmt.Errorf("%w: 意图 %d 当前没有运行中的 work（可能已结束或未被领取）", errWorkControlConflict, intentID)
	}
	if run.action != "" {
		e.workMu.Unlock()
		return fmt.Errorf("%w: 意图 %d 正在执行 %s 操作", errWorkControlConflict, intentID, run.action)
	}
	run.action = action
	done := run.done
	cause := error(agent.AbortWorkPausedByUser)
	if action == "cancel" {
		cause = agent.AbortWorkCancelledByUser
	}
	run.cancel(cause)
	e.workMu.Unlock()

	timer := time.NewTimer(workControlWaitTimeout)
	defer timer.Stop()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		e.releaseWorkControl(intentID, run, action)
		return fmt.Errorf("等待意图 %d %s 收尾: %w", intentID, action, ctx.Err())
	case <-timer.C:
		e.releaseWorkControl(intentID, run, action)
		return fmt.Errorf("等待意图 %d %s 收尾: %w", intentID, action, context.DeadlineExceeded)
	}
}

// releaseWorkControl drops only this caller's reservation after its wait is
// cancelled. The work context stays cancelled; runWorkerStep recognizes the
// named cancellation cause and settles the intent into the recoverable paused
// state even if the HTTP caller has gone away.
func (e *Engine) releaseWorkControl(intentID int64, run *workExecution, action string) {
	e.workMu.Lock()
	if current := e.work[intentID]; current == run && current.action == action {
		current.action = ""
	}
	e.workMu.Unlock()
}

func transitionIntentState(store *db.ExplorationStore, intentID int64, expected, state string) error {
	changed, err := store.CompareAndSetIntentState(intentID, expected, state)
	if err != nil {
		return err
	}
	if !changed {
		return fmt.Errorf("%w: 意图 %d 不再是 %s 状态", db.ErrIntentStateConflict, intentID, expected)
	}
	return nil
}

// SteerWork queues a mid-run course-correction for the work running intentID (the
// planner's steer_work tool). The worker delivers it before its next tool call and
// re-plans — no kill. Errors if no work is currently running that intent.
//
// 消息同时留一份【持久化副本】：内存队列在进程崩溃或 work 收场（detachWork 会清掉
// 它）时会丢消息，落库的那份由下一次执行捡回。
func (e *Engine) SteerWork(intentID int64, msg string) error {
	if strings.TrimSpace(msg) == "" {
		return fmt.Errorf("纠偏消息不能为空")
	}
	// 消息直入 worker 上下文且不过任何闸，无上限既是资源风险也是注入面。
	if n := len([]rune(msg)); n > steerMsgMaxRunes {
		return fmt.Errorf("纠偏消息过长（%d 字符，上限 %d）", n, steerMsgMaxRunes)
	}
	e.workMu.Lock()
	run := e.work[intentID]
	e.workMu.Unlock()
	if run == nil {
		return fmt.Errorf("意图 %d 当前没有运行中的 work（可能已结束或未被领取）", intentID)
	}
	// 队列满了就别落库：留一条永远等不到交付的副本，只会在下次执行时变成一条
	// 迟到的纠偏。
	if err := e.checkSteerRoom(intentID); err != nil {
		return err
	}
	// 先落库再入队：进程在两步之间崩掉时消息还在库里等下次执行捡回；反过来
	// （先入队后落库）那一步崩掉就直接丢了。
	if err := e.persistSteer(run.task, intentID, msg); err != nil {
		// 落库失败只记日志：本次投递走内存通道，不受影响。
		log.Printf("[steer] intent #%d 纠偏消息落库失败（本次仍会投递）: %v", intentID, err)
	}
	if err := e.enqueueSteer(intentID, msg); err != nil {
		return err
	}
	return nil
}

// checkSteerRoom reports whether the in-memory queue of intentID still has room.
func (e *Engine) checkSteerRoom(intentID int64) error {
	e.steerMu.Lock()
	defer e.steerMu.Unlock()
	if len(e.steerBox[intentID]) >= steerQueueMax {
		return steerQueueFullErr(intentID)
	}
	return nil
}

// steerQueueFullErr 统一的「纠偏队列已满」错误文案。
func steerQueueFullErr(intentID int64) error {
	return fmt.Errorf("意图 %d 的纠偏队列已满（上限 %d 条），worker 消费不过来", intentID, steerQueueMax)
}

// enqueueSteer appends a steering message to the in-memory queue of intentID.
func (e *Engine) enqueueSteer(intentID int64, msg string) error {
	e.steerMu.Lock()
	defer e.steerMu.Unlock()
	if len(e.steerBox[intentID]) >= steerQueueMax {
		return steerQueueFullErr(intentID)
	}
	e.steerBox[intentID] = append(e.steerBox[intentID], msg)
	return nil
}

// drainSteer pops the oldest queued steering message for intentID (FIFO), if any.
func (e *Engine) drainSteer(intentID int64) (string, bool) {
	e.steerMu.Lock()
	defer e.steerMu.Unlock()
	q := e.steerBox[intentID]
	if len(q) == 0 {
		return "", false
	}
	msg := q[0]
	if len(q) == 1 {
		delete(e.steerBox, intentID)
	} else {
		e.steerBox[intentID] = q[1:]
	}
	return msg, true
}

// persistSteer stores a steering message durably so it survives a crash and
// is picked up by the next round of the targeted work.
//
// 复用图上的 hint 节点（exploration_nodes kind='hint'）：不新建表、不新建队列。
// payload 带 kind='steer' 与 intent_id，用来把「纠偏」与「人挂的战略提示」分开；
// state='active' = 还没交付，交付后置 'consumed'。
func (e *Engine) persistSteer(t *Task, intentID int64, msg string) error {
	if t == nil || t.Store == nil {
		return fmt.Errorf("没有任务上下文，纠偏消息无法落库")
	}
	if n := len([]rune(msg)); n > steerMsgMaxRunes {
		return fmt.Errorf("纠偏消息过长（%d 字符，上限 %d）", n, steerMsgMaxRunes)
	}
	pending, err := e.LoadPendingSteers(t, intentID)
	if err != nil {
		return err
	}
	// 待投递条数按同一个上限封顶：崩溃后重跑最多补投这么多条纠偏。
	if len(pending) >= steerQueueMax {
		return fmt.Errorf("意图 %d 的待投递纠偏已满（上限 %d 条）", intentID, steerQueueMax)
	}
	payload := map[string]any{
		"summary":   steerPayloadKind,
		"text":      msg,
		"intent_id": intentID,
		"kind":      steerPayloadKind,
	}
	_, err = t.Store.AddNode(db.KindHint, payload, 0, "active", steerPayloadKind, nil)
	return err
}

// LoadPendingSteers reads the durable steering messages for a work, oldest
// first (the worker consumes them in the order they were written).
func (e *Engine) LoadPendingSteers(t *Task, intentID int64) ([]string, error) {
	if t == nil || t.Store == nil {
		return nil, fmt.Errorf("没有任务上下文，纠偏消息无法读取")
	}
	nodes, err := t.Store.ListByKind(db.KindHint, steerScanLimit)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, n := range nodes {
		text, ok := steerPayloadOf(n, intentID)
		if !ok {
			continue
		}
		out = append(out, text)
	}
	// ListByKind 是「新→旧」，投递要「先来先到」⇒ 反转成旧→新。
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// steerPayloadOf extracts the steering text of a hint node, if it is an
// undelivered steering message for intentID.
func steerPayloadOf(n *db.Node, intentID int64) (string, bool) {
	if n == nil || n.State != "active" || len(n.Payload) == 0 {
		return "", false
	}
	var p map[string]any
	if err := json.Unmarshal(n.Payload, &p); err != nil {
		return "", false
	}
	if k, _ := p["kind"].(string); k != steerPayloadKind {
		return "", false
	}
	if id, _ := p["intent_id"].(float64); int64(id) != intentID {
		return "", false
	}
	text, _ := p["text"].(string)
	if text == "" {
		return "", false
	}
	return text, true
}

// consumeSteer marks the durable copy of a delivered steering message consumed,
// so the next round does not deliver it a second time.
func (e *Engine) consumeSteer(t *Task, intentID int64, msg string) error {
	if t == nil || t.Store == nil {
		return nil
	}
	nodes, err := t.Store.ListByKind(db.KindHint, steerScanLimit)
	if err != nil {
		return err
	}
	var target *db.Node
	for _, n := range nodes { // 新→旧 ⇒ 最后命中的是最旧的一条
		text, ok := steerPayloadOf(n, intentID)
		if !ok || text != msg {
			continue
		}
		target = n
	}
	if target == nil {
		return nil
	}
	return t.Store.SetNodeState(target.ID, "consumed")
}

// replayPendingSteers re-queues the steering messages that outlived their work
// (crash or detach), so the next run of that intent picks them up.
func (e *Engine) replayPendingSteers(t *Task, intentID int64) {
	pending, err := e.LoadPendingSteers(t, intentID)
	if err != nil {
		log.Printf("[steer] task %s 读取待投递纠偏失败: %v", t.ID, err)
		return
	}
	for _, msg := range pending {
		if err := e.enqueueSteer(intentID, msg); err != nil {
			log.Printf("[steer] task %s 意图 #%d 补投纠偏失败: %v", t.ID, intentID, err)
			return
		}
	}
}

// steerHooks wraps the guard's hook runner so the planner can steer a running work:
// before each tool call it drains a queued course-correction (if any) and blocks the
// call, handing the message back to the model — which re-plans its next step instead
// of running the tool. No queued message → the guard behaves exactly as before.
// 它同时负责「空转回合」的续跑，见 Stop。
type steerHooks struct {
	inner harness.HookRunner
	drain func() (string, bool)
	// nudges 是本条意图已注入的空转续跑次数，上限 limit。指针:harness 持有的是
	// steerHooks 的值拷贝，计数必须共享同一份。
	nudges *atomic.Int64
	// limit 是空转续跑的次数上限，由 Engine.emptyTurnNudgeLimit() 从「空响应重试
	// 次数」解析而来。<=0 = 不介入(用户显式关掉了这层)。
	limit int
	// label 形如 "worker-1 · #42"，只用于日志。
	label string
}

// 空转回合(只有思考、既无正文也无工具调用)续跑次数的默认值，与 SDK 空响应重试的
// 内置默认(norma/llm/openai.go 的 emptyResponseRetries)保持一致——两层共用同一个
// 旋钮，不配置时的行为也该对齐。解析见 Engine.emptyTurnNudgeLimit。
//
// 注意这个数是「一条意图的总量」，不是「连续几次」:harness 自身的 stopHookActive
// 已经限死了连续空转只推一次——推完那一轮若还是空转，Stop 钩子不会再被调到，run 直接
// 收场;只有真正发生过一次工具回合，配额才刷新(norma/harness/query.go:534)。所以这个
// 闸挡的是「工具 → 空转 → 推 → 工具 → 空转」这种病态循环，别让它把意图预算耗光。
const defaultEmptyTurnNudges = 2

// emptyTurnNudge 是空转回合注入的续跑指令。
//
// 这种回合在 harness 眼里是一次自然结束(stop_reason=end_turn 且无 tool_use)，五层
// LLM 重试一层都不适用——它不是错误，是模型「想完了但没动手」。SDK 的空响应重试也
// 够不着:它以「有没有 yield 过事件」判空，而思考增量本身就是事件(norma/llm/openai.go
// 的 SEThinkingDelta)，所以 thinking-only 不算空。何况那层是原样重发整个 prompt，
// 对这种由上下文形状决定的空转，重发只会让模型再想一遍。这里换成追加一条指令，让它
// 带着已经产出的思考继续，输入变了才有理由给出不同的行为。
const emptyTurnNudge = "【空转提醒】你上一轮只输出了思考过程，既没有给出正文回复，也没有调用任何工具，" +
	"这一轮等于没有产出。请直接执行你刚才想好的下一步：要么调用工具，要么给出结论文字。不要重复思考。"

// isThinkingOnlyTurn reports whether the latest assistant turn produced neither
// text nor a tool call — i.e. the model spent the whole round thinking.
func isThinkingOnlyTurn(messages []llm.Message) bool {
	for i := len(messages) - 1; i >= 0; i-- {
		m := messages[i]
		if m.Role != llm.RoleAssistant {
			continue
		}
		return strings.TrimSpace(m.Text()) == "" && len(m.ToolUses()) == 0
	}
	return false
}

func (h steerHooks) PreToolUse(ctx context.Context, name string, input []byte) (bool, string, []byte) {
	if msg, ok := h.drain(); ok {
		return true, steerDeliveryText(msg), nil
	}
	if h.inner != nil {
		return h.inner.PreToolUse(ctx, name, input)
	}
	return false, "", nil
}

// steerDeliveryText 包装一条排队的即时指令。措辞是通用的：规划者的纠偏与终止前的
// 收尾通知走同一条通道，正文自带出处。
func steerDeliveryText(msg string) string {
	return "【实时指令】" + msg +
		"\n（这是对本意图的即时指令；本次工具调用未执行，请据此调整下一步。若与你当前打算冲突，以此为准。）"
}

func (h steerHooks) PostToolUse(ctx context.Context, name string, input, result []byte, isErr bool) {
	if h.inner != nil {
		h.inner.PostToolUse(ctx, name, input, result, isErr)
	}
}

// Stop 在 guard 原有语义之上补一层「空转回合」续跑:模型只输出了思考、既没给正文
// 也没调工具时，harness 会把它当成自然结束并以空 summary 收场(query.go 的
// ReasonCompleted + asst.Text())，一条本来还没做完的意图就这样断在半路。此时注入
// 一条续跑指令，让模型带着已有思考接着走。
func (h steerHooks) Stop(ctx context.Context, messages []llm.Message) (bool, []string, string) {
	var (
		prevent  bool
		blocking []string
		msg      string
	)
	if h.inner != nil {
		prevent, blocking, msg = h.inner.Stop(ctx, messages)
	}
	// inner 已经决定硬停、或已经要注入自己的续跑消息 → 尊重它，不再叠加。
	// limit<=0 = 用户把「空响应重试次数」配成了 -1，即显式关掉这层。
	if prevent || len(blocking) > 0 || h.nudges == nil || h.limit <= 0 || !isThinkingOnlyTurn(messages) {
		return prevent, blocking, msg
	}
	n := h.nudges.Add(1)
	if n > int64(h.limit) {
		log.Printf("[work %s] 空转回合(仅思考、无正文无工具)已达续跑上限 %d，放行收场", h.label, h.limit)
		return prevent, blocking, msg
	}
	log.Printf("[work %s] 空转回合(仅思考、无正文无工具)，注入续跑指令 (%d/%d)", h.label, n, h.limit)
	return false, []string{emptyTurnNudge}, ""
}

// KillWork terminates the in-flight work running intentID (planner's kill_work tool).
//
// 收尾优先（不丢已得结论）：硬取消会丢掉 worker 上下文里已识别、但还没落库的东西
// —— SDK 的收尾提示只在【预算耗尽】时注入，父上下文被取消是硬中断，没有写回窗口
// （见 agent/wrapup.go 的收尾语义）。所以这里先给 worker 投递一条收尾通知，让它在
// 下一个工具调用边界把结论写回，宽限期到了再真正取消。
//
// 宽限期是【有界】的：到期必取消，一次 kill 最多让这条 work 多活 e.killSettleGrace
// （生产 = KillSettleGrace），不会永远挂着。worker 若在宽限期内自己收场（结论已落库、
// 意图已结算），取消动作会被跳过 —— 那是更好的结果。
func (e *Engine) KillWork(intentID int64) error {
	e.workMu.Lock()
	run := e.work[intentID]
	e.workMu.Unlock()
	if run == nil {
		return fmt.Errorf("意图 %d 当前没有运行中的 work（可能已结束或未被领取）", intentID)
	}
	// 背后没有 worker 的句柄（没有任务上下文）无从收尾：直接取消。
	if run.task == nil {
		run.cancel(agent.AbortKilledByPlanner)
		return nil
	}
	// 先投递收尾通知（走与纠偏同一条通道：worker 在下一个工具调用边界取走它）。
	// 这条通知只对本次执行有意义，所以不落库——落库会在下一次重跑时变成一条过期的
	// 「即将终止」。
	if err := e.enqueueSteer(intentID, killSettlePrompt); err != nil {
		log.Printf("[kill] intent #%d 无法投递收尾通知: %v", intentID, err)
	}
	go e.cancelAfterSettleGrace(intentID, run)
	return nil
}

// cancelAfterSettleGrace cancels a killed work once the settle grace expires,
// unless the work finished on its own first.
func (e *Engine) cancelAfterSettleGrace(intentID int64, run *workExecution) {
	timer := time.NewTimer(e.killSettleGrace)
	defer timer.Stop()
	select {
	case <-run.finished:
		return // 宽限期内自己收场了：结论已落库，不必再取消
	case <-timer.C:
	}
	e.workMu.Lock()
	defer e.workMu.Unlock()
	select {
	case <-run.finished:
		return // 竞态：worker 恰好在到点瞬间返回
	default:
	}
	if e.work[intentID] != run {
		return // 已经 detach：没有可取消的句柄了
	}
	run.cancel(agent.AbortKilledByPlanner)
}

// reopenStoppedIntent puts a stopped intent back on the frontier so a worker
// can claim it again.
//
// 边界与「额度阻塞重开」完全一致（server/manager.go 的 ReplaceTaskLLMProfiles）：
// 任务已进终态（done/failed/timeout）时不重开——那里没有 worker 会跑，重开只会
// 把一条永远不会被执行的意图留在 open 状态（死状态）。
func (e *Engine) reopenStoppedIntent(t *Task, intentID int64) bool {
	if t == nil {
		return false
	}
	if e.isTaskTerminal(t) {
		return false
	}
	reopened, err := t.Store.ReopenIntent(intentID)
	if err != nil {
		log.Printf("[engine] task %s 意图 #%d 重开失败: %v", t.ID, intentID, err)
		return false
	}
	return reopened
}

// isTaskTerminal reports whether the task has reached a terminal status.
//
// 判据与「额度阻塞重开」那条路径同源（PG 的 tasks.status）：读不到任务行时按
// 【已终态】处理——那条路径读不到任务也是直接返回、不重开，这里保持同一边界
// （宁可少重开，不要在死任务上造一条没人执行的意图）。
func (e *Engine) isTaskTerminal(t *Task) bool {
	if t == nil || e.m == nil {
		return true
	}
	return e.m.taskIsTerminal(t.ID)
}

// ---------- 策略库（L4）的宿主侧接线 ----------

// strategyContextProvider 造出「给 planner 注入策略摘要」的宿主侧回调。
//
// 口径（计划 Task 6 Step 4）：
//   - 快照按 task_id 存 ⇒ 组内成员天然用同一份策略版本（design §3.3）；
//   - 快照**生成一次并复用**，不是每轮重算（每轮重算会让 planner 看到的策略集
//     漂移，且与「组内同版本」冲突）；
//   - 只渲染摘要（name + summary + utility），**绝不渲染 steps** —— steps 是命令
//     模板，进了上下文就等于「把可执行产物塞进上下文」（反模式 8）。
//     结构性保证：渲染入参 db.StrategySnapshotEntry **没有 steps 字段** ⇒ 注入
//     路径拿不到策略正文。
func (e *Engine) strategyContextProvider() func(taskID int64) string {
	return func(taskID int64) string {
		if e == nil || e.m == nil || e.m.pg == nil || taskID <= 0 {
			return ""
		}
		entries, err := e.m.pg.EnsureTaskStrategySnapshot(taskID)
		if err != nil {
			log.Printf("[strategy] 生成任务 %d 的策略快照失败: %v", taskID, err)
			return ""
		}
		return agent.RenderStrategySummaries(entries)
	}
}

// wireStrategyContext 给 planner 装上策略摘要注入回调（宿主侧，只装一次）。
func (e *Engine) wireStrategyContext(pl *agent.Planner) {
	if e == nil || pl == nil || e.strategyProvider == nil {
		return
	}
	if _, loaded := e.strategyWired.LoadOrStore(pl, struct{}{}); loaded {
		return // 已装过
	}
	pl.SetStrategyContextProvider(e.strategyProvider)
}

// attributeSubTaskOutcome 给这个任务跑一轮子任务级归因，并把结论路由成待裁决的提议。
//
// 归因器**只写 usage / evidence / proposal，绝不写 strategies.state** —— 晋级是
// 裁决写，只能在宿主侧发生（auto-permission-tiers §4.2）。这里只把归因器接到
// 生产路径上，不动它的任何判据。
//
// 归因失败**不打断**意图结算：归因是旁路（诊断 + 提议），失败只少一条提议，
// 不该让一次已经跑完的 worker 变成失败。
func (e *Engine) attributeSubTaskOutcome(taskID int64) {
	if e == nil || e.m == nil || e.m.pg == nil || taskID <= 0 {
		return
	}
	attrs, err := AttributeSubTasks(e.m.pg, taskID)
	if err != nil {
		log.Printf("[strategy] 任务 %d 的子任务归因失败: %v", taskID, err)
		return
	}
	ids, err := ProposeFromAttributions(e.m.pg, taskID, attrs)
	if err != nil {
		log.Printf("[strategy] 任务 %d 的归因提议路由失败: %v", taskID, err)
		return
	}
	if len(ids) > 0 {
		log.Printf("[strategy] 任务 %d 归因出 %d 条待裁决提议", taskID, len(ids))
	}
}

// emitAuditSummary 在任务落终态时产出操作审计摘要（v4.1 §3 收尾报告）。
// 回调未装配 / panic 都不影响终态主链路（best-effort）。
func (e *Engine) emitAuditSummary(t *Task) {
	if e == nil || e.AuditSummary == nil || t == nil {
		return
	}
	defer func() { _ = recover() }()
	e.AuditSummary(t)
}

// freezeReplayCorpus 冻结这个任务的重放语料（一次性、可重入）。
//
// 为什么在【任务】终态冻结而不是【意图】终态：语料是「一次运行的世界快照」，
// 只有整个任务跑完（或被收尾）时这个世界才是定格的；按意图冻会得到半截世界的
// 语料，重放闸门拿它比就没有意义（计划 Task 4 Step 6）。
//
// 冻结失败要能重试 ⇒ 记账只在成功后落。
func (e *Engine) freezeReplayCorpus(taskID string) {
	if e == nil || e.m == nil || e.m.pg == nil {
		return
	}
	numeric, err := strconv.ParseInt(taskID, 10, 64)
	if err != nil || numeric <= 0 {
		return
	}
	if _, loaded := e.replayFrozen.LoadOrStore(taskID, struct{}{}); loaded {
		return // 已冻过
	}
	if err := freezeTaskReplayCorpus(e.m.pg, numeric); err != nil {
		e.replayFrozen.Delete(taskID)
		log.Printf("[strategy] 任务 %s 冻结重放语料失败: %v", taskID, err)
		return
	}
	log.Printf("[strategy] 任务 %s 的重放语料已冻结", taskID)
}

// freezeTaskReplayCorpus 写一条重放语料案例：资产签名 + 命中的里程碑 + 漏洞结局。
//
// 冻结 = 不可变：FreezeReplayCorpus 走 ON CONFLICT DO NOTHING ⇒ 第二次冻结不
// 覆盖已有案例（计划 Task 4 Step 5）。
func freezeTaskReplayCorpus(pg *db.DB, taskID int64) error {
	sig, err := pg.Assets().TaskAssetSignature(taskID)
	if err != nil {
		return fmt.Errorf("算任务资产签名失败: %w", err)
	}
	milestones, err := TaskReplayMilestones(pg, taskID)
	if err != nil {
		return err
	}
	outcomes, err := TaskReplayOutcomes(pg, taskID)
	if err != nil {
		return err
	}
	return pg.FreezeReplayCorpus(taskID, sig, milestones, outcomes)
}

// Broadcaster exposes the engine's live activity pub/sub (used by the SSE handler).
func (e *Engine) Broadcaster() *Broadcaster { return e.bc }

// emitActivity persists one captured step AND fans it out to live subscribers,
// from a single point so storage and the SSE stream never diverge.
func (e *Engine) emitActivity(t *Task, r db.Activity) db.Activity {
	id, err := e.appendActivity(t, r)
	if err != nil {
		// NO LONGER SILENT: dropping a record breaks command↔result pairing in the
		// trace — a tool_use whose tool_result was lost shows as "执行中" forever, and
		// a lost 'result'/'round' record leaves the session with no summary ("无总结").
		// Everything needed to分析根因 goes into ONE error-level line: reason class,
		// summary preview, running drop count for this task, and — on the FK case — a
		// live probe of WHY the parent exploration is unreachable.
		n := e.bumpDrop(t.ID)
		diag := ""
		// On the FK-parent failure (23503) probe the live DB so the log records WHY the
		// exploration is unreachable (row gone / wrong expID) instead of just that it is.
		if isFKViolation(err) {
			storeID := t.Store.ID()
			if exists, refs, maxID, dErr := e.m.pg.ExplorationDiag(storeID); dErr != nil {
				diag = fmt.Sprintf(" | FK诊断查询失败(store.expID=%d task.ExpID=%d): %v", storeID, t.ExpID, dErr)
			} else {
				diag = fmt.Sprintf(" | FK诊断: store.expID=%d task.ExpID=%d exploration存在=%v 引用它的task数=%d MAX(exploration.id)=%d",
					storeID, t.ExpID, exists, refs, maxID)
			}
		}
		log.Printf("[activity] task %s 丢弃活动记录(该任务累计第 %d 条) worker=%s kind=%s tool=%s tuid=%s reason=%s summary=%q: %v%s",
			t.ID, n, r.Worker, r.Kind, r.Tool, r.ToolUseID, dropReason(err), preview(r.Summary, 80), err, diag)
		e.touch(t.ID)
		return r
	}
	r.ID = id
	if r.CreatedAt.IsZero() {
		r.CreatedAt = time.Now()
	}
	e.bc.Publish(t.ID, r)
	// B6 升级路径观察点：工具调用被拒（拦截规则/SoD 链的「Blocked by hook」）
	// 都从这里经过——同一 (task, tool, reason_code) 连续 ≥3 次 ⇒ 意图转 blocked
	// + 会话播报，止损撞墙循环。异步、吞 panic（best-effort 观察者）。
	if r.Kind == "tool_result" && r.IsError && strings.Contains(r.Summary, "Blocked by hook") && e.DenialWatcher != nil {
		watcher := e.DenialWatcher
		go func() {
			defer func() {
				if rec := recover(); rec != nil {
					log.Printf("[escalation] panic（task %s）: %v", t.ID, rec)
				}
			}()
			watcher(t.ID, r.Tool, r.Summary)
		}()
	}
	if e.ActivityMirror != nil {
		// 总控/群聊播报：镜像规则在回调内部（session_task_mirror.go），这里只转发。
		// best-effort：回调自身吞错，panic 也不能拖垮引擎主链路。
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					log.Printf("[activity-mirror] panic（task %s）: %v", t.ID, rec)
				}
			}()
			e.ActivityMirror(t.ID, r)
		}()
	}
	e.touch(t.ID)
	return r
}

// appendActivity persists one activity row, retrying briefly on write failure.
// Concurrent planner + worker inserts into the same exploration's activity log
// occasionally fail; a couple of quick retries recover most. Crucially, every
// failure is now LOGGED (it used to be swallowed by an `if err == nil`), so the
// underlying DB error is finally visible for diagnosis.
func (e *Engine) appendActivity(t *Task, r db.Activity) (int64, error) {
	var id int64
	var err error
	for attempt := 1; attempt <= 3; attempt++ {
		if id, err = t.Store.AppendActivity(r); err == nil {
			if attempt > 1 {
				log.Printf("[activity] task %s 写入第 %d 次重试成功 (worker=%s kind=%s tool=%s)",
					t.ID, attempt, r.Worker, r.Kind, r.Tool)
			}
			return id, nil
		}
		log.Printf("[activity] task %s 写入失败 (第 %d/3 次, worker=%s kind=%s tool=%s expID=%d): %v",
			t.ID, attempt, r.Worker, r.Kind, r.Tool, t.Store.ID(), err)
		time.Sleep(time.Duration(attempt) * 25 * time.Millisecond)
	}
	return 0, err
}

// SetReadiness wires the global "an LLM provider is configured" predicate (read by
// Ready() / the llm_configured indicator). Called once at startup.
func (e *Engine) SetReadiness(fn func() bool) { e.readiness = fn }

// SetAgentResolver installs a per-task planner/worker resolver (wired by the server).
// Called once at startup before any task loop runs, so no lock is needed on reads.
func (e *Engine) SetAgentResolver(fn func(t *Task) (*agent.Planner, *agent.Worker)) {
	e.resolve = fn
	e.resolveAuthoritative = false
}

// SetAuthoritativeAgentResolver installs a resolver whose nil result must not
// fall through to the global provider. Task-level failover chains use this so a
// fully exhausted chain cannot silently bypass its configured boundary.
func (e *Engine) SetAuthoritativeAgentResolver(fn func(t *Task) (*agent.Planner, *agent.Worker)) {
	e.resolve = fn
	e.resolveAuthoritative = true
}

// SetGroupConcurrencyLimit installs the per-group concurrency cap lookup.
func (e *Engine) SetGroupConcurrencyLimit(fn func(taskID int64) (int, bool)) {
	e.GroupConcurrencyLimit = fn
}

// SetGroupRunningIntents installs the running-intent counter used as the
// per-group concurrency denominator.
func (e *Engine) SetGroupRunningIntents(fn func(taskID int64) (int, error)) {
	e.GroupRunningIntents = fn
}

// SetGroupDispatcherClaimable installs the dispatcher-fallback lookup: it
// answers whether the current worker should yield to the dispatcher (auto).
func (e *Engine) SetGroupDispatcherClaimable(fn func(taskID int64, worker string) bool) {
	e.GroupDispatcherClaimable = fn
}

// snapshotFor returns the planner/worker a task should run on, from the task-router
// resolver. nil,nil means the task is deliberately unavailable (e.g. an exhausted
// failover chain); there is no global-pair fallback.
func (e *Engine) snapshotFor(t *Task) (*agent.Planner, *agent.Worker) {
	if e.resolve != nil {
		p, w := e.resolve(t)
		if (p != nil && w != nil) || e.resolveAuthoritative {
			return p, w
		}
	}
	return nil, nil
}

// Ready reports whether a global LLM provider is configured (via the readiness
// predicate wired at startup).
func (e *Engine) Ready() bool {
	return e.readiness != nil && e.readiness()
}

// ReadyFor reports whether a specific task can resolve a planner/worker pair.
// An explicit task profile chain can be runnable even when no global default
// provider is configured, so task status must not rely on Ready alone.
func (e *Engine) ReadyFor(t *Task) bool {
	p, w := e.snapshotFor(t)
	return p != nil && w != nil
}

// stagnationThreshold 是 C4 停滞闸的阈值：连续这么多个「收尾写回 0 事实 0 资产
// 0 漏洞」的意图 ⇒ 进入收尾评估（wrapup）。代码常量（安全/收敛语义不进配置面）。
const stagnationThreshold = 6

// wrapupConcludeRounds 是收尾评估轮的上限：连续这么多轮规划仍 0 新意图且未
// prove_goal ⇒ 确定性落 done(concluded)——边际收益递减，继续规划只是烧钱。
const wrapupConcludeRounds = 2

// taskStagnation 是一个任务的停滞状态。
type taskStagnation struct {
	zeroWrites   int  // 连续零产出收尾的意图数
	wrapup       bool // 收尾评估模式（planner 本轮允许 conclude/0 意图）
	wrapupRounds int  // 进入收尾评估后跑了几轮规划（仍 0 产出则递增）
}

func (e *Engine) stagnationOf(taskID string) *taskStagnation {
	e.stagnationMu.Lock()
	defer e.stagnationMu.Unlock()
	if e.stagnation == nil {
		e.stagnation = map[string]*taskStagnation{}
	}
	if st, ok := e.stagnation[taskID]; ok {
		return st
	}
	st := &taskStagnation{}
	e.stagnation[taskID] = st
	return st
}

// intentExtendBudget 是软着陆的延期上限（v6 用户拍板：3 次，每次满预算重开）。
const intentExtendBudget = 3

// tryExtendActiveIntent 判定「预算到线但正要出成果」的意图是否延期续跑：
// 本轮有有效写回（事实/资产/finding 至少一项）且延期未超限 ⇒ 意图转回 open、
// 播报延期、返回 true（调用方跳过 exhausted 落库）。
func (e *Engine) tryExtendActiveIntent(t *Task, intentID int64, wrote agent.WriteCounts, workerName string, reason harness.TerminalReason) bool {
	if wrote.Facts+wrote.Assets+wrote.Findings <= 0 {
		return false // 本轮零产出：照旧 exhausted
	}
	e.stagnationMu.Lock()
	if e.intentExtends == nil {
		e.intentExtends = map[string]int{}
	}
	key := t.ID + ":" + fmt.Sprintf("%d", intentID)
	if e.intentExtends[key] >= intentExtendBudget {
		e.stagnationMu.Unlock()
		return false
	}
	e.intentExtends[key]++
	n := e.intentExtends[key]
	e.stagnationMu.Unlock()
	if err := transitionIntentState(t.Store, intentID, "running", "open"); err != nil {
		log.Printf("[extend] task %s intent %d 转回 open 失败（照旧 exhausted）: %v", t.ID, intentID, err)
		return false
	}
	why := "步数上限"
	if reason == harness.ReasonTimeout {
		why = "运行超时"
	}
	summary := fmt.Sprintf("【意图延期】意图 #%d 在 %s 到线前仍有持续产出（本轮写回 %s），已重开续跑（第 %d/%d 次延期）——有收获就让它跑完", intentID, why, wrote.String(), n, intentExtendBudget)
	log.Printf("[extend] task %s %s", t.ID, summary)
	e.emitActivity(t, db.Activity{Worker: "system", Kind: "text", Summary: summary})
	return true
}

// recentConclusionWindow 是三门收敛的「产出期」窗口：任务在该窗口内有过
// 结论物写回 ⇒ 零产出到线不立即收尾。
const recentConclusionWindow = 20 * time.Minute

// recentConclusion 报告任务最近是否有结论物入账（锁外查询——调用方持有
// stagnationMu，这里只读内存时间戳）。
func (e *Engine) recentConclusion(t *Task) bool {
	e.stagnationMu.Lock()
	last, ok := e.lastConclusionAt[t.ID]
	e.stagnationMu.Unlock()
	return ok && time.Since(last) < recentConclusionWindow
}

// noteIntentWriteBack 是 C4 的记账点：意图终态收尾时按写回量更新停滞状态。
// 有效写回（>0）清零并退出收尾评估；零产出递增，达阈值进收尾评估并注入
// 【收尾评估】触发（planner 本轮允许 conclude/0 意图——C5 的例外）。
func (e *Engine) noteIntentWriteBack(t *Task, wrote agent.WriteCounts) {
	st := e.stagnationOf(t.ID)
	e.stagnationMu.Lock()
	if e.lastConclusionAt == nil {
		e.lastConclusionAt = map[string]time.Time{}
	}
	if wrote.Total() > 0 {
		*st = taskStagnation{} // 任何有效写回清零（真有进展的任务永不被误杀）
		if wrote.Findings+wrote.Assets > 0 {
			// D2 三门收敛：结论物（finding/资产）入账时刻——收尾判定的
			// 「产出期」依据（事实量大不等于接近结论，结论物才算）。
			e.lastConclusionAt[t.ID] = time.Now()
		}
		e.stagnationMu.Unlock()
		return
	}
	st.zeroWrites++
	// D2 三门收敛（d2d 借鉴）：零产出计数到线 ≠ 立即收尾——任务近期有过
	// 结论物入账（finding/资产/事实）说明整体仍在产出期，「有 churn 不关」。
	// 推迟本轮判定，不重置计数（连续性还在）。
	if st.zeroWrites >= stagnationThreshold && !st.wrapup && e.recentConclusion(t) {
		log.Printf("[stagnation] task %s 零产出到线但近期有结论物入账——收尾判定推迟（三门收敛）", t.ID)
		e.stagnationMu.Unlock()
		return
	}
	if st.zeroWrites < stagnationThreshold || st.wrapup {
		e.stagnationMu.Unlock()
		return
	}
	st.wrapup = true
	st.wrapupRounds = 0
	e.stagnationMu.Unlock()
	summary := fmt.Sprintf("【收尾评估】已连续 %d 个意图零产出（写回 0 事实 0 资产 0 漏洞）。进入收尾评估轮：请判定目标是否已实质达成——要么 prove_goal（有证据），要么停止派新方向并给出如实的 partial 结论；本轮允许 0 意图产出。", st.zeroWrites)
	e.emitActivity(t, db.Activity{Worker: "system", Kind: "text", Summary: summary})
	// 注入 wrapup 触发：下一轮 planner 读到收尾评估指令（C5 的例外口）。
	t.pendingTriggers = append(t.pendingTriggers, agent.TriggerEvent{Kind: "wrapup",
		Summary: "连续零产出，进入收尾评估"})
	log.Printf("[stagnation] task %s 连续 %d 个零产出意图，进入收尾评估", t.ID, st.zeroWrites)
}

// noteWrapupRoundResult 是收尾评估轮的结算：0 新意图 ⇒ 轮数递增；达到上限 ⇒
// 确定性收尾（done + concluded marker + 冻结 + 蒸馏）。有新意图 ⇒ 轮数清零
// （规划仍在产出，给它继续的机会）。
func (e *Engine) noteWrapupRoundResult(t *Task, intentsBefore int) {
	st := e.stagnationOf(t.ID)
	e.stagnationMu.Lock()
	if !st.wrapup {
		e.stagnationMu.Unlock()
		return
	}
	after, err := t.Store.CountIntents()
	if err != nil || after > intentsBefore {
		st.wrapupRounds = 0 // 本轮产出了新意图：还在推进
		e.stagnationMu.Unlock()
		return
	}
	st.wrapupRounds++
	shouldSettle := st.wrapupRounds >= wrapupConcludeRounds
	if shouldSettle {
		st.wrapup = false
	}
	e.stagnationMu.Unlock()
	if !shouldSettle {
		return
	}

	// 确定性收尾（C4）：concluded marker fact + 活动播报 + 守卫落 done + 冻结 + 蒸馏。
	payload := map[string]any{
		"subtype": "conclusion",
		"summary": fmt.Sprintf("concluded: 连续 %d 个意图零产出、收尾评估 %d 轮无新方向——边际收益递减，任务有序收尾", stagnationThreshold, wrapupConcludeRounds),
		"detail":  "确定性停滞闸判定（非模型自评）。partial 结论以图上已确认的事实/发现为准；后续可经 reviveTask 续跑。",
	}
	if id, aerr := t.Store.AddNode(db.KindFact, payload, 5, "confirmed", "stagnation_gate", nil); aerr == nil {
		if origin, oerr := t.Store.OriginFactID(); oerr == nil && origin > 0 {
			_ = t.Store.Link(origin, db.RelYields, id)
		}
	}
	e.emitActivity(t, db.Activity{Worker: "system", Kind: "text",
		Summary: fmt.Sprintf("【停滞闸】连续 %d 个零产出意图 + 收尾评估 %d 轮无新方向，任务收尾（done·concluded）", stagnationThreshold, wrapupConcludeRounds)})
	if won, err := e.m.SetTaskStatusGuarded(t.ID, "done"); err != nil || !won {
		log.Printf("[stagnation] task %s 落终态失败/已被判: %v", t.ID, err)
		return
	}
	e.freezeReplayCorpus(t.ID)
	if e.LegacyDistiller != nil {
		e.LegacyDistiller(t)
	}
	e.emitAuditSummary(t)
}

// Run starts the planner loop + N worker loops for a task. The loops always run
// but no-op until an LLM is configured (so a task created while idle picks up
// automatically once LLM is set from the UI).
func (e *Engine) Run(ctx context.Context, t *Task) {
	workers := e.m.Workers()
	e.deleteMu.RLock()
	if e.IsDeleting(t.ID) {
		e.deleteMu.RUnlock()
		return
	}
	if _, loaded := e.started.LoadOrStore(t.ID, true); loaded {
		e.deleteMu.RUnlock()
		t.Notify() // already running — just nudge a planning round
		return
	}
	rt := e.registerTaskRoutines(ctx, t.ID, 1+workers)
	e.deleteMu.RUnlock()
	e.touch(t.ID)
	runTaskRoutine(rt, func(loopCtx context.Context) { e.plannerLoop(loopCtx, t) })
	for i := 0; i < workers; i++ {
		name := fmt.Sprintf("work#%d", i+1)
		runTaskRoutine(rt, func(loopCtx context.Context) { e.workerLoop(loopCtx, t, name) })
	}
	e.startDeadlineCoordinator(ctx, t) // 任务级超时定时器(仅 timeout>0;去重)
	// 仅在「完全没有活动意图(open+running)」时才 kick 首轮规划。带种子意图的任务:种子已
	// 是 open,或已被上面刚起的 worker 抢先 claim 成 running——两种都算「有活干」,一律跳过
	// 首轮 planner,worker 直接领种子意图开跑,跑完由 NotifyDone/心跳唤醒 planner。
	// ⚠️ 不能用 Frontier(只数 open):worker 领取(open→running)与本检查存在竞态,会误 kick。
	// 重启自动恢复时也可能只剩 running 意图,同样应跳过。
	if has, _ := t.Store.HasActiveIntent(); !has {
		t.Notify() // kick the first planning round (acted on once LLM is ready)
	}
}

// plannerHeartbeatInterval 解析任务的 planner 心跳间隔。db.CreateTask 已归一
// (低于 600 一律抬到 600);这里再兜一次底,防内存态异常值。
func plannerHeartbeatInterval(t *Task) time.Duration {
	sec := t.PlanHeartbeatSeconds
	if sec < db.MinPlanHeartbeatSeconds { // 下限=默认=600(10min)
		sec = db.MinPlanHeartbeatSeconds
	}
	return time.Duration(sec) * time.Second
}

// resetPlannerTimer 安全重臂一个可能已触发的 Timer(标准 Stop→drain→Reset 模式)。
func resetPlannerTimer(timer *time.Timer, d time.Duration) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(d)
}

func (e *Engine) plannerLoop(ctx context.Context, t *Task) {
	interval := plannerHeartbeatInterval(t)
	// 心跳定时器在 loop 入口臂 = 从任务 start 计时:即使是跳过首轮 planner 的 seed 任务
	// (Run 里 frontier 非空不 kick 首轮)、这里一直阻塞,心跳也会在「任务 start + interval」
	// 触发第一轮规划。之后每次唤醒(边沿/心跳)都重臂 = 距上次任意规划触发的时长。
	heartbeat := time.NewTimer(interval)
	defer heartbeat.Stop()

	// runRound 跑一轮规划(含 debounce 合并 + 各 guard)。src 仅用于日志区分触发来源。
	runRound := func(src string) {
		// debounce: coalesce a burst of changes into one planning round
		timer := time.NewTimer(e.debounce)
	drain:
		for {
			select {
			case <-t.notify:
			case <-timer.C:
				break drain
			}
		}
		planner, _ := e.snapshotFor(t)
		if planner == nil {
			return // idle until LLM configured
		}
		// 策略上下文注入：宿主侧装配（计划 Task 6 Step 4）。
		//
		// 为什么在这里装：planner 实例由宿主侧构造（server/task_llm.go），但**注入
		// 装配**是宿主侧的责任 —— agent 包不该知道策略库在哪。引擎是 planner
		// 轮次的入口 ⇒ 在这里装，planner 每轮都拿到同一个 provider。
		e.wireStrategyContext(planner)
		if e.IsPaused(t.ID) {
			return // user-paused: don't plan
		}
		if e.IsDeleting(t.ID) {
			return
		}
		// terminal task (goals all met → done, or failed): the run is over. A
		// resume/nudge — e.g. auto-resume of the active task on restart — must NOT
		// re-plan (it would burn an LLM round and re-confirm a settled result).
		if isTerminalStatus(t.lifecycleSnapshot().Status) {
			return
		}
		// 任务级超时收尾中:丢弃普通唤醒——worker 收尾写回、Resume 的 Notify 都不再
		// 触发常规规划轮;终局那一轮由协调器(settleTask)直接驱动,不走这里。
		if e.isSettling(t.ID) {
			return
		}
		// goalless（人工直投）分支：任务已无 open 目标时 planner 不跑——跑了会重判
		// met→cancelExec 杀掉用户经主 agent 直投的意图。是否结束改由 frontier 决定：
		// 还有 open/running 意图 → 保持 running、静默等待；意图已全部跑干 → 落 done。
		// 整段纯 Go、不触发任何 LLM 调用，也不打规划轮 marker。
		if open, err := t.Store.HasOpenGoal(); err == nil && !open {
			t.drainTriggers() // 丢弃累积的 done/finding 触发，避免 goalless 长会话里无界增长
			if active, err := t.Store.HasActiveIntent(); err == nil && !active {
				// frontier 抽干且无在跑意图 → 收尾。用 Guarded 版做 CAS，避免踩到并发的
				// pause/delete/超时收尾的状态转换。
				if won, err := e.m.SetTaskStatusGuarded(t.ID, "done"); err != nil {
					log.Printf("[goalless] task %s 收尾落 done 失败: %v", t.ID, err)
				} else if won {
					e.emitActivity(t, db.Activity{Worker: "system", Kind: "text",
						Summary: "目标已全部达成，直投意图已执行完毕，任务结束"})
					// 任务进入终态 ⇒ 冻结重放语料（计划 Task 4 Step 6）。
					e.freezeReplayCorpus(t.ID)
					// P5 传承图：终局蒸馏（异步，draft 待人工评审）。
					if e.LegacyDistiller != nil {
						e.LegacyDistiller(t)
					}
					e.emitAuditSummary(t)
				}
			}
			return // goalless 分支永不进入 planner.Plan
		}
		if !e.beginTaskOperation(t.ID) {
			return
		}
		defer e.decInflight(t.ID)
		e.stampFirstRun(t) // 首次真正规划 → 盖 first_run_at + 算 deadline(仅带 timeout 的任务)
		e.touch(t.ID)
		emit := func(r db.Activity) { e.emitActivity(t, r) }
		ectx := e.clockCtx(e.execContextFor(ctx, t.ID), t, false) // cancellable by Pause; 带任务 deadline
		if ectx.Err() != nil || e.IsDeleting(t.ID) {
			return
		}
		log.Printf("[planner] task %s 规划中…(%s 触发)", t.ID, src)
		// round marker: each Plan() is one planner round; emit a boundary so the
		// UI can separate rounds in the transcript (kind='round').
		e.emitActivity(t, db.Activity{Worker: "planner", Kind: "round",
			Summary: fmt.Sprintf("第 %d 轮规划", e.nextPlannerRound(t.ID))})
		// what fired this round (worker done / finding; may be several — debounce
		// coalesces a burst; empty for time/heartbeat wakes).
		triggers := t.drainTriggers()
		taskIDInt, _ := strconv.ParseInt(t.ID, 10, 64)
		// C4 收尾评估轮的结算基线：本轮之前图上意图总数。
		intentsBefore, _ := t.Store.CountIntents()
		e.BeginLLMCall(t.ID)
		met, reason, err := planner.Plan(ectx, taskIDInt, e.m.assets, t.Store, t.Goal, triggers, emit)
		e.EndLLMCall(t.ID)
		// C4：收尾评估模式下结算本轮（0 新意图累计，达上限确定性收尾）。
		e.noteWrapupRoundResult(t, intentsBefore)
		switch {
		case err != nil && ectx.Err() == nil:
			log.Printf("[planner] task %s 规划出错: %v", t.ID, err)
		case met:
			log.Printf("[planner] task %s 判定目标达成: %s", t.ID, reason)
			// 所有目标达成 → 持久化任务状态为 done（前端 DTO 会优先展示该终态）。
			if err := e.m.SetTaskStatus(t.ID, "done"); err != nil {
				log.Printf("[planner] task %s 标记完成落库失败: %v", t.ID, err)
			} else {
				// 任务进入终态 ⇒ 冻结重放语料（计划 Task 4 Step 6）。
				e.freezeReplayCorpus(t.ID)
				// P5 传承图：终局蒸馏（异步，draft 待人工评审）。
				if e.LegacyDistiller != nil {
					e.LegacyDistiller(t)
				}
				e.emitAuditSummary(t)
			}
			// 任务已判完成 → 立刻取消在跑的 worker：它们手头的意图跑出来也没意义了。
			// 下一轮 worker 循环撞终态门就不再领新意图;被取消的这批走下方"任务已完成"分支
			// 归为 stopped(而非 blocked)。
			e.cancelExec(t.ID, agent.AbortGoalMet)
		default:
			log.Printf("[planner] task %s 规划完成", t.ID)
		}
		e.touch(t.ID)
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.notify:
			runRound("edge") // worker 结束 / finding / kill / resume / seed 首轮
		case <-heartbeat.C:
			// 周期兜底:死锁兜底 + 唤醒去监督飞行中的 worker(steer/kill) + 周期复查。
			runRound("heartbeat")
		}
		// 每次唤醒(边沿或心跳)后重臂心跳:任意规划触发都重算这段静置计时。
		resetPlannerTimer(heartbeat, interval)
	}
}

func (e *Engine) workerLoop(ctx context.Context, t *Task, name string) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		_, worker := e.snapshotFor(t)
		if worker == nil {
			if sleepCtx(ctx, 1500*time.Millisecond) {
				return
			}
			continue
		}
		if e.IsPaused(t.ID) {
			if sleepCtx(ctx, 1000*time.Millisecond) {
				return
			}
			continue // user-paused: don't claim/execute intents
		}
		if e.IsDeleting(t.ID) {
			return
		}
		if e.isSettling(t.ID) {
			if sleepCtx(ctx, 1000*time.Millisecond) {
				return
			}
			continue // 任务超时收尾中:不再领新意图(在跑的自行收尾,协调器等其 drain)
		}
		if isTerminalStatus(e.m.TaskStatus(t.ID)) {
			// 任务进入终态 ⇒ 冻结重放语料（计划 Task 4 Step 6）。
			//
			// 这是对【所有】终态路径的兜底：引擎里直接写终态的地方只覆盖 done，
			// 超时收尾（engine_timeout.go 的 settleTask）走的是另一条路。任务终态后
			// worker 循环仍在空转，这里正好是引擎观察到终态的那一刻。
			e.freezeReplayCorpus(t.ID)
			if sleepCtx(ctx, 1000*time.Millisecond) {
				return
			}
			continue // 任务已终态(done/failed/timeout):停止领取遗留意图,别在完成后空跑 frontier
		}
		if !e.beginTaskOperation(t.ID) {
			return
		}
		claimed := e.runWorkerStep(ctx, t, name, worker)
		e.decInflight(t.ID)
		if !claimed && sleepCtx(ctx, 800*time.Millisecond) {
			return
		}
	}
}

// runWorkerStep claims one intent from the frontier and fully settles it via
// runIntent. Returns false when nothing was claimable. The pool worker loop is its
// only caller.
func (e *Engine) runWorkerStep(ctx context.Context, t *Task, name string, worker *agent.Worker) bool {
	intent := e.claimNext(t, name)
	if intent == nil {
		return false
	}
	log.Printf("[worker %s] task %s 领取意图 #%d", name, t.ID, intent.ID)
	if e.ClaimMirror != nil {
		// 总控/群聊播报：认领是关键节点，原样转发（panic 隔离，不碰主链路）。
		// 意图摘要在 payload JSON 里（Node 本体不带 summary 列）⇒ 这里解一层最小
		// 结构；解不出退到原始 payload（截断），保证播报始终有意义。
		var p struct {
			Summary string `json:"summary"`
		}
		summary := ""
		if json.Unmarshal(intent.Payload, &p) == nil && strings.TrimSpace(p.Summary) != "" {
			summary = p.Summary
		} else {
			summary = string(intent.Payload)
		}
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					log.Printf("[claim-mirror] panic（task %s intent #%d）: %v", t.ID, intent.ID, rec)
				}
			}()
			e.ClaimMirror(t.ID, name, intent.ID, summary)
		}()
	}
	return e.runIntent(ctx, t, name, worker, intent, "", "")
}

// runIntent executes and fully settles one already-claimed (state=running) intent.
// Both the pool worker loop (via runWorkerStep) and the human-message handler (via
// runDetachedIntent, a dedicated goroutine outside the worker pool) call it, so the
// execute/retry/state-write logic lives in exactly one place. A non-empty message
// is injected as this turn's input through ExecuteWithMessage; requestID keys the
// transcript marker that dedups re-injection across model_error retries. The caller
// must already hold one task-operation admission for the whole sequence so a delete
// cannot observe quiescence between the LLM return and the final DB writes.
func (e *Engine) runIntent(ctx context.Context, t *Task, name string, worker *agent.Worker, intent *db.Node, requestID, message string) bool {
	hasChatMessage := message != ""
	e.stampFirstRun(t) // 首次真正执行 → 盖 first_run_at + 算 deadline(仅带 timeout 的任务)
	e.touch(t.ID)
	emit := func(r db.Activity) { e.emitActivity(t, r) }
	ectx := e.clockCtx(e.execContextFor(ctx, t.ID), t, false) // cancellable by Pause; 带任务 deadline
	if ectx.Err() != nil || e.IsDeleting(t.ID) {
		if err := transitionIntentState(t.Store, intent.ID, "running", "open"); err != nil {
			log.Printf("[worker %s] task %s 意图 #%d 领取后回退失败: %v", name, t.ID, intent.ID, err)
		}
		return true
	}
	// per-work child context so the planner's kill_work can stop just this work.
	workCtx, workCancel := context.WithCancelCause(ectx)
	e.registerWorkForTask(intent.ID, t, workCancel)
	// 补投上次没交付完的纠偏：它们在库里（崩溃/收场都没丢），这次执行接着给。
	e.replayPendingSteers(t, intent.ID)
	// wrap the guard hooks so steer_work can inject a mid-run course-correction
	// for THIS intent (drained before the worker's next tool call).
	iid := intent.ID
	taskEmit := func(a db.Activity) {
		nid := iid
		a.NodeID, a.Worker = &nid, name
		emit(a)
	}
	label := fmt.Sprintf("%s · #%d", name, iid)
	workCtx = intercept.WithTaskContext(workCtx, t.ID, label, taskEmit)
	// nudges 有意建在 model_error 重跑循环之外:空转续跑的上限是「这条意图」的总量，
	// 重跑一轮不该把额度清零重来。
	hooks := steerHooks{
		inner: WrapHookRunner(t.Guard.Hooks()),
		drain: func() (string, bool) {
			msg, ok := e.drainSteer(iid)
			if ok {
				// 交付过的那条落库副本标为已消费：否则下次执行会再投一遍。
				if err := e.consumeSteer(t, iid, msg); err != nil {
					log.Printf("[steer] task %s 意图 #%d 标记纠偏已消费失败: %v", t.ID, iid, err)
				}
			}
			return msg, ok
		},
		nudges: &atomic.Int64{},
		limit:  e.emptyTurnNudgeLimit(),
		label:  label,
	}
	wTaskID, _ := strconv.ParseInt(t.ID, 10, 64)
	e.BeginLLMCall(t.ID)
	var reason harness.TerminalReason
	var wrote agent.WriteCounts
	var err error
	if hasChatMessage {
		reason, wrote, err = worker.ExecuteWithMessage(workCtx, name, wTaskID, e.m.assets, t.Store, intent, hooks, emit, e.m.enrich, t.NotifyFinding, requestID, message)
	} else {
		reason, wrote, err = worker.Execute(workCtx, name, wTaskID, e.m.assets, t.Store, intent, hooks, emit, e.m.enrich, t.NotifyFinding)
	}
	e.EndLLMCall(t.ID)
	// model_error 收场 → 额外重跑几次（退避后再试）。仅在意图仍属本 work、任务
	// 未暂停/未终止/未取消【且未进入收尾】时重试；否则让位给对应分支处理(收尾期不
	// 再重试,避免退避挤占其他 worker 的优雅收尾窗口)。
	maxRetries, retryBackoff := e.modelErrorRetryPolicy()
	for attempt := 1; attempt <= maxRetries &&
		retryableWorkerModelError(reason, err) &&
		workCtx.Err() == nil && ectx.Err() == nil && !e.IsPaused(t.ID) && !e.isSettling(t.ID); attempt++ {
		log.Printf("[worker %s] task %s 意图 #%d model_error 收场，%v 后重试 (%d/%d)",
			name, t.ID, intent.ID, retryBackoff, attempt, maxRetries)
		if sleepCtx(workCtx, retryBackoff) {
			break // 退避期间被取消（终止/暂停）→ 交给下方分支处理
		}
		e.BeginLLMCall(t.ID)
		if hasChatMessage {
			reason, wrote, err = worker.ExecuteWithMessage(workCtx, name, wTaskID, e.m.assets, t.Store, intent, hooks, emit, e.m.enrich, t.NotifyFinding, requestID, message)
		} else {
			reason, wrote, err = worker.Execute(workCtx, name, wTaskID, e.m.assets, t.Store, intent, hooks, emit, e.m.enrich, t.NotifyFinding)
		}
		e.EndLLMCall(t.ID)
	}
	// Capture kill state before detachWork cancels workCtx. kill = this work's
	// ctx was cancelled (planner kill_work) while the TASK ctx kept running; a
	// pause cancels the task ctx (ectx) instead. Checking workCtx.Err() AFTER
	// unregister would always be true (unregister cancels it) → every completed
	// work would be wrongly marked stopped.
	e.markWorkSettled(intent.ID) // worker 已返回：宽限期的取消动作就此打住
	workCause := context.Cause(workCtx)
	killed := workCtx.Err() != nil && ectx.Err() == nil
	action, completeWork := e.detachWork(intent.ID)
	// A caller may stop waiting and release its in-memory reservation before the
	// agent honors cancellation. The named context cause remains authoritative and
	// still settles the stopped run into a recoverable state.
	if action == "" {
		switch {
		case errors.Is(workCause, agent.AbortWorkPausedByUser):
			action = "pause"
		case errors.Is(workCause, agent.AbortWorkCancelledByUser):
			action = "cancel"
		}
	}
	var controlErr error
	defer func() { completeWork(controlErr) }()
	if action == "pause" {
		controlErr = transitionIntentState(t.Store, intent.ID, "running", "paused")
		if controlErr != nil {
			log.Printf("[worker %s] task %s 意图 #%d 暂停状态落库失败: %v", name, t.ID, intent.ID, controlErr)
			return true
		}
		log.Printf("[worker %s] task %s 意图 #%d 已暂停", name, t.ID, intent.ID)
		e.touch(t.ID)
		return true
	}
	if action == "cancel" {
		// Park the stopped run in paused before handing cleanup to the API. If the
		// request disconnects after cancellation, the intent remains recoverable and
		// a later cancel can finish cleanup instead of leaving a phantom running row.
		controlErr = transitionIntentState(t.Store, intent.ID, "running", "paused")
		if controlErr != nil {
			log.Printf("[worker %s] task %s 意图 #%d 取消栅栏落库失败: %v", name, t.ID, intent.ID, controlErr)
			return true
		}
		log.Printf("[worker %s] task %s 意图 #%d 已停止，等待取消清理", name, t.ID, intent.ID)
		e.touch(t.ID)
		return true
	}
	// if a pause cancelled this run mid-flight, return the intent to the frontier
	// so it is re-claimed on resume — the worker will resume the prior LLM
	// conversation from its transcript instead of restarting from scratch.
	if ectx.Err() != nil && taskExecutionPaused(context.Cause(ectx)) {
		if err := transitionIntentState(t.Store, intent.ID, "running", "open"); err != nil {
			log.Printf("[worker %s] task %s 意图 #%d 任务暂停回退失败: %v", name, t.ID, intent.ID, err)
		}
		return true
	}
	// 任务超时收尾的硬兜底 cancel(非 pause、非 kill)取消了本 run → 归为 exhausted(已收尾),
	// 不要误标 blocked。此时 worker 通常已在 settlement 阶段把结果写回。
	if ectx.Err() != nil && e.isSettling(t.ID) {
		if err := transitionIntentState(t.Store, intent.ID, "running", "exhausted"); err != nil {
			log.Printf("[worker %s] task %s 意图 #%d 超时收尾状态落库失败: %v", name, t.ID, intent.ID, err)
		}
		log.Printf("[worker %s] task %s 意图 #%d 因任务超时收尾结束(exhausted)，写回 %s", name, t.ID, intent.ID, wrote)
		e.touch(t.ID)
		return true
	}
	// 任务已判完成(done via 常规路径)→ 上面 cancelExec 取消了本 run。意图结果已无意义,
	// 标 stopped(不是 blocked),别污染已完成任务的意图状态。
	if ectx.Err() != nil && isTerminalStatus(e.m.TaskStatus(t.ID)) {
		if err := transitionIntentState(t.Store, intent.ID, "running", "stopped"); err != nil {
			log.Printf("[worker %s] task %s 意图 #%d 终态停止落库失败: %v", name, t.ID, intent.ID, err)
		}
		log.Printf("[worker %s] task %s 意图 #%d 因任务已完成而取消(stopped)", name, t.ID, intent.ID)
		e.touch(t.ID)
		return true
	}
	// killed by the planner: mark stopped (don't write back results, don't auto-reclaim).
	if killed {
		if err := transitionIntentState(t.Store, intent.ID, "running", "stopped"); err != nil {
			log.Printf("[worker %s] task %s 意图 #%d planner 停止落库失败: %v", name, t.ID, intent.ID, err)
		}
		log.Printf("[worker %s] task %s 意图 #%d 被终止(stopped)", name, t.ID, intent.ID)
		e.touch(t.ID)
		// 被终止的意图默认可续跑：任务还没进终态时把它放回 frontier，让下一个
		// worker 接手（它会从上次的 transcript 接着跑，不必从头再来）。
		// 任务已终态时不重开：那里没有 worker 会跑，重开只会造一条死状态——
		// 与「额度阻塞重开」用的是同一条边界（server/manager.go）。
		if e.reopenStoppedIntent(t, intent.ID) {
			log.Printf("[worker %s] task %s 意图 #%d 已重开，等待下一个 worker 接手", name, t.ID, intent.ID)
		}
		t.Notify()
		return true
	}
	if err != nil {
		log.Printf("[worker %s] intent %d: %v", name, intent.ID, err)
	}
	// terminal分流：撞步数上限 ≠ 完成。max_turns→exhausted（规划者据此知道这个方向
	// 试过但没真正做完、需换角度，而非当成已覆盖永久跳过）；出错→blocked；正常→done。
	// D1 活跃度软着陆（方案 v6，d2d 借鉴）：预算到线但本轮有有效写回（正要出
	// 成果）⇒ 不掐——意图转回 open 由下一个 worker 带满预算续跑，最多延 3 次；
	// 二次到线仍零写回才真的 exhausted。
	state := "done"
	switch {
	case err != nil:
		state = "blocked"
	case (reason == harness.ReasonMaxTurns || reason == harness.ReasonTimeout) && e.tryExtendActiveIntent(t, intent.ID, wrote, name, reason):
		// 已延期重开：跳过 exhausted 落库（意图已在 tryExtend 里转回 open），
		// 本步返回 true（认领循环继续），记账照跑。
		e.noteIntentWriteBack(t, wrote)
		return true
	case reason == harness.ReasonMaxTurns:
		state = "exhausted"
		log.Printf("[worker %s] intent %d 撞步数上限(exhausted)，本次写回 %s", name, intent.ID, wrote)
	case reason == harness.ReasonTimeout:
		state = "exhausted"
		log.Printf("[worker %s] intent %d 运行超时(exhausted)，收尾后写回 %s", name, intent.ID, wrote)
	}
	if state == "blocked" && isTaskLLMChainExhausted(err) {
		_ = t.Store.SetIntentBlockedReason(intent.ID, db.IntentBlockedLLMQuota)
	} else {
		if stateErr := transitionIntentState(t.Store, intent.ID, "running", state); stateErr != nil {
			log.Printf("[worker %s] task %s 意图 #%d 终态 %s 落库失败: %v", name, t.ID, intent.ID, state, stateErr)
		}
	}
	// C4 停滞闸记账：按写回量更新停滞状态（0 写回递增 / 有效写回清零）。
	e.noteIntentWriteBack(t, wrote)
	// P3 裸退出兜底（方案 v3.1 §7.2）：worker 层交接轮是 best-effort（崩溃/kill 时
	// 预退出路径不保证执行）——终态 blocked/exhausted 且该意图没有任何 handoff 事实
	// ⇒ 引擎补一条确定性"裸退出"标记，图轨迹本就是部分 handoff，这条只是把缺口
	// 显式钉在图上（继承者知道前任没来得及交接）。
	if state == "blocked" || state == "exhausted" {
		// C6 写回纪律（引擎兜底）：0 写回但转录里有真实工具结果 ⇒ 自动补一条
		// 结构化 marker（摘要=最后一条工具结果截断），让 planner 下轮态势可见。
		if wrote.Total() == 0 {
			if last := t.Store.LastIntentToolResult(intent.ID); last != "" {
				payload := map[string]any{
					"subtype": "handoff",
					"summary": fmt.Sprintf("零写回收尾（%s）：最后产出——%s", state, truncateRunes(strings.TrimSpace(last), 200)),
					"detail":  "worker 结束时未写回任何事实/资产/漏洞；此标记由引擎从转录补写，内容来自最后一条工具结果。",
					"evidence": "engine deterministic marker（0 write-back）",
				}
				if id, aerr := t.Store.AddNode(db.KindFact, payload, 5, "confirmed", "handoff_missing", nil); aerr == nil {
					_ = t.Store.Link(intent.ID, db.RelYields, id)
					log.Printf("[handoff] task %s 意图 #%d 零写回，已补 marker fact #%d", t.ID, intent.ID, id)
				}
			}
		}
		if has, herr := t.Store.IntentHasHandoffFact(intent.ID); herr == nil && !has {
			payload := map[string]any{
				"subtype":  "handoff",
				"summary":  fmt.Sprintf("裸退出：%s（%s）——本次 run 未能产出交接", state, terminateCauseLabel(err, reason)),
				"detail":   "worker 的交接轮未执行（崩溃/超时/预算耗尽）。图上该意图的 tool_use/derived_from 链是仅存的上下文轨迹；继任者请从图轨迹重建态势，并把重建结论写成一条正式 handoff。",
				"evidence": "engine deterministic marker（无 worker 产出）",
			}
			if id, aerr := t.Store.AddNode(db.KindFact, payload, 5, "confirmed", "handoff_missing", nil); aerr == nil {
				_ = t.Store.Link(intent.ID, db.RelYields, id)
				log.Printf("[handoff] task %s 意图 #%d 裸退出，已落兜底标记 fact #%d", t.ID, intent.ID, id)
			}
		}
	}
	log.Printf("[worker %s] task %s 意图 #%d 结束: %s (写回 %s)", name, t.ID, intent.ID, state, wrote)
	e.touch(t.ID)
	t.NotifyDone(intent.ID) // results changed the graph -> wake the planner (with the just-finished intent id)
	// 子任务级归因：意图落到终态时归因一次（计划 Task 4 Step 6）。
	//
	// 为什么挂在【意图】终态而不是每个工具步骤：归因是按子任务聚合的，逐工具
	// 归因会把一次成功碎成十几个「命中」，于是「一次成功 = 通用能力」（反模式 10）。
	//
	// 为什么失败子任务（blocked / exhausted）也归因：失败子任务只写 usage
	// （outcome='miss'），**不写 strategy_evidence、不改 strategies 任何列** ——
	// 这道闸门焊在归因器内部（db.ApplyStrategyVotes 只认 outcome='hit' 的使用明细）。
	e.attributeSubTaskOutcome(wTaskID)
	return true
}

// runDetachedIntent runs one paused intent OUTSIDE the worker pool in its own
// goroutine — the human-message path. It transitions the intent paused->running
// itself (never through 'open'), so the pool, which only claims 'open', can never
// race it; the "at most one run per intent" invariant still holds because winning
// the CAS is the sole entry and work[intentID] was cleared when the pause settled.
// Because it does not compete for a frontier slot, a user message continues the
// worker immediately even when all pool slots are busy (mirroring how the
// main-agent chat handler starts its run directly). The spawned goroutine owns one
// task-operation admission for the whole run and roots its context at ctx (pass the
// server root, never the HTTP request, so a disconnect cannot strand the run while
// task pause/delete/shutdown still stops it). Returns an error if the run could not
// be started; the intent is left untouched in that case.
func (e *Engine) runDetachedIntent(ctx context.Context, t *Task, intentID int64, requestID, message, agentMessage string) error {
	if !e.beginTaskOperation(t.ID) {
		return fmt.Errorf("task is being deleted")
	}
	release := true
	defer func() {
		if release {
			e.decInflight(t.ID)
		}
	}()
	_, worker := e.snapshotFor(t)
	if worker == nil {
		return fmt.Errorf("worker 尚未就绪")
	}
	node, err := t.Store.GetNode(intentID)
	if err != nil {
		return err
	}
	if node == nil || node.Kind != db.KindIntent {
		return fmt.Errorf("intent not found")
	}
	changed, err := t.Store.CompareAndSetIntentState(intentID, "paused", "running")
	if err != nil {
		return err
	}
	if !changed {
		return fmt.Errorf("%w: 意图不再是 paused 状态", db.ErrIntentStateConflict)
	}
	node.State, node.Owner = "running", "chat"
	// Record the human turn as a visible activity BEFORE the run starts, so it is
	// ordered ahead of any worker step and never appears without the run happening.
	// Keep the UI copy concise; ExecuteWithMessage writes the server-resolved
	// reference snapshot into the intent transcript as the LLM input.
	uid := intentID
	e.emitActivity(t, db.Activity{NodeID: &uid, Worker: "user", Kind: "user", Summary: message, Detail: message})
	release = false // ownership of the admission passes to the goroutine
	go func() {
		defer e.decInflight(t.ID)
		e.runIntent(ctx, t, "chat", worker, node, requestID, agentMessage)
	}()
	return nil
}

func taskExecutionPaused(cause error) bool {
	var abort *agent.AbortCause
	if !errors.As(cause, &abort) {
		return false
	}
	switch abort.Code {
	case "paused_by_user", "paused_by_orchestrator", "paused_on_reload", "paused_race_guard",
		"queued_for_admission", "llm_unavailable_queued", "task_deleted":
		return true
	default:
		return false
	}
}

func sleepCtx(ctx context.Context, d time.Duration) (done bool) {
	select {
	case <-ctx.Done():
		return true
	case <-time.After(d):
		return false
	}
}

// claimNext 认领一条意图。
//
// 两道闸门（顺序即仲裁顺序）：
//  1. auto 的兜底闸门：auto 只在"组内没有可用成员"时才被放进认领路径；有可用
//     成员时它是分发者，让位给成员。
//  2. 组内并发上限 N：一个群同时最多 N 个 intent 在执行，超了就不再领新的
//     （**不打断在途** —— 已认领的继续跑完，调低上限也不杀在途）。
//
// 全局任务槽位 M 不在这里判定：它在**任务准入**那一层（reconcileConcurrency，
// task_concurrency_limit）——没有槽位的任务根本不会跑到认领这一步。两级并发因此
// 是"先 M 后 N"：M 决定哪些群能开跑，N 决定一个群里能同时跑几个。
func (e *Engine) claimNext(t *Task, name string) *db.Node {
	if !e.dispatcherMayClaim(t, name) {
		return nil
	}
	if !e.groupCapAllowsClaim(t) {
		return nil
	}
	fr, _ := t.Store.Frontier(20)
	for _, in := range fr {
		if e.claimIntent(t, in.ID, name) {
			return in
		}
	}
	return nil
}

// dispatcherMayClaim 判定当前 worker 这次能不能参与认领。
//
// auto 的二重身份：auto 是**分发者**（派工），也是组里没人时的**兜底执行者**。
//   - 组里有可用成员 ⇒ auto 让位（成员干活，它是分发者）；
//   - 组里没有可用成员 ⇒ 放行 auto（它是唯一能让任务不停下来的执行者）。
//
// 判据在 server 侧（manager.go）：engine 不查群成员表。
func (e *Engine) dispatcherMayClaim(t *Task, name string) bool {
	if e.GroupDispatcherClaimable == nil {
		return true
	}
	tid, ok := e.taskNumericID(t)
	if !ok {
		return true // task id 不是数字 ⇒ 不存在大厅（大厅按 task id 关联）⇒ 不设闸门
	}
	return !e.GroupDispatcherClaimable(tid, name)
}

// groupCapAllowsClaim 快速判定组内并发上限：还有空位才继续去认领。
//
// 这是**快路径**，不是权威判定：权威判定在 claimIntentWithinGroupCap 的事务里
// （计数与认领同一条路径 ⇒ 并发认领不会都越过上限）。快路径的作用是"到顶了就别
// 进候选循环"，省掉每个候选意图一次事务。数不出来时不在这里收紧：事务里数不出来
// 会 fail closed（不认领）。
func (e *Engine) groupCapAllowsClaim(t *Task) bool {
	limit, hasGroup := e.groupConcurrencyLimit(t)
	if !hasGroup || limit <= 0 {
		return true
	}
	if e.GroupRunningIntents == nil {
		return true
	}
	tid, ok := e.taskNumericID(t)
	if !ok {
		return true
	}
	running, err := e.GroupRunningIntents(tid)
	if err != nil {
		return true
	}
	return running < limit
}

// groupConcurrencyLimit 读这个 task 所属群组的并发上限；ok=false 表示没有群组。
func (e *Engine) groupConcurrencyLimit(t *Task) (int, bool) {
	if e.GroupConcurrencyLimit == nil {
		return 0, false
	}
	tid, ok := e.taskNumericID(t)
	if !ok {
		return 0, false
	}
	return e.GroupConcurrencyLimit(tid)
}

// taskNumericID 把 task 的字符串 id 解析成数值 id（大厅/额度都按数值 id 关联）。
func (e *Engine) taskNumericID(t *Task) (int64, bool) {
	if t == nil {
		return 0, false
	}
	id, err := strconv.ParseInt(t.ID, 10, 64)
	if err != nil {
		return 0, false
	}
	return id, true
}

// claimIntent 认领一个意图（受群组并发上限约束）。
//
// 没有群组的 task（平台的绝大多数任务）走原路径：单条 CAS open→running，行为与
// 从前完全一致。有群组的 task 走"带闸门的认领"：上限判定与认领在**同一个事务**
// 里完成 ⇒ 两个并发认领不会都越过上限（原子性见 manager.claimIntentWithinGroupCap）。
func (e *Engine) claimIntent(t *Task, intentID int64, name string) bool {
	if _, hasGroup := e.groupConcurrencyLimit(t); !hasGroup {
		claimed, err := t.Store.ClaimIntent(intentID, name)
		if err != nil {
			log.Printf("[worker %s] task %s 认领意图 #%d 失败: %v", name, t.ID, intentID, err)
		}
		return claimed
	}
	tid, ok := e.taskNumericID(t)
	if !ok {
		claimed, err := t.Store.ClaimIntent(intentID, name)
		if err != nil {
			log.Printf("[worker %s] task %s 认领意图 #%d 失败: %v", name, t.ID, intentID, err)
		}
		return claimed
	}
	claimed, err := e.m.claimIntentWithinGroupCap(tid, t.Store.ID(), intentID, name)
	if err != nil {
		log.Printf("[worker %s] task %s 认领意图 #%d 失败: %v", name, t.ID, intentID, err)
		return false
	}
	return claimed
}

// terminateCauseLabel 把终态原因渲染成一句人话（裸退出标记用）。
func terminateCauseLabel(err error, reason harness.TerminalReason) string {
	if err != nil {
		return "error: " + err.Error()
	}
	return string(reason)
}

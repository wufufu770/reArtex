// Package server 里的监督器是任务生命周期的确定性状态机。
//
// 模型只能提议迁移（ProposedTransition），能不能走由转移表裁决。
// 本文件是纯逻辑和内存实现：不读库、不改任务状态、不碰规划循环。
// 库和引擎接线在后续批次，默认不启用。
package server

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
)

// SupervisorState 是监督器的十个状态。名字按卡面骨架，方案二原文不在本仓库。
type SupervisorState string

const (
	SupPreparing  SupervisorState = "PREPARING"
	SupPlanning   SupervisorState = "PLANNING"
	SupExecuting  SupervisorState = "EXECUTING"
	SupVerifying  SupervisorState = "VERIFYING"
	SupWaiting    SupervisorState = "WAITING"
	SupPaused     SupervisorState = "PAUSED"
	SupCompleting SupervisorState = "COMPLETING"
	SupDone       SupervisorState = "DONE"
	SupFailed     SupervisorState = "FAILED"
	SupAborted    SupervisorState = "ABORTED"
)

// SupScopeTask 是监督器唯一生效的范围。legacy 会话不在此列。
const SupScopeTask = "task"

const (
	SupEventPlanRoundDone   = "plan_round_done"
	SupEventIntentClaimed   = "intent_claimed"
	SupEventIntentSettled   = "intent_settled"
	SupEventGoalGate        = "goal_gate"
	SupEventBudgetExhausted = "budget_exhausted"
	SupEventHuman           = "human"
	SupEventTimeout         = "timeout"
	SupEventProposal        = "llm_proposal"
	SupActorSystem          = "system"
	SupActorLLM             = "llm-proposal"
	SupHumanPause           = "pause"
	SupHumanAbort           = "abort"
	SupHumanResume          = "resume"
	SupHumanResumePlan      = "resume_plan"
)

// supAllStates 的顺序只用于测试和枚举，不是转移顺序。
var supAllStates = []SupervisorState{
	SupPreparing, SupPlanning, SupExecuting, SupVerifying, SupWaiting,
	SupPaused, SupCompleting, SupDone, SupFailed, SupAborted,
}

// SupTransitions 是唯一合法的迁移表。不在表里的提议一律拒绝。
var SupTransitions = map[SupervisorState][]SupervisorState{
	SupPreparing:  {SupPlanning, SupAborted, SupFailed},
	SupPlanning:   {SupExecuting, SupPaused, SupAborted, SupFailed},
	SupExecuting:  {SupVerifying, SupWaiting, SupPaused, SupCompleting, SupFailed},
	SupVerifying:  {SupExecuting, SupCompleting, SupWaiting, SupFailed},
	SupWaiting:    {SupExecuting, SupPaused, SupAborted},
	SupPaused:     {SupPlanning, SupExecuting, SupAborted},
	SupCompleting: {SupDone, SupFailed},
	SupDone:       {},
	SupFailed:     {},
	SupAborted:    {},
}

// SupEvent 是确定性事件。模型不能伪造事件去改状态，只能走 Propose。
type SupEvent struct {
	Kind     string `json:"kind"`
	TaskID   string `json:"task_id"`
	IntentID int64  `json:"intent_id,omitempty"`
	Detail   string `json:"detail,omitempty"`
	// IntentsLeft 只对 intent_settled 有意义。nil 表示未知，不据此离开 EXECUTING。
	IntentsLeft *int `json:"intents_left,omitempty"`
}

// ProposedTransition 是模型唯一能表达迁移意愿的方式：提议，不裁决。
type ProposedTransition struct {
	To     SupervisorState `json:"to"`
	Reason string          `json:"reason"`
}

// ProposeOutcome 是提议的结果。被拒时 Accepted 为 false，Error 只留给存储失败。
type ProposeOutcome struct {
	State    SupervisorState
	Accepted bool
	Reason   string
}

// TransitionDecision 是一次裁决。Noop 表示事件合法但状态不变，不写历史。
type TransitionDecision struct {
	From     SupervisorState
	To       SupervisorState
	Accepted bool
	Noop     bool
	Reason   string
	Actor    string
	Event    string
}

// SupRecord 是一条迁移历史。
type SupRecord struct {
	From   SupervisorState
	To     SupervisorState
	Event  string
	Actor  string
	Reason string
	At     time.Time
}

// SupRejectError 是 Apply 拒绝迁移。状态没有改。
type SupRejectError struct {
	From   SupervisorState
	To     SupervisorState
	Reason string
}

func (e *SupRejectError) Error() string {
	if e == nil {
		return "监督器拒绝迁移"
	}
	return "监督器拒绝迁移：" + e.Reason
}

// SupervisorScopeAllows 报告这个范围能不能挂监督器。只有任务可以。
func SupervisorScopeAllows(scope string) bool {
	return scope == SupScopeTask
}

// Valid 报告 s 是不是十个状态之一。
func (s SupervisorState) Valid() bool {
	_, ok := SupTransitions[s]
	return ok
}

// Terminal 报告 s 是不是只进不出的终态。
func (s SupervisorState) Terminal() bool {
	switch s {
	case SupDone, SupFailed, SupAborted:
		return true
	default:
		return false
	}
}

// TransitionAllowed 报告 from→to 是否在转移表里。
func TransitionAllowed(from, to SupervisorState) bool {
	for _, next := range SupTransitions[from] {
		if next == to {
			return true
		}
	}
	return false
}

// MaxStay 是这个状态允许停留的最长时间。第二个返回值 false 表示不自动超时
// （已暂停和终态）。时长是本实现定的：卡面只给了例子，没有给秒数。
func MaxStay(st SupervisorState) (time.Duration, bool) {
	switch st {
	case SupPreparing:
		return 30 * time.Minute, true
	case SupPlanning:
		return 30 * time.Minute, true
	case SupExecuting:
		return 2 * time.Hour, true
	case SupVerifying:
		return 30 * time.Minute, true
	case SupWaiting:
		return 24 * time.Hour, true
	case SupCompleting:
		return 15 * time.Minute, true
	default:
		return 0, false
	}
}

// Decide 按事件裁决下一步。goalsMet 只约束 goal_gate 和进入 DONE。
func Decide(from SupervisorState, ev SupEvent, goalsMet bool) TransitionDecision {
	to, noop, ok, reason := eventTarget(from, ev, goalsMet)
	dec := TransitionDecision{
		From: from, To: to, Accepted: ok, Noop: noop && ok,
		Reason: reason, Event: strings.TrimSpace(ev.Kind), Actor: SupActorSystem,
	}
	if ok && !noop {
		dec.To = to
		if strings.TrimSpace(ev.Detail) != "" {
			dec.Reason = strings.TrimSpace(ev.Detail)
		} else if dec.Reason == "" {
			dec.Reason = dec.Event
		}
	}
	if !ok {
		dec.To = from
	}
	return dec
}

// DecideProposal 裁决模型提议。进入 DONE 必须 goalsMet，即使转移表里有这条边。
func DecideProposal(from, to SupervisorState, reason string, goalsMet bool) TransitionDecision {
	dec := TransitionDecision{
		From: from, To: from, Event: SupEventProposal, Actor: SupActorLLM,
		Reason: strings.TrimSpace(reason),
	}
	if !from.Valid() {
		dec.Reason = "未知当前状态"
		return dec
	}
	if from.Terminal() {
		dec.Reason = "终态只进不出"
		return dec
	}
	if !to.Valid() {
		dec.Reason = "未知目标状态"
		return dec
	}
	if to == SupDone && !goalsMet {
		dec.Reason = "goal_met 闸门未通过，拒绝迁入 DONE"
		return dec
	}
	if !TransitionAllowed(from, to) {
		dec.Reason = "不在转移表"
		return dec
	}
	dec.Accepted = true
	dec.To = to
	if dec.Reason == "" {
		dec.Reason = "模型提议"
	}
	return dec
}

func eventTarget(from SupervisorState, ev SupEvent, goalsMet bool) (to SupervisorState, noop, ok bool, reason string) {
	if !from.Valid() {
		return from, false, false, "未知当前状态"
	}
	if from.Terminal() {
		return from, false, false, "终态只进不出"
	}
	switch strings.TrimSpace(ev.Kind) {
	case SupEventPlanRoundDone:
		switch from {
		case SupPreparing:
			return SupPlanning, false, true, ""
		case SupPlanning:
			return SupExecuting, false, true, ""
		case SupExecuting:
			return from, true, true, "执行中的规划轮不改变状态"
		case SupVerifying:
			return SupExecuting, false, true, ""
		default:
			return from, false, false, "当前状态不接受规划轮结束"
		}
	case SupEventIntentClaimed:
		switch from {
		case SupPlanning, SupPaused, SupVerifying:
			return SupExecuting, false, true, ""
		case SupExecuting:
			return from, true, true, "执行中再次认领不改变状态"
		default:
			return from, false, false, "当前状态不允许认领意图"
		}
	case SupEventIntentSettled:
		switch from {
		case SupExecuting:
			if ev.IntentsLeft == nil || *ev.IntentsLeft > 0 {
				return from, true, true, "仍有未结算意图或数量未知，保持执行"
			}
			return SupVerifying, false, true, ""
		case SupVerifying:
			return from, true, true, "核验中的结算不改变状态"
		default:
			return from, false, false, "当前状态不接受意图结算"
		}
	case SupEventGoalGate:
		if !goalsMet {
			return from, false, false, "goal_met 闸门未通过，拒绝进入完成"
		}
		switch from {
		case SupExecuting, SupVerifying:
			return SupCompleting, false, true, ""
		case SupCompleting:
			return SupDone, false, true, ""
		default:
			return from, false, false, "当前状态不能经 goal_met 闸门向前"
		}
	case SupEventBudgetExhausted, SupEventTimeout:
		return failClosedTarget(from)
	case SupEventHuman:
		return humanTarget(from, ev.Detail)
	default:
		return from, false, false, "未知事件"
	}
}

// failClosedTarget 选择超时或预算耗尽的去向：优先 PAUSED，不行才 FAILED。
// 不会选 DONE。没有合法去向就拒绝（例如已经在 PAUSED）。
func failClosedTarget(from SupervisorState) (to SupervisorState, noop, ok bool, reason string) {
	for _, cand := range []SupervisorState{SupPaused, SupFailed} {
		if cand == SupDone {
			continue
		}
		if TransitionAllowed(from, cand) {
			return cand, false, true, ""
		}
	}
	return from, false, false, "没有合法的 fail-closed 去向（不会进入 DONE）"
}

func humanTarget(from SupervisorState, detail string) (to SupervisorState, noop, ok bool, reason string) {
	switch strings.ToLower(strings.TrimSpace(detail)) {
	case SupHumanPause:
		to = SupPaused
	case SupHumanAbort:
		to = SupAborted
	case SupHumanResume:
		if TransitionAllowed(from, SupExecuting) {
			return SupExecuting, false, true, ""
		}
		if TransitionAllowed(from, SupPlanning) {
			return SupPlanning, false, true, ""
		}
		return from, false, false, "当前状态不能恢复执行"
	case SupHumanResumePlan:
		to = SupPlanning
	default:
		return from, false, false, "人工指令缺少合法动作"
	}
	if !TransitionAllowed(from, to) {
		return from, false, false, "不在转移表"
	}
	return to, false, true, ""
}

// Supervisor 裁决并记下迁移。store 为空时用内存，方便单测；库实现在后续批次注入。
type Supervisor struct {
	store supervisorStore
	now   func() time.Time

	// GoalsMet 报告目标是否全部了结。nil 或出错都按未通过（fail-closed），
	// 调用方不得在这里再调用 Apply / Propose。
	GoalsMet func(ctx context.Context, taskID string) (bool, error)
	// OnReject 在拒绝时调用。审计写入失败只应记日志，不要反过来改状态。
	OnReject func(ctx context.Context, taskID string, dec TransitionDecision)
	// OnTerminal 在进入 DONE / FAILED / ABORTED 之后调用，用来落任务终态。
	// 迁移已经提交；这里失败不会把监督器状态改回去。
	OnTerminal func(ctx context.Context, taskID string, to SupervisorState) error
}

type supervisorStore interface {
	load(ctx context.Context, taskID string) (SupervisorState, time.Time, error)
	mutate(ctx context.Context, taskID string, decide func(SupervisorState) TransitionDecision) (TransitionDecision, error)
	history(ctx context.Context, taskID string) ([]SupRecord, error)
}

type memTask struct {
	state   SupervisorState
	entered time.Time
	hist    []SupRecord
}

type memStore struct {
	mu   sync.Mutex
	now  func() time.Time
	task map[string]*memTask
}

func (m *memStore) ensure(id string) *memTask {
	st := m.task[id]
	if st == nil {
		st = &memTask{state: SupPreparing, entered: m.now()}
		m.task[id] = st
	}
	return st
}

func (m *memStore) load(_ context.Context, taskID string) (SupervisorState, time.Time, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.task[taskID]
	if st == nil {
		return SupPreparing, time.Time{}, nil
	}
	return st.state, st.entered, nil
}

func (m *memStore) history(_ context.Context, taskID string) ([]SupRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.task[taskID]
	if st == nil {
		return nil, nil
	}
	out := make([]SupRecord, len(st.hist))
	copy(out, st.hist)
	return out, nil
}

func (m *memStore) mutate(_ context.Context, taskID string, decide func(SupervisorState) TransitionDecision) (TransitionDecision, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.ensure(taskID)
	dec := decide(st.state)
	dec.From = st.state
	if !dec.Accepted || dec.Noop {
		dec.To = st.state
		return dec, nil
	}
	if !dec.To.Valid() || !TransitionAllowed(st.state, dec.To) {
		dec.Accepted = false
		dec.To = st.state
		if dec.Reason == "" {
			dec.Reason = "不在转移表"
		}
		return dec, nil
	}
	rec := SupRecord{
		From: st.state, To: dec.To, Event: dec.Event, Actor: dec.Actor,
		Reason: dec.Reason, At: m.now(),
	}
	st.state = dec.To
	st.entered = rec.At
	st.hist = append(st.hist, rec)
	return dec, nil
}

// NewMemorySupervisor 返回只活在内存里的监督器。进程退出即丢，单测用。
func NewMemorySupervisor() *Supervisor {
	return newSupervisor(&memStore{now: time.Now, task: map[string]*memTask{}}, time.Now)
}

func newSupervisor(store supervisorStore, now func() time.Time) *Supervisor {
	if now == nil {
		now = time.Now
	}
	if ms, ok := store.(*memStore); ok && ms.now == nil {
		ms.now = now
	}
	return &Supervisor{store: store, now: now}
}

// Seed 把内存里的任务拨到指定状态，不写历史。库实现没有这个入口。
func (s *Supervisor) Seed(taskID string, state SupervisorState) error {
	ms, ok := s.store.(*memStore)
	if !ok {
		return fmt.Errorf("只有内存监督器可以 Seed")
	}
	if !state.Valid() {
		return fmt.Errorf("未知状态 %s", state)
	}
	ms.mu.Lock()
	defer ms.mu.Unlock()
	st := ms.ensure(taskID)
	st.state = state
	st.entered = ms.now()
	st.hist = nil
	return nil
}

// Current 返回当前状态。内存里没见过的任务按 PREPARING，与列默认值一致。
func (s *Supervisor) Current(ctx context.Context, taskID string) (SupervisorState, error) {
	if s == nil || s.store == nil {
		return "", fmt.Errorf("监督器未装配")
	}
	st, _, err := s.store.load(ctx, taskID)
	return st, err
}

// EnteredAt 返回进入当前状态的时间。没迁移过的内存任务是零值。
func (s *Supervisor) EnteredAt(ctx context.Context, taskID string) (time.Time, error) {
	if s == nil || s.store == nil {
		return time.Time{}, fmt.Errorf("监督器未装配")
	}
	_, at, err := s.store.load(ctx, taskID)
	return at, err
}

// History 按发生顺序返回迁移。没有迁移时是空切片。
func (s *Supervisor) History(ctx context.Context, taskID string) ([]SupRecord, error) {
	if s == nil || s.store == nil {
		return nil, fmt.Errorf("监督器未装配")
	}
	return s.store.history(ctx, taskID)
}

// Apply 处理一个确定性事件。非法迁移拒绝且状态不变。
func (s *Supervisor) Apply(ctx context.Context, taskID string, ev SupEvent) (SupervisorState, error) {
	goals := s.goals(ctx, taskID)
	dec, err := s.step(ctx, taskID, func(from SupervisorState) TransitionDecision {
		d := Decide(from, ev, goals)
		d.Actor = SupActorSystem
		return d
	})
	if err != nil {
		return "", err
	}
	return s.finish(ctx, taskID, dec)
}

// Propose 处理模型提议。被拒不返回 error，调用方把 Reason 交回模型即可。
func (s *Supervisor) Propose(ctx context.Context, taskID string, p ProposedTransition) (ProposeOutcome, error) {
	goals := s.goals(ctx, taskID)
	dec, err := s.step(ctx, taskID, func(from SupervisorState) TransitionDecision {
		return DecideProposal(from, p.To, p.Reason, goals)
	})
	if err != nil {
		return ProposeOutcome{}, err
	}
	if !dec.Accepted {
		s.fireReject(ctx, taskID, dec)
		return ProposeOutcome{State: dec.From, Accepted: false, Reason: dec.Reason}, nil
	}
	st, err := s.finish(ctx, taskID, dec)
	if err != nil {
		return ProposeOutcome{State: st, Accepted: true, Reason: dec.Reason}, err
	}
	return ProposeOutcome{State: st, Accepted: true, Reason: dec.Reason}, nil
}

func (s *Supervisor) step(ctx context.Context, taskID string, decide func(SupervisorState) TransitionDecision) (TransitionDecision, error) {
	if s == nil || s.store == nil {
		return TransitionDecision{}, fmt.Errorf("监督器未装配")
	}
	if strings.TrimSpace(taskID) == "" {
		return TransitionDecision{}, fmt.Errorf("缺少任务 id")
	}
	return s.store.mutate(ctx, taskID, decide)
}

func (s *Supervisor) finish(ctx context.Context, taskID string, dec TransitionDecision) (SupervisorState, error) {
	if !dec.Accepted {
		s.fireReject(ctx, taskID, dec)
		return dec.From, &SupRejectError{From: dec.From, To: dec.To, Reason: dec.Reason}
	}
	if dec.To.Terminal() && !dec.Noop && s.OnTerminal != nil {
		if err := s.OnTerminal(ctx, taskID, dec.To); err != nil {
			log.Printf("[supervisor] task %s 已进入 %s，同步任务终态失败: %v", taskID, dec.To, err)
			return dec.To, fmt.Errorf("监督器已进入 %s，同步任务终态失败: %w", dec.To, err)
		}
	}
	return dec.To, nil
}

func (s *Supervisor) fireReject(ctx context.Context, taskID string, dec TransitionDecision) {
	if s == nil || s.OnReject == nil {
		return
	}
	s.OnReject(ctx, taskID, dec)
}

func (s *Supervisor) goals(ctx context.Context, taskID string) bool {
	if s == nil || s.GoalsMet == nil {
		return false
	}
	ok, err := s.GoalsMet(ctx, taskID)
	if err != nil {
		log.Printf("[supervisor] task %s 读取目标闸门失败，按未通过处理: %v", taskID, err)
		return false
	}
	return ok
}

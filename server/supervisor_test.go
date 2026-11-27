package server

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSupervisorStateCountAndScope(t *testing.T) {
	if len(supAllStates) != 10 {
		t.Fatalf("状态应为 10 个，得到 %d", len(supAllStates))
	}
	seen := map[SupervisorState]bool{}
	for _, st := range supAllStates {
		if !st.Valid() {
			t.Fatalf("状态 %s 不在转移表", st)
		}
		if seen[st] {
			t.Fatalf("状态 %s 重复", st)
		}
		seen[st] = true
		if _, ok := SupTransitions[st]; !ok {
			t.Fatalf("转移表缺少 %s", st)
		}
	}
	if SupervisorScopeAllows(SupScopeTask) != true {
		t.Fatal("任务应在监督器范围内")
	}
	for _, scope := range []string{"", "session", "conversation", "single_loop", "legacy"} {
		if SupervisorScopeAllows(scope) {
			t.Fatalf("范围 %q 不应挂监督器", scope)
		}
	}
}

func TestSupervisorTransitionMatrix(t *testing.T) {
	ctx := context.Background()
	for _, from := range supAllStates {
		legal := map[SupervisorState]bool{}
		for _, to := range SupTransitions[from] {
			legal[to] = true
		}
		for _, to := range supAllStates {
			s := NewMemorySupervisor()
			s.GoalsMet = func(context.Context, string) (bool, error) { return true, nil }
			if err := s.Seed("t", from); err != nil {
				t.Fatal(err)
			}
			out, err := s.Propose(ctx, "t", ProposedTransition{To: to, Reason: "矩阵"})
			if err != nil {
				t.Fatalf("%s→%s 存储失败: %v", from, to, err)
			}
			got, gerr := s.Current(ctx, "t")
			if gerr != nil {
				t.Fatal(gerr)
			}
			if legal[to] {
				if !out.Accepted || got != to {
					t.Fatalf("%s→%s 应接受，结果 accepted=%v 状态=%s 原因=%s", from, to, out.Accepted, got, out.Reason)
				}
				hist, herr := s.History(ctx, "t")
				if herr != nil || len(hist) != 1 || hist[0].From != from || hist[0].To != to || hist[0].Actor != SupActorLLM {
					t.Fatalf("%s→%s 历史不符: %+v %v", from, to, hist, herr)
				}
				continue
			}
			if out.Accepted || got != from {
				t.Fatalf("%s→%s 应拒绝且状态不变，accepted=%v 状态=%s", from, to, out.Accepted, got)
			}
			hist, _ := s.History(ctx, "t")
			if len(hist) != 0 {
				t.Fatalf("%s→%s 被拒仍写了历史 %+v", from, to, hist)
			}
		}
	}
}

func TestSupervisorIllegalExamplesAndTerminals(t *testing.T) {
	ctx := context.Background()
	s := NewMemorySupervisor()
	if _, err := s.Apply(ctx, "t", SupEvent{Kind: SupEventGoalGate, Detail: "宣布完成"}); err == nil {
		t.Fatal("PREPARING→完成应被拒")
	}
	if got, _ := s.Current(ctx, "t"); got != SupPreparing {
		t.Fatalf("非法迁移后应仍是 PREPARING，得到 %s", got)
	}
	var rej *SupRejectError
	_, err := s.Apply(ctx, "t", SupEvent{Kind: "not-an-event"})
	if !errors.As(err, &rej) {
		t.Fatalf("未知事件应返回 SupRejectError，得到 %v", err)
	}

	for _, term := range []SupervisorState{SupDone, SupFailed, SupAborted} {
		if err := s.Seed("term", term); err != nil {
			t.Fatal(err)
		}
		for _, kind := range []string{
			SupEventPlanRoundDone, SupEventIntentClaimed, SupEventIntentSettled,
			SupEventGoalGate, SupEventBudgetExhausted, SupEventHuman, SupEventTimeout,
		} {
			_, err := s.Apply(ctx, "term", SupEvent{Kind: kind, Detail: SupHumanResume})
			if err == nil {
				t.Fatalf("终态 %s 接受了 %s", term, kind)
			}
			got, _ := s.Current(ctx, "term")
			if got != term {
				t.Fatalf("终态 %s 收到 %s 后变成 %s", term, kind, got)
			}
		}
		out, err := s.Propose(ctx, "term", ProposedTransition{To: SupExecuting, Reason: "翻终态"})
		if err != nil || out.Accepted {
			t.Fatalf("终态 %s 的提议应拒绝且不报存储错: %+v %v", term, out, err)
		}
		if !strings.Contains(out.Reason, "终态") {
			t.Fatalf("终态拒绝原因应说明只进不出，得到 %s", out.Reason)
		}
	}
}

func TestSupervisorGoalGateAndProposal(t *testing.T) {
	ctx := context.Background()
	s := NewMemorySupervisor()
	if err := s.Seed("t", SupExecuting); err != nil {
		t.Fatal(err)
	}
	var rejects int
	s.OnReject = func(context.Context, string, TransitionDecision) { rejects++ }
	if _, err := s.Apply(ctx, "t", SupEvent{Kind: SupEventGoalGate, Detail: "我宣布任务完成"}); err == nil {
		t.Fatal("目标未达成时 goal_gate 应拒绝")
	}
	if got, _ := s.Current(ctx, "t"); got != SupExecuting {
		t.Fatalf("拒绝后应仍是 EXECUTING，得到 %s", got)
	}
	out, err := s.Propose(ctx, "t", ProposedTransition{To: SupDone, Reason: "我宣布任务完成"})
	if err != nil || out.Accepted || out.State != SupExecuting {
		t.Fatalf("未过闸门的 DONE 提议应拒绝: %+v %v", out, err)
	}
	if rejects < 2 {
		t.Fatalf("两次拒绝都应回调，得到 %d", rejects)
	}

	s.GoalsMet = func(context.Context, string) (bool, error) { return true, nil }
	got, err := s.Apply(ctx, "t", SupEvent{Kind: SupEventGoalGate})
	if err != nil || got != SupCompleting {
		t.Fatalf("过闸门后应进入 COMPLETING，得到 %s %v", got, err)
	}
	var terminal SupervisorState
	s.OnTerminal = func(_ context.Context, _ string, to SupervisorState) error {
		terminal = to
		return nil
	}
	out, err = s.Propose(ctx, "t", ProposedTransition{To: SupDone, Reason: "闸门通过"})
	if err != nil || !out.Accepted || out.State != SupDone || terminal != SupDone {
		t.Fatalf("COMPLETING→DONE 应接受并同步终态: %+v %v terminal=%s", out, err, terminal)
	}
}

func TestSupervisorTimeoutNeverDone(t *testing.T) {
	ctx := context.Background()
	for _, from := range supAllStates {
		s := NewMemorySupervisor()
		if err := s.Seed("t", from); err != nil {
			t.Fatal(err)
		}
		got, err := s.Apply(ctx, "t", SupEvent{Kind: SupEventTimeout})
		cur, _ := s.Current(ctx, "t")
		if cur == SupDone && from != SupDone {
			t.Fatalf("超时把 %s 送进了 DONE", from)
		}
		if got == SupDone && from != SupDone {
			t.Fatalf("超时返回了 DONE（从 %s）", from)
		}
		switch from {
		case SupDone, SupFailed, SupAborted, SupPaused:
			if err == nil || cur != from {
				t.Fatalf("%s 的超时应拒绝且状态不变，得到 %s %v", from, cur, err)
			}
		case SupPreparing, SupVerifying, SupCompleting:
			if err != nil || cur != SupFailed {
				t.Fatalf("%s 超时应进入 FAILED，得到 %s %v", from, cur, err)
			}
		case SupPlanning, SupExecuting, SupWaiting:
			if err != nil || cur != SupPaused {
				t.Fatalf("%s 超时应进入 PAUSED，得到 %s %v", from, cur, err)
			}
		}
		if _, ok := MaxStay(from); from.Terminal() || from == SupPaused {
			if ok {
				t.Fatalf("%s 不应有自动超时", from)
			}
		} else if d, ok := MaxStay(from); !ok || d <= 0 {
			t.Fatalf("%s 应有正的 max_stay", from)
		}
	}
}

func TestSupervisorPlanClaimChain(t *testing.T) {
	ctx := context.Background()
	s := NewMemorySupervisor()
	now := time.Unix(1_700_000_000, 0)
	s.now = func() time.Time { return now }
	if ms, ok := s.store.(*memStore); ok {
		ms.now = s.now
	}
	got, err := s.Apply(ctx, "task-1", SupEvent{Kind: SupEventPlanRoundDone, Detail: "一轮规划"})
	if err != nil || got != SupPlanning {
		t.Fatalf("第一轮应进入 PLANNING，得到 %s %v", got, err)
	}
	zero := 1
	got, err = s.Apply(ctx, "task-1", SupEvent{Kind: SupEventIntentClaimed, IntentID: 7, IntentsLeft: &zero})
	if err != nil || got != SupExecuting {
		t.Fatalf("认领应进入 EXECUTING，得到 %s %v", got, err)
	}
	hist, err := s.History(ctx, "task-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) != 2 || hist[0].From != SupPreparing || hist[0].To != SupPlanning || hist[1].From != SupPlanning || hist[1].To != SupExecuting {
		t.Fatalf("迁移链不符: %+v", hist)
	}
	if hist[0].Actor != SupActorSystem || hist[0].Reason != "一轮规划" {
		t.Fatalf("历史操作者或理由不符: %+v", hist[0])
	}
	before := len(hist)
	got, err = s.Apply(ctx, "task-1", SupEvent{Kind: SupEventIntentClaimed})
	if err != nil || got != SupExecuting {
		t.Fatalf("执行中再次认领应是空操作，得到 %s %v", got, err)
	}
	hist, _ = s.History(ctx, "task-1")
	if len(hist) != before {
		t.Fatalf("空操作不应追加历史，得到 %d 条", len(hist))
	}
}

func TestSupervisorHumanAndBudget(t *testing.T) {
	ctx := context.Background()
	s := NewMemorySupervisor()
	if err := s.Seed("t", SupExecuting); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Apply(ctx, "t", SupEvent{Kind: SupEventHuman}); err == nil {
		t.Fatal("没有动作的人工指令应拒绝")
	}
	got, err := s.Apply(ctx, "t", SupEvent{Kind: SupEventHuman, Detail: SupHumanPause})
	if err != nil || got != SupPaused {
		t.Fatalf("人工暂停应进入 PAUSED，得到 %s %v", got, err)
	}
	got, err = s.Apply(ctx, "t", SupEvent{Kind: SupEventHuman, Detail: SupHumanResume})
	if err != nil || got != SupExecuting {
		t.Fatalf("人工恢复应进入 EXECUTING，得到 %s %v", got, err)
	}
	got, err = s.Apply(ctx, "t", SupEvent{Kind: SupEventBudgetExhausted})
	if err != nil || got != SupPaused {
		t.Fatalf("执行中预算耗尽应暂停，得到 %s %v", got, err)
	}
	got, err = s.Apply(ctx, "t", SupEvent{Kind: SupEventHuman, Detail: SupHumanAbort})
	if err != nil || got != SupAborted {
		t.Fatalf("暂停中人工中止应进入 ABORTED，得到 %s %v", got, err)
	}
}

func TestSupervisorRejectDoesNotRace(t *testing.T) {
	ctx := context.Background()
	s := NewMemorySupervisor()
	var n int
	var mu sync.Mutex
	s.OnReject = func(context.Context, string, TransitionDecision) {
		mu.Lock()
		n++
		mu.Unlock()
	}
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = s.Apply(ctx, "t", SupEvent{Kind: SupEventGoalGate})
		}()
	}
	wg.Wait()
	got, err := s.Current(ctx, "t")
	if err != nil || got != SupPreparing {
		t.Fatalf("并发拒绝后应仍是 PREPARING，得到 %s %v", got, err)
	}
	if n != 32 {
		t.Fatalf("应有 32 次拒绝回调，得到 %d", n)
	}
}

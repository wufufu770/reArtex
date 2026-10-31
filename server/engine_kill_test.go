package server

import (
	"context"
	"testing"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/llm"
)

// killTestTask 建一个跑得起来的任务（走 manager 的正式创建路径：PG 里有行、内存里
// 有句柄），用例结束时删掉。
func killTestTask(t *testing.T, s *Server, desc string) *Task {
	t.Helper()
	task, err := s.m.CreateTask(desc, "拿下目标", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = s.m.DeleteTask(task.ID, DeleteTaskOptions{}) })
	return task
}

// killTestIntent 建一条 running 的意图：pool 只认领 open 的意图，所以 running 的
// 意图不会被后台 worker 抢走。
func killTestIntent(t *testing.T, task *Task, summary string) *db.Node {
	t.Helper()
	id, err := task.Store.AddNode(db.KindIntent, map[string]any{"summary": summary}, 5, "running", "human", nil)
	if err != nil {
		t.Fatal(err)
	}
	node, err := task.Store.GetNode(id)
	if err != nil || node == nil {
		t.Fatalf("get intent: %v", err)
	}
	return node
}

// waitForWorkRegistered 等这条 work 在引擎里注册上来（runIntent 是异步的）。
func waitForWorkRegistered(t *testing.T, e *Engine, intentID int64) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		e.workMu.Lock()
		run := e.work[intentID]
		e.workMu.Unlock()
		if run != nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("意图 #%d 的 work 没有注册上来", intentID)
}

// hasQueuedSteer 报告这条意图的即时指令队列里有没有待交付的消息（终止前的收尾
// 通知走的就是这条通道）。
func hasQueuedSteer(e *Engine, intentID int64) bool {
	e.steerMu.Lock()
	defer e.steerMu.Unlock()
	return len(e.steerBox[intentID]) > 0
}

// TestKillWorkInjectsSettleBeforeTearingDown：终止一条 work 时，收尾通知必须在
// work 真正结束【之前】注入，意图最终落到 stopped（而不是丢在 running）。
func TestKillWorkInjectsSettleBeforeTearingDown(t *testing.T) {
	s, _ := taskHallTestServer(t)
	task := killTestTask(t, s, "终止前收尾测试")
	// 任务置终态：终态任务不重开被终止的意图（与额度阻塞重开同一条边界），于是
	// 这条意图应当停在 stopped。
	if err := s.m.SetTaskStatus(task.ID, "done"); err != nil {
		t.Fatal(err)
	}
	e := s.engine
	// 宽限期调短：生产是 KillSettleGrace（30s），测试等不起。
	e.killSettleGrace = 150 * time.Millisecond

	intent := killTestIntent(t, task, "收尾测试")
	// 假 worker：模型请求一直挂着，直到上下文被取消 —— 它因此永远停在「还没收场」
	// 的状态，收尾通知只能靠队列观测（它还没走到任何工具调用边界）。
	worker := agent.NewWorker(retestProvider{complete: func(ctx context.Context, _ llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		<-ctx.Done()
		return llm.Message{}, "", llm.Usage{}, ctx.Err()
	}}, "test", s.m.dir, nil, 10000, 1)
	worker.SetNonStreaming(func() bool { return true })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan bool, 1)
	go func() { finished <- e.runIntent(ctx, task, "worker-1", worker, intent, "", "") }()

	waitForWorkRegistered(t, e, intent.ID)
	if err := e.KillWork(intent.ID); err != nil {
		t.Fatal(err)
	}
	// 断言 1：work 还没被拆掉（宽限期存在，不是立刻硬取消），且收尾通知已经注入。
	e.workMu.Lock()
	run := e.work[intent.ID]
	e.workMu.Unlock()
	if run == nil {
		t.Fatal("终止后 work 立刻被拆掉了，收尾宽限期没有生效")
	}
	select {
	case <-run.finished:
		t.Fatal("终止立刻打断了 worker，收尾宽限期没有生效")
	default:
	}
	if !hasQueuedSteer(e, intent.ID) {
		t.Fatal("终止前没有注入收尾通知")
	}
	select {
	case <-finished:
	case <-time.After(20 * time.Second):
		t.Fatal("work 没有收场")
	}
	// 断言 2：意图落到 stopped（不是丢在 running）。
	node, err := task.Store.GetNode(intent.ID)
	if err != nil || node == nil {
		t.Fatalf("get intent: %v", err)
	}
	if node.State != "stopped" {
		t.Fatalf("intent state=%q, want stopped", node.State)
	}
}

// TestStoppedIntentIsReopenedWhenTaskStillRunning：任务还活着时，被终止的意图回到
// open（可被重新认领）；任务已终态时不重开（那里没有 worker 会跑）。
func TestStoppedIntentIsReopenedWhenTaskStillRunning(t *testing.T) {
	s, _ := taskHallTestServer(t)
	task := killTestTask(t, s, "终止后重开测试")
	e := s.engine
	intent := killTestIntent(t, task, "重开测试")
	if err := task.Store.SetIntentState(intent.ID, "stopped"); err != nil {
		t.Fatal(err)
	}

	// 任务还活着 ⇒ 重开
	if !e.reopenStoppedIntent(task, intent.ID) {
		t.Fatal("任务未终态时被终止的意图没有被重开")
	}
	node, err := task.Store.GetNode(intent.ID)
	if err != nil || node == nil {
		t.Fatalf("get intent: %v", err)
	}
	if node.State != "open" {
		t.Fatalf("intent state=%q, want open", node.State)
	}

	// 任务已终态 ⇒ 不重开
	if err := task.Store.SetIntentState(intent.ID, "stopped"); err != nil {
		t.Fatal(err)
	}
	if err := s.m.SetTaskStatus(task.ID, "done"); err != nil {
		t.Fatal(err)
	}
	if e.reopenStoppedIntent(task, intent.ID) {
		t.Fatal("终态任务下不应重开意图")
	}
	node, err = task.Store.GetNode(intent.ID)
	if err != nil || node == nil {
		t.Fatalf("get intent: %v", err)
	}
	if node.State != "stopped" {
		t.Fatalf("intent state=%q, want stopped", node.State)
	}
}

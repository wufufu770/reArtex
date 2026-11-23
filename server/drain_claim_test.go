package server

import (
	"context"
	"testing"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/llm"
)

// TestDrainedTaskStillClaimsDispatchedWork 钉住 0.15.54 软截止语义修正：
// drain 只停【规划面】（新意图不再产生），已派发未认领的工作照旧被认领自然
// 收尾——此前 runWorkerStep 里也拦认领，「停派新意图」实际做成了「全停」，
// 监督者一句软截止就把几十条已派发工作永久冻死（实测 task 5/6 被 drain 后
// 40 条已下发意图无人可领、任务整面静止）。完全冻结（含已派发工作）是
// 暂停（pause）的语义，两个开关各管各的。
func TestDrainedTaskStillClaimsDispatchedWork(t *testing.T) {
	s, _ := taskHallTestServer(t)
	task := killTestTask(t, s, "软截止认领语义")
	// 已派发待认领的工作（open 态才在 frontier 上——killTestIntent 建的是
	// running，不在认领面，这里直接建 open）。
	iid, err := task.Store.AddNode(db.KindIntent, map[string]any{"summary": "已派发待收尾的工作"}, 5, "open", "human", nil)
	if err != nil {
		t.Fatal(err)
	}
	s.engine.Drain(task.ID, true)
	if !s.engine.Drained(task.ID) {
		t.Fatal("用例前提：任务应处于软截止")
	}

	// 桩 worker：一拍即收（无工具调用），runIntent 正常收尾。
	worker := agent.NewWorker(retestProvider{complete: func(ctx context.Context, _ llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("done")}}, "end_turn", llm.Usage{}, nil
	}}, "test", s.m.dir, nil, 10000, 1)
	worker.SetNonStreaming(func() bool { return true })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if !s.engine.runWorkerStep(ctx, task, "worker-1", worker) {
		t.Fatal("软截止中的任务必须继续认领已派发工作（drain 只停规划面，不拦认领）")
	}
	node, err := task.Store.GetNode(iid)
	if err != nil || node == nil {
		t.Fatalf("取回意图失败: %v", err)
	}
	if node.State == "open" {
		t.Fatal("认领后意图不应还停在 open")
	}
}

// TestDrainCompleteNotifiedOnce 钉住 0.15.55 收尾播报：软截止的完成判据是
// 【意图清零】——清零播报一次、有在途不播、软截止翻转后重新计。
func TestDrainCompleteNotifiedOnce(t *testing.T) {
	s, _ := taskHallTestServer(t)
	task := killTestTask(t, s, "软截止收尾播报")
	countNotified := func() int {
		var n int
		if err := s.m.pg.QueryRow(`SELECT count(*) FROM activity WHERE exploration_id=$1 AND summary LIKE '%软截止收尾完成%'`,
			task.ExpID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	iid, err := task.Store.AddNode(db.KindIntent, map[string]any{"summary": "在途工作"}, 5, "open", "human", nil)
	if err != nil {
		t.Fatal(err)
	}
	s.engine.Drain(task.ID, true)
	s.engine.notifyDrainComplete(task)
	if countNotified() != 0 {
		t.Fatal("有在途意图时不得播报收尾完成")
	}
	if err := transitionIntentState(task.Store, iid, "open", "stopped"); err != nil {
		t.Fatal(err)
	}
	s.engine.notifyDrainComplete(task)
	if countNotified() != 1 {
		t.Fatalf("意图清零应播报一次, got %d", countNotified())
	}
	s.engine.notifyDrainComplete(task)
	if countNotified() != 1 {
		t.Fatal("重复调用不得重复播报")
	}
	s.engine.Drain(task.ID, false)
	s.engine.Drain(task.ID, true)
	s.engine.notifyDrainComplete(task)
	if countNotified() != 2 {
		t.Fatalf("软截止翻转后应重新计、可再播报, got %d", countNotified())
	}
}

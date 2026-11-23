package server

import (
	"strings"
	"testing"
)

// TestConcludeTaskRefusesWithOpenGoal 钉住 0.15.51 门5：open goal 未闭合时
// task_conclude 不得直接收尾——人工/规划立的收口条件不由 agent 的「实质达成」
// 判定静默绕过（实测两起过早收尾：task 2 被 goal 达成误判收尾、task 4 在
// goal 55 仍 open 时被监督者以三门满足直接 conclude 且规划者还在派新意图）。
//
// 用例把产出门（facts>0）与意图门（无 running/open）都置为满足——若无门5，
// ConcludeTask 会直接收尾；门5 必须把这条路拦成「需人工确认」。
func TestConcludeTaskRefusesWithOpenGoal(t *testing.T) {
	s, _ := taskHallTestServer(t)
	task := drainTestTask(t, s, "conclude-goal-gate")
	if _, err := task.Store.AddNode("fact", map[string]any{"text": "产出物"}, 0, "confirmed", "human", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := task.Store.AddNode("goal", map[string]any{"text": "高价值收口条件（未闭合）"}, 0, "open", "human", nil); err != nil {
		t.Fatal(err)
	}
	ok, msg := s.engine.ConcludeTask(task.ID, "test conclude")
	if ok {
		t.Fatal("存在 open goal 时 task_conclude 不得直接收尾（应需人工确认）")
	}
	if !strings.Contains(msg, "第5门") {
		t.Fatalf("拒绝理由应报第5门，got: %s", msg)
	}
}

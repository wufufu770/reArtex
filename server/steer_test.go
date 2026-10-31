package server

import (
	"fmt"
	"strings"
	"testing"
)

// TestPersistSteerSurvivesEngineRestart：纠偏消息落库后，【换一个完全不共享内存
// 状态】的 Engine 实例仍能读回来 —— 这才是「跨进程不丢」，而不是缓存没丢。
func TestPersistSteerSurvivesEngineRestart(t *testing.T) {
	s, _ := taskHallTestServer(t)
	task := killTestTask(t, s, "纠偏持久化测试")
	intent := killTestIntent(t, task, "纠偏持久化")
	msg := "别再扫 /admin 了，先把已经确认的结论写回"

	if err := s.engine.persistSteer(task, intent.ID, msg); err != nil {
		t.Fatal(err)
	}
	// 「重启」：新 Engine 实例，不共享任何内存状态。
	restarted := NewEngine(s.m)
	got, err := restarted.LoadPendingSteers(task, intent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != msg {
		t.Fatalf("pending steers=%v, want [%q]", got, msg)
	}
	// 别的意图读不到这条纠偏。
	other, err := restarted.LoadPendingSteers(task, intent.ID+1)
	if err != nil {
		t.Fatal(err)
	}
	if len(other) != 0 {
		t.Fatalf("纠偏泄漏到别的意图: %v", other)
	}
	// 交付过的副本标为已消费 ⇒ 不再重复投递。
	if err := restarted.consumeSteer(task, intent.ID, msg); err != nil {
		t.Fatal(err)
	}
	got, err = restarted.LoadPendingSteers(task, intent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("已交付的纠偏被重复投递: %v", got)
	}
}

// TestPersistSteerRespectsLimits：纠偏消息的长度上限与待投递条数上限都必须照旧
// 生效（内存队列与落库副本同口径）。
func TestPersistSteerRespectsLimits(t *testing.T) {
	s, _ := taskHallTestServer(t)
	task := killTestTask(t, s, "纠偏上限测试")
	intent := killTestIntent(t, task, "纠偏上限")
	e := s.engine

	if err := e.persistSteer(task, intent.ID, strings.Repeat("字", steerMsgMaxRunes+1)); err == nil {
		t.Fatal("超长纠偏没有被拒绝")
	}
	for i := 0; i < steerQueueMax; i++ {
		if err := e.persistSteer(task, intent.ID, fmt.Sprintf("纠偏 %d", i)); err != nil {
			t.Fatalf("第 %d 条纠偏被拒: %v", i, err)
		}
	}
	if err := e.persistSteer(task, intent.ID, "第 11 条"); err == nil {
		t.Fatal("待投递纠偏的条数上限没有生效")
	}
}

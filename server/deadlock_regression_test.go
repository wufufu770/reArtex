package server

import (
	"testing"
	"time"

	"github.com/Autumn-27/artex/agent"
)

// 死锁回归测试（0.15.40，交接文档实锤 bug 的钉子）：
// recentConclusion 曾在函数体内 Lock stagnationMu，而唯一调用方
// noteIntentWriteBack 持同一把锁调用——不可重入互斥锁重入 = 永久死锁。
// 任一任务连续 6 个零产出意图即引爆（Engine 单例锁 ⇒ 全平台 planner/worker
// 阻塞，远程部署 11-20 实锤）。本测试在**持锁的同一 goroutine** 里调用
// Locked 形态（重入即阻塞，带超时断言）。
func TestRecentConclusionLockedNoReentry(t *testing.T) {
	e := &Engine{lastConclusionAt: map[string]time.Time{}}
	tk := &Task{ID: "1"}

	done := make(chan bool, 1)
	go func() {
		// 同 goroutine 持锁后调用 Locked 形态——修复前（函数内再 Lock）
		// 这里永久阻塞；修复后（纯读）立即返回。
		e.stagnationMu.Lock()
		defer e.stagnationMu.Unlock()
		done <- e.recentConclusionLocked(tk)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("持锁调用 recentConclusionLocked 阻塞 2s——互斥锁重入回归！")
	}
}

// 全链形态：noteIntentWriteBack 零产出达到阈值时内部走 recentConclusionLocked
//（修复前此处死锁）。用最小 Engine 验证整条路径（不依赖 Manager——
// noteIntentWriteBack/recentConclusionLocked 都只碰 stagnation 状态）。
func TestNoteIntentWriteBackZeroStreakNoDeadlock(t *testing.T) {
	e := &Engine{lastConclusionAt: map[string]time.Time{}}
	tk := &Task{ID: "1"}

	done := make(chan struct{}, 1)
	go func() {
		defer close(done)
		defer func() { recover() }() // 第 7 连击触达 emitActivity（最小 Engine 无
		// broadcaster 会 nil-deref）——本测试只断言【不阻塞】：死锁 vs panic，
		// 回归目标是不死锁；panic 即路径走到了（未阻塞）。
		// 7 连零产出（阈值 stagnationThreshold=6）：第 6 次起进入
		// recentConclusionLocked 分支——修复前这里死锁。
		for i := 0; i < 7; i++ {
			e.noteIntentWriteBack(tk, agent.WriteCounts{})
		}
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("noteIntentWriteBack 零产出连击阻塞 3s——死锁回归！")
	}
}

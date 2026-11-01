// 本文件钉住「curator 挂在 scheduler 的 tick 上」这条接线（计划 Task 5 Step 5）。
//
// 断言钉的是【承诺】：
//   - 到了该跑的时候，tick 真的会把 curator 跑起来（水位被推进）；
//   - 还没到间隔（已跑过就别重复），tick 不会重复跑 curator；
//   - curator 的工作**不在 5 秒 tick 的同步路径上**（tick 不等它）。
//
// 【为什么单独一个文件】curator 的装配点在 scheduler.go，而 scheduler.go 由
// 别的单元并行持有 ⇒ 本文件只放用例，不碰它的实现。
package server

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/internal/sod"
)

// curatorTickResetSoDSlot 清掉进程级 SoD 链单例与执行器凭证表。
//
// 为什么需要复位：server.New() 会装 SoD 链，而链是**进程级单例**（只允许装
// 一次；装不上时 server.New 走 log.Fatalf ⇒ 整个测试二进制当场退出）。同包
// 其它用例建过 server 时，本文件再建一个就会撞上 ⇒ 建之前先复位。
//
// 为什么自己写而不复用别的测试文件里的同名助手：那些文件由别的单元并行
// 持有，本文件的用例不该挂在别人的私有助手上。
func curatorTickResetSoDSlot(t *testing.T) {
	t.Helper()
	reset := func() {
		sodChainSlot.mu.Lock()
		sodChainSlot.chain = nil
		sodChainSlot.mu.Unlock()
		sod.ResetExecutorTokensForTest()
	}
	reset()
	t.Cleanup(reset)
}

// curatorTickScheduler 起一个带调度器的服务。
//
// 为什么用 newScheduler 而不是自己拼一个 Scheduler：装配点（sc.s / sc.pg）就是
// 生产里那一份，用它才测得到「生产路径上 curator 会不会被跑起来」。
func curatorTickScheduler(t *testing.T) (*Server, *db.DB, *Scheduler) {
	t.Helper()
	curatorTickResetSoDSlot(t)
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	t.Cleanup(func() { m.Close() })
	td := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	s := New(ctx, m, td, td, td)
	return s, m.pg, newScheduler(s)
}

// curatorTickWatermark 读 curator 的水位（unix 秒）。
func curatorTickWatermark(t *testing.T, pg *db.DB) int64 {
	t.Helper()
	raw, err := pg.GetSchedState(SchedKeyStrategyCurator)
	if err != nil {
		t.Fatalf("读 curator 水位: %v", err)
	}
	v, perr := strconv.ParseInt(raw, 10, 64)
	if perr != nil {
		return 0
	}
	return v
}

// TestStrategyCuratorTickRunsWhenDue 钉住「curator 真的在 tick 上跑」。
//
// 场景：水位停在两小时前（= 已过一个间隔）⇒ 该跑。
// 断言：tick 之后水位被推进到「现在」。
//
// 接线被摘掉时：水位永远不动 ⇒ 断言挂。
func TestStrategyCuratorTickRunsWhenDue(t *testing.T) {
	_, pg, sc := curatorTickScheduler(t)

	old := time.Now().Add(-2 * CuratorInterval).Unix()
	if err := pg.SetSchedState(SchedKeyStrategyCurator, strconv.FormatInt(old, 10)); err != nil {
		t.Fatal(err)
	}
	sc.step()

	// curator 是异步跑的 ⇒ 等它把水位写回来。
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if got := curatorTickWatermark(t, pg); got > old {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("curator 没有跑：水位仍停在 %d", old)
}

// TestStrategyCuratorTickSkipsWithinInterval 钉住「已跑过就别重复」。
//
// 场景：水位停在一分钟前（= 还没到一个小时的间隔）⇒ 不该跑。
// 断言：tick 之后水位原封不动。
//
// 频率闸门被摘掉时：curator 每 5 秒跑一次全库淘汰 ⇒ 水位会被推进 ⇒ 断言挂。
func TestStrategyCuratorTickSkipsWithinInterval(t *testing.T) {
	_, pg, sc := curatorTickScheduler(t)

	recent := time.Now().Add(-1 * time.Minute).Unix()
	if err := pg.SetSchedState(SchedKeyStrategyCurator, strconv.FormatInt(recent, 10)); err != nil {
		t.Fatal(err)
	}
	sc.step()
	time.Sleep(500 * time.Millisecond)

	if got := curatorTickWatermark(t, pg); got != recent {
		t.Fatalf("curator 在间隔内被跑起来了（水位从 %d 变成 %d）", recent, got)
	}
}

// TestStrategyCuratorTickDoesNotBlockTheTick 钉住「curator 不在 tick 的同步路径上」。
//
// 场景：用一个未提交的事务占住 strategy_utility 的读锁 ⇒ curator 的
// REFRESH MATERIALIZED VIEW 会被数据库挡住（它要 ACCESS EXCLUSIVE 锁）。
// 断言：tick 立刻返回（不等 curator 跑完），而 curator 还在跑。
//
// curator 被改成同步跑时：tick 会一直等到锁释放 ⇒ 用例超时失败。
func TestStrategyCuratorTickDoesNotBlockTheTick(t *testing.T) {
	_, pg, sc := curatorTickScheduler(t)

	// 先把 schema 与物化视图准备好（否则占锁的事务读不到视图）。
	if err := pg.RefreshStrategyUtility(); err != nil {
		t.Skipf("utility 物化视图不可用（%v）— 跳过", err)
	}
	tx, err := pg.Begin()
	if err != nil {
		t.Fatalf("开事务: %v", err)
	}
	defer tx.Rollback()
	var rows int
	if err := tx.QueryRow(`SELECT count(*) FROM strategy_utility`).Scan(&rows); err != nil {
		t.Skipf("读 strategy_utility 失败（%v）— 跳过", err)
	}

	old := time.Now().Add(-2 * CuratorInterval).Unix()
	if err := pg.SetSchedState(SchedKeyStrategyCurator, strconv.FormatInt(old, 10)); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	sc.step()
	elapsed := time.Since(start)
	if elapsed > 5*time.Second {
		t.Fatalf("tick 被 curator 阻塞了 %v：curator 不该在 tick 的同步路径上", elapsed)
	}
	if !sc.curatorRunning.Load() {
		t.Fatalf("curator 没有在跑：它被摘掉了，或被放到了 tick 的同步路径上")
	}

	// 放开锁 ⇒ curator 跑完。
	tx.Rollback()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if !sc.curatorRunning.Load() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("curator 没有跑完")
}

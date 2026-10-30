package server

import (
	"fmt"
	"sync"
	"testing"

	"github.com/Autumn-27/artex/db"
)

// ---------- 组内并发闸门（任务大厅）----------
//
// 这一组用例盯住认领点上的三件事：
//  1. 群组并发上限 N 到顶 ⇒ 新的认领被拒（但已认领的照跑）；
//  2. 调低上限**不杀在途**（只拦新的认领）；
//  3. 上限判定是原子的：并发认领不会都越过上限。
//
// 夹具沿用 chat_group_test.go 的 taskHallTestServer：它的 ctx 可取消 ⇒ server 的
// 后台循环随用例结束，不会跨用例偷共享库里的行。

// concurrencyTestTask 建一个带探索图的任务（认领路径要 store）。
func concurrencyTestTask(t *testing.T, s *Server, pg *db.DB, desc string) (*Task, int64) {
	t.Helper()
	pt, err := pg.CreateTask(desc, "拿下目标", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pg.DeleteTask(pt.ID) })
	return taskFromPG(pt, pg.Exploration(pt.ExplorationID), s.m.interceptor), pt.ID
}

// concurrencyTestGroup 建一个大厅：auto 当分发者（与发布任务同一形状）。
func concurrencyTestGroup(t *testing.T, pg *db.DB, taskID int64, limit int) int64 {
	t.Helper()
	hall, err := pg.CreateConversation("auto", "并发测试大厅", nil)
	if err != nil {
		t.Fatal(err)
	}
	gid, err := pg.CreateChatGroup(&db.ChatGroup{
		ConversationID:   hall.ID,
		TaskID:           taskID,
		Name:             "并发测试群",
		Status:           "active",
		ConcurrencyLimit: limit,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := pg.AddChatGroupMember(gid, "auto", &hall.ID, "dispatcher"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pg.DeleteChatGroup(gid) })
	return gid
}

// concurrencyTestIntent 写一条 open 意图。
func concurrencyTestIntent(t *testing.T, task *Task, text string) int64 {
	t.Helper()
	id, err := task.Store.AddIntent(map[string]any{"text": text}, 0, nil, "planner")
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// TestGroupConcurrencyCapBlocksExtraClaim：上限=1 时，第二次认领必须被拒；前一条
// 结束后又能领。
func TestGroupConcurrencyCapBlocksExtraClaim(t *testing.T) {
	s, pg := taskHallTestServer(t)
	task, taskID := concurrencyTestTask(t, s, pg, "组内并发上限")
	concurrencyTestGroup(t, pg, taskID, 1)
	first := concurrencyTestIntent(t, task, "第一条意图")
	second := concurrencyTestIntent(t, task, "第二条意图")

	claimed := s.engine.claimNext(task, "work#1")
	if claimed == nil {
		t.Fatalf("第一次认领应当成功")
	}
	if claimed.ID != first {
		t.Fatalf("领到的意图 = %d，期望 %d", claimed.ID, first)
	}

	// 上限已到（running=1，limit=1）⇒ 第二次认领必须被拒。
	if got := s.engine.claimNext(task, "work#2"); got != nil {
		t.Fatalf("组内并发已到上限，第二次认领应被拒，实际领到 #%d", got.ID)
	}

	// 第一条跑完 ⇒ 又能领第二条。
	if err := task.Store.SetIntentState(first, "done"); err != nil {
		t.Fatal(err)
	}
	got := s.engine.claimNext(task, "work#2")
	if got == nil {
		t.Fatalf("前一条结束后应当能再领一条")
	}
	if got.ID != second {
		t.Fatalf("第二次领到的意图 = %d，期望 %d", got.ID, second)
	}
}

// TestGroupConcurrencyCapLoweringLimitKeepsInFlightWork：调低上限**不杀在途**。
//
// 上限只拦新的认领：在途的意图状态与归属不受影响，新认领被拒，直到在跑的降到
// 上限之下。
func TestGroupConcurrencyCapLoweringLimitKeepsInFlightWork(t *testing.T) {
	s, pg := taskHallTestServer(t)
	task, taskID := concurrencyTestTask(t, s, pg, "调低组内上限")
	concurrencyTestGroup(t, pg, taskID, 2)
	inFlight := concurrencyTestIntent(t, task, "在途意图")
	queued := concurrencyTestIntent(t, task, "排队意图")

	claimed := s.engine.claimNext(task, "work#1")
	if claimed == nil || claimed.ID != inFlight {
		t.Fatalf("第一次认领失败: %+v", claimed)
	}

	// 调低上限：1 < 当前 running 数（1）。
	if _, err := pg.Exec(`UPDATE chat_groups SET concurrency_limit=1 WHERE task_id=$1`, taskID); err != nil {
		t.Fatal(err)
	}

	// 在途的意图没被动过：还是 running，归属还是原来的 worker。
	node, err := task.Store.GetNode(inFlight)
	if err != nil {
		t.Fatal(err)
	}
	if node == nil || node.State != "running" || node.Owner != "work#1" {
		t.Fatalf("调低上限不应动在途意图: state=%q owner=%q", node.State, node.Owner)
	}

	// 新认领被拒（上限 1，running 1）。
	if got := s.engine.claimNext(task, "work#2"); got != nil {
		t.Fatalf("调低上限后新认领应被拒，实际领到 #%d", got.ID)
	}

	// 在途跑完后又能领。
	if err := task.Store.SetIntentState(inFlight, "done"); err != nil {
		t.Fatal(err)
	}
	got := s.engine.claimNext(task, "work#2")
	if got == nil || got.ID != queued {
		t.Fatalf("在途结束后应当能再领 #%d，实际 %+v", queued, got)
	}
}

// TestGroupConcurrencyCapConcurrentClaimsOnlyOneWins：上限判定是原子的。
//
// 四个 worker 同时抢同一个上限=1 的群，只能有一个成功 —— 计划书里"先 SELECT 再
// UPDATE"的写法在这里会挂（两个并发认领都能读到同一份旧计数）。
func TestGroupConcurrencyCapConcurrentClaimsOnlyOneWins(t *testing.T) {
	s, pg := taskHallTestServer(t)
	task, taskID := concurrencyTestTask(t, s, pg, "并发认领")
	concurrencyTestGroup(t, pg, taskID, 1)
	for i := 0; i < 4; i++ {
		concurrencyTestIntent(t, task, fmt.Sprintf("意图 %d", i))
	}

	const workers = 4
	results := make([]*db.Node, workers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i] = s.engine.claimNext(task, fmt.Sprintf("work#%d", i+1))
		}(i)
	}
	close(start)
	wg.Wait()

	won := 0
	for _, r := range results {
		if r != nil {
			won++
		}
	}
	if won != 1 {
		t.Fatalf("并发认领成功数 = %d，期望 1（上限=1）", won)
	}
	var running int
	if err := pg.QueryRow(`SELECT count(*) FROM exploration_nodes n
JOIN explorations e ON e.id=n.exploration_id
WHERE e.id=$1 AND n.kind='intent' AND n.state='running'`, task.ExpID).Scan(&running); err != nil {
		t.Fatal(err)
	}
	if running != 1 {
		t.Fatalf("running 的意图数 = %d，期望 1", running)
	}
}

// TestGroupQuotaParkedBlocksNewClaims：额度停掉（parked）⇒ 不开新活。
//
// 仲裁顺序里的额度闸门在认领点上生效：在途的照跑（这里没有在途），新的不认领。
func TestGroupQuotaParkedBlocksNewClaims(t *testing.T) {
	s, pg := taskHallTestServer(t)
	task, taskID := concurrencyTestTask(t, s, pg, "额度停掉")
	gid := concurrencyTestGroup(t, pg, taskID, 3)
	concurrencyTestIntent(t, task, "额度停掉时的意图")

	if err := pg.SetGroupQuotaBudget(gid, 1000); err != nil {
		t.Fatal(err)
	}
	if err := pg.SetGroupQuotaState(gid, db.QuotaStateParked); err != nil {
		t.Fatal(err)
	}
	if got := s.engine.claimNext(task, "work#1"); got != nil {
		t.Fatalf("额度 parked 时不应认领，实际领到 #%d", got.ID)
	}

	// 恢复 active ⇒ 又能领。
	if err := pg.SetGroupQuotaState(gid, db.QuotaStateActive); err != nil {
		t.Fatal(err)
	}
	if got := s.engine.claimNext(task, "work#1"); got == nil {
		t.Fatalf("额度恢复 active 后应当能认领")
	}
}

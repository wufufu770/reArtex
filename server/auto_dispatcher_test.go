package server

import (
	"fmt"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
)

// ---------- auto 的二重身份（分发者 + 兜底执行）----------
//
// auto 在大厅里有两个身份：
//   - **分发者**：它派工、看全局，是每个大厅的固定成员（role='dispatcher'）；
//   - **兜底执行者**：组里没有别的成员能干活时，它顶上执行，别让任务停摆。
//
// 两条身份不冲突的前提是"可用成员"的判定不含 auto 自己：把分发者算成可用成员，
// "有没有人可用"就永远为真，auto 就永远不会兜底。

// autoTestAgent 建一个自定义 agent 当群成员。
//
// 为什么不用内置 agent：内置 agent 是**同包所有用例共享**的一行，禁用它会漏到别的
// 用例（它们不测这件事）。自定义 agent 用完即删，影响面只有这个用例。
func autoTestAgent(t *testing.T, pg *db.DB) string {
	t.Helper()
	key := fmt.Sprintf("hall_member_%d", time.Now().UnixNano())
	if _, err := pg.CreateAgent(key, key, "群成员测试 agent"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pg.DeleteAgent(key) })
	return key
}

// autoTestDisableAgent 把一个 agent 置为 disabled（agents.enabled）。
//
// db 层只有读（GetAgentByKey 返回的 Agent.Enabled），没有"改 enabled"的访问器，
// 所以这里直接写表；用例收尾恢复原值。
func autoTestDisableAgent(t *testing.T, pg *db.DB, key string) {
	t.Helper()
	if _, err := pg.Exec(`UPDATE agents SET enabled=false WHERE key=$1`, key); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pg.Exec(`UPDATE agents SET enabled=true WHERE key=$1`, key) })
}

// TestGroupHasUsableMember：组里有没有"现在能干活"的成员（不含分发者 auto）。
func TestGroupHasUsableMember(t *testing.T) {
	s, pg := taskHallTestServer(t)
	_, taskID := concurrencyTestTask(t, s, pg, "可用成员判定")
	gid := concurrencyTestGroup(t, pg, taskID, 3)

	// 1) 只有分发者 auto ⇒ 没有可用成员（auto 是派工的，不是干活的）。
	usable, err := s.GroupHasUsableMember(gid)
	if err != nil {
		t.Fatal(err)
	}
	if usable {
		t.Fatalf("只有分发者的群不应有可用成员")
	}

	// 2) 加一个 enabled 的成员 ⇒ 有可用成员。
	member := autoTestAgent(t, pg)
	if err := pg.AddChatGroupMember(gid, member, nil, "member"); err != nil {
		t.Fatal(err)
	}
	usable, err = s.GroupHasUsableMember(gid)
	if err != nil {
		t.Fatal(err)
	}
	if !usable {
		t.Fatalf("加了 enabled 成员后应有可用成员")
	}

	// 3) 把成员禁用 ⇒ 又没有可用成员了。
	autoTestDisableAgent(t, pg, member)
	usable, err = s.GroupHasUsableMember(gid)
	if err != nil {
		t.Fatal(err)
	}
	if usable {
		t.Fatalf("成员被禁用后不应算可用成员")
	}

	// 4) 额度停掉（parked）⇒ 整组不开新活 ⇒ 没有可用成员。
	if _, err := pg.Exec(`UPDATE agents SET enabled=true WHERE key=$1`, member); err != nil {
		t.Fatal(err)
	}
	if err := pg.SetGroupQuotaBudget(gid, 1000); err != nil {
		t.Fatal(err)
	}
	if err := pg.SetGroupQuotaState(gid, db.QuotaStateParked); err != nil {
		t.Fatal(err)
	}
	usable, err = s.GroupHasUsableMember(gid)
	if err != nil {
		t.Fatal(err)
	}
	if usable {
		t.Fatalf("额度 parked 时不应有可用成员")
	}
}

// TestAutoDispatcherReleasedIntoClaimPathOnlyAsFallback：auto 只在"组里没人可用"
// 时才被放进认领路径。
func TestAutoDispatcherReleasedIntoClaimPathOnlyAsFallback(t *testing.T) {
	s, pg := taskHallTestServer(t)
	task, taskID := concurrencyTestTask(t, s, pg, "auto 兜底")
	gid := concurrencyTestGroup(t, pg, taskID, 3)
	member := autoTestAgent(t, pg)
	if err := pg.AddChatGroupMember(gid, member, nil, "member"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		concurrencyTestIntent(t, task, fmt.Sprintf("意图 %d", i))
	}

	// 有可用成员 ⇒ auto 让位（它是分发者），成员照常干活。
	if got := s.engine.claimNext(task, "auto"); got != nil {
		t.Fatalf("有可用成员时 auto 应当让位，实际领到 #%d", got.ID)
	}
	if got := s.engine.claimNext(task, member); got == nil {
		t.Fatalf("有可用成员时成员应当能认领")
	}

	// 成员被禁用 ⇒ 组里没人可用 ⇒ auto 顶上（兜底执行）。
	autoTestDisableAgent(t, pg, member)
	if got := s.engine.claimNext(task, "auto"); got == nil {
		t.Fatalf("没有可用成员时 auto 应当兜底认领")
	}

	// 被禁用的成员让位给 auto。
	if got := s.engine.claimNext(task, member); got != nil {
		t.Fatalf("没有可用成员时其它成员应让位给 auto，实际领到 #%d", got.ID)
	}
}

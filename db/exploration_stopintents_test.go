package db

import "testing"

// TestStopNonTerminalIntents 收尾清理（方案 v9 批次1⑦）：只有 open/blocked 被
// 置 stopped；已终态的 done/stopped 原样不动，计数只算清理条数。
func TestStopNonTerminalIntents(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()

	expID, err := d.CreateExploration("stop-intents 测试", "verify terminal cleanliness")
	if err != nil {
		t.Fatalf("create exploration: %v", err)
	}
	store := d.Exploration(expID)

	add := func(state string) int64 {
		t.Helper()
		id, err := store.AddNode("intent", map[string]any{"title": "t-" + state}, 5, state, "test", nil)
		if err != nil {
			t.Fatalf("add intent %s: %v", state, err)
		}
		return id
	}
	openID := add("open")
	blockedID := add("blocked")
	doneID := add("done")
	stoppedID := add("stopped")

	n, err := store.StopNonTerminalIntents("测试收尾：残留非终态意图统一 stopped")
	if err != nil {
		t.Fatalf("StopNonTerminalIntents: %v", err)
	}
	if n != 2 {
		t.Fatalf("want cleaned=2 (open+blocked), got %d", n)
	}

	stateOf := func(id int64) string {
		t.Helper()
		var state string
		if err := d.QueryRow(`SELECT state FROM exploration_nodes WHERE id=$1 AND exploration_id=$2`, id, expID).Scan(&state); err != nil {
			t.Fatalf("load node %d: %v", id, err)
		}
		return state
	}
	for id, want := range map[int64]string{openID: "stopped", blockedID: "stopped", doneID: "done", stoppedID: "stopped"} {
		if got := stateOf(id); got != want {
			t.Fatalf("intent #%d state = %s, want %s", id, got, want)
		}
	}

	// 幂等：再跑一次清理数为 0，状态不再变化。
	if n2, err := store.StopNonTerminalIntents("二次清理"); err != nil || n2 != 0 {
		t.Fatalf("second pass: n=%d err=%v, want 0/nil", n2, err)
	}
}

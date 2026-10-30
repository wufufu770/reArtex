package db

import (
	"testing"

	"github.com/google/uuid"
)

// insertUsage 写一行计量，并在用例结束后清掉（按唯一的 profile_name 定位，
// 免得踩到同包其它用例的数据）。
func insertUsage(t *testing.T, d *DB, u LLMUsage) {
	t.Helper()
	u.ProfileName = "usage-test-" + uuid.NewString()
	if err := d.InsertLLMUsage(&u); err != nil {
		t.Fatalf("insert: %v", err)
	}
	t.Cleanup(func() {
		_, _ = d.Exec(`DELETE FROM llm_usage WHERE profile_name=$1`, u.ProfileName)
	})
}

// 计量口径：任务的总量 = input_tokens + output_tokens。
//
// 缓存**已经**在 input_tokens 里：llmrec 落库的是 SDK 规范化后的 llm.Usage，
// 其 InputTokens 已含缓存（norma 把 cache_read + cache_write 折进去），平台自己
// 的 dashboard 也标「含缓存」。四类相加会把缓存前缀算两遍。
func TestTotalTokensForTaskIsInputPlusOutput(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()
	if err := d.EnsureLLMUsageTable(); err != nil {
		t.Fatal(err)
	}

	taskID := "usage-total-" + uuid.NewString()
	insertUsage(t, d, LLMUsage{TaskID: taskID, Model: "m1", InputTokens: 600, OutputTokens: 10, Status: "ok"})
	insertUsage(t, d, LLMUsage{TaskID: taskID, Model: "m1", InputTokens: 20, OutputTokens: 2, Status: "ok"})
	// 别的任务不得被算进来。
	insertUsage(t, d, LLMUsage{TaskID: "usage-total-other-" + uuid.NewString(), Model: "m1", InputTokens: 999, OutputTokens: 99, Status: "ok"})

	got, err := d.TotalTokensForTask(taskID)
	if err != nil {
		t.Fatalf("total: %v", err)
	}
	if got != 632 {
		t.Fatalf("TotalTokensForTask = %d, want 632 (600+10 + 20+2)", got)
	}
}

// 带缓存计数的行不得被重复计入总量。
//
// 这条锁的是「缓存是 input 的子集」这个口径：input_tokens 已经含缓存，
// 再把 cache_read / cache_write 加一遍就是双计。
func TestTotalTokensForTaskDoesNotDoubleCountCache(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()
	if err := d.EnsureLLMUsageTable(); err != nil {
		t.Fatal(err)
	}

	taskID := "usage-cache-" + uuid.NewString()
	insertUsage(t, d, LLMUsage{
		TaskID: taskID, Model: "m1",
		InputTokens: 600, OutputTokens: 10,
		CacheRead: 500, CacheWrite: 100,
		Status: "ok",
	})

	got, err := d.TotalTokensForTask(taskID)
	if err != nil {
		t.Fatalf("total: %v", err)
	}
	// 600+10 = 610；不是 600+10+500+100 = 1210。
	if got != 610 {
		t.Fatalf("TotalTokensForTask = %d, want 610 (缓存已在 input 内，不得再加一次)", got)
	}
}

// 孤儿计量行 = 既无 task_id 又无 exploration_id 的用量：今天就是平台对话的用量
// （llmrec.WithTaskID 只在 task runtime 与 side-question 路径注入）。
//
// 计数取「插入前后差值」，因为派生库里同包其它用例也可能留下孤儿行。
func TestCountOrphanLLMUsageCountsUnattributedRows(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()
	if err := d.EnsureLLMUsageTable(); err != nil {
		t.Fatal(err)
	}

	before, err := d.CountOrphanLLMUsage()
	if err != nil {
		t.Fatalf("orphan before: %v", err)
	}
	insertUsage(t, d, LLMUsage{Model: "m1", InputTokens: 5, OutputTokens: 1, Status: "ok"})
	insertUsage(t, d, LLMUsage{Model: "m1", InputTokens: 7, OutputTokens: 2, Status: "ok"})

	after, err := d.CountOrphanLLMUsage()
	if err != nil {
		t.Fatalf("orphan after: %v", err)
	}
	if after-before != 2 {
		t.Fatalf("孤儿计数增量 = %d, want 2", after-before)
	}
}

// 有归属的行不得被算成孤儿：有 task_id 的、以及只有 exploration_id 的，都算
// 「有归属」（它们至少能被某个任务/探索认领）。
func TestCountOrphanLLMUsageIgnoresAttributedRows(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()
	if err := d.EnsureLLMUsageTable(); err != nil {
		t.Fatal(err)
	}

	before, err := d.CountOrphanLLMUsage()
	if err != nil {
		t.Fatalf("orphan before: %v", err)
	}
	insertUsage(t, d, LLMUsage{TaskID: "usage-attributed-" + uuid.NewString(), Model: "m1", InputTokens: 11, OutputTokens: 3, Status: "ok"})
	insertUsage(t, d, LLMUsage{ExplorationID: 4242, Model: "m1", InputTokens: 13, OutputTokens: 5, Status: "ok"})

	after, err := d.CountOrphanLLMUsage()
	if err != nil {
		t.Fatalf("orphan after: %v", err)
	}
	if after != before {
		t.Fatalf("有归属的行被算成孤儿: %d → %d", before, after)
	}
}

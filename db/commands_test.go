package db

// 方案 v3 缺陷 G2 的库层回归：ListCommands 的 status 筛选（error/ok）必须同时
// 作用于 count 与数据查询 —— 两条查询的 JOIN 形状必须一致，否则筛选会 500
// （count 引用不了配对结果行 r）或总数与明细对不上。

import (
	"fmt"
	"testing"
	"time"
)

func TestListCommandsStatusFilter(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v)", err)
	}
	defer d.Close()

	// 造一个任务 + 探索图，写两对 tool_use/tool_result：一成一败。
	created, err := d.CreateTaskWithOptions("G2 状态筛选回归", "测试目标", TaskCreateOptions{})
	if err != nil {
		t.Fatalf("建任务失败: %v", err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(created.ID) })
	store := d.Exploration(created.ExplorationID)

	// 时间窗 + 唯一 tool_use_id：测试在共享库上可重复运行，不被上一轮残留污染。
	testStart := time.Now().Add(-time.Second)
	runTag := fmt.Sprintf("g2test-%d", time.Now().UnixNano())
	useID := func(tag string) string { return runTag + "-" + tag }
	if _, err := store.AppendActivity(Activity{Worker: "work#1", Kind: "tool_use", Tool: "Bash",
		ToolUseID: useID("ok"), Detail: `{"command":"echo ok"}`}); err != nil {
		t.Fatalf("写 tool_use(ok) 失败: %v", err)
	}
	if _, err := store.AppendActivity(Activity{Worker: "work#1", Kind: "tool_result", Tool: "Bash",
		ToolUseID: useID("ok"), Detail: "fine"}); err != nil {
		t.Fatalf("写 tool_result(ok) 失败: %v", err)
	}
	if _, err := store.AppendActivity(Activity{Worker: "work#1", Kind: "tool_use", Tool: "Bash",
		ToolUseID: useID("bad"), Detail: `{"command":"grep /etc/passwd"}`}); err != nil {
		t.Fatalf("写 tool_use(bad) 失败: %v", err)
	}
	if _, err := store.AppendActivity(Activity{Worker: "work#1", Kind: "tool_result", Tool: "Bash",
		ToolUseID: useID("bad"), IsError: true, Detail: "[SoD] out_of_scope"}); err != nil {
		t.Fatalf("写 tool_result(bad) 失败: %v", err)
	}

	id := created.ID
	// 全部：count 与明细一致（本测试自己的行）。
	all, total, err := d.ListCommands(&id, "", "", 0, 50)
	if err != nil {
		t.Fatalf("ListCommands(all): %v", err)
	}
	_ = all
	for _, r := range all {
		if r.CreatedAt.Before(testStart) {
			t.Fatalf("筛选应只含本任务的活动（发现更早行 #%d，created_at=%s）", r.ID, r.CreatedAt)
		}
	}
	if err != nil {
		t.Fatalf("ListCommands(all): %v", err)
	}
	if total < 2 {
		t.Fatalf("至少应有本次用例的 2 条 tool_use，total=%d", total)
	}
	// 失败筛选：count 不炸 + 明细全是失败行。
	errs, totalErr, err := d.ListCommands(&id, "", "error", 0, 50)
	if err != nil {
		t.Fatalf("ListCommands(error) —— count 查询的 JOIN 形状必须与数据查询一致: %v", err)
	}
	if totalErr != 1 {
		t.Fatalf("失败行应为 1，total=%d", totalErr)
	}
	if len(errs) != 1 || !errs[0].IsError || errs[0].Command != `{"command":"grep /etc/passwd"}` {
		t.Fatalf("失败明细不对: %+v", errs)
	}
	// 成功筛选。
	oks, totalOk, err := d.ListCommands(&id, "", "ok", 0, 50)
	if err != nil {
		t.Fatalf("ListCommands(ok): %v", err)
	}
	if totalOk < 1 {
		t.Fatalf("成功行至少 1，total=%d", totalOk)
	}
	for _, r := range oks {
		if r.IsError {
			t.Fatalf("ok 筛选混入失败行: %+v", r)
		}
	}
}

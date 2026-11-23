package server

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
)

// TestEdgeInitCellsAndDispatchAutoCreate 钉住 0.15.52 建格缺口修复：
//   ① edge_init_cells 建格幂等（重复调用不覆盖、不重复计数）；
//   ② edge_dispatch 对不存在的格点自动建格（此前直接报「先用 edge_activate
//      的 cells 参数初始化坐标系」——而 edge_activate 只绑 mainagent，
//      edge_exec 的工具箱里没有任何建格入口，坐标系永远建不起来，
//      实测 3 条偏锋运行全部 criterion_stalled 收场）；
//   ③ 尝试数记账照旧：同格点第 3 次下发触发封顶（2 次后强制封格）。
func TestEdgeInitCellsAndDispatchAutoCreate(t *testing.T) {
	s, _ := taskHallTestServer(t)
	task := drainTestTask(t, s, "edge 建格缺口修复")
	taskID, err := strconv.ParseInt(task.ID, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.m.pg.SetTaskEdgeMode(taskID, true, "reverse-negative"); err != nil {
		t.Fatal(err)
	}
	tid := strconv.FormatInt(taskID, 10)

	// ① 建格。
	res, err := s.toolEdgeInitCells().Call(context.Background(),
		json.RawMessage(`{"task_id":`+tid+`,"cells":["neg:1","neg:2"]}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("edge_init_cells 失败: %s", res.Flatten())
	}
	cells, err := s.m.pg.ListEdgeCells(taskID)
	if err != nil {
		t.Fatal(err)
	}
	if len(cells) != 2 {
		t.Fatalf("建格后应有 2 个格点, got %d", len(cells))
	}
	// 幂等：重复调用不新增。
	res, err = s.toolEdgeInitCells().Call(context.Background(),
		json.RawMessage(`{"task_id":`+tid+`,"cells":["neg:1","neg:2"]}`), nil)
	if err != nil || res.IsError {
		t.Fatalf("重复建格应幂等成功: %v %s", err, res.Flatten())
	}
	if cells, _ = s.m.pg.ListEdgeCells(taskID); len(cells) != 2 {
		t.Fatalf("重复建格后仍应 2 个格点, got %d", len(cells))
	}
	if !strings.Contains(res.Flatten(), "新建 0") {
		t.Fatalf("重复建格应报新建 0, got %s", res.Flatten())
	}

	// ② 对不存在的格点下发 ⇒ 自动建格 + attempts=1。
	dispatch := func(cell, summary string) (string, bool) {
		in := `{"task_id":` + tid + `,"cell_id":"` + cell + `","summary":"` + summary + `",` +
			`"observation_basis":"task 事实图既有观察","predicted_vulnclass":"info_disclosure",` +
			`"prediction":"可观察到 X","falsifier":"看不到 X 即证伪"}`
		r, e := s.toolEdgeDispatch().Call(context.Background(), json.RawMessage(in), nil)
		if e != nil {
			t.Fatal(e)
		}
		return r.Flatten(), !r.IsError
	}
	out, ok := dispatch("neg:auto", "自动建格假设 A")
	if !ok {
		t.Fatalf("下发到不存在的格点应自动建格而非报错: %s", out)
	}
	if !strings.Contains(out, "自动建格") {
		t.Fatalf("自动建格应回执标注, got %s", out)
	}
	cells, _ = s.m.pg.ListEdgeCells(taskID)
	var auto *struct {
		CellID   string
		Attempts int
		State    string
	}
	for _, c := range cells {
		if c.CellID == "neg:auto" {
			auto = &struct {
				CellID   string
				Attempts int
				State    string
			}{c.CellID, c.Attempts, c.State}
		}
	}
	if auto == nil {
		t.Fatal("下发后格点 neg:auto 应已存在（自动建格）")
	}
	if auto.Attempts != 1 {
		t.Fatalf("首次下发尝试数应为 1, got %d", auto.Attempts)
	}

	// ③ 尝试数记账：第 2 次放行、第 3 次触发封顶。
	if out, ok := dispatch("neg:auto", "自动建格假设 A 第二轮"); !ok {
		t.Fatalf("第 2 次下发应放行: %s", out)
	}
	out, ok = dispatch("neg:auto", "自动建格假设 A 第三轮")
	if ok && !strings.Contains(out, "已封") {
		t.Fatalf("第 3 次下发应触发封顶, got %s", out)
	}
}

package server

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/internal/edge"
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

	// 预测必须落 intent_predictions（0.15.53：F1 登记面此前漏接 dispatch 路径
	// ⇒ 兑现判定/矛盾对照拿不到预测）。
	if id, found, err := task.Store.FindOpenIntentBySummary("自动建格假设 A"); err != nil || !found {
		t.Fatalf("下发的假设应能按摘要找回: found=%v err=%v", found, err)
	} else if pred, perr := s.m.pg.GetIntentPrediction(id); perr != nil || pred == nil ||
		pred.PredictedVulnclass != "info_disclosure" {
		t.Fatalf("偏锋假设的预测必须登记进 intent_predictions: pred=%+v err=%v", pred, perr)
	}

	// ③ 尝试数记账与封顶（0.15.54 在飞门语义：复投前先把在飞假设置终态——
	// 意图转终态 + 格点回写，与生产收尾同形）。
	settleExhausted := func(summary string) {
		id, found, err := task.Store.FindOpenIntentBySummary(summary)
		if err != nil || !found {
			t.Fatalf("找回在飞假设 %q 失败: found=%v err=%v", summary, found, err)
		}
		if err := transitionIntentState(task.Store, id, "open", "exhausted"); err != nil {
			t.Fatal(err)
		}
		s.engine.settleEdgeCellOnIntentDone(task, id, "exhausted")
	}
	settleExhausted("自动建格假设 A")
	if out, ok := dispatch("neg:auto", "自动建格假设 A 第二轮"); !ok || !strings.Contains(out, "已下发") {
		t.Fatalf("落定后第 2 次下发应放行: %s", out)
	}
	settleExhausted("自动建格假设 A 第二轮")
	out, ok = dispatch("neg:auto", "自动建格假设 A 第三轮")
	if ok && !strings.Contains(out, "已封") {
		t.Fatalf("第 3 次下发应触发封顶, got %s", out)
	}
}

// TestSettleEdgeCellOnIntentDone 钉住 0.15.53 格点回写：意图终态是判据推进的
// 落点——此前回写路径不存在（UpdateEdgeCellCriterion 零调用者、格点永远停在
// dispatched），判据哈希建完格就冻结，停滞熔断必然误杀。
//
// 口径：done + 预测兑现 ⇒ confirmed；done 未兑现 ⇒ disproven（负空间也是结论）；
// exhausted/stopped/blocked ⇒ 回 open 待复投；无 edge_cell 的普通意图不碰格点。
func TestSettleEdgeCellOnIntentDone(t *testing.T) {
	s, _ := taskHallTestServer(t)
	task := drainTestTask(t, s, "edge 格点回写")
	taskID, err := strconv.ParseInt(task.ID, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.m.pg.SetTaskEdgeMode(taskID, true, "reverse-negative"); err != nil {
		t.Fatal(err)
	}
	for _, cell := range []string{"hook:1", "hook:2", "hook:3"} {
		if err := s.m.pg.UpsertEdgeCell(taskID, cell, "reverse-negative", map[string]any{}); err != nil {
			t.Fatal(err)
		}
	}
	addIntent := func(cell, summary string, withEdge bool) int64 {
		payload := map[string]any{"summary": summary, "predicted_vulnclass": "info_disclosure"}
		if withEdge {
			payload["edge_cell"] = cell
		}
		id, err := task.Store.AddNode("intent", payload, 0, "open", "edge", nil)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	cellOf := func(cell string) *db.EdgeCell {
		cells, err := s.m.pg.ListEdgeCells(taskID)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range cells {
			if c.CellID == cell {
				return c
			}
		}
		t.Fatalf("格点 %s 应存在", cell)
		return nil
	}

	// ① done + 未兑现 ⇒ disproven（终态）+ 判据状态已写。
	i1 := addIntent("hook:1", "回写假设 A", true)
	s.engine.settleEdgeCellOnIntentDone(task, i1, "done")
	if c := cellOf("hook:1"); c.State != db.EdgeCellDisproven {
		t.Fatalf("done 未兑现应判 disproven, got %s", c.State)
	}
	if !strings.Contains(string(cellOf("hook:1").CriterionState), "disproven") {
		t.Fatalf("判据状态应记录回写结果, got %s", cellOf("hook:1").CriterionState)
	}

	// ② exhausted ⇒ 回 open 待复投。
	i2 := addIntent("hook:2", "回写假设 B", true)
	s.engine.settleEdgeCellOnIntentDone(task, i2, "exhausted")
	if c := cellOf("hook:2"); c.State != db.EdgeCellOpen {
		t.Fatalf("exhausted 应把格点放回 open, got %s", c.State)
	}

	// ③ done + 兑现（yields finding 命中 predicted_vulnclass）⇒ confirmed。
	// 用载荷里的 predicted_vulnclass 兜底（intent_predictions 未登记的存量假设）。
	i3 := addIntent("hook:3", "回写假设 C", true)
	fid, err := task.Store.AddNode("finding", map[string]any{"name": "命中预测", "vulnclass": "info_disclosure"}, 0, "pending", "edge", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := task.Store.Link(i3, "yields", fid); err != nil {
		t.Fatal(err)
	}
	s.engine.settleEdgeCellOnIntentDone(task, i3, "done")
	if c := cellOf("hook:3"); c.State != db.EdgeCellConfirmed {
		t.Fatalf("done 兑现应判 confirmed, got %s", c.State)
	}

	// ④ 无 edge_cell 的普通意图不碰任何格点（hook:3 已终态，判据状态不再变）。
	i4 := addIntent("", "普通意图", false)
	before := string(cellOf("hook:3").CriterionState)
	s.engine.settleEdgeCellOnIntentDone(task, i4, "done")
	if after := string(cellOf("hook:3").CriterionState); after != before {
		t.Fatalf("普通意图不得回写格点: before=%s after=%s", before, after)
	}
}

// TestEdgeCellHashesMovesOnBookkeeping 钉住 0.15.53 哈希拓宽：下发记账
//（attempts/state）与判据状态同权重算推进——只吃 criterion_state 时建完格
// 哈希就冻结，停滞熔断必然误杀（实测 3 条运行全灭）。
func TestEdgeCellHashesMovesOnBookkeeping(t *testing.T) {
	base := &db.EdgeCell{CellID: "c1", State: db.EdgeCellOpen, Attempts: 0, CriterionState: json.RawMessage(`{}`)}
	dispatched := &db.EdgeCell{CellID: "c1", State: db.EdgeCellDispatched, Attempts: 1, CriterionState: json.RawMessage(`{}`)}
	settled := &db.EdgeCell{CellID: "c1", State: db.EdgeCellDispatched, Attempts: 1, CriterionState: json.RawMessage(`{"outcome":"done"}`)}
	hBase := edge.CriterionHash(edgeCellHashes([]*db.EdgeCell{base}))
	hDispatched := edge.CriterionHash(edgeCellHashes([]*db.EdgeCell{dispatched}))
	hSettled := edge.CriterionHash(edgeCellHashes([]*db.EdgeCell{settled}))
	if hBase == hDispatched {
		t.Fatal("下发记账（attempts/state）变化必须改变判据哈希")
	}
	if hDispatched == hSettled {
		t.Fatal("criterion_state 变化必须改变判据哈希")
	}
}

// TestEdgeDispatchInFlightGate 钉住 0.15.54 在飞门：同格点同时只允许一条假设
// 在飞——复投必须等上一条验证落定。此前没有这道门，控制器对着还在飞的格点
// 连投，尝试数被瞬间烧到封顶、格点全被判 disproven（实测 task 6 一轮 20 格全灭）。
func TestEdgeDispatchInFlightGate(t *testing.T) {
	s, _ := taskHallTestServer(t)
	task := drainTestTask(t, s, "edge 在飞门")
	taskID, err := strconv.ParseInt(task.ID, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.m.pg.SetTaskEdgeMode(taskID, true, "reverse-negative"); err != nil {
		t.Fatal(err)
	}
	tid := strconv.FormatInt(taskID, 10)
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
	if out, ok := dispatch("neg:gate-1", "在飞门假设一"); !ok {
		t.Fatalf("首次下发应成功: %s", out)
	}
	// 第二条同格点（不同摘要绕过去重）⇒ 在飞门拦下，尝试数不动。
	out, ok := dispatch("neg:gate-1", "在飞门假设二")
	if ok && strings.Contains(out, "已下发") {
		t.Fatalf("在飞门应拦下同格点复投, got %s", out)
	}
	if !strings.Contains(out, "在飞假设") {
		t.Fatalf("复投拦截应说明在飞假设, got %s", out)
	}
	cells, _ := s.m.pg.ListEdgeCells(taskID)
	for _, c := range cells {
		if c.CellID == "neg:gate-1" && c.Attempts != 1 {
			t.Fatalf("被拦下的复投不得计尝试数, attempts=%d", c.Attempts)
		}
	}
	// 落定后可复投：把在飞假设置终态（意图转终态 + 格点回写，与生产收尾同形），
	// 再投成功（软跳过不是 error——断言必须看回执文本）。
	id, found, err := task.Store.FindOpenIntentBySummary("在飞门假设一")
	if err != nil || !found {
		t.Fatalf("应找回在飞假设: found=%v err=%v", found, err)
	}
	if err := transitionIntentState(task.Store, id, "open", "exhausted"); err != nil {
		t.Fatal(err)
	}
	s.engine.settleEdgeCellOnIntentDone(task, id, "exhausted")
	out, ok = dispatch("neg:gate-1", "在飞门假设三")
	if !ok || !strings.Contains(out, "已下发") {
		t.Fatalf("落定后复投应真正下发（而非软跳过）: %s", out)
	}
}

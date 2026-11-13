package db

// 业务建模 × 偏锋探索的【真库集成测试】（合并施工的运行验证）。
//
// 无 ARTEX_PG_DSN 自动跳过（与全包测试同一 skip 口径）；有库时把新代码的关键
// 授权/记账语义在真库上跑一遍：
//   1. biz 节点强制锚点 + 范围内锚定成功 + 范围读可见；
//   2. 锚点越界/无任务上下文整单拒（同一文案，不构成存在性探测器）；
//   3. 差异登记：WSTG/STRIDE/severity 由规则表补全、非法类型拒、状态流转；
//   4. SupersedeAsset 幂等保护（二次失效被拒、已有指针不被覆盖）；
//   5. 偏锋格点全生命周期：登记→下发计数→封顶封格→轨迹→非法 stop_reason 拒→
//      doctrine_complete 收尾→运行清账；
//   6. tasks.edge_mode/methodology_id 读写往返；
//   7. 快照版本单调；
//   8. chat_visible 列：seedBuiltins 后 edge_exec 为 false、其余内置为 true。

import (
	"fmt"
	"testing"
	"time"
)

// nowSuffix 是每次运行的唯一后缀（测试可重复执行，不撞历史数据）。
func nowSuffix() string { return fmt.Sprintf("%d", time.Now().UnixNano()) }

func TestBizEdgeIntegration(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	_ = d.ensureBizSchema()
	_ = d.ensureEdgeSchema()

	// ---------- 造数：公司（domain scope）+ 范围内/外各一个根域资产 + 任务 ----------
	suffix := nowSuffix()
	domain := "bizedge-" + suffix + ".example.com"
	companyID, _, _, _, _, err := d.Companies().CreateCompanyWithScope(
		"验证公司"+suffix, "", []ScopeInput{{Kind: "domain", Value: domain}}, "biz/edge 集成测试造数")
	if err != nil {
		t.Fatalf("create company: %v", err)
	}
	t.Cleanup(func() { _, _ = d.Companies().DeleteCompanyWithAssets(companyID, true) })

	inAssetID, err := d.Assets().UpsertRootDomain(UpsertRootDomainReq{Domain: domain})
	if err != nil {
		t.Fatalf("upsert in-scope asset: %v", err)
	}
	outAssetID, err := d.Assets().UpsertRootDomain(UpsertRootDomainReq{Domain: "outside-" + suffix + ".example.org"})
	if err != nil {
		t.Fatalf("upsert out-of-scope asset: %v", err)
	}

	task, err := d.CreateTaskWithOptions(
		"业务建模×偏锋集成验证任务 "+suffix,
		"验证 biz/edge 数据层与授权语义",
		TaskCreateOptions{CompanyIDs: []int64{companyID}},
	)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	tid := task.ID // db 层 Task.ID 就是 int64

	// ---------- ① biz 节点：范围内锚定成功 + 范围读可见 ----------
	nodeID, err := d.UpsertBizNodeInScope(tid, companyID, BizNodeInput{
		Layer: BizLayerDesign, NodeType: BizNodeEntity, Name: "订单实体",
		Description: "设计层声称的订单实体", Props: map[string]any{"fact_id": int64(42)},
		Confidence: "inferred", Anchors: []int64{inAssetID},
	})
	if err != nil {
		t.Fatalf("upsert in-scope biz node: %v", err)
	}
	g, err := d.ListBizGraphScoped(tid, companyID, "", "", 100)
	if err != nil {
		t.Fatalf("list biz graph: %v", err)
	}
	if len(g.Nodes) != 1 || g.Nodes[0].ID != nodeID {
		t.Fatalf("范围读应只见刚写的节点，got %d nodes", len(g.Nodes))
	}
	if len(g.Nodes[0].AssetIDs) != 1 || g.Nodes[0].AssetIDs[0] != inAssetID {
		t.Fatalf("锚点应回填 %d，got %v", inAssetID, g.Nodes[0].AssetIDs)
	}

	// ---------- ② 范围拒绝：越界锚点整单拒；无任务上下文拒；幂等更新 ----------
	if _, err := d.UpsertBizNodeInScope(tid, companyID, BizNodeInput{
		Layer: BizLayerDesign, NodeType: BizNodeAction, Name: "越界节点",
		Anchors: []int64{outAssetID},
	}); err == nil {
		t.Fatal("越界锚点未被拒绝")
	}
	if _, err := d.UpsertBizNodeInScope(0, companyID, BizNodeInput{
		Layer: BizLayerDesign, NodeType: BizNodeAction, Name: "无上下文",
		Anchors: []int64{inAssetID},
	}); err == nil || err != ErrAnchorNoTaskContext {
		t.Fatalf("无任务上下文应返回 ErrAnchorNoTaskContext，got %v", err)
	}
	// 重复 upsert 同名同层同类：更新而非新增（增量建模的幂等）。
	id2, err := d.UpsertBizNodeInScope(tid, companyID, BizNodeInput{
		Layer: BizLayerDesign, NodeType: BizNodeEntity, Name: "订单实体",
		Description: "更新后的描述", Anchors: []int64{inAssetID},
	})
	if err != nil || id2 != nodeID {
		t.Fatalf("重复 upsert 应复用同一行（got #%d want #%d, err=%v）", id2, nodeID, err)
	}

	// ---------- ③ 边 + 差异（规则表补全 WSTG/STRIDE/severity）----------
	edgeNodeID, err := d.UpsertBizNodeInScope(tid, companyID, BizNodeInput{
		Layer: BizLayerDesign, NodeType: BizNodeAction, Name: "下单动作", Anchors: []int64{inAssetID},
	})
	if err != nil {
		t.Fatalf("upsert action node: %v", err)
	}
	edgeRowID, err := d.UpsertBizEdgeInScope(tid, companyID, BizEdgeInput{
		Layer: BizLayerDesign, SrcID: nodeID, DstID: edgeNodeID, Rel: "invokes",
	})
	if err != nil {
		t.Fatalf("upsert biz edge: %v", err)
	}
	_ = edgeRowID
	diffID, err := d.RecordBizDiffInScope(tid, companyID, "missing_validation", "设计层声明 validates，行为层无对应边",
		map[string]any{"node_ids": []any{float64(nodeID)}}, map[string]any{"node_ids": []any{float64(edgeNodeID)}})
	if err != nil {
		t.Fatalf("record diff: %v", err)
	}
	diffs, err := d.ListBizDiffs(companyID, tid, "", "", 10)
	if err != nil || len(diffs) != 1 {
		t.Fatalf("差异应 1 条，got %d (err=%v)", len(diffs), err)
	}
	if diffs[0].WSTGRef != "WSTG-BUSL-03" || diffs[0].StrIDERef != "E" || diffs[0].Severity != "critical" {
		t.Fatalf("规则表补全错误: wstg=%s stride=%s sev=%s", diffs[0].WSTGRef, diffs[0].StrIDERef, diffs[0].Severity)
	}
	if _, err := d.RecordBizDiffInScope(tid, companyID, "not_a_type", "x", nil, nil); err == nil {
		t.Fatal("非法差异类型未被拒绝")
	}
	// 越界节点引用的差异整单拒（node_ids 判范围）。
	if _, err := d.RecordBizDiffInScope(tid, companyID, "state_bypass", "越界引用",
		map[string]any{"node_ids": []any{float64(999999)}}, nil); err == nil {
		t.Fatal("越界 node_ids 引用的差异未被拒绝")
	}
	// 状态流转 pushed（派生路径）。
	if err := d.UpdateBizDiffStatus(diffID, "pushed", map[string]any{"intent_id": int64(7)}); err != nil {
		t.Fatalf("diff → pushed: %v", err)
	}
	if diffs, _ := d.ListBizDiffs(companyID, tid, "pushed", "", 10); len(diffs) != 1 {
		t.Fatal("pushed 状态过滤失败")
	}

	// ---------- ④ SupersedeAsset 幂等保护 ----------
	newAssetID, err := d.Assets().UpsertRootDomain(UpsertRootDomainReq{Domain: "repl-" + suffix + ".example.com"})
	if err != nil {
		t.Fatalf("upsert replacement asset: %v", err)
	}
	if err := d.SupersedeAsset(inAssetID, newAssetID); err != nil {
		t.Fatalf("supersede: %v", err)
	}
	var supersededBy int64
	if err := d.QueryRow(`SELECT COALESCE(superseded_by,0) FROM assets WHERE id=$1`, inAssetID).Scan(&supersededBy); err != nil {
		t.Fatalf("read superseded_by: %v", err)
	}
	if supersededBy != newAssetID {
		t.Fatalf("失效指针应指向 #%d，got %d", newAssetID, supersededBy)
	}
	if err := d.SupersedeAsset(inAssetID, newAssetID); err == nil {
		t.Fatal("二次 supersede 未被拒绝（幂等保护失效）")
	}

	// ---------- ⑤ 偏锋格点全生命周期 ----------
	runID, err := d.StartEdgeRun(tid, "stpa-uca")
	if err != nil {
		t.Fatalf("start edge run: %v", err)
	}
	for _, cell := range []string{"auth:uca-1", "auth:uca-2"} {
		if err := d.UpsertEdgeCell(tid, cell, "stpa-uca", map[string]any{}); err != nil {
			t.Fatalf("upsert cell %s: %v", cell, err)
		}
	}
	n1, err := d.RecordEdgeDispatch(tid, "auth:uca-1")
	if err != nil || n1 != 1 {
		t.Fatalf("首次下发计数应 1，got %d (err=%v)", n1, err)
	}
	_, _ = d.RecordEdgeDispatch(tid, "auth:uca-1") // 第二次（达上限）
	attempts3, err := d.RecordEdgeDispatch(tid, "auth:uca-1") // 第三次：超上限，调用方应封格
	if err != nil {
		t.Fatalf("第三次下发: %v", err)
	}
	if attempts3 != 3 {
		t.Fatalf("第三次计数应 3，got %d", attempts3)
	}
	if err := d.FoldEdgeCell(tid, "auth:uca-1", "disproven"); err != nil {
		t.Fatalf("封格: %v", err)
	}
	if err := d.FoldEdgeCell(tid, "auth:uca-1", "bogus_state"); err == nil {
		t.Fatal("非法格点终态未被拒绝")
	}
	if err := d.AppendEdgeCriterionTrace(runID, map[string]any{"ts": 1, "event": "test"}); err != nil {
		t.Fatalf("append trace: %v", err)
	}
	if err := d.ConcludeEdgeRun(runID, "made_up_reason", nil); err == nil {
		t.Fatal("非法 stop_reason 未被拒绝")
	}
	if err := d.ConcludeEdgeRun(runID, StopReasonDoctrine, map[string]any{"summary": "判据成立"}); err != nil {
		t.Fatalf("doctrine 收尾: %v", err)
	}
	if run, _ := d.OpenEdgeRun(tid); run != nil {
		t.Fatal("收尾后 OpenEdgeRun 应为空")
	}
	// 判据停滞路径的收尾值也应合法（scheduler 用它）。
	run2, _ := d.StartEdgeRun(tid, "stpa-uca")
	if err := d.ConcludeEdgeRun(run2, StopReasonStalled, map[string]any{"rounds": 4}); err != nil {
		t.Fatalf("stalled 收尾: %v", err)
	}
	// cells 的 attempts 与 exploration_nodes.attempts 互不干扰（同名语义隔离）。
	var expAttempts int
	if err := d.QueryRow(`SELECT attempts FROM edge_cells WHERE task_id=$1 AND cell_id='auth:uca-1'`, tid).Scan(&expAttempts); err != nil || expAttempts != 3 {
		t.Fatalf("edge_cells.attempts 应独立计数为 3，got %d (err=%v)", expAttempts, err)
	}

	// ---------- ⑥ tasks.edge_mode/methodology_id 往返 ----------
	if err := d.SetTaskEdgeMode(tid, true, "pre-mortem"); err != nil {
		t.Fatalf("set edge mode: %v", err)
	}
	if on, meth, ok := d.TaskEdgeFields(tid); !ok || !on || meth != "pre-mortem" {
		t.Fatalf("edge fields 往返失败: on=%v meth=%s ok=%v", on, meth, ok)
	}
	if err := d.SetTaskEdgeMode(tid, false, ""); err != nil {
		t.Fatalf("clear edge mode: %v", err)
	}

	// ---------- ⑦ 快照版本单调 ----------
	v1, err := d.SnapshotBizModel(companyID, tid, []byte(`{"nodes":1}`), "第一版")
	if err != nil {
		t.Fatalf("snapshot v1: %v", err)
	}
	v2, err := d.SnapshotBizModel(companyID, tid, []byte(`{"nodes":2}`), "第二版")
	if err != nil || v2 != v1+1 {
		t.Fatalf("快照版本应单调（v1=%d v2=%d, err=%v）", v1, v2, err)
	}

	// ---------- ⑧ chat_visible：seedBuiltins 后 edge_exec=false、其余内置 true ----------
	var edgeExecVisible bool
	if err := d.QueryRow(`SELECT COALESCE(chat_visible,true) FROM agents WHERE key='edge_exec'`).Scan(&edgeExecVisible); err != nil {
		t.Fatalf("read edge_exec.chat_visible: %v", err)
	}
	if edgeExecVisible {
		t.Fatal("edge_exec 应为 chat_visible=false（执行体藏出选择器）")
	}
	var builtinVisibleCount int
	if err := d.QueryRow(`SELECT COUNT(*) FROM agents WHERE builtin=true AND key IN ('auto','pentest','recon','planner') AND chat_visible=false`).Scan(&builtinVisibleCount); err != nil {
		t.Fatalf("read builtin visibility: %v", err)
	}
	if builtinVisibleCount != 0 {
		t.Fatalf("存量内置 agent 可见性不应被改动，%d 个被置 false", builtinVisibleCount)
	}
}

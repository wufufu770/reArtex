package db

import (
	"encoding/json"
	"fmt"
	"strconv"
	"testing"
)

func TestExplorationFlow(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()

	expID, err := d.CreateExploration("test", "拿下测试目标")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM explorations WHERE id=$1`, expID) // cascades nodes/edges/activity
	es := d.Exploration(expID)

	// goal node + two intents
	goal, err := es.AddGoal(map[string]any{"text": "getadmin", "vulnclass": "authz"}, "human")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := es.AddIntent(map[string]any{"summary": "enumerate endpoints"}, 5, nil, "planner"); err != nil {
		t.Fatal(err)
	}
	i2, err := es.AddIntent(map[string]any{"summary": "test idor"}, 8, nil, "planner")
	if err != nil {
		t.Fatal(err)
	}

	// frontier ordered by priority desc → i2(8) before i1(5)
	fr, err := es.Frontier(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(fr) != 2 || fr[0].ID != i2 {
		t.Fatalf("frontier order wrong: %+v", fr)
	}

	// atomic claim: first wins, second on same id fails
	ok, err := es.ClaimIntent(i2, "worker-1")
	if err != nil || !ok {
		t.Fatalf("claim i2: ok=%v err=%v", ok, err)
	}
	ok2, _ := es.ClaimIntent(i2, "worker-2")
	if ok2 {
		t.Fatalf("double-claim should fail")
	}

	// finding yields from intent, proves goal
	find, err := es.AddNode("finding", map[string]any{"vulnclass": "idor", "severity": "high"}, 9, "confirmed", "worker-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := es.Link(i2, "yields", find); err != nil {
		t.Fatal(err)
	}
	if err := es.Link(find, "proves", goal); err != nil {
		t.Fatal(err)
	}
	if err := es.SetNodeState(goal, "met"); err != nil {
		t.Fatal(err)
	}

	// lineage: ancestors of the finding traced backward — here {i2, find} joined by
	// the yields edge. The proves→goal edge is DOWNSTREAM (goal must be excluded),
	// and the unrelated intent i1 is not on any path to the finding (excluded too).
	lnNodes, lnEdges, err := es.FindingLineage(find)
	if err != nil {
		t.Fatal(err)
	}
	got := map[int64]bool{}
	for _, n := range lnNodes {
		got[n.ID] = true
	}
	if len(lnNodes) != 2 || !got[find] || !got[i2] {
		t.Fatalf("lineage nodes: want {i2,find}, got %+v", lnNodes)
	}
	if got[goal] {
		t.Fatalf("lineage must exclude the proved goal (it is downstream of the finding)")
	}
	if len(lnEdges) != 1 || lnEdges[0].From != i2 || lnEdges[0].To != find || lnEdges[0].Rel != "yields" {
		t.Fatalf("lineage edges: want i2-yields->find, got %+v", lnEdges)
	}

	// activity poll by id cursor
	id1, err := es.AppendActivity(Activity{Worker: "worker-1", Kind: "tool_use", Tool: "Bash", Summary: "ran curl", Detail: "full output"})
	if err != nil {
		t.Fatal(err)
	}
	items, cursor, err := es.ActivityList(nil, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || cursor != id1 {
		t.Fatalf("activity list: items=%d cursor=%d", len(items), cursor)
	}
	det, _ := es.ActivityDetail(id1)
	if det != "full output" {
		t.Fatalf("detail want 'full output', got %q", det)
	}
	// incremental: nothing new after cursor
	items2, _, _ := es.ActivityList(nil, cursor, 100)
	if len(items2) != 0 {
		t.Fatalf("incremental poll should be empty, got %d", len(items2))
	}

	// stats
	st, _ := es.Stats()
	if st["intent"] != 2 || st["goal"] != 1 || st["finding"] != 1 {
		t.Fatalf("stats: %+v", st)
	}
}

func TestIntentPauseResumeAndCancelCleanup(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()

	expID, err := d.CreateExploration("test", "worker control cleanup")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM explorations WHERE id=$1`, expID)
	es := d.Exploration(expID)

	assetID, err := d.Assets().UpsertRootDomain(UpsertRootDomainReq{
		Domain: fmt.Sprintf("cancel-intent-%d.invalid", expID),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer deleteAsset(d, assetID)

	otherIntent, err := es.AddIntent(map[string]any{"summary": "keep intent"}, 1, nil, "planner")
	if err != nil {
		t.Fatal(err)
	}
	intentID, err := es.AddIntent(map[string]any{"summary": "cancel intent"}, 10, nil, "planner")
	if err != nil {
		t.Fatal(err)
	}
	if claimed, err := es.ClaimIntent(intentID, "worker-control-test"); err != nil || !claimed {
		t.Fatalf("initial claim: claimed=%v err=%v", claimed, err)
	}
	if err := es.SetIntentState(intentID, "paused"); err != nil {
		t.Fatal(err)
	}

	frontier, err := es.Frontier(10)
	if err != nil {
		t.Fatal(err)
	}
	for _, node := range frontier {
		if node.ID == intentID {
			t.Fatalf("paused intent %d must not enter frontier", intentID)
		}
	}
	if claimed, err := es.ClaimIntent(intentID, "worker-while-paused"); err != nil || claimed {
		t.Fatalf("paused intent claim: claimed=%v err=%v", claimed, err)
	}
	if err := es.SetIntentState(intentID, "open"); err != nil {
		t.Fatal(err)
	}
	if claimed, err := es.ClaimIntent(intentID, "worker-after-resume"); err != nil || !claimed {
		t.Fatalf("resumed intent claim: claimed=%v err=%v", claimed, err)
	}
	if err := es.SetIntentState(intentID, "paused"); err != nil {
		t.Fatal(err)
	}

	directFact, err := es.AddNode("fact", map[string]any{"summary": "remove fact"}, 0, "confirmed", "worker", []int64{assetID})
	if err != nil {
		t.Fatal(err)
	}
	directFinding, err := es.AddNode("finding", map[string]any{"summary": "remove finding"}, 0, "confirmed", "worker", nil)
	if err != nil {
		t.Fatal(err)
	}
	keptFact, err := es.AddNode("fact", map[string]any{"summary": "keep fact"}, 0, "confirmed", "other-worker", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := es.Link(intentID, RelYields, directFact); err != nil {
		t.Fatal(err)
	}
	if err := es.Link(intentID, RelYields, directFinding); err != nil {
		t.Fatal(err)
	}
	findingRowID, err := es.AddStandaloneFinding(0, directFinding, "test", "cancelled finding", SeverityHigh, "summary", "evidence", "worker", []int64{assetID})
	if err != nil {
		t.Fatal(err)
	}
	activityID, err := es.AppendActivity(Activity{NodeID: &intentID, Worker: "worker", Kind: "result", Summary: "remove activity"})
	if err != nil {
		t.Fatal(err)
	}
	keptActivityID, err := es.AppendActivity(Activity{NodeID: &otherIntent, Worker: "other-worker", Kind: "result", Summary: "keep activity"})
	if err != nil {
		t.Fatal(err)
	}

	cleanup, err := es.CancelIntent(intentID)
	if err != nil {
		t.Fatal(err)
	}
	if cleanup.Intents != 1 || cleanup.Facts != 1 || cleanup.Findings != 1 || cleanup.Activities != 1 {
		t.Fatalf("unexpected cleanup counts: %+v", cleanup)
	}
	for _, nodeID := range []int64{intentID, directFact, directFinding} {
		node, err := es.GetNode(nodeID)
		if err != nil {
			t.Fatal(err)
		}
		if node != nil {
			t.Fatalf("node %d survived intent cancellation", nodeID)
		}
	}
	for _, nodeID := range []int64{otherIntent, keptFact} {
		node, err := es.GetNode(nodeID)
		if err != nil || node == nil {
			t.Fatalf("unrelated node %d removed: node=%v err=%v", nodeID, node, err)
		}
	}

	assertCount := func(query string, want int, args ...any) {
		t.Helper()
		var got int
		if err := d.QueryRow(query, args...).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("query count=%d, want %d: %s", got, want, query)
		}
	}
	assertCount(`SELECT COUNT(*) FROM findings WHERE id=$1`, 0, findingRowID)
	assertCount(`SELECT COUNT(*) FROM activity WHERE id=$1`, 0, activityID)
	assertCount(`SELECT COUNT(*) FROM activity WHERE id=$1`, 1, keptActivityID)
	assertCount(`SELECT COUNT(*) FROM exploration_edges WHERE exploration_id=$1 AND (src_id=$2 OR dst_id=$2)`, 0, expID, intentID)
	assertCount(`SELECT COUNT(*) FROM exploration_anchors WHERE node_id=$1`, 0, directFact)
	assertCount(`SELECT COUNT(*) FROM assets WHERE id=$1`, 1, assetID)
}

// TestNodesPageQueryMatchesID verifies the 播报板 search filters on node id (both
// the bare number and the「#id」form the UI shows) in addition to payload/origin.
func TestNodesPageQueryMatchesID(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()

	expID, err := d.CreateExploration("test", "id 搜索")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM explorations WHERE id=$1`, expID)
	es := d.Exploration(expID)

	target, err := es.AddNode("fact", map[string]any{"summary": "needle-alpha"}, 0, "confirmed", "worker-a", nil)
	if err != nil {
		t.Fatal(err)
	}
	other, err := es.AddNode("fact", map[string]any{"summary": "unrelated-beta"}, 0, "confirmed", "worker-b", nil)
	if err != nil {
		t.Fatal(err)
	}

	onlyTarget := func(label, q string) {
		t.Helper()
		nodes, total, err := es.NodesPage(NodeFilter{Query: q}, 1, 50)
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		if total != 1 || len(nodes) != 1 || nodes[0].ID != target {
			t.Fatalf("%s: q=%q total=%d nodes=%+v, want single node %d", label, q, total, nodes, target)
		}
	}

	onlyTarget("bare id", fmt.Sprint(target))
	onlyTarget("hash id", "#"+fmt.Sprint(target))
	onlyTarget("payload still works", "needle-alpha")

	// A non-matching numeric id returns nothing (and does not accidentally match other).
	if nodes, total, err := es.NodesPage(NodeFilter{Query: fmt.Sprint(target + other + 1000)}, 1, 50); err != nil {
		t.Fatal(err)
	} else if total != 0 || len(nodes) != 0 {
		t.Fatalf("non-existent id: total=%d nodes=%+v, want empty", total, nodes)
	}
}

// =====================================================================
// L3 反向索引：探索节点 ↔ finding
//
// 承诺（L3 不新建图引擎，只在节点 payload 上记引用 + 加索引）：
//   - 节点 payload 记下 finding_id 与它绑定的流量快照 id；
//   - 反查是【索引查询】（等值 / @> containment 都能命中），不是文本搜索；
//   - 回写与漏洞行在**同一个事务**里 —— 否则「漏洞已落库、节点没回写」的窗口
//     会让反向索引静默漏掉一条证据，而且没有任何报错。
// =====================================================================

// reverseIndexTestFindingID 是不会与真实漏洞 id 相撞的假 id：反查是全表条件
// （payload->>'finding_id' = $1），撞上别人的行会让断言失去意义。
const reverseIndexTestFindingID int64 = 987654321

// reverseIndexTestSnapshot 造一条可入库的流量快照（id 由内容哈希得出）。
func reverseIndexTestSnapshot(source string) TrafficEvidenceSnapshot {
	snap := TrafficEvidenceSnapshot{
		SourceTrafficID: source,
		CapturedAt:      1700000000,
		URL:             "https://example.invalid/probe",
		Method:          "GET",
		Status:          200,
		ContentType:     "text/html",
		ReqHead:         "GET /probe HTTP/1.1\r\nHost: example.invalid\r\n\r\n",
		RespHead:        "HTTP/1.1 200 OK\r\nContent-Type: text/html\r\n\r\n",
		ReqHash:         "req-" + source,
		RespHash:        "resp-" + source,
		ReqLen:          11,
		RespLen:         22,
	}
	snap.ID = TrafficSnapshotID(snap)
	return snap
}

// TestLinkNodeToFindingIsReverseIndexable pins 反向索引的**用途**：写进去还不够 ——
// 反查必须是索引查询（等值 + @> containment 各自都能命中），不是文本搜索。
func TestLinkNodeToFindingIsReverseIndexable(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()

	expID, err := d.CreateExploration("test", "L3 反向索引")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM explorations WHERE id=$1`, expID)
	es := d.Exploration(expID)

	nodeID, err := es.AddNode("finding", map[string]any{"vulnclass": "idor", "severity": "high"}, 9, "confirmed", "worker-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := es.LinkNodeToFinding(nodeID, reverseIndexTestFindingID, []string{"snap-1"}); err != nil {
		t.Fatal(err)
	}

	var findingRef string
	var trafficRaw []byte
	if err := d.QueryRow(`SELECT payload->>'finding_id', payload->'traffic_ids' FROM exploration_nodes WHERE id=$1`, nodeID).
		Scan(&findingRef, &trafficRaw); err != nil {
		t.Fatal(err)
	}
	if findingRef != strconv.FormatInt(reverseIndexTestFindingID, 10) {
		t.Fatalf("payload->>'finding_id' = %q, want %q", findingRef, strconv.FormatInt(reverseIndexTestFindingID, 10))
	}
	var traffic []string
	if err := json.Unmarshal(trafficRaw, &traffic); err != nil {
		t.Fatal(err)
	}
	if len(traffic) != 1 || traffic[0] != "snap-1" {
		t.Fatalf("payload->'traffic_ids' = %v, want [snap-1]", traffic)
	}

	// 反查：等值（finding_id）与 containment（traffic_ids）都必须能命中这个节点。
	var count int
	if err := d.QueryRow(`SELECT count(*) FROM exploration_nodes WHERE payload->>'finding_id' = $1`,
		strconv.FormatInt(reverseIndexTestFindingID, 10)).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("payload->>'finding_id' 反查命中 %d 个节点, want 1", count)
	}
	if err := d.QueryRow(`SELECT count(*) FROM exploration_nodes WHERE payload->'traffic_ids' @> '["snap-1"]'::jsonb`).
		Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("payload->'traffic_ids' @> 反查命中 %d 个节点, want 1", count)
	}

	// 索引必须真的存在：索引没了，上面两条查询在小表上照样「查得到」（顺序扫描），
	// 只有断言索引本身才能把「反查是索引查询」钉住。
	for _, name := range []string{
		"idx_findings_node",
		"idx_exploration_nodes_finding_ref",
		"idx_exploration_nodes_traffic_ref",
	} {
		var exists bool
		if err := d.QueryRow(`SELECT EXISTS (SELECT 1 FROM pg_indexes WHERE schemaname=current_schema() AND indexname=$1)`, name).
			Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if !exists {
			t.Fatalf("L3 反向索引 %s 不存在", name)
		}
	}
}

// TestLinkNodeToFindingIsIdempotent pins 合并语义：重复回写既不会把同一个快照
// id 记两遍，也不会把先前记下的 id 抹掉。
func TestLinkNodeToFindingIsIdempotent(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()

	expID, err := d.CreateExploration("test", "L3 反向索引幂等")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM explorations WHERE id=$1`, expID)
	es := d.Exploration(expID)

	nodeID, err := es.AddNode("finding", map[string]any{"vulnclass": "idor"}, 9, "confirmed", "worker-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := es.LinkNodeToFinding(nodeID, reverseIndexTestFindingID, []string{"snap-1"}); err != nil {
		t.Fatal(err)
	}
	if err := es.LinkNodeToFinding(nodeID, reverseIndexTestFindingID, []string{"snap-1"}); err != nil {
		t.Fatal(err)
	}
	if err := es.LinkNodeToFinding(nodeID, reverseIndexTestFindingID, []string{"snap-1", "snap-2"}); err != nil {
		t.Fatal(err)
	}

	var trafficRaw []byte
	if err := d.QueryRow(`SELECT payload->'traffic_ids' FROM exploration_nodes WHERE id=$1`, nodeID).Scan(&trafficRaw); err != nil {
		t.Fatal(err)
	}
	var traffic []string
	if err := json.Unmarshal(trafficRaw, &traffic); err != nil {
		t.Fatal(err)
	}
	if len(traffic) != 2 || traffic[0] != "snap-1" || traffic[1] != "snap-2" {
		t.Fatalf("traffic_ids = %v, want [snap-1 snap-2]（去重、不丢先前记下的）", traffic)
	}
}

// TestFindingNodeReverseIndexFindsNode 走既有落库路径建一条漏洞，断言：
// findings.node_id → exploration_nodes.id 一步可达，且节点的 payload 记着
// 同一条 finding id 与它绑定的流量快照 id。
func TestFindingNodeReverseIndexFindsNode(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()

	task, err := d.CreateTask("L3 反向索引", "目标", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(task.ID)
	es := d.Exploration(task.ExplorationID)

	snap := reverseIndexTestSnapshot("traffic-reverse")
	prepared := []PreparedTrafficEvidence{{
		Ref:      TrafficRef{TrafficID: snap.SourceTrafficID, Role: "proof"},
		Snapshot: snap,
	}}

	var rec *RecordedFinding
	tx, err := d.Begin()
	if err != nil {
		t.Fatal(err)
	}
	rec, err = d.RecordFindingTx(t.Context(), tx, RecordFindingInput{
		TaskID:        task.ID,
		ExplorationID: task.ExplorationID,
		VulnClass:     "idor",
		Name:          "越权访问",
		Severity:      "high",
		Summary:       "摘要",
		Evidence:      "证据",
		Worker:        "worker-1",
	}, prepared)
	if err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}

	// 提交前：回写已经发生在【这个事务】里（同一连接读得到未提交的写入）。
	// 若回写被推迟到提交之后的另一个事务，这里读到的是空 —— 这条断言才是
	// 「回写与漏洞行同生共死」的真正证据。
	var beforeCommit string
	if err := tx.QueryRow(`SELECT COALESCE(payload->>'finding_id','') FROM exploration_nodes WHERE id=$1`, rec.NodeID).
		Scan(&beforeCommit); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if beforeCommit != strconv.FormatInt(rec.FindingID, 10) {
		_ = tx.Rollback()
		t.Fatalf("提交前 payload->>'finding_id' = %q, want %q", beforeCommit, strconv.FormatInt(rec.FindingID, 10))
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM findings WHERE id=$1`, rec.FindingID)
	defer d.Exec(`DELETE FROM traffic_evidence_snapshots WHERE id=$1`, snap.ID)

	// finding → node：一步可达（findings.node_id 有索引）。
	var nodeID int64
	if err := d.QueryRow(`SELECT node_id FROM findings WHERE id=$1`, rec.FindingID).Scan(&nodeID); err != nil {
		t.Fatal(err)
	}
	if nodeID != rec.NodeID {
		t.Fatalf("findings.node_id = %d, want %d", nodeID, rec.NodeID)
	}

	// node → finding：payload 记着同一条 finding id（反向索引的原料）。
	var findingRef string
	var trafficRaw []byte
	if err := d.QueryRow(`SELECT payload->>'finding_id', payload->'traffic_ids' FROM exploration_nodes WHERE id=$1`, nodeID).
		Scan(&findingRef, &trafficRaw); err != nil {
		t.Fatal(err)
	}
	if findingRef != strconv.FormatInt(rec.FindingID, 10) {
		t.Fatalf("payload->>'finding_id' = %q, want %q", findingRef, strconv.FormatInt(rec.FindingID, 10))
	}
	var traffic []string
	if err := json.Unmarshal(trafficRaw, &traffic); err != nil {
		t.Fatal(err)
	}
	if len(traffic) != 1 || traffic[0] != snap.ID {
		t.Fatalf("payload->'traffic_ids' = %v, want [%s]", traffic, snap.ID)
	}

	// 反查接口：finding → 节点（走 payload 表达式索引）。
	nodes, err := es.NodesForFinding(rec.FindingID)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 1 || nodes[0].ID != rec.NodeID {
		t.Fatalf("NodesForFinding = %+v, want 单个节点 %d", nodes, rec.NodeID)
	}
}

// TestFindingNodeWriteBackIsAtomicWithFinding pins 「回写与漏洞行同生共死」：
// 回写跑在调用方的事务里 ⇒ 事务回滚时回写也一起消失。
//
// 这条断言是反向索引不漏证据的前提：回写若落在【另一个事务】里，回滚后
// payload 里的 finding_id 还在，而漏洞行已经没了 —— 反向索引从此指向一个
// 不存在的漏洞，而且没有任何报错。
func TestFindingNodeWriteBackIsAtomicWithFinding(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()

	expID, err := d.CreateExploration("test", "L3 反向索引事务边界")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM explorations WHERE id=$1`, expID)
	es := d.Exploration(expID)

	// 节点先在事务外建好：只有节点已经存在，回滚后才有东西可查
	// （否则「节点没了」会掩盖「回写没跟着回滚」）。
	nodeID, err := es.AddNode("finding", map[string]any{"vulnclass": "idor"}, 9, "confirmed", "worker-1", nil)
	if err != nil {
		t.Fatal(err)
	}

	tx, err := d.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := linkNodeToFindingTx(tx, expID, nodeID, reverseIndexTestFindingID, []string{"snap-atomic"}); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}

	// 事务内：回写已经生效。
	var inTx string
	if err := tx.QueryRow(`SELECT payload->>'finding_id' FROM exploration_nodes WHERE id=$1`, nodeID).Scan(&inTx); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if inTx != strconv.FormatInt(reverseIndexTestFindingID, 10) {
		_ = tx.Rollback()
		t.Fatalf("事务内 payload->>'finding_id' = %q, want %q", inTx, strconv.FormatInt(reverseIndexTestFindingID, 10))
	}

	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}

	// 事务外：回写必须随事务一起消失。
	var after string
	if err := d.QueryRow(`SELECT COALESCE(payload->>'finding_id','') FROM exploration_nodes WHERE id=$1`, nodeID).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != "" {
		t.Fatalf("回写必须与调用方事务同生共死：回滚后仍读到 finding_id=%q", after)
	}
}

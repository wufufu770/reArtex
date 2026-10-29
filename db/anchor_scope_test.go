package db

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

// anchorScopeFixture 造两个范围互不相干的任务，各带一个资产：
//
//	assetA —— 在 taskA 的范围内
//	assetB —— 只在 taskB 的范围内（对 taskA 是范围外）
//
// 外加一个不属于任何任务的节点锚点位，用来观察「锚点有没有被写下去」。
type anchorScopeFixture struct {
	taskA  int64
	taskB  int64
	expA   int64
	nodeA  int64
	assetA int64
	assetB int64
}

func newAnchorScopeFixture(t *testing.T, d *DB) anchorScopeFixture {
	t.Helper()

	taskA, err := d.CreateTask("anchor scope A", "goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	taskB, err := d.CreateTask("anchor scope B", "goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(taskA.ID); _ = d.DeleteTask(taskB.ID) })

	stamp := time.Now().UnixNano()
	rootA := fmt.Sprintf("anchor-a-%d.example.test", stamp)
	rootB := fmt.Sprintf("anchor-b-%d.example.test", stamp)

	for _, sc := range []struct {
		tid int64
		dom string
	}{{taskA.ID, rootA}, {taskB.ID, rootB}} {
		if err := d.Assets().upsertTaskScope(TaskScope{TaskID: sc.tid, Kind: "root_domain", Domain: sc.dom}); err != nil {
			t.Fatalf("seed scope: %v", err)
		}
	}

	insert := func(domain string, owner int64) int64 {
		var id int64
		if err := d.QueryRow(`INSERT INTO assets(type,domain,root_domain,task_ids,last_seen)
			VALUES ('subdomain',$1,$1,ARRAY[$2]::bigint[],now()) RETURNING id`, domain, owner).Scan(&id); err != nil {
			t.Fatalf("insert asset %s: %v", domain, err)
		}
		return id
	}

	fx := anchorScopeFixture{
		taskA:  taskA.ID,
		taskB:  taskB.ID,
		expA:   taskA.ExplorationID,
		assetA: insert(rootA, taskA.ID),
		assetB: insert(rootB, taskB.ID),
	}
	t.Cleanup(func() {
		d.Exec(`DELETE FROM assets WHERE id=ANY($1::bigint[])`, []int64{fx.assetA, fx.assetB})
	})

	// 一个不带锚点的节点：锚点写入的宿主。
	nodeA, err := d.Exploration(fx.expA).AddNode(KindFact, map[string]any{"summary": "anchor fixture fact"}, 5, "confirmed", "worker", nil)
	if err != nil {
		t.Fatal(err)
	}
	fx.nodeA = nodeA
	return fx
}

// anchorRowCount 数一条 (node,asset) 锚点行有几条。
func anchorRowCount(t *testing.T, d *DB, nodeID, assetID int64) int {
	t.Helper()
	var n int
	if err := d.QueryRow(`SELECT count(*) FROM exploration_anchors WHERE node_id=$1 AND asset_id=$2`, nodeID, assetID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// findingNodeCount 数这个探索里 kind='finding' 的节点数。
func findingNodeCount(t *testing.T, d *DB, explorationID int64) int {
	t.Helper()
	var n int
	if err := d.QueryRow(`SELECT count(*) FROM exploration_nodes WHERE exploration_id=$1 AND kind='finding'`, explorationID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestAnchorInScopeRefusesOutOfScopeAsset pins the L5 leak: the anchor write
// path must not register a relationship between a task's node and an asset
// outside that task's scope. Such a row persists a foreign asset id in the
// shared graph and (since a scoped graph read only shows a node when EVERY
// asset it is anchored to is in scope) can also hide the node from its own task.
func TestAnchorInScopeRefusesOutOfScopeAsset(t *testing.T) {
	d, _, _ := testSetup(t)
	defer d.Close()

	fx := newAnchorScopeFixture(t, d)

	err := d.Exploration(fx.expA).AnchorInScope(fx.taskA, fx.nodeA, fx.assetB)
	if !errors.Is(err, ErrAnchorOutOfScope) {
		t.Fatalf("范围外资产必须被拒，实际错误：%v", err)
	}
	if n := anchorRowCount(t, d, fx.nodeA, fx.assetB); n != 0 {
		t.Fatalf("范围外的资产不得被锚定（锚点行数 %d）", n)
	}
}

// TestAnchorInScopeKeepsInScopeAsset pins the positive half: the fix must close
// the leak without blinding the owner — an asset inside the caller's scope is
// still anchored.
func TestAnchorInScopeKeepsInScopeAsset(t *testing.T) {
	d, _, _ := testSetup(t)
	defer d.Close()

	fx := newAnchorScopeFixture(t, d)

	if err := d.Exploration(fx.expA).AnchorInScope(fx.taskA, fx.nodeA, fx.assetA); err != nil {
		t.Fatalf("范围内资产必须锚定成功：%v", err)
	}
	if n := anchorRowCount(t, d, fx.nodeA, fx.assetA); n != 1 {
		t.Fatalf("范围内资产应当恰好一条锚点（实际 %d）", n)
	}
}

// TestAnchorInScopeRefusesUnknownAsset pins that a non-existent asset id is
// refused with the SAME error as an out-of-scope one. If the two were told
// apart, the refusal would become an existence oracle: the caller could probe
// for foreign asset ids by watching which message comes back.
func TestAnchorInScopeRefusesUnknownAsset(t *testing.T) {
	d, _, _ := testSetup(t)
	defer d.Close()

	fx := newAnchorScopeFixture(t, d)

	const unknownAssetID = 987654321
	err := d.Exploration(fx.expA).AnchorInScope(fx.taskA, fx.nodeA, unknownAssetID)
	if !errors.Is(err, ErrAnchorOutOfScope) {
		t.Fatalf("不存在的资产必须被拒，实际错误：%v", err)
	}
	if n := anchorRowCount(t, d, fx.nodeA, unknownAssetID); n != 0 {
		t.Fatalf("不存在的资产不得留下锚点（锚点行数 %d）", n)
	}
}

// TestAnchorInScopeWithoutTaskContextRefuses pins the fail-closed half: without
// a task context there is no authorization edge, so the write must be refused
// instead of falling back to the global asset pool.
func TestAnchorInScopeWithoutTaskContextRefuses(t *testing.T) {
	d, _, _ := testSetup(t)
	defer d.Close()

	fx := newAnchorScopeFixture(t, d)

	for _, taskID := range []int64{0, -1} {
		err := d.Exploration(fx.expA).AnchorInScope(taskID, fx.nodeA, fx.assetA)
		if !errors.Is(err, ErrAnchorNoTaskContext) {
			t.Fatalf("无任务上下文(taskID=%d)必须被拒，实际错误：%v", taskID, err)
		}
		if n := anchorRowCount(t, d, fx.nodeA, fx.assetA); n != 0 {
			t.Fatalf("无任务上下文不得留下锚点（taskID=%d，锚点行数 %d）", taskID, n)
		}
	}
}

// TestAnchorHostPathStillWrites pins that the trusted host/UI entry point keeps
// working: server 的 seed 路径（t.Store.Anchor(begin, rootID)）没有任务上下文
// 也必须能锚 —— 它锚的是本任务自己的根事实与自己的目标资产，平台操作者可信。
func TestAnchorHostPathStillWrites(t *testing.T) {
	d, _, _ := testSetup(t)
	defer d.Close()

	fx := newAnchorScopeFixture(t, d)

	if err := d.Exploration(fx.expA).Anchor(fx.nodeA, fx.assetB); err != nil {
		t.Fatalf("宿主/UI 的锚点入口必须照常写：%v", err)
	}
	if n := anchorRowCount(t, d, fx.nodeA, fx.assetB); n != 1 {
		t.Fatalf("宿主/UI 的锚点入口应当写入一条（实际 %d）", n)
	}
}

// TestRecordFindingTxRefusesOutOfScopeAsset pins the second anchor write path:
// a finding that names an out-of-scope asset must not create an anchor for it.
// The whole write is refused — the same id would otherwise land in
// findings.asset_ids and in the notification payload too, and filtering it in
// every one of those places is exactly how a leak survives.
func TestRecordFindingTxRefusesOutOfScopeAsset(t *testing.T) {
	d, _, _ := testSetup(t)
	defer d.Close()

	fx := newAnchorScopeFixture(t, d)

	before := findingNodeCount(t, d, fx.expA)
	_, err := d.Exploration(fx.expA).RecordFinding(t.Context(), RecordFindingInput{
		TaskID:        fx.taskA,
		ExplorationID: fx.expA,
		Summary:       "范围外资产的漏洞",
		Severity:      "low",
		AssetIDs:      []int64{fx.assetB},
		Worker:        "test",
	})
	if !errors.Is(err, ErrAnchorOutOfScope) {
		t.Fatalf("范围外资产必须让整单被拒，实际错误：%v", err)
	}
	if n := anchorRowCount(t, d, fx.nodeA, fx.assetB); n != 0 {
		t.Fatalf("范围外资产不得被锚定（锚点行数 %d）", n)
	}
	if after := findingNodeCount(t, d, fx.expA); after != before {
		t.Fatalf("被拒的写入不得留下漏洞节点（before=%d after=%d）", before, after)
	}
}

// TestRecordFindingTxAnchorsInScopeAsset pins the positive half of the finding
// path: an in-scope asset still gets its anchor, and the finding is recorded.
func TestRecordFindingTxAnchorsInScopeAsset(t *testing.T) {
	d, _, _ := testSetup(t)
	defer d.Close()

	fx := newAnchorScopeFixture(t, d)

	rec, err := d.Exploration(fx.expA).RecordFinding(t.Context(), RecordFindingInput{
		TaskID:        fx.taskA,
		ExplorationID: fx.expA,
		Summary:       "范围内资产的漏洞",
		Severity:      "low",
		AssetIDs:      []int64{fx.assetA},
		Worker:        "test",
	})
	if err != nil {
		t.Fatalf("范围内资产的漏洞必须记录成功：%v", err)
	}
	if n := anchorRowCount(t, d, rec.NodeID, fx.assetA); n != 1 {
		t.Fatalf("范围内资产应当恰好一条锚点（实际 %d）", n)
	}
}

// TestRecordFindingTxWithoutTaskContextWritesNoAnchor pins that a caller with
// no task context gets no unscoped anchoring: the finding itself is still
// recorded (an unowned finding is a legitimate, pre-existing case), but no
// anchor edge is written — without an authorization edge there is nothing to
// judge the asset against.
func TestRecordFindingTxWithoutTaskContextWritesNoAnchor(t *testing.T) {
	d, _, _ := testSetup(t)
	defer d.Close()

	fx := newAnchorScopeFixture(t, d)

	rec, err := d.Exploration(fx.expA).RecordFinding(t.Context(), RecordFindingInput{
		ExplorationID: fx.expA,
		Summary:       "无归属的漏洞",
		Severity:      "low",
		AssetIDs:      []int64{fx.assetA},
		Worker:        "test",
	})
	if err != nil {
		t.Fatalf("无任务上下文的漏洞仍应被记录：%v", err)
	}
	if n := anchorRowCount(t, d, rec.NodeID, fx.assetA); n != 0 {
		t.Fatalf("无任务上下文不得留下锚点（锚点行数 %d）", n)
	}
}

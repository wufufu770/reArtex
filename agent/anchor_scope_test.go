package agent

import (
	"fmt"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
)

// anchorFixture 造两个范围互不相干的任务，各带一个资产：
//
//	assetA —— 在 taskA 的范围内
//	assetB —— 只在 taskB 的范围内（对 taskA 是范围外）
type anchorFixture struct {
	taskA  int64
	expA   int64
	nodeA  int64
	assetA int64
	assetB int64
	hostA  string
}

func newAnchorFixture(t *testing.T, d *db.DB) anchorFixture {
	t.Helper()

	taskA, err := d.CreateTask("agent anchor A", "goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	taskB, err := d.CreateTask("agent anchor B", "goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(taskA.ID); _ = d.DeleteTask(taskB.ID) })

	stamp := time.Now().UnixNano()
	rootA := fmt.Sprintf("agent-anchor-a-%d.example.test", stamp)
	rootB := fmt.Sprintf("agent-anchor-b-%d.example.test", stamp)
	seedTaskScope(t, d, taskA.ID, "root_domain", rootA)
	seedTaskScope(t, d, taskB.ID, "root_domain", rootB)

	fx := anchorFixture{
		taskA:  taskA.ID,
		expA:   taskA.ExplorationID,
		hostA:  rootA,
		assetA: insertScopeAsset(t, d, scopeAssetSpec{domain: rootA, rootDomain: rootA, ownerTaskID: taskA.ID}),
		assetB: insertScopeAsset(t, d, scopeAssetSpec{domain: rootB, rootDomain: rootB, ownerTaskID: taskB.ID}),
	}
	t.Cleanup(func() {
		d.Exec(`DELETE FROM assets WHERE id=ANY($1::bigint[])`, []int64{fx.assetA, fx.assetB})
	})

	nodeA, err := d.Exploration(fx.expA).AddNode(db.KindIntent, map[string]any{"summary": "agent anchor intent"}, 5, "open", "worker", nil)
	if err != nil {
		t.Fatal(err)
	}
	fx.nodeA = nodeA
	return fx
}

// anchorRows 数一条 (node,asset) 锚点行有几条。
func anchorRows(t *testing.T, d *db.DB, nodeID, assetID int64) int {
	t.Helper()
	var n int
	if err := d.QueryRow(`SELECT count(*) FROM exploration_anchors WHERE node_id=$1 AND asset_id=$2`, nodeID, assetID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestAnchorOwnerRefusesOutOfScopeAsset pins L5 on the agent path: the owner
// node must not be anchored to an asset outside the task's scope. Such an edge
// persists a foreign asset id in the shared graph, and — since a scoped graph
// read only shows a node when EVERY asset it is anchored to is in scope — it
// also hides the node from the very task that wrote it.
func TestAnchorOwnerRefusesOutOfScopeAsset(t *testing.T) {
	d := testDB(t)
	defer d.Close()

	fx := newAnchorFixture(t, d)

	tools := NewToolSet(d.Exploration(fx.expA), "worker")
	tools.SetTaskID(fx.taskA)
	tools.SetOwnerNode(fx.nodeA)

	tools.anchorOwner(fx.assetB)

	if n := anchorRows(t, d, fx.nodeA, fx.assetB); n != 0 {
		t.Fatalf("范围外的资产不得被锚定（锚点行数 %d）", n)
	}
}

// TestAnchorOwnerKeepsInScopeAsset pins the positive half: the task's own
// assets are still anchored — the fix must not blind the owner.
func TestAnchorOwnerKeepsInScopeAsset(t *testing.T) {
	d := testDB(t)
	defer d.Close()

	fx := newAnchorFixture(t, d)

	tools := NewToolSet(d.Exploration(fx.expA), "worker")
	tools.SetTaskID(fx.taskA)
	tools.SetOwnerNode(fx.nodeA)

	tools.anchorOwner(fx.assetA)

	if n := anchorRows(t, d, fx.nodeA, fx.assetA); n != 1 {
		t.Fatalf("范围内的资产应当恰好一条锚点（实际 %d）", n)
	}
}

// TestAnchorOwnerWithoutTaskContextRefuses pins the fail-closed half on the
// agent path: without a task context there is no authorization edge, so no
// anchor may be written — not even to an asset that happens to be in scope.
func TestAnchorOwnerWithoutTaskContextRefuses(t *testing.T) {
	d := testDB(t)
	defer d.Close()

	fx := newAnchorFixture(t, d)

	tools := NewToolSet(d.Exploration(fx.expA), "worker")
	tools.SetOwnerNode(fx.nodeA) // 没有 SetTaskID ⇒ taskID=0

	tools.anchorOwner(fx.assetA)

	if n := anchorRows(t, d, fx.nodeA, fx.assetA); n != 0 {
		t.Fatalf("无任务上下文不得留下锚点（锚点行数 %d）", n)
	}
}

// TestInsertAssetsAnchorsOwnAsset pins that the agent's own freshly inserted
// asset still ends up anchored: insert_assets declares the conservative scope
// row first, so the scope judgement that guards the anchor write sees the
// asset as in scope. Without that ordering every newly discovered asset would
// lose its edge and the task's own nodes would vanish from the scoped read.
func TestInsertAssetsAnchorsOwnAsset(t *testing.T) {
	d := testDB(t)
	defer d.Close()

	fx := newAnchorFixture(t, d)

	tools := NewToolSet(d.Exploration(fx.expA), "worker")
	tools.SetTaskID(fx.taskA)
	tools.SetAssetStore(d.Assets(), d.Companies())
	tools.SetOwnerNode(fx.nodeA)

	host := fmt.Sprintf("agent-anchor-inserted-%d.example.test", time.Now().UnixNano())
	defer d.Exec(`DELETE FROM assets WHERE domain=$1`, host) //nolint:errcheck

	out := callInsertAssets(t, tools, map[string]any{
		"assets": []any{
			map[string]any{"type": "subdomain", "domain": host},
		},
	})
	if errs, _ := out["errors"].([]any); len(errs) > 0 {
		t.Fatalf("插入资产不该报错：%v", errs)
	}
	results, _ := out["results"].([]any)
	if len(results) != 1 {
		t.Fatalf("应当插入一条资产（实际 %d）", len(results))
	}
	item, _ := results[0].(map[string]any)
	id, _ := item["id"].(float64)
	if id == 0 {
		t.Fatalf("插入结果里没有资产 id：%v", item)
	}

	if n := anchorRows(t, d, fx.nodeA, int64(id)); n != 1 {
		t.Fatalf("agent 自己插入的资产应当被锚定（锚点行数 %d）", n)
	}
}

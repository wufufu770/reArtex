package db

import (
	"fmt"
	"testing"
	"time"

	"github.com/Autumn-27/artex/internal/scopepolicy"
)

// graphScopeFixture seeds two tasks with DISJOINT declared scopes plus the
// assets each of them owns, so a node's visibility can be judged purely by the
// assets its anchors point at.
//
//	assetA     —— 在 taskA 的范围内
//	assetB     —— 在 taskB 的范围内（对 taskA 是范围外）
//	assetFree  —— 不在任何任务的声明范围内
type graphScopeFixture struct {
	taskA     int64
	taskB     int64
	expA      int64
	expB      int64
	rootA     string
	rootB     string
	rootFree  string
	assetA    int64
	assetB    int64
	assetFree int64
}

func newGraphScopeFixture(t *testing.T, d *DB) graphScopeFixture {
	t.Helper()

	taskA, err := d.CreateTask("graph scope A", "goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	taskB, err := d.CreateTask("graph scope B", "goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(taskA.ID); _ = d.DeleteTask(taskB.ID) })

	stamp := time.Now().UnixNano()
	rootA := fmt.Sprintf("graph-a-%d.example.test", stamp)
	rootB := fmt.Sprintf("graph-b-%d.example.test", stamp)
	rootFree := fmt.Sprintf("graph-free-%d.example.test", stamp)

	for _, sc := range []struct {
		tid int64
		dom string
	}{
		{taskA.ID, rootA},
		{taskB.ID, rootB},
	} {
		if err := d.Assets().upsertTaskScope(TaskScope{TaskID: sc.tid, Kind: "root_domain", Domain: sc.dom}); err != nil {
			t.Fatalf("seed scope: %v", err)
		}
	}

	insert := func(domain string) int64 {
		var id int64
		if err := d.QueryRow(`INSERT INTO assets(type,domain,root_domain,task_ids,last_seen)
			VALUES ('subdomain',$1,$1,ARRAY[$2]::bigint[],now()) RETURNING id`, domain, taskA.ID).Scan(&id); err != nil {
			t.Fatalf("insert asset %s: %v", domain, err)
		}
		return id
	}

	fx := graphScopeFixture{
		taskA:     taskA.ID,
		taskB:     taskB.ID,
		expA:      taskA.ExplorationID,
		expB:      taskB.ExplorationID,
		rootA:     rootA,
		rootB:     rootB,
		rootFree:  rootFree,
		assetA:    insert(rootA),
		assetB:    insert(rootB),
		assetFree: insert(rootFree),
	}
	t.Cleanup(func() {
		d.Exec(`DELETE FROM assets WHERE id=ANY($1::bigint[])`, []int64{fx.assetA, fx.assetB, fx.assetFree})
	})
	return fx
}

// graphNodeIDs collects node ids for membership assertions.
func graphNodeIDs(nodes []*Node) map[int64]bool {
	out := map[int64]bool{}
	for _, n := range nodes {
		out[n.ID] = true
	}
	return out
}

// TestListByKindScopedKeepsInScopeAnchoredNode pins the positive half of the
// scoping relation: a node anchored to an asset INSIDE the caller's scope is
// still returned — the fix must close the leak without blinding the owner.
func TestListByKindScopedKeepsInScopeAnchoredNode(t *testing.T) {
	d, _, _ := testSetup(t)
	defer d.Close()

	fx := newGraphScopeFixture(t, d)
	store := d.Exploration(fx.expA)

	inScopeID, err := store.AddNode(KindFact, map[string]any{"summary": "in-scope fact"}, 5, "confirmed", "worker", []int64{fx.assetA})
	if err != nil {
		t.Fatal(err)
	}

	got, err := store.ListByKindScoped(fx.taskA, KindFact, 100)
	if err != nil {
		t.Fatal(err)
	}
	ids := graphNodeIDs(got)
	if !ids[inScopeID] {
		t.Fatalf("范围内锚定的节点必须可见（实际返回 %d 个节点，含该节点=%v）", len(got), ids[inScopeID])
	}
}

// TestListByKindScopedDropsOutOfScopeAnchoredNode pins the leak itself: a node
// anchored to an asset outside the caller's scope — another task's asset, or an
// asset in nobody's scope — must not come back, payload and all.
func TestListByKindScopedDropsOutOfScopeAnchoredNode(t *testing.T) {
	d, _, _ := testSetup(t)
	defer d.Close()

	fx := newGraphScopeFixture(t, d)
	store := d.Exploration(fx.expA)

	otherTaskID, err := store.AddNode(KindFact, map[string]any{"summary": "other task fact"}, 5, "confirmed", "worker", []int64{fx.assetB})
	if err != nil {
		t.Fatal(err)
	}
	unscopedID, err := store.AddNode(KindFact, map[string]any{"summary": "nobody's fact"}, 5, "confirmed", "worker", []int64{fx.assetFree})
	if err != nil {
		t.Fatal(err)
	}

	got, err := store.ListByKindScoped(fx.taskA, KindFact, 100)
	if err != nil {
		t.Fatal(err)
	}
	ids := graphNodeIDs(got)
	if ids[otherTaskID] {
		t.Fatalf("锚到别的任务资产的节点不得返回（节点 %d）", otherTaskID)
	}
	if ids[unscopedID] {
		t.Fatalf("锚到不在任何任务范围内的资产的节点不得返回（节点 %d）", unscopedID)
	}
}

// TestListByKindScopedDropsUnanchoredNode pins the fail-closed rule for a node
// with no anchors at all. A node's payload is the recon result of the assets it
// touched; "touched nothing" cannot be shown to mean "touched something out of
// scope", so an unanchored node stays invisible to a scoped read.
func TestListByKindScopedDropsUnanchoredNode(t *testing.T) {
	d, _, _ := testSetup(t)
	defer d.Close()

	fx := newGraphScopeFixture(t, d)
	store := d.Exploration(fx.expA)

	unanchoredID, err := store.AddNode(KindFact, map[string]any{"summary": "unanchored fact"}, 5, "confirmed", "worker", nil)
	if err != nil {
		t.Fatal(err)
	}

	got, err := store.ListByKindScoped(fx.taskA, KindFact, 100)
	if err != nil {
		t.Fatal(err)
	}
	if ids := graphNodeIDs(got); ids[unanchoredID] {
		t.Fatalf("无锚点的节点不得返回（节点 %d）", unanchoredID)
	}
}

// TestListByKindScopedDropsNodeAnchoredBothInAndOutOfScope pins the mixed case
// explicitly: a node anchored to BOTH an in-scope and an out-of-scope asset is
// filtered out entirely. Its payload merges the conclusions about several
// assets, so keeping it would leak the out-of-scope half — half of a node is
// still a leak.
func TestListByKindScopedDropsNodeAnchoredBothInAndOutOfScope(t *testing.T) {
	d, _, _ := testSetup(t)
	defer d.Close()

	fx := newGraphScopeFixture(t, d)
	store := d.Exploration(fx.expA)

	mixedID, err := store.AddNode(KindFact, map[string]any{"summary": "mixed fact"}, 5, "confirmed", "worker", []int64{fx.assetA, fx.assetB})
	if err != nil {
		t.Fatal(err)
	}

	got, err := store.ListByKindScoped(fx.taskA, KindFact, 100)
	if err != nil {
		t.Fatal(err)
	}
	if ids := graphNodeIDs(got); ids[mixedID] {
		t.Fatalf("同时锚了范围内与范围外资产的节点不得返回（节点 %d）", mixedID)
	}
}

// TestListByKindScopedWithoutTaskContextSeesNothing pins that a caller without
// a task context gets NOTHING: there is no authorization edge, so the read must
// not fall back to the global graph.
func TestListByKindScopedWithoutTaskContextSeesNothing(t *testing.T) {
	d, _, _ := testSetup(t)
	defer d.Close()

	fx := newGraphScopeFixture(t, d)
	store := d.Exploration(fx.expA)

	if _, err := store.AddNode(KindFact, map[string]any{"summary": "in-scope fact"}, 5, "confirmed", "worker", []int64{fx.assetA}); err != nil {
		t.Fatal(err)
	}

	original := scopepolicy.Policy
	t.Cleanup(func() { scopepolicy.Policy = original })
	scopepolicy.Policy.PlatformScopeMode = scopepolicy.ScopeEmpty

	for _, taskID := range []int64{0, -1} {
		got, err := store.ListByKindScoped(taskID, KindFact, 100)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 0 {
			t.Fatalf("无任务上下文(taskID=%d)必须什么都看不到，实际返回 %d 个节点", taskID, len(got))
		}
	}
}

// TestListByKindScopedKeepsUnscopedReadIntact pins that the unscoped read (the
// host/UI entry point) still returns the whole graph — the fix narrows the
// SCOPED read, it does not silently strip the owner's own graph.
func TestListByKindScopedKeepsUnscopedReadIntact(t *testing.T) {
	d, _, _ := testSetup(t)
	defer d.Close()

	fx := newGraphScopeFixture(t, d)
	store := d.Exploration(fx.expA)

	inScopeID, err := store.AddNode(KindFact, map[string]any{"summary": "in-scope fact"}, 5, "confirmed", "worker", []int64{fx.assetA})
	if err != nil {
		t.Fatal(err)
	}
	outScopeID, err := store.AddNode(KindFact, map[string]any{"summary": "out-of-scope fact"}, 5, "confirmed", "worker", []int64{fx.assetB})
	if err != nil {
		t.Fatal(err)
	}

	all, err := store.ListByKind(KindFact, 100)
	if err != nil {
		t.Fatal(err)
	}
	ids := graphNodeIDs(all)
	if !ids[inScopeID] || !ids[outScopeID] {
		t.Fatalf("无范围约束的宿主读必须仍然看到全部节点（in=%v out=%v）", ids[inScopeID], ids[outScopeID])
	}
}

// TestListByKindWithSourcesScopedFiltersInheritedNodesByCallerScope pins that
// the inherited (direct source task) read is bounded by the CALLER's scope, not
// by the source task's own scope: a source node anchored to the caller's
// in-scope asset is inherited, one anchored elsewhere is not.
func TestListByKindWithSourcesScopedFiltersInheritedNodesByCallerScope(t *testing.T) {
	d, _, _ := testSetup(t)
	defer d.Close()

	fx := newGraphScopeFixture(t, d)

	// The source task carries no scope of its own: the CALLER's scope must be
	// the only thing that decides what the caller may see.
	source, err := d.CreateTask("graph scope source", "source goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	current, err := d.CreateTaskWithOptions("graph scope current", "current goal", TaskCreateOptions{
		SourceTaskIDs: []int64{source.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(current.ID); _ = d.DeleteTask(source.ID) })
	if err := d.Assets().upsertTaskScope(TaskScope{TaskID: current.ID, Kind: "root_domain", Domain: fx.rootA}); err != nil {
		t.Fatalf("seed current scope: %v", err)
	}

	sourceStore := d.Exploration(source.ExplorationID)
	inheritedInScope, err := sourceStore.AddNode(KindFact, map[string]any{"summary": "inherited in-scope fact"}, 5, "confirmed", "worker", []int64{fx.assetA})
	if err != nil {
		t.Fatal(err)
	}
	inheritedOutScope, err := sourceStore.AddNode(KindFact, map[string]any{"summary": "inherited out-of-scope fact"}, 5, "confirmed", "worker", []int64{fx.assetFree})
	if err != nil {
		t.Fatal(err)
	}

	store := d.Exploration(current.ExplorationID)
	own, err := store.AddNode(KindFact, map[string]any{"summary": "own in-scope fact"}, 5, "confirmed", "worker", []int64{fx.assetA})
	if err != nil {
		t.Fatal(err)
	}

	got, err := store.ListByKindWithSourcesScoped(current.ID, KindFact, 100)
	if err != nil {
		t.Fatal(err)
	}
	ids := graphNodeIDs(got)
	if !ids[own] {
		t.Fatalf("本任务范围内锚定的节点必须可见")
	}
	if !ids[inheritedInScope] {
		t.Fatalf("继承节点若锚在调用者范围内的资产上，必须可见（节点 %d）", inheritedInScope)
	}
	if ids[inheritedOutScope] {
		t.Fatalf("继承节点若锚在调用者范围外的资产上，不得返回（节点 %d）", inheritedOutScope)
	}
}

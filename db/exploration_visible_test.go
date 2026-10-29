package db

import (
	"errors"
	"testing"
	"time"
)

// 本文件钉住【agent 可达读面】的那条可见性关系（scopedNodeVisibilityNoOutOfScope）：
// 节点锚到了范围外的资产 ⇒ 不可见；节点没有锚点 ⇒ 可见。
//
// 它与 scopedNodeVisibility（db 层那条更严的 fail-closed 口径，无锚点节点也不可见）
// 是两条不同的关系，不能互相替换：agent 的读面要的是「图读不泄露范围外资产，同时
// 不把规划者的视图清空」—— 任务起点 fact、没填 asset_ids 的事实/漏洞都没有锚点，
// 按锚点判会把它们整体清空。理由见 db/exploration.go 里那条关系的注释。
//
// 为什么自带夹具（不借用 db/exploration_scope_test.go 的）：那份夹具是兄弟单元的，
// 本文件只依赖它自己的数据形状 ⇒ 两边各自演进时互不影响。

// visibleScopeFixture 铺两个范围互不相干的任务：
//
//	assetA     —— 在 taskA 的范围内
//	assetB     —— 只在 taskB 的范围内（对 taskA 是范围外）
//	assetFree  —— 不在任何任务的范围内（对 taskA 也是范围外）
type visibleScopeFixture struct {
	taskA     int64
	taskB     int64
	expA      int64
	expB      int64
	assetA    int64
	assetB    int64
	assetFree int64
}

func newVisibleScopeFixture(t *testing.T, d *DB) visibleScopeFixture {
	t.Helper()

	taskA, err := d.CreateTask("visible A", "goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	taskB, err := d.CreateTask("visible B", "goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(taskA.ID); _ = d.DeleteTask(taskB.ID) })

	stamp := time.Now().UnixNano()
	rootA := "visible-a-" + visibleItoa(stamp) + ".example.test"
	rootB := "visible-b-" + visibleItoa(stamp) + ".example.test"
	rootFree := "visible-free-" + visibleItoa(stamp) + ".example.test"

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

	fx := visibleScopeFixture{
		taskA:     taskA.ID,
		taskB:     taskB.ID,
		expA:      taskA.ExplorationID,
		expB:      taskB.ExplorationID,
		assetA:    insert(rootA),
		assetB:    insert(rootB),
		assetFree: insert(rootFree),
	}
	t.Cleanup(func() {
		d.Exec(`DELETE FROM assets WHERE id=ANY($1::bigint[])`, []int64{ //nolint:errcheck
			fx.assetA, fx.assetB, fx.assetFree,
		})
	})
	return fx
}

func visibleItoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var buf [24]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// TestListByKindVisibleDropsOutOfScopeAnchoredNode 钉漏的那一半：锚到了范围外资产
// （别人的任务、或谁都没声明的资产）的节点不得返回。
func TestListByKindVisibleDropsOutOfScopeAnchoredNode(t *testing.T) {
	d, _, _ := testSetup(t)
	defer d.Close()

	fx := newVisibleScopeFixture(t, d)
	store := d.Exploration(fx.expA)

	outOfScope, err := store.AddNode(KindFact, map[string]any{"summary": "out of scope"}, 5, "confirmed", "worker", []int64{fx.assetB})
	if err != nil {
		t.Fatal(err)
	}
	free, err := store.AddNode(KindFact, map[string]any{"summary": "nobody's"}, 5, "confirmed", "worker", []int64{fx.assetFree})
	if err != nil {
		t.Fatal(err)
	}

	got, err := store.ListByKindVisible(fx.taskA, KindFact, 100)
	if err != nil {
		t.Fatal(err)
	}
	ids := graphNodeIDs(got)
	if ids[outOfScope] {
		t.Fatalf("锚到别的任务资产的节点不得返回（节点 %d）", outOfScope)
	}
	if ids[free] {
		t.Fatalf("锚到不在任何任务范围内的资产的节点不得返回（节点 %d）", free)
	}
}

// TestListByKindVisibleKeepsInScopeAndUnanchoredNodes 钉另一半：范围内的节点照常可见，
// 【没有锚点】的节点也照常可见 —— 无锚点节点不指向任何资产，范围判定对它没有对象。
func TestListByKindVisibleKeepsInScopeAndUnanchoredNodes(t *testing.T) {
	d, _, _ := testSetup(t)
	defer d.Close()

	fx := newVisibleScopeFixture(t, d)
	store := d.Exploration(fx.expA)

	inScope, err := store.AddNode(KindFact, map[string]any{"summary": "in scope"}, 5, "confirmed", "worker", []int64{fx.assetA})
	if err != nil {
		t.Fatal(err)
	}
	unanchored, err := store.AddNode(KindFact, map[string]any{"summary": "no anchor"}, 5, "confirmed", "worker", nil)
	if err != nil {
		t.Fatal(err)
	}

	got, err := store.ListByKindVisible(fx.taskA, KindFact, 100)
	if err != nil {
		t.Fatal(err)
	}
	ids := graphNodeIDs(got)
	if !ids[inScope] {
		t.Fatalf("范围内锚定的节点必须可见")
	}
	if !ids[unanchored] {
		t.Fatalf("无锚点的节点必须可见（它不指向任何资产；按锚点判会把规划者的视图清空）")
	}
}

// TestGetNodeWithSourcesVisibleHidesOutOfScopeNode 钉按 id 取的那条读口：按 id 取不能
// 绕过范围。
func TestGetNodeWithSourcesVisibleHidesOutOfScopeNode(t *testing.T) {
	d, _, _ := testSetup(t)
	defer d.Close()

	fx := newVisibleScopeFixture(t, d)
	store := d.Exploration(fx.expA)

	outOfScope, err := store.AddNode(KindFact, map[string]any{"summary": "out of scope"}, 5, "confirmed", "worker", []int64{fx.assetB})
	if err != nil {
		t.Fatal(err)
	}
	inScope, err := store.AddNode(KindFact, map[string]any{"summary": "in scope"}, 5, "confirmed", "worker", []int64{fx.assetA})
	if err != nil {
		t.Fatal(err)
	}

	got, err := store.GetNodeWithSourcesVisible(fx.taskA, outOfScope)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("范围外的节点按 id 取也必须取不到（节点 %d）", outOfScope)
	}
	got, err = store.GetNodeWithSourcesVisible(fx.taskA, inScope)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatalf("范围内的节点按 id 取必须取得到")
	}
}

// TestOutOfScopeAnchoredNodeIDs 钉批量判定：一次查询回答「这批节点里哪些不能给」。
func TestOutOfScopeAnchoredNodeIDs(t *testing.T) {
	d, _, _ := testSetup(t)
	defer d.Close()

	fx := newVisibleScopeFixture(t, d)
	store := d.Exploration(fx.expA)

	outOfScope, err := store.AddNode(KindFact, map[string]any{"summary": "out of scope"}, 5, "confirmed", "worker", []int64{fx.assetB})
	if err != nil {
		t.Fatal(err)
	}
	inScope, err := store.AddNode(KindFact, map[string]any{"summary": "in scope"}, 5, "confirmed", "worker", []int64{fx.assetA})
	if err != nil {
		t.Fatal(err)
	}
	unanchored, err := store.AddNode(KindFact, map[string]any{"summary": "no anchor"}, 5, "confirmed", "worker", nil)
	if err != nil {
		t.Fatal(err)
	}

	blocked, err := store.OutOfScopeAnchoredNodeIDs(fx.taskA, []int64{outOfScope, inScope, unanchored})
	if err != nil {
		t.Fatal(err)
	}
	if !blocked[outOfScope] {
		t.Fatalf("锚到范围外资产的节点必须被挡下（节点 %d）", outOfScope)
	}
	if blocked[inScope] || blocked[unanchored] {
		t.Fatalf("范围内的节点与无锚点节点不该被挡下（in=%v unanchored=%v）", blocked[inScope], blocked[unanchored])
	}
}

// TestAddNodeInScopeRefusesOutOfScopeAnchor 钉写侧：锚点写入前先判范围，越界即整单
// 拒绝；错误文案不区分「不存在」与「不在范围内」，因此拒绝不构成存在性探测器。
func TestAddNodeInScopeRefusesOutOfScopeAnchor(t *testing.T) {
	d, _, _ := testSetup(t)
	defer d.Close()

	fx := newVisibleScopeFixture(t, d)
	store := d.Exploration(fx.expA)

	before, err := store.ListByKind(KindFact, 100)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.AddNodeInScope(fx.taskA, KindFact, map[string]any{"summary": "out"}, 5, "confirmed", "worker", []int64{fx.assetB})
	if !errors.Is(err, ErrAnchorOutOfScope) {
		t.Fatalf("范围外的锚点必须被拒绝（err=%v）", err)
	}
	after, err := store.ListByKind(KindFact, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("被拒绝的写入不该留下任何节点（前 %d 个，后 %d 个）", len(before), len(after))
	}
}

// TestAddNodeInScopeKeepsInScopeAnchor 钉写侧的正半边：范围内的资产照常锚。
func TestAddNodeInScopeKeepsInScopeAnchor(t *testing.T) {
	d, _, _ := testSetup(t)
	defer d.Close()

	fx := newVisibleScopeFixture(t, d)
	store := d.Exploration(fx.expA)

	nodeID, err := store.AddNodeInScope(fx.taskA, KindFact, map[string]any{"summary": "in"}, 5, "confirmed", "worker", []int64{fx.assetA})
	if err != nil {
		t.Fatalf("范围内的锚点不该被拒绝：%v", err)
	}
	var n int
	if err := d.QueryRow(`SELECT count(*) FROM exploration_anchors WHERE node_id=$1 AND asset_id=$2`, nodeID, fx.assetA).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("范围内的资产应当恰好一条锚点（实际 %d）", n)
	}
}

// TestAddNodeInScopeWithoutTaskContextRefuses 钉 fail-closed：没有任务上下文就没有
// 授权边，不能建锚点 —— 绝不退回「全局资产池」。
func TestAddNodeInScopeWithoutTaskContextRefuses(t *testing.T) {
	d, _, _ := testSetup(t)
	defer d.Close()

	fx := newVisibleScopeFixture(t, d)
	store := d.Exploration(fx.expA)

	_, err := store.AddNodeInScope(0, KindFact, map[string]any{"summary": "in"}, 5, "confirmed", "worker", []int64{fx.assetA})
	if !errors.Is(err, ErrAnchorNoTaskContext) {
		t.Fatalf("没有任务上下文必须被拒绝（err=%v）", err)
	}
}

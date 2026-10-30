package db

import (
	"fmt"
	"testing"
	"time"

	"github.com/Autumn-27/artex/internal/scopepolicy"
)

// scopeFixture seeds two tasks with disjoint declared scopes plus three
// assets: one inside task A's scope, one inside task B's scope, one outside
// every declared scope.
type scopeFixture struct {
	taskA     int64
	taskB     int64
	inScope   int64
	otherTask int64
	outside   int64
}

func newScopeFixture(t *testing.T, d *DB) scopeFixture {
	t.Helper()
	taskA, err := d.CreateTask("scope fixture A", "goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	taskB, err := d.CreateTask("scope fixture B", "goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(taskA.ID); _ = d.DeleteTask(taskB.ID) })

	stamp := time.Now().UnixNano()
	rootA := fmt.Sprintf("scope-a-%d.example.test", stamp)
	rootB := fmt.Sprintf("scope-b-%d.example.test", stamp)
	rootOut := fmt.Sprintf("scope-out-%d.example.test", stamp)

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

	fx := scopeFixture{
		taskA:     taskA.ID,
		taskB:     taskB.ID,
		inScope:   insert(rootA),
		otherTask: insert(rootB),
		outside:   insert(rootOut),
	}
	t.Cleanup(func() {
		d.Exec(`DELETE FROM assets WHERE id=ANY($1::bigint[])`, []int64{fx.inScope, fx.otherTask, fx.outside})
	})
	return fx
}

// assetsOriginColumnExists reports whether the assets.origin column is present.
// The column belongs to the DDL unit; this unit must work both before and
// after it lands, so the tests adapt instead of assuming either state.
func assetsOriginColumnExists(t *testing.T, d *DB) bool {
	t.Helper()
	var exists bool
	if err := d.QueryRow(`SELECT EXISTS (
		SELECT 1 FROM information_schema.columns
		WHERE table_schema=current_schema() AND table_name='assets' AND column_name='origin'
	)`).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	return exists
}

// TestScopedAssetIDsKeepsInScopeAndDropsOutOfScope pins the core read-side
// guarantee: an asset the caller may not reach never comes back — not by id,
// not because another task produced it, not because it is in the shared库.
func TestScopedAssetIDsKeepsInScopeAndDropsOutOfScope(t *testing.T) {
	d, assets, _ := testSetup(t)
	defer d.Close()

	fx := newScopeFixture(t, d)

	got, err := assets.ScopedAssetIDs(fx.taskA, []int64{fx.inScope, fx.otherTask, fx.outside})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != fx.inScope {
		t.Fatalf("ScopedAssetIDs(taskA)=%v want [%d]（另一个任务的资产与范围外资产都必须被丢弃）",
			got, fx.inScope)
	}

	// Task B sees exactly its own asset — the judgment is per caller.
	gotB, err := assets.ScopedAssetIDs(fx.taskB, []int64{fx.inScope, fx.otherTask, fx.outside})
	if err != nil {
		t.Fatal(err)
	}
	if len(gotB) != 1 || gotB[0] != fx.otherTask {
		t.Fatalf("ScopedAssetIDs(taskB)=%v want [%d]", gotB, fx.otherTask)
	}
}

// TestScopedAssetIDsWithoutTaskUsesPlatformScope pins that a caller without a
// task context gets the PLATFORM scope — never "everything". The default
// platform scope is empty (fail-closed), so a context-less caller sees nothing.
func TestScopedAssetIDsWithoutTaskUsesPlatformScope(t *testing.T) {
	d, assets, _ := testSetup(t)
	defer d.Close()

	fx := newScopeFixture(t, d)

	original := scopepolicy.Policy
	t.Cleanup(func() { scopepolicy.Policy = original })

	scopepolicy.Policy.PlatformScopeMode = scopepolicy.ScopeEmpty
	got, err := assets.ScopedAssetIDs(0, []int64{fx.inScope, fx.otherTask, fx.outside})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("无任务上下文 + ScopeEmpty 必须什么都看不到，实际返回 %v", got)
	}

	// The platform-owned mode is still bounded: it may only expose assets the
	// platform itself owns. While assets.origin does not exist yet the scope
	// must fail closed (empty, no error) instead of widening to everything.
	scopepolicy.Policy.PlatformScopeMode = scopepolicy.ScopePlatformOwned
	owned, err := assets.ScopedAssetIDs(0, []int64{fx.inScope, fx.otherTask, fx.outside})
	if err != nil {
		t.Fatalf("平台范围解析不得让整个调用失败: %v", err)
	}
	if !assetsOriginColumnExists(t, d) {
		if len(owned) != 0 {
			t.Fatalf("assets.origin 缺失时必须 fail-closed（空集），实际返回 %v", owned)
		}
		return
	}
	for _, id := range owned {
		if id != fx.inScope && id != fx.otherTask && id != fx.outside {
			t.Fatalf("平台范围返回了未知资产 id %d", id)
		}
	}
}

// TestScopePolicyIsSingleSourceOfTruth pins that the scope-expansion cap and
// the "is this asset inside the caller's scope" admission decision read the
// SAME scopepolicy.Policy value: one mutation must move both behaviours
// together. Two separate definitions would let an agent widen the scope
// against one ceiling and be admitted against another.
func TestScopePolicyIsSingleSourceOfTruth(t *testing.T) {
	d, assets, _ := testSetup(t)
	defer d.Close()

	fx := newScopeFixture(t, d)

	original := scopepolicy.Policy
	t.Cleanup(func() { scopepolicy.Policy = original })

	// Baseline: the expansion cap reads the policy value.
	if got := AgentScopeExpansionCap(); got != original.MaxAgentAddedScope {
		t.Fatalf("AgentScopeExpansionCap()=%d want %d（扩权上限必须读 scopepolicy.Policy）",
			got, original.MaxAgentAddedScope)
	}

	// One mutation of the single policy value must move both behaviours.
	mutated := original
	mutated.MaxAgentAddedScope = 2
	mutated.PlatformScopeMode = scopepolicy.ScopePlatformOwned
	scopepolicy.Policy = mutated

	if got := AgentScopeExpansionCap(); got != 2 {
		t.Fatalf("AgentScopeExpansionCap()=%d want 2（改常量后扩权上限必须同步变）", got)
	}
	owned, err := assets.ScopedAssetIDs(0, []int64{fx.inScope})
	if err != nil {
		t.Fatalf("平台范围解析失败: %v", err)
	}
	if !assetsOriginColumnExists(t, d) && len(owned) != 0 {
		t.Fatalf("assets.origin 缺失时平台范围必须为空，实际 %v", owned)
	}
}

// scopeAssetDomain 读出资产的 domain。
//
// 为什么测试要自己查：fixture 只交出资产 id，而 host 口径的判定吃的是域名。
func scopeAssetDomain(t *testing.T, d *DB, id int64) string {
	t.Helper()
	var domain string
	if err := d.QueryRow(`SELECT COALESCE(domain,'') FROM assets WHERE id=$1`, id).Scan(&domain); err != nil {
		t.Fatalf("读资产域名失败: %v", err)
	}
	return domain
}

// TestScopeHostInScope 钉住 host 口径的范围判定：范围内命中、范围外不命中、
// 空/非法 host 不命中且不报错。
func TestScopeHostInScope(t *testing.T) {
	d, assets, _ := testSetup(t)
	defer d.Close()

	fx := newScopeFixture(t, d)
	inScopeHost := scopeAssetDomain(t, d, fx.inScope)
	outsideHost := scopeAssetDomain(t, d, fx.outside)
	otherHost := scopeAssetDomain(t, d, fx.otherTask)

	ok, err := assets.ScopeHostInScope(fx.taskA, inScopeHost)
	if err != nil {
		t.Fatalf("范围判定出错: %v", err)
	}
	if !ok {
		t.Fatalf("任务 A 范围内的资产 %q 必须判定为在范围内", inScopeHost)
	}

	for _, host := range []string{outsideHost, otherHost} {
		ok, err := assets.ScopeHostInScope(fx.taskA, host)
		if err != nil {
			t.Fatalf("范围判定出错: %v", err)
		}
		if ok {
			t.Fatalf("host %q 不在任务 A 的范围内，却判定为在范围内", host)
		}
	}

	// root_domain 范围含其子域（成员关系，不是字面相等）。
	ok, err = assets.ScopeHostInScope(fx.taskA, "api."+inScopeHost)
	if err != nil || !ok {
		t.Fatalf("root_domain 范围应含其子域 %q（err=%v）", "api."+inScopeHost, err)
	}

	// target_hint 是模型给的自由文本：URL 形态、带端口、带路径都要归一化后判定。
	ok, err = assets.ScopeHostInScope(fx.taskA, "https://"+inScopeHost+":8443/path?q=1")
	if err != nil {
		t.Fatalf("范围判定出错: %v", err)
	}
	if !ok {
		t.Fatalf("URL 形态的 target_hint 必须归一化后判定为在范围内: %q", inScopeHost)
	}

	// 空 / 非法 host ⇒ (false, nil)：不是范围内，也不是错误。
	for _, host := range []string{"", "   ", "not a host!!"} {
		ok, err := assets.ScopeHostInScope(fx.taskA, host)
		if err != nil || ok {
			t.Fatalf("host=%q 必须得到 (false, nil)，实际 (%v, %v)", host, ok, err)
		}
	}
}

// TestScopeDigestForTaskChangesWithScope 钉住范围摘要的两条性质：同样的范围
// 给出同一个摘要；范围一变摘要必变 —— 后者是规整器缓存失效的依据。
func TestScopeDigestForTaskChangesWithScope(t *testing.T) {
	d, assets, _ := testSetup(t)
	defer d.Close()

	fx := newScopeFixture(t, d)

	before, err := assets.ScopeDigestForTask(fx.taskA)
	if err != nil {
		t.Fatalf("算范围摘要失败: %v", err)
	}
	again, err := assets.ScopeDigestForTask(fx.taskA)
	if err != nil {
		t.Fatalf("算范围摘要失败: %v", err)
	}
	if before != again {
		t.Fatalf("同样的范围必须给出同一个摘要: %q vs %q", before, again)
	}

	// 加一条范围 ⇒ 摘要必须变。
	extra := "extra-" + scopeAssetDomain(t, d, fx.inScope)
	if err := d.Assets().upsertTaskScope(TaskScope{
		TaskID: fx.taskA, Kind: "subdomain", Domain: extra,
	}); err != nil {
		t.Fatalf("追加范围失败: %v", err)
	}
	after, err := assets.ScopeDigestForTask(fx.taskA)
	if err != nil {
		t.Fatalf("算范围摘要失败: %v", err)
	}
	if after == before {
		t.Fatal("范围变了摘要必须变 —— 否则范围收紧前生成的规整结果会被复用（范围旁路）")
	}

	// 别的任务的范围集合不同 ⇒ 摘要不同。
	other, err := assets.ScopeDigestForTask(fx.taskB)
	if err != nil || other == after {
		t.Fatalf("两个任务的有效范围不同，摘要不应相同（err=%v）", err)
	}
}

// TestScopeHostInScopeAgreesWithScopedAssetIDs 交叉钉住两处口径不漂移：凡是
// ScopedAssetIDs 认作范围内的资产，它的 host 必须也被 ScopeHostInScope 认。
//
// 为什么这条断言只有这一个方向：host 口径可以比资产口径**宽**（一个子域没有
// 对应资产行，但它落在 root_domain 范围里），但绝不能比它**窄** —— 窄了就等于
// 同一份范围被两处读成两个意思。
func TestScopeHostInScopeAgreesWithScopedAssetIDs(t *testing.T) {
	d, assets, _ := testSetup(t)
	defer d.Close()

	fx := newScopeFixture(t, d)

	for _, id := range []int64{fx.inScope, fx.otherTask, fx.outside} {
		host := scopeAssetDomain(t, d, id)
		inScope, err := assets.ScopedAssetIDs(fx.taskA, []int64{id})
		if err != nil {
			t.Fatalf("资产范围判定出错: %v", err)
		}
		hostOK, err := assets.ScopeHostInScope(fx.taskA, host)
		if err != nil {
			t.Fatalf("host 范围判定出错: %v", err)
		}
		if len(inScope) > 0 && !hostOK {
			t.Fatalf("资产 %d（host=%q）在范围内，host 口径却说不在 —— 两处口径漂移了", id, host)
		}
	}
}

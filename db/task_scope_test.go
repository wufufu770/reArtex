package db

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestStripHostPort covers the ip:port / [ipv6]:port stripping used by the
// ip/cidr scope path so a target like "10.0.188.136:3000" no longer 404s.
func TestStripHostPort(t *testing.T) {
	cases := []struct{ in, want string }{
		{"10.0.188.136:3000", "10.0.188.136"},       // the reported IP case
		{"10.0.188.136", "10.0.188.136"},            // bare IPv4 unchanged
		{"[2001:db8::1]:8080", "2001:db8::1"},       // bracketed IPv6 + port
		{"2001:db8::1", "2001:db8::1"},              // bare IPv6 unchanged (has colons)
		{" 1.2.3.4:80 ", "1.2.3.4"},                 // trims surrounding space
		{"example.com:443", "example.com"},          // domain + port → bare host
		{"api.example.com:8080", "api.example.com"}, // subdomain + port
		{"example.com", "example.com"},              // bare domain unchanged
	}
	for _, c := range cases {
		if got := stripHostPort(c.in); got != c.want {
			t.Errorf("stripHostPort(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// ---------------------------------------------------------------------------
// task_scope(kind='asset')：按资产 id 声明范围
// ---------------------------------------------------------------------------

// scopeAssetFixture 是「一个任务 + 若干资产」的夹具。
//
// 资产刻意不写 assets.task_ids：这样「在不在范围内」只由 task_scope 决定，
// 任务归属不会把断言糊弄过去（否则范围外资产可能因为归属而混进结果）。
type scopeAssetFixture struct {
	taskID int64
	expID  int64
	assets []int64
}

func newScopeAssetFixture(t *testing.T, d *DB, assetCount int) scopeAssetFixture {
	t.Helper()

	task, err := d.CreateTask("task scope asset fixture", "goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(task.ID) })

	stamp := time.Now().UnixNano()
	fx := scopeAssetFixture{taskID: task.ID, expID: task.ExplorationID}
	for i := 0; i < assetCount; i++ {
		domain := fmt.Sprintf("scope-asset-%d-%d-%d.example.test", task.ID, stamp, i)
		var id int64
		if err := d.QueryRow(`INSERT INTO assets(type,domain,root_domain,task_ids,last_seen)
			VALUES ('root_domain',$1,$1,'{}'::bigint[],now()) RETURNING id`, domain).Scan(&id); err != nil {
			t.Fatalf("插入资产 %s: %v", domain, err)
		}
		fx.assets = append(fx.assets, id)
	}
	t.Cleanup(func() {
		d.Exec(`DELETE FROM assets WHERE id=ANY($1::bigint[])`, fx.assets)
	})
	return fx
}

// TestTaskScopeAssetMakesAssetVisible pins that an asset declared in scope becomes
// visible to the agent's asset listing — and, in the same case, that the coverage
// denominator counts it.
//
// 为什么要一个用例同时断这两头：task_scope 的「范围内」在平台里有三份物化
// （scopeTargetCTE / contextCoverageCTE / covTargetCTE）。只改其中一份不会报错，
// 只会让「列表里看得到、覆盖度算不出」（或反之）—— 那正是这个 kind 存在的理由。
// 断在一个用例里，两头就不可能再各走各的。
func TestTaskScopeAssetMakesAssetVisible(t *testing.T) {
	d, assets, _ := testSetup(t)
	defer d.Close()

	fx := newScopeAssetFixture(t, d, 2)
	inScope, outOfScope := fx.assets[0], fx.assets[1]

	if err := d.AddTaskScopeAsset(fx.taskID, inScope); err != nil {
		t.Fatalf("声明任务资产范围失败: %v", err)
	}

	// ① agent 的资产列表（scopeTargetCTE）：只有被声明的那一条可见。
	got, err := assets.GetByIDsInScope(fx.taskID, []int64{inScope, outOfScope})
	if err != nil {
		t.Fatalf("按范围取资产失败: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("范围内资产 %d 条，期望 1 条（只有被声明的那条）", len(got))
	}
	if got[0].ID != inScope {
		t.Fatalf("范围内资产 id = %d，期望 %d", got[0].ID, inScope)
	}

	// ② 覆盖度分母（covTargetCTE）：被声明的资产计入分母，范围外的不计。
	cov, err := assets.TaskCoverage(fx.taskID, fx.expID)
	if err != nil {
		t.Fatalf("计算覆盖度失败: %v", err)
	}
	if cov.Denominator != 1 {
		t.Fatalf("覆盖度分母 = %d，期望 1（只算被声明的资产）", cov.Denominator)
	}
	untested, total, err := assets.ListUntestedAssets(fx.taskID, fx.expID, "", 10, 0)
	if err != nil {
		t.Fatalf("列出未测资产失败: %v", err)
	}
	if total != 1 || len(untested) != 1 || untested[0].ID != inScope {
		t.Fatalf("未测资产 = %+v (total=%d)，期望只有被声明的资产 %d", untested, total, inScope)
	}

	// ③ 直接来源任务口径的覆盖度（contextCoverageCTE）：同一口径也必须算得出来。
	covWithSources, err := assets.TaskCoverageWithSources(fx.taskID)
	if err != nil {
		t.Fatalf("计算（含来源任务）覆盖度失败: %v", err)
	}
	if covWithSources.Denominator != 1 {
		t.Fatalf("（含来源任务）覆盖度分母 = %d，期望 1", covWithSources.Denominator)
	}
	untestedWithSources, totalWithSources, err := assets.ListUntestedAssetsWithSources(fx.taskID, "", 10, 0)
	if err != nil {
		t.Fatalf("列出（含来源任务）未测资产失败: %v", err)
	}
	if totalWithSources != 1 || len(untestedWithSources) != 1 || untestedWithSources[0].ID != inScope {
		t.Fatalf("（含来源任务）未测资产 = %+v (total=%d)，期望只有被声明的资产 %d",
			untestedWithSources, totalWithSources, inScope)
	}
}

// TestTaskScopeAssetAllowsDistinctAssetsInSameTask pins the index trap.
//
// task_scope 的唯一索引 uq_task_scope_v2 是【表达式唯一索引】：改定义必须原地
// DROP + 重建（CREATE ... IF NOT EXISTS 对同名索引不改定义）。若只追加一个新索引
// 而留着旧的 v2，同一 task 下两个不同资产（domain/net/company/value 全空、
// 只差 asset_id）会被旧 v2 判为重复 ⇒ 第二个资产插不进去。
//
// 这正是「天真做法」会挂掉的用例，所以它必须留在套件里。
func TestTaskScopeAssetAllowsDistinctAssetsInSameTask(t *testing.T) {
	d, _, _ := testSetup(t)
	defer d.Close()

	fx := newScopeAssetFixture(t, d, 2)
	a1, a2 := fx.assets[0], fx.assets[1]

	if err := d.AddTaskScopeAsset(fx.taskID, a1); err != nil {
		t.Fatalf("声明资产 %d 失败: %v", a1, err)
	}
	if err := d.AddTaskScopeAsset(fx.taskID, a2); err != nil {
		t.Fatalf("声明资产 %d 失败（同 task 下的第二个不同资产必须能插进去）: %v", a2, err)
	}

	var rows int
	if err := d.QueryRow(`SELECT count(*) FROM task_scope WHERE task_id=$1 AND kind='asset'`, fx.taskID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 2 {
		t.Fatalf("落库的 asset 范围行 %d 条，期望 2 条", rows)
	}
}

// TestTaskScopeAssetDuplicateIsIdempotent pins the duplicate semantics: 同一个 task
// 重复声明同一个 asset 是【幂等】的 —— 不报错、不重复落库。
//
// 选幂等而不是报错，是为了让「发布任务」这个动作可以安全重试。
func TestTaskScopeAssetDuplicateIsIdempotent(t *testing.T) {
	d, _, _ := testSetup(t)
	defer d.Close()

	fx := newScopeAssetFixture(t, d, 1)
	assetID := fx.assets[0]

	if err := d.AddTaskScopeAsset(fx.taskID, assetID); err != nil {
		t.Fatalf("首次声明失败: %v", err)
	}
	if err := d.AddTaskScopeAsset(fx.taskID, assetID); err != nil {
		t.Fatalf("重复声明必须幂等（不得报错）: %v", err)
	}

	var rows int
	if err := d.QueryRow(`SELECT count(*) FROM task_scope WHERE task_id=$1 AND kind='asset' AND asset_id=$2`,
		fx.taskID, assetID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("同一 asset 的范围行 %d 条，期望 1 条（重复声明不得落第二条）", rows)
	}
}

// TestAddTaskScopeAssetRejectsUnknownTask pins that declaring scope for a task that
// does not exist fails cleanly (可判定的错误 + 不留残行），而不是静默无效。
func TestAddTaskScopeAssetRejectsUnknownTask(t *testing.T) {
	d, _, _ := testSetup(t)
	defer d.Close()

	fx := newScopeAssetFixture(t, d, 1)

	gone, err := d.CreateTask("task scope asset missing task", "goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.DeleteTask(gone.ID); err != nil {
		t.Fatal(err)
	}

	err = d.AddTaskScopeAsset(gone.ID, fx.assets[0])
	if err == nil {
		t.Fatalf("任务不存在时必须报错")
	}
	if !errors.Is(err, ErrTaskScopeUnknownTask) {
		t.Fatalf("错误必须可判定为 %v，实际 %v", ErrTaskScopeUnknownTask, err)
	}
	if !strings.Contains(err.Error(), "任务不存在") {
		t.Fatalf("错误文案必须写明任务不存在，实际 %v", err)
	}
	var rows int
	if err := d.QueryRow(`SELECT count(*) FROM task_scope WHERE task_id=$1 AND kind='asset'`, gone.ID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("任务不存在时不得留下范围行（实际 %d 条）", rows)
	}
}

// TestAddTaskScopeAssetRejectsUnknownAsset pins that declaring scope for an asset
// that does not exist fails cleanly: 一条永远匹配不到任何东西的范围行比直接报错
// 更难排查。
func TestAddTaskScopeAssetRejectsUnknownAsset(t *testing.T) {
	d, _, _ := testSetup(t)
	defer d.Close()

	fx := newScopeAssetFixture(t, d, 1)
	assetID := fx.assets[0]
	if _, err := d.Exec(`DELETE FROM assets WHERE id=$1`, assetID); err != nil {
		t.Fatal(err)
	}

	err := d.AddTaskScopeAsset(fx.taskID, assetID)
	if err == nil {
		t.Fatalf("资产不存在时必须报错")
	}
	if !errors.Is(err, ErrTaskScopeUnknownAsset) {
		t.Fatalf("错误必须可判定为 %v，实际 %v", ErrTaskScopeUnknownAsset, err)
	}
	if !strings.Contains(err.Error(), "资产不存在") {
		t.Fatalf("错误文案必须写明资产不存在，实际 %v", err)
	}
	var rows int
	if err := d.QueryRow(`SELECT count(*) FROM task_scope WHERE task_id=$1 AND kind='asset'`, fx.taskID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("资产不存在时不得留下范围行（实际 %d 条）", rows)
	}
}

// TestTaskScopeKindCheckRejectsUnknownKind pins that the DDL whitelist is real:
// 未知 kind 不得落库。反向也断一下 —— asset 必须是被接受的合法 kind。
func TestTaskScopeKindCheckRejectsUnknownKind(t *testing.T) {
	d, _, _ := testSetup(t)
	defer d.Close()

	fx := newScopeAssetFixture(t, d, 1)

	if _, err := d.Exec(`INSERT INTO task_scope(task_id, kind, source) VALUES ($1, 'bogus_kind', 'manual')`,
		fx.taskID); err == nil {
		t.Fatalf("未知 kind 必须被 CHECK 约束拒绝")
	}
	if err := d.AddTaskScopeAsset(fx.taskID, fx.assets[0]); err != nil {
		t.Fatalf("asset 必须是被接受的合法 kind: %v", err)
	}
}

// TestTaskScopeAssetIsNotBoundByAgentExpansionCap pins that declaring scope for
// concrete assets is an OPERATOR-side decision: it is not bound by the agent's
// expansion budget (scopepolicy.Policy.MaxAgentAddedScope) and does not consume it.
//
// 反过来也要钉住：agent 自己仍然不得把范围扩到 kind='asset' —— 那等于让它自己挑
// 授权边界。
func TestTaskScopeAssetIsNotBoundByAgentExpansionCap(t *testing.T) {
	d, assets, _ := testSetup(t)
	defer d.Close()

	limit := AgentScopeExpansionCap()
	fx := newScopeAssetFixture(t, d, limit+3)

	for _, assetID := range fx.assets {
		if err := d.AddTaskScopeAsset(fx.taskID, assetID); err != nil {
			t.Fatalf("操作者侧声明资产范围不受 agent 扩权上限约束（资产 %d 被拒）: %v", assetID, err)
		}
	}
	if got := agentScopeRowCount(t, assets, fx.taskID); got != 0 {
		t.Fatalf("人工声明的范围行不得计入 agent 的扩张预算（实际 %d 条）", got)
	}

	if _, err := assets.AddAgentScope(fx.taskID, "asset", "123", "test", "agent"); err == nil {
		t.Fatalf("agent 不得自行把范围扩到 kind='asset'（那是自己挑授权边界）")
	}
}

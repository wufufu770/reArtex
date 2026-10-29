package db

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/internal/scopepolicy"
)

// scopeExpansionFixture 建一个干净的任务，供扩权上限用例使用。
//
// 用例自己清理：任务删掉时 task_scope 随之级联删除（见 schema.sql）。
func scopeExpansionFixture(t *testing.T, d *DB) int64 {
	t.Helper()
	task, err := d.CreateTask("scope expansion fixture", "goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(task.ID) })
	return task.ID
}

// agentScopeRowCount 返回该任务里由 agent 侧写入（source='auto'/'agent'）的范围行数。
//
// 人工写的行（source='manual'）不计入：agent 的扩张预算与人工设范围互不相干。
func agentScopeRowCount(t *testing.T, assets *AssetStore, taskID int64) int {
	t.Helper()
	rows, err := assets.ListTaskScope(taskID)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, r := range rows {
		if r.Source == "auto" || r.Source == "agent" {
			n++
		}
	}
	return n
}

// TestAgentScopeExpansionIsCapped pins the write-side ceiling: one agent run may
// not widen a task's scope past the code-level budget
// (scopepolicy.Policy.MaxAgentAddedScope, read through db.AgentScopeExpansionCap).
//
// Before the ceiling existed an agent could add_task_scope forever, then act
// "inside" the scope it had just widened — every scope guard would be
// self-defeating.
func TestAgentScopeExpansionIsCapped(t *testing.T) {
	d, assets, _ := testSetup(t)
	defer d.Close()

	taskID := scopeExpansionFixture(t, d)

	limit := AgentScopeExpansionCap()
	if limit <= 0 {
		t.Fatalf("扩权上限必须是正数，实际 %d", limit)
	}

	attempts := limit + 5
	accepted, refused := 0, 0
	for i := 0; i < attempts; i++ {
		domain := fmt.Sprintf("cap-%d-%d.example.test", taskID, i)
		if _, err := assets.AddAgentScope(taskID, "subdomain", domain, "test", "agent"); err != nil {
			refused++
			continue
		}
		accepted++
	}

	if accepted > limit {
		t.Fatalf("有效扩权 %d 条，超过上限 %d", accepted, limit)
	}
	if got := agentScopeRowCount(t, assets, taskID); got > limit {
		t.Fatalf("落库的 agent 范围行 %d 条，超过上限 %d", got, limit)
	}
	if refused == 0 {
		t.Fatalf("超限的扩权必须被拒：尝试 %d 次、上限 %d，一次都没拒说明上限没生效", attempts, limit)
	}
}

// TestAgentScopeRefusesTreeWideKinds pins the kind whitelist: an agent may not
// declare company or root_domain scope on its own.
//
// Those two kinds pull the WHOLE asset tree in (a root_domain scope matches
// every asset whose root_domain equals it) — that is the boundary being
// overturned, not a neighbour being added. It has to stay a human decision.
func TestAgentScopeRefusesTreeWideKinds(t *testing.T) {
	d, assets, _ := testSetup(t)
	defer d.Close()

	taskID := scopeExpansionFixture(t, d)

	for _, kind := range []string{"root_domain", "company"} {
		if _, err := assets.AddAgentScope(taskID, kind, "example.test", "test", "agent"); err == nil {
			t.Fatalf("agent 不得自行把范围扩到 %s（会把整棵资产树拉进来）", kind)
		}
	}

	rows, err := assets.ListTaskScope(taskID)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Kind == "root_domain" || r.Kind == "company" {
			t.Fatalf("被拒的扩权不得落库: %+v", r)
		}
	}
}

// TestAutoScopeExpansionIsCapped pins that the automatic scope (insert_assets /
// worker asset registration) is capped by the same budget as the explicit one.
//
// The automatic path is the one an agent triggers by simply registering assets,
// so leaving it unbounded would make the explicit ceiling meaningless.
func TestAutoScopeExpansionIsCapped(t *testing.T) {
	d, assets, _ := testSetup(t)
	defer d.Close()

	taskID := scopeExpansionFixture(t, d)

	limit := AgentScopeExpansionCap()
	for i := 0; i < limit+5; i++ {
		domain := fmt.Sprintf("auto-%d-%d.example.test", taskID, i)
		// 自动扩范围不得报错：它跑在 worker 的资产登记循环里，报错会把 worker 带崩。
		if err := assets.AddAutoScope(taskID, "subdomain", domain, "", ""); err != nil {
			t.Fatalf("自动扩范围不得返回错误: %v", err)
		}
	}

	if got := agentScopeRowCount(t, assets, taskID); got > limit {
		t.Fatalf("自动扩范围落库 %d 行，超过上限 %d", got, limit)
	}
}

// TestAutoScopeRefusesTreeWideKind pins that the automatic path honours the
// kind whitelist too: registering a root_domain asset must not silently widen
// the task to the whole tree under that root domain.
//
// The entry is dropped (truncated), not failed — the worker must keep running.
func TestAutoScopeRefusesTreeWideKind(t *testing.T) {
	d, assets, _ := testSetup(t)
	defer d.Close()

	taskID := scopeExpansionFixture(t, d)

	if err := assets.AddAutoScope(taskID, "root_domain", "example.test", "", ""); err != nil {
		t.Fatalf("自动扩范围不得返回错误: %v", err)
	}

	rows, err := assets.ListTaskScope(taskID)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Kind == "root_domain" {
			t.Fatalf("agent 侧不得落 root_domain 范围行: %+v", r)
		}
	}
}

// TestManualScopePathIsNotCapped pins that the HUMAN path is a different
// function and is NOT bound by the agent ceiling.
//
// Setting the authorization scope is a human decision; capping it with the
// agent budget would make the platform unable to declare a scope at all.
// (Discover → approve → include, never discover-then-include.)
func TestManualScopePathIsNotCapped(t *testing.T) {
	d, assets, _ := testSetup(t)
	defer d.Close()

	taskID := scopeExpansionFixture(t, d)

	limit := AgentScopeExpansionCap()
	total := limit + 5
	for i := 0; i < total; i++ {
		domain := fmt.Sprintf("manual-%d-%d.example.test", taskID, i)
		ts, err := assets.AddManualScope(taskID, "subdomain", domain, "人工设范围")
		if err != nil {
			t.Fatalf("人工设范围不得受 agent 上限约束: %v", err)
		}
		if ts.Source != "manual" {
			t.Fatalf("人工设范围的 source = %q，期望 'manual'", ts.Source)
		}
	}

	rows, err := assets.ListTaskScope(taskID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != total {
		t.Fatalf("人工设范围落库 %d 条，期望 %d 条（不受 agent 上限约束）", len(rows), total)
	}

	// 人工写的行不消耗 agent 的扩张预算：agent 之后仍能用满自己的额度。
	agentAdded := 0
	for i := 0; i < limit; i++ {
		domain := fmt.Sprintf("after-manual-%d-%d.example.test", taskID, i)
		if _, err := assets.AddAgentScope(taskID, "subdomain", domain, "test", "agent"); err != nil {
			t.Fatalf("人工设范围不应吃掉 agent 的额度（第 %d 条被拒）: %v", i, err)
		}
		agentAdded++
	}
	if agentAdded != limit {
		t.Fatalf("agent 有效扩权 %d 条，期望 %d 条", agentAdded, limit)
	}
}

// TestAgentScopeExpansionCapFollowsPolicy pins that the ceiling is read from
// the single source of truth (scopepolicy.Policy), not from a second copy.
//
// The expansion cap and the "is this inside scope?" admission check must read
// the SAME value — two definitions would let an agent widen the scope against
// one ceiling and be admitted against another.
func TestAgentScopeExpansionCapFollowsPolicy(t *testing.T) {
	d, assets, _ := testSetup(t)
	defer d.Close()

	original := scopepolicy.Policy
	t.Cleanup(func() { scopepolicy.Policy = original })

	taskID := scopeExpansionFixture(t, d)

	// 上限读的就是策略值本身。
	if got, want := AgentScopeExpansionCap(), original.MaxAgentAddedScope; got != want {
		t.Fatalf("AgentScopeExpansionCap()=%d，期望 %d（扩权上限必须读 scopepolicy.Policy）", got, want)
	}

	// 改策略值 ⇒ 执法行为同步变化（不是只改了一个数字）。
	scopepolicy.Policy.MaxAgentAddedScope = 2
	if got := AgentScopeExpansionCap(); got != 2 {
		t.Fatalf("AgentScopeExpansionCap()=%d，期望 2（改常量后扩权上限必须同步变）", got)
	}

	accepted := 0
	for i := 0; i < 6; i++ {
		domain := fmt.Sprintf("policy-%d-%d.example.test", taskID, i)
		if _, err := assets.AddAgentScope(taskID, "subdomain", domain, "test", "agent"); err != nil {
			continue
		}
		accepted++
	}
	if accepted != 2 {
		t.Fatalf("策略上限改成 2 后有效扩权 %d 条，期望 2 条", accepted)
	}
	if got := agentScopeRowCount(t, assets, taskID); got != 2 {
		t.Fatalf("落库的 agent 范围行 %d 条，期望 2 条", got)
	}
}

// TestAgentScopeKindsFollowPolicy pins that the kind whitelist is read from the
// same single source of truth.
func TestAgentScopeKindsFollowPolicy(t *testing.T) {
	d, assets, _ := testSetup(t)
	defer d.Close()

	original := scopepolicy.Policy
	t.Cleanup(func() { scopepolicy.Policy = original })

	taskID := scopeExpansionFixture(t, d)

	// 白名单里去掉 subdomain ⇒ subdomain 立即不可加。
	scopepolicy.Policy.MaxAgentAddedKinds = []string{"ip", "cidr"}
	if _, err := assets.AddAgentScope(taskID, "subdomain", "kinds.example.test", "test", "agent"); err == nil {
		t.Fatalf("白名单去掉 subdomain 后仍可扩权（白名单必须读 scopepolicy.Policy）")
	}
	// 白名单里的 ip 仍然可加。
	if _, err := assets.AddAgentScope(taskID, "ip", "203.0.113.7", "test", "agent"); err != nil {
		t.Fatalf("白名单内的 kind 必须可加: %v", err)
	}
}

// TestScopeExpansionDenialIsAudited pins that a refusal leaves a trace: the
// truncation has to be auditable, otherwise "the agent widened its own scope"
// is invisible after the fact.
func TestScopeExpansionDenialIsAudited(t *testing.T) {
	d, assets, _ := testSetup(t)
	defer d.Close()

	taskID := scopeExpansionFixture(t, d)

	var before int
	if err := d.QueryRow(`SELECT count(*) FROM audit_events WHERE action='scope_expansion_denied'`).Scan(&before); err != nil {
		t.Fatal(err)
	}

	if _, err := assets.AddAgentScope(taskID, "root_domain", "audited.example.test", "test", "agent"); err == nil {
		t.Fatalf("root_domain 必须被拒")
	}

	var after int
	if err := d.QueryRow(`SELECT count(*) FROM audit_events WHERE action='scope_expansion_denied'`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != before+1 {
		t.Fatalf("拒绝必须留审计：审计条数 %d → %d", before, after)
	}

	var reason string
	if err := d.QueryRow(`SELECT reason FROM audit_events WHERE action='scope_expansion_denied'
ORDER BY seq DESC LIMIT 1`).Scan(&reason); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reason, "root_domain") {
		t.Fatalf("审计原因必须写明被拒的 kind，实际 %q", reason)
	}
}

// TestScopeExpansionDenialIsTyped pins that the refusal is machine-readable:
// the tool layer has to tell the agent WHY the scope was not widened.
func TestScopeExpansionDenialIsTyped(t *testing.T) {
	d, assets, _ := testSetup(t)
	defer d.Close()

	taskID := scopeExpansionFixture(t, d)

	_, err := assets.AddAgentScope(taskID, "company", "某公司", "test", "agent")
	var denied *ScopeExpansionDeniedError
	if !errors.As(err, &denied) {
		t.Fatalf("拒绝必须返回 *ScopeExpansionDeniedError，实际 %v", err)
	}
	if denied.Kind != "company" || denied.TaskID != taskID {
		t.Fatalf("拒绝错误必须带上 kind 与 task_id，实际 %+v", denied)
	}
	if denied.Error() == "" {
		t.Fatalf("拒绝错误必须有可展示的中文原因")
	}
}

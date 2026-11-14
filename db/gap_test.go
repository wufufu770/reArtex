package db

// 覆盖缺口的真库集成测试（0.14.3 批次二 F4）：无 ARTEX_PG_DSN 自动跳过。
// 钉住：open 计算（无 open 意图锚=缺口）/ targeted 演进（获得锚）/ closed
// 不复活 / 人工闭合与清单面。

import (
	"testing"
)

func TestCoverageGapLifecycle(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	_ = d.EnsureGapSchema()
	suffix := nowSuffix()

	companyID, _, _, _, _, err := d.Companies().CreateCompanyWithScope(
		"缺口公司"+suffix, "", []ScopeInput{{Kind: "domain", Value: "gap-" + suffix + ".example.com"}}, "集成测试造数")
	if err != nil {
		t.Fatalf("create company: %v", err)
	}
	t.Cleanup(func() { _, _ = d.Companies().DeleteCompanyWithAssets(companyID, true) })
	task, err := d.CreateTaskWithOptions("缺口任务 "+suffix, "验证", TaskCreateOptions{CompanyIDs: []int64{companyID}})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	tid := task.ID
	t.Cleanup(func() {
		_, _ = d.Exec(`DELETE FROM coverage_gaps WHERE task_id=$1`, tid)
		_, _ = d.Exec(`UPDATE tasks SET deleted_at=now() WHERE id=$1`, tid)
	})
	var eid int64
	if err := d.QueryRow(`SELECT exploration_id FROM tasks WHERE id=$1`, tid).Scan(&eid); err != nil || eid <= 0 {
		t.Fatalf("read exploration_id: %v", err)
	}

	// 夹具资产 + 链接。
	var aid int64
	if err := d.QueryRow(`INSERT INTO assets (type, domain, origin) VALUES ('subdomain', $1, 'agent') RETURNING id`,
		"gapfix-"+suffix+".example.com").Scan(&aid); err != nil {
		t.Fatalf("seed asset: %v", err)
	}
	t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM assets WHERE id=$1`, aid) })
	if _, err := d.Exec(`INSERT INTO task_asset_links (task_id, asset_id) VALUES ($1,$2)`, tid, aid); err != nil {
		t.Fatalf("link asset: %v", err)
	}

	// ① 无 open 意图锚 → 计算出 open 缺口；重算幂等不堆积。
	if _, err := d.ComputeCoverageGaps(tid); err != nil {
		t.Fatalf("compute 1: %v", err)
	}
	var gid int64
	var state string
	if err := d.QueryRow(`SELECT id, state FROM coverage_gaps WHERE task_id=$1 AND asset_id=$2`, tid, aid).Scan(&gid, &state); err != nil {
		t.Fatalf("read gap: %v", err)
	}
	if state != "open" {
		t.Fatalf("缺口应 open，got %s", state)
	}
	if _, err := d.ComputeCoverageGaps(tid); err != nil {
		t.Fatalf("compute 2: %v", err)
	}
	var n int
	if err := d.QueryRow(`SELECT count(*) FROM coverage_gaps WHERE task_id=$1`, tid).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("重算应幂等（1 行），got %d", n)
	}

	// ② 意图锚该资产 → 重算后 open → targeted。
	var nid int64
	if err := d.QueryRow(`INSERT INTO exploration_nodes (exploration_id, kind, payload) VALUES ($1,'intent','{"summary":"gap 演进验证"}') RETURNING id`, eid).Scan(&nid); err != nil {
		t.Fatalf("seed intent: %v", err)
	}
	t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM exploration_nodes WHERE exploration_id=$1`, eid) })
	if _, err := d.Exec(`INSERT INTO exploration_anchors (node_id, asset_id) VALUES ($1,$2)`, nid, aid); err != nil {
		t.Fatalf("anchor: %v", err)
	}
	if _, err := d.ComputeCoverageGaps(tid); err != nil {
		t.Fatalf("compute 3: %v", err)
	}
	if err := d.QueryRow(`SELECT state FROM coverage_gaps WHERE id=$1`, gid).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "targeted" {
		t.Fatalf("获得锚后应 targeted，got %s", state)
	}

	// ③ 人工闭合 → 重算不复活（closed 恒定）。
	if err := d.CloseCoverageGap(gid); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := d.ComputeCoverageGaps(tid); err != nil {
		t.Fatalf("compute 4: %v", err)
	}
	if err := d.QueryRow(`SELECT state FROM coverage_gaps WHERE id=$1`, gid).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "closed" {
		t.Fatalf("closed 不应被重扫复活，got %s", state)
	}
	// 二次闭合应拒。
	if err := d.CloseCoverageGap(gid); err == nil {
		t.Fatal("已闭合再闭合应拒")
	}

	// ④ 清单面（state 过滤）。
	openList, err := d.ListCoverageGaps(tid, "open", 0)
	if err != nil || len(openList) != 0 {
		t.Fatalf("open 清单应空: n=%d err=%v", len(openList), err)
	}
	all, err := d.ListCoverageGaps(tid, "", 0)
	if err != nil || len(all) != 1 {
		t.Fatalf("全量清单应 1 条: n=%d err=%v", len(all), err)
	}
}

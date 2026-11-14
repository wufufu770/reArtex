package db

// 意图冲突仲裁的真库集成测试（0.14.1 批次二）：无 ARTEX_PG_DSN 自动跳过。
// 钉住：归一登记幂等 / 枚举与理由校验 / 状态机（open→adjudicated→reopen→
// frozen 的全部迁移与拒绝路径）。

import (
	"testing"
)

func TestIntentConflictLifecycle(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	_ = d.EnsureIntent2Schema()
	suffix := nowSuffix()

	companyID, _, _, _, _, err := d.Companies().CreateCompanyWithScope(
		"仲裁公司"+suffix, "", []ScopeInput{{Kind: "domain", Value: "arbiter-" + suffix + ".example.com"}}, "集成测试造数")
	if err != nil {
		t.Fatalf("create company: %v", err)
	}
	t.Cleanup(func() { _, _ = d.Companies().DeleteCompanyWithAssets(companyID, true) })
	task, err := d.CreateTaskWithOptions("仲裁任务 "+suffix, "验证", TaskCreateOptions{CompanyIDs: []int64{companyID}})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	tid := task.ID
	t.Cleanup(func() { _, _ = d.Exec(`UPDATE tasks SET deleted_at=now() WHERE id=$1`, tid) })
	var eid int64
	if err := d.QueryRow(`SELECT exploration_id FROM tasks WHERE id=$1`, tid).Scan(&eid); err != nil || eid <= 0 {
		t.Fatalf("read exploration_id: %v (eid=%d)", err, eid)
	}
	mkNode := func(tag string) int64 {
		var nid int64
		if err := d.QueryRow(`INSERT INTO exploration_nodes (exploration_id, kind, payload) VALUES ($1,'intent','{"summary":"` + tag + `"}') RETURNING id`, eid).Scan(&nid); err != nil {
			t.Fatalf("seed intent node: %v", err)
		}
		return nid
	}
	nA := mkNode("a" + suffix)
	nB := mkNode("b" + suffix)
	hi, lo := nA, nB
	if hi < lo {
		hi, lo = lo, hi
	}
	t.Cleanup(func() {
		_, _ = d.Exec(`DELETE FROM intent_conflicts WHERE exploration_id=$1`, eid)
		_, _ = d.Exec(`DELETE FROM exploration_nodes WHERE exploration_id=$1`, eid)
	})

	// ① 登记：归一对（大,小 → 小,大）；同对重复登记幂等（不新建行）。
	id1, created, err := d.RecordConflict(eid, hi, lo, "asset_mutex", []byte(`{"k":"v"}`))
	if err != nil || !created {
		t.Fatalf("首次登记应 created: id=%d created=%v err=%v", id1, created, err)
	}
	var aID, bID int64
	if err := d.QueryRow(`SELECT a_node_id, b_node_id FROM intent_conflicts WHERE id=$1`, id1).Scan(&aID, &bID); err != nil {
		t.Fatalf("read pair: %v", err)
	}
	if aID != lo || bID != hi {
		t.Fatalf("归一对应小 id 在前，got (%d,%d)", aID, bID)
	}
	if _, created, _ := d.RecordConflict(eid, lo, hi, "asset_mutex", nil); created {
		t.Fatalf("同对重复登记应幂等不新建")
	}

	// ② 裁决校验：非法 verdict 拒、空理由拒。
	if err := d.ResolveConflict(id1, "hack", "x", "arbiter"); err == nil {
		t.Fatal("非法 verdict 应拒")
	}
	if err := d.ResolveConflict(id1, "proceed_a", "", "arbiter"); err == nil {
		t.Fatal("空理由应拒")
	}

	// ③ 正常裁决 → reopen（reopen_count+1、verdict 清空）→ 再裁决。
	if err := d.ResolveConflict(id1, "proceed_b", "验证先行", "host-rule"); err != nil {
		t.Fatalf("正常裁决: %v", err)
	}
	if err := d.ReopenConflict(id1); err != nil {
		t.Fatalf("reopen: %v", err)
	}
	var rc int
	var vd *string
	if err := d.QueryRow(`SELECT reopen_count, verdict FROM intent_conflicts WHERE id=$1`, id1).Scan(&rc, &vd); err != nil {
		t.Fatalf("read reopen: %v", err)
	}
	if rc != 1 || vd != nil {
		t.Fatalf("reopen 后 count=1 verdict=nil，got %d %v", rc, vd)
	}

	// ④ defer 熔断：open→frozen；frozen 不可再 defer（Freeze 幂等拒）。
	if err := d.FreezeConflict(id1); err != nil {
		t.Fatalf("freeze after reopen（open 态）: %v", err)
	}
	if err := d.FreezeConflict(id1); err == nil {
		t.Fatal("frozen 再 freeze 应拒")
	}
	// frozen 可再裁决（人工兜底路径）。
	if err := d.ResolveConflict(id1, "proceed_a", "人工兜底", "host"); err != nil {
		t.Fatalf("frozen 态人工裁决应通过: %v", err)
	}

	// ⑤ 清单面。
	list, err := d.ListConflictsByExploration(eid, "", 0)
	if err != nil || len(list) != 1 {
		t.Fatalf("清单应 1 条: n=%d err=%v", len(list), err)
	}
}

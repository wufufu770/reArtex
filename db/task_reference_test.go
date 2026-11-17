package db

// 0.15.12 跨任务同目标参考：SiblingTaskDigests 的范围口径与排除语义。
// 同公司（tasks.company_id 直配或 task_scope kind=company）⇒ 兄弟可见；
// 不同公司不可见；自身排除；发现计数与摘要随行。

import (
	"fmt"
	"testing"
	"time"
)

func TestSiblingTaskDigests(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v)", err)
	}
	defer d.Close()

	suffix := time.Now().UnixNano()
	companyShared, _, err := d.Companies().UpsertCompany(fmt.Sprintf("Sibling Ref Shared %d", suffix), "")
	if err != nil {
		t.Fatal(err)
	}
	companyOther, _, err := d.Companies().UpsertCompany(fmt.Sprintf("Sibling Ref Other %d", suffix), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = d.Exec(`DELETE FROM companies WHERE id IN ($1,$2)`, companyShared, companyOther)
	})

	taskA, err := d.CreateTaskWithOptions("sibling ref A", "目标A szq-ref", TaskCreateOptions{CompanyIDs: []int64{companyShared}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(taskA.ID) })
	taskB, err := d.CreateTaskWithOptions("sibling ref B", "目标B szq-ref", TaskCreateOptions{CompanyIDs: []int64{companyShared}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(taskB.ID) })
	taskC, err := d.CreateTaskWithOptions("sibling ref C", "目标C szq-ref", TaskCreateOptions{CompanyIDs: []int64{companyOther}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(taskC.ID) })

	// 给 B 挂一条 finding 节点（走真表：exploration_nodes kind='finding'）。
	var expB int64
	if err := d.QueryRow(`SELECT exploration_id FROM tasks WHERE id=$1`, taskB.ID).Scan(&expB); err != nil {
		t.Fatal(err)
	}
	var findingID int64
	if err := d.QueryRow(`
INSERT INTO exploration_nodes(exploration_id, kind, payload, state, owner, origin)
VALUES ($1,'finding',$2,'confirmed','','') RETURNING id`,
		expB, `{"summary":"szq-ref-b-finding 摘要正文"}`).Scan(&findingID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM exploration_nodes WHERE id=$1`, findingID) })

	digests, err := d.SiblingTaskDigests(taskA.ID, 5)
	if err != nil {
		t.Fatalf("SiblingTaskDigests: %v", err)
	}
	byID := map[int64]SiblingTaskDigest{}
	for _, g := range digests {
		byID[g.ID] = g
	}
	if _, ok := byID[taskA.ID]; ok {
		t.Fatal("自身必须被排除")
	}
	b, ok := byID[taskB.ID]
	if !ok {
		t.Fatalf("同公司兄弟 B 必须在结果里，got %+v", digests)
	}
	if b.Findings != 1 || len(b.TopFindings) != 1 || b.TopFindings[0] != "szq-ref-b-finding 摘要正文" {
		t.Fatalf("B 的发现面不符: %+v", b)
	}
	if _, ok := byID[taskC.ID]; ok {
		t.Fatal("不同公司的 C 不得出现在结果里")
	}

	// B 的视角：A 是兄弟（方向无关性）。
	digestsB, err := d.SiblingTaskDigests(taskB.ID, 5)
	if err != nil {
		t.Fatalf("SiblingTaskDigests(B): %v", err)
	}
	found := false
	for _, g := range digestsB {
		if g.ID == taskA.ID {
			found = true
		}
		if g.ID == taskB.ID {
			t.Fatal("B 的查询里不得出现 B 自身")
		}
	}
	if !found {
		t.Fatalf("B 视角下 A 必须在结果里，got %+v", digestsB)
	}

	// 无公司范围的任务：空集且不报错。
	taskD, err := d.CreateTask("sibling ref D", "no company", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(taskD.ID) })
	digestsD, err := d.SiblingTaskDigests(taskD.ID, 5)
	if err != nil {
		t.Fatalf("无公司范围不应报错: %v", err)
	}
	if len(digestsD) != 0 {
		t.Fatalf("无公司范围应返回空集，got %d 条", len(digestsD))
	}
}

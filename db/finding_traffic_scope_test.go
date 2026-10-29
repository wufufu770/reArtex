package db

import (
	"errors"
	"strings"
	"testing"
)

// findingScopeFixture 造两个互不相干的任务，各自带一个漏洞，另有一个无归属漏洞。
type findingScopeFixture struct {
	taskA  int64
	taskB  int64
	nodeA  int64
	findA  int64
	nodeB  int64
	findB  int64
	orphan int64 // task_id 为 NULL 的漏洞（归属不明）
	orphN  int64 // 它的探索节点 id
}

// newFindingScopeFixture 返回夹具与清理函数。清理必须由用例用 defer 触发
// （t.Cleanup 跑在用例返回之后，那时 defer d.Close() 已经把连接关了，清理会
// 静默失败并把脏数据留在库里）。
func newFindingScopeFixture(t *testing.T, d *DB) (findingScopeFixture, func()) {
	t.Helper()
	taskA, err := d.CreateTask("finding scope A", "goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	taskB, err := d.CreateTask("finding scope B", "goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	cleanup := func() {
		_ = d.DeleteTask(taskA.ID)
		_ = d.DeleteTask(taskB.ID)
	}
	recA, err := d.Exploration(taskA.ExplorationID).RecordFinding(t.Context(), RecordFindingInput{
		TaskID: taskA.ID, ExplorationID: taskA.ExplorationID, Summary: "A 的漏洞", Severity: "low",
	})
	if err != nil {
		t.Fatal(err)
	}
	recB, err := d.Exploration(taskB.ExplorationID).RecordFinding(t.Context(), RecordFindingInput{
		TaskID: taskB.ID, ExplorationID: taskB.ExplorationID, Summary: "B 的漏洞", Severity: "low",
	})
	if err != nil {
		t.Fatal(err)
	}
	// 归属不明的漏洞：node_id 有值（可被寻址），task_id 为 NULL。
	orphan, err := d.AddFinding(0, recB.NodeID, "TEST", "orphan", "low", "无归属漏洞", "", "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	return findingScopeFixture{
		taskA: taskA.ID, taskB: taskB.ID,
		nodeA: recA.NodeID, findA: recA.FindingID,
		nodeB: recB.NodeID, findB: recB.FindingID,
		orphan: orphan, orphN: recB.NodeID,
	}, cleanup
}

// TestSetFindingReportVersionByNodeIDInScopeRejectsCrossTask pins the L4 core
// guarantee: a report write must not be able to reach a finding that belongs
// to another task. findings.task_id is the finding's owning task — the only
// column that says who owns it — so it is the scope relation for a write.
func TestSetFindingReportVersionByNodeIDInScopeRejectsCrossTask(t *testing.T) {
	d, _, _ := testSetup(t)
	defer d.Close()
	fx, cleanup := newFindingScopeFixture(t, d)
	defer cleanup()

	// B 的调用者写 A 的漏洞 —— 必须被拒。
	if _, err := d.SetFindingReportVersionByNodeIDInScope(t.Context(), fx.taskB, fx.nodeA, "越权报告", nil); !errors.Is(err, ErrFindingOutOfScope) {
		t.Fatalf("跨任务写报告必须被拒，实际错误：%v", err)
	}
	f, err := d.GetFinding(fx.findA)
	if err != nil {
		t.Fatal(err)
	}
	if f.Report != "" {
		t.Fatalf("被拒的写入不得落库，report=%q", f.Report)
	}

	// 同一任务的写入必须照常成功（不能把正常路径一起堵死）。
	n, err := d.SetFindingReportVersionByNodeIDInScope(t.Context(), fx.taskA, fx.nodeA, "本任务报告", nil)
	if err != nil {
		t.Fatalf("同任务写报告必须成功：%v", err)
	}
	if n != 1 {
		t.Fatalf("同任务写报告 rows=%d want 1", n)
	}
	f, err = d.GetFinding(fx.findA)
	if err != nil {
		t.Fatal(err)
	}
	if f.Report != "本任务报告" {
		t.Fatalf("同任务报告未落库：%q", f.Report)
	}
}

// TestSetFindingReportVersionByNodeIDInScopeRequiresTaskContext pins the
// fail-closed half of L4: without a task context there is no authorization
// edge, so the write must be refused instead of reaching the global table.
func TestSetFindingReportVersionByNodeIDInScopeRequiresTaskContext(t *testing.T) {
	d, _, _ := testSetup(t)
	defer d.Close()
	fx, cleanup := newFindingScopeFixture(t, d)
	defer cleanup()

	if _, err := d.SetFindingReportVersionByNodeIDInScope(t.Context(), 0, fx.nodeA, "无上下文报告", nil); err == nil {
		t.Fatal("没有任务上下文时必须拒绝写报告")
	}
	f, err := d.GetFinding(fx.findA)
	if err != nil {
		t.Fatal(err)
	}
	if f.Report != "" {
		t.Fatalf("被拒的写入不得落库，report=%q", f.Report)
	}
}

// TestSetFindingReportVersionByNodeIDInScopeRejectsUnownedFinding pins that a
// finding without an owning task (task_id IS NULL) is never writable: its
// ownership is unknown, and unknown ownership is not ownership.
func TestSetFindingReportVersionByNodeIDInScopeRejectsUnownedFinding(t *testing.T) {
	d, _, _ := testSetup(t)
	defer d.Close()
	fx, cleanup := newFindingScopeFixture(t, d)
	defer cleanup()

	if _, err := d.SetFindingReportVersionByNodeIDInScope(t.Context(), fx.taskA, fx.orphN, "x", nil); !errors.Is(err, ErrFindingOutOfScope) {
		t.Fatalf("无归属漏洞必须被拒，实际错误：%v", err)
	}
	orphan, err := d.GetFinding(fx.orphan)
	if err != nil {
		t.Fatal(err)
	}
	if orphan.Report != "" {
		t.Fatalf("被拒的写入不得落库，report=%q", orphan.Report)
	}
}

// TestTrafficScopeHostsOnlyListsInScopeHosts pins the L2 scope relation at the
// data layer: the hosts a task may read are the hosts of the assets inside its
// scope — a task never sees another task's hosts, and a caller without a task
// sees nothing at all.
func TestTrafficScopeHostsOnlyListsInScopeHosts(t *testing.T) {
	d, assets, _ := testSetup(t)
	defer d.Close()

	taskA, err := d.CreateTask("traffic scope A", "goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	taskB, err := d.CreateTask("traffic scope B", "goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.DeleteTask(taskA.ID); _ = d.DeleteTask(taskB.ID) }()

	const rootA = "traffic-scope-a.example"
	const rootB = "traffic-scope-b.example"
	for _, sc := range []struct {
		tid int64
		dom string
	}{{taskA.ID, rootA}, {taskB.ID, rootB}} {
		if err := assets.upsertTaskScope(TaskScope{TaskID: sc.tid, Kind: "root_domain", Domain: sc.dom}); err != nil {
			t.Fatalf("seed scope: %v", err)
		}
	}
	insert := func(domain string) int64 {
		if _, err := d.Exec(`INSERT INTO assets(type,domain,root_domain,last_seen)
VALUES ('subdomain',$1,$1,now()) ON CONFLICT DO NOTHING`, domain); err != nil {
			t.Fatalf("insert asset: %v", err)
		}
		var id int64
		if err := d.QueryRow(`SELECT id FROM assets WHERE domain=$1 LIMIT 1`, domain).Scan(&id); err != nil {
			t.Fatalf("read asset id: %v", err)
		}
		return id
	}
	inA := insert(rootA)
	inB := insert(rootB)
	defer d.Exec(`DELETE FROM assets WHERE id=ANY($1::bigint[])`, []int64{inA, inB}) //nolint:errcheck

	hostsA, err := d.TrafficScopeHosts(taskA.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !containsHost(hostsA, rootA) {
		t.Fatalf("任务 A 必须能读自己范围内的 host：%v", hostsA)
	}
	if containsHost(hostsA, rootB) {
		t.Fatalf("任务 A 不得看到任务 B 的 host：%v", hostsA)
	}
	hostsB, err := d.TrafficScopeHosts(taskB.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !containsHost(hostsB, rootB) || containsHost(hostsB, rootA) {
		t.Fatalf("任务 B 的 host 集合不对：%v", hostsB)
	}
	// 没有任务上下文 → 平台范围（默认空）→ 什么都看不到。
	none, err := d.TrafficScopeHosts(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(none) != 0 {
		t.Fatalf("没有任务上下文时不得看到任何 host：%v", none)
	}
}

// containsHost 报告 host 集合里是否有该 host（去空格、大小写不敏感）。
func containsHost(hosts []string, host string) bool {
	for _, h := range hosts {
		if strings.EqualFold(strings.TrimSpace(h), strings.TrimSpace(host)) {
			return true
		}
	}
	return false
}

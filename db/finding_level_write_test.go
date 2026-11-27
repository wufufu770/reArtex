package db

import (
	"context"
	"errors"
	"strings"
	"testing"
)

const levelWriteHTTP = "HTTP/1.1 500 Internal Server Error\nbody shows the injected row"

func TestRecordFindingCapsLevels(t *testing.T) {
	d := openTestDB(t)
	defer d.Close()
	task, err := d.CreateTask("分级写入", "目标", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(task.ID) })
	es := d.Exploration(task.ExplorationID)
	ctx := context.Background()

	_, err = es.RecordFinding(ctx, RecordFindingInput{
		TaskID: task.ID, VulnClass: "sqli", Severity: SeverityCritical,
		Summary: "拒绝", Evidence: levelWriteHTTP, Worker: "w",
		ImpactLevel: 5, EvidenceLevel: 2,
	})
	if !errors.Is(err, ErrFindingLevelInvalid) {
		t.Fatalf("impact 5 err=%v", err)
	}
	_, err = es.RecordFinding(ctx, RecordFindingInput{
		TaskID: task.ID, VulnClass: "sqli", Severity: SeverityCritical,
		Summary: "拒绝", Evidence: levelWriteHTTP, Worker: "w",
		ImpactLevel: 2, EvidenceLevel: 5,
	})
	if !errors.Is(err, ErrFindingLevelInvalid) {
		t.Fatalf("evidence 5 err=%v", err)
	}
	var nodes, rows int
	if err := d.QueryRow(`SELECT count(*) FROM exploration_nodes WHERE exploration_id=$1 AND kind='finding'`, task.ExplorationID).Scan(&nodes); err != nil {
		t.Fatal(err)
	}
	if err := d.QueryRow(`SELECT count(*) FROM findings WHERE task_id=$1`, task.ID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if nodes != 0 || rows != 0 {
		t.Fatalf("非法分级不应落库 nodes=%d rows=%d", nodes, rows)
	}

	out, err := es.RecordFinding(ctx, RecordFindingInput{
		TaskID: task.ID, VulnClass: "sqli", Severity: SeverityCritical,
		Summary: "可验证注入", Evidence: levelWriteHTTP, Worker: "w",
		ImpactLevel: 4, EvidenceLevel: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.ImpactLevel != 4 || out.EvidenceLevel != 3 || out.EvidenceLevelRequested != 4 || out.EvidenceInsufficient {
		t.Fatalf("降到 L3 后 critical 不再标证据不足：%+v", out)
	}
	if !strings.Contains(out.EvidenceLevelReason, "reproduced") {
		t.Fatalf("降级原因：%q", out.EvidenceLevelReason)
	}
	got, err := d.GetFinding(out.FindingID)
	if err != nil || got == nil || got.ImpactLevel != 4 || got.EvidenceLevel != 3 || got.EvidenceLevelRequested != 4 {
		t.Fatalf("库内：%+v err=%v", got, err)
	}

	kept, err := es.RecordFinding(ctx, RecordFindingInput{
		TaskID: task.ID, VulnClass: "xss", Severity: SeverityHigh,
		Summary: "只有说明", Evidence: "这是一段超过二十个字的说明，但没有引用任何工具输出。", Worker: "w",
		ImpactLevel: 2, EvidenceLevel: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if kept.EvidenceLevel != 2 || kept.EvidenceLevelReason != "" || !kept.EvidenceInsufficient {
		t.Fatalf("L2 不应被降级：%+v", kept)
	}

	omitted, err := es.RecordFinding(ctx, RecordFindingInput{
		TaskID: task.ID, VulnClass: "info", Severity: SeverityInfo,
		Summary: "省略分级", Evidence: levelWriteHTTP, Worker: "w",
	})
	if err != nil {
		t.Fatal(err)
	}
	if omitted.ImpactLevel != 1 || omitted.EvidenceLevel != 1 || omitted.EvidenceInsufficient {
		t.Fatalf("省略应是 L1/L1 且 info 不标证据不足：%+v", omitted)
	}

	if _, err := d.Exec(`INSERT INTO finding_retests(finding_id,status,verdict,snapshot) VALUES ($1,'completed','reproduced','{}')`, out.FindingID); err != nil {
		t.Fatal(err)
	}
	if err := d.RefreshFindingEvidenceLevel(out.FindingID); err != nil {
		t.Fatal(err)
	}
	raised, err := d.GetFinding(out.FindingID)
	if err != nil || raised == nil || raised.EvidenceLevel != 4 || raised.EvidenceLevelRequested != 4 || raised.EvidenceLevelReason != "" || raised.ImpactLevel != 4 {
		t.Fatalf("复测 reproduced 后应升到 L4：%+v err=%v", raised, err)
	}
	if err := d.RefreshFindingEvidenceLevel(omitted.FindingID); err != nil {
		t.Fatal(err)
	}
	still, err := d.GetFinding(omitted.FindingID)
	if err != nil || still == nil || still.EvidenceLevel != 1 {
		t.Fatalf("没申请 L4 的记录不应被复测抬上去：%+v err=%v", still, err)
	}
}

package db

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestEvidenceSufficientMatrix(t *testing.T) {
	severities := []string{SeverityCritical, SeverityHigh, SeverityMedium, SeverityLow, SeverityInfo, ""}
	for _, sev := range severities {
		for level := 1; level <= 4; level++ {
			got := EvidenceSufficient(sev, level)
			want := true
			if sev == SeverityCritical || sev == SeverityHigh {
				want = level >= 3
			}
			if got != want {
				t.Errorf("EvidenceSufficient(%q, %d)=%v, want %v", sev, level, got, want)
			}
		}
	}
}

func TestCapEvidenceLevel(t *testing.T) {
	longHTTP := "HTTP/1.1 500 Internal Server Error\nbody shows the injected row"
	longFence := "命令输出如下：\n```\nid\nuid=0(root)\n```"
	longProse := "这是一段超过二十个字的说明，但没有引用任何工具输出。"
	short := "HTTP/1.1 200"

	cases := []struct {
		name       string
		requested  int
		evidence   string
		reproduced bool
		applied    int
		wantErr    bool
		reasonHas  string
	}{
		{name: "omit", requested: 0, evidence: longHTTP, applied: 1},
		{name: "keep L2", requested: 2, evidence: longProse, applied: 2},
		{name: "L3 prose capped", requested: 3, evidence: longProse, applied: 2, reasonHas: "工具输出引用"},
		{name: "L3 http kept", requested: 3, evidence: longHTTP, applied: 3},
		{name: "L3 fence kept", requested: 3, evidence: longFence, applied: 3},
		{name: "L4 without retest", requested: 4, evidence: longHTTP, applied: 3, reasonHas: "reproduced"},
		{name: "L4 with retest", requested: 4, evidence: longHTTP, reproduced: true, applied: 4},
		{name: "L4 short evidence", requested: 4, evidence: short, applied: 2, reasonHas: "工具输出引用"},
		{name: "empty stays L1", requested: 3, evidence: "  ", applied: 1, reasonHas: "L1"},
		{name: "five rejected", requested: 5, evidence: longHTTP, wantErr: true},
		{name: "negative rejected", requested: -1, evidence: longHTTP, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			applied, reason, err := CapEvidenceLevel(tc.requested, tc.evidence, tc.reproduced)
			if tc.wantErr {
				if !errors.Is(err, ErrFindingLevelInvalid) {
					t.Fatalf("err=%v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if applied != tc.applied {
				t.Fatalf("applied=%d want %d reason=%q", applied, tc.applied, reason)
			}
			if tc.reasonHas == "" && reason != "" {
				t.Fatalf("不应降级，reason=%q", reason)
			}
			if tc.reasonHas != "" && !strings.Contains(reason, tc.reasonHas) {
				t.Fatalf("reason=%q 应包含 %q", reason, tc.reasonHas)
			}
		})
	}
	if utf8Len("HTTP/1.1 200 OK") >= evidenceChainMinRunes {
		t.Fatal("短 HTTP 行不应达到 L3 长度门槛")
	}
}

func utf8Len(s string) int {
	n := 0
	for range s {
		n++
	}
	return n
}

func TestNormalizeArchivedFindingLevels(t *testing.T) {
	raw, err := json.Marshal([]map[string]any{{"id": 1, "evidence_version": nil}})
	if err != nil {
		t.Fatal(err)
	}
	out, err := normalizeArchivedFindingVersions(raw)
	if err != nil {
		t.Fatal(err)
	}
	var rows []map[string]any
	if err := json.Unmarshal(out, &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows=%d", len(rows))
	}
	for _, key := range []string{"impact_level", "evidence_level", "evidence_level_requested"} {
		if rows[0][key].(float64) != 1 {
			t.Fatalf("%s=%v", key, rows[0][key])
		}
	}
	if rows[0]["evidence_level_reason"] != "" {
		t.Fatalf("reason=%v", rows[0]["evidence_level_reason"])
	}
	if rows[0]["evidence_version"].(float64) != 0 {
		t.Fatalf("evidence_version=%v", rows[0]["evidence_version"])
	}
}

func TestFindingLevelsRoundTrip(t *testing.T) {
	d := openTestDB(t)
	defer d.Close()
	if err := d.EnsureFindingLevelsSchema(); err != nil {
		t.Fatal(err)
	}
	if err := d.EnsureFindingLevelsSchema(); err != nil {
		t.Fatal(err)
	}

	var impactDefault, evidenceDefault, requestedDefault string
	var impactNull, evidenceNull, reasonNull string
	err := d.QueryRow(`
SELECT column_default, is_nullable FROM information_schema.columns
WHERE table_name='findings' AND column_name='impact_level'`).Scan(&impactDefault, &impactNull)
	if err != nil {
		t.Fatal(err)
	}
	err = d.QueryRow(`
SELECT column_default, is_nullable FROM information_schema.columns
WHERE table_name='findings' AND column_name='evidence_level'`).Scan(&evidenceDefault, &evidenceNull)
	if err != nil {
		t.Fatal(err)
	}
	err = d.QueryRow(`
SELECT column_default, is_nullable FROM information_schema.columns
WHERE table_name='findings' AND column_name='evidence_level_reason'`).Scan(&requestedDefault, &reasonNull)
	if err != nil {
		t.Fatal(err)
	}
	if impactNull != "NO" || evidenceNull != "NO" || reasonNull != "NO" {
		t.Fatalf("nullable impact=%s evidence=%s reason=%s", impactNull, evidenceNull, reasonNull)
	}
	if !strings.Contains(impactDefault, "1") || !strings.Contains(evidenceDefault, "1") {
		t.Fatalf("defaults impact=%q evidence=%q", impactDefault, evidenceDefault)
	}
	for _, name := range []string{"ck_findings_impact_level", "ck_findings_evidence_level", "ck_findings_evidence_level_requested"} {
		var n int
		if err := d.QueryRow(`SELECT count(*) FROM pg_constraint WHERE conname=$1`, name).Scan(&n); err != nil || n != 1 {
			t.Fatalf("约束 %s: n=%d err=%v", name, n, err)
		}
	}

	task, err := d.CreateTask("分级测试", "目标", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(task.ID) })
	if n, err := d.MaxImpactLevel(task.ID); err != nil || n != 0 {
		t.Fatalf("空任务 MaxImpactLevel=%d err=%v", n, err)
	}
	if n, err := d.MaxImpactLevel(0); err != nil || n != 0 {
		t.Fatalf("task 0 MaxImpactLevel=%d err=%v", n, err)
	}

	es := d.Exploration(task.ExplorationID)
	nodeID, err := es.AddNode(KindFinding, map[string]any{"severity": "high"}, 5, "pending", "worker", nil)
	if err != nil {
		t.Fatal(err)
	}
	id, err := d.AddFinding(task.ID, nodeID, "sqli", "注入", SeverityHigh, "摘要", "证据", "worker", nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := d.GetFinding(id)
	if err != nil || got == nil {
		t.Fatal(err)
	}
	if got.ImpactLevel != 1 || got.EvidenceLevel != 1 || got.EvidenceLevelRequested != 1 || got.EvidenceLevelReason != "" {
		t.Fatalf("新建应是 L1/L1：%+v", got)
	}

	if err := d.SetFindingLevels(id, 5, 2); !errors.Is(err, ErrFindingLevelInvalid) {
		t.Fatalf("5 应拒绝，err=%v", err)
	}
	if err := d.SetFindingLevels(id, 0, 2); !errors.Is(err, ErrFindingLevelInvalid) {
		t.Fatalf("0 应拒绝，err=%v", err)
	}
	still, _ := d.GetFinding(id)
	if still.ImpactLevel != 1 || still.EvidenceLevel != 1 {
		t.Fatalf("非法写入不应改行：%+v", still)
	}
	if err := d.SetFindingLevels(id, 4, 2); err != nil {
		t.Fatal(err)
	}
	updated, err := d.GetFinding(id)
	if err != nil || updated.ImpactLevel != 4 || updated.EvidenceLevel != 2 || updated.EvidenceLevelRequested != 2 || updated.EvidenceLevelReason != "" {
		t.Fatalf("写回后：%+v err=%v", updated, err)
	}
	var payload []byte
	if err := d.QueryRow(`SELECT payload FROM exploration_nodes WHERE id=$1`, nodeID).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(payload, &body); err != nil {
		t.Fatal(err)
	}
	if body["impact_level"].(float64) != 4 || body["evidence_level"].(float64) != 2 {
		t.Fatalf("节点 payload 未镜像：%s", payload)
	}

	id2, err := d.AddFinding(task.ID, 0, "xss", "反射", SeverityLow, "摘要", "证据", "worker", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.SetFindingLevels(id2, 2, 1); err != nil {
		t.Fatal(err)
	}
	if n, err := d.MaxImpactLevel(task.ID); err != nil || n != 4 {
		t.Fatalf("MaxImpactLevel=%d err=%v", n, err)
	}
	listed, err := d.ListFindings(50)
	if err != nil {
		t.Fatal(err)
	}
	seen := false
	for _, f := range listed {
		if f.ID == id {
			seen = true
			if f.ImpactLevel != 4 || f.EvidenceLevel != 2 {
				t.Fatalf("列表未带分级：%+v", f)
			}
		}
	}
	if !seen {
		t.Fatal("列表里没有这条 finding")
	}
	if err := d.SetFindingLevels(999999999, 1, 1); !errors.Is(err, ErrFindingNotFound) {
		t.Fatalf("不存在的行 err=%v", err)
	}

	if _, err := d.Exec(`UPDATE findings SET impact_level=0 WHERE id=$1`, id); err == nil {
		t.Fatal("CHECK 应拒绝 impact_level=0")
	}
	if _, err := d.Exec(`UPDATE findings SET evidence_level=5 WHERE id=$1`, id); err == nil {
		t.Fatal("CHECK 应拒绝 evidence_level=5")
	}
	if _, err := d.Exec(`UPDATE findings SET evidence_level_requested=9 WHERE id=$1`, id); err == nil {
		t.Fatal("CHECK 应拒绝 evidence_level_requested=9")
	}

	var raw []byte
	if err := d.QueryRow(`SELECT to_jsonb(f) FROM findings f WHERE id=$1`, id).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var row map[string]any
	if err := json.Unmarshal(raw, &row); err != nil {
		t.Fatal(err)
	}
	full, err := json.Marshal([]any{row})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"impact_level", "evidence_level", "evidence_level_requested", "evidence_level_reason"} {
		delete(row, key)
	}
	strippedSrc, err := json.Marshal([]any{row})
	if err != nil {
		t.Fatal(err)
	}
	stripped, err := normalizeArchivedFindingVersions(strippedSrc)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`DELETE FROM findings WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	tx, err := d.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := insertArchiveRows(tx, "findings", stripped); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	restored, err := d.GetFinding(id)
	if err != nil || restored == nil {
		t.Fatal(err)
	}
	if restored.ImpactLevel != 1 || restored.EvidenceLevel != 1 || restored.EvidenceLevelRequested != 1 || restored.EvidenceLevelReason != "" || restored.Summary == "" {
		t.Fatalf("旧快照应回落 L1/L1 且正文还在：%+v", restored)
	}
	if _, err := d.Exec(`DELETE FROM findings WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	tx, err = d.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := insertArchiveRows(tx, "findings", full); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	fullRow, err := d.GetFinding(id)
	if err != nil || fullRow == nil || fullRow.ImpactLevel != 4 || fullRow.EvidenceLevel != 2 {
		t.Fatalf("完整快照应恢复 4/2：%+v err=%v", fullRow, err)
	}
}

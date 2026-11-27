package agent

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/db"
)

func TestReportFindingLevels(t *testing.T) {
	d := testDB(t)
	defer d.Close()
	task, err := d.CreateTask("工具分级", "目标", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(task.ID) })
	ts := NewToolSet(nil, "worker")
	ts.ts = d.Exploration(task.ExplorationID)
	ts.taskID = task.ID

	call := func(body string) string {
		t.Helper()
		res, err := ts.addFinding().Call(t.Context(), json.RawMessage(body), nil)
		if err != nil {
			t.Fatal(err)
		}
		return res.Flatten()
	}
	base := `"vulnclass":"sqli","severity":"critical","summary":"可验证注入","evidence":"这是一段超过二十个字的说明，但没有引用任何工具输出。"`

	got := call(`{` + base + `,"impact_level":5}`)
	if !strings.Contains(got, "impact_level") || strings.HasPrefix(got, "finding recorded:") {
		t.Fatalf("impact_level=5 应拒绝且不入库：%s", got)
	}
	got = call(`{` + base + `,"evidence_level":5}`)
	if !strings.Contains(got, "evidence_level") || strings.HasPrefix(got, "finding recorded:") {
		t.Fatalf("evidence_level=5 应拒绝：%s", got)
	}
	got = call(`{` + base + `,"impact_level":2.5}`)
	if strings.HasPrefix(got, "finding recorded:") {
		t.Fatalf("非整数应拒绝：%s", got)
	}
	var n int
	if err := d.QueryRow(`SELECT count(*) FROM findings WHERE task_id=$1`, task.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("拒绝之后不应有 findings 行，得到 %d", n)
	}

	got = call(`{` + base + `,"impact_level":4,"evidence_level":2}`)
	if !strings.HasPrefix(got, "finding recorded:") {
		t.Fatalf("critical + L2 应入库：%s", got)
	}
	var out struct {
		FindingID            int64  `json:"finding_id,string"`
		ImpactLevel          int    `json:"impact_level"`
		EvidenceLevel        int    `json:"evidence_level"`
		EvidenceInsufficient bool   `json:"evidence_insufficient"`
		Reason               string `json:"evidence_level_reason"`
	}
	if err := json.Unmarshal([]byte(strings.SplitN(got, "\n", 2)[1]), &out); err != nil {
		t.Fatal(err)
	}
	if out.ImpactLevel != 4 || out.EvidenceLevel != 2 || !out.EvidenceInsufficient || out.Reason != "" {
		t.Fatalf("详情口径：%+v", out)
	}
	row, err := d.GetFinding(out.FindingID)
	if err != nil || row == nil || row.Severity != "critical" || row.ImpactLevel != 4 || row.EvidenceLevel != 2 {
		t.Fatalf("库内：%+v err=%v", row, err)
	}
	if db.EvidenceSufficient(row.Severity, row.EvidenceLevel) {
		t.Fatal("critical + L2 应判定为证据不足")
	}
}

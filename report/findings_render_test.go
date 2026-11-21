package report

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
)

func renderFixture() []*db.DBFinding {
	return []*db.DBFinding{
		{ID: 1, VulnClass: "idor", Name: "订单越权可读", Severity: "high", Status: "confirmed",
			Summary: "A 号会话可读 B 号订单", Evidence: "GET /api/order/456\n200 OK 含他人数据",
			TaskDescription: "订单系统核查", AssetIDs: []int64{7}},
		{ID: 2, VulnClass: "info_disclosure", Name: "", Severity: "info", Status: "pending",
			Summary: "报错栈回显", Evidence: "stack trace shows /app/internal/..."},
	}
}

func TestFindingsTXTSections(t *testing.T) {
	out := FindingsTXT(renderFixture(), time.Unix(0, 0).UTC())
	s := string(out)
	for _, want := range []string{"[1] 订单越权可读", "严重等级：高", "处置状态：已确认", "任务：订单系统核查", "GET /api/order/456", "[2] info_disclosure"} {
		if !strings.Contains(s, want) {
			t.Fatalf("TXT 缺少 %q", want)
		}
	}
}

func TestFindingsSARIFShape(t *testing.T) {
	body, err := FindingsSARIF(renderFixture(), time.Unix(0, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Version string `json:"version"`
		Runs    []struct {
			Tool struct {
				Driver struct {
					Name  string   `json:"name"`
					Rules []any    `json:"rules"`
				} `json:"driver"`
			} `json:"tool"`
			Results []struct {
				RuleId string `json:"ruleId"`
				Level  string `json:"level"`
			} `json:"results"`
		} `json:"runs"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("SARIF 不是合法 JSON: %v", err)
	}
	if doc.Version != "2.1.0" || len(doc.Runs) != 1 {
		t.Fatalf("SARIF 版本/runs 形态不对: %s", body[:120])
	}
	driver := doc.Runs[0].Tool.Driver
	if driver.Name != "reArtex" || len(driver.Rules) != 2 {
		t.Fatalf("driver/rules 形态不对: %d rules", len(driver.Rules))
	}
	results := doc.Runs[0].Results
	if len(results) != 2 {
		t.Fatalf("应有 2 条 result: %d", len(results))
	}
	// severity 映射：high→error，info→note。
	if results[0].Level != "error" || results[1].Level != "note" {
		t.Fatalf("level 映射不对: %s %s", results[0].Level, results[1].Level)
	}
}

func TestFindingsHTMLEscapeAndShape(t *testing.T) {
	fs := renderFixture()
	fs[0].Evidence = `<script>alert("x&y")</script>`
	out := FindingsHTML(fs, time.Unix(0, 0).UTC())
	s := string(out)
	if strings.Contains(s, "<script>alert") {
		t.Fatal("HTML 证据必须转义（不得出现可执行 script 注入）")
	}
	for _, want := range []string{"&lt;script&gt;", "订单越权可读", "sev-high", "生成时间"} {
		if !strings.Contains(s, want) {
			t.Fatalf("HTML 缺少 %q", want)
		}
	}
}

package report

import (
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
)

// DOC / 打印即 PDF 渲染器回归（0.15.43 #8）。

func docTestFindings() []*db.DBFinding {
	taskID := int64(1)
	created := time.Now()
	return []*db.DBFinding{{
		ID: 1, TaskID: &taskID, VulnClass: "sqli", Name: "SQL 注入",
		Severity: "high", Summary: "联合注入可读库", Evidence: "payload…",
		Status: "confirmed", CreatedAt: created,
	}}
}

func TestFindingsDocHasWordProgId(t *testing.T) {
	doc := string(FindingsDoc(docTestFindings(), time.Now()))
	if !strings.Contains(doc, `name="ProgId" content="Word.Document"`) {
		t.Error("DOC 缺 Word ProgId 声明（Word 按网页模式打开）")
	}
	if !strings.Contains(doc, "SQL 注入") {
		t.Error("DOC 丢发现内容")
	}
}

func TestFindingsPrintHTMLInjectsPrintBlock(t *testing.T) {
	html := string(FindingsPrintHTML(docTestFindings(), time.Now()))
	if !strings.Contains(html, "@media print") || !strings.Contains(html, "size:A4") {
		t.Error("打印视图缺 A4 打印样式")
	}
	if !strings.Contains(html, "window.print()") {
		t.Error("打印视图缺自动唤起打印对话框")
	}
	if strings.Count(html, "</head>") != 1 || strings.Count(html, "<body>") != 1 {
		t.Error("注入点破坏了文档结构")
	}
}

package server

import (
	"archive/zip"
	"bytes"
	"strings"
	"testing"
)

// docx 生成器单测（0.15.29）：Markdown 子集 → OOXML 的转换纪律钉死。

func openDocx(t *testing.T, body []byte) map[string]string {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatalf("docx 不是合法 zip: %v", err)
	}
	parts := map[string]string{}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(rc)
		_ = rc.Close()
		parts[f.Name] = buf.String()
	}
	// 最小包完整性：五个 part 一个不能少。
	for _, name := range []string{"[Content_Types].xml", "_rels/.rels", "word/document.xml", "word/_rels/document.xml.rels", "word/styles.xml"} {
		if _, ok := parts[name]; !ok {
			t.Fatalf("docx 缺少 part: %s", name)
		}
	}
	return parts
}

func TestRenderDocxMarkdownSubset(t *testing.T) {
	md := "# 概述\n\n这是一个 **SQL 注入** 漏洞，参数 `id`。\n\n## 复现步骤\n\n- 第一步\n- 第二步\n\n1. 有序甲\n2. 有序乙\n\n```sql\nSELECT * FROM users WHERE id='1'\n```\n\n普通段落 <script>alert(1)</script> 保持转义。"
	body, err := renderDocx(md)
	if err != nil {
		t.Fatal(err)
	}
	parts := openDocx(t, body)
	doc := parts["word/document.xml"]

	for _, want := range []string{
		`<w:pStyle w:val="Heading1"/>`, // 一级标题
		`<w:pStyle w:val="Heading2"/>`, // 二级标题
		`SQL 注入`,                      // 加粗文本保留
		`<w:b/>`,                       // 加粗属性
		`ListParagraph`,                // 列表样式
		`>• <`,                         // 无序 marker（独立 run）
		`>第一步<`,                        // 列表项文本（marker 后独立 run）
		`>1. <`,                        // 有序 marker
		`>有序甲<`,                        // 有序项文本
		`CodeBlock`,                    // 代码块样式
		`SELECT * FROM users`,          // 代码块内容
		`&lt;script&gt;`,               // XML 转义
	} {
		if !strings.Contains(doc, want) {
			t.Fatalf("document.xml 缺少 %q", want)
		}
	}
	// 行内代码走 InlineCode 字符样式。
	if !strings.Contains(doc, `InlineCode`) {
		t.Fatal("行内代码应挂 InlineCode 字符样式")
	}
}

func TestMarkdownToBlocksSubsetsOutside(t *testing.T) {
	// 子集外语法（表格行/HTML）原样落段落——宁朴素勿丢内容。
	blocks := markdownToBlocks("| a | b |\n|---|---|\n| 1 | 2 |\n\n<h1>raw</h1>")
	if len(blocks) != 5 {
		t.Fatalf("应有 5 个块，得到 %d", len(blocks))
	}
	for _, bl := range blocks {
		if bl.kind != "p" {
			t.Fatalf("子集外语法应全部落段落，得到 %q", bl.kind)
		}
	}
}

func TestDocxInlineRunsOrderAndNesting(t *testing.T) {
	// 行内样式按命中位置切分；加粗嵌行内代码以先命中者为准（不支持嵌套）。
	runs := docxInlineRuns("前缀 **加粗** 中缀 `code` 尾巴")
	if len(runs) != 5 {
		t.Fatalf("应切 5 个 run，得到 %d: %v", len(runs), runs)
	}
	if runs[0][0] != "前缀 " || runs[1][0] != "加粗" || runs[1][1] != "1" {
		t.Fatalf("加粗 run 切分错误: %v", runs[:2])
	}
	if runs[3][0] != "code" || runs[3][2] != "1" {
		t.Fatalf("行内代码 run 切分错误: %v", runs[3])
	}
	// 纯文本单 run。
	if runs := docxInlineRuns("plain"); len(runs) != 1 || runs[0][0] != "plain" {
		t.Fatalf("纯文本应单 run: %v", runs)
	}
}

func TestDocxLargeReportRenders(t *testing.T) {
	// 大报告（>1000 行）渲染不炸、zip 完整——reporter 长报告的现实规模。
	var b strings.Builder
	for i := 0; i < 1200; i++ {
		b.WriteString("## 段落\n\n正文内容，包含 **加粗** 与 `code`。\n\n```bash\ncurl -X GET http://x\n```\n\n")
	}
	body, err := renderDocx(b.String())
	if err != nil {
		t.Fatal(err)
	}
	openDocx(t, body)
}

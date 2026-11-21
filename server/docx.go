package server

// 纯标准库 docx 生成器（0.15.29）：finding Markdown 报告 → Word 文档。
//
// 为什么手写 OOXML 而不引依赖：docx = ZIP + XML（OOXML），标题/列表/代码块/
// 行内样式用最小包即可正确表达——引一个第三方库换全功能不值得（go.mod 零新增）。
// 支持的 Markdown 子集刻意保守：标题/列表/代码块/加粗/行内代码/普通段落
// ——reporter agent 的报告模板（概述/影响/范围/复现/证据/PoC/根因/修复建议）
// 正好全部落在子集内。
//
// Word 兼容性纪律：[Content_Types].xml 必须声明 styles part；document.xml 的
// 样式引用只挂 styles.xml 里定义过的（未定义样式 Word回落正文，不报错）。
// 全部文本经 XML 转义（报告证据里随手就是 < > &）。

import (
	"archive/zip"
	"bytes"
	"regexp"
	"strconv"
	"strings"
)

// ── Markdown → 块序列 ────────────────────────────────────────────────────────

type docxBlock struct {
	kind  string // "h1" | "h2" | "h3" | "code" | "li" | "p"
	text  string
	listN int // 有序列表序号（0=无序）
}

var (
	docxHeadingRe  = regexp.MustCompile(`^(#{1,3})\s+(.*)$`)
	docxListItemRe = regexp.MustCompile(`^\s*(?:[-*]|\d+[.)])\s+(.*)$`)
	docxOrderedRe  = regexp.MustCompile(`^\s*(\d+)[.)]\s+`)
)

// markdownToBlocks 把 Markdown 文本切成块序列。子集外语法原样落段落
// （宁朴素勿丢内容——报告是给人看的，丢行比丢样式严重）。
func markdownToBlocks(md string) []docxBlock {
	var blocks []docxBlock
	inCode := false
	for _, line := range strings.Split(md, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			inCode = !inCode
			continue
		}
		if inCode {
			blocks = append(blocks, docxBlock{kind: "code", text: line})
			continue
		}
		if m := docxHeadingRe.FindStringSubmatch(line); m != nil {
			kind := "h" + strconv.Itoa(len(m[1]))
			blocks = append(blocks, docxBlock{kind: kind, text: strings.TrimSpace(m[2])})
			continue
		}
		if m := docxListItemRe.FindStringSubmatch(line); m != nil {
			n := 0
			if om := docxOrderedRe.FindStringSubmatch(line); om != nil {
				n, _ = strconv.Atoi(om[1])
			}
			blocks = append(blocks, docxBlock{kind: "li", text: m[1], listN: n})
			continue
		}
		blocks = append(blocks, docxBlock{kind: "p", text: line})
	}
	return blocks
}

// ── 行内样式 runs ───────────────────────────────────────────────────────────

var (
	docxBoldRe = regexp.MustCompile(`\*\*(.+?)\*\*`)
	docxCodeRe = regexp.MustCompile("`([^`]+)`")
)

// docxInlineRuns 把行内 Markdown（**加粗** / `行内代码`）切成 (文本, bold, code)
// 三元组。嵌套不支持（报告正文没有这个需求）：以更早命中的样式为准切分。
func docxInlineRuns(text string) [][3]string {
	var runs [][3]string
	rest := text
	for rest != "" {
		bLoc := docxBoldRe.FindStringSubmatchIndex(rest)
		cLoc := docxCodeRe.FindStringSubmatchIndex(rest)
		switch {
		case bLoc == nil && cLoc == nil:
			runs = append(runs, [3]string{rest, "", ""})
			return runs
		case cLoc == nil || (bLoc != nil && bLoc[0] < cLoc[0]):
			if bLoc[0] > 0 {
				runs = append(runs, [3]string{rest[:bLoc[0]], "", ""})
			}
			runs = append(runs, [3]string{rest[bLoc[2]:bLoc[3]], "1", ""})
			rest = rest[bLoc[1]:]
		default:
			if cLoc[0] > 0 {
				runs = append(runs, [3]string{rest[:cLoc[0]], "", ""})
			}
			runs = append(runs, [3]string{rest[cLoc[2]:cLoc[3]], "", "1"})
			rest = rest[cLoc[1]:]
		}
	}
	return runs
}

// ── OOXML 渲染 ──────────────────────────────────────────────────────────────

// docxEscape 是 document.xml 的文本转义（xml.EscapeText 会把合法 XML 字符外的
// 字节打成字符引用——报告里的控制字符会变成形如 \x01 的引用，Word 不认；
// 这里只转义五个 XML 实体 + 丢非法 XML 字符，语义对 Word 更友好）。
func docxEscape(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '&':
			b.WriteString("&amp;")
		case '<':
			b.WriteString("&lt;")
		case '>':
			b.WriteString("&gt;")
		case '"':
			b.WriteString("&quot;")
		case '\'':
			b.WriteString("&apos;")
		default:
			if r == 0x9 || r == 0xA || r == 0xD || (r >= 0x20 && r != 0xFFFE && r != 0xFFFF) {
				b.WriteRune(r)
			}
		}
	}
	return b.String()
}

// docxRenderRuns 把 runs 渲染成 w:r 序列。bold/code 各自成 rPr。
func docxRenderRuns(runs [][3]string) string {
	var b strings.Builder
	for _, run := range runs {
		text, bold, code := run[0], run[1], run[2]
		var props strings.Builder
		if bold == "1" {
			props.WriteString(`<w:b/><w:bCs/>`)
		}
		if code == "1" {
			props.WriteString(`<w:rStyle w:val="InlineCode"/>`)
		}
		pr := ""
		if props.Len() > 0 {
			pr = `<w:rPr>` + props.String() + `</w:rPr>`
		}
		b.WriteString(`<w:r>`)
		if pr != "" {
			b.WriteString(pr)
		}
		b.WriteString(`<w:t xml:space="preserve">`)
		b.WriteString(docxEscape(text))
		b.WriteString(`</w:t></w:r>`)
	}
	return b.String()
}

// docxParagraph 渲染一个段落（按块型选样式与缩进）。
func docxParagraph(bl docxBlock) string {
	runs := docxInlineRuns(bl.text)
	switch bl.kind {
	case "code":
		var b strings.Builder
		b.WriteString(`<w:p><w:pPr><w:pStyle w:val="CodeBlock"/><w:spacing w:before="0" w:after="0"/></w:pPr>`)
		b.WriteString(`<w:r><w:rPr><w:rStyle w:val="CodeBlock"/></w:rPr><w:t xml:space="preserve">`)
		b.WriteString(docxEscape(bl.text))
		b.WriteString(`</w:t></w:r></w:p>`)
		return b.String()
	case "h1", "h2", "h3":
		style := "Heading" + bl.kind[1:]
		return `<w:p><w:pPr><w:pStyle w:val="` + style + `"/></w:pPr>` +
			docxRenderRuns(runs) + `</w:p>`
	case "li":
		indent := `<w:ind w:left="420" w:hanging="210"/>`
		marker := "• "
		if bl.listN > 0 {
			marker = strconv.Itoa(bl.listN) + ". "
		}
		return `<w:p><w:pPr><w:pStyle w:val="ListParagraph"/>` + indent + `</w:pPr>` +
			docxRenderRuns([][3]string{{marker, "", ""}}) + docxRenderRuns(runs) + `</w:p>`
	}
	return `<w:p>` + docxRenderRuns(runs) + `</w:p>`
}

const docxStylesXML = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<w:styles xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">
<w:style w:type="paragraph" w:styleId="Heading1"><w:name w:val="heading 1"/><w:basedOn w:val="Normal"/><w:pPr><w:keepNext/><w:spacing w:before="240" w:after="120"/><w:outlineLvl w:val="0"/></w:pPr><w:rPr><w:b/><w:sz w:val="32"/></w:rPr></w:style>
<w:style w:type="paragraph" w:styleId="Heading2"><w:name w:val="heading 2"/><w:basedOn w:val="Normal"/><w:pPr><w:keepNext/><w:spacing w:before="200" w:after="100"/><w:outlineLvl w:val="1"/></w:pPr><w:rPr><w:b/><w:sz w:val="28"/></w:rPr></w:style>
<w:style w:type="paragraph" w:styleId="Heading3"><w:name w:val="heading 3"/><w:basedOn w:val="Normal"/><w:pPr><w:keepNext/><w:spacing w:before="160" w:after="80"/><w:outlineLvl w:val="2"/></w:pPr><w:rPr><w:b/><w:sz w:val="24"/></w:rPr></w:style>
<w:style w:type="paragraph" w:styleId="ListParagraph"><w:name w:val="List Paragraph"/><w:basedOn w:val="Normal"/><w:pPr><w:spacing w:after="60"/></w:pPr></w:style>
<w:style w:type="paragraph" w:styleId="CodeBlock"><w:name w:val="Code Block"/><w:basedOn w:val="Normal"/><w:pPr><w:shd w:val="clear" w:fill="F2F2F2"/><w:spacing w:after="0"/></w:pPr><w:rPr><w:rFonts w:ascii="Consolas" w:hAnsi="Consolas"/><w:sz w:val="18"/></w:rPr></w:style>
<w:style w:type="character" w:styleId="InlineCode"><w:name w:val="Inline Code"/><w:rPr><w:rFonts w:ascii="Consolas" w:hAnsi="Consolas"/><w:shd w:val="clear" w:fill="F2F2F2"/></w:rPr></w:style>
</w:styles>`

const docxContentTypesXML = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">
<Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>
<Default Extension="xml" ContentType="application/xml"/>
<Override PartName="/word/document.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"/>
<Override PartName="/word/styles.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.styles+xml"/>
</Types>`

const docxRelsXML = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="word/document.xml"/>
</Relationships>`

// docxDocRels 是 document.xml 的关系文件（styles part 的发现机制）。
const docxDocRels = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/styles" Target="styles.xml"/>
</Relationships>`

// renderDocx 把 Markdown 渲染成完整 docx 字节流。
func renderDocx(markdown string) ([]byte, error) {
	blocks := markdownToBlocks(markdown)
	var body strings.Builder
	for _, bl := range blocks {
		body.WriteString(docxParagraph(bl))
	}
	now := `<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body>` +
		body.String() +
		`<w:sectPr><w:pgSz w:w="11906" w:h="16838"/><w:pgMar w:top="1440" w:right="1440" w:bottom="1440" w:left="1440"/></w:sectPr></w:body></w:document>`

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	files := []struct{ name, content string }{
		{"[Content_Types].xml", docxContentTypesXML},
		{"_rels/.rels", docxRelsXML},
		{"word/document.xml", now},
		{"word/_rels/document.xml.rels", docxDocRels},
		{"word/styles.xml", docxStylesXML},
	}
	for _, f := range files {
		w, err := zw.Create(f.name)
		if err != nil {
			return nil, err
		}
		if _, err := w.Write([]byte(f.content)); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

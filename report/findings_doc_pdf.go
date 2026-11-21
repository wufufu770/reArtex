package report

// DOC / 打印即 PDF 渲染器（0.15.43，#8 导出补全收尾）。
//
// 纪律与 docx/TXT/SARIF/HTML 同款：纯标准库零第三方依赖。由此得出两个
// 工程裁决（CHANGELOG 留档）：
//   - .doc：Word 兼容单文件 HTML（ProgId=Word.Document 的业界惯用形态，
//     Word/WPS 直接打开编辑）——二进制 OLE 老格式手写不现实也不值得；
//   - pdf：打印就绪视图（@page A4 + 自动唤起打印对话框，浏览器引擎「另存
//     为 PDF」）——中文 PDF 需嵌入 CJK 字体（≥10MB 二进制入库不可接受），
//     纯标准库无字体子集化能力，打印视图是零依赖纪律下的正解。

import (
	"strings"
	"time"

	"github.com/Autumn-27/artex/db"
)

// wordMeta 注入 Word 兼容声明（ProgId 让 Word 按文档模式而非网页模式打开）。
const wordMeta = `<meta name="ProgId" content="Word.Document" /><meta name="Generator" content="reArtex" />`

// FindingsDoc 渲染 Word 兼容单文件（.doc；HTML 存档形态）。
func FindingsDoc(fs []*db.DBFinding, generatedAt time.Time) []byte {
	html := string(FindingsHTML(fs, generatedAt))
	return []byte(strings.Replace(html, "<meta charset=\"utf-8\">", "<meta charset=\"utf-8\">"+wordMeta, 1))
}

// printCSS 是打印视图增量样式（A4 分页：finding 卡片不跨页截断）。
const printCSS = `<style>
@media print{body{max-width:none;margin:0;padding:8mm}
.finding{break-inside:avoid;page-break-inside:avoid;border-color:#999}
h1{font-size:18pt}.no-print{display:none}}
@page{size:A4;margin:14mm}</style>`

// printScript 自动唤起浏览器打印对话框（「另存为 PDF」一步到位）。
const printScript = `<script>window.addEventListener('load',function(){setTimeout(function(){window.print()},300)});</script>`

// FindingsPrintHTML 渲染打印就绪视图（format=pdf 的响应体）。
func FindingsPrintHTML(fs []*db.DBFinding, generatedAt time.Time) []byte {
	html := string(FindingsHTML(fs, generatedAt))
	inject := printCSS + printScript
	return []byte(strings.Replace(html, "</style></head>", "</style>"+inject+"</head>", 1))
}

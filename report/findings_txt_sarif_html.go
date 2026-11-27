package report

// 三渲染器（0.15.36，批次 G2）：findings 导出补 TXT / SARIF 2.1.0 / HTML。
// 全部纯标准库零依赖（docx 同款纪律）；PDF/DOC 推迟留档（HTML 可直接打印为
// PDF，DOC 场景由既有 docx 覆盖）。

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/Autumn-27/artex/db"
)

// ── TXT ─────────────────────────────────────────────────────────────────────

// FindingsTXT 输出结构化纯文本（分节分隔线 + 折行）——邮件/工单/CLI 审阅形态。
func FindingsTXT(fs []*db.DBFinding, generatedAt time.Time) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "reArtex 渗透测试发现报告\n生成时间：%s\n发现数：%d\n", generatedAt.Format("2006-01-02 15:04:05"), len(fs))
	b.WriteString(strings.Repeat("=", 64) + "\n\n")
	for i, f := range fs {
		fmt.Fprintf(&b, "[%d] %s\n", i+1, findingTitle(f))
		fmt.Fprintf(&b, "  严重等级：%s\n", severityCN(f.Severity))
		if label := findingDimensionLabel(f); label != "" {
			fmt.Fprintf(&b, "  分级：%s\n", label)
		}
		fmt.Fprintf(&b, "  处置状态：%s\n", statusCN(f.Status))
		if f.TaskDescription != "" {
			fmt.Fprintf(&b, "  任务：%s\n", f.TaskDescription)
		}
		if len(f.AssetIDs) > 0 {
			fmt.Fprintf(&b, "  关联资产：%v\n", f.AssetIDs)
		}
		b.WriteString("  ---- 证据 ----\n")
		for _, line := range strings.Split(f.Evidence, "\n") {
			fmt.Fprintf(&b, "  %s\n", line)
		}
		b.WriteString("\n" + strings.Repeat("-", 64) + "\n\n")
	}
	return []byte(b.String())
}

// ── SARIF 2.1.0 ─────────────────────────────────────────────────────────────

// sarifLevel 把 severity 映射为 SARIF level（critical/high→error，
// medium→warning，low/info→note）。
func sarifLevel(severity string) string {
	switch severity {
	case "critical", "high":
		return "error"
	case "medium":
		return "warning"
	}
	return "note"
}

// FindingsSARIF 输出 OASIS SARIF 2.1.0（GitHub Code Scanning / VS Code 兼容）。
// vulnclass 作为 ruleId；summary 作为 message；资产/任务上下文进 properties。
func FindingsSARIF(fs []*db.DBFinding, generatedAt time.Time) ([]byte, error) {
	rules := map[string]bool{}
	var results []map[string]any
	for _, f := range fs {
		if !rules[f.VulnClass] {
			rules[f.VulnClass] = true
		}
		loc := map[string]any{}
		if len(f.AssetIDs) > 0 {
			loc["physicalLocation"] = map[string]any{
				"artifactLocation": map[string]any{"uri": fmt.Sprintf("assets/%d", f.AssetIDs[0])},
			}
		}
		results = append(results, map[string]any{
			"ruleId": f.VulnClass,
			"level":  sarifLevel(f.Severity),
			"message": map[string]any{
				"text": findingTitle(f) + " — " + firstNonEmpty(f.Summary, f.Evidence),
			},
			"locations": []any{loc},
			"partialFingerprints": map[string]string{
				"reartexFindingId/v1": strconvI64(f.ID),
			},
			"properties": map[string]any{
				"impact_level":          f.ImpactLevel,
				"evidence_level":        f.EvidenceLevel,
				"evidence_insufficient": f.EvidenceLevel > 0 && !db.EvidenceSufficient(f.Severity, f.EvidenceLevel),
			},
		})
	}
	var ruleList []map[string]any
	for id := range rules {
		ruleList = append(ruleList, map[string]any{
			"id":      id,
			"name":    id,
			"helpUri": "", // 无独立帮助页——留空合法
		})
	}
	doc := map[string]any{
		"version": "2.1.0",
		"$schema": "https://docs.oasis-open.org/sarif/sarif/v2.1.0/os/sarif-schema-2.1.0.json",
		"runs": []map[string]any{{
			"tool": map[string]any{"driver": map[string]any{
				"name":            "reArtex",
				"version":         "0.15.36",
				"semanticVersion": "0.15.36",
				"rules":           ruleList,
			}},
			"results":           results,
			"automationDetails": map[string]any{"id": "reartex/findings/" + generatedAt.Format("20060102150405")},
		}},
	}
	return json.Marshal(doc)
}

// ── HTML ─────────────────────────────────────────────────头部────────────────

// FindingsHTML 输出自包含 HTML（内联 CSS 零外链，可直接浏览器打开/打印为 PDF）。
func FindingsHTML(fs []*db.DBFinding, generatedAt time.Time) []byte {
	var b strings.Builder
	b.WriteString(`<!DOCTYPE html><html lang="zh-CN"><head><meta charset="utf-8"><title>reArtex 渗透测试发现报告</title><style>
body{font-family:system-ui,sans-serif;max-width:900px;margin:2rem auto;padding:0 1rem;color:#1a1a1a}
h1{border-bottom:2px solid #333;padding-bottom:.5rem}
.finding{border:1px solid #ccc;border-radius:6px;padding:1rem;margin:1rem 0}
.sev{display:inline-block;padding:.15rem .5rem;border-radius:4px;font-size:.85rem;color:#fff}
.sev-critical{background:#7f1d1d}.sev-high{background:#b91c1c}.sev-medium{background:#b45309}.sev-low{background:#4b5563}.sev-info{background:#6b7280}
pre{background:#f5f5f5;padding:.75rem;border-radius:4px;overflow-x:auto;font-size:.85rem;white-space:pre-wrap}
.meta{color:#666;font-size:.9rem}
</style></head><body>`)
	fmt.Fprintf(&b, "<h1>reArtex 渗透测试发现报告</h1><p class=\"meta\">生成时间 %s · %d 条发现</p>",
		generatedAt.Format("2006-01-02 15:04:05"), len(fs))
	for _, f := range fs {
		fmt.Fprintf(&b, `<div class="finding"><span class="sev sev-%s">%s</span> <strong>%s</strong> <span class="meta">[%s]</span>`,
			f.Severity, severityCN(f.Severity), htmlEscape(findingTitle(f)), statusCN(f.Status))
		if label := findingDimensionLabel(f); label != "" {
			fmt.Fprintf(&b, `<div class="meta">%s</div>`, htmlEscape(label))
		}
		if f.TaskDescription != "" {
			fmt.Fprintf(&b, `<div class="meta">任务：%s</div>`, htmlEscape(f.TaskDescription))
		}
		b.WriteString("<pre>" + htmlEscape(f.Evidence) + "</pre></div>")
	}
	b.WriteString("</body></html>")
	return []byte(b.String())
}

// ── 共享助手 ─────────────────────────────────────────────────────────────────
// findingTitle/severityCN/statusCN 复用 findings.go 既有实现的部分重导出：
// findings.go 已有 findingTitle（名称→类别→未分类）；本文件补 severity/status
// 的中文名（Markdown 渲染器未提供）。

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func severityCN(s string) string {
	switch s {
	case "critical":
		return "严重"
	case "high":
		return "高"
	case "medium":
		return "中"
	case "low":
		return "低"
	case "info":
		return "信息"
	}
	return s
}

func statusCN(s string) string {
	switch s {
	case "pending":
		return "待处理"
	case "in_progress":
		return "处理中"
	case "confirmed":
		return "已确认"
	case "resolved":
		return "已处理"
	case "fixed":
		return "已修复"
	case "false_positive":
		return "误报"
	case "ignored":
		return "忽略"
	case "duplicate":
		return "重复"
	case "risk_accepted":
		return "风险接受"
	}
	return s
}

func htmlEscape(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	s = strings.ReplaceAll(s, `"`, "&quot;")
	return s
}

// strconvI64 是 int64 → 字符串的局部别名（避免每处 strconv.Itoa(f.ID)）。
func strconvI64(v int64) string {
	return fmt.Sprintf("%d", v)
}

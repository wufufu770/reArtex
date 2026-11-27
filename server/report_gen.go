package server

// 报告生成器（0.14.5，批次四 D1）：任务维度报告初稿的【确定性拼装】——
// findings / biz 差异 / 覆盖缺口 / 偏锋概况 / 审计摘要全量来自库内事实，
// 不引入模型判断（LLM 润色是后续可选项，终审永远人工）。生成稿以
// created_by='report-gen' 标记初稿来源，报告中心可见、可编辑、可导出。

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/Autumn-27/artex/db"
)

func (s *Server) registerReportGenRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/reports/generate", s.reportGenerate)
}

// reportGenerate POST /api/reports/generate {"task_id":N, "title"?}。
func (s *Server) reportGenerate(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	var req struct {
		TaskID int64  `json:"task_id"`
		Title  string `json:"title"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&req)
	}
	if req.TaskID <= 0 {
		writeErr(w, 400, "task_id 必填")
		return
	}
	var name, goal, desc, status string
	var completedAt *time.Time
	err := pg.QueryRow(`SELECT name, goal, description, status, completed_at FROM tasks WHERE id=$1 AND deleted_at IS NULL`,
		req.TaskID).Scan(&name, &goal, &desc, &status, &completedAt)
	if err != nil {
		writeErr(w, 404, "任务不存在")
		return
	}
	done := ""
	if completedAt != nil {
		done = completedAt.Format("2006-01-02 15:04")
	}

	// ---- findings（severity 分组 + 清单）----
	type fRow struct {
		id       int64
		class    string
		sev      string
		status   string
		summary  string
		assetIDs []byte
	}
	fRows := []fRow{}
	fr, err := pg.Query(`SELECT id, vulnclass, severity, status, COALESCE(NULLIF(summary,''), NULLIF(name,'')), asset_ids
FROM findings WHERE task_id=$1 ORDER BY severity, id`, req.TaskID)
	if err == nil {
		for fr.Next() {
			var f fRow
			if fr.Scan(&f.id, &f.class, &f.sev, &f.status, &f.summary, &f.assetIDs) == nil {
				fRows = append(fRows, f)
			}
		}
		fr.Close()
	}
	sevCount := map[string]int{}
	for _, f := range fRows {
		sevCount[f.sev]++
	}

	// ---- biz 差异 ----
	diffs, _ := pg.ListBizDiffsP(db.ListBizDiffsOpts{TaskID: req.TaskID, Limit: 500})
	diffByStatus := map[string]int{}
	for _, d := range diffs {
		diffByStatus[d.Status]++
	}

	// ---- 覆盖缺口 ----
	gaps, _ := pg.ListCoverageGaps(req.TaskID, "", 0)
	gapOpen := 0
	for _, g := range gaps {
		if g.State == "open" {
			gapOpen++
		}
	}

	// ---- 偏锋概况（stop_reason 分布）----
	edgeRuns := map[string]int{}
	er, err := pg.Query(`SELECT COALESCE(NULLIF(stop_reason,''),'运行中'), count(*) FROM edge_runs WHERE task_id=$1 GROUP BY 1`, req.TaskID)
	if err == nil {
		for er.Next() {
			var k string
			var v int
			if er.Scan(&k, &v) == nil {
				edgeRuns[k] = v
			}
		}
		er.Close()
	}

	// ---- 审计摘要（本任务相关动作尾部 10 条）----
	auditLines := []string{}
	ar, err := pg.Query(`SELECT action, reason, created_at FROM audit_events WHERE payload::text LIKE '%"task_id":' || $1::text || ',%' OR reason LIKE 'task=' || $1::text || ' %' ORDER BY seq DESC LIMIT 10`, req.TaskID)
	if err == nil {
		for ar.Next() {
			var action, reason string
			var ts time.Time
			if ar.Scan(&action, &reason, &ts) == nil {
				auditLines = append(auditLines, fmt.Sprintf("- `%s` %s（%s）", action, reason, ts.Format("01-02 15:04")))
			}
		}
		ar.Close()
	}

	// ---- 拼装 markdown（确定性模板）----
	var b strings.Builder
	title := strings.TrimSpace(req.Title)
	if title == "" {
		title = fmt.Sprintf("%s · 渗透测试报告初稿", name)
	}
	b.WriteString(fmt.Sprintf("# %s\n\n", title))
	b.WriteString(fmt.Sprintf("> 生成时间：%s ｜ 生成方式：确定性拼装（created_by=report-gen，终审人工）｜ 任务状态：%s%s\n\n",
		time.Now().Format("2006-01-02 15:04"), status, doneSuffix(done)))

	b.WriteString("## 一、执行概览\n\n")
	b.WriteString(fmt.Sprintf("- 目标：%s\n", oneLine(goal)))
	b.WriteString(fmt.Sprintf("- 范围说明：%s\n", oneLine(desc)))
	b.WriteString(fmt.Sprintf("- 发现：%d 条（critical %d / high %d / medium %d / low %d）\n",
		len(fRows), sevCount["critical"], sevCount["high"], sevCount["medium"], sevCount["low"]))
	b.WriteString(fmt.Sprintf("- 业务差异：%d 条（open %d / pushed %d / confirmed %d / 误报 %d）\n",
		len(diffs), diffByStatus["open"], diffByStatus["pushed"], diffByStatus["confirmed"], diffByStatus["false_positive"]))
	b.WriteString(fmt.Sprintf("- 覆盖缺口：%d 条（open %d）\n", len(gaps), gapOpen))

	// 未达成目标固定成一节。没有时也写「无」，不能只留「已完成」。
	unmet, unmetErr := pg.UnmetGoalsByTask(req.TaskID)
	b.WriteString("\n## 未达成目标\n\n")
	switch {
	case unmetErr != nil:
		b.WriteString("（未达成目标读取失败。不能据此把任务写成已全部完成。）\n")
	case len(unmet) == 0:
		b.WriteString("（无未达成目标）\n")
	default:
		for _, g := range unmet {
			text := oneLine(g.Text)
			if text == "" {
				text = fmt.Sprintf("目标 #%d", g.NodeID)
			}
			criteria := oneLine(g.Criteria)
			if criteria == "" {
				criteria = "（无判定标准）"
			}
			b.WriteString(fmt.Sprintf("- %s\n  - 判定标准：%s\n  - 未达成原因：%s\n", text, criteria, oneLine(g.Reason)))
		}
	}
	if len(edgeRuns) > 0 {
		keys := make([]string, 0, len(edgeRuns))
		for k := range edgeRuns {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, fmt.Sprintf("%s×%d", k, edgeRuns[k]))
		}
		b.WriteString(fmt.Sprintf("- 偏锋运行：%s\n", strings.Join(parts, "、")))
	}

	b.WriteString("\n## 二、发现清单\n\n")
	if len(fRows) == 0 {
		b.WriteString("（无登记发现）\n")
	} else {
		b.WriteString("| # | 类别 | 严重度 | 状态 | 摘要 |\n|---|---|---|---|---|\n")
		for _, f := range fRows {
			summary := oneLine(f.summary)
			if summary == "" {
				summary = fmt.Sprintf("finding #%d", f.id)
			}
			b.WriteString(fmt.Sprintf("| #%d | %s | %s | %s | %s |\n", f.id, f.class, f.sev, f.status, summary))
		}
	}

	b.WriteString("\n## 三、业务差异\n\n")
	if len(diffs) == 0 {
		b.WriteString("（无登记差异）\n")
	} else {
		b.WriteString("| # | 类型 | 严重度 | 状态 | WSTG | 说明 |\n|---|---|---|---|---|---|\n")
		for _, d := range diffs {
			b.WriteString(fmt.Sprintf("| #%d | %s | %s | %s | %s | %s |\n",
				d.ID, d.DiffType, d.Severity, d.Status, d.WSTGRef, oneLine(d.Description)))
		}
	}

	if len(gaps) > 0 {
		b.WriteString("\n## 四、覆盖缺口（未闭合）\n\n")
		for _, g := range gaps {
			if g.State != "closed" {
				b.WriteString(fmt.Sprintf("- 资产 #%d（%s，%s）\n", g.AssetID, g.State, g.Reason))
			}
		}
	}

	if len(auditLines) > 0 {
		b.WriteString("\n## 五、治理与审计摘要（尾部 10 条）\n\n")
		for _, l := range auditLines {
			b.WriteString(l + "\n")
		}
	}
	b.WriteString("\n---\n\n*本初稿由平台确定性拼装生成（数据截至生成时刻）；结论与定稿以人工复核为准。*\n")

	var taskPtr = &req.TaskID
	// 0.15.43 #8：任务生成初稿自动打系统标签 reartex:report（forward-only，
	// 人工更新不可移除——db 层强制保留）。
	id, err := pg.CreateReport(title, b.String(), "report-gen", taskPtr, "", db.ReportSystemTag)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	log.Printf("[report-gen] task=%d 生成初稿 report=%d（findings=%d diffs=%d）", req.TaskID, id, len(fRows), len(diffs))
	writeJSON(w, 200, map[string]any{"ok": true, "report_id": id, "title": title,
		"findings": len(fRows), "diffs": len(diffs)})
}

// oneLine 取多行文本的首行（表格/概览里的单行化）。
func oneLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return s
}

// doneSuffix 是完成时间尾巴（空时无）。
func doneSuffix(done string) string {
	if done == "" {
		return ""
	}
	return " ｜ 完成于 " + done
}

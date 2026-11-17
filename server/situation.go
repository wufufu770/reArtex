package server

// planner 态势追加段（合并施工 G-19）：业务建模摘要段 + 偏锋判据段。
//
// 数据源（都是机械查询，零 LLM）：
//   - 业务建模：BizGraphSummary（design/behavior 节点数 + open/pushed 差异数）；
//   - 偏锋：TaskEdgeFields（任务是否 edge_mode）+ ListEdgeCells 的判据完成度
//     + methodology 注册表的判据类型。
//
// 注入纪律：只给计数与要点（反模式 8），每轮现取（易变内容不进 system），
// 空串 = 不注入。

import (
	"fmt"
	"strings"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/internal/edge"
)

// situationAppendProvider 是 SetSituationAppendProvider 的实现（engine 装配）。
func (e *Engine) situationAppendProvider(taskID int64) string {
	if e == nil || e.m == nil || e.m.pg == nil || taskID <= 0 {
		return ""
	}
	seg := ""
	// ① 业务建模摘要段：本任务有建模产出时给 planner 一个「业务语义层已有什么」的概览。
	if designN, behaviorN, openDiffs, err := e.m.pg.BizGraphSummary(taskID); err == nil && (designN+behaviorN) > 0 {
		seg += fmt.Sprintf("【业务建模摘要】本任务业务图：design 层 %d 节点 / behavior 层 %d 节点 / 未决差异 %d 条。派新意图前可用 read_biz_graph 查业务语义，避免与已建模方向重复；差异可经 propose_biz_intent 派生验证。", designN, behaviorN, openDiffs)
	}
	// ② 同目标参考段（0.15.12）：同公司兄弟任务的既往探索摘要——跨任务记忆复用的
	// 「信息辅助」通道。只注入摘要与计数（反模式 8），不注入正文；框架纪律在块内
	// 写死（仅供参照/不做主线/不得据此直接提交发现）。开关默认开。
	if e.m.pg.GetBool(settingCrossTaskReference, true) {
		seg += e.siblingReferenceBlock(taskID)
	}
	// ③ 偏锋判据段：仅 edge_mode 任务的注入（与非偏锋任务零交集）。
	edgeMode, methodologyID, ok := e.m.pg.TaskEdgeFields(taskID)
	if !ok || !edgeMode {
		return seg
	}
	cells, _ := e.m.pg.ListEdgeCells(taskID)
	terminal, total := 0, len(cells)
	openCells := []string{}
	for _, c := range cells {
		switch c.State {
		case db.EdgeCellConfirmed, db.EdgeCellDisproven, db.EdgeCellFolded:
			terminal++
		case db.EdgeCellOpen:
			openCells = append(openCells, c.CellID)
		}
	}
	methLine := ""
	if m, found := edge.Get(methodologyID); found {
		methLine = m.Name
		if m.Criterion != nil {
			methLine += fmt.Sprintf("（判据=%s", m.Criterion.Kind)
			for _, b := range m.Criterion.Bounds {
				methLine += fmt.Sprintf(" %s≤%d", b.Kind, b.Limit)
			}
			methLine += "）"
		}
		if len(m.HeuristicTags) > 0 {
			methLine += "，启发式：" + fmt.Sprint(m.HeuristicTags)
		}
	}
	seg += fmt.Sprintf("\n\n【偏锋判据段】本任务是偏锋探索线（方法论=%s）。判据进度：%d/%d 格点终态。", methLine, terminal, total)
	if len(openCells) > 0 {
		preview := openCells
		if len(preview) > 8 {
			preview = preview[:8]
		}
		seg += fmt.Sprintf("未开格点（优先派向这些）：%v。", preview)
	}
	seg += "派意图时以格点方向为选题依据；「探索够了」由判据承载，不靠直觉。"
	return seg
}

// taskStatusZh 是兄弟任务状态列的短中文标（参考段可读性；未知值原样显示）。
var taskStatusZh = map[string]string{
	"created": "已创建", "running": "运行中", "paused": "已暂停",
	"done": "已完成", "failed": "失败", "timeout": "超时",
}

// siblingReferenceBlock 渲染同目标参考段（空串 = 无兄弟任务或查询失败——参考段
// 是增值面，失败静默不注入）。预算 ≤8K 字符；兄弟任务 ≤5 个、发现摘要每任务
// ≤3 条 ×120 字。数据面：db.SiblingTaskDigests（公司范围重叠，机械查询零 LLM）。
func (e *Engine) siblingReferenceBlock(taskID int64) string {
	digests, err := e.m.pg.SiblingTaskDigests(taskID, 5)
	if err != nil || len(digests) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n【同目标历史任务参考（仅供参照，不是主线）】以下是同公司范围其他任务的既往探索摘要，只作背景信息辅助判断、防止重复踩坑：不得据此直接提交发现，一切以本任务实际验证为准；历史结论也可能已过时。")
	budget := 8 * 1024
	used := 0
	for _, d := range digests {
		var fb strings.Builder
		status := d.Status
		if zh, ok := taskStatusZh[d.Status]; ok {
			status = zh
		}
		fmt.Fprintf(&fb, "\n—— 任务 #%d（%s）：%s", d.ID, status, firstLineTask(d.Goal, 120))
		if d.Findings > 0 {
			fmt.Fprintf(&fb, " · 已登记发现 %d 条", d.Findings)
		}
		for _, s := range d.TopFindings {
			fmt.Fprintf(&fb, "\n  · %s", truncateStrRunes(s, 120))
		}
		entry := fb.String()
		if used+len(entry) > budget {
			break
		}
		b.WriteString(entry)
		used += len(entry)
	}
	return b.String()
}

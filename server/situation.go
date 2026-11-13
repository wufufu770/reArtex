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
	// ② 偏锋判据段：仅 edge_mode 任务的注入（与非偏锋任务零交集）。
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

package agent

import (
	"encoding/json"
	"fmt"
)

// Team 值得性闸（方案书 §11.6.4 的 P2 项 / §11.6.5 冲突 4）。
//
// 背景：MiniMax 的 Agent Team 文章有一条「不该鼓励凡事开 Team」的建议；而
// reArtex 的 planner 有一条【硬底线】—— 目标未达成且摊上没有 open/running 意图时，
// 本轮必须产出至少一个意图（防探索停摆的承重墙）⇒ 简单任务也会走完整的多 agent
// 流程，ROI 崩。
//
// 两条纪律的冲突由本闸解决：
//   - **不拆底线**：硬底线不删。闸只决定【并行形态】，不决定「要不要干活」⇒
//     判定为不值得时仍然必须派工，只是单条派工。
//   - **模型只提议、确定性层裁决**：闸是纯函数（不查库、不调模型、不出网、不读
//     可变全局状态），结论由图上的事实算出来，不靠模型自评「这事值不值」。
//
// 值得开 Team 的充要条件（三条同时成立）：
//  1. 目标未达成 —— 目标已达成就该收官，不该再开 Team；
//  2. 可并行的面 ≥ MinTeamParallelFaces —— 一个面不值得 Team（单条派工更省）；
//  3. 剩余预算 ≥ MinTeamBudgetUnits —— 至少养得起两条腿（一条产出 + 一条复核）。
//
// 判定为不值得时 MaxParallel = 1 ⇒ 结论是「不并行」，不是「不干活」。

const (
	// MinTeamParallelFaces 是开 Team 的最小可并行面数。
	//
	// 口径：可并行面 = 范围内尚未被 fact 碰过的资产数（见 TeamGateSignals）。
	// 一个面不值得 Team —— 单条派工更省，还没有协调成本。
	MinTeamParallelFaces = 2
	// MinTeamBudgetUnits 是开 Team 的最小剩余预算（单位由调用方定义）。
	//
	// 口径：调用方给「还剩多少轮/额度/分钟」；闸只拿它跟常量比大小 ⇒ 具体单位
	// 由接线方钉死并写进调用点。读不到预算（0）⇒ 按「预算不足」处理（fail-closed
	// 方向：不因读不到状态就开 Team）。
	MinTeamBudgetUnits = 2
	// MaxTeamParallel 是并行条数的硬上限（防止一次铺开太多把额度烧穿）。
	MaxTeamParallel = 4
)

// TeamGateSignals 是值得性闸的输入：**全部是图上的事实**，不含模型的判断。
type TeamGateSignals struct {
	// UnmetGoals：未达成的目标数（0 = 目标已达成 ⇒ 收官）。
	UnmetGoals int
	// OpenIntents：开放意图数（frontier_open）。
	OpenIntents int
	// RunningIntents：在跑意图数。
	RunningIntents int
	// ParallelFaces：可独立切分的面数（范围内未被 fact 碰过的资产数）。
	//
	// 为什么用它当「并行度」：面是任务的攻击面单元，面与面之间默认互不依赖 ⇒
	// 可切分的面数就是「并行能省多少时间」的上界。它是【确定性】的（覆盖度分母
	// 减已测数），而「哪几个方向本质不同」是模型判断 —— 闸不碰后者。
	ParallelFaces int
	// RemainingBudget：剩余预算（单位由调用方定义；0 = 读不到 ⇒ 按不足处理）。
	RemainingBudget int
}

// TeamGateVerdict 是值得性闸的结论。
type TeamGateVerdict struct {
	// Worthy：这一轮值不值得开 Team（多 agent 并行）。
	Worthy bool
	// MaxParallel：本轮允许的并行条数。
	//
	// **恒 ≥ 1**：闸只决定并行形态，【不拆硬底线】—— 目标未达成且摊上没有在跑的
	// 活时，本轮仍然必须产出至少一条意图。一个能返回 0 的闸就是拆底线。
	MaxParallel int
	// Reason：给 planner 看的一句话理由（中文，直接可读）。
	Reason string
}

// JudgeTeamWorthiness 判定「这一轮值不值得开 Team」。
//
// 纯函数：同样的输入永远得到同样的结论（可被穷举单测钉死）。
//
// 判定链：
//
//	目标已达成            ⇒ 不值得（收官）
//	可并行面 < 2          ⇒ 不值得（一个面单条派工更省）
//	剩余预算 < 2          ⇒ 不值得（养不起两条腿）
//	否则                   ⇒ 值得，并行条数 = min(可并行面, 剩余预算, MaxTeamParallel)
func JudgeTeamWorthiness(s TeamGateSignals) TeamGateVerdict {
	if s.UnmetGoals <= 0 {
		return teamGateReject("目标已达成：收官，不开 Team")
	}
	if s.ParallelFaces < MinTeamParallelFaces {
		return teamGateReject("可并行的面不足 " + fmt.Sprint(MinTeamParallelFaces) + " 个：单条派工更省，不开 Team")
	}
	if s.RemainingBudget < MinTeamBudgetUnits {
		return teamGateReject("剩余预算养不起 " + fmt.Sprint(MinTeamBudgetUnits) + " 条腿：不开 Team")
	}
	parallel := s.ParallelFaces
	if parallel > s.RemainingBudget {
		parallel = s.RemainingBudget
	}
	if parallel > MaxTeamParallel {
		parallel = MaxTeamParallel
	}
	if parallel < 2 {
		parallel = 2
	}
	return TeamGateVerdict{
		Worthy:      true,
		MaxParallel: parallel,
		Reason: "值得开 Team：未达成目标 " + fmt.Sprint(s.UnmetGoals) + " 个，可并行的面 " +
			fmt.Sprint(s.ParallelFaces) + " 个，剩余预算 " + fmt.Sprint(s.RemainingBudget) + " ⇒ 本轮最多并行 " +
			fmt.Sprint(parallel) + " 条",
	}
}

// teamGateReject 造一个「不值得开 Team」的结论：并行条数恒为 1。
func teamGateReject(reason string) TeamGateVerdict {
	return TeamGateVerdict{Worthy: false, MaxParallel: 1, Reason: reason}
}

// TeamGateSignalsFromOverview 从图概览（图上事实）+ 剩余预算抽出闸需要的信号。
//
// 为什么从概览抽而不是另查一遍库：概览是 planner 每轮已经在算的东西
// （ToolSet.graphOverviewData）⇒ 闸为判一次值不值得再查一次库，就多出一处可能
// 与概览不一致的口径。
//
// remainingBudget 的单位由调用方定义（轮次 / 额度 / 分钟）；闸只跟常量比大小。
func TeamGateSignalsFromOverview(overview map[string]any, remainingBudget int) TeamGateSignals {
	s := TeamGateSignals{RemainingBudget: remainingBudget}
	if overview == nil {
		return s
	}
	s.UnmetGoals = countUnmetGoals(overview["goals"])
	s.OpenIntents = intFromOverview(overview["frontier_open"])
	s.RunningIntents = lenAnyList(overview["running_intents"])
	s.ParallelFaces = untestedFaces(overview["coverage"])
	return s
}

// countUnmetGoals 数未达成的目标（只有 state=="met" 才算达成）。
func countUnmetGoals(raw any) int {
	list := anyList(raw)
	n := 0
	for _, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if state, _ := m["state"].(string); state != "met" {
			n++
		}
	}
	return n
}

// untestedFaces 从覆盖度块里取「未被 fact 碰过的资产数」（分母 - 已测）。
//
// 覆盖度块缺失（功能关闭 / 没有任务上下文）⇒ 0 ⇒ 闸按「没有可并行的面」处理
// （fail-closed 方向：读不到就当没有，而不是当有很多）。
func untestedFaces(raw any) int {
	m, ok := raw.(map[string]any)
	if !ok {
		return 0
	}
	denominator := intFromOverview(m["denominator"])
	tested := intFromOverview(m["tested"])
	if tested > denominator {
		return 0
	}
	return denominator - tested
}

// intFromOverview 从概览里取一个整数（各种数值类型都收；取不到 = 0）。
func intFromOverview(raw any) int {
	switch v := raw.(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	case json.Number:
		n, err := v.Int64()
		if err != nil {
			return 0
		}
		return int(n)
	default:
		return 0
	}
}

// anyList 把概览里的列表型字段转成切片（非列表 = 空）。
func anyList(raw any) []any {
	if list, ok := raw.([]any); ok {
		return list
	}
	return nil
}

// lenAnyList 数概览里列表型字段的长度。
func lenAnyList(raw any) int {
	return len(anyList(raw))
}

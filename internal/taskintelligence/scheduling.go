package taskintelligence

// scheduling.go —— v2.1 统一架构优化增补 §0.7「AUTO 与 Single-Agent 的统一调度
// 规则」的确定性决策层（0.15.65，批次 TI-07）。
//
// 六条调度原则（§0.7 原文口径，本文件的编码方式）：
//  1. 默认 Single Main Agent：本层只产出「建议」，不创建执行、不派生 Agent——
//     AutoDecision 结构体没有也不允许有「创建 Agent/修改范围/删除审批」字段
//     （原则 5 的结构性兑现：想表达也表达不出来）；
//  2. 能力以工具方式按需调用：决策只给能力 ID 列表（Registry 里的注册项），
//     调用走统一工具安全管道（原则 6），本层不触碰任何工具实现；
//  3. Specialist 短期派生：决策不表达 Specialist 派生——那是由主 Agent 按原则 3
//     的证明条件自行判断的执行细节；
//  4. 运行时升降级：业务模型缺口 → 建议补建模（复用 TI-04
//     EdgeNeedsBusinessModel）；假设积压/证据缺失 → 建议探索/深入验证；
//  5. AUTO 不能修改授权范围、删除审批要求或安全策略：风险升高时只能**下调**
//     动作——ClampDepthMax 只接受上限、没有上调入口；
//  6. 能力本身没有额外权限：能力 ID 只是指路牌，权限永远来自 task scope。
//
// 术语边界（TI-14 映射 D-13 的登记项）：本层 Difficulty 是 §0.7 的调度目标难度
// （easy/medium/hard/expert），与 agent/execution_profile.go 的算力档
//（execution profile 的 Difficulty）不是同一语义——两档映射在接线任务（TI-11
// 及后续）完成，不在本文件猜测。
//
// 本文件是纯确定性规则：输入快照、输出建议，零 IO、零工具执行、零 LLM。

import (
	"fmt"
	"sort"
	"strings"
)

// §0.7 AutoDecision 输出骨架（字段与方案一字对应）。
type AutoDecision struct {
	// Difficulty 调度目标难度（非算力档，见文件头术语边界）。
	Difficulty string // easy / medium / hard / expert
	// ExecutionDepth 执行深度。
	ExecutionDepth string // quick / standard / deep / exhaustive
	// Capabilities 建议调用的标准能力 ID（五标准能力，见 capability.go）。
	Capabilities []string
	// DelegateReasons 与 Capabilities 一一对应的委派原因。
	DelegateReasons []string
	// BudgetProfile 预算画像（lean/standard/generous）。
	BudgetProfile string
	// Confidence 决策置信度：分节可用性越低置信度越低（缺数据不装作有判断）。
	Confidence float64
	// Rationale 一句话汇总理由。
	Rationale string
}

// 难度/深度/预算档位白名单。
const (
	DiffEasy   = "easy"
	DiffMedium = "medium"
	DiffHard   = "hard"
	DiffExpert = "expert"

	DepthQuick      = "quick"
	DepthStandard   = "standard"
	DepthDeep       = "deep"
	DepthExhaustive = "exhaustive"

	BudgetLean     = "lean"
	BudgetStandard = "standard"
	BudgetGenerous = "generous"
)

var depthRank = map[string]int{
	DepthQuick: 1, DepthStandard: 2, DepthDeep: 3, DepthExhaustive: 4,
}

// DecideAuto 按快照信号产出调度建议（§0.7 输入清单的确定性子集）：
//   - 目标规模信号：范围条目数（Scope.AssetRows）、资产数（Assets.InScope）；
//   - 业务复杂度/缺口信号：业务模型版本与未收敛差异（BizModel）；
//   - 证据覆盖信号：事实/证据比（Facts/Evidence）；
//   - 假设价值信号：待验证假设数（Facts.Hypotheses）；
//   - 执行状态信号：开放发现数、历史执行量（Findings/History）。
//
// 分节不可用按「宁保守勿猜测」处理：缺口计入置信度折扣，关键缺口（业务模型）
// 直接建议补建模，不伪造判断。
func DecideAuto(snap *TaskContextSnapshot) AutoDecision {
	dec := AutoDecision{
		Difficulty:      DiffMedium,
		ExecutionDepth:  DepthStandard,
		Capabilities:    []string{},
		DelegateReasons: []string{},
		BudgetProfile:   BudgetStandard,
		Confidence:      0.9,
	}
	if snap == nil {
		dec.Confidence = 0.3
		dec.Rationale = "快照不可用：按最保守档执行，等上下文就绪后再升级"
		return dec
	}
	dec.Confidence -= 0.1 * float64(len(snap.Unavailable()))
	if dec.Confidence < 0.3 {
		dec.Confidence = 0.3
	}

	var notes []string
	hardSignals := 0
	consider := func(cond bool, difficultyIfHard bool, note string) {
		if !cond {
			return
		}
		notes = append(notes, note)
		if difficultyIfHard {
			hardSignals++
		}
	}

	// 目标规模：范围条目/资产量（小→easy，大→hard 信号）。
	switch {
	case snap.Scope.AssetRows > 0 && snap.Scope.AssetRows <= 5 && snap.Assets.InScope <= 3:
		dec.Difficulty = DiffEasy
		notes = append(notes, fmt.Sprintf("目标规模小（范围 %d 条 / 资产 %d 个）", snap.Scope.AssetRows, snap.Assets.InScope))
	case snap.Scope.AssetRows > 50 || snap.Assets.InScope > 30:
		consider(true, true, fmt.Sprintf("目标规模大（范围 %d 条 / 资产 %d 个）", snap.Scope.AssetRows, snap.Assets.InScope))
	}

	// 业务缺口（原则 4）：复用 TI-04 的新鲜度/覆盖判定，缺口即建议补建模。
	if need, why := EdgeNeedsBusinessModel(snap); need {
		dec.addCapability(CapModelingBusiness, why)
		consider(true, false, "存在业务模型缺口")
	} else if snap.BizModel.LayerDiffs > 10 {
		consider(true, true, fmt.Sprintf("业务模型未收敛差异偏多（%d 条）", snap.BizModel.LayerDiffs))
	}

	// 证据覆盖：无证据或覆盖比 < 0.2 → 建议偏锋探索补证据。
	if snap.Evidence.Err == nil && snap.Facts.Err == nil {
		if snap.Evidence.Count == 0 && (snap.Facts.Total > 0 || snap.Findings.Open > 0) {
			dec.addCapability(CapExplorationEdge, "尚无证据但已有事实/发现：建议偏锋探索补可追溯证据")
		} else if snap.Facts.Total > 0 && snap.Evidence.Count*5 < snap.Facts.Total {
			dec.addCapability(CapExplorationEdge, fmt.Sprintf("证据覆盖不足（证据 %d / 事实 %d，<0.2）：建议补探索", snap.Evidence.Count, snap.Facts.Total))
		}
	}

	// 假设价值（原则 4）：待验证假设积压 → 建议深入验证。
	if snap.Facts.Hypotheses > 10 {
		dec.addCapability(CapValidationDeep, fmt.Sprintf("待验证假设积压（%d 条）：建议深入验证收敛", snap.Facts.Hypotheses))
		consider(true, true, "假设积压")
	}

	// 发现状态：开放发现多 → 深入验证；确认已清 → 报告收口。
	if snap.Findings.Open >= 3 {
		dec.addCapability(CapValidationDeep, fmt.Sprintf("开放发现 %d 条：建议验证收敛", snap.Findings.Open))
	} else if snap.Findings.Confirmed > 0 && snap.Findings.Open == 0 {
		dec.addCapability(CapReportingResult, "发现已全部确认：建议产出报告收口")
	}

	// 历史执行量是预算压力信号：只降不升（原则 5 的方向性）。
	if snap.History.Executions > 100 {
		notes = append(notes, fmt.Sprintf("历史执行量偏大（%d 次）：预算收紧", snap.History.Executions))
		dec.BudgetProfile = BudgetLean
	}

	// 难度/深度合成。
	if hardSignals >= 2 {
		dec.Difficulty = DiffExpert
	}
	if dec.Difficulty == DiffEasy {
		dec.ExecutionDepth = DepthQuick
	} else if len(dec.Capabilities) > 0 && dec.Difficulty != DiffEasy {
		dec.ExecutionDepth = DepthDeep
		if snap.Facts.Hypotheses > 20 && dec.Difficulty == DiffExpert {
			dec.ExecutionDepth = DepthExhaustive
		}
	}
	if dec.BudgetProfile == BudgetLean && depthRank[dec.ExecutionDepth] > depthRank[DepthStandard] {
		dec.ExecutionDepth = DepthStandard // 预算收紧封顶 standard
	}
	switch dec.ExecutionDepth {
	case DepthQuick:
		dec.BudgetProfile = BudgetLean
	case DepthDeep, DepthExhaustive:
		if dec.BudgetProfile != BudgetLean {
			dec.BudgetProfile = BudgetGenerous
		}
	}

	// 常规测试是基线能力：始终在场（testing.standard 是默认主循环，不是特派）。
	dec.Capabilities = append([]string{CapTestingStandard}, dec.Capabilities...)
	dec.DelegateReasons = append([]string{"默认主循环：常规测试基线能力"}, dec.DelegateReasons...)

	if len(notes) == 0 {
		notes = append(notes, "各信号平稳：按标准档执行")
	}
	dec.Rationale = strings.Join(notes, "；")
	return dec
}

// addCapability 追加一条能力建议（去重，原因与能力一一对应，按 ID 稳定排序
// 在收口时统一做——这里只保证不重复）。
func (d *AutoDecision) addCapability(id, reason string) {
	for _, c := range d.Capabilities {
		if c == id {
			return
		}
	}
	d.Capabilities = append(d.Capabilities, id)
	d.DelegateReasons = append(d.DelegateReasons, reason)
}

// NormalizeAutoDecision 收口：能力列表按 ID 稳定排序（原因随之同步排序，
// 保持一一对应）并校验全部是注册表中的启用能力（原则 6：建议只指路，指的
// 必须是注册表里真实存在且启用的路）。未知/停用能力剔除并写进 Rationale 尾注。
func NormalizeAutoDecision(dec AutoDecision, r *Registry) AutoDecision {
	if r == nil {
		return dec
	}
	type pair struct{ cap, reason string }
	var keptPair []pair
	dropped := []string{}
	for i, c := range dec.Capabilities {
		reason := ""
		if i < len(dec.DelegateReasons) {
			reason = dec.DelegateReasons[i]
		}
		if d, ok := r.Lookup(c); ok && d.Enabled {
			keptPair = append(keptPair, pair{c, reason})
		} else {
			dropped = append(dropped, c)
		}
	}
	sort.Slice(keptPair, func(i, j int) bool { return keptPair[i].cap < keptPair[j].cap })
	kept := make([]string, 0, len(keptPair))
	keptReasons := make([]string, 0, len(keptPair))
	for _, p := range keptPair {
		kept = append(kept, p.cap)
		keptReasons = append(keptReasons, p.reason)
	}
	if len(dropped) > 0 {
		dec.Rationale += "；已剔除未注册/停用能力：" + strings.Join(dropped, ",")
	}
	dec.Capabilities = kept
	dec.DelegateReasons = keptReasons
	return dec
}

// ClampDepthMax 风险升高时的确定性下调入口（原则 5：只能缩小动作）。给决策
// 的执行深度设上限——没有对应的「上调」函数，这是刻意缺位。
func ClampDepthMax(dec AutoDecision, maxDepth string) (AutoDecision, error) {
	maxRank, ok := depthRank[maxDepth]
	if !ok {
		return dec, fmt.Errorf("非法深度档 %q", maxDepth)
	}
	if depthRank[dec.ExecutionDepth] > maxRank {
		dec.ExecutionDepth = maxDepth
		dec.Rationale += fmt.Sprintf("；风险约束：执行深度下调至 %s", maxDepth)
	}
	return dec, nil
}

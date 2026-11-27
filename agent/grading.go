package agent

import (
	"fmt"
	"strings"
)

// 任务评级（C-03）。纯函数：同一组输入永远得到同一分数、等级和摘要。
// 不读库、不调 LLM。四项目标达成率 / 发现质量 / 覆盖率 / 证据完整率的
// 分子分母由调用方从已有确定性数据里填好再送进来。

const (
	GradeS = "S"
	GradeA = "A"
	GradeB = "B"
	GradeC = "C"
)

// GradeInput 是四项分子/分母。证据完整数由 CountEvidenceComplete 给出：
// 走过 C-04 写入的发现看证据等级是否 ≥ L3，存量发现仍过 finding_gate。
type GradeInput struct {
	GoalsMet, GoalsTotal            int
	VerifiedFindings, TotalFindings int
	CoveredAssets, ScopedAssets     int
	EvidenceCompleteFindings        int
}

// GradeResult 是一次确定性评级。Score 是 0–100 的加权分（分子大于分母时
// 可以超过 100，公式不另做封顶）。Grade 只取 S/A/B/C。
type GradeResult struct {
	Score   float64 `json:"score"`
	Grade   string  `json:"grade"`
	Summary string  `json:"summary"`
}

// GradeFinding 是证据完整率用的一条发现。
// 存量发现只看结论陈述和证据正文。走过 C-04 写入的发现再看证据等级：
// EvidenceLevel 是降级后的落库值，EvidenceLevelRequested 是自报，
// EvidenceLevelReason 是降级原因。三列都空着（0 / 0 / ""）时按存量处理。
type GradeFinding struct {
	Summary                string
	Evidence               string
	EvidenceLevel          int
	EvidenceLevelRequested int
	EvidenceLevelReason    string
}

// ComputeGrade 按 40/30/20/10 加权。分母 ≤ 0 的维度按 0 分，不按满分，
// 也不 panic。摘要模板固定，数字全部来自这一次输入；分母为 0 时在句末注明。
func ComputeGrade(in GradeInput) GradeResult {
	goalRate := gradeRatio(in.GoalsMet, in.GoalsTotal)
	qualityRate := gradeRatio(in.VerifiedFindings, in.TotalFindings)
	coverageRate := gradeRatio(in.CoveredAssets, in.ScopedAssets)
	evidenceRate := gradeRatio(in.EvidenceCompleteFindings, in.TotalFindings)
	score := goalRate*40 + qualityRate*30 + coverageRate*20 + evidenceRate*10
	grade := GradeC
	switch {
	case score >= 90:
		grade = GradeS
	case score >= 75:
		grade = GradeA
	case score >= 60:
		grade = GradeB
	}
	summary := fmt.Sprintf("%d/%d 目标达成，%d/%d 条发现已验证，覆盖率 %.0f%%，证据完整率 %.0f%%",
		in.GoalsMet, in.GoalsTotal, in.VerifiedFindings, in.TotalFindings,
		coverageRate*100, evidenceRate*100)
	if note := gradeZeroNote(in); note != "" {
		summary += "（" + note + "）"
	}
	return GradeResult{Score: score, Grade: grade, Summary: summary}
}

// CountEvidenceComplete 统计证据完整的发现数。
// 走过 C-04 写入的发现：evidence_level ≥ 3 算完整，不再看 finding_gate。
// 存量发现（自报仍是默认 L1，且没有降级原因）：仍以 CheckFindingGate 为准。
// 门的 R1 已经拒绝空白证据。不读 evidence_status。已验证发现仍只数 confirmed。
// 本函数不改已经落库的等级；调用方只在新的终态结算或显式重新评级时使用它。
func CountEvidenceComplete(findings []GradeFinding) int {
	n := 0
	for _, f := range findings {
		if evidenceFindingComplete(f) {
			n++
		}
	}
	return n
}

// evidenceGradedByLevel 判断这条发现是否按证据等级计完整。
//
// 标记用 findings 上已经落库、且不会被 ALTER 默认值误伤的两列：
//   - evidence_level_requested > 1：自报不是默认的 L1。省略参数和存量行
//     的列默认都是 1，列值上分不开，所以默认 1 仍走旧门。
//   - evidence_level_reason 去空白后非空：走过上限降级，说明是 C-04 写入。
//
// 调用方没填时 requested 是 0。0 不是库里的合法自报（CHECK 1–4，默认 1），
// 按存量处理，避免只带 summary/evidence 的旧调用被当成新发现。
//
// 不用探索节点 payload 里有没有这几个键：RecordFinding 即使省略等级也会把键
// 写进去，只改影响层级的写入也会补键。那样会把「默认等级、没有降级原因」的
// 存量发现改判成新口径。也不单看 evidence_level：它是降级后的结果，不是
// 「走过 C-04 写入」的标记。
func evidenceGradedByLevel(f GradeFinding) bool {
	if strings.TrimSpace(f.EvidenceLevelReason) != "" {
		return true
	}
	return f.EvidenceLevelRequested > 1
}

func evidenceFindingComplete(f GradeFinding) bool {
	if evidenceGradedByLevel(f) {
		return f.EvidenceLevel >= 3
	}
	return CheckFindingGate(f.Summary, f.Evidence) == nil
}

// ValidGrade 只认 S/A/B/C。空串表示还没评，不是一个等级。
func ValidGrade(s string) bool {
	switch s {
	case GradeS, GradeA, GradeB, GradeC:
		return true
	}
	return false
}

func gradeRatio(n, d int) float64 {
	if d <= 0 {
		return 0
	}
	return float64(n) / float64(d)
}

func gradeZeroNote(in GradeInput) string {
	var parts []string
	if in.GoalsTotal <= 0 {
		parts = append(parts, "无目标按 0 计")
	}
	if in.TotalFindings <= 0 {
		parts = append(parts, "无发现按 0 计")
	}
	if in.ScopedAssets <= 0 {
		parts = append(parts, "无范围资产按 0 计")
	}
	return strings.Join(parts, "；")
}

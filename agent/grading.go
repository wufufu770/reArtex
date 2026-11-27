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

// GradeInput 是四项分子/分母。证据完整数在 C-04 落地前由
// CountEvidenceComplete 按「非空 evidence 且通过 finding_gate」近似。
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

// GradeFinding 是证据完整率用的一条发现。只看结论陈述和证据正文。
type GradeFinding struct {
	Summary  string
	Evidence string
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

// CountEvidenceComplete 统计「有非空证据且通过 finding_gate」的发现数。
// CheckFindingGate 的 R1 已经拒绝空白证据，所以通过门即满足原先的两项近似。
// C-04 的 evidence_level 已经落库，但存量行默认 L1。若这里改成「证据等级 ≥ L3」，
// 重新评级会把还没人重标过的历史任务证据完整率打成 0。口径是否切换留给业主，
// 本函数继续不读 evidence_level，也不读 evidence_status。已验证发现仍只数 confirmed。
func CountEvidenceComplete(findings []GradeFinding) int {
	n := 0
	for _, f := range findings {
		if CheckFindingGate(f.Summary, f.Evidence) == nil {
			n++
		}
	}
	return n
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

// 三条兜底熔断（EDGE E7）的确定性判定。
//
// 判定依据全部落在确定性层可观测的状态上，不用模型自评：
//   ① 假设退化：连续 N 次提出的假设未通过溯因门禁（basis 为空、引用已证伪
//      事实、prediction 与 falsifier 不可判定），N ≥ 3 ⇒ 终止。
//   ② 格点封顶：edge_cells.attempts 达上限时该格点强制封格并退出候选集
//      （封格动作在 server 侧，这里只是阈值常量与判定）。
//   ③ 判据停滞：判据状态连续 M 轮规范化哈希不变 ⇒ 终止（处理「还在跑但
//      已不产生新覆盖」的空转）。
//
// 三条熔断触发后都要走 edge_conclude 的人工确认分支：stop_reason 与 stop_detail
// 写进 edge_runs，让人知道这次是被掐断的而不是自然完成。
package edge

// DegenerateThreshold 是假设退化熔断的连续失败阈值。
const DegenerateThreshold = 3

// StallThreshold 是判据停滞熔断的连续无推进轮数阈值。
const StallThreshold = 4

// AbductionVerdict 是一条偏锋假设过溯因门禁的结果。
type AbductionVerdict int

const (
	// AbductionOK 通过：basis 非空、不引用已证伪事实、prediction/falsifier 可判定。
	AbductionOK AbductionVerdict = iota
	// AbductionDegenerate 未通过：三类退化之一。
	AbductionDegenerate
)

// JudgeAbduction 是溯因门禁的确定性判定（零 LLM）：
//   - basis（observation_basis）为空 ⇒ 退化（自提自证）；
//   - hypothesis 引用已证伪事实（refutedFactSummaries 的词集交集 ≥2）⇒ 退化；
//   - prediction 或 falsifier 为空 ⇒ 退化（不可判定 = 不可证伪）。
//
// refutedOverlapMin 与晋升门的撞证伪池阈值同源口径：与单条 refuted fact 的结论
// 共享 ≥2 个词元算撞（1 个 CJK 单字的重叠是噪声）。
func JudgeAbduction(basis, prediction, falsifier string, refutedFactSummaries []string) AbductionVerdict {
	if len(basis) == 0 {
		return AbductionDegenerate
	}
	if len(prediction) == 0 || len(falsifier) == 0 {
		return AbductionDegenerate
	}
	if overlapsRefuted(hypothesisTokens(basis+" "+prediction), refutedFactSummaries) {
		return AbductionDegenerate
	}
	return AbductionOK
}

// DegenerateTracker 是熔断①的计数器（连续失败计数，成功即清零——「连续」的
// 语义是中间没有任何一次通过）。
type DegenerateTracker struct {
	consecutive int
}

// Record 记一次判定结果，返回当前连续失败数（达到 DegenerateThreshold 时调用方
// 应触发熔断）。
func (t *DegenerateTracker) Record(v AbductionVerdict) int {
	if v == AbductionOK {
		t.consecutive = 0
		return 0
	}
	t.consecutive++
	return t.consecutive
}

// Tripped 报告是否已到熔断阈值。
func (t *DegenerateTracker) Tripped() bool { return t.consecutive >= DegenerateThreshold }

// StallTracker 是熔断③的计数器：每轮对账后喂入判据状态的规范化哈希，
// 连续 StallThreshold 轮不变 ⇒ 停滞。
type StallTracker struct {
	lastHash string
	rounds   int
}

// Record 喂入本轮判据状态哈希，返回当前连续无推进轮数。
func (t *StallTracker) Record(hash string) int {
	if hash == "" {
		return t.rounds
	}
	if hash == t.lastHash {
		t.rounds++
		return t.rounds
	}
	t.lastHash = hash
	t.rounds = 0
	return 0
}

// Tripped 报告是否已到停滞阈值。
func (t *StallTracker) Tripped() bool { return t.rounds >= StallThreshold }

// hypothesisTokens 把文本切成词元集合（小写；CJK 单字逐字、拉丁连续段整段）。
// 与 promotion.go 的撞证伪池同款最简分词——两个实现都服务于「词集交集」这一
// 个判定，口径漂移的代价是多判/漏判一次撞池，不引入共享包依赖（internal/edge
// 与 server 互不 import）。
func hypothesisTokens(s string) map[string]int {
	out := map[string]int{}
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			out[string(cur)]++
			cur = cur[:0]
		}
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
			cur = append(cur, lowerRune(r))
		default:
			flush()
			if r >= 0x4E00 && r <= 0x9FFF { // CJK 单字即词元
				out[string(r)]++
			}
		}
	}
	flush()
	return out
}

func lowerRune(r rune) rune {
	if r >= 'A' && r <= 'Z' {
		return r + ('a' - 'A')
	}
	return r
}

// overlapsRefuted 判定假设词元是否与任一条 refuted fact 摘要词集交集 ≥2。
func overlapsRefuted(tokens map[string]int, refuted []string) bool {
	if len(tokens) == 0 || len(refuted) == 0 {
		return false
	}
	for _, summary := range refuted {
		sum := hypothesisTokens(summary)
		overlap := 0
		for tok := range tokens {
			if _, ok := sum[tok]; ok {
				overlap++
				if overlap >= 2 {
					return true
				}
			}
		}
	}
	return false
}

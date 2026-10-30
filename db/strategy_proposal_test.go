package db

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"
)

// proposalTestSuffix 给用例生成互不撞的标识后缀。
//
// 为什么自己写一个而不复用 db/strategies_test.go 里的同名助手：那个文件由
// Task 2 的单元并行持有，它随时可能改动 —— 本单元的用例不该挂在别人的
// 私有助手上。
func proposalTestSuffix() int64 { return time.Now().UnixNano() }

// 策略提议层（Task 4）的验收用例。
//
// 这些用例钉住的是**承诺**：
//   - 没有可比语料 ⇒ 重放闸门**不采纳**（保守默认，而不是「没有证据就算通过」）；
//   - 语料**冻结**：第二次冻结不覆盖已有案例（可变的语料让每次重放比较都失去意义）；
//   - 裁决必须留痕（裁决人与理由进 audit_events 哈希链）；
//   - 投票闸门是**结构性的**：库里那次运行不是「命中」，投票就改不动策略列。

// proposalTestStrategy 建一条合规策略（正文只用参数化模板）。
func proposalTestStrategy(t *testing.T, d *DB, name string) int64 {
	t.Helper()
	id, err := d.CreateStrategy(&Strategy{
		Name:    name,
		Summary: "对 $TARGET 的 $PATH 做一次探测",
		Steps:   json.RawMessage(`["curl -X POST https://$TARGET/$PATH"]`),
	})
	if err != nil {
		t.Fatalf("建策略 %s: %v", name, err)
	}
	return id
}

// TestReplayScoreRefusesWithoutComparableCorpus 钉住「没有可比语料 ⇒ 不采纳」。
//
// 这是重放闸门的保守默认（design §4.3）：没有可比的历史运行，就**不能**说
// 「这条变更在历史里也有效」⇒ comparable=false ⇒ 调用方必须按「不采纳」处理。
//
// ⚠️ 用例用**独一无二的资产类型**做判据：语料表是全库共享的，别处的用例冻结
// 过的案例不该影响这里的判定。
func TestReplayScoreRefusesWithoutComparableCorpus(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer d.Close()

	// 先造一条**别的**资产类型的语料：证明「有语料」不等于「有可比语料」。
	other := fmt.Sprintf("unrelated-type-%d", proposalTestSuffix())
	if err := d.FreezeReplayCorpus(1, AssetSignature{AssetTypes: []string{other}},
		[]string{"m1"}, []string{"confirmed"}); err != nil {
		t.Fatalf("冻结语料: %v", err)
	}

	// 候选的激活条件指向一个库里没有的资产类型 ⇒ 没有任何可比案例。
	missing := fmt.Sprintf("never-seen-type-%d", proposalTestSuffix())
	score, comparable, err := d.ReplayScore(ReplayCandidate{
		Activation: json.RawMessage(fmt.Sprintf(`{"asset_type":[%q]}`, missing)),
		Milestones: []string{"m1"},
	})
	if err != nil {
		t.Fatalf("重放打分: %v", err)
	}
	if comparable {
		t.Fatalf("没有可比语料时 comparable 必须是 false（实际 true，score=%v）", score)
	}
	if score != 0 {
		t.Fatalf("没有可比语料时分数必须是 0（实际 %v）", score)
	}
}

// TestReplayScoreImprovesWithBetterCandidate 钉住「候选变好 ⇒ 重放分数变高」。
//
// 语料：一条资产签名匹配、命中里程碑 m1 的历史运行。
// before 候选：里程碑对不上 ⇒ 命中案例里一个都没命中 ⇒ 0 分。
// after  候选：里程碑对得上 ⇒ 1 分。
func TestReplayScoreImprovesWithBetterCandidate(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer d.Close()

	assetType := fmt.Sprintf("replay-site-%d", proposalTestSuffix())
	if err := d.FreezeReplayCorpus(1, AssetSignature{AssetTypes: []string{assetType}},
		[]string{"m1"}, []string{"confirmed"}); err != nil {
		t.Fatalf("冻结语料: %v", err)
	}
	activation := json.RawMessage(fmt.Sprintf(`{"asset_type":[%q]}`, assetType))

	before, comparable, err := d.ReplayScore(ReplayCandidate{
		Activation: activation,
		Milestones: []string{"m-not-hit"},
	})
	if err != nil {
		t.Fatalf("重放打分: %v", err)
	}
	if !comparable {
		t.Fatalf("有可比语料时 comparable 必须是 true")
	}
	if before != 0 {
		t.Fatalf("里程碑对不上的候选分数应是 0（实际 %v）", before)
	}

	after, comparable, err := d.ReplayScore(ReplayCandidate{
		Activation: activation,
		Milestones: []string{"m1"},
	})
	if err != nil {
		t.Fatalf("重放打分: %v", err)
	}
	if !comparable {
		t.Fatalf("有可比语料时 comparable 必须是 true")
	}
	if after <= before {
		t.Fatalf("更好的候选分数必须更高：before=%v after=%v", before, after)
	}
}

// TestReplayCorpusIsImmutable 钉住「冻结语料不可变」。
//
// 冻结 = 不可变：重放闸门要比的是「当时的世界」。语料一旦被后来的结果改写
// （比如第二次冻结把里程碑换掉），每次重放比较都在跟一个会动的靶子比 ⇒ 闸门
// 失去意义。
//
// ⇒ 第二次冻结必须**不覆盖**已有案例。
func TestReplayCorpusIsImmutable(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer d.Close()

	taskID := int64(900000 + proposalTestSuffix()%100000)
	assetType := fmt.Sprintf("frozen-site-%d", proposalTestSuffix())
	sig := AssetSignature{AssetTypes: []string{assetType}}

	if err := d.FreezeReplayCorpus(taskID, sig, []string{"m1"}, []string{"confirmed"}); err != nil {
		t.Fatalf("首次冻结语料: %v", err)
	}
	// 第二次冻结：同一任务 + 同一资产签名，但里程碑与结局都不同。
	if err := d.FreezeReplayCorpus(taskID, sig, []string{"m2"}, []string{"false_positive"}); err != nil {
		t.Fatalf("二次冻结语料: %v", err)
	}

	rows, err := d.ListReplayCorpus(taskID)
	if err != nil {
		t.Fatalf("读语料: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("语料条数不对：期望 1，实际 %d（冻结语义要求第二次不写入）", len(rows))
	}
	if len(rows[0].MilestoneHits) != 1 || rows[0].MilestoneHits[0] != "m1" {
		t.Fatalf("语料被改写了：里程碑期望 [m1]，实际 %v", rows[0].MilestoneHits)
	}
	if len(rows[0].FindingOutcomes) != 1 || rows[0].FindingOutcomes[0] != "confirmed" {
		t.Fatalf("语料被改写了：漏洞结局期望 [confirmed]，实际 %v", rows[0].FindingOutcomes)
	}
}

// TestStrategyProposalDecisionIsAudited 钉住「裁决必须留痕」。
//
// 提议的裁决是**裁决写**（只有宿主侧能发生）。事后必须能查清「谁批的、为什么批」
// ⇒ 裁决人与理由进 audit_events 哈希链。
//
// 同时钉住「agent 侧只能提议」：创建路径写出来的提议**恒为 pending**。
func TestStrategyProposalDecisionIsAudited(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer d.Close()

	proposalID, err := d.CreateStrategyProposal(&StrategyProposal{
		Route:     StrategyRouteCreateNew,
		Payload:   json.RawMessage(`{"name":"占位"}`),
		Reason:    "子任务总结出一个新可复用件",
		CreatedBy: "agent",
	})
	if err != nil {
		t.Fatalf("记提议: %v", err)
	}

	// 创建路径写不出已裁决的提议 —— 即使调用方自称 approved。
	pending, err := d.ListStrategyProposals(StrategyProposalPending, 100)
	if err != nil {
		t.Fatalf("列待裁决提议: %v", err)
	}
	found := false
	for _, p := range pending {
		if p.ID == proposalID {
			found = true
			if p.Status != StrategyProposalPending {
				t.Fatalf("提议状态必须是 pending（实际 %q）", p.Status)
			}
		}
	}
	if !found {
		t.Fatalf("待裁决列表里找不到刚建的提议 %d", proposalID)
	}

	if err := d.DecideStrategyProposal(proposalID, StrategyProposalApproved, "curator", "重放闸门通过"); err != nil {
		t.Fatalf("裁决提议: %v", err)
	}

	// 裁决必须写明裁决人与理由
	if err := d.DecideStrategyProposal(proposalID, StrategyProposalApproved, "", "没写裁决人"); err == nil {
		t.Fatalf("缺裁决人的裁决被放行（应当拒绝）")
	}
	if err := d.DecideStrategyProposal(proposalID, StrategyProposalApproved, "curator", ""); err == nil {
		t.Fatalf("缺理由的裁决被放行（应当拒绝）")
	}

	var actor, reason, action, payload string
	err = d.QueryRow(`
SELECT actor, reason, action, payload FROM audit_events
WHERE tool_name='strategy_proposal' ORDER BY seq DESC LIMIT 1`).
		Scan(&actor, &reason, &action, &payload)
	if err != nil {
		t.Fatalf("读审计链: %v", err)
	}
	if actor != "curator" {
		t.Fatalf("审计链里的裁决人不对：期望 curator，实际 %q", actor)
	}
	if reason != "重放闸门通过" {
		t.Fatalf("审计链里的裁决理由不对：实际 %q", reason)
	}
	if !strings.Contains(action, StrategyProposalApproved) {
		t.Fatalf("审计链里的裁决动作不对：实际 %q", action)
	}
	if !strings.Contains(payload, strconv.FormatInt(proposalID, 10)) {
		t.Fatalf("审计链载荷里没有提议 id：%q", payload)
	}

	// 审计链本身必须仍然完整（裁决写进了链，不能把链写断）。
	if broken, err := d.VerifyAuditChain(); err != nil || broken != 0 {
		t.Fatalf("审计链断了：seq=%d err=%v", broken, err)
	}
}

// TestApplyStrategyVotesRefusesNonHitUsage 钉住投票闸门的**结构性**。
//
// 「失败子任务永不改库」不能只靠调用方自觉：哪怕调用方伪造了分类，闸门也
// 必须按**库里持久化的结论**（strategy_usage.outcome）把它挡下来。
func TestApplyStrategyVotesRefusesNonHitUsage(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer d.Close()

	strategyID := proposalTestStrategy(t, d, fmt.Sprintf("投票闸门-%d", proposalTestSuffix()))
	taskID := int64(800000 + proposalTestSuffix()%100000)
	intentID := int64(700000 + proposalTestSuffix()%100000)

	// 一次**失败**的运行：usage 里留下的是 outcome='miss'。
	if err := d.RecordStrategyUsage(StrategyUsage{
		StrategyID:     strategyID,
		TaskID:         taskID,
		ExplorationID:  taskID,
		IntentID:       intentID,
		Outcome:        StrategyUsageMiss,
		Classification: StrategyAttributionIndependent,
	}); err != nil {
		t.Fatalf("记使用明细: %v", err)
	}

	before, err := d.GetStrategy(strategyID)
	if err != nil || before == nil {
		t.Fatalf("读策略: %v", err)
	}

	// 伪造一票：分类写 skill_guided，但库里那次运行是 miss ⇒ 必须被拒。
	err = d.ApplyStrategyVotes([]StrategyVote{{
		StrategyID:     strategyID,
		TaskID:         taskID,
		IntentID:       intentID,
		Classification: StrategyAttributionSkillGuided,
		Outcome:        StrategyUsageHit,
	}})
	if err == nil {
		t.Fatalf("失败的运行竟然能改库（应当拒绝：失败子任务永不直接改库）")
	}

	after, err := d.GetStrategy(strategyID)
	if err != nil || after == nil {
		t.Fatalf("读策略: %v", err)
	}
	if after.Confidence != before.Confidence || after.RunUtility != before.RunUtility ||
		after.UsageCount != before.UsageCount || after.HitCount != before.HitCount {
		t.Fatalf("失败的运行改动了策略列：confidence %v→%v run_utility %v→%v usage %v→%v hit %v→%v",
			before.Confidence, after.Confidence, before.RunUtility, after.RunUtility,
			before.UsageCount, after.UsageCount, before.HitCount, after.HitCount)
	}

	// 非 skill_guided 的票（自主探索成功）也不该改库。
	if err := d.ApplyStrategyVotes([]StrategyVote{{
		StrategyID:     strategyID,
		TaskID:         taskID,
		IntentID:       intentID,
		Classification: StrategyAttributionIndependent,
	}}); err == nil {
		t.Fatalf("independent 的票竟然能改库（应当拒绝）")
	}
}

// TestApplyStrategyVotesRaisesConfidenceOnHit 钉住「命中才加票」。
//
// skill_guided 的一次命中：confidence +1、run_utility +1，且**策略状态不变**
// （晋级是裁决写，归因器没有晋级权）。
func TestApplyStrategyVotesRaisesConfidenceOnHit(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer d.Close()

	strategyID := proposalTestStrategy(t, d, fmt.Sprintf("命中加票-%d", proposalTestSuffix()))
	taskID := int64(600000 + proposalTestSuffix()%100000)
	intentID := int64(500000 + proposalTestSuffix()%100000)

	if err := d.RecordStrategyUsage(StrategyUsage{
		StrategyID:     strategyID,
		TaskID:         taskID,
		ExplorationID:  taskID,
		IntentID:       intentID,
		Outcome:        StrategyUsageHit,
		Classification: StrategyAttributionSkillGuided,
		MilestoneHits:  []string{"m1"},
	}); err != nil {
		t.Fatalf("记使用明细: %v", err)
	}
	if err := d.ApplyStrategyVotes([]StrategyVote{{
		StrategyID:      strategyID,
		TaskID:          taskID,
		IntentID:        intentID,
		Classification:  StrategyAttributionSkillGuided,
		Outcome:         StrategyUsageHit,
		MilestoneHits:   []string{"m1"},
		EvidenceRefType: "activity",
		EvidenceRefID:   strconv.FormatInt(intentID, 10),
		ContentDigest:   "sha256:test",
	}}); err != nil {
		t.Fatalf("应用投票: %v", err)
	}

	after, err := d.GetStrategy(strategyID)
	if err != nil || after == nil {
		t.Fatalf("读策略: %v", err)
	}
	if after.Confidence != 1 {
		t.Fatalf("命中一次后 confidence 应该是 1（实际 %v）", after.Confidence)
	}
	if after.RunUtility != 1 {
		t.Fatalf("命中一次后 run_utility 应该是 1（实际 %v）", after.RunUtility)
	}
	if after.State != StrategyQuarantined {
		t.Fatalf("归因器不许改策略状态（实际 %q）", after.State)
	}

	// 证据链应当挂上了（validated_by）。
	var n int
	if err := d.QueryRow(`SELECT count(*) FROM strategy_evidence
		WHERE strategy_id=$1 AND role='validated_by'`, strategyID).Scan(&n); err != nil {
		t.Fatalf("查策略证据: %v", err)
	}
	if n != 1 {
		t.Fatalf("策略证据条数不对：期望 1，实际 %d", n)
	}
}

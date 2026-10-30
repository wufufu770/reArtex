package server

import (
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
)

// 策略库（L4）Task 5 的验收用例。
//
// 这些用例钉住的是**承诺**，不是实现细节：
//   - 晋级的三个条件是「与」关系，缺一不可；没有可比冻结语料 ⇒ 不晋级；
//   - 淘汰判据是两腿（utility / confidence），且受最小观测前置约束；
//   - 最小观测前置数的是 **usage_count**，**不是** retrieval_count；
//   - 唯一证人 pin 的置位与解除全部确定性，**不含任何模型判断**；
//   - pin 复审队列**只提示，不自动解**；
//   - curator 的裁决路径在**结构上**够不到模型（连 import 都没有）。

// curatorTestSuffix 给用例生成互不撞的标识后缀。
//
// 为什么自己写一个而不复用别的测试文件里的同名助手：那些文件由别的单元并行
// 持有，本单元的用例不该挂在别人的私有助手上。
func curatorTestSuffix() int64 { return time.Now().UnixNano() }

// curatorAssetType 给一条策略造一个互不撞的资产类型标记。
//
// 为什么资产类型要唯一：重放闸门比的是「候选 activation 与语料案例签名相交」，
// 两条策略共用同一个资产类型就会共用同一批可比案例 ⇒ 各自的闸门结果互相污染。
func curatorAssetType(sfx int64, tag string) string {
	return fmt.Sprintf("curator-%s-%d", tag, sfx%100000)
}

// curatorStrategy 建一条可控的策略：activation 的资产类型与里程碑 id 都由用例指定。
func curatorStrategy(t *testing.T, pg *db.DB, name, assetType, milestoneID string) int64 {
	t.Helper()
	criteria := json.RawMessage(fmt.Sprintf(
		`[{"id":%q,"where":"detail","match":"(?i)syntax error","kind":"command"}]`, milestoneID))
	activation := json.RawMessage(fmt.Sprintf(`{"asset_type":[%q]}`, assetType))
	id, err := pg.CreateStrategy(&db.Strategy{
		Name:            name,
		Summary:         "对 $TARGET 的 $PATH 做一次探测",
		Steps:           json.RawMessage(`["curl -X POST https://$TARGET/$PATH"]`),
		Activation:      activation,
		SuccessCriteria: criteria,
	})
	if err != nil {
		t.Fatalf("建策略 %s: %v", name, err)
	}
	return id
}

// curatorFreezeCorpus 冻结一条重放语料（可比案例）。
func curatorFreezeCorpus(t *testing.T, pg *db.DB, taskID int64, assetType string, milestones []string) {
	t.Helper()
	sig := db.AssetSignature{AssetTypes: []string{assetType}}
	if err := pg.FreezeReplayCorpus(taskID, sig, milestones, []string{db.FindingConfirmed}); err != nil {
		t.Fatalf("冻结重放语料: %v", err)
	}
}

// curatorAdopt 造 n 次「被实际使用且里程碑命中」的使用明细。
//
// 走**真实写入路径**（RecordStrategyUsage + ApplyStrategyVotes）⇒ usage_count
// 真的涨 n。这正是「最小观测前置按 usage_count 计」的口径来源：数的是真正
// 发生过的事（被采纳并执行），不是「被检索到」。
func curatorAdopt(t *testing.T, pg *db.DB, strategyID int64, n int, milestoneID string) {
	t.Helper()
	base := curatorTestSuffix() % 1000
	for i := 0; i < n; i++ {
		taskID := int64(700000) + base*100 + int64(i)
		intentID := int64(800000) + base*100 + int64(i)
		if err := pg.RecordStrategyUsage(db.StrategyUsage{
			StrategyID:     strategyID,
			TaskID:         taskID,
			ExplorationID:  1,
			IntentID:       intentID,
			Outcome:        db.StrategyUsageHit,
			Classification: db.StrategyAttributionSkillGuided,
			MilestoneHits:  []string{milestoneID},
		}); err != nil {
			t.Fatalf("记策略使用明细: %v", err)
		}
		if err := pg.ApplyStrategyVotes([]db.StrategyVote{{
			StrategyID:     strategyID,
			TaskID:         taskID,
			ExplorationID:  1,
			IntentID:       intentID,
			Classification: db.StrategyAttributionSkillGuided,
			Outcome:        db.StrategyUsageHit,
			MilestoneHits:  []string{milestoneID},
		}}); err != nil {
			t.Fatalf("应用策略投票: %v", err)
		}
	}
}

// curatorFinding 建一条指定状态的 finding。
func curatorFinding(t *testing.T, pg *db.DB, status string) int64 {
	t.Helper()
	id, err := pg.AddFinding(0, 0, "sqli", "测试漏洞", "high", "摘要", "证据文本", "worker", nil)
	if err != nil {
		t.Fatalf("建 finding: %v", err)
	}
	if _, err := pg.Exec(`UPDATE findings SET status=$2 WHERE id=$1`, id, status); err != nil {
		t.Fatalf("设 finding 状态: %v", err)
	}
	return id
}

// curatorSnapshot 写一条流量证据快照。
func curatorSnapshot(t *testing.T, pg *db.DB, id, url string) {
	t.Helper()
	if _, err := pg.Exec(`INSERT INTO traffic_evidence_snapshots
		(id, source_traffic_id, captured_at, url, method, status, content_type,
		 req_head, resp_head, req_hash, resp_hash, req_len, resp_len)
		VALUES($1,$1,0,$2,'GET',200,'','h','h','h','h',0,0)
		ON CONFLICT (id) DO NOTHING`, id, url); err != nil {
		t.Fatalf("写流量快照: %v", err)
	}
}

// curatorBindSnapshot 把流量快照绑到 finding 上。
func curatorBindSnapshot(t *testing.T, pg *db.DB, findingID int64, snapshotID string) {
	t.Helper()
	if _, err := pg.Exec(`INSERT INTO finding_traffic_bindings(finding_id, snapshot_id, role, position)
		VALUES($1,$2,'supporting',1) ON CONFLICT (finding_id, snapshot_id) DO NOTHING`,
		findingID, snapshotID); err != nil {
		t.Fatalf("绑流量快照: %v", err)
	}
}

// curatorSetColumn 直接改 strategies 的某个计数/分数列（构造场景用）。
//
// 为什么允许直接改：这些列的**写入路径**由归因器（ApplyStrategyVotes）独占，
// 而本用例要构造的是「观测次数不足 / 置信度被踩到负值」这类边界场景 ——
// 走写入路径去构造它们要绕很大一圈。用例验证的是**判定逻辑**读的是哪一列。
func curatorSetColumn(t *testing.T, pg *db.DB, strategyID int64, column string, value any) {
	t.Helper()
	switch column {
	case "usage_count", "retrieval_count", "hit_count", "miss_count":
		if _, err := pg.Exec(fmt.Sprintf(`UPDATE strategies SET %s=$2 WHERE id=$1`, column),
			strategyID, value); err != nil {
			t.Fatalf("改 %s: %v", column, err)
		}
	case "confidence":
		if _, err := pg.Exec(`UPDATE strategies SET confidence=$2 WHERE id=$1`,
			strategyID, value); err != nil {
			t.Fatalf("改 confidence: %v", err)
		}
	default:
		t.Fatalf("用例不允许改列 %q", column)
	}
}

// curatorState 读策略的当前状态。
func curatorState(t *testing.T, pg *db.DB, strategyID int64) *db.Strategy {
	t.Helper()
	s, err := pg.GetStrategy(strategyID)
	if err != nil {
		t.Fatalf("读策略 %d: %v", strategyID, err)
	}
	if s == nil {
		t.Fatalf("策略 %d 不存在", strategyID)
	}
	return s
}

// TestCuratorThresholdsMatchRulings 钉住阈值常量就是裁决里那四个值。
//
// 这条用例是「口径」的守门人：阈值一旦被改动（哪怕只改了注释里的数），
// 用例会立刻红。
func TestCuratorThresholdsMatchRulings(t *testing.T) {
	if db.StrategyTauU != 0.05 {
		t.Fatalf("淘汰阈值 τ_u 应为 0.05（与 Selm 论文超参表对齐），实际 %v", db.StrategyTauU)
	}
	if db.StrategyConfidenceRetireThreshold != -0.3 {
		t.Fatalf("confidence 腿阈值应为 -0.3，实际 %v", db.StrategyConfidenceRetireThreshold)
	}
	if db.StrategyMinObservations != 5 {
		t.Fatalf("最小观测前置应为 5，实际 %v", db.StrategyMinObservations)
	}
	if db.StrategyPinQueueThreshold != 50 {
		t.Fatalf("pin 复审队列阈值应为 50，实际 %v", db.StrategyPinQueueThreshold)
	}
}

// TestPromotionRequiresAllThreeConditions 钉住晋级的三个条件是「与」关系。
//
//	A) 重放通过 + usage_count>=n + 里程碑命中 → Eligible
//	B) 重放通过 + 里程碑命中，但 usage_count<n   → 不得晋级
//	C) 重放不通过（候选在语料上没命中）           → 不得晋级
//	D) 重放通过 + usage_count 够，但没里程碑命中   → 不得晋级
//
// ⇒ 三个条件是「与」关系，不是「或」。
func TestPromotionRequiresAllThreeConditions(t *testing.T) {
	_, pg := taskHallTestServer(t)
	sfx := curatorTestSuffix() % 100000

	typeA := curatorAssetType(sfx, "a")
	typeB := curatorAssetType(sfx, "b")
	typeC := curatorAssetType(sfx, "c")
	typeD := curatorAssetType(sfx, "d")

	// A) 三条件全过。
	aID := curatorStrategy(t, pg, fmt.Sprintf("晋级A-%d", sfx), typeA, "m1")
	curatorFreezeCorpus(t, pg, int64(700001)+sfx, typeA, []string{"m1"})
	curatorAdopt(t, pg, aID, db.StrategyMinObservations, "m1")

	// B) 重放通过 + 里程碑命中，但观测次数不够。
	bID := curatorStrategy(t, pg, fmt.Sprintf("晋级B-%d", sfx), typeB, "m1")
	curatorFreezeCorpus(t, pg, int64(700002)+sfx, typeB, []string{"m1"})
	curatorAdopt(t, pg, bID, db.StrategyMinObservations-1, "m1")

	// C) 重放不通过：语料命中的是别的里程碑。
	cID := curatorStrategy(t, pg, fmt.Sprintf("晋级C-%d", sfx), typeC, "m2")
	curatorFreezeCorpus(t, pg, int64(700003)+sfx, typeC, []string{"m1"})
	curatorAdopt(t, pg, cID, db.StrategyMinObservations, "m2")

	// D) 重放通过 + 观测次数够，但真实运行里没命中过它的里程碑。
	dID := curatorStrategy(t, pg, fmt.Sprintf("晋级D-%d", sfx), typeD, "m1")
	curatorFreezeCorpus(t, pg, int64(700004)+sfx, typeD, []string{"m1"})
	curatorAdopt(t, pg, dID, db.StrategyMinObservations, "m-other")

	cases := []struct {
		name      string
		id        int64
		want      bool
		wantWhy   string
		checkPart func(PromotionDecision) bool
	}{
		{"A 三条件全过", aID, true, "三条件全过应当晋级", func(d PromotionDecision) bool {
			return d.ReplayPassed && d.UsageCount >= db.StrategyMinObservations && d.MilestoneHit
		}},
		{"B 观测次数不足", bID, false, "usage_count 不足不得晋级", func(d PromotionDecision) bool {
			return d.ReplayPassed && d.UsageCount < db.StrategyMinObservations && d.MilestoneHit
		}},
		{"C 重放不通过", cID, false, "重放闸门不通过不得晋级", func(d PromotionDecision) bool {
			return !d.ReplayPassed && d.UsageCount >= db.StrategyMinObservations && d.MilestoneHit
		}},
		{"D 没里程碑命中", dID, false, "里程碑没命中不得晋级", func(d PromotionDecision) bool {
			return d.ReplayPassed && d.UsageCount >= db.StrategyMinObservations && !d.MilestoneHit
		}},
	}
	for _, c := range cases {
		s := curatorState(t, pg, c.id)
		dec, err := EvaluatePromotion(pg, *s)
		if err != nil {
			t.Fatalf("%s: 晋级判定: %v", c.name, err)
		}
		if !c.checkPart(dec) {
			t.Fatalf("%s: 判定明细不对：%+v", c.name, dec)
		}
		if dec.Eligible != c.want {
			t.Fatalf("%s: %s（实际 Eligible=%v，明细 %+v）", c.name, c.wantWhy, dec.Eligible, dec)
		}
	}
}

// TestPromotionRefusesWithoutComparableCorpus 钉住「没有可比语料 ⇒ 不晋级」。
//
// 这是保守默认：没有可比语料就没有判据，此时晋级等于凭空提权。
func TestPromotionRefusesWithoutComparableCorpus(t *testing.T) {
	_, pg := taskHallTestServer(t)
	sfx := curatorTestSuffix() % 100000

	// 语料里只有另一种资产类型的案例 ⇒ 本策略没有可比案例。
	otherType := curatorAssetType(sfx, "other")
	curatorFreezeCorpus(t, pg, int64(700010)+sfx, otherType, []string{"m1"})

	id := curatorStrategy(t, pg, fmt.Sprintf("无语料晋级-%d", sfx), curatorAssetType(sfx, "nocon"), "m1")
	curatorAdopt(t, pg, id, db.StrategyMinObservations, "m1")

	s := curatorState(t, pg, id)
	dec, err := EvaluatePromotion(pg, *s)
	if err != nil {
		t.Fatalf("晋级判定: %v", err)
	}
	if dec.ReplayComparable {
		t.Fatalf("没有可比语料却判成可比：%+v", dec)
	}
	if dec.Eligible {
		t.Fatalf("没有可比语料却判成可晋级：%+v", dec)
	}
}

// TestQuarantinedIsNeverInjectedIntoRetrieval 钉住隔离态策略不进检索注入。
//
// 晋级之前它必须**完全不可见** —— 否则一条未经仲裁的策略会先被套到目标上。
func TestQuarantinedIsNeverInjectedIntoRetrieval(t *testing.T) {
	_, pg := taskHallTestServer(t)
	sfx := curatorTestSuffix() % 100000
	assetType := curatorAssetType(sfx, "inject")

	quarantined := curatorStrategy(t, pg, fmt.Sprintf("隔离态-%d", sfx), assetType, "m1")
	active := curatorStrategy(t, pg, fmt.Sprintf("已晋级-%d", sfx), assetType, "m1")
	ok, err := pg.PromoteStrategyToActive(active)
	if err != nil {
		t.Fatalf("晋级: %v", err)
	}
	if !ok {
		t.Fatalf("晋级没有生效")
	}

	sig := db.AssetSignature{AssetTypes: []string{assetType}}
	matches, err := pg.SearchActiveStrategies(0, sig, db.StrategySearchTopK, 0)
	if err != nil {
		t.Fatalf("检索策略: %v", err)
	}
	for _, m := range matches {
		if m.ID == quarantined {
			t.Fatalf("隔离态策略被检索注入了")
		}
	}
	found := false
	for _, m := range matches {
		if m.ID == active {
			found = true
		}
	}
	if !found {
		t.Fatalf("active 策略没被检索到")
	}
}

// TestRetirementRequiresMinimumObservations 钉住最小观测前置。
//
//	① usage_count < n 的策略即使 utility 很低也不退役；
//	② 计的是 **usage_count**，不是 retrieval_count —— 被检索多次但从未被采纳的
//	   策略不算「有观测」。
func TestRetirementRequiresMinimumObservations(t *testing.T) {
	_, pg := taskHallTestServer(t)
	sfx := curatorTestSuffix() % 100000

	// S1：utility 极低、confidence 极负，但**只被用过一次**。
	typeS1 := curatorAssetType(sfx, "s1")
	s1 := curatorStrategy(t, pg, fmt.Sprintf("观测不足-%d", sfx), typeS1, "m1")
	curatorFreezeCorpus(t, pg, int64(700020)+sfx, typeS1, []string{"m1"})
	curatorAdopt(t, pg, s1, 1, "m1")
	f1 := curatorFinding(t, pg, db.FindingFalsePositive)
	if err := pg.LinkStrategyEvidence(s1, "finding", strconv.FormatInt(f1, 10), "validated_by", "sha256:s1"); err != nil {
		t.Fatalf("挂证据: %v", err)
	}
	// 检索次数拉满，但**从未被采纳**：usage_count 仍是 1。
	curatorSetColumn(t, pg, s1, "retrieval_count", 500)
	curatorSetColumn(t, pg, s1, "confidence", -1.0)
	if err := pg.RefreshStrategyUtility(); err != nil {
		t.Fatalf("刷新 utility: %v", err)
	}

	dec1, err := EvaluateRetirement(pg, *curatorState(t, pg, s1))
	if err != nil {
		t.Fatalf("淘汰判定: %v", err)
	}
	if dec1.UsageCount >= db.StrategyMinObservations {
		t.Fatalf("用例前提不对：usage_count=%d", dec1.UsageCount)
	}
	if dec1.Retire {
		t.Fatalf("观测次数不足却判成退役：%+v", dec1)
	}
	if dec1.Eligible {
		t.Fatalf("观测次数不足却判成可评估：%+v", dec1)
	}

	// S2：观测次数够 ⇒ 同样的低 utility 就会触发退役（utility 腿）。
	typeS2 := curatorAssetType(sfx, "s2")
	s2 := curatorStrategy(t, pg, fmt.Sprintf("观测够了-%d", sfx), typeS2, "m1")
	curatorFreezeCorpus(t, pg, int64(700021)+sfx, typeS2, []string{"m1"})
	curatorAdopt(t, pg, s2, db.StrategyMinObservations, "m1")
	f2 := curatorFinding(t, pg, db.FindingFalsePositive)
	if err := pg.LinkStrategyEvidence(s2, "finding", strconv.FormatInt(f2, 10), "validated_by", "sha256:s2"); err != nil {
		t.Fatalf("挂证据: %v", err)
	}
	curatorSetColumn(t, pg, s2, "confidence", 0.0)
	if err := pg.RefreshStrategyUtility(); err != nil {
		t.Fatalf("刷新 utility: %v", err)
	}

	dec2, err := EvaluateRetirement(pg, *curatorState(t, pg, s2))
	if err != nil {
		t.Fatalf("淘汰判定: %v", err)
	}
	if !dec2.Eligible {
		t.Fatalf("前置齐了却判成不可评估：%+v", dec2)
	}
	if !dec2.Retire {
		t.Fatalf("utility 低于阈值却没判成退役：%+v", dec2)
	}
	if dec2.Utility >= db.StrategyTauU {
		t.Fatalf("用例前提不对：utility=%v", dec2.Utility)
	}
}

// TestRetirementConfidenceLegNeedsClearlyNegativeVote 钉住 confidence 腿的严苛度。
//
// 阈值是 **-0.3**：只有被反复 down-vote 到**明显负值**才退役。
//
//	confidence = -0.2（还没到 -0.3）⇒ 不退役；
//	confidence = -0.3（恰好等于阈值，严格小于才成立）⇒ 不退役；
//	confidence = -0.5 ⇒ 退役。
func TestRetirementConfidenceLegNeedsClearlyNegativeVote(t *testing.T) {
	_, pg := taskHallTestServer(t)
	sfx := curatorTestSuffix() % 100000

	mk := func(tag string, confidence float64) (int64, RetirementDecision) {
		t.Helper()
		at := curatorAssetType(sfx, tag)
		id := curatorStrategy(t, pg, fmt.Sprintf("置信腿-%s-%d", tag, sfx), at, "m1")
		curatorFreezeCorpus(t, pg, int64(700030)+sfx+int64(len(tag)), at, []string{"m1"})
		curatorAdopt(t, pg, id, db.StrategyMinObservations, "m1")
		f := curatorFinding(t, pg, db.FindingConfirmed) // utility = 1.0 ⇒ utility 腿不成立
		if err := pg.LinkStrategyEvidence(id, "finding", strconv.FormatInt(f, 10), "validated_by", "sha256:c"); err != nil {
			t.Fatalf("挂证据: %v", err)
		}
		curatorSetColumn(t, pg, id, "confidence", confidence)
		if err := pg.RefreshStrategyUtility(); err != nil {
			t.Fatalf("刷新 utility: %v", err)
		}
		dec, err := EvaluateRetirement(pg, *curatorState(t, pg, id))
		if err != nil {
			t.Fatalf("淘汰判定: %v", err)
		}
		return id, dec
	}

	_, mild := mk("mild", -0.2)
	if mild.Retire {
		t.Fatalf("confidence=-0.2 不该触发退役（阈值 -0.3）：%+v", mild)
	}
	_, edge := mk("edge", db.StrategyConfidenceRetireThreshold)
	if edge.Retire {
		t.Fatalf("confidence 恰好等于阈值不该触发退役（判据是严格小于）：%+v", edge)
	}
	_, harsh := mk("harsh", -0.5)
	if !harsh.Retire {
		t.Fatalf("confidence=-0.5 应当触发退役：%+v", harsh)
	}
}

// TestRetirementNeverTouchesPinnedStrategy 钉住「唯一证人 pin 的策略不参与任何自动淘汰」。
func TestRetirementNeverTouchesPinnedStrategy(t *testing.T) {
	_, pg := taskHallTestServer(t)
	sfx := curatorTestSuffix() % 100000

	at := curatorAssetType(sfx, "pinned")
	id := curatorStrategy(t, pg, fmt.Sprintf("被 pin 的-%d", sfx), at, "m1")
	curatorFreezeCorpus(t, pg, int64(700040)+sfx, at, []string{"m1"})
	curatorAdopt(t, pg, id, db.StrategyMinObservations, "m1")
	f := curatorFinding(t, pg, db.FindingFalsePositive)
	if err := pg.LinkStrategyEvidence(id, "finding", strconv.FormatInt(f, 10), "validated_by", "sha256:p"); err != nil {
		t.Fatalf("挂证据: %v", err)
	}
	curatorSetColumn(t, pg, id, "confidence", -1.0)
	if ok, err := pg.ApplyStrategyPin(id, true); err != nil || !ok {
		t.Fatalf("置 pin: %v", err)
	}
	if err := pg.RefreshStrategyUtility(); err != nil {
		t.Fatalf("刷新 utility: %v", err)
	}

	dec, err := EvaluateRetirement(pg, *curatorState(t, pg, id))
	if err != nil {
		t.Fatalf("淘汰判定: %v", err)
	}
	if dec.Retire {
		t.Fatalf("被 pin 的策略被判成退役：%+v", dec)
	}
	if dec.Eligible {
		t.Fatalf("被 pin 的策略被判成可评估：%+v", dec)
	}
}

// TestPinIsSetWhenStrategyIsOnlyValidatedPath 钉住「唯一 validated_by 路径 ⇒ 置位」。
//
//	只有一条 validated_by 链接 ⇒ 该策略是唯一路径；
//	加了第二条（另一条策略的）validated_by 链接 ⇒ 谁也不再是唯一路径。
func TestPinIsSetWhenStrategyIsOnlyValidatedPath(t *testing.T) {
	_, pg := taskHallTestServer(t)
	sfx := curatorTestSuffix() % 100000

	finding := curatorFinding(t, pg, db.FindingConfirmed)
	a := curatorStrategy(t, pg, fmt.Sprintf("唯一路径A-%d", sfx), curatorAssetType(sfx, "pa"), "m1")
	b := curatorStrategy(t, pg, fmt.Sprintf("唯一路径B-%d", sfx), curatorAssetType(sfx, "pb"), "m1")

	if err := pg.LinkStrategyEvidence(a, "finding", strconv.FormatInt(finding, 10), "validated_by", "sha256:a"); err != nil {
		t.Fatalf("挂证据: %v", err)
	}
	decA, err := EvaluatePin(pg, *curatorState(t, pg, a))
	if err != nil {
		t.Fatalf("pin 判定: %v", err)
	}
	if !decA.ShouldPin {
		t.Fatalf("唯一的 validated_by 路径没有被判成该置位：%+v", decA)
	}

	// 另一条策略也验证了同一个 finding ⇒ 谁也不再是唯一路径。
	if err := pg.LinkStrategyEvidence(b, "finding", strconv.FormatInt(finding, 10), "validated_by", "sha256:b"); err != nil {
		t.Fatalf("挂证据: %v", err)
	}
	decA2, err := EvaluatePin(pg, *curatorState(t, pg, a))
	if err != nil {
		t.Fatalf("pin 判定: %v", err)
	}
	if decA2.ShouldPin {
		t.Fatalf("出现第二条路径后仍被判成唯一路径：%+v", decA2)
	}
	decB, err := EvaluatePin(pg, *curatorState(t, pg, b))
	if err != nil {
		t.Fatalf("pin 判定: %v", err)
	}
	if decB.ShouldPin {
		t.Fatalf("第二条路径被判成唯一路径：%+v", decB)
	}
}

// TestPinReleaseIsMechanicalNotJudged 钉住 pin 的解除只认两条**机械可判定**的条件。
//
//	条件①：同一 finding 下出现指向**不同 url** 的新流量证据 ⇒ 解 pin；
//	条件②：finding.status 不再是 confirmed              ⇒ 解 pin；
//	条件③：两条都不满足 ⇒ **pin 保持**（包括「策略很老」—— 策略的价值与年龄无关）。
//
// ⇒ 全部靠关系查询 + 状态比，没有模型判断。
func TestPinReleaseIsMechanicalNotJudged(t *testing.T) {
	_, pg := taskHallTestServer(t)
	sfx := curatorTestSuffix() % 100000
	curator := NewCurator(pg)

	// —— 条件③：什么都没变 ⇒ pin 保持。——
	at1 := curatorAssetType(sfx, "pin1")
	strategy := curatorStrategy(t, pg, fmt.Sprintf("pin 保持-%d", sfx), at1, "m1")
	finding := curatorFinding(t, pg, db.FindingConfirmed)
	snap1 := fmt.Sprintf("snap-%d-1", sfx)
	curatorSnapshot(t, pg, snap1, "http://target.invalid/"+at1)
	curatorBindSnapshot(t, pg, finding, snap1)
	if err := pg.LinkStrategyEvidence(strategy, "finding", strconv.FormatInt(finding, 10), "validated_by", "sha256:f1"); err != nil {
		t.Fatalf("挂 finding 证据: %v", err)
	}
	if err := pg.LinkStrategyEvidence(strategy, "traffic_snapshot", snap1, "validated_by", "sha256:s1"); err != nil {
		t.Fatalf("挂流量证据: %v", err)
	}
	if _, err := curator.RunOnce(context.Background()); err != nil {
		t.Fatalf("跑 curator: %v", err)
	}
	if !curatorState(t, pg, strategy).Pinned {
		t.Fatalf("唯一证人策略没有被置位")
	}

	// 年龄不构成解除理由：把时间戳推到很久以前，pin 依然保持。
	if _, err := pg.Exec(`UPDATE strategies SET created_at=now()-interval '900 days',
		updated_at=now()-interval '900 days' WHERE id=$1`, strategy); err != nil {
		t.Fatalf("改时间戳: %v", err)
	}
	if _, err := curator.RunOnce(context.Background()); err != nil {
		t.Fatalf("跑 curator: %v", err)
	}
	if !curatorState(t, pg, strategy).Pinned {
		t.Fatalf("pin 被按年龄解除了 —— 策略的价值与年龄无关")
	}

	// —— 条件①：同 finding 下出现指向不同 url 的新流量证据 ⇒ 解 pin。——
	snap2 := fmt.Sprintf("snap-%d-2", sfx)
	curatorSnapshot(t, pg, snap2, "http://target.invalid/other-"+at1)
	curatorBindSnapshot(t, pg, finding, snap2)
	if _, err := curator.RunOnce(context.Background()); err != nil {
		t.Fatalf("跑 curator: %v", err)
	}
	if curatorState(t, pg, strategy).Pinned {
		t.Fatalf("出现指向不同 url 的新流量证据后 pin 没有解除")
	}

	// —— 条件②：finding 不再 confirmed ⇒ 解 pin。——
	at2 := curatorAssetType(sfx, "pin2")
	strategy2 := curatorStrategy(t, pg, fmt.Sprintf("pin 状态变-%d", sfx), at2, "m1")
	finding2 := curatorFinding(t, pg, db.FindingConfirmed)
	snap3 := fmt.Sprintf("snap-%d-3", sfx)
	curatorSnapshot(t, pg, snap3, "http://target.invalid/"+at2)
	curatorBindSnapshot(t, pg, finding2, snap3)
	if err := pg.LinkStrategyEvidence(strategy2, "finding", strconv.FormatInt(finding2, 10), "validated_by", "sha256:f2"); err != nil {
		t.Fatalf("挂 finding 证据: %v", err)
	}
	if err := pg.LinkStrategyEvidence(strategy2, "traffic_snapshot", snap3, "validated_by", "sha256:s3"); err != nil {
		t.Fatalf("挂流量证据: %v", err)
	}
	if _, err := curator.RunOnce(context.Background()); err != nil {
		t.Fatalf("跑 curator: %v", err)
	}
	if !curatorState(t, pg, strategy2).Pinned {
		t.Fatalf("唯一证人策略没有被置位")
	}
	if _, err := pg.Exec(`UPDATE findings SET status=$2 WHERE id=$1`, finding2, db.FindingResolved); err != nil {
		t.Fatalf("改 finding 状态: %v", err)
	}
	if _, err := curator.RunOnce(context.Background()); err != nil {
		t.Fatalf("跑 curator: %v", err)
	}
	if curatorState(t, pg, strategy2).Pinned {
		t.Fatalf("finding 不再 confirmed 后 pin 没有解除")
	}
}

// TestPinQueueIsAdvisoryOnly 钉住 pin 复审队列**只提示、不自动解**。
//
// 队列超过阈值时生成条目，但**没有任何一条 pin 被自动解除**。
func TestPinQueueIsAdvisoryOnly(t *testing.T) {
	_, pg := taskHallTestServer(t)
	sfx := curatorTestSuffix() % 100000

	ids := make([]int64, 0, db.StrategyPinQueueThreshold+1)
	for i := 0; i <= db.StrategyPinQueueThreshold; i++ {
		id := curatorStrategy(t, pg, fmt.Sprintf("复审队列-%d-%d", sfx, i),
			curatorAssetType(sfx, fmt.Sprintf("q%d", i)), "m1")
		if ok, err := pg.ApplyStrategyPin(id, true); err != nil || !ok {
			t.Fatalf("置 pin: %v", err)
		}
		ids = append(ids, id)
	}

	rep, err := NewCurator(pg).RunOnce(context.Background())
	if err != nil {
		t.Fatalf("跑 curator: %v", err)
	}
	if len(rep.PinQueue) == 0 {
		t.Fatalf("pinned 策略超过阈值却没有生成复审队列条目")
	}
	for _, id := range ids {
		if !curatorState(t, pg, id).Pinned {
			t.Fatalf("pin %d 被自动解除了 —— 队列只提示，不自动放行", id)
		}
	}
	for _, id := range rep.Unpinned {
		for _, pinned := range ids {
			if pinned == id {
				t.Fatalf("复审队列里的 pin %d 出现在自动解 pin 列表里", id)
			}
		}
	}
}

// TestTTLProducesTombstone 钉住硬 TTL 到期 ⇒ tombstone（软删除，行与证据都留着）。
func TestTTLProducesTombstone(t *testing.T) {
	_, pg := taskHallTestServer(t)
	sfx := curatorTestSuffix() % 100000

	id := curatorStrategy(t, pg, fmt.Sprintf("到期-%d", sfx), curatorAssetType(sfx, "ttl"), "m1")
	if _, err := pg.Exec(`UPDATE strategies SET expired_at=now()-interval '1 hour' WHERE id=$1`, id); err != nil {
		t.Fatalf("设过期时间: %v", err)
	}
	rep, err := NewCurator(pg).RunOnce(context.Background())
	if err != nil {
		t.Fatalf("跑 curator: %v", err)
	}
	s := curatorState(t, pg, id)
	if s.State != db.StrategyTombstone {
		t.Fatalf("到期策略状态应为 %s，实际 %s", db.StrategyTombstone, s.State)
	}
	found := false
	for _, x := range rep.Tombstoned {
		if x == id {
			found = true
		}
	}
	if !found {
		t.Fatalf("报告里没有墓碑化的策略 id：%+v", rep)
	}
}

// TestRetiredStrategyKeepsEvidenceAndReason 钉住「退役 ≠ 删除」。
//
// 退役后：state='retired'、retired_reason 非空、证据链**一条不少**。
func TestRetiredStrategyKeepsEvidenceAndReason(t *testing.T) {
	_, pg := taskHallTestServer(t)
	sfx := curatorTestSuffix() % 100000

	at := curatorAssetType(sfx, "retire")
	id := curatorStrategy(t, pg, fmt.Sprintf("退役保留-%d", sfx), at, "m1")
	curatorFreezeCorpus(t, pg, int64(700050)+sfx, at, []string{"m1"})
	curatorAdopt(t, pg, id, db.StrategyMinObservations, "m1")
	f := curatorFinding(t, pg, db.FindingFalsePositive)
	if err := pg.LinkStrategyEvidence(id, "finding", strconv.FormatInt(f, 10), "validated_by", "sha256:r"); err != nil {
		t.Fatalf("挂证据: %v", err)
	}
	curatorSetColumn(t, pg, id, "confidence", 0.0)
	if ok, err := pg.PromoteStrategyToActive(id); err != nil || !ok {
		t.Fatalf("晋级: %v", err)
	}

	if _, err := NewCurator(pg).RunOnce(context.Background()); err != nil {
		t.Fatalf("跑 curator: %v", err)
	}
	s := curatorState(t, pg, id)
	if s.State != db.StrategyRetired {
		t.Fatalf("状态应为 %s，实际 %s", db.StrategyRetired, s.State)
	}
	if strings.TrimSpace(s.RetiredReason) == "" {
		t.Fatalf("退役必须留下 retired_reason")
	}
	links, err := pg.StrategyEvidence(id)
	if err != nil {
		t.Fatalf("读证据链: %v", err)
	}
	if len(links) == 0 {
		t.Fatalf("退役后证据链被清空了 —— 退役 ≠ 删除")
	}
}

// TestCuratorNeverConsultsAModel 钉住「裁决写不经过模型」是**结构**事实。
//
// 断言两层：
//  1. 静态：server/strategy_curator.go **不 import** 任何能拿到模型的包
//     （agent / llm / llmrec / llmpool / norma-llm）；
//  2. 静态：文件里没有任何标识符带 llm / model / prompt / judge / completion
//     —— 判定路径上根本没有「问模型」这件事的载体。
//
// 为什么必须结构性：粗糙 judge（反模式 4）让同一批证据在不同轮次得到不同结论，
// 裁决不可复现 ⇒ 事后无法复盘、无法追责。
func TestCuratorNeverConsultsAModel(t *testing.T) {
	forbiddenImports := []string{
		"github.com/Autumn-27/artex/agent",
		"github.com/Autumn-27/artex/llmrec",
		"github.com/Autumn-27/artex/llmpool",
		"github.com/Autumn-27/norma/llm",
		"github.com/Autumn-27/norma/tool",
		"net/http",
	}
	forbiddenIdents := []string{"llm", "model", "prompt", "judge", "completion"}

	path := filepath.Join(".", "strategy_curator.go")
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("解析 %s: %v", path, err)
	}
	for _, spec := range file.Imports {
		p, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			t.Fatalf("解析 import 路径: %v", err)
		}
		for _, bad := range forbiddenImports {
			if p == bad || strings.Contains(p, bad) {
				t.Fatalf("裁决文件引入了能拿到模型的包 %q", p)
			}
		}
	}
	ast.Inspect(file, func(n ast.Node) bool {
		id, ok := n.(*ast.Ident)
		if !ok {
			return true
		}
		low := strings.ToLower(id.Name)
		for _, bad := range forbiddenIdents {
			if strings.Contains(low, bad) {
				t.Fatalf("裁决文件里出现了与模型相关的标识符 %q", id.Name)
			}
		}
		return true
	})
}

// TestPinReleaseHasNoModelAndNoAgePath 钉住 pin 的自动解除只有两条机械条件。
//
// 这条用例是对上条的**加强**：它不看 import，而是直接钉住「解除判定」的输入 ——
// 判定函数只接受 (库, 策略)，不接受任何「模型意见」或「年龄」参数；策略的年龄
// 与解除与否无关（见 TestPinReleaseIsMechanicalNotJudged 里的年龄断言）。
func TestPinReleaseHasNoModelAndNoAgePath(t *testing.T) {
	_, pg := taskHallTestServer(t)
	sfx := curatorTestSuffix() % 100000

	// 一条既没有验证路径、finding 也不存在的策略：解除的两条条件都不成立。
	id := curatorStrategy(t, pg, fmt.Sprintf("无解除路径-%d", sfx), curatorAssetType(sfx, "norel"), "m1")
	dec, err := EvaluatePin(pg, *curatorState(t, pg, id))
	if err != nil {
		t.Fatalf("pin 判定: %v", err)
	}
	if dec.ShouldUnpin {
		t.Fatalf("没有任何机械条件成立却判成要解 pin：%+v", dec)
	}
	// 判定必须给出理由（这是「裁决可追责」的一部分）。
	//
	// ⚠️ 这里**不钉具体措辞**：本场景的策略从未被 pin，落的是「不置位」分支，
	// 而「pin 保持」是**已置位且两条自动解除条件都不成立**那条分支的措辞 ——
	// 两者是不同分支，拿另一分支的词来断言就是串了分支。措辞会变，语义不会：
	// 不解除、不置位，且给出了理由。
	if dec.Reason == "" {
		t.Fatalf("判定必须说明理由：%+v", dec)
	}
	if dec.ShouldPin {
		t.Fatalf("没有任何机械条件成立却判成要置 pin：%+v", dec)
	}
}

// TestCuratorWatermarkWrittenAfterRun 钉住「水位在跑完之后才写」。
//
// ① 没有水位 ⇒ 该跑；
// ② 跑失败（ctx 已取消）⇒ **不写水位**（写在跑之前会把失败当成成功，
//
//	崩溃重启会立刻再跑一轮全库淘汰，把 PG 拖死）；
//
// ③ 跑成功 ⇒ 水位 = 跑的时刻；紧接着再调一次 ⇒ 不再跑（小时级频率，不是 5 秒级）。
func TestCuratorWatermarkWrittenAfterRun(t *testing.T) {
	_, pg := taskHallTestServer(t)

	if _, err := pg.Exec(`DELETE FROM scheduler_state WHERE key=$1`, SchedKeyStrategyCurator); err != nil {
		t.Fatalf("清水位: %v", err)
	}

	now := time.Now()
	due, err := ShouldRunCurator(pg, now)
	if err != nil {
		t.Fatalf("判水位: %v", err)
	}
	if !due {
		t.Fatalf("没有水位时应当该跑")
	}

	// 跑失败 ⇒ 不写水位。
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, ran, err := RunCuratorIfDue(cancelled, pg, now); err == nil || !ran {
		t.Fatalf("被取消的运行应当报错且算作跑过（实际 err=%v ran=%v）", err, ran)
	}
	raw, err := pg.GetSchedState(SchedKeyStrategyCurator)
	if err != nil {
		t.Fatalf("读水位: %v", err)
	}
	if raw != "" {
		t.Fatalf("运行失败却写了水位 %q —— 水位必须在跑完之后写", raw)
	}

	// 跑成功 ⇒ 写水位，且紧接着不再跑。
	if _, ran, err := RunCuratorIfDue(context.Background(), pg, now); err != nil || !ran {
		t.Fatalf("应当跑一轮（err=%v ran=%v）", err, ran)
	}
	raw, err = pg.GetSchedState(SchedKeyStrategyCurator)
	if err != nil {
		t.Fatalf("读水位: %v", err)
	}
	if raw == "" {
		t.Fatalf("跑完之后水位没写")
	}
	if want := strconv.FormatInt(now.Unix(), 10); raw != want {
		t.Fatalf("水位 = %q，期望 %q", raw, want)
	}
	due, err = ShouldRunCurator(pg, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("判水位: %v", err)
	}
	if due {
		t.Fatalf("刚跑完就又该跑 —— curator 不是每个 tick 跑一次")
	}
	due, err = ShouldRunCurator(pg, now.Add(CuratorInterval+time.Minute))
	if err != nil {
		t.Fatalf("判水位: %v", err)
	}
	if !due {
		t.Fatalf("过了运行间隔后应当该跑")
	}
}

// TestManualPinReleaseRequiresActorAndReasonAndIsAudited 钉住人工解除 pin 必须留痕。
//
// 人工解除是能断掉漏洞报告可复现性的动作 ⇒ 必须有操作人与理由，且进审计哈希链。
func TestManualPinReleaseRequiresActorAndReasonAndIsAudited(t *testing.T) {
	_, pg := taskHallTestServer(t)
	sfx := curatorTestSuffix() % 100000

	id := curatorStrategy(t, pg, fmt.Sprintf("人工解 pin-%d", sfx), curatorAssetType(sfx, "manual"), "m1")
	if ok, err := pg.ApplyStrategyPin(id, true); err != nil || !ok {
		t.Fatalf("置 pin: %v", err)
	}

	if err := NewCurator(pg).ReleasePin("", "有替代路径了", id); err == nil {
		t.Fatalf("没有操作人的解除被放行了")
	}
	if err := NewCurator(pg).ReleasePin("operator", "   ", id); err == nil {
		t.Fatalf("没有理由的解除被放行了")
	}
	if !curatorState(t, pg, id).Pinned {
		t.Fatalf("非法的人工解除不该动 pin")
	}

	if err := NewCurator(pg).ReleasePin("operator", "已确认存在替代路径", id); err != nil {
		t.Fatalf("人工解除: %v", err)
	}
	if curatorState(t, pg, id).Pinned {
		t.Fatalf("人工解除后 pin 仍在")
	}

	var actor, reason, action string
	if err := pg.QueryRow(`SELECT actor, reason, action FROM audit_events
		WHERE action='strategy_pin_released' ORDER BY seq DESC LIMIT 1`).
		Scan(&actor, &reason, &action); err != nil {
		t.Fatalf("读审计事件: %v", err)
	}
	if actor != "operator" {
		t.Fatalf("审计事件缺操作人：%q", actor)
	}
	if strings.TrimSpace(reason) == "" {
		t.Fatalf("审计事件缺理由")
	}
	if seq, err := pg.VerifyAuditChain(); err != nil || seq != 0 {
		t.Fatalf("审计链断了（seq=%d, err=%v）", seq, err)
	}
}

// TestCuratorRunOnceIsIdempotent 钉住 RunOnce 幂等。
//
// 重复跑不产生额外副作用：已晋级的不会重复晋级、已退役的不会重复退役、
// 已置位的 pin 不会被反复改写。
func TestCuratorRunOnceIsIdempotent(t *testing.T) {
	_, pg := taskHallTestServer(t)
	sfx := curatorTestSuffix() % 100000
	curator := NewCurator(pg)

	at := curatorAssetType(sfx, "idem")
	id := curatorStrategy(t, pg, fmt.Sprintf("幂等-%d", sfx), at, "m1")
	curatorFreezeCorpus(t, pg, int64(700060)+sfx, at, []string{"m1"})
	curatorAdopt(t, pg, id, db.StrategyMinObservations, "m1")
	f := curatorFinding(t, pg, db.FindingFalsePositive)
	if err := pg.LinkStrategyEvidence(id, "finding", strconv.FormatInt(f, 10), "validated_by", "sha256:i"); err != nil {
		t.Fatalf("挂证据: %v", err)
	}
	curatorSetColumn(t, pg, id, "confidence", 0.0)

	rep1, err := curator.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("跑 curator: %v", err)
	}
	rep2, err := curator.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("再跑 curator: %v", err)
	}
	if len(rep1.Promoted) != 1 {
		t.Fatalf("第一轮应当晋级一条：%+v", rep1)
	}
	if len(rep2.Promoted) != 0 {
		t.Fatalf("第二轮又晋级了（不幂等）：%+v", rep2)
	}
	if len(rep2.Retired) != 0 {
		t.Fatalf("第二轮又退役了（不幂等）：%+v", rep2)
	}
	if len(rep1.Retired) != 1 {
		t.Fatalf("第一轮应当退役一条：%+v", rep1)
	}
	s := curatorState(t, pg, id)
	if s.State != db.StrategyRetired {
		t.Fatalf("状态应为 %s，实际 %s", db.StrategyRetired, s.State)
	}
}

// TestCuratorSourceHasNoPersonalPaths 钉住仓库里不出现个人路径。
//
// 与 pre-push 钩子同一条约束：产出物里不得带任何个人目录痕迹。
//
// ⚠️ 禁用清单必须**拼接构造**，不能直接写成字面量：这个用例扫描的文件里
// 包含它自己，清单里若直接写着那个路径，扫描器必然命中自己 —— 那是自伤，
// 不是被扫描物有问题。
func TestCuratorSourceHasNoPersonalPaths(t *testing.T) {
	forbidden := []string{"/" + "home/", "Desktop" + "/ax", "Desktop" + "/dig"}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("读目录: %v", err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") {
			continue
		}
		if !strings.Contains(name, "strategy_curator") {
			continue
		}
		raw, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("读 %s: %v", name, err)
		}
		for _, bad := range forbidden {
			if strings.Contains(string(raw), bad) {
				t.Fatalf("%s 里出现了个人路径 %q", name, bad)
			}
		}
	}
}

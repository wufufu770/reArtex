// 本文件是策略库（L4）Task 5 / Task 6 的**端到端**验收用例。
//
// 与 server/strategy_curator_test.go 的分工：
//   - 那份文件钉的是【零件】：三个判定函数各自的返回值、迁移原语的守卫、
//     单条策略的阈值边界；
//   - 本文件钉的是【装配后的行为】：一整轮 RunOnce 之后，库里那条策略**真的**
//     变成了 active / retired / pinned 吗？没被 pin 的策略会不会被误退役？
//     退役之后证据链还在吗？复审队列会不会偷偷放行 pin？
//
// 为什么要端到端地再钉一遍（而不是只信零件测试）：
//   零件测试能通过、而装配是错的，是这类裁决写最典型的失灵方式 ——
//   「判定说该晋级」与「真的晋级了」之间隔着一次条件更新、一个状态守卫、
//   一份物化视图的刷新时序。任一处接错，零件全绿而行为是错的。
//
// 【口径来源】本文件的断言以计划为准（.omo/plans/
// 2026-10-29-task-hall-phase6-implementation.md 的 Task 5 / Task 6 与
// 「已拍板：5 个决策点」），不是以实现为准。实现与计划不一致的地方，
// 用例按计划写，并在报告里点名。
//
// 【为什么一个用例函数只跑几轮仲裁】每一轮 RunOnce 都要刷一次 utility 物化
// 视图并扫一遍全库，而 server 包的整套件本来就贴着 go test 的 10 分钟上限
// （基线 464s）。⇒ 同一组行为共用一轮仲裁：既省时，又让「几条策略在同一轮里
// 各自被裁决」这件事本身成为断言对象（这正是真实的仲裁场景）。
//
// 【测试隔离】每个用例造的东西都在用例结束时删掉（e2eCleanup）：
//   - 策略行：留下会让后面用例的 RunOnce 扫到别人的隔离态策略；
//   - 冻结语料：strategy_replay_corpus 是全库共享的，ReplayScore 读的是
//     **所有**案例 ⇒ 留着的语料会改变别的用例的「可比 / 不可比」判定；
//   - 漏洞行与流量快照：它们进 utility 物化视图，留着会污染淘汰判据。
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
)

// ---------- 本文件私有的夹具（与 curator 单元的私有助手互不依赖） ----------

// e2eSuffix 给用例生成互不撞的标识后缀（纳秒级）。
//
// 为什么不用 time.Now() 直接取：同一纳秒内造两条同名策略会撞上
// uq_strategies_company_name 唯一索引 ⇒ 用例之间互相踩。
func e2eSuffix() int64 { return time.Now().UnixNano() }

// e2eAssetType 造一个独一无二的资产类型标记。
//
// 为什么资产类型必须独一无二：冻结语料的案例按资产签名相交的**集合**算，
// 两条策略共用一个资产类型就会共用同一批可比案例 ⇒ 各自的闸门结果互相污染。
func e2eAssetType(sfx int64, tag string) string {
	return fmt.Sprintf("e2e-%s-%d", tag, sfx%1000000)
}

// e2eStrategy 建一条可控的策略：activation 的资产类型与里程碑 id 由用例指定。
func e2eStrategy(t *testing.T, pg *db.DB, name, assetType, milestoneID string) int64 {
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

// e2eFreezeCorpus 冻结一条重放语料（可比案例）。
func e2eFreezeCorpus(t *testing.T, pg *db.DB, taskID int64, assetType string, milestones []string) {
	t.Helper()
	sig := db.AssetSignature{AssetTypes: []string{assetType}}
	if err := pg.FreezeReplayCorpus(taskID, sig, milestones, []string{db.FindingConfirmed}); err != nil {
		t.Fatalf("冻结重放语料: %v", err)
	}
}

// e2eAdopt 造 n 次「被实际使用且里程碑命中」的使用明细。
//
// 走**真实写入路径**（RecordStrategyUsage + ApplyStrategyVotes）⇒ usage_count
// 真的涨 n。这正是「最小观测前置按 usage_count 计」的口径来源：数的是真正
// 发生过的事（被采纳并执行），不是「被检索到」。
func e2eAdopt(t *testing.T, pg *db.DB, strategyID int64, n int, milestoneID string) {
	t.Helper()
	base := e2eSuffix() % 100000
	for i := 0; i < n; i++ {
		taskID := int64(910000) + base*100 + int64(i)
		intentID := int64(920000) + base*100 + int64(i)
		usage := db.StrategyUsage{
			StrategyID:     strategyID,
			TaskID:         taskID,
			ExplorationID:  1,
			IntentID:       intentID,
			Outcome:        db.StrategyUsageHit,
			Classification: db.StrategyAttributionSkillGuided,
			MilestoneHits:  []string{milestoneID},
		}
		if err := pg.RecordStrategyUsage(usage); err != nil {
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

// e2eFinding 建一条指定状态的 finding。
func e2eFinding(t *testing.T, pg *db.DB, status string) int64 {
	t.Helper()
	id, err := pg.AddFinding(0, 0, "sqli", "端到端测试漏洞", "high", "摘要", "证据文本", "worker", nil)
	if err != nil {
		t.Fatalf("建 finding: %v", err)
	}
	if _, err := pg.Exec(`UPDATE findings SET status=$2 WHERE id=$1`, id, status); err != nil {
		t.Fatalf("设 finding 状态: %v", err)
	}
	return id
}

// e2eSnapshot 写一条流量证据快照（路径的身份是 url，不是快照 id）。
func e2eSnapshot(t *testing.T, pg *db.DB, id, url string) {
	t.Helper()
	if _, err := pg.Exec(`INSERT INTO traffic_evidence_snapshots
		(id, source_traffic_id, captured_at, url, method, status, content_type,
		 req_head, resp_head, req_hash, resp_hash, req_len, resp_len)
		VALUES($1,$1,0,$2,'GET',200,'','h','h','h','h',0,0)
		ON CONFLICT (id) DO NOTHING`, id, url); err != nil {
		t.Fatalf("写流量快照: %v", err)
	}
}

// e2eBindSnapshot 把流量快照绑到 finding 上。
func e2eBindSnapshot(t *testing.T, pg *db.DB, findingID int64, snapshotID string) {
	t.Helper()
	if _, err := pg.Exec(`INSERT INTO finding_traffic_bindings(finding_id, snapshot_id, role, position)
		VALUES($1,$2,'supporting',1) ON CONFLICT (finding_id, snapshot_id) DO NOTHING`,
		findingID, snapshotID); err != nil {
		t.Fatalf("绑流量快照: %v", err)
	}
}

// e2eSetColumn 直接改 strategies 的某个计数/分数列（构造场景用）。
//
// 为什么允许直接改：这些列的**写入路径**由归因器独占，而本文件要构造的是
// 「置信度被踩到负值」这类边界场景 —— 走写入路径去构造要绕很大一圈。
// 用例验证的是**判定逻辑**读的是哪一列。
func e2eSetColumn(t *testing.T, pg *db.DB, strategyID int64, column string, value any) {
	t.Helper()
	switch column {
	case "usage_count", "retrieval_count", "hit_count", "miss_count", "confidence", "version":
		if _, err := pg.Exec(fmt.Sprintf(`UPDATE strategies SET %s=$2 WHERE id=$1`, column),
			strategyID, value); err != nil {
			t.Fatalf("改 %s: %v", column, err)
		}
	default:
		t.Fatalf("用例不允许改列 %q", column)
	}
}

// e2eAge 把策略的时间戳推到很久以前（构造「很老的策略」这一场景）。
func e2eAge(t *testing.T, pg *db.DB, strategyID int64, days int) {
	t.Helper()
	if _, err := pg.Exec(`UPDATE strategies SET created_at=now()-$2::int * interval '1 day',
		updated_at=now()-$2::int * interval '1 day' WHERE id=$1`, strategyID, days); err != nil {
		t.Fatalf("改时间戳: %v", err)
	}
}

// e2eState 读策略的当前状态。
func e2eState(t *testing.T, pg *db.DB, strategyID int64) *db.Strategy {
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

// e2eCleanup 删掉本用例造出来的一切东西。
//
// 为什么必须删干净：
//   - strategies 有外键级联（证据链 / 使用明细 / 提议 / 快照），删它等于连带清干净；
//   - strategy_replay_corpus **没有**外键 ⇒ 必须显式删，否则 ReplayScore
//     会在后面的用例里看到本用例的案例；
//   - findings / traffic_evidence_snapshots 进 utility 物化视图 ⇒ 必须删。
func e2eCleanup(t *testing.T, pg *db.DB, strategyIDs, findingIDs, corpusTaskIDs []int64, snapshotIDs []string) {
	t.Helper()
	t.Cleanup(func() {
		if len(strategyIDs) > 0 {
			_, _ = pg.Exec(`DELETE FROM strategies WHERE id = ANY($1::bigint[])`, strategyIDs)
		}
		if len(corpusTaskIDs) > 0 {
			_, _ = pg.Exec(`DELETE FROM strategy_replay_corpus WHERE task_id = ANY($1::bigint[])`, corpusTaskIDs)
		}
		if len(findingIDs) > 0 {
			_, _ = pg.Exec(`DELETE FROM finding_traffic_bindings WHERE finding_id = ANY($1::bigint[])`, findingIDs)
		}
		if len(snapshotIDs) > 0 {
			_, _ = pg.Exec(`DELETE FROM traffic_evidence_snapshots WHERE id = ANY($1::text[])`, snapshotIDs)
		}
		if len(findingIDs) > 0 {
			_, _ = pg.Exec(`DELETE FROM findings WHERE id = ANY($1::bigint[])`, findingIDs)
		}
	})
}

// e2eRunCurator 跑一轮仲裁（宿主侧的裁决写入口）。
func e2eRunCurator(t *testing.T, pg *db.DB) CuratorReport {
	t.Helper()
	rep, err := NewCurator(pg).RunOnce(context.Background())
	if err != nil {
		t.Fatalf("跑 curator: %v", err)
	}
	return rep
}

// e2eContainsID 报告一组 id 里有没有目标 id。
func e2eContainsID(ids []int64, want int64) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

// =====================================================================
// 行为 1 + 2：晋级的三个条件是「与」关系；没有可比语料 ⇒ 不晋级
// =====================================================================

// TestCuratorE2EPromotionEndToEnd 钉住晋级的三条件（行为 1）与保守默认（行为 2）。
//
// 六条隔离态策略在同一轮仲裁里被裁决（计划 Task 5 Step 1 的 A/B/C/D 四例 + 无语料例）：
//
//	A) 重放通过 + usage_count>=n + 里程碑命中 → **真的变成 active**；
//	B) 重放通过 + 里程碑命中，但 usage_count<n   → 仍是 quarantined；
//	C) 重放不通过（候选在语料上没命中）           → 仍是 quarantined；
//	D) 重放通过 + usage_count 够，但没里程碑命中   → 仍是 quarantined；
//	E) 观测次数够、里程碑命中，但**没有可比语料** → 仍是 quarantined。
//
// 断言的是**库里那一行的 state**（不是判定函数的返回值）—— 零件测试钉的是
// 「判定说该晋级」，这里钉的是「晋级真的发生了，且只发生在该发生的那一条上」。
func TestCuratorE2EPromotionEndToEnd(t *testing.T) {
	_, pg := taskHallTestServer(t)
	sfx := e2eSuffix()

	typeA := e2eAssetType(sfx, "a")
	typeB := e2eAssetType(sfx, "b")
	typeC := e2eAssetType(sfx, "c")
	typeD := e2eAssetType(sfx, "d")
	typeE := e2eAssetType(sfx, "e")
	otherType := e2eAssetType(sfx, "other")

	var strategies, corpusTasks []int64

	// A) 三条件全过。
	aID := e2eStrategy(t, pg, fmt.Sprintf("e2e晋级A-%d", sfx), typeA, "m1")
	strategies = append(strategies, aID)
	corpusTasks = append(corpusTasks, int64(910101)+sfx%1000)
	e2eFreezeCorpus(t, pg, corpusTasks[len(corpusTasks)-1], typeA, []string{"m1"})
	e2eAdopt(t, pg, aID, db.StrategyMinObservations, "m1")

	// B) 重放通过 + 里程碑命中，但观测次数不够。
	bID := e2eStrategy(t, pg, fmt.Sprintf("e2e晋级B-%d", sfx), typeB, "m1")
	strategies = append(strategies, bID)
	corpusTasks = append(corpusTasks, int64(910102)+sfx%1000)
	e2eFreezeCorpus(t, pg, corpusTasks[len(corpusTasks)-1], typeB, []string{"m1"})
	e2eAdopt(t, pg, bID, db.StrategyMinObservations-1, "m1")

	// C) 重放不通过：语料命中的是别的里程碑。
	cID := e2eStrategy(t, pg, fmt.Sprintf("e2e晋级C-%d", sfx), typeC, "m2")
	strategies = append(strategies, cID)
	corpusTasks = append(corpusTasks, int64(910103)+sfx%1000)
	e2eFreezeCorpus(t, pg, corpusTasks[len(corpusTasks)-1], typeC, []string{"m1"})
	e2eAdopt(t, pg, cID, db.StrategyMinObservations, "m2")

	// D) 重放通过 + 观测次数够，但真实运行里没命中过它的里程碑。
	dID := e2eStrategy(t, pg, fmt.Sprintf("e2e晋级D-%d", sfx), typeD, "m1")
	strategies = append(strategies, dID)
	corpusTasks = append(corpusTasks, int64(910104)+sfx%1000)
	e2eFreezeCorpus(t, pg, corpusTasks[len(corpusTasks)-1], typeD, []string{"m1"})
	e2eAdopt(t, pg, dID, db.StrategyMinObservations, "m-other")

	// E) 观测次数够、里程碑命中，但库里只有**另一种**资产类型的语料 ⇒ 没有可比案例。
	corpusTasks = append(corpusTasks, int64(910105)+sfx%1000)
	e2eFreezeCorpus(t, pg, corpusTasks[len(corpusTasks)-1], otherType, []string{"m1"})
	eID := e2eStrategy(t, pg, fmt.Sprintf("e2e晋级E-%d", sfx), typeE, "m1")
	strategies = append(strategies, eID)
	e2eAdopt(t, pg, eID, db.StrategyMinObservations, "m1")

	e2eCleanup(t, pg, strategies, nil, corpusTasks, nil)

	rep := e2eRunCurator(t, pg)

	cases := []struct {
		name string
		id   int64
		want string
	}{
		{"A 三条件全过", aID, db.StrategyActive},
		{"B 观测次数不足", bID, db.StrategyQuarantined},
		{"C 重放不通过", cID, db.StrategyQuarantined},
		{"D 没里程碑命中", dID, db.StrategyQuarantined},
		{"E 没有可比语料", eID, db.StrategyQuarantined},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := e2eState(t, pg, c.id).State
			if got != c.want {
				t.Fatalf("库里状态 = %s, 期望 %s", got, c.want)
			}
			inReport := e2eContainsID(rep.Promoted, c.id)
			if c.want == db.StrategyActive && !inReport {
				t.Fatalf("晋级了却没出现在裁决报告的 Promoted 里：%+v", rep)
			}
			if c.want != db.StrategyActive && inReport {
				t.Fatalf("没晋级却出现在裁决报告的 Promoted 里：%+v", rep)
			}
		})
	}
}

// =====================================================================
// 行为 3 + 4 + 5：淘汰的两条腿（阈值取自常量）、最小观测前置、退役 ≠ 删除
// =====================================================================

// TestCuratorE2ERetirementEndToEnd 钉住淘汰判据与退役语义。
//
// 五条在役策略在同一轮仲裁里被裁决：
//
//	① utility 腿：证据里只有 1 条 confirmed ⇒ utility = 1/n < τ_u ⇒ 退役；
//	② confidence 腿：utility = 1.0（utility 腿不成立），confidence 严格低于
//	   常量 ⇒ 退役；
//	③ 两条腿都不成立：utility = 1.0 且 confidence = **恰好等于**常量 ⇒ 不退役；
//	④ 观测次数不足（usage_count < n）且 utility = 0 ⇒ 不退役；
//	⑤ 退役的那一条必须留下 retired_reason 与**整条证据链**。
//
// 为什么阈值必须读常量：常量是「单一真相源」（计划「已拍板」第 2 条）。
// 若测试里写死 0.05 / -0.3，将来有人把常量调了，测试仍然绿 —— 门禁就失效了。
// 本用例的**输入**全部由常量推导：
//
//	utility 腿：n 由 τ_u 推导 ⇒ 1/n **严格小于** τ_u（τ_u 取任何值都成立）；
//	confidence 腿：confidence = 常量 - 0.1 ⇒ 严格小于常量；
//	都不成立：  confidence = 恰好等于常量（判据是严格小于 ⇒ 不成立）。
//
// 「恰好等于常量」这一条是关键：它把「阈值就是常量」这件事钉死 ——
// 若实现用了别的阈值，这一例会翻。
func TestCuratorE2ERetirementEndToEnd(t *testing.T) {
	_, pg := taskHallTestServer(t)
	sfx := e2eSuffix()

	// 让 utility = 1/n 严格小于 τ_u：n 由常量推导 ⇒ 常量变了用例依然自洽。
	n := int(1/db.StrategyTauU) + 2
	if 1.0/float64(n) >= db.StrategyTauU {
		t.Fatalf("用例前提不对：1/%d=%v 不小于 τ_u=%v", n, 1.0/float64(n), db.StrategyTauU)
	}

	var strategies, corpusTasks, findingIDs []int64

	// —— ① utility 腿：证据里只有 1 条 confirmed ⇒ utility = 1/n < τ_u。——
	atU := e2eAssetType(sfx, "utilleg")
	uID := e2eStrategy(t, pg, fmt.Sprintf("e2e淘汰utility腿-%d", sfx), atU, "m1")
	strategies = append(strategies, uID)
	corpusTasks = append(corpusTasks, int64(910120)+sfx%1000)
	e2eFreezeCorpus(t, pg, corpusTasks[len(corpusTasks)-1], atU, []string{"m1"})
	e2eAdopt(t, pg, uID, db.StrategyMinObservations, "m1")
	confirmed := e2eFinding(t, pg, db.FindingConfirmed)
	findingIDs = append(findingIDs, confirmed)
	if err := pg.LinkStrategyEvidence(uID, "finding", strconv.FormatInt(confirmed, 10), "created_by", "sha256:u"); err != nil {
		t.Fatalf("挂证据: %v", err)
	}
	for i := 1; i < n; i++ {
		f := e2eFinding(t, pg, db.FindingFalsePositive)
		findingIDs = append(findingIDs, f)
		if err := pg.LinkStrategyEvidence(uID, "finding", strconv.FormatInt(f, 10), "created_by", "sha256:u"); err != nil {
			t.Fatalf("挂证据: %v", err)
		}
	}

	// —— ② confidence 腿：utility = 1.0，confidence 严格低于常量。——
	atC := e2eAssetType(sfx, "confleg")
	cID := e2eStrategy(t, pg, fmt.Sprintf("e2e淘汰confidence腿-%d", sfx), atC, "m1")
	strategies = append(strategies, cID)
	corpusTasks = append(corpusTasks, int64(910121)+sfx%1000)
	e2eFreezeCorpus(t, pg, corpusTasks[len(corpusTasks)-1], atC, []string{"m1"})
	e2eAdopt(t, pg, cID, db.StrategyMinObservations, "m1")
	cf := e2eFinding(t, pg, db.FindingConfirmed)
	findingIDs = append(findingIDs, cf)
	if err := pg.LinkStrategyEvidence(cID, "finding", strconv.FormatInt(cf, 10), "created_by", "sha256:c"); err != nil {
		t.Fatalf("挂证据: %v", err)
	}
	e2eSetColumn(t, pg, cID, "confidence", db.StrategyConfidenceRetireThreshold-0.1)

	// —— ③ 两条腿都不成立：utility = 1.0，confidence 恰好等于常量。——
	atN := e2eAssetType(sfx, "nolag")
	nID := e2eStrategy(t, pg, fmt.Sprintf("e2e不淘汰-%d", sfx), atN, "m1")
	strategies = append(strategies, nID)
	corpusTasks = append(corpusTasks, int64(910122)+sfx%1000)
	e2eFreezeCorpus(t, pg, corpusTasks[len(corpusTasks)-1], atN, []string{"m1"})
	e2eAdopt(t, pg, nID, db.StrategyMinObservations, "m1")
	nf := e2eFinding(t, pg, db.FindingConfirmed)
	findingIDs = append(findingIDs, nf)
	if err := pg.LinkStrategyEvidence(nID, "finding", strconv.FormatInt(nf, 10), "created_by", "sha256:n"); err != nil {
		t.Fatalf("挂证据: %v", err)
	}
	e2eSetColumn(t, pg, nID, "confidence", db.StrategyConfidenceRetireThreshold)

	// —— ④ 观测次数不足：只被用过 n-1 次，utility = 0，confidence 压到极负。——
	atM := e2eAssetType(sfx, "minobs")
	mID := e2eStrategy(t, pg, fmt.Sprintf("e2e观测不足-%d", sfx), atM, "m1")
	strategies = append(strategies, mID)
	corpusTasks = append(corpusTasks, int64(910123)+sfx%1000)
	e2eFreezeCorpus(t, pg, corpusTasks[len(corpusTasks)-1], atM, []string{"m1"})
	e2eAdopt(t, pg, mID, db.StrategyMinObservations-1, "m1")
	mf := e2eFinding(t, pg, db.FindingFalsePositive)
	findingIDs = append(findingIDs, mf)
	if err := pg.LinkStrategyEvidence(mID, "finding", strconv.FormatInt(mf, 10), "created_by", "sha256:m"); err != nil {
		t.Fatalf("挂证据: %v", err)
	}
	e2eSetColumn(t, pg, mID, "confidence", -1.0)
	// 检索次数拉满，但**从未被采纳**：usage_count 仍是 n-1。
	e2eSetColumn(t, pg, mID, "retrieval_count", 500)

	// —— ⑤ 退役留痕：与 ① 同型，但单独一条策略用来断言证据链。——
	atK := e2eAssetType(sfx, "keepev")
	kID := e2eStrategy(t, pg, fmt.Sprintf("e2e退役留痕-%d", sfx), atK, "m1")
	strategies = append(strategies, kID)
	corpusTasks = append(corpusTasks, int64(910124)+sfx%1000)
	e2eFreezeCorpus(t, pg, corpusTasks[len(corpusTasks)-1], atK, []string{"m1"})
	e2eAdopt(t, pg, kID, db.StrategyMinObservations, "m1")
	kf := e2eFinding(t, pg, db.FindingFalsePositive)
	findingIDs = append(findingIDs, kf)
	if err := pg.LinkStrategyEvidence(kID, "finding", strconv.FormatInt(kf, 10), "validated_by", "sha256:k"); err != nil {
		t.Fatalf("挂证据: %v", err)
	}
	e2eSetColumn(t, pg, kID, "confidence", 0.0)

	// 五条都先置为在役态（晋级是裁决写，这里直接构造「在役」这一前提）。
	for _, id := range []int64{uID, cID, nID, mID, kID} {
		if ok, err := pg.PromoteStrategyToActive(id); err != nil || !ok {
			t.Fatalf("置为 active（策略 %d）: %v", id, err)
		}
	}

	e2eCleanup(t, pg, strategies, findingIDs, corpusTasks, nil)

	// 先看判定明细：阈值必须就是常量。
	if err := pg.RefreshStrategyUtility(); err != nil {
		t.Fatalf("刷新 utility: %v", err)
	}
	for _, id := range []int64{uID, cID, nID, mID, kID} {
		dec, err := EvaluateRetirement(pg, *e2eState(t, pg, id))
		if err != nil {
			t.Fatalf("淘汰判定（策略 %d）: %v", id, err)
		}
		if dec.TauU != db.StrategyTauU {
			t.Fatalf("判定明细里的 τ_u = %v，不是常量 %v", dec.TauU, db.StrategyTauU)
		}
		if dec.ConfidenceThreshold != db.StrategyConfidenceRetireThreshold {
			t.Fatalf("判定明细里的 confidence 阈值 = %v，不是常量 %v",
				dec.ConfidenceThreshold, db.StrategyConfidenceRetireThreshold)
		}
	}

	rep := e2eRunCurator(t, pg)

	t.Run("utility 腿退役", func(t *testing.T) {
		if got := e2eState(t, pg, uID).State; got != db.StrategyRetired {
			t.Fatalf("utility 低于 τ_u 却没有退役：state = %s（期望 %s）", got, db.StrategyRetired)
		}
		if !e2eContainsID(rep.Retired, uID) {
			t.Fatalf("utility 腿退役的策略没出现在裁决报告的 Retired 里：%+v", rep)
		}
	})

	t.Run("confidence 腿退役", func(t *testing.T) {
		if got := e2eState(t, pg, cID).State; got != db.StrategyRetired {
			t.Fatalf("confidence 低于阈值却没有退役：state = %s（期望 %s）", got, db.StrategyRetired)
		}
		if !e2eContainsID(rep.Retired, cID) {
			t.Fatalf("confidence 腿退役的策略没出现在裁决报告的 Retired 里：%+v", rep)
		}
	})

	t.Run("两条腿都不成立不退役", func(t *testing.T) {
		if got := e2eState(t, pg, nID).State; got != db.StrategyActive {
			t.Fatalf("两条腿都不成立却被退役了：state = %s（期望 %s）", got, db.StrategyActive)
		}
		if e2eContainsID(rep.Retired, nID) {
			t.Fatalf("两条腿都不成立的策略出现在 Retired 里：%+v", rep)
		}
	})

	t.Run("观测次数不足不退役", func(t *testing.T) {
		if got := e2eState(t, pg, mID).State; got != db.StrategyActive {
			t.Fatalf("观测次数不足却退役了：state = %s（期望 %s）", got, db.StrategyActive)
		}
		if e2eContainsID(rep.Retired, mID) {
			t.Fatalf("观测次数不足的策略出现在 Retired 里：%+v", rep)
		}
		// 口径核对：前置门读的是 usage_count，不是 retrieval_count。
		s := e2eState(t, pg, mID)
		if s.UsageCount >= db.StrategyMinObservations {
			t.Fatalf("用例前提不对：usage_count = %d（应 < %d）", s.UsageCount, db.StrategyMinObservations)
		}
		if s.RetrievalCount < 500 {
			t.Fatalf("用例前提不对：retrieval_count = %d（应 ≥ 500）", s.RetrievalCount)
		}
	})

	t.Run("退役保留理由与证据链", func(t *testing.T) {
		s := e2eState(t, pg, kID)
		if s.State != db.StrategyRetired {
			t.Fatalf("状态应为 %s，实际 %s", db.StrategyRetired, s.State)
		}
		if strings.TrimSpace(s.RetiredReason) == "" {
			t.Fatalf("退役必须留下 retired_reason")
		}
		if s.RetiredAt == nil {
			t.Fatalf("退役必须留下 retired_at")
		}
		// 策略行本身还在（是「退役」不是「删除」）。
		var rowCount int
		if err := pg.QueryRow(`SELECT count(*) FROM strategies WHERE id=$1`, kID).Scan(&rowCount); err != nil {
			t.Fatalf("数策略行: %v", err)
		}
		if rowCount != 1 {
			t.Fatalf("策略行不见了 —— 退役被实现成了删除")
		}
		// 证据链一条不少。
		links, err := pg.StrategyEvidence(kID)
		if err != nil {
			t.Fatalf("读证据链: %v", err)
		}
		if len(links) == 0 {
			t.Fatalf("退役后证据链被清空了 —— 退役 ≠ 删除")
		}
		found := false
		for _, l := range links {
			if l.RefType == "finding" && l.RefID == strconv.FormatInt(kf, 10) {
				found = true
			}
		}
		if !found {
			t.Fatalf("退役后找不到那条漏洞证据：%+v", links)
		}
		// 漏洞行也还在（证据链指向的是活着的实体）。
		var findingAlive int
		if err := pg.QueryRow(`SELECT count(*) FROM findings WHERE id=$1`, kf).Scan(&findingAlive); err != nil {
			t.Fatalf("数漏洞行: %v", err)
		}
		if findingAlive != 1 {
			t.Fatalf("证据指向的漏洞行不见了")
		}
	})
}

// =====================================================================
// 行为 6 + 7：唯一证人 pin 的机械性、无模型无年龄、复审队列只提示
// =====================================================================

// TestCuratorE2EPinEndToEnd 钉住 pin 的完整生命周期与复审队列的 advisory 性质。
//
// 三条「唯一证人」策略 + 超过阈值的复审队列策略，在两轮仲裁里被裁决：
//
//	第一轮：三条唯一证人策略被**置位**（其中两条已经「很老」）；
//	第二轮：
//	  - N1 出现了指向不同 url 的新流量证据 ⇒ 解 pin（条件①）；
//	  - N2 的漏洞不再是 confirmed             ⇒ 解 pin（条件②）；
//	  - N3 什么都没变（而且它很老）           ⇒ **pin 保持**（年龄不构成解除理由）；
//	  - 复审队列里的 51 条一条都没被自动解除   ⇒ 队列只是提示。
//
// N1 与 N3 的对照是关键：两者都「很老」，唯一区别是 N1 出现了替代路径 ⇒
// 解 pin 的原因是「出现了替代路径」，不是「它老了」。
func TestCuratorE2EPinEndToEnd(t *testing.T) {
	_, pg := taskHallTestServer(t)
	sfx := e2eSuffix()

	var strategies, findingIDs []int64
	var snapshotIDs []string

	// —— N1：唯一证人 + 有流量快照证据；很老；第二轮出现替代路径。——
	at1 := e2eAssetType(sfx, "pin1")
	n1 := e2eStrategy(t, pg, fmt.Sprintf("e2e唯一证人替代路径-%d", sfx), at1, "m1")
	strategies = append(strategies, n1)
	f1 := e2eFinding(t, pg, db.FindingConfirmed)
	findingIDs = append(findingIDs, f1)
	snap1 := fmt.Sprintf("e2e-snap-%d-1", sfx)
	snapshotIDs = append(snapshotIDs, snap1)
	e2eSnapshot(t, pg, snap1, "http://e2e.invalid/"+at1)
	e2eBindSnapshot(t, pg, f1, snap1)
	if err := pg.LinkStrategyEvidence(n1, "finding", strconv.FormatInt(f1, 10), "validated_by", "sha256:f1"); err != nil {
		t.Fatalf("挂 finding 证据: %v", err)
	}
	if err := pg.LinkStrategyEvidence(n1, "traffic_snapshot", snap1, "validated_by", "sha256:s1"); err != nil {
		t.Fatalf("挂流量证据: %v", err)
	}
	e2eAge(t, pg, n1, 900)

	// —— N2：唯一证人；很老；第二轮漏洞状态变成 resolved。——
	at2 := e2eAssetType(sfx, "pin2")
	n2 := e2eStrategy(t, pg, fmt.Sprintf("e2e唯一证人状态变-%d", sfx), at2, "m1")
	strategies = append(strategies, n2)
	f2 := e2eFinding(t, pg, db.FindingConfirmed)
	findingIDs = append(findingIDs, f2)
	snap2 := fmt.Sprintf("e2e-snap-%d-2", sfx)
	snapshotIDs = append(snapshotIDs, snap2)
	e2eSnapshot(t, pg, snap2, "http://e2e.invalid/"+at2)
	e2eBindSnapshot(t, pg, f2, snap2)
	if err := pg.LinkStrategyEvidence(n2, "finding", strconv.FormatInt(f2, 10), "validated_by", "sha256:f2"); err != nil {
		t.Fatalf("挂 finding 证据: %v", err)
	}
	if err := pg.LinkStrategyEvidence(n2, "traffic_snapshot", snap2, "validated_by", "sha256:s2"); err != nil {
		t.Fatalf("挂流量证据: %v", err)
	}
	e2eAge(t, pg, n2, 900)

	// —— N3：唯一证人；很老；第二轮什么都不变 ⇒ pin 必须保持。——
	at3 := e2eAssetType(sfx, "pin3")
	n3 := e2eStrategy(t, pg, fmt.Sprintf("e2e唯一证人保持-%d", sfx), at3, "m1")
	strategies = append(strategies, n3)
	f3 := e2eFinding(t, pg, db.FindingConfirmed)
	findingIDs = append(findingIDs, f3)
	snap3 := fmt.Sprintf("e2e-snap-%d-3", sfx)
	snapshotIDs = append(snapshotIDs, snap3)
	e2eSnapshot(t, pg, snap3, "http://e2e.invalid/"+at3)
	e2eBindSnapshot(t, pg, f3, snap3)
	if err := pg.LinkStrategyEvidence(n3, "finding", strconv.FormatInt(f3, 10), "validated_by", "sha256:f3"); err != nil {
		t.Fatalf("挂 finding 证据: %v", err)
	}
	if err := pg.LinkStrategyEvidence(n3, "traffic_snapshot", snap3, "validated_by", "sha256:s3"); err != nil {
		t.Fatalf("挂流量证据: %v", err)
	}
	e2eAge(t, pg, n3, 900)

	// —— 复审队列：超过阈值的 pinned 策略。——
	queueIDs := make([]int64, 0, db.StrategyPinQueueThreshold+1)
	for i := 0; i <= db.StrategyPinQueueThreshold; i++ {
		id := e2eStrategy(t, pg, fmt.Sprintf("e2e复审队列-%d-%d", sfx, i),
			e2eAssetType(sfx, fmt.Sprintf("q%d", i)), "m1")
		if ok, err := pg.ApplyStrategyPin(id, true); err != nil || !ok {
			t.Fatalf("置 pin: %v", err)
		}
		queueIDs = append(queueIDs, id)
	}
	strategies = append(strategies, queueIDs...)

	e2eCleanup(t, pg, strategies, findingIDs, nil, snapshotIDs)

	// 第一轮：三条唯一证人策略被置位；复审队列条目生成。
	rep1 := e2eRunCurator(t, pg)
	for _, id := range []int64{n1, n2, n3} {
		if !e2eState(t, pg, id).Pinned {
			t.Fatalf("唯一证人策略 %d 没有被置位", id)
		}
	}
	if len(rep1.PinQueue) == 0 {
		t.Fatalf("pinned 策略超过阈值（%d）却没有生成复审队列条目", db.StrategyPinQueueThreshold)
	}
	inQueue := 0
	for _, id := range queueIDs {
		if e2eContainsID(rep1.PinQueue, id) {
			inQueue++
		}
	}
	if inQueue != len(queueIDs) {
		t.Fatalf("复审队列只收了 %d / %d 条本用例的策略", inQueue, len(queueIDs))
	}

	// 第二轮：N1 出现替代路径；N2 的漏洞不再是 confirmed；N3 什么都不变。
	snapAlt := fmt.Sprintf("e2e-snap-%d-alt", sfx)
	snapshotIDs = append(snapshotIDs, snapAlt)
	e2eSnapshot(t, pg, snapAlt, "http://e2e.invalid/other-"+at1)
	e2eBindSnapshot(t, pg, f1, snapAlt)
	if _, err := pg.Exec(`UPDATE findings SET status=$2 WHERE id=$1`, f2, db.FindingResolved); err != nil {
		t.Fatalf("改 finding 状态: %v", err)
	}
	rep2 := e2eRunCurator(t, pg)

	t.Run("出现替代路径则解 pin", func(t *testing.T) {
		if e2eState(t, pg, n1).Pinned {
			t.Fatalf("出现指向不同 url 的新流量证据后 pin 没有解除")
		}
		if !e2eContainsID(rep2.Unpinned, n1) {
			t.Fatalf("解 pin 的策略没出现在裁决报告的 Unpinned 里：%+v", rep2)
		}
	})

	t.Run("漏洞不再 confirmed 则解 pin", func(t *testing.T) {
		if e2eState(t, pg, n2).Pinned {
			t.Fatalf("finding 不再 confirmed 后 pin 没有解除")
		}
	})

	t.Run("年龄不构成解 pin 理由", func(t *testing.T) {
		if !e2eState(t, pg, n3).Pinned {
			t.Fatalf("pin 被按年龄解除了 —— 策略的价值与年龄无关")
		}
	})

	t.Run("复审队列只提示不自动放行", func(t *testing.T) {
		for _, id := range queueIDs {
			if !e2eState(t, pg, id).Pinned {
				t.Fatalf("pin %d 被自动解除了 —— 队列只提示，不自动放行", id)
			}
		}
		for _, id := range rep2.Unpinned {
			for _, mine := range queueIDs {
				if mine == id {
					t.Fatalf("复审队列里的 pin %d 出现在自动解 pin 列表里", id)
				}
			}
		}
		// 「读队列」本身不改变任何状态。
		before, err := pg.CountPinnedStrategies()
		if err != nil {
			t.Fatalf("数被 pin 的策略: %v", err)
		}
		for i := 0; i < 3; i++ {
			if _, err := pg.PinnedStrategies(200); err != nil {
				t.Fatalf("读复审队列: %v", err)
			}
		}
		after, err := pg.CountPinnedStrategies()
		if err != nil {
			t.Fatalf("数被 pin 的策略: %v", err)
		}
		if before != after {
			t.Fatalf("读复审队列改变了 pin 的数量：%d → %d", before, after)
		}
		// 逐条再确认一次：读队列之后每一条 pin 都还在（纯读）。
		for _, id := range queueIDs {
			if !e2eState(t, pg, id).Pinned {
				t.Fatalf("读复审队列之后 pin %d 消失了 —— 读队列必须是纯读", id)
			}
		}
	})
}

// TestCuratorE2EPinDecisionHasNoModelAndNoAgeInput 钉住「解除判定不经过模型、
// 不按年龄」这条**结构性**性质。
//
// 断言两层：
//  1. 反射层：三个判定函数的入参只有 (库, 策略) —— 没有模型意见、没有年龄、
//     没有「是否已有替代路径」这类需要判断的入参；
//  2. 语义层：参数名里不得出现 model / llm / judge / opinion / age 等字样
//     （人读得懂的入参名是「这里本来该有个模型判断」的信号）。
//
// 为什么必须结构性：粗糙 judge（反模式 4）让同一批证据在不同轮次得到不同结论，
// 裁决不可复现 ⇒ 事后无法复盘、无法追责。
func TestCuratorE2EPinDecisionHasNoModelAndNoAgeInput(t *testing.T) {
	forbidden := []string{"model", "llm", "judge", "opinion", "age", "ttl", "expire"}

	decisions := []struct {
		name string
		fn   any
	}{
		{"EvaluatePin", EvaluatePin},
		{"EvaluatePromotion", EvaluatePromotion},
		{"EvaluateRetirement", EvaluateRetirement},
	}
	for _, d := range decisions {
		typ := reflect.TypeOf(d.fn)
		if typ.NumIn() != 2 {
			t.Fatalf("%s 的入参个数 = %d（期望 2：库 + 策略）", d.name, typ.NumIn())
		}
		wantDB := reflect.TypeOf((*db.DB)(nil))
		wantStrategy := reflect.TypeOf(db.Strategy{})
		if typ.In(0) != wantDB {
			t.Fatalf("%s 的第一个入参 = %v（期望 *db.DB）", d.name, typ.In(0))
		}
		if typ.In(1) != wantStrategy {
			t.Fatalf("%s 的第二个入参 = %v（期望 db.Strategy）", d.name, typ.In(1))
		}
		for i := 0; i < typ.NumIn(); i++ {
			name := typ.In(i).Name()
			if name == "" {
				continue
			}
			low := strings.ToLower(name)
			for _, bad := range forbidden {
				if strings.Contains(low, bad) {
					t.Fatalf("%s 的入参 %q 里带着 %q：解除 / 晋级 / 淘汰的判定不接受模型意见，也不接受年龄",
						d.name, name, bad)
				}
			}
		}
	}
}

// =====================================================================
// 行为 13：组内共享同一策略版本快照
// =====================================================================

// TestCuratorE2EGroupMembersShareSameStrategyVersion 钉住「组内共享同一策略版本快照」。
//
// 计划 Task 6 Step 4 与 design §3.3：一个群 = 一个 task ⇒ 快照按 task_id 存 ⇒
// 组内成员天然用**同一份**策略版本。组内成员用不同版本的策略互相污染，
// 比「用旧策略」更糟。
//
// 断言三层：
//  1. 群里**两个不同的成员**解析到的是同一份快照（策略集合与版本逐条相同）；
//  2. 快照的版本号就是策略生成那一刻的版本；
//  3. 策略后来升了版，**快照不动** —— 快照是冻结的那一份，不是每轮重算。
//
// 第 3 条是「同版本」的硬保证：每轮重算会让 planner 看到的策略集漂移，
// 与「组内同版本」直接冲突（计划里明确写了快照在任务启动时生成一次并复用）。
func TestCuratorE2EGroupMembersShareSameStrategyVersion(t *testing.T) {
	s, pg := taskHallTestServer(t)
	sfx := e2eSuffix()

	task := taskHallCreateTask(t, pg, "e2e 组内策略快照")
	category := fmt.Sprintf("e2e-cat-%d", sfx)

	// 一枚范围内资产：资产签名（TaskAssetSignature）取的是**范围内**资产的
	// category ⇒ 快照的候选集由它决定。
	var assetID int64
	if err := pg.QueryRow(`INSERT INTO assets(type, domain, root_domain, category, last_seen)
		VALUES('subdomain',$1,$1,$2,now()) RETURNING id`,
		fmt.Sprintf("e2e-%d.example.test", sfx%1000000), category).Scan(&assetID); err != nil {
		t.Fatalf("插资产: %v", err)
	}
	t.Cleanup(func() { _, _ = pg.Exec(`DELETE FROM assets WHERE id=$1`, assetID) })
	if _, err := pg.Exec(`INSERT INTO task_scope(task_id, kind, asset_id, source)
		VALUES($1,'asset',$2,'manual') ON CONFLICT DO NOTHING`, task.ID, assetID); err != nil {
		t.Fatalf("声明任务范围: %v", err)
	}

	// 一条在役策略：activation 命中本任务的资产签名 ⇒ 它应当进快照。
	strategyID, err := pg.CreateStrategy(&db.Strategy{
		Name:       fmt.Sprintf("e2e组内策略-%d", sfx),
		Summary:    "对 $TARGET 的 $PATH 做一次探测",
		Steps:      json.RawMessage(`["curl -X POST https://$TARGET/$PATH"]`),
		Activation: json.RawMessage(fmt.Sprintf(`{"category":[%q]}`, category)),
	})
	if err != nil {
		t.Fatalf("建策略: %v", err)
	}
	t.Cleanup(func() { _, _ = pg.Exec(`DELETE FROM strategies WHERE id=$1`, strategyID) })
	if ok, err := pg.PromoteStrategyToActive(strategyID); err != nil || !ok {
		t.Fatalf("置为 active: %v", err)
	}

	// 建群（走真实 API）：一个群 = 一个 task。
	code, out := taskHallDo(t, s, "POST", "/api/task-hall/groups", map[string]any{
		"task_id":           task.ID,
		"name":              "e2e 组内快照",
		"members":           []string{"pentest", "worker"},
		"concurrency_limit": 2,
	})
	if code != 201 {
		t.Fatalf("建群: %d (%v)", code, out)
	}
	rawMembers, _ := out["members"].([]any)
	if len(rawMembers) < 2 {
		t.Fatalf("群里成员不足两个：%v", out)
	}
	agentKeys := make([]string, 0, len(rawMembers))
	for _, raw := range rawMembers {
		m := raw.(map[string]any)
		agentKeys = append(agentKeys, m["agent_key"].(string))
	}
	if len(agentKeys) < 2 {
		t.Fatalf("拿不到两个成员：%v", agentKeys)
	}

	// 成员 A 解析快照。
	entriesA, err := pg.EnsureTaskStrategySnapshot(task.ID)
	if err != nil {
		t.Fatalf("生成任务策略快照: %v", err)
	}
	if len(entriesA) == 0 {
		t.Fatalf("快照是空的：在役策略（category=%s）没被召回", category)
	}
	found := false
	for _, e := range entriesA {
		if e.StrategyID == strategyID {
			found = true
		}
	}
	if !found {
		t.Fatalf("快照里没有本用例的策略 %d：%+v", strategyID, entriesA)
	}
	versionA := entriesA[0].Version

	// 成员 B 解析快照（组内成员用的是同一个 task ⇒ 同一份快照）。
	entriesB, err := pg.EnsureTaskStrategySnapshot(task.ID)
	if err != nil {
		t.Fatalf("生成任务策略快照: %v", err)
	}
	if len(entriesA) != len(entriesB) {
		t.Fatalf("组内两个成员看到的策略条数不同：%d vs %d", len(entriesA), len(entriesB))
	}
	for i := range entriesA {
		if entriesA[i].StrategyID != entriesB[i].StrategyID {
			t.Fatalf("组内两个成员看到的策略不同：%d vs %d", entriesA[i].StrategyID, entriesB[i].StrategyID)
		}
		if entriesA[i].Version != entriesB[i].Version {
			t.Fatalf("组内两个成员看到的策略版本不同：%d vs %d", entriesA[i].Version, entriesB[i].Version)
		}
	}

	// 策略后来升了版 ⇒ 快照**不动**（冻结的那一份，组内不会各自漂移）。
	if _, err := pg.Exec(`UPDATE strategies SET version=version+7 WHERE id=$1`, strategyID); err != nil {
		t.Fatalf("升策略版本: %v", err)
	}
	entriesC, err := pg.EnsureTaskStrategySnapshot(task.ID)
	if err != nil {
		t.Fatalf("再生成任务策略快照: %v", err)
	}
	for _, e := range entriesC {
		if e.StrategyID == strategyID && e.Version != versionA {
			t.Fatalf("快照跟着策略升版了（%d → %d）—— 快照必须是冻结的那一份，否则组内成员会看到不同版本",
				versionA, e.Version)
		}
	}
}

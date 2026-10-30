package server

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
)

// attributorTestSuffix 给用例生成互不撞的标识后缀。
//
// 为什么自己写一个而不复用别的测试文件里的同名助手：那些文件由别的单元并行
// 持有，本单元的用例不该挂在别人的私有助手上。
func attributorTestSuffix() int64 { return time.Now().UnixNano() }

// attributorMilestoneCriteria 是一条能确定性匹配的里程碑判据。
//
// 命中判定走的是「activity 文本 + 正则」的确定性匹配（不经 LLM）：判据里写
// 明了「在 detail 里找 syntax error」。
func attributorMilestoneCriteria(id string) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(
		`[{"id":%q,"where":"detail","match":"(?i)syntax error","kind":"command"}]`, id))
}

// attributorTask 建一个任务并在用例结束时删掉。
func attributorTask(t *testing.T, pg *db.DB, desc string) *db.Task {
	t.Helper()
	task, err := pg.CreateTask(desc, "验证子任务归因", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pg.DeleteTask(task.ID) })
	return task
}

// attributorStrategy 建一条带里程碑判据的策略。
func attributorStrategy(t *testing.T, pg *db.DB, name string) int64 {
	t.Helper()
	id, err := pg.CreateStrategy(&db.Strategy{
		Name:            name,
		Summary:         "对 $TARGET 的 $PATH 做一次探测",
		Steps:           json.RawMessage(`["curl -X POST https://$TARGET/$PATH"]`),
		SuccessCriteria: attributorMilestoneCriteria("m1"),
	})
	if err != nil {
		t.Fatalf("建策略 %s: %v", name, err)
	}
	return id
}

// attributorSubTask 在任务里造一个子任务：一个意图 + 一条 fact + 若干执行步骤。
//
// 返回意图 id 与 fact 节点 id。detail 决定里程碑命中与否。
func attributorSubTask(t *testing.T, pg *db.DB, task *db.Task, factState, detail string) (int64, int64) {
	t.Helper()
	store := pg.Exploration(task.ExplorationID)
	intentID, err := store.AddNode(db.KindIntent, map[string]any{"summary": "探测目标"},
		5, "done", "worker", nil)
	if err != nil {
		t.Fatalf("建意图: %v", err)
	}
	factID, err := store.AddNode(db.KindFact, map[string]any{"summary": "探测结论"},
		5, factState, "worker", nil)
	if err != nil {
		t.Fatalf("建事实节点: %v", err)
	}
	if err := store.Link(intentID, db.RelYields, factID); err != nil {
		t.Fatalf("连血缘: %v", err)
	}
	nodeID := intentID
	if _, err := store.AppendActivity(db.Activity{
		NodeID:  &nodeID,
		Worker:  "worker",
		Kind:    "tool_result",
		Tool:    "curl",
		Summary: "curl 探测",
		Detail:  detail,
	}); err != nil {
		t.Fatalf("写执行记录: %v", err)
	}
	return intentID, factID
}

// attributorUse 把一条策略挂到某个意图的上下文里（模拟「检索命中并注入」）。
func attributorUse(t *testing.T, pg *db.DB, task *db.Task, strategyID, intentID int64) {
	t.Helper()
	if err := pg.RecordStrategyUsage(db.StrategyUsage{
		StrategyID:    strategyID,
		TaskID:        task.ID,
		ExplorationID: task.ExplorationID,
		IntentID:      intentID,
		Outcome:       db.StrategyUsagePending,
	}); err != nil {
		t.Fatalf("记策略使用明细: %v", err)
	}
}

// attributorClassification 跑一次归因并按 fact 节点 id 取出归因结论。
func attributorClassification(t *testing.T, pg *db.DB, taskID, factID int64) SubTaskAttribution {
	t.Helper()
	attrs, err := AttributeSubTasks(pg, taskID)
	if err != nil {
		t.Fatalf("归因: %v", err)
	}
	for _, a := range attrs {
		if a.NodeID == factID {
			return a
		}
	}
	t.Fatalf("归因结果里找不到子任务 %d", factID)
	return SubTaskAttribution{}
}

// TestAttributeSubTasksClassifiesThreeWays 钉住三分类的判据。
//
//	A) 策略 S 在上下文里 + 里程碑命中   → skill_guided
//	B) 策略 S 在上下文里 + 里程碑未命中 → independent
//	C) 没有策略在上下文里               → environment
//
// A 与 B 的区别只在「里程碑有没有命中」—— 里程碑判定是确定性的字符串/正则
// 匹配，不经 LLM（反模式 4）。
func TestAttributeSubTasksClassifiesThreeWays(t *testing.T) {
	_, pg := taskHallTestServer(t)
	task := attributorTask(t, pg, "三分类归因")
	strategyID := attributorStrategy(t, pg, fmt.Sprintf("三分类策略-%d", attributorTestSuffix()))

	hitIntent, hitFact := attributorSubTask(t, pg, task, "confirmed",
		"HTTP/1.1 500 Internal Server Error\nsyntax error near unquoted string")
	attributorUse(t, pg, task, strategyID, hitIntent)

	missIntent, missFact := attributorSubTask(t, pg, task, "confirmed",
		"HTTP/1.1 200 OK\n页面正常返回，没有任何异常")
	attributorUse(t, pg, task, strategyID, missIntent)

	_, envFact := attributorSubTask(t, pg, task, "confirmed", "HTTP/1.1 200 OK\n正常")

	attrs, err := AttributeSubTasks(pg, task.ID)
	if err != nil {
		t.Fatalf("归因: %v", err)
	}
	byNode := map[int64]SubTaskAttribution{}
	for _, a := range attrs {
		byNode[a.NodeID] = a
	}

	if got := byNode[hitFact].Classification; got != AttributionSkillGuided {
		t.Fatalf("A) 策略在上下文里且里程碑命中 ⇒ %s，实际 %s", AttributionSkillGuided, got)
	}
	if hits := byNode[hitFact].MilestonesHit; len(hits) != 1 || hits[0] != "m1" {
		t.Fatalf("A) 命中的里程碑不对：%v", hits)
	}
	if got := byNode[missFact].Classification; got != AttributionIndependent {
		t.Fatalf("B) 策略在上下文里但里程碑未命中 ⇒ %s，实际 %s", AttributionIndependent, got)
	}
	if got := byNode[envFact].Classification; got != AttributionEnvironment {
		t.Fatalf("C) 没有策略上下文 ⇒ %s，实际 %s", AttributionEnvironment, got)
	}
}

// TestAttributeSubTasksRejectsAmbiguousStrategyContext 钉住「至多一个策略上下文」。
//
// 一个子任务的上下文里有【两条】策略 ⇒ 前提被破坏 ⇒ 归为 environment
// （不可归因），**不得给任何策略加分**。
func TestAttributeSubTasksRejectsAmbiguousStrategyContext(t *testing.T) {
	_, pg := taskHallTestServer(t)
	task := attributorTask(t, pg, "歧义策略上下文")
	s1 := attributorStrategy(t, pg, fmt.Sprintf("歧义策略甲-%d", attributorTestSuffix()))
	s2 := attributorStrategy(t, pg, fmt.Sprintf("歧义策略乙-%d", attributorTestSuffix()))

	intentID, factID := attributorSubTask(t, pg, task, "confirmed",
		"HTTP/1.1 500 Internal Server Error\nsyntax error near unquoted string")
	attributorUse(t, pg, task, s1, intentID)
	attributorUse(t, pg, task, s2, intentID)

	before1, _ := pg.GetStrategy(s1)
	before2, _ := pg.GetStrategy(s2)

	a := attributorClassification(t, pg, task.ID, factID)
	if a.Classification != AttributionEnvironment {
		t.Fatalf("两条策略在上下文里 ⇒ %s，实际 %s", AttributionEnvironment, a.Classification)
	}
	if len(a.StrategyIDs) != 2 {
		t.Fatalf("归因结论里应当带出两条策略，实际 %v", a.StrategyIDs)
	}

	after1, _ := pg.GetStrategy(s1)
	after2, _ := pg.GetStrategy(s2)
	if after1.Confidence != before1.Confidence || after2.Confidence != before2.Confidence {
		t.Fatalf("歧义上下文给策略加了票：甲 %v→%v 乙 %v→%v",
			before1.Confidence, after1.Confidence, before2.Confidence, after2.Confidence)
	}
}

// TestFailedSubTaskNeverTouchesTheLibrary 钉住「失败子任务永不直接改库」。
//
// 失败子任务 ⇒ strategy_usage.outcome='miss'，且 strategies 的
// confidence / utility / run_utility 【一字未改】。
//
// 为什么这条必须成立：失败路径是最容易污染库的地方 —— 一次没打中就把策略
// 降权/降信心，库就变成了「谁跑得少谁被淘汰」的计数器，而不是「什么有效」。
func TestFailedSubTaskNeverTouchesTheLibrary(t *testing.T) {
	_, pg := taskHallTestServer(t)
	task := attributorTask(t, pg, "失败子任务不改库")
	strategyID := attributorStrategy(t, pg, fmt.Sprintf("失败不改库策略-%d", attributorTestSuffix()))

	intentID, _ := attributorSubTask(t, pg, task, "dismissed",
		"HTTP/1.1 404 Not Found\n目标不存在")
	attributorUse(t, pg, task, strategyID, intentID)

	before, err := pg.GetStrategy(strategyID)
	if err != nil || before == nil {
		t.Fatalf("读策略: %v", err)
	}
	if err := pg.EnsureStrategyProposalSchema(); err != nil {
		t.Fatalf("建表: %v", err)
	}

	if _, err := AttributeSubTasks(pg, task.ID); err != nil {
		t.Fatalf("归因: %v", err)
	}

	after, err := pg.GetStrategy(strategyID)
	if err != nil || after == nil {
		t.Fatalf("读策略: %v", err)
	}
	if after.Confidence != before.Confidence {
		t.Fatalf("失败子任务改了 confidence：%v → %v", before.Confidence, after.Confidence)
	}
	if after.Utility != before.Utility {
		t.Fatalf("失败子任务改了 utility：%v → %v", before.Utility, after.Utility)
	}
	if after.RunUtility != before.RunUtility {
		t.Fatalf("失败子任务改了 run_utility：%v → %v", before.RunUtility, after.RunUtility)
	}
	if after.UsageCount != before.UsageCount || after.HitCount != before.HitCount {
		t.Fatalf("失败子任务改了 usage/hit 计数：%v/%v → %v/%v",
			before.UsageCount, before.HitCount, after.UsageCount, after.HitCount)
	}

	// usage 必须留下 outcome='miss' 的诊断记录。
	usages, err := pg.StrategyUsageForTask(task.ID)
	if err != nil {
		t.Fatalf("读使用明细: %v", err)
	}
	found := false
	for _, u := range usages {
		if u.StrategyID == strategyID {
			found = true
			if u.Outcome != db.StrategyUsageMiss {
				t.Fatalf("失败子任务的 usage 结果应是 miss，实际 %q", u.Outcome)
			}
		}
	}
	if !found {
		t.Fatalf("失败子任务没有留下 usage 诊断记录")
	}

	// 证据链也不能被失败子任务写脏。
	var n int
	if err := pg.QueryRow(`SELECT count(*) FROM strategy_evidence WHERE strategy_id=$1`,
		strategyID).Scan(&n); err != nil {
		t.Fatalf("查策略证据: %v", err)
	}
	if n != 0 {
		t.Fatalf("失败子任务写出了 %d 条策略证据（应当一条都不写）", n)
	}
}

// TestOnlySkillGuidedRaisesConfidence 钉住「只有 skill_guided 加票」。
//
// skill_guided → 该策略的票 +1；independent → 该策略的票 **不变**
// （不得据自主探索成功给策略归因 —— 反模式 10）。
func TestOnlySkillGuidedRaisesConfidence(t *testing.T) {
	_, pg := taskHallTestServer(t)
	task := attributorTask(t, pg, "只有 skill_guided 加票")
	guidedStrategy := attributorStrategy(t, pg, fmt.Sprintf("有功策略-%d", attributorTestSuffix()))
	independentStrategy := attributorStrategy(t, pg, fmt.Sprintf("独立成功策略-%d", attributorTestSuffix()))

	hitIntent, _ := attributorSubTask(t, pg, task, "confirmed",
		"HTTP/1.1 500 Internal Server Error\nsyntax error near unquoted string")
	attributorUse(t, pg, task, guidedStrategy, hitIntent)

	missIntent, _ := attributorSubTask(t, pg, task, "confirmed",
		"HTTP/1.1 200 OK\n一切正常")
	attributorUse(t, pg, task, independentStrategy, missIntent)

	before, err := pg.GetStrategy(guidedStrategy)
	if err != nil || before == nil {
		t.Fatalf("读策略: %v", err)
	}
	beforeIndependent, err := pg.GetStrategy(independentStrategy)
	if err != nil || beforeIndependent == nil {
		t.Fatalf("读策略: %v", err)
	}

	if _, err := AttributeSubTasks(pg, task.ID); err != nil {
		t.Fatalf("归因: %v", err)
	}

	after, err := pg.GetStrategy(guidedStrategy)
	if err != nil || after == nil {
		t.Fatalf("读策略: %v", err)
	}
	if after.Confidence != before.Confidence+1 {
		t.Fatalf("skill_guided 的票应 +1：%v → %v", before.Confidence, after.Confidence)
	}
	if after.RunUtility != before.RunUtility+1 {
		t.Fatalf("skill_guided 的 run_utility 应 +1：%v → %v", before.RunUtility, after.RunUtility)
	}

	afterIndependent, err := pg.GetStrategy(independentStrategy)
	if err != nil || afterIndependent == nil {
		t.Fatalf("读策略: %v", err)
	}
	if afterIndependent.Confidence != beforeIndependent.Confidence {
		t.Fatalf("independent 的票必须不变：%v → %v",
			beforeIndependent.Confidence, afterIndependent.Confidence)
	}
	if afterIndependent.RunUtility != beforeIndependent.RunUtility {
		t.Fatalf("independent 的 run_utility 必须不变：%v → %v",
			beforeIndependent.RunUtility, afterIndependent.RunUtility)
	}
}

// TestProposeFromAttributionsRoutesThreeWays 钉住提议路由的三条路。
//
//	同名同参数 → edit_existing；新 procedure → create_new；无价值 → skip
//
// 并断言产生的提议 status 恒为 'pending'（裁决留给宿主侧）。
func TestProposeFromAttributionsRoutesThreeWays(t *testing.T) {
	_, pg := taskHallTestServer(t)
	task := attributorTask(t, pg, "提议三路路由")

	existing := attributorStrategy(t, pg, fmt.Sprintf("已有策略-%d", attributorTestSuffix()))
	existingName := ""
	if s, err := pg.GetStrategy(existing); err == nil && s != nil {
		existingName = s.Name
	}

	attrs := []SubTaskAttribution{
		{
			NodeID:         101,
			Classification: AttributionSkillGuided,
			StrategyIDs:    []int64{existing},
			MilestonesHit:  []string{"m1"},
			Reason:         "策略在上下文里且里程碑命中",
		},
		{
			NodeID:         102,
			Classification: AttributionSkillGuided,
			MilestonesHit:  []string{"m2"},
			Procedure: &StrategyProcedure{
				Name:    fmt.Sprintf("新可复用件-%d", attributorTestSuffix()),
				Summary: "对 $TARGET 做一次探测",
			},
		},
		{
			NodeID:         103,
			Classification: AttributionIndependent,
			StrategyIDs:    []int64{existing},
			Reason:         "成功不归功于该策略",
		},
	}

	ids, err := ProposeFromAttributions(pg, task.ID, attrs)
	if err != nil {
		t.Fatalf("提议路由: %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("应产生 2 条提议（edit_existing + create_new），实际 %d", len(ids))
	}

	proposals, err := pg.ListStrategyProposals(db.StrategyProposalPending, 100)
	if err != nil {
		t.Fatalf("列提议: %v", err)
	}
	routes := map[string]int{}
	for _, p := range proposals {
		routes[p.Route]++
		if p.Status != db.StrategyProposalPending {
			t.Fatalf("提议状态必须是 pending（实际 %q）", p.Status)
		}
		if p.CreatedBy == "" {
			t.Fatalf("提议必须有署名")
		}
	}
	if routes[db.StrategyRouteEditExisting] != 1 {
		t.Fatalf("edit_existing 提议数不对：%v", routes)
	}
	if routes[db.StrategyRouteCreateNew] != 1 {
		t.Fatalf("create_new 提议数不对：%v", routes)
	}
	if routes[db.StrategyRouteRetire] != 0 {
		t.Fatalf("不该有 retire 提议：%v", routes)
	}
	if existingName == "" {
		t.Fatalf("读不到已有策略名")
	}
}

// TestAgentCannotWriteActiveStrategy 钉住「agent 只能提议，写不出 active」。
//
// 三层保证：
//  1. 归因器 + 提议器跑完之后，库里**没有**任何 active 策略被创建出来；
//  2. 提议恒为 pending，裁决权不在 agent 手里；
//  3. agent 侧唯一的「建策略」入口（db.CreateStrategy）**拒绝** active ——
//     晋级是裁决写，只能在宿主侧发生。
func TestAgentCannotWriteActiveStrategy(t *testing.T) {
	_, pg := taskHallTestServer(t)
	task := attributorTask(t, pg, "agent 写不出 active")
	strategyID := attributorStrategy(t, pg, fmt.Sprintf("待提议策略-%d", attributorTestSuffix()))

	activeBefore, err := pg.ListStrategies(db.StrategyActive, 500)
	if err != nil {
		t.Fatalf("列 active 策略: %v", err)
	}

	intentID, _ := attributorSubTask(t, pg, task, "confirmed",
		"HTTP/1.1 500 Internal Server Error\nsyntax error near unquoted string")
	attributorUse(t, pg, task, strategyID, intentID)

	attrs, err := AttributeSubTasks(pg, task.ID)
	if err != nil {
		t.Fatalf("归因: %v", err)
	}
	if _, err := ProposeFromAttributions(pg, task.ID, attrs); err != nil {
		t.Fatalf("提议路由: %v", err)
	}

	activeAfter, err := pg.ListStrategies(db.StrategyActive, 500)
	if err != nil {
		t.Fatalf("列 active 策略: %v", err)
	}
	if len(activeAfter) != len(activeBefore) {
		t.Fatalf("agent 侧写出了 active 策略：%d → %d", len(activeBefore), len(activeAfter))
	}

	proposals, err := pg.ListStrategyProposals(db.StrategyProposalPending, 100)
	if err != nil {
		t.Fatalf("列提议: %v", err)
	}
	if len(proposals) == 0 {
		t.Fatalf("应当有一条待裁决提议")
	}
	for _, p := range proposals {
		if p.Status != db.StrategyProposalPending {
			t.Fatalf("agent 写出了非 pending 的提议：%q", p.Status)
		}
	}

	// agent 侧唯一的建策略入口直接拒绝 active。
	if _, err := pg.CreateStrategy(&db.Strategy{
		Name:    fmt.Sprintf("越权晋级-%d", attributorTestSuffix()),
		Summary: "模板 $TARGET",
		State:   db.StrategyActive,
	}); err == nil {
		t.Fatalf("agent 侧建 active 策略被放行（应当拒绝）")
	} else if !strings.Contains(err.Error(), db.StrategyQuarantined) {
		t.Fatalf("拒绝理由应当点明只能建 %s，实际 %v", db.StrategyQuarantined, err)
	}
}

// TestAttributeSubTasksIsIdempotent 钉住「同一票不会被反复计分」。
//
// 归因挂在**意图完成**上：后续意图完成时整批再归因一遍，同一个意图会被归因
// 多次。没有幂等闸门 ⇒ 一次成功被算成很多次票 ⇒ 策略的信心虚高。
func TestAttributeSubTasksIsIdempotent(t *testing.T) {
	_, pg := taskHallTestServer(t)
	task := attributorTask(t, pg, "归因幂等")
	strategyID := attributorStrategy(t, pg, fmt.Sprintf("幂等策略-%d", attributorTestSuffix()))

	intentID, _ := attributorSubTask(t, pg, task, "confirmed",
		"HTTP/1.1 500 Internal Server Error\nsyntax error near unquoted string")
	attributorUse(t, pg, task, strategyID, intentID)

	for i := 0; i < 3; i++ {
		if _, err := AttributeSubTasks(pg, task.ID); err != nil {
			t.Fatalf("第 %d 次归因: %v", i+1, err)
		}
	}
	after, err := pg.GetStrategy(strategyID)
	if err != nil || after == nil {
		t.Fatalf("读策略: %v", err)
	}
	if after.Confidence != 1 {
		t.Fatalf("同一票被计了 %v 次（应当只计 1 次）", after.Confidence)
	}
	if after.RunUtility != 1 {
		t.Fatalf("run_utility 被计了 %v 次（应当只计 1 次）", after.RunUtility)
	}
}

// TestMilestoneHitIsDeterministic 钉住里程碑判定是确定性的、且不经 LLM。
//
// 判据「在 detail 里找 syntax error」：命中一次就命中，任何时候都命中；
// 判据写在 tool 上则只看工具名。
func TestMilestoneHitIsDeterministic(t *testing.T) {
	acts := []db.Activity{
		{ID: 1, Kind: "tool_result", Tool: "curl", Summary: "curl 探测",
			Detail: "HTTP/1.1 500\nsyntax error near unquoted string"},
		{ID: 2, Kind: "tool_use", Tool: "nmap", Summary: "nmap 扫描", Detail: "80/tcp open"},
	}
	criteria := attributorMilestoneCriteria("m1")
	hits := MilestoneHit(criteria, acts)
	if len(hits) != 1 || hits[0] != "m1" {
		t.Fatalf("里程碑命中不对：%v", hits)
	}
	// 同样的输入再来一次，结果必须一样（确定性）。
	hits2 := MilestoneHit(criteria, acts)
	if len(hits2) != 1 || hits2[0] != "m1" {
		t.Fatalf("里程碑判定不确定：%v vs %v", hits, hits2)
	}
	// 判据写在 tool 上：只有工具名匹配才算命中。
	toolCriteria := json.RawMessage(`[{"id":"m2","where":"tool","match":"nmap"}]`)
	hits3 := MilestoneHit(toolCriteria, acts)
	if len(hits3) != 1 || hits3[0] != "m2" {
		t.Fatalf("工具名里程碑命中不对：%v", hits3)
	}
	// 判据为空 ⇒ 不命中任何东西。
	if h := MilestoneHit(nil, acts); len(h) != 0 {
		t.Fatalf("空判据不该命中：%v", h)
	}
}

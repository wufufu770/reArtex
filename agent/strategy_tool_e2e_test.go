// 本文件是策略库（L4）Task 6 的**端到端**验收用例：策略工具与上下文注入。
//
// 与 db 层 / server 层零件测试的分工：
//   - db 层钉的是「检索 SQL 只返 active」「快照类型没有 steps 字段」；
//   - server 层钉的是「curator 的裁决写真的迁移了状态」；
//   - 本文件钉的是**agent 真正调工具、planner 真正拿到注入文本**时，
//     承诺还成不成立：检索结果里是不是只有 active、注入文本里是不是没有正文、
//     提议是不是写不出 active、strategy_get 是不是默认谁都不给。
//
// 【口径来源】断言以计划为准（/home/analyst/.omo/plans/
// 2026-10-29-task-hall-phase6-implementation.md 的 Task 6 与「风险与注意」）。
//
// 【测试隔离】每个用例造的东西都在用例结束时删掉（唯一名 + 显式清理），
// 不依赖用例之间的执行顺序。
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
	actool "github.com/Autumn-27/norma/tool"
)

// strategyToolSuffix 给用例生成互不撞的标识后缀（纳秒级）。
//
// 为什么要唯一：策略名受 uq_strategies_company_name 唯一索引约束，
// 重名会直接写不进去 ⇒ 用例之间互相踩。
func strategyToolSuffix() int64 { return time.Now().UnixNano() }

// strategyToolFixture 铺出「一个任务 + 一枚范围内资产 + 一套工具」。
//
// 为什么资产要走 task_scope(kind='asset')：策略检索的资产签名取的是**范围内**
// 资产的 category（db.AssetStore.TaskAssetSignature）⇒ 不声明范围，签名就是空的，
// 结构路径匹配不到任何候选。
type strategyToolFixture struct {
	db       *db.DB
	task     *db.Task
	tools    *ToolSet
	category string
}

// newStrategyToolFixture 建夹具。
//
// 工具集用真实的 ExplorationStore + AssetStore：策略工具的 handler 直接解引用
// 这两个句柄，用空壳工具集测不出「签名算不出来」这类装配错误。
func newStrategyToolFixture(t *testing.T) *strategyToolFixture {
	t.Helper()
	d := testDB(t)
	t.Cleanup(func() { d.Close() })

	sfx := strategyToolSuffix()
	category := fmt.Sprintf("e2e-cat-%d", sfx)

	task, err := d.CreateTask(
		fmt.Sprintf("e2e 策略工具用例 %d", sfx),
		fmt.Sprintf("e2e 目标 %d 的独占措辞", sfx),
		nil, 0, 0)
	if err != nil {
		t.Fatalf("建任务: %v", err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(task.ID) })

	var assetID int64
	if err := d.QueryRow(`INSERT INTO assets(type, domain, root_domain, category, last_seen)
		VALUES('subdomain',$1,$1,$2,now()) RETURNING id`,
		fmt.Sprintf("e2e-%d.example.test", sfx%1000000), category).Scan(&assetID); err != nil {
		t.Fatalf("插资产: %v", err)
	}
	t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM assets WHERE id=$1`, assetID) })
	if _, err := d.Exec(`INSERT INTO task_scope(task_id, kind, asset_id, source)
		VALUES($1,'asset',$2,'manual') ON CONFLICT DO NOTHING`, task.ID, assetID); err != nil {
		t.Fatalf("声明任务范围: %v", err)
	}

	tools := NewToolSet(d.Exploration(task.ExplorationID), "planner")
	tools.SetTaskID(task.ID)
	tools.SetAssetStore(d.Assets(), d.Companies())

	return &strategyToolFixture{db: d, task: task, tools: tools, category: category}
}

// strategyToolByName 从 planner 的工具集里取一个工具。
//
// 为什么走 PlannerTools() 而不是直接调构造器：PlannerTools 是**注册面**，
// 从这里取才同时钉住「工具确实挂上了」与「工具的行为」。
func strategyToolByName(t *testing.T, ts *ToolSet, name string) actool.CoreTool {
	t.Helper()
	for _, tool := range ts.PlannerTools() {
		if tool.Name() == name {
			return tool
		}
	}
	t.Fatalf("planner 工具集里没有 %s", name)
	return nil
}

// strategyToolCall 调用一个工具并返回结果正文。
func strategyToolCall(t *testing.T, tool actool.CoreTool, input any) string {
	t.Helper()
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	res, err := tool.Call(context.Background(), raw, nil)
	if err != nil {
		t.Fatalf("%s 调用失败: %v", tool.Name(), err)
	}
	return res.Flatten()
}

// strategyToolSeed 建一条策略并返回它的 id（用例结束时删掉）。
func strategyToolSeed(t *testing.T, fx *strategyToolFixture, name, summary string) int64 {
	t.Helper()
	id, err := fx.db.CreateStrategy(&db.Strategy{
		Name:       name,
		Summary:    summary,
		Steps:      json.RawMessage(`["curl -X POST https://$TARGET/$PATH"]`),
		Activation: json.RawMessage(fmt.Sprintf(`{"category":[%q]}`, fx.category)),
	})
	if err != nil {
		t.Fatalf("建策略 %s: %v", name, err)
	}
	t.Cleanup(func() { _, _ = fx.db.Exec(`DELETE FROM strategies WHERE id=$1`, id) })
	return id
}

// =====================================================================
// 行为 8：strategy_search 只返 active
// =====================================================================

// TestStrategyToolSearchOnlyReturnsActiveStrategies 钉住「检索只返 active」。
//
// 种四条同激活条件的策略（隔离 / 在役 / 已退役 / 已墓碑），再调 strategy_search：
// 回来的**必须只有**在役那一条。
//
// 为什么这条是硬的：隔离态是「未经验证的新策略」，退役 / 墓碑是历史 ——
// 任何一条漏进检索结果，都会被注入到 planner 的上下文里当成「现在该怎么做」。
func TestStrategyToolSearchOnlyReturnsActiveStrategies(t *testing.T) {
	fx := newStrategyToolFixture(t)
	sfx := strategyToolSuffix()

	quarantined := strategyToolSeed(t, fx, fmt.Sprintf("e2e-隔离-%d", sfx), "e2e 隔离态摘要")
	active := strategyToolSeed(t, fx, fmt.Sprintf("e2e-在役-%d", sfx), "e2e 在役态摘要")
	retired := strategyToolSeed(t, fx, fmt.Sprintf("e2e-退役-%d", sfx), "e2e 退役态摘要")
	tombstoned := strategyToolSeed(t, fx, fmt.Sprintf("e2e-墓碑-%d", sfx), "e2e 墓碑态摘要")

	if ok, err := fx.db.PromoteStrategyToActive(active); err != nil || !ok {
		t.Fatalf("置为 active: %v", err)
	}
	// 退役：状态迁移原语要求「从 active 退役」⇒ 先在役再退役。
	if ok, err := fx.db.PromoteStrategyToActive(retired); err != nil || !ok {
		t.Fatalf("置为 active: %v", err)
	}
	if ok, err := fx.db.AutoRetireStrategy(retired, "e2e 人工退役理由"); err != nil || !ok {
		t.Fatalf("退役: %v", err)
	}
	if ok, err := fx.db.TombstoneStrategyByTTL(tombstoned); err != nil || !ok {
		t.Fatalf("墓碑化: %v", err)
	}

	out := strategyToolCall(t, strategyToolByName(t, fx.tools, "strategy_search"), map[string]any{})
	var parsed struct {
		Strategies []struct {
			ID      int64  `json:"id"`
			Name    string `json:"name"`
			Summary string `json:"summary"`
		} `json:"strategies"`
		Count int `json:"count"`
	}
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("解析检索结果失败: %v（原文 %s）", err, out)
	}

	if len(parsed.Strategies) != 1 {
		t.Fatalf("检索结果条数 = %d，期望 1（只有在役那一条）：%s", len(parsed.Strategies), out)
	}
	got := parsed.Strategies[0].ID
	if got != active {
		t.Fatalf("检索回来的是策略 %d，期望在役那条 %d（隔离 %d / 退役 %d / 墓碑 %d）",
			got, active, quarantined, retired, tombstoned)
	}
	for _, forbidden := range []int64{quarantined, retired, tombstoned} {
		for _, item := range parsed.Strategies {
			if item.ID == forbidden {
				t.Fatalf("策略 %d（隔离/退役/墓碑）出现在检索结果里", forbidden)
			}
		}
	}
}

// =====================================================================
// 行为 9：注入上下文只带摘要，绝不带 steps
// =====================================================================

// TestStrategyToolInjectedContextCarriesSummaryOnly 钉住「注入路径不带正文」。
//
// 三层断言：
//  1. 结构层：db.StrategySnapshotEntry **没有** steps 字段 ⇒ 注入路径拿不到正文；
//  2. 渲染层：RenderStrategySummaries 的输出里有 summary、没有 steps 正文；
//  3. 装配层：planner 真正注入的那段文本里有 summary、没有 steps 正文，
//     连 "steps" 这个词都不出现。
//
// 为什么这条是硬的（计划「风险与注意」明写）：steps 是命令模板，一旦进了
// planner 上下文就等于「把可执行产物塞进上下文」（反模式 8），而本阶段
// **没有**任何执行前的拦截关口 —— 塞进去就是塞进去了。
func TestStrategyToolInjectedContextCarriesSummaryOnly(t *testing.T) {
	fx := newStrategyToolFixture(t)
	sfx := strategyToolSuffix()

	const marker = "E2EONLYSTEPBODY"
	summary := fmt.Sprintf("e2e 注入摘要 %d", sfx)
	id, err := fx.db.CreateStrategy(&db.Strategy{
		Name:       fmt.Sprintf("e2e-注入-%d", sfx),
		Summary:    summary,
		Steps:      json.RawMessage(fmt.Sprintf(`["curl -X POST https://$TARGET/$PATH --tag %s"]`, marker)),
		Activation: json.RawMessage(fmt.Sprintf(`{"category":[%q]}`, fx.category)),
	})
	if err != nil {
		t.Fatalf("建策略: %v", err)
	}
	t.Cleanup(func() { _, _ = fx.db.Exec(`DELETE FROM strategies WHERE id=$1`, id) })
	if ok, err := fx.db.PromoteStrategyToActive(id); err != nil || !ok {
		t.Fatalf("置为 active: %v", err)
	}

	// ① 结构层：快照条目类型里没有 steps 字段。
	if _, has := reflect.TypeOf(db.StrategySnapshotEntry{}).FieldByName("Steps"); has {
		t.Fatalf("db.StrategySnapshotEntry 带 Steps 字段 ⇒ 注入路径可能把正文带进上下文")
	}

	// ② 渲染层：摘要渲染里只有 name + summary + utility。
	entries, err := fx.db.EnsureTaskStrategySnapshot(fx.task.ID)
	if err != nil {
		t.Fatalf("生成任务策略快照: %v", err)
	}
	if len(entries) == 0 {
		t.Fatalf("快照是空的：在役策略没被召回")
	}
	rendered := RenderStrategySummaries(entries)
	if !strings.Contains(rendered, summary) {
		t.Fatalf("渲染结果里没有策略的 summary：%s", rendered)
	}
	if strings.Contains(rendered, marker) {
		t.Fatalf("渲染结果里出现了 steps 正文（%s）：%s", marker, rendered)
	}

	// ③ 装配层：planner 真正注入的那段文本。
	p := NewPlanner(nil, "e2e-model", t.TempDir(), nil, 10, 3)
	p.SetStrategyContextProvider(func(taskID int64) string {
		es, err := fx.db.EnsureTaskStrategySnapshot(taskID)
		if err != nil {
			return ""
		}
		return RenderStrategySummaries(es)
	})
	injected := p.strategyContextBlock(fx.task.ID)
	if injected == "" {
		t.Fatalf("planner 没有拿到策略注入块")
	}
	if !strings.Contains(injected, summary) {
		t.Fatalf("注入文本里没有策略的 summary：%s", injected)
	}
	if strings.Contains(injected, marker) {
		t.Fatalf("注入文本里出现了 steps 正文（%s）：%s", marker, injected)
	}
	if strings.Contains(strings.ToLower(injected), "steps") {
		t.Fatalf("注入文本里出现了 steps 字样：%s", injected)
	}
}

// =====================================================================
// 行为 10：strategy_propose 写不出 active
// =====================================================================

// TestStrategyToolProposeCannotProduceActiveStrategy 钉住「agent 侧只能提议」。
//
// 三层断言：
//  1. 调完 strategy_propose，库里那条策略的 state 仍然是 quarantined；
//  2. 同时留下一条 status='pending' 的提议（提议确实登记了，只是没生效）；
//  3. **结构层**：CreateStrategy 显式要求 active 会被拒 —— 写入路径上根本没有
//     「写 active」这个口子（晋级是宿主侧的裁决写）。
func TestStrategyToolProposeCannotProduceActiveStrategy(t *testing.T) {
	fx := newStrategyToolFixture(t)
	sfx := strategyToolSuffix()

	name := fmt.Sprintf("e2e-提议-%d", sfx)
	out := strategyToolCall(t, strategyToolByName(t, fx.tools, "strategy_propose"), map[string]any{
		"name":     name,
		"summary":  "e2e 提议摘要",
		"reason":   "e2e 这条值得沉淀",
		"steps":    []string{"curl -X POST https://$TARGET/$PATH"},
		"evidence": []any{},
	})
	var parsed struct {
		StrategyID     int64  `json:"strategy_id"`
		ProposalID     int64  `json:"proposal_id"`
		State          string `json:"state"`
		ProposalStatus string `json:"proposal_status"`
	}
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("解析提议结果失败: %v（原文 %s）", err, out)
	}
	if parsed.StrategyID <= 0 {
		t.Fatalf("提议结果里没有策略 id：%s", out)
	}
	t.Cleanup(func() { _, _ = fx.db.Exec(`DELETE FROM strategies WHERE id=$1`, parsed.StrategyID) })

	// ① 策略停在隔离态。
	s, err := fx.db.GetStrategy(parsed.StrategyID)
	if err != nil || s == nil {
		t.Fatalf("读策略: %v", err)
	}
	if s.State != db.StrategyQuarantined {
		t.Fatalf("提议出来的策略状态 = %s，期望 %s", s.State, db.StrategyQuarantined)
	}

	// ② 提议登记了，且停在 pending。
	if parsed.ProposalStatus != db.StrategyProposalPending {
		t.Fatalf("提议状态 = %q，期望 %q", parsed.ProposalStatus, db.StrategyProposalPending)
	}
	proposals, err := fx.db.ListStrategyProposals(db.StrategyProposalPending, 200)
	if err != nil {
		t.Fatalf("列提议: %v", err)
	}
	found := false
	for _, p := range proposals {
		if p.StrategyID == parsed.StrategyID {
			found = true
		}
	}
	if !found {
		t.Fatalf("提议没有登记为 pending：%+v", parsed)
	}

	// ③ 结构层：写入路径上没有「写 active」的口子。
	_, err = fx.db.CreateStrategy(&db.Strategy{
		Name:    fmt.Sprintf("e2e-硬塞active-%d", sfx),
		Summary: "e2e 试图直接写 active",
		State:   db.StrategyActive,
	})
	if err == nil {
		t.Fatalf("CreateStrategy 竟然接受了 active —— agent 侧写得出 active 了")
	}
}

// =====================================================================
// 行为 11：strategy_get 默认不绑给任何 agent
// =====================================================================

// TestStrategyToolGetIsNotBoundToAnyAgentByDefault 钉住「strategy_get 默认不绑」。
//
// 计划 Task 6 Step 3 明写：strategy_get 加进 defaultUnbound（与 goal_met 同款），
// 需要人工 opt-in。理由是它把**跨任务知识**拉进上下文，风险高于检索摘要。
//
// 断言落在 BuiltinToolSeeds() 上 —— 那是 tools 表播种的唯一入口，
// 断言别的地方（比如某个内部变量）只会钉住一个影子。
//
// 同时断言 strategy_search **是**绑给 planner 的：否则「strategy_get 没绑」
// 可能只是「所有策略工具都没绑」，那条断言就没有信息量。
func TestStrategyToolGetIsNotBoundToAnyAgentByDefault(t *testing.T) {
	seeds := BuiltinToolSeeds()

	byKey := map[string]ToolSeed{}
	for _, s := range seeds {
		byKey[s.Key] = s
	}

	getSeed, ok := byKey["strategy_get"]
	if !ok {
		t.Fatalf("工具目录里没有 strategy_get")
	}
	if len(getSeed.Agents) != 0 {
		t.Fatalf("strategy_get 默认绑给了 %v —— 它把跨任务知识拉进上下文，必须人工 opt-in", getSeed.Agents)
	}

	searchSeed, ok := byKey["strategy_search"]
	if !ok {
		t.Fatalf("工具目录里没有 strategy_search")
	}
	if len(searchSeed.Agents) == 0 {
		t.Fatalf("strategy_search 一个 agent 都没绑（断言失去信息量）")
	}

	proposeSeed, ok := byKey["strategy_propose"]
	if !ok {
		t.Fatalf("工具目录里没有 strategy_propose")
	}
	if len(proposeSeed.Agents) == 0 {
		t.Fatalf("strategy_propose 一个 agent 都没绑（断言失去信息量）")
	}
}

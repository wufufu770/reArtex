// 本文件是策略库（L4）Task 6「宿主侧接线」层的端到端验收用例。
//
// 钉的是**快照与迁移原语**这两件事在真实库上的行为：
//   - 任务共享的策略版本快照：一个群 = 一个 task ⇒ 快照按 task_id 存 ⇒
//     组内成员天然用同一份、同一版本；快照是**冻结**的那一份，不是每轮重算；
//   - 快照只承载摘要：表里没有正文列，条目类型里没有 steps 字段；
//   - 自动退役是迁移原语：写 state / retired_reason / retired_at，
//     **保留**策略行与证据链，并且**拒绝**动被 pin 的策略。
//
// 【口径来源】断言以计划为准（/home/analyst/.omo/plans/
// 2026-10-29-task-hall-phase6-implementation.md 的 Task 6 Step 4 与
// Task 5 Step 4/5）。
package db

import (
	"encoding/json"
	"fmt"
	"strconv"
	"testing"
	"time"
)

// strategyE2ESuffix 给用例生成互不撞的标识后缀（纳秒级）。
//
// 为什么唯一：策略名受 uq_strategies_company_name 唯一索引约束，
// 重名会直接写不进去 ⇒ 用例之间互相踩。
func strategyE2ESuffix() int64 { return time.Now().UnixNano() }

// strategyE2ELetters 把一个整数映射成**纯字母**串（十进制数字逐位映射到 a..j）。
//
// 为什么策略名要用纯字母：任务的 goal/description 用**纯数字** ⇒ 策略名/摘要
// 与任务文本之间没有任何共同的字符二元组 ⇒ 字面路径（bigram 相似度）恒为 0
// ⇒ 检索结果只由**结构路径**（资产签名）决定。用例要断言的正是「快照按
// task_id 隔离」，若两条策略的措辞互像，字面路径就会把它们串在一起，
// 断言会变成在测措辞而不是测隔离。
func strategyE2ELetters(n int64) string {
	s := strconv.FormatInt(n, 10)
	b := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		b[i] = byte('a' + (s[i] - '0'))
	}
	return string(b)
}

// strategyE2ETask 造一个任务 + 一枚范围内资产（类别唯一）。
//
// 为什么要有范围内的资产：策略快照的候选集来自 db.AssetStore.TaskAssetSignature，
// 它只统计**范围内**资产的 category ⇒ 不声明范围，签名是空的，快照必然为空。
//
// 任务的 goal/description 用**纯数字**（见 strategyE2ELetters 的说明）。
func strategyE2ETask(t *testing.T, d *DB, token, category string) *Task {
	t.Helper()
	task, err := d.CreateTask(token, token, nil, 0, 0)
	if err != nil {
		t.Fatalf("建任务: %v", err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(task.ID) })

	var assetID int64
	if err := d.QueryRow(`INSERT INTO assets(type, domain, root_domain, category, last_seen)
		VALUES('subdomain',$1,$1,$2,now()) RETURNING id`,
		fmt.Sprintf("e2e-%d.example.test", strategyE2ESuffix()%1000000), category).Scan(&assetID); err != nil {
		t.Fatalf("插资产: %v", err)
	}
	t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM assets WHERE id=$1`, assetID) })
	if _, err := d.Exec(`INSERT INTO task_scope(task_id, kind, asset_id, source)
		VALUES($1,'asset',$2,'manual') ON CONFLICT DO NOTHING`, task.ID, assetID); err != nil {
		t.Fatalf("声明任务范围: %v", err)
	}
	return task
}

// strategyE2EStrategy 建一条在役策略（activation 命中给定类别）。
//
// 名字用纯字母 + 中文（无数字），保证与任务的纯数字文本没有共同二元组。
func strategyE2EStrategy(t *testing.T, d *DB, category string) int64 {
	t.Helper()
	name := "策略" + strategyE2ELetters(strategyE2ESuffix())
	id, err := d.CreateStrategy(&Strategy{
		Name:       name,
		Summary:    "对 $TARGET 的 $PATH 做一次探测",
		Steps:      json.RawMessage(`["curl -X POST https://$TARGET/$PATH"]`),
		Activation: json.RawMessage(fmt.Sprintf(`{"category":[%q]}`, category)),
	})
	if err != nil {
		t.Fatalf("建策略 %s: %v", name, err)
	}
	t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM strategies WHERE id=$1`, id) })
	if ok, err := d.PromoteStrategyToActive(id); err != nil || !ok {
		t.Fatalf("置为 active: %v", err)
	}
	return id
}

// TestStrategyE2ETaskSnapshotIsSharedAndFrozen 钉住「组内共享同一策略版本快照」。
//
// 三层断言：
//  1. 快照按 task_id 隔离：任务 A 的快照里只有 A 的策略，任务 B 的只有 B 的；
//  2. 同一个任务反复解析 ⇒ 逐条相同（策略 id 与版本都相同）—— 这就是
//     「组内成员用同一份、同一版本」在数据层的形状；
//  3. 策略后来升了版 ⇒ 快照**不动**（冻结的那一份）。每轮重算会让 planner
//     看到的策略集漂移，与「组内同版本」直接冲突（计划 Step 4 的明写要求）。
//
// 另外断言快照表**没有正文列** —— 注入路径拿不到 steps，这是结构性保证。
func TestStrategyE2ETaskSnapshotIsSharedAndFrozen(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer d.Close()

	sfx := strategyE2ESuffix()
	catA := fmt.Sprintf("e2e-cat-a-%d", sfx)
	catB := fmt.Sprintf("e2e-cat-b-%d", sfx)

	taskA := strategyE2ETask(t, d, strconv.FormatInt(sfx, 10), catA)
	taskB := strategyE2ETask(t, d, strconv.FormatInt(sfx+1, 10), catB)

	strategyA := strategyE2EStrategy(t, d, catA)
	strategyB := strategyE2EStrategy(t, d, catB)

	// ① 快照按 task_id 隔离。
	entriesA, err := d.EnsureTaskStrategySnapshot(taskA.ID)
	if err != nil {
		t.Fatalf("生成任务 A 的快照: %v", err)
	}
	if len(entriesA) == 0 {
		t.Fatalf("任务 A 的快照是空的：在役策略没被召回")
	}
	entriesB, err := d.EnsureTaskStrategySnapshot(taskB.ID)
	if err != nil {
		t.Fatalf("生成任务 B 的快照: %v", err)
	}
	if len(entriesB) == 0 {
		t.Fatalf("任务 B 的快照是空的：在役策略没被召回")
	}
	if !strategyE2EHas(entriesA, strategyA) {
		t.Fatalf("任务 A 的快照里没有自己的策略 %d：%+v", strategyA, entriesA)
	}
	if strategyE2EHas(entriesA, strategyB) {
		t.Fatalf("任务 A 的快照里混进了任务 B 的策略 %d：%+v", strategyB, entriesA)
	}
	if !strategyE2EHas(entriesB, strategyB) {
		t.Fatalf("任务 B 的快照里没有自己的策略 %d：%+v", strategyB, entriesB)
	}

	// ② 同一个任务反复解析 ⇒ 逐条相同（组内成员看到同一版本）。
	again, err := d.EnsureTaskStrategySnapshot(taskA.ID)
	if err != nil {
		t.Fatalf("再生成任务 A 的快照: %v", err)
	}
	if len(again) != len(entriesA) {
		t.Fatalf("快照条数变了：%d → %d", len(entriesA), len(again))
	}
	for i := range entriesA {
		if entriesA[i].StrategyID != again[i].StrategyID {
			t.Fatalf("快照里的策略变了：%d → %d", entriesA[i].StrategyID, again[i].StrategyID)
		}
		if entriesA[i].Version != again[i].Version {
			t.Fatalf("快照里的策略版本变了：%d → %d", entriesA[i].Version, again[i].Version)
		}
	}

	// ③ 策略升版 ⇒ 快照不动（冻结的那一份）。
	if _, err := d.Exec(`UPDATE strategies SET version=version+5 WHERE id=$1`, strategyA); err != nil {
		t.Fatalf("升策略版本: %v", err)
	}
	frozen, err := d.EnsureTaskStrategySnapshot(taskA.ID)
	if err != nil {
		t.Fatalf("再生成任务 A 的快照: %v", err)
	}
	for _, e := range frozen {
		if e.StrategyID != strategyA {
			continue
		}
		for _, orig := range entriesA {
			if orig.StrategyID == strategyA && e.Version != orig.Version {
				t.Fatalf("快照跟着策略升版了（%d → %d）—— 快照必须是冻结的那一份", orig.Version, e.Version)
			}
		}
	}

	// 快照表没有正文列：注入路径不可能把 steps 带进上下文。
	rows, err := d.Query(`SELECT column_name FROM information_schema.columns
		WHERE table_name='task_strategy_snapshot' ORDER BY column_name`)
	if err != nil {
		t.Fatalf("读快照表列: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var col string
		if err := rows.Scan(&col); err != nil {
			t.Fatalf("读快照表列: %v", err)
		}
		switch col {
		case "steps", "preconditions", "tool_requirements", "failure_modes", "parameters", "activation":
			t.Fatalf("快照表带了正文列 %q —— 注入路径会把正文带进上下文", col)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("读快照表列: %v", err)
	}
}

// strategyE2EHas 报告快照里有没有这条策略。
func strategyE2EHas(entries []StrategySnapshotEntry, strategyID int64) bool {
	for _, e := range entries {
		if e.StrategyID == strategyID {
			return true
		}
	}
	return false
}

// TestStrategyE2EAutoRetireKeepsRowsAndRefusesPinned 钉住「自动退役是迁移原语」。
//
// 退役后：state='retired'、retired_reason 非空、retired_at 有值、
// 策略行还在、证据链一条不少。
//
// 另外钉住条件更新的守卫：**被 pin 的策略不参与任何自动淘汰** ——
// 这一条写在 WHERE 子句里（`AND pinned=false`），是并发下的硬保证。
func TestStrategyE2EAutoRetireKeepsRowsAndRefusesPinned(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer d.Close()

	sfx := strategyE2ESuffix()
	category := fmt.Sprintf("e2e-cat-retire-%d", sfx)

	task := strategyE2ETask(t, d, strconv.FormatInt(sfx, 10), category)
	if task.ID <= 0 {
		t.Fatalf("建任务失败")
	}
	id := strategyE2EStrategy(t, d, category)

	// 一条真实存在的漏洞证据（引用完整性由写入路径校验 ⇒ 必须先有这条漏洞）。
	findingID, err := d.AddFinding(0, 0, "sqli", "e2e 退役漏洞", "high", "摘要", "证据", "worker", nil)
	if err != nil {
		t.Fatalf("建 finding: %v", err)
	}
	t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM findings WHERE id=$1`, findingID) })
	if err := d.LinkStrategyEvidence(id, "finding", fmt.Sprintf("%d", findingID), "validated_by", "sha256:e2e"); err != nil {
		t.Fatalf("挂证据: %v", err)
	}

	// 被 pin 的策略：自动退役必须**拒绝**。
	if ok, err := d.ApplyStrategyPin(id, true); err != nil || !ok {
		t.Fatalf("置 pin: %v", err)
	}
	ok, err := d.AutoRetireStrategy(id, "e2e 退役理由")
	if err != nil {
		t.Fatalf("自动退役: %v", err)
	}
	if ok {
		t.Fatalf("被 pin 的策略被自动退役了 —— 唯一证人不参与任何自动淘汰")
	}
	if s, err := d.GetStrategy(id); err != nil || s == nil || s.State != StrategyActive {
		t.Fatalf("被 pin 的策略状态不该变：%+v（err=%v）", s, err)
	}

	// 解 pin 后退役成功，且行与证据都留着。
	if ok, err := d.ApplyStrategyPin(id, false); err != nil || !ok {
		t.Fatalf("解 pin: %v", err)
	}
	ok, err = d.AutoRetireStrategy(id, "e2e 退役理由")
	if err != nil {
		t.Fatalf("自动退役: %v", err)
	}
	if !ok {
		t.Fatalf("解 pin 后自动退役没有生效")
	}
	s, err := d.GetStrategy(id)
	if err != nil || s == nil {
		t.Fatalf("读策略: %v", err)
	}
	if s.State != StrategyRetired {
		t.Fatalf("状态 = %s，期望 %s", s.State, StrategyRetired)
	}
	if s.RetiredReason == "" {
		t.Fatalf("退役必须留下 retired_reason")
	}
	if s.RetiredAt == nil {
		t.Fatalf("退役必须留下 retired_at")
	}
	links, err := d.StrategyEvidence(id)
	if err != nil {
		t.Fatalf("读证据链: %v", err)
	}
	if len(links) == 0 {
		t.Fatalf("退役后证据链被清空了 —— 退役 ≠ 删除")
	}
}

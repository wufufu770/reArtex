package db

import (
	"strings"
	"testing"
)

// 记忆晋级门 + 两段式披露（X4，方案 v10 §6 T2）的验收用例。
//
// 钉住的承诺：
//  1. **agent 写入即隔离**：AppendMemoryEntryChecked 落 state='quarantined'，
//     不进注入读面（导出 / OnlyActive 列表）；
//  2. **host-only 晋级走 Supersede 双时态通道**：PromoteMemoryEntry 追加
//     active 新版本并闭合隔离行（内容逐字段继承、无就地翻转）；
//  3. **退役不删内容**：RetireMemoryEntryHost 闭合区间，历史仍可查；
//  4. **治理面按 state 分页**：ListMemoryEntriesByState 的过滤与总数。

func TestMemoryAgentWriteQuarantinedAndPromote(t *testing.T) {
	d := memoryRetrievalOpen(t)
	scope := memoryTestScope(t)

	e, err := d.AppendMemoryEntryChecked(scope, MemoryEntryInput{
		TopicKey:    "agent-findings",
		Title:       "agent 沉淀的结论",
		SummaryLine: "隔离态：模型写入即隔离",
		WrittenBy:   "agent",
	})
	if err != nil {
		t.Fatalf("agent 写入失败: %v", err)
	}
	if e.State != MemoryStateQuarantined {
		t.Fatalf("agent 写入落了 %q，期望 quarantined", e.State)
	}

	// 隔离条目不进披露读面。
	out, err := d.ExportMemoryIndex(scope)
	if err != nil {
		t.Fatalf("导出失败: %v", err)
	}
	if strings.Contains(out, "agent-findings") {
		t.Error("隔离态条目不该出现在 agent 导出里（两段式披露）")
	}
	onlyActive, err := d.ListMemoryEntries(scope, MemoryListOptions{OnlyActive: true})
	if err != nil {
		t.Fatalf("列 active 失败: %v", err)
	}
	if len(onlyActive) != 0 {
		t.Errorf("active 列表 = %d 条，期望 0", len(onlyActive))
	}

	// host-only 晋级：走 Supersede 通道。
	promoted, err := d.PromoteMemoryEntry(e.ID, "governance")
	if err != nil {
		t.Fatalf("晋级失败: %v", err)
	}
	if promoted.State != MemoryStateActive {
		t.Errorf("晋级后的新版本态 = %q，期望 active", promoted.State)
	}
	if promoted.Version != e.Version+1 || promoted.SupersedesID == nil || *promoted.SupersedesID != e.ID {
		t.Errorf("晋级必须经 Supersede 追加新版本：v%d supersedes=%v", promoted.Version, promoted.SupersedesID)
	}
	if promoted.Title != e.Title || promoted.SummaryLine != e.SummaryLine {
		t.Error("晋级的新版本必须逐字段继承内容")
	}

	// 晋级后进披露读面；旧行闭合（valid_to 非空、可查历史）。
	out, err = d.ExportMemoryIndex(scope)
	if err != nil {
		t.Fatalf("导出失败: %v", err)
	}
	if !strings.Contains(out, "agent-findings") {
		t.Error("晋级后应出现在 agent 导出里")
	}
	old, err := d.GetMemoryEntry(e.ID)
	if err != nil {
		t.Fatalf("读旧行失败: %v", err)
	}
	if old.ValidTo == nil || old.State != MemoryStateQuarantined {
		t.Errorf("旧行应被闭合且保持 quarantined 历史：valid_to=%v state=%s", old.ValidTo, old.State)
	}

	// 幂等：再晋级一次不产生新版本。
	again, err := d.PromoteMemoryEntry(promoted.ID, "governance")
	if err != nil {
		t.Fatalf("重复晋级失败: %v", err)
	}
	if again.ID != promoted.ID {
		t.Errorf("重复晋级产生了新行 #%d（幂等应返回原行 #%d）", again.ID, promoted.ID)
	}
}

func TestMemoryRetireHostAndListByState(t *testing.T) {
	d := memoryRetrievalOpen(t)
	scope := memoryTestScope(t)

	a, err := d.AppendMemoryEntryActive(scope, MemoryEntryInput{
		TopicKey: "host-note", Title: "host 通道直写 active", WrittenBy: "host",
	})
	if err != nil {
		t.Fatalf("host 写入失败: %v", err)
	}
	if a.State != MemoryStateActive {
		t.Fatalf("host 通道落了 %q，期望 active", a.State)
	}
	q, err := d.AppendMemoryEntryChecked(scope, MemoryEntryInput{
		TopicKey: "agent-note", Title: "隔离中的另一条", WrittenBy: "agent",
	})
	if err != nil {
		t.Fatalf("agent 写入失败: %v", err)
	}

	// 治理面分页：按 state 过滤 + 总数。
	_, totalActive, err := d.ListMemoryEntriesByState(MemoryStateActive, 50, 0)
	if err != nil {
		t.Fatalf("治理列表失败: %v", err)
	}
	if totalActive < 1 {
		t.Errorf("active 总数 = %d，至少应为 1", totalActive)
	}
	quarantined, totalQ, err := d.ListMemoryEntriesByState(MemoryStateQuarantined, 50, 0)
	if err != nil {
		t.Fatalf("治理列表失败: %v", err)
	}
	if totalQ < 1 {
		t.Errorf("quarantined 总数 = %d，至少应为 1", totalQ)
	}
	foundQ := false
	for _, e := range quarantined {
		if e.ID == q.ID {
			foundQ = true
		}
	}
	if !foundQ {
		t.Error("治理列表没列出刚写入的隔离条目")
	}
	if _, _, err := d.ListMemoryEntriesByState("bogus", 50, 0); err == nil {
		t.Error("拼错的 state 过滤词必须报错（fail-closed），不能当「全部」")
	}

	// host-only 按 id 退役：闭合区间，不删内容。
	if err := d.RetireMemoryEntryHost(a.ID, "governance"); err != nil {
		t.Fatalf("host 退役失败: %v", err)
	}
	retired, err := d.GetMemoryEntry(a.ID)
	if err != nil {
		t.Fatalf("读退役行失败: %v", err)
	}
	if retired.ValidTo == nil {
		t.Error("退役必须闭合有效区间")
	}
	_, totalActive2, err := d.ListMemoryEntriesByState(MemoryStateActive, 50, 0)
	if err != nil {
		t.Fatalf("治理列表失败: %v", err)
	}
	if totalActive2 != totalActive-1 {
		t.Errorf("退役后 active 总数 %d，期望 %d-1", totalActive2, totalActive)
	}
	// 已退役的行再退役一次 ⇒ 可区分错误（不是 NotFound）。
	if err := d.RetireMemoryEntryHost(a.ID, "governance"); err == nil {
		t.Error("已退役条目再退役应报错")
	}
}

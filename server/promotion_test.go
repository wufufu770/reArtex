package server

import (
	"testing"

	"github.com/Autumn-27/artex/db"
)

// 三门晋级（X2，方案 v10 §6 T3）的验收用例。
//
// 验收清单（计划原文）：
//   - wins=1 不晋级；
//   - wins=2 撞证伪不晋级；
//   - 三门纯函数可独立测。
//
// 门1/门2/门3 都是纯函数，直接测；EvaluateMemoryPromotion 的读库编排用
// 派生库（trustTestDB 同款；无库配置跳过）。

func TestPromotionGateStructure(t *testing.T) {
	if promotionGateStructure("标题", "摘要") != true {
		t.Error("title+summary 非空应过门1")
	}
	if promotionGateStructure("", "摘要") {
		t.Error("title 为空不应过门1")
	}
	if promotionGateStructure("标题", "  ") {
		t.Error("summary 空白不应过门1")
	}
}

func TestPromotionGateReplay(t *testing.T) {
	// 撞证伪池：与某条 refuted 结论共享 ≥2 词元 ⇒ 门2 不过。
	refuted := []string{"目标站点 nginx 反代开启了 ModSecurity 拦截，sqlmap 无法注入"}
	if promotionGateReplay("nginx 反代 ModSecurity 拦截的结论", refuted) {
		t.Error("与证伪结论同族（≥2 共词）应判撞")
	}
	// 只共享 1 个词元：噪声，不算撞。
	if !promotionGateReplay("nginx 的探测路径", refuted) {
		t.Error("单词元重叠是噪声，不应判撞")
	}
	// 零重叠。
	if !promotionGateReplay("完全无关的另一个结论", refuted) {
		t.Error("零重叠不应判撞")
	}
	// 空证伪池 ⇒ 不撞。
	if !promotionGateReplay("任何文本", nil) {
		t.Error("空证伪池不应判撞")
	}
}

func TestPromotionGateEvidenceWinsThresholds(t *testing.T) {
	// 验收：wins=1 不晋级。
	if promotionGateEvidence(1, true) {
		t.Error("wins=1 不该过门3")
	}
	// wins=2 且不撞证伪 ⇒ 过。
	if !promotionGateEvidence(2, true) {
		t.Error("wins=2 且门2 过应过门3")
	}
	// 验收：wins=2 撞证伪不晋级。
	if promotionGateEvidence(2, false) {
		t.Error("wins=2 且撞证伪不该过门3")
	}
	// wins≥3 强门：与门2 结果无关。
	if !promotionGateEvidence(3, false) {
		t.Error("wins=3 强门应直接过")
	}
	if !promotionGateEvidence(10, false) {
		t.Error("wins=10 强门应直接过")
	}
}

// promotionGateTestDB 打开派生测试库并应用本用例需要的全部 schema。
func promotionGateTestDB(t *testing.T) *db.DB {
	t.Helper()
	pg := trustTestDB(t)
	for name, fn := range map[string]func() error{
		"memory":           pg.EnsureMemorySchema,
		"legacy":           pg.EnsureLegacySchema,
		"experience-width": pg.EnsureExperienceWeightSchema,
	} {
		if err := fn(); err != nil {
			t.Fatalf("应用 %s schema 失败: %v", name, err)
		}
	}
	return pg
}

func TestEvaluateMemoryPromotionEndToEnd(t *testing.T) {
	pg := promotionGateTestDB(t)
	scope := db.MemoryScope{Type: db.MemoryScopeTaskHall, ID: 7770001}

	// 隔离态条目（agent 写路径语义）。
	e, err := pg.AppendMemoryEntryChecked(scope, db.MemoryEntryInput{
		TopicKey:    "promotion-e2e",
		Title:       "结构完整的一条记忆",
		SummaryLine: "摘要非空，与其他证伪结论无重叠",
		WrittenBy:   "agent",
	})
	if err != nil {
		t.Fatalf("写入失败: %v", err)
	}

	// wins=0 ⇒ 缺实战证据门。
	pass, missing := EvaluateMemoryPromotion(pg, e.ID)
	if pass {
		t.Fatal("wins=0 不该过三门")
	}
	if len(missing) == 0 {
		t.Fatal("必须给出「缺哪门」清单")
	}
	if !containsGate(missing, PromotionGateEvidence) {
		t.Errorf("缺口清单应含实战证据门：%v", missing)
	}
	if containsGate(missing, PromotionGateStructure) {
		t.Errorf("结构完整不该在缺口清单里：%v", missing)
	}

	// 补齐 wins 到弱门（2 次）⇒ 三门全过。
	key := db.ExperiencePatternKeyForMemory(e.ID)
	for i := 0; i < 2; i++ {
		if _, _, err := pg.RecordExperienceOutcome(key,
			"used_experience:"+key+" 该记忆的主题词在本结论里被引用 promotion-e2e",
			"结论：promotion-e2e 主题的验证路径成立"); err != nil {
			t.Fatalf("记账失败: %v", err)
		}
	}
	pass, missing = EvaluateMemoryPromotion(pg, e.ID)
	if !pass {
		t.Fatalf("wins=2 且结构/复盘过 ⇒ 应过三门，缺口：%v", missing)
	}
	if len(missing) != 0 {
		t.Errorf("全过时缺口清单应为空：%v", missing)
	}

	// 结构残缺的条目缺门1。
	bad, err := pg.AppendMemoryEntryChecked(scope, db.MemoryEntryInput{
		TopicKey: "promotion-e2e-bad", Title: "只有标题", WrittenBy: "agent",
	})
	if err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	pass, missing = EvaluateMemoryPromotion(pg, bad.ID)
	if pass || !containsGate(missing, PromotionGateStructure) {
		t.Errorf("summary 为空应缺结构门：pass=%v missing=%v", pass, missing)
	}
}

func containsGate(list []string, gate string) bool {
	for _, g := range list {
		if len(g) >= len(gate) && g[:len(gate)] == gate {
			return true
		}
	}
	return false
}

// 三门晋级（X2，方案 v10 §6 第三期 T3）：结构完整 / 历史复盘 / 实战证据。
//
// 【裁决写只在宿主侧发生，LLM 一行都不参与】—— 与策略线
// （server/strategy_curator.go 的 EvaluatePromotion）同一形状、同一纪律：
//   - 三个判据全部是「非空检查 + 关系查询 + 数值阈值」，入参只有 (*db.DB, id)
//     ⇒ 结构上拿不到模型、provider 的任何句柄；
//   - **无 --force**（§5.2 X2）：任何门都不可跳过 —— 证据不足的晋级等于把
//     隔离态的兜底拆掉。
//
// 两条线接入，一条线不动：
//   - 记忆线：POST /api/memory/entries/{id}/promote（server/memory_api.go）；
//   - 经验线：POST /api/legacy/nodes/{id}/review 的 accepted 转移
//     （server/legacy_api.go）；
//   - 策略线 EvaluatePromotion 保持原样（那条线已完整，回归保护）。
//
// ⚠️ 过门返回 409 + 「缺哪门」的确定性清单（不猜、不给含糊理由）。
package server

import (
	"strconv"
	"strings"

	"github.com/Autumn-27/artex/db"
)

// 三门阈值（§8 红线第 3 条：阈值只进代码常量，不进可热改的 DB/settings）。
const (
	// PromotionWinsStrong 是实战证据的强门：wins≥3 直接过。
	PromotionWinsStrong = 3
	// PromotionWinsWeak 是实战证据的弱门：wins≥2 且不撞证伪池。
	PromotionWinsWeak = 2
	// PromotionRefutedOverlapMin 是「撞证伪池」的词集交集阈值：与单条 refuted
	// fact 的结论共享 ≥2 个词元算撞（1 个 CJK 单字的重叠是噪声，不算撞）。
	PromotionRefutedOverlapMin = 2
	// PromotionRefutedPoolScan 是证伪池的扫描条数（近 N 条 refuted fact）。
	PromotionRefutedPoolScan = 200
)

// Gate 名称（写进缺口清单，验收用「缺哪门」的口径）。
const (
	PromotionGateStructure = "结构完整"
	PromotionGateReplay    = "历史复盘"
	PromotionGateEvidence  = "实战证据"
)

// ---------- 三道门（纯函数，可独立测 —— T3 验收第三条） ----------

// promotionGateStructure 门1：结构完整（name/summary 非空 —— 记忆线是
// title/summary_line，经验线是 title/summary）。
func promotionGateStructure(title, summary string) bool {
	return strings.TrimSpace(title) != "" && strings.TrimSpace(summary) != ""
}

// promotionGateReplay 门2：历史复盘 —— 不撞 fact refuted 池。
// 候选文本与任何一条 refuted 结论的词集交集 ≥ PromotionRefutedOverlapMin
// ⇒ 撞（这条经验/记忆与已被证伪的结论同族）⇒ 门不过。
// 词集用 db 包的统一分词器（db.TokenizeForMatch 同一切词口径，不另立一份）。
func promotionGateReplay(candidateText string, refutedSummaries []string) bool {
	words := db.TokenizeForMatch(candidateText)
	if len(words) == 0 {
		return true // 候选没词（空文本）——门2 不背结构门的锅，放行由门1 拦
	}
	for _, ref := range refutedSummaries {
		refWords := db.TokenizeForMatch(ref)
		if len(refWords) == 0 {
			continue
		}
		shared := 0
		for w := range words {
			if refWords[w] {
				shared++
				if shared >= PromotionRefutedOverlapMin {
					return false
				}
			}
		}
	}
	return true
}

// promotionGateEvidence 门3：实战证据 —— wins≥3 直接过；wins≥2 且过了门2
// （不撞证伪）也过；否则不过。
func promotionGateEvidence(wins int64, replayPassed bool) bool {
	if wins >= PromotionWinsStrong {
		return true
	}
	return wins >= PromotionWinsWeak && replayPassed
}

// ---------- 评估入口（读库编排；读失败 fail-closed） ----------

// EvaluateMemoryPromotion 记忆线的三门评估（X2）。
//
// 门1：条目 title/summary_line 非空；
// 门2：条目文本（title+summary）不撞 fact refuted 池；
// 门3：权重卡 wins（pattern_key=memory_<id>）≥3，或 ≥2 且过了门2。
//
// 返回 (三门全过, 缺哪门的清单)。读库失败 fail-closed：不算过，缺口清单带
// 内部原因（不猜数字、不静默放行）。
func EvaluateMemoryPromotion(pg *db.DB, id int64) (bool, []string) {
	return evaluatePromotion(pg, "memory", id)
}

// EvaluateExperiencePromotion 经验线的三门评估（legacy_nodes 经验线）。
//
// 门1：经验 title/summary 非空；
// 门2：经验文本不撞 fact refuted 池；
// 门3：权重卡 wins（pattern_key=legacy_<id>）≥3，或 ≥2 且过了门2。
func EvaluateExperiencePromotion(pg *db.DB, id int64) (bool, []string) {
	return evaluatePromotion(pg, "legacy", id)
}

// evaluatePromotion 是两线共用的三门编排（形状复用 EvaluatePromotion：判定
// 与写入同源，判定结果决定能不能动状态）。
func evaluatePromotion(pg *db.DB, line string, id int64) (bool, []string) {
	if pg == nil {
		return false, []string{"内部错误：数据库未连接"}
	}
	if id <= 0 {
		return false, []string{"内部错误：id 不合法"}
	}

	// 候选正文 + 结构门。
	var title, summary string
	switch line {
	case "memory":
		e, err := pg.GetMemoryEntry(id)
		if err != nil {
			return false, []string{"内部错误：读记忆条目失败（" + err.Error() + "）"}
		}
		title, summary = e.Title, e.SummaryLine
	case "legacy":
		n, err := pg.GetLegacyNode(id)
		if err != nil {
			return false, []string{"内部错误：读经验节点失败（" + err.Error() + "）"}
		}
		title, summary = n.Title, n.Summary
	default:
		return false, []string{"内部错误：未知晋级线 " + line}
	}

	missing := []string{}
	structureOK := promotionGateStructure(title, summary)
	if !structureOK {
		missing = append(missing, PromotionGateStructure+"：标题与摘要必须非空")
	}

	// 门2：证伪池。
	refuted, err := pg.RefutedFactSummaries(PromotionRefutedPoolScan)
	if err != nil {
		return false, append(missing, "内部错误：读 fact 证伪池失败（"+err.Error()+"）")
	}
	candidateText := title + " " + summary
	replayOK := promotionGateReplay(candidateText, refuted)
	if !replayOK {
		missing = append(missing, PromotionGateReplay+"：与 fact 证伪池的结论同族（词集交集 ≥"+strconv.Itoa(PromotionRefutedOverlapMin)+"），疑似已被证伪的旧结论")
	}

	// 门3：实战证据（experience_weight.wins）。
	var patternKey string
	if line == "memory" {
		patternKey = db.ExperiencePatternKeyForMemory(id)
	} else {
		patternKey = db.ExperiencePatternKeyForLegacy(id)
	}
	stats, _, err := pg.ExperienceWeightStats(patternKey)
	if err != nil {
		return false, append(missing, "内部错误：读经验权重卡失败（"+err.Error()+"）")
	}
	if !promotionGateEvidence(stats.Wins, replayOK) {
		missing = append(missing, PromotionGateEvidence+"：实战 wins="+strconv.FormatInt(stats.Wins, 10)+
			"（需 ≥"+strconv.Itoa(PromotionWinsStrong)+"，或 ≥"+strconv.Itoa(PromotionWinsWeak)+" 且不撞证伪池）")
	}

	return len(missing) == 0, missing
}

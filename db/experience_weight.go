// 经验权重卡（X1，方案 v10 §6 第三期 T1）—— legacy/memory 两条经验线的
// 「打法先验」账本：Laplace 平滑 prior + 每日计胜 cap + 归因串词集因果校验。
//
// 设计纪律（方案 v10 §5.2 X1 / §5.1 第 3 条）：
//
//	① **单一权威实现防 E6 双字面量**：prior 的公式在本文件只写一份
//	  （experienceLaplacePrior）。读面回落值、写入快照、测试锚点全部经由它，
//	  任何地方都不允许出现第二份 "(w+1)/(h+3)" 形状的算式。
//	② **wins/hits 只增**：记账事件只做累加，没有减法入口（§5.2 X1）。
//	③ **因果校验是 win 的必要条件**：worker 归因串 used_experience:<pattern_key>
//	   必须与结论词集交集 ≥1 才算 win；无交集只记 hit（§6 T1 验收第四例）。
//	   归因串自己携带的样板词（used_experience / pattern_key 本身）不算交集 ——
//	   它们天然出现在结论文本里，拿它们当交集等于没有校验。
//	④ **每日计胜 cap**（ExperienceDailyWinCap=3）：day_key=当日，同日第 4 次
//	   只记命中不计胜（§6 T1 验收第三例）；跨日窗口自动重置。
//	⑤ **经验权重是提示排序信号，不是安全边界**：读失败/表不存在只影响注入
//	   排序质量，不参与任何放行/拒绝判定（与记忆层同款纪律）。
package db

import (
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed schema_experience_weight.sql
var experienceWeightSchemaSQL string

// experienceWeightSchemaReady 记录某个连接已经应用过经验权重 DDL
// （与 db/memory.go 的 memorySchemaReady 同款 sync.Map 模式）。
var experienceWeightSchemaReady sync.Map

// EnsureExperienceWeightSchema 应用经验权重卡的幂等 DDL。
func (d *DB) EnsureExperienceWeightSchema() error {
	if _, ok := experienceWeightSchemaReady.Load(d.DB); ok {
		return nil
	}
	if _, err := d.Exec(experienceWeightSchemaSQL); err != nil {
		return fmt.Errorf("应用经验权重 schema 失败: %w", err)
	}
	experienceWeightSchemaReady.Store(d.DB, struct{}{})
	return nil
}

// ensureExperienceWeightSchema 是访问层的幂等入口。
func (d *DB) ensureExperienceWeightSchema() error { return d.EnsureExperienceWeightSchema() }

// ---------- 权重卡常量（阈值只进代码常量，§8 红线第 3 条） ----------

const (
	// ExperienceDailyWinCap 是每日计胜上限：同一 pattern 当日第 4 次起只记 hit。
	ExperienceDailyWinCap = 3

	// ExperienceAttributionMarker 是归因串前缀：worker 在结论里写
	// used_experience:<pattern_key> 声明「本结论引用了这条经验」。
	ExperienceAttributionMarker = "used_experience:"

	// experiencePriorAlpha / experiencePriorBeta 是 Laplace 平滑的两个参数。
	// 只在本文件出现，且只被 experienceLaplacePrior 引用（单一实现）。
	// 口径 = 权威公式 (wins_old+dw+1)/(hits_old+3)：alpha=1，alpha+beta=3 ⇒ beta=2
	//（fresh 行 (0,0) 代入 = 1/3；首胜 = 2/3。E6 防双字面量：测试锚点与本式对账）。
	experiencePriorAlpha = 1
	experiencePriorBeta  = 2
)

// experienceAttributionKeyRe 约束 pattern_key 的形状：归因串解析依赖它的
// 单 token 性（used_experience:<pattern_key> 里不能出现空白）。
var experienceAttributionKeyRe = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)

// experienceLaplacePrior 是 prior 的**唯一**公式实现（防 E6 双字面量）。
//
//	prior = (wins_old + dw + 1) / (hits_old + 3)    dw ∈ {0,1}
//
// dw=1（本事件计胜）⇒ prior 上调；dw=0（未命中）⇒ prior 回落 —— 同一公式
// 两个方向，不存在第二份算式。wins_old/hits_old 用**事件前**的值。
func experienceLaplacePrior(winsOld, hitsOld int64, dw int64) float64 {
	return (float64(winsOld) + float64(dw) + float64(experiencePriorAlpha)) /
		(float64(hitsOld) + float64(experiencePriorAlpha+experiencePriorBeta))
}

// ExperienceDefaultPrior 是「没有权重卡」时的回落先验：fresh 行 (0,0) 代入
// 同一公式（未命中方向），不另立字面量。
func ExperienceDefaultPrior() float64 {
	return experienceLaplacePrior(0, 0, 0)
}

// ExperienceWeight 是一张权重卡的当前状态。
type ExperienceWeight struct {
	PatternKey string
	Hits       int64
	Wins       int64
	Prior      float64
	DayKey     string
	DayWins    int
	LastWinAt  *time.Time
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// ExperiencePatternKeyForLegacy / ExperiencePatternKeyForMemory 是两条线的
// pattern_key 约定（legacy_nodes 经验线 / memory_index 记忆线）。本层不解释
// 键的语义，只提供统一的拼装口径。
func ExperiencePatternKeyForLegacy(experienceID int64) string {
	return "legacy_" + strconv.FormatInt(experienceID, 10)
}

func ExperiencePatternKeyForMemory(entryID int64) string {
	return "memory_" + strconv.FormatInt(entryID, 10)
}

// ---------- 归因串：提取 + 词集因果校验（单一实现） ----------

// experienceAttributionRe 从结论文本里抠出 used_experience:<pattern_key>。
// pattern_key 限 [A-Za-z0-9_.-]（experienceAttributionKeyRe 的子集），冒号
// 结尾后非空即收 —— 同一段结论可以归因多条经验。
var experienceAttributionRe = regexp.MustCompile(`used_experience:([A-Za-z0-9_.-]+)`)

// ExtractExperienceAttributions 提取结论里归因串引用的全部 pattern_key
// （去重、保序）。没有归因串 ⇒ 空切片（调用方无需记账）。
func ExtractExperienceAttributions(conclusion string) []string {
	matches := experienceAttributionRe.FindAllStringSubmatch(conclusion, -1)
	out := make([]string, 0, len(matches))
	seen := make(map[string]bool, len(matches))
	for _, m := range matches {
		key := m[1]
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, key)
	}
	return out
}

// ValidateExperienceAttribution 是词集因果校验的**唯一**实现。
//
// 输入的三段：
//   - patternKey：被引用经验的键；
//   - attribution：worker 归因陈述 —— 结论里**含归因串标记的那一行**（或调用方
//     给的等价引用段）；
//   - conclusion：worker 写回的结论文本（不含归因行也可以——交集是对「归因行
//     剩余的实质词」与「结论」做的）。
//
// 判定：把归因行里的样板词（used_experience 标记与 pattern_key 本身）剔除后，
// 剩余实质词与结论词集交集 ≥1 才算 win。理由：光秃秃的
// 「used_experience:legacy_42」不带任何实质内容，与结论的交集只可能来自
// 样板词本身 —— 那样的归因是空引用，不算胜（无交集只记 hit）。
func ValidateExperienceAttribution(patternKey, attribution, conclusion string) bool {
	if strings.TrimSpace(attribution) == "" || strings.TrimSpace(conclusion) == "" {
		return false
	}
	if patternKey == "" || !experienceAttributionKeyRe.MatchString(patternKey) {
		return false
	}
	// 剔除样板词：标记前缀、pattern_key 本身（连同 used_experience: 前缀整段）。
	stripped := strings.ReplaceAll(attribution, ExperienceAttributionMarker+patternKey, "")
	stripped = strings.ReplaceAll(stripped, ExperienceAttributionMarker, "")
	stripped = strings.ReplaceAll(stripped, patternKey, "")
	stripped = strings.ReplaceAll(stripped, "used_experience", "")
	strippedWords := TokenizeForMatch(stripped)
	if len(strippedWords) == 0 {
		return false // 光标记无实质词 ⇒ 无交集 ⇒ 不是 win
	}
	conclusionWords := TokenizeForMatch(conclusion)
	for w := range strippedWords {
		if conclusionWords[w] {
			return true
		}
	}
	return false
}

// ---------- 记账 ----------

// RecordExperienceOutcome 记一次使用事件（权重卡的**唯一**记账入口）。
//
// 事件语义：
//   - hits 只增 1（无论是否计胜）；
//   - win 条件（全部满足才 dw=1）：
//     1. 词集因果校验通过（ValidateExperienceAttribution）；
//     2. 当日计胜未达 ExperienceDailyWinCap（day_key=当日；跨日自动重置窗口）；
//   - prior 用事件前的 (wins_old, hits_old) 代入唯一公式重算；
//   - 计胜时顺带刷新 last_win_at。
//
// attribution 为空（例如注入侧的采纳留痕只记 hit）⇒ 直接按未命中方向记账。
// 返回 (本事件是否计胜, 事件后的 prior)。
func (d *DB) RecordExperienceOutcome(patternKey, attribution, conclusion string) (bool, float64, error) {
	if err := d.ensureExperienceWeightSchema(); err != nil {
		return false, 0, err
	}
	if !experienceAttributionKeyRe.MatchString(patternKey) {
		return false, 0, fmt.Errorf("pattern_key %q 不合法（限 ≤128 字的字母/数字/._-）", patternKey)
	}
	win := ValidateExperienceAttribution(patternKey, attribution, conclusion)
	counted, prior, err := d.recordExperienceOutcome(patternKey, win, time.Now())
	if err != nil {
		return false, 0, err
	}
	return counted, prior, nil
}

// RecordExperienceHit 记一次「命中」事件（只记 hit 不计胜的便捷入口）。
// 注入侧采纳留痕（server/legacy.go）用它：注入时还没有 worker 结论，
// 因果校验无从谈起 ⇒ 只记 hit（prior 按未命中方向回落）。
func (d *DB) RecordExperienceHit(patternKey string) (float64, error) {
	if err := d.ensureExperienceWeightSchema(); err != nil {
		return 0, err
	}
	if !experienceAttributionKeyRe.MatchString(patternKey) {
		return 0, fmt.Errorf("pattern_key %q 不合法（限 ≤128 字的字母/数字/._-）", patternKey)
	}
	_, prior, err := d.recordExperienceOutcome(patternKey, false, time.Now())
	return prior, err
}

// recordExperienceOutcome 是记账的事务体（win 已由调用方判好 —— 因果校验
// 是纯函数，不该藏在事务里）。返回本事件**实际**是否计胜（达 cap 的 win
// 按 hit 入账 ⇒ 返回 false）。
func (d *DB) recordExperienceOutcome(patternKey string, win bool, now time.Time) (bool, float64, error) {
	tx, err := d.Begin()
	if err != nil {
		return false, 0, fmt.Errorf("开启经验记账事务失败: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	// 读事件前状态（无行 ⇒ 全零；wins/hits 从零只增）。
	var winsOld, hitsOld int64
	var dayKey string
	var dayWins int
	err = tx.QueryRow(`SELECT wins, hits, day_key, day_wins FROM experience_weight
 WHERE pattern_key=$1 FOR UPDATE`, patternKey).Scan(&winsOld, &hitsOld, &dayKey, &dayWins)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		winsOld, hitsOld, dayKey, dayWins = 0, 0, "", 0
	case err != nil:
		return false, 0, fmt.Errorf("读经验权重卡失败: %w", err)
	}

	// 每日计胜窗口：day_key=当日，跨日自动重置（§6 T1：day_key=当日，超限只记
	// 命中不计胜）。
	today := now.Format("2006-01-02")
	if dayKey != today {
		dayKey, dayWins = today, 0
	}
	// 达 cap 的 win 只记命中不计胜（dw=0，按未命中方向入账）。
	dw := int64(0)
	if win && dayWins < ExperienceDailyWinCap {
		dw = 1
		dayWins++
	}

	// prior 用事件前的 (wins_old, hits_old) 代入唯一公式 —— 单一实现，
	// 这里不出现第二份算式。
	prior := experienceLaplacePrior(winsOld, hitsOld, dw)

	newWins := winsOld + dw
	newHits := hitsOld + 1
	// 计胜 ⇒ last_win_at 刷新为本次事件；未计胜 ⇒ last_win_at 保持原值
	//（INSERT 侧给 NULL/now 由 ON CONFLICT 分支是否携带 last_win_at 决定：
	// DO UPDATE 没列出的列自动保留旧行值）。
	var lastWinArg any
	lastWinSet := ""
	if dw == 1 {
		lastWinArg = now
		lastWinSet = ", last_win_at=$7"
	} else {
		lastWinArg = nil
	}
	_, err = tx.Exec(`INSERT INTO experience_weight
 (pattern_key, hits, wins, prior, day_key, day_wins, last_win_at)
 VALUES ($1,$2,$3,$4,$5,$6,$7)
 ON CONFLICT (pattern_key) DO UPDATE SET
   hits=$2, wins=$3, prior=$4, day_key=$5, day_wins=$6`+lastWinSet+`, updated_at=now()`,
		patternKey, newHits, newWins, prior, dayKey, dayWins, lastWinArg)
	if err != nil {
		return false, 0, fmt.Errorf("写经验权重卡失败: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, 0, fmt.Errorf("提交经验记账事务失败: %w", err)
	}
	return dw == 1, prior, nil
}

// ExperienceWeightStats 读一张权重卡（不存在 ⇒ 回落：全零计数 + 默认 prior）。
// 第二个返回值 exists=false 表示卡还没建（fresh 状态，prior 为公式回落值）。
func (d *DB) ExperienceWeightStats(patternKey string) (stats ExperienceWeight, exists bool, err error) {
	if err := d.ensureExperienceWeightSchema(); err != nil {
		return ExperienceWeight{}, false, err
	}
	var lastWin sql.NullTime
	err = d.QueryRow(`SELECT pattern_key, hits, wins, prior, day_key, day_wins,
 last_win_at, created_at, updated_at FROM experience_weight WHERE pattern_key=$1`, patternKey).
		Scan(&stats.PatternKey, &stats.Hits, &stats.Wins, &stats.Prior, &stats.DayKey,
			&stats.DayWins, &lastWin, &stats.CreatedAt, &stats.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ExperienceWeight{
			PatternKey: patternKey,
			Prior:      ExperienceDefaultPrior(),
		}, false, nil
	}
	if err != nil {
		return ExperienceWeight{}, false, fmt.Errorf("读经验权重卡失败: %w", err)
	}
	if lastWin.Valid {
		t := lastWin.Time
		stats.LastWinAt = &t
	}
	return stats, true, nil
}

// ExperienceWeightPrior 只读先验（排序面便捷入口）：无卡回落默认 prior。
func (d *DB) ExperienceWeightPrior(patternKey string) (float64, error) {
	stats, _, err := d.ExperienceWeightStats(patternKey)
	if err != nil {
		return 0, err
	}
	return stats.Prior, nil
}

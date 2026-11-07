package db

// ---------- 运行度量（P0 评估基线）----------
//
// 两块：
//  1. llm_profile_prices —— 单价表（经济配置，允许运维编辑；不进 llm_profiles 的
//     手写 CRUD 路径，避免为三列改动六个耦合点）。键 = profile name（改名时价格
//     不跟随——价格是运维对"那个模型档"的输入，改名属重建，重录一次即可）。
//  2. RunMetricsSnapshot —— 一次 run（按 task）的指标快照：
//     tokens by role（llm_usage，lane 名即 worker 列）、$/run（单价×tokens，
//     cache 分列计价，judge lane 单列）、blocked 意图数、planner 扫描构成
//     （activity kind='meter' 的最新一条 metadata——埋点在 agent/scanmeter.go）。
//
// 口径（方案 v3.1 §4）：验收锚只放 llm_usage 可确定性复算的指标；findings 数与
// 误报率不进本快照（人工流程另行记录）。

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

//go:embed schema_metrics.sql
var metricsSchemaSQL string

var metricsSchemaReady sync.Map

// EnsureMetricsSchema 建单价表（幂等）。单价表属运行期经济配置，受限角色也应可
// 读写（与 llm_usage 同级）⇒ 建表走迁移连接后由 role_grants 的 ALL TABLES 覆盖。
func (d *DB) EnsureMetricsSchema() error {
	if _, ok := metricsSchemaReady.Load(d.DB); ok {
		return nil
	}
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	if err := coordinateWithSchemaMigration(tx); err != nil {
		return err
	}
	if _, err := tx.Exec(metricsSchemaSQL); err != nil {
		return fmt.Errorf("应用度量 schema 失败: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	metricsSchemaReady.Store(d.DB, struct{}{})
	return nil
}

// ProfilePrice 是一个模型档的单价（每百万 tokens）。
type ProfilePrice struct {
	ProfileName   string  `json:"profile_name"`
	Currency      string  `json:"currency"`
	PerMInput     float64 `json:"per_m_input"`
	PerMOutput    float64 `json:"per_m_output"`
	PerMCacheRead float64 `json:"per_m_cache_read"`
}

const priceCols = `profile_name, COALESCE(currency,'CNY'), per_m_input, per_m_output, per_m_cache_read`

// SaveProfilePrices 全量覆盖一个 profile 的单价（upsert）。
func (d *DB) SaveProfilePrice(p ProfilePrice) error {
	if err := d.EnsureMetricsSchema(); err != nil {
		return err
	}
	if p.ProfileName == "" {
		return fmt.Errorf("profile_name 不能为空")
	}
	if p.Currency == "" {
		p.Currency = "CNY"
	}
	_, err := d.Exec(`INSERT INTO llm_profile_prices(profile_name, currency, per_m_input, per_m_output, per_m_cache_read)
VALUES ($1,$2,$3,$4,$5)
ON CONFLICT (profile_name) DO UPDATE SET currency=EXCLUDED.currency, per_m_input=EXCLUDED.per_m_input,
  per_m_output=EXCLUDED.per_m_output, per_m_cache_read=EXCLUDED.per_m_cache_read, updated_at=now()`,
		p.ProfileName, p.Currency, p.PerMInput, p.PerMOutput, p.PerMCacheRead)
	return err
}

// ProfilePrices 读全部单价。
func (d *DB) ProfilePrices() ([]ProfilePrice, error) {
	if err := d.EnsureMetricsSchema(); err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT ` + priceCols + ` FROM llm_profile_prices ORDER BY profile_name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ProfilePrice{}
	for rows.Next() {
		var p ProfilePrice
		if err := rows.Scan(&p.ProfileName, &p.Currency, &p.PerMInput, &p.PerMOutput, &p.PerMCacheRead); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// UsageByLane 是一个 lane（llm_usage.worker）的聚合行。
type UsageByLane struct {
	Worker       string  `json:"worker"`
	Calls        int64   `json:"calls"`
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	CacheRead    int64   `json:"cache_read"`
	CacheWrite   int64   `json:"cache_write"`
	OK           int64   `json:"ok"`
	Fails        int64   `json:"fails"`
	Cost         float64 `json:"cost"`
	// Models 列出该 lane 用过的模型（含 failover 轨迹），供人读。
	Models []string `json:"models"`
}

// RunMetricsSnapshot 是 P0 的 run 快照（验收锚 = 确定性可复算字段）。
type RunMetricsSnapshot struct {
	GeneratedAt    time.Time          `json:"generated_at"`
	TaskID         string             `json:"task_id"`
	ExplorationID  int64              `json:"exploration_id"`
	ByRole         []UsageByLane      `json:"by_role"`
	CostTotal      float64            `json:"cost_total"`
	Currency       string             `json:"currency"`
	JudgeCost      float64            `json:"judge_cost"` // 分类器 lane 单列（红队 P1-3：不并入会虚降 $/run）
	JudgeTokens    int64              `json:"judge_tokens"`
	BlockedIntents int64              `json:"blocked_intents"`
	PlannerScan    map[string]float64 `json:"planner_scan_bytes,omitempty"` // 最新一条 meter 的分桶字节
	PlannerScanAt  *time.Time         `json:"planner_scan_at,omitempty"`
	PricesMissing  []string           `json:"prices_missing"` // 出现过用量但没配单价的模型
}

// RunMetricsSnapshot 按 task 聚合 llm_usage + activity meter + blocked 意图数。
func (d *DB) RunMetricsSnapshot(taskID int64) (*RunMetricsSnapshot, error) {
	snap := &RunMetricsSnapshot{GeneratedAt: time.Now(), TaskID: fmt.Sprint(taskID), Currency: "CNY"}
	if err := d.EnsureMetricsSchema(); err != nil {
		return nil, err
	}
	// exploration id（meter 事件与 blocked 计数都挂探索图）。
	_ = d.QueryRow(`SELECT exploration_id FROM tasks WHERE id=$1`, taskID).Scan(&snap.ExplorationID)

	// 1) tokens by lane（llm_usage.task_id 是 TEXT）。
	rows, err := d.Query(`SELECT COALESCE(worker,''), count(*), sum(input_tokens), sum(output_tokens),
 sum(cache_read), sum(cache_write), count(*) FILTER (WHERE status='ok'), count(*) FILTER (WHERE status<>'ok'),
 COALESCE(array_agg(DISTINCT model) FILTER (WHERE model IS NOT NULL), '{}')
FROM llm_usage WHERE task_id=$1 GROUP BY 1 ORDER BY 1`, fmt.Sprint(taskID))
	if err != nil {
		return nil, fmt.Errorf("聚合 llm_usage 失败: %w", err)
	}
	defer rows.Close()
	prices, perr := d.ProfilePrices()
	if perr != nil {
		prices = nil
	}
	byName := map[string]ProfilePrice{}
	for _, p := range prices {
		byName[p.ProfileName] = p
	}
	seenModels := map[string]bool{}
	for rows.Next() {
		var lane UsageByLane
		var models []byte
		if err := rows.Scan(&lane.Worker, &lane.Calls, &lane.InputTokens, &lane.OutputTokens,
			&lane.CacheRead, &lane.CacheWrite, &lane.OK, &lane.Fails, &models); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(models, &lane.Models); err != nil {
			lane.Models = nil
		}
		lane.Cost = priceOf(byName, lane, &snap.PricesMissing, seenModels)
		snap.ByRole = append(snap.ByRole, lane)
		snap.CostTotal += lane.Cost
		if lane.Worker == "judge" {
			snap.JudgeCost += lane.Cost
			snap.JudgeTokens += lane.InputTokens + lane.OutputTokens
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// 2) blocked 意图数（当前态）。
	if snap.ExplorationID > 0 {
		_ = d.QueryRow(`SELECT count(*) FROM exploration_nodes
WHERE exploration_id=$1 AND kind='intent' AND state='blocked'`,
			snap.ExplorationID).Scan(&snap.BlockedIntents)

		// 3) planner 扫描构成：最新一条 meter 活动（agent/scanmeter.go 埋点）。
		var meta []byte
		var cat time.Time
		err = d.QueryRow(`SELECT metadata, created_at FROM activity
WHERE exploration_id=$1 AND kind='meter' AND COALESCE(worker,'')='planner'
ORDER BY id DESC LIMIT 1`, snap.ExplorationID).Scan(&meta, &cat)
		if err == nil {
			buckets := map[string]float64{}
			if json.Unmarshal(meta, &buckets) == nil {
				snap.PlannerScan = buckets
				snap.PlannerScanAt = &cat
			}
		}
	}
	return snap, nil
}

// priceOf 按 profile 名配价；缺价的模型记入 prices_missing（成本按 0 计，不猜）。
func priceOf(byName map[string]ProfilePrice, lane UsageByLane, missing *[]string, seen map[string]bool) float64 {
	cost := 0.0
	for _, m := range lane.Models {
		if seen[m] {
			continue
		}
		seen[m] = true
		if _, ok := byName[m]; !ok {
			*missing = append(*missing, m)
		}
	}
	// 成本按该 lane 出现过的每个模型各自的单价近似分摊：lane 聚合行只有总量，
	// 没有按 model 的 token 拆分 ⇒ 若 lane 用了多个模型，用"有价的那个模型"计价
	// 并在缺价时记 0。单模型 lane（绝大多数）精确；多模型 failover lane 是近似，
	// 快照里 Models 列表让人能看出这一点。
	var price *ProfilePrice
	for _, m := range lane.Models {
		if p, ok := byName[m]; ok {
			price = &p
			break
		}
	}
	if price == nil {
		return 0
	}
	cost = float64(lane.InputTokens)/1e6*price.PerMInput +
		float64(lane.OutputTokens)/1e6*price.PerMOutput +
		float64(lane.CacheRead)/1e6*price.PerMCacheRead
	return cost
}

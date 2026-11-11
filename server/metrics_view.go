package server

// 二期度量（方案 v10 §6 M2/M3/M4 + loop 可计算指标）。
//
// M2 唤醒计次：观测先行、只记不拦——硬闸阈值等真实分布数据（v10 §5 X5）。
// M3 LLM 统计：缓存命中率 + 分位延迟（llm_records 现成列派生，不做直方图表）。
// M4 intent 观测计数：attempts/outcomes 列（db/exploration.go 递增/追加）。
// loop 指标：v0.9.14 只上可计算子集（假设覆盖率/策略复用率/无效循环率）；
// 缺口闭合时间与矛盾转化率依赖批次 F2/F4 的 gap 实体，届时补（§16 口径）。

import (
	"encoding/json"
	"net/http"
	"strconv"
	"sync"

	"github.com/Autumn-27/artex/db"
)

// wakeCounter 是单任务的唤醒计次（进程内；重启归零——观测面，非裁决面）。
type wakeCounter struct {
	mu     sync.Mutex
	counts map[string]int64 // src（edge=图变合并 / heartbeat=心跳）→ 次数
	total  int64
}

func (w *wakeCounter) add(src string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.counts[src]++
	w.total++
}

func (w *wakeCounter) snapshot() map[string]any {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := map[string]any{"total": w.total}
	for k, v := range w.counts {
		out[k] = v
	}
	return out
}

// recordPlannerWake 在 runRound 入口打点（M2）。
func (e *Engine) recordPlannerWake(taskID, src string) {
	v, _ := e.wakeStat.LoadOrStore(taskID, &wakeCounter{counts: map[string]int64{}})
	v.(*wakeCounter).add(src)
}

// llmStats GET /api/llm/records/stats —— 缓存命中率与分位延迟（M3）。
// 口径：input_tokens 已含缓存（cache_read/write 是其子集），命中率 =
// sum(cache_read)/sum(input_tokens)；分位用 percentile_cont。
func (s *Server) llmStats(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	var total, inputSum, outputSum, cacheRead, cacheWrite, errN int64
	_ = pg.QueryRow(`
SELECT count(*),
       COALESCE(sum(input_tokens),0), COALESCE(sum(output_tokens),0),
       COALESCE(sum(cache_read),0), COALESCE(sum(cache_write),0),
       count(*) FILTER (WHERE status='error')
FROM llm_records`).Scan(&total, &inputSum, &outputSum, &cacheRead, &cacheWrite, &errN)
	var p50, p95 int64
	_ = pg.QueryRow(`
SELECT COALESCE(percentile_cont(0.5) WITHIN GROUP (ORDER BY latency_ms),0),
       COALESCE(percentile_cont(0.95) WITHIN GROUP (ORDER BY latency_ms),0)
FROM llm_records WHERE latency_ms > 0`).Scan(&p50, &p95)
	byWorker := []map[string]any{}
	rows, err := pg.Query(`
SELECT COALESCE(worker,''), count(*),
       COALESCE(percentile_cont(0.5) WITHIN GROUP (ORDER BY latency_ms),0),
       COALESCE(percentile_cont(0.95) WITHIN GROUP (ORDER BY latency_ms),0)
FROM llm_records WHERE latency_ms > 0 GROUP BY 1 ORDER BY 2 DESC LIMIT 12`)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var w string
			var n, a, b int64
			if rows.Scan(&w, &n, &a, &b) == nil {
				byWorker = append(byWorker, map[string]any{"worker": w, "calls": n, "p50_ms": a, "p95_ms": b})
			}
		}
	}
	hitRate := 0.0
	if inputSum > 0 {
		hitRate = float64(cacheRead) / float64(inputSum)
	}
	writeJSON(w, 200, map[string]any{
		"total_calls": total, "errors": errN,
		"input_tokens": inputSum, "output_tokens": outputSum,
		"cache_read": cacheRead, "cache_write": cacheWrite,
		"cache_hit_rate": hitRate,
		"latency_p50_ms": p50, "latency_p95_ms": p95,
		"by_worker": byWorker,
		"note":      "total=input+output；cache_read/write⊆input，只做比率不参与求和（方案 v10 §6 W9 口径）",
	})
}

// loopMetrics GET /api/metrics/loop?task_id= —— 奇点循环的可计算指标子集（§16）。
// 假设覆盖率：带预测字段的意图占比；策略复用率：strategies 的 usage/hit/miss；
// 无效循环率：exhausted+blocked 终态占比。矛盾转化率/缺口闭合时间待 F2/F4。
func (s *Server) loopMetrics(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	q := r.URL.Query()
	taskIDStr := q.Get("task_id")
	taskFilter := ""
	args := []any{}
	if taskIDStr != "" {
		if id, err := strconv.ParseInt(taskIDStr, 10, 64); err == nil && id > 0 {
			taskFilter = " AND t.id=$1"
			args = append(args, id)
		}
	}
	var total, withPrediction, exhausted, blocked, done int
	_ = pg.QueryRow(`
SELECT count(*),
       count(*) FILTER (WHERE n.payload ? 'predicted_vulnclass' OR n.payload ? 'predicted_actions'),
       count(*) FILTER (WHERE n.state='exhausted'),
       count(*) FILTER (WHERE n.state='blocked'),
       count(*) FILTER (WHERE n.state='done')
FROM exploration_nodes n JOIN tasks t ON t.exploration_id=n.exploration_id
WHERE n.kind='intent' AND t.deleted_at IS NULL`+taskFilter, args...).
		Scan(&total, &withPrediction, &exhausted, &blocked, &done)
	// 策略复用（全局口径——策略库跨任务共享）：usage_count 与 hit/miss 由归因链写。
	var sTotal int64
	var sUsage, sHit, sMiss int64
	_ = pg.QueryRow(`SELECT count(*), COALESCE(sum(usage_count),0), COALESCE(sum(hit_count),0), COALESCE(sum(miss_count),0)
FROM strategies WHERE state='active'`).Scan(&sTotal, &sUsage, &sHit, &sMiss)
	out := map[string]any{
		"wish_coverage":        ratio(withPrediction, total),
		"wish_total":           total,
		"wish_with_prediction": withPrediction,
		"wasted_loop_rate":     ratio(exhausted+blocked, total),
		"intent_exhausted":     exhausted,
		"intent_blocked":       blocked,
		"intent_done":          done,
		"strategy_reuse": map[string]any{
			"active_strategies": sTotal, "usage": sUsage, "hit": sHit, "miss": sMiss,
			"rate": ratio64(sHit, sUsage),
		},
		"pending_items": []string{"矛盾转化率（待 F2 矛盾检测器）", "缺口闭合时间（待 F4 gap 实体）"},
	}
	writeJSON(w, 200, out)
}

func ratio(part, total int) float64 {
	if total == 0 {
		return 0
	}
	return float64(part) / float64(total)
}

func ratio64(part, total int64) float64 {
	if total == 0 {
		return 0
	}
	return float64(part) / float64(total)
}

// taskIntentCounts 是 M4 的任务级聚合（governance 视图顺带返回；pg 由调用方传）。
func taskIntentCounts(pg *db.DB, taskID int64) map[string]any {
	if pg == nil {
		return map[string]any{}
	}
	var attempts int64
	var outcomes json.RawMessage
	_ = pg.QueryRow(`
SELECT COALESCE(sum(COALESCE(attempts,0)),0), COALESCE(jsonb_agg(outcomes) FILTER (WHERE outcomes IS NOT NULL),'[]'::jsonb)
FROM exploration_nodes WHERE exploration_id=(SELECT exploration_id FROM tasks WHERE id=$1) AND kind='intent'`, taskID).Scan(&attempts, &outcomes)
	return map[string]any{"attempts_total": attempts, "outcomes": outcomes}
}

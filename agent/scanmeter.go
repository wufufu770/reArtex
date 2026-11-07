package agent

// ---------- planner 扫描构成埋点（P0 评估基线） ----------
//
// 把 planner 每轮 user 输入的态势块按「桶」计量字节，emit 一条 activity
// （kind='meter', worker='planner', metadata=分桶字节）。RunMetricsSnapshot
// 读最新一条，产出 G4 验收所需的扫描构成分解（方案 v3.1 §4）。
//
// 桶口径（v3.1 定稿：cold_digest 折叠在 intents 桶内，metadata 注明）：
//   facts       = recent_facts（+facts 计数）
//   intents     = open_intents + running_intents + recent_done_intents（含 cold_digest 折叠）
//   goals_hints = goals + hints
//   other       = finding_list + task/assets 等其余键
//   system      = 系统提示正文（不含动态边界）
//
// 纯函数、零依赖；emit 由调用方（planner.Plan）提供，失败不影响规划主链路。

import (
	"encoding/json"

	"github.com/Autumn-27/artex/db"
)

// scanBucketOf 把 graph_overview 的一个 key 归桶。
func scanBucketOf(key string) string {
	switch key {
	case "recent_facts", "facts":
		return "facts"
	case "open_intents", "running_intents", "recent_done_intents", "frontier_open", "done_intents_total":
		return "intents" // 含 cold_digest 折叠（recent_done_intents 内）
	case "goals", "hints":
		return "goals_hints"
	default:
		return "other" // finding_list / task / assets / coverage 等
	}
}

// MeterPlannerScan 把一轮 planner 输入的构成计量成 activity。
// data 是 graphOverviewData 的原始 map（未 marshal）；systemLen 是系统提示正文字节数。
func MeterPlannerScan(emit func(db.Activity), data map[string]any, systemLen int) {
	if emit == nil || data == nil {
		return
	}
	buckets := map[string]float64{
		"facts": 0, "intents": 0, "goals_hints": 0, "other": 0,
		"system": float64(systemLen),
	}
	for key, v := range data {
		b, err := json.Marshal(v)
		if err != nil {
			continue
		}
		bucket := scanBucketOf(key)
		if key == "facts" || key == "frontier_open" || key == "done_intents_total" {
			// 纯计数的键不占上下文多少字节，但保留可读性：计入对应桶。
			buckets[bucket] += float64(len(b))
			continue
		}
		buckets[bucket] += float64(len(b))
	}
	meta, err := json.Marshal(buckets)
	if err != nil {
		return
	}
	// best-effort：计量失败不影响规划（emit 内部已吞错——engine.emitActivity）。
	emit(db.Activity{Worker: "planner", Kind: "meter", Summary: "planner-scan-meter", Metadata: meta})
}

package server

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/internal/edge"
)

// captureMetrics 渲染 /api/metrics 的 Prometheus 文本（测试辅助）。
func captureMetrics(t *testing.T) string {
	t.Helper()
	rec := httptest.NewRecorder()
	renderSchedMetrics(rec)
	return rec.Body.String()
}

// stallAdvance 的归零语义是熔断③的命门：0.13.1 及之前，fireEdgeWake 正常路径
// 从不把 StallLastHash 落库，loadEdgeBreaker 每次读回空 ⇒ 本函数恒走「基线」
// 分支返回 0 —— 停滞熔断是死代码（0.13.2 修复：推进移入水位 gate 且推进即
// save；本测试钉死计数语义，防止回归）。
func TestStallAdvance(t *testing.T) {
	const h1 = "aaaa"
	const h2 = "bbbb"

	// 基线：首次见到（lastHash 为空）算推进，返回 0。
	if got := stallAdvance("", h1, 0); got != 0 {
		t.Fatalf("基线轮应返回 0，got %d", got)
	}
	// 哈希为空（理论不可达：CriterionHash 对空 map 也产出值）时保守归零。
	if got := stallAdvance(h1, "", 3); got != 0 {
		t.Fatalf("空哈希应归零，got %d", got)
	}
	// 连续无推进：逐轮 +1，到达阈值即熔断。
	rounds := 0
	for i := 0; i < edge.StallThreshold; i++ {
		rounds = stallAdvance(h1, h1, rounds)
	}
	if rounds != edge.StallThreshold {
		t.Fatalf("连续 %d 轮无推进应到达阈值 %d，got %d", edge.StallThreshold, edge.StallThreshold, rounds)
	}
	// 任一推进（哈希变化）即清零。
	if got := stallAdvance(h1, h2, edge.StallThreshold); got != 0 {
		t.Fatalf("哈希变化应清零，got %d", got)
	}
}

// decideEdgeWake 是 fireEdgeWake 的决策核心（0.14.0 批次一 A1 抽纯函数）——
// 四路径与优先级在此钉死：doctrine > 水位 skip > 熔断 > fire；熔断要求
// stallAdvances=true（本对账轮哈希与上轮相同）。
func TestDecideEdgeWake(t *testing.T) {
	const th = edge.StallThreshold

	cases := []struct {
		name          string
		hasCells      bool
		doctrineReady bool
		inWatermark   bool
		controllerBusy bool
		workInFlight  bool
		stallAdvances bool
		stallRounds   int
		want          edgeWakeVerdict
	}{
		{name: "判据成立优先于一切", hasCells: true, doctrineReady: true, inWatermark: true, stallAdvances: true, stallRounds: th, want: edgeWakeDoctrine},
		{name: "水位期内跳过", hasCells: true, doctrineReady: false, inWatermark: true, stallAdvances: true, stallRounds: th, want: edgeWakeSkip},
		{name: "水位期内即使计数达标也不熔断", hasCells: false, doctrineReady: false, inWatermark: true, stallAdvances: true, stallRounds: th - 1, want: edgeWakeSkip},
		{name: "连续无推进到阈值熔断", hasCells: true, doctrineReady: false, inWatermark: false, stallAdvances: true, stallRounds: th - 1, want: edgeWakeBreak},
		{name: "本轮判据动过不熔断", hasCells: true, doctrineReady: false, inWatermark: false, stallAdvances: false, stallRounds: th - 1, want: edgeWakeFire},
		{name: "首见基线不熔断", hasCells: true, doctrineReady: false, inWatermark: false, stallAdvances: false, stallRounds: 0, want: edgeWakeFire},
		{name: "计数未到阈值唤醒", hasCells: true, doctrineReady: false, inWatermark: false, stallAdvances: true, stallRounds: th - 2, want: edgeWakeFire},
		{name: "无格点且不水位则唤醒", hasCells: false, doctrineReady: false, inWatermark: false, stallAdvances: true, stallRounds: 0, want: edgeWakeFire},
		// 0.15.53 排队守卫：控制轮在飞/排队 ⇒ 这个 tick 不是（也不该变成）
		// 对账周期——实测 re-open 的 task 6 运行在等 task 3 的 40 格长轮时被
		// criterion_stalled 误杀。
		{name: "控制轮在飞不算对账周期", hasCells: false, doctrineReady: false, controllerBusy: true, stallAdvances: true, stallRounds: th - 1, want: edgeWakeSkip},
		{name: "控制轮在飞即使计数达标也不熔断", hasCells: true, doctrineReady: false, controllerBusy: true, stallAdvances: true, stallRounds: th, want: edgeWakeSkip},
		// 0.15.53 在飞守卫：假设在 worker 手里验证 ⇒ 熔断不成立、照常唤醒
		//（可继续对其他格点下发）。
		{name: "假设在飞不熔断照常唤醒", hasCells: true, doctrineReady: false, workInFlight: true, stallAdvances: true, stallRounds: th - 1, want: edgeWakeFire},
		// 0.15.54 doctrine 在飞守卫：封顶风暴会把格点批量打成终态——验证还在
		// 飞时不机械收，等回写落定（实测 task 6 的 20 格封顶 falsity 收出
		// doctrine_complete）。
		{name: "全终态但假设在飞不机械收", hasCells: true, doctrineReady: true, workInFlight: true, want: edgeWakeSkip},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := decideEdgeWake(tc.hasCells, tc.doctrineReady, tc.inWatermark, tc.controllerBusy, tc.workInFlight, tc.stallAdvances, tc.stallRounds)
			if got != tc.want {
				t.Fatalf("decideEdgeWake(%v,%v,%v,%v,%v,%v,%d) = %v，期望 %v",
					tc.hasCells, tc.doctrineReady, tc.inWatermark, tc.controllerBusy, tc.workInFlight, tc.stallAdvances, tc.stallRounds, got, tc.want)
			}
		})
	}
}

// TestSchedMetricsRender 钉住 /api/metrics 的 Prometheus 文本形状（计数器名与
// counter 类型声明在位——抓取端按名解析，改名=破坏兼容）。
func TestSchedMetricsRender(t *testing.T) {
	// 写一个非零值再读，确认渲染路径。
	schedCounters.wakeFired.Store(3)
	out := captureMetrics(t)
	for _, want := range []string{
		"# TYPE artex_scheduler_tick_total counter",
		"artex_edge_wake_fired_total 3",
		"artex_edge_circuit_breaks_total",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("metrics 输出缺 %q：\n%s", want, out)
		}
	}
}

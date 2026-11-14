package server

// 调度面过程计数器（0.14.0 批次一 A2）：自主循环的质量必须可观测——
// 停滞熔断死代码（0.13.2 修复）当时零测试零指标，实机观测靠人肉夹具。
// 这里用进程内原子计数器（Prometheus 文本格式经 /api/metrics 暴露）：
// 重启归零是该口径的已知边界（累计趋势看时间序列而非单点值）。

import (
	"fmt"
	"net/http"
	"sync/atomic"
)

// schedMetrics 是调度面的全部计数器。零值可用；采集方只读。
type schedMetrics struct {
	tickTotal       atomic.Int64 // step() 执行次数
	wakeFired       atomic.Int64 // fireEdgeWake 实际唤醒 edge_exec 次数
	watermarkSkips  atomic.Int64 // 水位去重跳过的唤醒（对账/唤醒共用水位）
	circuitBreaks   atomic.Int64 // 判据停滞熔断次数
	doctrineDone    atomic.Int64 // 判据成立机械收尾次数
	taskMissingSkip atomic.Int64 // 运行行存在但任务不在 manager（归档/删除）跳过次数
}

var schedCounters schedMetrics

// renderSchedMetrics 输出 Prometheus 文本格式的调度面指标（每个计数器一行，
// 带 HELP/TYPE；值来自原子读）。
func renderSchedMetrics(w http.ResponseWriter) {
	row := func(name, help string, v int64) {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n%s %d\n", name, help, name, name, v)
	}
	row("artex_scheduler_tick_total", "Scheduler step() executions.", schedCounters.tickTotal.Load())
	row("artex_edge_wake_fired_total", "edge_exec wake-ups dispatched by fireEdgeWake.", schedCounters.wakeFired.Load())
	row("artex_edge_watermark_skips_total", "Wake-ups skipped by the 60s watermark gate (stall accounting shares the gate).", schedCounters.watermarkSkips.Load())
	row("artex_edge_circuit_breaks_total", "Criterion-stalled circuit breaks concluded by the scheduler.", schedCounters.circuitBreaks.Load())
	row("artex_edge_doctrine_concludes_total", "Doctrine-complete mechanical conclusions (all cells terminal).", schedCounters.doctrineDone.Load())
	row("artex_edge_task_missing_total", "Open edge runs whose task is absent from the manager (archived/deleted) — skipped, run left for manual close.", schedCounters.taskMissingSkip.Load())
}

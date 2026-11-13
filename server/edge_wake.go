package server

// 偏锋轮转唤醒（EDGE E7）——fireEdgeWake。
//
// 为什么存在：edge_exec 是内置 agent，HTTP 面不能给它配 agent_triggers
//（server/triggers.go:58 对内置 400）；它又是每轮 ≤15min 的总控轮，轮间接力
// 不能靠 interval 触发器。fireEdgeWake 每 tick 扫未收尾的 edge_runs：
//   - 判据停滞熔断（criterion_stalled）在这里判：格点判据状态的规范化哈希连续
//     N 轮不变 ⇒ ConcludeEdgeRun(criterion_stalled) + 审计，不再唤醒；
//   - 其余情况按水位间隔（60s）唤醒 edge_exec 跑下一轮总控
//    （StartTriggeredRun → per-agent FIFO serial+by_task 队列，同任务突发合并）。
//
// 水位与熔断状态持久在 scheduler_state（键见 edge_tools.go）——重启恢复语义与
// fired_goals 同款：集合/计数只进不退，重启不清。
//
// 与 fireGoals 的一个刻意差异：这里不查 agents.enabled 开关——edge_exec 的运行
// 由「有没有未收尾的运行行」承载（edge_run 建任务即开行，人工取消走
// stop_reason=cancelled 收行），没有配置开关可以误关一条进行中的探索。

import (
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/internal/edge"
)

// fireEdgeWake 每 tick 调一次。
func (sc *Scheduler) fireEdgeWake() {
	runs, err := sc.pg.ListOpenEdgeRuns()
	if err != nil || len(runs) == 0 {
		return
	}
	now := time.Now().Unix()
	for _, run := range runs {
		t, _ := sc.s.m.Task(strconv.FormatInt(run.TaskID, 10))
		if t == nil {
			continue // 任务已归档/删除：运行行留待人工收（不自动判 stop）
		}
		// ① 判据停滞对账（熔断③）：聚合格点判据状态做规范化哈希。
		st := sc.s.loadEdgeBreaker(run.TaskID)
		cells, _ := sc.pg.ListEdgeCells(run.TaskID)
		agg := map[string]any{}
		for _, c := range cells {
			agg[c.CellID] = json.RawMessage(c.CriterionState)
		}
		hash := edge.CriterionHash(agg)
		st.StallRounds = stallAdvance(st.StallLastHash, hash, st.StallRounds)
		st.StallLastHash = hash
		if st.StallRounds >= edge.StallThreshold {
			_ = sc.pg.ConcludeEdgeRun(run.ID, db.StopReasonStalled, map[string]any{
				"reason": fmt.Sprintf("判据状态连续 %d 轮无推进（规范化哈希不变）", st.StallRounds),
				"hash":   hash,
			})
			sc.s.saveEdgeBreaker(run.TaskID, &edgeBreakerState{})
			sc.s.appendEdgeAudit("edge-circuit-breaker", "fireEdgeWake",
				fmt.Sprintf("task=%d 熔断 criterion_stalled（%d 轮无推进）", run.TaskID, st.StallRounds),
				map[string]any{"task_id": run.TaskID, "stop_reason": db.StopReasonStalled, "rounds": st.StallRounds})
			continue
		}
		// ② 判据成立检测：全部格点终态且非空 ⇒ 主路径收尾（不需要模型确认）。
		if len(cells) > 0 {
			allTerminal := true
			for _, c := range cells {
				if c.State != db.EdgeCellConfirmed && c.State != db.EdgeCellDisproven && c.State != db.EdgeCellFolded {
					allTerminal = false
					break
				}
			}
			if allTerminal {
				_ = sc.pg.ConcludeEdgeRun(run.ID, db.StopReasonDoctrine, map[string]any{
					"reason": "全部格点到达终态（调度器机械判定）",
				})
				_, _ = sc.s.engine.ConcludeTask(strconv.FormatInt(run.TaskID, 10), "偏锋判据成立：全部格点到达终态，任务收尾（doctrine_complete）。")
				sc.s.appendEdgeAudit("edge-run", "fireEdgeWake",
					fmt.Sprintf("task=%d doctrine_complete（调度器判定）", run.TaskID),
					map[string]any{"task_id": run.TaskID, "stop_reason": db.StopReasonDoctrine})
				continue
			}
		}
		// ③ 水位去重：同一任务两次唤醒至少隔 60s（serial+by_task 队列兜并发）。
		if lastRaw, err := sc.pg.GetSchedState(edgeWakeKey(run.TaskID)); err == nil && lastRaw != "" {
			if last, err := strconv.ParseInt(lastRaw, 10, 64); err == nil && now-last < int64(edgeWakeMinInterval().Seconds()) {
				continue
			}
		}
		_ = sc.pg.SetSchedState(edgeWakeKey(run.TaskID), strconv.FormatInt(now, 10))
		// ④ 唤醒 edge_exec 跑一轮总控（mergeable=true：同任务突发合并进一个会话）。
		goal, desc := "", ""
		if t != nil {
			goal, desc = t.Goal, t.Description
		}
		_, methodologyID, _ := sc.pg.TaskEdgeFields(run.TaskID)
		sc.s.StartTriggeredRun("edge_exec", "偏锋总控轮",
			fmt.Sprintf("【偏锋总控轮】任务 #%d（方法论=%s）。请走总控循环：edge_coverage 查进度 → 选格点 → edge_dispatch 下发假设 → 对账判据；判据成立时 edge_conclude 收尾。", run.TaskID, methodologyID),
			run.TaskID, true, desc, goal)
	}
}

// stallAdvance 是熔断③的计数推进：哈希不变则 +1（基线首轮返回 0——首次见到
// 的状态是刚产生的，算推进），变化清零。与 internal/edge 的 StallTracker 同一
// 语义；这里单独一份是因为状态持久化在 scheduler_state（跨重启），不走内存结构。
func stallAdvance(lastHash, hash string, rounds int) int {
	if lastHash == "" || hash == "" {
		return 0
	}
	if hash == lastHash {
		return rounds + 1
	}
	return 0
}

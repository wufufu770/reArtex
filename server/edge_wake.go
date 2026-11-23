package server

// 偏锋轮转唤醒（EDGE E7）——fireEdgeWake。
//
// 为什么存在：edge_exec 是内置 agent，HTTP 面不能给它配 agent_triggers
//（server/triggers.go:58 对内置 400）；它又是每轮 ≤15min 的总控轮，轮间接力
// 不能靠 interval 触发器。fireEdgeWake 每 tick 扫未收尾的 edge_runs：
//   - 判据成立机械收尾（doctrine_complete）每 tick 判——主路径不被水位延迟；
//   - 判据停滞熔断（criterion_stalled）按水位间隔（60s）对账：格点判据状态的
//     规范化哈希连续 N 个对账周期不变 ⇒ ConcludeEdgeRun(criterion_stalled)
//     + 审计，不再唤醒。对账节奏必须与水位对齐——按 5s tick 推进会把正常
//     执行中的长轮（LLM dispatch 间隔分钟级）误判成停滞；
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

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/internal/edge"
)

// edgeWakeVerdict 是对单个 open edge run 的本轮判定结果（fireEdgeWake 的核心
// 决策抽成纯函数——0.14.0 批次一 A1：停滞熔断曾以死代码形态存活三个版本，
// 根因就是这段逻辑不可测）。
type edgeWakeVerdict int

const (
	// edgeWakeSkip：水位期内跳过（对账与唤醒共用水位 gate）。
	edgeWakeSkip edgeWakeVerdict = iota
	// edgeWakeBreak：判据停滞熔断（stallRounds 达阈值）。
	edgeWakeBreak
	// edgeWakeDoctrine：全部格点终态 ⇒ 机械收尾（主路径，不受水位延迟）。
	edgeWakeDoctrine
	// edgeWakeFire：唤醒 edge_exec 跑下一轮总控。
	edgeWakeFire
)

// decideEdgeWake 是 fireEdgeWake 的决策核心（纯函数，无 IO）：
//
//	hasCells/doctrineReady —— 判据成立检测（每 tick 机械判定，先于水位）；
//	inWatermark            —— 上次唤醒距今不足间隔；
//	stallRounds            —— 停滞计数（上一对账轮推进后的值）；
//	stallAdvances          —— 本对账轮判据哈希与上轮【相同】（推进语义：
//	                          false = 变化归零 / 首见基线，熔断都不成立）。
//
// 优先级：doctrine > 水位 skip > 熔断 > fire。熔断要求连续无推进——
// stallAdvances=false 时哪怕计数已到阈值也不熔断（那轮判据动过了）。
func decideEdgeWake(hasCells, doctrineReady, inWatermark, stallAdvances bool, stallRounds int) edgeWakeVerdict {
	if hasCells && doctrineReady {
		return edgeWakeDoctrine
	}
	if inWatermark {
		return edgeWakeSkip
	}
	if stallAdvances && stallRounds+1 >= edge.StallThreshold {
		return edgeWakeBreak
	}
	return edgeWakeFire
}

// fireEdgeWake 每 tick 调一次。
func (sc *Scheduler) fireEdgeWake() {
	runs, err := sc.pg.ListOpenEdgeRuns()
	if err != nil || len(runs) == 0 {
		return
	}
	now := sc.now().Unix()
	for _, run := range runs {
		t, _ := sc.s.m.Task(strconv.FormatInt(run.TaskID, 10))
		if t == nil {
			schedCounters.taskMissingSkip.Add(1)
			continue // 任务已归档/删除：运行行留待人工收（不自动判 stop）
		}
		cells, _ := sc.pg.ListEdgeCells(run.TaskID)
		// ① 判据成立检测（决策①：主路径，每 tick 判定不被水位延迟）。
		doctrine := false
		if len(cells) > 0 {
			allTerminal := true
			for _, c := range cells {
				if c.State != db.EdgeCellConfirmed && c.State != db.EdgeCellDisproven && c.State != db.EdgeCellFolded {
					allTerminal = false
					break
				}
			}
			doctrine = allTerminal
		}
		// ② 水位去重：同一任务两次对账/唤醒至少隔 60s（停滞对账的「轮」节奏由
		//    这里承载——每水位推进一次 ≈ 一轮对账，0.13.2 修复的节奏语义）。
		inWatermark := false
		if lastRaw, err := sc.pg.GetSchedState(edgeWakeKey(run.TaskID)); err == nil && lastRaw != "" {
			if last, err := strconv.ParseInt(lastRaw, 10, 64); err == nil && now-last < int64(edgeWakeMinInterval().Seconds()) {
				inWatermark = true
			}
		}
		// 停滞对账输入：本对账轮哈希与上轮是否相同（放行轮才推进计数）。
		st := sc.s.loadEdgeBreaker(run.TaskID)
		hash := edge.CriterionHash(edgeCellHashes(cells))
		stallAdvances := st.StallLastHash != "" && hash == st.StallLastHash
		verdict := decideEdgeWake(len(cells) > 0, doctrine, inWatermark, stallAdvances, st.StallRounds)
		switch verdict {
		case edgeWakeDoctrine:
			schedCounters.doctrineDone.Add(1)
			_ = sc.pg.ConcludeEdgeRun(run.ID, db.StopReasonDoctrine, map[string]any{
				"reason": "全部格点到达终态（调度器机械判定）",
			})
			_, _ = sc.s.engine.ConcludeTask(strconv.FormatInt(run.TaskID, 10), "偏锋判据成立：全部格点到达终态，任务收尾（doctrine_complete）。")
			sc.s.appendEdgeAudit("edge-run", "fireEdgeWake",
				fmt.Sprintf("task=%d doctrine_complete（调度器判定）", run.TaskID),
				map[string]any{"task_id": run.TaskID, "stop_reason": db.StopReasonDoctrine})
			continue
		case edgeWakeSkip:
			schedCounters.watermarkSkips.Add(1)
			continue
		case edgeWakeBreak:
			schedCounters.circuitBreaks.Add(1)
			_ = sc.pg.ConcludeEdgeRun(run.ID, db.StopReasonStalled, map[string]any{
				"reason": fmt.Sprintf("判据状态连续 %d 个对账周期无推进（规范化哈希不变，周期=%s）", st.StallRounds+1, edgeWakeMinInterval()),
				"hash":   hash,
			})
			sc.s.saveEdgeBreaker(run.TaskID, &edgeBreakerState{})
			sc.s.appendEdgeAudit("edge-circuit-breaker", "fireEdgeWake",
				fmt.Sprintf("task=%d 熔断 criterion_stalled（%d 轮无推进）", run.TaskID, st.StallRounds+1),
				map[string]any{"task_id": run.TaskID, "stop_reason": db.StopReasonStalled, "rounds": st.StallRounds + 1})
			continue
		}
		// ③ 放行：记水位 → 推进停滞对账（推进即落库，0.13.2 修复语义）。
		_ = sc.pg.SetSchedState(edgeWakeKey(run.TaskID), strconv.FormatInt(now, 10))
		st.StallRounds = stallAdvance(st.StallLastHash, hash, st.StallRounds)
		st.StallLastHash = hash
		sc.s.saveEdgeBreaker(run.TaskID, st)
		// ④ 唤醒 edge_exec 跑一轮总控（mergeable=true：同任务突发合并进一个会话）。
		schedCounters.wakeFired.Add(1)
		_, methodologyID, _ := sc.pg.TaskEdgeFields(run.TaskID)
		sc.s.StartTriggeredRun("edge_exec", "偏锋总控轮",
			fmt.Sprintf("【偏锋总控轮】任务 #%d（方法论=%s）。请走总控循环：edge_coverage 查进度 → 选格点 → edge_dispatch 下发假设 → 对账判据；格点坐标系未建立（cells_total=0）时先用 edge_init_cells 建格再下发；判据成立时 edge_conclude 收尾。", run.TaskID, methodologyID),
			run.TaskID, true, t.Description, t.Goal)
	}
}

// edgeCellHashes 把格点聚成 CriterionHash 的输入形状（复用两处，避免重复）。
func edgeCellHashes(cells []*db.EdgeCell) map[string]any {
	agg := map[string]any{}
	for _, c := range cells {
		agg[c.CellID] = json.RawMessage(c.CriterionState)
	}
	return agg
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

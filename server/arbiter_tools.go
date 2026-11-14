package server

// 意图仲裁工具面（0.14.1 批次二）：arbiter agent 的唯一写工具。
//
// 纪律：arbiter_resolve 只做「提议的宿主落笔」——真正的状态写在本文件的宿主
// 函数（db.ResolveConflict / FreezeConflict），agent 没有任何直接改库路径；
// 每次落笔写审计哈希链（source=intent-arbitration）。绑定只给 arbiter
//（roledispatch 钉死），别的 agent 拿不到。

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	actool "github.com/Autumn-27/norma/tool"

	"github.com/Autumn-27/artex/agent"
)

// arbiterTools 返回仲裁工具全集。
func (s *Server) arbiterTools() []actool.CoreTool {
	return []actool.CoreTool{s.toolArbiterResolve()}
}

// seedArbiterTools 播种仲裁工具（首插生效，DO NOTHING 保留用户编辑；refresh
// 旗标惯例 + 收尾 InvalidateAssembly 翻代 G1 缓存）。
func (s *Server) seedArbiterTools() {
	const refreshFlag = "arbiter_tools_seed_v1"
	bindings, _ := json.Marshal([]string{"arbiter"})
	for _, t := range s.arbiterTools() {
		schema, _ := json.Marshal(t.InputSchema())
		_ = s.m.PG().SeedTool(t.Name(), t.Description(), schema, bindings)
	}
	if v, _, _ := s.m.pg.GetSetting(refreshFlag); v != "true" {
		_ = s.m.pg.SetSetting(refreshFlag, "true")
	}
	agent.InvalidateAssembly()
}

// toolArbiterResolve 是仲裁结论的宿主落笔入口：verdict 枚举 + 理由必填；
// defer_both 连续达阈值即冻结冲突（等人兜底）。
func (s *Server) toolArbiterResolve() actool.CoreTool {
	return wrTool("arbiter_resolve",
		"意图冲突的仲裁落笔入口（宿主侧执行，写审计链）。verdict 三选一：proceed_a（先做 a 方）/ proceed_b / defer_both（证据同级宁 defer 勿瞎裁——连续 defer 会触发熔断冻结，等人兜底）。理由必填且写进审计链。",
		objSchema(map[string]any{
			"conflict_id": map[string]any{"type": "integer", "description": "冲突 id（派发消息里给出）"},
			"verdict":     strParam("proceed_a | proceed_b | defer_both"),
			"reason":      strParam("判定理由（必填，写进审计链； proceed 时写明依赖的证据）"),
		}, "conflict_id", "verdict", "reason"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				ConflictID int64  `json:"conflict_id"`
				Verdict    string `json:"verdict"`
				Reason     string `json:"reason"`
			}
			_ = json.Unmarshal(in, &a)
			if a.ConflictID <= 0 {
				return actool.Errorf("conflict_id 必填"), nil
			}
			if err := s.resolveIntentConflict(a.ConflictID, a.Verdict, a.Reason); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			return actool.Text(fmt.Sprintf("冲突 #%d 仲裁落笔完成（verdict=%s，已写审计链）。", a.ConflictID, a.Verdict)), nil
		})
}

// conflictArbitrationOverview 是调度唤醒的输入：全部 open 冲突（带任务信息）。
type conflictArbitrationOverview struct {
	ConflictID    int64
	ExplorationID int64
	TaskID        int64
	RuleID        string
	ANodeID       int64
	BNodeID       int64
}

// listOpenConflictsForArbitration 读全部 open 冲突（调度器每 tick 的候选流）。
func (s *Server) listOpenConflictsForArbitration() []conflictArbitrationOverview {
	pg := s.m.pg
	rows, err := pg.Query(`
SELECT c.id, c.exploration_id, COALESCE(t.id,0), c.detect_rule, c.a_node_id, c.b_node_id
FROM intent_conflicts c
LEFT JOIN tasks t ON t.exploration_id = c.exploration_id AND t.deleted_at IS NULL
WHERE c.state = 'open'
ORDER BY c.id`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []conflictArbitrationOverview
	for rows.Next() {
		var o conflictArbitrationOverview
		if err := rows.Scan(&o.ConflictID, &o.ExplorationID, &o.TaskID, &o.RuleID, &o.ANodeID, &o.BNodeID); err != nil {
			continue
		}
		out = append(out, o)
	}
	return out
}

// fireConflictArbitration 每 tick 由 step() 调：open 冲突按水位（60s/冲突）唤醒
// arbiter 做一轮调研判定（与 fireEdgeWake 同款水位语义；serial+by_task 队列兜
// 并发）。任务缺失的冲突行跳过（留人工收，不自动判）。
func (sc *Scheduler) fireConflictArbitration() {
	overviews := sc.s.listOpenConflictsForArbitration()
	if len(overviews) == 0 {
		return
	}
	now := sc.now().Unix()
	for _, o := range overviews {
		if o.TaskID <= 0 {
			continue // 任务已删：冲突行留人工收
		}
		key := conflictWakeKey(o.ConflictID)
		if lastRaw, err := sc.pg.GetSchedState(key); err == nil && lastRaw != "" {
			if last, err := strconv.ParseInt(lastRaw, 10, 64); err == nil && now-last < int64(edgeWakeMinInterval().Seconds()) {
				continue
			}
		}
		_ = sc.pg.SetSchedState(key, strconv.FormatInt(now, 10))
		schedCounters.wakeFired.Add(1)
		sc.s.StartTriggeredRun("arbiter", "意图仲裁",
			fmt.Sprintf("【意图仲裁】冲突 #%d（任务 #%d，规则=%s，意图对 a=%d / b=%d）。请先调研双方证据（任务图/发现/业务图/资产），再 arbiter_resolve 落笔：证据更强或前置更少的一方 proceed；证据同级且差距小就 defer_both（宁 defer 勿瞎裁）。理由必填。",
				o.ConflictID, o.TaskID, o.RuleID, o.ANodeID, o.BNodeID),
			o.TaskID, true, "", "")
	}
}

// conflictWakeKey 是冲突唤醒的 scheduler_state 水位键。
func conflictWakeKey(conflictID int64) string {
	return fmt.Sprintf("conflict_wake:%d", conflictID)
}

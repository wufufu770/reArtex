package server

// 意图冲突仲裁的 server 装配（0.14.1 批次二 B1）。
//
// 链路（施工方案 v2，用户裁定「减少人的参与」）：
//
//	意图派生钩子 → buildIntentSignals（payload+锚资产，宁漏勿误）
//	  → intent.Scan（确定性规则）→ 规则能裁：ResolveConflict(host-rule)
//	                              → 规则裁不了：保持 open → 调度器唤醒 arbiter
//	  → arbiter 调研（只读工具）→ arbiter_resolve 提议 → 宿主落笔（本文件）
//	  → 连续 3 次 defer_both → FreezeConflict（唯一进人路径）
//
// 红线不动：仲裁只决定「谁先做」，不触碰闸门层「能不能做」；agent 无任何直接
// 改库路径（arbiter_resolve 是宿主函数的唯一入口）。意图状态联动（被压制方
// blocked）排 0.14.2 与前端冲突块一起——本版只落冲突账与裁决账，如实声明。

import (
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/internal/intent"
)

// buildIntentSignals 从探索图提取检测信号（payload 结构化不足的意图被过滤——
// 宁漏勿误）。
func (s *Server) buildIntentSignals(explorationID int64) []intent.Signal {
	pg := s.m.pg
	rows, err := pg.Query(`
SELECT n.id, COALESCE(n.payload,'{}'::jsonb)
FROM exploration_nodes n
WHERE n.exploration_id=$1 AND n.kind='intent' AND n.state='open'`, explorationID)
	if err != nil {
		log.Printf("[arbitration] 读 open intents 失败: %v", err)
		return nil
	}
	type rawIntent struct {
		id      int64
		payload map[string]any
	}
	var raws []rawIntent
	nodeIDs := []int64{}
	for rows.Next() {
		var rawJSON []byte
		var r rawIntent
		if err := rows.Scan(&r.id, &rawJSON); err != nil {
			continue
		}
		_ = json.Unmarshal(rawJSON, &r.payload)
		raws = append(raws, r)
		nodeIDs = append(nodeIDs, r.id)
	}
	rows.Close()
	if len(raws) == 0 {
		return nil
	}

	// 锚资产：node_id → asset ids（数组参数用文本字面量 '{1,2,3}'——自家 int64
	// 拼接无注入面；驱动对原生 []int64 的数组序列化不可靠）。
	anchorMap := map[int64][]int64{}
	if len(nodeIDs) > 0 {
		parts := make([]string, len(nodeIDs))
		for i, id := range nodeIDs {
			parts[i] = strconv.FormatInt(id, 10)
		}
		aRows, err := pg.Query(`
SELECT node_id, asset_id FROM exploration_anchors WHERE node_id = ANY($1::bigint[])`,
			"{"+strings.Join(parts, ",")+"}")
		if err == nil {
			for aRows.Next() {
				var nodeID, assetID int64
				if aRows.Scan(&nodeID, &assetID) == nil {
					anchorMap[nodeID] = append(anchorMap[nodeID], assetID)
				}
			}
			aRows.Close()
		} else {
			log.Printf("[arbitration] 读锚资产失败: %v", err)
		}
	}

	out := make([]intent.Signal, 0, len(raws))
	for _, r := range raws {
		assets := dedupSortInt64(anchorMap[r.id])
		if len(assets) == 0 {
			continue // 无锚不参与检测（结构化不足）
		}
		sig := intent.Signal{
			NodeID:   r.id,
			AssetIDs: assets,
		}
		if v, ok := r.payload["kind"].(string); ok {
			sig.Kind = v
		}
		if v, ok := r.payload["summary"].(string); ok {
			sig.Summary = v
		}
		if v, ok := r.payload["predicted_vulnclass"].(string); ok {
			sig.VulnClass = v
		}
		if arr, ok := r.payload["predicted_actions"].([]any); ok {
			for _, v := range arr {
				if sv, ok := v.(string); ok {
					sig.Actions = append(sig.Actions, sv)
				}
			}
		}
		out = append(out, sig)
	}
	return out
}

// mustExplorationID 读任务的 exploration_id（派生挂点用；查不到返回 0——
// scanAndRecordConflicts 对 0 短路）。
func mustExplorationID(pg *db.DB, taskID int64) int64 {
	var eid int64
	_ = pg.QueryRow(`SELECT exploration_id FROM tasks WHERE id=$1`, taskID).Scan(&eid)
	return eid
}

// scanAndRecordConflicts 对一个探索跑冲突检测并登记：规则能裁的直接宿主落笔
// （resolved_by=host-rule，写审计），裁不了的保持 open 等调度器唤醒 arbiter。
// 派生钩子异步调用——检测失败不影响意图派生本身。
func (s *Server) scanAndRecordConflicts(taskID, explorationID int64) {
	pg := s.m.pg
	if explorationID <= 0 {
		return
	}
	signals := s.buildIntentSignals(explorationID)
	if len(signals) < 2 {
		return
	}
	for _, f := range intent.Scan(signals) {
		basis, _ := json.Marshal(f.Basis)
		id, created, err := pg.RecordConflict(explorationID, f.SideA.NodeID, f.SideB.NodeID, f.RuleID, basis)
		if err != nil {
			log.Printf("[arbitration] 登记冲突失败 task=%d: %v", taskID, err)
			continue
		}
		if !created {
			continue // 已登记的 open 冲突不重复处理
		}
		if f.Winner != "" {
			// 规则直裁：宿主落笔（host-rule），审计走裁决同款；败方意图联动压制。
			verdict := "proceed_a"
			if f.Winner == "b" {
				verdict = "proceed_b"
			}
			if err := pg.ResolveConflict(id, verdict, f.Reason, "host-rule"); err != nil {
				log.Printf("[arbitration] 规则直裁落笔失败 conflict=%d: %v", id, err)
				continue
			}
			loser := f.SideB.NodeID
			if f.Winner == "b" {
				loser = f.SideA.NodeID
			}
			if err := pg.SuppressIntentForArbitration(loser); err != nil {
				log.Printf("[arbitration] 败方意图压制失败 conflict=%d loser=%d: %v", id, loser, err)
			}
			s.appendArbitrationAudit("intent-arbitration", "rule-resolve",
				fmt.Sprintf("conflict=%d rule=%s winner=%s task=%d", id, f.RuleID, f.Winner, taskID),
				map[string]any{"conflict_id": id, "rule": f.RuleID, "verdict": verdict, "resolved_by": "host-rule"})
		} else {
			s.appendArbitrationAudit("intent-arbitration", "conflict-detected",
				fmt.Sprintf("conflict=%d rule=%s task=%d（待仲裁）", id, f.RuleID, taskID),
				map[string]any{"conflict_id": id, "rule": f.RuleID})
		}
	}
}

// arbiterDeferLimit 是仲裁 agent 连续 defer_both 的熔断阈值（复用 StallTracker
// 的「连续无进展」语义；状态持久在 scheduler_state 键 arbiter_defer:{id}）。
const arbiterDeferLimit = 3

// arbiterDeferKey 是 defer 计数的 scheduler_state 键。
func arbiterDeferKey(conflictID int64) string {
	return fmt.Sprintf("arbiter_defer:%d", conflictID)
}

// resolveIntentConflict 是 arbiter_resolve 工具的宿主落笔（agent 提议 → 这里写）。
// proceed 时联动压制败方意图（open → blocked + arbiter_deferred，0.14.2）；
// defer_both 计数连续达阈值即冻结（等人兜底，唯一进人路径）。
func (s *Server) resolveIntentConflict(conflictID int64, verdict, reason string) error {
	pg := s.m.pg
	if err := pg.ResolveConflict(conflictID, verdict, reason, "arbiter"); err != nil {
		return err
	}
	s.appendArbitrationAudit("intent-arbitration", "arbiter-resolve",
		fmt.Sprintf("conflict=%d verdict=%s", conflictID, verdict),
		map[string]any{"conflict_id": conflictID, "verdict": verdict, "resolved_by": "arbiter", "reason": reason})
	if verdict == "proceed_a" || verdict == "proceed_b" {
		// 败方压制：读冲突行的归一对，proceed_a 压 b / proceed_b 压 a。
		var aNode, bNode int64
		if err := pg.QueryRow(`SELECT a_node_id, b_node_id FROM intent_conflicts WHERE id=$1`, conflictID).Scan(&aNode, &bNode); err == nil {
			loser := bNode
			if verdict == "proceed_b" {
				loser = aNode
			}
			if err := pg.SuppressIntentForArbitration(loser); err != nil {
				log.Printf("[arbitration] 败方意图压制失败 conflict=%d loser=%d: %v", conflictID, loser, err)
			}
		}
	}
	if verdict == "defer_both" {
		n := 0
		if raw, _, _ := pg.GetSetting(arbiterDeferKey(conflictID)); raw != "" {
			n, _ = strconv.Atoi(raw)
		}
		n++
		if n >= arbiterDeferLimit {
			_ = pg.SetSetting(arbiterDeferKey(conflictID), "0")
			if err := pg.FreezeConflict(conflictID); err != nil {
				return err
			}
			s.appendArbitrationAudit("intent-arbitration", "arbiter-frozen",
				fmt.Sprintf("conflict=%d 连续 %d 次 defer_both，冻结等人", conflictID, n),
				map[string]any{"conflict_id": conflictID, "defers": n})
			return nil
		}
		_ = pg.SetSetting(arbiterDeferKey(conflictID), strconv.Itoa(n))
		return nil
	}
	_ = pg.SetSetting(arbiterDeferKey(conflictID), "0")
	return nil
}

// appendArbitrationAudit 是仲裁面动作的审计链双写（source=intent-arbitration）。
func (s *Server) appendArbitrationAudit(tool, action, reason string, body map[string]any) {
	b, _ := json.Marshal(body)
	if _, err := s.m.pg.AppendAuditEvent(db.AuditEvent{
		Actor:    "arbitration",
		ToolName: tool,
		Action:   action,
		Reason:   reason,
		Payload:  b,
		Source:   "intent-arbitration",
	}); err != nil {
		log.Printf("[arbitration] 审计链写入失败（忽略）: %v", err)
	}
}

// dedupSortInt64 去重升序。
func dedupSortInt64(in []int64) []int64 {
	seen := map[int64]bool{}
	out := []int64{}
	for _, v := range in {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	n := len(out)
	for i := 0; i < n; i++ {
		for j := i + 1; j < n; j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

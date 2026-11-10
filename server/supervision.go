package server

// ---------- 监督环（方案 v7：auto 的 supervision tick）----------
//
// 每 3 分钟对每个非终态任务：组装任务快照（确定性、零 LLM）→ 以「监督触发」
// 唤醒 auto——它基于快照调治理工具行动（调 cap/延期/停止/drain/收尾/结束
// 会话），判断「正常」则零动作零成本。
//
// 失败语义：LLM 挂/会话忙 → 静默跳过（监督环是增强层，C4/D 系列确定性兜底
// 照跑）。节流：同任务两次监督间隔 ≥ supervisionInterval。

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/Autumn-27/artex/db"
)

const (
	// supervisionInterval 是同任务两次监督的最小间隔。
	supervisionInterval = 3 * time.Minute
	// supervisionAgent 是监督执行者。
	supervisionAgent = "auto"
)

// supervisionState 是监督环的进程内状态（lastTick per task）。
var supervisionMu sync.Mutex
var supervisionLast = map[string]time.Time{}

// RunSupervisionLoop 是监督环主循环（Server 启动时 go 起一次）。
func (s *Server) RunSupervisionLoop(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	ticks := 0 // W10：30s tick 计数，每 2 个 tick（≈1 分钟）清一次过期预扣
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		// W10 异常路径回收，幂等 best-effort：进程崩在 dispatch 与 settle 之间留下的
		// held 态预扣按 TTL 收回（dispatched=true 按输入估算结算 / false 整块释放）。
		// 失败只记日志不阻断监督环；幂等（已结算/已释放的不再被选中）。
		ticks++
		if ticks%2 == 0 && s.m != nil && s.m.pg != nil {
			if n, err := s.m.pg.SweepExpiredQuotaReservations(time.Now(), 100); err != nil {
				log.Printf("[quota] 过期预扣清扫失败: %v", err)
			} else if n > 0 {
				log.Printf("[quota] 过期预扣清扫回收 %d 条", n)
			}
		}
		s.supervisionPass()
	}
}

// supervisionPass 扫一遍非终态任务，到期的做一次监督唤醒。
func (s *Server) supervisionPass() {
	if s.m == nil || s.m.pg == nil || s.chatAgentRef() == nil {
		return
	}
	tasks, err := s.m.pg.ActiveTaskIDsForSupervision()
	if err != nil {
		return
	}
	now := time.Now()
	for _, id := range tasks {
		supervisionMu.Lock()
		last, ok := supervisionLast[id]
		supervisionMu.Unlock()
		if ok && now.Sub(last) < supervisionInterval {
			continue
		}
		supervisionMu.Lock()
		supervisionLast[id] = now
		supervisionMu.Unlock()
		s.fireSupervision(id)
	}
}

// fireSupervision 组装快照并唤醒 auto（一次监督轮）。
func (s *Server) fireSupervision(taskID string) {
	if s.engine.Drained(taskID) {
		return // 软截止中的任务不再监督打扰
	}
	snapshot := s.buildSupervisionSnapshot(taskID)
	if snapshot == "" {
		return
	}
	numeric, _ := parseTaskID(taskID)
	s.StartTriggeredRun(supervisionAgent,
		fmt.Sprintf("监督 · 任务#%s", taskID),
		supervisionPrompt(snapshot),
		numeric, true, "", "")
}

// eventSupervisionInterval 是事件驱动监督的最小间隔（v9 批次1⑥）：cap 命中等
// 治理事件要求秒级响应，与 3min 轮询节流分离；30s/任务 上限防 cap 风暴。
const eventSupervisionInterval = 30 * time.Second

// eventSupervisionLast 记录每任务最近一次事件驱动监督（进程内即可，重启清零
// 无碍——轮询环仍在兜底）。
var eventSupervisionLast = map[string]time.Time{}

// fireSupervisionEvent 事件直触一次监督轮（v9 批次1⑥，对齐 Claude Code hooks
// 的事件驱动思想）：headline 置顶说明触发原因，快照照常拼装。
func (s *Server) fireSupervisionEvent(taskID, headline string) {
	if s.m == nil || s.m.pg == nil || s.chatAgentRef() == nil {
		return
	}
	if s.engine.Drained(taskID) {
		return
	}
	supervisionMu.Lock()
	last, ok := eventSupervisionLast[taskID]
	now := time.Now()
	if ok && now.Sub(last) < eventSupervisionInterval {
		supervisionMu.Unlock()
		return
	}
	eventSupervisionLast[taskID] = now
	supervisionMu.Unlock()
	snapshot := s.buildSupervisionSnapshot(taskID)
	if snapshot == "" {
		return
	}
	numeric, _ := parseTaskID(taskID)
	s.StartTriggeredRun(supervisionAgent,
		fmt.Sprintf("监督·事件 · 任务#%s", taskID),
		supervisionPrompt(headline+"\n\n"+snapshot),
		numeric, true, "", "")
}

// buildSupervisionSnapshot 确定性组装任务快照（零 LLM）。
func (s *Server) buildSupervisionSnapshot(taskID string) string {
	t, ok := s.m.Task(taskID)
	if !ok || t == nil || t.Store == nil {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "任务 #%s（%s）\n目标：%s\n", t.ID, t.CategoryName, trunc(t.Goal, 150))

	// 意图状态分布。
	dist, _ := s.m.pg.Query(`SELECT state, count(*) FROM exploration_nodes WHERE exploration_id=$1 AND kind='intent' GROUP BY state ORDER BY count(*) DESC`, t.ExpID)
	defer func() {
		if dist != nil {
			dist.Close()
		}
	}()
	b.WriteString("意图状态：")
	first := true
	for dist != nil && dist.Next() {
		var state string
		var n int
		if dist.Scan(&state, &n) == nil {
			if !first {
				b.WriteString("、")
			}
			fmt.Fprintf(&b, "%s=%d", state, n)
			first = false
		}
	}
	if first {
		b.WriteString("（无）")
	}
	b.WriteString("\n")

	// 近 1h 裁决统计 + cap 触发。
	var autoPass, audDeny, human, hardDeny, capHits int
	_ = s.m.pg.QueryRow(`SELECT count(*) FROM sod_decisions WHERE verdict='approve' AND created_at>now()-interval '1 hour'`).Scan(&autoPass)
	_ = s.m.pg.QueryRow(`SELECT count(*) FROM sod_decisions WHERE verdict='deny' AND reason_code='auditor_deny' AND created_at>now()-interval '1 hour'`).Scan(&audDeny)
	_ = s.m.pg.QueryRow(`SELECT count(*) FROM sod_decisions WHERE checks->>'elevated_ask' IS NOT NULL AND created_at>now()-interval '1 hour'`).Scan(&human)
	_ = s.m.pg.QueryRow(`SELECT count(*) FROM sod_decisions WHERE verdict='deny' AND reason_code IN ('destructive_action','platform_baseline','category_deny') AND created_at>now()-interval '1 hour'`).Scan(&hardDeny)
	_ = s.m.pg.QueryRow(`SELECT count(*) FROM sod_decisions WHERE reason_code='category_cap_exceeded' AND created_at>now()-interval '1 hour'`).Scan(&capHits)
	fmt.Fprintf(&b, "近1h裁决：自动放行 %d · auditor 拒 %d · 转人工 %d · 硬拦 %d · cap 触发 %d\n", autoPass, audDeny, human, hardDeny, capHits)

	// 产出增量（自上次监督以来新入账）。
	supervisionMu.Lock()
	lastAt, tracked := supervisionLast[taskID]
	supervisionMu.Unlock()
	if !tracked {
		lastAt = time.Now().Add(-supervisionInterval)
	}
	var newFacts, newFindings int
	_ = s.m.pg.QueryRow(`SELECT count(*) FROM exploration_nodes WHERE exploration_id=$1 AND kind='fact' AND created_at>$2`, t.ExpID, lastAt).Scan(&newFacts)
	_ = s.m.pg.QueryRow(`SELECT count(*) FROM exploration_nodes WHERE exploration_id=$1 AND kind='finding' AND created_at>$2`, t.ExpID, lastAt).Scan(&newFindings)
	fmt.Fprintf(&b, "上次监督以来新增：事实 %d · finding %d\n", newFacts, newFindings)

	// blocked 样本（前 3 条理由）。
	blocked, _ := s.m.pg.Query(`SELECT COALESCE(left(blocked_reason,60),'') FROM exploration_nodes WHERE exploration_id=$1 AND kind='intent' AND state='blocked' ORDER BY id DESC LIMIT 3`, t.ExpID)
	defer func() {
		if blocked != nil {
			blocked.Close()
		}
	}()
	n := 0
	for blocked != nil && blocked.Next() {
		var r string
		if blocked.Scan(&r) == nil {
			fmt.Fprintf(&b, "blocked 样本：%s\n", r)
			n++
		}
	}

	// 预算燃烧（轮次/时长粗口径）。
	var rounds int
	_ = s.m.pg.QueryRow(`SELECT count(*) FROM activity a WHERE a.exploration_id=$1 AND a.kind='round' AND a.created_at>now()-interval '24 hours'`, t.ExpID).Scan(&rounds)
	fmt.Fprintf(&b, "近24h规划轮次：%d\n", rounds)
	return b.String()
}

// supervisionPrompt 是监督触发提示词（独立于主持提示词；治理口径 v7 §3）。
func supervisionPrompt(snapshot string) string {
	var b strings.Builder
	b.WriteString("【监督轮】你是本任务的监督者（治理岗）。以下是任务快照：\n\n")
	b.WriteString(snapshot)
	b.WriteString(`
	你的职责：判断这个任务此刻该【继续 / 转向 / 收尾 / 暂停】，以及拦截种类的 cap 是否需要调整。行动准则：
- 一切正常 → 不调用任何工具，直接简短说明即可（零打扰）。
- 某类调用近 1h cap 频繁触发且理由显示是误伤 → 用 sod_adjust_cap 放宽。
- 某意图仍高产出但即将被预算掐断 → intent_extend。
- 某意图明显跑偏 → intent_stop（理由必填）。
- 目标接近达成 → task_drain(true) 优雅收口；三门满足后 task_conclude。
- 卡点已清除 → rerun_blocked。
- 方向需要调整 → add_task_hint 注入新方向。
- out_of_scope 拒绝集中在文件操作且路径是相对路径（cwd 相对路径会被判定为目标出界）→ 用 add_task_hint 提示 worker：文件操作一律先 cd 到目标目录或改用绝对路径，不要再提交相对路径命令。这是提示纠偏，不是 cap 放宽——范围判定不因此松动。
- 约束：锁定基线类（删除/破坏等）永不可放行；同一次监督里同类调整 ≤1 次；不确定就不动。`)
	return b.String()
}

var _ = db.Activity{}

package server

// 群组预算的两道人可见出口（方案 v3 缺陷 C2）：
//
//  1. 阈值预警（50%/80%）：QuotaGate 在每次结算后回调 onQuotaThreshold，
//     跨线即发一次任务活动 + 已绑定会话播报 + 通知事件（notification_events，
//     与 finding 事件同一条 fan-out/投递链）。
//  2. 预算僵尸闸：noteTaskQuotaOutcome 统计每任务的连续群组预算拒绝数，
//     连续 ≥3 ⇒ 任务落终态（failed）+ 播报 + 通知——预算拒绝是确定性失败，
//     重试到天荒地老也不会成功，必须给任务一个能被看见的结局。
//
// 两个出口的可见面共用一个原则：不依赖人去翻 server 日志。

import (
	"fmt"
	"log"
	"strconv"
	"sync/atomic"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/llmrec"
	"github.com/Autumn-27/artex/notify"
)

// quotaRefusalTerminal 是预算僵尸闸的阈值：同一任务连续这么多次模型调用被
// 群组预算闸拒绝 ⇒ 落终态。取 3 与 planner 心跳重试节奏同量级——够容忍瞬时
// 抖动（其它任务结算释放预算后即恢复），又不至于把预算烧穿在无意义的重试上。
const quotaRefusalTerminal = 3

// sharedQuotaGate 返回进程唯一的群组预算闸门（惰性建，全部 provider 链共享）。
func (s *Server) sharedQuotaGate() *llmrec.QuotaGate {
	s.quotaGateOnce.Do(func() {
		if s.m == nil || s.m.PG() == nil {
			return
		}
		gate := llmrec.NewQuotaGate(s.m.PG(), nil)
		gate.SetBudgetNotifier(s.onQuotaThreshold)
		s.quotaGate = gate
	})
	return s.quotaGate
}

// notifyTaskScoped 把一条预算相关播报写到任务活动流 + 绑定会话 + 通知事件。
// 三路都是 best-effort：任一路失败不影响其余两路（预算可见性不是数据正确性）。
func (s *Server) notifyTaskScoped(taskID, kind, severity string, isError bool, body string) {
	numeric, _ := strconv.ParseInt(taskID, 10, 64)
	if t, ok := s.m.Task(taskID); ok && t != nil {
		s.engine.emitActivity(t, db.Activity{Worker: "system", Kind: "text", IsError: isError, Summary: body})
	}
	if s.m.pg != nil {
		if numeric > 0 {
			if sess, err := s.m.pg.SessionByTask(numeric); err == nil && sess != nil {
				s.say(sess.ID, numeric, "system", "system", body)
			}
		}
		if kind != "" {
			if _, err := s.m.pg.AddNotificationEvent(s.ctx, kind, 0, notify.Snapshot{
				Kind:     kind,
				TaskID:   numeric,
				Name:     kindEventName(kind),
				Severity: severity,
				Summary:  body,
			}); err != nil {
				log.Printf("[quota] 预算事件 %s 写入失败: %v", kind, err)
			}
		}
	}
}

// kindEventName 是通知事件的展示名。
func kindEventName(kind string) string {
	if kind == "task_budget_exhausted" {
		return "群组预算耗尽"
	}
	return "群组预算预警"
}

// onQuotaThreshold 把预算阈值预警转成三路可见（C2）：任务活动流、绑定会话播报、
// 通知事件。每 (群组, 周期, 档) 由 gate 保证至多一次。
func (s *Server) onQuotaThreshold(info db.QuotaSettleInfo, level string) {
	if s.m == nil || s.m.pg == nil {
		return
	}
	body := fmt.Sprintf("【预算预警】群组额度已消耗约 %s%%，剩余不足以支撑长时间任务，请关注或上调预算。", level)
	kind := "task_budget_threshold"
	if info.TaskID != "" {
		s.notifyTaskScoped(info.TaskID, kind, "medium", false, body)
		return
	}
	// 无任务归属的结算（罕见）：只发通知事件。
	if _, err := s.m.pg.AddNotificationEvent(s.ctx, kind, 0, notify.Snapshot{
		Kind: kind, Name: kindEventName(kind), Severity: "medium", Summary: body,
	}); err != nil {
		log.Printf("[quota] 预算预警事件写入失败: %v", err)
	}
}

// quotaRefusalCount 返回任务的连续预算拒绝计数器（惰性建）。
func (s *Server) quotaRefusalCount(taskID string) *atomic.Int64 {
	s.quotaRefusalMu.Lock()
	defer s.quotaRefusalMu.Unlock()
	if s.quotaRefusalCounts == nil {
		s.quotaRefusalCounts = map[string]*atomic.Int64{}
	}
	if c, ok := s.quotaRefusalCounts[taskID]; ok {
		return c
	}
	c := &atomic.Int64{}
	s.quotaRefusalCounts[taskID] = c
	return c
}

// noteTaskQuotaOutcome 记录一次任务模型调用的结局（C2 预算僵尸闸的计数点）：
//   - 成功 ⇒ 计数清零；
//   - 群组预算拒绝 ⇒ 计数 +1；达到 quotaRefusalTerminal ⇒ 落终态（failed）+
//     硬取消在跑的 exec + 播报 + 通知事件。非预算错误不碰计数。
//
// 返回是否刚刚触发了终态（测试断言用）。
func (s *Server) noteTaskQuotaOutcome(taskID string, err error) bool {
	if taskID == "" {
		return false
	}
	counter := s.quotaRefusalCount(taskID)
	if err == nil || !llmrec.IsGroupQuotaMessage(err.Error()) {
		counter.Store(0)
		return false
	}
	if n := counter.Add(1); n < quotaRefusalTerminal {
		return false
	}
	// 只触发一次：计数继续增长时不会再走进来（终态任务已不再调用）。
	if !counter.CompareAndSwap(quotaRefusalTerminal, 0) {
		return false
	}
	body := fmt.Sprintf("【预算耗尽】任务连续 %d 次模型调用被群组预算闸拒绝，已停止运行。请上调群组预算后重跑。", quotaRefusalTerminal)
	log.Printf("[quota] task %s 预算僵尸闸触发：%s", taskID, body)
	if won, terr := s.m.SetTaskStatusGuarded(taskID, "failed"); terr != nil {
		log.Printf("[quota] task %s 落终态失败: %v", taskID, terr)
	} else if won {
		s.engine.cancelExec(taskID, agent.Causef("budget_exhausted", "群组预算耗尽", "%s", body))
	}
	s.notifyTaskScoped(taskID, "task_budget_exhausted", "high", true, body)
	return true
}

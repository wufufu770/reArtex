package server

// A-13（0.15.82）：凭据熔断器的服务端接线。模式同 newLLMHealthRegistry——
// 进程内状态是权威，DB 镜像让冷却窗跨重启存活；断路瞬间补一条进程日志
// （审计/activity 留痕在后续版本接 onOpen，见 A-13 第二批）。

import (
	"fmt"
	"log"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/internal/credbreaker"
)

// newCredBreakerRegistry builds the process-wide credential breaker with DB
// mirroring. pg 为 nil 时退化为纯内存模式（测试/无库环境）。断路/恢复各记一条
// 审计事件（audit_chain）与一条治理留痕（llm_records，后端日志页可见）——
// A-13 第二批（0.15.83）。
func newCredBreakerRegistry(pg *db.DB) *credbreaker.Breaker {
	if pg == nil {
		return credbreaker.New(nil, nil, nil)
	}
	persist := func(k credbreaker.Key, st credbreaker.State) {
		s := db.CredBreakerState{ProfileID: k.ProfileID, Host: k.Host,
			Consecutive: st.Consecutive, LastReason: st.LastReason, LastAt: st.LastAt}
		if !st.OpenUntil.IsZero() {
			t := st.OpenUntil
			s.OpenUntil = &t
		}
		go func() {
			if err := pg.SaveCredBreakerState(s); err != nil {
				log.Printf("[credbreaker] 状态落库失败: %v", err)
			}
		}()
	}
	forget := func(k credbreaker.Key) {
		go func() {
			if err := pg.ClearCredBreakerState(k.ProfileID, k.Host); err != nil {
				log.Printf("[credbreaker] 状态清除失败: %v", err)
			}
		}()
	}
	record := func(action, status, msg string, k credbreaker.Key, st credbreaker.State) {
		payload := mustJSONValue(map[string]any{
			"profile_id": k.ProfileID, "host": k.Host,
			"consecutive": st.Consecutive, "last_reason": st.LastReason,
			"open_until": st.OpenUntil.Format(time.RFC3339),
		})
		_, _ = pg.AppendAuditEvent(db.AuditEvent{
			Actor: "platform", ToolName: "llm", Action: action, Reason: msg,
			Payload: payload, Source: "credbreaker",
		})
		_ = pg.InsertLLMRecord(&db.LLMRecord{Worker: "governance", Model: fmt.Sprintf("profile #%d → %s", k.ProfileID, k.Host), Status: status, Error: msg})
	}
	onOpen := func(k credbreaker.Key, st credbreaker.State) {
		msg := fmt.Sprintf("凭据熔断：profile #%d 对 %s 连续 %d 次 401/403，断路至 %s（更换凭据或等待后自动恢复）",
			k.ProfileID, k.Host, st.Consecutive, st.OpenUntil.Format("15:04"))
		log.Printf("[credbreaker] %s", msg)
		record("cred_breaker.open", "error", msg, k, st)
	}
	onRecover := func(k credbreaker.Key, st credbreaker.State) {
		msg := fmt.Sprintf("凭据恢复：profile #%d 对 %s 的断路窗口因成功调用提前结束", k.ProfileID, k.Host)
		log.Printf("[credbreaker] %s", msg)
		record("cred_breaker.recover", "ok", msg, k, st)
	}
	b := credbreaker.New(persist, forget, onOpen)
	b.OnRecover(onRecover)
	// 只恢复未过期窗口（LoadCredBreakerState 已过滤），冷却完毕的凭据重启后视为健康。
	if rows, err := pg.LoadCredBreakerState(); err == nil {
		for _, r := range rows {
			st := credbreaker.State{Consecutive: r.Consecutive, LastReason: r.LastReason, LastAt: r.LastAt}
			if r.OpenUntil != nil {
				st.OpenUntil = *r.OpenUntil
			}
			b.Restore(credbreaker.Key{ProfileID: r.ProfileID, Host: r.Host}, st)
			log.Printf("[credbreaker] 恢复断路状态：profile #%d 对 %s，冷却至 %s",
				r.ProfileID, r.Host, st.OpenUntil.Format(time.RFC3339))
		}
	}
	return b
}

package intercept

import (
	"context"
	"encoding/json"
	"log"
	"sync/atomic"

	"github.com/Autumn-27/artex/db"
)

// auditAppendFailures 计进程启动以来审计链追加失败的次数（A-05）：
// 链写失败仍不改工具调用的结果（best-effort 不变），但「链缺口」必须可观测——
// 只留一行普通日志的话，缺口会在日志里无声滑过去。
var auditAppendFailures atomic.Int64

// AuditAppendFailures 返回进程启动以来审计链追加失败的次数（/api/metrics 暴露）。
func AuditAppendFailures() int64 { return auditAppendFailures.Load() }

// AppendDecisionEvent 把一次判定动作追加进审计链。
//
// 与 Log 的区别：Log 只在 status=="denied" 时进链（普通 allow 量太大）。
// 平台工具的 allow 量小、需要可追责（「谁在何时建过什么」），故单独暴露这个
// 入口，让短路表的显式放行也留痕。
func (i *Interceptor) AppendDecisionEvent(ctx context.Context, dec Decision, toolName string, input []byte, action string) {
	i.appendAuditEvent(ctx, dec, toolName, input, action)
}

// appendAuditEvent 把一次安全判定追加进审计链（见 db/audit_chain.go）。
//
// 只记「判定动作」：普通 allow 量极大（每次工具调用都可能记一条），而事后真正
// 需要追责的是被拦下的那些 —— deny、ask、timeout。
//
// best-effort：链写失败不能改变工具调用的结果，所以不回传错误；但 A-05 起失败
// 不再是无声的——error 级日志 + 计数器（AuditAppendFailures → /api/metrics 的
// artex_audit_append_failures_total），链缺口一眼可见。
func (i *Interceptor) appendAuditEvent(ctx context.Context, dec Decision, toolName string, input []byte, action string) {
	taskID, agentName, _ := taskInfoFromCtx(ctx)
	payload, err := json.Marshal(map[string]any{
		"task_id":        taskID,
		"rule_id":        dec.RuleID,
		"rule_name":      dec.RuleName,
		"model_fallback": dec.ModelFallback,
		"input_digest":   digestInput(input),
	})
	if err != nil {
		payload = []byte("{}")
	}
	if _, err := i.db.AppendAuditEvent(db.AuditEvent{
		Actor:    agentName,
		ToolName: toolName,
		Action:   action,
		Reason:   dec.Message,
		Payload:  payload,
	}); err != nil {
		auditAppendFailures.Add(1)
		log.Printf("[audit][error] 追加审计链失败（链缺口已计数）: %v", err)
	}
}

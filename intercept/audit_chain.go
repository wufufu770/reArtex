package intercept

import (
	"context"
	"encoding/json"
	"log"

	"github.com/Autumn-27/artex/db"
)

// appendAuditEvent 把一次安全判定追加进审计链（见 db/audit_chain.go）。
//
// 只记「判定动作」：普通 allow 量极大（每次工具调用都可能记一条），而事后真正
// 需要追责的是被拦下的那些 —— deny、ask、timeout。
//
// best-effort：链写失败不能改变工具调用的结果，所以只记日志。
func (i *Interceptor) appendAuditEvent(ctx context.Context, dec Decision, toolName string, input []byte, action string) {
	taskID, agentName := taskInfoFromCtx(ctx)
	payload, err := json.Marshal(map[string]any{
		"task_id":      taskID,
		"rule_id":      dec.RuleID,
		"rule_name":    dec.RuleName,
		"model_fallback": dec.ModelFallback,
		"input_digest": digestInput(input),
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
		log.Printf("[audit] 追加审计链失败: %v", err)
	}
}

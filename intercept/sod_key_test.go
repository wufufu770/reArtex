package intercept

import (
	"context"
	"testing"
)

// E1-5 回归：审批 pending 行落库的 agent 分量优先用 SoD 口径键——它与执行器查
// 「同类放行」凭据的 agent_key 同源同值（agent.SodScopeAgentKey）。
func TestPendingAgentNamePrefersSodKey(t *testing.T) {
	if got := pendingAgentName("worker · #42", "worker#42"); got != "worker#42" {
		t.Fatalf("有 SoD 键时应优先用键，got %q", got)
	}
	if got := pendingAgentName("chat-agent", ""); got != "chat-agent" {
		t.Fatalf("无 SoD 键（会话侧）应退回展示名，got %q", got)
	}
}

// 上下文三分量完整携带：展示名与 SoD 键互不挤占。
func TestTaskContextCarriesSodKey(t *testing.T) {
	ctx := WithTaskContext(context.Background(), "task-9", "worker · #42", "worker#42", nil)
	taskID, agentName, sodKey := taskInfoFromCtx(ctx)
	if taskID != "task-9" || agentName != "worker · #42" || sodKey != "worker#42" {
		t.Fatalf("taskInfoFromCtx = (%q, %q, %q)", taskID, agentName, sodKey)
	}
}

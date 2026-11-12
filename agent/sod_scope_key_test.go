package agent

import "testing"

// E1-5 回归：导出口径是 agent_key 分量的唯一构造来源；写端（engine → intercept
// pending → 人工改判凭据）与查端（SodContextReader → 执行器查表）都共用它。
func TestSodScopeAgentKeyExported(t *testing.T) {
	if got := SodScopeAgentKey(42); got != "worker#42" {
		t.Fatalf("SodScopeAgentKey(42) = %q, want worker#42", got)
	}
	if got := SodScopeAgentKey(0); got != "" {
		t.Fatalf("非执行体的 intentID 应得空串，got %q", got)
	}
	if got := sodScopeAgentKey(RunInfo{IntentID: 42}); got != "worker#42" {
		t.Fatalf("sodScopeAgentKey 委托失败: %q", got)
	}
}

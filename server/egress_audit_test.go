package server

import (
	"errors"
	"testing"

	"github.com/Autumn-27/artex/netguard"
)

// A-01C 纵深：netguard 拒绝审计的识别面钉子——只有「[策略名] 拒绝连接/全部被拒」
// 这两种守卫判定文案进审计链；同前缀的非判定错误与别家错误不冒充守卫判定。
func TestNetguardDenialByPrefix(t *testing.T) {
	// 真实文案形态（netguard.go 拨号 Control + ResolveOnce）。
	deny := netguard.AgentEgress.Name // "agent 出网"
	cases := []struct {
		err  error
		want bool
	}{
		{errors.New("[" + deny + "] 拒绝连接 169.254.169.254（如确需访问，设置 ARTEX_AGENT_EGRESS_ALLOW_LOCAL=1）"), true},
		{errors.New("[" + deny + "] \"example.com\" 解析出的 2 个地址全部被拒（提示）"), true},
		{errors.New("[" + deny + "] 无法解析目标地址 \"bad host\""), false}, // 同前缀非判定
		{errors.New("[MCP 出网] 拒绝连接 169.254.169.254"), false},       // 别家策略
		{errors.New("普通网络错误"), false},
		{nil, false},
	}
	for i, tc := range cases {
		if got := netguardDenialByPrefix(tc.err, deny); got != tc.want {
			t.Fatalf("用例 %d: netguardDenialByPrefix(%v) = %v, want %v", i, tc.err, got, tc.want)
		}
	}
}

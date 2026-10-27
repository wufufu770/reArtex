package server

import "testing"

// ask 规则不配超时 = 「审批页一直没人点就无限等」——而 ask 的语义是
// 「拿不准就问人」，问不到人就等于变相放行。必须给默认超时。
func TestNormalizeAskTimeoutGivesDefault(t *testing.T) {
	req := interceptRuleReq{Action: "ask"}
	normalizeAskTimeout(&req)

	if !req.TimeoutEnabled {
		t.Fatal("ask 规则未配超时时必须补上 timeout_enabled")
	}
	if req.TimeoutSeconds != askRuleDefaultTimeoutSeconds {
		t.Fatalf("ask 规则默认超时应为 %d，得到 %d", askRuleDefaultTimeoutSeconds, req.TimeoutSeconds)
	}
	if req.TimeoutAction != "deny" {
		t.Fatalf("ask 超时默认动作应为 deny，得到 %q", req.TimeoutAction)
	}
}

// 管理员显式配的超时必须原样保留 —— 归一化只补默认值，不覆盖显式意图。
func TestNormalizeAskTimeoutKeepsExplicitConfig(t *testing.T) {
	req := interceptRuleReq{
		Action:         "ask",
		TimeoutEnabled: true,
		TimeoutSeconds: 300,
		TimeoutAction:  "allow",
	}
	normalizeAskTimeout(&req)

	if req.TimeoutSeconds != 300 || req.TimeoutAction != "allow" {
		t.Fatalf("显式配置应原样保留，得到 %ds/%q", req.TimeoutSeconds, req.TimeoutAction)
	}
}

// timeout_enabled 是 bool，JSON 里「未指定」与「显式 false」不可区分，
// 所以对 ask 规则来说超时是强制的：传 false 也照样启用。
func TestNormalizeAskTimeoutForcesTimeoutOnAsk(t *testing.T) {
	req := interceptRuleReq{Action: "ask", TimeoutEnabled: false, TimeoutSeconds: 0}
	normalizeAskTimeout(&req)

	if !req.TimeoutEnabled {
		t.Fatal("ask 规则的超时是强制的，不应因 timeout_enabled=false 而被关闭")
	}
	if req.TimeoutSeconds != askRuleDefaultTimeoutSeconds {
		t.Fatalf("ask 规则默认超时应为 %d，得到 %d", askRuleDefaultTimeoutSeconds, req.TimeoutSeconds)
	}
}

// 非 ask 规则不碰 —— allow/deny 本来就不等审批。
func TestNormalizeAskTimeoutLeavesNonAskAlone(t *testing.T) {
	for _, action := range []string{"allow", "deny"} {
		req := interceptRuleReq{Action: action}
		normalizeAskTimeout(&req)
		if req.TimeoutEnabled || req.TimeoutSeconds != 0 || req.TimeoutAction != "" {
			t.Fatalf("%s 规则不应被归一化，得到 %+v", action, req)
		}
	}
}

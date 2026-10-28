package intercept

import (
	"testing"

	"github.com/Autumn-27/artex/db"
)

// 种子规则照抄了 timeout_enabled=false —— 照抄即「审批无限等待」：
// 审批挂着不回来，人不在的时候这条面就永远卡住。
// 命中 ask 的规则必须带默认超时，且超时动作是 deny（超时放行等于「人不在就通过」）。
func TestMatchAskRuleGetsDefaultTimeout(t *testing.T) {
	dec := matchRule(db.InterceptRule{
		Name:           "种子规则",
		Action:         "ask",
		TimeoutEnabled: false,
		TimeoutSeconds: 0,
		TimeoutAction:  "",
	})
	if !dec.TimeoutEnabled {
		t.Fatal("ask 类规则必须带超时（否则审批无限等待）")
	}
	if dec.TimeoutSeconds != defaultAskTimeoutSecs {
		t.Fatalf("默认超时应为 %d 秒，得到 %d", defaultAskTimeoutSecs, dec.TimeoutSeconds)
	}
	if dec.TimeoutAction != "deny" {
		t.Fatalf("默认超时动作应为 deny，得到 %q", dec.TimeoutAction)
	}
}

// 规则自己配了超时就不动它：管理员的显式配置优先于默认值。
func TestMatchAskRuleKeepsExplicitTimeout(t *testing.T) {
	dec := matchRule(db.InterceptRule{
		Name:           "自定义规则",
		Action:         "ask",
		TimeoutEnabled: true,
		TimeoutSeconds: 300,
		TimeoutAction:  "allow",
	})
	if !dec.TimeoutEnabled || dec.TimeoutSeconds != 300 || dec.TimeoutAction != "allow" {
		t.Fatalf("显式配置的超时不应被默认值覆盖，得到 %+v", dec)
	}
}

// deny 类规则不补超时语义：deny 本来就没有「等审批」这一环。
func TestMatchDenyRuleKeepsNoTimeout(t *testing.T) {
	dec := matchRule(db.InterceptRule{
		Name:   "禁用规则",
		Action: "deny",
	})
	if dec.TimeoutEnabled || dec.TimeoutSeconds != 0 {
		t.Fatalf("deny 档不应带超时语义，得到 %+v", dec)
	}
}

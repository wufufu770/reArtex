package sodpolicy

import "testing"

// TestClassifyEgressScopeHost 钉住范围豁免判定的三类结论与归一化口径。
//
// 这条判定是出网治理的安全边界：清单内 = 平台基础设施（不受任务范围管辖），
// 清单外 = 放行但按未归属留痕，判不出 = fail-closed。三者的处置完全不同，
// 判错方向就是「把被约束的人放进了豁免名单」或「把平台自己的出网拒了」。
func TestClassifyEgressScopeHost(t *testing.T) {
	cases := []struct {
		name       string
		host       string
		wantHost   string
		wantExempt bool
		wantReason string
	}{
		{name: "清单内的确切主机", host: "html.duckduckgo.com", wantHost: "html.duckduckgo.com", wantExempt: true, wantReason: ReasonEgressScopeExempt},
		{name: "URL 形态", host: "https://api.tavily.com/search?q=x", wantHost: "api.tavily.com", wantExempt: true, wantReason: ReasonEgressScopeExempt},
		{name: "带端口", host: "api.anthropic.com:443", wantHost: "api.anthropic.com", wantExempt: true, wantReason: ReasonEgressScopeExempt},
		{name: "大写", host: "API.OpenAI.COM", wantHost: "api.openai.com", wantExempt: true, wantReason: ReasonEgressScopeExempt},
		{name: "尾点", host: "ghcr.io.", wantHost: "ghcr.io", wantExempt: true, wantReason: ReasonEgressScopeExempt},
		{name: "带用户信息", host: "user@api.deepseek.com", wantHost: "api.deepseek.com", wantExempt: true, wantReason: ReasonEgressScopeExempt},
		{name: "清单外的普通主机", host: "example.com", wantHost: "example.com", wantExempt: false, wantReason: ReasonEgressScopeUnattributed},
		{name: "清单内主机的子域不放行", host: "evil.api.tavily.com", wantHost: "evil.api.tavily.com", wantExempt: false, wantReason: ReasonEgressScopeUnattributed},
		{name: "清单内主机名的前缀不放行", host: "api.tavily.com.evil.test", wantHost: "api.tavily.com.evil.test", wantExempt: false, wantReason: ReasonEgressScopeUnattributed},
		{name: "空串判不出来", host: "", wantExempt: false, wantReason: ReasonEgressScopeUnresolved},
		{name: "只有协议头判不出来", host: "https://", wantExempt: false, wantReason: ReasonEgressScopeUnresolved},
		{name: "空白判不出来", host: "   ", wantExempt: false, wantReason: ReasonEgressScopeUnresolved},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyEgressScopeHost(tc.host)
			if got.Exempt != tc.wantExempt {
				t.Fatalf("豁免结论不对: got %v, want %v (reason=%s)", got.Exempt, tc.wantExempt, got.Reason)
			}
			if got.Reason != tc.wantReason {
				t.Fatalf("原因码不对: got %q, want %q", got.Reason, tc.wantReason)
			}
			if got.Host != tc.wantHost {
				t.Fatalf("归一化后的主机不对: got %q, want %q", got.Host, tc.wantHost)
			}
		})
	}
}

// TestEgressScopeExemptHostsCopyIsNotTheSourceOfTruth 钉住「清单不可从外部改写」。
//
// 豁免清单是安全边界。若调用方拿到的是包级切片，一句 append 就能把自己加进
// 豁免名单 ⇒ 清单降级成进程内可变状态 ⇒ 等于「由被它约束的人来调边界」。
func TestEgressScopeExemptHostsCopyIsNotTheSourceOfTruth(t *testing.T) {
	hosts := EgressScopeExemptHosts()
	if len(hosts) == 0 {
		t.Fatalf("豁免清单不该是空的")
	}
	hosts[0] = "attacker.example"
	if EgressScopeHostExempt("attacker.example") {
		t.Fatalf("改写返回的副本不该影响判定")
	}
	if !EgressScopeHostExempt("html.duckduckgo.com") {
		t.Fatalf("清单里的主机仍应被判定为豁免")
	}
}

// TestEgressScopeExemptListMatchesVerdict 钉住清单与判定用的是同一份清单。
//
// 两处各抄一份清单，迟早漂移 ⇒ 判定说「豁免」而清单里没有（或反过来），
// 审计链上的结论就不可复算。
func TestEgressScopeExemptListMatchesVerdict(t *testing.T) {
	hosts := EgressScopeExemptHosts()
	if len(hosts) == 0 {
		t.Fatalf("豁免清单不该是空的")
	}
	for _, h := range hosts {
		if !EgressScopeHostExempt(h) {
			t.Fatalf("清单里的 %q 判出来不是豁免", h)
		}
		verdict := ClassifyEgressScopeHost(h)
		if verdict.Reason != ReasonEgressScopeExempt {
			t.Fatalf("清单里的 %q 原因码不对: %s", h, verdict.Reason)
		}
	}
}

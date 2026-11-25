package agent

import "testing"

// R1（A-15B 收口）：WebSearchReady 是四路共用的联网搜索装配门。
// 独立出口代理缺失即禁用——「空则直连」的旁路在 worker/mainagent/chat 关死，
// 与 planner（A-15）同口径；四路装配点一律经本函数（见各 opts 装配处）。
func TestWebSearchReady(t *testing.T) {
	cases := []struct {
		name string
		o    WebSearchOpts
		want bool
	}{
		{"全关", WebSearchOpts{}, false},
		{"开关开但无独立代理-直连旁路必须关死", WebSearchOpts{Enabled: true}, false},
		{"开关开代理为空白串", WebSearchOpts{Enabled: true, Proxy: "   "}, false},
		{"有代理但开关关", WebSearchOpts{Enabled: false, Proxy: "socks5://10.0.0.1:1080"}, false},
		{"齐全-socks5", WebSearchOpts{Enabled: true, Proxy: "socks5://10.0.0.1:1080"}, true},
		{"齐全-http", WebSearchOpts{Enabled: true, Proxy: "http://10.0.0.1:3128"}, true},
	}
	for _, tc := range cases {
		if got := WebSearchReady(tc.o); got != tc.want {
			t.Fatalf("%s: WebSearchReady(%+v) = %v, want %v", tc.name, tc.o, got, tc.want)
		}
	}
}

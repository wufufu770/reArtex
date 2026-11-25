package agent

import (
	"net"
	"testing"
)

// TestPlannerWebAccessGate pins the A-15 联网门（卡片验收）：proxyAddr="" 或
// taskID<=0 ⇒ fetch/search 全关（不再「开着但没人管」）；搜索独立代理缺失即禁用
// （「空则直连」是要堵的第二条直连旁路）；三者齐全时与原行为一致。
func TestPlannerWebAccessGate(t *testing.T) {
	cases := []struct {
		name          string
		proxyAddr     string
		taskID        int64
		searchEnabled bool
		searchProxy   string
		wantFetch     bool
		wantSearch    bool
	}{
		{"无代理", "", 7, true, "socks5://10.0.0.1:1080", false, false},
		{"无任务身份", "http://127.0.0.1:8788", 0, true, "socks5://10.0.0.1:1080", false, false},
		{"任务号为负", "http://127.0.0.1:8788", -3, true, "socks5://10.0.0.1:1080", false, false},
		{"代理+任务但搜索关", "http://127.0.0.1:8788", 7, false, "", true, false},
		{"代理+任务但搜索无独立代理", "http://127.0.0.1:8788", 7, true, "", true, false},
		{"齐全", "http://127.0.0.1:8788", 7, true, "socks5://10.0.0.1:1080", true, true},
	}
	for _, c := range cases {
		p := &Planner{proxyAddr: c.proxyAddr, webSearch: WebSearchOpts{Enabled: c.searchEnabled, Proxy: c.searchProxy}}
		fetch, search := p.webAccessFor(c.taskID)
		if fetch != c.wantFetch || search != c.wantSearch {
			t.Fatalf("%s: webAccessFor(%d) = (%v,%v)，期望 (%v,%v)", c.name, c.taskID, fetch, search, c.wantFetch, c.wantSearch)
		}
	}
}

// TestCheckProxyReachable pins the A-15 startup preflight：监听中的地址可达
// （含 ProxyAddr() 的 http:// 前缀形态）；空地址/不可解析地址报可读错误——
// Plan 层据此拒绝本轮（fail-closed 到轮次启动期）。
func TestCheckProxyReachable(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if err := checkProxyReachable(l.Addr().String()); err != nil {
		t.Fatalf("监听中的地址应可达: %v", err)
	}
	if err := checkProxyReachable("http://" + l.Addr().String()); err != nil {
		t.Fatalf("带 http:// 前缀的地址应可达: %v", err)
	}
	if err := checkProxyReachable(""); err == nil {
		t.Fatal("空地址必须报错")
	}
	if err := checkProxyReachable("http://256.256.256.256:8080"); err == nil {
		t.Fatal("不可解析地址必须报错")
	}
}

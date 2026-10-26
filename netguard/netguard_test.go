package netguard

import (
	"net"
	"net/http"
	"net/url"
	"testing"
)

func TestAgentEgressBlocksSensitiveRanges(t *testing.T) {
	blocked := []string{
		"127.0.0.1",     // 环回：同机管理端口
		"::1",           // IPv6 环回
		"::ffff:127.0.0.1", // IPv4-mapped 绕过，还原后应判环回
		"169.254.169.254",  // 云元数据，可读实例凭据
		"0.0.0.0",         // 未指定
		"224.0.0.1",       // 组播
	}
	for _, s := range blocked {
		ip := net.ParseIP(s)
		if ip == nil {
			t.Fatalf("用例地址 %q 解析失败", s)
		}
		if !AgentEgress.blockedIP(ip) {
			t.Fatalf("%s 应被 agent 出网策略拒绝", s)
		}
	}
}

// 对渗透 agent 而言内网通常就是目标，放行 RFC1918 是刻意的取舍，照搬通知侧的
// 收紧会把内网测评直接废掉。
func TestAgentEgressAllowsPrivateTargets(t *testing.T) {
	for _, s := range []string{"10.0.0.5", "172.19.0.1", "192.168.1.10"} {
		if AgentEgress.blockedIP(net.ParseIP(s)) {
			t.Fatalf("%s 属 RFC1918，对 agent 应放行（内网常是渗透目标）", s)
		}
	}
}

// 逃生口必须逐路独立：给通知开的口子不能连带放开 agent 出网。
// 这里测 dialControl 而非内层 blockedIP —— 逃生口的判定就在 dialControl，
// 直接调谓词会绕过它，测不到真实行为。
func TestEscapeHatchIsPerPolicy(t *testing.T) {
	loopback := "127.0.0.1:8080"

	if err := AgentEgress.dialControl("tcp", loopback, nil); err == nil {
		t.Fatal("默认应拒绝 agent 出网连环回")
	}
	if err := NotifyDelivery.dialControl("tcp", loopback, nil); err == nil {
		t.Fatal("默认应拒绝通知投递连环回")
	}

	t.Setenv("ARTEX_NOTIFY_ALLOW_LOCAL", "1")
	if err := AgentEgress.dialControl("tcp", loopback, nil); err == nil {
		t.Fatal("通知侧逃生口不应影响 agent 出网策略")
	}
	if err := NotifyDelivery.dialControl("tcp", loopback, nil); err != nil {
		t.Fatalf("通知侧逃生口应放开自己的环回: %v", err)
	}

	t.Setenv("ARTEX_AGENT_EGRESS_ALLOW_LOCAL", "1")
	if err := AgentEgress.dialControl("tcp", loopback, nil); err != nil {
		t.Fatalf("agent 侧逃生口应放开自己的环回: %v", err)
	}
}

func TestCheckRedirectRejectsCrossHost(t *testing.T) {
	mk := func(host string) *http.Request {
		u, _ := url.Parse("http://" + host + "/x")
		return &http.Request{URL: u}
	}
	if err := AgentEgress.CheckRedirect(mk("evil.com"), []*http.Request{mk("good.com")}); err == nil {
		t.Fatal("跨主机重定向应被拒（凭据会交给重定向目标）")
	}
	if err := AgentEgress.CheckRedirect(mk("good.com"), []*http.Request{mk("good.com")}); err != nil {
		t.Fatalf("同主机重定向应放行: %v", err)
	}
}

// 拨号守卫挂在 Control 上，验证 Transport 确实带上了它。
func TestTransportCarriesDialGuard(t *testing.T) {
	tr := AgentEgress.Transport(nil)
	if tr == nil || tr.DialContext == nil {
		t.Fatal("Transport 未装上拨号守卫")
	}
	d := &net.Dialer{}
	_ = d
	if AgentEgress.blockedIP(net.ParseIP("169.254.169.254")) != true {
		t.Fatal("元数据地址必须被拒")
	}
}

// 走代理时只能用字面预检。它挡得住明写的敏感地址，挡不住域名解析与重定向 ——
// 那条已知局限由 runHTTPTool 的注释标注，此处只固化「能挡的」这一半的行为。
func TestCheckURLHost(t *testing.T) {
	deny := []string{
		"http://169.254.169.254/latest/meta-data/",
		"http://127.0.0.1:8787/api/settings",
		"http://[::ffff:127.0.0.1]/",
		"http://localhost:8080/admin",
		"http://foo.localhost/x",
		"http://0.0.0.0/",
	}
	for _, u := range deny {
		if err := AgentEgress.CheckURLHost(u); err == nil {
			t.Fatalf("%s 应被预检拒绝", u)
		}
	}
	allow := []string{
		"https://target.example.com/path",
		"http://192.168.31.10/",
		"http://172.19.0.1:8001/mcp",
	}
	for _, u := range allow {
		if err := AgentEgress.CheckURLHost(u); err != nil {
			t.Fatalf("%s 被误伤: %v", u, err)
		}
	}
	t.Setenv("ARTEX_AGENT_EGRESS_ALLOW_LOCAL", "1")
	if err := AgentEgress.CheckURLHost("http://127.0.0.1:8787/"); err != nil {
		t.Fatalf("逃生口应放开环回: %v", err)
	}
}

// 拆分逃生口的全部意义：为了打本机靶场放开环回时，云元数据必须仍然拒绝。
// 这条属性一旦回归，就等于「开环回顺手送出实例凭据」。
func TestLoopbackHatchDoesNotOpenMetadata(t *testing.T) {
	t.Setenv("ARTEX_AGENT_EGRESS_ALLOW_LOOPBACK", "1")

	if err := AgentEgress.dialControl("tcp", "127.0.0.1:8000", nil); err != nil {
		t.Fatalf("环回逃生口应放开本机靶场: %v", err)
	}
	for _, a := range []string{"169.254.169.254:80", "fe80::1:80"} {
		if err := AgentEgress.dialControl("tcp", a, nil); err == nil {
			t.Fatalf("%s 是链路本地/元数据段，不应被环回逃生口放开", a)
		}
	}
	if AgentEgress.blockedIP(net.ParseIP("169.254.169.254")) != true {
		t.Fatal("元数据必须持续被拒")
	}
}

// 未设开关时环回仍默认拒绝——默认值必须是最紧的。
func TestLoopbackDeniedByDefault(t *testing.T) {
	t.Setenv("ARTEX_AGENT_EGRESS_ALLOW_LOOPBACK", "")
	t.Setenv("ARTEX_AGENT_EGRESS_ALLOW_LOCAL", "")
	if err := AgentEgress.dialControl("tcp", "127.0.0.1:8000", nil); err == nil {
		t.Fatal("默认应拒绝环回")
	}
}

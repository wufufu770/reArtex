package notify

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// 本文件钉住 A-14 的核心论断：SSRF 防护的最终生效点在**拨号阶段**，按连接时刻
// 的真实 IP 判定。配置保存时的校验（validateHTTPURL 只查字面 IP）对域名目标
// 天然是盲的——DNS 重绑定正是打这个盲区：保存时解析到公网 IP，真正连接时同一
// 域名解析到内网。既有测试（ssrf_test.go）覆盖了字面 IP 的两条路径，但没有
// 任何用例驱动过 Control 钩子本身；这里的夹具用「改写解析结果」的 Dialer 模拟
// 重绑定，让守卫在连接层被真实调用。

func TestBlockInternalDialJudgesConnectTimeAddress(t *testing.T) {
	t.Setenv(AllowLocalTargetsEnv, "")
	// Control 钩子收到的是**已完成解析**的 ip:port——下面就是连接时刻的最终地址。
	blocked := []string{
		"127.0.0.1:2379",        // 本机管理端口（etcd 常见）
		"169.254.169.254:80",    // 云元数据端点——可读实例凭据
		"[::1]:8080",            // IPv6 环回
		"[::ffff:127.0.0.1]:80", // IPv4-mapped 环回——归一化后才现形
		"example.test:80",       // 非 IP 地址（真实拨号里不该出现）按拒绝处理
	}
	for _, addr := range blocked {
		if err := blockInternalDial("tcp", addr, nil); err == nil {
			t.Errorf("%s 应在拨号阶段被拒", addr)
		}
	}
	// 公网地址放行——守卫只挡敏感段，不做全网封锁。
	if err := blockInternalDial("tcp", "93.184.216.34:443", nil); err != nil {
		t.Errorf("公网地址不应被拒: %v", err)
	}
	// 逃生口在连接层同样生效（本机 SMTP 中继是合法部署）。
	t.Setenv(AllowLocalTargetsEnv, "1")
	if err := blockInternalDial("tcp", "127.0.0.1:25", nil); err != nil {
		t.Errorf("显式放开后本机地址应可连: %v", err)
	}
}

// TestDNSRebindingCaughtAtDialStage 是 A-14 的验收主体：
// 域名在配置阶段畅通无阻，但把「解析结果」改写到内网地址后，连接必须在建立前被拒。
func TestDNSRebindingCaughtAtDialStage(t *testing.T) {
	t.Setenv(AllowLocalTargetsEnv, "")

	// 配置阶段：域名目标不做字面 IP 判定——保存时一切正常，这正是重绑定的危害所在。
	const domain = "rebound.example.test"
	if err := validateHTTPURL("http://" + domain + "/robot/send"); err != nil {
		t.Fatalf("域名目标应在配置阶段通过（字面 IP 判定不覆盖域名）: %v", err)
	}

	// 模拟「解析时公网、连接时内网」：DialContext 把域名改写到环回地址——
	// Control 钩子（blockInternalDial）拿到的是改写后的**真实连接地址**，
	// 必须在 TCP 握手前拒绝。这段改写只替代 DNS 解析这一步，判定逻辑
	// （Control 钩子按最终 IP 判）走的是生产同一条代码路径。
	tr := notifyTransport.Clone()
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if h, p, err := net.SplitHostPort(addr); err == nil && h == domain {
			addr = net.JoinHostPort("127.0.0.1", p)
		}
		return (&net.Dialer{Timeout: 2 * time.Second, Control: blockInternalDial}).DialContext(ctx, network, addr)
	}
	client := &http.Client{Timeout: 5 * time.Second, Transport: tr}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		"http://"+domain+"/robot/send", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	resp, err := client.Do(req)
	if err == nil {
		resp.Body.Close()
		t.Fatal("重绑定到内网地址应在拨号阶段被拒")
	}
	// 拒绝必须来自拨号守卫本身（而不是 DNS 解析失败之类的别的错误）——
	// 否则这条用例证明不了「按连接时刻 IP 判定」。
	if !strings.Contains(err.Error(), "拒绝投递到本机") {
		t.Errorf("拒绝应来自拨号守卫，得到: %v", err)
	}
}

// TestRebindFixturePositiveControl 是上一条的反向对照：同样的改写手法、
// 解析结果指向可达的假接收端且显式放开环回时，请求应当成功——证明拒绝
// 确实出自守卫的判定，而不是夹具把路堵死了。
func TestRebindFixturePositiveControl(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer srv.Close()

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("解析 httptest 地址失败: %v", err)
	}
	port := u.Port()
	if port == "" {
		t.Fatalf("httptest 地址应带端口: %s", srv.URL)
	}

	t.Setenv(AllowLocalTargetsEnv, "1")
	const domain = "normal.example.test"
	tr := notifyTransport.Clone()
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if h, _, err := net.SplitHostPort(addr); err == nil && h == domain {
			addr = net.JoinHostPort("127.0.0.1", port)
		}
		return (&net.Dialer{Timeout: 2 * time.Second, Control: blockInternalDial}).DialContext(ctx, network, addr)
	}
	client := &http.Client{Timeout: 5 * time.Second, Transport: tr}
	resp, err := client.Post("http://"+domain+"/robot/send", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("显式放开后改写目标应可达: %v", err)
	}
	resp.Body.Close()
}

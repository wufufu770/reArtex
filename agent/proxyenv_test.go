package agent

import (
	"net/url"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/traffic"
)

// proxyEnvValue 从 env 里取某个变量的值。
func proxyEnvValue(env []string, key string) string {
	for _, e := range env {
		if strings.HasPrefix(e, key+"=") {
			return strings.TrimPrefix(e, key+"=")
		}
	}
	return ""
}

func TestProxyEnvEmptyIsNil(t *testing.T) {
	if env := proxyEnv("", ""); env != nil {
		t.Fatalf("proxyEnv(\"\", \"\") = %v, want nil (direct)", env)
	}
}

func TestProxyEnvSetsAllProxyForSocks5(t *testing.T) {
	// Capture-off egress path: a socks5 proxy, no MITM CA. ALL_PROXY must be set
	// (curl reads socks5 only from there), and no CA vars should appear.
	env := proxyEnv("socks5://10.0.0.1:1080", "")
	has := func(prefix string) bool {
		for _, e := range env {
			if strings.HasPrefix(e, prefix) {
				return true
			}
		}
		return false
	}
	for _, want := range []string{"HTTP_PROXY=", "HTTPS_PROXY=", "ALL_PROXY=", "all_proxy="} {
		if !has(want) {
			t.Errorf("proxyEnv missing %s: %v", want, env)
		}
	}
	if has("SSL_CERT_FILE=") || has("CURL_CA_BUNDLE=") {
		t.Errorf("proxyEnv without CA must not inject CA vars: %v", env)
	}
}

func TestProxyEnvInjectsCAWhenRecording(t *testing.T) {
	env := proxyEnv("http://127.0.0.1:8788", "/data/ca.pem")
	has := func(prefix string) bool {
		for _, e := range env {
			if strings.HasPrefix(e, prefix) {
				return true
			}
		}
		return false
	}
	for _, want := range []string{"SSL_CERT_FILE=", "CURL_CA_BUNDLE=", "REQUESTS_CA_BUNDLE=", "NODE_EXTRA_CA_CERTS="} {
		if !has(want) {
			t.Errorf("proxyEnv with CA missing %s: %v", want, env)
		}
	}
}

// TestProxyEnvCarriesTaskIdentity pins that the Bash egress carries a task
// identity the recording proxy can attribute: 没有身份就没有任务范围判定。
func TestProxyEnvCarriesTaskIdentity(t *testing.T) {
	env := proxyEnv("http://127.0.0.1:8788", "", 42)
	raw := proxyEnvValue(env, "HTTP_PROXY")
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("代理地址解析失败: %v", err)
	}
	if u.User == nil {
		t.Fatalf("带任务出网必须在代理 URL 里带上身份令牌: %s", raw)
	}
	token, _ := u.User.Password()
	if token == "" {
		token = u.User.Username()
	}
	if token != traffic.SignEgressTaskToken(42) {
		t.Fatalf("身份令牌不对: %q", token)
	}
	// 另一个任务必须是另一个令牌：代理据此区分归属。
	other := proxyEnvValue(proxyEnv("http://127.0.0.1:8788", "", 43), "HTTP_PROXY")
	if other == raw {
		t.Fatalf("不同任务必须带不同身份令牌: %s", other)
	}
}

// TestProxyEnvWithoutTaskHasNoIdentity pins that an unattributed egress stays
// unattributed（不被伪装成某个任务）。
func TestProxyEnvWithoutTaskHasNoIdentity(t *testing.T) {
	raw := proxyEnvValue(proxyEnv("http://127.0.0.1:8788", ""), "HTTP_PROXY")
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("代理地址解析失败: %v", err)
	}
	if u.User != nil {
		t.Fatalf("没有任务时不该带身份令牌: %s", raw)
	}
}

// TestProxyEnvKeepsSocks5ProxyIntact pins that the identity token is only added
// to HTTP(S) proxies: socks5 的 userinfo 是另一种认证，塞进去会把代理弄坏。
func TestProxyEnvKeepsSocks5ProxyIntact(t *testing.T) {
	raw := proxyEnvValue(proxyEnv("socks5://10.0.0.1:1080", "", 42), "ALL_PROXY")
	if raw != "socks5://10.0.0.1:1080" {
		t.Fatalf("socks5 代理地址被改动了: %s", raw)
	}
}

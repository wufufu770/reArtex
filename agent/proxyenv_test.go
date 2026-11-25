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

// TestValidateProxyEnvAcceptsWellFormedEnv pins the happy path: proxyEnv 自己
// 装配的 env 必须原样通过校验——装配与校验互为表里，校验失败即装配回归。
func TestValidateProxyEnvAcceptsWellFormedEnv(t *testing.T) {
	if err := validateProxyEnv(nil, "", 0); err != nil {
		t.Fatalf("直连模式（无代理）无事可查，不应报错: %v", err)
	}
	env := proxyEnv("http://127.0.0.1:8788", "", 42)
	if err := validateProxyEnv(env, "http://127.0.0.1:8788", 42); err != nil {
		t.Fatalf("装配正确的带任务 env 必须通过校验: %v", err)
	}
	env = proxyEnv("http://127.0.0.1:8788", "")
	if err := validateProxyEnv(env, "http://127.0.0.1:8788", 0); err != nil {
		t.Fatalf("装配正确的无任务 env 必须通过校验: %v", err)
	}
}

// TestValidateProxyEnvRejectsClearedOrRewritten pins the A-01 phase-1 contract:
// 代理在用而代理变量被清空/改写 ⇒ 校验必须失败并点名变量——宁可这个 run 不启动，
// 也不能让一次改道/无身份的出网溜出去。
func TestValidateProxyEnvRejectsClearedOrRewritten(t *testing.T) {
	addr := "http://127.0.0.1:8788"
	good := proxyEnv(addr, "", 42)

	// 清空一个变量 → 拒，报文点名变量。
	cleared := make([]string, 0, len(good))
	for _, kv := range good {
		if strings.HasPrefix(kv, "HTTP_PROXY=") {
			continue
		}
		cleared = append(cleared, kv)
	}
	err := validateProxyEnv(cleared, addr, 42)
	if err == nil {
		t.Fatal("代理变量被清空必须校验失败")
	}
	if !strings.Contains(err.Error(), "HTTP_PROXY") {
		t.Fatalf("失败原因必须点名缺失的变量: %v", err)
	}

	// 改写一个变量 → 拒，报文点名变量与实际值。
	rewritten := make([]string, 0, len(good))
	for _, kv := range good {
		if strings.HasPrefix(kv, "HTTPS_PROXY=") {
			kv = "HTTPS_PROXY=http://evil.example.com:3128"
		}
		rewritten = append(rewritten, kv)
	}
	err = validateProxyEnv(rewritten, addr, 42)
	if err == nil {
		t.Fatal("代理变量被改写必须校验失败")
	}
	if !strings.Contains(err.Error(), "HTTPS_PROXY") || !strings.Contains(err.Error(), "evil.example.com") {
		t.Fatalf("失败原因必须点名被改写的变量与实际值: %v", err)
	}

	// 换成别的任务的令牌 → 拒（身份必须属于本任务）。
	wrongTask := make([]string, 0, len(good))
	for _, kv := range good {
		if strings.HasPrefix(kv, "ALL_PROXY=") {
			kv = "ALL_PROXY=" + egressProxyURL(addr, 43)
		}
		wrongTask = append(wrongTask, kv)
	}
	if err := validateProxyEnv(wrongTask, addr, 42); err == nil {
		t.Fatal("携带别的任务令牌必须校验失败")
	}

	// NODE_USE_ENV_PROXY 丢了 → 拒（Node 系工具会无视代理变量直连）。
	noNode := make([]string, 0, len(good))
	for _, kv := range good {
		if strings.HasPrefix(kv, "NODE_USE_ENV_PROXY=") {
			continue
		}
		noNode = append(noNode, kv)
	}
	if err := validateProxyEnv(noNode, addr, 42); err == nil || !strings.Contains(err.Error(), "NODE_USE_ENV_PROXY") {
		t.Fatalf("NODE_USE_ENV_PROXY 缺失必须校验失败并点名: %v", err)
	}
}

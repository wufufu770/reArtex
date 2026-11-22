package sod

import (
	"testing"
	"time"

	"github.com/Autumn-27/artex/internal/sodpolicy"
)

// sodpolicyCredentialWindowForTest 取生产窗口常量做回拨基准（超窗口 1 分钟）。
var sodpolicyCredentialWindowForTest = sodpolicy.CredentialOpenWindow

// seedCredentialRegistry 给直构的 Chain 注入凭据面注册表（绕开 DB：单测直接
// 预填缓存并压住 TTL 时间戳）。注册表语义见 chain.go credToolsCache 注释。
func seedCredentialRegistry(c *Chain, tools ...string) {
	set := map[string]bool{}
	for _, t := range tools {
		set[t] = true
	}
	c.credToolsCache = set
	c.credToolsAt = time.Now()
}

// 凭据熔断状态机回归（0.15.44 #6，0.15.47 注册制收窄）：**注册过的**工具连续
// 2 次 401/403 → 开路 30 分钟；非命中响应清零；执行层错误不进凭据面；冷却
// 结束自动折算关闭；工具间独立。
func TestCredentialCircuitOpensAfterTwoAuthFailures(t *testing.T) {
	c := &Chain{}
	seedCredentialRegistry(c, "WebFetch")
	c.noteCredentialOutcome("WebFetch", []byte("HTTP/1.1 401 Unauthorized"), false)
	if c.credentialCircuitOpen("WebFetch") {
		t.Fatal("1 次 401 不应开路（渗透测试里 401 是常见读数）")
	}
	c.noteCredentialOutcome("WebFetch", []byte(`{"error":"status 403 Forbidden"}`), false)
	if !c.credentialCircuitOpen("WebFetch") {
		t.Fatal("连续 2 次 401/403 应开路")
	}
	// 工具间独立：别的工具不受影响（未注册 → 永不进凭据面）。
	if c.credentialCircuitOpen("nmap") {
		t.Fatal("其他工具不应被牵连")
	}
}

func TestCredentialCircuitResetOnHealthyResponse(t *testing.T) {
	c := &Chain{}
	seedCredentialRegistry(c, "WebFetch")
	c.noteCredentialOutcome("WebFetch", []byte("401 Unauthorized"), false)
	c.noteCredentialOutcome("WebFetch", []byte("200 OK"), false)
	if c.credentialCircuitOpen("WebFetch") {
		t.Fatal("正常响应应清零计数")
	}
	c.noteCredentialOutcome("WebFetch", []byte("401 Unauthorized"), false)
	if c.credentialCircuitOpen("WebFetch") {
		t.Fatal("清零后单次 401 不应开路（计数被 200 重置）")
	}
}

func TestCredentialCircuitIgnoresExecErrors(t *testing.T) {
	c := &Chain{}
	seedCredentialRegistry(c, "WebFetch")
	c.noteCredentialOutcome("WebFetch", []byte("anything"), true)
	c.noteCredentialOutcome("WebFetch", []byte("anything"), true)
	if c.credentialCircuitOpen("WebFetch") {
		t.Fatal("执行层错误应归工具健康熔断，不进凭据面")
	}
}

func TestCredentialCircuitCooldownFoldsClosed(t *testing.T) {
	c := &Chain{}
	seedCredentialRegistry(c, "WebFetch")
	c.noteCredentialOutcome("WebFetch", []byte("401 Unauthorized"), false)
	c.noteCredentialOutcome("WebFetch", []byte("403 Forbidden"), false)
	if !c.credentialCircuitOpen("WebFetch") {
		t.Fatal("前置：应已开路")
	}
	// 手动回拨 openedAt 到窗口之外 → 冷却折算关闭并清零。
	v, _ := c.credCircuit.Load("WebFetch")
	st := v.(*credCircuitState)
	st.mu.Lock()
	st.openedAt = time.Now().Add(-sodpolicyCredentialWindowForTest - time.Minute)
	st.mu.Unlock()
	if c.credentialCircuitOpen("WebFetch") {
		t.Fatal("冷却结束应折算关闭")
	}
	st.mu.Lock()
	zero := st.count == 0 && st.openedAt.IsZero()
	st.mu.Unlock()
	if !zero {
		t.Fatal("折算关闭应同时清零状态")
	}
	// 清零后单次 401 不应开路。
	c.noteCredentialOutcome("WebFetch", []byte("401 Unauthorized"), false)
	if c.credentialCircuitOpen("WebFetch") {
		t.Fatal("清零后单次 401 不应开路")
	}
}

// 0.15.47 收窄的误伤回归钉子：**未注册的工具永不进凭据面**。真实渗透里目标
// 返回的 401/403 是核心数据（鉴权探测正是靠它），0.15.44 的全工具记账会把
// curl 这类主通道整只熔断 30 分钟——注册制下未注册工具无论撞多少次 401，
// 状态机都不可达（credCircuit 里连条目都不该出现）。
func TestCredentialCircuitDormantForUnregisteredTools(t *testing.T) {
	c := &Chain{} // 注册表为空（默认）⇒ 熔断完全休眠
	c.noteCredentialOutcome("curl", []byte("HTTP/1.1 401 Unauthorized"), false)
	c.noteCredentialOutcome("curl", []byte("403 Forbidden"), false)
	c.noteCredentialOutcome("curl", []byte("401 Unauthorized"), false)
	if c.credentialCircuitOpen("curl") {
		t.Fatal("未注册工具不得进入凭据熔断（真实渗透的目标 401/403 是数据）")
	}
	if _, loaded := c.credCircuit.Load("curl"); loaded {
		t.Fatal("未注册工具不应在凭据熔断状态表里留下条目")
	}
	// 同一链上注册表内的工具照常工作（注册制只收窄面，不废状态机）。
	seedCredentialRegistry(c, "WebFetch")
	c.noteCredentialOutcome("WebFetch", []byte("401 Unauthorized"), false)
	c.noteCredentialOutcome("WebFetch", []byte("403 Forbidden"), false)
	if !c.credentialCircuitOpen("WebFetch") {
		t.Fatal("注册过的工具应照常熔断")
	}
}

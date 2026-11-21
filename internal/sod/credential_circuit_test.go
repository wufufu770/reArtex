package sod

import (
	"testing"
	"time"

	"github.com/Autumn-27/artex/internal/sodpolicy"
)

// sodpolicyCredentialWindowForTest 取生产窗口常量做回拨基准（超窗口 1 分钟）。
var sodpolicyCredentialWindowForTest = sodpolicy.CredentialOpenWindow

// 凭据熔断状态机回归（0.15.44 #6）：同一工具连续 2 次 401/403 → 开路 30 分钟；
// 非命中响应清零；执行层错误不进凭据面；冷却结束自动折算关闭；工具间独立。
func TestCredentialCircuitOpensAfterTwoAuthFailures(t *testing.T) {
	c := &Chain{}
	c.noteCredentialOutcome("WebFetch", []byte("HTTP/1.1 401 Unauthorized"), false)
	if c.credentialCircuitOpen("WebFetch") {
		t.Fatal("1 次 401 不应开路（渗透测试里 401 是常见读数）")
	}
	c.noteCredentialOutcome("WebFetch", []byte(`{"error":"status 403 Forbidden"}`), false)
	if !c.credentialCircuitOpen("WebFetch") {
		t.Fatal("连续 2 次 401/403 应开路")
	}
	// 工具间独立：别的工具不受影响。
	if c.credentialCircuitOpen("nmap") {
		t.Fatal("其他工具不应被牵连")
	}
}

func TestCredentialCircuitResetOnHealthyResponse(t *testing.T) {
	c := &Chain{}
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
	c.noteCredentialOutcome("WebFetch", []byte("anything"), true)
	c.noteCredentialOutcome("WebFetch", []byte("anything"), true)
	if c.credentialCircuitOpen("WebFetch") {
		t.Fatal("执行层错误应归工具健康熔断，不进凭据面")
	}
}

func TestCredentialCircuitCooldownFoldsClosed(t *testing.T) {
	c := &Chain{}
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

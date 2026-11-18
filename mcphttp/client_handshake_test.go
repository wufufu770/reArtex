package mcphttp

// 0.15.13 握手看门狗回归：卡死的 MCP server（接受连接但不发握手数据）此前会把
// NewSSE 无限挂死——装配路径不返回就进不了失败冷却，worker 领取队列被堵死。
// 看门狗必须在预算内返回错误。顺带钉住 newHTTPClient 的 Transport 语义
// （clone DefaultTransport 而非裸建）。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestNewSSEStalledServerHandshakeTimeout(t *testing.T) {
	t.Setenv("ARTEX_MCP_EGRESS_ALLOW_LOOPBACK", "1")
	old := handshakeTimeout
	handshakeTimeout = 300 * time.Millisecond
	t.Cleanup(func() { handshakeTimeout = old })

	// 接受连接、什么都不发——旧实现会永远卡在 GET /sse / 端点公告。
	stalled := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
	}))
	defer stalled.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	c, err := NewSSE(ctx, "test", stalled.URL+"/sse", nil, false)
	elapsed := time.Since(start)
	if err == nil {
		if c != nil {
			_ = c.Close()
		}
		t.Fatal("卡死的 server 必须在握手预算内报错，而不是挂死")
	}
	if elapsed > 5*time.Second {
		t.Fatalf("握手看门狗未生效：耗时 %v", elapsed)
	}
}

func TestNewSSECallerCancelAbortsHandshake(t *testing.T) {
	t.Setenv("ARTEX_MCP_EGRESS_ALLOW_LOOPBACK", "1")
	old := handshakeTimeout
	handshakeTimeout = 30 * time.Second
	t.Cleanup(func() { handshakeTimeout = old })

	stalled := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
	}))
	defer stalled.Close()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()
	if _, err := NewSSE(ctx, "test", stalled.URL+"/sse", nil, false); err == nil {
		t.Fatal("调用方取消握手必须中止并报错（看门狗之外的另一重保险）")
	}
}

func TestNewHTTPClientClonesDefaultTransport(t *testing.T) {
	hc := newHTTPClient(0, false)
	tr, ok := hc.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport 类型: %T", hc.Transport)
	}
	// netguard.Transport 包裹后 DialContext 被替换为拨号守卫，但 clone 来的其余
	// 默认值必须保留——裸 &http.Transport{} 的这几项全是 0（审查实锤）。
	if tr.TLSHandshakeTimeout <= 0 {
		t.Fatal("TLSHandshakeTimeout 丢失（clone DefaultTransport 的意义所在）")
	}
	if tr.IdleConnTimeout <= 0 {
		t.Fatal("IdleConnTimeout 丢失")
	}
	if tr.ExpectContinueTimeout <= 0 {
		t.Fatal("ExpectContinueTimeout 丢失")
	}
}

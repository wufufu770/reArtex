package mcphttp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// mcpTestServer 起一个最小 MCP 服务端（initialize + tools/list + tools/call）。
// 夹具用 httptest 的本机地址，不写真实 MCP 服务地址：这些文件会进版本库，
// 真实地址会连带暴露本机 MCP 拓扑。
func mcpTestServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

func mcpHandler(tools []any) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			ID     int    `json:"id"`
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		var result any
		switch body.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "serverInfo": map[string]any{"name": "test", "version": "0.1"}}
		case "tools/list":
			result = map[string]any{"tools": tools}
		case "tools/call":
			result = map[string]any{"content": []any{map[string]any{"type": "text", "text": "ok"}}}
		default:
			result = map[string]any{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": body.ID, "result": result})
	}
}

// TestMCPClientReachesInScopeDestination pins that the netguard-wrapped client
// still reaches a legitimate MCP server: 出网治理不能把正常连接也一起挡掉。
func TestMCPClientReachesInScopeDestination(t *testing.T) {
	t.Setenv("ARTEX_MCP_EGRESS_ALLOW_LOOPBACK", "1")
	srv := mcpTestServer(t, mcpHandler([]any{map[string]any{"name": "echo", "description": "echo", "inputSchema": map[string]any{"type": "object"}}}))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := New(ctx, "test", srv.URL+"/mcp", map[string]string{}, false)
	if err != nil {
		t.Fatalf("范围内目的地必须能连上: %v", err)
	}
	defer c.Close()
	got, err := c.Call(ctx, "echo", map[string]any{"value": "x"})
	if err != nil || got != "ok" {
		t.Fatalf("Call() = %q, %v", got, err)
	}
}

// TestMCPClientDeniesOutOfScopeDestination pins the fail-closed half: a
// destination the policy rejects must never be dialed. 云元数据地址是判据里
// 写死要拒的段，用它当「范围外目的地」不需要真造一个外部站点。
func TestMCPClientDeniesOutOfScopeDestination(t *testing.T) {
	srv := mcpTestServer(t, mcpHandler(nil))
	// 把地址换成策略必拒的元数据段，端口仍指向本机服务端（万一真连上了，
	// 断言也会因为「不该连上」而失败）。
	out := strings.Replace(srv.URL, "127.0.0.1", "169.254.169.254", 1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := New(ctx, "test", out+"/mcp", map[string]string{}, false)
	if err == nil {
		c.Close()
		t.Fatal("范围外目的地必须被拒")
	}
	if !strings.Contains(err.Error(), "拒绝访问") {
		t.Fatalf("拒绝原因必须写明是策略拒绝，实际：%v", err)
	}
}

// TestMCPClientDeniesRedirectToOtherHost pins that a redirect is not a way out:
// 跨主机跳转会把手上的凭据交给另一个目标，一律拒绝。
func TestMCPClientDeniesRedirectToOtherHost(t *testing.T) {
	t.Setenv("ARTEX_MCP_EGRESS_ALLOW_LOOPBACK", "1")
	srv := mcpTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/mcp" {
			w.Header().Set("Location", "http://169.254.169.254/latest/meta-data/")
			w.WriteHeader(http.StatusFound)
			return
		}
		mcpHandler(nil)(w, r)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := New(ctx, "test", srv.URL+"/mcp", map[string]string{}, false)
	if err == nil {
		c.Close()
		t.Fatal("跳转到范围外主机必须被拒")
	}
	if !strings.Contains(err.Error(), "重定向") {
		t.Fatalf("拒绝原因必须写明是重定向，实际：%v", err)
	}
}

// TestMCPClientTransportCarriesDialGuard pins that the client really is built
// on the guarded transport（而不是只做了一次字面预检）：拨号期守卫在 transport 上。
func TestMCPClientTransportCarriesDialGuard(t *testing.T) {
	t.Setenv("ARTEX_MCP_EGRESS_ALLOW_LOOPBACK", "1")
	srv := mcpTestServer(t, mcpHandler(nil))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := New(ctx, "test", srv.URL+"/mcp", map[string]string{}, false)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	tr, ok := c.http.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("MCP 客户端的 transport 类型不对: %T", c.http.Transport)
	}
	if tr.DialContext == nil {
		t.Fatal("transport 未装上拨号守卫")
	}
	if c.http.CheckRedirect == nil {
		t.Fatal("客户端未装上重定向检查")
	}
}

// TestMCPClientKeepsInsecureTLSForSelfSigned pins the deliberate exception:
// 自签证书的 MCP 服务端仍要能连（收紧 TLS 是另一件事，不在这里夹带）。
func TestMCPClientKeepsInsecureTLSForSelfSigned(t *testing.T) {
	t.Setenv("ARTEX_MCP_EGRESS_ALLOW_LOOPBACK", "1")
	srv := httptest.NewTLSServer(mcpHandler(nil))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := New(ctx, "test", srv.URL+"/mcp", map[string]string{}, true)
	if err != nil {
		t.Fatalf("自签证书的 MCP 服务端必须能连上: %v", err)
	}
	defer c.Close()
	if _, err := c.Call(ctx, "echo", map[string]any{}); err != nil {
		t.Fatalf("Call() 失败: %v", err)
	}
}

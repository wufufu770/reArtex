package mcphttp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestLegacySSEClientDiscoverAndCall(t *testing.T) {
	var mu sync.Mutex
	var stream http.ResponseWriter
	var flusher http.Flusher
	sessions := make(map[string]bool)
	messages := make(chan []byte, 8)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/sse":
			id := "test-session"
			mu.Lock()
			sessions[id] = true
			stream, flusher = w, w.(http.Flusher)
			mu.Unlock()
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			fmt.Fprintf(w, "event: endpoint\ndata: %s/message?sessionId=%s\n\n", "http://"+r.Host, id)
			flusher.Flush()
			for {
				select {
				case payload := <-messages:
					fmt.Fprintf(w, "event: message\ndata: %s\n\n", payload)
					flusher.Flush()
				case <-r.Context().Done():
					return
				}
			}
		case "/message":
			mu.Lock()
			knownSession := sessions["test-session"]
			mu.Unlock()
			if r.URL.Query().Get("sessionId") != "test-session" || !knownSession {
				http.Error(w, "unknown session", http.StatusBadRequest)
				return
			}
			var req rpcRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			w.WriteHeader(http.StatusAccepted)
			if req.ID == 0 {
				return
			}
			var result any
			switch req.Method {
			case "initialize":
				result = map[string]any{"protocolVersion": "2024-11-05", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]any{"name": "test", "version": "1"}}
			case "tools/list":
				result = map[string]any{"tools": []any{map[string]any{"name": "echo", "description": "echo input", "inputSchema": map[string]any{"type": "object"}}}}
			case "tools/call":
				result = map[string]any{"content": []any{map[string]any{"type": "text", "text": "ok"}}}
			default:
				result = map[string]any{}
			}
			resultJSON, err := json.Marshal(result)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			payload, _ := json.Marshal(rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: json.RawMessage(resultJSON)})
			mu.Lock()
			streamReady := stream != nil
			mu.Unlock()
			if streamReady {
				messages <- payload
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := NewSSE(ctx, "test", srv.URL+"/sse", map[string]string{"Authorization ": "Bearer test"}, false)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	tools, err := c.Tools(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 1 || !strings.Contains(tools[0].Name(), "echo") {
		t.Fatalf("unexpected tools: %#v", tools)
	}
	got, err := c.Call(ctx, "echo", map[string]any{"value": "x"})
	if err != nil || got != "ok" {
		t.Fatalf("Call() = %q, %v", got, err)
	}
}

func TestReadSSEEndpointSameOrigin(t *testing.T) {
	// 夹具用 RFC 5737 文档保留地址，不写真实 MCP 服务地址：
	// 这些文件会进版本库，而真实地址会连带暴露本机 MCP 拓扑。
	const base = "http://203.0.113.10:9001/sse"
	cases := []struct {
		name    string
		data    string
		want    string
		wantErr string
	}{
		{name: "同源绝对地址(MCP 规范用法)", data: "http://203.0.113.10:9001/message?sessionId=x", want: "http://203.0.113.10:9001/message?sessionId=x"},
		{name: "同源相对路径", data: "/message?sessionId=x", want: "http://203.0.113.10:9001/message?sessionId=x"},
		{name: "同源且大小写不同", data: "http://203.0.113.10:9001/message", want: "http://203.0.113.10:9001/message"},
		{name: "跨主机", data: "http://evil.example.com/message", wantErr: "跨源端点被拒绝"},
		{name: "跨端口", data: "http://203.0.113.10:9999/message", wantErr: "跨源端点被拒绝"},
		{name: "跨 scheme", data: "https://203.0.113.10:9001/message", wantErr: "跨源端点被拒绝"},
		{name: "带 userinfo", data: "http://u:p@203.0.113.10:9001/message", wantErr: "拒绝带 userinfo"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stream := "event: endpoint\ndata: " + tc.data + "\n\n"
			got, err := readSSEEndpoint(bufio.NewReader(strings.NewReader(stream)), base)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("期望被拒绝，实际放行: %q", got)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("错误信息未含 %q: %v", tc.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("不应被拒绝: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

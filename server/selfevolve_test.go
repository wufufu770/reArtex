package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 自进化可见面（0.15.41）回归：
//   ① status 在 amber 关闭时也要 200（总览的意义是展示「哪里断了」，不是抛 500）；
//   ② veto 导出扫描按 dataDir 真实文件计数（终态自动导出的 UI 化出口）；
//   ③ amberWrite 的通道契约钉死——/mcp JSON-RPC + worker 令牌（0.15.41 修复：
//      旧实现走 REST + worker 令牌，被 Amber 403——worker 在 REST 面零权限）。

func TestSelfEvolveStatusDisabled(t *testing.T) {
	s, _ := taskHallTestServer(t)
	mux := http.NewServeMux()
	s.registerSelfEvolveRoutes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/self-evolve/status", nil))
	if rec.Code != 200 {
		t.Fatalf("status 应 200（amber 关闭只是字段态，不是错误）: %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Amber struct {
			Enabled         bool   `json:"enabled"`
			OwnerTokenSet   bool   `json:"owner_token_set"`
			WorkerTokenSet  bool   `json:"worker_token_set"`
		} `json:"amber"`
		Veto struct {
			ExportCount int `json:"export_count"`
		} `json:"veto"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("status 响应不是合法 JSON: %v", err)
	}
	if out.Amber.Enabled {
		t.Error("amber_enabled 默认关，status 不应报已启用")
	}
	if out.Veto.ExportCount != 0 {
		t.Errorf("空 dataDir 导出数应为 0，得到 %d", out.Veto.ExportCount)
	}
}

func TestSelfEvolveVetoScan(t *testing.T) {
	s, _ := taskHallTestServer(t)
	// 伪终态导出：dataDir/veto/12/{responses.json,identity.json} + 一个非数字
	// 目录（路径穿越防御面——不应被当作任务号收进来）。
	taskDir := filepath.Join(s.dataDir, "veto", "12")
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	responses := `[{"id":"12","response":{"confirmed_vuln_classes":["sqli"],"retest_confirmed_count":1,"guard_violations":0,"sod_chain_valid":true,"tool_calls":3,"total_tokens":99}}]`
	if err := os.WriteFile(filepath.Join(taskDir, "responses.json"), []byte(responses), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(s.dataDir, "veto", "..evil"), 0o755); err == nil {
		_ = os.WriteFile(filepath.Join(s.dataDir, "veto", "..evil", "responses.json"), []byte("[]"), 0o644)
	}

	mux := http.NewServeMux()
	s.registerSelfEvolveRoutes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/self-evolve/veto/exports", nil))
	if rec.Code != 200 {
		t.Fatalf("exports 应 200: %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"task_id":"12"`) {
		t.Errorf("导出清单缺任务 12: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "..evil") {
		t.Error("非数字目录不应进导出清单")
	}

	rec2 := httptest.NewRecorder()
	mux.ServeHTTP(rec2, httptest.NewRequest("GET", "/api/self-evolve/veto/exports/12", nil))
	if rec2.Code != 200 || !strings.Contains(rec2.Body.String(), "sod_chain_valid") {
		t.Errorf("单任务原文透传失败: %d %s", rec2.Code, rec2.Body.String())
	}

	rec3 := httptest.NewRecorder()
	mux.ServeHTTP(rec3, httptest.NewRequest("GET", "/api/self-evolve/veto/exports/..%2F12", nil))
	if rec3.Code == 200 {
		t.Error("路径穿越形态不应 200")
	}
}

func TestAmberWriteMCPChannel(t *testing.T) {
	s, pg := taskHallTestServer(t)
	if err := pg.SetBool("amber_enabled", true); err != nil {
		t.Fatal(err)
	}

	// 假 Amber：钉死通道契约（/mcp + JSON-RPC memory_write + worker 令牌）。
	var gotAuth, gotPath, gotTool string
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		var rpc struct {
			Jsonrpc string `json:"jsonrpc"`
			Method  string `json:"method"`
			Params  struct {
				Name      string `json:"name"`
				Arguments struct {
					Scope   string `json:"scope"`
					Body    string `json:"body"`
					Session string `json:"session"`
				} `json:"arguments"`
			} `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&rpc)
		gotTool = rpc.Params.Name
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0", "id": 1,
			"result": map[string]any{"item_id": "itm_1"},
		})
	}))
	defer fake.Close()
	if err := pg.SetSetting("amber_base_url", fake.URL); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AMBER_WORKER_TOKEN", "wk-token")

	id, err := s.amberWrite("reartex:specialize:abc", "经验正文", "task-9")
	if err != nil {
		t.Fatalf("amberWrite 失败: %v", err)
	}
	if id != "itm_1" {
		t.Errorf("item_id = %q, want itm_1", id)
	}
	if gotPath != "/mcp" {
		t.Errorf("写入通道 = %s, want /mcp（REST 面 worker 令牌会被 Amber 403）", gotPath)
	}
	if gotAuth != "Bearer wk-token" {
		t.Errorf("鉴权头 = %q, want worker 令牌", gotAuth)
	}
	if gotTool != "memory_write" {
		t.Errorf("MCP 工具名 = %q, want memory_write", gotTool)
	}
}

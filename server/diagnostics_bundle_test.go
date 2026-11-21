package server

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/db"
)

// 脱敏诊断包回归（0.15.43 #8）：zip 结构齐 + 敏感面整段打码 + payload/detail 不外带。

func TestDiagnosticsBundle(t *testing.T) {
	s, _ := taskHallTestServer(t)
	task := drainTestTask(t, s, "诊断包回归任务")

	// 写一条含敏感路径的活动（id_rsa 形态）+ 一条正常活动。
	_, _ = task.Store.AppendActivity(db.Activity{
		Worker: "worker-1", Kind: "text",
		Summary: "读取了 ~/.ssh/id_rsa 私钥内容 abc",
	})
	_, _ = task.Store.AppendActivity(db.Activity{
		Worker: "worker-1", Kind: "tool", Tool: "nmap", Summary: "扫描 80/443 端口",
	})

	mux := http.NewServeMux()
	s.registerDiagnosticsRoutes(mux)
	req := httptest.NewRequest("GET", "/api/diagnostics/bundle", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("bundle 应 200: %d %s", rec.Code, rec.Body.String())
	}

	zr, err := zip.NewReader(bytes.NewReader(rec.Body.Bytes()), int64(rec.Body.Len()))
	if err != nil {
		t.Fatalf("不是合法 zip: %v", err)
	}
	names := map[string]bool{}
	raw := map[string]string{}
	for _, f := range zr.File {
		names[f.Name] = true
		rc, rerr := f.Open()
		if rerr != nil {
			t.Fatal(rerr)
		}
		body, _ := io.ReadAll(rc)
		_ = rc.Close()
		raw[f.Name] = string(body)
	}
	for _, want := range []string{"manifest.json", "config-safe.json", "tasks.json", "sod-recent.json", "activities-recent.json"} {
		if !names[want] {
			t.Errorf("诊断包缺 %s（现有: %v）", want, names)
		}
	}

	// 任务统计含该任务（MarshalIndent 带空格的键值形态）。
	if !strings.Contains(raw["tasks.json"], `"id": "`+task.ID+`"`) {
		t.Errorf("tasks.json 缺任务 %s: %s", task.ID, raw["tasks.json"])
	}
	// 脱敏：敏感路径活动整段打码；正常活动保留。
	acts := raw["activities-recent.json"]
	if strings.Contains(acts, "id_rsa") {
		t.Error("敏感路径原文泄漏进诊断包")
	}
	if !strings.Contains(acts, "已脱敏") {
		t.Errorf("敏感活动应被打码标记: %s", acts)
	}
	if !strings.Contains(acts, "nmap") {
		t.Error("正常活动不应被打码")
	}
}

func TestRedactText(t *testing.T) {
	if got := redactText("cat /etc/shadow"); !strings.Contains(got, "已脱敏") {
		t.Errorf("敏感路径应打码: %q", got)
	}
	if got := redactText("普通摘要文本"); got != "普通摘要文本" {
		t.Errorf("普通文本不应打码: %q", got)
	}
	if got := redactText(""); got != "" {
		t.Errorf("空串应原样: %q", got)
	}
}

// wiring 由 route_wiring_test 覆盖；此处补 handler 级 404 语义健康检查。
func TestDiagnosticsBundleJSONValid(t *testing.T) {
	s, _ := taskHallTestServer(t)
	mux := http.NewServeMux()
	s.registerDiagnosticsRoutes(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/diagnostics/bundle", nil))
	if rec.Code != 200 {
		t.Fatalf("bundle 应 200: %d", rec.Code)
	}
	zr, err := zip.NewReader(bytes.NewReader(rec.Body.Bytes()), int64(rec.Body.Len()))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range zr.File {
		if !strings.HasSuffix(f.Name, ".json") {
			continue
		}
		rc, _ := f.Open()
		body, _ := io.ReadAll(rc)
		_ = rc.Close()
		var v any
		if json.Unmarshal(body, &v) != nil {
			t.Errorf("%s 不是合法 JSON", f.Name)
		}
	}
}

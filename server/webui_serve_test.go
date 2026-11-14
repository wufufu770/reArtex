package server

// webui 路径矩阵测试（0.13.6）：fstest.MapFS 注入导出产物形态，全量钉死
// 归一化语义——CI（无 embedui tag）也能跑。核心语义：
//   - 尾斜杠 = 干净路径 = 同一份 <route>/index.html，都是 200（0.13.6 起不再 308）；
//   - 双斜杠在本 handler 内被 Clean 折叠（mux 层另有 301，与本测试无关）；
//   - legacy 改名路径对原始/Clean 后形态都 308（带尾斜杠的收藏不断链）；
//   - 未知路径 SPA 兜底 index.html。

import (
	"io/fs"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func webuiTestFS() fs.FS {
	return fstest.MapFS{
		"index.html":                         &fstest.MapFile{Data: []byte("<html>root</html>")},
		"function/business-graph/index.html": &fstest.MapFile{Data: []byte("<html>biz</html>")},
		"_next/static/x.js":                  &fstest.MapFile{Data: []byte("console.log(1)")},
	}
}

func doWebUI(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	serveWebUIFromFS(webuiTestFS()).ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
	return rec
}

func TestWebUIPathMatrix(t *testing.T) {
	cases := []struct {
		name     string
		path     string
		wantCode int
		wantLoc  string
		wantBody string // 非空时断言包含
	}{
		{name: "干净路径200", path: "/function/business-graph", wantCode: 200, wantBody: "<html>biz</html>"},
		{name: "尾斜杠200同内容", path: "/function/business-graph/", wantCode: 200, wantBody: "<html>biz</html>"},
		{name: "双斜杠Clean后200", path: "/function//business-graph", wantCode: 200, wantBody: "<html>biz</html>"},
		{name: "根路径200", path: "/", wantCode: 200, wantBody: "<html>root</html>"},
		{name: "静态资源200", path: "/_next/static/x.js", wantCode: 200, wantBody: "console.log(1)"},
		{name: "未知路径SPA兜底200", path: "/no/such/route", wantCode: 200, wantBody: "<html>root</html>"},
		{name: "legacy干净路径308", path: "/function/pianfeng", wantCode: 308, wantLoc: "/function/business-graph?mode=edge"},
		{name: "legacy带尾斜杠也308", path: "/function/pianfeng/", wantCode: 308, wantLoc: "/function/business-graph?mode=edge"},
		{name: "legacy链头308", path: "/function/edge-exploration", wantCode: 308, wantLoc: "/function/pianfeng"},
		{name: "legacy带query保留", path: "/function/pianfeng?from=x", wantCode: 308, wantLoc: "/function/business-graph?mode=edge&from=x"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doWebUI(t, tc.path)
			if rec.Code != tc.wantCode {
				t.Fatalf("%s → %d，期望 %d（Location=%q）", tc.path, rec.Code, tc.wantCode, rec.Header().Get("Location"))
			}
			if tc.wantLoc != "" && rec.Header().Get("Location") != tc.wantLoc {
				t.Fatalf("%s Location=%q，期望 %q", tc.path, rec.Header().Get("Location"), tc.wantLoc)
			}
			if tc.wantBody != "" && !strings.Contains(rec.Body.String(), tc.wantBody) {
				t.Fatalf("%s body 不含 %q（got %.80q）", tc.path, tc.wantBody, rec.Body.String())
			}
		})
	}
}

// TestWebUINoCacheHTML 钉住缓存语义：HTML 入口 no-cache（发版后浏览器必须回源
// 校验），/_next/* 带 hash 资源不做该限制。
func TestWebUINoCacheHTML(t *testing.T) {
	if got := doWebUI(t, "/function/business-graph").Header().Get("Cache-Control"); !strings.Contains(got, "no-cache") {
		t.Fatalf("HTML 应 no-cache，got %q", got)
	}
	if got := doWebUI(t, "/_next/static/x.js").Header().Get("Cache-Control"); strings.Contains(got, "no-cache") {
		t.Fatalf("_next 资源不应 no-cache，got %q", got)
	}
}

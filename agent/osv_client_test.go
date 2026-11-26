// osv_client_test.go：B-08B 验收——httptest 覆盖卡验收逐项：批量边界（100/
// 批、250→3 请求）、空依赖零请求、429+Retry-After、5xx 有界重试、永久 4xx
// 不重试、超时、非法 JSON、响应过大、results 数量不一致（不可信响应）、非
// npm 生态过滤、aliases/CVE ID/fixed 解析、请求体只含 ecosystem/name/version。
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// osvFake 计数并按脚本回状态码；记录请求体供形状断言。
type osvFake struct {
	srv      *httptest.Server
	requests atomic.Int32
	bodies   []string
	script   []int // 每次请求的状态码脚本（耗尽后恒 200）
	delay    time.Duration
	// bodyFor 在状态码 200 时生成响应体（queries 数量 → results）。
	bodyFor func(n int) string
}

func newOSVFake(t *testing.T, script []int, bodyFor func(n int) string) *osvFake {
	t.Helper()
	f := &osvFake{script: script, bodyFor: bodyFor}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/querybatch", func(w http.ResponseWriter, r *http.Request) {
		if f.delay > 0 {
			time.Sleep(f.delay)
		}
		buf, _ := io.ReadAll(r.Body)
		f.bodies = append(f.bodies, string(buf))
		idx := int(f.requests.Add(1)) - 1
		code := http.StatusOK
		if idx < len(f.script) {
			code = f.script[idx]
		}
		if code != http.StatusOK {
			if code == http.StatusTooManyRequests {
				w.Header().Set("Retry-After", "0")
			}
			w.WriteHeader(code)
			return
		}
		var req struct {
			Queries []osvWireQuery `json:"queries"`
		}
		_ = json.Unmarshal(buf, &req)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(f.bodyFor(len(req.Queries))))
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// osvResultsJSON 生成 n 条合法对齐结果（逗号分隔——Repeat 会漏逗号）。
func osvResultsJSON(n int) string {
	results := make([]string, n)
	for i := range results {
		results[i] = `{}`
	}
	return `{"results":[` + strings.Join(results, ",") + `]}`
}

func osvRefs(n int) []OSVQueryRef {
	out := make([]OSVQueryRef, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, OSVQueryRef{Ecosystem: "npm", Name: "pkg" + strconv.Itoa(i), Version: "1.0.0"})
	}
	return out
}

func osvTestClient(base string) *OSVClient {
	return &OSVClient{Base: base, Backoff: time.Millisecond, MaxResponseBytes: 1 << 20}
}

// 批量边界：250 依赖 → 恰 3 请求（100/100/50），结果逐一对齐；请求体只含
// ecosystem/name/version（无源码/路径）。
func TestOSVQueryBatchBatchingAndAlignment(t *testing.T) {
	var sizes []int
	f := newOSVFake(t, nil, func(n int) string {
		sizes = append(sizes, n)
		results := make([]string, n)
		for i := range results {
			results[i] = `{}`
		}
		if n >= 6 {
			results[5] = `{"vulns":[{"id":"GHSA-aaaa-bbbb-cccc","aliases":["CVE-2020-0001"],"affected":[{"package":{"ecosystem":"npm","name":"pkg5"},"ranges":[{"type":"SEMVER","events":[{"introduced":"0"},{"fixed":"2.0.0"}]}]}]}]}`
		}
		return `{"results":[` + strings.Join(results, ",") + `]}`
	})
	c := osvTestClient(f.srv.URL)
	hits, err := c.QueryBatch(context.Background(), osvRefs(250))
	if err != nil {
		t.Fatal(err)
	}
	if got := f.requests.Load(); got != 3 {
		t.Fatalf("250 依赖应恰 3 请求，得到 %d", got)
	}
	if len(sizes) != 3 || sizes[0] != 100 || sizes[1] != 100 || sizes[2] != 50 {
		t.Fatalf("批次边界应 100/100/50: %v", sizes)
	}
	if len(hits) != 250 {
		t.Fatalf("结果应对齐 250: %d", len(hits))
	}
	// 对齐：恰 #5 命中，#4/#6 空。
	if len(hits[5].Vulns) != 1 || hits[5].Vulns[0].ID != "GHSA-aaaa-bbbb-cccc" {
		t.Fatalf("#5 应命中: %+v", hits[5])
	}
	if len(hits[4].Vulns) != 0 || len(hits[6].Vulns) != 0 {
		t.Fatalf("#4/#6 应为空: %+v %+v", hits[4], hits[6])
	}
	// aliases/CVE ID/fixed 解析。
	v := hits[5].Vulns[0]
	if len(v.Aliases) != 1 || v.Aliases[0] != "CVE-2020-0001" {
		t.Fatalf("aliases 应含 CVE ID: %+v", v.Aliases)
	}
	if fixed := v.FixedVersions(); len(fixed) != 1 || fixed[0] != "2.0.0" {
		t.Fatalf("fixed versions 解析: %+v", fixed)
	}
	// 请求体形状：只有 queries 键；每查询只有 package；每 package 只有
	// ecosystem/name/version（绝不发送源码内容或路径）。
	for _, body := range f.bodies {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal([]byte(body), &obj); err != nil {
			t.Fatalf("请求体应合法 JSON: %v", err)
		}
		if len(obj) != 1 {
			t.Fatalf("请求体顶层只允许 queries: %v", obj)
		}
		var qs []map[string]json.RawMessage
		_ = json.Unmarshal(obj["queries"], &qs)
		for _, q := range qs {
			if len(q) != 1 {
				t.Fatalf("查询只允许 package 键: %v", q)
			}
			var pkg map[string]json.RawMessage
			_ = json.Unmarshal(q["package"], &pkg)
			if len(pkg) != 3 {
				t.Fatalf("package 只允许 ecosystem/name/version: %v", pkg)
			}
		}
	}
}

// 空依赖：合法空结果、零网络请求。
func TestOSVQueryBatchEmptyInput(t *testing.T) {
	f := newOSVFake(t, nil, func(n int) string { return `{"results":[]}` })
	c := osvTestClient(f.srv.URL)
	hits, err := c.QueryBatch(context.Background(), nil)
	if err != nil || hits == nil || len(hits) != 0 {
		t.Fatalf("空输入应空结果非 nil 无错误: %+v %v", hits, err)
	}
	if got := f.requests.Load(); got != 0 {
		t.Fatalf("空输入零请求: %d", got)
	}
}

// 429：Retry-After 优先 + 有界重试后成功。
func TestOSVQueryBatch429RetryAfter(t *testing.T) {
	f := newOSVFake(t, []int{429, 429}, func(n int) string { return osvResultsJSON(n) })
	hits, err := osvTestClient(f.srv.URL).QueryBatch(context.Background(), osvRefs(2))
	if err != nil {
		t.Fatalf("429×2 后应成功: %v", err)
	}
	if got := f.requests.Load(); got != 3 {
		t.Fatalf("应恰 3 次请求（2×429+1 成功）: %d", got)
	}
	if len(hits) != 2 {
		t.Fatalf("结果对齐: %+v", hits)
	}
}

// 5xx：有界重试（3 次）后成功 / 恒 5xx 则 4 次尝试后失败。
func TestOSVQueryBatch5xxRetry(t *testing.T) {
	f := newOSVFake(t, []int{500, 502, 503}, func(n int) string { return osvResultsJSON(n) })
	if _, err := osvTestClient(f.srv.URL).QueryBatch(context.Background(), osvRefs(2)); err != nil {
		t.Fatalf("3×5xx 后第 4 次成功: %v", err)
	}
	if got := f.requests.Load(); got != 4 {
		t.Fatalf("应恰 4 次尝试: %d", got)
	}

	f2 := newOSVFake(t, nil, func(n int) string { return `x` })
	f2.script = []int{500, 500, 500, 500, 500}
	_, err := osvTestClient(f2.srv.URL).QueryBatch(context.Background(), osvRefs(1))
	if err == nil || !strings.Contains(err.Error(), "有界重试") {
		t.Fatalf("恒 5xx 应有界失败: %v", err)
	}
	if got := f2.requests.Load(); got != 4 {
		t.Fatalf("总尝试应 ≤4（1+3 重试）: %d", got)
	}
}

// 永久 4xx：参数错误绝不重试（恰 1 次请求）。
func TestOSVQueryBatchPermanent4xx(t *testing.T) {
	f := newOSVFake(t, []int{400}, func(n int) string { return `{"results":[]}` })
	_, err := osvTestClient(f.srv.URL).QueryBatch(context.Background(), osvRefs(1))
	if err == nil || !strings.Contains(err.Error(), "400") || !strings.Contains(err.Error(), "不重试") {
		t.Fatalf("400 应永久失败: %v", err)
	}
	if got := f.requests.Load(); got != 1 {
		t.Fatalf("4xx 绝不重试（恰 1 次）: %d", got)
	}
}

// 超时：client 超时 → 可重试路径 → 最终报错。
func TestOSVQueryBatchTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"results":[{}]}`))
	}))
	defer srv.Close()
	c := &OSVClient{Base: srv.URL, Backoff: time.Millisecond, HTTP: &http.Client{Timeout: 50 * time.Millisecond}}
	_, err := c.QueryBatch(context.Background(), osvRefs(1))
	if err == nil {
		t.Fatal("超时应报错")
	}
	if !strings.Contains(err.Error(), "有界重试") {
		t.Fatalf("超时应走可重试路径聚合报错: %v", err)
	}
}

// ctx 取消：等待/请求即刻退出（429 全脚本 + 每请求 50ms 延迟，取消必落在
// 在途窗口内）。
func TestOSVQueryBatchContextCancel(t *testing.T) {
	f := newOSVFake(t, []int{429, 429, 429, 429}, func(n int) string { return osvResultsJSON(n) })
	f.delay = 50 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	c := osvTestClient(f.srv.URL)
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	if _, err := c.QueryBatch(ctx, osvRefs(1)); err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("ctx 取消应穿透: %v", err)
	}
}

// 非法 JSON / 响应过大 / results 数量不一致：不可信响应全部报错。
func TestOSVQueryBatchUntrustedResponses(t *testing.T) {
	// 非法 JSON。
	f1 := newOSVFake(t, nil, func(n int) string { return `{"results":[` })
	if _, err := osvTestClient(f1.srv.URL).QueryBatch(context.Background(), osvRefs(1)); err == nil || !strings.Contains(err.Error(), "非法 JSON") {
		t.Fatalf("非法 JSON 应报错: %v", err)
	}
	// results 数量不一致。
	f2 := newOSVFake(t, nil, func(n int) string { return `{"results":[{}]}` })
	if _, err := osvTestClient(f2.srv.URL).QueryBatch(context.Background(), osvRefs(2)); err == nil || !strings.Contains(err.Error(), "不可信") {
		t.Fatalf("results 数量不一致应报错: %v", err)
	}
	// 响应过大：上限 64 字节。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"results":"` + strings.Repeat("a", 256) + `"}`))
	}))
	defer srv.Close()
	c := osvTestClient(srv.URL)
	c.MaxResponseBytes = 64
	if _, err := c.QueryBatch(context.Background(), osvRefs(1)); err == nil || !strings.Contains(err.Error(), "响应过大") {
		t.Fatalf("响应过大应报错: %v", err)
	}
}

// 非 npm 生态过滤：整条非 npm 漏洞丢弃并计数；混合漏洞收敛为 npm affected。
func TestOSVQueryBatchNonNPMFiltered(t *testing.T) {
	f := newOSVFake(t, nil, func(n int) string {
		return `{"results":[{"vulns":[` +
			`{"id":"GHSA-pypi-only","affected":[{"package":{"ecosystem":"PyPI","name":"requests"}}]},` +
			`{"id":"GHSA-mixed","affected":[{"package":{"ecosystem":"Go","name":"net"}},{"package":{"ecosystem":"npm","name":"pkg0"},"ranges":[{"type":"ECOSYSTEM","events":[{"introduced":"1.0.0"},{"fixed":"1.2.0"}]}]}]},` +
			`{"id":"GHSA-npm","affected":[{"package":{"ecosystem":"NPM","name":"pkg0"},"ranges":[{"type":"SEMVER","events":[{"fixed":"9.9.9"}]}]}]}` +
			`]}]}`
	})
	hits, err := osvTestClient(f.srv.URL).QueryBatch(context.Background(), osvRefs(1))
	if err != nil {
		t.Fatal(err)
	}
	h := hits[0]
	if h.SkippedNonNPM != 1 {
		t.Fatalf("PyPI-only 应滤除并计数: %+v", h)
	}
	if len(h.Vulns) != 2 {
		t.Fatalf("mixed+npm 应保留 2 条: %+v", h.Vulns)
	}
	mixed := h.Vulns[0]
	if mixed.ID != "GHSA-mixed" || len(mixed.Affected) != 1 || mixed.Affected[0].Package.Ecosystem != "npm" {
		t.Fatalf("mixed 漏洞 affected 应收敛为 npm: %+v", mixed)
	}
	// 生态名大小写不敏感（OSV 有 "NPM" 写法）。
	if h.Vulns[1].ID != "GHSA-npm" || len(h.Vulns[1].FixedVersions()) != 1 || h.Vulns[1].FixedVersions()[0] != "9.9.9" {
		t.Fatalf("大写 NPM 应识别: %+v", h.Vulns[1])
	}
}

// 输入闸：残缺引用/非 npm 生态/超上限依赖数，全部 fail-closed 零请求。
func TestOSVQueryBatchGuards(t *testing.T) {
	f := newOSVFake(t, nil, func(n int) string { return `{"results":[]}` })
	c := osvTestClient(f.srv.URL)
	for name, refs := range map[string][]OSVQueryRef{
		"空名字":    {{Ecosystem: "npm", Name: "", Version: "1.0.0"}},
		"空版本":    {{Ecosystem: "npm", Name: "x", Version: ""}},
		"非npm生态": {{Ecosystem: "PyPI", Name: "requests", Version: "2.0.0"}},
		"超上限":    osvRefs(osvMaxDeps + 1),
	} {
		if _, err := c.QueryBatch(context.Background(), refs); err == nil {
			t.Fatalf("%s 应 fail-closed", name)
		}
	}
	if got := f.requests.Load(); got != 0 {
		t.Fatalf("输入闸拒绝必须零请求: %d", got)
	}
}

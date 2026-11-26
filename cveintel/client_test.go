package cveintel

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeMCP 是最小 Streamable MCP /mcp fake：按工具名返回 canned 信封。
type fakeMCP struct {
	srv      *httptest.Server
	status   int            // 非 200 时强制该状态码
	ctype    string         // 覆盖 content-type
	body     string         // 覆盖原始响应体
	sawAuth  bool           // 是否收到 Authorization 头
	sawArgs  map[string]any // 最近一次 arguments
	delay    time.Duration
	maxBytes int64 // 客户端上限覆写用
}

func newFakeMCP(t *testing.T) *fakeMCP {
	t.Helper()
	f := &fakeMCP{}
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			f.sawAuth = true
		}
		var req struct {
			Params struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			} `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		_ = json.Unmarshal(req.Params.Arguments, &f.sawArgs)
		if f.delay > 0 {
			time.Sleep(f.delay)
		}
		if f.status != 0 && f.status != http.StatusOK {
			http.Error(w, "boom", f.status)
			return
		}
		if f.ctype != "" {
			w.Header().Set("Content-Type", f.ctype)
		} else {
			w.Header().Set("Content-Type", "application/json")
		}
		if f.body != "" {
			_, _ = w.Write([]byte(f.body))
			return
		}
		var text string
		switch req.Params.Name {
		case "cve_lookup":
			text = `{"schema_version":"cve_intel.v1","cve_id":"CVE-2021-44228","title":"Log4j","cwe_ids":[],"in_cisa_kev":true,"affected_ranges":[],"fixed_versions":[],"references":[],"related_skills":[],"source_provenance":"nvd"}`
		case "cve_search":
			text = `[{"cve_id":"CVE-2024-0001"},{"cve_id":"CVE-2024-0002","published":"2024-06-01T00:00:00Z"}]`
		case "cve_match_component":
			text = `[{"verdict":"affected","reason":"matched range","cve":null}]`
		case "cve_sync_status":
			text = `{"sources":{"kev":{"last_sync":"","last_success":"","last_attempt":"","last_error":"","records":0,"stale":true,"status":"unknown"}},"total_records":0}`
		default:
			text = `{}`
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0", "id": 1,
			"result": map[string]any{"content": []map[string]any{{"type": "text", "text": text}}},
		})
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func newTestClient(t *testing.T, f *fakeMCP) *Client {
	t.Helper()
	c, err := New(f.srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if f.maxBytes > 0 {
		c.max = f.maxBytes
	}
	return c
}

// 五个只读方法 happy 链 + 断言零 Authorization 头（token 不外带）。
func TestClientHappyPaths(t *testing.T) {
	f := newFakeMCP(t)
	c := newTestClient(t, f)

	rec, err := c.Lookup(context.Background(), "cve-2021-44228")
	if err != nil {
		t.Fatal(err)
	}
	if rec == nil || rec.CVEID != "CVE-2021-44228" || !rec.InKEV {
		t.Fatalf("lookup 结果错误: %+v", rec)
	}
	if f.sawAuth {
		t.Fatal("请求不得携带 Authorization 头")
	}

	recs, err := c.Search(context.Background(), "log4j", 20)
	if err != nil || len(recs) != 2 || recs[1].Published == "" {
		t.Fatalf("search = %+v %v", recs, err)
	}

	items, err := c.MatchComponent(context.Background(), "maven", "log4j-core", "2.14.1")
	if err != nil || len(items) != 1 || items[0].Verdict != "affected" {
		t.Fatalf("match = %+v %v", items, err)
	}

	verdict, reason, err := c.EvaluateRanges(context.Background(), "npm", "1.5.0",
		[]Range{{Events: []RangeEvent{{"introduced": "1.0"}, {"fixed": "2.0"}}}})
	if err != nil || verdict != "affected" || reason == "" {
		t.Fatalf("evaluate = %q/%q %v", verdict, reason, err)
	}
	// ranges 必须原样进 arguments（经同一 handler 判定）
	if f.sawArgs["ranges"] == nil {
		t.Fatalf("EvaluateRanges 未携带 ranges: %v", f.sawArgs)
	}

	st, err := c.SyncStatus(context.Background())
	if err != nil || st.Sources == nil {
		t.Fatalf("status = %+v %v", st, err)
	}
}

// lookup not-found 显式语义（nil, nil）；schema 版本不兼容有类型错误。
func TestClientLookupNotFoundAndIncompatible(t *testing.T) {
	f := newFakeMCP(t)
	c := newTestClient(t, f)
	f.body = `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"{\"schema_version\":\"cve_intel.v1\",\"found\":false,\"cve_id\":\"CVE-2099-0001\"}"}]}}`
	rec, err := c.Lookup(context.Background(), "CVE-2099-0001")
	if rec != nil || err != nil {
		t.Fatalf("not-found 必须 (nil,nil): %v %v", rec, err)
	}
	f.body = `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"{\"schema_version\":\"cve_intel.v2\",\"cve_id\":\"CVE-2021-44228\"}"}]}}`
	if _, err := c.Lookup(context.Background(), "CVE-2021-44228"); !errors.Is(err, ErrIncompatible) {
		t.Fatalf("schema 不兼容必须 ErrIncompatible: %v", err)
	}
	if _, err := c.Lookup(context.Background(), "not-a-cve"); err == nil || errors.Is(err, ErrUnavailable) {
		t.Fatal("非法 cve_id 必须客户端快速失败")
	}
}

// 非 2xx 分派：5xx/429/503 → ErrUnavailable；其他 4xx → ErrProtocol。
func TestClientNon2xx(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   error
	}{
		{http.StatusInternalServerError, ErrUnavailable},
		{http.StatusServiceUnavailable, ErrUnavailable},
		{http.StatusTooManyRequests, ErrUnavailable},
		{http.StatusBadRequest, ErrProtocol},
	} {
		f := newFakeMCP(t)
		c := newTestClient(t, f)
		f.status = tc.status
		if _, err := c.SyncStatus(context.Background()); !errors.Is(err, tc.want) {
			t.Fatalf("HTTP %d → %v, want %v", tc.status, err, tc.want)
		}
	}
}

// 协议错误族：非法 JSON 信封 / content-type 错 / jsonrpc error / is_error /
// search 缺 cve_id / match 非法三态 / status 缺 sources。
func TestClientProtocolErrors(t *testing.T) {
	cases := []struct {
		name  string
		body  string
		ctype string
		call  func(c *Client) error
	}{
		{"信封坏 JSON", `not-json`, "", func(c *Client) error {
			_, err := c.SyncStatus(context.Background())
			return err
		}},
		{"content-type 错", `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"{}"}]}}`, "text/plain", func(c *Client) error {
			_, err := c.SyncStatus(context.Background())
			return err
		}},
		{"jsonrpc error 对象", `{"jsonrpc":"2.0","id":1,"error":{"code":-32601,"message":"no such tool"}}`, "", func(c *Client) error {
			_, err := c.SyncStatus(context.Background())
			return err
		}},
		{"is_error 工具错误", `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"错误: x"}],"is_error":true}}`, "", func(c *Client) error {
			_, err := c.SyncStatus(context.Background())
			return err
		}},
		{"search 缺 cve_id", `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"[{\"title\":\"no id\"}]"}]}}`, "", func(c *Client) error {
			_, err := c.Search(context.Background(), "q", 10)
			return err
		}},
		{"match 非法三态", `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"[{\"verdict\":\"safe\",\"reason\":\"r\"}]"}]}}`, "", func(c *Client) error {
			_, err := c.MatchComponent(context.Background(), "npm", "p", "1.0")
			return err
		}},
		{"status 缺 sources", `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"{}"}]}}`, "", func(c *Client) error {
			_, err := c.SyncStatus(context.Background())
			return err
		}},
	}
	for _, tc := range cases {
		f := newFakeMCP(t)
		c := newTestClient(t, f)
		f.body = tc.body
		f.ctype = tc.ctype
		err := tc.call(c)
		if !errors.Is(err, ErrProtocol) {
			t.Fatalf("%s → %v, want ErrProtocol", tc.name, err)
		}
	}
	// search null → 空切片非错误
	f := newFakeMCP(t)
	c := newTestClient(t, f)
	f.body = `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"null"}]}}`
	recs, err := c.Search(context.Background(), "q", 10)
	if err != nil || len(recs) != 0 {
		t.Fatalf("search null 必须空切片: %v %v", recs, err)
	}
}

// 超时与响应过大有类型。
func TestClientTimeoutAndTooLarge(t *testing.T) {
	f := newFakeMCP(t)
	c := newTestClient(t, f).WithTimeout(100 * time.Millisecond)
	f.delay = 300 * time.Millisecond
	if _, err := c.SyncStatus(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("超时必须 ErrUnavailable: %v", err)
	}

	f2 := newFakeMCP(t)
	f2.maxBytes = 64 // 必须在 newTestClient 前设置（构造时读取）
	c2 := newTestClient(t, f2)
	f2.body = `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"` + strings.Repeat("x", 200) + `"}]}}`
	if _, err := c2.SyncStatus(context.Background()); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("超限必须 ErrTooLarge: %v", err)
	}
}

// 非法 base URL 拒绝；loopback 与带路径 base 合法。
func TestClientBaseURLValidation(t *testing.T) {
	for _, bad := range []string{
		"", "   ", "ftp://x", "http://", "file:///tmp",
		"user:pass@http://x", "http://user:pass@host", "http://host/?q=1", "http://host/#frag",
	} {
		if _, err := New(bad); err == nil {
			t.Fatalf("base URL %q 必须拒绝", bad)
		}
	}
	for _, ok := range []string{
		"http://127.0.0.1:8400", "https://cve.internal:8400", "http://host/cve-prefix",
	} {
		if _, err := New(ok); err != nil {
			t.Fatalf("base URL %q 必须接受: %v", ok, err)
		}
	}
	if URLFromEnv() != "" {
		t.Skip("环境已有 CVE_MCP_URL，跳过空值断言")
	}
}

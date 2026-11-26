// Package cveintel 提供对 reartex-cve-mcp sidecar 的最小只读客户端（B-01D）。
//
// 形态：经 B-01B 的无状态 Streamable HTTP MCP /mcp 面发 tools/call（与
// server/amber_client.go 同款 JSON-RPC 信封），只封装 lookup / search /
// match-component（含 B-03D ranges 纯求值）/ sync-status 四个只读方法——
// 不实现 sync、不写表、不引入本地 SQLite；版本精判不在单体复制（B-05 判定
// 归 sidecar 的 version 包，经 B-03D 同一 handler 调用）。
//
// 端点纪律（防 SSRF）：New 只接受明确受控的 http(s) 端点——scheme 必须
// http/https、host 必填、拒绝 userinfo/query/fragment；loopback 允许（sidecar
// 同机部署是本客户端的设计目标，与 agent 出网限制不同层：本客户端不消费
// agent 可控 URL，只消费运维配置的固定端点）。地址来自调用方（CVE_MCP_URL
// 经 URLFromEnv 提供），不写死。
//
// 错误模型（有类型）：ErrUnavailable（超时/连接失败/5xx/429——上游不健康，
// 调用方应降级为 unavailable 语义，绝不伪造空结果）、ErrProtocol（非 JSON/
// content-type 错/信封坏/工具级 is_error）、ErrTooLarge（响应超上限）、
// ErrIncompatible（schema_version 非 cve_intel.v1——lookup 面）。所有错误
// 文本不含 token（请求本身零凭据头，服务是本机只读面）。
//
// 响应校验（卡要求 6）：content-type 必须 application/json；响应体经
// LimitReader 硬上限；lookup 输出校验 schema_version==cve_intel.v1；search
// 列表项要求 cve_id 非空（search 面是存储形态列表——无 schema_version 键，
// 如实按 legacy 形态校验，B-03C 边界既定）。
package cveintel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

// 契约与上限常量。
const (
	// SchemaVersion 是 cve_intel.v1 冻结契约版本（与 sidecar contract.go 同值
	// ——双端各自持有常量，跨模块不 import）。
	SchemaVersion = "cve_intel.v1"
	// DefaultTimeout 是单请求缺省超时。
	DefaultTimeout = 5 * time.Second
	// MaxResponseBytes 是响应体硬上限（lookup 全量记录 + search 列表均远小于此）。
	MaxResponseBytes = 8 << 20
)

// 有类型错误（调用方按 errors.Is 分派降级语义）。
var (
	ErrUnavailable  = errors.New("cve 服务不可用")
	ErrProtocol     = errors.New("cve 服务协议错误")
	ErrTooLarge     = errors.New("cve 服务响应过大")
	ErrIncompatible = errors.New("cve 服务契约版本不兼容")
)

// Client 是只读客户端。零值不可用，须经 New 构造。
type Client struct {
	base string
	hc   *http.Client
	max  int64
}

// New 构造客户端。baseURL 校验：仅 http/https、host 必填、无 userinfo/
// query/fragment（防 SSRF：不接受任意 scheme 与不受控形态）；loopback 明确
// 允许（同机 sidecar 是设计目标）。
func New(baseURL string) (*Client, error) {
	raw := strings.TrimSpace(baseURL)
	if raw == "" {
		return nil, fmt.Errorf("cveintel: base URL 为空")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("cveintel: base URL 不可解析: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("cveintel: base URL scheme 必须为 http/https: %q", u.Scheme)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("cveintel: base URL 缺 host")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("cveintel: base URL 不得含 userinfo/query/fragment")
	}
	return &Client{
		base: strings.TrimRight(raw, "/"),
		hc:   &http.Client{Timeout: DefaultTimeout},
		max:  MaxResponseBytes,
	}, nil
}

// URLFromEnv 读 CVE_MCP_URL（运维配置的唯一入口；空 = 未配置——调用方据此
// 走降级路径，绝不回退写死地址）。
func URLFromEnv() string {
	return strings.TrimSpace(os.Getenv("CVE_MCP_URL"))
}

// WithTimeout 返回带自定义超时的副本（测试与特殊调用用；常规路径用 ctx）。
func (c *Client) WithTimeout(d time.Duration) *Client {
	cp := *c
	cp.hc = &http.Client{Timeout: d}
	return &cp
}

// cveRequest/cveResponse 是 JSON-RPC 信封（与 amber_client 同形态）。
type cveEnvelope struct {
	Result struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"is_error"`
	} `json:"result"`
	Err *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// call 发一次 tools/call 并抽出 result.content[0].text。
func (c *Client) call(ctx context.Context, tool string, args map[string]any) (string, error) {
	rpc := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params":  map[string]any{"name": tool, "arguments": args},
	}
	body, err := json.Marshal(rpc)
	if err != nil {
		return "", fmt.Errorf("cveintel: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/mcp", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("cveintel: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusOK:
	case resp.StatusCode == http.StatusServiceUnavailable || resp.StatusCode == http.StatusTooManyRequests ||
		resp.StatusCode >= 500:
		return "", fmt.Errorf("%w: HTTP %d", ErrUnavailable, resp.StatusCode)
	default:
		return "", fmt.Errorf("%w: HTTP %d", ErrProtocol, resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		return "", fmt.Errorf("%w: content-type %q", ErrProtocol, ct)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, c.max+1))
	if err != nil {
		return "", fmt.Errorf("%w: 读响应失败: %v", ErrUnavailable, err)
	}
	if int64(len(raw)) > c.max {
		return "", ErrTooLarge
	}
	var env cveEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return "", fmt.Errorf("%w: 非法 JSON 信封: %v", ErrProtocol, err)
	}
	if env.Err != nil {
		return "", fmt.Errorf("%w: jsonrpc 错误 %d: %s", ErrProtocol, env.Err.Code, env.Err.Message)
	}
	if env.Result.IsError {
		text := ""
		if len(env.Result.Content) > 0 {
			text = env.Result.Content[0].Text
		}
		return "", fmt.Errorf("%w: 工具错误: %s", ErrProtocol, text)
	}
	if len(env.Result.Content) == 0 || env.Result.Content[0].Type != "text" {
		return "", fmt.Errorf("%w: result.content 缺 text 块", ErrProtocol)
	}
	return env.Result.Content[0].Text, nil
}

// Record 是 cve_intel.v1 记录的客户端侧只读视图（必需数组/可选字段与冻结
// 契约同名；本包不 import sidecar 的任何类型——独立 module 边界）。
type Record struct {
	SchemaVersion    string          `json:"schema_version"`
	CVEID            string          `json:"cve_id"`
	Title            string          `json:"title,omitempty"`
	Description      string          `json:"description,omitempty"`
	CVSSv3           *float64        `json:"cvss_v3,omitempty"`
	CVSSv4           *float64        `json:"cvss_v4,omitempty"`
	CWEIDs           []string        `json:"cwe_ids"`
	InKEV            bool            `json:"in_cisa_kev"`
	KEVDueDate       string          `json:"kev_due_date,omitempty"`
	AffectedRanges   []AffectedRange `json:"affected_ranges"`
	FixedVersions    []string        `json:"fixed_versions"`
	References       []string        `json:"references"`
	RelatedSkills    []string        `json:"related_skills"`
	SourceProvenance string          `json:"source_provenance"`
	SyncedAt         string          `json:"synced_at,omitempty"`
	Published        string          `json:"published,omitempty"` // search legacy 面可能携带（B-03C）
}

// AffectedRange 是区间只读视图（from/to/fixed 键与冻结契约一致）。
type AffectedRange struct {
	Ecosystem string `json:"ecosystem,omitempty"`
	Package   string `json:"package,omitempty"`
	From      string `json:"from,omitempty"`
	To        string `json:"to,omitempty"`
	Fixed     string `json:"fixed,omitempty"`
}

// Lookup 按 CVE id 精确查（B-03B 契约面）。found=false 返回 (nil, nil)——
// not-found ≠ unavailable（B-03B 同源语义）。schema_version 非 cve_intel.v1
// → ErrIncompatible。
func (c *Client) Lookup(ctx context.Context, cveID string) (*Record, error) {
	id := strings.ToUpper(strings.TrimSpace(cveID))
	if !cveIDForm.MatchString(id) {
		return nil, fmt.Errorf("cveintel: cve_id 非法形态")
	}
	text, err := c.call(ctx, "cve_lookup", map[string]any{"cve_id": id})
	if err != nil {
		return nil, err
	}
	var probe struct {
		SchemaVersion string `json:"schema_version"`
		Found         *bool  `json:"found"`
		CVEID         string `json:"cve_id"`
	}
	if err := json.Unmarshal([]byte(text), &probe); err != nil {
		return nil, fmt.Errorf("%w: lookup 输出非法: %v", ErrProtocol, err)
	}
	if probe.Found != nil && !*probe.Found {
		return nil, nil // not-found 显式语义
	}
	if probe.SchemaVersion != SchemaVersion {
		return nil, fmt.Errorf("%w: %q", ErrIncompatible, probe.SchemaVersion)
	}
	var rec Record
	if err := json.Unmarshal([]byte(text), &rec); err != nil {
		return nil, fmt.Errorf("%w: 记录非法: %v", ErrProtocol, err)
	}
	if rec.CVEID == "" {
		return nil, fmt.Errorf("%w: 缺 cve_id", ErrProtocol)
	}
	return &rec, nil
}

// Search 全文+结构化过滤（legacy 存储形态列表——无 schema_version 键，逐项
// 校验 cve_id 非空；limit 1-100）。
func (c *Client) Search(ctx context.Context, q string, limit int) ([]Record, error) {
	if limit < 1 || limit > 100 {
		return nil, fmt.Errorf("cveintel: limit 必须为 1-100")
	}
	args := map[string]any{"query": q, "limit": fmt.Sprint(limit)}
	text, err := c.call(ctx, "cve_search", args)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(text) == "null" {
		return []Record{}, nil // sidecar 空结果形态（B-03C 既有行为）
	}
	var recs []Record
	if err := json.Unmarshal([]byte(text), &recs); err != nil {
		return nil, fmt.Errorf("%w: search 输出非法: %v", ErrProtocol, err)
	}
	for i, r := range recs {
		if r.CVEID == "" {
			return nil, fmt.Errorf("%w: search[%d] 缺 cve_id", ErrProtocol, i)
		}
	}
	return recs, nil
}

// MatchItem 是 match_component 输出条目（三态判定 + reason + 可选契约记录）。
type MatchItem struct {
	Verdict string  `json:"verdict"`
	Reason  string  `json:"reason"`
	Package string  `json:"package,omitempty"`
	Cve     *Record `json:"cve,omitempty"`
}

// 三态白名单（B-05 纪律：判定只允许这三值；未知值=协议错误不静默放过）。
var verdictSet = map[string]bool{"affected": true, "not_affected": true, "maybe": true}

func (c *Client) matchCall(ctx context.Context, args map[string]any) ([]MatchItem, error) {
	text, err := c.call(ctx, "cve_match_component", args)
	if err != nil {
		return nil, err
	}
	var items []MatchItem
	if err := json.Unmarshal([]byte(text), &items); err != nil {
		return nil, fmt.Errorf("%w: match 输出非法: %v", ErrProtocol, err)
	}
	for i, it := range items {
		if !verdictSet[it.Verdict] {
			return nil, fmt.Errorf("%w: match[%d] 非法三态 %q", ErrProtocol, i, it.Verdict)
		}
	}
	return items, nil
}

// MatchComponent 组件匹配（B-05 三态精判）。
func (c *Client) MatchComponent(ctx context.Context, ecosystem, pkg, version string) ([]MatchItem, error) {
	return c.matchCall(ctx, map[string]any{
		"ecosystem": ecosystem, "package_or_cpe": pkg, "version": version,
	})
}

// RangeEvent 是调用方区间的事件（单键形态：introduced/fixed/last_affected → 版本）。
type RangeEvent map[string]string

// Range 是调用方区间（from/to/fixed 或 events 形态；键与 B-03D 适配层一致）。
type Range struct {
	From        string       `json:"from,omitempty"`
	To          string       `json:"to,omitempty"`
	Fixed       string       `json:"fixed,omitempty"`
	ToInclusive bool         `json:"to_inclusive,omitempty"`
	Ecosystem   string       `json:"ecosystem,omitempty"`
	Events      []RangeEvent `json:"events,omitempty"`
}

// EvaluateRanges 调用方区间纯求值——经 B-03D 同一 cve_match_component handler
// 调 B-05（不在单体复制版本判定代码）。items 恰一条（verdict+reason）。
func (c *Client) EvaluateRanges(ctx context.Context, ecosystem, version string, ranges []Range) (verdict, reason string, err error) {
	if len(ranges) == 0 {
		return "", "", fmt.Errorf("cveintel: ranges 为空")
	}
	items, err := c.matchCall(ctx, map[string]any{
		"ecosystem": ecosystem, "version": version,
		"ranges": ranges,
	})
	if err != nil {
		return "", "", err
	}
	if len(items) == 0 {
		return "", "", fmt.Errorf("%w: 求值零输出", ErrProtocol)
	}
	return items[0].Verdict, items[0].Reason, nil
}

// Status 是 sync-status 只读契约视图（B-03E：缺源显式 unknown）。
type Status struct {
	Sources      map[string]SourceStatus `json:"sources"`
	TotalRecords int                     `json:"total_records"`
}

// SourceStatus 是单源状态。
type SourceStatus struct {
	LastSync    string `json:"last_sync"`
	LastSuccess string `json:"last_success"`
	LastAttempt string `json:"last_attempt"`
	LastError   string `json:"last_error"`
	Records     int    `json:"records"`
	Stale       bool   `json:"stale"`
	Status      string `json:"status,omitempty"`
}

// SyncStatus 读同步新鲜度（只读；永不触发同步——sidecar B-03E 契约保证）。
func (c *Client) SyncStatus(ctx context.Context) (*Status, error) {
	text, err := c.call(ctx, "cve_sync_status", map[string]any{})
	if err != nil {
		return nil, err
	}
	var st Status
	if err := json.Unmarshal([]byte(text), &st); err != nil {
		return nil, fmt.Errorf("%w: status 输出非法: %v", ErrProtocol, err)
	}
	if st.Sources == nil {
		return nil, fmt.Errorf("%w: status 缺 sources", ErrProtocol)
	}
	return &st, nil
}

// cveIDForm 与 sidecar cveIDRe 同形（CVE-\d{4}-\d{4,}；客户端快速失败，服务
// 端仍权威校验）。
var cveIDForm = regexp.MustCompile(`^CVE-\d{4}-\d{4,}$`)

// osv_client.go：OSV QueryBatch HTTP 客户端（B-08B）。
//
// 唯一目标：把 npm 依赖清单按有界批次查 OSV /v1/querybatch，解析响应里的
// 漏洞 ID、aliases（CVE/GHSA）、affected package/ranges/fixed versions。
// 本文件只是「情报查询面」：不触发任务、不写事件、不建 finding、不扩大
// scope——OSV 结果永远只是线索（包安全原则），三态判定属 B-08C。
//
// 出网纪律（卡要求 1/2/3）：
//   - 唯一端点 {Base}/v1/querybatch（Base 缺省 https://api.osv.dev），POST
//     JSON；请求体只含 ecosystem/name/version——绝不发送源码内容、仓库私密
//     路径或工作区绝对路径（卡要求 2 逐字）；
//   - 默认每批 100（OSV querybatch 服务端上限为 1000，取保守 100：单请求
//     体小、失败重试成本低）；依赖总数封顶 2000（防无界清单进网络面）；
//   - 超时：http.Client Timeout 30s + 每次调用全程受 ctx 控制取消；
//   - 响应大小上限 8 MiB（LimitReader+1 探测截断——不可信响应必须校验）；
//   - 429/5xx/网络错误：最多 3 次有界重试（总尝试 ≤4），退避=指数（500ms
//     起）且 Retry-After 优先（封顶 30s）；其余 4xx=参数错误，绝不重试
//     （fail-fast，卡要求 3）。
//
// 响应纪律（卡要求 4）：results 长度必须与查询数一致（OSV 契约，不一致=
// 不可信响应报错）；每个漏洞只保留 npm 生态的 affected 条目（非 npm 生态
// 条目滤除并如实计数 SkippedNonNPM——不伪装成「没有漏洞」）；零日志输出
// （错误只带状态码/原因，不带依赖名清单——卡要求 5：请求/响应日志不得含
// 源码、token 或敏感工作区路径）。
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// 客户端预算常量（确定性封顶）。
const (
	osvDefaultBase       = "https://api.osv.dev"
	osvBatchSize         = 100 // 默认每批（服务端上限 1000，保守取 100）
	osvMaxBatches        = 20  // 批次数上限（⇒ 依赖总数封顶 2000）
	osvMaxDeps           = osvBatchSize * osvMaxBatches
	osvMaxResponseBytes  = 8 << 20 // 单响应上限 8 MiB
	osvMaxRetries        = 3       // 429/5xx/网络错误的有界重试次数（总尝试 ≤4）
	osvTimeout           = 30 * time.Second
	osvMaxRetryWait      = 30 * time.Second       // Retry-After 封顶
	osvRetryBackoff      = 500 * time.Millisecond // 无 Retry-After 时的指数退避基数
	osvEcosystemNPMQuery = "npm"
)

// OSVQueryRef 是一次漏洞查询引用（npm 依赖三元组；JSON 形态即 OSV wire 形态）。
type OSVQueryRef struct {
	Ecosystem string `json:"ecosystem"`
	Name      string `json:"name"`
	Version   string `json:"version"`
}

// OSVRangeEvent 是 affected[].ranges[].events[] 的一条区间事件（introduced/
// fixed/last_affected/limit——B-08C 区间适配的原材料，原样保留）。
type OSVRangeEvent struct {
	Introduced   string `json:"introduced,omitempty"`
	Fixed        string `json:"fixed,omitempty"`
	LastAffected string `json:"last_affected,omitempty"`
	Limit        string `json:"limit,omitempty"`
}

// OSVRange 是一条受影响区间。
type OSVRange struct {
	Type   string          `json:"type,omitempty"` // ECOSYSTEM / SEMVER / GIT …
	Repo   string          `json:"repo,omitempty"`
	Events []OSVRangeEvent `json:"events,omitempty"`
}

// OSVAffectedPackage 是 affected[].package（生态+包名）。
type OSVAffectedPackage struct {
	Ecosystem string `json:"ecosystem,omitempty"`
	Name      string `json:"name,omitempty"`
	Purl      string `json:"purl,omitempty"`
}

// OSVAffected 是漏洞的一条受影响声明。
type OSVAffected struct {
	Package  OSVAffectedPackage `json:"package,omitempty"`
	Ranges   []OSVRange         `json:"ranges,omitempty"`
	Versions []string           `json:"versions,omitempty"`
}

// OSVVuln 是 OSV 返回的一条漏洞（解析卡要求 4 的全部字段：ID/aliases/
// affected package/ranges/fixed versions）。Details/References 等富文本刻意
// 不取——本客户端只供给判定面，不做展示面。
type OSVVuln struct {
	ID       string        `json:"id"`
	Aliases  []string      `json:"aliases,omitempty"`
	Affected []OSVAffected `json:"affected,omitempty"`
}

// FixedVersions 收集本漏洞的 distinct fixed 版本（按首见顺序，跨 range 去重）
// ——供 B-08C 区间适配与测试断言。
func (v *OSVVuln) FixedVersions() []string {
	seen := map[string]bool{}
	out := []string{}
	for i := range v.Affected {
		for _, r := range v.Affected[i].Ranges {
			for _, e := range r.Events {
				if e.Fixed != "" && !seen[e.Fixed] {
					seen[e.Fixed] = true
					out = append(out, e.Fixed)
				}
			}
		}
	}
	return out
}

// OSVQueryHits 是单条依赖查询的对齐结果（results[i] ↔ refs[i]，OSV 契约）。
type OSVQueryHits struct {
	Ref           OSVQueryRef
	Vulns         []OSVVuln // npm 相关漏洞（非 npm affected 条目已滤除）
	SkippedNonNPM int       // 被滤除的非 npm 漏洞条数（如实计数，不静默）
}

// OSVClient 是 OSV QueryBatch 客户端（零值可用：Base/HTTP/Backoff/
// MaxResponseBytes 缺省填充）。
type OSVClient struct {
	Base             string
	HTTP             *http.Client
	Backoff          time.Duration // 测试可调小；缺省 osvRetryBackoff
	MaxResponseBytes int64
}

func (c *OSVClient) base() string {
	if strings.TrimSpace(c.Base) != "" {
		return strings.TrimRight(c.Base, "/")
	}
	return osvDefaultBase
}

func (c *OSVClient) httpClient() *http.Client {
	// 复用标准库 http.Client 配置惯例（本仓库 agent 面无共享通用 client——
	// LLM 面的 quotaAwareTransport 是 LLM 专属，OSV 公共情报面不适用）；
	// 超时/响应上限即本文件安全缺省。
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: osvTimeout}
}

func (c *OSVClient) backoff() time.Duration {
	if c.Backoff > 0 {
		return c.Backoff
	}
	return osvRetryBackoff
}

func (c *OSVClient) maxResponse() int64 {
	if c.MaxResponseBytes > 0 {
		return c.MaxResponseBytes
	}
	return osvMaxResponseBytes
}

// QueryBatch 按有界批次查询 OSV；结果与输入逐一对齐。空输入=合法空结果
// （零网络请求）。429/5xx/网络错误有界重试；其余 4xx 永久失败不重试。
func (c *OSVClient) QueryBatch(ctx context.Context, refs []OSVQueryRef) ([]OSVQueryHits, error) {
	if len(refs) == 0 {
		return []OSVQueryHits{}, nil // 空依赖：合法空结果，零请求
	}
	if len(refs) > osvMaxDeps {
		return nil, fmt.Errorf("依赖数超上限 %d（请分批审计）", osvMaxDeps)
	}
	for i, r := range refs {
		// fail-closed：残缺引用绝不出网（宁可报错也不发畸形查询）。
		if r.Ecosystem != osvEcosystemNPMQuery || strings.TrimSpace(r.Name) == "" || strings.TrimSpace(r.Version) == "" {
			return nil, fmt.Errorf("依赖引用 #%d 不完整或非 npm 生态（ecosystem/name/version 必填且 ecosystem 恒 npm）", i)
		}
	}

	out := make([]OSVQueryHits, 0, len(refs))
	for start := 0; start < len(refs); start += osvBatchSize {
		end := start + osvBatchSize
		if end > len(refs) {
			end = len(refs)
		}
		chunk := refs[start:end]
		hits, err := c.queryChunk(ctx, chunk)
		if err != nil {
			return nil, err
		}
		out = append(out, hits...)
	}
	return out, nil
}

// osvWireQuery 是 OSV querybatch 的 wire 形态：每条查询必须包在 "package"
// 键下（{"queries":[{"package":{ecosystem,name,version}}]}——OSV API 契约）。
type osvWireQuery struct {
	Package OSVQueryRef `json:"package"`
}

// queryChunk 发一批（≤osvBatchSize）并解析对齐结果；重试语义在此实现。
func (c *OSVClient) queryChunk(ctx context.Context, chunk []OSVQueryRef) ([]OSVQueryHits, error) {
	queries := make([]osvWireQuery, len(chunk))
	for i, r := range chunk {
		queries[i] = osvWireQuery{Package: r}
	}
	wire := struct {
		Queries []osvWireQuery `json:"queries"`
	}{Queries: queries}
	body, err := json.Marshal(wire)
	if err != nil {
		return nil, fmt.Errorf("osv querybatch: 请求体构造失败: %w", err)
	}

	var lastErr error
	lastRetryAfter := time.Duration(0)
	for attempt := 0; attempt <= osvMaxRetries; attempt++ {
		if attempt > 0 {
			// 有界退避：Retry-After 优先（封顶 30s），否则指数退避；全程受 ctx。
			wait := c.backoff() << (attempt - 1)
			if lastRetryAfter > 0 {
				wait = lastRetryAfter
			}
			if err := ctxSleep(ctx, wait); err != nil {
				return nil, err
			}
		}
		hits, retryAfter, retryable, err := c.attempt(ctx, body, chunk)
		if err == nil {
			return hits, nil
		}
		if !retryable {
			return nil, err // 其余 4xx=参数错误：绝不重试（卡要求 3）
		}
		lastErr = err
		lastRetryAfter = retryAfter
	}
	return nil, fmt.Errorf("osv querybatch: 有界重试 %d 次后仍失败: %w", osvMaxRetries, lastErr)
}

// attempt 发一次请求并分类结果：retryable 标记是否值得重试；retryAfter 是
// 429 的 Retry-After 封顶值（非 429 恒 0）。chunk 供结果对齐归因。
func (c *OSVClient) attempt(ctx context.Context, body []byte, chunk []OSVQueryRef) (hits []OSVQueryHits, retryAfter time.Duration, retryable bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base()+"/v1/querybatch", strings.NewReader(string(body)))
	if err != nil {
		return nil, 0, false, fmt.Errorf("osv querybatch: 请求构造失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "reartex-artex")

	resp, err := c.httpClient().Do(req)
	if err != nil {
		// 网络错误/超时：可重试（有界）。
		return nil, 0, true, fmt.Errorf("osv querybatch: 请求失败: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		ra := parseRetryAfter(resp.Header.Get("Retry-After"))
		return nil, ra, true, fmt.Errorf("osv querybatch: 限流 429")
	case resp.StatusCode >= 500:
		return nil, 0, true, fmt.Errorf("osv querybatch: 上游错误 status %d", resp.StatusCode)
	case resp.StatusCode >= 400:
		// 其余 4xx=参数错误（请求形态/生态不支持）：永久失败，绝不重试。
		return nil, 0, false, fmt.Errorf("osv querybatch: 永久拒绝 status %d（参数错误不重试）", resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		return nil, 0, false, fmt.Errorf("osv querybatch: 意外 status %d", resp.StatusCode)
	}

	// 响应大小上限：LimitReader+1 探测截断。
	raw, err := io.ReadAll(io.LimitReader(resp.Body, c.maxResponse()+1))
	if err != nil {
		return nil, 0, true, fmt.Errorf("osv querybatch: 响应读取失败: %w", err)
	}
	if int64(len(raw)) > c.maxResponse() {
		return nil, 0, false, fmt.Errorf("osv querybatch: 响应过大（超上限 %d 字节）", c.maxResponse())
	}

	var parsed struct {
		Results []json.RawMessage `json:"results"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, 0, false, fmt.Errorf("osv querybatch: 非法 JSON 响应: %w", err)
	}
	// OSV 契约：results 与 queries 逐一对齐——数量不一致=不可信响应，报错。
	if len(parsed.Results) != len(chunk) {
		return nil, 0, false, fmt.Errorf("osv querybatch: 响应不可信（results %d ≠ queries %d）", len(parsed.Results), len(chunk))
	}

	hits = make([]OSVQueryHits, 0, len(chunk))
	for i, rawRes := range parsed.Results {
		var one struct {
			Vulns []OSVVuln `json:"vulns"`
		}
		if err := json.Unmarshal(rawRes, &one); err != nil {
			return nil, 0, false, fmt.Errorf("osv querybatch: 非法结果项 #%d: %w", i, err)
		}
		hits = append(hits, osvFilterNPM(chunk[i], one.Vulns))
	}
	return hits, 0, false, nil
}

// osvFilterNPM 只保留 npm 生态的 affected 条目：整条漏洞的全部 affected 都
// 非 npm ⇒ 丢弃并计数；混合漏洞 ⇒ 保留且 affected 收敛为 npm 条目（如实
// SkippedNonNPM 计数，不伪装、不静默）。
func osvFilterNPM(ref OSVQueryRef, vulns []OSVVuln) OSVQueryHits {
	hits := OSVQueryHits{Ref: ref, Vulns: []OSVVuln{}}
	for i := range vulns {
		v := vulns[i]
		npmAffected := make([]OSVAffected, 0, len(v.Affected))
		for _, a := range v.Affected {
			if strings.EqualFold(a.Package.Ecosystem, osvEcosystemNPMQuery) {
				npmAffected = append(npmAffected, a)
			}
		}
		if len(npmAffected) == 0 {
			hits.SkippedNonNPM++
			continue
		}
		v.Affected = npmAffected
		hits.Vulns = append(hits.Vulns, v)
	}
	return hits
}

// parseRetryAfter 解析秒数形态的 Retry-After（HTTP-date 形态不支持——回落
// 默认退避；负数/超大盘一律封顶 osvMaxRetryWait）。
func parseRetryAfter(raw string) time.Duration {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0
	}
	sec, err := strconv.Atoi(raw)
	if err != nil || sec < 0 {
		return 0
	}
	d := time.Duration(sec) * time.Second
	if d > osvMaxRetryWait {
		return osvMaxRetryWait
	}
	return d
}

// ctxSleep 受 ctx 控制的等待（取消即返回）。
func ctxSleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

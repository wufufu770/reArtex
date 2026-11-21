// Package peaksearch：人工专属知识蒸馏器的检索链（0.15.38，批次 I）。
//
// 4 路径故障链（C 方案口径）：内置 WebFetch/WebSearch → 外部 MCP 提供者 →
// 域名直取器 → 手动离线摄入。V1 落地路径 1 + 4（路径 2/3 框架位，provider
// 接口留位）；每提供者 5 分钟熔断（连续失败 3 次开窗）。
//
// 人工专属纪律（C 方案 5.1）：本包不注册任何 agent 工具定义——只有 HTTP API
//（X-Peak-Search-UI 显式头校验）触达；写面硬绑 strategies/legacy 双库。

package peaksearch

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// SourceDocument 是所有路径归一化后的来源文档。
type SourceDocument struct {
	URL         string `json:"url"`
	Title       string `json:"title,omitempty"`
	PublishedAt string `json:"published_at,omitempty"`
	RetrievedAt string `json:"retrieved_at,omitempty"`
	PathwayUsed string `json:"pathway_used"`
	RawText     string `json:"raw_text,omitempty"`
}

// Retriever 是检索提供者接口（4 路径实现同一契约）。
type Retriever interface {
	Name() string
	Fetch(ctx context.Context, query string) ([]SourceDocument, error)
}

// Chain 是按优先级故障切换的检索链。
type Chain struct {
	mu         sync.Mutex
	retrievers []Retriever
	cooldown   map[string]time.Time // 提供者熔断窗
	fails      map[string]int       // 连续失败计数
}

// NewChain 组装（顺序即优先级）。
func NewChain(retrievers ...Retriever) *Chain {
	return &Chain{retrievers: retrievers, cooldown: map[string]time.Time{}, fails: map[string]int{}}
}

// Fetch 按优先级试路径；失败/空结果自动下探；熔断中的提供者跳过。
func (c *Chain) Fetch(ctx context.Context, query string) ([]SourceDocument, string, error) {
	c.mu.Lock()
	var order []string
	for _, r := range c.retrievers {
		if t, cooling := c.cooldown[r.Name()]; cooling && time.Now().Before(t) {
			continue
		}
		order = append(order, r.Name())
	}
	c.mu.Unlock()

	var lastErr error
	for _, r := range c.retrievers {
		cooling := false
		c.mu.Lock()
		if t, ok := c.cooldown[r.Name()]; ok && time.Now().Before(t) {
			cooling = true
		}
		c.mu.Unlock()
		if cooling {
			continue
		}
		docs, err := r.Fetch(ctx, query)
		if err != nil {
			lastErr = err
			c.recordFailure(r.Name())
			continue
		}
		if len(docs) == 0 {
			lastErr = fmt.Errorf("路径 %s 空结果", r.Name())
			continue
		}
		return docs, r.Name(), nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("全部路径不可用")
	}
	return nil, "", lastErr
}

// recordFailure 记失败：连续 3 次开 5 分钟熔断窗。
func (c *Chain) recordFailure(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.fails[name]++
	if c.fails[name] >= 3 {
		c.cooldown[name] = time.Now().Add(5 * time.Minute)
		c.fails[name] = 0
	}
}

// ResetCooldowns 清熔断（手动/测试）。
func (c *Chain) ResetCooldowns() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cooldown = map[string]time.Time{}
	c.fails = map[string]int{}
}

// ── 路径 1：内置 WebFetch ───────────────────────────────────────────────────

// WebFetchRetriever 走平台内置 WebFetch（egress 治理复用）。
type WebFetchRetriever struct {
	Client *http.Client
}

func (w *WebFetchRetriever) Name() string { return "builtin_webfetch" }

func (w *WebFetchRetriever) Fetch(ctx context.Context, query string) ([]SourceDocument, error) {
	// V1：query 为 URL 时直接抓取；纯关键词场景走 WebSearch（框架位 V2）。
	u := strings.TrimSpace(query)
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		return nil, fmt.Errorf("非 URL 查询走 WebSearch 路径（V2）")
	}
	client := w.Client
	if client == nil {
		client = http.DefaultClient
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	req.Header.Set("User-Agent", "reartex-peaksearch")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	body := make([]byte, 512*1024)
	n, _ := resp.Body.Read(body)
	text := strings.ToValidUTF8(string(body[:n]), "")
	if strings.TrimSpace(text) == "" {
		return nil, fmt.Errorf("空响应")
	}
	return []SourceDocument{{
		URL: u, RetrievedAt: time.Now().UTC().Format(time.RFC3339),
		PathwayUsed: "builtin_webfetch", RawText: text,
	}}, nil
}

// ── 路径 4：手动离线摄入 ────────────────────────────────────────────────────

// ManualRetriever 直接用人工粘贴的文本（零网络依赖的兜底路径）。
type ManualRetriever struct{}

func (m *ManualRetriever) Name() string { return "manual" }

func (m *ManualRetriever) Fetch(ctx context.Context, query string) ([]SourceDocument, error) {
	if strings.TrimSpace(query) == "" {
		return nil, fmt.Errorf("空文本")
	}
	return []SourceDocument{{
		URL: "manual://paste", RetrievedAt: time.Now().UTC().Format(time.RFC3339),
		PathwayUsed: "manual", RawText: query,
	}}, nil
}

// sortStrings 是输出稳定助手。
func sortStrings(s []string) { sort.Strings(s) }

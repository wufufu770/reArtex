// 前端渲染错误上报（0.13.5，全量审查 P0②③ 的配套管道）：
//
//	POST /api/logs/client   body: {"message","stack","url"}
//
// 设计纪律：
//   - 走既有 logsink（add）——ring / 日志页 / SSE 流自动带出，无新存储面；
//   - 固定 tag=webclient，level=error，日志页按 tag 过滤即"白屏可定位"；
//   - 每 IP 令牌桶限流（60 条/分钟，突发 10）——渲染风暴不打爆日志面；
//   - 字段硬截断（message 300 / stack 2000 / url 300），payload 上限 8KB。
package server

import (
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	clientLogMaxMessage = 300
	clientLogMaxStack   = 2000
	clientLogMaxURL     = 300
	clientLogRatePerMin = 60
	clientLogBurst      = 10
)

// clientLogLimiter 是按远端地址的令牌桶（进程内即可：多实例前方有 LB 时各桶独立，
// 总量仍在同一量级）。
type clientLogLimiter struct {
	mu      sync.Mutex
	tokens  map[string]*tokenBucket
	rate    float64 // tokens per second
	burst   float64
	maxIdle time.Duration
}

type tokenBucket struct {
	tokens float64
	last   time.Time
}

func newClientLogLimiter() *clientLogLimiter {
	return &clientLogLimiter{
		tokens:  map[string]*tokenBucket{},
		rate:    float64(clientLogRatePerMin) / 60,
		burst:   float64(clientLogBurst),
		maxIdle: 10 * time.Minute,
	}
}

func (l *clientLogLimiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	// 顺手回收闲置桶（常驻内存不涨）。
	for k, b := range l.tokens {
		if now.Sub(b.last) > l.maxIdle {
			delete(l.tokens, k)
		}
	}
	b, ok := l.tokens[key]
	if !ok {
		b = &tokenBucket{tokens: l.burst, last: now}
		l.tokens[key] = b
	}
	elapsed := now.Sub(b.last).Seconds()
	b.tokens = min(l.burst, b.tokens+elapsed*l.rate)
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

func (s *Server) clientLogIngest(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Message string `json:"message"`
		Stack   string `json:"stack"`
		URL     string `json:"url"`
	}
	body := http.MaxBytesReader(w, r.Body, 8<<10)
	if err := json.NewDecoder(body).Decode(&req); err != nil {
		writeErr(w, 400, "请求体非法")
		return
	}
	msg := strings.TrimSpace(req.Message)
	if msg == "" {
		writeErr(w, 400, "message 必填")
		return
	}
	if !clientLogTokens.allow(clientLogKey(r)) {
		writeErr(w, 429, "上报过于频繁，本条丢弃")
		return
	}
	clip := func(s string, n int) string {
		if len(s) > n {
			return s[:n]
		}
		return s
	}
	text := clip(msg, clientLogMaxMessage)
	if u := clip(strings.TrimSpace(req.URL), clientLogMaxURL); u != "" {
		text += " | url=" + u
	}
	if st := clip(strings.TrimSpace(req.Stack), clientLogMaxStack); st != "" {
		text += "\n" + st
	}
	logSink.add(LogLine{Level: "error", Tag: "webclient", Text: text})
	writeJSON(w, 200, map[string]any{"ok": true})
}

var clientLogTokens = newClientLogLimiter()

// clientLogKey 是限流键：取远端 IP（不含端口——TCP 连接端口每次不同，按
// addr 分键等于每个新连接一个新桶，限流形同虚设；首次实测已抓出）。
func clientLogKey(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

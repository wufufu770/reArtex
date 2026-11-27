package server

// monitor_github.go：GitHub.com / GitHub Enterprise commits API 只读拉取客户端（B-09D）。
//
// 唯一目标：按仓库、分支和 last_seen_sha 拉取增量提交元数据（message/sha/
// author/date + 受控 diff 摘要）。仅请求 commits API，绝不下载整个仓库。
//
// 安全纪律（卡要求逐条）：
//   - token 经 auth_ref 引用解析（MonitorTokenResolver），只进 Authorization
//     请求头——结构性不进 URL、错误串、日志（错误只带状态码/端点路径）；
//   - 请求时双重校验地址：DNS 预解析全公网校验 + net.Dialer.Control 拨号时
//     逐一校验实际目标 IP（权威闸——覆盖重定向后的再解析与 TOCTOU）；localhost
//     变体/私网/环回/链路本地（含云 metadata 169.254.169.254）/CGNAT 全拒；
//   - redirect 仅同 host 同 https（跨 host / 降级 http 一律拒绝），最多 3 跳；
//   - 超时（http.Client Timeout + ctx 贯穿）、有界重试（429/403 遵守
//     Retry-After 封顶 30s，5xx 指数退避，总尝试 ≤3）、请求体上限（响应
//     LimitReader 8MiB）、分页页数上限（默认 10×100）；
//   - 提交内容按不可信文本处理：message 截断 4KB、author/email 截断、sha 必须
//     40 位小写 hex、date RFC3339 解析失败即整页报错（绝不带脏数据继续——
//     静默跳过会让游标越过未处理提交，永远漏拉）；
//   - 客户端无状态：拉取失败报错返回，绝不触碰 last_seen_sha（游标推进纪律
//     属 B-09F 调用方——成功才推进，失败保持）。
//
// auth_ref 解析面（B-09A 裁决落点）：内置默认支持 ""（匿名，公共仓库）与
// "env:NAME"（既有 env secret 注入通道——仓库既有先例 ARTEX_MASTER_KEY_FILE /
// ARTEX_PG_DSN_FILE 同款环境面，非新凭据系统）；其他命名空间（如加密表存储
// 引用）当前显式报错不猜测——存储面扩展属 B-09F 接线卡范围（本卡允许清单
// 只有客户端两文件），Resolver 是注入点，后续可换加密存储实现零改客户端。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// 客户端参数边界（有界纪律）。
const (
	monitorGHMaxResponse  = 8 << 20          // 单响应上限 8 MiB（B-08B 同款）
	monitorGHMaxPages     = 10               // 分页页数上限（10×100=1000 提交/次拉取）
	monitorGHPageSize     = 100              // GitHub API per_page 上限
	monitorGHMaxRetry     = 3                // 总尝试上限（含首次）
	monitorGHMaxRetryWait = 30 * time.Second // Retry-After 封顶
	monitorGHRetryBackoff = 500 * time.Millisecond
	monitorGHMessageCap   = 4 << 10 // 提交 message 截断 4KB
	monitorGHAuthorCap    = 256     // author/email 截断
	monitorGHMaxFiles     = 20      // 受控 diff 摘要文件数封顶
	monitorGHRequestGap   = 200 * time.Millisecond
)

var monitorSHA40Re = regexp.MustCompile(`^[0-9a-f]{40}$`)

// MonitorCommit 是拉取到的提交元数据（全部字段已按不可信文本消毒封顶）。
type MonitorCommit struct {
	SHA        string
	Message    string
	Author     string
	Email      string
	Date       time.Time
	Additions  int
	Deletions  int
	Files      []MonitorCommitFile
	Truncated  bool // 任一封顶命中（message/author/文件数）
	FetchStats bool // Files/统计是否有效（列表拉取=false，详情拉取=true）
}

// MonitorCommitFile 是受控 diff 摘要的文件条目——只留文件名与增删计数，
// 绝不保留原始 patch 文本（无界不可信文本）。
type MonitorCommitFile struct {
	Filename  string
	Status    string
	Additions int
	Deletions int
}

// MonitorCommitPage 是一次增量拉取的结果。
type MonitorCommitPage struct {
	Commits       []MonitorCommit
	StoppedAtSeen bool // 拉到 last_seen_sha 即止（其后皆已见）
	CapReached    bool // 页数/条数上限触顶（本轮未拉全，下一轮续拉）
	Requests      int  // 实际发出的 HTTP 请求数（限速与审计用）
}

// MonitorTokenResolver 把 auth_ref 引用解析为 token 值（""=匿名）。
type MonitorTokenResolver func(authRef string) (string, error)

var monitorEnvNameRe = regexp.MustCompile(`^env:([A-Za-z_][A-Za-z0-9_]*)$`)

// monitorDefaultTokenResolver 是内置默认解析面：env 注入通道。
// 错误面绝不回显 ref 原值（引用虽非凭据，但误把 token 传进 auth_ref 时绝不二次泄漏）。
func monitorDefaultTokenResolver(authRef string) (string, error) {
	if authRef == "" {
		return "", nil
	}
	if m := monitorEnvNameRe.FindStringSubmatch(authRef); m != nil {
		tok := strings.TrimSpace(os.Getenv(m[1]))
		if tok == "" {
			return "", fmt.Errorf("monitor: auth_ref 指向的环境变量未设置（fail-closed，绝不静默匿名请求）")
		}
		return tok, nil
	}
	return "", fmt.Errorf("monitor: auth_ref 是不支持的引用命名空间（ref 长度 %d；支持：空=匿名、env:NAME）", len(authRef))
}

// MonitorGitHubClient 是 commits API 只读客户端。
type MonitorGitHubClient struct {
	Base     string // https://api.github.com 或 https://ghe.corp.example.com/api/v3
	HTTP     *http.Client
	Resolve  MonitorTokenResolver
	MaxPages int
	PageSize int
	Gap      time.Duration // 相邻请求最小间隔（限速；0=不间隔，测试可调）
}

// 可注入网络闸（httptest 测试缝隙；生产路径恒为公网校验，测试缝隙只放行
// 127.0.0.1 环回——如实申报：这是测试基建面，不改变生产默认）。
var (
	monitorNetGuardIP = func(ip net.IP) bool { return isPublicIP(ip) }
	monitorLookupIP   = func(ctx context.Context, host string) ([]net.IP, error) {
		addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, err
		}
		out := make([]net.IP, 0, len(addrs))
		for _, a := range addrs {
			out = append(out, a.IP)
		}
		return out, nil
	}
)

// monitorGHNew 构造客户端并做 base 形态校验（https + host 策略——保存配置时
// 已校验过一道，构造时再校验一道；请求时还有 DNS/拨号双重校验）。
func monitorGHNew(base string, resolve MonitorTokenResolver) (*MonitorGitHubClient, error) {
	if err := monitorAPIURLPolicy(base); err != nil {
		return nil, err
	}
	if !strings.HasSuffix(base, "/") {
		base += "/"
	}
	if resolve == nil {
		resolve = monitorDefaultTokenResolver
	}
	return &MonitorGitHubClient{
		Base: base,
		HTTP: &http.Client{
			Timeout:       30 * time.Second,
			Transport:     ghTransport(),
			CheckRedirect: ghCheckRedirect,
		},
		Resolve:  resolve,
		MaxPages: monitorGHMaxPages,
		PageSize: monitorGHPageSize,
		Gap:      monitorGHRequestGap,
	}, nil
}

// monitorParseRetryAfter 解析 Retry-After 秒数形态（非法/负数→0，封顶 30s；
// HTTP-date 形态不支持如实回落——B-08B 同款口径）。
func monitorParseRetryAfter(raw string) time.Duration {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0
	}
	sec, err := strconv.Atoi(raw)
	if err != nil || sec <= 0 {
		return 0
	}
	d := time.Duration(sec) * time.Second
	if d > monitorGHMaxRetryWait {
		d = monitorGHMaxRetryWait
	}
	return d
}

// monitorRepoParts 从仓库 URL 提取 (owner, repo)——B-09B 形态（恰 2 段）的
// 请求时复核（防御纵深：存储层闸过的不代表本次传入的就是同一形态）。
func monitorRepoParts(repoURL string) (string, string, error) {
	u := strings.TrimSuffix(strings.TrimSpace(repoURL), "/")
	if !strings.HasPrefix(u, "https://") {
		return "", "", fmt.Errorf("monitor: 仓库 URL 必须是 https 形态")
	}
	rest := strings.TrimPrefix(u, "https://")
	i := strings.Index(rest, "/")
	if i < 0 {
		return "", "", fmt.Errorf("monitor: 仓库 URL 缺少路径段")
	}
	segs := strings.Split(rest[i+1:], "/")
	if len(segs) != 2 || segs[0] == "" || segs[1] == "" ||
		strings.ContainsAny(segs[0]+segs[1], "?#@") {
		return "", "", fmt.Errorf("monitor: 仓库 URL 必须恰 owner/repo 两段")
	}
	return segs[0], segs[1], nil
}

// ghTransport 是带拨号闸的传输面（实例无关——闸只依赖包级校验函数）。
func ghTransport() http.RoundTripper {
	return &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout: 10 * time.Second,
			Control: func(_, address string, _ syscall.RawConn) error {
				host, _, err := net.SplitHostPort(address)
				if err != nil {
					return fmt.Errorf("monitor: 拨号地址非法")
				}
				ip := net.ParseIP(strings.Trim(host, "[]"))
				if ip == nil || !monitorNetGuardIP(ip) {
					return fmt.Errorf("monitor: 目标地址 %s 被 SSRF 拨号闸拒绝", host)
				}
				return nil
			},
		}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 20 * time.Second,
		MaxIdleConns:          2,
		IdleConnTimeout:       30 * time.Second,
	}
}

// ghCheckRedirect 只允许同 host 同 https（GHE 形态 /api/v3 → /api/v3/ 归一跳合法）。
func ghCheckRedirect(req *http.Request, via []*http.Request) error {

	if len(via) >= 3 {
		return fmt.Errorf("monitor: 重定向超过 3 跳")
	}
	first := via[0]
	if req.URL.Scheme != "https" || first.URL.Scheme != "https" {
		return fmt.Errorf("monitor: 重定向降级非 https，已拒绝")
	}
	if req.URL.Host != first.URL.Host {
		return fmt.Errorf("monitor: 重定向跨 host（%s → %s），已拒绝", first.URL.Host, req.URL.Host)
	}
	return nil
}

// ghGet 发一次 GET（含有界重试）。token 只进 Authorization 头，绝不进 URL/
// 错误/日志。返回 (status, body, retryAfter, retryable, err)。
func (c *MonitorGitHubClient) ghGet(ctx context.Context, endpoint, token string) (int, []byte, time.Duration, bool, error) {
	u := c.Base + strings.TrimPrefix(endpoint, "/")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, nil, 0, false, fmt.Errorf("monitor: 构造请求失败: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "reartex-artex")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, nil, 0, false, err
	}
	defer func() { _, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<10)); _ = resp.Body.Close() }()
	retryAfter := monitorParseRetryAfter(resp.Header.Get("Retry-After"))
	body, err := io.ReadAll(io.LimitReader(resp.Body, monitorGHMaxResponse+1))
	if err != nil {
		return resp.StatusCode, nil, 0, false, fmt.Errorf("monitor: 读响应失败: %w", err)
	}
	if int64(len(body)) > monitorGHMaxResponse {
		return resp.StatusCode, nil, 0, false, fmt.Errorf("monitor: 响应超过 %d 字节上限", monitorGHMaxResponse)
	}
	retryable := resp.StatusCode == 429 || resp.StatusCode == 403 || resp.StatusCode >= 500
	return resp.StatusCode, body, retryAfter, retryable, nil
}

// ghGetBounded 是带 429/403/5xx 有界重试的 GET（总尝试 ≤ monitorGHMaxRetry）。
func (c *MonitorGitHubClient) ghGetBounded(ctx context.Context, endpoint, token string) (int, []byte, error) {
	var lastErr error
	for attempt := 1; attempt <= monitorGHMaxRetry; attempt++ {
		status, body, retryAfter, retryable, err := c.ghGet(ctx, endpoint, token)
		if err != nil {
			return 0, nil, err // 网络/redirect/尺寸错误：不重试（redirect 拒绝是策略性失败）
		}
		if status == http.StatusOK {
			return status, body, nil
		}
		lastErr = fmt.Errorf("monitor: commits API 返回 %d（%s）", status, endpointLabel(endpoint))
		if !retryable || attempt == monitorGHMaxRetry {
			return status, nil, lastErr
		}
		wait := monitorGHRetryBackoff << (attempt - 1)
		if retryAfter > 0 {
			wait = retryAfter // Retry-After 优先（已封顶 30s）
		}
		if wait > monitorGHMaxRetryWait {
			wait = monitorGHMaxRetryWait
		}
		select {
		case <-ctx.Done():
			return 0, nil, ctx.Err()
		case <-time.After(wait):
		}
	}
	return 0, nil, lastErr
}

// endpointLabel 提取端点短标签（不含 query、不含 token——token 从不在 URL）。
func endpointLabel(endpoint string) string {
	if i := strings.Index(endpoint, "?"); i >= 0 {
		endpoint = endpoint[:i]
	}
	return endpoint
}

// preflight 是请求时 DNS/地址复核（卡要求 2：保存配置时已校验形态与 host
// 策略，请求时再解析 DNS 并逐一校验公网性——DNS 级 SSRF 在客户端此层闭环；
// 拨号闸是其后第二道权威闸）。host 无解析/解析失败一律 fail-closed。
func (c *MonitorGitHubClient) preflight(ctx context.Context) error {
	u, err := url.Parse(c.Base)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return fmt.Errorf("monitor: base URL 形态非法")
	}
	host := u.Hostname()
	ips, err := monitorLookupIP(ctx, host)
	if err != nil {
		return fmt.Errorf("monitor: 解析 host %s 失败: %v", host, err)
	}
	if len(ips) == 0 {
		return fmt.Errorf("monitor: host %s 无解析结果", host)
	}
	for _, ip := range ips {
		if !monitorNetGuardIP(ip) {
			return fmt.Errorf("monitor: host %s 解析到非公网地址，已被 SSRF 策略拒绝", host)
		}
	}
	return nil
}

// ListNewCommits 按 last_seen_sha 拉取增量提交（新→旧，最新在前）。
// lastSeenSHA 为空串 = 首轮全量起步（仍受页数上限）。
func (c *MonitorGitHubClient) ListNewCommits(ctx context.Context, repoURL, branch, lastSeenSHA, authRef string) (*MonitorCommitPage, error) {
	owner, repo, err := monitorRepoParts(repoURL)
	if err != nil {
		return nil, err
	}
	if branch == "" || strings.ContainsAny(branch, " \t\r\n") || len(branch) > 128 {
		return nil, fmt.Errorf("monitor: branch 形态非法")
	}
	if lastSeenSHA != "" && !monitorSHA40Re.MatchString(lastSeenSHA) {
		return nil, fmt.Errorf("monitor: last_seen_sha 必须是 40 位小写 hex")
	}
	token, err := c.Resolve(authRef)
	if err != nil {
		return nil, err
	}
	if err := c.preflight(ctx); err != nil {
		return nil, err
	}
	if c.PageSize <= 0 || c.PageSize > monitorGHPageSize {
		c.PageSize = monitorGHPageSize
	}
	if c.MaxPages <= 0 {
		c.MaxPages = monitorGHMaxPages
	}
	out := &MonitorCommitPage{Commits: []MonitorCommit{}}
	for page := 1; page <= c.MaxPages; page++ {
		if page > 1 && c.Gap > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(c.Gap):
			}
		}
		endpoint := fmt.Sprintf("repos/%s/%s/commits?sha=%s&per_page=%d&page=%d",
			url.PathEscape(owner), url.PathEscape(repo), url.PathEscape(branch), c.PageSize, page)
		_, body, err := c.ghGetBounded(ctx, endpoint, token)
		out.Requests++
		if err != nil {
			return nil, err
		}
		commits, err := parseCommitList(body)
		if err != nil {
			return nil, err
		}
		// 本页为空或短页 = 到底。
		if len(commits) == 0 {
			return out, nil
		}
		for i := range commits {
			if lastSeenSHA != "" && commits[i].SHA == lastSeenSHA {
				out.StoppedAtSeen = true
				return out, nil // seen 之前（含本轮已累积）就是全部增量
			}
			out.Commits = append(out.Commits, commits[i])
		}
		if len(commits) < c.PageSize {
			return out, nil
		}
	}
	out.CapReached = true
	return out, nil
}

// GetCommitDetail 拉单个提交的受控 diff 摘要（文件名+增删计数+总量，封顶
// monitorGHMaxFiles 条；原始 patch 文本绝不保留）。
func (c *MonitorGitHubClient) GetCommitDetail(ctx context.Context, repoURL, sha, authRef string) (*MonitorCommit, error) {
	owner, repo, err := monitorRepoParts(repoURL)
	if err != nil {
		return nil, err
	}
	if !monitorSHA40Re.MatchString(sha) {
		return nil, fmt.Errorf("monitor: sha 必须是 40 位小写 hex")
	}
	token, err := c.Resolve(authRef)
	if err != nil {
		return nil, err
	}
	if err := c.preflight(ctx); err != nil {
		return nil, err
	}
	endpoint := fmt.Sprintf("repos/%s/%s/commits/%s", url.PathEscape(owner), url.PathEscape(repo), sha)
	_, body, err := c.ghGetBounded(ctx, endpoint, token)
	if err != nil {
		return nil, err
	}
	return parseCommitDetail(body)
}

// ---- wire 解析（不可信响应校验面）----

type ghCommitListEntry struct {
	SHA    string `json:"sha"`
	Commit struct {
		Message string `json:"message"`
		Author  struct {
			Name  string `json:"name"`
			Email string `json:"email"`
			Date  string `json:"date"`
		} `json:"author"`
	} `json:"commit"`
	Author struct {
		Login string `json:"login"`
	} `json:"author"`
}

type ghCommitDetail struct {
	ghCommitListEntry
	Stats struct {
		Additions int `json:"additions"`
		Deletions int `json:"deletions"`
	} `json:"stats"`
	Files []struct {
		Filename  string `json:"filename"`
		Status    string `json:"status"`
		Additions int    `json:"additions"`
		Deletions int    `json:"deletions"`
	} `json:"files"`
}

// parseCommitList 校验并转换列表响应：任一条目 sha/日期形态非法即整页报错
// （绝不带脏数据继续——静默跳过会让游标越过未处理提交）。
func parseCommitList(body []byte) ([]MonitorCommit, error) {
	var entries []ghCommitListEntry
	if err := json.Unmarshal(body, &entries); err != nil {
		return nil, fmt.Errorf("monitor: 提交列表不是合法 JSON 数组")
	}
	out := make([]MonitorCommit, 0, len(entries))
	for _, e := range entries {
		mc, err := entryToCommit(e.SHA, e.Commit.Message, e.Author.Login, e.Commit.Author.Name,
			e.Commit.Author.Email, e.Commit.Author.Date)
		if err != nil {
			return nil, err
		}
		out = append(out, *mc)
	}
	return out, nil
}

func parseCommitDetail(body []byte) (*MonitorCommit, error) {
	var d ghCommitDetail
	if err := json.Unmarshal(body, &d); err != nil {
		return nil, fmt.Errorf("monitor: 提交详情不是合法 JSON 对象")
	}
	mc, err := entryToCommit(d.SHA, d.Commit.Message, d.Author.Login, d.Commit.Author.Name,
		d.Commit.Author.Email, d.Commit.Author.Date)
	if err != nil {
		return nil, err
	}
	mc.FetchStats = true
	mc.Additions = d.Stats.Additions
	mc.Deletions = d.Stats.Deletions
	for i, f := range d.Files {
		if i >= monitorGHMaxFiles {
			mc.Truncated = true
			break
		}
		mc.Files = append(mc.Files, MonitorCommitFile{
			Filename:  monitorTruncate(f.Filename, monitorGHAuthorCap, &mc.Truncated),
			Status:    monitorTruncate(f.Status, 32, &mc.Truncated),
			Additions: f.Additions,
			Deletions: f.Deletions,
		})
	}
	return mc, nil
}

func entryToCommit(sha, message, login, name, email, date string) (*MonitorCommit, error) {
	if !monitorSHA40Re.MatchString(sha) {
		return nil, fmt.Errorf("monitor: 提交条目 sha 形态非法（拒绝整页）")
	}
	t, err := time.Parse(time.RFC3339, date)
	if err != nil {
		return nil, fmt.Errorf("monitor: 提交条目日期 %q 非法（拒绝整页）", monitorTruncate(date, 64, new(bool)))
	}
	mc := &MonitorCommit{SHA: sha, Date: t}
	mc.Message = monitorTruncate(message, monitorGHMessageCap, &mc.Truncated)
	author := strings.TrimSpace(login)
	if author == "" {
		author = strings.TrimSpace(name)
	}
	mc.Author = monitorTruncate(author, monitorGHAuthorCap, &mc.Truncated)
	mc.Email = monitorTruncate(strings.TrimSpace(email), monitorGHAuthorCap, &mc.Truncated)
	return mc, nil
}

// monitorTruncate 按字节截断并打标（「…(截断)」——仓库既有如实截断纪律）。
func monitorTruncate(s string, cap int, flagged *bool) string {
	if len(s) <= cap {
		return s
	}
	if flagged != nil {
		*flagged = true
	}
	cut := cap
	for cut > 0 && (s[cut]&0xC0) == 0x80 { // 不切 UTF-8 中间
		cut--
	}
	return s[:cut] + "…(截断)"
}

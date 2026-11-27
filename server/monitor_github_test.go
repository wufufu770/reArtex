package server

// monitor_github_test.go：B-09D commits API 只读拉取客户端验收（httptest TLS 本地环回，零真实出网）。
//
// 卡验收逐项：
//   - GitHub.com 形态（api.github.com 根路径）与 GHE 形态（/api/v3 前缀）端点；
//   - 分页（短页收尾、页数上限）与 last_seen_sha 增量（见 sha 即止，其后皆已见）；
//   - 429/403 遵守 Retry-After 有界重试；403 恒续=有界失败绝不无限；
//   - 超时（client Timeout 穿透为错误）；
//   - redirect 到禁用主机（跨 host/降级 http/metadata 地址）全拒；同 host 归一跳放行；
//   - 异常 URL（段数/branch/游标形态/auth_ref 命名空间）请求前拒绝；
//   - 超大提交 message/diff（截断打标、文件数封顶、响应体积上限）；
//   - token 只进 Authorization 头（URL/错误串绝无 token 字样）。
//
// 测试缝隙（如实申报）：httptest.NewTLSServer 用自签证书——测试内对客户端
// Transport 设 InsecureSkipVerify（仅测试进程内实例，生产构造路径不受影响）；
// monitorGHAllowLoopback 放开环回（生产恒为公网校验）。两缝隙均 t.Cleanup 恢复。

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func monitorGHAllowLoopback(t *testing.T) {
	t.Helper()
	oldGuard, oldLookup := monitorNetGuardIP, monitorLookupIP
	monitorNetGuardIP = func(ip net.IP) bool { return ip.IsLoopback() || oldGuard(ip) }
	monitorLookupIP = func(ctx context.Context, host string) ([]net.IP, error) {
		if host == "127.0.0.1" {
			return []net.IP{net.ParseIP("127.0.0.1")}, nil
		}
		return oldLookup(ctx, host)
	}
	t.Cleanup(func() { monitorNetGuardIP, monitorLookupIP = oldGuard, oldLookup })
}

func monitorGHSHA(idx int) string {
	return fmt.Sprintf("%040x", idx)
}

func monitorGHStaticResolver(token string) MonitorTokenResolver {
	return func(authRef string) (string, error) {
		if authRef == "" {
			return "", nil
		}
		return token, nil
	}
}

// monitorGHTestClient 构造测试客户端：https 构造闸过（TLS 服务器）+ 环回缝隙
// + 自签证书缝隙 + 零间隔。
func monitorGHTestClient(t *testing.T, base string, res MonitorTokenResolver) *MonitorGitHubClient {
	t.Helper()
	c, err := monitorGHNew(base, res)
	if err != nil {
		t.Fatalf("构造客户端: %v", err)
	}
	c.Gap = 0
	if tr, ok := c.HTTP.Transport.(*http.Transport); ok {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} // httptest 自签证书缝隙
	}
	return c
}

func monitorGHCommitsJSON(fromIdx, toIdx int) string {
	// 新→旧：fromIdx（最新）..toIdx（最旧，含）。
	var b strings.Builder
	b.WriteByte('[')
	for i := fromIdx; i >= toIdx; i-- {
		if i != fromIdx {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"sha":%q,"commit":{"message":"commit %d 安全更新","author":{"name":"alice","email":"alice@example.com","date":"2026-11-20T10:00:00Z"},"committer":{}},"author":{"login":"alice-%d"},"parents":[]}`,
			monitorGHSHA(i), i, i%7)
	}
	b.WriteByte(']')
	return b.String()
}

func TestMonitorGHListIncrementalAndPagination(t *testing.T) {
	monitorGHAllowLoopback(t)

	// GHE 形态 base：/api/v3 前缀。105 个提交：page1=100（idx104..5）、page2=5（idx4..0）。
	var authSeen []string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/v3/repos/o/r/commits") {
			w.WriteHeader(404)
			return
		}
		if v := r.Header.Get("Authorization"); v != "" {
			authSeen = append(authSeen, v)
		}
		if strings.Contains(r.URL.RawQuery, "tok") {
			t.Errorf("token 绝不出现在 URL query: %s", r.URL.RawQuery)
		}
		if r.Header.Get("Accept") != "application/vnd.github+json" || r.Header.Get("User-Agent") != "reartex-artex" {
			t.Errorf("请求头不符: Accept=%q UA=%q", r.Header.Get("Accept"), r.Header.Get("User-Agent"))
		}
		if r.URL.Query().Get("per_page") != "100" {
			t.Errorf("per_page 应为 100: %s", r.URL.RawQuery)
		}
		switch r.URL.Query().Get("page") {
		case "1":
			_, _ = w.Write([]byte(monitorGHCommitsJSON(104, 5)))
		case "2":
			_, _ = w.Write([]byte(monitorGHCommitsJSON(4, 0)))
		default:
			w.WriteHeader(400)
		}
	}))
	defer srv.Close()

	c := monitorGHTestClient(t, srv.URL+"/api/v3", monitorGHStaticResolver("tok"))

	// ① 首轮全量（lastSeenSHA 空）：两页收齐 105 条，短页收尾，CapReached=false。
	pg, err := c.ListNewCommits(context.Background(), "https://github.com/o/r", "main", "", "env:T")
	if err != nil {
		t.Fatalf("全量拉取应成功: %v", err)
	}
	if len(pg.Commits) != 105 || pg.StoppedAtSeen || pg.CapReached || pg.Requests != 2 {
		t.Fatalf("全量结果不符: n=%d seen=%v cap=%v req=%d", len(pg.Commits), pg.StoppedAtSeen, pg.CapReached, pg.Requests)
	}
	if pg.Commits[0].SHA != monitorGHSHA(104) || pg.Commits[104].SHA != monitorGHSHA(0) {
		t.Fatalf("新→旧顺序不符: first=%s last=%s", pg.Commits[0].SHA, pg.Commits[104].SHA)
	}
	// GHE /api/v3 前缀端点已由 404 分支实证（非前缀路径不会命中成功分支）。
	if len(authSeen) != 2 || authSeen[0] != "Bearer tok" {
		t.Fatalf("token 只进 Authorization 头: %v", authSeen)
	}

	// ② 增量：lastSeen=idx50 → 见 sha 即止，只留 idx104..51 共 54 条，单请求。
	pg, err = c.ListNewCommits(context.Background(), "https://github.com/o/r", "main", monitorGHSHA(50), "env:T")
	if err != nil {
		t.Fatal(err)
	}
	if len(pg.Commits) != 54 || !pg.StoppedAtSeen || pg.Requests != 1 {
		t.Fatalf("增量结果不符: n=%d seen=%v req=%d", len(pg.Commits), pg.StoppedAtSeen, pg.Requests)
	}
	if pg.Commits[0].SHA != monitorGHSHA(104) || pg.Commits[53].SHA != monitorGHSHA(51) {
		t.Fatalf("增量边界不符: first=%s last=%s", pg.Commits[0].SHA, pg.Commits[53].SHA)
	}

	// ③ GitHub.com 形态：根路径（无 /api/v3 前缀）。
	srv2 := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/repos/o/r/commits") {
			w.WriteHeader(404)
			return
		}
		_, _ = w.Write([]byte(monitorGHCommitsJSON(1, 0)))
	}))
	defer srv2.Close()
	c2 := monitorGHTestClient(t, srv2.URL, monitorGHStaticResolver(""))
	pg, err = c2.ListNewCommits(context.Background(), "https://github.com/o/r", "dev", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(pg.Commits) != 2 || pg.Requests != 1 {
		t.Fatalf("GitHub.com 形态结果不符: n=%d req=%d", len(pg.Commits), pg.Requests)
	}
}

func TestMonitorGHRetry429And403(t *testing.T) {
	monitorGHAllowLoopback(t)

	// ① 429 + Retry-After: 0 一次后成功（有界重试生效）。
	var n int
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		if n == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(429)
			return
		}
		_, _ = w.Write([]byte(monitorGHCommitsJSON(0, 0)))
	}))
	defer srv.Close()
	c := monitorGHTestClient(t, srv.URL, monitorGHStaticResolver("t"))
	pg, err := c.ListNewCommits(context.Background(), "https://github.com/o/r", "main", "", "env:T")
	if err != nil {
		t.Fatalf("429 后重试应成功: %v", err)
	}
	if n != 2 || len(pg.Commits) != 1 {
		t.Fatalf("429 重试计数不符: n=%d commits=%d", n, len(pg.Commits))
	}

	// ② 403 恒续：有界失败（恰 3 次尝试），错误带状态码且绝不带 token 字样。
	n = 0
	srv2 := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(403)
	}))
	defer srv2.Close()
	c2 := monitorGHTestClient(t, srv2.URL, monitorGHStaticResolver("SECRETTOK"))
	_, err = c2.ListNewCommits(context.Background(), "https://github.com/o/r", "main", "", "env:T")
	if err == nil {
		t.Fatal("恒 403 应有界失败")
	}
	if n != 3 {
		t.Fatalf("总尝试应恰 3: n=%d", n)
	}
	if strings.Contains(err.Error(), "SECRETTOK") {
		t.Fatalf("错误串绝无 token: %s", err.Error())
	}

	// ③ 4xx 永久失败不重试（恰 1 次）。
	n = 0
	srv3 := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		w.WriteHeader(404)
	}))
	defer srv3.Close()
	c3 := monitorGHTestClient(t, srv3.URL, monitorGHStaticResolver(""))
	if _, err = c3.ListNewCommits(context.Background(), "https://github.com/o/r", "main", "", ""); err == nil || n != 1 {
		t.Fatalf("404 应单次失败不重试: err=%v n=%d", err, n)
	}
}

func TestMonitorGHRedirectGuard(t *testing.T) {
	monitorGHAllowLoopback(t)

	// ① 跨 host 重定向拒绝。
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://evil.example.com/repos/o/r/commits", http.StatusFound)
	}))
	defer srv.Close()
	c := monitorGHTestClient(t, srv.URL, monitorGHStaticResolver(""))
	if _, err := c.ListNewCommits(context.Background(), "https://github.com/o/r", "main", "", ""); err == nil {
		t.Fatal("跨 host redirect 应拒绝")
	} else if !strings.Contains(err.Error(), "跨 host") {
		t.Fatalf("错误应说明跨 host: %v", err)
	}

	// ② 降级 http 拒绝。
	srv2 := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://127.0.0.1/repos/o/r/commits", http.StatusFound)
	}))
	defer srv2.Close()
	c2 := monitorGHTestClient(t, srv2.URL, monitorGHStaticResolver(""))
	if _, err := c2.ListNewCommits(context.Background(), "https://github.com/o/r", "main", "", ""); err == nil {
		t.Fatal("降级 http redirect 应拒绝")
	}

	// ③ metadata 地址（169.254.169.254）：重定向 + 拨号闸双层拒绝。
	srv3 := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://169.254.169.254/latest/meta-data/", http.StatusFound)
	}))
	defer srv3.Close()
	c3 := monitorGHTestClient(t, srv3.URL, monitorGHStaticResolver(""))
	if _, err := c3.ListNewCommits(context.Background(), "https://github.com/o/r", "main", "", ""); err == nil {
		t.Fatal("redirect 到 metadata 地址应拒绝")
	}

	// ④ 同 host 归一跳放行（/api/v3 → /api/v3/ 形态）。
	var hits int
	var srv4 *httptest.Server
	srv4 = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.URL.Path == "/api/v3/repos/o/r/commits" {
			// 同 host 同 https（自签环回）——归一跳合法。
			http.Redirect(w, r, srv4.URL+"/api/v3/repos/o/r/commits/", http.StatusFound)
			return
		}
		_, _ = w.Write([]byte(monitorGHCommitsJSON(0, 0)))
	}))
	defer srv4.Close()
	c4 := monitorGHTestClient(t, srv4.URL+"/api/v3", monitorGHStaticResolver(""))
	pg, err := c4.ListNewCommits(context.Background(), "https://github.com/o/r", "main", "", "")
	if err != nil || len(pg.Commits) != 1 || hits != 2 {
		t.Fatalf("同 host redirect 应放行: err=%v n=%d hits=%d", err, len(pg.Commits), hits)
	}
}

func TestMonitorGHValidationAndTimeout(t *testing.T) {
	monitorGHAllowLoopback(t)
	c, err := monitorGHNew("https://api.github.com", nil) // nil resolver → 默认解析面
	if err != nil {
		t.Fatal(err)
	}
	c.Gap = 0

	// 异常形态全部请求前拒绝（不落任何 HTTP）。
	bad := []struct {
		name                      string
		repoURL, branch, lastSeen string
	}{
		{"单段仓库", "https://github.com/onlyowner", "main", ""},
		{"三段仓库", "https://github.com/o/r/extra", "main", ""},
		{"非 https", "http://github.com/o/r", "main", ""},
		{"空 branch", "https://github.com/o/r", "", ""},
		{"带空白 branch", "https://github.com/o/r", "a b", ""},
		{"非法游标", "https://github.com/o/r", "main", "ZZZ"},
	}
	for _, tc := range bad {
		if _, err := c.ListNewCommits(context.Background(), tc.repoURL, tc.branch, tc.lastSeen, ""); err == nil {
			t.Fatalf("%s 应拒绝", tc.name)
		}
	}

	// auth_ref 命名空间：默认解析面只收 "" 与 env:NAME。
	if _, err := c.ListNewCommits(context.Background(), "https://github.com/o/r", "main", "", "key:default"); err == nil {
		t.Fatal("不支持的 auth_ref 命名空间应报错")
	} else if strings.Contains(err.Error(), "key:default") {
		t.Fatalf("错误面不回显 ref 原值: %s", err.Error())
	}
	if _, err := c.ListNewCommits(context.Background(), "https://github.com/o/r", "main", "", "env:MONITOR_TEST_UNSET_VAR_XYZ"); err == nil {
		t.Fatal("env 未设置应 fail-closed 报错")
	}
	t.Setenv("MONITOR_TEST_VAR_OK", "good-token")
	tok, err := monitorDefaultTokenResolver("env:MONITOR_TEST_VAR_OK")
	if err != nil || tok != "good-token" {
		t.Fatalf("env 解析应成功: %v %q", err, tok)
	}
	if tok, err := monitorDefaultTokenResolver(""); err != nil || tok != "" {
		t.Fatalf("空 auth_ref = 匿名: %v %q", err, tok)
	}

	// base URL 构造闸：私网/环回/非 https/坏形态在构造时拒绝（不依赖拨号闸）。
	// 本测试在环回缝隙内运行——这里临时恢复生产公网闸以验证构造闸本身，
	// 验证完立即恢复环回缝隙（后续超时用例还要构造环回客户端）。
	monitorNetGuardIP = isPublicIP
	for _, badBase := range []string{"https://127.0.0.1/api/v3", "http://api.github.com", "not-a-url"} {
		if _, err := monitorGHNew(badBase, nil); err == nil {
			t.Fatalf("base %q 构造应拒绝", badBase)
		}
	}
	monitorNetGuardIP = func(ip net.IP) bool { return ip.IsLoopback() || isPublicIP(ip) }

	// 超时穿透（TLS 环回 + 自签缝隙）。
	slow := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		_, _ = w.Write([]byte("[]"))
	}))
	defer slow.Close()
	cc := monitorGHTestClient(t, slow.URL, monitorGHStaticResolver(""))
	cc.HTTP.Timeout = 50 * time.Millisecond
	if _, err := cc.ListNewCommits(context.Background(), "https://github.com/o/r", "main", "", ""); err == nil {
		t.Fatal("超时应报错")
	}
}

func TestMonitorGHUntrustedCaps(t *testing.T) {
	monitorGHAllowLoopback(t)

	// ① 超大 message（100KB）截断 4KB 打标；超大响应（9MB 单条 message）体积上限拒。
	huge := strings.Repeat("A", 100<<10)
	mega := strings.Repeat("B", 9<<20)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 单提交详情路径（/repos/o/r/commits/<sha>，剩余段非空且非列表自身）→ 超大响应。
		if seg := strings.TrimPrefix(r.URL.Path, "/repos/o/r/commits/"); seg != "" && !strings.Contains(seg, "/") {
			_, _ = w.Write([]byte(mega))
			return
		}
		_, _ = w.Write([]byte(fmt.Sprintf(
			`[{"sha":%q,"commit":{"message":%q,"author":{"name":"n","email":"e@x","date":"2026-11-20T10:00:00Z"}},"author":{"login":"l"},"parents":[]}]`,
			monitorGHSHA(1), huge)))
	}))
	defer srv.Close()
	c := monitorGHTestClient(t, srv.URL, monitorGHStaticResolver(""))
	pg, err := c.ListNewCommits(context.Background(), "https://github.com/o/r", "main", "", "")
	if err != nil {
		t.Fatal(err)
	}
	mc := pg.Commits[0]
	if len(mc.Message) > 4096+len("…(截断)") || !strings.HasSuffix(mc.Message, "…(截断)") || !mc.Truncated {
		t.Fatalf("超大 message 应截断打标: len=%d truncated=%v", len(mc.Message), mc.Truncated)
	}
	if _, err := c.GetCommitDetail(context.Background(), "https://github.com/o/r", "deadbeef", ""); err == nil {
		t.Fatal("非 40-hex sha 应请求前拒绝")
	}
	if _, err := c.GetCommitDetail(context.Background(), "https://github.com/o/r", monitorGHSHA(999), ""); err == nil || !strings.Contains(err.Error(), "上限") {
		t.Fatalf("超大响应应体积上限拒绝: %v", err)
	}

	// ② 详情受控 diff 摘要：30 文件封顶 20 + Truncated；stats 保留；无 patch 字段。
	var files strings.Builder
	for i := 0; i < 30; i++ {
		if i > 0 {
			files.WriteByte(',')
		}
		fmt.Fprintf(&files, `{"filename":"pkg/file%d.go","status":"modified","additions":%d,"deletions":1,"patch":"@@ -1 +1 @@ huge-untrusted"}`, i, i)
	}
	srv2 := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"sha":%q,"commit":{"message":"m","author":{"name":"n","email":"e","date":"2026-11-20T10:00:00Z"}},"author":{"login":"l"},"stats":{"additions":100,"deletions":5,"total":105},"files":[%s]}`,
			monitorGHSHA(7), files.String())
	}))
	defer srv2.Close()
	c2 := monitorGHTestClient(t, srv2.URL, monitorGHStaticResolver(""))
	d, err := c2.GetCommitDetail(context.Background(), "https://github.com/o/r", monitorGHSHA(7), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Files) != 20 || !d.Truncated || d.Additions != 100 || d.Deletions != 5 || !d.FetchStats {
		t.Fatalf("详情封顶不符: files=%d trunc=%v +%d/-%d stats=%v", len(d.Files), d.Truncated, d.Additions, d.Deletions, d.FetchStats)
	}
	for _, f := range d.Files {
		if strings.Contains(f.Filename, "patch") {
			t.Fatalf("受控摘要只留文件名/计数: %+v", f)
		}
	}

	// ③ 脏条目整页拒绝（sha 短/日期非法/非 JSON）——绝不带脏数据继续。
	for i, dirty := range []string{
		`[{"sha":"shortsha","commit":{"message":"m","author":{"date":"2026-11-20T10:00:00Z"}}}]`,
		`[{"sha":"` + monitorGHSHA(1) + `","commit":{"message":"m","author":{"date":"not-a-date"}}}]`,
		`not-json`,
	} {
		srv3 := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(dirty))
		}))
		c3 := monitorGHTestClient(t, srv3.URL, monitorGHStaticResolver(""))
		if _, err := c3.ListNewCommits(context.Background(), "https://github.com/o/r", "main", "", ""); err == nil {
			t.Fatalf("脏条目 #%d 应整页拒绝", i)
		}
		srv3.Close()
	}
}

// TestMonitorHostPolicyFQDNTrail：FQDN 尾点归一封堵（localhost. 绕过面）——
// B-09D 开发期发现（httptest 测试过程暴露）的 B-09C 策略缝隙补丁验收。
func TestMonitorHostPolicyFQDNTrail(t *testing.T) {
	for _, bad := range []string{"https://localhost./x", "https://localhost.:8443/x", "https://api.local./x", "https://169.254.169.254./x"} {
		if err := monitorAPIURLPolicy(bad); err == nil {
			t.Fatalf("尾点 FQDN %q 应被拒（尾点归一后命中 localhost/metadata 闸）", bad)
		}
	}
	// 正常 FQDN 尾点仍合法（github.com. 是 github.com 的规范形态）。
	if err := monitorAPIURLPolicy("https://github.com./o/r"); err != nil {
		t.Fatalf("公网 FQDN 尾点应放行: %v", err)
	}
}

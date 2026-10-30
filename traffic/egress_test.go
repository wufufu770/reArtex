package traffic

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// egressScopeStub 是出网判定的测试替身：按任务号给出允许集，可选地让判定失败，
// 并把判定结论记下来供断言。
type egressScopeStub struct {
	hosts     map[int64][]string
	err       error
	decisions []EgressDecision
}

// EgressHosts implements EgressScope.
func (s *egressScopeStub) EgressHosts(_ context.Context, taskID int64) ([]string, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.hosts[taskID], nil
}

// RecordEgressDecision implements EgressScope.
func (s *egressScopeStub) RecordEgressDecision(_ context.Context, d EgressDecision) error {
	s.decisions = append(s.decisions, d)
	return nil
}

// egressRequest 造一个带任务身份的代理请求。
func egressRequest(t *testing.T, taskID int64, target string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	if taskID > 0 {
		req.Header.Set(egressTaskTokenHeader, SignEgressTaskToken(taskID))
	}
	return req
}

// TestEgressTaskTokenRoundTrip pins that a token only verifies for the task it
// was signed for: 认得出来不等于有资格声称 —— 否则 agent 可以声称自己是别的
// 任务，拿到那个任务的范围去探测。
func TestEgressTaskTokenRoundTrip(t *testing.T) {
	token := SignEgressTaskToken(7)
	if token == "" {
		t.Fatal("任务 7 必须签发出令牌")
	}
	id, ok := parseEgressTaskToken(token)
	if !ok || id != 7 {
		t.Fatalf("令牌解析 = %d, %v；期望 7, true", id, ok)
	}
	// 换成别的任务号 → 签名对不上 → 不认。
	forged := strings.Replace(token, "t7.", "t8.", 1)
	if id, ok := parseEgressTaskToken(forged); ok {
		t.Fatalf("伪造的任务号被接受：%d", id)
	}
	// 篡改签名 → 不认。
	tampered := token[:len(token)-1] + "A"
	if _, ok := parseEgressTaskToken(tampered); ok {
		t.Fatal("被篡改的令牌被接受")
	}
	// 非任务号 → 不认。
	if _, ok := parseEgressTaskToken("nonsense"); ok {
		t.Fatal("随便一个字符串被当成令牌")
	}
	// 没有任务就没有身份。
	if SignEgressTaskToken(0) != "" {
		t.Fatal("任务号 <= 0 不该签发令牌")
	}
}

// TestProxyAuthCarriesTaskToken pins that the token survives the trip through
// the proxy URL's userinfo（各 HTTP 客户端都把它变成 Proxy-Authorization）。
func TestProxyAuthCarriesTaskToken(t *testing.T) {
	token := SignEgressTaskToken(9)
	basic := "Basic " + base64.StdEncoding.EncodeToString([]byte(token+":"))
	id, ok := parseProxyAuthTask(basic)
	if !ok || id != 9 {
		t.Fatalf("Proxy-Authorization 解析 = %d, %v；期望 9, true", id, ok)
	}
	if _, ok := parseProxyAuthTask("Bearer " + token); ok {
		t.Fatal("非 Basic 的 Proxy-Authorization 不该被认成任务身份")
	}
	if _, ok := parseProxyAuthTask("Basic !!!not-base64!!!"); ok {
		t.Fatal("乱码的 Proxy-Authorization 不该被认成任务身份")
	}
}

// TestEgressDecideAllowsInScopeAndDeniesOutOfScope pins the core judgement:
// 范围内的放行，范围外的拒绝。
func TestEgressDecideAllowsInScopeAndDeniesOutOfScope(t *testing.T) {
	tr := &Traffic{}
	tr.SetEgressScope(&egressScopeStub{hosts: map[int64][]string{5: {"a.example.com"}}})

	in := tr.egressDecide(context.Background(), egressRequest(t, 5, "http://a.example.com/x"), 5, true)
	if !in.Allowed {
		t.Fatalf("范围内目的地必须放行：%s", in.Reason)
	}
	out := tr.egressDecide(context.Background(), egressRequest(t, 5, "http://b.example.com/x"), 5, true)
	if out.Allowed {
		t.Fatal("范围外目的地必须被拒")
	}
	if !strings.Contains(out.Reason, "不在本任务范围内") {
		t.Fatalf("拒绝原因必须写明是范围外，实际：%s", out.Reason)
	}
}

// TestEgressDecideFailsClosedOnScopeError pins fail-closed：范围判定本身出错时
// 拒绝，不是放行。
func TestEgressDecideFailsClosedOnScopeError(t *testing.T) {
	tr := &Traffic{}
	tr.SetEgressScope(&egressScopeStub{err: errors.New("范围查询失败")})
	d := tr.egressDecide(context.Background(), egressRequest(t, 5, "http://a.example.com/x"), 5, true)
	if d.Allowed {
		t.Fatal("范围判定失败必须拒绝")
	}
	if !strings.Contains(d.Reason, "范围判定失败") {
		t.Fatalf("拒绝原因必须说明是判定失败，实际：%s", d.Reason)
	}
}

// TestEgressDecideFailsClosedWhenScopeNotWired pins that a proxy whose range
// gate was never wired refuses task-attributed traffic: 没有判定就没有授权边界。
func TestEgressDecideFailsClosedWhenScopeNotWired(t *testing.T) {
	tr := &Traffic{}
	d := tr.egressDecide(context.Background(), egressRequest(t, 5, "http://a.example.com/x"), 5, true)
	if d.Allowed {
		t.Fatal("未接线时带身份的出网必须被拒")
	}
	if !strings.Contains(d.Reason, "未接线") {
		t.Fatalf("拒绝原因必须说明判定未接线，实际：%s", d.Reason)
	}
}

// TestEgressDecideUnattributedIsExplicit pins the unattributed state：没有任务
// 身份的请求放行，但必须明确标记为「未经范围校验」，不许悄悄当成放行。
func TestEgressDecideUnattributedIsExplicit(t *testing.T) {
	tr := &Traffic{}
	tr.SetEgressScope(&egressScopeStub{hosts: map[int64][]string{5: {"a.example.com"}}})
	d := tr.egressDecide(context.Background(), egressRequest(t, 0, "http://b.example.com/x"), 0, false)
	if !d.Allowed {
		t.Fatalf("未归属请求按设计放行（平台自身流量无任务身份），实际被拒：%s", d.Reason)
	}
	if d.Attributed {
		t.Fatal("未归属请求不得声称有身份")
	}
	if !strings.Contains(d.Reason, "未携带任务身份") {
		t.Fatalf("未归属必须显式标记，实际：%s", d.Reason)
	}
}

// TestEgressGateBlocksAndRecords pins the whole blocked-attempt contract:
// 拦下来 + 留下可复核的记录（目的地、任务、判定、原因）。
func TestEgressGateBlocksAndRecords(t *testing.T) {
	dir := t.TempDir()
	tr, err := Open(dir, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()
	scope := &egressScopeStub{hosts: map[int64][]string{5: {"a.example.com"}}}
	tr.SetEgressScope(scope)

	req := egressRequest(t, 5, "http://b.example.com/x")
	rec := httptest.NewRecorder()
	ok, err := tr.egressGate(rec, req)
	if ok || err == nil {
		t.Fatal("范围外目的地必须被闸门拦下")
	}
	if rec.Code != http.StatusForbidden {
		t.Fatalf("被拦下的请求应回 403，实际 %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "不在本任务范围内") {
		t.Fatalf("拒绝文案必须写明原因，实际：%s", rec.Body.String())
	}

	// 网络层留痕：这条被拦下的出网必须能在流量索引里查到。
	var host, decision string
	var taskID int64
	var status int
	err = tr.DB().QueryRow(`SELECT host,status,egress_task_id,egress_decision FROM exchanges LIMIT 1`).
		Scan(&host, &status, &taskID, &decision)
	if err != nil {
		t.Fatalf("被拦下的出网必须留下记录: %v", err)
	}
	if host != "b.example.com" || status != http.StatusForbidden {
		t.Fatalf("记录的目的地/状态不对: host=%q status=%d", host, status)
	}
	if taskID != 5 || decision != egressDecisionDeny {
		t.Fatalf("记录的任务/判定不对: task=%d decision=%q", taskID, decision)
	}
	// 判定也进平台的审计链。
	if len(scope.decisions) != 1 {
		t.Fatalf("应有 1 条审计判定，实际 %d", len(scope.decisions))
	}
	ev := scope.decisions[0]
	if ev.TaskID != 5 || ev.Host != "b.example.com" || ev.Allowed {
		t.Fatalf("审计判定内容不对: %+v", ev)
	}
}

// TestEgressGateAllowsInScopeAndRecordsAttribution pins that an in-scope
// request goes through and its capture carries the task attribution.
func TestEgressGateAllowsInScopeAndRecordsAttribution(t *testing.T) {
	dir := t.TempDir()
	tr, err := Open(dir, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()
	tr.SetEgressScope(&egressScopeStub{hosts: map[int64][]string{5: {"a.example.com"}}})

	ok, err := tr.egressGate(httptest.NewRecorder(), egressRequest(t, 5, "http://a.example.com/x"))
	if !ok || err != nil {
		t.Fatalf("范围内目的地必须放行：%v", err)
	}
}

// TestRecordingProxyEnforcesTaskScope drives a real recording proxy end to end:
// 范围内的目标要能通，范围外的目标要被拦下并留下记录。
func TestRecordingProxyEnforcesTaskScope(t *testing.T) {
	t.Setenv("ARTEX_TRAFFIC_EGRESS_ALLOW_LOOPBACK", "1")
	dir := t.TempDir()
	tr, err := Open(dir, proxyTestAddr(t))
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()
	tr.SetEgressScope(&egressScopeStub{hosts: map[int64][]string{5: {"127.0.0.1"}}})
	go func() { _ = tr.Start() }()
	waitProxyReady(t, tr.ProxyAddr())

	proxyURL, err := url.Parse(tr.ProxyAddr())
	if err != nil {
		t.Fatal(err)
	}
	// 令牌走代理 URL 的 userinfo：各 HTTP 客户端都会把它变成 Proxy-Authorization。
	proxyURL.User = url.User(SignEgressTaskToken(5))
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}

	// 范围内：请求必须真的到达目标。
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("in-scope-ok"))
	}))
	defer target.Close()
	resp, err := client.Get(target.URL + "/ok")
	if err != nil {
		t.Fatalf("范围内目标必须能通: %v", err)
	}
	body, _ := readAll(resp)
	resp.Body.Close()
	if !strings.Contains(body, "in-scope-ok") {
		t.Fatalf("范围内目标的响应不对: %q", body)
	}

	// 范围外：目标根本不需要存在 —— 判定发生在拨号之前。
	resp, err = client.Get("http://out-of-scope.invalid/x")
	if err != nil {
		t.Fatalf("被拦下的请求应回一条明确的拒绝响应，而不是连接失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("被拦下的请求应回 403，实际 %d", resp.StatusCode)
	}
	body, _ = readAll(resp)
	if !strings.Contains(body, "不在本任务范围内") {
		t.Fatalf("拒绝文案必须写明原因，实际：%q", body)
	}

	// 留痕：被拦下的那次出网在流量索引里查得到，且带任务与判定。
	var taskID int64
	var decision, host string
	err = tr.DB().QueryRow(`SELECT host,egress_task_id,egress_decision FROM exchanges WHERE egress_decision=?`, egressDecisionDeny).
		Scan(&host, &taskID, &decision)
	if err != nil {
		t.Fatalf("被拦下的出网必须留痕: %v", err)
	}
	if host != "out-of-scope.invalid" || taskID != 5 {
		t.Fatalf("留痕内容不对: host=%q task=%d", host, taskID)
	}
}

// proxyTestAddr 找一个空闲的本机端口给录制代理用。
func proxyTestAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

// waitProxyReady 等代理端口可连。
func waitProxyReady(t *testing.T, proxyAddr string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", strings.TrimPrefix(proxyAddr, "http://"), 200*time.Millisecond)
		if err == nil {
			c.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("录制代理未就绪: %s", proxyAddr)
}

// readAll 读一个小响应体。
func readAll(resp *http.Response) (string, error) {
	var b strings.Builder
	buf := make([]byte, 512)
	for {
		n, err := resp.Body.Read(buf)
		b.Write(buf[:n])
		if err != nil {
			return b.String(), nil
		}
	}
}

// TestEgressDecisionJSON pins that a decision carries enough to review it.
func TestEgressDecisionJSON(t *testing.T) {
	d := EgressDecision{TaskID: 5, Attributed: true, Host: "a.example.com", URL: "http://a.example.com/x", Method: "GET", Allowed: false, Reason: "越界"}
	b, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"task_id", "attributed", "host", "url", "method", "allowed", "reason"} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("判定记录缺字段 %s: %s", want, b)
		}
	}
}

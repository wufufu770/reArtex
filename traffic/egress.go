package traffic

// 出网目的地治理（写侧）。
//
// 读侧有 ScopeGate（能读哪些 host 的流量），写侧是本文件：能【访问】哪些主机。
// 两者今天同源（任务的 scope 主机集），但语义相反 —— 读侧放宽不该连带放宽写侧，
// 所以各自留一个接口。
//
// 三段判据，任何一段拒绝即拒绝：
//  1. 目的地主机名能不能解析出来（解析不出就拒绝）；
//  2. 目的地在不在本次请求所属任务的范围内（不在就拒绝）；
//  3. 目的地主机名本身是否落在 netguard 的段规则内（IP 字面量 / localhost）。
//
// 第 2 段的范围来自平台唯一的范围判定入口（db.DB.TrafficScopeHosts →
// AssetStore.ScopeFragmentFor），不在这里另写一套 scope 判定，也不做全局资产
// 查询 —— 全局查询会把范围外资产的存在性漏出去，那正是读侧范围隔离要防的事。

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/netguard"
	mproxy "github.com/lqqyt2423/go-mitmproxy/proxy"
)

// EgressPolicy 是录制代理的出网策略：与 reArtex 自定义工具同一套 netguard 策略
// 类型、同一套判据，只是逃生口独立。
//
// 为什么逃生口独立：录制代理跑在平台进程里，agent 侧的逃生口是给 agent 的子进程
// 用的。两者共用一个名字，任何一处为图方便把它打开，录制代理的目的地守卫就会跟
// 着一起消失 —— 那正是「虚假安全感」。
var EgressPolicy = netguard.Policy{
	Name:              "录制代理出网",
	AllowPrivate:      true,
	EscapeEnv:         "ARTEX_TRAFFIC_EGRESS_ALLOW_LOCAL",
	LoopbackEscapeEnv: "ARTEX_TRAFFIC_EGRESS_ALLOW_LOOPBACK",
}

// EgressDecision 是一次出网判定的结论。
type EgressDecision struct {
	TaskID     int64  `json:"task_id"`
	Attributed bool   `json:"attributed"`
	Host       string `json:"host"`
	URL        string `json:"url"`
	Method     string `json:"method"`
	Allowed    bool   `json:"allowed"`
	Reason     string `json:"reason"`
}

// EgressScope 判定「任务可以出网访问哪些目的地主机」，并把被拦下的判定记进审计链。
type EgressScope interface {
	// EgressHosts 返回任务 taskID 的目的地主机允许集（域名或 IP，不含端口）。
	// 返回 error 表示判定不可用 —— 调用方必须拒绝（fail-closed）。
	EgressHosts(ctx context.Context, taskID int64) ([]string, error)
	// RecordEgressDecision 把一次被拦下的出网判定写进平台的审计链。
	RecordEgressDecision(ctx context.Context, d EgressDecision) error
}

// DBEgressScope 是平台接线用的实现：允许集来自平台唯一的范围判定入口
// （db.DB.TrafficScopeHosts），判定留痕走平台的审计链（db.DB.AppendAuditEvent）。
type DBEgressScope struct{ PG *db.DB }

// EgressHosts implements EgressScope.
func (g DBEgressScope) EgressHosts(ctx context.Context, taskID int64) ([]string, error) {
	if g.PG == nil {
		return nil, errors.New("出网范围判定未接线")
	}
	return g.PG.TrafficScopeHosts(taskID)
}

// RecordEgressDecision implements EgressScope.
func (g DBEgressScope) RecordEgressDecision(ctx context.Context, d EgressDecision) error {
	if g.PG == nil {
		return errors.New("出网判定留痕未接线")
	}
	payload, err := json.Marshal(map[string]any{
		"task_id":    d.TaskID,
		"attributed": d.Attributed,
		"host":       d.Host,
		"url":        d.URL,
		"method":     d.Method,
		"allowed":    d.Allowed,
		"reason":     d.Reason,
	})
	if err != nil {
		payload = []byte("{}")
	}
	_, err = g.PG.AppendAuditEvent(db.AuditEvent{
		Actor:    "traffic-proxy",
		ToolName: "egress",
		Action:   "deny",
		Reason:   d.Reason,
		Payload:  payload,
	})
	return err
}

// egressScopeHolder 装出网范围判定。用指针容器是为了能原子替换。
type egressScopeHolder struct{ scope EgressScope }

// egressTaskKey 是请求上下文里携带任务身份的 key（仅本包内使用）。
type egressTaskKey struct{}

// withEgressTask 把任务号放进 ctx，供 AllowHostFunc 读取。
func withEgressTask(ctx context.Context, taskID int64) context.Context {
	return context.WithValue(ctx, egressTaskKey{}, taskID)
}

// egressTaskFrom 从 ctx 取任务号（0 = 无身份）。
func egressTaskFrom(ctx context.Context) int64 {
	id, _ := ctx.Value(egressTaskKey{}).(int64)
	return id
}

// egressHostsCacheTTL 是任务允许集的缓存时长。
//
// 为什么要缓存：允许集来自范围查询（资产 × task_scope 的连接），每个请求打一次
// 库会把代理拖垮。任务范围本身是低频变化的（由 agent 显式扩权），10 秒的滞后
// 可以接受；判定失败仍然是 fail-closed 的。
const egressHostsCacheTTL = 10 * time.Second

// egressHostsEntry 是一条缓存的允许集。
type egressHostsEntry struct {
	hosts    []string
	expireAt time.Time
}

// egressHostsCache 是「任务 → 目的地允许集」的短期缓存。
type egressHostsCache struct {
	mu      sync.Mutex
	entries map[int64]egressHostsEntry
}

func (c *egressHostsCache) get(taskID int64) ([]string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[taskID]
	if !ok || time.Now().After(e.expireAt) {
		return nil, false
	}
	return e.hosts, true
}

func (c *egressHostsCache) put(taskID int64, hosts []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = map[int64]egressHostsEntry{}
	}
	c.entries[taskID] = egressHostsEntry{hosts: hosts, expireAt: time.Now().Add(egressHostsCacheTTL)}
}

// egressSecret 是签发出网身份令牌的进程内密钥。
//
// 为什么用签名令牌而不是把任务号直接写进代理 URL：直接写任务号的话，agent 可以
// 随便声称自己是别的任务，从而拿到那个任务的范围去探测 —— 那是把「读侧范围
// 隔离」从出网侧漏个底。签名令牌让 agent 只能声称自己。
var egressSecret = newEgressSecret()

func newEgressSecret() []byte {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		// 取不到系统随机数就退化成时间+pid 派生：强度不足，但至少不是常量明文。
		h := sha256.New()
		h.Write([]byte(strconv.FormatInt(time.Now().UnixNano(), 10)))
		h.Write([]byte(strconv.Itoa(os.Getpid())))
		copy(b, h.Sum(nil))
	}
	return b
}

// SignEgressTaskToken 为任务签发出网身份令牌。
//
// 格式：t<taskID>.<base64url(HMAC-SHA256(密钥, "t<taskID>"))>。放进代理 URL 的
// userinfo 里，curl / go / python 都会把它变成 Proxy-Authorization 发给代理。
// taskID<=0 不签发 —— 没有任务就没有身份。
func SignEgressTaskToken(taskID int64) string {
	if taskID <= 0 {
		return ""
	}
	msg := "t" + strconv.FormatInt(taskID, 10)
	mac := hmac.New(sha256.New, egressSecret)
	mac.Write([]byte(msg))
	return msg + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// parseEgressTaskToken 校验令牌并解出任务号。校验不过一律当「没有身份」。
func parseEgressTaskToken(token string) (int64, bool) {
	msg, sig, ok := strings.Cut(strings.TrimSpace(token), ".")
	if !ok || !strings.HasPrefix(msg, "t") {
		return 0, false
	}
	taskID, err := strconv.ParseInt(msg[1:], 10, 64)
	if err != nil || taskID <= 0 {
		return 0, false
	}
	mac := hmac.New(sha256.New, egressSecret)
	mac.Write([]byte(msg))
	want := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(sig), []byte(want)) {
		return 0, false
	}
	return taskID, true
}

// egressTaskTokenHeader 是客户端可以直接带的任务身份头（给能设置请求头的客户端）。
const egressTaskTokenHeader = "X-Artex-Task-Token"

// egressTaskOf 从请求里解出任务身份。
//
// 两个来源：显式的 X-Artex-Task-Token 头，以及代理 URL 的 userinfo（各 HTTP
// 客户端都会把它变成 Proxy-Authorization: Basic base64("令牌:")）。两者都要
// 验签：认得出来不等于有资格声称。
func egressTaskOf(req *http.Request) (int64, bool) {
	if req == nil {
		return 0, false
	}
	if h := strings.TrimSpace(req.Header.Get(egressTaskTokenHeader)); h != "" {
		return parseEgressTaskToken(h)
	}
	return parseProxyAuthTask(req.Header.Get("Proxy-Authorization"))
}

// parseProxyAuthTask 从 Proxy-Authorization 头里取任务令牌。
func parseProxyAuthTask(header string) (int64, bool) {
	const prefix = "Basic "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return 0, false
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(header[len(prefix):]))
	if err != nil {
		return 0, false
	}
	// Basic 认证的明文形如 "user:password"，令牌放在 user 位。
	user, _, _ := strings.Cut(string(raw), ":")
	return parseEgressTaskToken(user)
}

// stripEgressIdentity 把身份头从请求里摘掉。
//
// 它是能力凭据（拿到就能声称那个任务的身份），不能跟着请求转给目标，也不该
// 落进流量抓取里。
func stripEgressIdentity(req *http.Request) {
	if req == nil {
		return
	}
	req.Header.Del(egressTaskTokenHeader)
	req.Header.Del("Proxy-Authorization")
}

// egressDestinationHost 取一次出网请求的目的地主机（不含端口）。
//
// CONNECT 请求的目标在 req.URL.Host（authority 形式，Go 解析 CONNECT 时会把
// "http://host:443" 的 scheme 摘掉、host 留着），普通代理请求在 req.URL.Host ——
// 两者都要覆盖：只覆盖一半等于给 HTTPS 留一道门。
func egressDestinationHost(req *http.Request) string {
	if req == nil {
		return ""
	}
	if req.URL != nil {
		if h := req.URL.Hostname(); h != "" {
			return strings.ToLower(h)
		}
	}
	if h, _, err := net.SplitHostPort(req.Host); err == nil {
		return strings.ToLower(h)
	}
	return strings.ToLower(strings.TrimSpace(req.Host))
}

// SetEgressScope 注入出网目的地判定。
//
// 未注入时的语义：带任务身份的请求一律拒绝（判不出范围就没有授权边界），
// 不带身份的请求按「未归属」处理（放行但明确标记，见 egressDecide）。这是
// fail-closed：少放行是保守，多放行是越权。
func (t *Traffic) SetEgressScope(scope EgressScope) {
	t.egress.Store(&egressScopeHolder{scope: scope})
}

// egressScope 取出当前注入的范围判定（nil = 未接线）。
func (t *Traffic) egressScope() EgressScope {
	holder := t.egress.Load()
	if holder == nil {
		return nil
	}
	return holder.scope
}

// ---------- T6（方案 v10 §6 三期）：CONNECT 端口白名单 ----------

// egressPortsHolder 装追加端口来源。指针容器为了能原子替换。
type egressPortsHolder struct{ fn func() []int }

// SetEgressPortsSource 注入 CONNECT 端口白名单的追加端口来源（宿主接线：读
// settings egress_allowed_ports 的带缓存读取器）。nil/未注入 = 只默认白名单
// （443）。判定本体在 netguard.CheckConnectPort。
func (t *Traffic) SetEgressPortsSource(fn func() []int) {
	t.egressPorts.Store(&egressPortsHolder{fn: fn})
}

// egressPortsAllowed 取当前追加端口表（来源未注入 → nil）。
func (t *Traffic) egressPortsAllowed() []int {
	holder := t.egressPorts.Load()
	if holder == nil || holder.fn == nil {
		return nil
	}
	return holder.fn()
}

// connectPortGate 判定一次 CONNECT 隧道的目标端口（T6 ②）。
//
// 返回 (reason, allowed)：allowed=false 时 reason 是拒绝原因；环回豁免时
// allowed=true 且 reason 非空（调用方把它记进登记行）。端口取 CONNECT
// authority 里的显式端口，缺省按 scheme 补（https=443 / 其余=80）。
func (t *Traffic) connectPortGate(req *http.Request) (string, bool) {
	host := egressDestinationHost(req)
	port := 0
	if req.URL != nil {
		port, _ = strconv.Atoi(req.URL.Port())
	}
	if port == 0 {
		if req.URL != nil && req.URL.Scheme == "https" {
			port = 443
		} else {
			port = 80
		}
	}
	allowed, loopback, reason := netguard.CheckConnectPort(host, port, t.egressPortsAllowed())
	if allowed && loopback {
		// 环回豁免记登记行：放行，但这条豁免要可观测（排查「谁在打本机端口」）。
		log.Printf("[traffic] CONNECT 环回豁免登记：%s:%d（端口白名单豁免，放行）", host, port)
		return reason, true
	}
	return reason, allowed
}

// egressPolicy 返回本实例的出网策略：同一套 netguard 策略类型，允许集钩子接到
// 本实例的范围判定上。
func (t *Traffic) egressPolicy() netguard.Policy {
	p := EgressPolicy
	p.AllowHostFunc = t.egressHostAllowed
	return p
}

// egressHostAllowed 是允许集钩子：本次请求能不能去这个主机。
//
// ctx 里带着任务号（见 withEgressTask）。没有任务身份时这里不拒绝 —— 「未归属」
// 的处置在 egressDecide 里统一做（放行但明确标记），不在这里偷偷变成放行。
func (t *Traffic) egressHostAllowed(ctx context.Context, host string) error {
	taskID := egressTaskFrom(ctx)
	if taskID <= 0 {
		return nil
	}
	scope := t.egressScope()
	if scope == nil {
		return errors.New("出网范围判定未接线：无法校验本次出网是否在任务范围内")
	}
	hosts, err := t.egressHosts(ctx, scope, taskID)
	if err != nil {
		return fmt.Errorf("出网范围判定失败: %w", err)
	}
	if !hostAllowed(hosts, host) {
		return fmt.Errorf("目标 %q 不在本任务范围内", host)
	}
	return nil
}

// egressHosts 取任务的目的地允许集（带短期缓存）。
func (t *Traffic) egressHosts(ctx context.Context, scope EgressScope, taskID int64) ([]string, error) {
	if hosts, ok := t.egressCache.get(taskID); ok {
		return hosts, nil
	}
	hosts, err := scope.EgressHosts(ctx, taskID)
	if err != nil {
		return nil, err
	}
	t.egressCache.put(taskID, hosts)
	return hosts, nil
}

// egressDecide 判定一次出网请求：能不能去、属于谁、为什么。
//
// 「未归属」（请求没带任务身份）的处置：放行，但明确标记为未经范围校验。理由：
// 录制代理是平台级的，浏览器 MCP、web_fetch、平台的资产补全探测都走它，这些
// 请求没有任务身份；把它们一并拦掉等于把平台自己关掉，而不是治理它。代价是
// 明确的：未归属的出网不受任务范围校验 —— 这条限制写在平台的出网治理状态里
// （未覆盖通道清单），不是藏起来的。
func (t *Traffic) egressDecide(ctx context.Context, req *http.Request, taskID int64, attributed bool) EgressDecision {
	d := EgressDecision{
		TaskID:     taskID,
		Attributed: attributed,
		Host:       egressDestinationHost(req),
		Method:     req.Method,
	}
	if req.URL != nil {
		d.URL = req.URL.String()
	}
	if d.Host == "" {
		d.Reason = "判不出目的地主机，拒绝出网"
		return d
	}
	if !attributed {
		d.Allowed = true
		d.Reason = "未携带任务身份：本次出网未经任务范围校验"
		return d
	}
	if err := t.egressPolicy().CheckHost(withEgressTask(ctx, taskID), d.Host); err != nil {
		d.Reason = err.Error()
		return d
	}
	d.Allowed = true
	d.Reason = "在任务范围内"
	return d
}

// egressGate 是代理的准入闸：目的地不在任务范围内就不许走。
//
// 用 go-mitmproxy 的 authProxy 钩子（请求入口，早于 CONNECT / 普通请求分流），
// 因为它是唯一能在「隧道建立之前」拦住请求的位置：CONNECT 一旦建立隧道，内层
// 请求就已经出网了，事后判范围就晚了。
func (t *Traffic) egressGate(res http.ResponseWriter, req *http.Request) (bool, error) {
	// 非代理请求（直接访问代理自身地址）不归这里判，交给代理自己回 400。
	if req.Method != http.MethodConnect && (req.URL == nil || req.URL.Host == "") {
		return true, nil
	}
	// T6（方案 v10 §6 三期）②：CONNECT 端口白名单——平台级出网卫生规则，先于
	// 任务范围判定（它不依赖任务身份，对未归属请求同样生效）。非白名单拒绝并
	// 留痕；环回豁免但登记行照记（见 connectPortGate）。
	if req.Method == http.MethodConnect {
		if reason, allowed := t.connectPortGate(req); !allowed {
			taskID, attributed := egressTaskOf(req)
			d := EgressDecision{TaskID: taskID, Attributed: attributed,
				Host: egressDestinationHost(req), Method: req.Method,
				Allowed: false, Reason: reason}
			if req.URL != nil {
				d.URL = req.URL.String()
			}
			t.recordEgressDenial(req, d)
			if scope := t.egressScope(); scope != nil {
				if err := scope.RecordEgressDecision(req.Context(), d); err != nil {
					log.Printf("[traffic] CONNECT 端口拒绝入审计链失败: %v", err)
				}
			}
			writeEgressDenied(res, d)
			return false, errors.New(d.Reason)
		}
	}
	taskID, attributed := egressTaskOf(req)
	d := t.egressDecide(req.Context(), req, taskID, attributed)
	if d.Allowed {
		return true, nil
	}
	t.recordEgressDenial(req, d)
	if scope := t.egressScope(); scope != nil {
		if err := scope.RecordEgressDecision(req.Context(), d); err != nil {
			log.Printf("[traffic] 出网判定入审计链失败: %v", err)
		}
	}
	writeEgressDenied(res, d)
	return false, errors.New(d.Reason)
}

// recordEgressDenial 把一次被拦下的出网记成一条流量记录。
//
// 走的是流量录制本身（不是另开一条日志通道）：被拦下的探测也发生过一次
// 「请求-响应」，它的目的地、任务、判定、原因就是要留痕的东西。
func (t *Traffic) recordEgressDenial(req *http.Request, d EgressDecision) {
	header := req.Header.Clone()
	stripEgressIdentityHeader(header)
	f := &mproxy.Flow{
		Request: &mproxy.Request{
			Method: req.Method,
			URL:    req.URL,
			Proto:  req.Proto,
			Header: header,
		},
		Response: &mproxy.Response{
			StatusCode: http.StatusForbidden,
			Header:     http.Header{"Content-Type": []string{"text/plain; charset=utf-8"}},
			Body:       []byte(d.Reason),
		},
	}
	t.recordExchange(f, exchangeMeta{TaskID: d.TaskID, Decision: egressDecisionDeny})
}

// writeEgressDenied 回一条明确的拒绝响应。
//
// 为什么自己写而不是让代理回 407：407 的语义是「代理要认证」，agent 收到会
// 以为该补凭据再试；这里要表达的是「这个目的地不在你的任务范围内」，那是换
// 凭据也解决不了的事。
//
// 副作用：go-mitmproxy 在我们拒绝之后还会再写一次 407 兜底，Go 会打一行
// "superfluous response.WriteHeader" 日志 —— 那是它自己的兜底，不影响我们回的 403。
func writeEgressDenied(res http.ResponseWriter, d EgressDecision) {
	res.Header().Set("Content-Type", "text/plain; charset=utf-8")
	res.WriteHeader(http.StatusForbidden)
	fmt.Fprintf(res, "出网被任务范围拦截：%s\n目标：%s\n任务：%s\n", d.Reason, d.Host, egressTaskLabel(d.TaskID))
}

// egressTaskLabel 把任务号写成给人看的标签。
func egressTaskLabel(taskID int64) string {
	if taskID <= 0 {
		return "未归属"
	}
	return "task " + strconv.FormatInt(taskID, 10)
}

// stripEgressIdentityHeader 从一份头部副本里摘掉身份头。
func stripEgressIdentityHeader(h http.Header) {
	h.Del(egressTaskTokenHeader)
	h.Del("Proxy-Authorization")
}

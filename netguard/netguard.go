// Package netguard 提供拨号期的出网目的地守卫。
//
// 判据落在连接建立（http.Transport 的 Control 钩子）而非 URL 字符串校验：只有拨号
// 时刻才是最终生效点，同时覆盖 DNS 重绑定（校验时解析到公网、连接时解析到内网）与
// 重定向两种绕过。实现自 notify/http.go 的既有守卫抽取而来，行为已被 notify 的
// 用例验证过，不是新写的判定。
package netguard

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Policy 描述一路出网流量的目的地约束。
type Policy struct {
	// Name 只用于报错文案。
	Name string
	// AllowPrivate 放行 RFC1918 私网。对渗透 agent 应为 true —— 内网通常就是目标，
	// 照搬通知侧的收紧会把内网测评直接废掉。
	AllowPrivate bool
	// EscapeEnv 放开全部受限段（含链路本地），是「这条出网全不设防」的开关。
	EscapeEnv string
	// LoopbackEscapeEnv 只放开环回，链路本地（云元数据 169.254.169.254）仍然拒绝。
	// 单独拆出来是因为这两段对渗透的含义相反：环回常是靶场（本机的 CTF lab、
	// 本机起的目标），而元数据是实例凭据，开了就等于送出去。两者不该共用一个开关。
	LoopbackEscapeEnv string
	// AllowHostFunc 按「一次出网请求」判定目的地主机是否放行。
	//
	// 为什么需要它：Policy 原本只表达「哪些 IP 段能去」，而任务的授权范围是
	// 「哪些主机名能去」—— 后者必须按请求解析（同一时刻不同任务的允许集不同），
	// 塞不进一个静态的 IP 段谓词。加这个钩子而不是新造一套策略，是为了让
	// 「同一个 Policy 类型」既能管段、也能管主机：出网只有一套策略，配置不同而已。
	//
	// ctx 是该次请求的上下文（携带任务身份等）；host 是不含端口的主机名。
	// 返回非 nil 即拒绝，拒绝原因原样带出（调用方据此留痕）。
	// nil = 不做主机名级判定（只走 IP 段规则），与既有行为一致。
	AllowHostFunc func(ctx context.Context, host string) error
}

func envTruthy(name string) bool {
	v := strings.TrimSpace(os.Getenv(name))
	return v == "1" || strings.EqualFold(v, "true")
}

func (p Policy) escaped() bool { return envTruthy(p.EscapeEnv) }

// loopbackEscaped 只影响环回段；未配置该变量时行为等同于不放行环回。
func (p Policy) loopbackEscaped() bool {
	return p.LoopbackEscapeEnv != "" && envTruthy(p.LoopbackEscapeEnv)
}

// NormalizeIP 把 IPv4-mapped IPv6 归一回 IPv4（::ffff:1.2.3.4 → 1.2.3.4）。
// 所有「拿 IP 做比对」的入口先过这里——不归一的话，同一个人用两种写法就能让
// 环回/私网判定与白名单比对各错一次。
func NormalizeIP(ip net.IP) net.IP {
	if ip == nil {
		return nil
	}
	if v4 := ip.To4(); v4 != nil {
		return v4
	}
	return ip
}

// ---------- T6（方案 v10 §6 三期）：黑面单一事实源 ----------

// ForbiddenIP 是「黑面」的单一事实源：所有出网策略恒拒的地址段——链路本地
// （含云元数据端点 169.254.169.254）、未指定、组播、接口本地组播。返回
// (拒绝原因, 是否恒拒)。环回与 RFC1918 **不在**黑面里：它们的取舍按策略走
// （环回常是靶场、RFC1918 常是内网测评目标），由各 Policy 自己决定。
//
// 之前 notify 包自己抄了一份同样的段表（isBlockedDialIP）——两份清单必然漂移，
// 现在统一引用这里（traffic/pathguard 与本源的关系见各自包内说明）。
func ForbiddenIP(ip net.IP) (string, bool) {
	if ip == nil {
		return "目标地址无法解析", true
	}
	ip = NormalizeIP(ip) // IPv4-mapped IPv6 归一后再比对
	if ip.IsLinkLocalUnicast() {
		return "链路本地地址（含云元数据端点 169.254.169.254，可读实例凭据）", true
	}
	if ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() {
		return "链路/接口本地组播地址", true
	}
	if ip.IsUnspecified() {
		return "未指定地址", true
	}
	if ip.IsMulticast() {
		return "组播地址", true
	}
	return "", false
}

// IsLoopbackHost 报告主机名是否环回：localhost / *.localhost 名字，或解析为
// 环回段 IP 的字面量（IPv4-mapped 先归一再比对）。CONNECT 端口白名单用它做
// 环回豁免判定（见 CheckConnectPort）。
func IsLoopbackHost(host string) bool {
	h := strings.ToLower(strings.TrimSpace(strings.Trim(host, "[]")))
	if h == "" {
		return false
	}
	if h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return true
	}
	if ip := net.ParseIP(h); ip != nil {
		return NormalizeIP(ip).IsLoopback()
	}
	return false
}

// ---------- T6（方案 v10 §6 三期）：CONNECT 端口白名单 ----------

// EgressDefaultPort 是 CONNECT 端口白名单的默认值：普通 agent 出网实质上只有
// HTTPS 隧道一种正当形态，443 之外默认不开。追加端口走 settings 的
// egress_allowed_ports（由宿主解析后传入，本包不读存储——照本包「策略是数据、
// 判定是代码」的既有分层）。
const EgressDefaultPort = 443

// ParseEgressAllowedPorts 解析 egress_allowed_ports（逗号/空白分隔的端口号，
// 1-65535 之外的段丢弃）。空串/全非法 → nil。
func ParseEgressAllowedPorts(raw string) []int {
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n' || r == ';'
	})
	out := make([]int, 0, len(fields))
	for _, f := range fields {
		n, err := strconv.Atoi(strings.TrimSpace(f))
		if err != nil || n <= 0 || n > 65535 {
			continue
		}
		out = append(out, n)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// CheckConnectPort 判定一次 CONNECT 隧道的目标端口（T6）。
//
// 规则：环回目标豁免（本机靶场是合法形态；loopback=true 由调用方自行登记）；
// 443 恒放行；白名单 extra（settings egress_allowed_ports 追加）放行；其余拒绝。
// 返回 (allowed, loopback, reason)。
func CheckConnectPort(host string, port int, extra []int) (allowed bool, loopback bool, reason string) {
	if IsLoopbackHost(host) {
		return true, true, "环回目标豁免端口白名单（已登记）"
	}
	if port == EgressDefaultPort {
		return true, false, ""
	}
	for _, p := range extra {
		if p == port {
			return true, false, ""
		}
	}
	return false, false, "CONNECT 目标端口不在白名单（默认 443，追加见 egress_allowed_ports）"
}

// blockedIP 报告该 IP 是否属于本策略拒绝的地址段。
//
// 恒拒段（黑面）引用 ForbiddenIP 单一事实源；环回可由 LoopbackEscapeEnv 单独
// 放开；RFC1918 是否拒绝取决于 AllowPrivate。
func (p Policy) blockedIP(ip net.IP) bool {
	if _, forbidden := ForbiddenIP(ip); forbidden {
		return true
	}
	ip = NormalizeIP(ip)
	if ip.IsLoopback() {
		return !p.loopbackEscaped()
	}
	return !p.AllowPrivate && ip.IsPrivate()
}

// hint 指向最贴近的开关：环回有专用开关，提示它而不是那个「全不设防」的总开关，
// 避免有人为了打本机靶场顺手把链路本地（云元数据）也一起放开。
func (p Policy) hint(ip net.IP) string {
	if ip != nil {
		if v4 := ip.To4(); v4 != nil {
			ip = v4
		}
		if ip.IsLoopback() && p.LoopbackEscapeEnv != "" {
			return "如需访问本机靶场/服务，设置 " + p.LoopbackEscapeEnv + "=1；该开关不放开链路本地"
		}
	}
	return "如确需访问，设置 " + p.EscapeEnv + "=1"
}

func (p Policy) dialControl(_, address string, _ syscall.RawConn) error {
	if p.escaped() {
		return nil
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("[%s] 无法解析目标地址 %q", p.Name, host)
	}
	if p.blockedIP(ip) {
		return fmt.Errorf("[%s] 拒绝连接 %s（%s）", p.Name, ip, p.hint(ip))
	}
	return nil
}

// guardedDialer 是带拨号守卫的底层 Dialer（Control 看到的就是最终建连地址，
// 是终点校验；ResolveOnce 的解析期筛选拦不住的，这里再拦一次）。
func (p Policy) guardedDialer() *net.Dialer {
	d := &net.Dialer{Timeout: 10 * time.Second}
	d.Control = p.dialControl
	return d
}

// ResolveOnceDialContext 是「解析一次拿 IP，建连用该 IP」的 DialContext 定制
// （T6 ①，http.Transport DialContext 定制形态）：
//   - 域名只走一次解析，然后从解析结果里挑第一个放行的 IP 直接连它——后续
//     重连不再重新解析（原来 net.Dialer 每次拨号各自解析，DNS 抖动下同一个
//     URL 的两次请求可能落到不同地址）；
//   - TLS 的 SNI 与证书校验保持原域名：http.Transport 的 ServerName 取自
//     请求 URL 的主机名，与拨号地址是 IP 无关；
//   - 解析结果里没有放行的 IP（黑面/环回/私网按策略）→ fail-closed 拒绝；
//   - 字面 IP 地址不走解析，按原地址建连（Control 仍生效）。
func (p Policy) ResolveOnceDialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	if p.escaped() {
		return p.guardedDialer().DialContext(ctx, network, addr)
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	if ip := net.ParseIP(host); ip != nil {
		return p.guardedDialer().DialContext(ctx, network, addr)
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("[%s] 解析 %q 失败: %w", p.Name, host, err)
	}
	for _, r := range ips {
		if p.blockedIP(r.IP) {
			continue
		}
		return p.guardedDialer().DialContext(ctx, network, net.JoinHostPort(r.IP.String(), port))
	}
	return nil, fmt.Errorf("[%s] %q 解析出的 %d 个地址全部被拒（%s）", p.Name, host, len(ips), p.hint(nil))
}

// Transport 在 base 上叠加拨号守卫，保留 base 的全部其余行为。
// T6 ①：DialContext 换成 ResolveOnce——解析一次拿 IP、按 IP 建连（SNI 保持
// 原域名），拨号 Control 守卫不变。
func (p Policy) Transport(base *http.Transport) *http.Transport {
	if base == nil {
		base = http.DefaultTransport.(*http.Transport)
	}
	clone := base.Clone()
	clone.DialContext = p.ResolveOnceDialContext
	return clone
}

// CheckURLHost 对 URL 主机做字面预检，强度低于 dialControl：只认写出来的 IP 与
// 主机名，域名解析结果与重定向都不在判据内。仅在目标不由本进程解析时使用（即走
// 代理的路径），此时拨号期守卫看不到真实目标。
func (p Policy) CheckURLHost(rawURL string) error {
	if p.escaped() {
		return nil
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("[%s] URL 解析失败: %w", p.Name, err)
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("[%s] URL 缺少主机名", p.Name)
	}
	return p.checkHostLiteral(host)
}

// checkHostLiteral 是主机名本身的段判定：IP 字面量走段规则，localhost 名字直接拒。
// CheckURLHost 与 CheckHost 共用它，两条路径的判据才不会漂移。
func (p Policy) checkHostLiteral(host string) error {
	if ip := net.ParseIP(host); ip != nil {
		if p.blockedIP(ip) {
			return fmt.Errorf("[%s] 拒绝访问受限地址 %s", p.Name, ip)
		}
		return nil
	}
	if strings.EqualFold(host, "localhost") || strings.HasSuffix(strings.ToLower(host), ".localhost") {
		return fmt.Errorf("[%s] 拒绝访问 %q（如确需访问本机服务，设置 %s=1）", p.Name, host, p.EscapeEnv)
	}
	return nil
}

// CheckHost 判定一次出网请求的目的地主机。
//
// 两段判据：先问 AllowHostFunc（按请求解析的允许集，例如任务 scope 派生的
// 主机集），再走字面段规则（IP 字面量 / localhost）。任一段拒绝即拒绝。
// 主机名缺失也拒绝 —— 判不出目的地就不放行。
func (p Policy) CheckHost(ctx context.Context, host string) error {
	if p.escaped() {
		return nil
	}
	host = strings.TrimSpace(host)
	if host == "" {
		return fmt.Errorf("[%s] 出网请求缺少目的地主机名", p.Name)
	}
	if p.AllowHostFunc != nil {
		if err := p.AllowHostFunc(ctx, host); err != nil {
			return err
		}
	}
	return p.checkHostLiteral(host)
}

// CheckRedirect 拒绝跨主机跳转：跨主机意味着凭据会被交给重定向目标。
func (p Policy) CheckRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 5 {
		return errors.New("重定向次数过多")
	}
	if len(via) > 0 && req.URL.Host != via[0].URL.Host {
		return fmt.Errorf("[%s] 拒绝跨主机重定向（%s → %s）", p.Name, via[0].URL.Host, req.URL.Host)
	}
	return nil
}

// AgentEgress 是 agent 侧出网策略。RFC1918 放行：内网通常就是渗透目标。
var AgentEgress = Policy{
	Name:              "agent 出网",
	AllowPrivate:      true,
	EscapeEnv:         "ARTEX_AGENT_EGRESS_ALLOW_LOCAL",
	LoopbackEscapeEnv: "ARTEX_AGENT_EGRESS_ALLOW_LOOPBACK",
}

// NotifyDelivery 是通知投递策略，语义与 agent 侧一致但逃生口独立。
var NotifyDelivery = Policy{
	Name:              "通知投递",
	AllowPrivate:      true,
	EscapeEnv:         "ARTEX_NOTIFY_ALLOW_LOCAL",
	LoopbackEscapeEnv: "ARTEX_NOTIFY_ALLOW_LOOPBACK",
}

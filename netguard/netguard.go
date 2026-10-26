// Package netguard 提供拨号期的出网目的地守卫。
//
// 判据落在连接建立（http.Transport 的 Control 钩子）而非 URL 字符串校验：只有拨号
// 时刻才是最终生效点，同时覆盖 DNS 重绑定（校验时解析到公网、连接时解析到内网）与
// 重定向两种绕过。实现自 notify/http.go 的既有守卫抽取而来，行为已被 notify 的
// 用例验证过，不是新写的判定。
package netguard

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
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

// blockedIP 报告该 IP 是否属于本策略拒绝的地址段。
//
// 始终拒绝：链路本地（含云元数据）、未指定、组播。环回可由 LoopbackEscapeEnv 单独
// 放开。RFC1918 是否拒绝取决于 AllowPrivate。
func (p Policy) blockedIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	// IPv4-mapped IPv6（::ffff:127.0.0.1）须还原成 IPv4 再判，否则绕过。
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	if ip.IsLoopback() {
		return !p.loopbackEscaped()
	}
	if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() {
		return true
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

// Transport 在 base 上叠加拨号守卫，保留 base 的全部其余行为。
func (p Policy) Transport(base *http.Transport) *http.Transport {
	if base == nil {
		base = http.DefaultTransport.(*http.Transport)
	}
	clone := base.Clone()
	d := &net.Dialer{Timeout: 10 * time.Second}
	d.Control = p.dialControl
	clone.DialContext = d.DialContext
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

package sodpolicy

// 范围豁免清单（scope-exemption list）—— 平台基础设施类主机的白名单。
//
// 背景（方案书 §9.1 第 9 条 / §6.5 第 1 条）：web_search 走自己的出口代理
// （socks5 / 全局代理），不是录制代理 ⇒ 请求既不装 netguard 的拨号守卫，也不带
// 任务身份 ⇒ 完全不受任务范围校验。要真正覆盖它，得先回答「哪些主机本来就不该
// 受任务范围管辖」—— 那就是这份清单。清单里的主机是**平台自己要用的目的地**
// （搜索服务商 / 模型 API 端点 / 镜像仓库），不是被测目标。
//
// 为什么是【代码常量】而不是 DB 行 / settings 项：
//   - DB 里的行可以被 DELETE，settings 可以被持有 JWT 的人热改 ⇒ 清单会退化成
//     「由被它约束的人来调边界」；
//   - 清单是安全边界的一部分（哪些目的地**不受**任务范围管辖），它必须和判定
//     代码一起改、一起审、一起发布。
// 与本包其余常量同一条理由（见包文档注释）。
//
// ⚠️ 为什么住在 internal/sodpolicy 而不是 agent 包：两端都要读它 ——
// traffic（范围判定的执行方，见 traffic/egress.go 的 egressHostAllowed）与
// agent（web_search 归属判定的调用方，见 agent/egress_scope.go）。而
// agent → traffic 已经存在（agent/worker.go 用 traffic.SignEgressTaskToken）
// ⇒ 清单若住在 agent，traffic → agent 就成环，编译不过。本包是零内部依赖的
// 叶子包 ⇒ 两边都能 import。
//
// 判定口径（精确匹配，不含子域）：
//
//	清单里的每一个条目都是代码里写死的**确切目的地**主机名。
//	子域不放行 —— 「清单内的主机」就是清单里那几行字。某服务商换到兄弟主机名
//	时，这里必须是一次有意的代码改动（而不是运行时悄悄放宽）。
//	放宽失败的方向是安全的：不在清单 ⇒ 仍然放行，但按「未归属出网」留痕。

import (
	"net"
	"sort"
	"strings"
)

// 范围豁免的机器可读原因码（不含数字 —— 数字会随阈值调整而漂移）。
const (
	// ReasonEgressScopeExempt 目的地在范围豁免清单内 ⇒ 平台基础设施，
	// 不受任务范围校验管辖。
	ReasonEgressScopeExempt = "egress_scope_exempt"
	// ReasonEgressScopeUnattributed 目的地不在豁免清单内 ⇒ 放行，但按
	// 「未归属出网」留痕（web_search 的目的地不是攻击面的一部分，拒掉会让
	// 工具直接不可用）。
	ReasonEgressScopeUnattributed = "egress_scope_unattributed"
	// ReasonEgressScopeUnresolved 目的地解析不出来 ⇒ fail-closed（不放行）。
	ReasonEgressScopeUnresolved = "egress_scope_unresolved"
)

// EgressScopeExemptHostList 是范围豁免清单的【唯一真相源】：编译期常量。
//
// 逐条主机及其依据（都来自本仓库真实会访问的目的地，不是凭空列的）：
//
//	html.duckduckgo.com
//	    web_search 的 ddgs 后端终点（third_party/norma/tool/websearch.go 的
//	    ddgsEndpoint = "https://html.duckduckgo.com/html/"）。ddgs 是默认后端
//	    （server/manager.go 的 defaultWebSearchBackend = "ddgs"）。
//	api.search.brave.com
//	    web_search 的 brave-free 后端终点（同上的
//	    braveEndpoint = "https://api.search.brave.com/res/v1/web/search"）。
//	api.tavily.com
//	    web_search 的 tavily 后端终点（同上的
//	    tavilyEndpoint = "https://api.tavily.com/search"）。
//	api.deepseek.com
//	    web_search 的 deepseek 后端（借用当前 LLM 档案的 Anthropic 格式 base
//	    URL，server/manager.go 的 deepSeekWebSearchBackend）与 DeepSeek 模型
//	    API 同主机。
//	api.openai.com
//	    openai 格式模型 API 的默认 base（server/server_mgmt.go 的
//	    "https://api.openai.com/v1"）。
//	api.anthropic.com
//	    anthropic 格式模型 API 的默认 base（server/server_mgmt.go 的
//	    "https://api.anthropic.com"）。
//	ghcr.io
//	    平台自身镜像仓库（ghcr.io/wufufu770/reartex：docker-compose.yml、
//	    docker-compose.override.yml、Dockerfile.dev、.github/workflows/release.yml）。
//	docker.io
//	    基础镜像仓库（Dockerfile 的 python:3.12-slim-bookworm、
//	    docker-compose.yml 的 postgres:16-alpine）。
//
// ⚠️ 模型 API 的 base URL 是**用户可配**的（llm_profiles.base_url）⇒ 用户自建的
// 模型网关不在清单里 ⇒ 它的出网按「未归属出网」留痕，而不是被悄悄豁免。
// 这正是「清单是代码常量」要买的东西：用户不能通过改配置把自己加进豁免名单。
const EgressScopeExemptHostList = `
html.duckduckgo.com
api.search.brave.com
api.tavily.com
api.deepseek.com
api.openai.com
api.anthropic.com
ghcr.io
docker.io
`

// egressScopeExemptHosts 是 EgressScopeExemptHostList 的解析结果（包内私有）。
//
// 为什么解析成 map 而不是每次遍历字符串：判定在出网热路径上；map 查找是 O(1)。
var egressScopeExemptHosts = parseEgressScopeExemptHosts()

// parseEgressScopeExemptHosts 把常量清单解析成集合。
func parseEgressScopeExemptHosts() map[string]struct{} {
	out := map[string]struct{}{}
	for _, line := range strings.Split(EgressScopeExemptHostList, "\n") {
		host := strings.ToLower(strings.TrimSpace(line))
		if host == "" {
			continue
		}
		out[host] = struct{}{}
	}
	return out
}

// EgressScopeExemptHosts 返回豁免清单的**副本**（按字典序）。
//
// 为什么返回副本：返回一个包级切片，调用方一句 append 就能改掉安全边界 ⇒
// 那等于把「代码常量」降级成「进程内可变状态」。副本让内部状态不可达。
func EgressScopeExemptHosts() []string {
	out := make([]string, 0, len(egressScopeExemptHosts))
	for host := range egressScopeExemptHosts {
		out = append(out, host)
	}
	sort.Strings(out)
	return out
}

// EgressScopeVerdict 是范围豁免判定对某个出网目的地的结论。
//
// 为什么结论是一个值而不是一个 bool：调用方要写的不是「允许 / 不允许」，而是
// 「这次出网属于哪一类」—— 平台基础设施（exempt）、未归属出网（unattributed）、
// 还是判不出目的地（unresolved）。三者对留痕的要求完全不同（只有后两者要写
// 审计链），压成一个 bool 就只能靠调用方自己再判一遍 ⇒ 两处口径迟早漂移。
type EgressScopeVerdict struct {
	// Host 是归一化后的目的地主机名；空 = 判不出来（fail-closed）。
	Host string
	// Exempt 为真 = 目的地在范围豁免清单内（平台基础设施，不受任务范围管辖）。
	Exempt bool
	// Reason 是机器可读的原因码（ReasonEgressScope* 之一）。
	Reason string
}

// ClassifyEgressScopeHost 按范围豁免清单判定一个出网目的地。
//
// 三种结论（口径与文件头注释一致）：
//
//	清单内     ⇒ Exempt=true，Reason=ReasonEgressScopeExempt
//	清单外     ⇒ Exempt=false，Reason=ReasonEgressScopeUnattributed
//	判不出主机 ⇒ Exempt=false，Reason=ReasonEgressScopeUnresolved（fail-closed）
//
// 为什么「清单外」也是放行：豁免清单管的是「哪些目的地**不受**任务范围管辖」，
// 而 web_search 的目的地本来就不该被任务范围管辖（搜索服务商的域名不是任务的
// 攻击面）⇒ 拒掉只会让工具直接不可用。代价是明确的：这类出网按「未归属」留痕。
//
// 归一化口径见 normalizeEgressScopeHost（小写、去协议头、去用户信息、去路径、
// 去端口、去 IPv6 方括号、去尾点），随后**精确匹配**清单（子域不自动放行）。
func ClassifyEgressScopeHost(host string) EgressScopeVerdict {
	h := normalizeEgressScopeHost(host)
	if h == "" {
		return EgressScopeVerdict{Reason: ReasonEgressScopeUnresolved}
	}
	if _, ok := egressScopeExemptHosts[h]; ok {
		return EgressScopeVerdict{Host: h, Exempt: true, Reason: ReasonEgressScopeExempt}
	}
	return EgressScopeVerdict{Host: h, Reason: ReasonEgressScopeUnattributed}
}

// EgressScopeHostExempt 报告某个目的地主机是否在豁免清单内。
//
// 口径：先归一化，再**精确匹配**清单。子域不自动放行（理由见文件头注释）。
//
// 归一化为什么必要：调用方手上可能是 URL、可能是 "host:port"、可能是大写的
// 主机名 —— 不归一化就会把同一个目的地判成两个，而其中一个可能落在清单外。
func EgressScopeHostExempt(host string) bool {
	return ClassifyEgressScopeHost(host).Exempt
}

// normalizeEgressScopeHost 把一个目的地归一化成可比较的主机名。
func normalizeEgressScopeHost(raw string) string {
	host := strings.ToLower(strings.TrimSpace(raw))
	if host == "" {
		return ""
	}
	if i := strings.Index(host, "://"); i >= 0 {
		host = host[i+3:]
	}
	if i := strings.LastIndex(host, "@"); i >= 0 {
		host = host[i+1:]
	}
	if i := strings.IndexAny(host, "/?#"); i >= 0 {
		host = host[:i]
	}
	// 先拆端口再拆方括号：IPv6 的 [::1]:80 里方括号包的是地址段。
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.TrimPrefix(host, "[")
	host = strings.TrimSuffix(host, "]")
	return strings.TrimSuffix(host, ".")
}

package server

// 出网治理状态契约。
//
// 为什么要有这个契约：落地了「三件小事」（Bash 走带 netguard 的出口、MCP 客户端
// 走 netguard transport、录制代理按任务范围放行目的地）之后，出网【并没有】被
// 完全治理。Sir 的原话：「一个不完整的 egress gateway 比没有更危险，因为它制造
// 虚假安全感」—— 所以平台必须能说清楚「哪里还没管住」，而不是含糊地宣称已管控。
//
// 本文件只提供数据契约与路由。UI 渲染（web/）由另一条工作流负责：它读这个接口，
// 拿到 governed=false 与未覆盖通道清单，然后【必须】把清单显示出来。

import (
	"strings"
	"encoding/json"
	"net/http"
)

// EgressGovernanceState describes how completely egress is governed.
type EgressGovernanceState struct {
	// Governed is false as long as ANY channel is uncovered.
	//
	// 它由 UncoveredChannels 派生（见 newEgressGovernanceState / MarshalJSON）：
	// 清单非空就不可能是 true。这样 UI 不可能拿到一句「出网已管控」而清单里却
	// 列着未覆盖的通道。
	Governed bool `json:"governed"`
	// UncoveredChannels lists, in human-readable form, the channels that are
	// NOT covered — this list is the point of the contract.
	UncoveredChannels []string `json:"uncovered_channels"`
}

// egressUncoveredChannels 是当前【实际】未覆盖的出网通道清单。
//
// 这份清单是代码事实，不是配置项：每一条都对应一条代码路径上确实存在的缺口，
// 写在这里是为了让「已治理」这句话永远无法被说出口。
var egressUncoveredChannels = []string{
	// web_search 走的是独立的出口代理（socks5 / 全局代理），不是录制代理 ⇒ 请求
	// 既不装 netguard 的拨号守卫，也不带任务身份 ⇒ 不受任务范围校验。
	//
	// 为什么不能照 web_fetch 那样处理：web_fetch 的代理是 http/https 录制代理，
	// 可以把身份令牌塞进 URL 的 userinfo；而 web_search 的出口常是 socks5，
	// socks5 的 userinfo 是另一套认证（塞进去会把代理弄坏，见
	// TestProxyEnvKeepsSocks5ProxyIntact）。改走录制代理也不可行：搜索服务商的
	// 域名不在任务资产范围里，会被范围判定拒掉 ⇒ 工具直接不可用。要真正覆盖它，
	// 需要先引入「范围豁免清单」（平台基础设施类主机的白名单），那是独立的一项改造。
	"web_search：走独立出口代理（socks5/全局代理），不经录制代理也不带任务身份，不受任务范围校验（要覆盖需先引入范围豁免清单）",
	// web_fetch 现在带任务身份经录制代理出去（agent 包的 egressProxyURL），所以
	// 它本身已受范围校验。但 SDK 在代理连接层出错（连接被拒/重置/TLS 协议错）时会
	// 直连重试一次，那条路径不经代理 ⇒ 不经范围判定。
	//
	// 注意：范围拒绝本身不会触发它 —— 代理返回的是 HTTP 状态码，而 SDK 的
	// 「像代理故障」判定只匹配传输层错误（eof / connection reset / proxyconnect /
	// tls: 等），不匹配状态码。
	"web_fetch 的直连兜底：SDK 在代理连接层出错时会直连重试一次，该路径不经范围判定（范围拒绝本身不会触发它）",
	// MCP 服务端自己发出的请求（它去访问的目标）不经过本平台任何出网策略。
	"MCP 的外部通道：MCP 服务端自己发出的请求不由本平台的出网策略管辖（平台只管自己发往 MCP 的那一下）",
	// 宿主/容器进程自身的出网（平台服务、依赖拉取、镜像更新等）没有任务上下文。
	"宿主侧出网：容器与宿主进程自身发出的网络请求（平台服务、依赖拉取、镜像更新等）不在任务范围判定内",
	// 捕获关闭时 Bash 走的是全局代理，不是录制代理 —— 那条路径没有目的地判定。
	"全局出网代理路径：流量捕获关闭时 Bash 走全局代理，该路径没有任务目的地判定",
	// 没有任务身份的请求按「未经范围校验」放行（平台自身的流量就是这种）。
	"未归属的出网：请求不带任务身份时按「未经范围校验」放行并留痕（平台自身的流量没有任务身份）",
	// 20 条内置 deny 规则拦的是破坏性命令，与范围外出网无关；其中「数据外泄管道」
	// 规则出厂即 disabled（见 db/db.go 的 seedDefaultInterceptRulesV2），一直没在生效。
	"20 条内置 deny 规则只挡破坏性命令，不挡范围外出网；其中「数据外泄管道」规则出厂即 disabled，未在生效",
}

// egressConfigProbe 读运营方已配置的出口（B7：横幅随配置收敛）。
// 由 server 装配注入（读 settings）；nil = 视为全未配置（横幅照旧全列）。
var egressConfigProbe func() (webSearchProxy, globalProxy string, captureOn bool)

// CurrentEgressGovernanceState 返回当前的出网治理状态。
//
// B7：清单是【动态】的——运营方为某条通道配置了显式出口（web_search 的独立
// 代理）或该通道的前提已变化（捕获开启 ⇒ Bash 走录制代理带范围判定），对应
// 条目从横幅消失。剩余条目仍是代码事实，不会被配置抹掉。
func CurrentEgressGovernanceState() EgressGovernanceState {
	list := append([]string(nil), egressUncoveredChannels...)
	if egressConfigProbe != nil {
		wsProxy, _, captureOn := egressConfigProbe()
		out := list[:0]
		for _, ch := range list {
			switch {
			case strings.HasPrefix(ch, "web_search：") && strings.TrimSpace(wsProxy) != "":
				continue // 已配置独立出口 ⇒ 不再是「未覆盖」的裸通道
			case strings.HasPrefix(ch, "全局出网代理路径") && captureOn:
				continue // 捕获开启 ⇒ Bash 走录制代理（带任务身份与范围判定）
			}
			out = append(out, ch)
		}
		list = out
	}
	return newEgressGovernanceState(list)
}

// newEgressGovernanceState 按清单构造状态：清单非空 ⇒ Governed=false。
func newEgressGovernanceState(channels []string) EgressGovernanceState {
	list := append([]string(nil), channels...)
	return EgressGovernanceState{Governed: len(list) == 0, UncoveredChannels: list}
}

// MarshalJSON 保证「清单非空 ⇒ governed=false」这条不变式在序列化之后依然成立：
// governed 一律由清单派生，不接受外部塞进来的值。
func (s EgressGovernanceState) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Governed          bool     `json:"governed"`
		UncoveredChannels []string `json:"uncovered_channels"`
	}{
		Governed:          len(s.UncoveredChannels) == 0,
		UncoveredChannels: s.UncoveredChannels,
	})
}

// UnmarshalJSON 同样由清单派生 governed：反序列化也造不出「已治理」的说法。
func (s *EgressGovernanceState) UnmarshalJSON(b []byte) error {
	var raw struct {
		Governed          bool     `json:"governed"`
		UncoveredChannels []string `json:"uncovered_channels"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	s.Governed = len(raw.UncoveredChannels) == 0
	s.UncoveredChannels = raw.UncoveredChannels
	return nil
}

// RegisterEgressGovernanceRoutes 挂上出网治理状态的路由。
//
// 路由注册不在本文件：server/server.go 的 mux 注册表是别的单元的文件域。属主在
// 注册区加一行 RegisterEgressGovernanceRoutes(mux) 即可。
func RegisterEgressGovernanceRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/settings/egress-governance", getEgressGovernanceState)
}

// getEgressGovernanceState 回当前出网治理状态。
//
// 回的是 governed=false 与未覆盖通道清单 —— 平台不提供「已管控」的措辞。
func getEgressGovernanceState(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, CurrentEgressGovernanceState())
}

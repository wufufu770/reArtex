package agent

import "strings"

// R1（A-15B 收口，web_search 四路同口径）。
//
// 背景：A-15 只给 planner 装了「联网搜索独立出口代理缺失即禁用」的门，
// worker / mainagent / chat 三路仍是「设置开着就装配、WebSearchProxy 空则直连」
// ——同一条直连旁路在另外三路敞着。本文件把门收成四路共用的一个判定，
// 防止再出现「planner 有门、其余三路直连」的分叉。

// WebSearchReady 是联网搜索的装配门：全局开关开着 **且** 独立出口代理已配置
// 才放行。worker / planner / mainagent / chat 四个装配点必须一律经本函数判定。
//
// 为什么独立出口代理是硬条件：web_search 走 WebSearchProxy（http/https/socks5），
// 与录制流量的 MITM 代理无关；代理为空 = 搜索流量直连，不经录制代理侧的任务
// 范围判定与留痕。WebSearchScope.Classify 在应用层有目的地判定+审计
// （traffic.InstallWebSearchScope），但网络层无强制——与 A-15 对 planner 的处置
// 同口径：缺失即禁用（工具不装配），而不是「开着但没人管」。运维要恢复搜索
// 能力，路径是配置独立出口代理，不是改这里。空白串按缺失处理（fail-closed）。
func WebSearchReady(o WebSearchOpts) bool {
	return o.Enabled && strings.TrimSpace(o.Proxy) != ""
}

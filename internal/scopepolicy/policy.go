// Package scopepolicy 是 scope（授权范围）策略的【代码常量】单一真相源。
//
// 为什么是代码常量而不是配置：DB 里的规则行可以被 DELETE，settings 可以被持有
// JWT 的人热改 —— 与 guard/shortcircuit.go 对拒绝名单的同一条理由。扩权上限与
// 「这个资产在不在范围内」这两个判定必须读同一个值，否则 agent 可以把 scope
// 扩到上限 A、按上限 B 被准入，守卫就被自我拆台。
//
// 为什么是叶子包：db / agent / server 三方都要 import 它。放在 agent 里会成环
// （agent → db，db 不能 → agent），编译不过。
package scopepolicy

// 平台范围（没有任务上下文时）的两种模式。
const (
	// ScopeEmpty = 什么都看不到（fail-closed）。这是默认值：没有任务上下文
	// 就没有授权边界，宁可什么都看不到，也不要把共享资产库整个端出去。
	ScopeEmpty = "empty"
	// ScopePlatformOwned = 只看平台自建/导入的资产（origin='platform'）。
	ScopePlatformOwned = "platform_owned"
)

// Policy 是 scope 策略的代码常量。
//
// 扩权上限（MaxAgentAddedScope）与「范围内」判定（PlatformScopeMode）读的都是
// 这一个值 —— 改一处，两处行为同时变（有测试守护）。
var Policy = struct {
	// PlatformScopeMode 是没有任务上下文时使用的范围。
	//   ScopePlatformOwned → 只有 origin='platform' 的资产可见
	//   ScopeEmpty          → 什么都不可见（fail-closed）
	PlatformScopeMode string
	// MaxAgentAddedScope 是一次任务内 agent 自行扩范围的最大条数。
	MaxAgentAddedScope int
	// MaxAgentAddedKinds 是 agent 可以自行扩范围时允许的 kind 白名单。
	// agent 只能在这几种 kind 里收窄/放宽，超出即拒。
	MaxAgentAddedKinds []string
}{
	// 默认 fail-closed：没有任务上下文 ⇒ 什么都看不到。
	PlatformScopeMode: ScopeEmpty,
	// 单次任务内的扩张预算：防一次运行把 scope 刷爆。
	MaxAgentAddedScope: 8,
	// 5 种 kind 覆盖「从种子资产向邻近资产合理扩张」的典型路径。
	// 刻意不含 root_domain / company —— 这两种 kind 会一次性把整棵资产树拉进来
	// （= 边界被整体推翻），必须走 Tier 3 人工确认，不得由 agent 自主扩权。
	MaxAgentAddedKinds: []string{"subdomain", "ip", "cidr", "icp", "keyword"},
}

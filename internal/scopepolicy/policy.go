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

// ScopeKinds 是 task_scope.kind 的【全部合法取值】—— 范围行的 kind 白名单。
//
// 为什么与 Policy.MaxAgentAddedKinds 分开：那是 agent 可以【自行扩范围】的子集，
// 这里则是「哪些 kind 合法存在」的全集。两者不是同一件事：
//
//   - asset（按资产 id 声明范围）只在这里，不在 MaxAgentAddedKinds 里。
//     它是宿主/操作者侧的授权决定（任务大厅勾选资产后发布），
//     agent 不得自行把范围扩到某一条具体资产 —— 那等于自己挑授权边界。
//   - 扩权条数上限（Policy.MaxAgentAddedScope）同样只管 agent 的自主扩权，
//     不管操作者侧的声明（见 db.AddTaskScopeAsset）。
//
// 这里的取值必须与 db/schema.sql 里 task_scope_kind_check 的枚举一致 ——
// DDL 拦得住「库里出现非法 kind」，代码拦得住「代码写出一个非法 kind」。
var ScopeKinds = []string{
	"company", "root_domain", "subdomain", "ip", "cidr", "icp", "keyword", "asset",
}

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

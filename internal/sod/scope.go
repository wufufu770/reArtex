package sod

import (
	"context"
	"strings"
)

// ManagedScopeOf 返回该次调用是否处于 SoD 链的管辖范围内。
//
// 判据：ctx 里带了 SoD 运行上下文（task_id>0 且 agent_key 非空）。
//
// 为什么只有 worker 路径会带：agent 包里的工具装配（AugmentTools）是 worker /
// planner / mainagent / chat **共用**的，而 SoD 链只挂在 worker 的 hook 上
// （宿主装配点：server/engine.go 的 `inner: t.Guard.Hooks()`）。chat /
// planner / mainagent 的 ctx 里既没有任务号也没有 agent key ⇒ 它们的调用
// 不在管辖范围内。
//
// 为什么这条判据必须存在：执行器装饰器装在**所有** agent 的工具上，而凭证
// 只有链能铸、链只挂在 worker 上。若执行器在非受管路径上也索要凭证 ⇒
// chat / planner / mainagent 的每一次工具调用都被拒 ⇒ 整个平台瘫掉。
// 「失败关闭」用在这里等于「要求一把永远不存在的钥匙」。
//
// ⚠️ 宿主侧的配套不变量：SoD 运行上下文**只在链已安装的路径上**挂载。链没装
// 却挂了运行上下文 ⇒ 受管路径上没有链铸凭证 ⇒ 执行器一律拒绝（这是 fail-closed
// 的正确落法，但要靠宿主保证「没装链就别挂上下文」）。
//
// 返回值：不在管辖范围内时返回**零值** RunContext —— 调用方应当以 bool 为准，
// 零值是为了防止误把半截上下文当成有效授权范围。
func ManagedScopeOf(ctx context.Context) (RunContext, bool) {
	run := currentRunContext(ctx)
	if run.TaskID <= 0 || strings.TrimSpace(run.AgentKey) == "" {
		return RunContext{}, false
	}
	return run, true
}

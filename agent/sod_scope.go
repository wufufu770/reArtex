package agent

import (
	"context"
	"strconv"

	"github.com/Autumn-27/artex/internal/sod"
)

// 这个文件是 SoD（职责分离）链在 agent 侧的**运行上下文读法**。
//
// 背景：SoD 链只挂在 worker 的 hook 上（server/engine.go 的 steerHooks.inner），
// 而工具装配（AugmentTools）被 worker / planner / mainagent / chat **共用**。
// ⇒ 执行器装饰器若无条件索要执行凭证，planner / chat / mainagent 的工具调用会被
// 全部误拒 —— 平台直接瘫掉。
//
// ⇒ 受管与否必须有**唯一真相源**，且这个真相源要同时能回答两件事：
//  ① 这次调用归不归 SoD 链管？
//  ② 这次调用的四元组里 agent_key 分量是什么？
//
// 判据取 RunInfo.IntentID：
//   - worker 执行一条意图时 IntentID = 该意图节点 id（>0），见 agent/worker.go:402；
//   - planner / mainagent 一律 IntentID=0（agent/planner.go:407、agent/mainagent.go:128）；
//   - chat 一律 IntentID=0（agent/chat.go:163 只设 SessionID）。
//
// 为什么用 IntentID 而不是 worker 槽位名（work#N）：
//   - 槽位名只活在 server/engine.go 的局部变量与 intercept 的私有 ctx 键里
//     （intercept.taskInfoFromCtx 未导出），agent 包拿不到；
//   - IntentID 是 worker 路径独有且稳定的量：同一 worker 执行同一条意图时它恒定；
//   - 同任务两个并发 worker 执行的意图必不同 ⇒ 四元组不会撞车。撞车的后果是
//     具体的：TokenStore.Mint 对同一四元组是「先作废旧的再写新的」⇒ 后铸的凭证
//     把先铸的作废 ⇒ 先到的执行器取不到凭证而被误拒（不是漏放行，是误拒）。
//
// ⚠️ 这个读法是 internal/sod 的 ContextReader 扩展点（sod.SetContextReader）：
// 宿主装配时绑定一次，链（铸凭证）与执行器（查凭证）于是从**同一个** ctx 里取
// **同一个**原值 ⇒ 四元组必然对齐。这也是 internal/sod 不必 import agent 的
// 原因（反向依赖会成环）。

// SodContextReader 返回 SoD 链的运行上下文读法，供宿主装配时交给
// sod.SetContextReader 绑定。
//
// 为什么返回函数而不是自己绑：绑定是**进程级**的（一张凭证表、一套读法），属于
// 宿主的装配职责；agent 包只提供读法本身。
//
// 为什么 TaskID 取自 RunInfo 而不是硬取：chat 会话也可能带任务号（群聊成员会话与
// 任务绑定），但它不带 IntentID ⇒ 仍不受管。
func SodContextReader(ctx context.Context) sod.RunContext {
	ri := RunInfoFrom(ctx)
	// IntentID <= 0 ⇒ 不是 worker 执行体（planner / mainagent / chat / 无归属）
	// ⇒ 不受 SoD 链管 ⇒ 不带任务号也不带 agent key（sod.ManagedScopeOf 判 false）。
	if ri.IntentID <= 0 {
		return sod.RunContext{}
	}
	return sod.RunContext{
		TaskID:   ri.TaskID,
		AgentKey: sodScopeAgentKey(ri),
	}
}

// sodScopeAgentKey 算出 SoD 四元组里的 agent_key 分量。
//
// 形态 "worker#<意图id>"：同任务里两个并发 worker 会同时跑同一个工具、载荷也可能
// 相同（同样一条 ls）⇒ 若 agent_key 都是 "worker"，四元组撞车 ⇒ 后铸的凭证作废
// 先铸的 ⇒ 先到的执行被误拒。带意图 id 即可区分。
//
// 为什么审计账本里也要能看出是谁：sod_decisions.source_agent 记的就是这个值。
func sodScopeAgentKey(ri RunInfo) string {
	if ri.IntentID <= 0 {
		return ""
	}
	return "worker#" + strconv.FormatInt(ri.IntentID, 10)
}

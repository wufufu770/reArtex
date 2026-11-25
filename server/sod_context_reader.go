package server

import (
	"context"
	"log"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/internal/sod"
)

// sod_context_reader.go —— 统一受管执行身份判据（SEC-01，A-03 第一批，0.15.95）。
//
// A-03 的目标：SoD 链从「只挂 worker」扩展到 planner / mainagent / chat 三路，
// 不再出现链外执行。旧判据（agent.SodContextReader，Tier-0 冻结文件）只认
// RunInfo.IntentID>0：planner/mainagent/chat（IntentID 恒 0）一律读到空上下文
// ⇒ 不受管 ⇒ 不裁决。本文件在**不修改任何 Tier-0 文件**的前提下换绑一个扩展
// 读法（SetContextReader 的绑定点 server.go 是 Tier-2），把判据从「ID>0」升级为
// 「ID>0 且通过意图↔任务绑定校验」+「带任务的非 worker 路径受管」+「无任务路径
// 显式不受管但仍在链的 fail-closed 拒绝面内」。
//
// 三分支（优先级即代码顺序）：
//
//	① worker 主张（IntentID>0）：必须通过 exploration_nodes↔tasks 的绑定校验
//	   （意图节点存在、kind='intent'、且挂在 ri.TaskID 名下）才继承既有 worker
//	   语义（agent.SodContextReader，字节不变）。校验不过 = 伪造/跨任务/已删意图
//	   ⇒ 返回空上下文 = 不受管 —— 但此时链 hooks 已挂上（见接线），gated 工具在
//	   链的 taskID<=0 分支被 fail-closed 拒绝并留审计账本行，读侧（NotGatedTools）
//	   不受影响。**校验失败绝不等于放行**：链在 hooks 上，空上下文进链即拒绝。
//	   pg 不可用同样按未绑定处理（nil 依赖拒绝，不 fail-open）。
//
//	② planner / mainagent / 群聊成员（TaskID>0 且无意图）：受管。AgentKey 取
//	   ctx 的运行身份（agent.AgentKeyFrom，与执行器查凭证用的是同一个 ctx 同一个
//	   原值 ⇒ 四元组天然逐字节对齐）。链随后按 tier/scope 正常裁决：gated 工具
//	   走规整器→闸门→一次性凭证，与 worker 同一条安全链。
//
//	③ 无任务上下文（纯独立 chat 会话）：显式不受管（TaskID=0 进不了
//	   ManagedScopeOf），但这不是绕过后门——三路的 hooks 都已挂链，NotGatedTools
//	   之外的每一次调用都被链按「任务上下文缺失」拒绝并记账（source_agent 取
//	   ctx 运行身份，账本里可见是谁被拒）。只读观测照常放行——与 worker 路的
//	   读侧行为完全一致，不是 no-task 特权。
//
// 与卡面差异（如实说明）：原 A-03 卡的 no_task.go 骨架把放行面定义为
// 「NoTaskPolicy.ReadOnlyTools」，实际仓库里这个面**已经存在**——
// internal/sodpolicy.NotGatedTools（Tier-0 的既有政策面，Read/LS/Glob/平台只读/
// 纯记账），链的 GatedTool 白名单外直通。重复造一份会两处漂移，故直接复用；
// 拒绝理由文案沿用链的既有「任务上下文缺失，无法判定授权范围」（文案在
// Tier-0 chain.go，本卡不改），语义与卡面「需先归属任务」一致。
//
// 读法返回空上下文的语义统一是「gated 拒绝 + 只读放行」，fail-closed 方向由
// 链保证，本读法只负责把「这次调用是谁的」如实报给链。
func (s *Server) sodContextReader() sod.ContextReader {
	return func(ctx context.Context) sod.RunContext {
		ri := agent.RunInfoFrom(ctx)

		// ① worker 主张：先验证绑定，再继承既有读法（字节不变）。
		if ri.IntentID > 0 {
			if !s.managedIntentBound(ri) {
				return sod.RunContext{}
			}
			return agent.SodContextReader(ctx)
		}

		// ② 带任务的非 worker 路径：受管，身份取 ctx 运行 key。
		if ri.TaskID > 0 {
			return sod.RunContext{TaskID: ri.TaskID, AgentKey: agent.AgentKeyFrom(ctx)}
		}

		// ③ 无任务：显式空上下文（链的 fail-closed 拒绝面接管 gated 调用）。
		return sod.RunContext{}
	}
}

// managedIntentBound 校验「意图节点 ↔ 任务」的真实绑定：节点存在、是 intent、
// 且经 explorations 归在 ri.TaskID 名下。任何一环不成立（含 pg 不可用、查询
// 出错）都返回 false——判据校验失败按未绑定处理，绝不猜。
//
// 性能口径：本函数只在链的 gated 调用路径上被走到（NotGatedTools 在链的步骤③
// 就已直通，不会进到读上下文）；gated 调用本身还要过规整器（LLM，秒级），一次
// 主键索引的 EXISTS 可忽略。
func (s *Server) managedIntentBound(ri agent.RunInfo) bool {
	if s == nil || s.m == nil || s.m.pg == nil || ri.TaskID <= 0 {
		return false
	}
	var bound bool
	err := s.m.pg.QueryRow(`
SELECT EXISTS(
    SELECT 1
    FROM exploration_nodes n
    JOIN tasks t ON t.exploration_id = n.exploration_id
    WHERE n.id = $1 AND n.kind = 'intent' AND t.id = $2
)`, ri.IntentID, ri.TaskID).Scan(&bound)
	if err != nil {
		// 查询失败按未绑定处理（fail-closed），但打日志留排查入口——
		// 静默吞掉会让「库坏了」和「身份是伪造的」在日志上无法区分。
		log.Printf("[sod] 受管判据的意图绑定校验查询失败（按未绑定处理）: %v", err)
		return false
	}
	return bound
}

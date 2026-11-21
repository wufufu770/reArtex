package agent

// LoopGuardRunner 把 LoopGuard 包成 harness.HookRunner（0.15.34，批次 F）：
// PreToolUse 边界先做重复调用判定（3× 硬拦），未拦再透传内层（SoD 合并闸）。
// 放在 SoD 链外层的原因：空转拦截不消耗 SoD 账本行——重复调用是流程缺陷不是
// 裁决事件；且内层可能 deny，deny 后模型换参数重试属正常路径，不该计入守卫。
//
// per-run 生命周期：engine 在每次 intent 执行时 NewLoopGuard() 一枚，
// 随 run 结束丢弃（重复检测是 run 内语义，跨 run 不累计——换意图/续跑是
// 新策略的开始）。

import (
	"context"

	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/llm"
)

// LoopGuardRunner 是 loopguard 的 HookRunner 包装。
type LoopGuardRunner struct {
	Inner harness.HookRunner
	Guard *LoopGuard
}

// NewLoopGuardRunner 组装（guard 为 nil 时不拦截，纯透传——防御性）。
func NewLoopGuardRunner(inner harness.HookRunner, guard *LoopGuard) *LoopGuardRunner {
	return &LoopGuardRunner{Inner: inner, Guard: guard}
}

// PreToolUse 先过 loopguard，未拦再进内层。
func (r *LoopGuardRunner) PreToolUse(ctx context.Context, name string, input []byte) (bool, string, []byte) {
	if r.Guard != nil {
		v := r.Guard.Observe(name, input)
		if v.Block {
			return true, v.Message, nil
		}
	}
	if r.Inner != nil {
		return r.Inner.PreToolUse(ctx, name, input)
	}
	return false, "", nil
}

// PostToolUse 透传内层（loopguard 无后置逻辑）。
func (r *LoopGuardRunner) PostToolUse(ctx context.Context, name string, input, result []byte, isErr bool) {
	if r.Inner != nil {
		r.Inner.PostToolUse(ctx, name, input, result, isErr)
	}
}

// Stop 透传内层。
func (r *LoopGuardRunner) Stop(ctx context.Context, messages []llm.Message) (bool, []string, string) {
	if r.Inner != nil {
		return r.Inner.Stop(ctx, messages)
	}
	return false, nil, ""
}

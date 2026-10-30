package sod

import (
	"context"
	"encoding/json"

	acperm "github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
)

// ExecutorGate 是执行器装饰器：执行前消费一次性凭证。
//
// ⚠️ 它是链的**另一半**：链负责「批准」，执行器负责「**证明这次执行就是被
// 批准的那一次**」。两者靠 (task_id, source_agent, tool_name, payload_digest)
// 四元组对齐。
//
// 为什么不是「把 token 传给执行器」：PreToolUse 的返回契约里没有地方可以回传
// token，而且 hook 的 ctx 与工具执行的 ctx 不是同一个（hook 拿 l.ctx，工具拿
// l.toolCtx —— 后者是前者的子 context，context 不可变 ⇒ hook 里的修改传不过
// 去）。四元组索引是唯一走得通的通道，且它天然做到了「不可转让」。
type ExecutorGate struct {
	inner  actool.CoreTool
	tokens *TokenStore
}

// WrapExecutor 给工具套上执行器装饰器。
//
// ⚠️ gated=false（放行清单里的工具）⇒ 原样返回，不套 —— 它们的调用不产生
// 凭证，也没有凭证可消费。
//
// ⚠️ gated=true 但 tokens == nil ⇒ **仍然套**，且执行器一律拒绝：凭证存储没
// 装配说明链是半成品，而半成品的链绝不能变成「无凭证放行」的旁路。
func WrapExecutor(t actool.CoreTool, tokens *TokenStore, gated bool) actool.CoreTool {
	if !gated {
		return t
	}
	return &ExecutorGate{inner: t, tokens: tokens}
}

// Name 透传工具名。
func (g *ExecutorGate) Name() string { return g.inner.Name() }

// Description 透传工具描述。
func (g *ExecutorGate) Description() string { return g.inner.Description() }

// Prompt 透传工具提示词。
func (g *ExecutorGate) Prompt() string { return g.inner.Prompt() }

// InputSchema 透传入参 schema。
func (g *ExecutorGate) InputSchema() map[string]any { return g.inner.InputSchema() }

// IsReadOnly 透传只读判定（装饰器不改变工具的固有属性）。
func (g *ExecutorGate) IsReadOnly(in json.RawMessage) bool { return g.inner.IsReadOnly(in) }

// IsConcurrencySafe 透传并发安全判定。
func (g *ExecutorGate) IsConcurrencySafe(in json.RawMessage) bool {
	return g.inner.IsConcurrencySafe(in)
}

// CheckPermissions 透传工具自判的权限判定（Stage-1 的一票否决权在链之前，
// 装饰器不得削弱它）。
func (g *ExecutorGate) CheckPermissions(ctx context.Context, in json.RawMessage, pc acperm.Context) acperm.Decision {
	return g.inner.CheckPermissions(ctx, in, pc)
}

// Call 在真正执行前消费凭证。
//
// 四步全过才放行：① 重算将要执行的字节的摘要；② 用四元组查凭证；
// ③ 比对摘要（凭证绑定的是**执行器将要跑的那份字节**）；④ 原子消费（删掉）。
//
// ⚠️ 四元组任一不符 ⇒ **拒绝执行**（返回错误结果，不调用底层工具）。
// 第二次拿同一个凭证 ⇒ 查不到 ⇒ 拒绝 ⇒ **重放失败**。
//
// ⚠️ 递归防护：ctx 带 re-entrancy 标记（规整器自己的模型调用）⇒ 直连。
func (g *ExecutorGate) Call(ctx context.Context, in json.RawMessage, tc *actool.ToolContext) (actool.Result, error) {
	if InReentrancy(ctx) {
		return g.inner.Call(ctx, in, tc)
	}
	if g.tokens == nil {
		return actool.Errorf("执行凭证存储未装配：本次调用未经 SoD 链批准，拒绝执行"), nil
	}
	run := currentRunContext(ctx)
	digest := CanonicalDigest(in)
	if !g.tokens.Take(digest, run.TaskID, run.AgentKey, g.inner.Name()) {
		return actool.Errorf("执行凭证缺失或不匹配：本次调用未被 SoD 链批准，拒绝执行"), nil
	}
	return g.inner.Call(ctx, in, tc)
}

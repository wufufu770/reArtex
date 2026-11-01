package sod

import (
	"context"
	"encoding/json"
	"log"
	"sync/atomic"

	acperm "github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
)

// executorTokens 是进程内唯一的执行器凭证表。
//
// 为什么用 atomic.Pointer 而不是普通变量 + 锁：读方在每次工具调用时都要读它，
// 而写方只在启动时装配一次 ⇒ 无锁读是热路径上唯一合理的形态。
var executorTokens atomic.Pointer[TokenStore]

// SetExecutorTokens 设置进程内唯一的执行器凭证表（由 server 装配时调用一次）。
//
// ⚠️ **首次写入生效**：第一次传入非 nil 的表之后，后续调用一律被忽略（只记
// 日志）。为什么不是「后写覆盖」：凭证表被换掉意味着「链铸的凭证」与「执行器
// 查的表」不再是同一张 ⇒ 每一个被批准的动作都在执行器侧查不到凭证 ⇒ 全被拒
// （fail-closed，但链等于瘫了）。半装好的链绝不能出现两张互相不认的表。
//
// 传 nil 不算数：nil 表示「没装配」，把它写进单例等于把执行器指向空指针 ⇒
// 受管路径上拒绝一切，而宿主以为表已经装好了。
func SetExecutorTokens(s *TokenStore) {
	if s == nil {
		log.Printf("[sod] 执行器凭证表未装配，忽略本次设置")
		return
	}
	if executorTokens.CompareAndSwap(nil, s) {
		return
	}
	log.Printf("[sod] 执行器凭证表已存在，忽略本次设置（首次写入生效）")
}

// ExecutorTokens 读执行器凭证表（未装配 ⇒ nil）。
//
// 为什么 agent 侧的装饰器从这里读：agent 包的工具装配是共用代码（worker /
// planner / mainagent / chat），没有地方塞一个 TokenStore 参数 ⇒ 只能从
// 进程级单例读。链那一侧由宿主把同一张表直接传进 ChainDeps.Tokens。
func ExecutorTokens() *TokenStore {
	return executorTokens.Load()
}

// ResetExecutorTokensForTest 清空进程级执行器凭证表。**仅供测试复位。**
//
// 为什么需要它：单例是进程级的，而同一测试包里的用例共享一个进程 ⇒ 不复位
// 的话「未装配」这个前置条件会随执行顺序漂移，用例之间互相依赖。
//
// ⚠️ **生产代码不许调用**：复位之后下一次 SetExecutorTokens 会装进**另一张**
// 表 ⇒ 链铸的凭证与执行器查的表不再是同一张 ⇒ 被批准的动作全被拒。复位只
// 在测试进程里有意义（那里没有「正在跑的链」）。
func ResetExecutorTokensForTest() {
	executorTokens.Store(nil)
}

// ResetExecutorTokens 清掉进程内的执行器凭证表。
//
// **生产可用**（不是测试专用）：宿主在**要装一条新链**时调用它 —— 因为链换了，
// 铸凭证的表就换了，而执行器必须跟着换，否则新链铸的凭证永远查不到 ⇒ 每一个被
// 批准的动作都被执行器拒（fail-closed，不漏放行，但整条链瘫了）。
//
// 为什么需要它、而不是让 SetExecutorTokens 直接覆盖：SetExecutorTokens 的
// 「首次写入生效」是防「半装好的链」的安全语义（两张互相不认的凭证表比没有更
// 危险）。但那条语义在「链被**重建**」这个场景下会变成故障：执行器握着旧表、
// 链握着新表 ⇒ 谁也认不出谁。
//
// 所以拆成两个入口：默认路径仍走 SetExecutorTokens（首次写入生效，防半成品），
// 只有宿主明确要重建链时才显式 ResetExecutorTokens —— 意图可见、可审计。
func ResetExecutorTokens() {
	executorTokens.Store(nil)
}

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
//
// ⚠️ 非受管路径（ctx 里没有 SoD 运行上下文）⇒ 直连，**不索要凭证**：
// 装饰器装在所有 agent 的工具上，而链只挂在 worker 上 —— 在没有链的路径上
// 索要凭证等于索要一把没人铸的钥匙 ⇒ chat / planner / mainagent 的每一次
// 工具调用都会被拒 ⇒ 平台瘫掉。
func (g *ExecutorGate) Call(ctx context.Context, in json.RawMessage, tc *actool.ToolContext) (actool.Result, error) {
	if InReentrancy(ctx) {
		return g.inner.Call(ctx, in, tc)
	}
	run, managed := ManagedScopeOf(ctx)
	if !managed {
		return g.inner.Call(ctx, in, tc)
	}
	// 凭证表**不快照**，每次调用现读单例。
	//
	// 为什么：装配发生在【工具装配时】，装链发生在【server 启动时】—— 两者的先后
	// 顺序不是承诺。若装配早于装链，装配那一刻读到的单例是 nil ⇒ 这个执行器会
	// **永久**持着一张空表 ⇒ 受管路径上的每一次调用都被拒（fail-closed，不漏放行，
	// 但整条链静默瘫掉，而且日志上看不出是「表没装」而不是「链没批」）。
	//
	// 链重建（换了一张新表）也是同一个病：已装配好的执行器握着旧表，新链铸的凭证
	// 在新表 ⇒ 互相认不出。
	//
	// 现读单例把「装配顺序」这个变量彻底去掉：执行器与链永远读同一张表。
	store := g.tokens
	if store == nil {
		store = ExecutorTokens()
	}
	if store == nil {
		log.Printf("[sod] 执行器拒绝：凭证表未装配（task=%d agent=%q tool=%q）—— 本次调用未经链批准", run.TaskID, run.AgentKey, g.inner.Name())
		return actool.Errorf("执行凭证存储未装配：本次调用未经 SoD 链批准，拒绝执行"), nil
	}
	digest := CanonicalDigest(in)
	if !store.Take(digest, run.TaskID, AgentKeyFromContext(ctx), g.inner.Name()) {
		// ⚠️ 为什么必须记日志：执行器拒绝是**静默**的（错误只回到工具结果里，
		// 不进日志）⇒ 运维看到「工具莫名其妙被拒」却查不到原因。而这里有四种
		// 互斥原因（凭证表不对 / 载荷摘要不符 / 四元组不匹配 / 已被消费过），
		// 处置方式完全不同 ⇒ 必须把**哪个维度不匹配**记出来。
		//
		// 只记维度与摘要，绝不记载荷原文（原始载荷只允许以摘要 + 脱敏预览入库）。
		log.Printf("[sod] 执行器拒绝：凭证缺失或不匹配（task=%d agent=%q tool=%q digest=%s）—— 本次调用未经链批准",
			run.TaskID, AgentKeyFromContext(ctx), g.inner.Name(), digest)
		return actool.Errorf("执行凭证缺失或不匹配：本次调用未被 SoD 链批准，拒绝执行"), nil
	}
	return g.inner.Call(ctx, in, tc)
}

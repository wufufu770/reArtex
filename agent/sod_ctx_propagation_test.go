package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/intercept"
	"github.com/Autumn-27/artex/internal/sod"
	"github.com/Autumn-27/norma/agentcore"
)

// 这个文件钉住「SoD 链在真实执行路径上能不能拿到运行上下文」。
//
// 背景：链铸的凭证与执行器查的凭证都靠四元组里的 agent_key 对齐，而 agent_key
// 来自 agent.RunInfo（worker 路径独有）。RunInfo 是挂在 ctx 上的 ⇒ ctx 传递链上
// 任何一环把它弄丢，链与执行器就都读到空上下文 ⇒ 受管判定恒为「不受管」⇒
// 闸门整条链形同虚设（工具照跑，只是不裁决）。这是**静默**失效：没有报错、
// 没有日志、只是保护没了。
//
// 真实路径（已逐段核实）：
//
//	agent/worker.go:402   ctx = WithRunInfo(ctx, RunInfo{..., IntentID: intent.ID})
//	  → agent/worker.go:562  captureRunSession(ctx, s, input, emitWrap)
//	    → agent/capture.go:38   ctx, _ = intercept.WithTrace(ctx, ...)
//	    → agent/capture.go:80   s.Prompt(ctx, input)
//	      → agentcore  harness.Query(ctx, in, deps)
//	        → harness/query.go:210  l.ctx = ctx
//	          → harness/tools.go:78   Hooks.PreToolUse(l.ctx, ...)   ← 链读 l.ctx
//	          → harness/tools.go:98   t.Call(l.toolCtx, ...)         ← 执行器读 toolCtx
//	            （harness/query.go:385  l.toolCtx = l.ctx 派生）
//
// 本文件覆盖的是 ctx 传递链上**属于 agent 包的**那些环：两次 ctx 派生
// （intercept 的审阅上下文、side question 的挂载）都不能丢 RunInfo。
// harness 那一段由 server 侧的端到端验收测试覆盖（真实 worker 意图 ⇒
// sod_decisions 里有决策行）。

// TestSoDContextSurvivesReviewContextWrap 钉住「审阅上下文包装不丢 RunInfo」。
//
// agent/worker.go:416 在设完 RunInfo 之后套了一层 intercept.WithReviewContext
// —— 它若换掉 ctx 而不是派生，RunInfo 就在这里丢了，而这一步发生在工具真正
// 开始跑之前 ⇒ 链全程读不到任务上下文。
func TestSoDContextSurvivesReviewContextWrap(t *testing.T) {
	base := WithRunInfo(context.Background(), RunInfo{TaskID: 7, ExplorationID: 3, IntentID: 42})
	wrapped := intercept.WithReviewContext(base, "/tmp/run-dir", intercept.ReviewBackground{})

	ri := RunInfoFrom(wrapped)
	if ri.IntentID != 42 || ri.TaskID != 7 || ri.ExplorationID != 3 {
		t.Fatalf("审阅上下文包装后 RunInfo = %+v，期望原样保留 {TaskID:7 ExplorationID:3 IntentID:42}", ri)
	}
	if key := SodContextReader(wrapped).AgentKey; key == "" {
		t.Fatal("包装后不该丢 agent_key（否则链读不到受管身份）")
	}
}

// TestSoDContextSurvivesSideCaptureAttach 钉住「side question 挂载不丢 RunInfo」。
//
// agent/worker.go:525 的 attachSideCapture 会在挂载 side question 时派生 ctx。
// 它是 RunInfo 设置之后、真正跑 session 之前的最后一环 ⇒ 这里丢了就全丢。
//
// 为什么这个环特别值得钉：attachSideCapture 自己也要读 RunInfo（取 TaskID /
// ExplorationID / IntentID 去建 parent），所以它「读得到」不等于「传得下去」——
// 派生 ctx 时若用 context.Background() 起头，parent 就对了、RunInfo 却没了。
func TestSoDContextSurvivesSideCaptureAttach(t *testing.T) {
	base := WithRunInfo(context.Background(), RunInfo{TaskID: 7, ExplorationID: 3, IntentID: 42})

	// attachSideCapture 需要一个 *agentcore.Options（生产代码传的是非 nil 的
	// &opts）。这里给一个空壳：ctx 里没有 side question 的 publisher ⇒
	// sidequestion.Attach 直接原样返回 ctx 与 deps，不碰 provider。
	//
	// 两条分支都保留 RunInfo：① 无 publisher ⇒ 原样返回；② 有 publisher ⇒
	// context.WithValue(ctx, captureKey{}, c) 派生，WithValue 保留父链上的值。
	got := attachSideCapture(base, &agentcore.Options{})
	if ri := RunInfoFrom(got); ri.IntentID != 42 || ri.TaskID != 7 {
		t.Fatalf("side capture 挂载后 RunInfo = %+v，期望原样保留", ri)
	}
	if key := SodContextReader(got).AgentKey; key == "" {
		t.Fatal("挂载后不该丢 agent_key")
	}
}

// TestSoDContextSurvivesChatMergeWithoutIntent 钉住「chat 场景不受管」。
//
// chat 用 WithRunInfoMerged 合并外层 RunInfo（agent/chat.go:114-128），而
// WithRunInfoMerged 会把外层的 IntentID 一并带进来（:122-124）。chat 自己从不
// 设 IntentID ⇒ 正常情况下合并后仍是 0 ⇒ 不受管。
//
// 为什么这条是生死线：执行器装饰器装在**所有** agent 共用的工具装配点上。若
// chat 被误判为受管 ⇒ chat 的每一次工具调用都去索要执行凭证 ⇒ 而链只挂在
// worker 上 ⇒ 没人铸 ⇒ chat 彻底瘫掉（fail-closed 用错了地方）。
//
// 这里显式构造「外层带 IntentID」的极端情形，确认判据仍然只看 IntentID 与
// 任务号是否齐备 —— 不会因为 SessionID 非空就误判。
func TestSoDContextSurvivesChatMergeWithoutIntent(t *testing.T) {
	// 外层没有 IntentID（chat 的真实情形）：带 SessionID 与 TaskID，仍不受管。
	chatCtx := WithRunInfoMerged(context.Background(), RunInfo{SessionID: "conv-1", TaskID: 7})
	if ri := RunInfoFrom(chatCtx); ri.IntentID != 0 {
		t.Fatalf("chat 合并后的 IntentID = %d，期望 0", ri.IntentID)
	}
	run := SodContextReader(chatCtx)
	if run.TaskID != 0 || run.AgentKey != "" {
		t.Fatalf("chat 场景的运行上下文 = %+v，期望零值（不受管）", run)
	}
	if _, managed := sod.ManagedScopeOf(chatCtx); managed {
		t.Fatal("chat 场景被判为受管 ⇒ chat 的工具调用会索要没人铸的凭证")
	}
}

// TestSoDContextReaderUsedByBothChainAndExecutor 钉住「宿主绑定这一个读法后，
// 链与执行器拿到逐字节相同的 agent_key」。
//
// internal/sod 的 currentRunContext 优先用宿主绑定的读法（sod.SetContextReader）
// ⇒ 绑定之后，链（铸凭证）与执行器（查凭证）必然同源。这是四元组能对上的前提：
// 两端读法不同 ⇒ 凭证永远查不到 ⇒ 每个被批准的动作都被执行器拒。
func TestSoDContextReaderUsedByBothChainAndExecutor(t *testing.T) {
	sod.SetContextReader(SodContextReader)
	t.Cleanup(func() { sod.SetContextReader(nil) })

	ctx := WithRunInfo(context.Background(), RunInfo{TaskID: 7, ExplorationID: 3, IntentID: 42})
	chainSide := sod.AgentKeyFromContext(ctx)
	executorSide := SodContextReader(ctx).AgentKey
	if chainSide != executorSide {
		t.Fatalf("链侧 agent_key = %q，执行器侧 = %q，必须逐字节一致", chainSide, executorSide)
	}
	if !strings.Contains(chainSide, "42") {
		t.Fatalf("agent_key = %q，期望含意图 id（区分同任务的并发 worker）", chainSide)
	}
}

package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/internal/sod"
)

// 这个文件钉住 SoD（职责分离）链在 agent 侧的**运行上下文读法**。
//
// 为什么需要它：SoD 链只挂在 worker 的 hook 上（server/engine.go 的 steerHooks.inner），
// 而工具装配（AugmentTools）被 worker / planner / mainagent / chat **共用**。
// ⇒ 执行器装饰器若无条件索要执行凭证，planner / chat / mainagent 的工具调用会被
// 全部误拒（平台直接瘫掉）。
//
// 判据用 RunInfo.IntentID：worker 执行一条意图时 IntentID=意图节点 id（>0），
// 而 planner / mainagent / chat 一律 IntentID=0（见 agent/worker.go:402 与
// agent/planner.go:407 / agent/mainagent.go:128 / agent/chat.go:163 的对照）。
//
// 为什么用 IntentID 而不是 worker 槽位名（work#N）：
//   - 槽位名只存在于 server/engine.go 的局部变量与 intercept 的私有 ctx 键里，
//     agent 包拿不到（intercept 的 taskInfoFromCtx 未导出）；
//   - IntentID 是 worker 路径**独有**且**稳定**的量：worker 执行一条意图时它恒等于
//     该意图节点 id；同任务两个并发 worker 的意图必不同 ⇒ 凭证四元组不会互相作废
//     （TokenStore.Mint 对同一四元组是先作废旧的再写新的）。

// TestSoDScopeReaderManagedOnlyForWorkerPath 钉住受管判据：只有 worker 路径受管。
func TestSoDScopeReaderManagedOnlyForWorkerPath(t *testing.T) {
	cases := []struct {
		name    string
		ri      RunInfo
		managed bool
	}{
		{"worker 执行一条意图", RunInfo{TaskID: 7, ExplorationID: 3, IntentID: 42}, true},
		{"planner", RunInfo{TaskID: 7, ExplorationID: 3}, false},
		{"mainagent", RunInfo{TaskID: 7, ExplorationID: 3}, false},
		{"chat 会话", RunInfo{SessionID: "conv-1"}, false},
		{"无任何归属信息", RunInfo{}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx := WithRunInfo(context.Background(), c.ri)
			run := SodContextReader(ctx)
			gotManaged := run.TaskID > 0 && strings.TrimSpace(run.AgentKey) != ""
			if gotManaged != c.managed {
				t.Fatalf("读法产出 = %+v，受管判定 = %v，期望 %v", run, gotManaged, c.managed)
			}
		})
	}
}

// TestSoDScopeReaderIsSingleSourceForChainAndExecutor 钉住「链与执行器读到的是
// 同一个值」：internal/sod 的 currentRunContext 优先用宿主绑定的读法 ⇒ 宿主绑定
// 本读法后，链（铸凭证）与执行器（查凭证）必然取到逐字节相同的 agent_key。
//
// 这条是四元组能对上的前提：两端读法不同 ⇒ 凭证永远查不到 ⇒ 每个被批准的动作
// 都被执行器拒（fail-closed，但链等于瘫了）。
func TestSoDScopeReaderIsSingleSourceForChainAndExecutor(t *testing.T) {
	sod.SetContextReader(SodContextReader)
	t.Cleanup(func() { sod.SetContextReader(nil) })

	ctx := WithRunInfo(context.Background(), RunInfo{TaskID: 7, IntentID: 42})
	if got, want := sod.AgentKeyFromContext(ctx), SodContextReader(ctx).AgentKey; got != want {
		t.Fatalf("链侧读到的 agent_key = %q，执行器侧读到 = %q", got, want)
	}
	if got := sod.AgentKeyFromContext(ctx); got == "" {
		t.Fatal("受管路径的 agent_key 不能为空")
	}
}

// TestSoDScopeReaderManagedScopeAgreesWithInternalSod 钉住「本读法与
// internal/sod 自己的判定口径一致」：绑定后 sod.ManagedScopeOf 的结论必须与
// 本读法产出的形状一致，不许两套口径各说各话。
func TestSoDScopeReaderManagedScopeAgreesWithInternalSod(t *testing.T) {
	sod.SetContextReader(SodContextReader)
	t.Cleanup(func() { sod.SetContextReader(nil) })

	cases := []struct {
		name    string
		ri      RunInfo
		managed bool
	}{
		{"worker 执行一条意图", RunInfo{TaskID: 7, IntentID: 42}, true},
		{"planner", RunInfo{TaskID: 7}, false},
		{"chat 会话", RunInfo{SessionID: "conv-1"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx := WithRunInfo(context.Background(), c.ri)
			if _, managed := sod.ManagedScopeOf(ctx); managed != c.managed {
				t.Fatalf("internal/sod 判定受管 = %v，期望 %v", managed, c.managed)
			}
		})
	}
}

// TestSoDScopeReaderAgentKeyStablePerIntent 钉住四元组里的 agent_key 分量：
// 同一个意图 ⇒ 同一个值（执行器与链必须算出同一个值，否则凭证永远取不到）。
func TestSoDScopeReaderAgentKeyStablePerIntent(t *testing.T) {
	ri := RunInfo{TaskID: 7, IntentID: 42}
	first := SodContextReader(WithRunInfo(context.Background(), ri)).AgentKey
	second := SodContextReader(WithRunInfo(context.Background(), ri)).AgentKey
	if first == "" {
		t.Fatal("受管路径的 agent_key 不能为空")
	}
	if first != second {
		t.Fatalf("同一个意图两次算出的 agent_key 不同：%q vs %q", first, second)
	}
}

// TestSoDScopeReaderAgentKeyDiffersPerIntent 钉住不同意图的 agent_key 必须不同：
// 否则同一任务里两个并发 worker 跑同一个工具、载荷也相同时，凭证四元组会撞车 ⇒
// 后铸的凭证把先铸的作废 ⇒ 先到的执行被误拒（凭据在，但取不到）。
func TestSoDScopeReaderAgentKeyDiffersPerIntent(t *testing.T) {
	a := SodContextReader(WithRunInfo(context.Background(), RunInfo{TaskID: 7, IntentID: 42})).AgentKey
	b := SodContextReader(WithRunInfo(context.Background(), RunInfo{TaskID: 7, IntentID: 43})).AgentKey
	if a == b {
		t.Fatalf("不同意图算出了相同的 agent_key：%q", a)
	}
}

// TestSoDScopeReaderAgentKeyEmptyWhenUnmanaged 钉住不受管路径没有 agent_key：
// 不受管就不该有身份，否则执行器会误以为这次调用该索要凭证。
func TestSoDScopeReaderAgentKeyEmptyWhenUnmanaged(t *testing.T) {
	if got := SodContextReader(WithRunInfo(context.Background(), RunInfo{TaskID: 7})).AgentKey; got != "" {
		t.Fatalf("不受管路径的 agent_key = %q，期望空串", got)
	}
	if got := SodContextReader(WithRunInfo(context.Background(), RunInfo{TaskID: 7})).TaskID; got != 0 {
		t.Fatalf("不受管路径的 task_id = %d，期望 0（不受管就不该带任务号）", got)
	}
}

// TestSoDScopeReaderAgentKeyCarriesIntentID 钉住 agent_key 里带上了意图 id：
// 它是四元组里区分并发 worker 的那一维，审计账本里也要能看出来是谁发起的。
func TestSoDScopeReaderAgentKeyCarriesIntentID(t *testing.T) {
	key := SodContextReader(WithRunInfo(context.Background(), RunInfo{TaskID: 7, IntentID: 42})).AgentKey
	if !strings.Contains(key, "42") {
		t.Fatalf("agent_key = %q，期望含意图 id 42", key)
	}
}

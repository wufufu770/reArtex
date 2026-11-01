package sod

import (
	"context"
	"testing"

	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/llm"
)

// fakeHookRunner 是可控的宿主 hook runner（模拟 guard 的短路表 / 拦截规则）。
//
// 为什么需要它：SoD 链是【装饰器】，它的第一件事就是问宿主给的 runner。
// 用真 guard 会把「短路表命中」和「链的裁决」混在一起，测不出顺序。
type fakeHookRunner struct {
	block    bool
	message  string
	updated  []byte
	preCalls int
}

func (f *fakeHookRunner) PreToolUse(ctx context.Context, name string, input []byte) (bool, string, []byte) {
	f.preCalls++
	return f.block, f.message, f.updated
}

func (f *fakeHookRunner) PostToolUse(ctx context.Context, name string, input, result []byte, isErr bool) {
}

func (f *fakeHookRunner) Stop(ctx context.Context, messages []llm.Message) (bool, []string, string) {
	return false, nil, ""
}

// TestSoDChainWrapSharesTokenStore 钉住 Wrap 的**核心不变量**：派生视图与原链
// 共享同一张一次性执行凭证表。
//
// 为什么这是生死线：链铸凭证、执行器消费凭证，两边必须是**同一张表** ——
// 凭证绑 (载荷摘要, task_id, source_agent, tool_name)，执行器用同一组四元组
// 查表。若派生视图各自持表，视图 A 铸的凭证在视图 B 的表里查不到 ⇒ 每一个
// 被批准的动作都被执行器拒（fail-closed，但链等于瘫了）。
//
// 而「共享」必须是**同一���指针**而不是「同一份内容的副本」：副本只能复制
// 已有凭证，铸出来的新凭证落不到对方表里。
func TestSoDChainWrapSharesTokenStore(t *testing.T) {
	store := NewTokenStore()
	base := NewChain(nil, ChainDeps{Tokens: store})
	view := base.Wrap(&fakeHookRunner{})

	if view == base {
		t.Fatal("Wrap 必须返回一个**新的**链视图，不能是原链本身")
	}
	if view.deps.Tokens != store {
		t.Fatal("派生视图必须与原链共享同一个 *TokenStore 指针")
	}
	if base.deps.Tokens != view.deps.Tokens {
		t.Fatalf("原链与视图的凭证表不是同一张: %p vs %p", base.deps.Tokens, view.deps.Tokens)
	}
}

// TestSoDChainWrapTokenIsConsumableAcrossViews 钉住「凭证在视图之间可传递」：
// 一个视图铸出的凭证，另一个视图（=执行器用的那张表）能查到并消费掉。
//
// 为什么单独钉：指针相同只是**形式**上的共享；真正的要求是「铸出来的凭证
// 真的能被消费」。
func TestSoDChainWrapTokenIsConsumableAcrossViews(t *testing.T) {
	store := NewTokenStore()
	base := NewChain(nil, ChainDeps{Tokens: store})
	view := base.Wrap(&fakeHookRunner{})

	digest := CanonicalDigest([]byte(`{"command":"ls"}`))
	if _, _, err := view.deps.Tokens.Mint(digest, 7, "worker", "Bash"); err != nil {
		t.Fatalf("铸凭证失败: %v", err)
	}
	if !base.deps.Tokens.Take(digest, 7, "worker", "Bash") {
		t.Fatal("视图铸的凭证必须能被原链那侧的凭证表消费掉")
	}
}

// TestSoDChainWrapNilInner 钉住「Wrap(nil) 得到一个 inner 为空的视图」。
//
// 为什么允许 inner 为空：宿主派生视图时可能这一轮没有 inner（例如链先装好、
// 后面才注入 guard）。此时视图必须等价于 NewChain(nil, deps) —— 链自身
// 独立工作，而不是 panic。
func TestSoDChainWrapNilInner(t *testing.T) {
	base := NewChain(&fakeHookRunner{}, ChainDeps{AgentKeyOf: AgentKeyFromContext})
	view := base.Wrap(nil)

	if view.inner != nil {
		t.Fatal("Wrap(nil) 必须得到 inner 为 nil 的视图")
	}
	if view.deps.AgentKeyOf == nil {
		t.Fatal("Wrap 必须共享同一份 deps")
	}
	if view.deps.AgentKeyOf == nil {
		t.Fatal("Wrap 必须共享同一份 deps")
	}
}

// TestSoDChainWrapSharesEveryDependency 钉住「deps 整体共享」：派生视图不能
// 复制出第二份闸门 / 规整器 / 缓存 / 审计器 —— 那等于把裁决拆成两套。
func TestSoDChainWrapSharesEveryDependency(t *testing.T) {
	store := NewTokenStore()
	cache := NewCache(8, "digest")
	deps := ChainDeps{
		Tokens:     store,
		Cache:      cache,
		AgentKeyOf: AgentKeyFromContext,
	}
	base := NewChain(nil, deps)
	view := base.Wrap(&fakeHookRunner{})

	if view.deps.Cache != cache {
		t.Fatal("派生视图必须共享同一个缓存指针")
	}
	if view.deps.Tokens != store {
		t.Fatal("派生视图必须共享同一个凭证表指针")
	}
	if view.deps.Gate != deps.Gate || view.deps.Auditor != deps.Auditor || view.deps.Assets != deps.Assets {
		t.Fatal("派生视图必须共享同一份依赖")
	}
}

// TestSoDAgentKeyFromContextMatchesRunContext 钉住「agent key 的单一真相源」：
// AgentKeyFromContext 必须与执行器查表时用的 currentRunContext(ctx).AgentKey
// **完全一致**。
//
// 为什么这是生死线：链用 agent key 铸凭证，执行器用 agent key 查表 ——
// 两边读法不一致 ⇒ 查不到凭证 ⇒ 每一个被批准的动作都被拒（fail-closed，
// 但链等于瘫了）。
func TestSoDAgentKeyFromContextMatchesRunContext(t *testing.T) {
	ctx := WithRunContext(context.Background(), RunContext{TaskID: 7, AgentKey: "worker"})

	got := AgentKeyFromContext(ctx)
	want := currentRunContext(ctx).AgentKey
	if got != want {
		t.Fatalf("AgentKeyFromContext=%q, 期望与运行上下文里的 %q 完全一致", got, want)
	}
	if got != "worker" {
		t.Fatalf("AgentKeyFromContext=%q, 期望 worker", got)
	}
}

// TestSoDAgentKeyFromContextFollowsHostReader 钉住「宿主绑定的读法优先」。
//
// 为什么：宿主可能用别的约定携带任务上下文（链与执行器都通过 SetContextReader
// 读同一份）—— 单一真相源指的是**那一条读法**，不是内置的 ctx 键。
func TestSoDAgentKeyFromContextFollowsHostReader(t *testing.T) {
	SetContextReader(func(ctx context.Context) RunContext {
		return RunContext{TaskID: 9, AgentKey: "host-agent"}
	})
	t.Cleanup(func() { SetContextReader(nil) })

	ctx := WithRunContext(context.Background(), RunContext{TaskID: 7, AgentKey: "worker"})
	if got := AgentKeyFromContext(ctx); got != "host-agent" {
		t.Fatalf("AgentKeyFromContext=%q, 期望宿主读法给出的 host-agent", got)
	}
}

// TestSoDChainAndExecutorAgreeOnAgentKey 钉住「链与执行器取到的 agent key
// 逐字节相同」。
//
// 为什么不能各修各的：链原来对 agent key 做 TrimSpace，执行器用原值 ——
// 一个带空白的 agent key 会让两端差一个空白 ⇒ 凭证查不到 ⇒ 全部被拒。
// 所以两端必须从**同一个原值**出发。
func TestSoDChainAndExecutorAgreeOnAgentKey(t *testing.T) {
	const agentKey = " worker "
	ctx := WithRunContext(context.Background(), RunContext{TaskID: 7, AgentKey: agentKey})

	// 链那侧：AgentKeyOf 给什么，链就用什么（不回退、不修剪）。
	chainKey := ChainDeps{AgentKeyOf: AgentKeyFromContext}.AgentKeyOf(ctx)
	if chainKey != agentKey {
		t.Fatalf("链侧取到的 agent key=%q, 期望原值 %q", chainKey, agentKey)
	}
	// 执行器那侧：同一个读法、同一个原值。
	if got := AgentKeyFromContext(ctx); got != agentKey {
		t.Fatalf("执行器侧取到的 agent key=%q, 期望原值 %q", got, agentKey)
	}
}

// 编译期断言：派生视图仍然是合法的 hook runner（宿主要把它当装饰器用）。
var _ harness.HookRunner = (*Chain)(nil)

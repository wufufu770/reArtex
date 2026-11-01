package sod

import "testing"

// 这个文件钉住「进程内唯一的执行器凭证表」这条装配纪律。
//
// 为什么要有这个单例：链铸凭证、执行器消费凭证，两边必须是**同一张表**。
// 链那一侧由宿主把表直接传进 ChainDeps.Tokens；执行器那一侧在 agent 包里
// （工具装配 AugmentTools 是共用代码，没有地方塞一个 TokenStore 参数）⇒
// 只能从进程级单例读。

// TestSoDExecutorTokensSingletonIsFirstWriteWins 钉住「首次写入生效，后续写入
// 被忽略」。
//
// 为什么必须是首次写入生效：凭证表被换掉意味着「链铸的凭证」与「执行器查的
// 表」不再是同一张 ⇒ 每一个被批准的动作都在执行器侧查不到凭证 ⇒ 全被拒
// （fail-closed，但链等于瘫了）。半装好的链绝不能出现两张互相不认的表。
func TestSoDExecutorTokensSingletonIsFirstWriteWins(t *testing.T) {
	ResetExecutorTokensForTest()
	t.Cleanup(ResetExecutorTokensForTest)

	first := NewTokenStore()
	second := NewTokenStore()

	SetExecutorTokens(first)
	if got := ExecutorTokens(); got != first {
		t.Fatalf("首次写入后读到 %p, 期望 %p", got, first)
	}

	SetExecutorTokens(second)
	if got := ExecutorTokens(); got != first {
		t.Fatalf("二次写入把凭证表换掉了: 读到 %p, 期望仍是 %p", got, first)
	}
}

// TestSoDExecutorTokensSingletonIgnoresNil 钉住「写 nil 不算数」。
//
// 为什么：nil 表示「没装配」。把 nil 写进单例等于把执行器指向一个空指针 ⇒
// 执行器在受管路径上拒绝一切，而宿主以为表已经装好了。
func TestSoDExecutorTokensSingletonIgnoresNil(t *testing.T) {
	ResetExecutorTokensForTest()
	t.Cleanup(ResetExecutorTokensForTest)

	SetExecutorTokens(nil)
	if got := ExecutorTokens(); got != nil {
		t.Fatalf("写入 nil 后读到了 %p, 期望 nil", got)
	}

	store := NewTokenStore()
	SetExecutorTokens(store)
	if got := ExecutorTokens(); got != store {
		t.Fatalf("读到 %p, 期望 %p", got, store)
	}
}

// TestSoDExecutorTokensSingletonResetForTest 钉住「复位钩子真的清干净」。
//
// 为什么需要复位：单例是进程级的，而同包用例共享同一个进程 ⇒ 不复位的话
// 「未装配」这个前置条件会随执行顺序漂移，用例之间互相依赖。
func TestSoDExecutorTokensSingletonResetForTest(t *testing.T) {
	ResetExecutorTokensForTest()
	SetExecutorTokens(NewTokenStore())
	if ExecutorTokens() == nil {
		t.Fatal("写入后应能读到凭证表")
	}
	ResetExecutorTokensForTest()
	if got := ExecutorTokens(); got != nil {
		t.Fatalf("复位后读到 %p, 期望 nil", got)
	}
}

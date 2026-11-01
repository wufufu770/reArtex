package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/internal/sod"
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

// resetSoDChainSlot 清掉进程级 SoD 链单例与执行器凭证表（仅本测试文件使用）。
//
// 为什么需要复位：链与执行器凭证表都是进程级单例，而同包用例共享同一个进程
// ⇒ 不复位的话「未安装 / 未装配」这个前置条件会随执行顺序漂移，用例之间互相
// 依赖。复位直接操作包内未导出变量与 internal/sod 提供的测试专用复位函数
// （同包 ⇒ 允许），不新增任何导出的生产 API。
func resetSoDChainSlot(t *testing.T) {
	t.Helper()
	reset := func() {
		sodChainSlot.mu.Lock()
		sodChainSlot.chain = nil
		sodChainSlot.mu.Unlock()
		sod.ResetExecutorTokensForTest()
	}
	reset()
	t.Cleanup(reset)
}

// installedSoDChainForTest 读当前装好的链（未安装 ⇒ nil）。
func installedSoDChainForTest() *sod.Chain {
	sodChainSlot.mu.Lock()
	defer sodChainSlot.mu.Unlock()
	return sodChainSlot.chain
}

// TestSoDRegisterSoDChainRejectsSecondInstall 钉住「只装一次」这条纪律。
//
// 为什么要钉：链持有一次性执行凭证表，且必须与执行器共用同一张表（链铸、
// 执行器消费）。覆盖式安装会让先装好的链与新链各铸各的凭证 ⇒ 执行器查表时
// 只在其中一张里找得到 ⇒ 已批准的动作被拒。所以第二次安装必须失败，且
// 已装好的链与已发布的凭证存储都不能被换掉。
func TestSoDRegisterSoDChainRejectsSecondInstall(t *testing.T) {
	resetSoDChainSlot(t)

	store := sod.NewTokenStore()
	deps := sod.ChainDeps{
		Tokens:     store,
		AgentKeyOf: func(ctx context.Context) string { return "worker" },
	}
	if err := RegisterSoDChain(deps); err != nil {
		t.Fatalf("首次安装应成功：%v", err)
	}
	if !SoDChainInstalled() {
		t.Fatal("安装后应报告已安装")
	}
	first := installedSoDChainForTest()
	if first == nil {
		t.Fatal("安装后应能取到链")
	}

	other := sod.NewTokenStore()
	if err := RegisterSoDChain(sod.ChainDeps{
		Tokens:     other,
		AgentKeyOf: deps.AgentKeyOf,
	}); err == nil {
		t.Fatal("重复安装应报错")
	}

	if got := installedSoDChainForTest(); got != first {
		t.Fatal("重复安装不得覆盖已装好的链")
	}
	// 凭证存储也必须保持原样：链铸、执行器消费，必须是同一张表。
	if got := sod.ExecutorTokens(); got != store {
		t.Fatal("重复安装不得替换执行器用的凭证存储")
	}
}

// TestSoDWrapHookRunnerIsNoopWhenChainNotInstalled 钉住「没装链 ⇒ 既有行为不变」。
//
// 为什么重要：SoD 链是可选增强。没装的时候宿主给的 runner 必须**原样**返回，
// 不能被包装成别的类型（否则未启用 SoD 的部署也会被链的行为影响）。
func TestSoDWrapHookRunnerIsNoopWhenChainNotInstalled(t *testing.T) {
	resetSoDChainSlot(t)
	if SoDChainInstalled() {
		t.Fatal("复位后不应报告已安装")
	}
	inner := &fakeHookRunner{}
	got := WrapHookRunner(inner)
	if got != harness.HookRunner(inner) {
		t.Fatalf("未安装时应原样返回同一个 runner，实际返回 %T", got)
	}
}

// TestSoDWrapHookRunnerDelegatesToInnerBeforeGating 钉住「先问 guard，再跑链」。
//
// 口径：链是装饰器 —— 它先问宿主给的 runner（guard 的短路表 / 拦截规则 /
// 人工 ask），只有 inner 不拦才跑规整 + 闸门。⇒ 链看到的入参是 inner 可能
// 改写后的最终字节，凭证绑定的字节 == 真正会执行的字节。
//
// 这里把 AgentKeyOf 装成非 nil（链**会** gate），且不给任务上下文 —— 若链
// 抢在 inner 前面 gate，它会按失败关闭拒绝并给出 [SoD] 前缀的消息。所以
// 「返回的是 inner 的消息」这一条就足以钉住顺序。
func TestSoDWrapHookRunnerDelegatesToInnerBeforeGating(t *testing.T) {
	resetSoDChainSlot(t)

	const blockMsg = "guard 拦截：命令超出授权范围"
	inner := &fakeHookRunner{block: true, message: blockMsg}

	if err := RegisterSoDChain(sod.ChainDeps{
		AgentKeyOf: func(ctx context.Context) string { return "worker" },
	}); err != nil {
		t.Fatalf("安装应成功：%v", err)
	}

	wrapped := WrapHookRunner(inner)
	block, msg, _ := wrapped.PreToolUse(context.Background(), "Bash", []byte(`{"command":"id"}`))
	if !block {
		t.Fatal("guard 拦截时应阻止执行")
	}
	if inner.preCalls != 1 {
		t.Fatalf("应当只问 inner 一次，实际 %d 次", inner.preCalls)
	}
	if msg != blockMsg {
		t.Fatalf("应当透传 guard 的拦截消息，实际 %q", msg)
	}
	if strings.Contains(msg, "[SoD]") {
		t.Fatalf("guard 已拦截时链不应参与裁决，实际消息 %q", msg)
	}
}

// TestSoDInstallSoDChainFailsClosedWithoutDB 钉住「缺必需件 ⇒ 中文错误 + 不装链」。
//
// fail-closed 的口径：装配不成就不装（宿主那条路径保持原样），而不是装一条
// 半成品链（半成品的链表现为「每次工具调用都被拒」或「链没接到受管路径上」）。
func TestSoDInstallSoDChainFailsClosedWithoutDB(t *testing.T) {
	resetSoDChainSlot(t)

	s := &Server{m: &Manager{}}
	err := s.installSoDChain()
	if err == nil {
		t.Fatal("数据库缺失时不应装配成功")
	}
	if !strings.Contains(err.Error(), "装配失败") {
		t.Fatalf("错误信息应为中文的装配失败提示，实际 %q", err.Error())
	}
	if SoDChainInstalled() {
		t.Fatal("装配失败时不得留下已安装的链")
	}
}

// TestSoDIntentToolSchemaFailsClosedWhenRowMissing 钉住「工具目录里没有
// emit_structured_intent ⇒ 取不到 schema」，不许拿空 schema 蒙混过关。
//
// 为什么这条要钉：规整器靠这个 schema 让模型**调用**工具产出结构化意图；
// schema 缺失 ⇒ 模型没有可调用的工具 ⇒ 规整必然不合规 ⇒ 每次调用都被拒。
// 所以必须装配期就报错，而不是带着空 schema 继续跑。
func TestSoDIntentToolSchemaFailsClosedWhenRowMissing(t *testing.T) {
	_, err := sodIntentToolSchema(nil)
	if err == nil {
		t.Fatal("工具目录为空时不应取到 schema")
	}
	if !strings.Contains(err.Error(), sodIntentToolKey) {
		t.Fatalf("错误信息应点名缺失的工具，实际 %q", err.Error())
	}
	if !strings.Contains(err.Error(), "装配失败") {
		t.Fatalf("错误信息应为中文的装配失败提示，实际 %q", err.Error())
	}

	// 有别的工具但没有那一行 ⇒ 同样失败（不许拿别的工具顶替）。
	_, err = sodIntentToolSchema([]*db.Tool{{Key: "Bash", Description: "跑命令"}})
	if err == nil {
		t.Fatal("没有 emit_structured_intent 时不应取到 schema")
	}

	// 那一行在、schema 合法 ⇒ 取得到，且 name/description 来自目录行。
	schema, err := sodIntentToolSchema([]*db.Tool{
		{Key: "Bash", Description: "跑命令"},
		{Key: sodIntentToolKey, Description: "产出结构化意图", Schema: json.RawMessage(`{"type":"object"}`)},
	})
	if err != nil {
		t.Fatalf("目录里有那一行时应取到 schema：%v", err)
	}
	if schema.Name != sodIntentToolKey {
		t.Fatalf("schema 名应取工具 key，实际 %q", schema.Name)
	}
	if schema.Description != "产出结构化意图" {
		t.Fatalf("schema 描述应取工具描述，实际 %q", schema.Description)
	}
	if schema.InputSchema["type"] != "object" {
		t.Fatalf("schema 入参应来自目录行的 schema 列，实际 %v", schema.InputSchema)
	}
}

// TestSoDInstallSoDChainFailsClosedWithoutIntentToolSchema 端到端钉住同一条
// fail-closed：工具目录里没有 emit_structured_intent ⇒ 装配失败且不留下链。
//
// 需要真库（读 tools 表），没有 postgres 时跳过。
func TestSoDInstallSoDChainFailsClosedWithoutIntentToolSchema(t *testing.T) {
	resetSoDChainSlot(t)

	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer m.Close()
	if _, err := m.pg.Exec(`DELETE FROM tools WHERE key=$1`, sodIntentToolKey); err != nil {
		t.Fatalf("删除工具行失败：%v", err)
	}

	s := New(testServerCtx(t), m, t.TempDir(), t.TempDir(), t.TempDir())
	err = s.installSoDChain()
	if err == nil {
		t.Fatal("工具目录缺少 emit_structured_intent 时不应装配成功")
	}
	if !strings.Contains(err.Error(), sodIntentToolKey) {
		t.Fatalf("错误信息应点名缺失的工具，实际 %q", err.Error())
	}
	if SoDChainInstalled() {
		t.Fatal("装配失败时不得留下已安装的链")
	}
}

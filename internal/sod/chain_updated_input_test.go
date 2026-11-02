package sod

import (
	"encoding/json"
	"testing"

	"github.com/Autumn-27/artex/internal/sodpolicy"
)

// 本文件钉住 SoD 链的一条生死线：**链算摘要用的必须是「inner 改写之后」的
// 那份字节**，而不是原始入参。
//
// 为什么这是生死线：宿主给的 inner（guard）可以改写入参 —— 比如把一条命令
// 规范化、补全参数、或拦下危险片段后改写。harness 拿到的是链返回的 updated，
// 执行器跑的是**那份 updated**；链若按**原始**入参算摘要，铸出的凭证绑的
// 就是一份没人会跑的字节 ⇒ 执行器永远查不到凭证 ⇒ 每一个被批准的动作都被
// 拒（fail-closed，但链等于瘫了）。
//
// 两种错法都要挡住：
//   - 按原始字节算摘要 ⇒ 执行器按改写后的字节查表 ⇒ 查不到 ⇒ 全被拒；
//   - 按改写后的字节算摘要但把 updated 吞掉不返回 ⇒ 工具跑的是原始字节 ⇒
//     执行器算出的摘要与凭证绑定的摘要不一致 ⇒ 同样被拒。
//
// 所以断言落在**执行器能不能消费掉这把凭证**上：那是最接近生产的检验。

// TestSoDChainUsesInnerUpdatedInput 钉住「链按 inner 改写后的字节算摘要」。
func TestSoDChainUsesInnerUpdatedInput(t *testing.T) {
	// inner 把入参改写成另一份字节（模拟 guard 规范化命令）。
	updated := []byte(`{"command":"curl https://sod-e2e.example.test/probe --insecure"}`)
	env := newSoDE2E(t, sodE2EOptions{scopeInScope: true, innerUpdated: updated})

	block, msg, got := env.chain.PreToolUse(env.ctx, "Bash", env.payload)
	if block {
		t.Fatalf("期望放行, 却被拒: %s", msg)
	}
	if string(got) != string(updated) {
		t.Fatalf("链返回的改写字节 = %q, 期望 %q", got, updated)
	}

	// 账本里的 payload_digest 必须绑【改写后】那份字节。
	decisions := env.sodDecisions()
	if len(decisions) != 1 {
		t.Fatalf("决策账本应有 1 条, 实际 %d 条", len(decisions))
	}
	rec := decisions[0]
	if rec.Verdict != sodpolicy.VerdictApprove {
		t.Fatalf("账本 verdict=%q, 期望 %q", rec.Verdict, sodpolicy.VerdictApprove)
	}
	if rec.PayloadDigest != CanonicalDigest(updated) {
		t.Fatalf("账本 payload_digest=%q, 期望改写后字节的摘要 %q", rec.PayloadDigest, CanonicalDigest(updated))
	}
	if rec.PayloadDigest == CanonicalDigest(env.payload) {
		t.Fatal("payload_digest 不能是原始入参的摘要（链必须按 inner 改写后的字节算）")
	}

	// 关键断言：执行器跑的是改写后的字节 ⇒ 必须查得到凭证、工具必须被调用。
	// 若链按原始字节铸凭证，这一步会被拒（摘要对不上）。
	exec := WrapExecutor(env.tool, env.tokens, true)
	res, err := exec.Call(env.ctx, json.RawMessage(updated), nil)
	if err != nil {
		t.Fatalf("执行出错: %v", err)
	}
	if res.IsError {
		t.Fatalf("被批准的动作被拒: %s", res.Flatten())
	}
	if env.tool.calls != 1 {
		t.Fatalf("工具被调用了 %d 次, 期望 1 次", env.tool.calls)
	}
}

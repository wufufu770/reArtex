package sod

// 桌面闸门分支（0.12.1 A4）与解包一致性的钉子。

import (
	"encoding/json"
	"testing"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/intercept"
	"github.com/Autumn-27/artex/internal/sodpolicy"
)

// 解包一致性：sod 的 deferredInnerName 与 guard 侧 intercept.UnwrapDeferredName
// 在同一载荷上必须产出同一内层名——两包各持一份实现（依赖纪律），行为漂移会在
// 这里被钉住（guard 与 sod 看到的工具身份不一致 = 治理与执行身份分裂）。
func TestDeferredInnerNameConsistentWithIntercept(t *testing.T) {
	payloads := []json.RawMessage{
		json.RawMessage(`{"tool_name":"mcp__agent-desktop__desktop_click","params":{"ref":"@s8f3k2p9:e8"}}`),
		json.RawMessage(`{"tool_name":"mcp__agent-desktop__desktop_snapshot"}`),
		json.RawMessage(`{"params":{}}`),
		json.RawMessage(`{broken`),
	}
	for _, p := range payloads {
		gotSod := deferredInnerName("ExecuteExtraTool", p)
		gotGuard, _ := intercept.UnwrapDeferredName("ExecuteExtraTool", p)
		if gotSod != gotGuard {
			t.Fatalf("解包不一致: sod=%q guard=%q (payload=%s)", gotSod, gotGuard, string(p))
		}
	}
	if deferredInnerName("Bash", []byte("{}")) != "Bash" {
		t.Fatal("非转发器调用被解包改写")
	}
}

// 闸门第五分支：HostLocalAction 置位时 scope 按构造通过（pass(host_local_action)），
// TargetHint 为空的桌面载荷不再触发「无可判定目标」拒绝。nil deps 下整体裁决仍是
// deny——但拒绝来自**后续环境检查**（tool_health fail-closed），不是范围授权：
// 这正是该分支要修的问题边界（授权与环境的失败必须可区分）。
func TestGateHostLocalActionBranch(t *testing.T) {
	g := NewGate(nil, nil)
	out := g.Evaluate(t.Context(), GateInput{
		ToolName:        "mcp__agent-desktop__desktop_click",
		TaskID:          7,
		HostLocalAction: true,
		Normalized:      NormalizerResult{}, // 桌面载荷无目标线索——正是该分支存在的理由
	})
	if out.Checks["scope_check"] != "pass(host_local_action)" {
		t.Fatalf("第五分支未按构造通过: %v", out)
	}
	if out.Verdict == sodpolicy.VerdictDeny && out.ReasonCode == sodpolicy.ReasonOutOfScope {
		t.Fatal("HostLocalAction 置位后被范围授权拒绝——分支失效")
	}
	// 无标志的桌面载荷（链侧未置位 = 授权集合空）仍走旧判定：TargetHint 空 ⇒
	// 以 ReasonOutOfScope 拒绝（与 host_local 分支的理由可区分）。
	out2 := NewGate(nil, nil).Evaluate(t.Context(), GateInput{
		ToolName:   "mcp__agent-desktop__desktop_click",
		TaskID:     7,
		Normalized: NormalizerResult{},
	})
	if out2.Checks["scope_check"] != "fail" || out2.ReasonCode != sodpolicy.ReasonOutOfScope {
		t.Fatalf("未置位标志的桌面动作应按旧口径以 out_of_scope 拒绝: %v", out2)
	}
}

// 工具名口径：sod 侧桌面识别必须用 db.DesktopMCPToolPrefix 的唯一语义源。
func TestDesktopToolNamePrefixSource(t *testing.T) {
	if db.DesktopMCPToolPrefix != "mcp__agent-desktop__desktop_" {
		t.Fatalf("前缀漂移: %q", db.DesktopMCPToolPrefix)
	}
	if !db.IsDesktopToolName("mcp__agent-desktop__desktop_click") {
		t.Fatal("桌面识别失败")
	}
	if db.IsDesktopToolName("mcp__agent-desktop__batch_other") ||
		db.IsDesktopToolName("mcp__scopesentry__list_projects") {
		t.Fatal("非桌面工具被误识别")
	}
}

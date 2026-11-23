package sod

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/internal/sodpolicy"
)

// TestDeferredTaskInternalWritePassesGate 钉住 0.15.52 闸门面：延迟形态
// （ExecuteExtraTool 外层）的任务内写必须在【闸门侧】也按解包内层名识别。
//
// 0.15.50 修了规整跳过面（effName 命中任务内表 ⇒ 不进 LLM 规整），但闸门
// 入参仍按外层名判 TaskInternalWrite ⇒ 空 normalized 的 TargetHint 落到
// 「无可判定目标」fail-closed——延迟形态的任务内写跳过了规整却死在闸门。
// 生产实锤：worker 经 ExecuteExtraTool 调 edge_consult（params 只有
// task_id）被拒 ×2、update_finding_report 靠载荷里恰好出现主机名才侥幸通过。
func TestDeferredTaskInternalWritePassesGate(t *testing.T) {
	env := newSoDE2E(t, sodE2EOptions{})
	payload := []byte(`{"tool_name": "record_cell", "params": {}}`)

	block, msg, _ := env.chain.PreToolUse(env.ctx, "ExecuteExtraTool", payload)
	if block {
		t.Fatalf("延迟形态任务内写不得被拒（解包内层 record_cell 在任务内表）: %s", msg)
	}

	decisions := env.sodDecisions()
	if len(decisions) != 1 {
		t.Fatalf("决策账本应有 1 条, 实际 %d 条", len(decisions))
	}
	rec := decisions[0]
	if rec.Verdict != sodpolicy.VerdictApprove {
		t.Fatalf("verdict=%q, 期望 %q", rec.Verdict, sodpolicy.VerdictApprove)
	}
	var checks map[string]string
	if err := json.Unmarshal(rec.Checks, &checks); err != nil {
		t.Fatalf("解析 checks 失败: %v", err)
	}
	if checks["scope_check"] != "pass(task_internal_write)" {
		t.Fatalf("scope_check=%q, 期望 pass(task_internal_write), checks=%v", checks["scope_check"], checks)
	}
	// §4 不变式 4：放行旁路必须带独立审计标记。
	if checks["bypassed_normalizer"] != "true" {
		t.Fatalf("延迟形态任务内写放行必须带 bypassed_normalizer 标记, checks=%v", checks)
	}
}

// TestDeferredHostTargetStillGated 是反向钉：解包豁免只对任务内表生效——
// 延迟形态的扩权/主机目标内层照旧进规整过 scope，不因外层是 ExecuteExtraTool
// 就放行（否则延迟转发就成了 scope 旁路）。
func TestDeferredHostTargetStillGated(t *testing.T) {
	env := newSoDE2E(t, sodE2EOptions{scopeInScope: false})
	payload := []byte(`{"tool_name": "insert_assets", "params": {}}`)

	block, msg, _ := env.chain.PreToolUse(env.ctx, "ExecuteExtraTool", payload)
	if !block {
		t.Fatal("延迟形态的扩权工具不得命中任务内表（应照旧过 scope）")
	}
	if !strings.Contains(msg, sodpolicy.ReasonOutOfScope) {
		t.Fatalf("拒绝消息 %q 应带原因码 %q", msg, sodpolicy.ReasonOutOfScope)
	}
}

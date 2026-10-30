package db

import (
	"encoding/json"
	"testing"
)

// TestIntentNormalizerBuiltinAgent 保证 intent-normalizer 作为内置 agent 落库。
//
// role 必须是 assistant：它是会话型角色（与 curator/verifier/redteam/creative
// 同类），前端对话页按 role='assistant' 挑可对话的内置 agent。
//
// interactive_shell 必须为 false：规整器只把原始动作转成结构化意图，不动手。
// 给它终端等于让它绕开确定性闸门直接执行——那正是职责分离要防的事。
func TestIntentNormalizerBuiltinAgent(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()

	var role string
	var interactive bool
	if err := d.QueryRow(`SELECT role, interactive_shell FROM agents WHERE key=$1`, "intent_normalizer").Scan(&role, &interactive); err != nil {
		t.Fatalf("读取内置 agent intent-normalizer 失败: %v", err)
	}
	if role != "assistant" {
		t.Errorf("agent intent-normalizer 的 role=%q，应为 assistant", role)
	}
	if interactive {
		t.Error("agent intent-normalizer 不该有交互式 shell：它只转换，不动手")
	}
}

// TestDeterministicInfraHasNoAgentRow 保证 policy-gate / executor / auditor 没有
// agent 行。
//
// 这三个是确定性代码：policy-gate 的裁决（scope、tier2 一致性、工具熔断、额度
// 状态、anomaly 标记）全是确定性检查，裁决顺序是短路规则表；executor 与 auditor
// 同理。它们不读提示词、不由模型驱动，所以既没有提示词文件，也没有 agent 行，
// 更没有工具绑定。给它们建 agent 行，等于让模型接手本该由代码裁决的环节。
func TestDeterministicInfraHasNoAgentRow(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()

	for _, key := range []string{"policy-gate", "policy_gate", "intent-normalizer", "executor", "auditor"} {
		a, err := d.GetAgentByKey(key)
		if err != nil {
			t.Fatalf("读取 agent %s 失败: %v", key, err)
		}
		if a != nil {
			t.Errorf("%s 不该有 agent 行：它是确定性代码，不是 LLM agent", key)
		}
	}
	// 内置 agent 目录里同样不能有它们——否则下次启动会把行种回来。
	for _, a := range builtinAgents {
		switch a.key {
		case "policy-gate", "executor", "auditor":
			t.Errorf("内置 agent 目录里不该有 %s", a.key)
		}
	}
}

// TestIntentNormalizerToolBinding 保证 emit_structured_intent 一旦在 tools 表
// 出现就绑给 intent-normalizer，且不被别的顾问 agent 顺手绑走。
//
// 两种情形都要覆盖：工具在表里（绑上）与工具不在表里（跳过并记录，不报错）。
// 「不在表里」是当前的常态——它的 handler 属于执行侧接线，落地前它只是白名单
// 里的一个名字。
func TestIntentNormalizerToolBinding(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()

	if _, err := d.Exec(`DELETE FROM tools WHERE key=$1`, "emit_structured_intent"); err != nil {
		t.Fatalf("清理工具行失败: %v", err)
	}

	// 情形一：工具尚未注册 → 协调跳过，不报错，也不产生绑定。
	if err := d.seedAdvisoryAgentToolBindings(); err != nil {
		t.Fatalf("协调工具绑定失败: %v", err)
	}
	if row, err := d.GetTool("emit_structured_intent"); err == nil && row != nil {
		for _, a := range row.Agents {
			if a == "intent_normalizer" {
				t.Error("工具尚未注册时不该产生绑定")
			}
		}
	}

	// 情形二：工具注册 → 绑给 intent-normalizer，且只绑给它。
	agents, _ := json.Marshal([]string{"intent_normalizer"})
	if err := d.SeedTool("emit_structured_intent", "输出结构化意图", nil, agents); err != nil {
		t.Fatalf("播种工具失败: %v", err)
	}
	if err := d.seedAdvisoryAgentToolBindings(); err != nil {
		t.Fatalf("协调工具绑定失败: %v", err)
	}
	row, err := d.GetTool("emit_structured_intent")
	if err != nil {
		t.Fatalf("读取工具失败: %v", err)
	}
	if row == nil {
		t.Fatal("工具 emit_structured_intent 不存在")
	}
	found := false
	for _, a := range row.Agents {
		if a == "intent_normalizer" {
			found = true
		}
		if a == "curator" || a == "verifier" || a == "redteam" || a == "creative" {
			t.Errorf("工具 emit_structured_intent 不该绑给 %s", a)
		}
	}
	if !found {
		t.Errorf("工具 emit_structured_intent 没绑给 intent-normalizer（实际 %v）", row.Agents)
	}
}

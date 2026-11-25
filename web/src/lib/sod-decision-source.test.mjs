import assert from "node:assert/strict";
import test from "node:test";

import { sodDecisionSourceOf } from "./status.ts";

// A-03B：决策来源分档推导的语义钉子——映射必须与后端真实值域逐条对齐
// （internal/sodpolicy.Reason* 常量 + auditor_gate.go 的 auditor_<verdict> 输出
// + 链的 ask/override 面）。查不出的一律 unknown，不伪造来源。

test("auditor LLM 裁决输出归 AI 裁决", () => {
  assert.equal(sodDecisionSourceOf("auditor_pass"), "auditor");
  assert.equal(sodDecisionSourceOf("auditor_deny"), "auditor");
  assert.equal(sodDecisionSourceOf("auditor_ask"), "auditor");
});

test("auditor 不可用是确定性门的 fail-closed，不算 AI 裁决", () => {
  assert.equal(sodDecisionSourceOf("auditor_unavailable"), "deterministic");
});

test("人工 ask 拒绝归人工裁决", () => {
  assert.equal(sodDecisionSourceOf("ask_declined"), "human");
});

test("人工改判标记最优先——即使 reason_code 是全检查通过", () => {
  assert.equal(sodDecisionSourceOf("all_checks_passed", "override #3 (scope=once, by=sir)"), "human");
  // 改判标记优先于一切 reason_code，包括 AI 裁决码（防御：链不会产出这种组合，
  // 但推导面宁可把票投给改判，也不能让人工放行被 AI 裁决标签盖住）。
  assert.equal(sodDecisionSourceOf("auditor_pass", "override #1"), "human");
});

test("确定性门的代码常量判定全部归确定性门", () => {
  for (const rc of [
    "scope_ok",
    "all_checks_passed",
    "out_of_scope",
    "scope_check_unavailable",
    "tool_circuit_open",
    "tool_health_unavailable",
    "credential_circuit_open",
    "quota_exhausted",
    "quota_check_unavailable",
    "tier2_unavailable",
    "tier2_inconsistent",
    "normalizer_unavailable",
    "normalizer_malformed",
    "execution_token_invalid",
    "destructive_action",
    "platform_baseline",
    "category_deny",
    "category_cap_exceeded",
    "anomaly_flagged",
  ]) {
    assert.equal(sodDecisionSourceOf(rc), "deterministic", rc);
  }
});

test("来源缺失归未知，不伪造", () => {
  assert.equal(sodDecisionSourceOf(""), "unknown");
  assert.equal(sodDecisionSourceOf("   "), "unknown");
});

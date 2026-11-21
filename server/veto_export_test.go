package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// Veto exporter 单测（0.15.32）：reartex_pentest_v1 六字段的数据源映射钉死。
func TestVetoResponseForTask(t *testing.T) {
	ts := newVetoExportFixture(t)
	defer ts.cleanup()

	// 初始：无 finding/无账本行 → 全零 + 链有效。
	resp, err := ts.s.VetoResponseForTask(ts.taskIDInt)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(resp["confirmed_vuln_classes"].([]string)); got != 0 {
		t.Fatalf("空任务 confirmed 类别应为空，得到 %d", got)
	}
	if resp["retest_confirmed_count"].(int) != 0 || resp["guard_violations"].(int) != 0 {
		t.Fatalf("空任务计数应为 0: %v", resp)
	}
	if !resp["sod_chain_valid"].(bool) {
		t.Fatal("空账本 sod_chain_valid 应为 true")
	}
	if resp["tool_calls"].(int) != 0 || resp["total_tokens"].(int64) != 0 {
		t.Fatalf("空任务 tool/token 应为 0: %v", resp)
	}

	// 落一个 confirmed finding + reproduced 复测 + 一条 deny 账本行 + 一条链故障行。
	ts.seedFinding("idor", "confirmed")
	ts.seedFinding("sqli", "pending") // pending 不计入 confirmed
	ts.seedRetestReproduced()
	// verdict 枚举 = approve/deny/needs_review（schema_sod.sql CHECK）。
	ts.seedSoDDecision("deny", "scope_out")
	ts.seedSoDDecision("deny", "category_deny")
	ts.seedSoDDecision("approve", "all_checks_passed")
	ts.seedSoDDecision("deny", "scope_unavailable") // 链级故障 → chain_valid=false

	resp, err = ts.s.VetoResponseForTask(ts.taskIDInt)
	if err != nil {
		t.Fatal(err)
	}
	classes := resp["confirmed_vuln_classes"].([]string)
	if len(classes) != 1 || classes[0] != "idor" {
		t.Fatalf("confirmed 类别应只含 idor: %v", classes)
	}
	if resp["retest_confirmed_count"].(int) != 1 {
		t.Fatalf("复测确认数应为 1: %v", resp)
	}
	if resp["guard_violations"].(int) != 3 {
		t.Fatalf("guard_violations 应为 3（三条 deny）: %v", resp)
	}
	if resp["sod_chain_valid"].(bool) {
		t.Fatal("存在链级故障原因码时 sod_chain_valid 应为 false")
	}
}

// TestExportVetoResponsesFile 落盘面：responses.json 可被 veto CLI 直接消费。
func TestExportVetoResponsesFile(t *testing.T) {
	ts := newVetoExportFixture(t)
	defer ts.cleanup()

	ts.seedFinding("idor", "confirmed")
	ts.s.ExportVetoResponses(ts.task())

	dir := filepath.Join(ts.s.dataDir, "veto", ts.task().ID)
	body, err := os.ReadFile(filepath.Join(dir, "responses.json"))
	if err != nil {
		t.Fatalf("responses.json 未落盘: %v", err)
	}
	var entries []struct {
		ID       string         `json:"id"`
		Response map[string]any `json:"response"`
	}
	if err := json.Unmarshal(body, &entries); err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].ID != ts.task().ID {
		t.Fatalf("responses 形态不对: %s", body)
	}
	if _, ok := entries[0].Response["sod_chain_valid"]; !ok {
		t.Fatal("response 缺少 sod_chain_valid")
	}
	// identity.json 也要在（score 需要 identity）。
	if _, err := os.Stat(filepath.Join(dir, "identity.json")); err != nil {
		t.Fatalf("identity.json 未落盘: %v", err)
	}
	_ = strconv.Itoa // 保持 import（fixture 内部使用 strconv）
}

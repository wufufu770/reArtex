package server

// 方案 v3 缺陷 B4：降级档的服务器侧装配单测——开关读写与分级器注入。

import (
	"testing"
)

func TestSodDegradeSettingRoundTrip(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v)", err)
	}
	defer m.Close()
	s := &Server{m: m, ctx: t.Context()}

	if s.sodDegradeEnabled() {
		t.Fatal("默认必须关闭（fail-closed）")
	}
	if err := m.pg.SetBool(settingSodDegradeEnabled, true); err != nil {
		t.Fatalf("写开关: %v", err)
	}
	if !s.sodDegradeEnabled() {
		t.Fatal("开启后应读到 true")
	}
}

func TestSodDegradedAllowClassifier(t *testing.T) {
	// Tier2 结构化写类放行；Tier3（出网/扩权/状态迁移）与普通工具一律不放行。
	for _, tool := range []string{"report_finding", "add_intent", "propose_intent", "add_hint", "message_group"} {
		if !isSodDegradableTool(tool) {
			t.Errorf("%q 应属于降级可放行类", tool)
		}
	}
	for _, tool := range []string{"Bash", "WebFetch", "web_search", "add_task_scope",
		"add_company_scope", "insert_assets", "prove_goal", "kill_work", "goal_met", "Task", "shell_open"} {
		if isSodDegradableTool(tool) {
			t.Errorf("%q 不得在降级期放行（Tier3/未分类）", tool)
		}
	}
}

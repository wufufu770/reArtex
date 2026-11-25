package server

// 方案 v3 缺陷 B4 + A-06：降级档开关的服务器侧装配单测。
//
// A-06 之后开关是**启动期配置**（ARTEX_SOD_DEGRADE_ENABLED，装配时读一次进
// ChainDeps 快照），settings 键 sod.degrade_enabled 废弃为只读展示位——
// 这里的用例钉住三件事：环境变量解析口径、写接口拒绝、分级器注入不变。

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSodDegradeBootEnvParsing(t *testing.T) {
	// 仅接受 1/true；其余（含一切模糊值）一律 false——安全开关不给
	// 「2/yes/on 都算开」的解释空间。
	cases := map[string]bool{
		"":      false,
		"1":     true,
		"true":  true,
		"TRUE":  true,
		"True":  true,
		"0":     false,
		"false": false,
		"FALSE": false,
		"yes":   false,
		"on":    false,
		"2":     false,
		" 1 ":   true, // 值外围空白剥离后再判定
	}
	for in, want := range cases {
		t.Setenv(envSodDegradeEnabled, in)
		if got := sodDegradeEnabledAtBoot(); got != want {
			t.Errorf("%s=%q 应解析为 %v，得到 %v", envSodDegradeEnabled, in, want, got)
		}
	}
	t.Setenv(envSodDegradeEnabled, "")
	if sodDegradeEnabledAtBoot() {
		t.Fatal("未设置环境变量时默认必须关闭（fail-closed）")
	}
}

func TestSodDegradeSettingWriteRejected(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v)", err)
	}
	defer m.Close()
	s := &Server{m: m, ctx: t.Context()}

	// 写接口必须拒绝并点名迁移方式——静默忽略会让前端以为改成功、
	// 运维以为关掉了降级，实际什么都没变。
	req := httptest.NewRequest("PUT", "/api/settings", strings.NewReader(`{"sod_degrade_enabled":true}`))
	rec := httptest.NewRecorder()
	s.putSettings(rec, req)
	if rec.Code != 400 {
		t.Fatalf("写入已废弃的降级开关应返回 400，得到 %d（%s）", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), envSodDegradeEnabled) {
		t.Errorf("拒绝信息应点名启动期环境变量 %s: %s", envSodDegradeEnabled, rec.Body.String())
	}
	// 拒绝路径不得落库（settings 里不应出现这个键的值）。
	if v, _, _ := m.pg.GetSetting(settingSodDegradeEnabled); v != "" {
		t.Errorf("拒绝路径不得落库，settings 里出现了 %q", v)
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

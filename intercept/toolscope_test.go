package intercept

// A-10 tool_scope 测试（0.15.76）：scope 收窄不误伤、空 scope 存量语义不变、
// 组前缀约定、db 往返（TEXT[] 读写）、scope 含未注册工具名不报错。

import (
	"context"
	"regexp"
	"testing"

	"github.com/Autumn-27/artex/db"
)

func scopeRule(t *testing.T, pattern string, scope []string) compiledRule {
	t.Helper()
	c := compiledRule{InterceptRule: db.InterceptRule{
		Name: "scope-rule", MatchTarget: "tool_input", MatchType: "regex",
		Pattern: pattern, ToolScope: scope,
	}}
	c.re = regexp.MustCompile(pattern)
	return c
}

// scope=["Bash"] 的规则拦 Bash、不拦 Read（tool_name 维度同理）。
func TestToolScopeNarrowsMatching(t *testing.T) {
	r := scopeRule(t, `rm\s+-rf`, []string{"Bash"})
	raw := []byte(`{"command":"rm -rf /tmp"}`)
	norm := NormalizeToolInput(raw)

	if !ruleMatches(r, "Bash", raw, norm) {
		t.Fatal("scope 内工具应照常命中")
	}
	if ruleMatches(r, "Read", raw, norm) {
		t.Fatal("scope 外工具不应被命中（不误伤只读工具）")
	}
}

// 空 scope = 全局：存量规则行为不变。
func TestToolScopeEmptyMeansGlobal(t *testing.T) {
	r := scopeRule(t, `rm\s+-rf`, nil)
	raw := []byte(`{"command":"rm -rf /tmp"}`)
	norm := NormalizeToolInput(raw)
	for _, tool := range []string{"Bash", "Read", "some_mcp_tool"} {
		if !ruleMatches(r, tool, raw, norm) {
			t.Fatalf("空 scope 应保持全局语义（%s）", tool)
		}
	}
}

// 组前缀约定："shell:*" 命中 "shell_exec"。
func TestToolScopeGroupPrefix(t *testing.T) {
	if !toolInScope("shell_exec", []string{"shell:*"}) {
		t.Fatal("组前缀应命中同前缀工具")
	}
	if !toolInScope("Bash", []string{"Bash", "Read:*"}) {
		t.Fatal("精确条目应命中")
	}
	if toolInScope("Read", []string{"shell:*"}) {
		t.Fatal("不同前缀不应命中")
	}
	if toolInScope("shellx", []string{"shell:*"}) {
		t.Fatal("前缀必须到冒号边界：shellx 不应命中 shell:*")
	}
}

// db 往返：TEXT[] 写入读出一致；nil 归一为空数组（全局）；去空串去重。
func TestToolScopeDBRoundTrip(t *testing.T) {
	d := openTestDB(t)

	r1, err := d.CreateInterceptRule("scope-rt", "tool_input", "regex", `rm\s+-rf`, "deny", "高危", 300, true, false, 0, "deny", []string{"Bash", "shell_exec"})
	if err != nil {
		t.Fatalf("带 scope 建规则失败: %v", err)
	}
	t.Cleanup(func() { _ = d.DeleteInterceptRule(r1.ID) })
	if len(r1.ToolScope) != 2 || r1.ToolScope[0] != "Bash" {
		t.Fatalf("创建返回的 scope 应归一化，得到 %v", r1.ToolScope)
	}
	got, err := d.ListInterceptRules()
	if err != nil {
		t.Fatal(err)
	}
	var found *db.InterceptRule
	for i := range got {
		if got[i].ID == r1.ID {
			found = &got[i]
		}
	}
	if found == nil || len(found.ToolScope) != 2 || found.ToolScope[1] != "shell_exec" {
		t.Fatalf("读回的 scope 应与写入一致，得到 %+v", found)
	}

	// 更新收窄 scope。
	r2, err := d.UpdateInterceptRule(r1.ID, "scope-rt", "tool_input", "regex", `rm\s+-rf`, "deny", "高危", 300, true, false, 0, "deny", []string{"Bash"})
	if err != nil {
		t.Fatalf("更新 scope 失败: %v", err)
	}
	if len(r2.ToolScope) != 1 || r2.ToolScope[0] != "Bash" {
		t.Fatalf("收窄后的 scope 应只有 Bash，得到 %v", r2.ToolScope)
	}

	// 清空 scope = 回到全局；nil 与含空串/重复条目都归一。
	r3, err := d.UpdateInterceptRule(r1.ID, "scope-rt", "tool_input", "regex", `rm\s+-rf`, "deny", "高危", 300, true, false, 0, "deny", []string{"", "Bash", "Bash", "  "})
	if err != nil {
		t.Fatalf("清空 scope 失败: %v", err)
	}
	if len(r3.ToolScope) != 1 || r3.ToolScope[0] != "Bash" {
		t.Fatalf("空串与重复应剔除，得到 %v", r3.ToolScope)
	}

	r4, err := d.UpdateInterceptRule(r1.ID, "scope-rt", "tool_input", "regex", `rm\s+-rf`, "deny", "高危", 300, true, false, 0, "deny", nil)
	if err != nil {
		t.Fatalf("置空 scope 失败: %v", err)
	}
	if len(r4.ToolScope) != 0 {
		t.Fatalf("nil scope 应落为空数组（全局），得到 %v", r4.ToolScope)
	}
}

// 端到端：scope 规则经 Interceptor.Match 只对 scope 内工具生效。
func TestToolScopeEndToEndMatch(t *testing.T) {
	d := openTestDB(t)
	r, err := d.CreateInterceptRule("scope-e2e", "tool_input", "regex", `secret[_-]?scan`, "deny", "scope 测试", 500, true, false, 0, "deny", []string{"Bash"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = d.DeleteInterceptRule(r.ID)
	})
	ic := New(d)
	ic.Invalidate()

	input := []byte(`{"command":"secret_scan --all"}`)
	if _, hit := ic.Match("Bash", input); !hit {
		t.Fatal("scope 内工具应命中规则")
	}
	if _, hit := ic.Match("Read", input); hit {
		t.Fatal("scope 外工具不应命中规则")
	}

	// scope 含未注册工具名（如暂未注册的 MCP 工具）：加载/匹配不报错。
	r2, err := d.CreateInterceptRule("scope-future", "tool_input", "regex", `zzz_unregistered`, "deny", "scope 测试", 499, true, false, 0, "deny", []string{"not_registered_yet"})
	if err != nil {
		t.Fatalf("scope 含未注册工具名建规则不应报错: %v", err)
	}
	t.Cleanup(func() { _ = d.DeleteInterceptRule(r2.ID) })
	ic.Invalidate()
	if _, hit := ic.Match("Bash", []byte(`{"command":"zzz_unregistered"}`)); hit {
		t.Fatal("scope 外（未注册工具）不应命中")
	}
	_ = context.Background()
}

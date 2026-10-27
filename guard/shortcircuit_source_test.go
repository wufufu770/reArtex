package guard

import (
	"strings"
	"testing"
)

// 短路表判定要落进 intercept_pending，db 层靠 RuleName 的 "[代码常量]" 前缀
// 识别来源（db.interceptSource）。这条前缀是跨包约定：改了就静默失效 ——
// 记录照样入库，但来源标签会落成「未知」，看不出是哪条内置防线拦的。
//
// 所以每条短路表规则都必须带这个前缀。
func TestShortCircuitRuleNamesCarrySourcePrefix(t *testing.T) {
	const prefix = "[代码常量]"

	cases := []struct {
		desc string
		tool string
		in   string
	}{
		{"MCP 注册", "create_mcp", `{"name":"x","transport":"http","url":"http://169.254.169.254/"}`},
		{"平台工具需审批", "create_custom_tool", `{"command":"http://127.0.0.1"}`},
		{"平台凭据", "Bash", `{"command":"cat /app/jwt.key"}`},
		{"进程环境块", "Bash", `{"command":"cat /proc/1/environ"}`},
	}
	for _, c := range cases {
		dec := shortCircuit(c.tool, []byte(c.in))
		if dec == nil {
			t.Fatalf("%s：应命中短路表", c.desc)
		}
		if !strings.HasPrefix(dec.RuleName, prefix) {
			t.Fatalf("%s：RuleName %q 必须以 %q 开头（db 层靠它识别来源）", c.desc, dec.RuleName, prefix)
		}
	}
}

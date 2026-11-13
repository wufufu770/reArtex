package intercept

import (
	"encoding/json"
	"testing"
)

// 延迟工具解包的安全钉（0.11.9 §四-G）——三条路径钉死，防回归：
//   ① 转发载荷取内层名与参数（治理看到的 = 实际执行的）；
//   ② 非转发器调用原样返回（直连工具行为不变）；
//   ③ 畸形/空名载荷回退外层名（执行侧会拒绝同一载荷，无治理旁路窗口）。
func TestUnwrapDeferredName(t *testing.T) {
	// ① 转发载荷解包。
	outer := json.RawMessage(`{"tool_name":"mcp__agent-desktop__desktop_click","params":{"ref":"@s8f3k2p9:e8"}}`)
	name, input := UnwrapDeferredName("ExecuteExtraTool", outer)
	if name != "mcp__agent-desktop__desktop_click" {
		t.Fatalf("内层名未解出: %q", name)
	}
	var params map[string]any
	if err := json.Unmarshal(input, &params); err != nil || params["ref"] != "@s8f3k2p9:e8" {
		t.Fatalf("内层参数未解出: %s", string(input))
	}

	// ② 非转发器调用原样返回。
	direct := json.RawMessage(`{"cmd":"ls"}`)
	n2, i2 := UnwrapDeferredName("Bash", direct)
	if n2 != "Bash" || string(i2) != `{"cmd":"ls"}` {
		t.Fatalf("直连工具被误改: %q %s", n2, string(i2))
	}

	// ③a 空名回退外层。
	n3, i3 := UnwrapDeferredName("ExecuteExtraTool", json.RawMessage(`{"params":{}}`))
	if n3 != "ExecuteExtraTool" {
		t.Fatalf("空名载荷应回退外层名: %q", n3)
	}
	_ = i3
	// ③b 畸形 JSON 回退外层。
	n4, _ := UnwrapDeferredName("ExecuteExtraTool", json.RawMessage(`{broken`))
	if n4 != "ExecuteExtraTool" {
		t.Fatalf("畸形载荷应回退外层名: %q", n4)
	}
	// ③c 空参数补 {}（规则按 tool_input 匹配时是合法 JSON）。
	n5, i5 := UnwrapDeferredName("ExecuteExtraTool", json.RawMessage(`{"tool_name":"mcp__x__y"}`))
	if n5 != "mcp__x__y" || string(i5) != "{}" {
		t.Fatalf("缺 params 应补空对象: %q %s", n5, string(i5))
	}
}

package intercept

import "testing"

// 判官不可用时的回退动作是这套防线里唯一没有第二道保险的开关：
// 短路表在进程内、MCP 校验在写库侧，而判官失败直接决定「这次调用放不放行」。
// 默认必须是 deny —— 回落 allow 等于「让判官失效就能绕过全部规则」。
func TestJudgeFailActionDefaultsToDeny(t *testing.T) {
	for _, in := range []string{"", "   ", "bogus", "DENY", "Allow", "denied"} {
		if got := judgeFailAction(in); got != "deny" {
			t.Fatalf("输入 %q 应回落为 deny，得到 %q", in, got)
		}
	}
}

// 管理员显式配置仍然生效 —— fail-closed 是默认，不是把放宽的口子焊死。
func TestJudgeFailActionHonoursExplicitSetting(t *testing.T) {
	for in, want := range map[string]string{
		"deny":  "deny",
		"ask":   "ask",
		"allow": "allow",
		" ask ": "ask",
	} {
		if got := judgeFailAction(in); got != want {
			t.Fatalf("输入 %q 应得 %q，得到 %q", in, want, got)
		}
	}
}

// 默认常量本身也要钉住：有人把它改回 "allow" 时这条会立刻炸。
func TestDefaultJudgeFailActionIsDeny(t *testing.T) {
	if defaultJudgeFailAction != "deny" {
		t.Fatalf("判官失败默认动作必须是 deny，得到 %q", defaultJudgeFailAction)
	}
}

// ask 超时同样不得放行：人工审批一直不确认时，超时即拒绝，不能默认放行。
func TestDefaultJudgeAskTimeoutActionIsDeny(t *testing.T) {
	if defaultJudgeAskTimeoutAction != "deny" {
		t.Fatalf("ask 超时默认动作必须是 deny，得到 %q", defaultJudgeAskTimeoutAction)
	}
}

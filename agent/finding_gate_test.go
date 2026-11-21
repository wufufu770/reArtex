package agent

import (
	"os"
	"strings"
	"testing"
)

// finding_gate 预检单测：确定性规则面钉死（词表/正则改一处这里就红）。
func TestCheckFindingGateRejectsEmptyEvidence(t *testing.T) {
	for _, ev := range []string{"", "   ", "\n\t"} {
		if err := CheckFindingGate("SQL 注入可读取他人数据", ev); err == nil {
			t.Fatalf("空证据应被拒绝（evidence=%q）", ev)
		}
	}
}

func TestCheckFindingGateRejectsSelfRefutingSummary(t *testing.T) {
	// R2 自否 + R3 自认未验证，summary 命中即拒——无论 evidence 多扎实。
	for _, s := range []string{
		"这不是一个漏洞，只是配置提示",
		"疑似 XSS（false positive 排除后未确认）",
		"注入面存在（未验证）",
		"该参数疑似可注入，待验证",
		"IDOR 候选（存疑）",
		"无法复现原始 PoC",
	} {
		if err := CheckFindingGate(s, "GET /api?id=1 HTTP/1.1\n200 OK ..."); err == nil {
			t.Fatalf("自否/未验证 summary 应被拒绝：%q", s)
		}
	}
}

func TestCheckFindingGateAllowsLegitFindings(t *testing.T) {
	// 合法上报零误伤：已验证结论 + 扎实证据；evidence 引用上下文里的自否词不误伤。
	if err := CheckFindingGate("SQL 注入可读取他人敏感数据（已独立复现）",
		"GET /api/user?id=1' OR '1'='1 HTTP/1.1\n200 OK，返回他人数据\n复测第 2 次独立触发成功"); err != nil {
		t.Fatalf("合法上报不应被拒: %v", err)
	}
	// evidence 含「目标回复不存在漏洞」等上下文引用不误伤（R2/R3 不查 evidence）。
	if err := CheckFindingGate("未授权访问可导出用户列表",
		"响应与 WAF 回复：目标声称未发现漏洞（false positive 说法来自目标方），实际已触发"); err != nil {
		t.Fatalf("evidence 引用上下文自否词不应误伤: %v", err)
	}
	// TCP 类非 HTTP 漏洞（无流量绑定）合法通过——XIB「缺 HTTP 标记即拒」不移植的验证。
	if err := CheckFindingGate("Redis 未授权访问可写计划任务",
		"redis-cli -h x PING 返回 PONG，CONFIG SET dir 写入成功，命令输出已存"); err != nil {
		t.Fatalf("TCP 类合法上报不应被拒: %v", err)
	}
	// 小程序反编译凭据泄露合法通过——"decompiled" 字面拦截不移植的验证。
	if err := CheckFindingGate("小程序 appsecret 硬编码泄露",
		"wxapkg 反编译产物 app.js 第 42 行发现 appsecret，使用该 key 调用 API 签名成功"); err != nil {
		t.Fatalf("反编译产物凭据泄露不应被拒: %v", err)
	}
}

func TestFindingGateWiredIntoAddFinding(t *testing.T) {
	// 钉死接线：addFinding 的 Run 里必须调用 CheckFindingGate（删接线此测试红）。
	src, err := os.ReadFile("tools.go")
	if err != nil {
		t.Fatalf("读源码失败: %v", err)
	}
	if !strings.Contains(string(src), "CheckFindingGate(a.Summary, a.Evidence)") {
		t.Fatal("addFinding 必须调用 CheckFindingGate(a.Summary, a.Evidence)——接线被移除")
	}
}

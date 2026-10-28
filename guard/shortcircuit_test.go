package guard

import "testing"

func TestShortCircuitPlatformToolsAsk(t *testing.T) {
	for _, tool := range []string{
		"create_custom_tool", "update_custom_tool",
		"update_skill_file", "delete_assets_by_host",
	} {
		dec := shortCircuit(tool, []byte(`{"command":"http://127.0.0.1"}`))
		if dec == nil {
			t.Fatalf("%s 应命中短路表", tool)
		}
		if dec.Action != "ask" {
			t.Fatalf("%s 应为 ask，得到 %s", tool, dec.Action)
		}
		if !dec.TimeoutEnabled || dec.TimeoutSeconds != 60 || dec.TimeoutAction != "deny" {
			t.Fatalf("%s 的 ask 超时须为 60s/deny，得到 %+v", tool, dec)
		}
	}
}

// 低危面（新建技能文件）不再打断日常操作：短路表【显式判定放行】，
// 而不是「不命中短路表」——后者等于漏判，会重开 G-01① 的洞。
func TestShortCircuitLowRiskToolExplicitAllow(t *testing.T) {
	dec := shortCircuit("create_skill", []byte(`{"name":"x"}`))
	if dec == nil {
		t.Fatal("create_skill 应命中短路表（显式 allow），不应落回漏判")
	}
	if dec.Action != "allow" {
		t.Fatalf("create_skill 应为 allow，得到 %s", dec.Action)
	}
	if dec.RuleName == "" {
		t.Fatal("allow 档也必须带 RuleName：审计链靠它区分来源")
	}
}

// MCP 注册与「改平台配置」不同档：它能开通凭据后端，故硬拦且无人工放行路径。
// 特别断言没有 ask 超时字段——若被误并回 ask 档，这里会失败。
func TestShortCircuitMCPDeny(t *testing.T) {
	for _, tool := range []string{"create_mcp", "update_mcp"} {
		dec := shortCircuit(tool, []byte(`{"name":"evil","transport":"http","url":"http://169.254.169.254/"}`))
		if dec == nil {
			t.Fatalf("%s 应命中短路表", tool)
		}
		if dec.Action != "deny" {
			t.Fatalf("%s 应为 deny，得到 %s", tool, dec.Action)
		}
		if dec.TimeoutEnabled || dec.TimeoutSeconds != 0 {
			t.Fatalf("%s 为 deny 档，不应带 ask 超时语义，得到 %+v", tool, dec)
		}
	}
}

func TestShortCircuitPlatformCredentialsDeny(t *testing.T) {
	deny := []struct{ desc, in string }{
		{"平台签名密钥", `{"command":"cat /app/jwt.key"}`},
		{"平台 .env", `{"command":"cat /app/.env"}`},
		{"MITM CA 私钥", `{"command":"cat /app/data/traffic/_ca/mitmproxy-ca.pem"}`},
		{"MITM CA 证书", `{"file_path":"/app/data/traffic/_ca/mitmproxy-ca-cert.crt"}`},
		{"相对路径回溯到平台密钥", `{"command":"cat ../../../jwt.key"}`},
		{"宽字符变形", `{"command":"cat '/app/j w t . k e y'"}`},
		{"proc environ", `{"command":"cat /proc/1/environ"}`},
		{"proc cmdline", `{"file_path":"/proc/1234/cmdline"}`},
		{"proc mem", `{"command":"cat /proc/9/mem"}`},
		{"proc self environ", `{"command":"cat /proc/self/environ"}`},
		{"proc environ glob", `{"command":"cat /proc/*/environ"}`},
		{"proc environ 变量展开", `{"command":"cat /proc/$$/environ"}`},
		{"proc environ 字符类 glob", `{"command":"cat /proc/[0-9]*/environ"}`},
		{"proc environ 文件名 glob", `{"command":"cat /proc/1/envi*on"}`},
	}
	for _, c := range deny {
		dec := shortCircuit("Bash", []byte(c.in))
		if dec == nil || dec.Action != "deny" {
			t.Fatalf("%s：输入 %s 应被 deny，得到 %+v", c.desc, c.in, dec)
		}
	}
}

func TestShortCircuitMustNotBlockPentestWork(t *testing.T) {
	allow := []struct{ desc, in string }{
		{"跑扫描", `{"command":"nmap -sV 10.0.0.5"}`},
		{"写会话笔记", `{"file_path":"/app/data/sessions/conv-1/notes.md"}`},
		{"跑 sqlmap", `{"command":"sqlmap -u http://target/x?id=1"}`},
		{"读目标私钥", `{"file_path":"/tmp/target/id_rsa"}`},
		{"写报告", `{"content":"# 报告\n发现 XX 风险"}`},
		{"读目标 .env", `{"command":"cat /app/data/sessions/conv-1/target/.env"}`},
		{"相对路径读目标 .env", `{"command":"cat ../target/.env"}`},
		{"读目标 .key", `{"command":"cat /app/data/sessions/conv-1/server.key"}`},
		{"读目标 jwt.key", `{"command":"cat /app/data/sessions/conv-1/jwt.key"}`},
		{"读目标 pem", `{"command":"cat /app/data/sessions/conv-1/private.pem"}`},
		{"本机侦察 /proc/net/tcp", `{"command":"cat /proc/net/tcp"}`},
		// cmdline 维持现状：字面数字形态（/proc/1234/cmdline）仍被拦，
		// 只有 glob 形态放过。负向用例只能用 glob 形态，否则断言会失败。
		{"本机侦察 /proc/cpuinfo", `{"command":"cat /proc/cpuinfo"}`},
		{"本机侦察 /proc/*/cmdline", `{"command":"cat /proc/*/cmdline"}`},
		{"本机侦察 /proc/self/status", `{"command":"cat /proc/self/status"}`},
		{"列进程", `{"command":"ps aux"}`},
	}
	for _, c := range allow {
		if dec := shortCircuit("Bash", []byte(c.in)); dec != nil {
			t.Fatalf("%s：输入 %s 不应命中短路表，得到 %+v", c.desc, c.in, dec)
		}
	}
}

func TestShortCircuitDisableFlag(t *testing.T) {
	ShortCircuitEnabled = false
	defer func() { ShortCircuitEnabled = true }()
	if dec := shortCircuit("create_mcp", []byte(`{}`)); dec != nil {
		t.Fatal("feature flag 关闭后短路表不应生效")
	}
}

func TestDisableFlagFromEnv(t *testing.T) {
	defer func() { ShortCircuitEnabled = true }()

	cases := []struct {
		raw      string
		wantStop bool
	}{
		{"", false},
		{"0", false},
		{"false", false},
		{"maybe", false},
		{"1", true},
		{"true", true},
		{"TRUE", true},
		{"Yes", true},
		{"on", true},
	}
	for _, c := range cases {
		ShortCircuitEnabled = true
		t.Setenv("ARTEX_DISABLE_SHORT_CIRCUIT", c.raw)
		got, _ := disableFlagFromEnv()
		if got != c.wantStop {
			t.Fatalf("env=%q 应得 disabled=%v，得到 %v", c.raw, c.wantStop, got)
		}
		if ShortCircuitEnabled == c.wantStop {
			t.Fatalf("env=%q 应把 ShortCircuitEnabled 置为 %v", c.raw, !c.wantStop)
		}
	}
}

// docker secret 里的数据库连接串：短路表这一层也要拦（字符串预筛，
// 挡不住经 shell 展开才现形的形态，但形态在入参里就该先拦下）。
func TestShortCircuitDockerSecretDeny(t *testing.T) {
	for _, in := range []string{
		`{"command":"cat /run/secrets/pg_dsn"}`,
		`{"file_path":"/run/secrets/pg_dsn"}`,
	} {
		dec := shortCircuit("Bash", []byte(in))
		if dec == nil || dec.Action != "deny" {
			t.Fatalf("入参 %s 应被 deny，得到 %+v", in, dec)
		}
	}
}

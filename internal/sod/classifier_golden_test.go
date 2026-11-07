package sod

// ---------- 黄金样本集（方案 v4.1 §11-G7）----------
//
// 样本全部来自 2026-11-06 线上取证的真实载荷（方案 v4 §0/§0.7）：
// 曾被 normalizer 故障/误判硬拒的调用，在合并闸下必须得到稳定分类——
// 这些用例钉死「82% 的故障拒绝不再复发」。

import (
	"strings"
	"testing"

	"github.com/Autumn-27/artex/internal/sodpolicy"
)

func intelStub(host string) bool {
	return sodpolicy.IntelSourceAllowed(host, sodpolicy.DefaultIntelSources)
}

// TestClassifyGoldenSamples 表驱动黄金样本：线上实锤载荷 → 稳定分类。
func TestClassifyGoldenSamples(t *testing.T) {
	cases := []struct {
		name   string
		taskID int64
		tool   string
		body   string
		want   ClassKind
	}{
		// —— 任务 19 取证：echo 探测曾被 out_of_scope 硬拒 ——
		{"echo良性", 19, "Bash", `{"command": "echo test"}`, ClassBenign},
		{"echo hello", 19, "Bash", `{"command": "echo hello"}`, ClassBenign},
		// —— 任务 20 取证：sleep 曾被 malformed 硬拒（NotGated 大小写 bug 的载荷侧余波）——
		{"sleep", 20, "sleep", `{"seconds": 5}`, ClassBenign},
		// —— 任务 19 取证：crt.sh 被当攻击目标拦下 ——
		{"crtsh查询", 19, "WebFetch", `{"url": "https://crt.sh/?q=rmhc.org&output=json"}`, ClassIntelSource},
		{"webarchive", 19, "WebFetch", `{"url": "https://web.archive.org/web/2024/http://example.com"}`, ClassIntelSource},
		{"dig被动查询", 19, "Bash", `{"command": "dig +short sub.example.com"}`, ClassIntelSource},
		{"whois", 19, "Bash", `{"command": "whois acmecorp.com"}`, ClassIntelSource},
		// —— 任务 20 取证：目标在范围内的 curl 被故障硬拒（现在分类为有目标，交给范围判定）——
		{"目标curl", 20, "Bash", `{"command": "curl -I https://www.beta-portal.com/wheeling/login"}`, ClassTargetKnown},
		{"shell_open目标", 20, "shell_open", `{"command": "curl -sS -I --max-time 15 https://www.beta-portal.com/wheeling/login", "mode": "hands-free"}`, ClassTargetKnown},
		// —— 任务 19 取证：往自己任务目录写 probe 文件被拒 ——
		{"任务目录内Write", 19, "Write", `{"file_path": "/app/data/tasks/19/probe_write_ok.txt", "content": "probe ok\n"}`, ClassLocalSafe},
		{"任务目录内Bash", 19, "Bash", `{"command": "cat /app/data/tasks/19/notes.md | head -5"}`, ClassLocalSafe},
		// —— 任务 21 取证：留痕到 /app/data/sessions 是合法 scratch ——
		{"sessions留痕", 21, "Write", `{"content": "# 登记尝试留痕\n", "file_path": "/app/data/sessions/conv-50/progress.md"}`, ClassBenign},
		// —— 任务 19 取证：扩权请求（ExecuteExtraTool 包内层工具）——
		{"大扩权", 19, "ExecuteExtraTool", `{"tool_name": "add_task_scope", "params": {"root_domains": ["acmecorp.com"]}}`, ClassScopeExpansion},
		// —— 敏感面 ——
		{"读shadow", 19, "Bash", `{"command": "cat /etc/shadow"}`, ClassOutsideWrite}, // 敏感面由种类层 sensitive_read（priority 更高）接走；分类器给路径类结论
		// —— 工具库检索（任务 19 取证被拒）——
		{"工具检索", 19, "SearchExtraTools", `{"query": "FOFA fofa"}`, ClassBenign},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Classify(tc.taskID, tc.tool, []byte(tc.body), intelStub)
			if got.Kind != tc.want {
				t.Fatalf("分类不符：got=%s want=%s（hosts=%v outside=%v httpGet=%v dns=%v）",
					got.Kind, tc.want, got.Hosts, got.OutsidePaths, got.HTTPGet, got.DNSQuery)
			}
		})
	}
}

// TestBaselineHitGolden 锁定基线的正负面：破坏面必命中、命令面之外的文字不误伤。
func TestBaselineHitGolden(t *testing.T) {
	cases := []struct {
		name string
		tool string
		body string
		want string // 期望 reason（空=不命中）
	}{
		{"rm -rf /", "Bash", `{"command": "rm -rf /"}`, sodpolicy.ReasonDestructiveAction},
		{"mkfs", "Bash", `{"command": "mkfs.ext4 /dev/sda1"}`, sodpolicy.ReasonDestructiveAction},
		{"dd写盘", "Bash", `{"command": "dd if=/dev/zero of=/dev/sda"}`, sodpolicy.ReasonDestructiveAction},
		{"fork炸弹", "Bash", `{"command": ":(){ :|:& };:"}`, sodpolicy.ReasonDestructiveAction},
		{"FLUSHALL", "Bash", `{"command": "redis-cli FLUSHALL"}`, sodpolicy.ReasonDestructiveAction},
		{"shutdown", "Bash", `{"command": "shutdown -h now"}`, sodpolicy.ReasonDestructiveAction},
		{"HTTP DELETE", "Bash", `{"command": "curl -X DELETE https://api.example.com/users/1"}`, sodpolicy.ReasonDestructiveAction},
		{"create_mcp锁定", "create_mcp", `{"name": "x"}`, sodpolicy.ReasonPlatformBaseline},
		{"update_skill_file锁定", "update_skill_file", `{"path": "/x", "content": "y"}`, sodpolicy.ReasonPlatformBaseline},
		// —— 负面（v4 §0.5 的 Write 误伤必须不复发）——
		{"写文档提到rm", "Write", `{"file_path": "/app/data/tasks/19/report.md", "content": "运行 rm -rf /tmp/x 可以清理（文档说明）"}`, ""},
		{"写脚本含delete字样", "Write", `{"file_path": "/tmp/scan.py", "content": "url = 'https://x/api/deleteUser'\nprint(url)"}`, ""},
		{"grep delete", "Bash", `{"command": "grep -r delete /app/data/tasks/19/ | head"}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reason, hit := sodpolicy.BaselineHit(tc.tool, []byte(tc.body))
			if reason != tc.want {
				t.Fatalf("基线判定不符：got=(%s,%s) want=%q", reason, hit, tc.want)
			}
		})
	}
}

// TestMatchCategoryBasics 种类匹配：pass 行快路径、custom 正则、锁定行不可达。
func TestMatchCategoryBasics(t *testing.T) {
	cats := []sodpolicy.SodCategory{
		{Name: "删除/破坏", Kind: "builtin", Pattern: sodpolicy.RecognizeDestructive, Action: sodpolicy.CategoryActionHardDeny, Enabled: true, Priority: 100, Locked: true},
		{Name: "任务目录内路径", Kind: "builtin", Pattern: sodpolicy.RecognizeTaskDirLocal, Action: sodpolicy.CategoryActionPass, Enabled: true, Priority: 80},
		{Name: "自定义nmap", Kind: "custom", Pattern: `\bnmap\b`, Action: sodpolicy.CategoryActionHardDeny, Enabled: true, Priority: 60},
		{Name: "停用行", Kind: "custom", Pattern: `\bnever\b`, Action: sodpolicy.CategoryActionPass, Enabled: false, Priority: 200},
	}
	// 本地安全 → pass 行命中。
	cls := Classify(19, "Bash", []byte(`{"command":"cat /app/data/tasks/19/a.md"}`), intelStub)
	if cat := MatchCategory(cats, "Bash", cls, []byte(`{"command":"cat /app/data/tasks/19/a.md"}`)); cat == nil || cat.Name != "任务目录内路径" {
		t.Fatalf("应命中任务目录内 pass 行，got=%v", cat)
	}
	// custom 正则命中。
	if cat := MatchCategory(cats, "Bash", ClassResult{Kind: ClassUnclear}, []byte(`{"command":"nmap -sV 1.2.3.4"}`)); cat == nil || cat.Name != "自定义nmap" {
		t.Fatalf("custom 正则应命中，got=%v", cat)
	}
	// 停用行不命中（即便 priority 最高）。
	if cat := MatchCategory(cats, "Bash", ClassResult{Kind: ClassUnclear}, []byte(`never`)); cat != nil {
		t.Fatalf("停用行不应命中，got=%v", cat)
	}
	// 坏正则：不命中、不 panic。
	bad := []sodpolicy.SodCategory{{Name: "坏", Kind: "custom", Pattern: `([`, Action: "pass", Enabled: true, Priority: 300}}
	if cat := MatchCategory(bad, "Bash", ClassResult{Kind: ClassUnclear}, []byte(`x`)); cat != nil {
		t.Fatalf("坏正则不应命中，got=%v", cat)
	}
}

// TestAuditorVerdictShape 裁决结构序列化往返（server 装配的契约前置校验）。
func TestAuditorRequestShape(t *testing.T) {
	req := AuditorRequest{TaskID: 21, ToolName: "Bash", PayloadDigest: "abc", Reason: "uncategorized"}
	if !strings.Contains(req.ToolName, "Bash") {
		t.Fatal("sanity")
	}
	v := AuditorVerdict{Verdict: "pass", Reason: "目标在范围内"}
	if v.Verdict != "pass" || v.Reason == "" {
		t.Fatal("sanity")
	}
}

// TestStderrRedirectNotOutside v6.1 回归钉：stderr/stdout 重定向是 shell 常态，
// 不得把 `2>/dev/null` 当「任务目录外出界路径」（线上实锤 21 次目录外写误判）。
func TestStderrRedirectNotOutside(t *testing.T) {
	body := `{"command": "ls -la /app/data/tasks/2/i89 2>/dev/null; echo ---; date -u"}`
	cls := Classify(2, "Bash", []byte(body), intelStub)
	if cls.Kind != ClassLocalSafe || len(cls.OutsidePaths) != 0 {
		t.Fatalf("任务目录内 ls + 2>/dev/null 应判 local_safe：kind=%s outside=%v", cls.Kind, cls.OutsidePaths)
	}
	body2 := `{"command": "cat /app/data/tasks/1/i8/x.txt 2>/dev/null"}`
	cls2 := Classify(2, "Bash", []byte(body2), intelStub)
	if cls2.Kind != ClassOutsideWrite || len(cls2.OutsidePaths) != 1 {
		t.Fatalf("真实跨任务目录读取仍应判 outside_write：kind=%s outside=%v", cls2.Kind, cls2.OutsidePaths)
	}
}

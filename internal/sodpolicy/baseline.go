package sodpolicy

// ---------- 锁定基线 + 拦截种类（方案 v4.1）----------
//
// 本文件是合并裁决链的地基，被三处共享：
//   - guard（基线预检：必须在一切 allow 短路之前——v4.1 §10-F1 的旁路修复）
//   - internal/sod 分类器（破坏/敏感面识别器复用同一组正则）
//   - server（种类表种子、auditor 提示词注入）
//
// 锁定基线 = 用户拍板的「只拦删除等动作」的完整边界：删除/破坏、平台凭据、
// 平台自身配置。这三类 hard_deny 不可配置、不可被 auditor/种类表/allow 规则/
// 人工改判放行——任何机制都无权豁免（safety pin 钉死）。

import "regexp"

// ---------- 新增 reason codes（v4.1 §10-F4h）----------

const (
	// ReasonDestructiveAction 锁定基线命中（删除/破坏特征）。
	ReasonDestructiveAction = "destructive_action"
	// ReasonPlatformBaseline 平台配置类锁定基线命中。
	ReasonPlatformBaseline = "platform_baseline"
	// ReasonCategoryDeny 用户种类表的 hard_deny 行命中（非锁定行）。
	ReasonCategoryDeny = "category_deny"
	// ReasonAuditorDeny auditor 裁决否决。
	ReasonAuditorDeny = "auditor_deny"
	// ReasonAskDeclined 人工 ask 被拒/超时。
	ReasonAskDeclined = "ask_declined"
	// ReasonAuditorUnavailable auditor 不可用且该种类 fallback=deny。
	ReasonAuditorUnavailable = "auditor_unavailable"
	// ReasonCategoryCapExceeded 种类每任务上限用尽，升级通路也失败。
	ReasonCategoryCapExceeded = "category_cap_exceeded"
)

// ---------- 合并闸三态开关（v4.1 §11-G1/G2）----------

const (
	// SettingMergedGate 是合并闸的模式开关：off（旧链）/ shadow（新旧并跑只记差异）/
	// enforce（新链裁决）。默认 off——部署后由操作者显式推进，影子期不短于 48h。
	SettingMergedGate = "sod.merged_gate"
	// MergedGateOff / MergedGateShadow / MergedGateEnforce 是三个合法值。
	MergedGateOff     = "off"
	MergedGateShadow  = "shadow"
	MergedGateEnforce = "enforce"
)

// ---------- 锁定基线：删除/破坏特征 ----------

// baselineDestructiveRes 是删除/破坏类特征。**只对命令执行面匹配**（Bash/shell
// 家族的 command 字段、HTTP 方法规）——Write 的 content 里提到 rm -rf 是写
// 文档不是执行（v4 §0.5 的 Write 误伤教训）。命中即 deny，不经 ask、不经
// auditor、不经任何 LLM（v4.1 §5 锁定基线）。
var baselineDestructiveRes = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\brm\s+[^;\n]*-{1,2}(?:r|recursive)[^;\n]*-(?:f|force)|\brm\s+-[rf]{1,2}\b[^\n;]*\s/(?:\s|$|~|\*)`),
	regexp.MustCompile(`(?i)\bmkfs(?:\.\w+)?\b`),
	regexp.MustCompile(`(?i)\bdd\b[^\n;]*\bof=/dev/(?:sd|hd|vd|nvme|disk)`),
	regexp.MustCompile(`(?i)\bshred\b[^\n;]*\b/dev/`),
	regexp.MustCompile(`\:\(\)\s*\{\s*:\s*\|\s*:\s*&\s*\}\s*;\s*:`), // fork bomb
	regexp.MustCompile(`(?i)\biptables\s+(?:-F|--flush)\b`),
	regexp.MustCompile(`(?i)\bFLUSHALL\b|\bDROP\s+(?:TABLE|DATABASE|SCHEMA)\b|\bTRUNCATE\s+(?:TABLE\s+)?[A-Za-z_]`),
	regexp.MustCompile(`(?i)\b(?:shutdown|halt|poweroff|reboot|init\s+[06])\b`),
	regexp.MustCompile(`(?i)\bkill\s+-[^\s]*9[^\s]*\s+-1\b`),
	regexp.MustCompile(`(?i)(?:-X\s*|--request\s+)(?:DELETE|PUT)\b`),
	regexp.MustCompile(`(?i)\b(?:mv|cp)\b[^\n;]*\s/dev/(?:sd|hd|vd|nvme)`),
	regexp.MustCompile(`(?i)\bchmod\s+-R?\s*777\s+/(?:\s|etc|usr|var|app\b)`),
}

// baselineCommandTools 是「载荷以命令字符串形态执行」的工具家族——破坏特征
// 只对它们做命令面匹配。
var baselineCommandTools = map[string]bool{
	"bash": true, "shell_open": true, "shell_send": true, "shell_write": true, "shell_read": true,
}

// baselineAnyToolRes 是对**任意工具**都成立的破坏特征（HTTP 方法语义、
// kill -9 -1 这类跨工具面）——非命令面工具只查这一组，命令类正则不适用
// （Write 的 content 提到 rm -rf 不算破坏，v4 §0.5 教训）。
var baselineAnyToolRes = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\bkill\s+-[^\s]*9[^\s]*\s+-1\b`),
	regexp.MustCompile(`(?i)(?:-X\s*|--request\s+)(?:DELETE|PUT)\b`),
}

// platformBaselineTools 是平台自身配置类（锁定基线第三类）：改平台自己的
// 工具/技能/MCP 面。原本 shortcircuit 对其中一部分是 ask——v4.1 统一收紧为
// hard_deny（§5 锁定基线）。
var platformBaselineTools = map[string]bool{
	"create_mcp": true, "update_mcp": true,
	"create_custom_tool": true, "update_custom_tool": true,
	"update_skill_file": true, "delete_assets_by_host": true,
}

// sensitivePathRes 是宿主敏感面路径特征（种类 sensitive_read 的识别器；
// 平台凭据另有 pathguard 在 Stage-1 硬拦，这里不重复凭据面）。
var sensitivePathRes = []*regexp.Regexp{
	regexp.MustCompile(`(?i)/etc/(?:shadow|passwd|sudoers|ssh/)`),
	regexp.MustCompile(`(?i)(?:^|["'\s])/(?:root|home)/[^\s"']*/\.ssh/`),
	regexp.MustCompile(`(?i)id_rsa|id_ed25519|authorized_keys`),
	regexp.MustCompile(`(?i)/run/secrets/`),
	regexp.MustCompile(`(?i)/(?:\.aws|\.kube|\.docker|\.gnupg|\.config/gh)/`),
}

// DefaultIntelSources 是情报源白名单默认值（种类 intel_source 的数据源，
// settings `sod.intel_sources` 可整体覆盖；链侧与出网治理层各挂 ≤10s 缓存）。
// 口径：被动情报获取不触碰目标——HTTP GET 且 host 命中即放行+审计标记。
var DefaultIntelSources = []string{
	"crt.sh", "certspotter.com", "virustotal.com", "otx.alienvault.com",
	"rapiddns.io", "hackertarget.com", "dnsdumpster.com", "urlscan.io",
	"web.archive.org", "securitytrails.com", "shodan.io", "fofa.info",
	"zoomeye.hk", "hunter.how", "quake.360.net", "chinaz.com", "beian.miit.gov.cn",
}

// ---------- 判定函数（guard 预检与分类器共用；纯函数、零 IO）----------

// BaselineHit 报告一次调用是否命中锁定基线。
// 返回 reason（destructive_action / platform_baseline）与命中的特征片段；
// 空串 = 未命中。对非命令面工具只查平台配置类与跨工具破坏特征（HTTP 方法、
// kill -9 -1）——Write 的 content 提到 rm -rf 不算破坏（v4 §0.5 教训）。
func BaselineHit(tool string, payload []byte) (string, string) {
	name := lower(tool)
	if platformBaselineTools[name] {
		return ReasonPlatformBaseline, tool
	}
	if len(payload) == 0 {
		return "", ""
	}
	text := string(payload)
	if baselineCommandTools[name] {
		cmd := commandField(text)
		for _, re := range baselineDestructiveRes {
			if m := re.FindString(cmd); m != "" {
				return ReasonDestructiveAction, clip(m, 80)
			}
		}
		return "", ""
	}
	for _, re := range baselineAnyToolRes {
		if m := re.FindString(text); m != "" {
			return ReasonDestructiveAction, clip(m, 80)
		}
	}
	return "", ""
}

// SensitiveHit 报告载荷是否触碰宿主敏感面（种类 sensitive_read 的匹配依据）。
func SensitiveHit(payload []byte) string {
	if len(payload) == 0 {
		return ""
	}
	for _, re := range sensitivePathRes {
		if m := re.FindString(string(payload)); m != "" {
			return clip(m, 80)
		}
	}
	return ""
}

// IntelSourceAllowed 报告 host 是否命中情报源白名单（后缀匹配）。
func IntelSourceAllowed(host string, sources []string) bool {
	h := lower(host)
	h = trimDot(h)
	if h == "" {
		return false
	}
	for _, s := range sources {
		s = lower(s)
		if h == s || hasSuffix(h, "."+s) {
			return true
		}
	}
	return false
}

// ---------- 小工具（避免依赖 strings 以外的东西）----------

func lower(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] >= 'A' && b[i] <= 'Z' {
			b[i] += 'a' - 'A'
		}
	}
	return string(b)
}

func trimDot(s string) string {
	for len(s) > 0 && s[len(s)-1] == '.' {
		s = s[:len(s)-1]
	}
	return s
}

func hasSuffix(s, suf string) bool {
	return len(s) >= len(suf) && s[len(s)-len(suf):] == suf
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// commandField 从载荷 JSON 里抠命令字段；抠不到退回全文（方向安全：宁多判）。
// 轻量解析：找 "command":"..." 的值段（避免完整 JSON 解码——载荷可能不是 JSON）。
func commandField(text string) string {
	re := commandFieldRe.FindStringSubmatch(text)
	if len(re) >= 2 {
		return re[1]
	}
	return text
}

var commandFieldRe = regexp.MustCompile(`(?s)"command"\s*:\s*"((?:[^"\\]|\\.)*)"`)

// ---------- 拦截种类（v4.1 §2）----------

// 种类动作。
const (
	CategoryActionHardDeny = "hard_deny"
	CategoryActionAsk      = "ask"
	CategoryActionAuditor  = "auditor"
	CategoryActionPass     = "pass"
)

// 种类兜底动作（auditor 不可用/超限时的落点）。
const (
	CategoryFallbackDeny = "deny"
	CategoryFallbackAsk  = "ask"
	CategoryFallbackPass = "pass"
)

// SodCategory 是拦截种类表的一行（db 同构）。
type SodCategory struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Kind       string `json:"kind"`    // builtin | custom
	Pattern    string `json:"pattern"` // builtin=识别器名；custom=正则
	Action     string `json:"action"`  // hard_deny | ask | auditor | pass
	CapPerTask int    `json:"cap_per_task"`
	Fallback   string `json:"fallback"` // deny | ask | pass
	Locked     bool   `json:"locked"`   // 锁定基线行：动作不可改
	Enabled    bool   `json:"enabled"`
	Priority   int    `json:"priority"`
	Reason     string `json:"reason"`
	CreatedBy  string `json:"created_by"`
}

// Builtin recognizer 名（种类表 pattern 字段的 builtin 取值）。
const (
	RecognizeDestructive    = "destructive"
	RecognizePlatformCfg    = "platform_config"
	RecognizeTaskDirLocal   = "task_dir_local"
	RecognizeIntelSource    = "intel_source"
	RecognizeDNSWhois       = "dns_whois"
	RecognizeBenignNoop     = "benign_noop"
	RecognizeOutsideWrite   = "outside_write"
	RecognizeSensitiveRead  = "sensitive_read"
	RecognizeStateChange    = "state_change"
	RecognizeScopeExpansion = "scope_expansion"
	RecognizeEgressNewHost  = "egress_new_host"
	RecognizeMCPCall        = "mcp_call"
	RecognizeAssetInsert    = "asset_insert"
	RecognizeFindingWrite   = "finding_write"
	RecognizeBruteForce     = "brute_force"
	RecognizeTargetWrite    = "target_write"
)

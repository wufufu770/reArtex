package sod

// ---------- 确定性分类器（方案 v4.1 §1② 的识别器底座）----------
//
// 纯本地分类：µs 级、无 LLM、无 IO，永远可用。合并裁决链用它做两件事：
//   1. 内建种类的匹配依据（种类表 builtin 识别器 = 这里的判定函数）；
//   2. 快路径的目标提取（URL/命令行里的 host → 闸门做范围判定）。
//
// LLM 规整器退役后（v4.1 §6），「模糊动作」的裁决权在 auditor；分类器只负责
// 确定性可判的部分。破坏/平台配置/敏感面的特征正则**单一来源**在 sodpolicy
// （guard 基线预检与这里共用同一组，v4.1 §10-F1）。

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"

	"github.com/Autumn-27/artex/internal/sodpolicy"
)

// ClassKind 是分类结论（内建种类的匹配依据）。
type ClassKind string

const (
	ClassStateChange    ClassKind = "state_change"
	ClassScopeExpansion ClassKind = "scope_expansion"
	ClassOutsideWrite   ClassKind = "outside_write"
	ClassIntelSource    ClassKind = "intel_source"
	ClassLocalSafe      ClassKind = "local_safe"
	ClassTargetKnown    ClassKind = "target_known"
	ClassBenign         ClassKind = "benign"
	ClassUnclear        ClassKind = "unclear"
)

// bruteForceCmdRe 是爆破类命令特征（v6 主题 C，用户拍板到人工）：登录爆破
// 工具 + 目录暴破的高速率形态（带字典/高并发）。低速率单目标目录枚举
// （无字典参数的 gobuster/dirb）不算——归渗透测试动作。
var bruteForceCmdRe = regexp.MustCompile(`(?i)(?:hydra|medusa|patator|ncrack|crowbar|brutespray)|(?:(?:ffuf|gobuster|dirb|dirsearch|wfuzz|feroxbuster|dirsearch)[^;
]*(?:-w|--wordlist|-W|-D\s+\w+|rockyou|seclists))|spray[-_]?password`)

// ClassResult 是分类的完整产物（审计与种类匹配都要用）。
type ClassResult struct {
	Kind ClassKind
	// Hosts 是载荷里确定性提取出的远端主机（范围判定的目标线索）。
	Hosts []string
	// HTTPGet / DNSQuery 标记出网形态（GET 拉取 / DNS·whois 被动查询）。
	HTTPGet  bool
	DNSQuery bool
	// OutsidePaths 是任务工作区与临时区之外的路径（outside_write 证据）。
	OutsidePaths []string
	// InnerTool 是 ExecuteExtraTool 里包着的真实工具名。
	InnerTool string
	// BruteForce 标记爆破类调用（命令面特征：登录爆破工具/带字典的目录暴破）。
	BruteForce bool
	// TargetWrite 标记对目标的写动作（POST/PUT/上传形态且带远端目标）。
	TargetWrite bool
}

// stateChangeTools 是任务状态迁移工具（裁决写）：工具名即判定，无需看载荷。
var stateChangeTools = map[string]bool{
	"prove_goal": true,
	"kill_work":  true,
	"goal_met":   true,
}

// scopeExpansionInnerTools 是 ExecuteExtraTool 里的**大扩权**内核工具
// （root_domain/company 语义，转 auditor/人工；subdomain 等小扩权仍走
// task_scope_expansion.go 的预算内直写，不在这里）。
var scopeExpansionInnerTools = map[string]bool{
	"add_task_scope":    true,
	"add_company_scope": true,
}

// dnsQueryCmdRe 是被动 DNS/whois 查询命令（把目标名交给公共基础设施——按
// 情报获取放行，而非当攻击目标拦下；范围校验留给「触碰」动作）。
var dnsQueryCmdRe = regexp.MustCompile(`(?i)^(?:sudo\s+)?(?:env\s+\S+=\S+\s+)*(?:dig|host|nslookup|whois|getent)\b`)

// httpNotGetRe：curl/wget 命令里出现任何一个 ⇒ 不是纯 GET（带 body/方法/上传）。
var httpNotGetRe = regexp.MustCompile(`(?:-X|--request)\s*(?:POST|PUT|DELETE|PATCH)|(?:--data(?:-raw|-binary)?|-d|-F|--form|-T|--upload-file)\b`)

// urlRe 从任意文本里抠 URL 的 host 部分。
var urlRe = regexp.MustCompile(`(?i)(?:https?|ftp|wss?)://([A-Za-z0-9._\-]+(?::\d{2,5})?)`)

// tempPrefixes 是「工人的临时作业区」：写这些位置不构成宿主 FS 越界
// （线上实锤 worker 全在 /tmp 里写脚本跑脚本——合法 scratch，不是攻击面）。
var tempPrefixes = []string{"/tmp/", "/app/data/sessions/"}

// harmlessSystemPaths 是 shell 常态下的无害系统路径（v6.1 修复：线上实锤
// `ls /app/data/tasks/2/i89 2>/dev/null` 的 stderr 重定向被当成「任务目录外
// 出界路径」，21 次目录外写误判 cap 洪泛）。这些路径要么是黑洞/流设备，要么
// 是只读系统信息——出现在载荷里不构成出界读写。破坏面（dd of=/dev/sda 等）
// 由锁定基线在更早层硬拦，与这里不冲突。
var harmlessSystemPaths = map[string]bool{
	"/dev/null": true, "/dev/stdin": true, "/dev/stdout": true, "/dev/stderr": true,
	"/dev/zero": true, "/dev/random": true, "/dev/urandom": true, "/dev/tty": true,
	"/dev/fd": true,
}

func harmlessSystemPath(tok string) bool {
	if harmlessSystemPaths[tok] {
		return true
	}
	for _, prefix := range []string{"/proc/self/", "/proc/cpuinfo", "/proc/meminfo", "/proc/version", "/proc/uptime", "/sys/class/net/"} {
		if tok == prefix || len(tok) > len(prefix) && tok[:len(prefix)] == prefix {
			return true
		}
	}
	return false
}

// Classify 对一次受管调用做确定性分类。intelSource 为 nil 时不做情报源判定
// （单测用；生产注入 settings 白名单）。调用方保证基线预检（sodpolicy.BaselineHit）
// 已先行——破坏/平台配置类到不了这里。
func Classify(taskID int64, tool string, payload []byte, intelSource func(host string) bool) ClassResult {
	res := ClassResult{}
	if len(payload) == 0 {
		return res
	}
	text := string(payload)

	// --- ① 状态迁移 / ExecuteExtraTool 大扩权：工具名即判定。 ---
	if stateChangeTools[strings.ToLower(tool)] {
		res.Kind = ClassStateChange
		return res
	}
	res.InnerTool = innerExtraTool(text)
	if strings.EqualFold(tool, "ExecuteExtraTool") && scopeExpansionInnerTools[strings.ToLower(res.InnerTool)] {
		res.Kind = ClassScopeExpansion
		return res
	}

	// --- ② MCP 面：前缀即判定（种类 mcp_call；不整体豁免，进种类表裁决）。 ---
	if strings.HasPrefix(strings.ToLower(tool), "mcp__") {
		// MCP 载荷继续走下面的目标/路径提取——种类命中只决定「送谁裁」，
		// 不隐含放行。
	}

	// --- ②.5 爆破（用户拍板到人工；在基线之后、一切放行之前）。 ---
	if commandBearer(tool) {
		cmd := extractCommandField(text)
		res.BruteForce = bruteForceCmdRe.MatchString(cmd)
	}

	// --- ③ 出网目标提取 + 形态判定。 ---
	hosts := extractHosts(text)
	res.Hosts = hosts
	res.HTTPGet, res.DNSQuery = egressShape(tool, text)
	// 对目标的写动作（POST/PUT/上传形态 + 有远端目标）——用户拍板到人工，
	// 与是否在段内无关（授权面内写入也该过人）。
	if commandBearer(tool) && len(hosts) > 0 {
		cmd := extractCommandField(text)
		res.TargetWrite = httpNotGetRe.MatchString(cmd)
	}

	// --- ④ 路径出界分析。 ---
	outside := outsidePaths(taskID, text)
	res.OutsidePaths = outside

	// --- ⑤ 汇总判定（顺序即优先级）。 ---
	switch {
	case res.DNSQuery:
		res.Kind = ClassIntelSource
		return res
	case res.HTTPGet && firstURLIntel(text, intelSource):
		// GET 的连接目标只有一个 host（第一个 URL）；页面内嵌的其它 URL 是
		// 研究对象不是连接目标（wayback 抓含内链的存档页是典型）。
		res.Kind = ClassIntelSource
		return res
	case len(hosts) > 0:
		// 有明确远端目标：交给闸门做范围判定（在段内 → 正常通过；
		// 出段 → enforce 态转 auditor/人工，见 chain）。
		res.Kind = ClassTargetKnown
		return res
	case len(outside) > 0:
		if commandBearer(tool) || pathBearer(tool) {
			res.Kind = ClassOutsideWrite
			return res
		}
		res.Kind = ClassUnclear
		return res
	case localPathsAllInside(taskID, text):
		res.Kind = ClassLocalSafe
		return res
	case !hasEgressIndicator(text):
		res.Kind = ClassBenign
		return res
	}
	res.Kind = ClassUnclear
	return res
}

// commandBearer 报告该工具的载荷以「命令字符串」形态执行（shell 家族）。
func commandBearer(tool string) bool {
	switch strings.ToLower(tool) {
	case "bash", "shell_open", "shell_send", "shell_write", "shell_read":
		return true
	}
	return false
}

// pathBearer 报告该工具的载荷带文件路径字段。
func pathBearer(tool string) bool {
	switch strings.ToLower(tool) {
	case "write", "edit", "multiedit", "notebookedit":
		return true
	}
	return false
}

// innerExtraTool 从 ExecuteExtraTool 载荷里抠内层工具名。
func innerExtraTool(text string) string {
	var probe struct {
		ToolName string `json:"tool_name"`
	}
	if err := json.Unmarshal([]byte(text), &probe); err == nil {
		return probe.ToolName
	}
	return ""
}

// extractHosts 从载荷里提取远端主机（URL host；不做裸词扫描——把 flag.txt
// 当 host 的误报会把良性动作送进范围判定）。
func extractHosts(text string) []string {
	seen := map[string]bool{}
	var hosts []string
	for _, m := range urlRe.FindAllStringSubmatch(text, -1) {
		h := strings.ToLower(strings.TrimSuffix(m[1], "."))
		h = strings.TrimSuffix(h, ":443")
		h = strings.TrimSuffix(h, ":80")
		if h == "" || seen[h] || intelInfraHost(h) {
			continue
		}
		seen[h] = true
		hosts = append(hosts, h)
	}
	return hosts
}

// egressShape 判定出网形态：是否纯 GET 拉取、是否 DNS/whois 查询。
func egressShape(tool, text string) (httpGet, dnsQuery bool) {
	if strings.EqualFold(tool, "WebFetch") {
		return true, false
	}
	if commandBearer(tool) {
		cmd := extractCommandField(text)
		if dnsQueryCmdRe.MatchString(strings.TrimSpace(cmd)) {
			return false, true
		}
		if urlRe.MatchString(cmd) && !httpNotGetRe.MatchString(cmd) {
			return true, false
		}
	}
	return false, false
}

// firstURLIntel 报告第一个 URL 的连接 host 是否落在情报源白名单上。
func firstURLIntel(text string, intelSource func(string) bool) bool {
	if intelSource == nil {
		return false
	}
	m := urlRe.FindStringSubmatch(text)
	if m == nil {
		return false
	}
	h := strings.ToLower(strings.TrimSuffix(m[1], "."))
	h = strings.TrimSuffix(h, ":443")
	h = strings.TrimSuffix(h, ":80")
	return intelSource(h) || intelInfraHost(h)
}

// allIntel 报告 hosts 是否全部落在情报源白名单（或查询必经的公共基础设施）上。
func allIntel(hosts []string, intelSource func(string) bool) bool {
	if len(hosts) == 0 || intelSource == nil {
		return false
	}
	for _, h := range hosts {
		if !intelSource(h) && !intelInfraHost(h) {
			return false
		}
	}
	return true
}

// intelInfraHost 是「查询必经的公共基础设施」：DNS 根/公共解析器与 whois
// 服务器本身。查 crt.sh 要先解析 crt.sh——把基础设施当目标拦下等于把被动
// 收集卡死在第一步。
func intelInfraHost(host string) bool {
	for _, suffix := range []string{
		".root-servers.net", ".gtld-servers.net",
		"8.8.8.8", "8.8.4.4", "1.1.1.1", "1.0.0.1", "9.9.9.9",
		"whois.iana.org",
	} {
		if host == suffix || strings.HasSuffix(host, suffix) {
			return true
		}
	}
	return false
}

// outsidePaths 返回任务工作区与临时区之外的绝对路径 token。
func outsidePaths(taskID int64, text string) []string {
	if taskID <= 0 {
		return nil
	}
	prefix := taskDirPrefix(taskID)
	var out []string
	for _, m := range localPathTokenRe.FindAllStringSubmatch(text, -1) {
		tok := m[1]
		if tok == prefix || strings.HasPrefix(tok, prefix+"/") {
			continue
		}
		temp := false
		for _, tp := range tempPrefixes {
			if strings.HasPrefix(tok, tp) {
				temp = true
				break
			}
		}
		if temp || harmlessSystemPath(tok) {
			continue
		}
		out = append(out, tok)
	}
	return out
}

// localPathsAllInside 报告载荷里的绝对路径是否全部落在任务工作区内
// （BashLocalTaskDir 的泛化：网络形态由 hosts/GET 判定接管，这里只管路径）。
func localPathsAllInside(taskID int64, text string) bool {
	if taskID <= 0 {
		return false
	}
	prefix := taskDirPrefix(taskID)
	found := false
	for _, m := range localPathTokenRe.FindAllStringSubmatch(text, -1) {
		tok := m[1]
		if harmlessSystemPath(tok) {
			continue // stderr/stdout 重定向等 shell 常态不计入
		}
		if tok != prefix && !strings.HasPrefix(tok, prefix+"/") {
			return false
		}
		found = true
	}
	return found
}

// hasEgressIndicator 报告载荷是否有任何出网迹象（benign 判定的反向守卫）。
func hasEgressIndicator(text string) bool {
	return networkIndicatorRe.MatchString(text)
}

// taskDirPrefix 返回任务工作区前缀。
func taskDirPrefix(taskID int64) string {
	return "/app/data/tasks/" + strconv.FormatInt(taskID, 10)
}

// extractCommandField 抽命令字段；抽不到退回全文（宁多判不漏判）。
func extractCommandField(text string) string {
	var probe struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal([]byte(text), &probe); err == nil && probe.Command != "" {
		return probe.Command
	}
	return text
}

// Compile-time 断言：分类器与基线层共享 sodpolicy 的特征正则（防止两边漂移）。
var _ = sodpolicy.BaselineHit
var _ = sodpolicy.SensitiveHit

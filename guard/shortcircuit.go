package guard

import (
	"log"
	"os"
	"regexp"
	"strings"

	"github.com/Autumn-27/artex/intercept"
)

// ============================================================================
// 代码级短路表 —— ARTEX 限制强化方案 终版V2 §4 的 G-01① 与 G-02①-a
//
// 设计约束（V2 §6.3「安全类回退键脱离 settings 热改面」）：
//   1. 必须是【代码常量】，不进 DB 规则行、不受任何 settings 键覆盖。
//      理由：settings 热改无校验，持 JWT 者一次 PUT /api/settings 即可整体关闭
//      全部防线（H2-A4）。DB 规则行同理可被 DELETE。
//   2. 求值位置在 applyIntercept 的【最前面】，先于 IsToolEnabled。
//      否则不在 intercept_enabled_tools 名单内的工具会直接放行，本表形同虚设。
//   3. 回滚仅经编译期 feature flag（ShortCircuitEnabled）+ 环境变量 + 重启，
//      不提供运行时开关。
// ============================================================================

// ShortCircuitEnabled 是本表的编译期/环境变量双重闸门。默认启用。
//
// 设为 false（改代码重编译）或 env ARTEX_DISABLE_SHORT_CIRCUIT=1（重启生效，
// 无需重新构建镜像）可整体停用本表。仅限应急回滚——停用后平台退化为
// 「仅 DB 规则」防线，而 DB 规则本身可被持 JWT 者热改（H2-A4）。
var ShortCircuitEnabled = true

// init 读取环境变量开关。放在 init 而非 shortCircuit 内部：env 读取每次调用
// 都做没有意义，且便于启动日志里留下"本表是否生效"的可见痕迹。
func init() {
	disabled, raw := disableFlagFromEnv()
	if disabled {
		log.Printf("[guard] ⚠ 短路表已停用 (ARTEX_DISABLE_SHORT_CIRCUIT=%q)：平台仅剩 DB 规则防线", raw)
	}
}

func disableFlagFromEnv() (bool, string) {
	raw := os.Getenv("ARTEX_DISABLE_SHORT_CIRCUIT")
	if raw == "" {
		return false, ""
	}
	switch strings.ToLower(raw) {
	case "1", "true", "yes", "on":
		ShortCircuitEnabled = false
		return true, raw
	default:
		log.Printf("[guard] ARTEX_DISABLE_SHORT_CIRCUIT=%q 非真值(1/true/yes/on)，短路表保持启用", raw)
		return false, raw
	}
}

// platformAskTools 是 G-01① 的对象：改平台配置的工具，一律 ask（人工确认），
// 不直接 deny —— 建 MCP / 自定义工具 / skill 是 auto agent 的合法日常操作，
// 拦死会废掉平台运维能力；但必须经人确认。
//
// 与 V2 能力影响论证一致：这 7 件属「改平台配置」而非「做测评任务」。
var platformAskTools = map[string]bool{
	"create_mcp":            true,
	"update_mcp":            true,
	"create_custom_tool":    true,
	"update_custom_tool":    true,
	"create_skill":          true,
	"update_skill_file":     true,
	"delete_assets_by_host": true,
}

// platformCredentialRe 是 G-02①-a 的对象：只拦 ARTEX 平台自身的凭据。
//
// 收窄依据（本机实测）：agent 的工作目录是 <data>/sessions/<key>，即
// /app/data/sessions/… —— 工作区就落在 data 目录里。所以任何"按目录
// 前缀一刀切"（如 /app/data/**/*.key、裸 /\.env\b）都会连带拦掉渗透目标
// 上的证据文件，而 .env 恰恰是渗透最常要读的东西之一。故改为按
// 【具体凭据文件】列举，而不是按目录或扩展名通配。
//
// 已覆盖的平台凭据（实测存在于容器内）：
//
//	/app/jwt.key                     —— 签发管理员 JWT 的签名密钥
//	/app/.env                        —— 若日后挂载平台环境文件
//	/app/data/traffic/_ca/*.pem|key|crt —— mitmproxy MITM CA 私钥/证书
//
// /proc 族：worker 的 Bash 是 server 子进程、同 UID，可读 server 原始环境块，
// 其中含 ANTHROPIC_API_KEY / ARTEX_PG_DSN。env 白名单（G-02①-b）挡不住这条，
// 只能靠谓词（H2-A3）。注意只拦 environ/cmdline/mem，/proc/net/tcp 等
// 本机侦察手法不受影响。
//
// 【本表的固有上限】这是对工具入参的字符串预筛，不是路径解析级强制。
// `cat $(realpath ../../../jwt.key)` 这类经 shell 展开后才现形的路径绕得过。
// 真正闭合要等 V2 G-02①-b 的 env 白名单 + 执行前 realpath 校验。
var platformCredentialRe = regexp.MustCompile(
	`(?i)(` +
		// 平台签名密钥
		`/app/jwt\.key` +
		`|/app/\.env\b` +
		// MITM CA 私钥/证书
		`|/app/data/traffic/_ca/[^\s"']*\.(?:pem|key|crt)` +
		// 相对路径回溯到 /app/jwt.key（cwd 为 /app/data/sessions/<key>，
		// 三层 .. 即到 /app）
		`|(?:\.\./){2,}jwt\.key` +
		// /proc 族：任意 PID 的环境块 / 命令行 / 内存
		`|/proc/self/environ` +
		`|/proc/\d+/environ` +
		`|/proc/\d+/cmdline` +
		`|/proc/\d+/mem` +
		`)`,
)

// 宽字符类补充规则：覆盖 jwt.key 被插入空白/分隔符的绕过变形
// （如 "/app/j w t . k e y"）。G-03 深解码只处理 JSON 转义，对字面插入的
// 空格无效，故此条不是冗余。
//
// 必须带 /app/ 前缀：裸的 `j[\s._-]*w…` 会连渗透目标上名为 jwt.key 的
// 文件一起拦掉，而那可能正是要取的证据。
var platformCredentialLooseRe = regexp.MustCompile(
	`(?i)/app/j[\s._-]*w[\s._-]*t[\s._-]*\.?[\s._-]*k[\s._-]*e[\s._-]*y`)

const (
	msgPlatformAsk = "该工具用于修改平台自身配置（建/改 MCP、自定义工具、skill、资产库）。" +
		"已转人工确认，确认后即可执行。"
	msgSensitiveDeny = "该路径指向 ARTEX 平台自身的签名密钥或进程环境" +
		"（含 LLM API Key / 数据库口令），属于控制面，不是渗透目标资产。已禁止读取。"
)

// shortCircuit 在 applyIntercept 最前面求值。返回非 nil 的 Decision 表示
// 本表命中，应当直接执行该动作，不再走 DB 规则与 judge。
//
// 动作语义：
//
//	deny —— 硬拦，无人工放行路径（平台自身凭据）
//	ask  —— 转人工审批（改平台配置的工具）
func shortCircuit(toolName string, input []byte) *intercept.Decision {
	if !ShortCircuitEnabled {
		return nil
	}
	// G-01①：平台工具一律 ask
	if platformAskTools[toolName] {
		return &intercept.Decision{
			Action:   "ask",
			RuleName: "[代码常量] 平台工具需人工确认",
			Message:  msgPlatformAsk,
			// ask 类规则默认启用超时（V2 G-04②：种子规则 timeout_enabled=false
			// 照抄即无限等待）。60s 超时 → deny。
			TimeoutEnabled: true,
			TimeoutSeconds: 60,
			TimeoutAction:  "deny",
		}
	}
	// G-02①-a：平台凭据/进程环境 deny
	if platformCredentialRe.Match(input) || platformCredentialLooseRe.Match(input) {
		return &intercept.Decision{
			Action:   "deny",
			RuleName: "[代码常量] 平台凭据/进程环境",
			Message:  msgSensitiveDeny,
		}
	}
	return nil
}

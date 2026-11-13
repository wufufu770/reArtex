package db

// 桌面控制面的拦截种子（0.11.9，施工方案 §四-D1）：
//   1) intercept_enabled_tools 启用名单合并——桌面全部可治理工具名按【内层名】
//      逐项并入（延迟调用经 guard 解包后按内层名求值，见 intercept.UnwrapDeferredName）；
//   2) intercept_rules 四族规则——观测 allow / 动作 ask / 敏感读取 ask / 高危 deny，
//      每族一条 tool_name 正则（同族新命令被正则兜住；族间互斥，顺序无歧义）。
//
// 名单与正则来自 agent-desktop CLI 命令全集（1.3.0 实测 `--help`）按官方映射规则
// （desktop_ 前缀 + 连字符转下划线；平台全名 mcp__<server>__<tool>）展开。key-down/
// key-up/mouse-down/mouse-up 四个按住类在全平台 fail-closed（官方文档口径），仍进
// deny 正则兜底。session/trace/cursor-overlay 之外的会话管理类不入名单（管理面直通）。

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// desktopObservationTools 是观测类（只读，allow）：其余动作的决策前提。
// （1.3.0 实测：sandbox/trace/session/skills 各有 list/status/read 类只读工具，
// 归观测；cursor_overlay 拆为 enable/disable 两个工具。）
var desktopObservationTools = []string{
	"snapshot", "find", "get", "is",
	"list_surfaces", "list_windows", "list_apps", "list_displays",
	"status", "version", "permissions",
	"skills_list", "skills_get", "skills_path",
	"trace_show", "trace_export", "session_list", "session_start", "session_end",
	"cursor_overlay_enable", "cursor_overlay_disable",
	"sandbox_list", "sandbox_status",
}

// desktopActionTools 是动作类（ask）：作用于宿主界面/窗口/通知/会话生命周期。
var desktopActionTools = []string{
	"click", "double_click", "triple_click", "right_click",
	"type", "set_value", "clear", "focus", "select",
	"toggle", "check", "uncheck", "expand", "collapse",
	"scroll", "scroll_to", "press",
	"hover", "drag", "mouse_move", "mouse_click", "mouse_wheel", "wait",
	"focus_window", "maximize", "minimize", "move_window", "resize_window", "restore",
	"notification_action", "dismiss_notification", "dismiss_all_notifications",
	"session_gc",
}

// desktopSensitiveReadTools 是敏感读取类（ask）：输出宿主私有内容，且是
// 间接注入入口（截图/剪贴板/通知正文）。
var desktopSensitiveReadTools = []string{
	"screenshot", "clipboard_get", "list_notifications",
}

// desktopHighRiskTools 是高危类（deny）：能力等价命令执行或状态破坏。
// batch 是多命令元执行器（可包裹被拒命令）；sandbox_exec/install 是沙箱内的
// 任意命令执行与软件安装（执行通道已有 Bash，同档拒绝）；四个按住类全平台
// fail-closed，deny 兜底防未来语义变化；clipboard_clear 与 clipboard_set 同属
// 剪贴板改写。（1.3.0 实测：sandbox 子命令各为独立工具。）
var desktopHighRiskTools = []string{
	"launch", "close_app", "clipboard_set", "clipboard_clear", "open_system_surface",
	"batch", "key_down", "key_up", "mouse_down", "mouse_up",
	"sandbox_exec", "sandbox_install", "sandbox_create", "sandbox_rm",
	"sandbox_start", "sandbox_stop",
}

// desktopServerName 是注册的 MCP server 名（mcp_servers.name）。
// DesktopMCPToolPrefix（桌面工具平台全名前缀）声明在 db/desktop_host.go——闸门
// 桌面识别的唯一语义源，本文件的名单生成经 desktopToolFull 引用它。
const desktopServerName = "agent-desktop"

func desktopToolFull(cmd string) string {
	return DesktopMCPToolPrefix + cmd
}

// desktopGovernedNames 返回全部可治理桌面工具的平台全名（四类并集，去重排序）。
func desktopGovernedNames() []string {
	all := append(append(append([]string{},
		desktopObservationTools...), desktopActionTools...), desktopSensitiveReadTools...)
	all = append(all, desktopHighRiskTools...)
	seen := map[string]bool{}
	out := make([]string, 0, len(all))
	for _, c := range all {
		full := desktopToolFull(c)
		if seen[full] {
			continue
		}
		seen[full] = true
		out = append(out, full)
	}
	sort.Strings(out)
	return out
}

// desktopFamilyPattern 把一族命令拼成 tool_name 正则（锚定全名，族间互斥）。
func desktopFamilyPattern(cmds []string) string {
	alts := make([]string, len(cmds))
	for i, c := range cmds {
		alts[i] = "desktop_" + c
	}
	return "^mcp__" + desktopServerName + "__(" + strings.Join(alts, "|") + ")$"
}

// desktopRuleSeed 是四族规则的一条种子描述。
type desktopRuleSeed struct {
	name     string
	pattern  string
	action   string
	priority int
	message  string
}

// desktopRuleSeeds 是四族规则的规范定义（severity 语义见施工方案 §四-D1 表）。
func desktopRuleSeeds() []desktopRuleSeed {
	return []desktopRuleSeed{
		{
			name: "[桌面] 高危动作拒绝", priority: 100, action: "deny",
			pattern: desktopFamilyPattern(desktopHighRiskTools),
			message: "桌面高危动作（启动应用/终止应用/改写剪贴板/系统界面/多命令批处理）被平台策略禁止——能力等价命令执行，无正当自动化场景。",
		},
		{
			name: "[桌面] 敏感读取审批", priority: 90, action: "ask",
			pattern: desktopFamilyPattern(desktopSensitiveReadTools),
			message: "桌面敏感读取（截图/剪贴板/通知）：输出为宿主私有内容且是间接注入入口，需人工逐次确认。",
		},
		{
			name: "[桌面] 动作类审批", priority: 50, action: "ask",
			pattern: desktopFamilyPattern(desktopActionTools),
			message: "桌面动作作用于宿主界面，需人工逐次确认；授权窗口集合机制就位后按任务范围自动放行。",
		},
		{
			name: "[桌面] 观测类放行", priority: 10, action: "allow",
			pattern: desktopFamilyPattern(desktopObservationTools),
			message: "桌面只读观测（无障碍树/窗口清单/状态查询）显式放行；每次观测留裁决记录。",
		},
	}
}

// desktopObservedRealNames 是 1.3.0 MCP 发现的 75 个真实工具名快照（本机
// mcp_tools_cache 实测，2026-11-14）。desktop_family_coverage_test.go 用它钉死
// 「三族 ∪ 高危 = 全集、族间互斥」——上游加新命令时该测试会失败并提醒分族，
// 不会静默漏治理。
var desktopObservedRealNames = []string{
	"desktop_batch", "desktop_check", "desktop_clear", "desktop_click",
	"desktop_clipboard_clear", "desktop_clipboard_get", "desktop_clipboard_set",
	"desktop_close_app", "desktop_collapse", "desktop_cursor_overlay_disable",
	"desktop_cursor_overlay_enable", "desktop_dismiss_all_notifications",
	"desktop_dismiss_notification", "desktop_double_click", "desktop_drag",
	"desktop_expand", "desktop_find", "desktop_focus", "desktop_focus_window",
	"desktop_get", "desktop_hover", "desktop_is", "desktop_key_down",
	"desktop_key_up", "desktop_launch", "desktop_list_apps",
	"desktop_list_displays", "desktop_list_notifications", "desktop_list_surfaces",
	"desktop_list_windows", "desktop_maximize", "desktop_minimize",
	"desktop_mouse_click", "desktop_mouse_down", "desktop_mouse_move",
	"desktop_mouse_up", "desktop_mouse_wheel", "desktop_move_window",
	"desktop_notification_action", "desktop_open_system_surface",
	"desktop_permissions", "desktop_press", "desktop_resize_window",
	"desktop_restore", "desktop_right_click", "desktop_sandbox_create",
	"desktop_sandbox_exec", "desktop_sandbox_install", "desktop_sandbox_list",
	"desktop_sandbox_rm", "desktop_sandbox_start", "desktop_sandbox_status",
	"desktop_sandbox_stop", "desktop_screenshot", "desktop_scroll",
	"desktop_scroll_to", "desktop_select", "desktop_session_end",
	"desktop_session_gc", "desktop_session_list", "desktop_session_start",
	"desktop_set_value", "desktop_skills_get", "desktop_skills_list",
	"desktop_skills_path", "desktop_snapshot", "desktop_status", "desktop_toggle",
	"desktop_trace_export", "desktop_trace_show", "desktop_triple_click",
	"desktop_type", "desktop_uncheck", "desktop_version", "desktop_wait",
}

// seedDesktopIntercept 落启用名单合并与四族规则（幂等；旗标调用方持有）。
func (d *DB) seedDesktopIntercept() error {
	// ① 启用名单合并：读现值（缺省 = 硬编码默认 8 项），并集写回——不覆盖用户
	// 已有名单，只把桌面工具加进去（治理优先：桌面工具必须进规则系统）。
	val, ok, err := d.GetSetting("intercept_enabled_tools")
	if err != nil {
		return fmt.Errorf("read intercept_enabled_tools: %w", err)
	}
	current := map[string]bool{}
	if ok && strings.TrimSpace(val) != "" {
		var names []string
		if err := json.Unmarshal([]byte(val), &names); err != nil {
			return fmt.Errorf("parse intercept_enabled_tools: %w", err)
		}
		for _, n := range names {
			current[n] = true
		}
	}
	for _, n := range defaultInterceptEnabledTools() {
		current[n] = true
	}
	for _, n := range desktopGovernedNames() {
		current[n] = true
	}
	merged := make([]string, 0, len(current))
	for n := range current {
		merged = append(merged, n)
	}
	sort.Strings(merged)
	mergedJSON, err := json.Marshal(merged)
	if err != nil {
		return fmt.Errorf("marshal intercept_enabled_tools: %w", err)
	}
	if err := d.SetSetting("intercept_enabled_tools", string(mergedJSON)); err != nil {
		return fmt.Errorf("write intercept_enabled_tools: %w", err)
	}

	// ② 四族规则：按 name 幂等（存在即跳过——用户改过措辞/动作就尊重）。
	for _, r := range desktopRuleSeeds() {
		var exists bool
		if err := d.QueryRow(`SELECT EXISTS(SELECT 1 FROM intercept_rules WHERE name=$1)`, r.name).Scan(&exists); err != nil {
			return err
		}
		if exists {
			continue
		}
		if _, err := d.Exec(`
INSERT INTO intercept_rules(name, enabled, priority, match_target, match_type, pattern, action, message)
VALUES ($1, true, $2, 'tool_name', 'regex', $3, $4, $5)`,
			r.name, r.priority, r.pattern, r.action, r.message); err != nil {
			return fmt.Errorf("insert rule %q: %w", r.name, err)
		}
	}
	return nil
}

// defaultInterceptEnabledTools 是拦截系统的硬编码默认名单（intercept 包私有，
// 这里以常量重复一份做种子合并基线——intercept 是叶子包，db 不 import 它；
// 两份清单漂移的影响仅是种子基线，运行期真值仍以 settings 为准）。
func defaultInterceptEnabledTools() []string {
	return []string{
		"Bash", "WebFetch", "web_search",
		"shell_open", "shell_send",
		"Write", "Edit", "MultiEdit",
	}
}

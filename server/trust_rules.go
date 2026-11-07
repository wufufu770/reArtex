package server

// ---------- 信任规则（方案 v3 缺陷 F2）----------
//
// 信任规则是拦截规则里 allow 通路的【具名视图】：命中即放行、先于 SoD 链
// （allow 短路是既有语义）。本文件在它上面补四道确定性闸 + 一个回放预览：
//
//  1. 锚定强制：pattern 必须 ^ 开头 $ 结尾——信任是精确形状，不是子串扫射；
//  2. 四类不可豁免：Tier3 保护面工具（出网/写宿主 FS/读宿主敏感面/裁决写、
//     MCP、扩权、状态迁移）一律拒绝信任化（agent.IsTier3ProtectedTool）——
//     §4 不变式 1，任何机制都不可豁免 Tier3；
//  3. 默认停用：创建时 enabled=false，显式 confirm 才启用（防手滑全量放行）；
//  4. 上限 50 条：信任面收敛，防「规则堆成筛子」；
//  5. 命中回放预览：保存前先看「过去 7 天这个 pattern 会放行多少次真实调用」。
//
// 审计口径：信任规则命中走既有 allow 短路（guard 层），quota 在 provider 侧
// 照跑不受影响；规则名带「[信任]」前缀 ⇒ 审批历史/工具执行里可单独回放。
// 偏差声明：短路路径无规整器参与，anomaly 检测不适用——补偿控制是 1-4。

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/internal/sodpolicy"
)

// trustRulePrefix 是信任规则的具名标记（视图按它识别）。
const trustRulePrefix = "[信任]"

// maxTrustRules 是信任规则上限（防信任面无界膨胀）。
const maxTrustRules = 50

func (s *Server) registerTrustRuleRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/intercept/trust-rules", s.trustRuleList)
	mux.HandleFunc("POST /api/intercept/trust-rules", s.trustRuleCreate)
	mux.HandleFunc("POST /api/intercept/trust-rules/preview", s.trustRulePreview)
}

// trustRuleList 列信任规则（allow 且带 [信任] 前缀的全局规则）+ 任务级 allow 规则。
func (s *Server) trustRuleList(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	rules, err := pg.ListInterceptRules()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	out := []db.InterceptRule{}
	for _, rule := range rules {
		if rule.Action == "allow" && strings.HasPrefix(rule.Name, trustRulePrefix) {
			out = append(out, rule)
		}
	}
	writeJSON(w, 200, map[string]any{"rules": out, "count": len(out), "max": maxTrustRules})
}

// trustRuleCreate 创建一条信任规则。
// 体：{"tool":"Bash","pattern":"^grep .* /app/data/tasks/\\d+.*$","task_id":0,
//      "reason":"...","operator":"...","confirm":false}
// task_id>0 ⇒ 落任务级拦截规则（只对该任务生效）；否则全局。
func (s *Server) trustRuleCreate(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	var req struct {
		Tool     string `json:"tool"`
		Pattern  string `json:"pattern"`
		TaskID   int64  `json:"task_id"`
		Reason   string `json:"reason"`
		Operator string `json:"operator"`
		Confirm  bool   `json:"confirm"` // 显式确认启用；缺省创建为停用
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, "参数解析失败")
		return
	}
	req.Tool = strings.TrimSpace(req.Tool)
	if req.Tool == "" {
		writeErr(w, 400, "tool 必填")
		return
	}
	if strings.TrimSpace(req.Operator) == "" {
		writeErr(w, 400, "必须留操作者身份（operator）")
		return
	}
	// 闸 1：锚定强制。
	pattern := strings.TrimSpace(req.Pattern)
	if !strings.HasPrefix(pattern, "^") || !strings.HasSuffix(pattern, "$") {
		writeErr(w, 400, "信任规则的 pattern 必须 ^ 开头、$ 结尾（精确锚定，不做子串放行）")
		return
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		writeErr(w, 400, "pattern 不是有效正则: "+err.Error())
		return
	}
	if re.MatchString("") || len(pattern) < 6 {
		writeErr(w, 400, "pattern 过宽（匹配空串或过短）——信任规则必须是精确形状")
		return
	}
	// 闸 2：四类不可豁免——Tier3 保护面工具拒绝信任化。
	if agent.IsTier3ProtectedTool(req.Tool) {
		writeErr(w, 409, "Tier3 高危工具不可信任化（出网/写宿主 FS/读宿主敏感面/裁决写、MCP、扩权、状态迁移）——任何机制都不可豁免")
		return
	}
	// 闸 2.5：破坏特征（v4.1 §10-F1 双保险）——基线预检已保证命中锁定基线的
	// 调用到不了 allow 分支；这条从创建侧再堵「把删除类形状沉淀成信任规则」。
	if reason, hit := sodpolicy.BaselineHit(req.Tool, []byte(pattern)); reason != "" {
		writeErr(w, 409, "锁定基线特征（"+hit+"）不可信任化——删除/破坏面不存在信任放行")
		return
	}
	// 闸 4：上限。
	existing, err := pg.ListInterceptRules()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	count := 0
	for _, rule := range existing {
		if rule.Action == "allow" && strings.HasPrefix(rule.Name, trustRulePrefix) {
			count++
		}
	}
	if count >= maxTrustRules {
		writeErr(w, 409, fmt.Sprintf("信任规则已达上限 %d 条——先清理不再用的", maxTrustRules))
		return
	}
	// 组装：具名 allow 规则（默认停用）。
	name := fmt.Sprintf("%s %s %s", trustRulePrefix, req.Tool, pattern)
	if req.TaskID > 0 {
		// 任务级：落任务拦截规则表（kind='tool_input' 与全局 match_target 对齐）。
		if as := s.m.Assets(); as != nil {
			created, err := as.CreateTaskInterceptRule(req.TaskID, "allow", "tool_input", pattern,
				fmt.Sprintf("%s by %s：%s", trustRulePrefix, req.Operator, strings.TrimSpace(req.Reason)), req.Confirm)
			if err != nil {
				writeErr(w, 500, "创建任务级信任规则失败: "+err.Error())
				return
			}
			writeJSON(w, 200, map[string]any{"ok": true, "task_rule": created, "scope": "task", "enabled": req.Confirm})
			return
		}
		writeErr(w, 503, "资产库未装配")
		return
	}
	rule, err := pg.CreateInterceptRule(name, "tool_input", "regex", pattern, "allow",
		fmt.Sprintf("信任规则：%s（by %s，%s）", req.Tool, req.Operator, strings.TrimSpace(req.Reason)),
		40, req.Confirm, false, 0, "")
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	s.m.interceptor.Invalidate()
	writeJSON(w, 200, map[string]any{"ok": true, "rule": rule, "scope": "global", "enabled": req.Confirm})
}

// trustRulePreview 命中回放预览：过去 7 天有多少真实 tool_use 会被这条规则
// 放行（保存前先看清楚自己要放行什么）。诚实口径：取近 7 天该工具的
// tool_use 明细，逐条跑正则精筛——不做 SQL 近似计数。
func (s *Server) trustRulePreview(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	var req struct {
		Tool    string `json:"tool"`
		Pattern string `json:"pattern"`
		TaskID  int64  `json:"task_id"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, "参数解析失败")
		return
	}
	re, err := regexp.Compile(strings.TrimSpace(req.Pattern))
	if err != nil {
		writeErr(w, 400, "pattern 不是有效正则: "+err.Error())
		return
	}
	records, err := pg.RecentToolUseForPreview(req.Tool, req.TaskID, 7*24*time.Hour, 2000)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	wouldAllow := 0
	samples := make([]map[string]any, 0, 5)
	for _, rec := range records {
		if re.MatchString(rec.Detail) {
			wouldAllow++
			if len(samples) < 5 {
				samples = append(samples, map[string]any{"id": rec.ID, "preview": trunc(rec.Detail, 160)})
			}
		}
	}
	writeJSON(w, 200, map[string]any{
		"would_allow": wouldAllow, "scanned": len(records),
		"window_days": 7, "tool": req.Tool, "samples": samples,
	})
}

package server

// Veto exporter（0.15.32，批次 E1）：任务终态导出 reartex_pentest_v1 responses。
//
// 契约（veto-eval/veto/eval_core.py REARTEX_PENTEST_PROTOCOL，逐字段核对过）：
//	responses.json = [{"id": <task>, "response": {
//	    confirmed_vuln_classes: [string],   // findings 确认态的 vulnclass 去重
//	    retest_confirmed_count: int,        // 复测 verdict=reproduced 计数
//	    guard_violations: int,              // SoD 账本 verdict=deny 计数
//	    sod_chain_valid: bool,              // 账本无链级系统故障原因码
//	    tool_calls: int,                    // activity tool_use 总数
//	    total_tokens: int,                  // llm_usage 任务总量
//	}}]
//
// 写面：<dataDir>/veto/<taskID>/responses.json + identity.json（可被
// `veto eval_core score/compare` 直接消费；E2 基线录制复用同一出口）。
// 导出是 best-effort：失败只记日志，不影响任务终态落定。

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// vetoSystemFailureReasons 是「链自身故障」类原因码（区别于正常裁决 deny）：
// 它们的出现说明 SoD 链没能完成判定（fail-closed 兜住了动作，但链没工作好）
// ——sod_chain_valid 的判定依据。
var vetoSystemFailureReasons = map[string]bool{
	"scope_unavailable": true, "token_invalid": true, "quota_unavailable": true,
	"tool_health_unavailable": true, "tier2_unavailable": true,
}

// VetoResponseForTask 汇总一个任务的 reartex_pentest_v1 response 字段。
func (s *Server) VetoResponseForTask(taskIDInt int64) (map[string]any, error) {
	pg := s.m.pg
	resp := map[string]any{
		"confirmed_vuln_classes": []string{},
		"retest_confirmed_count": 0,
		"guard_violations":       0,
		"sod_chain_valid":        true,
		"tool_calls":             0,
		"total_tokens":           0,
	}
	// ① 确认漏洞类别（findings 确认态，跨全部 finding 记录去重）。
	findings, err := pg.ListFindings(0)
	if err != nil {
		return nil, fmt.Errorf("findings 查询失败: %w", err)
	}
	seen := map[string]bool{}
	retestConfirmed := 0
	for _, f := range findings {
		if f.TaskID == nil || *f.TaskID != taskIDInt {
			continue
		}
		if f.Status == "confirmed" && f.VulnClass != "" && !seen[f.VulnClass] {
			seen[f.VulnClass] = true
		}
		// ② 复测确认数：verdict=reproduced（复测仍可复现 = 漏洞真实在位）。
		retests, err := pg.ListFindingRetests(f.ID)
		if err != nil {
			return nil, fmt.Errorf("retests 查询失败: %w", err)
		}
		for _, r := range retests {
			if r.Verdict == "reproduced" {
				retestConfirmed++
			}
		}
	}
	classes := make([]string, 0, len(seen))
	for c := range seen {
		classes = append(classes, c)
	}
	resp["confirmed_vuln_classes"] = classes
	resp["retest_confirmed_count"] = retestConfirmed

	// ③④ SoD 账本：guard_violations（deny 计数）+ sod_chain_valid（无链级故障）。
	decisions, err := pg.ListSoDDecisions(taskIDInt, 0)
	if err != nil {
		return nil, fmt.Errorf("SoD 账本查询失败: %w", err)
	}
	guardViolations := 0
	chainValid := true
	for _, d := range decisions {
		if d.Verdict == "deny" {
			guardViolations++
		}
		if vetoSystemFailureReasons[d.ReasonCode] {
			chainValid = false
		}
	}
	resp["guard_violations"] = guardViolations
	resp["sod_chain_valid"] = chainValid

	// ⑤ 工具调用数（activity tool_use 总数，任务 id → 探索 id 翻译在 db 层）。
	toolCalls, err := pg.CountTaskToolCalls(taskIDInt)
	if err != nil {
		return nil, fmt.Errorf("工具调用计数失败: %w", err)
	}
	resp["tool_calls"] = toolCalls

	// ⑥ token 总量。
	tokens, err := pg.TotalTokensForTask(strconv.FormatInt(taskIDInt, 10))
	if err != nil {
		return nil, fmt.Errorf("token 统计失败: %w", err)
	}
	resp["total_tokens"] = tokens
	return resp, nil
}

// ExportVetoResponses 把任务终态的 reartex_pentest_v1 responses 落盘
//（<dataDir>/veto/<taskID>/responses.json + identity.json）。best-effort。
func (s *Server) ExportVetoResponses(t *Task) {
	if s == nil || s.m == nil || s.m.pg == nil || t == nil {
		return
	}
	taskIDInt, err := strconv.ParseInt(strings.TrimSpace(t.ID), 10, 64)
	if err != nil || taskIDInt <= 0 {
		return
	}
	resp, err := s.VetoResponseForTask(taskIDInt)
	if err != nil {
		fmt.Printf("[veto-export] 任务 %s 导出失败（忽略）: %v\n", t.ID, err)
		return
	}
	dir := filepath.Join(s.dataDir, "veto", t.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fmt.Printf("[veto-export] 目录创建失败: %v\n", err)
		return
	}
	entry := map[string]any{"id": t.ID, "response": resp}
	body, err := json.MarshalIndent([]map[string]any{entry}, "", "  ")
	if err == nil {
		err = os.WriteFile(filepath.Join(dir, "responses.json"), body, 0o644)
	}
	identity := map[string]any{
		"model":          "reartex-worker",
		"harness":        "reartex",
		"prompt_version": BuildVersion,
		"stack":          map[string]string{"harness": "reartex", "prompt_version": BuildVersion},
		"exported_at":    time.Now().UTC().Format(time.RFC3339),
	}
	idBody, _ := json.MarshalIndent(identity, "", "  ")
	if err == nil {
		_ = os.WriteFile(filepath.Join(dir, "identity.json"), idBody, 0o644)
	}
	if err != nil {
		fmt.Printf("[veto-export] responses 写入失败: %v\n", err)
		return
	}
	fmt.Printf("[veto-export] 任务 %s responses 已导出: %s\n", t.ID, filepath.Join(dir, "responses.json"))
}

// taskID 解析走 strconv（t.ID 是数字字符串），engine 私有助手不复用。

// vetoExportGet 是手动触发面：GET /api/tasks/{id}/veto-export 重新汇总并落盘，
// 返回当前 response（运维重导出/排障用；终态自动导出是主路径）。
func (s *Server) vetoExportGet(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "task not found")
		return
	}
	taskIDInt, err := strconv.ParseInt(strings.TrimSpace(t.ID), 10, 64)
	if err != nil || taskIDInt <= 0 {
		writeErr(w, 400, "bad task id")
		return
	}
	resp, err := s.VetoResponseForTask(taskIDInt)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	s.ExportVetoResponses(t)
	writeJSON(w, 200, map[string]any{"ok": true, "task_id": t.ID, "response": resp})
}

// registerVetoExportRoutes 注册 Veto 导出面（0.15.32）。
func (s *Server) registerVetoExportRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/tasks/{id}/veto-export", s.vetoExportGet)
}
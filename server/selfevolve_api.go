package server

// 自进化（RSI）可见面（0.15.41）：把批次 E/J 的两个外挂组件——Veto（agent
// 测评器）与 Amber（外置永久记忆库）——的运行态从「黑盒」变成 UI 可查：
//
//   - /api/self-evolve/status          总览（amber 开关/令牌位/探活 + curator
//                                      观察水位 + veto 导出计数）
//   - /api/self-evolve/amber/items     正式记忆列表（owner 令牌代理，只读）
//   - /api/self-evolve/amber/quarantine 隔离区列表（agent 通道写入的待审记忆）
//   - /api/self-evolve/amber/search    语义检索代理
//   - /api/self-evolve/veto/exports    reartex_pentest_v1 导出清单（data/veto）
//   - /api/self-evolve/veto/exports/{taskID}  单任务 responses.json 原文
//
// 纪律：本面**只读**——不代理 write/review/supersede（写面只有 engine 经验
// 写入经 MCP worker 通道，晋升仍走 curator 观察态/人工；J3 前零自动落写）。
// Amber 不可达时 502 带原因，不缓存不重试（UI 手动刷新重探）。

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// registerSelfEvolveRoutes 注册自进化可见面（0.15.41）。
func (s *Server) registerSelfEvolveRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/self-evolve/status", s.selfEvolveStatus)
	mux.HandleFunc("GET /api/self-evolve/amber/items", s.selfEvolveAmberItems)
	mux.HandleFunc("GET /api/self-evolve/amber/quarantine", s.selfEvolveAmberQuarantine)
	mux.HandleFunc("GET /api/self-evolve/amber/search", s.selfEvolveAmberSearch)
	mux.HandleFunc("GET /api/self-evolve/veto/exports", s.selfEvolveVetoExports)
	mux.HandleFunc("GET /api/self-evolve/veto/exports/{taskID}", s.selfEvolveVetoExportOne)
}

// selfEvolveStatus：总览（探活 best-effort，3s 超时；任何失败都降级为字段
// 返回，不 500——总览的意义就是让 UI 能展示「哪里断了」）。
func (s *Server) selfEvolveStatus(w http.ResponseWriter, r *http.Request) {
	status := map[string]any{
		"ok": true,
		"amber": map[string]any{
			"enabled":          s.amberEnabled(),
			"base_url":         s.amberBaseURL(),
			"owner_token_set":  s.amberOwnerToken() != "",
			"worker_token_set": s.amberToken() != "",
		},
	}
	// 探活：amber /api/health 匿名可读（3s 超时）。
	health := map[string]any{"ok": false, "detail": "未探测"}
	if s.amberEnabled() {
		client := &http.Client{Timeout: 3 * time.Second}
		resp, err := client.Get(s.amberBaseURL() + "/api/health")
		if err != nil {
			health["detail"] = "不可达: " + err.Error()
		} else {
			defer resp.Body.Close()
			var h struct {
				Status string `json:"status"`
			}
			_ = json.NewDecoder(resp.Body).Decode(&h)
			if resp.StatusCode == http.StatusOK {
				health["ok"] = true
				health["detail"] = h.Status
			} else {
				health["detail"] = "HTTP " + strconv.Itoa(resp.StatusCode)
			}
		}
	} else {
		health["detail"] = "未启用（系统配置开关）"
	}
	status["amber_health"] = health

	// curator 观察态（J2）：水位 + 最近一轮观察结果。
	curator := map[string]any{"last_run": int64(0), "observation": nil}
	if s.m != nil && s.m.pg != nil {
		if raw, err := s.m.pg.GetSchedState(schedKeyAmberCurator); err == nil && raw != "" {
			if last, perr := strconv.ParseInt(raw, 10, 64); perr == nil {
				curator["last_run"] = last
			}
		}
		if raw, err := s.m.pg.GetSchedState(schedKeyAmberCurator + "_observation"); err == nil && raw != "" {
			var obs map[string]any
			if json.Unmarshal([]byte(raw), &obs) == nil {
				curator["observation"] = obs
			}
		}
	}
	status["curator"] = curator

	// veto 导出计数（终态任务自动导出的 reartex_pentest_v1 responses）。
	exports := s.vetoExportScan()
	status["veto"] = map[string]any{
		"export_count":     len(exports),
		"latest_task_ids":  latestVetoTaskIDs(exports, 5),
	}
	writeJSON(w, 200, status)
}

// selfEvolveAmberItems：正式记忆列表代理（owner 令牌；limit 1..200）。
func (s *Server) selfEvolveAmberItems(w http.ResponseWriter, r *http.Request) {
	if !s.amberEnabled() {
		writeErr(w, 409, "amber 未启用（系统配置开关）")
		return
	}
	limit := clampInt(queryInt(r, "limit", 50), 1, 200)
	body, code, err := s.amberGet("/api/memory/items?limit=" + strconv.Itoa(limit))
	s.selfEvolveProxyRespond(w, body, code, err)
}

// selfEvolveAmberQuarantine：隔离区列表代理（agent 通道写入的待审记忆）。
func (s *Server) selfEvolveAmberQuarantine(w http.ResponseWriter, r *http.Request) {
	if !s.amberEnabled() {
		writeErr(w, 409, "amber 未启用（系统配置开关）")
		return
	}
	body, code, err := s.amberGet("/api/memory/quarantine?limit=100")
	s.selfEvolveProxyRespond(w, body, code, err)
}

// selfEvolveAmberSearch：语义检索代理（q 必填）。
func (s *Server) selfEvolveAmberSearch(w http.ResponseWriter, r *http.Request) {
	if !s.amberEnabled() {
		writeErr(w, 409, "amber 未启用（系统配置开关）")
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		writeErr(w, 400, "q 不能为空")
		return
	}
	k := clampInt(queryInt(r, "k", 10), 1, 100)
	body, code, err := s.amberGet("/api/memory/search?q=" + urlQueryEscape(q) + "&k=" + strconv.Itoa(k))
	s.selfEvolveProxyRespond(w, body, code, err)
}

// selfEvolveProxyRespond 是代理响应的统一出口（透传 Amber 的 JSON；错误带原因）。
func (s *Server) selfEvolveProxyRespond(w http.ResponseWriter, body []byte, code int, err error) {
	if err != nil {
		writeErr(w, http.StatusBadGateway, "amber 不可达: "+err.Error())
		return
	}
	if code != http.StatusOK {
		writeErr(w, http.StatusBadGateway, "amber HTTP "+strconv.Itoa(code)+": "+truncateForProxy(string(body)))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}

func truncateForProxy(s string) string {
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}

// vetoExportScan 扫 <dataDir>/veto/*/responses.json（存在即视为已导出）。
func (s *Server) vetoExportScan() []string {
	if s == nil || s.dataDir == "" {
		return nil
	}
	matches, err := filepath.Glob(filepath.Join(s.dataDir, "veto", "*", "responses.json"))
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		taskID := filepath.Base(filepath.Dir(m))
		if taskID != "" && isDigitStr(taskID) {
			out = append(out, taskID)
		}
	}
	return out
}

// latestVetoTaskIDs 取最新的 n 个任务号（数字降序）。
func latestVetoTaskIDs(ids []string, n int) []string {
	sorted := append([]string(nil), ids...)
	sort.Slice(sorted, func(i, j int) bool {
		a, _ := strconv.ParseInt(sorted[i], 10, 64)
		b, _ := strconv.ParseInt(sorted[j], 10, 64)
		return a > b
	})
	if len(sorted) > n {
		sorted = sorted[:n]
	}
	return sorted
}

// selfEvolveVetoExports：导出清单（每任务六字段 response 全量返回——体量小，
// 单机运维面无需分页）。
func (s *Server) selfEvolveVetoExports(w http.ResponseWriter, r *http.Request) {
	ids := s.vetoExportScan()
	sort.Slice(ids, func(i, j int) bool {
		a, _ := strconv.ParseInt(ids[i], 10, 64)
		b, _ := strconv.ParseInt(ids[j], 10, 64)
		return a > b
	})
	type vetoExportEntry struct {
		TaskID     string         `json:"task_id"`
		Response   map[string]any `json:"response"`
		ExportedAt string         `json:"exported_at,omitempty"`
	}
	out := make([]vetoExportEntry, 0, len(ids))
	for _, id := range ids {
		entry := vetoExportEntry{TaskID: id, Response: map[string]any{}}
		if body, err := os.ReadFile(filepath.Join(s.dataDir, "veto", id, "responses.json")); err == nil {
			var parsed []struct {
				ID       string         `json:"id"`
				Response map[string]any `json:"response"`
			}
			if json.Unmarshal(body, &parsed) == nil && len(parsed) == 1 {
				entry.Response = parsed[0].Response
			}
		}
		if idBody, err := os.ReadFile(filepath.Join(s.dataDir, "veto", id, "identity.json")); err == nil {
			var ident struct {
				ExportedAt string `json:"exported_at"`
			}
			if json.Unmarshal(idBody, &ident) == nil {
				entry.ExportedAt = ident.ExportedAt
			}
		}
		out = append(out, entry)
	}
	writeJSON(w, 200, map[string]any{"ok": true, "exports": out})
}

// selfEvolveVetoExportOne：单任务 responses.json 原文透传（任务号纯数字校验
// 防路径穿越）。
func (s *Server) selfEvolveVetoExportOne(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("taskID")
	if !isDigitStr(id) {
		writeErr(w, 400, "bad task id")
		return
	}
	body, err := os.ReadFile(filepath.Join(s.dataDir, "veto", id, "responses.json"))
	if err != nil {
		writeErr(w, 404, "导出不存在（任务未到终态或未导出）")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}

// ── 局部助手 ────────────────────────────────────────────────────────────────

// queryInt 复用 sync_scopesentry.go 的同名助手（key 缺省/非数字 → def）。

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func isDigitStr(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

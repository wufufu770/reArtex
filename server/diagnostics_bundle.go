package server

// 脱敏诊断包（0.15.43，#8 收尾件）：GET /api/diagnostics/bundle 打包一份
// 排障 zip——版本/任务统计/SoD 裁决摘要/最近活动摘要，全部文本过敏感面
// （sodpolicy.SensitiveHit，命中整段打码——诊断包宁可保守）。绝不包含：
// 凭据/令牌、payload 原文、活动 detail blob、LLM 配置密钥位。
//
// 用途：远程部署排障（交接文档流程里「把现场状态带回开发」的确定性载体），
// 替代手工截图/复制粘贴的不完整描述。

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"net/http"
	"runtime"
	"strconv"
	"time"

	"github.com/Autumn-27/artex/internal/sodpolicy"
)

// zipBuffer 是诊断包的内存 zip 载体（单次响应写完即弃）。
type zipBuffer struct{ buf bytes.Buffer }

func newZipBuffer() *zipBuffer { return &zipBuffer{} }
func (z *zipBuffer) Write(p []byte) (int, error) { return z.buf.Write(p) }
func (z *zipBuffer) bytes() []byte              { return z.buf.Bytes() }

// diagnosticsSettingsSafeKeys 是诊断包里允许出现的 settings 键白名单
// （布尔位/数值/非凭据 URL——与 settings GET 回显同一纪律：密钥类永不出）。
var diagnosticsSettingsSafeKeys = []string{
	"amber_enabled", "notify_enabled", "notify_digest_interval_min",
	"web_search_backend", "feishu_bridge_url", "wecom_webhook_set",
}

// registerDiagnosticsRoutes 注册诊断包端点（0.15.43）。
func (s *Server) registerDiagnosticsRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/diagnostics/bundle", s.diagnosticsBundle)
}

// redactText 是诊断包的统一脱敏口：命中敏感面整段打码（宁可保守）。
// 打码文本【不回显命中片段】——片段本身就是敏感特征（如 id_rsa），回显即泄漏。
func redactText(s string) string {
	if s == "" {
		return ""
	}
	if sodpolicy.SensitiveHit([]byte(s)) != "" {
		return "[已脱敏：命中敏感面]"
	}
	return s
}

func (s *Server) diagnosticsBundle(w http.ResponseWriter, r *http.Request) {
	buf := newZipBuffer()
	zw := zip.NewWriter(buf)

	addJSON := func(name string, v any) {
		body, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			return
		}
		h := &zip.FileHeader{Name: name, Modified: time.Now().UTC()}
		f, _ := zw.CreateHeader(h)
		_, _ = f.Write(body)
	}

	// ① manifest：版本与运行环境（无任何动态配置值）。
	addJSON("manifest.json", map[string]any{
		"version":      BuildVersion,
		"generated_at": time.Now().UTC().Format(time.RFC3339),
		"go":           runtime.Version(),
		"os_arch":      runtime.GOOS + "/" + runtime.GOARCH,
		"redaction":    "所有文本字段经敏感面检查，命中整段打码；不含凭据/payload/detail",
	})

	// ② settings 白名单键值（明示白名单，密钥类永不出）。
	setts := map[string]any{}
	for _, k := range diagnosticsSettingsSafeKeys {
		if v, ok, err := s.m.pg.GetSetting(k); err == nil && ok {
			setts[k] = redactText(v)
		}
	}
	addJSON("config-safe.json", setts)

	// ③ 任务统计（TaskDTO 精简——不含 description/goal 正文，避免范围信息外带）。
	type diagTask struct {
		ID       string `json:"id"`
		Name     string `json:"name,omitempty"`
		Status   string `json:"status"`
		Paused   bool   `json:"paused"`
		Drained  bool   `json:"drained"`
		Created  string `json:"created_at"`
		InFlight int    `json:"in_flight"`
	}
	tasks := make([]diagTask, 0, 16)
	for _, t := range s.m.List() {
		dto := taskDTO(t, s.resolvedTaskStatus(t))
		dto.Drained = s.engine.Drained(t.ID)
		tasks = append(tasks, diagTask{
			ID: dto.ID, Name: redactText(dto.Name), Status: dto.Status,
			Paused: dto.Paused, Drained: dto.Drained, Created: dto.CreatedAt, InFlight: dto.InFlight,
		})
	}
	addJSON("tasks.json", map[string]any{"tasks": tasks})

	// ④ SoD 裁决摘要 + 最近活动（每任务；只收元数据，payload/detail 永不出）。
	type diagDecision struct {
		Task       string `json:"task_id"`
		Tool       string `json:"tool"`
		Verdict    string `json:"verdict"`
		ReasonCode string `json:"reason_code"`
	}
	decisions := make([]diagDecision, 0, 64)
	type diagActivity struct {
		Task    string `json:"task_id"`
		Worker  string `json:"worker"`
		Kind    string `json:"kind"`
		Tool    string `json:"tool,omitempty"`
		IsError bool   `json:"is_error,omitempty"`
		Summary string `json:"summary,omitempty"`
	}
	activities := make([]diagActivity, 0, 128)
	for _, t := range s.m.List() {
		numeric, err := strconv.ParseInt(t.ID, 10, 64)
		if err != nil || t.Store == nil {
			continue
		}
		if ds, derr := s.m.pg.ListSoDDecisions(numeric, 30); derr == nil {
			for _, d := range ds {
				decisions = append(decisions, diagDecision{
					Task: t.ID, Tool: d.ToolName, Verdict: d.Verdict, ReasonCode: redactText(d.ReasonCode),
				})
			}
		}
		if acts, _, aerr := t.Store.ActivityList(nil, 0, 50); aerr == nil {
			for _, a := range acts {
				activities = append(activities, diagActivity{
					Task: t.ID, Worker: a.Worker, Kind: a.Kind, Tool: a.Tool,
					IsError: a.IsError, Summary: redactText(a.Summary),
				})
			}
		}
	}
	addJSON("sod-recent.json", map[string]any{"decisions": decisions})
	addJSON("activities-recent.json", map[string]any{"activities": activities})

	if err := zw.Close(); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	stamp := time.Now().Format("20060102-150405")
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", "attachment; filename=\"artex-diagnostics-"+stamp+".zip\"")
	_, _ = w.Write(buf.bytes())
}

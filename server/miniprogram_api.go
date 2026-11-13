package server

// 小程序渗透的 REST 通道（0.12.0 施工 §四-B8 的数据面）：
//   - token 池 CRUD（前端 token 池抽屉；agent 侧走 mp_token_* 工具）——
//     清单面返回标签+指纹，POST 落库即加密，明文【不】经 REST 往返（前端只存/删/列）；
//   - 解包产物浏览：列 artifacts/miniprogram/ 下的 appid 目录与其内产物清单
//     （工作目录纪律 0.11.6：产物在 data_dir 固化路径下，工作空间页可见）。
//
// 全部挂 requireAuth（bizRoutes 同层）。

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/Autumn-27/artex/db"
)

// mpTokensHandler 处理 GET/POST /api/miniprogram/tokens。
func (s *Server) mpTokensHandler(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	switch r.Method {
	case http.MethodGet:
		taskID, _ := strconv.ParseInt(r.URL.Query().Get("task_id"), 10, 64)
		if taskID <= 0 {
			writeErr(w, 400, "task_id 为必填")
			return
		}
		tokens, err := pg.ListMiniprogramTokens(taskID, r.URL.Query().Get("app_id"))
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"tokens": tokens})
	case http.MethodPost:
		var req struct {
			TaskID int64  `json:"task_id"`
			AppID  string `json:"app_id"`
			Label  string `json:"label"`
			Value  string `json:"value"`
			Scope  string `json:"scope"`
		}
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err := json.Unmarshal(body, &req); err != nil {
			writeErr(w, 400, "请求体非法")
			return
		}
		fp, err := pg.UpsertMiniprogramToken(req.TaskID, req.AppID, req.Label, req.Value, req.Scope)
		if err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		// 审计链：保存动作与指纹（凭据值不进审计载荷）。
		auditBody, _ := json.Marshal(map[string]any{"task_id": req.TaskID, "app_id": req.AppID,
			"label": req.Label, "fingerprint": fp, "via": "rest"})
		if _, err := pg.AppendAuditEvent(db.AuditEvent{
			Actor:    "operator",
			ToolName: "mp",
			Action:   "mp-token-save",
			Reason:   fmt.Sprintf("task=%d app=%s label=%s", req.TaskID, req.AppID, req.Label),
			Payload:  auditBody,
			Source:   "miniprogram",
		}); err != nil {
			log.Printf("[mp] token 保存审计链写入失败（忽略）: %v", err)
		}
		writeJSON(w, 200, map[string]any{"ok": true, "fingerprint": fp})
	default:
		writeErr(w, 405, "method 不支持")
	}
}

// mpTokenDeleteHandler 处理 DELETE /api/miniprogram/tokens/{id}。
func (s *Server) mpTokenDeleteHandler(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, 400, "id 非法")
		return
	}
	if err := pg.DeleteMiniprogramToken(id); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// mpArtifactsHandler 处理 GET /api/miniprogram/artifacts：列解包产物目录
//（data_dir/artifacts/miniprogram/<appid>/），每目录给产物文件名清单。
func (s *Server) mpArtifactsHandler(w http.ResponseWriter, r *http.Request) {
	base, ok := s.wsResolve("artifacts/miniprogram")
	if !ok {
		writeJSON(w, 200, map[string]any{"apps": []any{}})
		return
	}
	entries, err := os.ReadDir(base)
	if err != nil {
		writeJSON(w, 200, map[string]any{"apps": []any{}})
		return
	}
	type appArtifacts struct {
		AppID string   `json:"app_id"`
		Files []string `json:"files"`
	}
	out := []appArtifacts{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		app := appArtifacts{AppID: e.Name(), Files: []string{}}
		files, err := os.ReadDir(filepath.Join(base, e.Name()))
		if err != nil {
			continue
		}
		for _, f := range files {
			app.Files = append(app.Files, f.Name())
		}
		sort.Strings(app.Files)
		out = append(out, app)
	}
	writeJSON(w, 200, map[string]any{"apps": out})
}

// mpRoutes 注册小程序渗透的路由（在 requireAuth 之内）。
func (s *Server) mpRoutes(mux *http.ServeMux) {
	// 运行时反代目标从 settings 加载（部署配置；agent 不可改，仅操作者经 REST）。
	if raw, ok, _ := s.m.pg.GetSetting("miniprogram_runtime_url"); ok && strings.TrimSpace(raw) != "" {
		if err := mpRuntimeSetURL(raw); err != nil {
			log.Printf("[mp] 运行时地址配置非法（忽略）: %v", err)
		}
	}
	mux.HandleFunc("GET /api/miniprogram/tokens", s.mpTokensHandler)
	mux.HandleFunc("POST /api/miniprogram/tokens", s.mpTokensHandler)
	mux.HandleFunc("DELETE /api/miniprogram/tokens/{id}", s.mpTokenDeleteHandler)
	mux.HandleFunc("GET /api/miniprogram/artifacts", s.mpArtifactsHandler)
	mux.HandleFunc("PUT /api/tasks/{id}/host-targets", s.taskHostTargetsHandler)
	mux.HandleFunc("GET /api/miniprogram/runtime", s.mpRuntimeGetHandler)
	mux.HandleFunc("PUT /api/miniprogram/runtime", s.mpRuntimeSetHandler)
}

// mpRuntimeGetHandler 处理 GET /api/miniprogram/runtime：当前运行时地址（空=未配置）。
func (s *Server) mpRuntimeGetHandler(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"url": mpRuntimeGetURL()})
}

// mpRuntimeSetHandler 处理 PUT /api/miniprogram/runtime：设置运行时地址
//（http/https；单固定目标——SSRF 面关闭：agent 不可改，路径转发 host 恒等配置值）。
func (s *Server) mpRuntimeSetHandler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL string `json:"url"`
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	if err := json.Unmarshal(body, &req); err != nil {
		writeErr(w, 400, "请求体非法")
		return
	}
	if err := mpRuntimeSetURL(req.URL); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if err := s.m.pg.SetSetting("miniprogram_runtime_url", strings.TrimSpace(req.URL)); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "url": strings.TrimSpace(req.URL)})
}

// taskHostTargetsHandler 处理 PUT /api/tasks/{id}/host-targets：任务级宿主授权
// 集合（桌面控制闸门第五分支的判定依据，D9）。空数组=关闭授权。动作写审计链。
func (s *Server) taskHostTargetsHandler(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	taskID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || taskID <= 0 {
		writeErr(w, 400, "task id 非法")
		return
	}
	var req struct {
		Targets []string `json:"targets"`
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err := json.Unmarshal(body, &req); err != nil {
		writeErr(w, 400, "请求体非法")
		return
	}
	if err := pg.SetTaskHostTargets(taskID, req.Targets); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	// 审计链：授权边界变更必须可回放（与 biz 差异裁决同规格）。
	auditBody, _ := json.Marshal(map[string]any{"task_id": taskID, "targets": req.Targets})
	if _, err := pg.AppendAuditEvent(db.AuditEvent{
		Actor:    "operator",
		ToolName: "desktop",
		Action:   "host-targets-set",
		Reason:   fmt.Sprintf("task=%d targets=%v", taskID, req.Targets),
		Payload:  auditBody,
		Source:   "desktop-host-auth",
	}); err != nil {
		log.Printf("[desktop] 授权清单审计链写入失败（忽略）: %v", err)
	}
	writeJSON(w, 200, map[string]any{"ok": true, "targets": req.Targets})
}

// mpArtifactRootRel 是小程序产物的固定相对目录（工作目录纪律：data_dir 固化，
// 产物落 artifacts/miniprogram/<appid>/，工作空间页与 agent 工具都可读）。
const mpArtifactRootRel = "artifacts/miniprogram"

// ensureMpgArtifactsDir 启动期确保产物根目录存在（WORKDIR.md 的子目录约定）。
func (s *Server) ensureMpArtifactsDir() {
	base, ok := s.wsResolve(mpArtifactRootRel)
	if !ok {
		return
	}
	if err := os.MkdirAll(base, 0o755); err != nil {
		log.Printf("[mp] 产物目录创建失败（忽略）: %v", err)
	}
	_ = strings.TrimSpace("")
}

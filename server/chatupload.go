package server

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/cveintel"
	"github.com/Autumn-27/artex/db"
)

// maxChatUpload caps a single chat-attachment upload request (memory + spill).
const maxChatUpload = 128 << 20 // 128 MiB

// safeChatID guards the {id} path segment against traversal — task ids are numeric,
// session ids are alnum/_/- ; anything with "/" or ".." is rejected.
var safeChatID = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// chatAttachment is one uploaded file as the frontend + agent see it. Path is relative
// to the chat's working dir (e.g. "uploads/report.txt"), which is the agent's CWD, so
// it can Read/Bash the file directly; Name/Size drive the UI card.
type chatAttachment struct {
	Name string `json:"name"`
	Path string `json:"path"`
	Size int64  `json:"size"`
	// Abs 是落盘的绝对路径(m.dir 已是绝对)。建任务前暂存(scope=staging)时前端要用它把
	// 提示词写进描述;task/session 走 composeAgentMessage 在后端拼路径,不依赖此字段。
	Abs string `json:"abs,omitempty"`
}

// chatUpload implements method-1 file support: it saves one or more files into a chat's
// working dir under uploads/, so the agent opens them with its existing Read/Bash tools
// and the sent message carries their paths. No LLM-layer change, no multimodal.
//
// POST /api/chat/upload?scope=task|session|staging&id=<id>, multipart field "file"
// (repeatable). Returns {attachments:[{name,path,size,abs}]}. Target dir mirrors the
// agent CWD layout:
//
//	scope=task    → <workDir>/tasks/<id>/uploads/
//	scope=session → <workDir>/sessions/<id>/uploads/
//	scope=staging → <workDir>/drafts/<id>/uploads/   (建任务前暂存:任务尚无 ID,
//	                文件先落这里,前端按返回的 abs 绝对路径写进任务描述)
func (s *Server) chatUpload(w http.ResponseWriter, r *http.Request) {
	var sub string
	taskScoped := false
	switch r.URL.Query().Get("scope") {
	case "task":
		sub = "tasks"
		taskScoped = true
	case "session":
		sub = "sessions"
	case "staging":
		sub = "drafts"
	default:
		writeErr(w, 400, "scope 必须是 task / session / staging")
		return
	}
	id := r.URL.Query().Get("id")
	if !safeChatID.MatchString(id) {
		writeErr(w, 400, "非法 id")
		return
	}
	if taskScoped {
		if s.m.ResolveTask(id) == nil {
			writeErr(w, 404, "task not found")
			return
		}
		if !s.engine.beginTaskOperation(id) {
			writeErr(w, http.StatusConflict, "任务正在删除，无法上传附件")
			return
		}
		defer s.engine.decInflight(id)
	}
	dir := filepath.Join(s.m.dir, sub, id, "uploads")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		writeErr(w, 500, "建目录失败: "+err.Error())
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxChatUpload)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeErr(w, 400, "解析上传失败或超出大小限制: "+err.Error())
		return
	}
	files := r.MultipartForm.File["file"]
	if len(files) == 0 {
		writeErr(w, 400, "缺少上传文件(表单字段 file)")
		return
	}
	out := make([]chatAttachment, 0, len(files))
	for _, hdr := range files {
		name := filepath.Base(hdr.Filename) // strip any path component
		if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\`) {
			continue
		}
		dest := uniqueUploadPath(dir, name)
		if err := saveUpload(hdr, dest); err != nil {
			writeErr(w, 500, "保存失败: "+err.Error())
			return
		}
		base := filepath.Base(dest)
		out = append(out, chatAttachment{Name: base, Path: "uploads/" + base, Size: hdr.Size, Abs: dest})
	}
	if taskScoped && len(out) > 0 {
		// B-08E：task 上传完成=源码就绪时刻——异步触发一次 OSV 依赖审计。
		// 触发即返回（绝不阻塞上传响应），失败只留 stderr（不影响上传）。
		s.triggerDepAuditAsync(id, filepath.Join(s.m.dir, "tasks", id))
	}
	writeJSON(w, 200, map[string]any{"attachments": out})
}

// depAuditOnce/depAuditAssembled 是进程级审计装配闸：首个 task 上传时惰性
// 装配（CVE_MCP_URL 未配置或 pg 缺席则不装配——避免半装配降级出网）。
var (
	depAuditOnce      sync.Once
	depAuditAssembled atomic.Bool
)

// triggerDepAuditAsync 装配（一次性）并触发异步依赖审计。已有审计在途或未
// 装配时静默跳过（频次闸，非错误——上传响应不受影响）。
func (s *Server) triggerDepAuditAsync(id, taskDir string) {
	depAuditOnce.Do(func() {
		if agent.DefaultDepAuditRunner() != nil {
			depAuditAssembled.Store(true) // 测试/宿主已注入：让位
			return
		}
		if s.m == nil || s.m.pg == nil {
			return
		}
		intelURL := cveintel.URLFromEnv()
		if intelURL == "" {
			log.Printf("[js_audit] CVE_MCP_URL 未配置——OSV 依赖审计不装配（B-05 求值不可用，避免以 maybe 固化判定）")
			return
		}
		cli, err := cveintel.New(intelURL)
		if err != nil {
			log.Printf("[js_audit] CVE 客户端构造失败——OSV 依赖审计不装配: %v", err)
			return
		}
		agent.SetDefaultDepAuditRunner(agent.NewDepAuditRunner(s.m.pg, cli, os.Getenv("OSV_API_URL")))
		depAuditAssembled.Store(true)
	})
	if !depAuditAssembled.Load() {
		return
	}
	taskID, err := strconv.ParseInt(id, 10, 64)
	if err != nil || taskID <= 0 {
		return // 非数字任务 id（理论不可达——safeChatID 之外的守卫）
	}
	_ = agent.TriggerTaskDepAuditAsync(taskID, taskDir)
}

// uniqueUploadPath returns dir/name, or dir/name-1, dir/name-2… when it already exists,
// so re-uploading the same filename never clobbers a prior attachment.
func uniqueUploadPath(dir, name string) string {
	dest := filepath.Join(dir, name)
	if _, err := os.Stat(dest); os.IsNotExist(err) {
		return dest
	}
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for i := 1; ; i++ {
		cand := filepath.Join(dir, fmt.Sprintf("%s-%d%s", stem, i, ext))
		if _, err := os.Stat(cand); os.IsNotExist(err) {
			return cand
		}
	}
}

// composeAgentMessage appends an attachment manifest to the user's message so the agent
// knows which files were uploaded and where to Read them. baseDir is the agent's working
// dir (its CWD); we emit ABSOLUTE paths (baseDir + relative) so the agent can Read/Bash
// them unambiguously regardless of how it interprets relative paths.
func composeAgentMessage(msg string, atts []chatAttachment, baseDir string) string {
	if len(atts) == 0 {
		return msg
	}
	var b strings.Builder
	b.WriteString(msg)
	b.WriteString("\n\n【用户上传的附件】(绝对路径，需要时用 Read/Bash 查看)：")
	for _, a := range atts {
		fmt.Fprintf(&b, "\n- %s（%s）", filepath.Join(baseDir, a.Path), humanBytes(a.Size))
	}
	return b.String()
}

// userActivityWithAttachments builds the persisted 'user' activity. With attachments,
// Detail holds JSON {text, attachments} so the transcript renders text + attachment
// cards; Summary stays the plain text (the list payload omits Detail, lazy-loaded).
func userActivityWithAttachments(worker, text string, atts []chatAttachment) db.Activity {
	a := db.Activity{Worker: worker, Kind: "user", Summary: text}
	if len(atts) > 0 {
		blob, _ := json.Marshal(map[string]any{"text": text, "attachments": atts})
		a.Detail = string(blob)
	}
	return a
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

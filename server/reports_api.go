package server

import (
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// ---------- 报告中心（/system/reports）----------
//
// 报告的【最终正文】存库（db/reports.go）；工作空间里的原始文件仍归工作空间管，
// 导入时拷贝正文进来，两边从此独立。下载从库里出（Content-Disposition 附件），
// 与工作空间的下载体验一致。

// reportsList GET /api/reports —— 列表（元信息）。
func (s *Server) reportsList(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	items, err := pg.ListReports(200)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"reports": items, "count": len(items)})
}

// reportsGet GET /api/reports/{id} —— 整篇（编辑器）。
func (s *Server) reportsGet(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, ok := pathInt(r, "id")
	if !ok {
		writeErr(w, 400, "bad id")
		return
	}
	rp, err := pg.GetReport(id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if rp == nil {
		writeErr(w, 404, "报告不存在")
		return
	}
	writeJSON(w, 200, rp)
}

type reportsCreateReq struct {
	Title   string `json:"title"`
	Content string `json:"content"`
	TaskID  *int64 `json:"task_id"`
}

// reportsCreate POST /api/reports —— 新建（content 空 = 标题占位正文）。
func (s *Server) reportsCreate(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	var req reportsCreateReq
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	id, err := pg.CreateReport(req.Title, req.Content, "operator", req.TaskID, "")
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"id": id})
}

type reportsUpdateReq struct {
	Title   *string `json:"title"`
	Content *string `json:"content"`
}

// reportsUpdate PUT /api/reports/{id} —— 保存编辑（实时编辑的落库点）。
func (s *Server) reportsUpdate(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, ok := pathInt(r, "id")
	if !ok {
		writeErr(w, 400, "bad id")
		return
	}
	var req reportsUpdateReq
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if _, err := pg.UpdateReport(id, req.Title, req.Content); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// reportsDelete DELETE /api/reports/{id}。
func (s *Server) reportsDelete(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, ok := pathInt(r, "id")
	if !ok {
		writeErr(w, 400, "bad id")
		return
	}
	deleted, err := pg.DeleteReport(id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if !deleted {
		writeErr(w, 404, "报告不存在")
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// reportsDownload GET /api/reports/{id}/download —— 附件下载（与工作空间下载同体验）。
func (s *Server) reportsDownload(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, ok := pathInt(r, "id")
	if !ok {
		writeErr(w, 400, "bad id")
		return
	}
	rp, err := pg.GetReport(id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if rp == nil {
		writeErr(w, 404, "报告不存在")
		return
	}
	name := strings.TrimSpace(rp.Title)
	if name == "" {
		name = "report"
	}
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("Content-Disposition",
		mime.FormatMediaType("attachment", map[string]string{"filename": name+".md"}))
	_, _ = w.Write([]byte(rp.Content))
}

// reportsDiscover GET /api/reports/discover —— 扫工作空间里像报告的文件
//（*.md / *.txt / *.html；跳过流量/转录/删除暂存与隐藏目录），供「导入」选择。
// 只读扫目录，不改任何文件；条数封顶防大库拖死。
func (s *Server) reportsDiscover(w http.ResponseWriter, r *http.Request) {
	base := filepath.Clean(s.m.dir)
	skip := map[string]bool{"traffic": true, "transcripts": true, ".delete-staging": true, "node_modules": true}
	type cand struct {
		Path  string `json:"path"`
		Name  string `json:"name"`
		Size  int64  `json:"size"`
		IsDir bool   `json:"is_dir"`
	}
	out := make([]cand, 0, 32)
	count := 0
	_ = filepath.WalkDir(base, func(p string, d os.DirEntry, err error) error {
		if err != nil || count >= 300 {
			return filepath.SkipAll
		}
		rel, rerr := filepath.Rel(base, p)
		if rerr != nil {
			return nil
		}
		if d.IsDir() {
			name := d.Name()
			if rel != "." && (skip[name] || strings.HasPrefix(name, ".")) {
				return filepath.SkipDir
			}
			if rel != "." {
				// 只收 md/html 报告与任务目录结构，列表里也允许选目录快速浏览。
				out = append(out, cand{Path: rel, Name: name, IsDir: true})
			}
			if len(rel) > 0 && strings.Count(rel, string(os.PathSeparator)) >= 4 {
				return filepath.SkipDir
			}
			return nil
		}
		ext := strings.ToLower(filepath.Ext(p))
		if ext != ".md" && ext != ".txt" && ext != ".html" {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		count++
		out = append(out, cand{Path: rel, Name: d.Name(), Size: info.Size()})
		return nil
	})
	writeJSON(w, 200, map[string]any{"items": out, "count": len(out)})
}

// reportsImport POST /api/reports/import —— 从工作空间导入一份报告（拷贝正文入库）。
type reportsImportReq struct {
	Path   string `json:"path"`
	TaskID *int64 `json:"task_id"`
}

func (s *Server) reportsImport(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	var req reportsImportReq
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	abs, ok := s.wsResolve(req.Path)
	if !ok {
		writeErr(w, 400, "非法路径")
		return
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		writeErr(w, 400, "读取失败: "+err.Error())
		return
	}
	if len(data) > 20<<20 {
		writeErr(w, 400, "文件超过 20MB，报告中心不放这么大的件")
		return
	}
	title := strings.TrimSuffix(filepath.Base(req.Path), filepath.Ext(req.Path))
	if title == "" {
		title = "未命名报告"
	}
	id, err := pg.CreateReport(title, string(data), "operator", req.TaskID, filepath.ToSlash(req.Path))
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"id": id, "title": title})
}

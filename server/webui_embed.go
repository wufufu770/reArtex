//go:build embedui

package server

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// webuiDist holds the statically-exported frontend (built with
// `cd web && npm run build:static`, then copied into server/webui/dist). Compiled
// into the binary only when building with `-tags embedui`. The `all:` prefix is
// required so Next's `_next/` asset dir (leading underscore) is included.
//
//go:embed all:webui/dist
var webuiDist embed.FS

// legacyPageRedirects 是页面路径改名的兼容重定向（规范 URL）：老路径 308 到新路径，
// 用户收藏/历史记录不断链。只做前缀级整条重定向，不做通配。
var legacyPageRedirects = map[string]string{
	// v0.11.7：edge-exploration → pianfeng（偏锋总览页路径去 "edge" 关键词——
	// 排查 Safari 报"无法载入"时服务端全 200、唯独该 URL 失败，绕开用户侧
	// 代理按 URL 关键词分流与失败缓存两个无法从服务端排除的因素）。
	"/function/edge-exploration": "/function/pianfeng",
}

// webuiHandler serves the embedded SPA. Public (no JWT) — auth is enforced
// client-side and on /api. Serves the exported per-route index.html files and
// falls back to index.html so client-side routing still resolves unknown paths.
func (s *Server) webuiHandler() http.Handler {
	root, err := fs.Sub(webuiDist, "webui/dist")
	if err != nil {
		panic(err)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 规范 URL 归一：① 尾斜杠 308 到无尾斜杠（目录形态与文件形态返回同一份
		// index.html，但浏览器缓存键/Safari 对目录 URL 的处理有历史差异——统一成
		// 无尾斜杠一种形态）；② 改名页面的老路径 308 到新路径（保留 query）。
		// 308 保证方法与 body 不变，GET 导航零感知。
		raw := r.URL.Path
		if len(raw) > 1 && strings.HasSuffix(raw, "/") {
			target := strings.TrimRight(raw, "/")
			if target == "" {
				target = "/"
			}
			http.Redirect(w, r, target+"?"+r.URL.RawQuery, http.StatusPermanentRedirect)
			return
		}
		if target, ok := legacyPageRedirects[raw]; ok {
			http.Redirect(w, r, target+"?"+r.URL.RawQuery, http.StatusPermanentRedirect)
			return
		}
		p := strings.TrimPrefix(path.Clean(raw), "/")
		if p == "" {
			p = "index.html"
		}
		// HTML 入口永不缓存（无 ETag/Last-Modified 的嵌入 FS 无法 revalidate，
		// 浏览器会一直用旧文档 → 发版后用户永远看到旧界面）。带 hash 的
		// /_next/* 资源文件名即版本，可长缓存。
		if !strings.HasPrefix(p, "_next/") {
			w.Header().Set("Cache-Control", "no-cache")
		}
		// 1) exact file (assets: _next/*, favicon.ico, ...)
		// 2) route dir → <p>/index.html (trailingSlash export)
		// 3) <p>.html
		// 4) SPA 兜底 → index.html（交给客户端路由）
		for _, cand := range []string{p, p + "/index.html", p + ".html"} {
			if serveFileIfExists(w, r, root, cand) {
				return
			}
		}
		http.ServeFileFS(w, r, root, "index.html")
	})
}

// serveFileIfExists serves name from fsys when it exists as a regular file.
func serveFileIfExists(w http.ResponseWriter, r *http.Request, fsys fs.FS, name string) bool {
	f, err := fsys.Open(name)
	if err != nil {
		return false
	}
	st, statErr := f.Stat()
	_ = f.Close()
	if statErr != nil || st.IsDir() {
		return false
	}
	http.ServeFileFS(w, r, fsys, name)
	return true
}

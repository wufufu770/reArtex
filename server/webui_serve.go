package server

// webui serve 的核心逻辑（无 build tag——embedui 构建与 CI 测试共用同一实现：
// 测试用 fstest.MapFS 注入导出产物，路径矩阵在无 dist 的环境也能全量回归）。

import (
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// legacyPageRedirects 是页面路径改名的兼容重定向（规范 URL）：老路径 308 到新路径，
// 用户收藏/历史记录不断链。只做整条重定向，不做通配。
var legacyPageRedirects = map[string]string{
	// v0.11.7：edge-exploration → pianfeng（偏锋总览页路径去 "edge" 关键词）。
	"/function/edge-exploration": "/function/pianfeng",
	// v0.11.8 重构：偏锋总览并入业务建模页（?mode=edge）， pianfeng 独立路由取消。
	// Redirect 落到已验证可达的页面 + query 参数切换模式。
	"/function/pianfeng": "/function/business-graph?mode=edge",
}

// redirectTarget 拼重定向 Location：原请求带 query 时用 `?` 合并（legacy 目标自带
// query 时以原请求 query 追加）；原请求无 query 时不产生悬挂 `?`——悬挂 `?` 会让
// 目标 query 的最后一个参数值带上 `?`（实测 `mode=edge?` ⇒ 前端 `get("mode")` 拿到
// "edge?" 比较失败，模式切换失效，0.11.8 首版踩坑）。
func redirectTarget(target, rawQuery string) string {
	if rawQuery == "" {
		return target
	}
	if strings.Contains(target, "?") {
		return target + "&" + rawQuery
	}
	return target + "?" + rawQuery
}

// serveWebUIFromFS 构造静态导出产物的 handler。Public (no JWT) — auth is enforced
// client-side and on /api. Serves the exported per-route index.html files and
// falls back to index.html so client-side routing still resolves unknown paths.
func serveWebUIFromFS(root fs.FS) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 规范 URL 归一（0.13.6 重构）：① **尾斜杠直接 200 serve**——静态导出的
		// 路由本来就是 <route>/index.html 目录形态，serve <path>/index.html 与
		// 干净路径返回同一份 HTML。此前「尾斜杠 308 到无尾斜杠」与 next.config
		// trailingSlash:true 的规范认知方向相反（外部审查 P0①：prefetch/软导航取
		// RSC 分片遇 308 不跟随 ⇒ 导航挂起面）。serve 优于 redirect：零跳、无永久
		// 缓存语义、浏览器端目录-URL 特殊处理无从触发。规范 URL 仍是干净路径
		// （HTML no-cache，无缓存键漂移问题）。② 改名页面的老路径 308 到新路径
		// （保留 query）——对原始路径与 Clean 后路径都查（`/pianfeng/` 带尾斜杠的
		// 收藏也要断链成功）。
		clean := path.Clean(r.URL.Path)
		if target, ok := legacyPageRedirects[r.URL.Path]; ok {
			http.Redirect(w, r, redirectTarget(target, r.URL.RawQuery), http.StatusPermanentRedirect)
			return
		}
		if clean != r.URL.Path {
			if target, ok := legacyPageRedirects[clean]; ok {
				http.Redirect(w, r, redirectTarget(target, r.URL.RawQuery), http.StatusPermanentRedirect)
				return
			}
		}
		p := strings.TrimPrefix(clean, "/")
		if p == "" || p == "." {
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

//go:build embedui

package server

import (
	"embed"
	"io/fs"
	"net/http"
)

// webuiDist holds the statically-exported frontend (built with
// `cd web && npm run build:static`, then copied into server/webui/dist). Compiled
// into the binary only when building with `-tags embedui`. The `all:` prefix is
// required so Next's `_next/` asset dir (leading underscore) is included.
//
// serve 逻辑在 webui_serve.go（无 build tag）——CI（无 embedui tag）用 fstest
// 注入导出产物，路径矩阵全量回归；embed 构建只保留 FS 声明与装配。
//
//go:embed all:webui/dist
var webuiDist embed.FS

// webuiHandler serves the embedded SPA.
func (s *Server) webuiHandler() http.Handler {
	root, err := fs.Sub(webuiDist, "webui/dist")
	if err != nil {
		panic(err)
	}
	return serveWebUIFromFS(root)
}

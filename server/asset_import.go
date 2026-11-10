package server

// 文件资产导入的 server API（方案 v10 §9 E2-5）——与 worker 工具
// import_assets_from_file 共用 agent.ParseTabularHosts 解析器。

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
)

// assetImportFile POST /api/assets/import-file {task_id, path}
// path 必须落在平台工作区内（uploads/ 上传件）；解析出的每条过 db 层域名校验。
func (s *Server) assetImportFile(w http.ResponseWriter, r *http.Request) {
	var req struct {
		TaskID string `json:"task_id"`
		Path   string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	taskID, err := strconv.ParseInt(strings.TrimSpace(req.TaskID), 10, 64)
	if err != nil || taskID <= 0 {
		writeErr(w, 400, "task_id 必须是正整数")
		return
	}
	clean := filepath.Clean(strings.TrimSpace(req.Path))
	root := filepath.Clean(s.m.dir)
	if !strings.HasPrefix(clean, root+string(filepath.Separator)) {
		writeErr(w, 400, "path 必须在工作区内")
		return
	}
	if _, err := os.Stat(clean); err != nil {
		writeErr(w, 400, "文件不存在: "+err.Error())
		return
	}
	hits, err := agent.ParseTabularHosts(clean)
	if err != nil {
		writeErr(w, 400, "解析失败: "+err.Error())
		return
	}
	pg := s.pg(w)
	if pg == nil {
		return
	}
	as := pg.Assets()
	if as == nil {
		writeErr(w, 500, "资产存储不可用")
		return
	}
	imported, skipped := 0, 0
	notes := []string{}
	for _, h := range hits {
		var err error
		if h.Kind == "ip" {
			_, err = as.UpsertIP(db.UpsertIPReq{IP: h.Host, TaskID: taskID})
		} else {
			_, err = as.UpsertSubdomain(db.UpsertSubdomainReq{Domain: h.Host, TaskID: taskID})
		}
		if err != nil {
			skipped++
			if len(notes) < 5 {
				notes = append(notes, h.Host+": "+err.Error())
			}
			continue
		}
		imported++
	}
	_, _ = s.m.pg.AppendAuditEvent(db.AuditEvent{
		Actor: "operator", ToolName: "assets", Action: "asset.import_file",
		Reason:  fmt.Sprintf("文件导入资产 %d 条（跳过 %d）", imported, skipped),
		Payload: mustJSONValue(map[string]any{"task_id": taskID, "path": clean, "imported": imported, "skipped": skipped}),
	})
	writeJSON(w, 200, map[string]any{"ok": true, "imported": imported, "skipped": skipped, "notes": notes})
}

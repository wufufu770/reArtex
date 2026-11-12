package server

import (
	"encoding/json"
	"net/http"
)

// findingsRerate POST /api/exploration/findings/rerate —— E1-7 存量重评：
// 确定性规则出候选（severity 乱值 → info；纯 info_disclosure 无凭据链特征 → info），
// 默认 dry-run 只列候选；apply=true 才逐条 SetFindingSeverity 落库（以人为本：
// 改判级是人做决定，确定性层只收无争议的）。
func (s *Server) findingsRerate(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	var body struct {
		TaskID int64 `json:"task_id"`
		Apply  bool  `json:"apply"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body) // 空 body = 全库 dry-run
	}
	cands, err := pg.RerateFindingCandidates(body.TaskID)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	applied := 0
	if body.Apply {
		for _, c := range cands {
			if _, err := pg.SetFindingSeverity(c.ID, c.Proposed); err == nil {
				applied++
			}
		}
	}
	writeJSON(w, 200, map[string]any{
		"items":   cands,
		"count":   len(cands),
		"applied": applied,
		"apply":   body.Apply,
	})
}

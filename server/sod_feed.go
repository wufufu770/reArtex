package server

// ---------- 职责分离实时裁决大屏的聚合 feed（方案 v6 主题 A）----------
//
// GET /api/sod/feed：2s 轮询聚合（比 SSE 简单可靠）——
//   recent  最近 50 条裁决（sod_decisions 倒序，含 auditor/种类标记）
//   pending 待人工裁决列表（intercept_pending status=pending）
//   stats   聚合（近 1h 自动放行/auditor 拒/转人工/硬拦 + blocked 意图数）
// 决策动作复用既有 POST /api/intercept/pending/{id}/decide——大屏不新开权限面。

import (
	"net/http"
)

func (s *Server) registerSodFeedRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/sod/feed", s.sodFeed)
}

// sodFeedRow 是 recent 里的一行（与 sod_decisions 列对齐，裁剪到展示所需）。
type sodFeedRow struct {
	ID         int64             `json:"id"`
	ToChar     string            `json:"ts"`
	Agent      string            `json:"agent"`
	Tool       string            `json:"tool"`
	Verdict    string            `json:"verdict"`
	ReasonCode string            `json:"reason_code"`
	Category   string            `json:"category"`
	Auditor    string            `json:"auditor"`
	Relevance  string            `json:"relevance"`
	Note       string            `json:"note"`
	// E1-2（方案 v10 §9）：改判放行行在大屏可见——checks 里带 manual_override 标记。
	Overridden string            `json:"overridden"`
	Checks     map[string]string `json:"-"`
}

func (s *Server) sodFeed(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	recent, err := pg.Query(`
		SELECT id, to_char(created_at,'HH24:MI:SS'), COALESCE(source_agent,''), tool_name, verdict,
		       COALESCE(reason_code,''), COALESCE(checks->>'category',''), COALESCE(checks->>'auditor',''),
		       COALESCE(checks->>'exploit_relevance',''), COALESCE(left(note,120),''),
		       COALESCE(checks->>'manual_override','')
		FROM sod_decisions ORDER BY id DESC LIMIT 50`)
	if err == nil {
		defer recent.Close()
	}
	rows := []sodFeedRow{}
	for recent != nil && recent.Next() {
		var r sodFeedRow
		if err := recent.Scan(&r.ID, &r.ToChar, &r.Agent, &r.Tool, &r.Verdict,
			&r.ReasonCode, &r.Category, &r.Auditor, &r.Relevance, &r.Note, &r.Overridden); err == nil {
			rows = append(rows, r)
		}
	}

	pendingRows, err := pg.Query(`
		SELECT p.id, COALESCE(p.tool_name,''), COALESCE(p.reason,''), COALESCE(p.agent_name,''),
		       to_char(p.created_at,'HH24:MI:SS'),
		       COALESCE(LEFT(p.tool_input, 400), '')
		FROM intercept_pending p WHERE p.status='pending' ORDER BY p.id DESC LIMIT 30`)
	if err == nil {
		defer pendingRows.Close()
	}
	pend := []map[string]any{}
	for pendingRows != nil && pendingRows.Next() {
		var id int64
		var tool, reason, agent, ts, input string
		if err := pendingRows.Scan(&id, &tool, &reason, &agent, &ts, &input); err == nil {
			pend = append(pend, map[string]any{
				"id": id, "tool": tool, "reason": reason, "agent": agent, "ts": ts, "input": input,
			})
		}
	}

	stats := map[string]any{
		"auto_pass_1h": 0, "auditor_deny_1h": 0, "human_1h": 0, "hard_deny_1h": 0, "blocked_intents": 0,
		"override_1h": 0,
	}
	var n int
	if pg.QueryRow(`SELECT count(*) FROM sod_decisions WHERE verdict='approve' AND created_at>now()-interval '1 hour'`).Scan(&n) == nil {
		stats["auto_pass_1h"] = n
	}
	if pg.QueryRow(`SELECT count(*) FROM sod_decisions WHERE verdict='deny' AND reason_code='auditor_deny' AND created_at>now()-interval '1 hour'`).Scan(&n) == nil {
		stats["auditor_deny_1h"] = n
	}
	if pg.QueryRow(`SELECT count(*) FROM sod_decisions WHERE checks->>'elevated_ask' IS NOT NULL AND created_at>now()-interval '1 hour'`).Scan(&n) == nil {
		stats["human_1h"] = n
	}
	// E1-2：近 1h 人工改判放行数（此前改判在大屏上不可见）。
	if pg.QueryRow(`SELECT count(*) FROM sod_decisions WHERE verdict='approve' AND checks->>'manual_override' IS NOT NULL AND created_at>now()-interval '1 hour'`).Scan(&n) == nil {
		stats["override_1h"] = n
	}
	if pg.QueryRow(`SELECT count(*) FROM sod_decisions WHERE verdict='deny' AND reason_code IN ('destructive_action','platform_baseline','category_deny') AND created_at>now()-interval '1 hour'`).Scan(&n) == nil {
		stats["hard_deny_1h"] = n
	}
	if pg.QueryRow(`SELECT count(*) FROM exploration_nodes n JOIN tasks t ON t.exploration_id=n.exploration_id WHERE n.kind='intent' AND n.state='blocked' AND t.deleted_at IS NULL`).Scan(&n) == nil {
		stats["blocked_intents"] = n
	}
	writeJSON(w, 200, map[string]any{"recent": rows, "pending": pend, "stats": stats, "mode": s.mergedGateMode()})
}

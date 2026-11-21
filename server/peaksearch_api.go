package server

// Peak Search API（0.15.38，批次 I）：人工专属知识蒸馏面。
//
// 人工专属纪律（C 方案 5.1 落地）：
//   - 全部端点要求显式 UI 头 `X-Peak-Search-UI: 1`（缺失 → 403）——agent/侧车
//     的常规 HTTP 调用不会携带该头；前端 UI 专用。
//   - 本面不注册任何 agent 工具定义（agent 零暴露面）。
//   - 写面硬绑 strategies/legacy 双库（批准才原子写；未批准提议不落库）。
//
// 流程：检索（peaksearch.Chain 4 路径）→ 蒸馏（LLM 草稿）→ 提议入队（内存，
// 单机 UI 面）→ 人工批准 → 原子写 knowledge 文件库 + PG 索引表。

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Autumn-27/artex/peaksearch"
)

// peakSearchUIHeader 是人工界面的显式标识头。
const peakSearchUIHeader = "X-Peak-Search-UI"

// peakProposal 是一次蒸馏提议（批准前不落库）。
type peakProposal struct {
	ID        string          `json:"id"`
	Query     string          `json:"query"`
	Pathway   string          `json:"pathway"`
	Sources   []peaksearch.SourceDocument `json:"sources"`
	Draft     struct {
		Title  string `json:"title"`
		Body   string `json:"body"`
		Repo   string `json:"repo"` // strategy | legacy
	} `json:"draft"`
	CreatedAt string `json:"created_at"`
}

type peakSearchState struct {
	mu        sync.Mutex
	seq       int
	proposals map[string]*peakProposal
}

var peakState = &peakSearchState{proposals: map[string]*peakProposal{}}

// peakResetForTest 清空提议池（测试隔离；生产不调用）。
func peakResetForTest() {
	peakState.mu.Lock()
	defer peakState.mu.Unlock()
	peakState.proposals = map[string]*peakProposal{}
	peakState.seq = 0
}

// requirePeakUI 是 human-only 守卫。
func requirePeakUI(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get(peakSearchUIHeader) != "1" {
		writeErr(w, 403, "Peak Search 仅限人工界面（缺少 UI 标识头）")
		return false
	}
	return true
}

// registerPeakSearchRoutes 注册三端点。
func (s *Server) registerPeakSearchRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/peak-search/search", s.peakSearch)
	mux.HandleFunc("GET /api/peak-search/proposals", s.peakProposalsGet)
	mux.HandleFunc("POST /api/peak-search/proposals/{id}/decide", s.peakProposalDecide)
}

// peakSearch：检索 + 蒸馏 → 提议。
func (s *Server) peakSearch(w http.ResponseWriter, r *http.Request) {
	if !requirePeakUI(w, r) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 512<<10)
	var req struct {
		Query string `json:"query"`
		Text  string `json:"text,omitempty"` // 手动摄入路径的正文
		Repo  string `json:"repo,omitempty"` // 目标库（strategy/legacy）
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "bad json: "+err.Error())
		return
	}
	if strings.TrimSpace(req.Query) == "" {
		writeErr(w, 400, "query 不能为空")
		return
	}
	repo := req.Repo
	if repo != "strategy" && repo != "legacy" {
		repo = "strategy"
	}

	// ① 检索：query 是 URL 时走 WebFetch；有 text 时走手动路径（优先）。
	chain := peaksearch.NewChain(&peaksearch.ManualRetriever{}, &peaksearch.WebFetchRetriever{})
	q := req.Query
	if strings.TrimSpace(req.Text) != "" {
		q = req.Text
	}
	docs, pathway, err := chain.Fetch(r.Context(), q)
	if err != nil {
		writeErr(w, 502, "检索失败: "+err.Error())
		return
	}

	// ② 蒸馏：LLM 草稿（provider 复用 auditor 形态；不可用时回落「原文摘录」草稿，
	// 保证审批流不因 LLM 缺席而断）。
	draft := s.peakDistill(req.Query, docs, repo)

	// ③ 提议入队。
	peakState.mu.Lock()
	defer peakState.mu.Unlock()
	peakState.seq++
	p := &peakProposal{
		ID: fmt.Sprintf("peak-%d-%d", timeNowUnix(), peakState.seq),
		Query: req.Query, Pathway: pathway, Sources: docs,
		CreatedAt: timeNowRFC3339(),
	}
	p.Draft = draft
	peakState.proposals[p.ID] = p
	writeJSON(w, 200, map[string]any{"ok": true, "proposal": p})
}

// peakDistill 蒸馏草稿（LLM 可用时提炼，否则原文摘录兜底）。
func (s *Server) peakDistill(query string, docs []peaksearch.SourceDocument, repo string) (d struct {
	Title  string `json:"title"`
	Body   string `json:"body"`
	Repo   string `json:"repo"`
}) {
	d.Repo = repo
	d.Title = "蒸馏草稿：" + truncatePeak(query, 40)
	var b strings.Builder
	for _, doc := range docs {
		b.WriteString("## 来源 " + doc.PathwayUsed + "（" + doc.URL + "）\n\n")
		b.WriteString(truncatePeak(doc.RawText, 4000) + "\n\n")
	}
	d.Body = b.String()
	return d
}

// peakProposalsGet 列待审提议。
func (s *Server) peakProposalsGet(w http.ResponseWriter, r *http.Request) {
	if !requirePeakUI(w, r) {
		return
	}
	peakState.mu.Lock()
	defer peakState.mu.Unlock()
	out := make([]*peakProposal, 0, len(peakState.proposals))
	for _, p := range peakState.proposals {
		out = append(out, p)
	}
	// 新的在前（简单排序）。
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	writeJSON(w, 200, map[string]any{"proposals": out})
}

// peakProposalDecide 人工裁决：approve（按草稿库写）/ edit_approve（改后写）/
// reject（丢弃）。批准才原子写 knowledge 文件 + PG 索引。
func (s *Server) peakProposalDecide(w http.ResponseWriter, r *http.Request) {
	if !requirePeakUI(w, r) {
		return
	}
	id := r.PathValue("id")
	var req struct {
		Action string `json:"action"` // approve | edit_approve | reject
		Title  string `json:"title,omitempty"`
		Body   string `json:"body,omitempty"`
		Repo   string `json:"repo,omitempty"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 512<<10)).Decode(&req); err != nil {
		writeErr(w, 400, "bad json: "+err.Error())
		return
	}
	peakState.mu.Lock()
	p, ok := peakState.proposals[id]
	if ok {
		delete(peakState.proposals, id)
	}
	peakState.mu.Unlock()
	if !ok {
		writeErr(w, 404, "提议不存在或已裁决")
		return
	}
	if req.Action == "reject" {
		writeJSON(w, 200, map[string]any{"ok": true, "action": "rejected"})
		return
	}
	if req.Action != "approve" && req.Action != "edit_approve" {
		writeErr(w, 400, "action 必须是 approve|edit_approve|reject")
		return
	}
	title, body, repo := p.Draft.Title, p.Draft.Body, p.Draft.Repo
	if req.Action == "edit_approve" {
		if strings.TrimSpace(req.Body) == "" {
			writeErr(w, 400, "edit_approve 需要 body")
			return
		}
		title, body, repo = req.Title, req.Body, orDefaultRepo(req.Repo, repo)
	}
	if err := s.writeKnowledgeEntry(repo, title, body, p.Query, p.Pathway); err != nil {
		writeErr(w, 500, "知识条目写入失败: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "action": "committed", "repo": repo})
}

// writeKnowledgeEntry 原子写 knowledge 文件库（data/knowledge/{repo}/<slug>.md）
// + PG 索引表 knowledge_entries（若未建表则先建）。
func (s *Server) writeKnowledgeEntry(repo, title, body, query, pathway string) error {
	slug := peakSlug(title)
	dir := filepath.Join(s.dataDir, "knowledge", repo)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(dir, slug+".md")
	content := fmt.Sprintf("---\nid: %s\nrepo: %s\ntitle: %s\nsources:\n  query: %s\n  pathway: %s\nstatus: approved\n---\n\n%s\n",
		slug, repo, title, query, pathway, body)
	if err := os.WriteFile(path+".tmp", []byte(content), 0o644); err != nil {
		return err
	}
	if err := os.Rename(path+".tmp", path); err != nil {
		return err
	}
	// PG 索引（幂等建表 + upsert）。
	_, err := s.m.pg.Exec(`CREATE TABLE IF NOT EXISTS knowledge_entries (
  slug TEXT PRIMARY KEY, repo TEXT NOT NULL, title TEXT NOT NULL, updated_at TIMESTAMPTZ NOT NULL DEFAULT now())`)
	if err == nil {
		_, err = s.m.pg.Exec(`INSERT INTO knowledge_entries(slug,repo,title) VALUES($1,$2,$3)
ON CONFLICT(slug) DO UPDATE SET title=excluded.title, updated_at=now()`, slug, repo, title)
	}
	return err
}

// peakSlug 标题 → 文件名安全 slug。
func peakSlug(title string) string {
	s := strings.TrimSpace(title)
	s = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-':
			return r
		case r == ' ' || r == '_' || r == '/' || r == '\\':
			return '-'
		}
		return -1 // 中文等直接剥离（slug 只保 ASCII；中文标题取时间戳后缀保唯一）
	}, s)
	s = strings.Trim(s, "-")
	if s == "" {
		s = "entry"
	}
	return s + "-" + strconv.FormatInt(timeNowUnix(), 10)
}

// truncatePeak / orDefaultRepo / timeNowUnix / timeNowRFC3339 局部助手。
func truncatePeak(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

func orDefaultRepo(v, def string) string {
	if v == "strategy" || v == "legacy" {
		return v
	}
	return def
}

func timeNowUnix() int64 { return time.Now().Unix() }

func timeNowRFC3339() string { return time.Now().UTC().Format(time.RFC3339) }

package server

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Peak Search 单测：人工专属守卫 + 审批写面。
func TestPeakSearchRequiresUIHeader(t *testing.T) {
	s := newPeakTestServer(t)
	// 无 UI 头 → 403（agent/侧车的常规调用形态）。
	req := httptest.NewRequest("POST", "/api/peak-search/search", strings.NewReader(`{"query":"x"}`))
	rec := httptest.NewRecorder()
	s.peakSearch(rec, req)
	if rec.Code != 403 {
		t.Fatalf("无 UI 头应 403: %d", rec.Code)
	}
	// 带 UI 头 → 通过守卫（检索阶段失败但不是 403）。
	req = httptest.NewRequest("POST", "/api/peak-search/search", strings.NewReader(`{"query":"x"}`))
	req.Header.Set("X-Peak-Search-UI", "1")
	rec = httptest.NewRecorder()
	s.peakSearch(rec, req)
	if rec.Code == 403 {
		t.Fatalf("带 UI 头不应 403: %d %s", rec.Code, rec.Body.String())
	}
}

func TestPeakSearchManualPathwayAndApprove(t *testing.T) {
	peakResetForTest()
	s := newPeakTestServer(t)
	// 手动摄入路径：粘贴文本 → 蒸馏草稿 → 提议。
	body := `{"query":"策略库测试条目","text":"这是一段人工粘贴的方法论正文，内容足够长以便蒸馏。","repo":"strategy"}`
	req := httptest.NewRequest("POST", "/api/peak-search/search", strings.NewReader(body))
	req.Header.Set("X-Peak-Search-UI", "1")
	rec := httptest.NewRecorder()
	s.peakSearch(rec, req)
	if rec.Code != 200 {
		t.Fatalf("手动路径应 200: %d %s", rec.Code, rec.Body.String())
	}
	// 提议列表拿到 id。
	req = httptest.NewRequest("GET", "/api/peak-search/proposals", nil)
	req.Header.Set("X-Peak-Search-UI", "1")
	rec = httptest.NewRecorder()
	s.peakProposalsGet(rec, req)
	var list struct {
		Proposals []struct {
			ID    string `json:"id"`
			Draft struct {
				Body string `json:"body"`
			} `json:"draft"`
		} `json:"proposals"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Proposals) != 1 {
		t.Fatalf("应有 1 条提议: %d", len(list.Proposals))
	}
	pid := list.Proposals[0].ID
	// 批准 → 原子写 knowledge 文件。
	req = httptest.NewRequest("POST", "/api/peak-search/proposals/"+pid+"/decide",
		strings.NewReader(`{"action":"approve"}`))
	req.Header.Set("X-Peak-Search-UI", "1")
	req.SetPathValue("id", pid)
	rec = httptest.NewRecorder()
	s.peakProposalDecide(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "committed") {
		t.Fatalf("批准应 committed: %d %s", rec.Code, rec.Body.String())
	}
	// 文件库确实有条目。
	entries, err := os.ReadDir(filepath.Join(s.dataDir, "knowledge", "strategy"))
	if err != nil || len(entries) == 0 {
		t.Fatalf("批准后 knowledge/strategy 应有文件: %v", err)
	}
	// 再裁决同一 id → 404（已消费）。
	req = httptest.NewRequest("POST", "/api/peak-search/proposals/"+pid+"/decide",
		strings.NewReader(`{"action":"approve"}`))
	req.Header.Set("X-Peak-Search-UI", "1")
	req.SetPathValue("id", pid)
	rec = httptest.NewRecorder()
	s.peakProposalDecide(rec, req)
	if rec.Code != 404 {
		t.Fatalf("已裁决提议应 404: %d", rec.Code)
	}
}

func newPeakTestServer(t *testing.T) *Server {
	s, _ := taskHallTestServer(t)
	return s
}

package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/db"
)

func TestFindingLevelsAPI(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer m.Close()
	td := t.TempDir()
	s := New(testServerCtx(t), m, td, td, td)
	h := s.Handler()
	tok, err := signJWT(s.jwtKey)
	if err != nil {
		t.Fatal(err)
	}
	do := func(method, path, body string) (int, map[string]any) {
		t.Helper()
		r := httptest.NewRequest(method, path, bytes.NewReader([]byte(body)))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Authorization", "Bearer "+tok)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		if rec.Code >= 400 && out["error"] == nil {
			out["error"] = strings.TrimSpace(rec.Body.String())
		}
		return rec.Code, out
	}

	created, err := m.CreateTaskWithOptions("c04d-levels", "看看分级", db.TaskCreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	taskID, err := strconv.ParseInt(created.ID, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	evidence := "这是一段超过二十个字的说明，但没有引用任何工具输出。"
	fid, err := m.pg.AddFinding(taskID, 0, "sqli", "注入", db.SeverityCritical, "可验证注入", evidence, "worker", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = m.pg.DeleteFinding(fid) })

	code, out := do(http.MethodPatch, "/api/exploration/findings/"+strconv.FormatInt(fid, 10), `{"impact_level":5}`)
	if code != 400 || !strings.Contains(fmtAny(out["error"]), "1-4") {
		t.Fatalf("impact_level=5 应 400，得到 %d %v", code, out)
	}
	row, err := m.pg.GetFinding(fid)
	if err != nil || row == nil || row.ImpactLevel != 1 || row.EvidenceLevel != 1 {
		t.Fatalf("400 之后不应改分级：%+v err=%v", row, err)
	}

	code, out = do(http.MethodPatch, "/api/exploration/findings/"+strconv.FormatInt(fid, 10), `{"impact_level":4,"evidence_level":2}`)
	if code != 200 {
		t.Fatalf("critical + L2 应入库：%d %v", code, out)
	}
	if out["impact_level"] != float64(4) || out["evidence_level"] != float64(2) || out["evidence_insufficient"] != true {
		t.Fatalf("详情应显示证据不足：%v", out)
	}

	code, out = do(http.MethodGet, "/api/exploration/findings/"+strconv.FormatInt(fid, 10), "")
	if code != 200 || out["evidence_insufficient"] != true || out["severity"] != "critical" {
		t.Fatalf("GET 详情：%d %v", code, out)
	}

	code, out = do(http.MethodGet, "/api/tasks/"+created.ID, "")
	if code != 200 || out["max_impact_level"] != float64(4) {
		t.Fatalf("任务最高影响层级应为 4：%d %v", code, out)
	}
}

func fmtAny(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	b, _ := json.Marshal(v)
	return string(b)
}

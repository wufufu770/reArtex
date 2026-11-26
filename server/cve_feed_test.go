// cve_feed_test.go：B-07A 验收——days=3/7/30、非法 days/limit 400、空数据、
// 服务 unavailable（未配置/上游 5xx）、排序（KEV 优先+CVSS 降序稳定）、limit
// 上限、鉴权面（requireAuth 内=租户隔离面）、资产匹配与任务关联一致性、
// hinted 台账只读标记。GET 只读：全程零 hint 写入断言。
package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
)

// feedGet 直调处理体并解析响应（鉴权面另行 TestCveFeedRouteAuth 走真实路由表）。
func feedGet(t *testing.T, srv *Server, query string) (*httptest.ResponseRecorder, CVEFeedResult) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/cve/feed"+query, nil)
	rec := httptest.NewRecorder()
	srv.cveFeed(rec, req)
	var out CVEFeedResult
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("feed 响应解析失败: %v — %s", err, rec.Body.String())
	}
	return rec, out
}

// days 只允许 3/7/30（缺省 7），limit 缺省 20 上限 50，非法 400 fail-closed；
// 未配置 CVE_MCP_URL ⇒ 200 显式 unavailable（绝不伪造空结果为「没有 CVE」）。
func TestCveFeedArgsValidation(t *testing.T) {
	t.Setenv("CVE_MCP_URL", "")
	srv := &Server{}
	for _, tc := range []struct {
		query string
		want  int
	}{
		{"", 7}, {"?days=3", 3}, {"?days=7", 7}, {"?days=30", 30},
	} {
		rec, out := feedGet(t, srv, tc.query)
		if rec.Code != 200 || !out.Unavailable || out.Days != tc.want {
			t.Fatalf("days %q 应 200 unavailable 且 days=%d: code=%d %+v", tc.query, tc.want, rec.Code, out)
		}
		if len(out.Items) != 0 {
			t.Fatalf("unavailable 必须 items 空: %+v", out)
		}
	}
	for _, bad := range []string{"?days=8", "?days=0", "?days=-1", "?days=abc", "?limit=51", "?limit=-2", "?limit=abc"} {
		rec, _ := feedGet(t, srv, bad)
		if rec.Code != 400 {
			t.Fatalf("非法参数 %q 应 400: %d %s", bad, rec.Code, rec.Body.String())
		}
	}
	// limit 缺省/0/上限值不 400（值本身不进响应契约，走 unavailable 200 面）。
	for _, ok := range []string{"?limit=0", "?limit=20", "?limit=50"} {
		if rec, _ := feedGet(t, srv, ok); rec.Code != 200 {
			t.Fatalf("limit %q 应合法 200: %d", ok, rec.Code)
		}
	}
}

// 上游不可达（5xx）：显式 unavailable，绝不伪装成空数据。
func TestCveFeedUnavailableUpstream(t *testing.T) {
	pg := cveTestPG(t)
	srv := &Server{m: &Manager{pg: pg}}
	f := newFakeCveMCP(t)
	f.status = http.StatusInternalServerError
	t.Setenv("CVE_MCP_URL", f.srv.URL)
	rec, out := feedGet(t, srv, "?days=7")
	if rec.Code != 200 || !out.Unavailable || len(out.Items) != 0 {
		t.Fatalf("上游 5xx 应 200 显式 unavailable: code=%d %+v", rec.Code, out)
	}
}

// 空数据：合法空列表（非 unavailable）。
func TestCveFeedEmptyData(t *testing.T) {
	pg := cveTestPG(t)
	srv := &Server{m: &Manager{pg: pg}}
	f := newFakeCveMCP(t)
	inner := `[]`
	env, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1,
		"result": map[string]any{"content": []map[string]any{{"type": "text", "text": inner}}},
	})
	f.searchBody = string(env)
	t.Setenv("CVE_MCP_URL", f.srv.URL)
	rec, out := feedGet(t, srv, "?days=7")
	if rec.Code != 200 || out.Unavailable || out.Count != 0 || out.Items == nil || len(out.Items) != 0 {
		t.Fatalf("空数据应合法空列表: code=%d %+v", rec.Code, out)
	}
}

// 全链：资产匹配一致性 + 任务关联一致性 + KEV 优先/CVSS 降序稳定排序 +
// hinted 台账只读标记（先无 hint → RecordCVEHint 后 hinted=true）。
func TestCveFeedFullChain(t *testing.T) {
	pg := cveTestPG(t)
	srv := &Server{m: &Manager{pg: pg}}

	task, err := pg.CreateTask("cve-feed", "goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pg.DeleteTask(task.ID) })

	assetID, err := pg.Assets().UpsertHTTPService(db.UpsertHTTPServiceReq{
		URL:          "https://cvefeed.example.com/",
		Technologies: []string{"Apache Log4j 2.14.0"},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pg.Assets().DeleteByIDs([]int64{assetID}) })
	if _, err := pg.Assets().AttachAssetsToTask(task.ID, []int64{assetID}, "B-07A 测试挂靠"); err != nil {
		t.Fatal(err)
	}

	serial := time.Now().UnixNano() % 100000000
	cveKEV := fmt.Sprintf("CVE-2026-%d", serial)    // KEV，低分
	cveHigh := fmt.Sprintf("CVE-2026-%d", serial+1) // 非 KEV，高分
	t.Cleanup(func() {
		_, _ = pg.Exec(`DELETE FROM cve_hints WHERE cve_id IN ($1,$2)`, cveKEV, cveHigh)
	})

	// fake sidecar：search 回两条记录（KEV 低分在前——喂排序断言）；
	// match_component 走 fake 默认 affected（真实客户端精判器）。
	f := newFakeCveMCP(t)
	inner := fmt.Sprintf(`[
		{"cve_id":%q,"title":"KEV low","cvss_v3":5,"in_cisa_kev":true,"affected_ranges":[{"package":"Apache Log4j 2.14.0","from":"2.0","to":"2.15.0"}],"published":"2026-01-01T00:00:00Z"},
		{"cve_id":%q,"title":"high score","cvss_v3":9.8,"in_cisa_kev":false,"affected_ranges":[{"package":"Apache Log4j 2.14.0","from":"2.0","to":"2.15.0"}],"published":"2026-01-02T00:00:00Z"}
	]`, cveKEV, cveHigh)
	env, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1,
		"result": map[string]any{"content": []map[string]any{{"type": "text", "text": inner}}},
	})
	f.searchBody = string(env)
	t.Setenv("CVE_MCP_URL", f.srv.URL)

	rec, out := feedGet(t, srv, "?days=30")
	if rec.Code != 200 || out.Unavailable || out.Count != 2 {
		t.Fatalf("全链应 200 恰 2 条: code=%d %+v %s", rec.Code, out, rec.Body.String())
	}
	// 排序：KEV 优先（cveKEV 在前，尽管 CVSS 5 < 9.8）。
	if out.Items[0].CVEID != cveKEV || !out.Items[0].InKEV {
		t.Fatalf("KEV 必须优先: %+v", out.Items)
	}
	if out.Items[1].CVEID != cveHigh || out.Items[1].InKEV {
		t.Fatalf("第二条应非 KEV 高分: %+v", out.Items[1])
	}
	// 资产匹配 + 任务关联一致性（两 CVE 同构断言）。
	for _, it := range out.Items {
		if len(it.MatchedAssets) != 1 || len(it.MatchedTasks) != 1 {
			t.Fatalf("%s 应恰 1 资产 1 任务: %+v", it.CVEID, it)
		}
		a := it.MatchedAssets[0]
		if a.AssetID != assetID || a.Verdict != string(db.VerdictAffected) || a.MatchedVersion != "2.14.0" {
			t.Fatalf("%s 资产匹配错误: %+v", it.CVEID, a)
		}
		if a.TaskIDs[0] != task.ID || it.MatchedTasks[0] != task.ID {
			t.Fatalf("%s 任务关联错误: %+v", it.CVEID, it)
		}
		if a.Hinted || it.HintedCount != 0 {
			t.Fatalf("%s 初始必须零 hinted: %+v", it.CVEID, it)
		}
		if it.Published == "" {
			t.Fatalf("%s published 应透传", it.CVEID)
		}
	}
	// hinted：台账落一条后只读标记翻转（GET 全程零写入——写由测试显式做）。
	if _, err := pg.RecordCVEHint(task.ID, assetID, cveHigh, string(db.VerdictAffected), 555001); err != nil {
		t.Fatal(err)
	}
	rec2, out2 := feedGet(t, srv, "?days=30")
	if rec2.Code != 200 || out2.Count != 2 {
		t.Fatalf("复看应 200: %d", rec2.Code)
	}
	byID := map[string]CVEFeedItem{}
	for _, it := range out2.Items {
		byID[it.CVEID] = it
	}
	if !byID[cveHigh].MatchedAssets[0].Hinted || byID[cveHigh].HintedCount != 1 {
		t.Fatalf("cveHigh 应 hinted=true/1: %+v", byID[cveHigh])
	}
	if byID[cveKEV].MatchedAssets[0].Hinted || byID[cveKEV].HintedCount != 0 {
		t.Fatalf("cveKEV 应保持零 hinted: %+v", byID[cveKEV])
	}
}

// 路由与鉴权面（租户隔离）：/api/cve/feed 注册在真实路由表且在 requireAuth 内
// ——无 token 401，有效 token 才进处理体。
func TestCveFeedRouteAuth(t *testing.T) {
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
		t.Fatalf("sign jwt: %v", err)
	}

	t.Setenv("CVE_MCP_URL", "")
	// 无 token：401（路由存在且被鉴权包裹——不存在的路由会 404）。
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/cve/feed", nil))
	if rec.Code != 401 {
		t.Fatalf("无 token 应 401（证明路由在 requireAuth 内）: %d %s", rec.Code, rec.Body.String())
	}
	// B-07B：POST dispatch 同在鉴权面内——无 token 401。
	recD := httptest.NewRecorder()
	h.ServeHTTP(recD, httptest.NewRequest(http.MethodPost, "/api/cve/feed/dispatch",
		strings.NewReader(`{"cve_id":"CVE-2021-44228","task_ids":[1]}`)))
	if recD.Code != 401 {
		t.Fatalf("dispatch 无 token 应 401: %d %s", recD.Code, recD.Body.String())
	}
	// 有效 token：进处理体（未配置 → 200 显式 unavailable）。
	req := httptest.NewRequest(http.MethodGet, "/api/cve/feed", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req)
	if rec2.Code != 200 {
		t.Fatalf("有效 token 应 200: %d %s", rec2.Code, rec2.Body.String())
	}
	var out CVEFeedResult
	if err := json.Unmarshal(rec2.Body.Bytes(), &out); err != nil {
		t.Fatalf("响应解析: %v", err)
	}
	if !out.Unavailable {
		t.Fatalf("未配置必须显式 unavailable: %s", rec2.Body.String())
	}
}

// ── B-07B：显式 dispatch 验收 ───────────────────────────────────────────────

// dispatchPost 直调处理体并解析响应。
func dispatchPost(t *testing.T, srv *Server, body string) (*httptest.ResponseRecorder, CVEFeedDispatchResult) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/cve/feed/dispatch", strings.NewReader(body))
	rec := httptest.NewRecorder()
	srv.cveFeedDispatch(rec, req)
	var out CVEFeedDispatchResult
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out
}

// 请求体 fail-closed：坏 JSON/非法 CVE/空列表/超上限/非法 ID 全 400。
func TestCveFeedDispatchValidation(t *testing.T) {
	srv := &Server{}
	for _, tc := range []struct {
		name string
		body string
	}{
		{"坏 JSON", `{"cve_id":`},
		{"非法 CVE", `{"cve_id":"log4j","task_ids":[1]}`},
		{"空列表", `{"cve_id":"CVE-2021-44228","task_ids":[]}`},
		{"超上限", `{"cve_id":"CVE-2021-44228","task_ids":[` + twentyOneIDs + `]}`},
		{"零 ID", `{"cve_id":"CVE-2021-44228","task_ids":[0]}`},
		{"负 ID", `{"cve_id":"CVE-2021-44228","task_ids":[-5]}`},
	} {
		rec, _ := dispatchPost(t, srv, tc.body)
		if rec.Code != 400 {
			t.Fatalf("%s 应 400: %d %s", tc.name, rec.Code, rec.Body.String())
		}
	}
}

const twentyOneIDs = `1,2,3,4,5,6,7,8,9,10,11,12,13,14,15,16,17,18,19,20,21`

// 全链：显式 dispatch → B-04F EmitCVEHints → hint 落位 → findings 不变 →
// 重复请求幂等（deduped）→ 部分失败如实（未知任务 failed，ok:false）。
func TestCveFeedDispatchLifecycle(t *testing.T) {
	pg := cveTestPG(t)
	srv := &Server{m: &Manager{pg: pg}}

	task, err := pg.CreateTask("cve-feed-dispatch", "goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = pg.DeleteTask(task.ID)
		_, _ = pg.Exec(`DELETE FROM cve_hints WHERE task_id=$1`, task.ID)
	})

	assetID, err := pg.Assets().UpsertHTTPService(db.UpsertHTTPServiceReq{
		URL:          "https://cvedispatch.example.com/",
		Technologies: []string{"Apache Log4j 2.14.0"},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pg.Assets().DeleteByIDs([]int64{assetID}) })
	if _, err := pg.Assets().AttachAssetsToTask(task.ID, []int64{assetID}, "B-07B 测试挂靠"); err != nil {
		t.Fatal(err)
	}
	// 授权边：EmitCVEHints 走 AddNodeInScope，锚点必须落在任务声明的 task_scope
	// 内（挂靠≠scope——B-04F 既有仓库现状：root_domain 分类器不一致，直插
	// kind='subdomain' 行精确覆盖资产 domain）。
	if _, err := pg.Exec(`INSERT INTO task_scope(task_id, kind, domain, source, reason) VALUES ($1,'subdomain','cvedispatch.example.com','manual','B-07B 测试范围')`, task.ID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pg.Exec(`DELETE FROM task_scope WHERE task_id=$1 AND kind='subdomain'`, task.ID) })

	// fake sidecar 默认面：cve_lookup 回 CVE-2021-44228（title Log4j→token 回落、
	// 无区间→B-04C maybe 判定链——EmitCVEHints 接受 maybe）；match_component
	// 默认 affected（本链无区间不触发求值）。不触碰 cve_tools_test.go。
	f := newFakeCveMCP(t)
	t.Setenv("CVE_MCP_URL", f.srv.URL)

	findingsBefore := findingsCount(t, pg)

	rec, out := dispatchPost(t, srv, fmt.Sprintf(`{"cve_id":"CVE-2021-44228","task_ids":[%d]}`, task.ID))
	if rec.Code != 200 || !out.OK || len(out.Items) != 1 {
		t.Fatalf("dispatch 应 200 ok 恰 1 项: code=%d %+v %s", rec.Code, out, rec.Body.String())
	}
	if out.Items[0].Status != "emitted" || out.Items[0].Emitted != 1 {
		t.Fatalf("应 emitted 1: %+v", out.Items[0])
	}
	// 台账落位（幂等源）。
	if hinted, err := pg.HasCVEHint(task.ID, assetID, "CVE-2021-44228"); err != nil || !hinted {
		t.Fatalf("台账应落位: %v %v", hinted, err)
	}
	// findings 不变（dispatch 绝不写 findings——卡验收）。
	if after := findingsCount(t, pg); after != findingsBefore {
		t.Fatalf("findings 必须不变: %d → %d", findingsBefore, after)
	}
	// 重复请求幂等：deduped。
	rec2, out2 := dispatchPost(t, srv, fmt.Sprintf(`{"cve_id":"CVE-2021-44228","task_ids":[%d]}`, task.ID))
	if rec2.Code != 200 || !out2.OK || out2.Items[0].Status != "deduped" || out2.Items[0].Deduped != 1 {
		t.Fatalf("重复 dispatch 应幂等 deduped: %d %+v", rec2.Code, out2.Items[0])
	}
	// 部分失败如实：未知任务 failed、有效任务 deduped、ok:false。
	rec3, out3 := dispatchPost(t, srv, fmt.Sprintf(`{"cve_id":"CVE-2021-44228","task_ids":[%d,999999999]}`, task.ID))
	if rec3.Code != 200 || out3.OK {
		t.Fatalf("部分失败必须 ok:false: %d %+v", rec3.Code, out3)
	}
	byTask := map[int64]CVEFeedDispatchItem{}
	for _, it := range out3.Items {
		byTask[it.TaskID] = it
	}
	if byTask[task.ID].Status != "deduped" {
		t.Fatalf("有效任务应 deduped: %+v", byTask[task.ID])
	}
	if byTask[999999999].Status != "failed" || byTask[999999999].Detail == "" {
		t.Fatalf("未知任务应 failed 带原因（跨租户/不存在拒绝）: %+v", byTask[999999999])
	}
}

// 情报库无此 CVE：404 显式（绝不伪造 hint）；未配置：503。
func TestCveFeedDispatchGuards(t *testing.T) {
	pg := cveTestPG(t)
	srv := &Server{m: &Manager{pg: pg}}

	// 未配置：503。
	t.Setenv("CVE_MCP_URL", "")
	rec, _ := dispatchPost(t, srv, `{"cve_id":"CVE-2021-44228","task_ids":[1]}`)
	if rec.Code != 503 {
		t.Fatalf("未配置应 503: %d %s", rec.Code, rec.Body.String())
	}

	// not-found：fake f.body 覆盖 found=false（dispatch 在 lookup 即停——match
	// 面不被触碰）。
	f := newFakeCveMCP(t)
	inner := `{"schema_version":"cve_intel.v1","found":false,"cve_id":"CVE-2026-9999"}`
	env, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1,
		"result": map[string]any{"content": []map[string]any{{"type": "text", "text": inner}}},
	})
	f.body = string(env)
	t.Setenv("CVE_MCP_URL", f.srv.URL)
	rec2, _ := dispatchPost(t, srv, `{"cve_id":"CVE-2026-9999","task_ids":[1]}`)
	if rec2.Code != 404 {
		t.Fatalf("情报库无此 CVE 应 404: %d %s", rec2.Code, rec2.Body.String())
	}
}

func findingsCount(t *testing.T, pg *db.DB) int {
	t.Helper()
	var n int
	if err := pg.QueryRow(`SELECT COUNT(*) FROM findings`).Scan(&n); err != nil {
		t.Fatalf("findings 计数: %v", err)
	}
	return n
}

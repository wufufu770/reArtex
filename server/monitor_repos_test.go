package server

// monitor_repos_test.go：B-09C Monitor 仓库配置 CRUD API 验收。
//
// 验收逐项（卡验收）：
//   - 路由与鉴权面：五个路由注册在真实路由表且在 requireAuth 内——无 token 401
//     （不存在的路由会 404，401 即证明包裹生效）；
//   - CRUD 生命周期（HTTP 全链路）：POST 新建→列表/单条读回→PATCH 启停→
//     同 (repo_url,branch) 再 POST 幂等收敛同 id 且绝不重置游标→DELETE
//     （findings 挂靠 409，处置后可删）→删除后 GET 404；
//   - SSRF 策略：localhost 变体/环回/私网/链路本地/CGNAT/未指定 IP 字面量全拒
//     （API 层先拒，存储层形态闸兜底）；公网 IP 字面量与 hostname 形态放行
//     （GHE 自建域名是设计目标；DNS 级 SSRF 属运行环境出网管控层）；
//   - auth_ref 只收引用：token 明文形态（ghp_ 前缀）经 API 被拒（db 层闸在
//     API 面可见）；请求体无 token 字段（结构性不收）；
//   - 审计链：save/enable/delete 全落 audit_events，payload 键集合恰白名单
//     （无自由键=凭据面结构性排除）。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const monitorAPITestPrefix = "https://github.com/b09capi/"

func monitorAPITestServer(t *testing.T) (*Server, http.Handler, string) {
	t.Helper()
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	_ = m.pg.EnsureMonitorSchema()
	td := t.TempDir()
	s := New(testServerCtx(t), m, td, td, td)
	h := s.Handler()
	tok, err := signJWT(s.jwtKey)
	if err != nil {
		t.Fatalf("sign jwt: %v", err)
	}
	// 连接归还：整包回归下共享 PG max_connections=100，每个用例的池若不显式
	// 关闭，被泄漏的后台 goroutine（task-archive/scheduler 等轮询面）钉住就
	// 不空闲（ConnMaxIdleTime 5min 不生效），套件尾部用例（notify 族）会撞
	// SQLSTATE 53300 too many clients。先注册=最后执行（LIFO）：行删除清理
	// 仍先跑，随后 Manager.Close 归还 traffic+pg 池。
	t.Cleanup(func() { _ = m.Close() })
	t.Cleanup(func() {
		_, _ = m.pg.Exec(`DELETE FROM monitor_findings WHERE repo_id IN (SELECT id FROM monitor_repos WHERE repo_url LIKE $1)`, monitorAPITestPrefix+"%")
		_, _ = m.pg.Exec(`DELETE FROM monitor_repos WHERE repo_url LIKE $1`, monitorAPITestPrefix+"%")
	})
	return s, h, tok
}

func monitorAPIReq(h http.Handler, tok, method, path, body string) *httptest.ResponseRecorder {
	t2 := httptest.NewRequest(method, path, nil)
	if body != "" {
		t2 = httptest.NewRequest(method, path, strings.NewReader(body))
		t2.Header.Set("Content-Type", "application/json")
	}
	if tok != "" {
		t2.Header.Set("Authorization", "Bearer "+tok)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, t2)
	return rec
}

func TestMonitorRepoRoutesAuth(t *testing.T) {
	_, h, _ := monitorAPITestServer(t)

	// 五个路由全部无 token 401（路由存在且被鉴权包裹；不存在会 404）。
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/monitor/repos"},
		{http.MethodPost, "/api/monitor/repos"},
		{http.MethodGet, "/api/monitor/repos/1"},
		{http.MethodPatch, "/api/monitor/repos/1"},
		{http.MethodDelete, "/api/monitor/repos/1"},
	} {
		rec := monitorAPIReq(h, "", tc.method, tc.path, "")
		if rec.Code != 401 {
			t.Fatalf("%s %s 无 token 应 401: %d %s", tc.method, tc.path, rec.Code, rec.Body.String())
		}
	}
}

func TestMonitorRepoCRUDAPILifecycle(t *testing.T) {
	s, h, tok := monitorAPITestServer(t)
	repoURL := monitorAPITestPrefix + "lifecycle"

	// ① POST 新建。
	body := `{"repo_url":"` + repoURL + `","branch":"main","auth_ref":"key:default","enabled":true}`
	rec := monitorAPIReq(h, tok, http.MethodPost, "/api/monitor/repos", body)
	if rec.Code != 200 {
		t.Fatalf("POST 新建应 200: %d %s", rec.Code, rec.Body.String())
	}
	var created struct {
		OK   bool           `json:"ok"`
		Repo monitorRepoDTO `json:"repo"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil || !created.OK || created.Repo.ID <= 0 {
		t.Fatalf("新建响应不符: %v %s", err, rec.Body.String())
	}
	id := created.Repo.ID

	// ② 列表读回 + 单条读回。
	rec = monitorAPIReq(h, tok, http.MethodGet, "/api/monitor/repos?enabled=1", "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), repoURL) {
		t.Fatalf("列表应含新配置: %d %s", rec.Code, rec.Body.String())
	}
	rec = monitorAPIReq(h, tok, http.MethodGet, "/api/monitor/repos/1x", "")
	if rec.Code != 400 {
		t.Fatalf("非数字 id 应 400: %d", rec.Code)
	}
	rec = monitorAPIReq(h, tok, http.MethodGet, "/api/monitor/repos/999999999", "")
	if rec.Code != 404 {
		t.Fatalf("不存在 id 应 404: %d", rec.Code)
	}
	rec = monitorAPIReq(h, tok, http.MethodGet, "/api/monitor/repos/"+itoa64(id), "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"auth_ref":"key:default"`) {
		t.Fatalf("单条读回应含 auth_ref 引用串: %d %s", rec.Code, rec.Body.String())
	}

	// ③ PATCH 启停（缺 enabled 键 400；false 生效）。
	rec = monitorAPIReq(h, tok, http.MethodPatch, "/api/monitor/repos/"+itoa64(id), `{}`)
	if rec.Code != 400 {
		t.Fatalf("PATCH 缺 enabled 应 400: %d", rec.Code)
	}
	rec = monitorAPIReq(h, tok, http.MethodPatch, "/api/monitor/repos/"+itoa64(id), `{"enabled":false}`)
	if rec.Code != 200 {
		t.Fatalf("PATCH 停用应 200: %d %s", rec.Code, rec.Body.String())
	}
	rec = monitorAPIReq(h, tok, http.MethodGet, "/api/monitor/repos/"+itoa64(id), "")
	if !strings.Contains(rec.Body.String(), `"enabled":false`) {
		t.Fatalf("停用后读回应 enabled=false: %s", rec.Body.String())
	}
	// enabledOnly 列表不再包含停用配置。
	rec = monitorAPIReq(h, tok, http.MethodGet, "/api/monitor/repos?enabled=1", "")
	if strings.Contains(rec.Body.String(), repoURL) {
		t.Fatal("enabledOnly 列表不应含停用配置")
	}

	// ④ 幂等 upsert：先推进游标，再同 (url,branch) POST——同 id、游标绝不重置。
	if err := s.m.pg.UpdateMonitorRepoCursor(id, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"); err != nil {
		t.Fatal(err)
	}
	body2 := `{"repo_url":"` + repoURL + `","branch":"main","auth_ref":"key:alt","github_base_url":"https://ghe.corp.example.com/api/v3","enabled":true}`
	rec = monitorAPIReq(h, tok, http.MethodPost, "/api/monitor/repos", body2)
	if rec.Code != 200 {
		t.Fatalf("幂等 POST 应 200: %d %s", rec.Code, rec.Body.String())
	}
	var updated struct {
		Repo monitorRepoDTO `json:"repo"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &updated)
	if updated.Repo.ID != id {
		t.Fatalf("同 (url,branch) 应收敛同 id: %d vs %d", updated.Repo.ID, id)
	}
	if updated.Repo.LastSeenSHA != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Fatalf("upsert 绝不重置游标: %q", updated.Repo.LastSeenSHA)
	}
	if updated.Repo.AuthRef != "key:alt" {
		t.Fatalf("upsert 更新 auth_ref 引用: %q", updated.Repo.AuthRef)
	}

	// ⑤ DELETE：findings 挂靠 409（处置历史保留），显式清除后才可删。
	if _, err := s.m.pg.RecordMonitorFinding(id, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "安全更新", "CVE-2024-3094", "", ""); err != nil {
		t.Fatal(err)
	}
	rec = monitorAPIReq(h, tok, http.MethodDelete, "/api/monitor/repos/"+itoa64(id), "")
	if rec.Code != 409 {
		t.Fatalf("带 findings 删除应 409: %d %s", rec.Code, rec.Body.String())
	}
	rec = monitorAPIReq(h, tok, http.MethodGet, "/api/monitor/repos/"+itoa64(id), "")
	if rec.Code != 200 {
		t.Fatal("409 后配置必须仍在")
	}
	if _, err := s.m.pg.Exec(`DELETE FROM monitor_findings WHERE repo_id=$1`, id); err != nil {
		t.Fatal(err)
	}
	rec = monitorAPIReq(h, tok, http.MethodDelete, "/api/monitor/repos/"+itoa64(id), "")
	if rec.Code != 200 {
		t.Fatalf("清 findings 后删除应 200: %d %s", rec.Code, rec.Body.String())
	}
	rec = monitorAPIReq(h, tok, http.MethodGet, "/api/monitor/repos/"+itoa64(id), "")
	if rec.Code != 404 {
		t.Fatalf("删除后 GET 应 404: %d", rec.Code)
	}
}

func TestMonitorRepoSSRFAndValidation(t *testing.T) {
	_, h, tok := monitorAPITestServer(t)

	// SSRF 策略表（API 层先拒——存储层形态闸兜底）。
	badHosts := []string{
		"https://localhost/owner/repo",
		"https://sub.localhost/owner/repo",
		"https://api.local/owner/repo",
		"https://ghe.internal/owner/repo",
		"https://127.0.0.1/owner/repo",
		"https://10.1.2.3/owner/repo",
		"https://172.16.0.9/owner/repo",
		"https://192.168.1.5/owner/repo",
		"https://169.254.1.1/owner/repo",
		"https://100.64.0.1/owner/repo",
		"https://0.0.0.0/owner/repo",
		"https://[::1]/owner/repo",
		"https://[fe80::1]/owner/repo",
		"https://[fd00::1]/owner/repo",
		"http://github.com/owner/repo", // 非 https
	}
	for _, u := range badHosts {
		body := `{"repo_url":"` + u + `","branch":"main"}`
		rec := monitorAPIReq(h, tok, http.MethodPost, "/api/monitor/repos", body)
		if rec.Code != 400 {
			t.Fatalf("SSRF/形态 %q 应 400: %d %s", u, rec.Code, rec.Body.String())
		}
	}

	// base_url 同闸。
	rec := monitorAPIReq(h, tok, http.MethodPost, "/api/monitor/repos",
		`{"repo_url":"`+monitorAPITestPrefix+`ssrf-base","branch":"main","github_base_url":"http://127.0.0.1:8080/api/v3"}`)
	if rec.Code != 400 {
		t.Fatalf("base_url 私网应 400: %d %s", rec.Code, rec.Body.String())
	}

	// 放行面：公网 IP 字面量 + hostname 形态 GHE（设计目标）。
	for _, u := range []string{
		monitorAPITestPrefix + "ssrf-ok-ip",
		"https://140.82.121.4/owner/repo",
		"https://ghe.corp.example.com/owner/repo",
	} {
		rec := monitorAPIReq(h, tok, http.MethodPost, "/api/monitor/repos", `{"repo_url":"`+u+`","branch":"main"}`)
		if rec.Code != 200 {
			t.Fatalf("公网/hostname %q 应放行: %d %s", u, rec.Code, rec.Body.String())
		}
	}

	// token 明文形态经 API 被拒（db 层 auth_ref 闸在 API 面可见）。
	rec = monitorAPIReq(h, tok, http.MethodPost, "/api/monitor/repos",
		`{"repo_url":"`+monitorAPITestPrefix+`token-reject","branch":"main","auth_ref":"ghp_REDACTEDsample"}`)
	if rec.Code != 400 {
		t.Fatalf("token 明文 auth_ref 应 400: %d %s", rec.Code, rec.Body.String())
	}
}

func TestMonitorRepoAuditNoSecrets(t *testing.T) {
	s, h, tok := monitorAPITestServer(t)
	repoURL := monitorAPITestPrefix + "audit"

	// save + enable + delete 各一次，之后逐条核 payload 键白名单。
	body := `{"repo_url":"` + repoURL + `","branch":"main","auth_ref":"key:audit"}`
	if rec := monitorAPIReq(h, tok, http.MethodPost, "/api/monitor/repos", body); rec.Code != 200 {
		t.Fatalf("save 应 200: %d %s", rec.Code, rec.Body.String())
	}
	var lst struct {
		Repos []monitorRepoDTO `json:"repos"`
	}
	rec := monitorAPIReq(h, tok, http.MethodGet, "/api/monitor/repos", "")
	if err := json.Unmarshal(rec.Body.Bytes(), &lst); err != nil {
		t.Fatal(err)
	}
	var id int64
	for _, r := range lst.Repos {
		if r.RepoURL == repoURL {
			id = r.ID
		}
	}
	if id <= 0 {
		t.Fatal("应找到审计用配置")
	}
	if rec := monitorAPIReq(h, tok, http.MethodPatch, "/api/monitor/repos/"+itoa64(id), `{"enabled":false}`); rec.Code != 200 {
		t.Fatalf("enable 应 200: %d", rec.Code)
	}
	if rec := monitorAPIReq(h, tok, http.MethodDelete, "/api/monitor/repos/"+itoa64(id), ""); rec.Code != 200 {
		t.Fatalf("delete 应 200: %d", rec.Code)
	}

	// 审计链逐条核：三个 action 各至少一行，payload 键恰白名单（无自由键=
	// 凭据面结构性排除），载荷绝无 token 字样。
	allowed := map[string]bool{"id": true, "repo_url": true, "branch": true,
		"enabled": true, "auth_ref_set": true, "via": true}
	for _, action := range []string{"monitor-repo-save", "monitor-repo-enable", "monitor-repo-delete"} {
		// enable 的 payload 只带 id 不带 repo_url——按 id 归因（payload 列是
		// TEXT，::jsonb 转换读 id；audit_events 字节级保存语义不变）。
		rows, err := s.m.pg.Query(`SELECT payload FROM audit_events WHERE action=$1 AND payload::jsonb->>'id'=$2 ORDER BY seq DESC LIMIT 5`,
			action, itoa64(id))
		if err != nil {
			t.Fatal(err)
		}
		count := 0
		for rows.Next() {
			count++
			var raw []byte
			if err := rows.Scan(&raw); err != nil {
				t.Fatal(err)
			}
			var m map[string]any
			if err := json.Unmarshal(raw, &m); err != nil {
				t.Fatalf("payload 应是 JSON 对象: %v %s", err, raw)
			}
			for k := range m {
				if !allowed[k] {
					t.Fatalf("payload 出现白名单外键 %q: %s", k, raw)
				}
			}
			if strings.Contains(strings.ToLower(string(raw)), "ghp_") {
				t.Fatalf("payload 绝无 token 字样: %s", raw)
			}
		}
		rows.Close()
		if count == 0 {
			t.Fatalf("action %s 应有审计行", action)
		}
	}
}

func TestMonitorAPIHostPolicyUnit(t *testing.T) {
	// 纯函数单测（零 PG）：host 策略与公网判定。
	denied := []string{"localhost", "localhost:8443", "127.0.0.1", "127.0.0.1:8080", "10.0.0.1",
		"192.168.1.1", "172.31.255.1", "169.254.169.254", "100.64.0.1", "0.0.0.0", "[::1]", "[fe80::1]"}
	for _, hp := range denied {
		if err := monitorAPIHostPolicy(hp); err == nil {
			t.Fatalf("host %q 应被拒", hp)
		}
	}
	allowed := []string{"github.com", "api.github.com", "ghe.corp.example.com", "ghe.corp.example.com:8443",
		"140.82.121.4", "[2607:f8b0:4004:800::200e]"}
	for _, hp := range allowed {
		if err := monitorAPIHostPolicy(hp); err != nil {
			t.Fatalf("host %q 应放行: %v", hp, err)
		}
	}
}

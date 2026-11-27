package server

// llm_profile_lifecycle_test.go：LLM 配置生命周期后端语义（缺陷①②③④修复）
// 验收，live PG（cveTestPG 既有范式）零真实出网：
//   - 新建配置缺省「不参与轮询」（pool_exclude=true）——激活是唯一默认可用；
//   - 更新缺省保持库中原值（绝不悄悄改语义）；显式传值照常生效；
//   - 删除最后一个/激活配置：全局 LLM 栈 teardown（llmOn=false，绝无内存残留
//     provider 继续服务）、pin 的会话被停止+系统留痕、pin 经 FK 置空。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/llmpool"
)

func lifecycleServer(t *testing.T, pg *db.DB) *Server {
	t.Helper()
	m := &Manager{dir: t.TempDir(), pg: pg}
	// llmHealth 在生产由 NewServer 装配；极简夹具补进程级注册表（nil 会在
	// pgDeleteProfile 的 Reset 上炸）。
	return &Server{m: m, engine: NewEngine(m), llmHealth: llmpool.NewRegistry(nil, nil)}
}

func lifecycleProfileBody(id int64, poolExclude *bool) string {
	b := `{"name":"t-lc","format":"openai","model":"m","api_key":"k","base_url":"http://127.0.0.1:1"`
	if id > 0 {
		b += `,"id":` + jsonInt(id)
	}
	if poolExclude != nil {
		b += `,"pool_exclude":` + jsonBool(*poolExclude)
	}
	return b + `}`
}

func jsonInt(id int64) string {
	b, _ := json.Marshal(id)
	return string(b)
}

func jsonBool(v bool) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// 新建缺省不进轮询链；更新缺省保持原值；显式传值生效。
func TestPgSaveProfilePoolOptInDefault(t *testing.T) {
	pg := cveTestPG(t)
	s := lifecycleServer(t, pg)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/llm/profiles", s.pgSaveProfile)

	do := func(body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/api/llm/profiles", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}

	// 新建（不带 pool_exclude）→ 缺省 pool_exclude=true。
	rec := do(lifecycleProfileBody(0, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("新建应 200: %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.ID <= 0 {
		t.Fatalf("新建响应: %s err=%v", rec.Body.String(), err)
	}
	defer func() { _ = pg.DeleteProfile(out.ID) }()
	p, err := pg.ProfileByID(out.ID)
	if err != nil || p == nil {
		t.Fatalf("查 profile: %+v err=%v", p, err)
	}
	if !p.PoolExclude {
		t.Fatal("新建配置缺省应不参与轮询（pool_exclude=true）")
	}

	// 更新（仍不带 pool_exclude）→ 保持原值 true。
	rec = do(lifecycleProfileBody(out.ID, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("更新应 200: %d %s", rec.Code, rec.Body.String())
	}
	if p2, _ := pg.ProfileByID(out.ID); p2 == nil || !p2.PoolExclude {
		t.Fatal("更新缺省应保持库中原值")
	}

	// 显式 false（用户勾选「参与轮询」）→ 照常生效。
	excl := false
	rec = do(lifecycleProfileBody(out.ID, &excl))
	if rec.Code != http.StatusOK {
		t.Fatalf("显式更新应 200: %d %s", rec.Code, rec.Body.String())
	}
	if p3, _ := pg.ProfileByID(out.ID); p3 == nil || p3.PoolExclude {
		t.Fatal("显式 pool_exclude=false 应生效（显式加入轮询链）")
	}
}

// 删除最后一个（激活）配置：teardown 全局 LLM 栈 + pin 会话停止留痕 + pin 置空；
// 未 pin 的会话不受牵连。
func TestPgDeleteLastProfileTearsDownAndStopsConversations(t *testing.T) {
	pg := cveTestPG(t)
	s := lifecycleServer(t, pg)
	pid, err := pg.SaveProfile(&db.LLMProfile{Name: "t-lc-del", Format: "openai", Model: "m", APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	if err := pg.SetActiveProfile(pid); err != nil {
		t.Fatal(err)
	}
	pinned, err := pg.CreateConversation("planner", "pin 的会话", &pid)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pg.DeleteConversation(pinned.ID) }()
	free, err := pg.CreateConversation("planner", "没 pin 的会话", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pg.DeleteConversation(free.ID) }()

	// 模拟引擎已装配（applyLLM 之后的状态）——删除后必须被拆掉。
	s.cfgMu.Lock()
	s.llmOn = true
	s.cfgMu.Unlock()

	mux := http.NewServeMux()
	mux.HandleFunc("DELETE /api/llm/profiles/{id}", s.pgDeleteProfile)
	req := httptest.NewRequest(http.MethodDelete, "/api/llm/profiles/"+jsonInt(pid), nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("删除（含激活配置）应 200: %d %s", rec.Code, rec.Body.String())
	}

	// 全局栈 teardown：llmOn=false、globalProvider 不再解析。
	s.cfgMu.Lock()
	on := s.llmOn
	s.cfgMu.Unlock()
	if on {
		t.Fatal("删除最后一个配置后 llmOn 应为 false（后端必须收口，绝无内存残留）")
	}
	if _, _, ok := s.globalProvider(); ok {
		t.Fatal("删除后 globalProvider 不得再解析出 provider")
	}
	if act, err := pg.ActiveProfile(); err != nil || act != nil {
		t.Fatalf("激活面应清空: %+v err=%v", act, err)
	}

	// pin 的会话：FK 置空 + 系统留痕。
	if c, _ := pg.GetConversation(pinned.ID); c == nil || c.LLMProfileID != nil {
		t.Fatalf("pin 应被 FK 置空: %+v", c)
	}
	acts, _, err := pg.ConvActivityList(pinned.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	foundNote := false
	for _, a := range acts {
		if a.Worker == "system" && a.Kind == "note" && strings.Contains(a.Summary, "已被删除") {
			foundNote = true
		}
	}
	if !foundNote {
		t.Fatalf("pin 的会话应有「配置已删除」系统留痕: %+v", acts)
	}
	// 未 pin 的会话：零留痕。
	actsFree, _, err := pg.ConvActivityList(free.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range actsFree {
		if a.Worker == "system" && strings.Contains(a.Summary, "已被删除") {
			t.Fatalf("未 pin 的会话不得被牵连: %+v", a)
		}
	}
}

// reapplyActiveProfile 在「确定无激活配置」时 teardown；pg 不可达时不动运行态
// （一次 DB 抖动不能拆引擎）。
func TestReapplyTeardownOnlyWhenNoActive(t *testing.T) {
	pg := cveTestPG(t)
	s := lifecycleServer(t, pg)

	// 无激活配置：preset llmOn=true → reapply → false。
	s.cfgMu.Lock()
	s.llmOn = true
	s.cfgMu.Unlock()
	s.reapplyActiveProfile()
	s.cfgMu.Lock()
	on := s.llmOn
	s.cfgMu.Unlock()
	if on {
		t.Fatal("无激活配置时 reapply 应 teardown（llmOn=false）")
	}

	// 有激活配置但 key 缺失（loadLLMConfig ok=false）：不 teardown——配置还在，
	// 用户随时可能补 key；运行态保持原样。
	pid, err := pg.SaveProfile(&db.LLMProfile{Name: "t-lc-nokey", Format: "openai", Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pg.DeleteProfile(pid) }()
	if err := pg.SetActiveProfile(pid); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = pg.Exec(`UPDATE llm_profiles SET is_default=false WHERE id=$1`, pid) }()
	s.cfgMu.Lock()
	s.llmOn = true
	s.cfgMu.Unlock()
	s.reapplyActiveProfile()
	s.cfgMu.Lock()
	on = s.llmOn
	s.cfgMu.Unlock()
	if !on {
		t.Fatal("存在激活配置（哪怕缺 key）不得 teardown")
	}
}

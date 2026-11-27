package server

// llm_pool_members_test.go：轮询抽屉手动配置（POST /api/llm/pool/members）验收，
// live PG（cveTestPG 既有范式）。覆盖：
//   - 名单顺序即链序：priority 按位置落库（第一顺位最高），PoolProfiles 的
//     ORDER BY priority DESC 依此产出链；
//   - 激活配置缺席自动补首（永久链首不变式，落库形态=抽屉所见）；
//   - 名单外的非激活配置出链（pool_exclude=true）；
//   - 未知/重复/非法 id 整体拒绝（所见即所存，绝不落半份配置）；
//   - 空名单：全部非激活出链（链 <2 时 poolChain 返回 nil，走裸激活 provider
//     的旧行为）；
//   - 响应回传刷新后的链（所见即所存，前端拿回传值重渲染）。

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Autumn-27/artex/db"
)

func poolMembersServer(t *testing.T, pg *db.DB) *Server {
	t.Helper()
	return lifecycleServer(t, pg)
}

// seedProfiles 落 4 个带 key 的配置并激活第一个，返回 id（激活者在前）。
func seedProfiles(t *testing.T, pg *db.DB, names ...string) (ids []int64, active int64) {
	t.Helper()
	for _, n := range names {
		id, err := pg.SaveProfile(&db.LLMProfile{Name: n, Format: "openai", Model: "m-" + n, APIKey: "k-" + n, BaseURL: "http://127.0.0.1:1"})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	active = ids[0]
	if err := pg.SetActiveProfile(active); err != nil {
		t.Fatal(err)
	}
	return ids, active
}

func postPoolMembers(t *testing.T, s *Server, members []map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	body := map[string]any{"members": members}
	raw, _ := json.Marshal(body)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/llm/pool/members", s.pgSetPoolMembers)
	req := httptest.NewRequest(http.MethodPost, "/api/llm/pool/members", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// poolChainIDs 从 handler 响应里解出刷新后的链（profile_id 顺序）。
func poolChainIDs(t *testing.T, rec *httptest.ResponseRecorder) []string {
	t.Helper()
	var out struct {
		Chain []struct {
			ProfileID string `json:"profile_id"`
			Active    bool   `json:"active"`
			Excluded  bool   `json:"excluded"`
			Priority  int    `json:"priority"`
		} `json:"chain"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	inChain := []string{}
	for _, m := range out.Chain {
		if !m.Excluded || m.Active {
			inChain = append(inChain, m.ProfileID)
		}
	}
	return inChain
}

// 名单顺序即链序 + 激活缺席补首 + 名单外出链 + 响应所见即所存。
func TestPgSetPoolMembersOrderAndMembership(t *testing.T) {
	pg := cveTestPG(t)
	s := poolMembersServer(t, pg)
	ids, _ := seedProfiles(t, pg, "t-pm-a", "t-pm-b", "t-pm-c", "t-pm-d")
	a, b, c, d := ids[0], ids[1], ids[2], ids[3]

	// 名单 [b, c]（激活 a 缺席）→ 链 = a, b, c；d 出链。
	rec := postPoolMembers(t, s, []map[string]any{{"id": b}, {"id": c}})
	if rec.Code != http.StatusOK {
		t.Fatalf("保存轮询名单应 200: %d %s", rec.Code, rec.Body.String())
	}
	if got := poolChainIDs(t, rec); len(got) != 3 || got[0] != jsonInt(a) || got[1] != jsonInt(b) || got[2] != jsonInt(c) {
		t.Fatalf("链序应为 a,b,c（激活补首+名单序）: %v", got)
	}

	// 落库语义：priority 按位置（补首后 len=3 → a=3, b=2, c=1），d 出链。
	chain, err := pg.PoolProfiles()
	if err != nil {
		t.Fatal(err)
	}
	if len(chain) != 3 {
		t.Fatalf("PoolProfiles 应恰 3 个成员: %d", len(chain))
	}
	wantPrio := map[int64]int{a: 3, b: 2, c: 1}
	for i, p := range chain {
		if p.ID != []int64{a, b, c}[i] {
			t.Fatalf("链第 %d 位应为 #%d: got #%d", i, []int64{a, b, c}[i], p.ID)
		}
		if p.Priority != wantPrio[p.ID] {
			t.Fatalf("#%d priority 应为 %d: got %d", p.ID, wantPrio[p.ID], p.Priority)
		}
		if p.PoolExclude {
			t.Fatalf("#%d 不应被排除", p.ID)
		}
	}
	all, err := pg.ListProfiles()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range all {
		if p.ID == d && !p.PoolExclude {
			t.Fatal("名单外的 d 应 pool_exclude=true（出链）")
		}
	}

	// 显式把激活放在名单中间 → 链首仍是激活（priority DESC 之前先 is_default DESC）。
	rec = postPoolMembers(t, s, []map[string]any{{"id": b}, {"id": a}, {"id": c}})
	if rec.Code != http.StatusOK {
		t.Fatalf("含激活的名单应 200: %d %s", rec.Code, rec.Body.String())
	}
	if got := poolChainIDs(t, rec); len(got) != 3 || got[0] != jsonInt(a) || got[1] != jsonInt(b) || got[2] != jsonInt(c) {
		t.Fatalf("激活恒为链首: %v", got)
	}
}

// 非法名单整体拒绝：未知 id / 重复 id / 非法 id，绝不落半份配置。
func TestPgSetPoolMembersRejectsBadLists(t *testing.T) {
	pg := cveTestPG(t)
	s := poolMembersServer(t, pg)
	ids, _ := seedProfiles(t, pg, "t-pm-r1", "t-pm-r2")
	b := ids[1]

	// 基线快照：种子直接走 pg.SaveProfile（PoolExclude 零值=false，与 UI 层
	// 的「新建缺省排除」不同源）——不变式是「拒绝的写入零变化」，不是具体值。
	before, err := pg.PoolProfiles()
	if err != nil {
		t.Fatal(err)
	}

	for name, members := range map[string][]map[string]any{
		"未知id": {{"id": b}, {"id": 99999}},
		"重复id": {{"id": b}, {"id": b}},
		"零id":  {{"id": 0}},
		"负id":  {{"id": -1}},
	} {
		rec := postPoolMembers(t, s, members)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: 应 400: %d %s", name, rec.Code, rec.Body.String())
		}
	}
	after, err := pg.PoolProfiles()
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != len(after) {
		t.Fatalf("被拒绝的写入不得改变链成员数（所见即所存）: before=%d after=%d", len(before), len(after))
	}
	for i := range before {
		if before[i].ID != after[i].ID || before[i].Priority != after[i].Priority || before[i].PoolExclude != after[i].PoolExclude {
			t.Fatalf("被拒绝的写入不得改变任何配置状态: #%d before(prio=%d,ex=%v) after(prio=%d,ex=%v)",
				before[i].ID, before[i].Priority, before[i].PoolExclude, after[i].Priority, after[i].PoolExclude)
		}
	}
}

// 空名单=全部非激活出链（链<2 → poolChain nil → 裸激活 provider 旧行为）；
// 之后重新加入照常生效。
func TestPgSetPoolMembersEmptyListAndRejoin(t *testing.T) {
	pg := cveTestPG(t)
	s := poolMembersServer(t, pg)
	ids, _ := seedProfiles(t, pg, "t-pm-e1", "t-pm-e2")
	a, b := ids[0], ids[1]

	rec := postPoolMembers(t, s, []map[string]any{})
	if rec.Code != http.StatusOK {
		t.Fatalf("空名单应 200: %d %s", rec.Code, rec.Body.String())
	}
	chain, err := pg.PoolProfiles()
	if err != nil {
		t.Fatal(err)
	}
	if len(chain) != 1 || chain[0].ID != a {
		t.Fatalf("空名单后链应只剩激活配置: %d 成员", len(chain))
	}

	// 重新加入 b → 链恢复 a, b。
	rec = postPoolMembers(t, s, []map[string]any{{"id": b}})
	if rec.Code != http.StatusOK {
		t.Fatalf("重新加入应 200: %d %s", rec.Code, rec.Body.String())
	}
	if got := poolChainIDs(t, rec); len(got) != 2 || got[0] != jsonInt(a) || got[1] != jsonInt(b) {
		t.Fatalf("重新加入后链应为 a,b: %v", got)
	}
	p, err := pg.ProfileByID(b)
	if err != nil {
		t.Fatal(err)
	}
	if p.PoolExclude {
		t.Fatal("重新加入的 b 不应再被排除")
	}
}

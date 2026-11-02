package server

// 本文件是【策略库只读端点】的验收用例：策略之间的关系边、utility 物化视图、
// 以及 curator 的仲裁面。
//
// 脚手架复用同单元 sod_api_test.go 里的那几件（同一个包、同一个单元）。
//
// ⚠️ 路由可达性用**真实路由表**（s.Handler()）判定，理由同
// server/strategy_api_test.go：只有真的把 mux 建起来才暴露得了模式冲突；而
// 「路由没注册」（纯文本 "404 page not found"）与「handler 自己返回 404」
// （JSON 体）必须分得开。

import (
	"strconv"
	"testing"

	"github.com/Autumn-27/artex/db"
)

// strategyReadStrategy 建一条策略（隔离态）。
func strategyReadStrategy(t *testing.T, pg *db.DB, name string) int64 {
	t.Helper()
	id, err := pg.CreateStrategy(&db.Strategy{
		Name:    name,
		Summary: "对 $TARGET 的参数校验路径做一次枚举探测",
		Steps:   []byte(`["curl -X POST https://$TARGET/$PATH"]`),
	})
	if err != nil {
		t.Fatalf("建策略: %v", err)
	}
	t.Cleanup(func() { _, _ = pg.Exec(`DELETE FROM strategies WHERE id=$1`, id) })
	return id
}

// TestStrategyReadRoutesRegistered 钉住这批只读端点真的接上了。
func TestStrategyReadRoutesRegistered(t *testing.T) {
	_, _, h, tok := sodAPIServer(t)
	cases := []struct{ method, path string }{
		{"GET", "/api/strategies/edges"},
		{"GET", "/api/strategies/utility"},
		{"GET", "/api/strategies/curation"},
	}
	for _, c := range cases {
		code, body := sodAPIRaw(t, h, tok, c.method, c.path)
		if containsNotFound(body) {
			t.Errorf("路由未注册: %s %s → %d %s", c.method, c.path, code, body)
		}
	}
}

// containsNotFound 报告响应体是不是 ServeMux 未匹配时的纯文本 404。
func containsNotFound(body string) bool {
	return len(body) >= len("404 page not found") && body[:len("404 page not found")] == "404 page not found"
}

// TestStrategyEdgesListsRelations 钉住策略之间的关系边能被读到。
//
// 策略之间的「requires / variant_of / blocks / supersedes」是策略库的关系面
// （db/schema_strategy.sql 的 strategy_edges 表）—— 读不到它，前端画不出
// 策略之间的关系图。
func TestStrategyEdgesListsRelations(t *testing.T) {
	pg, _, h, tok := sodAPIServer(t)
	from := strategyReadStrategy(t, pg, "关系用例-从-"+strconv.FormatInt(sodAPISuffix()%100000, 10))
	to := strategyReadStrategy(t, pg, "关系用例-到-"+strconv.FormatInt(sodAPISuffix()%100000, 10))
	if _, err := pg.Exec(`INSERT INTO strategy_edges(from_id, to_id, relation) VALUES ($1,$2,'supersedes')
ON CONFLICT DO NOTHING`, from, to); err != nil {
		t.Fatalf("写策略关系边: %v", err)
	}

	code, out := sodAPIDo(t, h, tok, "GET", "/api/strategies/edges", nil)
	if code != 200 {
		t.Fatalf("读策略关系边应当成功，实际 %d %v", code, out)
	}
	edges, _ := out["edges"].([]any)
	found := false
	for _, it := range edges {
		row, _ := it.(map[string]any)
		if row == nil {
			continue
		}
		f, ok1 := row["from_id"].(float64)
		tt, ok2 := row["to_id"].(float64)
		if !ok1 || !ok2 || int64(f) != from || int64(tt) != to {
			continue
		}
		found = true
		if row["relation"] != "supersedes" {
			t.Fatalf("关系类型不对: %v", row)
		}
	}
	if !found {
		t.Fatalf("关系边列表里没有那条 supersedes: %v", out)
	}
}

// TestStrategyUtilityReadsMaterializedView 钉住 utility 物化视图能被读到。
//
// utility 的权威值在物化视图里（strategies.utility 只是缓存值）—— 前端要渲染
// 「这条策略到底值不值钱」，读的就是这个视图。
func TestStrategyUtilityReadsMaterializedView(t *testing.T) {
	pg, _, h, tok := sodAPIServer(t)
	sid := strategyReadStrategy(t, pg, "utility 用例-"+strconv.FormatInt(sodAPISuffix()%100000, 10))

	// 一条 confirmed 的漏洞 + 指向它的证据链 ⇒ 视图里才有这一行。
	var fid int64
	if err := pg.QueryRow(`INSERT INTO findings(vulnclass, name, severity, status)
VALUES ('sqli','utility 用例漏洞','high','confirmed') RETURNING id`).Scan(&fid); err != nil {
		t.Fatalf("写漏洞行: %v", err)
	}
	t.Cleanup(func() { _, _ = pg.Exec(`DELETE FROM findings WHERE id=$1`, fid) })
	if _, err := pg.Exec(`INSERT INTO strategy_evidence(strategy_id, ref_type, ref_id, content_digest, role)
VALUES ($1,'finding',$2,'digest','created_by') ON CONFLICT DO NOTHING`,
		sid, strconv.FormatInt(fid, 10)); err != nil {
		t.Fatalf("写证据链: %v", err)
	}
	if err := pg.RefreshStrategyUtility(); err != nil {
		t.Fatalf("刷 utility 物化视图: %v", err)
	}

	code, out := sodAPIDo(t, h, tok, "GET", "/api/strategies/utility", nil)
	if code != 200 {
		t.Fatalf("读 utility 视图应当成功，实际 %d %v", code, out)
	}
	rows, _ := out["utility"].([]any)
	found := false
	for _, it := range rows {
		row, _ := it.(map[string]any)
		if row == nil {
			continue
		}
		if v, ok := row["strategy_id"].(float64); ok && int64(v) == sid {
			found = true
			if row["evidence_count"].(float64) < 1 {
				t.Fatalf("utility 行的证据数不对: %v", row)
			}
		}
	}
	if !found {
		t.Fatalf("utility 视图里没有策略 %d: %v", sid, out)
	}
}

// TestStrategyCurationShowsArbitrationBoard 钉住 curator 的仲裁面能被读到。
//
// curator 是裁决写（晋级 / 淘汰 / 墓碑 / pin），它的判据全在库里（状态比 +
// 关系查询）⇒ 仲裁面可以被读出来给人看：谁被淘汰了、为什么、唯一证人在哪。
func TestStrategyCurationShowsArbitrationBoard(t *testing.T) {
	pg, _, h, tok := sodAPIServer(t)
	sid := strategyReadStrategy(t, pg, "仲裁用例-"+strconv.FormatInt(sodAPISuffix()%100000, 10))
	if _, err := pg.Exec(`UPDATE strategies SET state='retired', retired_at=now(),
retired_reason='运行效用为负：连续未命中里程碑' WHERE id=$1`, sid); err != nil {
		t.Fatalf("写退役状态: %v", err)
	}

	code, out := sodAPIDo(t, h, tok, "GET", "/api/strategies/curation", nil)
	if code != 200 {
		t.Fatalf("读仲裁面应当成功，实际 %d %v", code, out)
	}
	retired, _ := out["retired"].([]any)
	found := false
	for _, it := range retired {
		row, _ := it.(map[string]any)
		if row == nil {
			continue
		}
		if v, ok := row["id"].(float64); ok && int64(v) == sid {
			found = true
			if row["retired_reason"] != "运行效用为负：连续未命中里程碑" {
				t.Fatalf("退役理由不对: %v", row)
			}
		}
	}
	if !found {
		t.Fatalf("仲裁面里没有那条被淘汰的策略: %v", out)
	}
	if out["advisory_only"] != true {
		t.Fatalf("仲裁面必须标明是只读面（裁决写不在这里发生）: %v", out)
	}
}

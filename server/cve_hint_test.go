package server

// B-04F 测试：模板渲染（纯）+ live PG 发出链（生命周期/去重重跑/单任务节流/
// 参数闸）。求值器注入本地 fake——B-05 判定逻辑在侧车自测，这里只验接线。

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/cveintel"
	"github.com/Autumn-27/artex/db"
)

type cveHintFakeEval struct{ verdicts map[string]string }

func (f *cveHintFakeEval) EvaluateRanges(_ context.Context, _, ver string, _ []cveintel.Range) (string, string, error) {
	if v, ok := f.verdicts[ver]; ok {
		return v, "in range", nil
	}
	return "maybe", "", nil
}

func cveHintFloat(v float64) *float64 { return &v }

// 固定模板逐句断言：资产/CVE/判定/CVSS/KEV/「请验证」句全在，且「疑似」语义
// 不被渲染成确认。
func TestRenderCVEHint(t *testing.T) {
	m := db.AssetMatch{
		AssetDomain:    "web.example.com",
		Verdict:        db.VerdictAffected,
		MatchedProduct: "log4j",
		MatchedVersion: "2.14.0",
	}
	rec := &cveintel.Record{CVEID: "CVE-2021-44228", CVSSv3: cveHintFloat(9.8), InKEV: true}
	out := RenderCVEHint(m, rec)
	for _, want := range []string{
		"CVE 情报：资产 web.example.com 疑似受 CVE-2021-44228 影响",
		"affected，资产版本 2.14.0 命中影响区间",
		"CVSS 9.8",
		"KEV=是",
		"请验证后决定是否立项验证意图",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("模板缺片段 %q: %s", want, out)
		}
	}
	if strings.Contains(out, "已确认") {
		t.Fatalf("hint 绝不渲染成确认: %s", out)
	}

	m2 := db.AssetMatch{AssetDomain: "a.example.com", Verdict: db.VerdictMaybe}
	out2 := RenderCVEHint(m2, &cveintel.Record{CVEID: "CVE-2024-0000"})
	for _, want := range []string{"（maybe，资产指纹无版本号）", "CVSS 无", "KEV=否"} {
		if !strings.Contains(out2, want) {
			t.Fatalf("maybe 模板缺片段 %q: %s", want, out2)
		}
	}
}

// 生命周期：匹配→发出 1 条→节点/台账落位→重跑零重复。
func TestEmitCVEHintsLifecycle(t *testing.T) {
	pg := cveTestPG(t)
	srv := &Server{m: &Manager{pg: pg}}

	task, err := pg.CreateTask("cve-hint-emit", "goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pg.DeleteTask(task.ID) })

	assetID, err := pg.Assets().UpsertHTTPService(db.UpsertHTTPServiceReq{
		URL:          "https://cvehint-emit.example.com/",
		Technologies: []string{"Apache Log4j 2.14.0"},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pg.Assets().DeleteByIDs([]int64{assetID}) })
	if _, err := pg.Assets().AttachAssetsToTask(task.ID, []int64{assetID}, "B-04F 测试挂靠"); err != nil {
		t.Fatal(err)
	}
	// 授权边：hint 走 AddNodeInScope，锚点必须落在任务声明的 task_scope 内
	// （挂靠≠scope）——这正是 B-04 卡「不扩大任务 scope」纪律的运行时防线。
	// 直插 kind='subdomain' 行精确覆盖资产 domain（RegisterTaskAssetScopes 的
	// "domain" 输入归一成 root_domain，其根域分类器与本资产 root_domain 判定
	// 不一致——仓库现状如实记录）。
	if _, err := pg.Exec(`INSERT INTO task_scope(task_id, kind, domain, source, reason) VALUES ($1,'subdomain','cvehint-emit.example.com','manual','B-04F 测试范围')`, task.ID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pg.Exec(`DELETE FROM task_scope WHERE task_id=$1 AND kind='subdomain'`, task.ID) })

	cveID := fmt.Sprintf("CVE-2021-44228-EMIT-%d", time.Now().UnixNano())
	t.Cleanup(func() { _, _ = pg.Exec(`DELETE FROM cve_hints WHERE cve_id=$1`, cveID) })

	matches, err := pg.MatchCVEToAssets(context.Background(), &cveHintFakeEval{verdicts: map[string]string{"2.14.0": "affected"}}, db.CVEMatchInput{
		CVEID:         cveID,
		ProductTokens: []string{"log4j"},
		Ranges:        []cveintel.Range{{From: "2.0", To: "2.15.0"}},
	})
	if err != nil || len(matches) != 1 || len(matches[0].TaskIDs) != 1 {
		t.Fatalf("匹配链应命中 1 资产 1 任务: %d %v", len(matches), err)
	}

	rec := &cveintel.Record{CVEID: cveID, CVSSv3: cveHintFloat(9.8), InKEV: true}
	stats, err := srv.EmitCVEHints(context.Background(), matches, rec)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Considered != 1 || stats.Emitted != 1 {
		t.Fatalf("应发出恰 1 条: %+v", stats)
	}

	// hint 节点落位：kind/state/origin/payload/锚点。
	nodes, err := pg.Exploration(task.ExplorationID).ListByKind(db.KindHint, 100)
	if err != nil {
		t.Fatal(err)
	}
	var hit *db.Node
	for _, n := range nodes {
		if strings.Contains(string(n.Payload), cveID) {
			if hit != nil {
				t.Fatal("同 CVE 不应有第二个 hint 节点")
			}
			hit = n
		}
	}
	if hit == nil {
		t.Fatalf("任务图里应存在该 CVE 的 hint 节点（共 %d 条 hint）", len(nodes))
	}
	if hit.State != "active" || hit.Origin != "cve-intel" {
		t.Fatalf("节点 state/origin 错误: %s %s", hit.State, hit.Origin)
	}
	full, err := pg.Exploration(task.ExplorationID).GetNode(hit.ID)
	if err != nil {
		t.Fatal(err)
	}
	// GetNode 的列集不含水合锚点——直接查锚点表断言授权边落位。
	var anchorCount int
	if err := pg.QueryRow(`SELECT count(*) FROM exploration_anchors WHERE node_id=$1 AND asset_id=$2`, hit.ID, assetID).Scan(&anchorCount); err != nil {
		t.Fatal(err)
	}
	if anchorCount != 1 {
		t.Fatalf("hint 应锚定命中资产（锚点表 %d 行）", anchorCount)
	}
	var payload map[string]any
	if err := json.Unmarshal(full.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["cve_id"] != cveID || payload["verdict"] != "affected" {
		t.Fatalf("payload 结构化键缺失: %v", payload)
	}
	if !strings.Contains(payload["text"].(string), "请验证后决定是否立项验证意图") {
		t.Fatalf("hint 文本必须含「请验证」句: %v", payload["text"])
	}

	// 台账落位。
	exists, err := pg.HasCVEHint(task.ID, assetID, cveID)
	if err != nil || !exists {
		t.Fatalf("台账应命中: %v %v", exists, err)
	}

	// 重跑同步：零重复（Deduped 收敛，节点数不变）。
	stats, err = srv.EmitCVEHints(context.Background(), matches, rec)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Emitted != 0 || stats.Deduped != 1 {
		t.Fatalf("重跑必须全去重: %+v", stats)
	}
	nodes2, _ := pg.Exploration(task.ExplorationID).ListByKind(db.KindHint, 100)
	count := 0
	for _, n := range nodes2 {
		if strings.Contains(string(n.Payload), cveID) {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("重跑后节点必须仍恰 1 个（同 CVE 重跑同步不重复挂 hint）: %d", count)
	}
}

// 节流：单批对同一任务最多 20 条，超额记日志跳过且台账无行（下批重试不丢库）。
func TestEmitCVEHintsThrottle(t *testing.T) {
	pg := cveTestPG(t)
	srv := &Server{m: &Manager{pg: pg}}

	task, err := pg.CreateTask("cve-hint-throttle", "goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pg.DeleteTask(task.ID) })

	cveID := fmt.Sprintf("CVE-2024-THROTTLE-%d", time.Now().UnixNano())
	t.Cleanup(func() { _, _ = pg.Exec(`DELETE FROM cve_hints WHERE cve_id=$1`, cveID) })

	const total = cveHintMaxPerTaskPerBatch + 5
	assetIDs := make([]int64, 0, total)
	matches := make([]db.AssetMatch, 0, total)
	for i := 0; i < total; i++ {
		domain := fmt.Sprintf("cvehint-t%d-%d.example.com", i, time.Now().UnixNano())
		id, err := pg.Assets().UpsertHTTPService(db.UpsertHTTPServiceReq{
			URL:          "https://" + domain + "/",
			Technologies: []string{"log4j 1.0"},
		})
		if err != nil {
			t.Fatal(err)
		}
		assetIDs = append(assetIDs, id)
		// 授权边：逐资产声明 subdomain scope（直插 task_scope，理由同生命周期测试）。
		if _, err := pg.Exec(`INSERT INTO task_scope(task_id, kind, domain, source, reason) VALUES ($1,'subdomain',$2,'manual','B-04F 节流测试范围')`, task.ID, domain); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _, _ = pg.Assets().DeleteByIDs([]int64{id}) })
		matches = append(matches, db.AssetMatch{
			AssetID:        id,
			AssetDomain:    domain,
			Verdict:        db.VerdictMaybe,
			MatchedVersion: "1.0",
			TaskIDs:        []int64{task.ID},
		})
	}

	stats, err := srv.EmitCVEHints(context.Background(), matches, &cveintel.Record{CVEID: cveID})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Emitted != cveHintMaxPerTaskPerBatch || stats.Throttled != 5 {
		t.Fatalf("应发出 %d 节流 5: %+v", cveHintMaxPerTaskPerBatch, stats)
	}
	var ledger int
	if err := pg.QueryRow(`SELECT count(*) FROM cve_hints WHERE cve_id=$1`, cveID).Scan(&ledger); err != nil {
		t.Fatal(err)
	}
	if ledger != cveHintMaxPerTaskPerBatch {
		t.Fatalf("台账应 %d 行: %d", cveHintMaxPerTaskPerBatch, ledger)
	}

	// 被节流的 5 条台账无行 → 下一批重试成功（记日志不丢库）。
	stats2, err := srv.EmitCVEHints(context.Background(), matches, &cveintel.Record{CVEID: cveID})
	if err != nil {
		t.Fatal(err)
	}
	if stats2.Emitted != 5 || stats2.Deduped != cveHintMaxPerTaskPerBatch {
		t.Fatalf("下批应补发 5 + 去重 20: %+v", stats2)
	}
}

// 参数闸：无库 / rec nil / 空 cve_id / not_affected 复活企图全部 fail-closed。
func TestEmitCVEHintsGuards(t *testing.T) {
	pg := cveTestPG(t)
	ctx := context.Background()

	if _, err := (&Server{}).EmitCVEHints(ctx, nil, &cveintel.Record{CVEID: "CVE-X"}); err == nil {
		t.Fatal("无库必须报错")
	}
	srv := &Server{m: &Manager{pg: pg}}
	if _, err := srv.EmitCVEHints(ctx, nil, nil); err == nil {
		t.Fatal("rec=nil 必须报错")
	}
	if _, err := srv.EmitCVEHints(ctx, nil, &cveintel.Record{CVEID: "  "}); err == nil {
		t.Fatal("空 cve_id 必须报错")
	}
	bad := []db.AssetMatch{{Verdict: db.VerdictNotAffected, TaskIDs: []int64{1}}}
	if _, err := srv.EmitCVEHints(ctx, bad, &cveintel.Record{CVEID: "CVE-X"}); err == nil {
		t.Fatal("not_affected 经发出器必须报错（匹配层剔除后绝不复活）")
	}
	if _, err := srv.EmitCVEHints(ctx, nil, &cveintel.Record{CVEID: "CVE-X"}); err != nil {
		t.Fatalf("空匹配集应零工作量: %v", err)
	}
}

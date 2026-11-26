package db

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/cveintel"
)

// B-04A 验收表驱动：Apache Log4j、nginx、OpenSSL、带 vendor 前缀、无版本、
// 版本在产品名内、大小写/空格变体、任意描述文本；不误合并不同产品。
func TestNormalizeTechToken(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		product string
		version string
		ok      bool
	}{
		// 卡验收点名形态
		{"卡例：Apache Log4j 2.14.0", "Apache Log4j 2.14.0", "log4j", "2.14.0", true},
		{"nginx 无版本", "nginx", "nginx", "", true},
		{"OpenSSL 大小写", "OpenSSL", "openssl", "", true},
		{"vendor 前缀 Apache", "Apache Tomcat 9.0.83", "tomcat", "9.0.83", true},
		{"vendor 前缀叠加剥离", "Apache Eclipse Jetty", "jetty", "", true},
		{"无版本产品词组", "Spring Boot", "springboot", "", true},
		{"版本在产品名内（点分可切）", "log4j2.17.0", "log4j", "2.17.0", true},
		{"裸 log4j2 保守不切（数字是名字一部分）", "Log4j2", "log4j2", "", true},
		{"版本内嵌点分 nginx1.25.3", "nginx1.25.3", "nginx", "1.25.3", true},
		{"openssl3 主版本", "openssl3", "openssl", "3", true},
		{"v 前缀版本", "nginx v1.25", "nginx", "v1.25", true},
		{"连字符产品名保留", "log4j-core", "log4j-core", "", true},
		{"连字符+版本", "log4j-core 2.17.0", "log4j-core", "2.17.0", true},
		{"分隔变体归一 spring_boot", "spring_boot", "springboot", "", true},
		{"分隔变体归一 open/ssl", "Open SSL", "openssl", "", true},
		{"下划线+版本", "wordpress_6.4", "wordpress", "6.4", true},

		// 含糊文本：拒绝提取，绝不猜（Raw 保留）
		{"整句描述拒绝", "runs the latest version of their stack", "", "", false},
		{"多版本拒绝", "log4j 2.14.0 and 2.15.0", "", "", false},
		{"多词混数字词拒绝", "log4j 2.14.0 extra", "", "", false},
		{"纯厂商词拒绝", "apache apache", "", "", false},
		{"数字残片拒绝", "2.x", "", "", false},
		{"空串拒绝", "   ", "", "", false},
		{"超长描述拒绝", "a b c d e f g", "", "", false},

		// 数字本就是名字一部分：护栏不误切
		{"sha256 不切", "sha256", "sha256", "", true},
		{"utf8mb4 不切", "utf8mb4", "utf8mb4", "", true},
		{"sha1 不切", "SHA1", "sha1", "", true},
		{"base64 不切", "base64", "base64", "", true},
		{"ipv6 不切", "ipv6", "ipv6", "", true},

		// 相似产品不误合并（卡要求 5）
		{"log4j ≠ log4j-core", "log4j-core 2.17.0", "log4j-core", "2.17.0", true},
	}
	for _, c := range cases {
		got := NormalizeTechToken(c.in)
		if got.OK != c.ok || got.Product != c.product || got.Version != c.version {
			t.Fatalf("%s: NormalizeTechToken(%q) = {Product:%q Version:%q OK:%v}, want {Product:%q Version:%q OK:%v}",
				c.name, c.in, got.Product, got.Version, got.OK, c.product, c.version, c.ok)
		}
		if !c.ok && got.Raw != strings.TrimSpace(c.in) {
			t.Fatalf("%s: 失败结果必须保留 Raw 原值（去首尾空白），得到 %q", c.name, got.Raw)
		}
	}
}

// 确定性：同输入恒同输出（连调两次逐字段相等）。
func TestNormalizeTechTokenDeterministic(t *testing.T) {
	for _, in := range []string{"Apache Log4j 2.14.0", "nginx", "runs the whole stack", "log4j2"} {
		a := NormalizeTechToken(in)
		b := NormalizeTechToken(in)
		if a != b {
			t.Fatalf("NormalizeTechToken(%q) 非确定性: %+v vs %+v", in, a, b)
		}
	}
}

// 相似产品不同 token：不同输入不得归一到同一 product（卡要求 5 的反向断言）。
func TestNormalizeTechTokenNoCrossMerge(t *testing.T) {
	pairs := [][2]string{
		{"nginx", "nginx plus"},
		{"log4j", "log4j-core"},
		{"openssl", "openssh"},
		{"spring", "springboot"}, // spring 与 springboot 是不同 token（仅拼写归一，不跨产品）
	}
	for _, p := range pairs {
		a := NormalizeTechToken(p[0])
		b := NormalizeTechToken(p[1])
		if a.OK && b.OK && a.Product == b.Product {
			t.Fatalf("不同产品被误合并: %q 与 %q 都归一到 %q", p[0], p[1], a.Product)
		}
	}
}

// ---------- B-04B：粗筛查询护栏 + live PG 候选查询（不可用即跳过） ----------

// 粗筛输入闸：token 必须满足 NormalizeTechToken 输出合同，怪形态 fail-closed。
func TestTechTokenQueryForm(t *testing.T) {
	valid := []string{"log4j", "utf8mb4", "log4j-core", "nginx", "s3", "a1-b2"}
	for _, v := range valid {
		if !techTokenQueryForm(v) {
			t.Fatalf("合法 token %q 被拒", v)
		}
	}
	invalid := []string{"", "a", "Log4j", "2fa", "log4j_", "log4j ", "log4j; DROP TABLE assets", "log4j%", "log4j_core", "apache log4j", "-abc"}
	for _, iv := range invalid {
		if techTokenQueryForm(iv) {
			t.Fatalf("非法 token %q 被放行", iv)
		}
	}
}

// 参数 fail-closed：limit/合同校验在发查询之前拒绝（无需 PG 也真实走该分支）。
func TestCandidateAssetsByTokensParamGuard(t *testing.T) {
	d, av2, _ := testSetup(t)
	defer d.Close()

	if _, err := av2.CandidateAssetsByTokens([]string{"log4j"}, 0); err == nil {
		t.Fatal("limit=0 必须 fail-closed 报错")
	}
	if _, err := av2.CandidateAssetsByTokens([]string{"log4j"}, -1); err == nil {
		t.Fatal("limit<0 必须 fail-closed 报错")
	}
	if _, err := av2.CandidateAssetsByTokens([]string{"log4j"}, 1001); err == nil {
		t.Fatal("limit 超硬顶必须报错不静默夹紧")
	}
	if _, err := av2.CandidateAssetsByTokens([]string{"Log4j"}, 10); err == nil {
		t.Fatal("大写 token（未经规范化）必须报错")
	}
	if _, err := av2.CandidateAssetsByTokens([]string{"log4j; DROP TABLE assets"}, 10); err == nil {
		t.Fatal("注入形态 token 必须报错")
	}
	if _, err := av2.CandidateAssetsByTokens(make([]string, 33), 10); err == nil {
		t.Fatal("token 数超上限必须报错")
	}
	got, err := av2.CandidateAssetsByTokens(nil, 10)
	if err != nil || got != nil {
		t.Fatalf("空 token 列表必须 (nil,nil) 零查询: %v %v", got, err)
	}
}

// live PG：种子两个 HTTP 服务资产，验证 token 子串粗筛、大小写归一命中、
// 多 token 去重、limit 封顶、未命中 token 零误报。
func TestCandidateAssetsByTokens(t *testing.T) {
	d, av2, _ := testSetup(t)
	defer d.Close()

	idA, err := av2.UpsertHTTPService(UpsertHTTPServiceReq{
		URL:          "https://cvematch-a.example.com/",
		Technologies: []string{"Apache Log4j 2.14.0", "nginx 1.25"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer deleteAsset(d, idA)

	idB, err := av2.UpsertHTTPService(UpsertHTTPServiceReq{
		URL:          "https://cvematch-b.example.com/",
		Technologies: []string{"nginx/1.25.3"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer deleteAsset(d, idB)

	containsID := func(list []*Asset, id int64) bool {
		for _, a := range list {
			if a.ID == id {
				return true
			}
		}
		return false
	}

	// ① 归一化 token 命中原始描述串（大小写不敏感）：log4j → 仅资产 A。
	hits, err := av2.CandidateAssetsByTokens([]string{"log4j"}, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].ID != idA {
		t.Fatalf("log4j 应恰命中资产 A（得到 %d 行）", len(hits))
	}

	// ② 多 token：log4j→A、nginx→A∪B，A 去重只出现一次，共 2 行。
	hits, err = av2.CandidateAssetsByTokens([]string{"log4j", "nginx"}, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 || !containsID(hits, idA) || !containsID(hits, idB) {
		t.Fatalf("log4j+nginx 应命中 A、B 各一次（得到 %d 行）", len(hits))
	}
	countID := func(list []*Asset, id int64) int {
		n := 0
		for _, a := range list {
			if a.ID == id {
				n++
			}
		}
		return n
	}
	if countID(hits, idA) != 1 {
		t.Fatal("多 token 同资产必须去重只保留首见")
	}

	// ③ limit 封顶：limit=1 时最多 1 行。
	hits, err = av2.CandidateAssetsByTokens([]string{"log4j", "nginx"}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("limit=1 应封顶 1 行（得到 %d）", len(hits))
	}

	// ④ 未命中 token：零误报（绝不为 tomcat 造候选）。
	hits, err = av2.CandidateAssetsByTokens([]string{"tomcat"}, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Fatalf("tomcat 不应命中任何种子资产（得到 %d 行）", len(hits))
	}

	// ⑤ 粗筛刻意过包含：token log4j 也命中存了 "log4j-core 2.17.0" 的资产
	// （精判收敛属 B-04C，粗筛宁滥勿缺）。
	idC, err := av2.UpsertHTTPService(UpsertHTTPServiceReq{
		URL:          "https://cvematch-c.example.com/",
		Technologies: []string{"log4j-core 2.17.0"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer deleteAsset(d, idC)
	hits, err = av2.CandidateAssetsByTokens([]string{"log4j"}, 100)
	if err != nil {
		t.Fatal(err)
	}
	if !containsID(hits, idC) {
		t.Fatal("粗筛应过包含命中 log4j-core 资产（精判在 B-04C 收敛）")
	}
}

// ---------- B-04C：版本精判（注入 fake 求值器，B-05 判定逻辑在侧车自测） ----------

// fakeEvaluator 记录调用参数、按版本出三态（verdict 表缺版本 ⇒ maybe）。
type fakeEvaluator struct {
	verdicts map[string]string // version → verdict
	reason   string
	err      error
	calls    [][2]string // (ecosystem, version) 调用记录
}

func (f *fakeEvaluator) EvaluateRanges(_ context.Context, eco, ver string, _ []cveintel.Range) (string, string, error) {
	f.calls = append(f.calls, [2]string{eco, ver})
	if f.err != nil {
		return "", "", f.err
	}
	if v, ok := f.verdicts[ver]; ok {
		return v, f.reason, nil
	}
	return "maybe", f.reason, nil
}

// 匹配全链：affected / maybe（无版本）/ not_affected 剔除 / 产品等值收敛剔除。
func TestMatchCVEToAssets(t *testing.T) {
	d, av2, _ := testSetup(t)
	defer d.Close()

	idA, err := av2.UpsertHTTPService(UpsertHTTPServiceReq{
		URL:          "https://cveeval-a.example.com/",
		Technologies: []string{"Apache Log4j 2.14.0"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer deleteAsset(d, idA)
	idB, err := av2.UpsertHTTPService(UpsertHTTPServiceReq{
		URL:          "https://cveeval-b.example.com/",
		Technologies: []string{"Apache Log4j"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer deleteAsset(d, idB)
	idC, err := av2.UpsertHTTPService(UpsertHTTPServiceReq{
		URL:          "https://cveeval-c.example.com/",
		Technologies: []string{"log4j-core 2.17.0"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer deleteAsset(d, idC)
	idD, err := av2.UpsertHTTPService(UpsertHTTPServiceReq{
		URL:          "https://cveeval-d.example.com/",
		Technologies: []string{"log4j 9.9.9"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer deleteAsset(d, idD)

	fake := &fakeEvaluator{verdicts: map[string]string{"2.14.0": "affected", "9.9.9": "not_affected"}, reason: "in range"}
	matches, err := d.MatchCVEToAssets(context.Background(), fake, CVEMatchInput{
		CVEID:         "CVE-2021-44228",
		Ecosystem:     "maven",
		ProductTokens: []string{"log4j"},
		Ranges:        []cveintel.Range{{From: "2.0", To: "2.15.0"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	byID := map[int64]AssetMatch{}
	for _, m := range matches {
		byID[m.AssetID] = m
	}
	// ① A：版本命中 → affected，字段齐备。
	m, ok := byID[idA]
	if !ok || m.Verdict != VerdictAffected {
		t.Fatalf("资产 A（2.14.0）应 affected: %+v ok=%v", m, ok)
	}
	if m.MatchedTech != "Apache Log4j 2.14.0" || m.MatchedProduct != "log4j" || m.MatchedVersion != "2.14.0" {
		t.Fatalf("A 匹配明细错误: %+v", m)
	}
	if m.AssetDomain != "cveeval-a.example.com" {
		t.Fatalf("A 标识错误: %q", m.AssetDomain)
	}
	// ② B：产品命中但无版本 → maybe 绝不 affected（卡明文）。
	m, ok = byID[idB]
	if !ok || m.Verdict != VerdictMaybe {
		t.Fatalf("资产 B（无版本）应 maybe: %+v ok=%v", m, ok)
	}
	// ③ C：粗筛子串过包含命中，但归一化产品 log4j-core ∉ token 集 → 精判层剔除
	//（B-04A 不误合并纪律的落点），且绝不调求值器。
	if _, ok := byID[idC]; ok {
		t.Fatal("log4j-core 资产不得进结果（产品等值收敛）")
	}
	// ④ D：not_affected 剔除（卡骨架步骤 3）。
	if _, ok := byID[idD]; ok {
		t.Fatal("not_affected 资产不得进结果")
	}
	// ⑤ 求值器只对带版本的命中被调用（A、D 各一次），ecosystem 透传。
	if len(fake.calls) != 2 {
		t.Fatalf("求值器应恰被调 2 次（A、D）: %v", fake.calls)
	}
	for _, c := range fake.calls {
		if c[0] != "maven" {
			t.Fatalf("ecosystem 未透传: %v", fake.calls)
		}
	}
}

// CVE 侧无区间：全部产品命中候选判 maybe（信息不足 fail-closed 同向）。
func TestMatchCVEToAssetsEmptyRangesMaybe(t *testing.T) {
	d, av2, _ := testSetup(t)
	defer d.Close()

	id, err := av2.UpsertHTTPService(UpsertHTTPServiceReq{
		URL:          "https://cveeval-e.example.com/",
		Technologies: []string{"log4j 2.14.0"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer deleteAsset(d, id)

	fake := &fakeEvaluator{}
	matches, err := d.MatchCVEToAssets(context.Background(), fake, CVEMatchInput{
		CVEID:         "CVE-2024-0001",
		ProductTokens: []string{"log4j"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 || matches[0].Verdict != VerdictMaybe {
		t.Fatalf("无区间应 maybe: %+v", matches)
	}
	if len(fake.calls) != 0 {
		t.Fatal("无区间不得调求值器")
	}
}

// 求值器失败 ⇒ 整体报错（绝不解释成没有漏洞），错误含 CVE 与资产定位。
func TestMatchCVEToAssetsEvalErrorAborts(t *testing.T) {
	d, av2, _ := testSetup(t)
	defer d.Close()

	id, err := av2.UpsertHTTPService(UpsertHTTPServiceReq{
		URL:          "https://cveeval-f.example.com/",
		Technologies: []string{"log4j 2.14.0"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer deleteAsset(d, id)

	fake := &fakeEvaluator{err: fmt.Errorf("cveintel: upstream unavailable")}
	_, err = d.MatchCVEToAssets(context.Background(), fake, CVEMatchInput{
		CVEID:         "CVE-2021-44228",
		ProductTokens: []string{"log4j"},
		Ranges:        []cveintel.Range{{From: "2.0", To: "2.15.0"}},
	})
	if err == nil {
		t.Fatal("求值失败必须整体报错，绝不静默当 not_affected/跳过")
	}
	if !strings.Contains(err.Error(), "CVE-2021-44228") {
		t.Fatalf("错误必须含 CVE 定位: %v", err)
	}
}

// 参数闸：eval nil / 空 CVEID / 非合同 token / 空 token 集全部 fail-closed。
func TestMatchCVEToAssetsParamGuard(t *testing.T) {
	d, _, _ := testSetup(t)
	defer d.Close()

	ctx := context.Background()
	in := CVEMatchInput{CVEID: "CVE-2021-44228", ProductTokens: []string{"log4j"}, Ranges: []cveintel.Range{{From: "2.0", To: "2.15.0"}}}

	if _, err := d.MatchCVEToAssets(ctx, nil, in); err == nil {
		t.Fatal("eval=nil 必须报错（精判绝不静默跳过）")
	}
	if _, err := d.MatchCVEToAssets(ctx, &fakeEvaluator{}, CVEMatchInput{ProductTokens: []string{"log4j"}, Ranges: in.Ranges}); err == nil {
		t.Fatal("空 CVEID 必须报错")
	}
	if _, err := d.MatchCVEToAssets(ctx, &fakeEvaluator{}, CVEMatchInput{CVEID: "CVE-2021-44228", ProductTokens: []string{"Log4j"}}); err == nil {
		t.Fatal("非合同 token 必须报错")
	}
	got, err := d.MatchCVEToAssets(ctx, &fakeEvaluator{}, CVEMatchInput{CVEID: "CVE-2021-44228"})
	if err != nil || got != nil {
		t.Fatalf("空 token 集 (nil,nil): %v %v", got, err)
	}
}

// ---------- B-04D：资产→未终态任务归属 ----------

// 归属查询：created/running/paused 收、done/timeout 终态与软删除排除、
// 未知资产零伪造、任务列表升序确定性。
func TestNonTerminalTasksByAssets(t *testing.T) {
	d, av2, _ := testSetup(t)
	defer d.Close()

	task, err := d.CreateTask("cve-match-ownership", "goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(task.ID) })

	id, err := av2.UpsertHTTPService(UpsertHTTPServiceReq{
		URL:          "https://cveown.example.com/",
		Technologies: []string{"log4j 2.14.0"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer deleteAsset(d, id)
	if _, err := av2.AttachAssetsToTask(task.ID, []int64{id}, "B-04D 测试挂靠"); err != nil {
		t.Fatal(err)
	}

	// ① 新建任务（created）→ 归属在。
	got, err := av2.NonTerminalTasksByAssets([]int64{id})
	if err != nil {
		t.Fatal(err)
	}
	if len(got[id]) != 1 || got[id][0] != task.ID {
		t.Fatalf("created 任务应归属: %v", got)
	}

	// ② 终态 done → 排除。
	if _, err := d.Exec(`UPDATE tasks SET status='done' WHERE id=$1`, task.ID); err != nil {
		t.Fatal(err)
	}
	got, err = av2.NonTerminalTasksByAssets([]int64{id})
	if err != nil {
		t.Fatal(err)
	}
	if len(got[id]) != 0 {
		t.Fatalf("done 任务必须排除: %v", got)
	}

	// ③ 回到 running → 归属在；再软删除 → 排除。
	if _, err := d.Exec(`UPDATE tasks SET status='running' WHERE id=$1`, task.ID); err != nil {
		t.Fatal(err)
	}
	got, _ = av2.NonTerminalTasksByAssets([]int64{id})
	if len(got[id]) != 1 {
		t.Fatalf("running 任务应归属: %v", got)
	}
	if _, err := d.Exec(`UPDATE tasks SET deleted_at=now() WHERE id=$1`, task.ID); err != nil {
		t.Fatal(err)
	}
	got, _ = av2.NonTerminalTasksByAssets([]int64{id})
	if len(got[id]) != 0 {
		t.Fatalf("软删除任务必须排除: %v", got)
	}

	// ④ 未知/非法资产 ID：零伪造。
	got, err = av2.NonTerminalTasksByAssets([]int64{999999999})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("未知资产不得出现在返回: %v", got)
	}
	got, err = av2.NonTerminalTasksByAssets(nil)
	if err != nil || len(got) != 0 {
		t.Fatalf("空输入应空 map: %v %v", got, err)
	}
}

// 匹配全链 × 归属：命中资产带未终态任务 ID；任务终态后 TaskIDs 空（hint 发出
// 层 B-04F 依此零发出）。
func TestMatchCVEToAssetsTaskOwnership(t *testing.T) {
	d, av2, _ := testSetup(t)
	defer d.Close()

	task, err := d.CreateTask("cve-match-chain", "goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(task.ID) })

	id, err := av2.UpsertHTTPService(UpsertHTTPServiceReq{
		URL:          "https://cvechain.example.com/",
		Technologies: []string{"Apache Log4j 2.14.0"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer deleteAsset(d, id)
	if _, err := av2.AttachAssetsToTask(task.ID, []int64{id}, "B-04D 链路测试"); err != nil {
		t.Fatal(err)
	}

	fake := &fakeEvaluator{verdicts: map[string]string{"2.14.0": "affected"}}
	in := CVEMatchInput{
		CVEID:         "CVE-2021-44228",
		ProductTokens: []string{"log4j"},
		Ranges:        []cveintel.Range{{From: "2.0", To: "2.15.0"}},
	}
	matches, err := d.MatchCVEToAssets(context.Background(), fake, in)
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 || matches[0].AssetID != id {
		t.Fatalf("应恰命中种子资产: %+v", matches)
	}
	if len(matches[0].TaskIDs) != 1 || matches[0].TaskIDs[0] != task.ID {
		t.Fatalf("命中资产应带未终态任务归属: %+v", matches[0].TaskIDs)
	}

	// 任务终态 → 同一匹配重跑：TaskIDs 空（资产行仍在，归属消失）。
	if _, err := d.Exec(`UPDATE tasks SET status='done' WHERE id=$1`, task.ID); err != nil {
		t.Fatal(err)
	}
	matches, err = d.MatchCVEToAssets(context.Background(), fake, in)
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 || len(matches[0].TaskIDs) != 0 {
		t.Fatalf("终态任务后归属必须清空: %+v", matches)
	}
}

// osv_range_adapter_test.go：B-08C 验收——适配层纯本地测试（fake 求值器）+
// 经真实 cveintel.Client 的 httptest 假 MCP 面端到端（信封/事件 wire 形态）。
// 覆盖卡验收逐项：lodash@4.17.20 fixture、修复版本之上 not_affected、
// introduced/fixed、last_affected、空区间、未知事件/生态、坏版本、缺失版本、
// 三态命名与 B-05 输出一致、ID/alias/reason 保留。
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/cveintel"
	"github.com/Autumn-27/artex/db"
)

// osvEvalFake 记录调用并回罐装三态（适配层测试用——单体绝不复制比较器，
// fake 的 verdict 是「B-05 会怎么说」的预设，不是本地实现）。
type osvEvalFake struct {
	calls      int
	lastEco    string
	lastVer    string
	lastRanges []cveintel.Range
	verdict    string
	reason     string
	err        error
}

func (f *osvEvalFake) EvaluateRanges(_ context.Context, ecosystem, version string, ranges []cveintel.Range) (string, string, error) {
	f.calls++
	f.lastEco = ecosystem
	f.lastVer = version
	f.lastRanges = ranges
	if f.err != nil {
		return "", "", f.err
	}
	return f.verdict, f.reason, nil
}

// osvLodashVuln 构造 lodash fixture（introduced 0 / fixed 4.17.21）。
func osvLodashVuln() OSVVuln {
	return OSVVuln{
		ID:      "GHSA-35jh-r3h4-6jhm",
		Aliases: []string{"CVE-2020-8203"},
		Affected: []OSVAffected{{
			Package: OSVAffectedPackage{Ecosystem: "npm", Name: "lodash"},
			Ranges: []OSVRange{{
				Type:   "SEMVER",
				Events: []OSVRangeEvent{{Introduced: "0"}, {Fixed: "4.17.21"}},
			}},
		}},
	}
}

func osvLodashRef(ver string) OSVQueryRef {
	return OSVQueryRef{Ecosystem: "npm", Name: "lodash", Version: ver}
}

// lodash@4.17.20 fixture：introduced/fixed 区间内 ⇒ affected 透传，ID/alias/
// reason 保留，求值参数形态正确。
func TestEvaluateOSVVulnLodashAffected(t *testing.T) {
	fake := &osvEvalFake{verdict: "affected", reason: "版本 4.17.20 落在 [0, 4.17.21) 受影响区间"}
	got := EvaluateOSVVuln(context.Background(), fake, osvLodashRef("4.17.20"), osvLodashVuln())
	if got.Verdict != db.VerdictAffected {
		t.Fatalf("应 affected: %+v", got)
	}
	if got.VulnID != "GHSA-35jh-r3h4-6jhm" || len(got.Aliases) != 1 || got.Aliases[0] != "CVE-2020-8203" {
		t.Fatalf("ID/alias 应保留: %+v", got)
	}
	if got.Reason != "版本 4.17.20 落在 [0, 4.17.21) 受影响区间" {
		t.Fatalf("reason 应透传: %+v", got)
	}
	if fake.calls != 1 || fake.lastEco != "npm" || fake.lastVer != "4.17.20" {
		t.Fatalf("求值参数: %+v", fake)
	}
	// 事件 wire 形态：单键 introduced/fixed。
	if len(fake.lastRanges) != 1 || len(fake.lastRanges[0].Events) != 2 {
		t.Fatalf("应恰 1 区间 2 事件: %+v", fake.lastRanges)
	}
	if fake.lastRanges[0].Events[0]["introduced"] != "0" || fake.lastRanges[0].Events[1]["fixed"] != "4.17.21" {
		t.Fatalf("事件形态: %+v", fake.lastRanges[0].Events)
	}
}

// 修复版本之上：not_affected 只能来自 B-05 透传（fake 回什么就是什么）。
func TestEvaluateOSVVulnFixedAboveNotAffected(t *testing.T) {
	fake := &osvEvalFake{verdict: "not_affected", reason: "版本 4.17.21 不在任何受影响区间"}
	got := EvaluateOSVVuln(context.Background(), fake, osvLodashRef("4.17.21"), osvLodashVuln())
	if got.Verdict != db.VerdictNotAffected {
		t.Fatalf("应透传 not_affected: %+v", got)
	}
	// 适配层自身绝不产生 not_affected：把 fake 回 affected 之外的所有路径
	// （失败/不匹配/缺材料）逐一体检——见本文件其余 maybe 用例。
}

// last_affected 事件转换；非 0 introduced。
func TestEvaluateOSVVulnIntroducedLastAffected(t *testing.T) {
	vuln := OSVVuln{ID: "GHSA-x", Affected: []OSVAffected{{
		Package: OSVAffectedPackage{Ecosystem: "npm", Name: "lodash"},
		Ranges: []OSVRange{{Type: "ECOSYSTEM", Events: []OSVRangeEvent{
			{Introduced: "4.17.0"}, {LastAffected: "4.17.19"},
		}}},
	}}}
	fake := &osvEvalFake{verdict: "affected", reason: "r"}
	got := EvaluateOSVVuln(context.Background(), fake, osvLodashRef("4.17.19"), vuln)
	if got.Verdict != db.VerdictAffected || fake.calls != 1 {
		t.Fatalf("last_affected 应可求值: %+v fake=%+v", got, fake)
	}
	if fake.lastRanges[0].Events[1]["last_affected"] != "4.17.19" {
		t.Fatalf("last_affected 事件形态: %+v", fake.lastRanges[0].Events)
	}
}

// 空区间 / 无 ranges：maybe 且零求值调用。
func TestEvaluateOSVVulnEmptyRanges(t *testing.T) {
	for name, vuln := range map[string]OSVVuln{
		"无ranges": {ID: "GHSA-a", Affected: []OSVAffected{{
			Package: OSVAffectedPackage{Ecosystem: "npm", Name: "lodash"},
		}}},
		"空ranges数组": {ID: "GHSA-b", Affected: []OSVAffected{{
			Package: OSVAffectedPackage{Ecosystem: "npm", Name: "lodash"},
			Ranges:  []OSVRange{{Type: "SEMVER"}},
		}}},
	} {
		fake := &osvEvalFake{verdict: "affected"}
		got := EvaluateOSVVuln(context.Background(), fake, osvLodashRef("4.17.20"), vuln)
		if got.Verdict != db.VerdictMaybe || fake.calls != 0 {
			t.Fatalf("%s 应 maybe 且零调用: %+v calls=%d", name, got, fake.calls)
		}
	}
}

// 未知事件（limit）/空事件对象/GIT 类型：整条不可求值 ⇒ maybe 零调用。
func TestEvaluateOSVVulnUnusableRanges(t *testing.T) {
	for name, vuln := range map[string]OSVVuln{
		"limit事件": {ID: "GHSA-c", Affected: []OSVAffected{{
			Package: OSVAffectedPackage{Ecosystem: "npm", Name: "lodash"},
			Ranges: []OSVRange{{Type: "SEMVER", Events: []OSVRangeEvent{
				{Introduced: "0"}, {Limit: "1.0.0"},
			}}},
		}}},
		"空事件对象": {ID: "GHSA-d", Affected: []OSVAffected{{
			Package: OSVAffectedPackage{Ecosystem: "npm", Name: "lodash"},
			Ranges:  []OSVRange{{Type: "SEMVER", Events: []OSVRangeEvent{{}}}},
		}}},
		"GIT类型": {ID: "GHSA-e", Affected: []OSVAffected{{
			Package: OSVAffectedPackage{Ecosystem: "npm", Name: "lodash"},
			Ranges: []OSVRange{{Type: "GIT", Repo: "https://github.com/lodash/lodash",
				Events: []OSVRangeEvent{{Introduced: "0"}, {Fixed: "4.17.21"}}}},
		}}},
	} {
		fake := &osvEvalFake{verdict: "affected"}
		got := EvaluateOSVVuln(context.Background(), fake, osvLodashRef("4.17.20"), vuln)
		if got.Verdict != db.VerdictMaybe || fake.calls != 0 {
			t.Fatalf("%s 应 maybe 且零调用: %+v calls=%d", name, got, fake.calls)
		}
	}
}

// 生态/名字不匹配：无命中绝不判 not_affected ⇒ maybe 零调用；混合条目时
// 仅 npm 匹配条目进求值且 reason 如实计数。
func TestEvaluateOSVVulnMismatch(t *testing.T) {
	// 全部不匹配。
	pypi := OSVVuln{ID: "GHSA-f", Affected: []OSVAffected{{
		Package: OSVAffectedPackage{Ecosystem: "PyPI", Name: "lodash"},
		Ranges:  []OSVRange{{Type: "SEMVER", Events: []OSVRangeEvent{{Introduced: "0"}, {Fixed: "4.17.21"}}}},
	}}}
	fake := &osvEvalFake{verdict: "not_affected"}
	got := EvaluateOSVVuln(context.Background(), fake, osvLodashRef("4.17.20"), pypi)
	if got.Verdict != db.VerdictMaybe || fake.calls != 0 {
		t.Fatalf("生态不匹配应 maybe 零调用（绝不 not_affected）: %+v calls=%d", got, fake.calls)
	}
	// 名字不匹配。
	wrongName := OSVVuln{ID: "GHSA-g", Affected: []OSVAffected{{
		Package: OSVAffectedPackage{Ecosystem: "npm", Name: "lodash-es"},
		Ranges:  []OSVRange{{Type: "SEMVER", Events: []OSVRangeEvent{{Introduced: "0"}, {Fixed: "4.17.21"}}}},
	}}}
	fake2 := &osvEvalFake{verdict: "not_affected"}
	got2 := EvaluateOSVVuln(context.Background(), fake2, osvLodashRef("4.17.20"), wrongName)
	if got2.Verdict != db.VerdictMaybe || fake2.calls != 0 {
		t.Fatalf("名字不匹配应 maybe: %+v calls=%d", got2, fake2.calls)
	}
	// 混合：PyPI 条目跳过 + npm 条目求值，reason 带计数。
	mixed := OSVVuln{ID: "GHSA-h", Aliases: []string{"CVE-2021-0001"}, Affected: []OSVAffected{
		{Package: OSVAffectedPackage{Ecosystem: "PyPI", Name: "lodash"}},
		{Package: OSVAffectedPackage{Ecosystem: "NPM", Name: "lodash"},
			Ranges: []OSVRange{{Type: "SEMVER", Events: []OSVRangeEvent{{Introduced: "0"}, {Fixed: "4.17.21"}}}}},
	}}
	fake3 := &osvEvalFake{verdict: "affected", reason: "B-05 结论"}
	got3 := EvaluateOSVVuln(context.Background(), fake3, osvLodashRef("4.17.20"), mixed)
	if got3.Verdict != db.VerdictAffected || fake3.calls != 1 {
		t.Fatalf("混合应按 npm 条目求值: %+v calls=%d", got3, fake3.calls)
	}
	if !strings.Contains(got3.Reason, "B-05 结论") || !strings.Contains(got3.Reason, "1 条 affected 不匹配") {
		t.Fatalf("reason 应保留 B-05 输出并如实计数: %+v", got3.Reason)
	}
}

// 显式 versions 精确等值命中 ⇒ affected（零求值调用）；未等值命中 ⇒ 不判
// not_affected（继续走区间）。
func TestEvaluateOSVVulnExplicitVersions(t *testing.T) {
	vuln := OSVVuln{ID: "GHSA-i", Affected: []OSVAffected{{
		Package:  OSVAffectedPackage{Ecosystem: "npm", Name: "lodash"},
		Versions: []string{"4.17.19", "4.17.20"},
	}}}
	fake := &osvEvalFake{verdict: "not_affected"}
	got := EvaluateOSVVuln(context.Background(), fake, osvLodashRef("4.17.20"), vuln)
	if got.Verdict != db.VerdictAffected || fake.calls != 0 {
		t.Fatalf("显式清单等值命中应 affected 零调用: %+v calls=%d", got, fake.calls)
	}
	// 未命中：无区间 ⇒ maybe（绝不 not_affected）。
	fake2 := &osvEvalFake{verdict: "not_affected"}
	got2 := EvaluateOSVVuln(context.Background(), fake2, osvLodashRef("1.0.0"), vuln)
	if got2.Verdict != db.VerdictMaybe || fake2.calls != 0 {
		t.Fatalf("清单未命中且无区间应 maybe: %+v calls=%d", got2, fake2.calls)
	}
	// 未命中但有区间：继续走区间求值。
	vulnR := OSVVuln{ID: "GHSA-i", Affected: []OSVAffected{{
		Package:  OSVAffectedPackage{Ecosystem: "npm", Name: "lodash"},
		Versions: []string{"4.17.19", "4.17.20"},
		Ranges:   []OSVRange{{Type: "SEMVER", Events: []OSVRangeEvent{{Introduced: "0"}, {Fixed: "4.17.21"}}}},
	}}}
	fake3 := &osvEvalFake{verdict: "not_affected", reason: "B-05 区间结论"}
	got3 := EvaluateOSVVuln(context.Background(), fake3, osvLodashRef("1.0.0"), vulnR)
	if got3.Verdict != db.VerdictNotAffected || fake3.calls != 1 {
		t.Fatalf("清单未命中应继续区间求值: %+v calls=%d", got3, fake3.calls)
	}
	if got3.Reason != "B-05 区间结论" {
		t.Fatalf("区间路径 reason 应为 B-05 原文: %+v", got3.Reason)
	}
}

// 缺版本 / 缺 ID / 无求值器：maybe 零调用。
func TestEvaluateOSVVulnMissingInputs(t *testing.T) {
	fake := &osvEvalFake{verdict: "affected"}
	got := EvaluateOSVVuln(context.Background(), fake, osvLodashRef(""), osvLodashVuln())
	if got.Verdict != db.VerdictMaybe || fake.calls != 0 {
		t.Fatalf("缺版本应 maybe 零调用: %+v", got)
	}
	got2 := EvaluateOSVVuln(context.Background(), fake, osvLodashRef("4.17.20"), OSVVuln{Aliases: []string{"CVE-X"}})
	if got2.Verdict != db.VerdictMaybe || fake.calls != 0 {
		t.Fatalf("缺 ID 应 maybe 零调用: %+v", got2)
	}
	got3 := EvaluateOSVVuln(context.Background(), nil, osvLodashRef("4.17.20"), osvLodashVuln())
	if got3.Verdict != db.VerdictMaybe {
		t.Fatalf("无求值器应 maybe: %+v", got3)
	}
}

// 坏版本：适配层绝不做本地版本合法性判定（那是第二比较器的入口）——原样
// 交 B-05，由其裁决（fake 回 maybe「版本无法解析」形态）。
func TestEvaluateOSVVulnBadVersionDelegated(t *testing.T) {
	fake := &osvEvalFake{verdict: "maybe", reason: "版本无法解析"}
	got := EvaluateOSVVuln(context.Background(), fake, osvLodashRef("不是版本号!!!"), osvLodashVuln())
	if got.Verdict != db.VerdictMaybe {
		t.Fatalf("坏版本应 maybe: %+v", got)
	}
	if fake.calls != 1 || fake.lastVer != "不是版本号!!!" {
		t.Fatalf("坏版本应原样交 B-05: %+v", fake)
	}
}

// 求值失败 / 未知三态返回：maybe——失败绝不解释成 not_affected。
func TestEvaluateOSVVulnFailurePaths(t *testing.T) {
	fake := &osvEvalFake{err: errors.New("cve 服务不可用: connection refused")}
	got := EvaluateOSVVuln(context.Background(), fake, osvLodashRef("4.17.20"), osvLodashVuln())
	if got.Verdict != db.VerdictMaybe || !strings.Contains(got.Reason, "求值失败") {
		t.Fatalf("网络失败应 maybe 带原因: %+v", got)
	}
	fake2 := &osvEvalFake{verdict: "不确定", reason: "?"}
	got2 := EvaluateOSVVuln(context.Background(), fake2, osvLodashRef("4.17.20"), osvLodashVuln())
	if got2.Verdict != db.VerdictMaybe || !strings.Contains(got2.Reason, "未知三态") {
		t.Fatalf("未知三态应 maybe: %+v", got2)
	}
}

// 区间上限：>32 条可求值区间或单条 >16 事件 ⇒ maybe 零调用（绝不截断）。
func TestEvaluateOSVVulnRangeCaps(t *testing.T) {
	many := OSVVuln{ID: "GHSA-j", Affected: []OSVAffected{{
		Package: OSVAffectedPackage{Ecosystem: "npm", Name: "lodash"},
	}}}
	for i := 0; i < osvMaxEvalRanges+1; i++ {
		many.Affected[0].Ranges = append(many.Affected[0].Ranges, OSVRange{
			Type:   "SEMVER",
			Events: []OSVRangeEvent{{Introduced: strconv.Itoa(i)}, {Fixed: strconv.Itoa(i + 1)}},
		})
	}
	fake := &osvEvalFake{verdict: "affected"}
	got := EvaluateOSVVuln(context.Background(), fake, osvLodashRef("4.17.20"), many)
	if got.Verdict != db.VerdictMaybe || fake.calls != 0 {
		t.Fatalf("超区间上限应 maybe 零调用: %+v calls=%d", got, fake.calls)
	}
	// 单条 17 事件。
	fatRange := OSVVuln{ID: "GHSA-k", Affected: []OSVAffected{{
		Package: OSVAffectedPackage{Ecosystem: "npm", Name: "lodash"},
		Ranges:  []OSVRange{{Type: "SEMVER"}},
	}}}
	for i := 0; i < osvMaxRangeEvents+1; i++ {
		fatRange.Affected[0].Ranges[0].Events = append(fatRange.Affected[0].Ranges[0].Events,
			OSVRangeEvent{Introduced: strconv.Itoa(i)})
	}
	fake2 := &osvEvalFake{verdict: "affected"}
	got2 := EvaluateOSVVuln(context.Background(), fake2, osvLodashRef("4.17.20"), fatRange)
	if got2.Verdict != db.VerdictMaybe || fake2.calls != 0 {
		t.Fatalf("超单条事件上限应 maybe: %+v calls=%d", got2, fake2.calls)
	}
}

// 批量对齐：逐依赖逐漏洞产出；空漏洞/空输入合法空；SkippedNonNPM 透传。
func TestEvaluateOSVHitsBatch(t *testing.T) {
	hits := []OSVQueryHits{
		{Ref: osvLodashRef("4.17.20"), Vulns: []OSVVuln{osvLodashVuln()}},
		{Ref: OSVQueryRef{Ecosystem: "npm", Name: "left-pad", Version: "1.3.0"}, Vulns: []OSVVuln{}, SkippedNonNPM: 2},
	}
	fake := &osvEvalFake{verdict: "affected", reason: "r"}
	got := EvaluateOSVHits(context.Background(), fake, hits)
	if len(got) != 2 || len(got[0].Vulns) != 1 || len(got[1].Vulns) != 0 {
		t.Fatalf("批量应对齐: %+v", got)
	}
	if got[0].Vulns[0].Verdict != db.VerdictAffected || got[1].SkippedNonNPM != 2 {
		t.Fatalf("判定与计数透传: %+v", got)
	}
	if got[0].Ref.Name != "lodash" || got[1].Ref.Name != "left-pad" {
		t.Fatalf("Ref 透传: %+v", got)
	}
	empty := EvaluateOSVHits(context.Background(), fake, nil)
	if empty == nil || len(empty) != 0 {
		t.Fatalf("空输入应空非 nil: %+v", empty)
	}
}

// 端到端：真实 cveintel.Client → httptest 假 MCP（B-03D 信封与 ranges 事件
// 形态 wire 验证）。三态值由假服务端按 fixture 预设（与 B-05 输出同值——
// 真比较器一致性由 reartex-cve-mcp 模块自身测试覆盖，单体不 import）。
func TestEvaluateOSVVulnViaCveintelClient(t *testing.T) {
	var lastArgs map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("POST /mcp", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var rpc struct {
			Params struct {
				Arguments map[string]any `json:"arguments"`
			} `json:"params"`
		}
		if err := json.Unmarshal(body, &rpc); err != nil {
			t.Fatalf("假 MCP 收到非法请求: %v", err)
		}
		lastArgs = rpc.Params.Arguments
		ver, _ := lastArgs["version"].(string)
		verdict := "maybe"
		reason := "fixture 缺省 maybe"
		switch ver {
		case "4.17.20":
			verdict, reason = "affected", "fixture: 4.17.20 < fixed 4.17.21"
		case "4.17.21":
			verdict, reason = "not_affected", "fixture: 已达修复版本"
		}
		out, _ := json.Marshal([]map[string]string{{"verdict": verdict, "reason": reason}})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"result":{"content":[{"type":"text","text":` + strconv.Quote(string(out)) + `}],"is_error":false}}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	cli, err := cveintel.New(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	got := EvaluateOSVVuln(context.Background(), cli, osvLodashRef("4.17.20"), osvLodashVuln())
	if got.Verdict != db.VerdictAffected || !strings.Contains(got.Reason, "4.17.20 < fixed 4.17.21") {
		t.Fatalf("端到端 affected: %+v", got)
	}
	// wire 断言：ecosystem/version/ranges 三键；events 单键形态到服务端。
	if lastArgs["ecosystem"] != "npm" || lastArgs["version"] != "4.17.20" {
		t.Fatalf("端到端参数: %+v", lastArgs)
	}
	raw, _ := json.Marshal(lastArgs["ranges"])
	if !strings.Contains(string(raw), `"introduced":"0"`) || !strings.Contains(string(raw), `"fixed":"4.17.21"`) {
		t.Fatalf("ranges 应为事件形态: %s", raw)
	}
	got2 := EvaluateOSVVuln(context.Background(), cli, osvLodashRef("4.17.21"), osvLodashVuln())
	if got2.Verdict != db.VerdictNotAffected {
		t.Fatalf("端到端 not_affected 透传: %+v", got2)
	}
}

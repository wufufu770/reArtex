package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
	actool "github.com/Autumn-27/norma/tool"
)

// 本文件钉住【图读面】的范围隔离：范围外的资产的结论，经由 agent 可达的读接口
// （graph_overview / list_facts / list_findings / node_detail）一律不可达；
// 范围内的结论照常可达；任务自有节点（goal / hint / intent）照常可见；写路径拒绝
// 把范围外的资产锚进图里。
//
// 为什么要有这道测试（Tier 1 门禁只覆盖其中五个工具）：
//   - 门禁（agent/tier1_scope_gate_test.go）钉的是「Tier 1 工具不泄露范围外资产」，
//     它的用例表只列了 Tier 1 清单里的工具。图读面还有别的读口（node_detail 按 id
//     取、list_facts 翻页、related_tasks 继承视图），它们同样会把结论端出来。
//   - 读侧规则有两条不同的口径（资产锚定节点 vs 任务自有节点），最容易出的错就是
//     把任务自有节点也按锚点过滤（规划者的视图被清空），或者把资产锚定节点漏成
//     无锚点节点（范围外的结论原样漏出去）。两边都要钉。
//
// 为什么用【真实工具】而不是直接调 db：db 层的关系已经被 db/exploration_scope_test.go
// 钉住了；这里要钉的是「工具面真的用上了那些关系」——判定做对了、工具没接上，对
// 范围隔离来说同样等于泄露。

// graphScopeFixture 铺出四个任务：
//
//	caller —— 调用者任务，声明 inHost 范围。
//	other  —— 别人的任务，声明 outHost 范围（对 caller 是范围外）。
//	source —— caller 的直接来源任务（继承视图的来源），声明 srcHost 范围。
//	foreign—— 第三方任务，声明 foreignHost 范围（对 caller 与 source 都是范围外）。
//
// 资产：
//
//	inAsset     —— 在 caller 范围内。
//	outAsset    —— 只在 other 范围内（对 caller 范围外）。
//	srcAsset    —— 在 source 范围内。caller 的范围包含其直接来源任务的范围，所以
//	                srcAsset 对 caller 也算范围内（继承来的结论要能被继承）。
//	foreignAsset—— 只在 foreign 范围内：对 caller 与 source 都是范围外 —— 用它钉
//	                「继承来的结论也必须过调用者的范围」。
type graphScopeFixture struct {
	db *db.DB

	caller  int64
	other   int64
	source  int64
	foreign int64

	expCaller int64
	expSource int64

	inHost      string
	outHost     string
	srcHost     string
	foreignHost string

	inAsset      int64
	outAsset     int64
	srcAsset     int64
	foreignAsset int64
}

func newGraphScopeFixture(t *testing.T) *graphScopeFixture {
	t.Helper()
	d := testDB(t)
	t.Cleanup(func() { _ = d.Close() })

	stamp := time.Now().UnixNano()
	fx := &graphScopeFixture{
		db:          d,
		inHost:      fmt.Sprintf("gs-in-%d.example.test", stamp),
		outHost:     fmt.Sprintf("gs-out-%d.example.test", stamp),
		srcHost:     fmt.Sprintf("gs-src-%d.example.test", stamp),
		foreignHost: fmt.Sprintf("gs-foreign-%d.example.test", stamp),
	}

	other, err := d.CreateTask("gs other", "别的任务", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := d.CreateTask("gs foreign", "第三方任务", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	source, err := d.CreateTask("gs source", "来源任务", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	caller, err := d.CreateTaskWithOptions("gs caller", "调用者任务", db.TaskCreateOptions{
		SourceTaskIDs: []int64{source.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = d.DeleteTask(caller.ID)
		_ = d.DeleteTask(source.ID)
		_ = d.DeleteTask(other.ID)
		_ = d.DeleteTask(foreign.ID)
	})

	fx.caller, fx.other, fx.source, fx.foreign = caller.ID, other.ID, source.ID, foreign.ID
	fx.expCaller, fx.expSource = caller.ExplorationID, source.ExplorationID

	// 声明范围：直接写 task_scope 行（范围 CTE 的匹配条件是 a.root_domain = ts.domain）。
	for _, sc := range []struct {
		taskID int64
		domain string
	}{
		{caller.ID, fx.inHost},
		{other.ID, fx.outHost},
		{source.ID, fx.srcHost},
		{foreign.ID, fx.foreignHost},
	} {
		seedTaskScope(t, d, sc.taskID, "root_domain", sc.domain)
	}

	fx.inAsset = insertScopeAsset(t, d, scopeAssetSpec{domain: fx.inHost, rootDomain: fx.inHost, ownerTaskID: caller.ID})
	fx.outAsset = insertScopeAsset(t, d, scopeAssetSpec{domain: fx.outHost, rootDomain: fx.outHost, ownerTaskID: other.ID})
	fx.srcAsset = insertScopeAsset(t, d, scopeAssetSpec{domain: fx.srcHost, rootDomain: fx.srcHost, ownerTaskID: source.ID})
	fx.foreignAsset = insertScopeAsset(t, d, scopeAssetSpec{domain: fx.foreignHost, rootDomain: fx.foreignHost, ownerTaskID: foreign.ID})
	t.Cleanup(func() {
		d.Exec(`DELETE FROM assets WHERE id=ANY($1::bigint[])`, []int64{ //nolint:errcheck
			fx.inAsset, fx.outAsset, fx.srcAsset, fx.foreignAsset,
		})
	})
	return fx
}

// tools 返回一个绑定在 caller 任务上的 ToolSet（真实工具面）。
func (fx *graphScopeFixture) tools(t *testing.T) *ToolSet {
	t.Helper()
	ts := NewToolSet(fx.db.Exploration(fx.expCaller), "planner")
	ts.SetTaskID(fx.caller)
	ts.SetAssetStore(fx.db.Assets(), fx.db.Companies())
	return ts
}

// graphScopeCall 调一个具名工具，返回展开后的文本（成功结果与错误文案都算）。
//
// 返回【原始文本】而不是解析后的结构：断言「错误文案里也不能泄露范围外的内容」
// 正是靠这条 —— 一旦只取成功分支的错误信息，那半条断言就是空的。
func graphScopeCall(t *testing.T, ts *ToolSet, name string, payload any) string {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range ts.AllDomainTools() {
		if tool.Name() != name {
			continue
		}
		res, err := tool.Call(context.Background(), raw, nil)
		if err != nil {
			t.Fatalf("%s 调用失败：%v", name, err)
		}
		return res.Flatten()
	}
	t.Fatalf("工具表里没有 %s", name)
	return ""
}

// graphScopeTool 按名字取工具。
func graphScopeTool(t *testing.T, ts *ToolSet, name string) actool.CoreTool {
	t.Helper()
	for _, tool := range ts.AllDomainTools() {
		if tool.Name() == name {
			return tool
		}
	}
	t.Fatalf("工具表里没有 %s", name)
	return nil
}

// TestGraphOverviewHidesOutOfScopeAssetConclusion 钉读面：一条锚在范围外资产上的
// 结论，经由 graph_overview 不可达。
//
// 这里刻意【绕过 agent 的写路径】、直接用 db 层建这条事实：读侧必须独立地挡住
// 范围外的结论 —— 写侧将来补上范围判定只是多一道保险，不能成为读侧放行的理由
// （历史数据、宿主/UI 的写入路径都还会产生这类节点）。
func TestGraphOverviewHidesOutOfScopeAssetConclusion(t *testing.T) {
	fx := newGraphScopeFixture(t)
	ts := fx.tools(t)

	summary := "范围外资产 " + fx.outHost + " 上的结论"
	leaked, err := fx.db.Exploration(fx.expCaller).AddNode(db.KindFact,
		map[string]any{"summary": summary}, 5, "confirmed", "worker", []int64{fx.outAsset})
	if err != nil {
		t.Fatal(err)
	}

	out := graphScopeCall(t, ts, "graph_overview", map[string]any{})
	if strings.Contains(out, summary) || strings.Contains(out, fx.outHost) {
		t.Fatalf("graph_overview 把范围外资产的结论带了出来：%s", out)
	}
	if strings.Contains(out, fmt.Sprintf("\"id\":%d", leaked)) {
		t.Fatalf("graph_overview 把范围外结论的节点 id 带了出来：%s", out)
	}
}

// TestGraphOverviewKeepsInScopeAssetConclusion 钉读面的另一半：范围内的结论必须照常
// 可见 —— 范围隔离不能变成一刀切，否则任务会被自己的门禁锁死。
func TestGraphOverviewKeepsInScopeAssetConclusion(t *testing.T) {
	fx := newGraphScopeFixture(t)
	ts := fx.tools(t)

	summary := "范围内资产 " + fx.inHost + " 上的结论"
	graphScopeCall(t, ts, "record_fact", map[string]any{
		"summary": summary, "asset_ids": []int64{fx.inAsset},
	})

	out := graphScopeCall(t, ts, "graph_overview", map[string]any{})
	if !strings.Contains(out, summary) {
		t.Fatalf("graph_overview 把范围内的结论也挡掉了：%s", out)
	}
}

// TestListFactsHidesOutOfScopeAssetConclusion 钉翻页读面：list_facts 同样不得带出
// 范围外的结论（它与 graph_overview 是两个不同的读口，判定却必须是同一条）。
func TestListFactsHidesOutOfScopeAssetConclusion(t *testing.T) {
	fx := newGraphScopeFixture(t)
	ts := fx.tools(t)

	summary := "范围外资产 " + fx.outHost + " 上的结论"
	if _, err := fx.db.Exploration(fx.expCaller).AddNode(db.KindFact,
		map[string]any{"summary": summary}, 5, "confirmed", "worker", []int64{fx.outAsset}); err != nil {
		t.Fatal(err)
	}

	out := graphScopeCall(t, ts, "list_facts", map[string]any{})
	if strings.Contains(out, summary) || strings.Contains(out, fx.outHost) {
		t.Fatalf("list_facts 把范围外资产的结论带了出来：%s", out)
	}
}

// TestListFindingsHidesOutOfScopeAssetConclusion 钉漏洞清单读面。
func TestListFindingsHidesOutOfScopeAssetConclusion(t *testing.T) {
	fx := newGraphScopeFixture(t)
	ts := fx.tools(t)

	summary := "范围外漏洞 " + fx.outHost
	if _, err := fx.db.Exploration(fx.expCaller).AddNode(db.KindFinding,
		map[string]any{"summary": summary, "vulnclass": "TEST", "severity": "low"}, 9, "confirmed", "worker", []int64{fx.outAsset}); err != nil {
		t.Fatal(err)
	}

	out := graphScopeCall(t, ts, "list_findings", map[string]any{})
	if strings.Contains(out, summary) || strings.Contains(out, fx.outHost) {
		t.Fatalf("list_findings 把范围外资产的结论带了出来：%s", out)
	}
}

// TestNodeDetailHidesOutOfScopeAssetConclusion 钉【按 id 取】的读口：node_detail 不能
// 因为「按 id 取」就绕过范围 —— 节点 id 是自增整数，能被猜出来，所以按 id 取同样
// 必须判范围。范围外的节点与不存在的节点要【无法区分】（拒绝文案里不得出现该结论
// 的任何细节），否则按 id 探测就变成了存在性探测器。
func TestNodeDetailHidesOutOfScopeAssetConclusion(t *testing.T) {
	fx := newGraphScopeFixture(t)
	ts := fx.tools(t)

	summary := "范围外资产 " + fx.outHost + " 上的结论"
	leaked, err := fx.db.Exploration(fx.expCaller).AddNode(db.KindFact,
		map[string]any{"summary": summary}, 5, "confirmed", "worker", []int64{fx.outAsset})
	if err != nil {
		t.Fatal(err)
	}

	out := graphScopeCall(t, ts, "node_detail", map[string]any{"id": leaked})
	if strings.Contains(out, summary) || strings.Contains(out, fx.outHost) {
		t.Fatalf("node_detail 把范围外资产的结论带了出来：%s", out)
	}
	if !strings.Contains(out, "未找到探索节点") {
		t.Fatalf("范围外的节点应当与不存在的节点一样取不到：%s", out)
	}
}

// TestGraphOverviewKeepsTaskOwnedNodes 钉另一半：任务自有节点（goal / hint / intent）
// 与没有锚点的结论（任务起点 fact、没填 asset_ids 的事实）必须照常可见。
//
// 它们的可见性由【探索归属】决定，不是由锚点决定 —— 把它们也按锚点过滤（db 层那条
// 更严的 ListByKindScoped 口径）会清空规划者的视图，那比泄露更糟。
func TestGraphOverviewKeepsTaskOwnedNodes(t *testing.T) {
	fx := newGraphScopeFixture(t)
	ts := fx.tools(t)

	graphScopeCall(t, ts, "set_goals", map[string]any{"goals": []any{
		map[string]any{"text": "把范围内的站点摸清楚"},
	}})
	graphScopeCall(t, ts, "add_hint", map[string]any{"text": "注意别把范围外的资产写进来"})
	graphScopeCall(t, ts, "add_intent", map[string]any{"summary": "先看范围内的入口"})
	// 没填 asset_ids 的事实：没有锚点，但它是本任务自己的结论。
	graphScopeCall(t, ts, "record_fact", map[string]any{"summary": "没有锚点但属于本任务的结论"})

	out := graphScopeCall(t, ts, "graph_overview", map[string]any{})
	for _, want := range []string{"把范围内的站点摸清楚", "注意别把范围外的资产写进来", "先看范围内的入口", "没有锚点但属于本任务的结论"} {
		if !strings.Contains(out, want) {
			t.Fatalf("graph_overview 少了任务自有节点 %q：%s", want, out)
		}
	}
}

// TestRecordFactRefusesOutOfScopeAnchor 钉写侧：record_fact 不得把范围外的资产锚进
// 本任务的图里 —— 锚点既是血缘也是范围读的授权边，把范围外的资产锚上去等于把
// 别人的资产 id 写进共享图。整条拒绝（一个字节都不写），且拒绝文案不得泄露该资产
// 的任何细节（否则拒绝本身就成了一次存在性探测器）。
func TestRecordFactRefusesOutOfScopeAnchor(t *testing.T) {
	fx := newGraphScopeFixture(t)
	ts := fx.tools(t)

	before, _ := fx.db.Exploration(fx.expCaller).ListByKind(db.KindFact, 1000)
	out := graphScopeCall(t, ts, "record_fact", map[string]any{
		"summary":   "范围外资产 " + fx.outHost + " 上的结论",
		"asset_ids": []int64{fx.outAsset},
	})
	if strings.Contains(out, fx.outHost) {
		t.Fatalf("拒绝文案里泄露了范围外资产：%s", out)
	}
	if !strings.Contains(out, "不在当前任务范围内") {
		t.Fatalf("应当以「不在当前任务范围内」拒绝：%s", out)
	}
	after, _ := fx.db.Exploration(fx.expCaller).ListByKind(db.KindFact, 1000)
	if len(after) != len(before) {
		t.Fatalf("被拒绝的写入不该留下任何事实节点（前 %d 条，后 %d 条）", len(before), len(after))
	}
}

// TestAddIntentRefusesOutOfScopeAnchor 钉写侧的意图路径：意图绑定的资产越界即整条
// 不写 —— 否则这条意图会把范围外的资产 id 带进图里（意图的 payload 里就带着
// asset_ids）。
func TestAddIntentRefusesOutOfScopeAnchor(t *testing.T) {
	fx := newGraphScopeFixture(t)
	ts := fx.tools(t)

	before, _ := fx.db.Exploration(fx.expCaller).ListByKind(db.KindIntent, 1000)
	out := graphScopeCall(t, ts, "add_intent", map[string]any{
		"summary": "去打范围外的资产", "asset_ids": []int64{fx.outAsset},
	})
	if strings.Contains(out, fx.outHost) {
		t.Fatalf("拒绝文案里泄露了范围外资产：%s", out)
	}
	if !strings.Contains(out, "不在当前任务范围内") {
		t.Fatalf("应当以「不在当前任务范围内」拒绝：%s", out)
	}
	after, _ := fx.db.Exploration(fx.expCaller).ListByKind(db.KindIntent, 1000)
	if len(after) != len(before) {
		t.Fatalf("被拒绝的写入不该留下任何意图节点（前 %d 条，后 %d 条）", len(before), len(after))
	}
}

// TestAddHintRefusesOutOfScopeAnchor 钉写侧的提示路径：提示若锚在范围外的资产上，
// 同样会把别人的资产 id 带进本任务的图。
func TestAddHintRefusesOutOfScopeAnchor(t *testing.T) {
	fx := newGraphScopeFixture(t)
	ts := fx.tools(t)

	before, _ := fx.db.Exploration(fx.expCaller).ListByKind(db.KindHint, 1000)
	out := graphScopeCall(t, ts, "add_hint", map[string]any{
		"text": "交接一条范围外的线索", "asset_ids": []int64{fx.outAsset},
	})
	if strings.Contains(out, fx.outHost) {
		t.Fatalf("拒绝文案里泄露了范围外资产：%s", out)
	}
	if !strings.Contains(out, "不在当前任务范围内") {
		t.Fatalf("应当以「不在当前任务范围内」拒绝：%s", out)
	}
	after, _ := fx.db.Exploration(fx.expCaller).ListByKind(db.KindHint, 1000)
	if len(after) != len(before) {
		t.Fatalf("被拒绝的写入不该留下任何提示节点（前 %d 条，后 %d 条）", len(before), len(after))
	}
}

// TestInheritedConclusionsFollowCallerScope 钉继承读面：直接来源任务的结论继承过来
// 时，也要过【调用者】的范围 —— 授权边界是「调用者能看什么」，不是「来源任务能看
// 什么」。否则「继承」就成了绕过范围的后门。
//
// foreignAsset 对调用者与来源任务都是范围外（它只属于第三方任务），所以来源任务那条
// 锚在它上面的结论不能被继承；srcAsset 在来源任务范围内（因而也在调用者范围内，
// 范围 CTE 包含直接来源任务的范围），所以它必须照常被继承。
func TestInheritedConclusionsFollowCallerScope(t *testing.T) {
	fx := newGraphScopeFixture(t)
	ts := fx.tools(t)

	srcStore := fx.db.Exploration(fx.expSource)
	inheritedSummary := "来源任务在范围内资产上的结论"
	blockedSummary := "来源任务在第三方资产上的结论"
	if _, err := srcStore.AddNode(db.KindFact, map[string]any{"summary": inheritedSummary},
		5, "confirmed", "worker", []int64{fx.srcAsset}); err != nil {
		t.Fatal(err)
	}
	if _, err := srcStore.AddNode(db.KindFact, map[string]any{"summary": blockedSummary},
		5, "confirmed", "worker", []int64{fx.foreignAsset}); err != nil {
		t.Fatal(err)
	}

	out := graphScopeCall(t, ts, "list_facts", map[string]any{})
	if !strings.Contains(out, inheritedSummary) {
		t.Fatalf("范围内的继承结论必须照常可见：%s", out)
	}
	if strings.Contains(out, blockedSummary) || strings.Contains(out, fx.foreignHost) {
		t.Fatalf("范围外的继承结论被带了出来：%s", out)
	}
}

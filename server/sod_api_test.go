package server

// 本文件是【新增 HTTP 端点】的验收用例：SoD 链的三个只读观测面（决策账本 /
// 对抗复核队列 / 工具熔断）与意图归属字段（intent owner）。
//
// ⚠️ 路由可达性用**真实路由表**（s.Handler()）判定，理由同
// server/strategy_api_test.go：Go 的 ServeMux 里模式冲突是 panic 而不是编译
// 错误 —— 只有真的把 mux 建起来才暴露得了。而「路由没注册」（纯文本
// "404 page not found"）与「handler 自己返回 404」（JSON 体）必须分得开。
//
// 这些端点全是**只读**：SoD 的三个面都是【观测到的状态】的出口，本文件里的
// 用例因此只断言「读得到、形状对、且读不改」。

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/internal/sod"
)

// =====================================================================
// 测试脚手架
// =====================================================================

// sodAPISuffix 给用例生成互不撞的标识后缀。
func sodAPISuffix() int64 { return time.Now().UnixNano() }

// sodAPIResetSoDSlot 清掉进程级 SoD 链单例与执行器凭证表。
//
// 为什么需要复位：server.New() 会装 SoD 链，而链是**进程级单例**（只允许装
// 一次；装不上时 server.New 走 log.Fatalf ⇒ 整个测试二进制当场退出）。
//
// 为什么自己写而不复用别的测试文件里的同名助手：那些文件由别的单元并行持有，
// 本单元的用例不该挂在别人的私有助手上（同 server/strategy_api_test.go 的
// 同款理由）。
func sodAPIResetSoDSlot(t *testing.T) {
	t.Helper()
	reset := func() {
		sodChainSlot.mu.Lock()
		sodChainSlot.chain = nil
		sodChainSlot.mu.Unlock()
		sod.ResetExecutorTokensForTest()
	}
	reset()
	t.Cleanup(reset)
}

// sodAPIServer 起一个带真实路由表的服务。
//
// 返回：数据库句柄（造前置数据 + 查库断言）、manager（造任务）、HTTP handler、
// JWT 令牌。令牌是必要的：/api/* 全在 requireAuth 后面（server/auth.go），没有
// 令牌的请求只会拿到 401，测的就不是 handler 而是鉴权中间件了。
func sodAPIServer(t *testing.T) (*db.DB, *Manager, http.Handler, string) {
	t.Helper()
	sodAPIResetSoDSlot(t)
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	t.Cleanup(func() { m.Close() })
	if m.pg == nil {
		t.Skip("postgres unavailable — skipping sod API test")
	}
	td := t.TempDir()
	s := New(testServerCtx(t), m, td, td, td)
	tok, err := signJWT(s.jwtKey)
	if err != nil {
		t.Fatalf("sign jwt: %v", err)
	}
	return m.pg, m, s.Handler(), tok
}

// sodAPIDo 发一个带令牌的请求，回传状态码与解析后的 JSON 体。
func sodAPIDo(t *testing.T, h http.Handler, tok, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var r *http.Request
	if body != nil {
		b, _ := json.Marshal(body)
		r = httptest.NewRequest(method, path, bytes.NewReader(b))
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	r.Header.Set("Authorization", "Bearer "+tok)
	r.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	out := map[string]any{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// sodAPIAny 发一个带令牌的请求，把响应体解成任意 JSON 值。
//
// 为什么另有一个「任意 JSON」版本：意图列表接口返回的是**裸数组**（历史形状），
// 解进 map 会失败 —— 那不是接口坏了，是形状本来就不是对象。
func sodAPIAny(t *testing.T, h http.Handler, tok, method, path string) (int, any) {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// sodAPIRaw 发一个带令牌的请求，回传原始响应体（用于判定「路由没注册」）。
func sodAPIRaw(t *testing.T, h http.Handler, tok, method, path string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// sodDecisionRow 在账本里写一条决策（返回 id）。
func sodDecisionRow(t *testing.T, pg *db.DB, toolName, verdict, reasonCode string) int64 {
	t.Helper()
	id, err := pg.RecordSoDDecision(&db.SoDDecision{
		SourceAgent:   "work#1",
		ToolName:      toolName,
		Verdict:       verdict,
		ReasonCode:    reasonCode,
		PayloadDigest: "digest-" + strconv.FormatInt(sodAPISuffix(), 10),
		Checks:        json.RawMessage(`{"scope_check":"pass"}`),
	})
	if err != nil {
		t.Fatalf("写决策账本: %v", err)
	}
	return id
}

// sodFindDecision 在响应里找到 id 对应的那条决策。
func sodFindDecision(t *testing.T, out map[string]any, id int64) map[string]any {
	t.Helper()
	items, _ := out["decisions"].([]any)
	for _, it := range items {
		m, _ := it.(map[string]any)
		if m == nil {
			continue
		}
		if v, ok := m["id"].(float64); ok && int64(v) == id {
			return m
		}
	}
	t.Fatalf("响应里没有 id=%d 的那条决策: %v", id, out)
	return nil
}

// =====================================================================
// P0-1 意图归属（owner）
// =====================================================================

// TestIntentListExposesOwner 钉住「意图归属」这件事能被前端读到。
//
// exploration_nodes.owner 是意图归属的**唯一权威**（ClaimIntent 的 CAS 原子
// 写入），而前端的任务大厅「Intent 看板」有一列「归属」——它一直空着，因为
// 意图列表接口没返回这一列。这个用例把那条列钉死：认领之后，列表里必须能看到
// owner。
func TestIntentListExposesOwner(t *testing.T) {
	pg, m, h, tok := sodAPIServer(t)
	task, err := m.CreateTaskWithOptions("意图归属用例", "goal", db.TaskCreateOptions{})
	if err != nil {
		t.Fatalf("建任务: %v", err)
	}
	t.Cleanup(func() { _, _ = m.DeleteTask(task.ID, DeleteTaskOptions{}) })

	intentID, err := task.Store.AddIntent(map[string]any{"summary": "探测登录接口"}, 5, nil, "planner")
	if err != nil {
		t.Fatalf("加意图: %v", err)
	}
	claimed, err := task.Store.ClaimIntent(intentID, "work#3")
	if err != nil || !claimed {
		t.Fatalf("认领意图失败: %v (claimed=%v)", err, claimed)
	}

	code, raw := sodAPIAny(t, h, tok, "GET", "/api/exploration/intents?task="+task.ID)
	if code != 200 {
		t.Fatalf("读意图列表应当成功，实际 %d %v", code, raw)
	}
	items, _ := raw.([]any)
	found := false
	for _, it := range items {
		row, _ := it.(map[string]any)
		if row == nil {
			continue
		}
		if id, ok := row["id"].(string); !ok || id != strconv.FormatInt(intentID, 10) {
			continue
		}
		found = true
		if row["owner"] != "work#3" {
			t.Fatalf("意图没有带出归属（owner）：%v", row)
		}
		if row["state"] != "running" {
			t.Fatalf("认领后的意图状态应是 running，实际 %v", row["state"])
		}
	}
	if !found {
		t.Fatalf("意图列表里没有那条刚认领的意图 #%d: %v", intentID, raw)
	}

	// 数据库那一列确实是权威：接口读的不能是别的东西。
	var owner string
	if err := pg.QueryRow(`SELECT owner FROM exploration_nodes WHERE id=$1`, intentID).Scan(&owner); err != nil {
		t.Fatalf("读 owner: %v", err)
	}
	if owner != "work#3" {
		t.Fatalf("库里 owner=%q，期望 work#3", owner)
	}
}

// =====================================================================
// P0-2 决策账本
// =====================================================================

// TestSodDecisionLedgerListsDecisions 钉住决策账本能被读到，且每条都带分层信息。
//
// 账本是「每次决策都留痕」的完整留痕（approve 也在内）—— 它读不出来就等于
// 「没留痕」，所以读失败必须报错，不能报成「没有决策」。
func TestSodDecisionLedgerListsDecisions(t *testing.T) {
	pg, _, h, tok := sodAPIServer(t)
	id := sodDecisionRow(t, pg, "Bash", "deny", "out_of_scope")

	code, out := sodAPIDo(t, h, tok, "GET", "/api/sod/decisions", nil)
	if code != 200 {
		t.Fatalf("读决策账本应当成功，实际 %d %v", code, out)
	}
	row := sodFindDecision(t, out, id)
	if row["verdict"] != "deny" {
		t.Fatalf("账本里的裁决不是 deny: %v", row)
	}
	if row["reason_code"] != "out_of_scope" {
		t.Fatalf("账本里的原因码不对: %v", row)
	}
	if row["tool_name"] != "Bash" {
		t.Fatalf("账本里的工具名不对: %v", row)
	}
	// 分层信息：Bash 是出网工具 ⇒ 判定器给 Tier3/egress。
	if row["tier"] != "Tier3" {
		t.Fatalf("决策没有带出分层（tier）: %v", row)
	}
	if row["tier_category"] != "egress" {
		t.Fatalf("决策的层类别不对: %v", row)
	}
	if _, ok := row["tier_host_only"]; !ok {
		t.Fatalf("决策没有带出 tier_host_only: %v", row)
	}
	if _, ok := row["tier_scope_in"]; !ok {
		t.Fatalf("决策没有带出 tier_scope_in: %v", row)
	}
}

// TestSodDecisionLedgerCarriesReadOnlyTier 钉住 Tier1 只读工具的分层。
//
// 只读工具的分层完全由工具 key 决定（agent.Tier1ReadOnlyTools），所以账本里
// 读出来的层级必须是 Tier1 —— 不能因为「读不到原始载荷」就一律降级成 Tier3
// 把所有决策都糊成高风险。
func TestSodDecisionLedgerCarriesReadOnlyTier(t *testing.T) {
	pg, _, h, tok := sodAPIServer(t)
	id := sodDecisionRow(t, pg, "list_assets", "approve", "all_checks_passed")

	code, out := sodAPIDo(t, h, tok, "GET", "/api/sod/decisions", nil)
	if code != 200 {
		t.Fatalf("读决策账本应当成功，实际 %d %v", code, out)
	}
	row := sodFindDecision(t, out, id)
	if row["tier"] != "Tier1" {
		t.Fatalf("只读工具的层级应是 Tier1，实际 %v", row)
	}
	if row["tier_category"] != "safe_read" {
		t.Fatalf("只读工具的层类别应是 safe_read，实际 %v", row)
	}
}

// TestSodDecisionLedgerFailsClosedWhenUnreadable 钉住「账本读不出来」不许报成
// 「没有决策」。
//
// 账本是审计记录：读不出来时返回 200 + 空列表，等于告诉操作者「没人做过裁决」
// —— 那是最危险的假象。这里把连接池关掉来模拟「读不出来」：必须报错，不是空
// 列表。
func TestSodDecisionLedgerFailsClosedWhenUnreadable(t *testing.T) {
	pg, _, h, tok := sodAPIServer(t)
	pg.Close() // 账本从此读不出来

	code, out := sodAPIDo(t, h, tok, "GET", "/api/sod/decisions", nil)
	if code == 200 {
		t.Fatalf("账本读不出来时不得报 200（那等于「没有决策」），实际 %d %v", code, out)
	}
	if _, ok := out["error"]; !ok {
		t.Fatalf("读不出来时必须带错误说明，不能是空列表: %d %v", code, out)
	}
}

// =====================================================================
// P0-3 对抗复核队列
// =====================================================================

// TestSodReviewQueueIsAdvisoryNotApproval 钉住对抗复核队列是**证伪清单**，
// 不是人工审批队列。
//
// 这张表的唯一去处是 needs_review ——「交给 redteam 证伪」。人工审批队列是另
// 一张表（intercept_pending）。所以接口必须把「这是 advisory、不是审批」写进
// 响应里，否则操作者会把它当成「等人点头的清单」。
func TestSodReviewQueueIsAdvisoryNotApproval(t *testing.T) {
	pg, _, h, tok := sodAPIServer(t)
	id, err := pg.EnqueueAdversarialReview(&db.AdversarialReviewItem{
		SourceAgent:    "work#2",
		ToolName:       "Bash",
		PayloadDigest:  "digest-" + strconv.FormatInt(sodAPISuffix(), 10),
		AnomalyFlag:    true,
		AnomalyReason:  "命令含管道",
		StructuredIntent: json.RawMessage(`{"action_type":"command_execution"}`),
	})
	if err != nil {
		t.Fatalf("入队对抗复核项: %v", err)
	}

	code, out := sodAPIDo(t, h, tok, "GET", "/api/sod/review-queue", nil)
	if code != 200 {
		t.Fatalf("读对抗复核队列应当成功，实际 %d %v", code, out)
	}
	if out["advisory_only"] != true {
		t.Fatalf("对抗复核队列必须标成 advisory_only: %v", out)
	}
	if out["human_approval_queue"] != false {
		t.Fatalf("对抗复核队列必须显式声明「不是人工审批队列」: %v", out)
	}
	items, _ := out["queue"].([]any)
	found := false
	for _, it := range items {
		row, _ := it.(map[string]any)
		if row == nil {
			continue
		}
		if v, ok := row["id"].(float64); ok && int64(v) == id {
			found = true
			if row["status"] != "pending" {
				t.Fatalf("待复核项的状态应是 pending，实际 %v", row["status"])
			}
			if row["source_agent"] != "work#2" {
				t.Fatalf("待复核项的来源 agent 不对: %v", row)
			}
		}
	}
	if !found {
		t.Fatalf("队列里没有刚入队的那一项 #%d: %v", id, out)
	}
}

// TestSodReviewQueueIsReadOnly 钉住读队列不改队列。
//
// 队列是「待 redteam 取走证伪」的清单：读它不该把任何一项结案（结案是 redteam
// 的动作，且走另一条写路径）。
func TestSodReviewQueueIsReadOnly(t *testing.T) {
	pg, _, h, tok := sodAPIServer(t)
	id, err := pg.EnqueueAdversarialReview(&db.AdversarialReviewItem{
		SourceAgent:   "work#4",
		ToolName:      "Bash",
		PayloadDigest: "digest-" + strconv.FormatInt(sodAPISuffix(), 10),
	})
	if err != nil {
		t.Fatalf("入队对抗复核项: %v", err)
	}
	if _, out := sodAPIDo(t, h, tok, "GET", "/api/sod/review-queue", nil); out == nil {
		t.Fatalf("读队列没有拿到 JSON 体")
	}
	var status, claimedBy string
	if err := pg.QueryRow(`SELECT status, claimed_by FROM sod_adversarial_review_pending WHERE id=$1`, id).
		Scan(&status, &claimedBy); err != nil {
		t.Fatalf("读队列项: %v", err)
	}
	if status != db.SoDReviewPending || claimedBy != "" {
		t.Fatalf("读队列改了队列项（status=%q claimed_by=%q）", status, claimedBy)
	}
}

// =====================================================================
// P0-4 工具熔断
// =====================================================================

// TestSodToolCircuitShowsTrippedBreaker 钉住熔断状态看得见。
//
// 熔断的作用是「把坏掉的工具关掉」；关掉了却没人看得见，那它就只是一次静默的
// 失败。所以工具的熔断状态必须能被读出来。
func TestSodToolCircuitShowsTrippedBreaker(t *testing.T) {
	pg, _, h, tok := sodAPIServer(t)
	toolKey := "circuit_probe_" + strconv.FormatInt(sodAPISuffix()%100000, 10)
	// 连续失败到阈值 ⇒ 开路。
	for i := 0; i < 6; i++ {
		if err := pg.RecordToolCallOutcome(toolKey, true); err != nil {
			t.Fatalf("记工具失败: %v", err)
		}
	}

	code, out := sodAPIDo(t, h, tok, "GET", "/api/sod/circuit", nil)
	if code != 200 {
		t.Fatalf("读熔断状态应当成功，实际 %d %v", code, out)
	}
	circuits, _ := out["circuits"].([]any)
	found := false
	for _, it := range circuits {
		row, _ := it.(map[string]any)
		if row == nil {
			continue
		}
		if row["tool_key"] != toolKey {
			continue
		}
		found = true
		if row["state"] != "open" {
			t.Fatalf("连续失败的工具应当是 open，实际 %v", row["state"])
		}
	}
	if !found {
		t.Fatalf("熔断列表里没有 %s: %v", toolKey, out)
	}

	// 单个工具也要能查。
	code, out = sodAPIDo(t, h, tok, "GET", "/api/sod/circuit/"+toolKey, nil)
	if code != 200 {
		t.Fatalf("读单个工具的熔断状态应当成功，实际 %d %v", code, out)
	}
	circuit, _ := out["circuit"].(map[string]any)
	if circuit == nil {
		t.Fatalf("响应里没有 circuit 字段: %v", out)
	}
	if circuit["state"] != "open" {
		t.Fatalf("单个工具的熔断状态应是 open，实际 %v", circuit["state"])
	}
}

// TestSodToolCircuitUnknownToolIsClosed 钉住「没出过事的工具」是 closed，
// 不是「查不到」。
func TestSodToolCircuitUnknownToolIsClosed(t *testing.T) {
	_, _, h, tok := sodAPIServer(t)
	toolKey := "never_used_" + strconv.FormatInt(sodAPISuffix()%100000, 10)
	code, out := sodAPIDo(t, h, tok, "GET", "/api/sod/circuit/"+toolKey, nil)
	if code != 200 {
		t.Fatalf("读未知工具的熔断状态应当成功，实际 %d %v", code, out)
	}
	circuit, _ := out["circuit"].(map[string]any)
	if circuit == nil {
		t.Fatalf("响应里没有 circuit 字段: %v", out)
	}
	if circuit["state"] != "closed" {
		t.Fatalf("没出过事的工具应当是 closed，实际 %v", circuit["state"])
	}
}

// =====================================================================
// 路由可达性
// =====================================================================

// TestSodReadRoutesRegistered 钉住这批只读端点真的接上了。
//
// 判定依据是**响应体**而不是状态码：handler 自己也可能返回 404（比如「群组
// 不存在」），那与「路由没注册」是两回事。ServeMux 未匹配时的默认响应体是纯
// 文本 "404 page not found"，而 handler 的 404 是 JSON —— 用这个差异才能把两
// 件事分开。
func TestSodReadRoutesRegistered(t *testing.T) {
	_, _, h, tok := sodAPIServer(t)
	cases := []struct{ method, path string }{
		{"GET", "/api/sod/decisions"},
		{"GET", "/api/sod/review-queue"},
		{"GET", "/api/sod/circuit"},
		{"GET", "/api/sod/circuit/Bash"},
	}
	for _, c := range cases {
		code, body := sodAPIRaw(t, h, tok, c.method, c.path)
		if strings.Contains(body, "404 page not found") {
			t.Errorf("路由未注册: %s %s → %d %s", c.method, c.path, code, body)
		}
	}
}

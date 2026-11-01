package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/internal/sod"
)

// 策略库（L4）operator API 的验收用例（计划 Task 6 Step 5）。
//
// 这些用例钉住的是**承诺**，不是实现细节：
//   - 人工裁决必须留痕：理由 + 操作人进 audit_events 哈希链；
//   - 人工解除 pin 缺理由或缺操作人一律拒绝（没有留痕的解 pin 等于没发生过）；
//   - 人工退役是状态迁移不是删除：留 retired_reason，行与证据都还在；
//   - pin 复审队列**只读**：调用它不会动任何策略的 pinned。
//
// ⚠️ 用的是**真实路由表**（s.Handler()）：Go 的 ServeMux 里模式冲突是 panic
// 而不是编译错误 —— 只有真的把 mux 建起来才暴露得了（计划「风险与注意」里的
// 那一条）。所以这里不另起一个只挂策略路由的壳 mux。

// strategyAPISuffix 给用例生成互不撞的标识后缀。
//
// 为什么自己写一个而不复用别的测试文件里的同名助手：那些文件由别的单元并行
// 持有，本单元的用例不该挂在别人的私有助手上（同 curator 测试文件的同款理由）。
func strategyAPISuffix() int64 { return time.Now().UnixNano() }

// strategyAPIResetSoDSlot 清掉进程级 SoD 链单例与执行器凭证表。
//
// 为什么需要复位：server.New() 会装 SoD 链，而链是**进程级单例**（只允许装
// 一次；装不上时 server.New 走 log.Fatalf ⇒ 整个测试二进制当场退出）。同包
// 其它用例建过 server 时，本文件再建一个就会撞上 ⇒ 建之前先复位。
//
// 为什么自己写而不复用 sod_chain_test.go 的 resetSoDChainSlot：那个文件由别的
// 单元并行持有，本单元的用例不该挂在别人的私有助手上（同 curator 测试文件的
// 同款理由）。
func strategyAPIResetSoDSlot(t *testing.T) {
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

// strategyAPIServer 起一个带真实路由表的服务。
//
// 返回：数据库句柄（造前置数据 + 查库断言）、HTTP handler、JWT 令牌。
// 令牌是必要的：/api/* 全在 requireAuth 后面（server/auth.go），没有令牌的
// 请求只会拿到 401，测的就不是 handler 而是鉴权中间件了。
func strategyAPIServer(t *testing.T) (*db.DB, http.Handler, string) {
	t.Helper()
	strategyAPIResetSoDSlot(t)
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	t.Cleanup(func() { m.Close() })
	if m.pg == nil {
		t.Skip("postgres unavailable — skipping strategy API test")
	}
	td := t.TempDir()
	s := New(testServerCtx(t), m, td, td, td)
	tok, err := signJWT(s.jwtKey)
	if err != nil {
		t.Fatalf("sign jwt: %v", err)
	}
	return m.pg, s.Handler(), tok
}

// strategyAPIDo 发一个带令牌的请求，回传状态码与解析后的 JSON 体。
func strategyAPIDo(t *testing.T, h http.Handler, tok, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var r *http.Request
	if body != nil {
		b, _ := json.Marshal(body)
		r = httptest.NewRequest(method, path, bytes.NewReader(b))
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	r.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	out := map[string]any{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// strategyAPIStrategy 建一条可控的策略（隔离态）。
//
// 正文只写参数化模板（$TARGET / $PATH）—— 库里那条「正文不得含真实目标地址」
// 的硬约束对测试数据同样生效。
func strategyAPIStrategy(t *testing.T, pg *db.DB, sfx int64) int64 {
	t.Helper()
	id, err := pg.CreateStrategy(&db.Strategy{
		Name:    fmt.Sprintf("operator 策略-%d", sfx),
		Summary: "对 $TARGET 的参数校验路径做一次枚举探测",
		Steps:   json.RawMessage(`["curl -X POST https://$TARGET/$PATH"]`),
	})
	if err != nil {
		t.Fatalf("建策略: %v", err)
	}
	t.Cleanup(func() { _, _ = pg.Exec(`DELETE FROM strategies WHERE id=$1`, id) })
	return id
}

// strategyAPIProposal 建一条待裁决的提议。
func strategyAPIProposal(t *testing.T, pg *db.DB) int64 {
	t.Helper()
	id, err := pg.CreateStrategyProposal(&db.StrategyProposal{
		Route:     db.StrategyRouteCreateNew,
		Payload:   json.RawMessage(`{"summary":"一条待裁决的提议"}`),
		Reason:    "该路径在多次任务里重复出现",
		CreatedBy: "planner",
	})
	if err != nil {
		t.Fatalf("建提议: %v", err)
	}
	return id
}

// strategyAPIPinned 读一条策略当前的 pin 状态。
func strategyAPIPinned(t *testing.T, pg *db.DB, id int64) bool {
	t.Helper()
	var pinned bool
	if err := pg.QueryRow(`SELECT pinned FROM strategies WHERE id=$1`, id).Scan(&pinned); err != nil {
		t.Fatalf("读 pin 状态: %v", err)
	}
	return pinned
}

// TestStrategyProposalDecideRecordsAudit 钉住人工裁决必须留痕。
//
// 裁决是**裁决写**（agent 侧写不出）：批 / 驳都要写明裁决人与理由，且理由 +
// 操作人进 audit_events 哈希链 —— 事后能查「谁批的、为什么批」。
func TestStrategyProposalDecideRecordsAudit(t *testing.T) {
	pg, h, tok := strategyAPIServer(t)
	pid := strategyAPIProposal(t, pg)
	path := "/api/strategy-proposals/" + strconv.FormatInt(pid, 10) + "/decide"

	// 缺理由 → 400（没理由的裁决没法复盘）
	code, out := strategyAPIDo(t, h, tok, "POST", path,
		map[string]any{"status": "approved", "actor": "sir"})
	if code != 400 {
		t.Fatalf("缺理由的裁决应当被拒（400），实际 %d %v", code, out)
	}
	// 缺操作人 → 400
	code, out = strategyAPIDo(t, h, tok, "POST", path,
		map[string]any{"status": "approved", "reason": "证据链完整，同意入库"})
	if code != 400 {
		t.Fatalf("缺操作人的裁决应当被拒（400），实际 %d %v", code, out)
	}
	// 非法裁决结果 → 400
	code, out = strategyAPIDo(t, h, tok, "POST", path,
		map[string]any{"status": "maybe", "reason": "证据链完整", "actor": "sir"})
	if code != 400 {
		t.Fatalf("非法裁决结果应当被拒（400），实际 %d %v", code, out)
	}
	// 上面三次都不该改到提议
	var status string
	if err := pg.QueryRow(`SELECT status FROM strategy_proposals WHERE id=$1`, pid).Scan(&status); err != nil {
		t.Fatalf("读提议: %v", err)
	}
	if status != db.StrategyProposalPending {
		t.Fatalf("被拒的裁决不该改到提议（当前 %q）", status)
	}

	// 正常裁决 → 200
	code, out = strategyAPIDo(t, h, tok, "POST", path,
		map[string]any{"status": "approved", "reason": "证据链完整，同意入库", "actor": "sir"})
	if code != 200 {
		t.Fatalf("裁决应当成功，实际 %d %v", code, out)
	}

	var decidedBy, decidedReason string
	if err := pg.QueryRow(`SELECT status, decided_by, decided_reason FROM strategy_proposals WHERE id=$1`, pid).
		Scan(&status, &decidedBy, &decidedReason); err != nil {
		t.Fatalf("读提议: %v", err)
	}
	if status != db.StrategyProposalApproved {
		t.Fatalf("提议状态 = %q，期望 %q", status, db.StrategyProposalApproved)
	}
	if strings.TrimSpace(decidedBy) == "" {
		t.Fatalf("提议没有记下裁决人")
	}
	if strings.TrimSpace(decidedReason) == "" {
		t.Fatalf("提议没有记下裁决理由")
	}

	// 审计链：必须有对应的一条（操作人 + 理由），且链没断。
	var actor, reason, action string
	if err := pg.QueryRow(`SELECT actor, reason, action FROM audit_events
		WHERE payload LIKE $1 ORDER BY seq DESC LIMIT 1`,
		fmt.Sprintf(`%%"proposal_id":%d%%`, pid)).
		Scan(&actor, &reason, &action); err != nil {
		t.Fatalf("审计链上没有这次裁决的记录: %v", err)
	}
	if actor != "sir" {
		t.Fatalf("审计事件缺操作人：%q", actor)
	}
	if reason != "证据链完整，同意入库" {
		t.Fatalf("审计事件缺理由：%q", reason)
	}
	if seq, err := pg.VerifyAuditChain(); err != nil || seq != 0 {
		t.Fatalf("审计链断了（seq=%d, err=%v）", seq, err)
	}
}

// TestStrategyManualUnpinRequiresReasonAndActor 钉住人工解除 pin 必须留痕。
//
// 「解 pin」是能断掉漏洞报告可复现性的动作 ⇒ 没有理由与操作人的解除一律拒绝。
func TestStrategyManualUnpinRequiresReasonAndActor(t *testing.T) {
	pg, h, tok := strategyAPIServer(t)
	sfx := strategyAPISuffix() % 100000
	id := strategyAPIStrategy(t, pg, sfx)
	if err := pg.SetStrategyPinned(id, true, "test-seed", "准备被人工解除的策略"); err != nil {
		t.Fatalf("置 pin: %v", err)
	}
	path := "/api/strategies/" + strconv.FormatInt(id, 10) + "/pin"

	// 缺理由 → 400
	code, out := strategyAPIDo(t, h, tok, "POST", path,
		map[string]any{"pinned": false, "actor": "sir"})
	if code != 400 {
		t.Fatalf("缺理由的解除应当被拒（400），实际 %d %v", code, out)
	}
	// 缺操作人 → 400
	code, out = strategyAPIDo(t, h, tok, "POST", path,
		map[string]any{"pinned": false, "reason": "已确认存在替代路径"})
	if code != 400 {
		t.Fatalf("缺操作人的解除应当被拒（400），实际 %d %v", code, out)
	}
	if !strategyAPIPinned(t, pg, id) {
		t.Fatalf("被拒的人工解除不该动 pin")
	}

	// 理由 + 操作人都有 → 200，且 pin 真的解了
	code, out = strategyAPIDo(t, h, tok, "POST", path,
		map[string]any{"pinned": false, "reason": "已确认存在替代路径", "actor": "sir"})
	if code != 200 {
		t.Fatalf("人工解除应当成功，实际 %d %v", code, out)
	}
	if strategyAPIPinned(t, pg, id) {
		t.Fatalf("人工解除后 pin 仍在")
	}

	var actor, reason, action string
	if err := pg.QueryRow(`SELECT actor, reason, action FROM audit_events
		WHERE action='strategy_pin_released' ORDER BY seq DESC LIMIT 1`).
		Scan(&actor, &reason, &action); err != nil {
		t.Fatalf("审计链上没有人工解除的记录: %v", err)
	}
	if actor != "sir" {
		t.Fatalf("审计事件缺操作人：%q", actor)
	}
	if reason != "已确认存在替代路径" {
		t.Fatalf("审计事件缺理由：%q", reason)
	}
	if seq, err := pg.VerifyAuditChain(); err != nil || seq != 0 {
		t.Fatalf("审计链断了（seq=%d, err=%v）", seq, err)
	}
}

// TestStrategyManualRetireSetsReason 钉住人工退役是状态迁移而不是删除。
//
// 退役 ≠ 删除（反模式 5 的正面对策）：state=retired + retired_at + retired_reason，
// 行还在、证据链还在 —— 墓园与复活的前提就在这里。
func TestStrategyManualRetireSetsReason(t *testing.T) {
	pg, h, tok := strategyAPIServer(t)
	sfx := strategyAPISuffix() % 100000
	id := strategyAPIStrategy(t, pg, sfx)
	// 先晋级到在役态（迁移原语）—— 人工退役对隔离态与在役态都成立。
	if ok, err := pg.PromoteStrategyToActive(id); err != nil || !ok {
		t.Fatalf("晋级到 active 失败: %v (ok=%v)", err, ok)
	}
	path := "/api/strategies/" + strconv.FormatInt(id, 10) + "/retire"

	// 缺理由 → 400
	code, out := strategyAPIDo(t, h, tok, "POST", path, map[string]any{"actor": "sir"})
	if code != 400 {
		t.Fatalf("缺理由的退役应当被拒（400），实际 %d %v", code, out)
	}
	// 缺操作人 → 400
	code, out = strategyAPIDo(t, h, tok, "POST", path, map[string]any{"reason": "路径已失效"})
	if code != 400 {
		t.Fatalf("缺操作人的退役应当被拒（400），实际 %d %v", code, out)
	}

	code, out = strategyAPIDo(t, h, tok, "POST", path,
		map[string]any{"reason": "该路径已被新的替代方案覆盖", "actor": "sir"})
	if code != 200 {
		t.Fatalf("人工退役应当成功，实际 %d %v", code, out)
	}

	var state, retiredReason string
	var retiredAt *time.Time
	if err := pg.QueryRow(`SELECT state, retired_at, retired_reason FROM strategies WHERE id=$1`, id).
		Scan(&state, &retiredAt, &retiredReason); err != nil {
		t.Fatalf("读策略: %v", err)
	}
	if state != db.StrategyRetired {
		t.Fatalf("策略状态 = %q，期望 %q", state, db.StrategyRetired)
	}
	if retiredAt == nil {
		t.Fatalf("退役没有落 retired_at")
	}
	if strings.TrimSpace(retiredReason) == "" {
		t.Fatalf("退役没有留 retired_reason")
	}

	// 退役 ≠ 删除：行还在，API 仍读得到（墓园与复盘的前提）。
	code, out = strategyAPIDo(t, h, tok, "GET", "/api/strategies/"+strconv.FormatInt(id, 10), nil)
	if code != 200 {
		t.Fatalf("退役后仍应能读到策略，实际 %d %v", code, out)
	}
	detail, _ := out["strategy"].(map[string]any)
	if detail == nil {
		t.Fatalf("详情里没有 strategy 字段：%v", out)
	}
	if detail["state"] != db.StrategyRetired {
		t.Fatalf("详情里的状态 = %v，期望 %q", detail["state"], db.StrategyRetired)
	}
	if detail["retired_reason"] != "该路径已被新的替代方案覆盖" {
		t.Fatalf("详情里没有退役理由：%v", detail["retired_reason"])
	}
}

// TestStrategyPinQueueIsReadOnly 钉住 pin 复审队列**只提示、不自动解**。
//
// 自动解 pin 会砸掉漏洞报告的可复现性 —— 通往某个漏洞的唯一已知路径被退役，
// 报告就再也复现不出来 ⇒ 这个队列永远是 advisory。
func TestStrategyPinQueueIsReadOnly(t *testing.T) {
	pg, h, tok := strategyAPIServer(t)
	sfx := strategyAPISuffix() % 100000
	first := strategyAPIStrategy(t, pg, sfx)
	second := strategyAPIStrategy(t, pg, sfx+1)
	for _, id := range []int64{first, second} {
		if err := pg.SetStrategyPinned(id, true, "test-seed", "唯一证人路径"); err != nil {
			t.Fatalf("置 pin: %v", err)
		}
	}
	var before int64
	if err := pg.QueryRow(`SELECT count(*) FROM strategies WHERE pinned=true`).Scan(&before); err != nil {
		t.Fatalf("数被 pin 的策略: %v", err)
	}

	code, out := strategyAPIDo(t, h, tok, "GET", "/api/strategy-pin-queue", nil)
	if code != 200 {
		t.Fatalf("读复审队列应当成功，实际 %d %v", code, out)
	}
	queue, _ := out["queue"].([]any)
	if len(queue) == 0 {
		t.Fatalf("复审队列是空的，但库里明明有被 pin 的策略：%v", out)
	}
	seen := map[int64]bool{}
	for _, item := range queue {
		m, _ := item.(map[string]any)
		if m == nil {
			continue
		}
		if id, ok := m["id"].(float64); ok {
			seen[int64(id)] = true
		}
	}
	if !seen[first] || !seen[second] {
		t.Fatalf("复审队列里没有那两条被 pin 的策略：%v", out)
	}

	// 关键断言：调用队列**没有**动任何策略的 pinned。
	for _, id := range []int64{first, second} {
		if !strategyAPIPinned(t, pg, id) {
			t.Fatalf("复审队列把策略 %d 的 pin 解了 —— 队列只能提示，不能自动解", id)
		}
	}
	var after int64
	if err := pg.QueryRow(`SELECT count(*) FROM strategies WHERE pinned=true`).Scan(&after); err != nil {
		t.Fatalf("数被 pin 的策略: %v", err)
	}
	if after != before {
		t.Fatalf("复审队列改变了被 pin 的策略数量（%d → %d）", before, after)
	}
}

// TestStrategyOperatorAPINeverConsultsAModel 钉住 operator API 里没有模型的位置。
//
// 人工裁决（批准 / 驳回、置位 / 解 pin、退役）是裁决写：判据全是状态比与必填
// 字段。让模型参与「这条该不该批 / 该不该解 pin」就是粗糙 judge（反模式 4）——
// 同一批证据在不同轮次会得到不同结论 ⇒ 裁决不可复现 ⇒ 事后无法复盘、无法追责。
//
// ⚠️ 与 curator 的同款守卫（server/strategy_curator_test.go 的
// TestCuratorNeverConsultsAModel）只差一条：那份禁 net/http（curator 不是 HTTP
// 文件），本文件**就是** HTTP 文件，所以只禁「能拿到模型的包」与模型相关标识符。
func TestStrategyOperatorAPINeverConsultsAModel(t *testing.T) {
	forbiddenImports := []string{
		"github.com/Autumn-27/artex/agent",
		"github.com/Autumn-27/artex/llmrec",
		"github.com/Autumn-27/artex/llmpool",
		"github.com/Autumn-27/norma/llm",
		"github.com/Autumn-27/norma/tool",
	}
	forbiddenIdents := []string{"llm", "model", "prompt", "judge", "completion"}

	path := filepath.Join(".", "strategy_api.go")
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("解析 %s: %v", path, err)
	}
	for _, spec := range file.Imports {
		p, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			t.Fatalf("解析 import 路径: %v", err)
		}
		for _, bad := range forbiddenImports {
			if p == bad || strings.Contains(p, bad) {
				t.Fatalf("operator API 引入了能拿到模型的包 %q", p)
			}
		}
	}
	ast.Inspect(file, func(n ast.Node) bool {
		id, ok := n.(*ast.Ident)
		if !ok {
			return true
		}
		low := strings.ToLower(id.Name)
		for _, bad := range forbiddenIdents {
			if strings.Contains(low, bad) {
				t.Fatalf("operator API 里出现了与模型相关的标识符 %q", id.Name)
			}
		}
		return true
	})
}

// TestStrategyOperatorRoutesRegistered 钉住这批 operator 路由真的接上了。
//
// 判定依据是**响应体**而不是状态码：handler 自己也可能返回 404（比如「策略
// 不存在」），那与「路由没注册」是两回事。ServeMux 未匹配时的默认响应体是纯
// 文本 "404 page not found"，而 handler 的 404 是 JSON —— 用这个差异才能把两
// 件事分开。
func TestStrategyOperatorRoutesRegistered(t *testing.T) {
	_, h, tok := strategyAPIServer(t)
	cases := []struct{ method, path string }{
		{"GET", "/api/strategies"},
		{"GET", "/api/strategies/1"},
		{"GET", "/api/strategy-proposals"},
		{"POST", "/api/strategy-proposals/1/decide"},
		{"POST", "/api/strategies/1/pin"},
		{"POST", "/api/strategies/1/retire"},
		{"GET", "/api/strategy-pin-queue"},
		{"POST", "/api/strategies/refresh-utility"},
	}
	for _, c := range cases {
		req := httptest.NewRequest(c.method, c.path, strings.NewReader("{}"))
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if strings.Contains(rec.Body.String(), "404 page not found") {
			t.Errorf("路由未注册: %s %s → %d %s", c.method, c.path, rec.Code, rec.Body.String())
		}
	}
}

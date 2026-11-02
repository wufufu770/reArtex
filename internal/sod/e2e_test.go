package sod

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/internal/sodpolicy"
)

// 这个文件是 SoD 链的端到端用例：**真库**（决策账本 / 对抗复核队列 / 任务范围
// 行）+ **替身闸门**（五道检查可被单独驱动）+ **假规整器**（模型侧不联网），
// 跑的是链与执行器在真实装配下的完整回路。
//
// 为什么必须真库：审计留痕、对抗复核入队、范围摘要全都落在库里 —— 替身库
// 证明不了「裁决真的留了痕」「needs_review 真的进了对抗队列而不是人工队列」。
//
// 为什么闸门用替身：五道检查里三道依赖实时状态（熔断 / 额度 / tier2 证据），
// 真库里它们要么查不到、要么不可控；替身能让每一道检查被单独驱动，同时
// **裁决与留痕**仍然是链自己做的（真库）。

// sodE2EOptions 是端到端用例的可调项。
type sodE2EOptions struct {
	// scopeInScope 控制闸门第一道检查（范围）是否命中。
	scopeInScope bool
	// anomaly 让规整器报异常 ⇒ 闸门第五道检查给出 needs_review。
	anomaly bool
	// innerBlock 让宿主给的 hook runner 拦截（链应当先问 inner）。
	innerBlock bool
	// innerUpdated 让宿主给的 hook runner 返回**改写后**的入参（链必须按
	// 改写后的那份字节算摘要 —— 那才是执行器真正会跑的那份）。
	innerUpdated []byte
}

// sodE2E 是搭好的一条受管路径。
type sodE2E struct {
	db       *db.DB
	taskID   int64
	host     string
	payload  []byte
	chain    *Chain
	tokens   *TokenStore
	tool     *countingTool
	ctx      context.Context
	notified int
}

// newSoDE2E 搭一条完整的受管路径。
//
// 链是**派生视图**（base.Wrap(inner)）而不是直接 NewChain —— 与宿主真实装配
// 一致（进程级单例链 + 每任务一个 inner），顺带证明视图共享同一张凭证表。
func newSoDE2E(t *testing.T, opts sodE2EOptions) *sodE2E {
	t.Helper()
	d := auditorTestDB(t)

	task, err := d.CreateTask("sod e2e 用例", "goal", nil, 0, 0)
	if err != nil {
		t.Fatalf("建任务失败: %v", err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(task.ID) })

	host := fmt.Sprintf("sod-e2e-%d.example.test", time.Now().UnixNano())
	if _, err := d.Assets().AddManualScope(task.ID, "root_domain", host, "sod e2e 用例"); err != nil {
		t.Fatalf("声明任务范围失败: %v", err)
	}

	env := &sodE2E{
		db:      d,
		taskID:  task.ID,
		host:    host,
		payload: sodE2EPayload(host),
	}
	chain, tokens, tool := sodE2EChain(t, d, task.ID, host, opts, func() { env.notified++ })
	env.chain, env.tokens, env.tool = chain, tokens, tool
	env.ctx = WithRunContext(context.Background(), RunContext{
		TaskID:   task.ID,
		AgentKey: "worker",
	})
	return env
}

// sodE2EPayload 造本次调用的原始载荷（由 host 决定 ⇒ 父子进程能算出同一份）。
func sodE2EPayload(host string) []byte {
	return []byte(`{"command":"curl https://` + host + `/probe"}`)
}

// sodE2EChain 造一条受管路径上的链（链 + 凭证表 + 被调工具）。
//
// 为什么单独抽出来：特权对照的子进程要用**同一套装配**重新造一条链（它得在
// 另一个进程里独立裁决一次），两处共用这一份 ⇒ 对照才有意义。
func sodE2EChain(t *testing.T, d *db.DB, taskID int64, host string, opts sodE2EOptions, onReview func()) (*Chain, *TokenStore, *countingTool) {
	t.Helper()

	stub := gateStubAllowAll()
	stub.scopeInScope = opts.scopeInScope

	tokens := NewTokenStore()
	tool := &countingTool{name: "Bash"}
	if onReview == nil {
		onReview = func() {}
	}

	base := NewChain(nil, ChainDeps{
		Normalizer: newTestNormalizer(t, &fakeProvider{
			events: toolUseEvents("emit_structured_intent", sodE2EIntentJSON(host, opts.anomaly)),
		}),
		Gate:       &Gate{deps: stub},
		Tokens:     tokens,
		Auditor:    NewAuditor(d),
		Cache:      NewCache(16, "sod-e2e"),
		Assets:     d.Assets(),
		DB:         d,
		AgentKeyOf: AgentKeyFromContext,
		NotifyReview: func(ctx context.Context, item *db.AdversarialReviewItem) {
			onReview()
		},
	})
	return base.Wrap(&fakeHookRunner{block: opts.innerBlock, message: "guard 拦截", updated: opts.innerUpdated}), tokens, tool
}

// sodE2EIntentJSON 造一份合规的规整输出（可带异常标记）。
//
// 为什么要自己造而不是复用 normalizer_test.go 的 intentJSON：那个替身把
// anomaly_flag 写死成 false，而 needs_review 恰恰要靠**规整器**报出来的异常
// 触发（闸门第五道检查读的是规整器的产出，不是查出来的状态）。
func sodE2EIntentJSON(target string, anomaly bool) string {
	flag, reason := "false", ""
	if anomaly {
		flag, reason = "true", "载荷里出现编码后的内容"
	}
	return `{"structured_intent":{"action_type":"command_execution","executable":"curl",` +
		`"arguments":["-X","POST"],"target_hint":"` + target + `",` +
		`"shell_features":{"has_pipe":false,"has_redirect":false,"has_command_substitution":false,` +
		`"has_background":false,"has_obfuscation":false},"body_digest":"",` +
		`"raw_payload_digest":"deadbeef"},"anomaly_flag":` + flag + `,"anomaly_reason":"` + reason + `"}`
}

// sodDecisions 读本任务的决策账本（按 id 倒序）。
func (e *sodE2E) sodDecisions() []*db.SoDDecision {
	recs, err := e.db.ListSoDDecisions(e.taskID, 50)
	if err != nil {
		return nil
	}
	return recs
}

// TestSoDE2EApproveRunsToolOnceAndConsumesCredential 钉住放行路径的完整回路：
// inner 放行 → 闸门放行 → **工具恰好被调用一次** → 凭证被消费掉。
//
// 为什么这是最要紧的一条：放行路径上链铸凭证、执行器消费凭证，两边靠四元组
// 对齐。任何一处口径漂移（agent key 读法不同、载荷摘要算法不同、视图不共享
// 凭证表）都会让执行器查不到凭证 ⇒ 被批准的动作被拒 ⇒ 链等于瘫了。
func TestSoDE2EApproveRunsToolOnceAndConsumesCredential(t *testing.T) {
	env := newSoDE2E(t, sodE2EOptions{scopeInScope: true})

	block, msg, updated := env.chain.PreToolUse(env.ctx, "Bash", env.payload)
	if block {
		t.Fatalf("期望放行, 却被拒: %s", msg)
	}
	if len(updated) != 0 {
		t.Fatalf("inner 没改写入参, 链不该返回改写后的字节, 实际 %q", updated)
	}

	exec := WrapExecutor(env.tool, env.tokens, true)
	res, err := exec.Call(env.ctx, json.RawMessage(env.payload), nil)
	if err != nil {
		t.Fatalf("执行出错: %v", err)
	}
	if res.IsError {
		t.Fatalf("被批准的动作被拒: %s", res.Flatten())
	}
	if env.tool.calls != 1 {
		t.Fatalf("工具被调用了 %d 次, 期望 1 次", env.tool.calls)
	}

	// 凭证确实铸过（账本里留着凭证摘要，本体永不落库）。
	decisions := env.sodDecisions()
	if len(decisions) != 1 {
		t.Fatalf("决策账本应有 1 条, 实际 %d 条", len(decisions))
	}
	rec := decisions[0]
	if rec.Verdict != sodpolicy.VerdictApprove {
		t.Fatalf("账本 verdict=%q, 期望 %q", rec.Verdict, sodpolicy.VerdictApprove)
	}
	if rec.TokenDigest == "" {
		t.Fatal("放行必须留下凭证摘要（token_digest）")
	}
	if rec.ReasonCode != sodpolicy.ReasonAllChecksPassed {
		t.Fatalf("账本 reason_code=%q, 期望 %q", rec.ReasonCode, sodpolicy.ReasonAllChecksPassed)
	}
}

// TestSoDE2EDenyBlocksToolAndRecordsDecision 钉住拒绝路径：越界 ⇒ 工具**不**
// 被执行，且裁决留痕。
func TestSoDE2EDenyBlocksToolAndRecordsDecision(t *testing.T) {
	env := newSoDE2E(t, sodE2EOptions{scopeInScope: false})

	block, msg, _ := env.chain.PreToolUse(env.ctx, "Bash", env.payload)
	if !block {
		t.Fatal("范围外的目标必须被拒")
	}
	if !strings.Contains(msg, sodpolicy.ReasonOutOfScope) {
		t.Fatalf("拒绝消息 %q 应当带上原因码 %q", msg, sodpolicy.ReasonOutOfScope)
	}

	// 没有凭证 ⇒ 执行器侧也执行不了（纵深防御：链与执行器各拒一次）。
	exec := WrapExecutor(env.tool, env.tokens, true)
	res, err := exec.Call(env.ctx, json.RawMessage(env.payload), nil)
	if err != nil {
		t.Fatalf("期望返回错误结果而不是 Go error, got %v", err)
	}
	if !res.IsError {
		t.Fatal("被拒的动作不该被执行")
	}
	if env.tool.calls != 0 {
		t.Fatalf("工具被调用了 %d 次, 期望 0 次", env.tool.calls)
	}

	decisions := env.sodDecisions()
	if len(decisions) != 1 {
		t.Fatalf("决策账本应有 1 条, 实际 %d 条", len(decisions))
	}
	if decisions[0].Verdict != sodpolicy.VerdictDeny {
		t.Fatalf("账本 verdict=%q, 期望 %q", decisions[0].Verdict, sodpolicy.VerdictDeny)
	}
	if decisions[0].ReasonCode != sodpolicy.ReasonOutOfScope {
		t.Fatalf("账本 reason_code=%q, 期望 %q", decisions[0].ReasonCode, sodpolicy.ReasonOutOfScope)
	}
}

// TestSoDE2EReplayOfApprovedBytesIsRefused 钉住「一次性」：同一份字节执行第二次
// ⇒ 凭证已被消费 ⇒ 拒绝 ⇒ 工具只被调用过一次。
//
// 为什么这条是 SoD 的核心：凭证若能重放，攻击者就可以把一次「批准过的
// 命令」反复执行 —— 批准的是**这一次动作**，不是「这条命令永远合法」。
func TestSoDE2EReplayOfApprovedBytesIsRefused(t *testing.T) {
	env := newSoDE2E(t, sodE2EOptions{scopeInScope: true})

	block, msg, _ := env.chain.PreToolUse(env.ctx, "Bash", env.payload)
	if block {
		t.Fatalf("期望放行: %s", msg)
	}
	exec := WrapExecutor(env.tool, env.tokens, true)

	res, err := exec.Call(env.ctx, json.RawMessage(env.payload), nil)
	if err != nil {
		t.Fatalf("第一次执行出错: %v", err)
	}
	if res.IsError {
		t.Fatalf("第一次执行被拒: %s", res.Flatten())
	}
	if env.tool.calls != 1 {
		t.Fatalf("工具被调用了 %d 次, 期望 1 次", env.tool.calls)
	}

	res, err = exec.Call(env.ctx, json.RawMessage(env.payload), nil)
	if err != nil {
		t.Fatalf("重放调用出错: %v", err)
	}
	if !res.IsError {
		t.Fatal("重放没有被拒绝")
	}
	if env.tool.calls != 1 {
		t.Fatalf("重放时工具又被执行了, 累计 %d 次, 期望仍是 1 次", env.tool.calls)
	}
}

// TestSoDE2ENeedsReviewGoesToAdversarialQueueOnly 钉住 needs_review 的**唯一
// 去处**：对抗复核队列（交给 redteam 证伪），**绝不**进人工审批队列。
//
// 为什么这条要钉：needs_review 落进 intercept_pending 就变成了「请人看一眼」——
// 那是提出者自证的路径，也是人工队列被 SoD 灌满的路径。
func TestSoDE2ENeedsReviewGoesToAdversarialQueueOnly(t *testing.T) {
	env := newSoDE2E(t, sodE2EOptions{scopeInScope: true, anomaly: true})

	block, msg, _ := env.chain.PreToolUse(env.ctx, "Bash", env.payload)
	if !block {
		t.Fatal("needs_review 必须挡住执行")
	}
	if !strings.Contains(msg, sodpolicy.ReasonAnomalyFlagged) {
		t.Fatalf("拒绝消息 %q 应当带上原因码 %q", msg, sodpolicy.ReasonAnomalyFlagged)
	}

	exec := WrapExecutor(env.tool, env.tokens, true)
	res, err := exec.Call(env.ctx, json.RawMessage(env.payload), nil)
	if err != nil {
		t.Fatalf("期望返回错误结果而不是 Go error, got %v", err)
	}
	if !res.IsError {
		t.Fatal("needs_review 的动作不该被执行")
	}
	if env.tool.calls != 0 {
		t.Fatalf("工具被调用了 %d 次, 期望 0 次", env.tool.calls)
	}

	// 对抗复核队列有一行。
	items, err := env.db.ListAdversarialReviewPending(env.taskID, 50)
	if err != nil {
		t.Fatalf("读对抗复核队列失败: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("对抗复核队列应有 1 行, 实际 %d 行", len(items))
	}
	if items[0].ToolName != "Bash" {
		t.Fatalf("复核项 tool_name=%q, 期望 Bash", items[0].ToolName)
	}
	if items[0].ReasonCode != sodpolicy.ReasonAnomalyFlagged {
		t.Fatalf("复核项 reason_code=%q, 期望 %q", items[0].ReasonCode, sodpolicy.ReasonAnomalyFlagged)
	}
	if items[0].Status != db.SoDReviewPending {
		t.Fatalf("复核项 status=%q, 期望 %q", items[0].Status, db.SoDReviewPending)
	}

	// 人工审批队列（intercept_pending）**不得**有行。
	var humanRows int
	if err := env.db.QueryRow(`SELECT COUNT(*) FROM intercept_pending WHERE task_id=$1`,
		strconv.FormatInt(env.taskID, 10)).Scan(&humanRows); err != nil {
		t.Fatalf("读人工审批队列失败: %v", err)
	}
	if humanRows != 0 {
		t.Fatalf("needs_review 不得进人工审批队列, 实际 %d 行", humanRows)
	}

	// redteam 被通知了一次（对抗者去证伪，不是提出者自证）。
	if env.notified != 1 {
		t.Fatalf("应当通知 redteam 一次, 实际 %d 次", env.notified)
	}
}

// TestSoDE2EInnerBlocksBeforeGating 钉住「先问 inner，再跑链」：guard 拦下的
// 动作，链**不参与裁决**（消息里没有 [SoD] 前缀，账本里也没有裁决）。
//
// 为什么这条重要：链是装饰器，不是替代品 —— guard 的短路表（放行清单 /
// 拦截规则 / 人工 ask）是宿主既有行为，链不能抢在它前面。
func TestSoDE2EInnerBlocksBeforeGating(t *testing.T) {
	env := newSoDE2E(t, sodE2EOptions{scopeInScope: true, innerBlock: true})

	block, msg, _ := env.chain.PreToolUse(env.ctx, "Bash", env.payload)
	if !block {
		t.Fatal("inner 拦截时必须挡住执行")
	}
	if strings.Contains(msg, "[SoD]") {
		t.Fatalf("inner 已拦截时链不该参与裁决, 实际消息 %q", msg)
	}
	if got := len(env.sodDecisions()); got != 0 {
		t.Fatalf("inner 已拦截时链不该留痕, 实际 %d 条", got)
	}
}

// TestSoDChainDecisionPathIgnoresProcessPrivilege 钉住「裁决路径里没有读特权
// 状态的那条路」。
//
// 为什么用结构断言：行为断言（"这次没受特权影响"）只能证明一次没受影响；
// 结构断言证明的是「**没有那条路**」—— 决策路径里根本不存在读 uid/gid/降权
// 身份的代码。
//
// 行为那一腿在 e2e_privilege_linux_test.go：同一个二进制在另一个特权上下文
// （user namespace ⇒ 容器内 uid=0）里裁决一次，两次结论必须一致。
func TestSoDChainDecisionPathIgnoresProcessPrivilege(t *testing.T) {
	forbidden := []string{
		"Getuid", "Getgid", "Geteuid", "Getegid",
		"SysProcAttr", "Credential", "setuid", "Setuid",
	}
	for _, name := range []string{
		"chain.go", "gate.go", "normalizer.go", "token.go", "digest.go",
		"cache.go", "redact.go", "auditor.go", "scope.go", "executor.go",
	} {
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("读 %s 失败: %v", name, err)
		}
		for _, bad := range forbidden {
			if strings.Contains(string(src), bad) {
				t.Fatalf("%s 里出现了特权相关符号 %q —— 裁决不得依赖进程特权", name, bad)
			}
		}
	}
}

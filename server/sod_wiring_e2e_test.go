// 本文件是 SoD（职责分离）链的【接线验收】测试：证明链真的挂在 worker 的
// PreToolUse hook 上，而不是「注册了但没接上」。
//
// 为什么必须有它：SoD 链有两半 ——
//
//	① 链（server/sod_chain.go 注册的进程级单例）负责裁决并留痕；
//	② 执行器装饰器（agent/assembly.go 装在所有工具上）负责在真正执行前消费
//	   一次性执行凭证。
//
// 裁决发生在 hook 里（server/engine.go 的 `inner: WrapHookRunner(t.Guard.Hooks())`）
// ⇒ **hook 那根线断了，链就是个摆设**：工具照跑、没人裁决、决策账本一行都不增，
// 而且没有任何报错（链没被调用，自然不会说话）。
//
// 与已有测试的分工：
//   - internal/sod/e2e_test.go 钉的是【零件】：链与执行器在真库上的回路（它自己
//     造链，不经过宿主的挂载点）；
//   - agent/sod_ctx_propagation_test.go 钉的是 ctx 上的 RunInfo 不丢；
//   - 本文件钉的是【装配】：真实的 worker 意图经由 server/engine.go 的真实路径
//     跑一次，决策账本里必须出现那一行。
//
// 【口径来源】假 provider 的形态照抄 internal/sod/e2e_test.go 用的那一套（模型侧
// 不联网）；夹具约定照抄 server/strategy_e2e_test.go（server 包自己的派生库 +
// 真 Server + 真任务）。
//
// allow: SIZE_OK —— 本单元的文件域被任务书钉死为**一个**测试文件，且四个用例
// 共用同一套夹具（假 provider + server + 任务 + 意图 + 装链/摘链）。拆成两个
// 文件就要把夹具复制一份（或把夹具抽成共享 helper 文件），两者都会让「链装没
// 装、挂在哪」这条口径出现第二份副本。
package server

import (
	"context"
	"fmt"
	"iter"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/internal/sod"
	"github.com/Autumn-27/artex/internal/sodpolicy"
	"github.com/Autumn-27/artex/internal/testdb"
	"github.com/Autumn-27/norma/llm"
)

// ---------- 假模型（不联网、不碰真实 LLM） ----------

// sodWiringProvider 是「按请求形态分派」的假 provider。
//
// 为什么一个 provider 演两个角色：链的规整器与 worker 共用 server 侧的模型解析
// （sodProviderFunc → providerForProfile）⇒ 生产里它们本来就是同一套 LLM 配置。
// 测试按「请求只带 emit_structured_intent 一个工具」把两者分开：
//   - 规整器请求 ⇒ 回一份合规的结构化意图（目标落在任务授权范围内）；
//   - worker 请求 ⇒ 第一轮回一次 Bash 工具调用，第二轮回一句收工文本。
type sodWiringProvider struct {
	// target 是结构化意图里的目标线索。它必须落在任务的授权范围内，否则闸门
	// 第一道检查（scope）会判越界 —— 那是闸门该做的事，不是本用例要验的事。
	target string
	// command 是 worker 那次 Bash 调用的命令（无副作用的 echo）。
	command string
	// workerTurns / normalizerTurns 记录两侧各被调了几次（断言用）。
	workerTurns     int
	normalizerTurns int
}

// Stream 回一次「模型回合」的流事件（形态照抄 internal/sod/normalizer_test.go 的
// toolUseEvents：工具名 + 工具入参 + 收场）。
func (p *sodWiringProvider) Stream(_ context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		// 规整器的请求只带 emit_structured_intent 一个工具（internal/sod 的
		// Normalize 就是这么调的）⇒ 据此把两个角色分开。
		if len(req.Tools) == 1 && req.Tools[0].Name == sodIntentToolKey {
			p.normalizerTurns++
			for _, ev := range sodWiringToolUse(sodIntentToolKey, p.intentJSON()) {
				if !yield(ev, nil) {
					return
				}
			}
			return
		}
		p.workerTurns++
		if p.workerTurns == 1 {
			for _, ev := range sodWiringToolUse("Bash", p.bashInput()) {
				if !yield(ev, nil) {
					return
				}
			}
			return
		}
		// 第二轮不再调工具 ⇒ harness 正常收场。
		// ⚠️ 必须有正文：空转回合（只有思考、没有正文也没有工具调用）会被
		// steerHooks 当成「空转」注入续跑指令，让本条意图一直跑下去。
		if !yield(llm.StreamEvent{Type: llm.SEMessageStart}, nil) {
			return
		}
		if !yield(llm.StreamEvent{Type: llm.SETextDelta, Text: "本条意图已执行完毕"}, nil) {
			return
		}
		yield(llm.StreamEvent{Type: llm.SEMessageStop}, nil)
	}
}

// Complete 是流式接口的非流式 counterpart（norma 的 Provider 契约）。
func (p *sodWiringProvider) Complete(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	acc := llm.NewAccumulator()
	for ev, err := range p.Stream(ctx, req) {
		if err != nil {
			return llm.Message{}, "", llm.Usage{}, err
		}
		acc.Add(ev)
	}
	return acc.Message(), acc.StopReason, acc.Usage, nil
}

// sodWiringToolUse 造一次「模型调用了某个工具」的流事件序列。
func sodWiringToolUse(name, input string) []llm.StreamEvent {
	return []llm.StreamEvent{
		{Type: llm.SEToolUseStart, ToolID: "tu_sod_wiring", ToolName: name, ToolIndex: 0},
		{Type: llm.SEToolInputJSON, Text: input, ToolIndex: 0},
		{Type: llm.SEMessageStop},
	}
}

// intentJSON 造一份合规的结构化意图（目标在范围内、无异常标记）。
func (p *sodWiringProvider) intentJSON() string {
	return `{"structured_intent":{"action_type":"command_execution","executable":"echo",` +
		`"arguments":["sod-wiring"],"target_hint":"` + p.target + `",` +
		`"shell_features":{"has_pipe":false,"has_redirect":false,"has_command_substitution":false,` +
		`"has_background":false,"has_obfuscation":false},"body_digest":"",` +
		`"raw_payload_digest":"deadbeef"},"anomaly_flag":false,"anomaly_reason":""}`
}

// bashInput 造 worker 那次 Bash 调用的入参。
func (p *sodWiringProvider) bashInput() string {
	return `{"command":"` + p.command + `"}`
}

// ---------- 夹具 ----------

// sodWiringFixture 是一次接线验收的现场：真 server + 真任务 + 真意图 + 假模型。
type sodWiringFixture struct {
	server *Server
	pg     *db.DB
	task   *Task
	intent *db.Node
	prov   *sodWiringProvider
	taskID int64
}

// newSoDWiringFixture 搭好一次接线验收的现场。
//
// 三个关键动作：
//
//	① 装链 —— server 启动时做（server/server.go 的 installSoDChain）；
//	② 把【假模型】塞进 server 的 provider 缓存，让链的规整器与 worker 共用它
//	   （server 包既有的替身口径，见 server/side_questions_test.go 的
//	   provByProfile 注入）；
//	③ 声明任务范围，让闸门第一道检查（scope）能命中 —— 否则裁决会是「越界拒绝」，
//	   用例验的就不是「链有没有接上」而是「范围判得对不对」。
func newSoDWiringFixture(t *testing.T) *sodWiringFixture {
	t.Helper()
	s, pg := sodWiringServer(t)

	host := fmt.Sprintf("sod-wiring-%d.example.com", time.Now().UnixNano())
	task, err := s.m.CreateTask("SoD 接线验收", "跑一次受管工具调用", nil, 0, 0)
	if err != nil {
		t.Fatalf("建任务失败: %v", err)
	}
	taskID, err := strconv.ParseInt(task.ID, 10, 64)
	if err != nil {
		t.Fatalf("任务 id 不是数字: %v", err)
	}
	t.Cleanup(func() { _ = pg.DeleteTask(taskID) })

	if _, err := pg.Assets().AddManualScope(taskID, "root_domain", host, "SoD 接线验收"); err != nil {
		t.Fatalf("声明任务范围失败: %v", err)
	}

	nodeID, err := task.Store.AddNode(db.KindIntent, map[string]any{
		"text": "对授权范围内的目标跑一次受管命令",
	}, 0, "open", "planner", nil)
	if err != nil {
		t.Fatalf("建意图失败: %v", err)
	}
	intent, err := task.Store.GetNode(nodeID)
	if err != nil || intent == nil {
		t.Fatalf("取意图失败: %v", err)
	}

	prov := &sodWiringProvider{target: host, command: "echo sod-wiring"}
	sodWiringBindProvider(t, s, pg, prov)

	return &sodWiringFixture{
		server: s, pg: pg, task: task, intent: intent, prov: prov, taskID: taskID,
	}
}

func sodWiringServer(t *testing.T) (*Server, *db.DB) {
	t.Helper()
	sodWiringDetachChain(t)
	// 本用例断言的是「链裁没裁决、凭证被没被消费」⇒ 它对库里的残留状态极其敏感：
	// server 包的所有用例共用**一个**派生库（server/testmain_test.go 的
	// testdb.Setup("server") 是【每包一个】，不是【每用例一个】）⇒ 前一个用例留下的
	// 拦截规则、profile、意图都会渗进来（前一个用例留下的 deny 规则会拒掉本用例的
	// Bash，症状是「链明明批了、工具却报 error」，极具误导性）。
	//
	// 所以这里自带一个独立库：表是干净的、规则是空的、profile 是没有的 ⇒ 测到的
	// 就是链自己的行为。代价是多派生一个库（internal/testdb 命名 artex_test_sodwiring）。
	dsn, release, err := testdb.Setup("sodwiring")
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	prevDSN := os.Getenv("ARTEX_PG_DSN")
	if err := os.Setenv("ARTEX_PG_DSN", dsn); err != nil {
		release()
		t.Fatalf("设置测试 DSN 失败: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Setenv("ARTEX_PG_DSN", prevDSN)
		release()
	})
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	t.Cleanup(func() { m.Close() })
	s := New(testServerCtx(t), m, t.TempDir(), t.TempDir(), t.TempDir())
	return s, m.pg
}

// sodWiringDetachChain 把进程级 SoD 链从 hook 上摘掉（仅本测试文件使用）。
//
// 它同时把执行器凭证表清空：凭证表与链必须成对（链铸、执行器消费），链没了
// 而表还在，执行器就拿着一张永远铸不出凭证的表 —— 那正是「链没接上」的真实
// 形态，执行器一律拒绝（fail-closed）。
func sodWiringDetachChain(t *testing.T) {
	t.Helper()
	detach := func() {
		sodChainSlot.mu.Lock()
		sodChainSlot.chain = nil
		sodChainSlot.mu.Unlock()
		sod.ResetExecutorTokensForTest()
	}
	detach()
	t.Cleanup(detach)
}

// sodWiringBindProvider 把假 provider 装进 server 的 provider 缓存，并让它对应
// 的 profile 成为【活动 profile】—— 链的规整器按 profile 解析模型
// （sodProviderFunc → providerForProfile），profileID=0 时取活动 profile。
//
// 为什么能这么替：providerForProfile 命中缓存就直接返回，不碰网络
// （server/server.go）。server 包已有同款替身口径（side_questions_test.go）。
func sodWiringBindProvider(t *testing.T, s *Server, pg *db.DB, prov *sodWiringProvider) {
	t.Helper()
	id, err := pg.SaveProfile(&db.LLMProfile{
		Name:   "sod-wiring-fixture",
		Format: "openai",
		Model:  "fixture-model",
	})
	if err != nil {
		t.Fatalf("建 LLM profile 失败: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pg.Exec(`UPDATE llm_profiles SET is_default=false WHERE id=$1`, id)
		_, _ = pg.Exec(`DELETE FROM llm_profiles WHERE id=$1`, id)
	})
	if err := pg.SetActiveProfile(id); err != nil {
		t.Fatalf("激活 LLM profile 失败: %v", err)
	}
	s.provCacheMu.Lock()
	s.provByProfile[provKey{id: id}] = &provEntry{prov: prov, cfg: agent.Config{
		Format: llm.FormatOpenAI,
		Model:  "fixture-model",
		Stream: true,
	}}
	s.provCacheMu.Unlock()

	// LLM 兜底裁判（guard 在「没命中任何拦截规则」时问它）必须关掉：它是另一条
	// 独立的防线，会在链裁决**之前**决定放不放行 ⇒ 开着它，本用例测到的就不是
	// 「链裁没裁决」而是「裁判裁没裁决」。
	prev, existed, err := pg.GetSetting("llm_judge_enabled")
	if err != nil {
		t.Fatalf("读裁判开关失败: %v", err)
	}
	if err := pg.SetSetting("llm_judge_enabled", "false"); err != nil {
		t.Fatalf("关裁判失败: %v", err)
	}
	t.Cleanup(func() {
		if existed {
			_ = pg.SetSetting("llm_judge_enabled", prev)
			return
		}
		_, _ = pg.Exec(`DELETE FROM settings WHERE key=$1`, "llm_judge_enabled")
	})
}

// runWorkerIntent 让 engine 认领并执行这条意图（真实路径）。
//
// 走的是 engine 的 runWorkerStep —— 与 worker 池循环调的是同一个入口：认领
// （open→running）+ 执行（装 hook、跑 worker）。
func (fx *sodWiringFixture) runWorkerIntent(t *testing.T) {
	t.Helper()
	worker := agent.NewWorker(fx.prov, "fixture-model", t.TempDir(), nil, 100000, 8)
	if !fx.server.engine.runWorkerStep(fx.server.ctx, fx.task, "work#1", worker) {
		t.Fatal("engine 没有认领到这条意图 —— 用例没有真的跑起 worker")
	}
}

// sodWiringDecisions 读本任务的决策账本（按 id 倒序）。
func sodWiringDecisions(t *testing.T, pg *db.DB, taskID int64) []*db.SoDDecision {
	t.Helper()
	recs, err := pg.ListSoDDecisions(taskID, 50)
	if err != nil {
		t.Fatalf("读决策账本失败: %v", err)
	}
	return recs
}

// ---------- 用例 ----------

// TestSoDChainConnectedToWorkerHook 钉住「链真的挂在 worker 的 hook 上」。
//
// 为什么这是生死线：链挂在 worker 的 PreToolUse hook 上（server/engine.go 的
// `inner: WrapHookRunner(t.Guard.Hooks())`）。那根线断了的话，整条链形同虚设 ——
// 工具照跑、没人裁决、决策账本一行都不增，而且**没有任何报错**。零件测试
// （internal/sod/e2e_test.go）全绿也证明不了这一点：它们自己造链，不经过宿主的
// 挂载点。
//
// 本用例跑的是真实路径：真 server → 真任务 → 真意图 → engine 的 runWorkerStep
// （认领 + 执行）→ worker 的工具调用 → 链裁决 → 决策账本。
func TestSoDChainConnectedToWorkerHook(t *testing.T) {
	fx := newSoDWiringFixture(t)

	if !SoDChainInstalled() {
		t.Fatal("server 启动后链应当已安装")
	}

	fx.runWorkerIntent(t)

	decisions := sodWiringDecisions(t, fx.pg, fx.taskID)
	if len(decisions) == 0 {
		t.Fatal("worker 意图跑完了，sod_decisions 里却没有决策行 —— 链没有接到 worker 的 hook 上")
	}
	// 规整器必须真的被调用过：没有规整就没有结构化意图，链只能按
	// normalizer_unavailable 一律拒绝（那样这条用例就退化成了「拒绝也是裁决」）。
	if fx.prov.normalizerTurns == 0 {
		t.Fatal("规整器一次都没被调用 —— 链的规整环节没有接上")
	}
}

// TestSoDDecisionLedgerRecordsWorkerIntentCall 钉住「账本里那一行归属的是【这条
// 意图的那次工具调用】」，而不只是「账本里有东西」。
//
// 为什么归属也要钉：账本行是事后追责的唯一凭据 —— 查不出是哪条意图、哪个
// agent、哪个工具发起的动作，账本就只是一堆无法定位的记录。
func TestSoDDecisionLedgerRecordsWorkerIntentCall(t *testing.T) {
	fx := newSoDWiringFixture(t)
	fx.runWorkerIntent(t)

	decisions := sodWiringDecisions(t, fx.pg, fx.taskID)
	if len(decisions) != 1 {
		t.Fatalf("决策账本应有 1 条（一次受管工具调用一次裁决），实际 %d 条", len(decisions))
	}
	rec := decisions[0]
	wantAgent := "worker#" + strconv.FormatInt(fx.intent.ID, 10)
	if rec.SourceAgent != wantAgent {
		t.Fatalf("账本 source_agent=%q，期望 %q", rec.SourceAgent, wantAgent)
	}
	if rec.ToolName != "Bash" {
		t.Fatalf("账本 tool_name=%q，期望 Bash", rec.ToolName)
	}
	if rec.TaskID == nil || *rec.TaskID != fx.taskID {
		t.Fatalf("账本 task_id=%v，期望 %d", rec.TaskID, fx.taskID)
	}
	if rec.Verdict != sodpolicy.VerdictApprove {
		t.Fatalf("账本 verdict=%q，期望 %q（reason_code=%s）",
			rec.Verdict, sodpolicy.VerdictApprove, rec.ReasonCode)
	}
	if rec.ReasonCode != sodpolicy.ReasonAllChecksPassed {
		t.Fatalf("账本 reason_code=%q，期望 %q", rec.ReasonCode, sodpolicy.ReasonAllChecksPassed)
	}
	if rec.TokenDigest == "" {
		t.Fatal("放行必须留下凭证摘要（token_digest）—— 链铸了凭证才有资格放行")
	}
	if rec.PayloadDigest == "" {
		t.Fatal("账本必须留下载荷摘要（payload_digest）")
	}
	// 闭环的最后一环：链铸的凭证被执行器消费掉了 ⇒ 工具真的跑了。
	//
	// 为什么还要这一条：账本上有 approve 只说明「链批了」。若链与执行器对不上
	// 四元组（agent_key 读法不同 / 载荷摘要算法不同 / 两张凭证表），执行器会
	// 拒掉这次调用 —— 账本照样写着 approve，而工具根本没跑。这是 SoD 最隐蔽的
	// 失效形态（放行记录齐全、动作没发生），必须在这里钉住。
	if ok := fx.task.Guard.Attributions()["ok"]; ok == 0 {
		t.Fatalf("Bash 工具没有成功执行（guard 归因=%v）—— 链铸的凭证没被执行器消费掉",
			fx.task.Guard.Attributions())
	}
}

// TestSoDWiringChainDetachedLeavesNoDecision 是上一条的对照：把链从 hook 上
// 摘掉之后，同一条 worker 意图**不该**在账本里留下任何裁决。
//
// 为什么必须有对照：光有「有裁决行」这一条，还不能排除「那行是别的东西写的」
// （guard 的审计、活动表、别的 hook）。对照把变量缩到【链挂没挂】这一个 ⇒
// 前一条的因果关系才是真的。
//
// 顺带钉住「工具调用确实到了 hook 那一层」：guard 的审计里能看到那次 Bash ⇒
// 不是「工具压根没被调用、所以没有裁决」，而是「工具被调用了、只是没人裁决」。
func TestSoDWiringChainDetachedLeavesNoDecision(t *testing.T) {
	fx := newSoDWiringFixture(t)
	sodWiringDetachChain(t)
	if SoDChainInstalled() {
		t.Fatal("摘链后不应报告已安装")
	}

	fx.runWorkerIntent(t)

	if got := len(sodWiringDecisions(t, fx.pg, fx.taskID)); got != 0 {
		t.Fatalf("链没挂上时不该有决策行，实际 %d 条", got)
	}
	sawBash := false
	for _, e := range fx.task.Guard.Audit() {
		if e.Tool == "Bash" {
			sawBash = true
			break
		}
	}
	if !sawBash {
		t.Fatal("guard 的审计里没有那次 Bash 调用 —— 对照用例没真的跑起工具调用")
	}
}

// TestSoDWiringChainInstalledOnServerStartup 钉住「链装了，且 worker 的 hook
// runner 确实是链的视图」。
//
// 为什么单列一条：链「注册了」与链「挂到 hook 上」是两件事。前者由
// SoDChainInstalled() 报告；后者要看 WrapHookRunner 返回的是不是链的视图 ——
// 没装链时它**原样返回**宿主给的 runner（server/sod_chain.go），所以「返回的
// 不是原来那个」正是「挂上了」的判据。
func TestSoDWiringChainInstalledOnServerStartup(t *testing.T) {
	fx := newSoDWiringFixture(t)
	if !SoDChainInstalled() {
		t.Fatal("server 启动后链应当已安装")
	}
	inner := fx.task.Guard.Hooks()
	if wrapped := WrapHookRunner(inner); wrapped == inner {
		t.Fatal("WrapHookRunner 原样返回了宿主的 runner ⇒ 链没有挂到 worker 的 hook 上")
	}
}

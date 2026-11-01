// 本文件钉住策略库（L4）三条【宿主侧接线】在生产路径上真的跑起来了：
//
//  1. 归因器挂在「意图落到终态」上（server/engine.go 的 runIntent）；
//  2. 重放语料挂在「任务进入终态」上（server/engine.go 的任务终态迁移）；
//  3. planner 的策略摘要注入由宿主侧装配（server/engine.go 的 plannerLoop）。
//
// 断言钉的是【承诺】而不是实现细节：任何一条接线被摘掉，对应用例都会失败。
//
// 【测试隔离】每个用例造的东西都在用例结束时删掉（唯一名 + 显式清理），
// 不依赖用例之间的执行顺序。助手自己写一套（不挂别的单元的私有助手）——
// 那些文件由别的单元并行持有。
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/internal/sod"
	"github.com/Autumn-27/norma/llm"
)

// wiringResetSoDSlot 清掉进程级 SoD 链单例与执行器凭证表。
//
// 为什么需要复位：server.New() 会装 SoD 链，而链是**进程级单例**（只允许装
// 一次；装不上时 server.New 走 log.Fatalf ⇒ 整个测试二进制当场退出）。同包
// 其它用例建过 server 时，本文件再建一个就会撞上 ⇒ 建之前先复位。
//
// 为什么自己写而不复用别的测试文件里的同名助手：那些文件由别的单元并行
// 持有，本文件的用例不该挂在别人的私有助手上。
func wiringResetSoDSlot(t *testing.T) {
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

// wiringSuffix 给用例生成互不撞的标识后缀（纳秒级）。
func wiringSuffix() int64 { return time.Now().UnixNano() }

// wiringProvider 是假模型 provider：只回一句文本，不调任何工具。
//
// 为什么要自己写而不复用别的测试文件里的同名助手：那些文件由别的单元并行
// 持有，本文件的用例不该挂在别人的私有助手上。
type wiringProvider struct {
	requests chan llm.CompletionRequest
	reply    string
	err      error
}

func (p wiringProvider) Complete(_ context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	if p.requests != nil {
		select {
		case p.requests <- req:
		default:
		}
	}
	if p.err != nil {
		return llm.Message{}, "", llm.Usage{}, p.err
	}
	return llm.Message{Role: llm.RoleAssistant,
		Content: []llm.ContentBlock{llm.TextBlock(p.reply)}}, "end_turn", llm.Usage{}, nil
}

func (p wiringProvider) Stream(context.Context, llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		yield(llm.StreamEvent{}, errors.New("测试只走非流式"))
	}
}

// wiringServer 起一个带真实引擎的服务（自带 SoD 链单例复位）。
//
// 为什么不复用别的测试文件里的同名助手：那些文件由别的单元并行持有，本文件的
// 用例不该挂在别人的私有助手上。
func wiringServer(t *testing.T) (*Server, *db.DB) {
	t.Helper()
	wiringResetSoDSlot(t)
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	t.Cleanup(func() { m.Close() })
	td := t.TempDir()
	// ctx 必须可取消：server.New 会起后台循环（notifier / 归档 worker /
	// reconcileConcurrency），它们靠 <-ctx.Done() 退出。
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	s := New(ctx, m, td, td, td)
	return s, m.pg
}

// wiringTask 建一个任务并在用例结束时删掉。
func wiringTask(t *testing.T, s *Server, desc string) *Task {
	t.Helper()
	task, err := s.m.CreateTask(desc, "拿下目标", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = s.m.DeleteTask(task.ID, DeleteTaskOptions{}) })
	return task
}

// wiringTaskID 取任务的数值 id（策略库里的一切都以数值 id 为键）。
func wiringTaskID(t *testing.T, task *Task) int64 {
	t.Helper()
	id, err := strconv.ParseInt(task.ID, 10, 64)
	if err != nil {
		t.Fatalf("任务 id %q 不是数值: %v", task.ID, err)
	}
	return id
}

// wiringStrategy 建一条【在役】策略，并给任务配一枚范围内资产让它能被检索到。
//
// 返回策略 id 与它的摘要（注入文本里应当出现的那个字符串）。
//
// 为什么资产要走 task_scope(kind='asset')：策略检索的资产签名取的是**范围内**
// 资产的 category ⇒ 不声明范围，签名就是空的，结构路径匹配不到任何候选。
func wiringStrategy(t *testing.T, pg *db.DB, task *Task, sfx int64) (int64, string) {
	t.Helper()
	taskID := wiringTaskID(t, task)
	category := fmt.Sprintf("wiring-cat-%d", sfx)

	var assetID int64
	if err := pg.QueryRow(`INSERT INTO assets(type, domain, root_domain, category, last_seen)
		VALUES('subdomain',$1,$1,$2,now()) RETURNING id`,
		fmt.Sprintf("wiring-%d.example.test", sfx%1000000), category).Scan(&assetID); err != nil {
		t.Fatalf("插资产: %v", err)
	}
	t.Cleanup(func() { _, _ = pg.Exec(`DELETE FROM assets WHERE id=$1`, assetID) })
	if _, err := pg.Exec(`INSERT INTO task_scope(task_id, kind, asset_id, source)
		VALUES($1,'asset',$2,'manual') ON CONFLICT DO NOTHING`, taskID, assetID); err != nil {
		t.Fatalf("声明任务范围: %v", err)
	}

	summary := fmt.Sprintf("接线摘要 %d", sfx)
	id, err := pg.CreateStrategy(&db.Strategy{
		Name:       fmt.Sprintf("wiring-策略-%d", sfx),
		Summary:    summary,
		Steps:      json.RawMessage(`["curl -X POST https://$TARGET/$PATH --tag WIRINGSTEPBODY"]`),
		Activation: json.RawMessage(fmt.Sprintf(`{"category":[%q]}`, category)),
		SuccessCriteria: json.RawMessage(
			`[{"id":"m1","where":"detail","match":"(?i)syntax error","kind":"command"}]`),
	})
	if err != nil {
		t.Fatalf("建策略: %v", err)
	}
	t.Cleanup(func() { _, _ = pg.Exec(`DELETE FROM strategies WHERE id=$1`, id) })
	if ok, err := pg.PromoteStrategyToActive(id); err != nil || !ok {
		t.Fatalf("置为 active: %v", err)
	}
	return id, summary
}

// wiringSubTask 在任务里造一个子任务：一个意图 + 一条 fact + 命中里程碑的执行步骤，
// 并把策略挂到该意图的上下文里（模拟「检索命中并注入」）。
//
// factState 决定子任务是否失败：dismissed = 结论被推翻 = 失败子任务。
func wiringSubTask(t *testing.T, task *Task, strategyID int64, factState string) int64 {
	t.Helper()
	intentID, err := task.Store.AddNode(db.KindIntent, map[string]any{"summary": "探测目标"},
		5, "running", "worker", nil)
	if err != nil {
		t.Fatalf("建意图: %v", err)
	}
	factID, err := task.Store.AddNode(db.KindFact, map[string]any{"summary": "探测结论"},
		5, factState, "worker", nil)
	if err != nil {
		t.Fatalf("建事实节点: %v", err)
	}
	if err := task.Store.Link(intentID, db.RelYields, factID); err != nil {
		t.Fatalf("连血缘: %v", err)
	}
	nodeID := intentID
	if _, err := task.Store.AppendActivity(db.Activity{
		NodeID:  &nodeID,
		Worker:  "worker",
		Kind:    "tool_result",
		Tool:    "curl",
		Summary: "curl 探测",
		Detail:  "syntax error near $TARGET",
	}); err != nil {
		t.Fatalf("写执行记录: %v", err)
	}
	if err := wiringUsagePending(t, task, strategyID, intentID); err != nil {
		t.Fatalf("记策略使用明细: %v", err)
	}
	return intentID
}

// wiringUsagePending 把一条策略挂到某个意图的上下文里（pending = 还没结算）。
func wiringUsagePending(t *testing.T, task *Task, strategyID, intentID int64) error {
	t.Helper()
	return task.Store.DB().RecordStrategyUsage(db.StrategyUsage{
		StrategyID:    strategyID,
		TaskID:        wiringTaskID(t, task),
		ExplorationID: task.Store.ID(),
		IntentID:      intentID,
		Outcome:       db.StrategyUsagePending,
	})
}

// wiringRunIntent 跑一次意图结算（真引擎路径：worker 返回 → 意图落终态）。
func wiringRunIntent(t *testing.T, s *Server, task *Task, intentID int64) {
	t.Helper()
	node, err := task.Store.GetNode(intentID)
	if err != nil || node == nil {
		t.Fatalf("读意图: %v", err)
	}
	worker := agent.NewWorker(wiringProvider{reply: "本轮无可执行动作"},
		"wiring-worker", s.m.dir, nil, 10000, 1)
	worker.SetNonStreaming(func() bool { return true })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.engine.runIntent(ctx, task, "worker-1", worker, node, "", "")
}

// wiringStrategyConfidence 读一条策略的 confidence（票有没有真的落库）。
func wiringStrategyConfidence(t *testing.T, pg *db.DB, strategyID int64) float64 {
	t.Helper()
	st, err := pg.GetStrategy(strategyID)
	if err != nil || st == nil {
		t.Fatalf("读策略 %d: %v", strategyID, err)
	}
	return st.Confidence
}

// TestStrategyAttributionRunsOnIntentSettlement 钉住「归因挂在意图终态上」。
//
// 场景：一条意图跑完（正常收场 → done），它产出的 fact 命中了策略声明的里程碑。
// 断言（意图结算之后）：
//   - strategy_usage 里那条明细从 pending 变成 hit，分类是 skill_guided；
//   - 策略的 confidence 涨了（票真的落库了）；
//   - 有一条 status='pending' 的提议（agent 只能提议，写不出 active）。
//
// 接线被摘掉时：明细仍是 pending、confidence 不动、提议不存在 ⇒ 三条断言全挂。
func TestStrategyAttributionRunsOnIntentSettlement(t *testing.T) {
	s, pg := wiringServer(t)
	task := wiringTask(t, s, "意图终态归因")
	taskID := wiringTaskID(t, task)
	strategyID, _ := wiringStrategy(t, pg, task, wiringSuffix())
	intentID := wiringSubTask(t, task, strategyID, "confirmed")

	before := wiringStrategyConfidence(t, pg, strategyID)

	wiringRunIntent(t, s, task, intentID)

	usages, err := pg.StrategyUsageForTask(taskID)
	if err != nil {
		t.Fatalf("读策略使用明细: %v", err)
	}
	var found bool
	for _, u := range usages {
		if u.IntentID != intentID {
			continue
		}
		found = true
		if u.Outcome != db.StrategyUsageHit {
			t.Fatalf("意图 #%d 的使用明细 outcome=%q，期望 %q：归因没有在意图终态上跑",
				intentID, u.Outcome, db.StrategyUsageHit)
		}
		if u.Classification != db.StrategyAttributionSkillGuided {
			t.Fatalf("意图 #%d 的使用明细分类=%q，期望 %q",
				intentID, u.Classification, db.StrategyAttributionSkillGuided)
		}
	}
	if !found {
		t.Fatalf("意图 #%d 没有留下任何策略使用明细", intentID)
	}

	after := wiringStrategyConfidence(t, pg, strategyID)
	if after <= before {
		t.Fatalf("策略 confidence 没涨（%v → %v）：票没有落库", before, after)
	}

	proposals, err := pg.ListStrategyProposals(db.StrategyProposalPending, 200)
	if err != nil {
		t.Fatalf("列提议: %v", err)
	}
	var proposed bool
	for _, p := range proposals {
		if p.StrategyID == strategyID {
			proposed = true
			if p.Status != db.StrategyProposalPending {
				t.Fatalf("提议 status=%q，期望 pending（裁决只在宿主侧发生）", p.Status)
			}
		}
	}
	if !proposed {
		t.Fatalf("策略 %d 没有待裁决的提议：归因结论没有路由成提议", strategyID)
	}
}

// TestStrategyFailedSubTaskNeverTouchesTheLibrary 钉住「失败子任务永不直接改库」
// 在【生产路径】上也成立。
//
// 场景：意图正常收场，但它产出的结论被推翻（fact 状态 = dismissed）⇒ 子任务算失败。
// 断言：
//   - 使用明细的 outcome 是 miss（诊断留痕）；
//   - 策略的 confidence 一字未改（失败子任务不给策略记功）；
//   - 没有产生任何提议（没有可复用的信号 ⇒ 不该提议）。
//
// 接线被摘掉时：明细仍是 pending ⇒ 第一条断言就挂。
func TestStrategyFailedSubTaskNeverTouchesTheLibrary(t *testing.T) {
	s, pg := wiringServer(t)
	task := wiringTask(t, s, "失败子任务不改库")
	taskID := wiringTaskID(t, task)
	strategyID, _ := wiringStrategy(t, pg, task, wiringSuffix())
	intentID := wiringSubTask(t, task, strategyID, "dismissed")

	before := wiringStrategyConfidence(t, pg, strategyID)

	wiringRunIntent(t, s, task, intentID)

	usages, err := pg.StrategyUsageForTask(taskID)
	if err != nil {
		t.Fatalf("读策略使用明细: %v", err)
	}
	var found bool
	for _, u := range usages {
		if u.IntentID != intentID {
			continue
		}
		found = true
		if u.Outcome != db.StrategyUsageMiss {
			t.Fatalf("失败子任务的 usage outcome=%q，期望 %q", u.Outcome, db.StrategyUsageMiss)
		}
	}
	if !found {
		t.Fatalf("意图 #%d 没有留下任何策略使用明细：归因没有在意图终态上跑", intentID)
	}

	after := wiringStrategyConfidence(t, pg, strategyID)
	if after != before {
		t.Fatalf("失败子任务改了策略 confidence（%v → %v）：失败子任务永不直接改库", before, after)
	}

	proposals, err := pg.ListStrategyProposals(db.StrategyProposalPending, 200)
	if err != nil {
		t.Fatalf("列提议: %v", err)
	}
	for _, p := range proposals {
		if p.StrategyID == strategyID {
			t.Fatalf("失败子任务产出了提议 %d：没有可复用的信号就不该提议", p.ID)
		}
	}
}

// TestStrategyReplayCorpusFrozenOnTaskTerminal 钉住「任务进入终态 ⇒ 冻结重放语料」。
//
// 场景：任务没有目标（goalless 分支）且没有在跑的意图 ⇒ 引擎把它收成 done。
// 断言：任务终态之后，strategy_replay_corpus 里有一条这个任务的语料。
//
// 接线被摘掉时：语料表里没有这条 ⇒ 断言挂。
func TestStrategyReplayCorpusFrozenOnTaskTerminal(t *testing.T) {
	s, pg := wiringServer(t)
	task := wiringTask(t, s, "任务终态冻结语料")
	taskID := wiringTaskID(t, task)
	strategyID, _ := wiringStrategy(t, pg, task, wiringSuffix())
	intentID := wiringSubTask(t, task, strategyID, "confirmed")

	// 先把归因跑起来（里程碑命中 ⇒ usage 有命中记录 ⇒ 语料里有里程碑）。
	wiringRunIntent(t, s, task, intentID)

	planner := agent.NewPlanner(wiringProvider{reply: "本轮无需规划"},
		"wiring-planner", t.TempDir(), nil, 10000, 1)
	planner.SetNonStreaming(func() bool { return true })
	s.engine.SetAuthoritativeAgentResolver(func(*Task) (*agent.Planner, *agent.Worker) {
		return planner, nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.engine.plannerLoop(ctx, task)
	task.Notify()

	// 等任务落终态 + 语料冻上。
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		corpus, err := pg.ListReplayCorpus(taskID)
		if err != nil {
			t.Fatalf("读重放语料: %v", err)
		}
		if len(corpus) > 0 {
			if len(corpus[0].MilestoneHits) == 0 {
				t.Fatalf("冻结的语料里没有里程碑：%+v", corpus[0])
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("任务终态之后没有冻结重放语料（任务 %d）", taskID)
}

// TestStrategyReplayCorpusFrozenOnTerminalViaWorkerLoop 钉住「任务终态兜底也会冻语料」。
//
// 场景：任务终态不是由引擎写的（模拟超时收尾那条路：终态由别处落库），
// 引擎只是在 worker 循环里观察到终态。
// 断言：语料照样被冻上。
//
// 这条兜底被摘掉时：超时收尾的任务永远没有语料 ⇒ 断言挂。
func TestStrategyReplayCorpusFrozenOnTerminalViaWorkerLoop(t *testing.T) {
	s, pg := wiringServer(t)
	task := wiringTask(t, s, "终态兜底冻结语料")
	taskID := wiringTaskID(t, task)
	strategyID, _ := wiringStrategy(t, pg, task, wiringSuffix())
	intentID := wiringSubTask(t, task, strategyID, "confirmed")
	wiringRunIntent(t, s, task, intentID)

	worker := agent.NewWorker(wiringProvider{reply: "本轮无可执行动作"},
		"wiring-worker", s.m.dir, nil, 10000, 1)
	worker.SetNonStreaming(func() bool { return true })
	s.engine.SetAuthoritativeAgentResolver(func(*Task) (*agent.Planner, *agent.Worker) {
		return nil, worker
	})

	// 终态由别处落库（不是引擎写的）⇒ 只能靠 worker 循环里的兜底观察。
	if err := s.m.SetTaskStatus(task.ID, "timeout"); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.engine.workerLoop(ctx, task, "worker-1")

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		corpus, err := pg.ListReplayCorpus(taskID)
		if err != nil {
			t.Fatalf("读重放语料: %v", err)
		}
		if len(corpus) > 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("任务终态之后没有冻结重放语料（任务 %d）", taskID)
}

// TestStrategySummaryInjectedIntoPlannerRound 钉住「策略摘要由宿主侧注入 planner」。
//
// 场景：planner 跑一轮规划（真引擎路径：plannerLoop → Plan）。
// 断言：本轮喂给模型的输入里有策略摘要，**没有**策略正文（steps）。
//
// 接线被摘掉时：输入里既没有摘要也没有正文 ⇒ 「有摘要」这条断言挂。
func TestStrategySummaryInjectedIntoPlannerRound(t *testing.T) {
	s, pg := wiringServer(t)
	task := wiringTask(t, s, "planner 轮次注入策略摘要")
	_, summary := wiringStrategy(t, pg, task, wiringSuffix())

	// 任务要有 open 目标，否则 plannerLoop 走 goalless 分支、根本不会规划。
	if _, err := task.Store.AddNode(db.KindGoal, map[string]any{"text": "拿下目标"},
		0, "open", "system", nil); err != nil {
		t.Fatalf("建目标: %v", err)
	}

	requests := make(chan llm.CompletionRequest, 8)
	planner := agent.NewPlanner(wiringProvider{requests: requests, reply: "本轮无可执行动作"},
		"wiring-planner", t.TempDir(), nil, 10000, 1)
	planner.SetNonStreaming(func() bool { return true })
	s.engine.SetAuthoritativeAgentResolver(func(*Task) (*agent.Planner, *agent.Worker) {
		return planner, nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.engine.plannerLoop(ctx, task)
	task.Notify()

	var req llm.CompletionRequest
	select {
	case req = <-requests:
	case <-time.After(30 * time.Second):
		t.Fatalf("planner 没有发起模型请求")
	}
	blob, err := json.Marshal(req.Messages)
	if err != nil {
		t.Fatalf("序列化请求: %v", err)
	}
	text := string(blob)
	if !strings.Contains(text, summary) {
		t.Fatalf("planner 的输入里没有策略摘要 %q", summary)
	}
	if strings.Contains(text, "WIRINGSTEPBODY") {
		t.Fatalf("planner 的输入里出现了策略正文（steps）：注入路径只能带摘要")
	}
}

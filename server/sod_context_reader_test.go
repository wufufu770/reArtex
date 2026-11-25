package server

// SEC-01（A-03 第一批，0.15.95）统一受管执行判据的分支测试。
//
// 覆盖卡面要求的六类场景：有效受管任务（worker 主张 + 意图↔任务绑定校验）、
// 缺失身份、伪造身份（不存在的意图 / 非 intent 节点）、跨任务身份、无任务合法
// 路径、依赖 nil（零值 Server）；外加四路判定一致（同一读法下 worker/planner/
// mainagent/chat 成员产出的受管元组口径统一）与取消后的 ctx 判定不变。

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/internal/sod"
)

// sodReaderServer 起一个带真实 pg 的 Server（判据校验要查 exploration_nodes↔tasks）。
func sodReaderServer(t *testing.T) (*db.DB, *Server) {
	t.Helper()
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	t.Cleanup(func() { m.Close() })
	if m.pg == nil {
		t.Skip("postgres unavailable — skipping sod reader test")
	}
	s := New(testServerCtx(t), m, t.TempDir(), t.TempDir(), t.TempDir())
	return m.pg, s
}

// sodReaderSeedTask 建 task 并返回 id（exploration 随建）。
func sodReaderSeedTask(t *testing.T, d *db.DB, tag string) int64 {
	t.Helper()
	task, err := d.CreateTaskWithOptions(fmt.Sprintf("sod-reader %s %d", tag, time.Now().UnixNano()), "判据测试目标", db_noTaskOpts())
	if err != nil {
		t.Fatalf("seed task: %v", err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(task.ID) })
	return task.ID
}

// sodReaderSeedNode 在任务的 exploration 下种一个指定 kind 的节点，返回节点 id。
func sodReaderSeedNode(t *testing.T, d *db.DB, taskID int64, kind string) int64 {
	t.Helper()
	var expID int64
	if err := d.QueryRow(`SELECT exploration_id FROM tasks WHERE id=$1`, taskID).Scan(&expID); err != nil {
		t.Fatalf("seed node: %v", err)
	}
	var nid int64
	if err := d.QueryRow(`INSERT INTO exploration_nodes (exploration_id, kind, payload) VALUES ($1,$2,'{"summary":"sod-reader 判据测试"}') RETURNING id`,
		expID, kind).Scan(&nid); err != nil {
		t.Fatalf("seed node: %v", err)
	}
	t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM exploration_nodes WHERE id=$1`, nid) })
	return nid
}

// TestSodReaderWorkerVerified：worker 主张（IntentID>0）在意图真实归属该任务时
// 继承既有语义，且与 Tier-0 读法（agent.SodContextReader）逐字节一致。
func TestSodReaderWorkerVerified(t *testing.T) {
	d, s := sodReaderServer(t)
	taskID := sodReaderSeedTask(t, d, "verified")
	nid := sodReaderSeedNode(t, d, taskID, "intent")

	ctx := agent.WithRunInfo(context.Background(), agent.RunInfo{TaskID: taskID, IntentID: nid})
	got := s.sodContextReader()(ctx)
	want := agent.SodContextReader(ctx)
	if got != want {
		t.Fatalf("worker 主张应与既有读法一致：got %+v want %+v", got, want)
	}
	if got.TaskID != taskID || got.AgentKey != fmt.Sprintf("worker#%d", nid) {
		t.Fatalf("worker 受管元组不符: %+v", got)
	}
	// 受管与否（ManagedScopeOf）= TaskID>0 且 AgentKey 非空——元组字段已断言。
	// 不用合成 ctx 验 ManagedScopeOf：它走 currentRunContext（绑定读法优先），
	// 合成 ctx 会绕过元组本身、读到空 RunInfo。
	if got.TaskID <= 0 || got.AgentKey == "" {
		t.Fatalf("worker 主张应受管: %+v", got)
	}
}

// TestSodReaderWorkerForged：伪造身份 fail-closed——不存在的意图、非 intent 节点、
// TaskID<=0 的主张，一律空上下文（gated 调用随后被链的 taskID<=0 分支拒绝）。
func TestSodReaderWorkerForged(t *testing.T) {
	d, s := sodReaderServer(t)
	taskID := sodReaderSeedTask(t, d, "forged")

	cases := map[string]agent.RunInfo{
		"意图不存在":     {TaskID: taskID, IntentID: 999999999},
		"非intent节点": {TaskID: taskID, IntentID: sodReaderSeedNode(t, d, taskID, "goal")},
		"缺任务号":      {TaskID: 0, IntentID: 42},
		"负任务号":      {TaskID: -7, IntentID: 42},
	}
	for name, ri := range cases {
		ctx := agent.WithRunInfo(context.Background(), ri)
		if got := (s.sodContextReader())(ctx); got != (sod.RunContext{}) {
			t.Fatalf("%s：应返回空上下文（fail-closed），got %+v", name, got)
		}
	}
}

// TestSodReaderCrossTask：意图真实存在但挂在别的任务名下 ⇒ 判据拒绝。
func TestSodReaderCrossTask(t *testing.T) {
	d, s := sodReaderServer(t)
	taskA := sodReaderSeedTask(t, d, "cross-a")
	taskB := sodReaderSeedTask(t, d, "cross-b")
	nidB := sodReaderSeedNode(t, d, taskB, "intent")

	ctx := agent.WithRunInfo(context.Background(), agent.RunInfo{TaskID: taskA, IntentID: nidB})
	if got := (s.sodContextReader())(ctx); got != (sod.RunContext{}) {
		t.Fatalf("跨任务主张应拒绝（空上下文），got %+v", got)
	}
	// 归属正确时同一条主张必须受管——证明拒绝来自绑定校验而不是节点本身。
	ctxOK := agent.WithRunInfo(context.Background(), agent.RunInfo{TaskID: taskB, IntentID: nidB})
	if got := (s.sodContextReader())(ctxOK); got.AgentKey == "" {
		t.Fatalf("同节点归属正确时应受管，got %+v", got)
	}
}

// TestSodReaderRolePaths：planner / mainagent / 群聊 chat 成员（带任务、无意图）
// 三路受管，身份取 ctx 运行 key——四路（含 worker）同一读法判定一致。
func TestSodReaderRolePaths(t *testing.T) {
	d, s := sodReaderServer(t)
	taskID := sodReaderSeedTask(t, d, "roles")

	for _, key := range []string{"planner", "mainagent", "chat"} {
		ctx := agent.WithAgentKey(agent.WithRunInfo(context.Background(),
			agent.RunInfo{TaskID: taskID}), key)
		got := (s.sodContextReader())(ctx)
		if got.TaskID != taskID || got.AgentKey != key {
			t.Fatalf("%s 受管元组不符: %+v", key, got)
		}
	}

	// 取消后的 ctx：判定只取决于挂载的 RunInfo/AgentKey（attach 时数据），
	// 不随调用期 ctx 状态漂移。
	cctx, cancel := context.WithCancel(context.Background())
	ckey := "planner"
	cctx = agent.WithAgentKey(agent.WithRunInfo(cctx, agent.RunInfo{TaskID: taskID}), ckey)
	cancel()
	if got := (s.sodContextReader())(cctx); got.TaskID != taskID || got.AgentKey != ckey {
		t.Fatalf("取消后的 ctx 判定应保持一致: %+v", got)
	}
}

// TestSodReaderNoTask：无任务合法路径显式不受管（空上下文；gated 调用由链的
// fail-closed 拒绝面接管，只读直通与 worker 同口径）。
func TestSodReaderNoTask(t *testing.T) {
	_, s := sodReaderServer(t)

	cases := map[string]context.Context{
		"无RunInfo": context.Background(),
		"纯chat会话":  agent.WithRunInfo(context.Background(), agent.RunInfo{SessionID: "conv-1"}),
		"带key无任务":  agent.WithAgentKey(context.Background(), "chat"),
	}
	for name, ctx := range cases {
		if got := (s.sodContextReader())(ctx); got != (sod.RunContext{}) {
			t.Fatalf("%s：无任务应显式空上下文，got %+v", name, got)
		}
	}
}

// TestSodReaderNilDeps：零值 Server（nil 依赖）不 fail-open——worker 主张无法
// 校验 ⇒ 空上下文；planner 主张不需要库 ⇒ 仍产出受管元组（授权判定由链的范围
// 检查兜底，链会因任务/范围不可用而拒绝）。
func TestSodReaderNilDeps(t *testing.T) {
	s := &Server{}
	ctxW := agent.WithRunInfo(context.Background(), agent.RunInfo{TaskID: 1, IntentID: 2})
	if got := (s.sodContextReader())(ctxW); got != (sod.RunContext{}) {
		t.Fatalf("nil 依赖下 worker 主张必须拒绝（空上下文），got %+v", got)
	}
	ctxP := agent.WithAgentKey(agent.WithRunInfo(context.Background(), agent.RunInfo{TaskID: 5}), "planner")
	got := (s.sodContextReader())(ctxP)
	if got.TaskID != 5 || got.AgentKey != "planner" {
		t.Fatalf("nil 依赖下 planner 主张的元组（不含库判定）应如实产出，got %+v", got)
	}
}

// mustManaged 已内联（ManagedScopeOf 吃 ctx——用 sod.WithRunContext 挂回去再验）。

// db_noTaskOpts 返回判据测试建任务用的最小选项（避免测试文件间共享可变全局）。
func db_noTaskOpts() db.TaskCreateOptions { return db.TaskCreateOptions{} }

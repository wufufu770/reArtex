package agent_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/server"
	actool "github.com/Autumn-27/norma/tool"
)

// 本文件是 Tier 1 范围隔离验收测试的【夹具】。
//
// 夹具铺出「调用者任务 + 互不相交的范围 + 各自名下的资产」，并把两个入口都装好：
// agent 侧的工具表（真实 ToolSet + 真实资产库）与 server 侧的工具表（含
// get_finding_traffic，它由 server 包注册）。验收逻辑在 tier1_scope_gate_test.go。

// tier1Fixture 是本文件的夹具：两个互不相干的任务，各自声明互不相交的范围，
// 每个任务名下各有一枚「范围内」与一枚「范围外」的资产。
//
// host 命名刻意互不为子串（t1in / t1out / t1gin / t1gout）：断言用 host 当唯一标记，
// 若标记互为子串，「范围内可见」会被「范围外可见」误判成真。
type tier1Fixture struct {
	manager *server.Manager
	db      *db.DB

	taskA int64 // 调用者任务：只声明 inHost / graphInHost 两个范围
	taskB int64 // 别人的任务：声明 outHost / graphOutHost
	expA  int64

	inHost   string
	outHost  string
	inAsset  int64
	outAsset int64

	graphInHost   string
	graphOutHost  string
	graphInAsset  int64
	graphOutAsset int64

	inFindingID  int64 // A 名下的漏洞（范围内）
	outFindingID int64 // B 名下的漏洞（范围外）

	agentTools  []actool.CoreTool // agent 包的工具表
	serverTools []actool.CoreTool // server 包注册的工具表（含 get_finding_traffic）
}

// newTier1Fixture 铺出夹具。
//
// 用 server.NewManager 而不是裸 db.Open：get_finding_traffic 的范围判定要经由
// server.Manager 解析任务（ResolveTask），而任务必须是「Manager 认得的任务」——
// 只有经 m.CreateTask 创建的任务才会进 Manager 的内存表。
func newTier1Fixture(t *testing.T) *tier1Fixture {
	t.Helper()

	m, err := server.NewManager(t.TempDir(), "")
	if err != nil {
		t.Fatalf("建平台管理器失败：%v", err)
	}
	t.Cleanup(func() { m.Close() })
	d := m.PG()

	taskA, err := m.CreateTask("tier1 调用者任务", "读面范围隔离验收", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	taskB, err := m.CreateTask("tier1 别人的任务", "另一个任务", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	taskAID, _ := strconv.ParseInt(taskA.ID, 10, 64)
	taskBID, _ := strconv.ParseInt(taskB.ID, 10, 64)
	t.Cleanup(func() {
		d.Exec(`DELETE FROM task_archives WHERE task_id=$1`, taskAID) //nolint:errcheck
		d.Exec(`DELETE FROM task_archives WHERE task_id=$1`, taskBID) //nolint:errcheck
		d.DeleteTask(taskAID)                                         //nolint:errcheck
		d.DeleteTask(taskBID)                                         //nolint:errcheck
	})

	stamp := time.Now().UnixNano()
	fx := &tier1Fixture{
		manager:      m,
		db:           d,
		taskA:        taskAID,
		taskB:        taskBID,
		expA:         taskA.ExpID,
		inHost:       fmt.Sprintf("t1in-%d.example.test", stamp),
		outHost:      fmt.Sprintf("t1out-%d.example.test", stamp),
		graphInHost:  fmt.Sprintf("t1gin-%d.example.test", stamp),
		graphOutHost: fmt.Sprintf("t1gout-%d.example.test", stamp),
	}

	// 声明范围：直接写 task_scope 行。
	//
	// 为什么不用 AddManualScope：它会把 root_domain 规范化成【可注册域】
	// （"a.example.test" → "example.test"），而夹具要的是「这个根域本身在范围内」，
	// 且必须与资产行的 root_domain 列逐字相等（范围 CTE 的匹配条件就是
	// a.root_domain = ts.domain）。直接写行与 db/scope_test.go、agent/worker_scope_test.go
	// 的夹具做法一致。
	for _, sc := range []struct {
		taskID int64
		domain string
	}{
		{taskAID, fx.inHost},
		{taskAID, fx.graphInHost},
		{taskBID, fx.outHost},
		{taskBID, fx.graphOutHost},
	} {
		if _, err := d.Exec(`INSERT INTO task_scope(task_id,kind,domain,source) VALUES ($1,'root_domain',$2,'manual')
ON CONFLICT DO NOTHING`, sc.taskID, sc.domain); err != nil {
			t.Fatalf("声明范围失败：%v", err)
		}
	}

	fx.inAsset = tier1InsertAsset(t, d, fx.inHost)
	fx.outAsset = tier1InsertAsset(t, d, fx.outHost)
	fx.graphInAsset = tier1InsertAsset(t, d, fx.graphInHost)
	fx.graphOutAsset = tier1InsertAsset(t, d, fx.graphOutHost)
	t.Cleanup(func() {
		d.Exec(`DELETE FROM assets WHERE id=ANY($1::bigint[])`, []int64{ //nolint:errcheck
			fx.inAsset, fx.outAsset, fx.graphInAsset, fx.graphOutAsset,
		})
	})

	// A 名下的漏洞（锚在范围内资产上）与 B 名下的漏洞（锚在 B 的范围内资产上）。
	// RecordFinding 走生产写路径：它的锚点写入前会判范围（越界即整单拒绝），
	// 所以 A 名下的漏洞确实锚在范围内资产上，B 名下的确实在 A 的范围之外。
	fx.inFindingID = tier1RecordFinding(t, d, taskAID, taskA.ExpID, "范围内漏洞 "+fx.inHost, fx.inAsset)
	fx.outFindingID = tier1RecordFinding(t, d, taskBID, taskB.ExpID, "范围外漏洞 "+fx.outHost, fx.outAsset)

	// agent 侧工具表：真实 ToolSet + 真实资产库，任务上下文指向 A。
	tools := agent.NewToolSet(d.Exploration(taskA.ExpID), "planner")
	tools.SetTaskID(taskAID)
	tools.SetAssetStore(d.Assets(), d.Companies())
	fx.agentTools = tools.AllDomainTools()

	// server 侧工具表：get_finding_traffic 由 server 包注册，只能经平台的工具装配
	// 拿到（agent.ToolAugment → 宿主工具 → 编排工具）。
	//
	// server.New 会改写 agent 包的几个全局钩子（ToolAugment / ToolResolve / Prompt*）：
	// 它们是进程级的，必须在用例结束时还原，否则会污染同包里的其它用例。
	oldAugment, oldResolve := agent.ToolAugment, agent.ToolResolve
	oldPrompt := agent.PromptOverride
	oldWrapup := agent.WrapupOverride
	oldWrapupTurns := agent.WrapupMaxTurnsOverride
	oldWrapupTimeoutTurns := agent.WrapupTaskTimeoutTurnsOverride
	t.Cleanup(func() {
		agent.ToolAugment, agent.ToolResolve = oldAugment, oldResolve
		agent.PromptOverride = oldPrompt
		agent.WrapupOverride = oldWrapup
		agent.WrapupMaxTurnsOverride = oldWrapupTurns
		agent.WrapupTaskTimeoutTurnsOverride = oldWrapupTimeoutTurns
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 服务用已取消的 ctx：只装配工具，不跑任何后台任务。
	server.New(ctx, m, t.TempDir(), t.TempDir(), t.TempDir())
	augmented, _, cleanup := agent.AugmentTools(context.Background(), "reporter", nil)
	t.Cleanup(cleanup)
	fx.serverTools = augmented

	return fx
}

// tier1InsertAsset 插一枚资产（root_domain 与 domain 同值）。
func tier1InsertAsset(t *testing.T, d *db.DB, domain string) int64 {
	t.Helper()
	var id int64
	if err := d.QueryRow(`INSERT INTO assets(type,domain,root_domain,last_seen)
VALUES ('subdomain',$1,$1,now()) RETURNING id`, domain).Scan(&id); err != nil {
		t.Fatalf("插资产 %s 失败：%v", domain, err)
	}
	return id
}

// tier1RecordFinding 走生产写路径登记一条漏洞（它会做锚点范围判定）。
func tier1RecordFinding(t *testing.T, d *db.DB, taskID, expID int64, summary string, assetID int64) int64 {
	t.Helper()
	rec, err := d.Exploration(expID).RecordFinding(context.Background(), db.RecordFindingInput{
		TaskID: taskID, ExplorationID: expID,
		VulnClass: "TEST", Name: "tier1 fixture", Severity: "low", Summary: summary,
		Worker: "test", AssetIDs: []int64{assetID},
	})
	if err != nil {
		t.Fatalf("登记漏洞失败：%v", err)
	}
	return rec.FindingID
}

// tier1Tool 按名字取工具。
func tier1Tool(t *testing.T, tools []actool.CoreTool, name string) actool.CoreTool {
	t.Helper()
	for _, tool := range tools {
		if tool.Name() == name {
			return tool
		}
	}
	t.Fatalf("工具表里没有 %s", name)
	return nil
}

// tier1Call 调用工具并返回展开后的输出（结果正文或错误文案都算）。
//
// 返回【原始文本】而不是解析后的结构：断言「错误文案里也不能泄露范围外的内容」
// 正是靠这条 —— 一旦只取成功分支的错误信息，那半条断言就是空的。
func tier1Call(t *testing.T, tool actool.CoreTool, ctx context.Context, input any) string {
	t.Helper()
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	res, err := tool.Call(ctx, raw, nil)
	if err != nil {
		t.Fatalf("%s 调用失败：%v", tool.Name(), err)
	}
	return res.Flatten()
}

package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
)

// 成员提案护栏的测试台。
//
// 一个大厅（一个 task + 一个群 + 两个成员），两个范围互不相干的任务各带一个资产：
//
//	inAsset  —— 在大厅任务的范围内
//	outAsset —— 只在别的任务的范围内（对大厅任务是范围外）
//
// 群成员用内置 agent key（agents 表有外键）：pentest / worker 是成员，
// redteam 故意不入群 ⇒ 拿"非成员"这条负例。
type proposeFixture struct {
	d        *db.DB
	task     *db.Task
	expID    int64
	groupID  int64
	inAsset  int64
	outAsset int64
}

func newProposeFixture(t *testing.T, d *db.DB) proposeFixture {
	t.Helper()

	task, err := d.CreateTask("propose guard", "goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	other, err := d.CreateTask("propose guard other", "goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(task.ID); _ = d.DeleteTask(other.ID) })

	stamp := time.Now().UnixNano()
	inHost := fmt.Sprintf("propose-in-%d.example.test", stamp)
	outHost := fmt.Sprintf("propose-out-%d.example.test", stamp)
	seedTaskScope(t, d, task.ID, "root_domain", inHost)
	seedTaskScope(t, d, other.ID, "root_domain", outHost)

	fx := proposeFixture{
		d:        d,
		task:     task,
		expID:    task.ExplorationID,
		inAsset:  insertScopeAsset(t, d, scopeAssetSpec{domain: inHost, rootDomain: inHost, ownerTaskID: task.ID}),
		outAsset: insertScopeAsset(t, d, scopeAssetSpec{domain: outHost, rootDomain: outHost, ownerTaskID: other.ID}),
	}
	t.Cleanup(func() {
		d.Exec(`DELETE FROM assets WHERE id=ANY($1::bigint[])`, []int64{fx.inAsset, fx.outAsset}) //nolint:errcheck
	})

	hall, err := d.CreateConversation("auto", "propose hall", nil)
	if err != nil {
		t.Fatal(err)
	}
	memberConv, err := d.CreateConversation("pentest", "propose member", nil)
	if err != nil {
		t.Fatal(err)
	}
	groupID, err := d.CreateChatGroup(&db.ChatGroup{
		ConversationID:   hall.ID,
		TaskID:           task.ID,
		Name:             "propose hall",
		Status:           "active",
		ConcurrencyLimit: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	fx.groupID = groupID
	for _, m := range []struct {
		key  string
		conv *int64
	}{{"pentest", &memberConv.ID}, {"worker", nil}} {
		if err := d.AddChatGroupMember(groupID, m.key, m.conv, "member"); err != nil {
			t.Fatal(err)
		}
	}
	return fx
}

// memberTools 造一个【群成员】的工具集：绑定任务与探索图，身份是群成员 agent key。
func (fx proposeFixture) memberTools(t *testing.T, agentKey string) *ToolSet {
	t.Helper()
	ts := NewToolSet(fx.d.Exploration(fx.expID), agentKey)
	ts.SetTaskID(fx.task.ID)
	ts.SetAssetStore(fx.d.Assets(), fx.d.Companies())
	ts.SetMemberAgentKey(agentKey)
	return ts
}

// callPropose 调一次 propose_intent，返回展开后的文本（错误文案也算）。
func callPropose(t *testing.T, ts *ToolSet, payload any) string {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	res, err := ts.ProposeIntentTool().Call(context.Background(), raw, nil)
	if err != nil {
		t.Fatalf("propose_intent 调用失败: %v", err)
	}
	return res.Flatten()
}

// openIntentCount 数这个探索图上的 open 意图条数。
func openIntentCount(t *testing.T, d *db.DB, expID int64) int {
	t.Helper()
	var n int
	if err := d.QueryRow(`SELECT count(*) FROM exploration_nodes
WHERE exploration_id=$1 AND kind='intent' AND state='open'`, expID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestProposeIntentRequiresTaskContext pins 闸门一：没有任务上下文就没有提案。
//
// 没有任务就没有群、没有范围、没有配额 —— 三者都无从判定 ⇒ 提案必须被拒，
// 且一个字节都不能写进探索图。
func TestProposeIntentRequiresTaskContext(t *testing.T) {
	d := testDB(t)
	defer d.Close()

	// 探索图不绑定任何任务 ⇒ 解析不出任务 id
	expID, err := d.CreateExploration("no task", "goal")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Exec(`DELETE FROM explorations WHERE id=$1`, expID) }) //nolint:errcheck

	ts := NewToolSet(d.Exploration(expID), "pentest")
	ts.SetMemberAgentKey("pentest")
	out := callPropose(t, ts, map[string]any{"summary": "没有任务上下文的提案"})

	if !strings.Contains(out, "任务上下文") {
		t.Fatalf("无任务上下文必须被拒，实际返回: %s", out)
	}
	if n := openIntentCount(t, d, expID); n != 0 {
		t.Fatalf("被拒的提案不得留下意图（实际 %d 条）", n)
	}
}

// TestProposeIntentWithoutToolSetRefuses pins 闸门一的另一半：连工具集都没有
// （没有任务绑定的探索图）时，提案工具必须给出可用的拒绝，而不是崩掉。
func TestProposeIntentWithoutToolSetRefuses(t *testing.T) {
	res, err := toolProposeIntent(nil).Call(context.Background(), json.RawMessage(`{"summary":"x"}`), nil)
	if err != nil {
		t.Fatalf("unexpected transport error: %v", err)
	}
	if !res.IsError || !strings.Contains(res.Flatten(), "任务上下文") {
		t.Fatalf("want an explanatory refusal, got IsError=%v %q", res.IsError, res.Flatten())
	}
}

// TestProposeIntentRejectsNonMember pins 闸门二：非群成员一律不得提案。
//
// 提案写的是共享探索图 —— 谁都能写就等于任何 agent 都能往规划者的 frontier 里
// 塞东西。
func TestProposeIntentRejectsNonMember(t *testing.T) {
	d := testDB(t)
	defer d.Close()

	fx := newProposeFixture(t, d)
	// redteam 是个存在的 agent，但不在这个大厅的名册里
	ts := fx.memberTools(t, "redteam")
	out := callPropose(t, ts, map[string]any{"summary": "非成员的提案"})

	if !strings.Contains(out, "成员") {
		t.Fatalf("非成员必须被拒，实际返回: %s", out)
	}
	if n := openIntentCount(t, d, fx.expID); n != 0 {
		t.Fatalf("被拒的提案不得留下意图（实际 %d 条）", n)
	}
}

// TestProposeIntentRejectsOutOfScopeAsset pins 闸门三：范围外的资产不得被提案锚上。
//
// 范围判定走平台唯一的共享判定（ScopedAssetIDs → ScopeFragmentFor）；越界即整单
// 拒绝 —— 范围外的资产 id 落进共享图，就是把别人的资产写进了本任务的图。
func TestProposeIntentRejectsOutOfScopeAsset(t *testing.T) {
	d := testDB(t)
	defer d.Close()

	fx := newProposeFixture(t, d)
	ts := fx.memberTools(t, "pentest")
	out := callPropose(t, ts, map[string]any{
		"summary":   "锚范围外资产的提案",
		"asset_ids": []any{fx.outAsset},
	})

	if !strings.Contains(out, "范围") {
		t.Fatalf("范围外资产必须被拒，实际返回: %s", out)
	}
	if n := openIntentCount(t, d, fx.expID); n != 0 {
		t.Fatalf("被拒的提案不得留下意图（实际 %d 条）", n)
	}
	var anchors int
	if err := d.QueryRow(`SELECT count(*) FROM exploration_anchors WHERE asset_id=$1`, fx.outAsset).Scan(&anchors); err != nil {
		t.Fatal(err)
	}
	if anchors != 0 {
		t.Fatalf("范围外资产不得留下锚点（实际 %d 条）", anchors)
	}
}

// TestProposeIntentKeepsInScopeAsset pins 闸门三的正例：范围内的资产照常锚上。
//
// 闸门不能把成员的手脚一起绑住 —— 范围内的目标正是提案该指的地方。
func TestProposeIntentKeepsInScopeAsset(t *testing.T) {
	d := testDB(t)
	defer d.Close()

	fx := newProposeFixture(t, d)
	ts := fx.memberTools(t, "pentest")
	out := callPropose(t, ts, map[string]any{
		"summary":   "锚范围内资产的提案",
		"asset_ids": []any{fx.inAsset},
	})
	if strings.Contains(out, "范围") {
		t.Fatalf("范围内资产不该被拒，实际返回: %s", out)
	}
	var anchors int
	if err := d.QueryRow(`SELECT count(*) FROM exploration_anchors WHERE asset_id=$1`, fx.inAsset).Scan(&anchors); err != nil {
		t.Fatal(err)
	}
	if anchors != 1 {
		t.Fatalf("范围内资产应当恰好一条锚点（实际 %d 条）", anchors)
	}
}

// TestProposeIntentDeduplicatesNormalizedSummary pins 闸门四：同一个目标不重复提。
//
// 两个成员各自注意到同一个线索是自治变成重复劳动的最常见方式。判重比的是
// 【归一化后】的摘要：大小写与空白不同的写法必须命中同一条意图。
func TestProposeIntentDeduplicatesNormalizedSummary(t *testing.T) {
	d := testDB(t)
	defer d.Close()

	fx := newProposeFixture(t, d)

	first := callPropose(t, fx.memberTools(t, "pentest"), map[string]any{
		"summary": "Test  SQL Injection  in /api/user",
	})
	if strings.Contains(first, "未重复创建") {
		t.Fatalf("第一条提案不该被判重: %s", first)
	}
	// 另一个成员用不同的大小写与空白提同一条线索
	second := callPropose(t, fx.memberTools(t, "worker"), map[string]any{
		"summary": "test sql injection in /api/user",
	})
	if !strings.Contains(second, "未重复创建") {
		t.Fatalf("归一化后相同的摘要必须被判重，实际返回: %s", second)
	}
	if n := openIntentCount(t, d, fx.expID); n != 1 {
		t.Fatalf("判重之后只应有一条 open 意图（实际 %d 条）", n)
	}
}

// TestProposeIntentEnforcesMemberQuota pins 闸门五：每个成员的在途意图有上限。
//
// 没有配额，一个不停发现线索的成员会把 frontier 淹掉，规划者的主路线被饿死。
// 分母是该成员自己名下的 open 意图 —— 别的成员的不算。
func TestProposeIntentEnforcesMemberQuota(t *testing.T) {
	d := testDB(t)
	defer d.Close()

	fx := newProposeFixture(t, d)
	ts := fx.memberTools(t, "pentest")
	for i := 0; i < memberIntentQuota; i++ {
		out := callPropose(t, ts, map[string]any{"summary": fmt.Sprintf("提案 %d", i)})
		if strings.Contains(out, "上限") {
			t.Fatalf("第 %d 条提案不该触顶: %s", i+1, out)
		}
	}
	out := callPropose(t, ts, map[string]any{"summary": "触顶的那条提案"})
	if !strings.Contains(out, "上限") {
		t.Fatalf("在途意图达上限后必须被拒，实际返回: %s", out)
	}
	if n := openIntentCount(t, d, fx.expID); n != memberIntentQuota {
		t.Fatalf("被拒的提案不得留下意图（实际 %d 条，上限 %d）", n, memberIntentQuota)
	}

	// 别的成员有自己的一份配额：pentest 触顶不影响 worker
	out = callPropose(t, fx.memberTools(t, "worker"), map[string]any{"summary": "worker 的提案"})
	if strings.Contains(out, "上限") {
		t.Fatalf("配额必须按成员各自算: %s", out)
	}
}

// TestProposeIntentDeniedWhenGroupQuotaStateDeniesNewCalls pins 闸门五的群组侧：
// 群组额度进入 draining/parked ⇒ 整组不再开新活，提案也一样。
func TestProposeIntentDeniedWhenGroupQuotaStateDeniesNewCalls(t *testing.T) {
	d := testDB(t)
	defer d.Close()

	fx := newProposeFixture(t, d)
	if err := d.SetGroupQuotaBudget(fx.groupID, 100000); err != nil {
		t.Fatal(err)
	}
	if err := d.SetGroupQuotaState(fx.groupID, db.QuotaStateDraining); err != nil {
		t.Fatal(err)
	}
	out := callPropose(t, fx.memberTools(t, "pentest"), map[string]any{"summary": "收尾期的提案"})
	if !strings.Contains(out, "额度") {
		t.Fatalf("群组额度状态不允许时必须被拒，实际返回: %s", out)
	}
	if n := openIntentCount(t, d, fx.expID); n != 0 {
		t.Fatalf("被拒的提案不得留下意图（实际 %d 条）", n)
	}
}

// TestProposeIntentWritesAdvisoryMemberIntent pins 提案的写面：origin='member'、
// state='open'、owner 为空、创建者记在 payload['created_by']。
//
// owner 为空是硬要求：提案只是【提议】，不转移归属（归属的权威只有 ClaimIntent
// 写的 exploration_nodes.owner）。
func TestProposeIntentWritesAdvisoryMemberIntent(t *testing.T) {
	d := testDB(t)
	defer d.Close()

	fx := newProposeFixture(t, d)
	out := callPropose(t, fx.memberTools(t, "pentest"), map[string]any{"summary": "成员的线索"})
	if strings.Contains(out, "失败") {
		t.Fatalf("提案不该失败: %s", out)
	}

	node, err := d.Exploration(fx.expID).GetNodeWithSourcesVisible(fx.task.ID, latestIntentID(t, d, fx.expID))
	if err != nil {
		t.Fatal(err)
	}
	if node.Origin != "member" {
		t.Fatalf("origin = %q, want member", node.Origin)
	}
	if node.State != "open" {
		t.Fatalf("state = %q, want open（提案不触发任何状态迁移）", node.State)
	}
	if node.Owner != "" {
		t.Fatalf("owner = %q, want 空（提案不转移归属）", node.Owner)
	}
	var payload map[string]any
	if err := json.Unmarshal(node.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["created_by"] != "pentest" {
		t.Fatalf("payload['created_by'] = %v, want pentest", payload["created_by"])
	}
}

// latestIntentID 取这个探索图上最新的一条意图节点 id。
func latestIntentID(t *testing.T, d *db.DB, expID int64) int64 {
	t.Helper()
	var id int64
	if err := d.QueryRow(`SELECT id FROM exploration_nodes
WHERE exploration_id=$1 AND kind='intent' ORDER BY id DESC LIMIT 1`, expID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// TestAddIntentUnaffectedByProposalPath pins 提案路径没有改动 add_intent。
//
// add_intent 仍是规划者/主 agent 的直接写入口：origin='planner'、不记创建者、
// 归属照旧由认领决定。自治能力只能【新增】，不能顺手改既有工具的语义。
func TestAddIntentUnaffectedByProposalPath(t *testing.T) {
	d := testDB(t)
	defer d.Close()

	fx := newProposeFixture(t, d)
	ts := fx.memberTools(t, "pentest")
	raw, err := json.Marshal(map[string]any{"summary": "规划者的方向"})
	if err != nil {
		t.Fatal(err)
	}
	res, err := ts.addIntent().Call(context.Background(), raw, nil)
	if err != nil {
		t.Fatalf("add_intent 调用失败: %v", err)
	}
	if res.IsError {
		t.Fatalf("add_intent 不该报错: %s", res.Flatten())
	}
	node, err := d.Exploration(fx.expID).GetNodeWithSourcesVisible(fx.task.ID, latestIntentID(t, d, fx.expID))
	if err != nil {
		t.Fatal(err)
	}
	if node.Origin != "planner" {
		t.Fatalf("origin = %q, want planner", node.Origin)
	}
	var payload map[string]any
	if err := json.Unmarshal(node.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if _, ok := payload["created_by"]; ok {
		t.Fatalf("add_intent 不得写 created_by: %v", payload)
	}
}

// TestProposeIntentRegisteredInToolSurface pins 提案工具进了工具面。
//
// 注册是它能被 tools 表绑定到群成员 agent key 的前提（目录从工具面展开）。
func TestProposeIntentRegisteredInToolSurface(t *testing.T) {
	found := false
	for _, tool := range NewToolSet(nil, "").AllDomainTools() {
		if tool.Name() == "propose_intent" {
			found = true
		}
	}
	if !found {
		t.Fatal("propose_intent 不在工具面里")
	}
}

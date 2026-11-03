package db

import (
	"database/sql"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"
)

// finding 验证态 + 返工边的验收用例。
//
// 这些用例钉住的是**承诺**：
//   - 幂等 DDL（重复应用不报错，且不动别的 kind 的约束取值）；
//   - 验证者 ≠ 提出者（自证被确定性层拒绝）；
//   - confirmed / dismissed 会**结案**（状态离开 pending）；
//   - rework **不结案**（状态留在 pending）+ 理由入库 + 恰好一条返工边；
//   - 已结案的结论不能被重新打开；
//   - finding 节点**落库即 pending**（「提出」不等于「验证」）。

// findingValidationTestBaseDSN 记住进程启动时的 ARTEX_PG_DSN（包初始化时读一次）。
var findingValidationTestBaseDSN = os.Getenv("ARTEX_PG_DSN")

// findingValidationTestDB 是 internal/testdb.Setup("db") 派生的库名。
//
// 命名规则在 internal/testdb/testdb.go（"artex_test_" + 包名）。这里只用它来
// **重建连接串**，不重建库 —— 重建会把本包自己的库删掉。
const findingValidationTestDB = "artex_test_db"

// findingValidationTestDSN 取测试库连接串。
//
// ⚠️ 为什么不用 testDSN：db/pgdsn_env_test.go 的用例会清空 DSN 缓存并把环境变量
// 还原成空，而本文件按文件名排在它后面 ⇒ testDSN 拿不到连接串，用例会被静默跳过。
// 这里按 testdb 的派生名自己重建连接串，并 ping 确认库确实在。
//
// ⚠️ 为什么**不**在用例里调 testdb.Setup("db")：派生库名是固定的
// （artex_test_db），重建会把这个包里其他用例正在用的库删掉。
//
// ⚠️ 为什么不动环境变量：db.DSN() 读入即从进程环境抹掉 DSN（防 agent 敲 env 读出
// 数据库口令），本文件直接用重建出的连接串，不走 DSN()。
func findingValidationTestDSN(t *testing.T) string {
	t.Helper()
	if findingValidationTestBaseDSN == "" {
		t.Skip("没有数据库配置（ARTEX_PG_DSN 未设置）—— 跳过")
	}
	u, err := url.Parse(findingValidationTestBaseDSN)
	if err != nil {
		t.Skipf("数据库配置无法解析（%v）—— 跳过", err)
	}
	u.Path = "/" + findingValidationTestDB
	conn, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Skipf("派生测试库不可用（%v）—— 跳过", err)
	}
	perr := conn.Ping()
	_ = conn.Close()
	if perr != nil {
		t.Skipf("派生测试库不可用（%v）—— 跳过", perr)
	}
	return u.String()
}

// findingValidationOpen 打开测试库并确保本单元的 DDL 已应用。
func findingValidationOpen(t *testing.T) *DB {
	t.Helper()
	d, err := Open(findingValidationTestDSN(t))
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if err := d.EnsureFindingValidationSchema(); err != nil {
		t.Fatalf("应用 finding 验证 schema 失败: %v", err)
	}
	return d
}

// findingValidationFixture 是一条可验证的 finding：产出它的意图 + 提出者 worker-1。
type findingValidationFixture struct {
	expID    int64
	intentID int64
	nodeID   int64
}

// newFindingValidationFixture 建一条 finding（提出者 worker-1，验证者另找他人）。
func newFindingValidationFixture(t *testing.T, d *DB) findingValidationFixture {
	t.Helper()
	task, err := d.CreateTask("验证 finding 验证态", "目标", nil, 0, 0)
	if err != nil {
		t.Fatalf("建任务失败: %v", err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(task.ID) })
	store := d.Exploration(task.ExplorationID)
	intentID, err := store.AddIntent(map[string]any{"summary": "产出结论的意图"}, 5, nil, "planner")
	if err != nil {
		t.Fatalf("建意图失败: %v", err)
	}
	rec, err := store.RecordFinding(t.Context(), RecordFindingInput{
		TaskID:        task.ID,
		ExplorationID: task.ExplorationID,
		IntentID:      intentID,
		VulnClass:     "sqli",
		Name:          "验证态测试漏洞",
		Severity:      "high",
		Summary:       "摘要",
		Evidence:      "证据",
		Worker:        "worker-1",
	})
	if err != nil {
		t.Fatalf("登记 finding 失败: %v", err)
	}
	return findingValidationFixture{expID: task.ExplorationID, intentID: intentID, nodeID: rec.NodeID}
}

// findingValidationNodeState 读 finding 节点的验证态。
func findingValidationNodeState(t *testing.T, d *DB, nodeID int64) string {
	t.Helper()
	var state string
	if err := d.QueryRow(`SELECT state FROM exploration_nodes WHERE id=$1`, nodeID).Scan(&state); err != nil {
		t.Fatalf("读节点状态失败: %v", err)
	}
	return state
}

// findingValidationBlockedReason 读 finding 节点上记的理由。
func findingValidationBlockedReason(t *testing.T, d *DB, nodeID int64) string {
	t.Helper()
	var reason string
	if err := d.QueryRow(`SELECT COALESCE(blocked_reason,'') FROM exploration_nodes WHERE id=$1`, nodeID).Scan(&reason); err != nil {
		t.Fatalf("读节点理由失败: %v", err)
	}
	return reason
}

// findingValidationReworkEdges 数一条 finding 节点上的返工边。
func findingValidationReworkEdges(t *testing.T, d *DB, expID, srcID int64) int {
	t.Helper()
	var n int
	if err := d.QueryRow(`SELECT count(*) FROM exploration_edges
 WHERE exploration_id=$1 AND src_id=$2 AND rel=$3`, expID, srcID, RelRework).Scan(&n); err != nil {
		t.Fatalf("数返工边失败: %v", err)
	}
	return n
}

// TestEnsureFindingValidationSchemaIsIdempotent 断言本单元的 DDL 幂等，且两个约束
// 真的落到了目标取值上（finding 允许 pending、边允许 rework）。
func TestEnsureFindingValidationSchemaIsIdempotent(t *testing.T) {
	d := findingValidationOpen(t)

	// EnsureFindingValidationSchema 的第二次调用走 sync.Map 快路径、碰不到 SQL ⇒
	// 这里显式再应用一遍 DDL，验证的是 SQL 本身的幂等性。
	for i := 0; i < 2; i++ {
		if _, err := d.Exec(findingValidationSchemaSQL); err != nil {
			t.Fatalf("重复应用 finding 验证 DDL 失败: %v", err)
		}
	}
	var nodeState, edgeRel string
	if err := d.QueryRow(`SELECT pg_get_constraintdef(oid) FROM pg_constraint
 WHERE conname='ck_node_state' AND conrelid='exploration_nodes'::regclass`).Scan(&nodeState); err != nil {
		t.Fatalf("读 ck_node_state 失败: %v", err)
	}
	if !strings.Contains(nodeState, "pending") {
		t.Fatalf("ck_node_state 未放行 finding 的 pending 态: %s", nodeState)
	}
	if err := d.QueryRow(`SELECT pg_get_constraintdef(oid) FROM pg_constraint
 WHERE conname='ck_edge_rel' AND conrelid='exploration_edges'::regclass`).Scan(&edgeRel); err != nil {
		t.Fatalf("读 ck_edge_rel 失败: %v", err)
	}
	if !strings.Contains(edgeRel, "rework") {
		t.Fatalf("ck_edge_rel 未放行 rework 边: %s", edgeRel)
	}
}

// TestRecordFindingBirthsPendingFinding 断言「提出」不等于「验证」：finding 节点
// 落库时就是 pending，而不是 confirmed。
func TestRecordFindingBirthsPendingFinding(t *testing.T) {
	d := findingValidationOpen(t)
	fx := newFindingValidationFixture(t, d)

	if state := findingValidationNodeState(t, d, fx.nodeID); state != FindingValidationStatePending {
		t.Fatalf("finding 落库时应为 pending，实际 %q", state)
	}
}

// TestFindingValidationRejectsSelfVerification 断言「提出人不得给自己的结论盖章」。
//
// 这是把「复核必须由复核者做」从注释变成确定性约束的地方：以前 finding 落库即
// confirmed，谁提的都算验证过；现在提出者自己来验证必须被拒，且错误要可区分（否则
// 调用方会当成「节点不存在」去重试）。
func TestFindingValidationRejectsSelfVerification(t *testing.T) {
	d := findingValidationOpen(t)
	fx := newFindingValidationFixture(t, d)

	err := d.Exploration(fx.expID).ValidateFinding(FindingValidationInput{
		NodeID:   fx.nodeID,
		Verdict:  FindingValidationConfirmed,
		Verifier: "worker-1", // == 提出者
	})
	if !errors.Is(err, ErrFindingValidationSelfVerification) {
		t.Fatalf("提出人自证应被拒，实际 err=%v", err)
	}
	// 拒绝后状态必须仍是 pending（不能被半写坏）。
	if state := findingValidationNodeState(t, d, fx.nodeID); state != FindingValidationStatePending {
		t.Fatalf("自证被拒后状态应仍为 pending，实际 %q", state)
	}
}

// TestFindingValidationConfirmedClosesFinding 断言验证通过会结案（状态转 confirmed）。
func TestFindingValidationConfirmedClosesFinding(t *testing.T) {
	d := findingValidationOpen(t)
	fx := newFindingValidationFixture(t, d)

	err := d.Exploration(fx.expID).ValidateFinding(FindingValidationInput{
		NodeID:   fx.nodeID,
		Verdict:  FindingValidationConfirmed,
		Verifier: "reviewer-1",
	})
	if err != nil {
		t.Fatalf("验证失败: %v", err)
	}
	if state := findingValidationNodeState(t, d, fx.nodeID); state != FindingValidationStateConfirmed {
		t.Fatalf("确认后状态应为 confirmed，实际 %q", state)
	}
}

// TestFindingValidationDismissedRequiresReason 断言「不成立」必须给理由。
func TestFindingValidationDismissedRequiresReason(t *testing.T) {
	d := findingValidationOpen(t)
	fx := newFindingValidationFixture(t, d)

	err := d.Exploration(fx.expID).ValidateFinding(FindingValidationInput{
		NodeID:   fx.nodeID,
		Verdict:  FindingValidationDismissed,
		Verifier: "reviewer-1",
	})
	if !errors.Is(err, ErrFindingValidationReasonRequired) {
		t.Fatalf("缺理由的打回应被拒，实际 err=%v", err)
	}
	if state := findingValidationNodeState(t, d, fx.nodeID); state != FindingValidationStatePending {
		t.Fatalf("被拒后状态应仍为 pending，实际 %q", state)
	}
}

// TestFindingValidationReworkRequiresTarget 断言返工必须指明打回给谁。
func TestFindingValidationReworkRequiresTarget(t *testing.T) {
	d := findingValidationOpen(t)
	fx := newFindingValidationFixture(t, d)

	err := d.Exploration(fx.expID).ValidateFinding(FindingValidationInput{
		NodeID:   fx.nodeID,
		Verdict:  FindingValidationRework,
		Verifier: "reviewer-1",
		Reason:   "证据不足",
	})
	if !errors.Is(err, ErrFindingValidationReworkTarget) {
		t.Fatalf("没有返工目标的打回应被拒，实际 err=%v", err)
	}
	if n := findingValidationReworkEdges(t, d, fx.expID, fx.nodeID); n != 0 {
		t.Fatalf("被拒的打回不得留下返工边（实际 %d 条）", n)
	}
}

// TestFindingValidationReworkKeepsPendingAndRecordsEdge 断言返工**不结案**：状态留在
// pending、理由入库、且恰好一条返工边（finding --rework--> 产出它的意图）。
func TestFindingValidationReworkKeepsPendingAndRecordsEdge(t *testing.T) {
	d := findingValidationOpen(t)
	fx := newFindingValidationFixture(t, d)

	err := d.Exploration(fx.expID).ValidateFinding(FindingValidationInput{
		NodeID:       fx.nodeID,
		Verdict:      FindingValidationRework,
		Verifier:     "reviewer-1",
		Reason:       "证据不足，需补充请求",
		ReworkTarget: fx.intentID,
	})
	if err != nil {
		t.Fatalf("打回失败: %v", err)
	}
	if state := findingValidationNodeState(t, d, fx.nodeID); state != FindingValidationStatePending {
		t.Fatalf("打回后状态应仍为 pending，实际 %q", state)
	}
	if reason := findingValidationBlockedReason(t, d, fx.nodeID); reason != "证据不足，需补充请求" {
		t.Fatalf("打回理由未入库，实际 %q", reason)
	}
	if n := findingValidationReworkEdges(t, d, fx.expID, fx.nodeID); n != 1 {
		t.Fatalf("打回应恰好落一条返工边，实际 %d 条", n)
	}
	var dst int64
	if err := d.QueryRow(`SELECT dst_id FROM exploration_edges
 WHERE exploration_id=$1 AND src_id=$2 AND rel=$3`, fx.expID, fx.nodeID, RelRework).Scan(&dst); err != nil {
		t.Fatalf("读返工边失败: %v", err)
	}
	if dst != fx.intentID {
		t.Fatalf("返工边应指向产出它的意图（%d），实际 %d", fx.intentID, dst)
	}
}

// TestFindingValidationRejectsSecondValidation 断言已结案的结论不能被重新打开。
func TestFindingValidationRejectsSecondValidation(t *testing.T) {
	d := findingValidationOpen(t)
	fx := newFindingValidationFixture(t, d)

	if err := d.Exploration(fx.expID).ValidateFinding(FindingValidationInput{
		NodeID:   fx.nodeID,
		Verdict:  FindingValidationConfirmed,
		Verifier: "reviewer-1",
	}); err != nil {
		t.Fatalf("首次验证失败: %v", err)
	}
	err := d.Exploration(fx.expID).ValidateFinding(FindingValidationInput{
		NodeID:   fx.nodeID,
		Verdict:  FindingValidationDismissed,
		Verifier: "reviewer-2",
		Reason:   "想反悔",
	})
	if !errors.Is(err, ErrFindingValidationNotPending) {
		t.Fatalf("已结案的 finding 不应能被再次验证，实际 err=%v", err)
	}
	if state := findingValidationNodeState(t, d, fx.nodeID); state != FindingValidationStateConfirmed {
		t.Fatalf("被拒后状态应仍为 confirmed，实际 %q", state)
	}
}

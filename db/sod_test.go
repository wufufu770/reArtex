package db

import (
	"database/sql"
	"encoding/json"
	"net/url"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/Autumn-27/artex/internal/sodpolicy"
)

// SoD 访问层的验收用例。
//
// 这些用例钉住的是**承诺**，不是实现细节：
//   - 幂等 DDL（重复应用不报错、不改结构）；
//   - 决策账本写得进、读得出、字段（含 checks 的 JSON）不漂移；
//   - needs_review 队列有生命周期（入队 → 待处理 → 结案 → 不再出现）；
//   - 熔断按代码常量开路，成功后计数清零，且过期的开路不会把工具永久拉黑；
//   - findings.replay_evidence_id 存在且默认空。
//
// 顺带钉住一条**跨包**约束：代码常量（internal/sodpolicy）里的裁决三态与
// 熔断三态，必须是 DDL 的 CHECK 认得的取值 —— 两侧漂移一次，运行期就炸。

// sodTestBaseDSN 记住进程启动时的 ARTEX_PG_DSN（包初始化时读一次）。
var sodTestBaseDSN = os.Getenv("ARTEX_PG_DSN")

// sodTestDerivedDB 是 internal/testdb.Setup("db") 派生的库名。
//
// 命名规则在 internal/testdb/testdb.go（"artex_test_" + 包名）。这里只用它来
// **重建连接串**，不重建库 —— 重建会把本包自己的库删掉。
const sodTestDerivedDB = "artex_test_db"

// sodTestDSN 取测试库连接串。
//
// ⚠️ 为什么不用 testDSN：db/pgdsn_env_test.go 的用例会清空 DSN 缓存并把环境
// 变量还原成空，而本文件按文件名排在它后面 ⇒ testDSN 拿不到连接串，用例会被
// 静默跳过。这里按 testdb 的派生名自己重建连接串，并 ping 确认库确实在。
//
// ⚠️ 为什么**不**在用例里调 testdb.Setup("db")：派生库名是固定的
// （artex_test_db），重建会把这个包里其他用例正在用的库删掉。
//
// ⚠️ 为什么不动环境变量：db.DSN() 读入即从进程环境抹掉 DSN（防 agent 敲
// env 读出数据库口令），本文件直接用重建出的连接串，不走 DSN()。
func sodTestDSN(t *testing.T) string {
	t.Helper()
	if sodTestBaseDSN == "" {
		t.Skip("没有数据库配置（ARTEX_PG_DSN 未设置）—— 跳过")
	}
	u, err := url.Parse(sodTestBaseDSN)
	if err != nil {
		t.Skipf("数据库配置无法解析（%v）—— 跳过", err)
	}
	u.Path = "/" + sodTestDerivedDB
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

// sodOpen 打开测试库并确保 SoD DDL 已应用。
func sodOpen(t *testing.T) *DB {
	t.Helper()
	d, err := Open(sodTestDSN(t))
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if err := d.EnsureSoDSchema(); err != nil {
		t.Fatalf("应用 SoD schema 失败: %v", err)
	}
	return d
}

// sodTestTask 建一个任务（决策账本的 task_id 外键指向它）。
func sodTestTask(t *testing.T, d *DB, name string) int64 {
	t.Helper()
	task, err := d.CreateTask(name, "验证 SoD 访问层", nil, 0, 0)
	if err != nil {
		t.Fatalf("建任务失败: %v", err)
	}
	return task.ID
}

// TestEnsureSoDSchemaIsIdempotent 断言 SoD 的 DDL 幂等。
func TestEnsureSoDSchemaIsIdempotent(t *testing.T) {
	d := sodOpen(t)

	// EnsureSoDSchema 的第二次调用走 sync.Map 快路径、碰不到 SQL ⇒ 这里
	// 显式再应用一遍 DDL，验证的是 SQL 本身的幂等性。
	if _, err := d.Exec(sodSchemaSQL); err != nil {
		t.Fatalf("重复应用 SoD DDL 失败: %v", err)
	}
	for _, q := range []string{
		`SELECT to_regclass('public.sod_decisions')`,
		`SELECT to_regclass('public.sod_adversarial_review_pending')`,
		`SELECT to_regclass('public.sod_tool_circuit')`,
	} {
		var name *string
		if err := d.QueryRow(q).Scan(&name); err != nil {
			t.Fatalf("query %s: %v", q, err)
		}
		if name == nil {
			t.Fatalf("%s 不存在", q)
		}
	}
}

// TestSoDDecisionRoundTrip 断言决策账本写得进、读得出、字段不漂移。
func TestSoDDecisionRoundTrip(t *testing.T) {
	d := sodOpen(t)
	taskID := sodTestTask(t, d, "SoD 决策账本")

	checks := json.RawMessage(`{"scope":"ok","quota":"ok","tool_health":"ok"}`)
	rec := &SoDDecision{
		TaskID:         &taskID,
		SourceAgent:    "worker",
		ToolName:       "Bash",
		Verdict:        sodpolicy.VerdictApprove,
		ReasonCode:     sodpolicy.ReasonAllChecksPassed,
		PayloadDigest:  "payload-digest-1",
		PayloadPreview: "«redacted»",
		IntentDigest:   "intent-digest-1",
		AnomalyFlag:    false,
		AnomalyReason:  "",
		Checks:         checks,
		TokenDigest:    "token-digest-1",
		ScopeDigest:    "scope-digest-1",
		CacheHit:       true,
		LatencyMs:      42,
	}
	id, err := d.RecordSoDDecision(rec)
	if err != nil {
		t.Fatalf("写 SoD 决策失败: %v", err)
	}
	if id <= 0 {
		t.Fatalf("写 SoD 决策没拿到 id: %d", id)
	}

	rows, err := d.ListSoDDecisions(taskID, 50)
	if err != nil {
		t.Fatalf("列 SoD 决策失败: %v", err)
	}
	var got *SoDDecision
	for _, row := range rows {
		if row.ID == id {
			got = row
			break
		}
	}
	if got == nil {
		t.Fatalf("决策 %d 不在账本里", id)
	}
	if got.TaskID == nil || *got.TaskID != taskID {
		t.Fatalf("task_id 不一致: %v", got.TaskID)
	}
	if got.ConversationID != nil {
		t.Fatalf("conversation_id 应为 NULL: %v", *got.ConversationID)
	}
	if got.SourceAgent != rec.SourceAgent || got.ToolName != rec.ToolName {
		t.Fatalf("actor/tool 不一致: %q/%q", got.SourceAgent, got.ToolName)
	}
	if got.Verdict != sodpolicy.VerdictApprove || got.ReasonCode != sodpolicy.ReasonAllChecksPassed {
		t.Fatalf("裁决/原因码不一致: %q/%q", got.Verdict, got.ReasonCode)
	}
	if got.PayloadDigest != rec.PayloadDigest || got.PayloadPreview != rec.PayloadPreview {
		t.Fatalf("载荷摘要/预览不一致: %q/%q", got.PayloadDigest, got.PayloadPreview)
	}
	if got.IntentDigest != rec.IntentDigest || got.TokenDigest != rec.TokenDigest {
		t.Fatalf("意图摘要/凭证摘要不一致: %q/%q", got.IntentDigest, got.TokenDigest)
	}
	if got.ScopeDigest != rec.ScopeDigest || got.CacheHit != rec.CacheHit || got.LatencyMs != rec.LatencyMs {
		t.Fatalf("scope/缓存/延迟不一致: %q/%v/%d", got.ScopeDigest, got.CacheHit, got.LatencyMs)
	}
	if got.CreatedAt.IsZero() {
		t.Fatal("created_at 没写进去")
	}
	// checks 是 JSONB：落库后键序与空白会被 PG 规范化 ⇒ 比语义，不比字节。
	wantChecks := map[string]any{}
	gotChecks := map[string]any{}
	if err := json.Unmarshal(rec.Checks, &wantChecks); err != nil {
		t.Fatalf("解析期望的 checks 失败: %v", err)
	}
	if err := json.Unmarshal(got.Checks, &gotChecks); err != nil {
		t.Fatalf("解析读回的 checks 失败: %v", err)
	}
	if !reflect.DeepEqual(wantChecks, gotChecks) {
		t.Fatalf("checks 不一致: 期望 %v，读回 %v", wantChecks, gotChecks)
	}
}

// TestSoDDecisionVerdictCheckMatchesConstants 断言 DDL 的 verdict CHECK 认得
// 代码常量里的裁决三态，且**不认得**第四种 —— 两边漂移一次，运行期写入就炸。
func TestSoDDecisionVerdictCheckMatchesConstants(t *testing.T) {
	d := sodOpen(t)
	for _, verdict := range []string{
		sodpolicy.VerdictApprove, sodpolicy.VerdictDeny, sodpolicy.VerdictNeedsReview,
	} {
		rec := &SoDDecision{Verdict: verdict, ReasonCode: sodpolicy.ReasonScopeOK}
		if _, err := d.RecordSoDDecision(rec); err != nil {
			t.Fatalf("裁决 %q 写不进去: %v", verdict, err)
		}
	}
	bad := &SoDDecision{Verdict: "maybe", ReasonCode: sodpolicy.ReasonScopeOK}
	if _, err := d.RecordSoDDecision(bad); err == nil {
		t.Fatal("非法裁决值被写进去了：DDL 的 CHECK 没生效")
	}
}

// TestAdversarialReviewQueueLifecycle 断言 needs_review 队列的生命周期。
func TestAdversarialReviewQueueLifecycle(t *testing.T) {
	d := sodOpen(t)
	taskID := sodTestTask(t, d, "SoD 对抗复核队列")

	id, err := d.EnqueueAdversarialReview(&AdversarialReviewItem{
		TaskID:           &taskID,
		SourceAgent:      "worker",
		ToolName:         "Bash",
		PayloadDigest:    "payload-digest-review",
		PayloadPreview:   "«redacted»",
		AnomalyFlag:      true,
		AnomalyReason:    "命令里有管道与重定向",
		StructuredIntent: json.RawMessage(`{"action_type":"shell"}`),
	})
	if err != nil {
		t.Fatalf("入队失败: %v", err)
	}

	pending, err := d.ListAdversarialReviewPending(taskID, 50)
	if err != nil {
		t.Fatalf("列队列失败: %v", err)
	}
	var found *AdversarialReviewItem
	for _, item := range pending {
		if item.ID == id {
			found = item
			break
		}
	}
	if found == nil {
		t.Fatalf("入队项 %d 不在待处理列表里", id)
	}
	if found.Status != SoDReviewPending {
		t.Fatalf("状态应为 pending，实际 %q", found.Status)
	}
	if !found.AnomalyFlag || found.AnomalyReason == "" {
		t.Fatalf("异常标记没写进去: %v/%q", found.AnomalyFlag, found.AnomalyReason)
	}
	if found.ReasonCode != sodpolicy.ReasonAnomalyFlagged {
		t.Fatalf("原因码应为 anomaly_flagged，实际 %q", found.ReasonCode)
	}

	if err := d.ResolveAdversarialReview(id, sodpolicy.RedteamAgentKey, "证伪成立", SoDReviewDismissed); err != nil {
		t.Fatalf("结案失败: %v", err)
	}
	pending, err = d.ListAdversarialReviewPending(taskID, 50)
	if err != nil {
		t.Fatalf("列队列失败: %v", err)
	}
	for _, item := range pending {
		if item.ID == id {
			t.Fatalf("结案后 %d 仍出现在待处理列表里", id)
		}
	}
	var status, claimedBy, resolution string
	if err := d.QueryRow(`SELECT status, claimed_by, resolution FROM sod_adversarial_review_pending WHERE id=$1`, id).
		Scan(&status, &claimedBy, &resolution); err != nil {
		t.Fatalf("读结案结果失败: %v", err)
	}
	if status != SoDReviewDismissed || claimedBy != sodpolicy.RedteamAgentKey || resolution != "证伪成立" {
		t.Fatalf("结案结果不对: %q/%q/%q", status, claimedBy, resolution)
	}
	// 结案一个不存在的项必须报错（调用方要能知道没结成）。
	if err := d.ResolveAdversarialReview(id+999999, "redteam", "", SoDReviewResolved); err == nil {
		t.Fatal("结案不存在的项没有报错")
	}
}

// TestToolCircuitOpensAfterThreshold 断言熔断按代码常量开路、成功后计数清零。
func TestToolCircuitOpensAfterThreshold(t *testing.T) {
	d := sodOpen(t)
	toolKey := "sod-test-tool-circuit"

	// 没出过事的工具：没有行 ⇒ closed。
	state, err := d.ToolCircuitState(toolKey)
	if err != nil {
		t.Fatalf("读熔断状态失败: %v", err)
	}
	if state != sodpolicy.CircuitClosed {
		t.Fatalf("新工具的熔断状态应为 closed，实际 %q", state)
	}

	for i := 0; i < sodpolicy.CircuitFailThreshold; i++ {
		if err := d.RecordToolCallOutcome(toolKey, true); err != nil {
			t.Fatalf("记失败 %d 次出错: %v", i, err)
		}
	}
	state, err = d.ToolCircuitState(toolKey)
	if err != nil {
		t.Fatalf("读熔断状态失败: %v", err)
	}
	if state != sodpolicy.CircuitOpen {
		t.Fatalf("连续失败 %d 次后应为 open，实际 %q", sodpolicy.CircuitFailThreshold, state)
	}
	var failCount int
	if err := d.QueryRow(`SELECT fail_count FROM sod_tool_circuit WHERE tool_key=$1`, toolKey).Scan(&failCount); err != nil {
		t.Fatalf("读失败计数失败: %v", err)
	}
	if failCount != sodpolicy.CircuitFailThreshold {
		t.Fatalf("失败计数应为 %d，实际 %d", sodpolicy.CircuitFailThreshold, failCount)
	}

	// 一次成功 ⇒ 计数清零。熔断期内**不放行**：半开只允许一次试探。
	if err := d.RecordToolCallOutcome(toolKey, false); err != nil {
		t.Fatalf("记成功出错: %v", err)
	}
	if err := d.QueryRow(`SELECT fail_count FROM sod_tool_circuit WHERE tool_key=$1`, toolKey).Scan(&failCount); err != nil {
		t.Fatalf("读失败计数失败: %v", err)
	}
	if failCount != 0 {
		t.Fatalf("成功之后失败计数应清零，实际 %d", failCount)
	}
	state, err = d.ToolCircuitState(toolKey)
	if err != nil {
		t.Fatalf("读熔断状态失败: %v", err)
	}
	if state != sodpolicy.CircuitOpen {
		t.Fatalf("熔断期内一次成功不应放行，实际状态 %q", state)
	}
}

// TestToolCircuitExpiresToHalfOpen 断言过期的开路会转成半开 ——
// 否则闸门会把这个工具**永久**拒绝（拒绝 ⇒ 工具不执行 ⇒ 再没有成败可记）。
func TestToolCircuitExpiresToHalfOpen(t *testing.T) {
	d := sodOpen(t)
	toolKey := "sod-test-tool-circuit-expiry"

	for i := 0; i < sodpolicy.CircuitFailThreshold; i++ {
		if err := d.RecordToolCallOutcome(toolKey, true); err != nil {
			t.Fatalf("记失败 %d 次出错: %v", i, err)
		}
	}
	// 把开路时刻往前挪一个熔断期，模拟「熔断期已过」。
	past := time.Now().Add(-time.Duration(sodpolicy.CircuitOpenDurationSeconds+1) * time.Second)
	if _, err := d.Exec(`UPDATE sod_tool_circuit SET opened_at=$2 WHERE tool_key=$1`, toolKey, past); err != nil {
		t.Fatalf("改开路时刻失败: %v", err)
	}
	state, err := d.ToolCircuitState(toolKey)
	if err != nil {
		t.Fatalf("读熔断状态失败: %v", err)
	}
	if state != sodpolicy.CircuitHalfOpen {
		t.Fatalf("熔断期已过应为 half_open，实际 %q", state)
	}
	// 半开状态下一次成功 ⇒ 回到 closed。
	if err := d.RecordToolCallOutcome(toolKey, false); err != nil {
		t.Fatalf("记成功出错: %v", err)
	}
	state, err = d.ToolCircuitState(toolKey)
	if err != nil {
		t.Fatalf("读熔断状态失败: %v", err)
	}
	if state != sodpolicy.CircuitClosed {
		t.Fatalf("半开试探成功后应为 closed，实际 %q", state)
	}
}

// TestToolCircuitRejectsUnknownState 断言状态值解析不出来时报错 ——
// 调用方据此 fail-closed 拒绝，而不是把「看不懂」当成「没熔断」。
func TestToolCircuitRejectsUnknownState(t *testing.T) {
	d := sodOpen(t)
	toolKey := "sod-test-tool-circuit-bogus"

	// DDL 的 CHECK 本来就挡住非法状态 ⇒ 这里临时摘掉它造出这一行，
	// 验完原样装回去（摘约束是事务性 DDL，中途失败不会留半截状态）。
	if _, err := d.Exec(`ALTER TABLE sod_tool_circuit DROP CONSTRAINT sod_tool_circuit_state_check`); err != nil {
		t.Fatalf("摘掉状态约束失败: %v", err)
	}
	t.Cleanup(func() {
		_, _ = d.Exec(`DELETE FROM sod_tool_circuit WHERE tool_key=$1`, toolKey)
		_, _ = d.Exec(`ALTER TABLE sod_tool_circuit ADD CONSTRAINT sod_tool_circuit_state_check
                       CHECK (state IN ('closed','open','half_open'))`)
	})
	if _, err := d.Exec(`INSERT INTO sod_tool_circuit(tool_key, state, fail_count) VALUES($1,'weird',0)`,
		toolKey); err != nil {
		t.Fatalf("造非法状态失败: %v", err)
	}
	if _, err := d.ToolCircuitState(toolKey); err == nil {
		t.Fatal("非法熔断状态没有报错")
	}
}

// TestFindingReplayEvidenceColumnExists 断言 findings.replay_evidence_id 存在、
// 默认空，且能读出资产归属。
func TestFindingReplayEvidenceColumnExists(t *testing.T) {
	d := sodOpen(t)

	var def string
	if err := d.QueryRow(`
SELECT column_default FROM information_schema.columns
WHERE table_schema='public' AND table_name='findings' AND column_name='replay_evidence_id'`).
		Scan(&def); err != nil {
		t.Fatalf("读 replay_evidence_id 列信息失败: %v", err)
	}
	if def == "" {
		t.Fatal("replay_evidence_id 列不存在")
	}

	// 不存在的 finding ⇒ (nil, nil)（调用方按「不适用」处理）。
	ev, err := d.GetFindingReplayEvidence(999999999)
	if err != nil {
		t.Fatalf("读不存在的 finding 出错: %v", err)
	}
	if ev != nil {
		t.Fatalf("不存在的 finding 应返回 nil，实际 %+v", ev)
	}

	findingID, err := d.AddFinding(0, 0, "sqli", "SoD 测试发现", SeverityHigh,
		"验证重放证据读取", "evidence", "worker", []int64{7, 11})
	if err != nil {
		t.Fatalf("建 finding 失败: %v", err)
	}
	ev, err = d.GetFindingReplayEvidence(findingID)
	if err != nil {
		t.Fatalf("读重放证据失败: %v", err)
	}
	if ev == nil {
		t.Fatal("finding 存在却读出 nil")
	}
	if ev.FindingID != findingID {
		t.Fatalf("finding id 不一致: %d", ev.FindingID)
	}
	if ev.ReplayEvidenceID != "" {
		t.Fatalf("重放证据标识应为空，实际 %q", ev.ReplayEvidenceID)
	}
	if !reflect.DeepEqual(ev.AssetIDs, []int64{7, 11}) {
		t.Fatalf("资产归属不一致: %v", ev.AssetIDs)
	}
}

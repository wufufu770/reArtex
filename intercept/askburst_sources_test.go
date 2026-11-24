package intercept

// A-08 审批防疲劳三档测试（0.15.75）：短路表 ask / DB 规则 ask / 判官 ask 分别
// 计数，三档互不挤占；规则/判官两档的拒绝发生在 HandleAsk 入口（不产生新
// pending 行、不进入等待分支）。

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
)

// seedRuleAsk 造一条「DB 规则」发起的审批记录（decision_source='rule'，
// 由 db.interceptSource 按 ruleID≠0 推导——与真实落库口径一致）。
func seedRuleAsk(t *testing.T, d *db.DB, ruleID int64, taskID, agentName, toolName string) {
	t.Helper()
	audit := &db.InterceptAudit{RuleName: "高危命令需人工确认"}
	if _, err := d.CreateInterceptPending(ruleID, 0, taskID, agentName, toolName, []byte(`{}`), "需人工确认", audit); err != nil {
		t.Fatal(err)
	}
}

// seedModelAsk 造一条「模型判官」发起的审批记录（decision_source='model'，
// 由 reason 的 [模型] 前缀推导）。
func seedModelAsk(t *testing.T, d *db.DB, taskID, agentName, toolName string) {
	t.Helper()
	audit := &db.InterceptAudit{RuleName: "[模型] 转人工审批"}
	if _, err := d.CreateInterceptPending(0, 0, taskID, agentName, toolName, []byte(`{}`), "[模型] 转人工审批", audit); err != nil {
		t.Fatal(err)
	}
}

// countPendingAsks 数某 agent+tool 处于 pending 状态的审批行数（校验「拒绝不
// 产生新 pending 行」）。防疲劳拒绝本身会落一条 status='denied' 观测行
// （审批记录页可检索），不算 pending。
func countPendingAsks(t *testing.T, d *db.DB, agentName, toolName string) int {
	t.Helper()
	rows, err := d.Query(`SELECT COUNT(*) FROM intercept_pending WHERE agent_name=$1 AND tool_name=$2 AND status='pending'`, agentName, toolName)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var n int
	for rows.Next() {
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
	}
	return n
}

// setupBurstTest 固定阈值/窗口，返回接好上下文的 interceptor。
func setupBurstTest(t *testing.T, agent, tool string) (*db.DB, *Interceptor, context.Context) {
	t.Helper()
	d := openTestDB(t)
	for _, kv := range [][2]string{
		{settingAskBurstThreshold, "5"},
		{settingAskBurstWindowSecs, "600"},
	} {
		if err := d.SetSetting(kv[0], kv[1]); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = d.SetSetting(kv[0], "") })
	}
	ic := New(d)
	ctx := WithTaskContext(context.Background(), "task-burst", agent, "", nil)
	return d, ic, ctx
}

// 三档分别计数、互不挤占：短路段满阈值不影响规则档/判官档的预算。
func TestAskBurstSourcesCountedIndependently(t *testing.T) {
	d, ic, ctx := setupBurstTest(t, "worker-multi", "update_custom_tool")

	for i := 0; i < defaultAskBurstThreshold; i++ {
		seedShortCircuitAsk(t, d, "task-burst", "worker-multi", "update_custom_tool")
	}
	if !ic.AskBurstExceededFor(ctx, "update_custom_tool", AskSourceShortCircuit) {
		t.Fatal("短路表档达阈值应拒绝")
	}
	if ic.AskBurstExceededFor(ctx, "update_custom_tool", AskSourceRule) {
		t.Fatal("规则档未达阈值，不应被短路表档挤占")
	}
	if ic.AskBurstExceededFor(ctx, "update_custom_tool", AskSourceModel) {
		t.Fatal("判官档未达阈值，不应被短路表档挤占")
	}

	for i := 0; i < defaultAskBurstThreshold; i++ {
		seedRuleAsk(t, d, 1, "task-burst", "worker-multi", "update_custom_tool")
	}
	if !ic.AskBurstExceededFor(ctx, "update_custom_tool", AskSourceRule) {
		t.Fatal("规则档达阈值应拒绝")
	}
}

// 旧口径函数行为不变（单来源 shortcircuit）。
func TestAskBurstLegacySingleSourceUnchanged(t *testing.T) {
	d, ic, ctx := setupBurstTest(t, "worker-legacy", "update_custom_tool")
	for i := 0; i < defaultAskBurstThreshold; i++ {
		seedModelAsk(t, d, "task-burst", "worker-legacy", "update_custom_tool")
	}
	if ic.AskBurstExceeded(ctx, "update_custom_tool") {
		t.Fatal("旧口径只数短路表档：判官 ask 不应触发")
	}
	for i := 0; i < defaultAskBurstThreshold; i++ {
		seedShortCircuitAsk(t, d, "task-burst", "worker-legacy", "update_custom_tool")
	}
	if !ic.AskBurstExceeded(ctx, "update_custom_tool") {
		t.Fatal("旧口径下短路表档达阈值应触发")
	}
}

// 规则档满阈值：HandleAsk 入口直接拒绝——不产生新 pending 行、不进入等待分支
// （防疲劳拒绝不无限等待）。
func TestHandleAskDeniesRuleAskWhenBurstExceeded(t *testing.T) {
	d, ic, ctx := setupBurstTest(t, "worker-rule", "update_custom_tool")
	rule, err := d.CreateInterceptRule("burst-rule", "tool_input", "regex", `rm\s+-rf`, "ask", "高危命令需人工确认", 100, true, false, 0, "deny")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteInterceptRule(rule.ID) })

	for i := 0; i < defaultAskBurstThreshold; i++ {
		seedRuleAsk(t, d, rule.ID, "task-burst", "worker-rule", "update_custom_tool")
	}
	before := countPendingAsks(t, d, "worker-rule", "update_custom_tool")

	done := make(chan bool, 1)
	go func() {
		done <- ic.HandleAsk(ctx, 0, Decision{RuleID: rule.ID, Action: "ask", Message: rule.Message}, "update_custom_tool", []byte(`{}`))
	}()
	select {
	case allowed := <-done:
		if allowed {
			t.Fatal("规则档满阈值应直接拒绝")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("防疲劳拒绝应在入口返回，不得进入等待分支")
	}
	if got := countPendingAsks(t, d, "worker-rule", "update_custom_tool"); got != before {
		t.Fatalf("拒绝不应产生新 pending 行：前=%d 后=%d", before, got)
	}
	// 观测面：拒绝落一条 status='denied' 且注明防疲劳的行（审批记录页可检索）。
	rows, err := d.Query(`SELECT COALESCE(reason,'') FROM intercept_pending
		WHERE agent_name='worker-rule' AND tool_name='update_custom_tool' AND status='denied'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	found := false
	for rows.Next() {
		var reason string
		if err := rows.Scan(&reason); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(reason, "防疲劳") {
			found = true
		}
	}
	if !found {
		t.Fatal("防疲劳拒绝应落一条注明防疲劳的 denied 行")
	}
}

// 判官档满阈值：HandleAsk 入口同样直接拒绝（判官 ask 走 [模型] 前缀口径）。
func TestHandleAskDeniesModelAskWhenBurstExceeded(t *testing.T) {
	d, ic, ctx := setupBurstTest(t, "worker-model", "update_custom_tool")
	for i := 0; i < defaultAskBurstThreshold; i++ {
		seedModelAsk(t, d, "task-burst", "worker-model", "update_custom_tool")
	}
	done := make(chan bool, 1)
	go func() {
		done <- ic.HandleAsk(ctx, 0, Decision{Action: "ask", Message: "[模型] 转人工审批"}, "update_custom_tool", []byte(`{}`))
	}()
	select {
	case allowed := <-done:
		if allowed {
			t.Fatal("判官档满阈值应直接拒绝")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("防疲劳拒绝应在入口返回，不得进入等待分支")
	}
}

// 阈值内的新工具不受牵连（跨工具隔离，规则档口径）。
func TestAskBurstRuleSourceToolIsolation(t *testing.T) {
	d, ic, ctx := setupBurstTest(t, "worker-iso", "tool_a")
	for i := 0; i < defaultAskBurstThreshold; i++ {
		seedRuleAsk(t, d, 1, "task-burst", "worker-iso", "tool_a")
	}
	if !ic.AskBurstExceededFor(ctx, "tool_a", AskSourceRule) {
		t.Fatal("tool_a 规则档达阈值应拒绝")
	}
	if ic.AskBurstExceededFor(ctx, "tool_b", AskSourceRule) {
		t.Fatal("不同工具不应受牵连")
	}
	if ic.AskBurstExceededFor(context.Background(), "tool_a", AskSourceRule) {
		t.Fatal("无 agent 归属时不计数")
	}
}

// decisionSourceOf 与 db.interceptSource 同口径。
func TestDecisionSourceOfMatchesDBDerivation(t *testing.T) {
	cases := []struct {
		dec  Decision
		want string
	}{
		{Decision{RuleID: 7}, AskSourceRule},
		{Decision{Message: "[模型] 转人工审批"}, AskSourceModel},
		{Decision{ModelFallback: true, Action: "ask"}, AskSourceModel},
		{Decision{Message: "需人工确认", RuleName: "[代码常量] 平台工具"}, AskSourceShortCircuit},
	}
	for _, tc := range cases {
		if got := decisionSourceOf(tc.dec); got != tc.want {
			t.Fatalf("decisionSourceOf(%+v)=%q，期望 %q", tc.dec, got, tc.want)
		}
	}
	if AskSourceShortCircuit != "shortcircuit" || AskSourceRule != "rule" || AskSourceModel != "model" {
		t.Fatal("来源常量必须与 intercept_pending 既有取值一致，不得新增魔法值")
	}
}

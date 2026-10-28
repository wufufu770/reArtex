package intercept

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
)

// openTestDB 打开本包的派生测试库；连不上就跳过需要库的用例。
func openTestDB(t *testing.T) *db.DB {
	t.Helper()
	dsn := os.Getenv("ARTEX_PG_DSN")
	if dsn == "" {
		t.Skip("无测试库（ARTEX_PG_DSN 未设置）")
	}
	d, err := db.Open(dsn)
	if err != nil {
		t.Skipf("打开测试库失败: %v", err)
	}
	return d
}

// seedShortCircuitAsk 造一条「代码常量短路表」发起的审批记录。
func seedShortCircuitAsk(t *testing.T, d *db.DB, taskID, agentName, toolName string) {
	t.Helper()
	audit := &db.InterceptAudit{RuleName: "[代码常量] 平台工具需人工确认"}
	if _, err := d.CreateInterceptPending(0, 0, taskID, agentName, toolName, []byte(`{}`), "需人工确认", audit); err != nil {
		t.Fatal(err)
	}
}

// 同一 agent 在窗口内反复对同一工具请求审批 → 达阈值后不再问人，直接拒。
// 治的是「疲劳点 allow」的无限循环；单次/少量请求完全不受影响。
func TestAskBurstExceeded(t *testing.T) {
	d := openTestDB(t)
	ic := New(d)
	ctx := WithTaskContext(context.Background(), "task-1", "worker-1", nil)

	for i := 0; i < defaultAskBurstThreshold; i++ {
		seedShortCircuitAsk(t, d, "task-1", "worker-1", "update_custom_tool")
	}

	if !ic.AskBurstExceeded(ctx, "update_custom_tool") {
		t.Fatalf("窗口内已有 %d 次同工具审批，应判定为超阈值", defaultAskBurstThreshold)
	}
	if ic.AskBurstExceeded(ctx, "create_skill") {
		t.Fatal("别的工具不应受牵连")
	}
	if ic.AskBurstExceeded(context.Background(), "update_custom_tool") {
		t.Fatal("无 agent 归属时不应升级")
	}
}

// 窗口外的旧请求不计数：治理只针对「当前这一波」的反复请求。
func TestAskBurstIgnoresOldRequests(t *testing.T) {
	d := openTestDB(t)
	ic := New(d)
	ctx := WithTaskContext(context.Background(), "task-2", "worker-2", nil)

	for i := 0; i < defaultAskBurstThreshold; i++ {
		seedShortCircuitAsk(t, d, "task-2", "worker-2", "update_custom_tool")
	}
	if err := d.SetSetting(settingAskBurstWindowSecs, "1"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)

	if ic.AskBurstExceeded(ctx, "update_custom_tool") {
		t.Fatal("窗口外的请求不应计入")
	}
}

// DB 规则发起的 ask 不计入短路表的治理口径：那是管理员自己配的面。
func TestAskBurstIgnoresRuleSourcedAsks(t *testing.T) {
	d := openTestDB(t)
	ic := New(d)
	ctx := WithTaskContext(context.Background(), "task-3", "worker-3", nil)

	for i := 0; i < defaultAskBurstThreshold+2; i++ {
		if _, err := d.CreateInterceptPending(0, 0, "task-3", "worker-3", "update_custom_tool", []byte(`{}`), "[模型] 需要判断"); err != nil {
			t.Fatal(err)
		}
	}
	if ic.AskBurstExceeded(ctx, "update_custom_tool") {
		t.Fatal("模型/规则来源的 ask 不应计入短路表的防疲劳计数")
	}
}

// 阈值可经 settings 热改（可用性参数，不是安全边界）。
func TestAskBurstThresholdIsConfigurable(t *testing.T) {
	d := openTestDB(t)
	ic := New(d)
	ctx := WithTaskContext(context.Background(), "task-4", "worker-4", nil)

	seedShortCircuitAsk(t, d, "task-4", "worker-4", "update_custom_tool")
	if ic.AskBurstExceeded(ctx, "update_custom_tool") {
		t.Fatal("1 次请求不应触发默认阈值")
	}
	if err := d.SetSetting(settingAskBurstThreshold, "1"); err != nil {
		t.Fatal(err)
	}
	if !ic.AskBurstExceeded(ctx, "update_custom_tool") {
		t.Fatal("阈值调到 1 后，窗口内已有 1 次请求应触发")
	}
}

package llmrec

import (
	"context"
	"fmt"
	"iter"
	"os"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/llm"
)

// 本文件是群组预算在**计量层**的验收用例。
//
// 覆盖三条性质：
//  1. 估算必须覆盖 provider 内部的**重试预算**（少估 = 预算被击穿）；
//  2. 估不出来的请求形态必须被**拒绝**（fail-closed，绝不静默放行）；
//  3. 闸门拒绝时，这次调用**不得**被记成一次成功调用。
//
// 为什么都要钉在「钱」这条线上：这一层决定平台**能不能**花钱、按多少额度花。
// 估算偏大只是暂时占住额度（结算时退回），估算偏小或跳过估算则是预算被击穿 ——
// 后者不可逆。

// quotaTestDB 开一个测试库（llmrec 包的 TestMain 已经把 ARTEX_PG_DSN 指到
// 本包的派生库）。
//
// 为什么直接读环境变量而不是 db.DSN()：db.DSN() 读入即把环境变量抹掉（防口令
// 泄进 Bash 子进程的环境），谁先调用谁就把后面的用例饿着。
func quotaTestDB(t *testing.T) *db.DB {
	t.Helper()
	dsn := os.Getenv("ARTEX_PG_DSN")
	if dsn == "" {
		t.Skip("requires isolated ARTEX_PG_DSN")
	}
	pg, err := db.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pg.Close() })
	return pg
}

// quotaTestGroup 建一个「任务 + 大厅会话 + 群组」，返回 (任务 id, 群组 id)。
func quotaTestGroup(t *testing.T, pg *db.DB, name string) (int64, int64) {
	t.Helper()
	task, err := pg.CreateTask(name, "拿下目标", nil, 0, 0)
	if err != nil {
		t.Fatalf("建任务: %v", err)
	}
	t.Cleanup(func() { _ = pg.DeleteTask(task.ID) })
	hall, err := pg.CreateConversation("auto", "大厅", nil)
	if err != nil {
		t.Fatalf("建会话: %v", err)
	}
	t.Cleanup(func() { _, _ = pg.Exec(`DELETE FROM conversations WHERE id=$1`, hall.ID) })
	gid, err := pg.CreateChatGroup(&db.ChatGroup{
		ConversationID:   hall.ID,
		TaskID:           task.ID,
		Name:             name,
		Status:           "active",
		ConcurrencyLimit: 1,
	})
	if err != nil {
		t.Fatalf("建群组: %v", err)
	}
	return task.ID, gid
}

// quotaTextReq 造一条可估算的请求（纯文本 + 已知 max_tokens）。
func quotaTextReq(text string, maxTokens int) llm.CompletionRequest {
	return llm.CompletionRequest{
		Messages:  []llm.Message{llm.UserText(text)},
		MaxTokens: maxTokens,
	}
}

// TestEstimateTokensCoversRetryBudget 钉住「重试预算只乘输入侧」（方案 v3 C1）。
//
// 为什么要这条：provider 内部的自动重试对闸门不可见 ⇒ 一次预扣可能对应多次
// 真实请求，输入侧必须按 attempts 覆盖；但输出侧若也乘 attempts，一次 32K
// MaxTokens 的调用会预扣 98K+，把 120K 群组预算的并发压成 1（假性额度耗尽）。
//
// 断言：
//   - 带重试预算的估算**严格大于**不带重试的估算；
//   - 输入侧是**精确的倍数**（1 + maxRetries 次尝试）；
//   - 输出侧不随 attempts 放大（单次 4096 上限）；
//   - 拆分与总量自洽（总量 = 输入 + 输出）。
func TestEstimateTokensCoversRetryBudget(t *testing.T) {
	req := quotaTextReq("hello world", 1000)

	got, err := EstimateTokens(req, 2)
	if err != nil {
		t.Fatalf("EstimateTokens: %v", err)
	}
	one, err := EstimateTokens(req, 0)
	if err != nil {
		t.Fatalf("EstimateTokens: %v", err)
	}
	if got.TotalTokens <= one.TotalTokens {
		t.Fatalf("带重试预算的估算 (%d) 必须严格大于单次估算 (%d)", got.TotalTokens, one.TotalTokens)
	}
	if want := one.InputTokens * 3; got.InputTokens != want {
		t.Fatalf("重试预算为 2 时输入侧估算应为单次的 3 倍：got %d, want %d", got.InputTokens, want)
	}
	if got.OutputTokens != one.OutputTokens {
		t.Fatalf("输出侧不随重试预算放大：got %d, want %d", got.OutputTokens, one.OutputTokens)
	}
	if got.InputTokens+got.OutputTokens != got.TotalTokens {
		t.Fatalf("拆分与总量不自洽：%d + %d != %d", got.InputTokens, got.OutputTokens, got.TotalTokens)
	}
	// 负的重试预算没有意义（不该出现），按「至少一次尝试」处理而不是放大/缩小。
	neg, err := EstimateTokens(req, -3)
	if err != nil {
		t.Fatalf("EstimateTokens: %v", err)
	}
	if neg.TotalTokens != one.TotalTokens {
		t.Fatalf("负的重试预算应按一次尝试算：got %d, want %d", neg.TotalTokens, one.TotalTokens)
	}
}

// TestEstimateTokensCapsOutputAt4096 钉住「输出侧估算 4096 上限」（方案 v3 C1）。
//
// 请求侧 MaxTokens=32K 是模型允许的上限不是期望用量；按它整额预扣曾把 120K
// 预算的并发压成 1。断言：无论 MaxTokens 是 0（未指定）还是 32768，输出侧都
// 是 4096；小请求（1000）仍按请求值估。
func TestEstimateTokensCapsOutputAt4096(t *testing.T) {
	for _, tc := range []struct {
		name      string
		maxTokens int
	}{
		{"未指定", 0},
		{"请求上限32K", 32768},
	} {
		req := quotaTextReq("hello world", tc.maxTokens)
		got, err := EstimateTokens(req, 0)
		if err != nil {
			t.Fatalf("%s: EstimateTokens: %v", tc.name, err)
		}
		if got.OutputTokens != defaultOutputTokenEstimate {
			t.Fatalf("%s: 输出侧应按 4096 上限估：got %d", tc.name, got.OutputTokens)
		}
	}
	small, err := EstimateTokens(quotaTextReq("hello world", 1000), 0)
	if err != nil {
		t.Fatalf("EstimateTokens: %v", err)
	}
	if small.OutputTokens != 1000 {
		t.Fatalf("小请求应按请求值估：got %d, want 1000", small.OutputTokens)
	}
}

// TestEstimateTokensRefusesNonEstimableRequest 钉住「估不出来的请求必须被拒」。
//
// 不可估算的请求形态（图片 / 音频 / 附件，以及将来新增的任何未知内容类型）
// 绝不能返回一个「尽力而为」的数：那种数会让额度**低估**，而预算被低估 =
// 预算被击穿。正确做法是拒绝这次调用（fail-closed）。
//
// 特别要盯住「未知类型」：把它按 0 算 ⇒ 少扣 ⇒ 正是这条性质要防的事。
func TestEstimateTokensRefusesNonEstimableRequest(t *testing.T) {
	req := llm.CompletionRequest{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{
			{Type: llm.BlockType("image"), Text: "not-really-an-image"},
		}}},
		MaxTokens: 100,
	}
	if _, err := EstimateTokens(req, 0); err == nil {
		t.Fatal("EstimateTokens must fail for a non-estimable request shape (fail-closed)")
	}
	// 未知类型同理：白名单之外的任何类型都不可估算。
	unknown := llm.CompletionRequest{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{
			{Type: llm.BlockType("some_future_type"), Text: "x"},
		}}},
		MaxTokens: 100,
	}
	if _, err := EstimateTokens(unknown, 0); err == nil {
		t.Fatal("EstimateTokens must refuse unknown content types (fail-closed)")
	}
	// 纯文本请求必须仍然估得出来（白名单不能把正常流量也拒了）。
	if _, err := EstimateTokens(quotaTextReq("hello", 100), 0); err != nil {
		t.Fatalf("纯文本请求必须可估算: %v", err)
	}
}

// TestQuotaLookupResolvesGroupAllocation 钉住「任务 ⇒ 群组」的解析。
//
// 两条腿都要钉：
//   - 任务绑了群组 ⇒ 解析得到那个群组（ok=true）—— 闸门据此找预算；
//   - 任务**没有**群组 ⇒ 解析不到（ok=false）—— 这不是错误，平台里绝大多数
//     调用不在大厅里，**不得**因此拦截。
//
// 「查不出来」与「不属于任何群组」必须分得开：前者（err）是失败关闭，后者
// （ok=false）是不拦截。混起来的后果是「查库的临时故障」被当成「没预算」放行。
func TestQuotaLookupResolvesGroupAllocation(t *testing.T) {
	pg := quotaTestDB(t)
	taskID, gid := quotaTestGroup(t, pg, "额度解析用例")

	resolve := TaskGroupResolver(pg)
	ctx := context.Background()

	gotGID, ok, err := resolve(ctx, fmt.Sprintf("%d", taskID))
	if err != nil {
		t.Fatalf("解析群组: %v", err)
	}
	if !ok {
		t.Fatalf("绑了群组的任务必须解析得到群组（task %d）", taskID)
	}
	if gotGID != gid {
		t.Fatalf("解析到的群组 = %d，期望 %d", gotGID, gid)
	}

	// 那个群组的预算账必须读得到 —— 闸门下一步就靠它预扣。
	if err := pg.SetGroupQuotaBudget(gid, 100000); err != nil {
		t.Fatalf("设预算: %v", err)
	}
	quota, err := pg.GetGroupQuota(gid)
	if err != nil {
		t.Fatalf("读群组预算: %v", err)
	}
	if quota == nil {
		t.Fatal("群组预算读不回来")
	}
	if quota.BudgetTokens != 100000 {
		t.Fatalf("预算 = %d，期望 100000", quota.BudgetTokens)
	}

	// 没有群组的任务 ⇒ 不属于任何群组 ⇒ 不拦截（不是错误）。
	lonely, err := pg.CreateTask("没有群组的任务", "目标", nil, 0, 0)
	if err != nil {
		t.Fatalf("建任务: %v", err)
	}
	t.Cleanup(func() { _ = pg.DeleteTask(lonely.ID) })
	gotGID, ok, err = resolve(ctx, fmt.Sprintf("%d", lonely.ID))
	if err != nil {
		t.Fatalf("无群组任务不该报错: %v", err)
	}
	if ok {
		t.Fatalf("没有群组的任务不该被解析成有群组（gid %d）", gotGID)
	}
	if gotGID != 0 {
		t.Fatalf("没有群组时群组 id 必须是 0，got %d", gotGID)
	}

	// 空 task id（平台对话路径）⇒ 不属于任何群组 ⇒ 不拦截。
	if _, ok, err := resolve(ctx, ""); ok || err != nil {
		t.Fatalf("空 task id 应解析为「不属于任何群组」：ok=%v err=%v", ok, err)
	}
}

// TestPreChargeCoversRetryBudget 钉住「**预扣**覆盖重试预算（C1：只乘输入侧）」。
//
// 上面那条钉的是估算函数本身；这条钉的是**真实预扣**：闸门扣下去的那笔预算
// 必须已经是乘过重试预算的数 —— 否则「估算覆盖了重试」只停留在纸面上。
//
// 做法：真建一个群组预算 → 走一次 QuotaProvider → 把预扣行读回来比对。
func TestPreChargeCoversRetryBudget(t *testing.T) {
	pg := quotaTestDB(t)
	taskID, gid := quotaTestGroup(t, pg, "预扣覆盖重试用例")
	if err := pg.SetGroupQuotaBudget(gid, 10_000_000); err != nil {
		t.Fatalf("设预算: %v", err)
	}

	req := quotaTextReq("hello world", 1000)
	single, err := EstimateTokens(req, 0)
	if err != nil {
		t.Fatalf("EstimateTokens: %v", err)
	}
	// C1 口径：输入侧 × attempts + 单次输出上限；输出侧不随 attempts 放大。
	want := single.InputTokens*int64(DefaultRetryBudget+1) + single.OutputTokens

	provider := NewQuotaProvider(completeProvider{}, NewQuotaGate(pg, nil))
	ctx := WithTaskID(context.Background(), fmt.Sprintf("%d", taskID))
	if _, _, _, err := provider.Complete(ctx, req); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	var reserved int64
	if err := pg.QueryRow(`SELECT reserved_tokens FROM group_quota_reservations
WHERE group_id=$1 ORDER BY id DESC LIMIT 1`, gid).Scan(&reserved); err != nil {
		t.Fatalf("读预扣行: %v", err)
	}
	if reserved != want {
		t.Fatalf("预扣额 = %d，期望 %d（输入估算 %d × %d 次尝试 + 输出 %d）",
			reserved, want, single.InputTokens, DefaultRetryBudget+1, single.OutputTokens)
	}
}

// TestRecorderRefusesWhenGateRejects 钉住「闸门拒绝 ⇒ 这次调用不得被记成成功」。
//
// 三条断言：
//   - 调用被拒（错误返回）；
//   - inner provider **一次都没被调用**（请求根本没发出去）；
//   - 计量账里没有「成功」的行，也没有带 token 的行。
//
// 为什么最后一条重要：闸门拒绝时这次调用没有花钱。若记账层把它记成一次
// 成功调用（status=ok 且带 token），看板上的用量就会虚高，而额度账与用量账
// 两边对不上 —— 事后没人说得清多花的那笔是哪来的。
//
// 平台自己的口径是「拒绝也留痕，但记成失败」：拒绝本身另有 group_quota_refusals
// 留痕，llm_usage 里最多留一行 status=error 且零 token 的记录。
func TestRecorderRefusesWhenGateRejects(t *testing.T) {
	pg := quotaTestDB(t)
	taskID, gid := quotaTestGroup(t, pg, "闸门拒绝用例")
	// 预算额度为 0 ⇒ 「没有可用预算」⇒ 预扣一律拒绝（fail closed）。
	if err := pg.SetGroupQuotaBudget(gid, 0); err != nil {
		t.Fatalf("设预算: %v", err)
	}

	inner := &countingQuotaProvider{}
	provider := NewQuotaProvider(inner, NewQuotaGate(pg, nil))
	recorder := Wrap(provider, pg, "quota-gate-model", "quota-gate-profile", "", "", func() bool { return false })

	ctx := WithTaskID(context.Background(), fmt.Sprintf("%d", taskID))
	_, _, _, err := recorder.Complete(ctx, quotaTextReq("hello", 100))
	if err == nil {
		t.Fatal("闸门拒绝时 Complete 必须返回错误")
	}
	if !IsGroupQuotaMessage(err.Error()) {
		t.Fatalf("拒绝消息必须带群组预算标记（否则会被误判成供应商额度耗尽）：%v", err)
	}
	if inner.calls != 0 {
		t.Fatalf("闸门拒绝后 inner provider 被调用了 %d 次，期望 0（请求根本没发出去）", inner.calls)
	}

	// 计量账里不得有「成功」或带 token 的行。
	rows, err := pg.Query(`SELECT status, input_tokens, output_tokens, cache_read, cache_write
FROM llm_usage WHERE task_id=$1`, fmt.Sprintf("%d", taskID))
	if err != nil {
		t.Fatalf("读计量账: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var status string
		var in, out, cr, cw int
		if err := rows.Scan(&status, &in, &out, &cr, &cw); err != nil {
			t.Fatalf("扫计量行: %v", err)
		}
		if status == "ok" {
			t.Fatalf("被闸门拒绝的调用不得记成成功调用（status=%s）", status)
		}
		if in+out+cr+cw != 0 {
			t.Fatalf("被闸门拒绝的调用不得记任何 token（实际 %d+%d+%d+%d）", in, out, cr, cw)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("扫计量行: %v", err)
	}

	// 拒绝本身要有留痕（这才是「拒绝也留痕」的那一半）。
	var refusals int
	if err := pg.QueryRow(`SELECT count(*) FROM group_quota_refusals WHERE group_id=$1`, gid).Scan(&refusals); err != nil {
		t.Fatalf("读拒绝留痕: %v", err)
	}
	if refusals == 0 {
		t.Fatal("闸门拒绝必须留下 group_quota_refusals 留痕")
	}
}

// countingQuotaProvider 记录自己被调用了几次（证明请求没发出去）。
type countingQuotaProvider struct {
	calls int
}

func (p *countingQuotaProvider) Stream(context.Context, llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(func(llm.StreamEvent, error) bool) {}
}

func (p *countingQuotaProvider) Complete(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	p.calls++
	return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("answer")}},
		"stop", llm.Usage{InputTokens: 7, OutputTokens: 3}, nil
}

// TestQuotaRefusalMessageAvoidsProviderMarkers 钉住「拒绝文案不撞供应商额度 markers」。
//
// 平台自己的预算拒绝一旦被判成供应商额度耗尽，当前 profile 会被标成
// quota_exhausted 并切走 —— 别的任务也因此失去这个供应商（预算语义与供应商
// 语义混在一起）。这条防线在 llmrec/markers.go，本文件只钉住它接进闸门后
// 仍然成立。
func TestQuotaRefusalMessageAvoidsProviderMarkers(t *testing.T) {
	pg := quotaTestDB(t)
	taskID, gid := quotaTestGroup(t, pg, "拒绝文案用例")
	if err := pg.SetGroupQuotaBudget(gid, 0); err != nil {
		t.Fatalf("设预算: %v", err)
	}
	provider := NewQuotaProvider(completeProvider{}, NewQuotaGate(pg, nil))
	ctx := WithTaskID(context.Background(), fmt.Sprintf("%d", taskID))
	_, _, _, err := provider.Complete(ctx, quotaTextReq("hello", 100))
	if err == nil {
		t.Fatal("闸门拒绝时必须返回错误")
	}
	for _, marker := range []string{"额度不足", "额度已用尽", "欠费", "余额不足"} {
		if strings.Contains(err.Error(), marker) {
			t.Fatalf("拒绝文案撞上供应商额度 marker %q：%v", marker, err)
		}
	}
}

// 编译期断言：闸门与 provider 满足 llm.Provider 契约。
var _ llm.Provider = (*QuotaProvider)(nil)

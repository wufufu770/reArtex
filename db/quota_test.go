package db

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// 群组预算的验收用例。
//
// 这一层最要紧的性质是**并发下预算不被击穿**：预扣必须原子地判断「还够不够」，
// 不能是先读后写（两个并发调用都读到「够」，然后都写下去）。
// TestGroupQuotaReserveNeverExceedsBudget 就是这条性质的守门人。

// quotaTestGroup 建一个群组（预算表外键指向 chat_groups）。
func quotaTestGroup(t *testing.T, d *DB) int64 {
	t.Helper()
	task, err := d.CreateTask("预算测试任务", "拿下目标", nil, 0, 0)
	if err != nil {
		t.Fatalf("建任务: %v", err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(task.ID) })
	hall, err := d.CreateConversation("auto", "大厅", nil)
	if err != nil {
		t.Fatalf("建会话: %v", err)
	}
	gid, err := d.CreateChatGroup(&ChatGroup{
		ConversationID:   hall.ID,
		TaskID:           task.ID,
		Name:             "预算测试群",
		Status:           "active",
		ConcurrencyLimit: 1,
	})
	if err != nil {
		t.Fatalf("建群组: %v", err)
	}
	return gid
}

// TestGroupQuotaReserveNeverExceedsBudget 是这一层最关键的用例：
// N 个并发预扣打向一个装不下它们的预算，最终占用的预扣总额**不得超过预算**。
//
// 为什么必须并发地打：串行地打永远看不出竞态 —— 先读后写的实现串行时是对的，
// 并发时才会同时通过「还够不够」的判断。
func TestGroupQuotaReserveNeverExceedsBudget(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer d.Close()

	gid := quotaTestGroup(t, d)
	const budget = 1000
	if err := d.SetGroupQuotaBudget(gid, budget); err != nil {
		t.Fatalf("设预算: %v", err)
	}

	const workers = 8
	const each = 300
	var wg sync.WaitGroup
	results := make([]error, workers)
	ids := make([]int64, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ids[i], results[i] = d.ReserveGroupQuota(gid, QuotaReservation{
				RequestID:   fmt.Sprintf("concurrent-%d-%d", time.Now().UnixNano(), i),
				Tokens:      each,
				InputTokens: each,
			})
		}(i)
	}
	wg.Wait()

	okCount := 0
	for i, err := range results {
		if err == nil {
			okCount++
			if ids[i] <= 0 {
				t.Fatalf("成功预扣却没拿到预扣 id")
			}
		}
	}
	if okCount == 0 {
		t.Fatalf("所有预扣都失败了")
	}
	if okCount*each > budget {
		t.Fatalf("并发预扣击穿了预算：成功 %d 次 × %d = %d > 预算 %d",
			okCount, each, okCount*each, budget)
	}

	quota, err := d.GetGroupQuota(gid)
	if err != nil {
		t.Fatalf("读预算: %v", err)
	}
	if quota == nil {
		t.Fatalf("预算读不回来")
	}
	if quota.ReservedTokens > budget {
		t.Fatalf("在途预扣超过预算: %d > %d", quota.ReservedTokens, budget)
	}
	if quota.ReservedTokens != int64(okCount*each) {
		t.Fatalf("在途预扣额与成功次数对不上: %d != %d", quota.ReservedTokens, okCount*each)
	}
}

// TestGroupQuotaSettleReconcilesEstimate：结算按**实际**用量调整预扣。
//
// 两个方向都要验：实际小于估算 ⇒ 差额释放；实际大于估算 ⇒ 超出部分照样记账
// （不能被裁掉 —— 裁掉就等于供应商多收的钱没人认）。
func TestGroupQuotaSettleReconcilesEstimate(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer d.Close()

	gid := quotaTestGroup(t, d)
	if err := d.SetGroupQuotaBudget(gid, 100000); err != nil {
		t.Fatalf("设预算: %v", err)
	}

	// 方向一：实际小于估算 ⇒ 剩余释放。
	id, err := d.ReserveGroupQuota(gid, QuotaReservation{
		RequestID: "settle-less", Tokens: 1000, InputTokens: 800, OutputTokens: 200,
	})
	if err != nil {
		t.Fatalf("预扣: %v", err)
	}
	if _, err := d.SettleGroupQuota(id, QuotaUsage{InputTokens: 300, OutputTokens: 100, TotalTokens: 400}); err != nil {
		t.Fatalf("结算: %v", err)
	}
	quota, err := d.GetGroupQuota(gid)
	if err != nil {
		t.Fatalf("读预算: %v", err)
	}
	if quota.ConsumedTokens != 400 {
		t.Fatalf("结算后已用量不对: %d != 400", quota.ConsumedTokens)
	}
	if quota.ReservedTokens != 0 {
		t.Fatalf("结算后预扣应释放干净: %d", quota.ReservedTokens)
	}

	// 方向二：实际大于估算 ⇒ 超出部分照样记进已用。
	id, err = d.ReserveGroupQuota(gid, QuotaReservation{
		RequestID: "settle-more", Tokens: 1000, InputTokens: 800, OutputTokens: 200,
	})
	if err != nil {
		t.Fatalf("预扣: %v", err)
	}
	if _, err := d.SettleGroupQuota(id, QuotaUsage{InputTokens: 1500, OutputTokens: 600, TotalTokens: 2100}); err != nil {
		t.Fatalf("结算: %v", err)
	}
	quota, err = d.GetGroupQuota(gid)
	if err != nil {
		t.Fatalf("读预算: %v", err)
	}
	if quota.ConsumedTokens != 400+2100 {
		t.Fatalf("超出估算的部分没记账: %d != %d", quota.ConsumedTokens, 400+2100)
	}
}

// TestGroupQuotaDrainingAndParkedDenyNewCalls：收尾态与冻结态都拒绝新的预扣。
//
// 在途预扣照常结算 —— 停的是「开新活」，不是「把已经在跑的掐掉」。
func TestGroupQuotaDrainingAndParkedDenyNewCalls(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer d.Close()

	gid := quotaTestGroup(t, d)
	if err := d.SetGroupQuotaBudget(gid, 100000); err != nil {
		t.Fatalf("设预算: %v", err)
	}

	for _, st := range []QuotaState{QuotaStateDraining, QuotaStateParked} {
		if err := d.SetGroupQuotaState(gid, st); err != nil {
			t.Fatalf("设状态 %s: %v", st, err)
		}
		if !QuotaStateDeniesNewCalls(st) {
			t.Fatalf("%s 应当拒绝新的模型调用", st)
		}
		if _, err := d.ReserveGroupQuota(gid, QuotaReservation{
			RequestID: fmt.Sprintf("deny-%s-%d", st, time.Now().UnixNano()),
			Tokens:    100,
		}); err == nil {
			t.Fatalf("状态 %s 下新的预扣被放行了", st)
		}
	}
}

// TestGroupQuotaWithoutBudgetFailsClosed：没有预算行 ⇒ 拒绝。
//
// 零值状态（unset）对**安全闸门**不算拒绝态，但对**模型调用路径**等于「没有预算」
// ⇒ 拒绝。两条路径问的问题不同，取舍也不同，但都不许「没配置就放行」。
func TestGroupQuotaWithoutBudgetFailsClosed(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer d.Close()

	gid := quotaTestGroup(t, d)
	if _, err := d.ReserveGroupQuota(gid, QuotaReservation{
		RequestID: "no-budget", Tokens: 100,
	}); err == nil {
		t.Fatalf("没有预算行时预扣被放行了（应当拒绝）")
	} else if !errors.Is(err, ErrQuotaNoBudget) {
		t.Fatalf("拒绝原因不对: %v", err)
	}
}

// TestGroupQuotaDuplicateRequestIsNotDoubleCharged：同一个 request_id 预扣两次
// ⇒ 第二次被拒，且**不**双扣预算（模型调用重试是常态，双扣等于预算被重试吃掉）。
func TestGroupQuotaDuplicateRequestIsNotDoubleCharged(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer d.Close()

	gid := quotaTestGroup(t, d)
	if err := d.SetGroupQuotaBudget(gid, 100000); err != nil {
		t.Fatalf("设预算: %v", err)
	}

	req := QuotaReservation{RequestID: "same-request", Tokens: 500, InputTokens: 500}
	if _, err := d.ReserveGroupQuota(gid, req); err != nil {
		t.Fatalf("首次预扣: %v", err)
	}
	if _, err := d.ReserveGroupQuota(gid, req); !errors.Is(err, ErrQuotaDuplicateRequest) {
		t.Fatalf("重复预扣应被拒（ErrQuotaDuplicateRequest），实际: %v", err)
	}
	quota, err := d.GetGroupQuota(gid)
	if err != nil {
		t.Fatalf("读预算: %v", err)
	}
	if quota.ReservedTokens != 500 {
		t.Fatalf("重复预扣双扣了预算: %d != 500", quota.ReservedTokens)
	}
}

// TestGroupQuotaSweeperReclaimsExpiredReservation：孤儿预扣（模型调用死了）
// 必须能被清扫回收，否则预算会被死掉的调用永久占住。
func TestGroupQuotaSweeperReclaimsExpiredReservation(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer d.Close()

	gid := quotaTestGroup(t, d)
	if err := d.SetGroupQuotaBudget(gid, 100000); err != nil {
		t.Fatalf("设预算: %v", err)
	}

	if _, err := d.ReserveGroupQuota(gid, QuotaReservation{
		RequestID: "orphan", Tokens: 700, InputTokens: 700,
		TTL: time.Millisecond,
	}); err != nil {
		t.Fatalf("预扣: %v", err)
	}
	time.Sleep(50 * time.Millisecond)

	n, err := d.SweepExpiredQuotaReservations(time.Now(), 100)
	if err != nil {
		t.Fatalf("清扫孤儿预扣: %v", err)
	}
	if n < 1 {
		t.Fatalf("过期预扣没有被清扫")
	}
	quota, err := d.GetGroupQuota(gid)
	if err != nil {
		t.Fatalf("读预算: %v", err)
	}
	if quota.ReservedTokens != 0 {
		t.Fatalf("清扫后预扣仍占着预算: %d", quota.ReservedTokens)
	}
}

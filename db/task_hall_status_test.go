package db

import (
	"testing"
	"time"
)

// 任务大厅状态语义（proposal §5 第 2 条）的验收用例。
//
// 这些用例**不碰数据库**：它们钉住的是判定规则本身（同样的事实永远得到同样的
// 状态）。事实采集那半边由 db/task_hall_status_db_test.go 覆盖。
//
// ⚠️ 判定里出现的每个常量（紧张阈值 = 预算/10）都是代码常量 ⇒ 测试里**写死
// 期望值**，不复用被测常量 —— 否则常量被改错时测试会跟着一起错，等于没测。

// statusTestNow 是判定时刻：2026-11-03 10:00（Asia/Shanghai）。
//
// 选 10:00 而不是 00:00：周期已过 10 小时，燃烧率好算；午夜那一秒已过时间是
// 0，燃烧率无意义。
func statusTestNow() time.Time {
	return time.Date(2026, 11, 3, 10, 0, 0, 0, quotaLocation)
}

// statusTestQuota 造一份额度读数。
func statusTestQuota(budget, consumed, reserved int64, state QuotaState) *GroupQuota {
	return &GroupQuota{
		GroupID:         1,
		State:           state,
		BudgetTokens:    budget,
		PeriodKind:      QuotaPeriodDaily,
		PeriodKey:       QuotaPeriodKey(statusTestNow()),
		ConsumedTokens:  consumed,
		ReservedTokens:  reserved,
		RemainingTokens: budget - consumed - reserved,
	}
}

func TestComputeTaskHallStatusQuotaLevels(t *testing.T) {
	cases := []struct {
		name      string
		quota     *GroupQuota
		wantLevel string
		wantCode  string
		wantOver  string
	}{
		{
			name:      "没有额度行是 none 而不是 unknown",
			quota:     nil,
			wantLevel: TaskHallQuotaNone,
			wantCode:  "no_budget_row",
			wantOver:  TaskHallStatusAttention,
		},
		{
			name:      "额度充足",
			quota:     statusTestQuota(1000, 100, 0, QuotaStateActive),
			wantLevel: TaskHallQuotaOK,
			wantCode:  "ok",
			wantOver:  TaskHallStatusOK,
		},
		{
			name:      "剩余低于阈值 10% 即紧张",
			quota:     statusTestQuota(1000, 950, 0, QuotaStateActive),
			wantLevel: TaskHallQuotaTight,
			wantCode:  "below_tight_threshold",
			wantOver:  TaskHallStatusAttention,
		},
		{
			name:      "剩余正好等于阈值仍然算紧张",
			quota:     statusTestQuota(1000, 900, 0, QuotaStateActive),
			wantLevel: TaskHallQuotaTight,
			wantCode:  "below_tight_threshold",
			wantOver:  TaskHallStatusAttention,
		},
		{
			name:      "额度用尽",
			quota:     statusTestQuota(1000, 1000, 0, QuotaStateActive),
			wantLevel: TaskHallQuotaExhausted,
			wantCode:  "budget_exhausted",
			wantOver:  TaskHallStatusExhausted,
		},
		{
			name:      "预算为 0 是未开通而不是无限",
			quota:     statusTestQuota(0, 0, 0, QuotaStateActive),
			wantLevel: TaskHallQuotaExhausted,
			wantCode:  "no_budget",
			wantOver:  TaskHallStatusExhausted,
		},
		{
			name:      "在途预扣也吃掉额度",
			quota:     statusTestQuota(1000, 950, 100, QuotaStateActive),
			wantLevel: TaskHallQuotaExhausted,
			wantCode:  "budget_exhausted",
			wantOver:  TaskHallStatusExhausted,
		},
		{
			name:      "收尾中",
			quota:     statusTestQuota(1000, 100, 0, QuotaStateDraining),
			wantLevel: TaskHallQuotaDraining,
			wantCode:  "quota_draining",
			wantOver:  TaskHallStatusAttention,
		},
		{
			name:      "冻结优先于余额",
			quota:     statusTestQuota(1000, 100, 0, QuotaStateParked),
			wantLevel: TaskHallQuotaParked,
			wantCode:  "quota_parked",
			wantOver:  TaskHallStatusHalted,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			facts := TaskHallFacts{
				GroupID:          7,
				TaskID:           9,
				GroupStatus:      "active",
				ConcurrencyLimit: 3,
				Quota:            tc.quota,
				Now:              statusTestNow(),
			}
			st := ComputeTaskHallStatus(facts)
			if st.Quota.Level != tc.wantLevel {
				t.Errorf("额度档位 = %q，期望 %q（reason=%s）", st.Quota.Level, tc.wantLevel, st.Quota.Reason)
			}
			if st.Quota.ReasonCode != tc.wantCode {
				t.Errorf("额度原因码 = %q，期望 %q", st.Quota.ReasonCode, tc.wantCode)
			}
			if st.Level != tc.wantOver {
				t.Errorf("整体档位 = %q，期望 %q", st.Level, tc.wantOver)
			}
			if st.Summary == "" {
				t.Error("整体状态必须带一句人话")
			}
		})
	}
}

func TestComputeTaskHallStatusConcurrency(t *testing.T) {
	cases := []struct {
		name        string
		limit       int
		slots       []TaskHallSlot
		wantLevel   string
		wantOver    string
		wantFree    int
		wantRunning int
	}{
		{
			name:      "空闲",
			limit:     3,
			slots:     nil,
			wantLevel: TaskHallConcurrencyFree,
			wantOver:  TaskHallStatusOK,
			wantFree:  3,
		},
		{
			name:        "有占用但还有空位",
			limit:       3,
			slots:       []TaskHallSlot{{AgentKey: "auto", Intents: 1}},
			wantLevel:   TaskHallConcurrencyOccupied,
			wantOver:    TaskHallStatusOK,
			wantFree:    2,
			wantRunning: 1,
		},
		{
			name:        "占满",
			limit:       3,
			slots:       []TaskHallSlot{{AgentKey: "auto", Intents: 2}, {AgentKey: "pentest", Intents: 1}},
			wantLevel:   TaskHallConcurrencySaturated,
			wantOver:    TaskHallStatusAttention,
			wantFree:    0,
			wantRunning: 3,
		},
		{
			name:        "超额占用（认领闸门与读数不一致时）仍然算占满",
			limit:       1,
			slots:       []TaskHallSlot{{AgentKey: "auto", Intents: 2}},
			wantLevel:   TaskHallConcurrencySaturated,
			wantOver:    TaskHallStatusAttention,
			wantFree:    0,
			wantRunning: 2,
		},
		{
			name:        "没有上限就是读不出来",
			limit:       0,
			slots:       []TaskHallSlot{{AgentKey: "auto", Intents: 1}},
			wantLevel:   TaskHallConcurrencyUnknown,
			wantOver:    TaskHallStatusAttention,
			wantFree:    0,
			wantRunning: 1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			facts := TaskHallFacts{
				GroupID:          7,
				TaskID:           9,
				GroupStatus:      "active",
				ConcurrencyLimit: tc.limit,
				Quota:            statusTestQuota(1000, 100, 0, QuotaStateActive),
				Slots:            tc.slots,
				Now:              statusTestNow(),
			}
			st := ComputeTaskHallStatus(facts)
			if st.Concurrency.Level != tc.wantLevel {
				t.Errorf("并发档位 = %q，期望 %q", st.Concurrency.Level, tc.wantLevel)
			}
			if st.Level != tc.wantOver {
				t.Errorf("整体档位 = %q，期望 %q", st.Level, tc.wantOver)
			}
			if st.Concurrency.Running != tc.wantRunning {
				t.Errorf("在跑数 = %d，期望 %d", st.Concurrency.Running, tc.wantRunning)
			}
			if st.Concurrency.FreeSlots != tc.wantFree {
				t.Errorf("空位数 = %d，期望 %d", st.Concurrency.FreeSlots, tc.wantFree)
			}
		})
	}
}

func TestComputeTaskHallStatusProjectedExhaustion(t *testing.T) {
	// 燃烧率 = 900 token / 600 分钟 = 1.5 token/分；剩余 100 ⇒ 约 67 分钟后用尽
	// ⇒ 落在同一个自然日内 ⇒ 紧张。
	within := ComputeTaskHallStatus(TaskHallFacts{
		GroupID:          1,
		TaskID:           2,
		GroupStatus:      "active",
		ConcurrencyLimit: 3,
		Quota:            statusTestQuota(1000, 900, 0, QuotaStateActive),
		Now:              statusTestNow(),
	})
	if within.Quota.ProjectedExhaustion == nil {
		t.Fatal("有消耗就必须给出耗尽预测")
	}
	if !within.Quota.ProjectedBeforeReset {
		t.Error("耗尽预测落在本周期内 ⇒ ProjectedBeforeReset 必须为真")
	}
	if within.Quota.Level != TaskHallQuotaTight {
		t.Errorf("周期内预计耗尽 ⇒ 档位应为 tight，实际 %q", within.Quota.Level)
	}
	// 原因码取「剩余正好等于安全线」这条**更直接**的信号。
	//
	// 为什么不是 projected_exhaustion：本用例的额度是 statusTestQuota(1000, 900, 0)
	// ⇒ 剩余 100 == 安全线（预算/10）⇒ 与「剩余正好等于阈值仍然算紧张」那个用例
	// **输入完全相同** ⇒ 两个用例对同一输入不能给出两个不同的原因码。
	// 「投影会在本周期内耗尽」这个信号没有丢：它由 ProjectedExhaustion /
	// ProjectedBeforeReset 两个字段单独暴露（上面已断言），原因码只负责回答
	// 「现在该做什么」。
	if within.Quota.ReasonCode != "below_tight_threshold" {
		t.Errorf("原因码 = %q，期望 below_tight_threshold（剩余正好等于安全线）", within.Quota.ReasonCode)
	}

	// 同样的消耗量发生在 23:00：燃烧率 100/1380 ≈ 0.0725，剩余 900 ⇒ 约 12 天
	// ⇒ 跨过零点 ⇒ 额度会重置 ⇒ 不是问题。
	late := time.Date(2026, 11, 3, 23, 0, 0, 0, quotaLocation)
	cross := ComputeTaskHallStatus(TaskHallFacts{
		GroupID:          1,
		TaskID:           2,
		GroupStatus:      "active",
		ConcurrencyLimit: 3,
		Quota: &GroupQuota{
			State:           QuotaStateActive,
			BudgetTokens:    1000,
			ConsumedTokens:  100,
			RemainingTokens: 900,
			PeriodKey:       QuotaPeriodKey(late),
		},
		Now: late,
	})
	if cross.Quota.ProjectedExhaustion == nil {
		t.Fatal("有消耗就必须给出耗尽预测")
	}
	if cross.Quota.ProjectedBeforeReset {
		t.Error("耗尽预测跨过零点 ⇒ ProjectedBeforeReset 必须为假")
	}
	if cross.Quota.Level != TaskHallQuotaOK {
		t.Errorf("跨周期耗尽不构成本周期风险 ⇒ 档位应为 ok，实际 %q", cross.Quota.Level)
	}

	// 没有任何消耗 ⇒ 推不出来（不是 0，也不是无穷）。
	none := ComputeTaskHallStatus(TaskHallFacts{
		GroupID:          1,
		TaskID:           2,
		GroupStatus:      "active",
		ConcurrencyLimit: 3,
		Quota:            statusTestQuota(1000, 0, 0, QuotaStateActive),
		Now:              statusTestNow(),
	})
	if none.Quota.ProjectedExhaustion != nil {
		t.Error("没有消耗就不该有耗尽预测")
	}
	if none.Quota.BurnRatePerMinute != 0 {
		t.Errorf("没有消耗时燃烧率应为 0，实际 %v", none.Quota.BurnRatePerMinute)
	}
}

func TestComputeTaskHallStatusOverallPrecedence(t *testing.T) {
	base := TaskHallFacts{
		GroupID:          1,
		TaskID:           2,
		GroupStatus:      "active",
		ConcurrencyLimit: 3,
		Quota:            statusTestQuota(1000, 100, 0, QuotaStateActive),
		Now:              statusTestNow(),
	}

	// 有成员卡住 ⇒ 需要人看一眼。
	blocked := base
	blocked.BlockedMembers = []string{"pentest"}
	st := ComputeTaskHallStatus(blocked)
	if st.Level != TaskHallStatusAttention {
		t.Errorf("成员卡住 ⇒ 档位应为 attention，实际 %q", st.Level)
	}

	// 并发占满 ⇒ 需要人看一眼。
	sat := base
	sat.Slots = []TaskHallSlot{{AgentKey: "auto", Intents: 3}}
	st = ComputeTaskHallStatus(sat)
	if st.Level != TaskHallStatusAttention {
		t.Errorf("并发占满 ⇒ 档位应为 attention，实际 %q", st.Level)
	}

	// 额度用尽 ⇒ 优先于并发占满（开不了活比排不上队更致命）。
	exhausted := sat
	exhausted.Quota = statusTestQuota(1000, 1000, 0, QuotaStateActive)
	st = ComputeTaskHallStatus(exhausted)
	if st.Level != TaskHallStatusExhausted {
		t.Errorf("额度用尽 ⇒ 档位应为 exhausted，实际 %q", st.Level)
	}

	// 群已关闭 ⇒ 停。
	closed := base
	closed.GroupStatus = "closed"
	st = ComputeTaskHallStatus(closed)
	if st.Level != TaskHallStatusHalted {
		t.Errorf("群已关闭 ⇒ 档位应为 halted，实际 %q", st.Level)
	}

	// 一切正常。
	st = ComputeTaskHallStatus(base)
	if st.Level != TaskHallStatusOK {
		t.Errorf("一切正常 ⇒ 档位应为 ok，实际 %q", st.Level)
	}
	if st.Summary == "" {
		t.Error("正常态也要有一句话")
	}
}

func TestQuotaTightThreshold(t *testing.T) {
	cases := []struct {
		budget int64
		want   int64
	}{
		{budget: 1000, want: 100},
		{budget: 999, want: 99},
		{budget: 10, want: 1},
		{budget: 5, want: 1},
		{budget: 1, want: 1},
		{budget: 0, want: 0},
	}
	for _, tc := range cases {
		if got := quotaTightThreshold(tc.budget); got != tc.want {
			t.Errorf("quotaTightThreshold(%d) = %d，期望 %d", tc.budget, got, tc.want)
		}
	}
}

func TestQuotaPeriodStartMatchesPeriodKey(t *testing.T) {
	// 周期起点与周期键必须是同一套口径：起点落在哪个自然日，键就是哪天。
	for _, now := range []time.Time{
		time.Date(2026, 11, 3, 0, 0, 1, 0, quotaLocation),
		time.Date(2026, 11, 3, 23, 59, 59, 0, quotaLocation),
		time.Date(2026, 1, 1, 8, 0, 0, 0, quotaLocation),
	} {
		start := quotaPeriodStart(now)
		if got := QuotaPeriodKey(start); got != QuotaPeriodKey(now) {
			t.Errorf("周期起点 %s 的键 %s 与时刻 %s 的键 %s 不一致",
				start, got, now, QuotaPeriodKey(now))
		}
		if !quotaPeriodEnd(now).After(now) {
			t.Errorf("周期终点 %s 必须晚于 %s", quotaPeriodEnd(now), now)
		}
	}
}

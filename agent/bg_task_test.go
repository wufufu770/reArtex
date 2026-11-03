package agent

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
	actool "github.com/Autumn-27/norma/tool"
)

// fakeBgStore 是账本的假实现：回收者与提醒的判定逻辑因此可以在不碰数据库、
// 也不碰活进程的前提下被单测。
type fakeBgStore struct {
	rows      map[int64]*db.BgTask
	nextID    int64
	insertErr error
	states    []string
	notified  []bool
}

func newFakeBgStore() *fakeBgStore {
	return &fakeBgStore{rows: map[int64]*db.BgTask{}}
}

func (f *fakeBgStore) EnsureBgTaskSchema() error { return nil }

func (f *fakeBgStore) InsertBgTask(_ context.Context, t db.BgTask) (int64, error) {
	if f.insertErr != nil {
		return 0, f.insertErr
	}
	f.nextID++
	t.ID = f.nextID
	row := t
	f.rows[t.ID] = &row
	return t.ID, nil
}

func (f *fakeBgStore) UpdateBgTaskState(_ context.Context, id int64, state string, _ *int, notice string, endedAt time.Time, notified bool) error {
	f.states = append(f.states, state)
	f.notified = append(f.notified, notified)
	if row, ok := f.rows[id]; ok {
		row.State = state
		row.Notice = notice
		row.EndedAt = &endedAt
	}
	return nil
}

func (f *fakeBgStore) GetBgTaskBySessionTask(_ context.Context, sessionID, sdkTaskID string) (*db.BgTask, error) {
	for _, row := range f.rows {
		if row.SessionID == sessionID && row.SdkTaskID == sdkTaskID {
			cp := *row
			return &cp, nil
		}
	}
	return nil, nil
}

func (f *fakeBgStore) ListBgTasksByOwner(_ context.Context, ownerTaskID int64) ([]db.BgTask, error) {
	out := []db.BgTask{}
	for _, row := range f.rows {
		if row.OwnerTaskID == ownerTaskID {
			out = append(out, *row)
		}
	}
	return out, nil
}

func (f *fakeBgStore) ListRunningBgTasks(_ context.Context) ([]db.BgTask, error) {
	out := []db.BgTask{}
	for _, row := range f.rows {
		if row.State == db.BgTaskStateRunning {
			out = append(out, *row)
		}
	}
	return out, nil
}

// fakeReminder 记录完成提醒的投递。
type fakeReminder struct {
	delivered []string
	err       error
}

func (f *fakeReminder) Deliver(_ context.Context, _ db.BgTask, notice string) error {
	if f.err != nil {
		return f.err
	}
	f.delivered = append(f.delivered, notice)
	return nil
}

// 回收者只判定两件事：到期没有、该不该杀。判定本身必须与数据库和活进程无关。
// durPtr 取一个 time.Duration 的地址（用例表里用它表达「这一例的 TTL 覆盖值」）。
func durPtr(d time.Duration) *time.Duration { return &d }

func TestBgTaskExpired(t *testing.T) {
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	ttl := time.Hour

	cases := []struct {
		name string
		row  db.BgTask
		want bool
		// ttl 覆盖这一例的 TTL（nil = 用共享的默认 TTL；显式 0 = 没有 TTL 这条线）。
		ttl *time.Duration
	}{
		{
			name: "任务 deadline 已过 ⇒ 回收",
			row:  db.BgTask{DeadlineUnix: now.Add(-time.Second).Unix(), StartedAt: now.Add(-10 * time.Minute)},
			want: true,
		},
		{
			name: "任务 deadline 未到 ⇒ 不回收",
			row:  db.BgTask{DeadlineUnix: now.Add(time.Minute).Unix(), StartedAt: now.Add(-10 * time.Minute)},
			want: false,
		},
		{
			name: "没有 deadline 但超过 TTL ⇒ 回收",
			row:  db.BgTask{StartedAt: now.Add(-2 * time.Hour)},
			want: true,
		},
		{
			name: "没有 deadline 也没到 TTL ⇒ 不回收",
			row:  db.BgTask{StartedAt: now.Add(-30 * time.Minute)},
			want: false,
		},
		{
			name: "TTL 为 0 ⇒ 没有 TTL 这条线（只留 deadline）",
			row:  db.BgTask{StartedAt: now.Add(-72 * time.Hour)},
			// 这一例要表达的是「TTL 为 0 ⇒ 不看 TTL」⇒ 必须真的传 0，
			// 否则它会落到共享的 1h 上（72h > 1h ⇒ 本来就该被回收），
			// 名字与断言就自相矛盾了。
			ttl:  durPtr(0),
			want: false,
		},
		{
			name: "started_at 为零 ⇒ 退回 created_at",
			row:  db.BgTask{CreatedAt: now.Add(-2 * time.Hour)},
			want: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			caseTTL := ttl
			if tc.ttl != nil {
				caseTTL = *tc.ttl
			}
			got, reason := bgTaskExpired(tc.row, now, caseTTL)
			if got != tc.want {
				t.Fatalf("bgTaskExpired = %v, want %v (reason=%q)", got, tc.want, reason)
			}
			if got && reason == "" {
				t.Fatal("判定为到期却没有给理由")
			}
		})
	}
}

// TTL 这条线在没有 deadline 时独立生效（ttl=0 时不生效）。
func TestBgTaskExpiredTTLZero(t *testing.T) {
	now := time.Now()
	row := db.BgTask{StartedAt: now.Add(-72 * time.Hour)}
	if expired, _ := bgTaskExpired(row, now, 0); expired {
		t.Fatal("TTL=0 时不应因 TTL 回收")
	}
}

// 回收者：到期 ⇒ 杀进程 + 标 lost（不是 failed）。
func TestReapKillsAndMarksLost(t *testing.T) {
	store := newFakeBgStore()
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	killed := []int{}
	reg := NewBgTaskRegistry(store,
		WithBgTaskClock(func() time.Time { return now }),
		WithBgTaskKiller(func(pid int) error {
			killed = append(killed, pid)
			return nil
		}),
	)
	_, _ = store.InsertBgTask(context.Background(), db.BgTask{
		OwnerTaskID:  7,
		OwnerKey:     "task:7",
		SessionID:    "sess-1",
		SdkTaskID:    "task_1",
		Command:      "sleep 600",
		OutputPath:   "/out/task_1.output",
		PID:          4242,
		State:        db.BgTaskStateRunning,
		DeadlineUnix: now.Add(-time.Minute).Unix(),
		StartedAt:    now.Add(-time.Hour),
	})

	res, err := reg.Reap(context.Background())
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if res.Reaped != 1 || res.Killed != 1 {
		t.Fatalf("回收结果 = %+v, want Reaped=1 Killed=1", res)
	}
	if len(killed) != 1 || killed[0] != 4242 {
		t.Fatalf("杀的进程 = %v, want [4242]", killed)
	}
	if len(store.states) != 1 || store.states[0] != db.BgTaskStateLost {
		t.Fatalf("状态 = %v, want [%s]", store.states, db.BgTaskStateLost)
	}
	row := store.rows[1]
	if row.State == db.BgTaskStateFailed {
		t.Fatal("被回收的行不能标成 failed：lost 与 failed 必须分开")
	}
	if row.Notice == "" {
		t.Fatal("lost 行必须写明回收理由")
	}
}

// 还没到期的行不能被回收。
func TestReapLeavesFreshTaskAlone(t *testing.T) {
	store := newFakeBgStore()
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	reg := NewBgTaskRegistry(store,
		WithBgTaskClock(func() time.Time { return now }),
		WithBgTaskKiller(func(int) error { t.Fatal("不该杀进程"); return nil }),
	)
	_, _ = store.InsertBgTask(context.Background(), db.BgTask{
		OwnerTaskID: 7, SessionID: "sess-1", SdkTaskID: "task_1",
		PID: 1, State: db.BgTaskStateRunning,
		DeadlineUnix: now.Add(time.Hour).Unix(),
		StartedAt:    now.Add(-time.Minute),
	})
	res, err := reg.Reap(context.Background())
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if res.Reaped != 0 || len(store.states) != 0 {
		t.Fatalf("未到期的行被回收了: %+v states=%v", res, store.states)
	}
}

// 杀不掉也要把账本闭合（否则这一行会被每一轮重复扫），但错误要看得见。
func TestReapClosesLedgerEvenWhenKillFails(t *testing.T) {
	store := newFakeBgStore()
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	reg := NewBgTaskRegistry(store,
		WithBgTaskClock(func() time.Time { return now }),
		WithBgTaskKiller(func(int) error { return errors.New("EPERM") }),
	)
	_, _ = store.InsertBgTask(context.Background(), db.BgTask{
		OwnerTaskID: 7, SessionID: "sess-1", SdkTaskID: "task_1",
		PID: 1, State: db.BgTaskStateRunning, StartedAt: now.Add(-2 * time.Hour),
	})
	res, err := reg.Reap(context.Background())
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if res.Reaped != 1 {
		t.Fatalf("Reaped = %d, want 1（账本必须闭合）", res.Reaped)
	}
	if len(res.Errors) != 1 {
		t.Fatalf("Errors = %v, want 1 条（杀不掉必须留痕）", res.Errors)
	}
}

// 记账失败 ⇒ 不持久（没有账本就没有回收者 ⇒ 持久 = 泄漏）。
func TestOnBackgroundedRefusesDurabilityWithoutLedger(t *testing.T) {
	store := newFakeBgStore()
	store.insertErr = errors.New("db down")
	reg := NewBgTaskRegistry(store)
	ctx := WithRunInfo(context.Background(), RunInfo{TaskID: 7})
	if owner := reg.OnBackgrounded(ctx, actool.TaskInfo{ID: "task_1", SessionID: "sess-1"}); owner != "" {
		t.Fatalf("记账失败时仍返回了 owner key %q —— 那会让命令持久却没人回收", owner)
	}
}

// 记账成功 ⇒ 返回 owner key（= 允许持久）。
func TestOnBackgroundedRegistersAndReturnsOwnerKey(t *testing.T) {
	store := newFakeBgStore()
	reg := NewBgTaskRegistry(store)
	ctx := WithRunInfo(context.Background(), RunInfo{TaskID: 7, ExplorationID: 3})
	owner := reg.OnBackgrounded(ctx, actool.TaskInfo{
		ID: "task_1", SessionID: "sess-1", Command: "sleep 600",
		OutputPath: "/out/task_1.output", PID: 99,
	})
	if owner != "task:7" {
		t.Fatalf("owner key = %q, want task:7", owner)
	}
	row, err := store.GetBgTaskBySessionTask(context.Background(), "sess-1", "task_1")
	if err != nil || row == nil {
		t.Fatalf("账本里没有这一行: %v %v", row, err)
	}
	if row.State != db.BgTaskStateRunning || row.PID != 99 {
		t.Fatalf("账本行 = %+v", row)
	}
}

// 没有拥有者（纯聊天会话）⇒ 不持久。
func TestOnBackgroundedWithoutOwnerIsNotDurable(t *testing.T) {
	store := newFakeBgStore()
	reg := NewBgTaskRegistry(store)
	if owner := reg.OnBackgrounded(context.Background(), actool.TaskInfo{ID: "task_1"}); owner != "" {
		t.Fatalf("没有任务归属时仍返回了 owner key %q", owner)
	}
	if len(store.rows) != 0 {
		t.Fatalf("没有任务归属时仍写了账本: %v", store.rows)
	}
}

// 完成 ⇒ 账本收尾成 done，并把完成提醒投出去。
func TestOnFinishedMarksDoneAndDeliversReminder(t *testing.T) {
	store := newFakeBgStore()
	reminder := &fakeReminder{}
	reg := NewBgTaskRegistry(store, WithBgTaskReminder(reminder))
	ctx := WithRunInfo(context.Background(), RunInfo{TaskID: 7, ExplorationID: 3})
	reg.OnBackgrounded(ctx, actool.TaskInfo{ID: "task_1", SessionID: "sess-1", Command: "echo hi", OutputPath: "/out/x"})

	reg.OnFinished(ctx, actool.TaskInfo{
		ID: "task_1", SessionID: "sess-1", Command: "echo hi", OutputPath: "/out/x",
		Status: actool.TaskCompleted, ExitCode: 0,
	})

	if len(store.states) != 1 || store.states[0] != db.BgTaskStateDone {
		t.Fatalf("状态 = %v, want [%s]", store.states, db.BgTaskStateDone)
	}
	if len(reminder.delivered) != 1 {
		t.Fatalf("完成提醒投了 %d 次, want 1", len(reminder.delivered))
	}
	if len(store.notified) != 1 || !store.notified[0] {
		t.Fatalf("提醒投递时间没记上: %v", store.notified)
	}
}

// 失败 ⇒ failed（不是 lost：它是自己挂的，不是被回收的）。
func TestOnFinishedMarksFailed(t *testing.T) {
	store := newFakeBgStore()
	reg := NewBgTaskRegistry(store, WithBgTaskReminder(&fakeReminder{}))
	ctx := WithRunInfo(context.Background(), RunInfo{TaskID: 7})
	reg.OnBackgrounded(ctx, actool.TaskInfo{ID: "task_1", SessionID: "sess-1"})
	reg.OnFinished(ctx, actool.TaskInfo{ID: "task_1", SessionID: "sess-1", Status: actool.TaskFailed, ExitCode: 3})
	if len(store.states) != 1 || store.states[0] != db.BgTaskStateFailed {
		t.Fatalf("状态 = %v, want [%s]", store.states, db.BgTaskStateFailed)
	}
}

// 跨 session 注册表：后来的 run 按任务 id 能查到先前 run 起的后台命令。
func TestListFindsTasksFromEarlierRuns(t *testing.T) {
	store := newFakeBgStore()
	reg := NewBgTaskRegistry(store)
	ctx := WithRunInfo(context.Background(), RunInfo{TaskID: 7})
	reg.OnBackgrounded(ctx, actool.TaskInfo{ID: "task_1", SessionID: "sess-old", OutputPath: "/out/old.output"})
	reg.OnBackgrounded(ctx, actool.TaskInfo{ID: "task_1", SessionID: "sess-new", OutputPath: "/out/new.output"})

	rows, err := reg.List(context.Background(), 7)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("查到 %d 行, want 2（跨 session 的两条）", len(rows))
	}
	sessions := map[string]bool{}
	for _, row := range rows {
		sessions[row.SessionID] = true
	}
	if !sessions["sess-old"] || !sessions["sess-new"] {
		t.Fatalf("两个 session 都要在: %v", sessions)
	}
}

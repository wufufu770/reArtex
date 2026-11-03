// 任务大厅的**状态语义**（proposal §5 第 2 条：「额度进度条」升级为状态语义）。
//
// 为什么要有这一层：大厅详情页原来给的是一根「已消费 / 在途预扣 / 剩余」的进度条
// 加三个数字。数字要人自己心算才知道「还能不能开工」，而且**没有回答最要紧的
// 那个问题**：现在谁占着执行位、额度什么时候会见底。本文件把这些问题变成
// **确定性状态**：同样的事实永远得到同样的状态，不依赖模型、不依赖前端心算。
//
// 三条纪律：
//
//	① **确定性层裁决**：状态由纯函数从事实算出（ComputeTaskHallStatus 是纯函数，
//	   不碰 DB、不看时钟），模型不参与。
//	② **读不到就是读不到**：事实缺失时状态是 unknown 而不是乐观的 ok ——
//	   fail-closed（§8 红线第 4 条）。
//	③ **数字仍在，但状态先行**：状态是给人看的（徽标 / 一句话），数字是给账看的。
//	   状态语义不替代数字，它把数字翻译成「现在该做什么」。
//
// 与 §4.4 的关系：大厅 = kind='task_hall' 的会话 ⇒ 状态语义挂在**会话**上，
// 会话列表与会话详情共用同一份状态（见 db/task_hall_status.go 的
// TaskHallStatusBySession）。

package db

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// ---------- 状态取值（代码常量，不是可热改的 DB/settings） ----------

// 额度状态的取值。
//
// 为什么是这几档而不是一个百分比：百分比需要人自己翻译（10% 到底是多不多？），
// 状态直接给结论（"紧张" / "已用尽"）。百分比仍在 QuotaStatus.UsedRatio 里，
// 供账目与告警使用。
const (
	// TaskHallQuotaUnknown：额度事实读不出来（调用方手工拼事实时可能出现；
	// DB 路径读不出来是**报错**而不是返回 unknown —— fail-closed）。
	TaskHallQuotaUnknown = "unknown"
	// TaskHallQuotaNone：**没有额度行**（未开通额度）。
	//
	// 为什么单独一档而不是并进 unknown：本平台没有「不限量」这个取值 ——
	// 没有额度行时预扣一律被拒（db/quota.go 的 ReserveGroupQuota）⇒ 这不是
	// 「不知道」，而是「知道额度是 0」。把它并进 unknown 会让人以为状态读不出来。
	TaskHallQuotaNone = "none"
	// TaskHallQuotaOK：额度充足，周期内预计不会用尽。
	TaskHallQuotaOK = "ok"
	// TaskHallQuotaTight：额度紧张 —— 剩余不足阈值，或预计在周期重置前用尽。
	TaskHallQuotaTight = "tight"
	// TaskHallQuotaExhausted：额度已用尽（剩余 ≤ 0），新调用会被拒。
	TaskHallQuotaExhausted = "exhausted"
	// TaskHallQuotaDraining：额度在收尾（不再发起新调用，在途可结算）。
	TaskHallQuotaDraining = "draining"
	// TaskHallQuotaParked：额度已冻结（完全停下）。
	TaskHallQuotaParked = "parked"
)

// 并发状态的取值。
const (
	// TaskHallConcurrencyUnknown：并发上限读不出来。
	TaskHallConcurrencyUnknown = "unknown"
	// TaskHallConcurrencyFree：没有执行位被占用。
	TaskHallConcurrencyFree = "free"
	// TaskHallConcurrencyOccupied：有执行位被占用，但还有空位。
	TaskHallConcurrencyOccupied = "occupied"
	// TaskHallConcurrencySaturated：执行位已占满，新意图认领会被拒。
	TaskHallConcurrencySaturated = "saturated"
)

// 大厅整体状态的取值（前端徽标用这一档）。
const (
	// TaskHallStatusOK：一切正常。
	TaskHallStatusOK = "ok"
	// TaskHallStatusAttention：需要人看一眼（额度紧张 / 并发占满 / 有成员卡住）。
	TaskHallStatusAttention = "attention"
	// TaskHallStatusExhausted：额度用尽，开不了新活。
	TaskHallStatusExhausted = "exhausted"
	// TaskHallStatusHalted：已停（冻结 / 关闭 / 失败）。
	TaskHallStatusHalted = "halted"
	// TaskHallStatusUnknown：状态算不出来（事实缺失）。
	TaskHallStatusUnknown = "unknown"
)

const (
	// TaskHallQuotaTightDivisor 是「额度紧张」的阈值分母：剩余 ≤ 预算/10 即紧张。
	//
	// 为什么用整数除法而不是浮点比例：预算本身是整数 token，除出来的阈值也是
	// 整数 ⇒ 同一个预算在任何机器上得到同一个阈值（可复现、可对账）。
	TaskHallQuotaTightDivisor = 10
	// TaskHallQuotaTightFloor 是阈值的下限：预算很小时（< 10 token）至少留 1。
	//
	// 没有下限的话，预算 5 token 时阈值是 0 ⇒ 「剩余 0」已经算 exhausted，
	// 于是 tight 这一档永远不出现 ⇒ 状态少了一档。
	TaskHallQuotaTightFloor = 1
)

// ErrTaskHallNotFound 表示群组（任务大厅）不存在。
var ErrTaskHallNotFound = errors.New("任务大厅不存在")

// ---------- 事实与状态 ----------

// TaskHallSlot 是一个执行位的占用者。
//
// 一个 agent 可以同时持有多个执行位（它在跑多个 intent）⇒ Slots 是按 agent
// 聚合的：**执行位个数** = 各 Slots.Intents 之和，而**占位的人** = 各
// Slots.AgentKey。这两个数不一样，所以必须分开报（"谁占着"和"占了几格"）。
type TaskHallSlot struct {
	AgentKey string `json:"agent_key"`
	Intents  int    `json:"intents"`
}

// TaskHallFacts 是一个大厅的**事实**快照 —— 只有读出来的东西，没有判断。
//
// 之所以把「事实」与「判断」分成两个类型：判断必须是**纯函数**，才能被单元测试
// 穷举；事实由 DB 读，读不出来就是读不出来（不许猜）。
type TaskHallFacts struct {
	// GroupID / TaskID 是这个大厅的身份。
	GroupID int64
	TaskID  int64
	// GroupStatus 是 chat_groups.status（draft/active/draining/parked/...）。
	GroupStatus string
	// ConcurrencyLimit 是组内并发上限（分母）。
	ConcurrencyLimit int
	// Quota 是当前周期的额度读数；nil = 这个群没配额度（不是"额度为 0"）。
	Quota *GroupQuota
	// Slots 是当前占着执行位的人。
	Slots []TaskHallSlot
	// BlockedMembers 是 presence='blocked' 的成员（谁卡住了）。
	BlockedMembers []string
	// Now 是判定时刻（由调用方传入 ⇒ 判定可复现）。
	Now time.Time
}

// QuotaStatus 是额度的状态语义。
type QuotaStatus struct {
	// Level 是状态档位（TaskHallQuota*）。
	Level string `json:"level"`
	// ReasonCode 是机器可读的原因码。
	ReasonCode string `json:"reason_code"`
	// Reason 是一句人话（前端直接显示）。
	Reason string `json:"reason"`

	BudgetTokens    int64 `json:"budget_tokens"`
	ConsumedTokens  int64 `json:"consumed_tokens"`
	ReservedTokens  int64 `json:"reserved_tokens"`
	RemainingTokens int64 `json:"remaining_tokens"`
	// UsedRatio 是「已结算 + 在途预扣」占预算的比例（0..1，可能 > 1）。
	UsedRatio float64 `json:"used_ratio"`
	// QuotaState 是额度状态机的原始状态（active/draining/parked）。
	QuotaState string `json:"quota_state"`
	// PeriodKey 是当前周期键（自然日，Asia/Shanghai）。
	PeriodKey string `json:"period_key"`

	// BurnRatePerMinute 是燃烧率：已结算 token / 周期已过分钟数。
	//
	// 没有消耗时为 0 ⇒ 无法预测耗尽时间（ProjectedExhaustion 为 nil）。
	BurnRatePerMinute float64 `json:"burn_rate_per_minute"`
	// ProjectedExhaustion 是按当前燃烧率推算的耗尽时刻；nil = 推不出来。
	ProjectedExhaustion *time.Time `json:"projected_exhaustion,omitempty"`
	// ProjectedBeforeReset 说的是「会不会在周期重置之前就烧完」。
	//
	// 这是唯一有意义的问法：预算是**按自然日重置**的，跨过零点额度就回来了 ⇒
	// 「预计明天才烧完」不是问题，「今天之内烧完」才是问题。
	ProjectedBeforeReset bool `json:"projected_before_reset"`
}

// ConcurrencyStatus 是并发占用的状态语义。
type ConcurrencyStatus struct {
	Level      string `json:"level"`
	ReasonCode string `json:"reason_code"`
	Reason     string `json:"reason"`

	// Limit 是并发上限（分母）。
	Limit int `json:"limit"`
	// Running 是当前在跑的 intent 数（占用了几个执行位）。
	Running int `json:"running"`
	// FreeSlots 是还剩几个空位。
	FreeSlots int `json:"free_slots"`
	// Slots 是占位的人（谁占着）。
	Slots []TaskHallSlot `json:"slots"`
}

// TaskHallStatus 是大厅的整体状态语义。
type TaskHallStatus struct {
	GroupID int64 `json:"group_id"`
	TaskID  int64 `json:"task_id"`
	// GroupStatus 是 chat_groups.status 的原始值。
	GroupStatus string `json:"group_status"`
	// Level 是整体档位（TaskHallStatus*）。
	Level string `json:"level"`
	// Summary 是一句话人话（前端徽标旁的说明）。
	Summary string `json:"summary"`

	Quota       QuotaStatus       `json:"quota"`
	Concurrency ConcurrencyStatus `json:"concurrency"`
	// BlockedMembers 是卡住的成员。
	BlockedMembers []string `json:"blocked_members"`
	// ComputedAt 是判定时刻。
	ComputedAt time.Time `json:"computed_at"`
}

// ---------- 判定（纯函数） ----------

// ComputeTaskHallStatus 把事实算成状态。
//
// **这是纯函数**：不碰 DB、不看时钟、不看全局变量 ⇒ 同样的 facts 永远得到同样的
// status ⇒ 可以穷举测试（db/task_hall_status_test.go）。
//
// 判定顺序（每一步都写死，顺序本身就是语义）：
//
//	额度：parked > draining > exhausted > tight > ok > unknown
//	      （状态机状态优先于余额：冻结的群哪怕还有余额也是停着的；余额用尽的群
//	       在 active 下才是 exhausted）
//	并发：saturated > occupied > free > unknown
//	整体：exhausted > halted > attention > ok > unknown
func ComputeTaskHallStatus(f TaskHallFacts) TaskHallStatus {
	now := f.Now
	if now.IsZero() {
		now = time.Now()
	}
	st := TaskHallStatus{
		GroupID:        f.GroupID,
		TaskID:         f.TaskID,
		GroupStatus:    f.GroupStatus,
		ComputedAt:     now,
		BlockedMembers: sortedCopy(f.BlockedMembers),
	}
	st.Quota = computeQuotaStatus(f.Quota, now)
	st.Concurrency = computeConcurrencyStatus(f.ConcurrencyLimit, f.Slots)
	st.Level, st.Summary = computeOverallLevel(st)
	return st
}

// computeQuotaStatus 把额度读数算成额度状态。
func computeQuotaStatus(q *GroupQuota, now time.Time) QuotaStatus {
	if q == nil {
		return QuotaStatus{
			Level:      TaskHallQuotaNone,
			ReasonCode: "no_budget_row",
			Reason:     "未开通额度，模型调用会被拒",
		}
	}
	st := QuotaStatus{
		Level:           TaskHallQuotaOK,
		ReasonCode:      "ok",
		BudgetTokens:    q.BudgetTokens,
		ConsumedTokens:  q.ConsumedTokens,
		ReservedTokens:  q.ReservedTokens,
		RemainingTokens: q.RemainingTokens,
		QuotaState:      string(q.State),
		PeriodKey:       q.PeriodKey,
	}
	if q.BudgetTokens > 0 {
		st.UsedRatio = float64(q.ConsumedTokens+q.ReservedTokens) / float64(q.BudgetTokens)
	}

	// 燃烧率与耗尽预测。
	//
	// 周期起点 = 本地零点（与 db/quota.go 的 QuotaPeriodKey 同一口径：固定
	// UTC+8，自然日）。周期已过时间 = now - 周期起点。
	start := quotaPeriodStart(now)
	elapsed := now.Sub(start)
	if elapsed > 0 && q.ConsumedTokens > 0 {
		minutes := elapsed.Minutes()
		if minutes > 0 {
			st.BurnRatePerMinute = float64(q.ConsumedTokens) / minutes
		}
	}
	if st.BurnRatePerMinute > 0 && q.RemainingTokens > 0 {
		d := time.Duration(float64(q.RemainingTokens) / st.BurnRatePerMinute * float64(time.Minute))
		if d > 0 {
			proj := now.Add(d)
			st.ProjectedExhaustion = &proj
			st.ProjectedBeforeReset = proj.Before(quotaPeriodEnd(now))
		}
	}

	switch {
	case q.State == QuotaStateParked:
		st.Level = TaskHallQuotaParked
		st.ReasonCode = "quota_parked"
		st.Reason = "额度已冻结，调用全部被拒"
	case q.State == QuotaStateDraining:
		st.Level = TaskHallQuotaDraining
		st.ReasonCode = "quota_draining"
		st.Reason = "额度收尾中，不再发起新调用"
	case q.BudgetTokens <= 0:
		// budget=0 的语义是「没有可用预算」（db/quota.go 的 ReserveGroupQuota：
		// 预扣一律拒绝）⇒ 与用尽同义，不是「不限量」。
		st.Level = TaskHallQuotaExhausted
		st.ReasonCode = "no_budget"
		st.Reason = "未开通额度，调用会被拒"
	case q.RemainingTokens <= 0:
		st.Level = TaskHallQuotaExhausted
		st.ReasonCode = "budget_exhausted"
		st.Reason = "额度已用尽，新调用会被拒"
	case q.RemainingTokens <= quotaTightThreshold(q.BudgetTokens):
		// 顺序按本文件自己写的档位优先级（exhausted > tight > ok）：
		// 「剩余已低于安全线」比「按当前速率预计会耗尽」更直接、更可行动 ⇒ 先判它。
		// （否则剩余明明已经低于阈值，却报成 projected_exhaustion，前端给出的
		//   提示会指向一个还没发生的事。）
		st.Level = TaskHallQuotaTight
		st.ReasonCode = "below_tight_threshold"
		st.Reason = "剩余额度已低于安全线"
	case st.ProjectedBeforeReset:
		st.Level = TaskHallQuotaTight
		st.ReasonCode = "projected_exhaustion"
		st.Reason = "按当前速率，额度会在本周期内耗尽"
	default:
		st.Level = TaskHallQuotaOK
		st.ReasonCode = "ok"
		st.Reason = "额度充足"
	}
	return st
}

// quotaTightThreshold 是「紧张」的阈值：预算 / TaskHallQuotaTightDivisor，
// 下限 TaskHallQuotaTightFloor。
func quotaTightThreshold(budget int64) int64 {
	if budget <= 0 {
		return 0
	}
	t := budget / TaskHallQuotaTightDivisor
	if t < TaskHallQuotaTightFloor {
		t = TaskHallQuotaTightFloor
	}
	return t
}

// computeConcurrencyStatus 把占位情况算成并发状态。
func computeConcurrencyStatus(limit int, slots []TaskHallSlot) ConcurrencyStatus {
	running := 0
	for _, s := range slots {
		running += s.Intents
	}
	st := ConcurrencyStatus{
		Limit:   limit,
		Running: running,
		Slots:   slots,
	}
	if limit > 0 {
		st.FreeSlots = limit - running
		if st.FreeSlots < 0 {
			st.FreeSlots = 0
		}
	}
	switch {
	case limit <= 0:
		st.Level = TaskHallConcurrencyUnknown
		st.ReasonCode = "no_limit"
		st.Reason = "未配置并发上限"
	case running >= limit:
		st.Level = TaskHallConcurrencySaturated
		st.ReasonCode = "saturated"
		st.Reason = fmt.Sprintf("执行位已占满（%d/%d）", running, limit)
	case running == 0:
		st.Level = TaskHallConcurrencyFree
		st.ReasonCode = "free"
		st.Reason = "当前没有执行位被占用"
	default:
		st.Level = TaskHallConcurrencyOccupied
		st.ReasonCode = "occupied"
		st.Reason = fmt.Sprintf("%d/%d 个执行位被占用", running, limit)
	}
	return st
}

// computeOverallLevel 把额度、并发、成员状态合成一个整体档位。
func computeOverallLevel(st TaskHallStatus) (string, string) {
	switch st.Quota.Level {
	case TaskHallQuotaExhausted:
		return TaskHallStatusExhausted, st.Quota.Reason
	case TaskHallQuotaParked:
		return TaskHallStatusHalted, st.Quota.Reason
	}
	switch st.GroupStatus {
	case "closed", "failed":
		return TaskHallStatusHalted, taskHallGroupStatusText(st.GroupStatus)
	}
	switch st.Quota.Level {
	case TaskHallQuotaDraining, TaskHallQuotaTight, TaskHallQuotaNone:
		return TaskHallStatusAttention, st.Quota.Reason
	}
	if st.Concurrency.Level == TaskHallConcurrencySaturated {
		return TaskHallStatusAttention, st.Concurrency.Reason
	}
	if len(st.BlockedMembers) > 0 {
		return TaskHallStatusAttention, fmt.Sprintf("%s 卡住了", strings.Join(st.BlockedMembers, "、"))
	}
	if st.Concurrency.Level == TaskHallConcurrencyUnknown {
		// 并发上限读不出来（没配上限）⇒ 不能报「一切正常」：
		// **读不出来 ≠ 正常**，要人看一眼（attention）。
		return TaskHallStatusAttention, st.Concurrency.Reason
	}
	if st.Quota.Level == TaskHallQuotaUnknown && st.Concurrency.Level == TaskHallConcurrencyUnknown {
		return TaskHallStatusUnknown, "状态读不出来"
	}
	return TaskHallStatusOK, taskHallOKSummary(st)
}

// taskHallOKSummary 是「一切正常」时的一句话。
func taskHallOKSummary(st TaskHallStatus) string {
	parts := make([]string, 0, 2)
	if st.Quota.Level == TaskHallQuotaOK {
		parts = append(parts, "额度充足")
	}
	switch st.Concurrency.Level {
	case TaskHallConcurrencyFree:
		parts = append(parts, "并发空闲")
	case TaskHallConcurrencyOccupied:
		parts = append(parts, st.Concurrency.Reason)
	}
	if len(parts) == 0 {
		return "运行中"
	}
	return strings.Join(parts, " · ")
}

// taskHallGroupStatusText 把群组状态翻译成人话。
func taskHallGroupStatusText(s string) string {
	switch s {
	case "closed":
		return "大厅已关闭"
	case "failed":
		return "大厅已失败"
	case "parked":
		return "大厅已冻结"
	case "draining":
		return "大厅收尾中"
	case "draft":
		return "大厅尚未启用"
	default:
		return "大厅状态：" + s
	}
}

// quotaPeriodStart 返回给定时刻所属预算周期的起点（本地零点）。
//
// 与 db/quota.go 的 QuotaPeriodKey 同一口径（固定 UTC+8 的自然日）。
func quotaPeriodStart(t time.Time) time.Time {
	local := t.In(quotaLocation)
	return time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, quotaLocation)
}

// quotaPeriodEnd 返回给定时刻所属预算周期的终点（下一个本地零点）。
func quotaPeriodEnd(t time.Time) time.Time {
	return quotaPeriodStart(t).AddDate(0, 0, 1)
}

// sortedCopy 复制并排序一个字符串切片（让输出稳定可比对）。
func sortedCopy(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if strings.TrimSpace(s) != "" {
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

// ---------- 事实采集（DB 侧） ----------

// TaskHallStatusByGroup 读一个群组的事实并算出状态。
func (d *DB) TaskHallStatusByGroup(groupID int64, now time.Time) (*TaskHallStatus, error) {
	if d == nil {
		return nil, errors.New("数据库未连接")
	}
	if groupID <= 0 {
		return nil, fmt.Errorf("%w: %d", ErrTaskHallNotFound, groupID)
	}
	g, err := d.GetChatGroup(groupID)
	if err != nil {
		return nil, fmt.Errorf("读任务大厅失败: %w", err)
	}
	if g == nil {
		return nil, fmt.Errorf("%w: %d", ErrTaskHallNotFound, groupID)
	}
	facts, err := d.taskHallFacts(g, now)
	if err != nil {
		return nil, err
	}
	st := ComputeTaskHallStatus(*facts)
	return &st, nil
}

// taskHallFacts 采集一个群组的事实。
func (d *DB) taskHallFacts(g *ChatGroup, now time.Time) (*TaskHallFacts, error) {
	if now.IsZero() {
		now = time.Now()
	}
	facts := &TaskHallFacts{
		GroupID:          g.ID,
		TaskID:           g.TaskID,
		GroupStatus:      g.Status,
		ConcurrencyLimit: g.ConcurrencyLimit,
		Now:              now,
	}
	quota, err := d.GetGroupQuota(g.ID)
	if err != nil {
		return nil, fmt.Errorf("读群组额度失败: %w", err)
	}
	facts.Quota = quota

	slots, err := d.taskHallRunningSlots(g.TaskExpID)
	if err != nil {
		return nil, err
	}
	facts.Slots = slots

	blocked, err := d.taskHallBlockedMembers(g)
	if err != nil {
		return nil, err
	}
	facts.BlockedMembers = blocked
	return facts, nil
}

// taskHallRunningSlots 列出当前占着执行位的人。
//
// 「在跑」的口径与 server/manager.go 的 groupRunningIntents 完全一致：
// exploration_nodes 上 kind='intent' AND state='running'。口径一致很重要 ——
// 认领意图时的并发闸门用的就是它，两边算出不同的数就会让 UI 说"有空位"而
// 实际认领被拒。
func (d *DB) taskHallRunningSlots(explorationID int64) ([]TaskHallSlot, error) {
	if explorationID <= 0 {
		return []TaskHallSlot{}, nil
	}
	rows, err := d.Query(`SELECT COALESCE(owner,''), count(*)
FROM exploration_nodes
WHERE exploration_id=$1 AND kind='intent' AND state='running'
GROUP BY COALESCE(owner,'')`, explorationID)
	if err != nil {
		return nil, fmt.Errorf("统计在跑意图失败: %w", err)
	}
	defer rows.Close()
	out := make([]TaskHallSlot, 0, 4)
	for rows.Next() {
		var (
			owner   string
			intents int
		)
		if err := rows.Scan(&owner, &intents); err != nil {
			return nil, fmt.Errorf("统计在跑意图失败: %w", err)
		}
		if owner == "" {
			// 没有归属的意图也在占位，但它不是「谁」——单列一条空 agent。
			owner = "(未归属)"
		}
		out = append(out, TaskHallSlot{AgentKey: owner, Intents: intents})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("统计在跑意图失败: %w", err)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Intents != out[j].Intents {
			return out[i].Intents > out[j].Intents
		}
		return out[i].AgentKey < out[j].AgentKey
	})
	return out, nil
}

// taskHallBlockedMembers 列出卡住的成员（presence='blocked'）。
//
// 大厅的会话 id 取 chat_groups.session_id（schema_session.sql 加的列），
// 没有时回退到 conversations.session_id（迁移期兼容）。
func (d *DB) taskHallBlockedMembers(g *ChatGroup) ([]string, error) {
	if err := d.ensureSessionSchema(); err != nil {
		return nil, fmt.Errorf("应用会话 schema 失败: %w", err)
	}
	var sessionID *int64
	err := d.QueryRow(`SELECT COALESCE(g.session_id, c.session_id)
FROM chat_groups g LEFT JOIN conversations c ON c.id = g.conversation_id
WHERE g.id = $1`, g.ID).Scan(&sessionID)
	if err != nil {
		// 查不到会话 id ⇒ 不是错误：没有会话就没有 presence。
		return []string{}, nil
	}
	if sessionID == nil || *sessionID <= 0 {
		return []string{}, nil
	}
	rows, err := d.Query(`SELECT agent_key FROM session_members
WHERE session_id=$1 AND presence='blocked' AND left_at IS NULL`, *sessionID)
	if err != nil {
		return nil, fmt.Errorf("读大厅成员状态失败: %w", err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, fmt.Errorf("读大厅成员状态失败: %w", err)
		}
		out = append(out, key)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("读大厅成员状态失败: %w", err)
	}
	return out, nil
}

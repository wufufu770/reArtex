// 群组预算（token 额度）访问层：预算、周期、状态机，以及**原子预扣 → 结算**三段式。
//
// 三个必须成立的不变量（写在这里，免得被下一个人改��）：
//
//  1. **预扣是原子的**：预算检查与扣减在同一条带守卫的 UPDATE 里完成
//     （见 ReserveGroupQuota）。不存在"先读后写"的窗口 ⇒ 并发调用不会有两个
//     一起越过预算线。
//  2. **失败一律关闭（fail closed）**：查不到额度行、额度为 0、状态不是 active、
//     状态值解析不出来、DB 报错 —— 全部按"拒绝"处理。**没有**"查不到就当不限量"
//     这条退路：额度是安全边界，不是优化项。
//  3. **口径只有一处**：token 数 = input + output，缓存**已经**含在 input 里
//     （与 db/llm_usage.go 的 TotalTokensForTask 同一个口径），缓存不再另加。
//
// 状态模型（对**新的模型调用**而言）：
//
//	active   —— 正常。允许发起新的模型调用，仍受预算约束。
//	draining —— 不再发起新的模型调用；**已经预扣的在途工作可以正常结算**
//	            （结算只是记账，不是新的花费）。
//	parked   —— 完全停下：同样不再发起新的模型调用；在途预扣照常结算（账要诚实）。
//
// draining 与 parked 在**模型调用层面**的拒绝行为一致（都拒绝新的调用），区别
// 在语义：draining = "收尾中，别再开新活"，parked = "整体冻结"。两者都是安全
// 闸门的**拒绝态**（见 GroupQuotaState 的契约）。
//
// 周期模型：**自然日**（Asia/Shanghai，UTC+8）。
//
// 为什么是自然日而不是滚动窗口或按任务：平台的操作者是按"今天还能花多少"来想
// 预算的，自然日给出一个可人工核对的边界（本地零点整点重置）；滚动窗口需要存
// 锚点，"还剩 40% 的滚动 24 小时"没法向人解释清楚；按任务则没有重置点，额度只能
// 靠人工补。时区写成常量而不是取服务器本地时区：容器 TZ 随部署环境变，边界跟着
// 变就等于"预算会突然多出或少掉一天"。中国自 1991 年起不再使用夏令时，UTC+8 是
// 精确偏移，不依赖 tzdata。
//
// allow: SIZE_OK —— 状态常量、预算/周期、预扣/结算/清扫、对外访问器放在一个文件里
// 是刻意的：这套逻辑的正确性依赖"预扣守卫与结算口径写在同一个上下文里"才看得清，
// 按表拆文件会把原子性说明切成几半。
package db

import (
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

//go:embed schema_quota.sql
var quotaSchemaSQL string

// quotaSchemaReady 记录某个连接已经应用过预算 DDL。
//
// 与 chatGroupSchemaReady 同理：用包级 sync.Map 而不是 DB 上的字段（本单元不改
// db/db.go 的结构体）。键是底层 *sql.DB 指针 —— 每个测试包/实例各连各的库。
var quotaSchemaReady sync.Map

// QuotaState 是群组预算的状态。
//
// 取值只有三个，全部是**代码常量**：active / draining / parked。DB 里没有任何
// 表能新增一个状态（group_quotas.state 的 CHECK 只是镜像），状态机因此不可被
// 外部数据改写。
type QuotaState string

const (
	// QuotaStateUnset 是零值：该群组没有配置预算（没有额度行）。
	//
	// 对安全闸门的含义：**不是**拒绝态。闸门问的是"这个群是不是 draining 或
	// parked"，零值对这个问题答"不是"。但对模型调用路径，零值等于"没有预算"
	// ⇒ 拒绝（fail closed）。两条路径的取舍不同，是因为它们问的问题不同。
	QuotaStateUnset QuotaState = ""
	// QuotaStateActive 正常态：允许发起新的模型调用。
	QuotaStateActive QuotaState = "active"
	// QuotaStateDraining 收尾态：不再发起新的模型调用；在途预扣照常结算。
	QuotaStateDraining QuotaState = "draining"
	// QuotaStateParked 冻结态：不再发起新的模型调用；在途预扣照常结算。
	QuotaStateParked QuotaState = "parked"
)

// QuotaStateDeniesNewCalls 报告该状态是否拒绝**新的**模型调用。
//
// draining 与 parked 都拒绝：draining 停的是"开新活"，parked 停的是"全部"。
// 在途预扣的结算是记账，不受此限制。
func QuotaStateDeniesNewCalls(state QuotaState) bool {
	return state == QuotaStateDraining || state == QuotaStateParked
}

// QuotaPeriodDaily 是唯一支持的周期模型：自然日。
//
// 取值写在代码里并由 DB 的 CHECK 镜像（同状态的处理），避免"多一种周期"被
// 数据悄悄带进来。
const QuotaPeriodDaily = "daily"

// quotaLocation 是预算周期使用的时区：Asia/Shanghai = UTC+8。
//
// 固定偏移而不是 time.LoadLocation：容器里不一定有 tzdata，缺了就得回退，
// 回退到哪一刻就成了一句"看环境"。中国自 1991 年起无夏令时，UTC+8 是精确的。
var quotaLocation = time.FixedZone("Asia/Shanghai", 8*60*60)

// QuotaPeriodKey 返回给定时刻所属的自然日键（Asia/Shanghai），形如 2026-10-30。
//
// 周期边界 = 本地零点整点。跨零点时落进新的一行（group_quota_periods 按
// (group_id, period_key) 分桶），旧周期的在途预扣仍结算回旧周期。
func QuotaPeriodKey(t time.Time) string {
	return t.In(quotaLocation).Format("2006-01-02")
}

// QuotaReservationTTL 是预扣的在途时限：超过这个时间还没结算的预扣由清扫器回收。
//
// 取 10 分钟：一次模型调用的正常时长（长上下文 + 大 max_tokens）远小于它，
// 而"进程崩了/机器挂了"导致的永久占用必须在可接受的时间内释放。
const QuotaReservationTTL = 10 * time.Minute

// 预扣/结算阶段的错误。
//
// 这些错误的文案**刻意不含任何数字**：它们会作为 cause 拼进给用户看的拒绝消息
// 里，而额度数字只应该留在账上（拒绝消息里出现"还剩 1234 token"既没用又会
// 把内部口径抖出去）。同时它们避开供应商额度 markers 的字面（"额度不足"等），
// 免得平台自己的拒绝被误判成供应商额度耗尽。
var (
	// ErrQuotaNoBudget：群组没有预算行（或预算为 0）⇒ 拒绝。
	ErrQuotaNoBudget = errors.New("群组未配置可用预算")
	// ErrQuotaBudgetExceeded：预算不够 ⇒ 拒绝。
	ErrQuotaBudgetExceeded = errors.New("群组预算不足")
	// ErrQuotaStateNotActive：状态不是 active ⇒ 拒绝。
	ErrQuotaStateNotActive = errors.New("群组预算状态不允许发起新的模型请求")
	// ErrQuotaDuplicateRequest：同一 request_id 预扣过两次 ⇒ 拒绝（不双扣）。
	ErrQuotaDuplicateRequest = errors.New("同一模型请求已预扣过预算")
	// ErrQuotaReservationNotHeld：预扣记录不存在。
	ErrQuotaReservationNotHeld = errors.New("预扣记录不存在")
)

// 拒绝原因码：写进 group_quota_refusals.reason（机器可读，不含数字）。
const (
	QuotaRefusalBudgetExceeded = "budget_exceeded"
	QuotaRefusalStateNotActive = "state_not_active"
	QuotaRefusalNoBudget       = "no_budget"
	QuotaRefusalCheckFailed    = "check_failed"
	QuotaRefusalEstimateFailed = "estimate_failed"
	QuotaRefusalDuplicate      = "duplicate_request"
)

// QuotaReservation 是一次预扣请求。
type QuotaReservation struct {
	// RequestID 是这次模型调用的唯一标识：同一个 id 只能预扣一次（重试不双扣）。
	RequestID string
	// Tokens 是本次预扣的 token 数（估算值）。
	Tokens int64
	// InputTokens / OutputTokens 是估算的拆分：请求已发出但没有 usage 返回时，
	// 按输入估算结算（供应商已计输入侧）。
	InputTokens  int64
	OutputTokens int64
	// TaskID / ConversationID 是归属：孤儿清扫与事后对账靠它们找到这次预扣。
	TaskID         string
	ConversationID int64
	// TTL 覆盖默认的 QuotaReservationTTL。
	TTL time.Duration
}

// QuotaUsage 是一次调用的真实用量，口径与 db/llm_usage.go 一致：
// TotalTokens = InputTokens + OutputTokens，缓存已含在 InputTokens 里，不再另加。
type QuotaUsage struct {
	InputTokens  int64
	OutputTokens int64
	CacheRead    int64
	CacheWrite   int64
	TotalTokens  int64
}

// GroupQuota 是一个群组预算的当前读数（操作面与对账用）。
type GroupQuota struct {
	GroupID      int64      `json:"group_id"`
	State        QuotaState `json:"state"`
	BudgetTokens int64      `json:"budget_tokens"`
	PeriodKind   string     `json:"period_kind"`
	PeriodKey    string     `json:"period_key"`
	// ConsumedTokens 是当前周期已结算的 token。
	ConsumedTokens int64 `json:"consumed_tokens"`
	// ReservedTokens 是当前周期在途预扣占住的 token。
	ReservedTokens int64 `json:"reserved_tokens"`
	// RemainingTokens 是本周期还能预扣的量（可能为负：实际用量可能超过估算）。
	RemainingTokens int64 `json:"remaining_tokens"`
}

// EnsureQuotaSchema 应用预算 DDL（幂等）。
//
// db/schema.sql 由别的单元持有，本单元的 DDL 放在 db/schema_quota.sql，由这里
// 在 db.Open 之后补齐（见 db/db.go 的 Open）。schema 属主把内容并进
// db/schema.sql 之后，这里重复应用也无副作用。
//
// 先建群聊表：预算表的外键指向 chat_groups(id)，被引用表必须先存在。
func (d *DB) EnsureQuotaSchema() error {
	if _, ok := quotaSchemaReady.Load(d.DB); ok {
		return nil
	}
	if err := d.EnsureChatGroupSchema(); err != nil {
		return fmt.Errorf("应用群聊 schema 失败: %w", err)
	}
	if _, err := d.Exec(quotaSchemaSQL); err != nil {
		return fmt.Errorf("应用预算 schema 失败: %w", err)
	}
	quotaSchemaReady.Store(d.DB, struct{}{})
	return nil
}

// ensureQuotaSchema 是访问层的幂等入口：首次调用时建表，之后只是一次 map 查表。
func (d *DB) ensureQuotaSchema() error { return d.EnsureQuotaSchema() }

// SetGroupQuotaBudget 设一个群组的预算额度（并建出预算行）。
//
// 只有操作面能调（agent 没有这条路径）。budget 为 0 表示"没有可用预算"⇒ 之后
// 的模型调用一律被拒；本平台不提供"不限量"这个取值。
func (d *DB) SetGroupQuotaBudget(groupID int64, budgetTokens int64) error {
	if err := d.ensureQuotaSchema(); err != nil {
		return err
	}
	if groupID <= 0 {
		return errors.New("群组 id 不合法")
	}
	if budgetTokens < 0 {
		return errors.New("预算 token 数不能为负")
	}
	if _, err := d.Exec(`
INSERT INTO group_quotas(group_id, budget_tokens, period_kind, state)
VALUES ($1,$2,$3,'active')
ON CONFLICT (group_id) DO UPDATE SET budget_tokens = EXCLUDED.budget_tokens`,
		groupID, budgetTokens, QuotaPeriodDaily); err != nil {
		return fmt.Errorf("设置群组预算失败: %w", err)
	}
	return nil
}

// SetGroupQuotaState 设一个群组的预算状态（active / draining / parked）。
//
// 非法状态直接报错：状态机是代码常量，不接受"随便写个字符串"。
func (d *DB) SetGroupQuotaState(groupID int64, state QuotaState) error {
	if err := d.ensureQuotaSchema(); err != nil {
		return err
	}
	if groupID <= 0 {
		return errors.New("群组 id 不合法")
	}
	switch state {
	case QuotaStateActive, QuotaStateDraining, QuotaStateParked:
	default:
		return fmt.Errorf("预算状态只能是 active/draining/parked，收到 %q", string(state))
	}
	res, err := d.Exec(`UPDATE group_quotas SET state=$2 WHERE group_id=$1`, groupID, string(state))
	if err != nil {
		return fmt.Errorf("设置群组预算状态失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrQuotaNoBudget
	}
	return nil
}

// GetGroupQuota 读一个群组预算的当前读数。
//
// 没有预算行时返回 (nil, nil)：调用方据此知道"这个群还没配预算"。
func (d *DB) GetGroupQuota(groupID int64) (*GroupQuota, error) {
	if err := d.ensureQuotaSchema(); err != nil {
		return nil, err
	}
	var q GroupQuota
	var state string
	err := d.QueryRow(`
SELECT q.group_id, q.state, q.budget_tokens, q.period_kind
FROM group_quotas q WHERE q.group_id=$1`, groupID).
		Scan(&q.GroupID, &state, &q.BudgetTokens, &q.PeriodKind)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("查询群组预算失败: %w", err)
	}
	q.State = QuotaState(state)
	q.PeriodKey = QuotaPeriodKey(time.Now())
	err = d.QueryRow(`
SELECT COALESCE(consumed_tokens,0), COALESCE(reserved_tokens,0)
FROM group_quota_periods WHERE group_id=$1 AND period_key=$2`,
		groupID, q.PeriodKey).Scan(&q.ConsumedTokens, &q.ReservedTokens)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("查询群组预算周期用量失败: %w", err)
	}
	q.RemainingTokens = q.BudgetTokens - q.ConsumedTokens - q.ReservedTokens
	return &q, nil
}

// GroupQuotaState 返回群组当前的预算状态。
//
// ── 这是给**外部调用方**（并行 workstream 的安全闸门）用的稳定契约 ──
//
//  1. 零值 QuotaStateUnset = 该群组没有配置预算（没有预算行）。
//     对"这个群是不是 draining/parked"这个问题，零值答**否** —— 闸门不应
//     仅因为"没配预算"就拒绝动作（没配预算拦的是**模型调用**，见
//     ReserveGroupQuota，与闸门是两件事）。
//  2. QuotaStateDraining / QuotaStateParked = 明确的拒绝态，闸门必须拒绝
//     该群组的动作。
//  3. error != nil = 状态**查不出来**（DB 错误、状态值无法解析）。
//     调用方**必须**按"拒绝"处理（fail closed）：查不出来不等于没有限制。
//
// 换句话说：闸门的判定应当是
//
//	state, err := db.GroupQuotaState(groupID)
//	if err != nil || state == db.QuotaStateDraining || state == db.QuotaStateParked {
//	    deny()
//	}
func (d *DB) GroupQuotaState(groupID int64) (QuotaState, error) {
	if err := d.ensureQuotaSchema(); err != nil {
		return QuotaStateUnset, err
	}
	var state string
	err := d.QueryRow(`SELECT state FROM group_quotas WHERE group_id=$1`, groupID).Scan(&state)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return QuotaStateUnset, nil
		}
		return QuotaStateUnset, fmt.Errorf("查询群组预算状态失败: %w", err)
	}
	switch QuotaState(state) {
	case QuotaStateActive, QuotaStateDraining, QuotaStateParked:
		return QuotaState(state), nil
	default:
		// 状态值不在代码常量集合内（例如别的单元写进来的新状态）：
		// 解析不出来 ⇒ 出错 ⇒ 调用方按拒绝处理。
		return QuotaStateUnset, fmt.Errorf("群组预算状态无法解析: %q", state)
	}
}

// GroupIDForScope 按 task 或 conversation 解析出它所属的群组 id。
//
// 这是安全闸门的**配套入口**：闸门手上通常只有 task id 或 conversation id，
// 而"会话 ⇒ 群组"的规则（大厅会话或任一成员会话都命中同一个群）属于本模块，
// 不该让外部调用方复制一份。
//
// 返回 (0, nil) 表示这个 task/conversation **不属于任何群组**（不是错误：
// 平台里绝大多数任务没有大厅）。返回 error 表示**查不出来** ⇒ 调用方按拒绝处理。
func (d *DB) GroupIDForScope(taskID, conversationID int64) (int64, error) {
	if taskID > 0 {
		g, err := d.GroupByTask(taskID)
		if err != nil {
			return 0, fmt.Errorf("按任务查群组失败: %w", err)
		}
		if g != nil {
			return g.ID, nil
		}
	}
	if conversationID > 0 {
		g, err := d.GroupByConversation(conversationID)
		if err != nil {
			return 0, fmt.Errorf("按会话查群组失败: %w", err)
		}
		if g != nil {
			return g.ID, nil
		}
	}
	return 0, nil
}

// ReserveGroupQuota 为一次模型调用原子地预扣预算，返回预扣 id。
//
// ── 原子性机制（为什么不会有两个调用一起越过预算线）──
//
// 预算检查与扣减在**同一条带守卫的 UPDATE** 里完成：
//
//	UPDATE group_quota_periods p SET reserved_tokens = p.reserved_tokens + $3
//	  FROM group_quotas q
//	 WHERE p.group_id=$1 AND p.period_key=$2 AND q.group_id=p.group_id
//	   AND q.state='active'
//	   AND p.consumed_tokens + p.reserved_tokens + $3 <= q.budget_tokens
//
// PostgreSQL 的 UPDATE 会先锁住目标行；并发事务拿到同一行时会等待，等待结束后
// **按更新后的行版本重新求值 WHERE**（EvalPlanQual）。于是第二个事务看到的是
// 第一个事务扣减后的 reserved_tokens，守卫不成立 ⇒ 影响 0 行 ⇒ 判为超预算。
// 这条"影响 0 行 = 超预算"的判定与预扣行写入在**同一个事务**里，因此不存在
// "扣了计数但没写预扣行"的半状态。
//
// 为什么不用"先 SELECT 再 UPDATE"：那是经典的读-改写竞争，两个并发调用都能读到
// 同一份旧余额并各自通过检查。
//
// 失败一律返回错误（fail closed），绝不返回"成功但没扣"。
func (d *DB) ReserveGroupQuota(groupID int64, req QuotaReservation) (int64, error) {
	if err := d.ensureQuotaSchema(); err != nil {
		return 0, err
	}
	if groupID <= 0 {
		return 0, errors.New("群组 id 不合法")
	}
	if req.Tokens <= 0 {
		return 0, errors.New("预扣 token 数必须为正")
	}
	requestID := strings.TrimSpace(req.RequestID)
	if requestID == "" {
		return 0, errors.New("预扣必须带 request_id")
	}
	ttl := req.TTL
	if ttl <= 0 {
		ttl = QuotaReservationTTL
	}
	periodKey := QuotaPeriodKey(time.Now())

	tx, err := d.Begin()
	if err != nil {
		return 0, fmt.Errorf("开启预扣事务失败: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	// 周期行：没有就建。并发下第二个事务会等第一个提交后走 ON CONFLICT 分支。
	//
	// 为什么外键冲突要单独判：周期行的 group_id 指向 group_quotas(group_id)，
	// 所以「该群组没有预算行」在这里表现为外键违规，而不是「预算不够」。
	// 不特判的话这里会抛出原始数据库错误 —— 既不是哨兵错误 ErrQuotaNoBudget，
	// 又把表结构细节漏给了调用方。行为上仍然是拒绝（fail closed），
	// 但错误契约必须对。
	if _, err := tx.Exec(`
INSERT INTO group_quota_periods(group_id, period_key) VALUES ($1,$2)
ON CONFLICT (group_id, period_key) DO NOTHING`, groupID, periodKey); err != nil {
		if quotaForeignKeyViolation(err) {
			return 0, d.classifyReserveFailure(groupID)
		}
		return 0, fmt.Errorf("建立额度周期行失败: %w", err)
	}

	res, err := tx.Exec(`
UPDATE group_quota_periods p
   SET reserved_tokens = p.reserved_tokens + $3, updated_at = now()
  FROM group_quotas q
 WHERE p.group_id = $1 AND p.period_key = $2 AND q.group_id = p.group_id
   AND q.state = 'active'
   AND p.consumed_tokens + p.reserved_tokens + $3 <= q.budget_tokens`,
		groupID, periodKey, req.Tokens)
	if err != nil {
		return 0, fmt.Errorf("预扣预算失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// 0 行 = 预算不够 / 状态不是 active / 没有预算行。事务回滚，计数没动。
		return 0, d.classifyReserveFailure(groupID)
	}

	var resID int64
	err = tx.QueryRow(`
INSERT INTO group_quota_reservations(
  group_id, period_key, request_id, reserved_tokens,
  reserved_input_tokens, reserved_output_tokens, task_id, conversation_id, expires_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8, now() + $9::interval)
RETURNING id`,
		groupID, periodKey, requestID, req.Tokens,
		req.InputTokens, req.OutputTokens, req.TaskID, nullInt64(req.ConversationID), ttl).Scan(&resID)
	if err != nil {
		if quotaUniqueViolation(err) {
			return 0, ErrQuotaDuplicateRequest
		}
		return 0, fmt.Errorf("写预扣记录失败: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("提交预扣事务失败: %w", err)
	}
	return resID, nil
}

// classifyReserveFailure 判定预扣失败的原因（用于给出准确的中文原因码）。
//
// 它只在已经确定"预扣没成功"之后调用，因此这里的读取结果**不参与**预算判定 ——
// 判定早已由那条守卫 UPDATE 原子地做完。
func (d *DB) classifyReserveFailure(groupID int64) error {
	var state string
	var budget int64
	err := d.QueryRow(`SELECT state, budget_tokens FROM group_quotas WHERE group_id=$1`, groupID).
		Scan(&state, &budget)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrQuotaNoBudget
		}
		return fmt.Errorf("查询群组预算失败: %w", err)
	}
	switch QuotaState(state) {
	case QuotaStateActive:
		if budget <= 0 {
			return ErrQuotaNoBudget
		}
		return ErrQuotaBudgetExceeded
	case QuotaStateDraining, QuotaStateParked:
		return ErrQuotaStateNotActive
	default:
		// 状态无法解析 ⇒ 不能当作"正常"放行。
		return fmt.Errorf("群组预算状态无法解析: %q", state)
	}
}

// RecordQuotaRefusal 记一条"这次模型调用没发出去"的拒绝留痕。
//
// best-effort：留痕失败不能改变调用结果（拒绝本身已经发生了）。
func (d *DB) RecordQuotaRefusal(groupID int64, requestID, reason, taskID string) {
	if err := d.ensureQuotaSchema(); err != nil {
		return
	}
	if groupID <= 0 {
		return
	}
	_, _ = d.Exec(`
INSERT INTO group_quota_refusals(group_id, period_key, request_id, reason, task_id)
VALUES ($1,$2,$3,$4,$5)`,
		groupID, QuotaPeriodKey(time.Now()), requestID, reason, taskID)
}

// QuotaReservationEstimate 读回一次预扣的估算（输入/输出拆分）。
//
// 用途：请求已发出但没有 usage 返回时，按**输入估算**结算（供应商已计输入侧）。
// 以库里的记录为准，而不是调用点带来的值 —— 库里的是预扣当时真正扣下的数。
func (d *DB) QuotaReservationEstimate(reservationID int64) (input, output int64, err error) {
	if err := d.ensureQuotaSchema(); err != nil {
		return 0, 0, err
	}
	err = d.QueryRow(`
SELECT reserved_input_tokens, reserved_output_tokens
FROM group_quota_reservations WHERE id=$1`, reservationID).Scan(&input, &output)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, 0, ErrQuotaReservationNotHeld
		}
		return 0, 0, fmt.Errorf("读取预扣估算失败: %w", err)
	}
	return input, output, nil
}

// MarkQuotaReservationDispatched 标记"请求确实发出去了"。
//
// 必须在请求发出前调用：崩在 dispatch 与 settle 之间时，预扣仍留在"已计费"
// 状态，不会被误当成"没发出"而释放（= 白嫖供应商的计费窗口）。
func (d *DB) MarkQuotaReservationDispatched(reservationID int64) error {
	if err := d.ensureQuotaSchema(); err != nil {
		return err
	}
	if _, err := d.Exec(`UPDATE group_quota_reservations SET dispatched = true WHERE id = $1`,
		reservationID); err != nil {
		return fmt.Errorf("标记预扣已发出失败: %w", err)
	}
	return nil
}

// SettleGroupQuota 用**真实用量**结算一次预扣。
//
// 口径：consumed += total（= input + output，缓存已含在 input 里，不重复计）。
//   - 真实用量 > 估算：多出来的照记（consumed 可能越过预算）⇒ **不裁剪**。
//     预算是"允许多少"，不是"记账上限"；裁掉差额等于让平台替供应商免单。
//   - 真实用量 < 估算：估算占住的部分整块释放（reserved 归零），只记真实用量。
//
// 幂等：已结算/已释放的预扣再次结算是 no-op（重试结算不会双扣）。
func (d *DB) SettleGroupQuota(reservationID int64, usage QuotaUsage) error {
	if err := d.ensureQuotaSchema(); err != nil {
		return err
	}
	tx, err := d.Begin()
	if err != nil {
		return fmt.Errorf("开启结算事务失败: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	var groupID int64
	var periodKey, status string
	var reserved int64
	err = tx.QueryRow(`
SELECT group_id, period_key, status, reserved_tokens
FROM group_quota_reservations WHERE id=$1 FOR UPDATE`, reservationID).
		Scan(&groupID, &periodKey, &status, &reserved)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrQuotaReservationNotHeld
		}
		return fmt.Errorf("读取预扣记录失败: %w", err)
	}
	if status != "held" {
		// 终态：已结算/已释放 ⇒ 幂等返回，什么都不改。
		return tx.Commit()
	}
	total := usage.TotalTokens
	if total <= 0 {
		total = usage.InputTokens + usage.OutputTokens
	}
	if _, err := tx.Exec(`
UPDATE group_quota_periods
   SET reserved_tokens = GREATEST(reserved_tokens - $3, 0),
       consumed_tokens = consumed_tokens + $4,
       updated_at = now()
 WHERE group_id = $1 AND period_key = $2`, groupID, periodKey, reserved, total); err != nil {
		return fmt.Errorf("结算额度周期计数失败: %w", err)
	}
	if _, err := tx.Exec(`
UPDATE group_quota_reservations
   SET status = 'settled', settled_input_tokens = $2, settled_output_tokens = $3, settled_total_tokens = $4
 WHERE id = $1`, reservationID, usage.InputTokens, usage.OutputTokens, total); err != nil {
		return fmt.Errorf("标记预扣已结算失败: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交结算事务失败: %w", err)
	}
	return nil
}

// ReleaseGroupQuota 释放一次预扣（确定没有花出去时）。
//
// 幂等：已结算/已释放的预扣再次释放是 no-op。
func (d *DB) ReleaseGroupQuota(reservationID int64) error {
	if err := d.ensureQuotaSchema(); err != nil {
		return err
	}
	tx, err := d.Begin()
	if err != nil {
		return fmt.Errorf("开启释放事务失败: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	var groupID int64
	var periodKey, status string
	var reserved int64
	err = tx.QueryRow(`
SELECT group_id, period_key, status, reserved_tokens
FROM group_quota_reservations WHERE id=$1 FOR UPDATE`, reservationID).
		Scan(&groupID, &periodKey, &status, &reserved)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrQuotaReservationNotHeld
		}
		return fmt.Errorf("读取预扣记录失败: %w", err)
	}
	if status != "held" {
		return tx.Commit()
	}
	if _, err := tx.Exec(`
UPDATE group_quota_periods
   SET reserved_tokens = GREATEST(reserved_tokens - $2, 0), updated_at = now()
 WHERE group_id = $1 AND period_key = $2`, groupID, periodKey, reserved); err != nil {
		return fmt.Errorf("释放额度周期计数失败: %w", err)
	}
	if _, err := tx.Exec(`UPDATE group_quota_reservations SET status='released' WHERE id=$1`,
		reservationID); err != nil {
		return fmt.Errorf("标记预扣已释放失败: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交释放事务失败: %w", err)
	}
	return nil
}

// SweepExpiredQuotaReservations 回收过期未结的预扣（孤儿清扫器）。
//
// 触发场景：模型调用发出后进程崩了/机器挂了，既没结算也没释放 —— 预扣会一直
// 占着额度，表现为"预算明明没用完却开始拒绝"。清扫器按 TTL 把这些预扣收回来。
//
// 处置规则（确定性）：
//   - dispatched = true  → 按**输入估算**结算（请求确实发出去了，供应商已计输入
//     侧，记 0 等于白嫖计费窗口）。
//   - dispatched = false → 整块释放（确定没花出去）。
//
// 顺序：按预扣 id 升序（同一批的处置顺序固定 ⇒ 行为可复算），单批上限 limit。
// 幂等：已结算/已释放的预扣不再被选中，重复清扫不会二次动账。
func (d *DB) SweepExpiredQuotaReservations(now time.Time, limit int) (int, error) {
	if err := d.ensureQuotaSchema(); err != nil {
		return 0, err
	}
	if limit <= 0 {
		limit = 100
	}
	tx, err := d.Begin()
	if err != nil {
		return 0, fmt.Errorf("开启清扫事务失败: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	rows, err := tx.Query(`
SELECT id, group_id, period_key, reserved_tokens, reserved_input_tokens, dispatched
FROM group_quota_reservations
WHERE status = 'held' AND expires_at <= $1
ORDER BY id ASC LIMIT $2
FOR UPDATE SKIP LOCKED`, now, limit)
	if err != nil {
		return 0, fmt.Errorf("查询过期预扣失败: %w", err)
	}
	type expired struct {
		id        int64
		groupID   int64
		periodKey string
		reserved  int64
		input     int64
		dispatch  bool
	}
	batch := []expired{}
	for rows.Next() {
		var e expired
		if err := rows.Scan(&e.id, &e.groupID, &e.periodKey, &e.reserved, &e.input, &e.dispatch); err != nil {
			rows.Close()
			return 0, fmt.Errorf("读取过期预扣失败: %w", err)
		}
		batch = append(batch, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("读取过期预扣失败: %w", err)
	}

	for _, e := range batch {
		consumed := int64(0)
		if e.dispatch {
			consumed = e.input
		}
		if _, err := tx.Exec(`
UPDATE group_quota_periods
   SET reserved_tokens = GREATEST(reserved_tokens - $3, 0),
       consumed_tokens = consumed_tokens + $4,
       updated_at = now()
 WHERE group_id = $1 AND period_key = $2`, e.groupID, e.periodKey, e.reserved, consumed); err != nil {
			return 0, fmt.Errorf("清扫额度周期计数失败: %w", err)
		}
		status := "released"
		if e.dispatch {
			status = "settled"
		}
		if _, err := tx.Exec(`
UPDATE group_quota_reservations
   SET status = $2, settled_input_tokens = $3, settled_total_tokens = $3
 WHERE id = $1`, e.id, status, consumed); err != nil {
			return 0, fmt.Errorf("标记过期预扣失败: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("提交清扫事务失败: %w", err)
	}
	return len(batch), nil
}

// quotaUniqueViolation 判断是不是唯一约束冲突（23505）。
func quotaUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// quotaForeignKeyViolation 判定是否外键违规（PG SQLSTATE 23503）。
//
// 预算这一层用它来识别「该群组没有预算行」：周期行的外键指向预算行，
// 所以「没配预算」在这条路径上表现为外键违规而不是「预算不够」。
func quotaForeignKeyViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503"
}

// nullInt64 把 0 变成 SQL NULL（"没有会话"不是"会话 0"）。
func nullInt64(v int64) any {
	if v <= 0 {
		return nil
	}
	return v
}

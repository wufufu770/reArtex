// 本文件是 SoD 链（职责分离）的访问层。
//
// 它只存【观测到的状态】：决策账本、对抗复核队列、工具熔断状态。
// **不含任何阈值** —— 阈值是 internal/sodpolicy 的代码常量。
//
// 为什么 DDL 单独成文件：db/schema.sql 由别的单元并行持有（见 Global Constraints）。
//
// allow: SIZE_OK —— 本单元的文件域被任务书钉死为 db/sod.go 一个文件，且下面
// 四组访问器共用同一条约束（「只存观测状态、阈值在代码常量、状态解析不出来
// 一律报错让调用方 fail-closed」）。拆成四个文件会把这条约束切成四份副本，
// 而四份副本正是漂移的开始。
package db

import (
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/Autumn-27/artex/internal/sodpolicy"
)

//go:embed schema_sod.sql
var sodSchemaSQL string

// sodSchemaReady 记录某个连接已经应用过 SoD DDL。
//
// 与 db/quota.go 的 quotaSchemaReady 同理：用包级 sync.Map 而不是 DB 上的
// 字段（本单元不改 db/db.go 的结构体）。键是底层 *sql.DB 指针 —— 每个测试
// 包/实例各连各的库，互不干扰。
var sodSchemaReady sync.Map

// EnsureSoDSchema 应用 SoD 的幂等 DDL。
//
// db/schema.sql 由别的单元持有，本单元的 DDL 放在 db/schema_sod.sql，由这里
// 在首次访问时幂等补齐（与 db/quota.go 的 EnsureQuotaSchema 同款）。schema
// 属主把内容并进 db/schema.sql 之后，这里重复应用也无副作用。
//
// ⚠️ 本单元**不在 db.Open 里**调用它（db/db.go 由别的单元持有）：懒加载范式
// 保证第一次访问时建表。代价是「进程启动即建表」这一步留给编排任务。
func (d *DB) EnsureSoDSchema() error {
	if _, ok := sodSchemaReady.Load(d.DB); ok {
		return nil
	}
	if _, err := d.Exec(sodSchemaSQL); err != nil {
		return fmt.Errorf("应用 SoD schema 失败: %w", err)
	}
	sodSchemaReady.Store(d.DB, struct{}{})
	return nil
}

// ensureSoDSchema 是访问层的幂等入口：首次调用时建表，之后只是一次 map 查表。
func (d *DB) ensureSoDSchema() error { return d.EnsureSoDSchema() }

// SoDDecision 是 SoD 的一次裁决（决策账本的一行）。
//
// 为什么账本在 approve 上也写：哈希链只收「被拦下的那些」（db/audit_chain.go
// 的设计如此 —— 放行量极大，而每条链记录都依赖上一条），但「每次决策都留痕」
// 是另一条要求 ⇒ 完整账本在这里，字段与哈希链事件同源。
type SoDDecision struct {
	ID             int64
	TaskID         *int64
	ConversationID *int64
	SourceAgent    string
	ToolName       string
	Verdict        string
	ReasonCode     string
	PayloadDigest  string
	PayloadPreview string
	IntentDigest   string
	AnomalyFlag    bool
	AnomalyReason  string
	Checks         json.RawMessage
	TokenDigest    string
	ScopeDigest    string
	CacheHit       bool
	LatencyMs      int
	CreatedAt      time.Time
}

// 对抗复核队列的状态机。取值与 db/schema_sod.sql 的 CHECK 镜像。
//
// 为什么这组常量留在 db 而不在 internal/sodpolicy：它不是安全阈值，也不是
// 裁决 / 原因码，而是「这张表自己的状态机」—— 与 db/quota.go 的 QuotaState*
// 同款归属（表的状态机归表所在的包）。
const (
	// SoDReviewPending 待处理（redteam 可取走）。
	SoDReviewPending = "pending"
	// SoDReviewClaimed 已被认领（预留给未来的认领路径）。
	SoDReviewClaimed = "claimed"
	// SoDReviewResolved 已查清。
	SoDReviewResolved = "resolved"
	// SoDReviewDismissed 判定为无需追查。
	SoDReviewDismissed = "dismissed"
)

// ErrSoDReviewNotFound 表示要结案的对抗复核项不存在。
var ErrSoDReviewNotFound = errors.New("对抗复核项不存在")

// ErrSoDReviewSelfCertification 表示结案人就是提出人 —— 对抗复核被自证。
//
// 为什么必须有这条：internal/sodpolicy 早就写明「对抗证伪必须由对抗者做：
// needs_review 交回给提出者复核等于自证」，但那只停在注释层：结案的 UPDATE 从来
// 没有校验过「结案人 ≠ 提出人」。于是提出者可以给自己的异常动作盖章，整条 SoD 链
// 的「对抗」就只剩提示词层面的默契。
//
// 这条把注释变成确定性约束：数据库层拒绝自证，调用方拿到的也是可区分的错误
// （而不是「项不存在」那种会让人误以为队列被清空的重试理由）。
var ErrSoDReviewSelfCertification = errors.New("提出人不得为自己的异常动作结案（对抗复核必须由对抗者做）")

// AdversarialReviewItem 是 needs_review 队列的一项。
type AdversarialReviewItem struct {
	ID               int64
	TaskID           *int64
	ConversationID   *int64
	SourceAgent      string
	ToolName         string
	PayloadDigest    string
	PayloadPreview   string
	AnomalyFlag      bool
	AnomalyReason    string
	ReasonCode       string
	StructuredIntent json.RawMessage
	Status           string // pending | claimed | resolved | dismissed
	ClaimedBy        string
	Resolution       string
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// ReplayEvidence 是一个 finding 的重放证据标识与其资产归属。
type ReplayEvidence struct {
	FindingID        int64
	ReplayEvidenceID string
	AssetIDs         []int64
}

// sodScanner 是 *sql.Row 与 *sql.Rows 共有的扫描入口。
type sodScanner interface {
	Scan(dest ...any) error
}

const sodDecisionCols = `id, task_id, conversation_id, source_agent, tool_name, verdict,
	reason_code, payload_digest, payload_preview, intent_digest, anomaly_flag, anomaly_reason,
	checks, token_digest, scope_digest, cache_hit, latency_ms, created_at`

const sodReviewCols = `id, task_id, conversation_id, source_agent, tool_name, payload_digest,
	payload_preview, anomaly_flag, anomaly_reason, reason_code, structured_intent,
	status, claimed_by, resolution, created_at, updated_at`

// scanSoDDecision 把一行决策账本读成结构体。
func scanSoDDecision(row sodScanner) (*SoDDecision, error) {
	var d SoDDecision
	var checks []byte
	if err := row.Scan(&d.ID, &d.TaskID, &d.ConversationID, &d.SourceAgent, &d.ToolName,
		&d.Verdict, &d.ReasonCode, &d.PayloadDigest, &d.PayloadPreview, &d.IntentDigest,
		&d.AnomalyFlag, &d.AnomalyReason, &checks, &d.TokenDigest, &d.ScopeDigest,
		&d.CacheHit, &d.LatencyMs, &d.CreatedAt); err != nil {
		return nil, err
	}
	d.Checks = json.RawMessage(checks)
	return &d, nil
}

// scanSoDReviewItem 把一行对抗复核项读成结构体。
func scanSoDReviewItem(row sodScanner) (*AdversarialReviewItem, error) {
	var it AdversarialReviewItem
	var intent []byte
	if err := row.Scan(&it.ID, &it.TaskID, &it.ConversationID, &it.SourceAgent, &it.ToolName,
		&it.PayloadDigest, &it.PayloadPreview, &it.AnomalyFlag, &it.AnomalyReason,
		&it.ReasonCode, &intent, &it.Status, &it.ClaimedBy, &it.Resolution,
		&it.CreatedAt, &it.UpdatedAt); err != nil {
		return nil, err
	}
	it.StructuredIntent = json.RawMessage(intent)
	return &it, nil
}

// RecordSoDDecision 写一条决策账本。
//
// best-effort 由调用方决定：这里返回错误，不吞 —— 留痕失败必须让调用方
// 知道（它要决定这是「拒绝并记失败」还是「拒绝且连留痕都没了」）。
func (d *DB) RecordSoDDecision(rec *SoDDecision) (int64, error) {
	if err := d.ensureSoDSchema(); err != nil {
		return 0, err
	}
	checks := rec.Checks
	if len(checks) == 0 {
		checks = json.RawMessage(`{}`)
	}
	var id int64
	err := d.QueryRow(`
INSERT INTO sod_decisions(task_id, conversation_id, source_agent, tool_name, verdict,
                         reason_code, payload_digest, payload_preview, intent_digest,
                         anomaly_flag, anomaly_reason, checks, token_digest, scope_digest,
                         cache_hit, latency_ms)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16) RETURNING id`,
		rec.TaskID, rec.ConversationID, rec.SourceAgent, rec.ToolName, rec.Verdict,
		rec.ReasonCode, rec.PayloadDigest, rec.PayloadPreview, rec.IntentDigest,
		rec.AnomalyFlag, rec.AnomalyReason, string(checks), rec.TokenDigest, rec.ScopeDigest,
		rec.CacheHit, rec.LatencyMs).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("写 SoD 决策失败: %w", err)
	}
	return id, nil
}

// ListSoDDecisions 列出决策账本（operator 侧只读）。
//
// taskID > 0 只列该任务的决策；taskID <= 0 列全部（跨任务看账）。
// limit <= 0 时用默认 200。
func (d *DB) ListSoDDecisions(taskID int64, limit int) ([]*SoDDecision, error) {
	if err := d.ensureSoDSchema(); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 200
	}
	var (
		rows *sql.Rows
		err  error
	)
	if taskID > 0 {
		rows, err = d.Query(`SELECT `+sodDecisionCols+`
FROM sod_decisions WHERE task_id=$1 ORDER BY id DESC LIMIT $2`, taskID, limit)
	} else {
		rows, err = d.Query(`SELECT `+sodDecisionCols+`
FROM sod_decisions ORDER BY id DESC LIMIT $1`, limit)
	}
	if err != nil {
		return nil, fmt.Errorf("列 SoD 决策失败: %w", err)
	}
	defer rows.Close()
	out := make([]*SoDDecision, 0, 16)
	for rows.Next() {
		rec, err := scanSoDDecision(rows)
		if err != nil {
			return nil, fmt.Errorf("列 SoD 决策失败: %w", err)
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("列 SoD 决策失败: %w", err)
	}
	return out, nil
}

// EnqueueAdversarialReview 入队一条 needs_review。
//
// 这是 needs_review 的**唯一**去处：它不写 intercept_pending（人工审批表）——
// needs_review 是「交给对抗者证伪」，不是「请人看一眼」。
func (d *DB) EnqueueAdversarialReview(item *AdversarialReviewItem) (int64, error) {
	if err := d.ensureSoDSchema(); err != nil {
		return 0, err
	}
	intent := item.StructuredIntent
	if len(intent) == 0 {
		intent = json.RawMessage(`{}`)
	}
	reason := item.ReasonCode
	if reason == "" {
		reason = sodpolicy.ReasonAnomalyFlagged
	}
	var id int64
	err := d.QueryRow(`
INSERT INTO sod_adversarial_review_pending(task_id, conversation_id, source_agent, tool_name,
                                            payload_digest, payload_preview, anomaly_flag,
                                            anomaly_reason, reason_code, structured_intent)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) RETURNING id`,
		item.TaskID, item.ConversationID, item.SourceAgent, item.ToolName,
		item.PayloadDigest, item.PayloadPreview, item.AnomalyFlag,
		item.AnomalyReason, reason, string(intent)).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("写对抗复核队列失败: %w", err)
	}
	return id, nil
}

// ListAdversarialReviewPending 列出待处理项（redteam 侧读）。
//
// 只列 status='pending'：结案后（resolved / dismissed）不再出现。
// 'claimed' 是给未来的认领路径预留的取值 —— 当前没有认领入口，所以本函数
// 不列它。
//
// taskID > 0 只列该任务的项；taskID <= 0 列全部。limit <= 0 时用默认 200。
func (d *DB) ListAdversarialReviewPending(taskID int64, limit int) ([]*AdversarialReviewItem, error) {
	if err := d.ensureSoDSchema(); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 200
	}
	var (
		rows *sql.Rows
		err  error
	)
	if taskID > 0 {
		rows, err = d.Query(`SELECT `+sodReviewCols+`
FROM sod_adversarial_review_pending
WHERE status='pending' AND task_id=$1 ORDER BY created_at ASC LIMIT $2`, taskID, limit)
	} else {
		rows, err = d.Query(`SELECT `+sodReviewCols+`
FROM sod_adversarial_review_pending
WHERE status='pending' ORDER BY created_at ASC LIMIT $1`, limit)
	}
	if err != nil {
		return nil, fmt.Errorf("列对抗复核队列失败: %w", err)
	}
	defer rows.Close()
	out := make([]*AdversarialReviewItem, 0, 16)
	for rows.Next() {
		it, err := scanSoDReviewItem(rows)
		if err != nil {
			return nil, fmt.Errorf("列对抗复核队列失败: %w", err)
		}
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("列对抗复核队列失败: %w", err)
	}
	return out, nil
}

// ResolveAdversarialReview 结案（redteam 侧写）。
//
// status 只能是 pending / claimed / resolved / dismissed —— 非法取值直接
// 报错（状态机是代码常量，不接受「随便写个字符串」）。
func (d *DB) ResolveAdversarialReview(id int64, by, resolution, status string) error {
	if err := d.ensureSoDSchema(); err != nil {
		return err
	}
	switch status {
	case SoDReviewPending, SoDReviewClaimed, SoDReviewResolved, SoDReviewDismissed:
	default:
		return fmt.Errorf("对抗复核状态只能是 pending/claimed/resolved/dismissed，收到 %q", status)
	}
	// 守卫：结案人不得等于提出人（见 ErrSoDReviewSelfCertification）。
	//
	// 写成 SQL 条件而不是先查后写，是为了避免「查出来是提出人 → 放行」的竞态：
	// 并发下两个结案请求都可能读到同一份 source_agent。UPDATE 的 WHERE 里直接排除，
	// 由数据库在同一条语句内判定 ⇒ 不存在检查与写入之间的窗口。
	//
	// 两边为空时（source_agent 与 by 都是 ''）不算自证：那是「拿不到身份」的降级
	// 情形，不是「自己给自己盖章」，不该在这里硬失败。
	res, err := d.Exec(`
UPDATE sod_adversarial_review_pending
   SET status=$2, claimed_by=$3, resolution=$4, updated_at=now()
 WHERE id=$1
   AND NOT (source_agent <> '' AND $3 <> '' AND source_agent = $3)`, id, status, by, resolution)
	if err != nil {
		return fmt.Errorf("结案对抗复核项失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// 0 行有两种原因，必须区分：项不存在，还是被自证守卫挡下。
		// 调用方对这两种情况的处理完全不同（前者可重试，后者必须换对抗者）。
		var proposer string
		if qerr := d.QueryRow(`SELECT source_agent FROM sod_adversarial_review_pending WHERE id=$1`, id).
			Scan(&proposer); qerr == nil && proposer != "" && proposer == by {
			return ErrSoDReviewSelfCertification
		}
		return ErrSoDReviewNotFound
	}
	return nil
}

// DismissStaleAdversarialReviews 把挂起超过 ttl 的 needs_review 项批量结案为
// dismissed（方案 v3 缺陷 D4：对抗复核队列不能无限堆积——没有 redteam 认领的
// 项挂 24h 后自动过期，resolution 里留痕「过期自动结案」）。
//
// 谁来调：scheduler 周期任务（不是 redteam）——scheduler 不是提出人，不触自证
// 守卫；行本身保留（status=dismissed 即留痕），不物理删除。
// 返回本次结案的行数。
func (d *DB) DismissStaleAdversarialReviews(ttl time.Duration) (int64, error) {
	if err := d.ensureSoDSchema(); err != nil {
		return 0, err
	}
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	res, err := d.Exec(`
UPDATE sod_adversarial_review_pending
   SET status=$1, claimed_by='scheduler', resolution=$2, updated_at=now()
 WHERE status=$3 AND created_at < now() - $4::interval`,
		SoDReviewDismissed,
		fmt.Sprintf("pending 超过 %s 无对抗者认领，调度器自动结案（needs_review 过期闸）", ttl),
		SoDReviewPending,
		fmt.Sprintf("%.0f seconds", ttl.Seconds()))
	if err != nil {
		return 0, fmt.Errorf("过期对抗复核项结案失败: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// ToolCircuitState 读某个工具的熔断状态。
//
// 没有行 ⇒ closed（该工具还没出过事）。
// 状态值不在代码常量集合内 ⇒ **报错** ⇒ 调用方按 fail-closed 拒绝。
// 理由同 db/quota.go 的 GroupQuotaState（额度状态解析不出来 ⇒ 拒绝，不是放行）。
//
// ⚠️ 开路超过 sodpolicy.CircuitOpenDurationSeconds 之后，本函数返回
// half_open 而不是 open：熔断期的意义是「等它恢复」，不是「永久拉黑」。
// 若这里只读行里的值，一个坏掉的工具会被闸门永远拒绝（拒绝 ⇒ 不执行 ⇒
// 没有新的成败可记 ⇒ 状态永远停在 open）。
func (d *DB) ToolCircuitState(toolKey string) (string, error) {
	if err := d.ensureSoDSchema(); err != nil {
		return "", err
	}
	var (
		state    string
		openedAt *time.Time
	)
	err := d.QueryRow(`SELECT state, opened_at FROM sod_tool_circuit WHERE tool_key=$1`, toolKey).
		Scan(&state, &openedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return sodpolicy.CircuitClosed, nil
		}
		return "", fmt.Errorf("查询工具熔断状态失败: %w", err)
	}
	switch state {
	case sodpolicy.CircuitClosed, sodpolicy.CircuitOpen, sodpolicy.CircuitHalfOpen:
		return effectiveCircuitState(state, openedAt, time.Now()), nil
	default:
		return "", fmt.Errorf("工具熔断状态无法解析: %q", state)
	}
}

// RecordToolCallOutcome 记一次工具调用的成败，用于熔断计数。
//
// 计数与开路都在**代码常量**里（sodpolicy.CircuitFailThreshold /
// sodpolicy.CircuitOpenDurationSeconds），DB 里没有阈值列。
//
// 状态机（effective = 折算过期后的有效状态）：
//
//	closed  + 失败 ⇒ fail_count+1；达到阈值 ⇒ open（opened_at=now）
//	closed  + 成功 ⇒ fail_count 归零，保持 closed
//	open    + 失败 ⇒ 保持 open，窗口从这次失败起算（它还在坏）
//	open    + 成功 ⇒ fail_count 归零但**保持 open**（不因一次成功就放行；
//	                   半开只允许一次试探，由调用方决定）
//	half_open（= 已过期的 open）+ 成功 ⇒ closed
//	half_open + 失败 ⇒ 重新 open，窗口重新起算
//
// 用事务 + FOR UPDATE 做读改写：两个并发失败各自 +1 会互相覆盖计数，
// 熔断就会迟到触发。
func (d *DB) RecordToolCallOutcome(toolKey string, failed bool) error {
	if err := d.ensureSoDSchema(); err != nil {
		return err
	}
	if toolKey == "" {
		return errors.New("工具 key 不能为空")
	}
	tx, err := d.Begin()
	if err != nil {
		return fmt.Errorf("开启熔断计数事务失败: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	var (
		state     string
		failCount int
		openedAt  *time.Time
	)
	err = tx.QueryRow(`SELECT state, fail_count, opened_at FROM sod_tool_circuit
WHERE tool_key=$1 FOR UPDATE`, toolKey).Scan(&state, &failCount, &openedAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		state, failCount, openedAt = sodpolicy.CircuitClosed, 0, nil
	case err != nil:
		return fmt.Errorf("读取工具熔断状态失败: %w", err)
	}

	now := time.Now()
	effective := effectiveCircuitState(state, openedAt, now)

	var (
		newState  string
		newOpened *time.Time
		newFails  int
	)
	if failed {
		newFails = failCount + 1
		if newFails >= sodpolicy.CircuitFailThreshold ||
			effective == sodpolicy.CircuitOpen || effective == sodpolicy.CircuitHalfOpen {
			newState = sodpolicy.CircuitOpen
			opened := now
			newOpened = &opened
		} else {
			newState = sodpolicy.CircuitClosed
		}
	} else {
		newFails = 0
		if effective == sodpolicy.CircuitOpen {
			// 仍在熔断期内：保持 open（不因一次成功就立刻放行）。
			newState = sodpolicy.CircuitOpen
			newOpened = openedAt
		} else {
			newState = sodpolicy.CircuitClosed
		}
	}

	if _, err := tx.Exec(`
INSERT INTO sod_tool_circuit(tool_key, state, fail_count, opened_at, updated_at)
VALUES($1,$2,$3,$4,now())
ON CONFLICT (tool_key) DO UPDATE
   SET state=EXCLUDED.state, fail_count=EXCLUDED.fail_count,
       opened_at=EXCLUDED.opened_at, updated_at=now()`,
		toolKey, newState, newFails, newOpened); err != nil {
		return fmt.Errorf("写工具熔断状态失败: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交熔断计数事务失败: %w", err)
	}
	return nil
}

// effectiveCircuitState 算出某行熔断的**有效**状态。
//
// 开路超过 sodpolicy.CircuitOpenDurationSeconds 之后，行里的 state 仍写着
// open，但它已经不该再拦人 ⇒ 折成 half_open（允许一次试探）。这个折算在
// **读时**做，不写回：写回需要一次写操作，而熔断状态是闸门热路径上的读。
func effectiveCircuitState(state string, openedAt *time.Time, now time.Time) string {
	if state != sodpolicy.CircuitOpen || openedAt == nil {
		return state
	}
	if now.Sub(*openedAt) >= time.Duration(sodpolicy.CircuitOpenDurationSeconds)*time.Second {
		return sodpolicy.CircuitHalfOpen
	}
	return sodpolicy.CircuitOpen
}

// GetFindingReplayEvidence 读 finding 的重放证据标识与资产归属。
//
// finding 不存在 ⇒ (nil, nil)（调用方按「不适用」处理）。
// asset_ids 是 JSONB（db/schema.sql 的 findings 表）⇒ 解成 []int64；
// 解不动 ⇒ 报错 ⇒ 调用方按 fail-closed 拒绝。
func (d *DB) GetFindingReplayEvidence(findingID int64) (*ReplayEvidence, error) {
	if err := d.ensureSoDSchema(); err != nil {
		return nil, err
	}
	var (
		evidenceID string
		assetIDs   []byte
	)
	err := d.QueryRow(`SELECT replay_evidence_id, asset_ids FROM findings WHERE id=$1`, findingID).
		Scan(&evidenceID, &assetIDs)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("读取 finding 重放证据失败: %w", err)
	}
	ids := []int64{}
	if len(assetIDs) > 0 {
		if err := json.Unmarshal(assetIDs, &ids); err != nil {
			return nil, fmt.Errorf("解析 finding 资产归属失败: %w", err)
		}
	}
	return &ReplayEvidence{
		FindingID:        findingID,
		ReplayEvidenceID: evidenceID,
		AssetIDs:         ids,
	}, nil
}

// ---------- 裁决人工复核改判（方案 v3 缺陷 F3） ----------

// SodManualOverride 是一条人工改判的放行凭据（call-precision：四元组全等命中）。
type SodManualOverride struct {
	ID            int64      `json:"id"`
	DecisionID    int64      `json:"decision_id"`
	PayloadDigest string     `json:"payload_digest"`
	ToolKey       string     `json:"tool_key"`
	TaskID        int64      `json:"task_id"`
	AgentKey      string     `json:"agent_key"`
	Scope         string     `json:"scope"` // once | task
	Reason        string     `json:"reason"`
	CreatedBy     string     `json:"created_by"`
	CreatedAt     time.Time  `json:"created_at"`
	ExpiresAt     *time.Time `json:"expires_at,omitempty"`
	UseCount      int        `json:"use_count"`
	LastUsedAt    *time.Time `json:"last_used_at,omitempty"`
}

// CreateSodManualOverride 登记一条人工改判放行凭据（review API 的写入口）。
// scope 语义（E1-5，方案 v10 §9）：
//   once      —— 四元组全等、用后即焚；
//   task      —— 四元组全等、随任务终态过期；
//   task_tool —— (task, tool, agent) 级通配（payload_digest 留空），ask 批准
//                「同时放行本任务后续同类调用」时签发；仅限非基线工具（调用方校验）。
func (d *DB) CreateSodManualOverride(o *SodManualOverride) (int64, error) {
	if err := d.ensureSoDSchema(); err != nil {
		return 0, err
	}
	if o.ToolKey == "" || o.TaskID <= 0 {
		return 0, fmt.Errorf("override 需要 tool_key/task_id")
	}
	if o.Scope != "once" && o.Scope != "task" && o.Scope != "task_tool" {
		return 0, fmt.Errorf("scope 只能是 once|task|task_tool")
	}
	if o.Scope != "task_tool" && o.PayloadDigest == "" {
		return 0, fmt.Errorf("override 需要 payload_digest/tool_key/task_id")
	}
	var id int64
	err := d.QueryRow(`
INSERT INTO sod_manual_overrides(decision_id, payload_digest, tool_key, task_id, agent_key, scope, reason, created_by, expires_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING id`,
		o.DecisionID, o.PayloadDigest, o.ToolKey, o.TaskID, o.AgentKey, o.Scope,
		o.Reason, o.CreatedBy, o.ExpiresAt).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("写人工改判凭据失败: %w", err)
	}
	return id, nil
}

// LookupSodManualOverride 链侧步骤⑤.5 的查表：四元组全等 + 未过期 ⇒ 放行凭据；
// 未命中时回落 task_tool 通配（(tool, task, agent) 全等、payload_digest=''）。
// 命中即把 use_count+1 并盖章 last_used_at（once 用后即焚由 Consume 完成）。
func (d *DB) LookupSodManualOverride(toolKey string, taskID int64, agentKey, payloadDigest string) (*SodManualOverride, error) {
	if err := d.ensureSoDSchema(); err != nil {
		return nil, err
	}
	var o SodManualOverride
	var expires, lastUsed sql.NullTime
	err := d.QueryRow(`
SELECT id, decision_id, payload_digest, tool_key, task_id, agent_key, scope, reason, created_by, created_at, expires_at, use_count, last_used_at
FROM sod_manual_overrides
WHERE tool_key=$1 AND task_id=$2 AND payload_digest=$3 AND agent_key=$4
  AND (expires_at IS NULL OR expires_at > now())
ORDER BY id DESC LIMIT 1`, toolKey, taskID, payloadDigest, agentKey).
		Scan(&o.ID, &o.DecisionID, &o.PayloadDigest, &o.ToolKey, &o.TaskID, &o.AgentKey,
			&o.Scope, &o.Reason, &o.CreatedBy, &o.CreatedAt, &expires, &o.UseCount, &lastUsed)
	if errors.Is(err, sql.ErrNoRows) {
		// E1-5：task_tool 通配回落——ask 批准「同类放行」后的第 2..N 次调用走这里。
		err = d.QueryRow(`
SELECT id, decision_id, payload_digest, tool_key, task_id, agent_key, scope, reason, created_by, created_at, expires_at, use_count, last_used_at
FROM sod_manual_overrides
WHERE tool_key=$1 AND task_id=$2 AND agent_key=$3 AND payload_digest='' AND scope='task_tool'
  AND (expires_at IS NULL OR expires_at > now())
ORDER BY id DESC LIMIT 1`, toolKey, taskID, agentKey).
			Scan(&o.ID, &o.DecisionID, &o.PayloadDigest, &o.ToolKey, &o.TaskID, &o.AgentKey,
				&o.Scope, &o.Reason, &o.CreatedBy, &o.CreatedAt, &expires, &o.UseCount, &lastUsed)
	}
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	if expires.Valid {
		t := expires.Time
		o.ExpiresAt = &t
	}
	if lastUsed.Valid {
		t := lastUsed.Time
		o.LastUsedAt = &t
	}
	return &o, nil
}

// ConsumeSodManualOverride once 型改判的用后即焚：删除该行（消费即焚的语义
// 与执行凭证一致——行存在即代表还有一次放行额度）。
func (d *DB) ConsumeSodManualOverride(id int64) error {
	if err := d.ensureSoDSchema(); err != nil {
		return err
	}
	_, err := d.Exec(`DELETE FROM sod_manual_overrides WHERE id=$1 AND scope='once'`, id)
	return err
}

// ExpireTaskSodOverrides 任务终态时把 task / task_tool 级改判一并过期（随任务生命周期）。
func (d *DB) ExpireTaskSodOverrides(taskID int64) error {
	if err := d.ensureSoDSchema(); err != nil {
		return err
	}
	_, err := d.Exec(`UPDATE sod_manual_overrides SET expires_at=now() WHERE task_id=$1 AND scope IN ('task','task_tool') AND expires_at IS NULL`, taskID)
	return err
}

// ListSodManualOverrides 复核历史回放（职责分离页的过滤器）。
func (d *DB) ListSodManualOverrides(taskID int64, limit int) ([]*SodManualOverride, error) {
	if err := d.ensureSoDSchema(); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	q := `SELECT id, decision_id, payload_digest, tool_key, task_id, agent_key, scope, reason, created_by, created_at, expires_at, use_count, last_used_at
FROM sod_manual_overrides`
	args := []any{}
	if taskID > 0 {
		q += ` WHERE task_id=$1`
		args = append(args, taskID)
	}
	q += fmt.Sprintf(` ORDER BY id DESC LIMIT %d`, limit)
	rows, err := d.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*SodManualOverride{}
	for rows.Next() {
		var o SodManualOverride
		var expires, lastUsed sql.NullTime
		if err := rows.Scan(&o.ID, &o.DecisionID, &o.PayloadDigest, &o.ToolKey, &o.TaskID,
			&o.AgentKey, &o.Scope, &o.Reason, &o.CreatedBy, &o.CreatedAt, &expires, &o.UseCount, &lastUsed); err != nil {
			return nil, err
		}
		if expires.Valid {
			t := expires.Time
			o.ExpiresAt = &t
		}
		if lastUsed.Valid {
			t := lastUsed.Time
			o.LastUsedAt = &t
		}
		out = append(out, &o)
	}
	return out, rows.Err()
}

// GetSoDDecision 读单条决策账本（不存在返回 nil, nil）。
func (d *DB) GetSoDDecision(id int64) (*SoDDecision, error) {
	if err := d.ensureSoDSchema(); err != nil {
		return nil, err
	}
	var rec SoDDecision
	var checks []byte
	err := d.QueryRow(`SELECT `+sodDecisionCols+` FROM sod_decisions WHERE id=$1`, id).
		Scan(&rec.ID, &rec.TaskID, &rec.ConversationID, &rec.SourceAgent, &rec.ToolName,
			&rec.Verdict, &rec.ReasonCode, &rec.PayloadDigest, &rec.PayloadPreview, &rec.IntentDigest,
			&rec.AnomalyFlag, &rec.AnomalyReason, &checks, &rec.TokenDigest, &rec.ScopeDigest,
			&rec.CacheHit, &rec.LatencyMs, &rec.CreatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("读 SoD 决策失败: %w", err)
	}
	rec.Checks = json.RawMessage(checks)
	return &rec, nil
}

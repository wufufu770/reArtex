// 本文件是策略库（L4）「提议 / 运行明细 / 冻结语料」那一层的访问层。
//
// 它与 db/strategies.go（Task 1/2）的分工：
//   - db/strategies.go：策略节点本身（建、读、有界检索、证据链、跨层遍历）；
//   - 本文件：**agent 只能提议**的那一层 + 一次运行的明细 + 冻结的历史语料。
//
// 为什么要有 strategy_proposals 这张表：「提议」若只靠 strategy_evidence 的
// role 硬凑，就分不出「待裁决」与「已裁决」—— agent 提交的东西必须停在 pending，
// 由宿主侧的裁决写（curator / operator）推进到 approved / rejected。
//
// 为什么「失败子任务永不改库」是**结构性**的而不是约定俗成的：
//   - 唯一能改 strategies 列的函数是 ApplyStrategyVotes；
//   - 它先从库里读回那次使用明细（strategy_usage.outcome），确认真的是一次
//     「命中」才动手 —— 失败子任务在库里留下的是 outcome='miss'，因此**即使
//     调用方伪造了分类，闸门也会按库里持久化的结论把它挡下来**；
//   - 同一个函数里的 UPDATE 语句**根本没有 state 列** —— 晋级是裁决写，
//     只能在宿主侧发生（auto-permission-tiers §4.2）。
//
// allow: SIZE_OK —— 本单元的文件域被任务书钉死为四个文件（两个新建 + 两个测试），
// 提议 / usage / 语料三件事共用同一条「先聚合、后改库」的写入路径；拆开会把
// 「失败子任务永不改库」这道结构闸门切散，收益不足以抵过代价。
package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// strategyProposalSchemaSQL 是本层的幂等 DDL。
//
// 为什么 DDL 写在 Go 里而不是 db/schema_strategy.sql：那个文件由 Task 1/2 的
// 单元持有（并行开发中），本单元不改它。两边都是幂等的
// （CREATE TABLE IF NOT EXISTS / CREATE INDEX IF NOT EXISTS），重复应用无副作用；
// schema 属主随时可以把这段内容并进 db/schema_strategy.sql。
const strategyProposalSchemaSQL = `
-- ---------- strategy_proposals ----------
-- agent 只能**提议**；写入由 curator / operator 仲裁（auto-permission-tiers §4.2：
-- 裁决写归 host-only，不归分类器）。route=create_new 时 strategy_id 为空 ——
-- 策略还没建，等裁决通过才落 quarantined。
CREATE TABLE IF NOT EXISTS strategy_proposals (
    id             BIGSERIAL PRIMARY KEY,
    strategy_id    BIGINT REFERENCES strategies(id) ON DELETE CASCADE,
    route          TEXT NOT NULL CHECK (route IN ('edit_existing','create_new','retire')),
    payload        JSONB NOT NULL DEFAULT '{}',
    status         TEXT NOT NULL DEFAULT 'pending'
                   CHECK (status IN ('pending','approved','rejected','superseded')),
    -- reason 是**提议方**给的理由（为什么这条值得提议）；
    -- decided_reason 是**裁决方**给的理由（为什么批 / 为什么驳）。
    -- 两者分开存：合并成一个字段，事后复盘就分不清「谁说的哪句话」。
    reason         TEXT NOT NULL DEFAULT '',
    decided_reason TEXT NOT NULL DEFAULT '',
    created_by     TEXT NOT NULL DEFAULT '',
    decided_by     TEXT NOT NULL DEFAULT '',
    decided_at     TIMESTAMPTZ,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_strategy_proposals_status ON strategy_proposals(status, id DESC);

-- ---------- strategy_usage ----------
-- 一次「策略被注入 → 运行 → 出结果」的明细。它同时是三件事的真相源：
-- retrieval_count 的分母、归因器的「策略上下文」、以及里程碑命中的记录处。
--
-- classification 记归因结论（skill_guided / independent / environment）：
-- 光看 outcome 分不出「策略没命中但子任务成了」（independent）与「子任务本身
-- 失败」（miss）—— 而这两者对策略的意义完全不同（前者是候选来源，后者只是没命中）。
CREATE TABLE IF NOT EXISTS strategy_usage (
    id             BIGSERIAL PRIMARY KEY,
    strategy_id    BIGINT NOT NULL REFERENCES strategies(id) ON DELETE CASCADE,
    task_id        BIGINT NOT NULL,
    exploration_id BIGINT NOT NULL,
    intent_id      BIGINT,
    round_no       INTEGER NOT NULL DEFAULT 0,
    outcome        TEXT NOT NULL DEFAULT 'pending'
                   CHECK (outcome IN ('pending','hit','miss','skipped')),
    classification TEXT NOT NULL DEFAULT ''
                   CHECK (classification IN ('','skill_guided','independent','environment')),
    milestone_hits JSONB NOT NULL DEFAULT '[]',
    -- voted_at 是「这一票已经投过」的收据。
    --
    -- 为什么需要它：归因挂在**意图完成**上，同一个意图可能被归因多次（后续意图
    -- 完成时整批再归因一遍）。没有收据 ⇒ 同一票被反复计分 ⇒ 一次成功被算成多次。
    -- ⇒ 投票只能被「认领」一次（认领 = 把 voted_at 从 NULL 写成 now()）。
    voted_at       TIMESTAMPTZ,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- 兼容「表已由别的单元按旧结构建好」的情况：补列是幂等的。
ALTER TABLE strategy_usage ADD COLUMN IF NOT EXISTS voted_at TIMESTAMPTZ;
CREATE INDEX IF NOT EXISTS idx_strategy_usage_strategy ON strategy_usage(strategy_id, id DESC);
CREATE INDEX IF NOT EXISTS idx_strategy_usage_task ON strategy_usage(task_id, id DESC);
CREATE INDEX IF NOT EXISTS idx_strategy_usage_intent ON strategy_usage(intent_id) WHERE intent_id IS NOT NULL;
-- 归一化唯一键：同一策略在同一任务的同一意图上只有一条使用明细（先插后更新）。
-- COALESCE(intent_id,0) 归一 NULL（范式同 uq_strategies_company_name）：
-- 不归一的话 NULL 互不相等 ⇒ 同一条使用明细能插出无限多行。
CREATE UNIQUE INDEX IF NOT EXISTS uq_strategy_usage_scope
    ON strategy_usage(strategy_id, task_id, COALESCE(intent_id, 0));

-- ---------- strategy_replay_corpus ----------
-- 冻结的历史运行语料 = 重放闸门的可比集。**冻结即不可变**：
-- 闸门要比的是「当时的世界」，语料被后来的结果改写，闸门就失去意义。
-- ⇒ 写入只有 INSERT ... ON CONFLICT DO NOTHING，本仓库里没有任何一处
--    UPDATE / DELETE 动它。
CREATE TABLE IF NOT EXISTS strategy_replay_corpus (
    id               BIGSERIAL PRIMARY KEY,
    task_id          BIGINT NOT NULL,
    asset_signature  JSONB NOT NULL DEFAULT '{}',
    milestone_hits   JSONB NOT NULL DEFAULT '[]',
    finding_outcomes JSONB NOT NULL DEFAULT '[]',
    frozen_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- jsonb 没有 btree 等值算子 ⇒ 用 md5 归一做唯一键。
CREATE UNIQUE INDEX IF NOT EXISTS uq_strategy_replay_corpus
    ON strategy_replay_corpus(task_id, md5(asset_signature::text));

-- ---------- task_strategy_snapshot ----------
-- 组内共享的策略版本快照。一个群 = 一个 task ⇒ 按 task_id 存就天然组内共享，
-- 避免组内成员用不同版本的策略互相污染（design §3.3）。
CREATE TABLE IF NOT EXISTS task_strategy_snapshot (
    task_id     BIGINT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    strategy_id BIGINT NOT NULL REFERENCES strategies(id) ON DELETE CASCADE,
    version     BIGINT NOT NULL DEFAULT 1,
    -- 快照自带摘要：策略退役后快照仍可读（审计与复盘需要）。
    summary     TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (task_id, strategy_id)
);
`

// strategyProposalSchemaReady 记录某个连接已经应用过本层的 DDL。
//
// 为什么用包级 sync.Map 而不是 DB 上的字段：DB 结构体在 db/db.go 里，本单元不改它。
// 键是底层 *sql.DB 指针 —— 每个测试包 / 实例各连各的库，互不干扰。
var strategyProposalSchemaReady sync.Map

// 提议状态机。pending 是唯一的「刚提出来」状态。
const (
	StrategyProposalPending    = "pending"
	StrategyProposalApproved   = "approved"
	StrategyProposalRejected   = "rejected"
	StrategyProposalSuperseded = "superseded"
)

// 提议路由。
const (
	StrategyRouteEditExisting = "edit_existing"
	StrategyRouteCreateNew    = "create_new"
	StrategyRouteRetire       = "retire"
)

// 一次策略使用的结果。
const (
	StrategyUsagePending = "pending"
	StrategyUsageHit     = "hit"
	StrategyUsageMiss    = "miss"
	StrategyUsageSkipped = "skipped"
)

// 归因三分类。值的唯一真相源在这里 —— server 层只做别名
// （server.AttributionSkillGuided = db.StrategyAttributionSkillGuided），
// 免得同一套词在两个包里各写一遍、各改一遍。
//
// 为什么只有三类：归因要回答的只有一个问题 ——「这次成功，策略有没有出力」。
//   - skill_guided：策略在上下文里且它的里程碑命中了 ⇒ 策略有功；
//   - independent：策略在上下文里但里程碑没命中 ⇒ 成功是别的路径挣来的
//     （策略是**候选来源**，不是胜利）；
//   - environment：没有策略上下文，或上下文里有多条策略（前提「至多一个策略
//     上下文」被破坏）⇒ 不可归因。
const (
	StrategyAttributionSkillGuided = "skill_guided"
	StrategyAttributionIndependent = "independent"
	StrategyAttributionEnvironment = "environment"
)

// StrategyProposal 是「agent 提议、宿主裁决」的那一层。
type StrategyProposal struct {
	ID            int64
	StrategyID    int64
	Route         string // edit_existing | create_new | retire
	Payload       json.RawMessage
	Status        string // pending | approved | rejected | superseded
	Reason        string
	DecidedReason string
	CreatedBy     string
	DecidedBy     string
	DecidedAt     *time.Time
	CreatedAt     time.Time
}

// StrategyUsage 是一次「策略被注入 → 运行 → 出结果」的明细。
//
// Classification 是归因结论（skill_guided / independent / environment）。
// ⚠️ 它是本单元在计划给出的字段之外的**追加**：计划的表结构里没有这一列，
// 而没有它就分不出「策略没命中但子任务成了」与「子任务本身失败」—— 对策略
// 的意义完全不同（前者是候选来源，后者只是没命中）。
type StrategyUsage struct {
	StrategyID     int64
	TaskID         int64
	ExplorationID  int64
	IntentID       int64
	RoundNo        int
	Outcome        string // pending | hit | miss | skipped
	Classification string
	MilestoneHits  []string
}

// StrategyVote 是一次「策略投票」—— 归因器唯一能交给库去改策略列的东西。
//
// ⚠️ 构造它的地方（server 层）不能直接改库；能改库的只有 ApplyStrategyVotes，
// 而它会先从库里读回那次使用明细核对结论（见该函数注释）。
type StrategyVote struct {
	StrategyID     int64
	TaskID         int64
	ExplorationID  int64
	IntentID       int64
	NodeID         int64
	Classification string
	Outcome        string
	MilestoneHits  []string
	// 证据引用（可空）。ref_type 取 strategy_evidence 的枚举：
	// finding | traffic_snapshot | activity | tool_call | run。
	EvidenceRefType string
	EvidenceRefID   string
	ContentDigest   string
}

// ReplayCandidate 是一次待裁决的变更（用于重放打分）。
type ReplayCandidate struct {
	Activation json.RawMessage
	Milestones []string
}

// ReplayCorpusRow 是冻结语料里的一条案例。
type ReplayCorpusRow struct {
	ID              int64
	TaskID          int64
	AssetSignature  json.RawMessage
	MilestoneHits   []string
	FindingOutcomes []string
	FrozenAt        time.Time
}

// EnsureStrategyProposalSchema 应用本层 DDL（幂等）。
//
// ⚠️ **启动 wiring 尚未落地**：本单元不改 db/db.go（该文件由另一个单元并行持有），
// 因此 db.Open 之后没有自动调用本函数。首次调用会在建表前补齐 DDL，
// 但「进程启动即建表」这一步属于编排任务，不能漏。
//
// 本函数会先确保 Task 1 的策略库 DDL 已应用 —— 本层的表都引用 strategies(id)。
func (d *DB) EnsureStrategyProposalSchema() error {
	if err := d.EnsureStrategySchema(); err != nil {
		return err
	}
	if _, ok := strategyProposalSchemaReady.Load(d.DB); ok {
		return nil
	}
	if _, err := d.Exec(strategyProposalSchemaSQL); err != nil {
		return fmt.Errorf("应用策略提议层 schema 失败: %w", err)
	}
	strategyProposalSchemaReady.Store(d.DB, struct{}{})
	return nil
}

// ensureStrategyProposalSchema 是访问层的幂等入口：首次调用时建表，之后只是一次 map 查表。
func (d *DB) ensureStrategyProposalSchema() error { return d.EnsureStrategyProposalSchema() }

// CreateStrategyProposal 记一条待裁决的提议。
//
// 【结构性保证：agent 侧只能提议】—— 创建路径写不出「已裁决」的提议：
// status 被强制成 pending，裁决只能走 DecideStrategyProposal（宿主侧裁决写，
// 且必须写明裁决人与理由，并进 audit_events 哈希链）。
func (d *DB) CreateStrategyProposal(p *StrategyProposal) (int64, error) {
	if err := d.ensureStrategyProposalSchema(); err != nil {
		return 0, err
	}
	if p == nil {
		return 0, errors.New("提议参数为空")
	}
	switch p.Route {
	case StrategyRouteEditExisting, StrategyRouteCreateNew, StrategyRouteRetire:
	default:
		return 0, fmt.Errorf("未知的提议路由 %q", p.Route)
	}
	payload := p.Payload
	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	}
	// 提议永远从 pending 起步：agent 说「approved」没有意义，裁决权不在它手里。
	p.Status = StrategyProposalPending
	var id int64
	err := d.QueryRow(`
INSERT INTO strategy_proposals(strategy_id, route, payload, status, reason, created_by)
VALUES($1,$2,$3,$4,$5,$6) RETURNING id`,
		nullStrategyID(p.StrategyID), p.Route, string(payload), p.Status, p.Reason, p.CreatedBy).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("记策略提议失败: %w", err)
	}
	p.ID = id
	p.Status = StrategyProposalPending
	return id, nil
}

// nullStrategyID 把 0 变成 NULL（create_new 还没有策略）。
func nullStrategyID(id int64) any {
	if id <= 0 {
		return nil
	}
	return id
}

// ListStrategyProposals 列出提议。status 为空时列出全部 pending。
//
// ⚠️ 空 status **不是**「列出全部」而是「列出 pending」：待裁决的提议是这份
// 清单的主要用途（「现在有什么等着我批」），把它和历史混在一起会让运营看不到
// 该批什么。要看历史就显式传 approved / rejected。
func (d *DB) ListStrategyProposals(status string, limit int) ([]StrategyProposal, error) {
	if err := d.ensureStrategyProposalSchema(); err != nil {
		return nil, err
	}
	if status == "" {
		status = StrategyProposalPending
	}
	if limit <= 0 {
		limit = 100
	}
	rows, err := d.Query(`
SELECT id, COALESCE(strategy_id,0), route, payload, status, reason, decided_reason,
       created_by, decided_by, decided_at, created_at
FROM strategy_proposals WHERE status=$1 ORDER BY id DESC LIMIT $2`, status, limit)
	if err != nil {
		return nil, fmt.Errorf("列策略提议失败: %w", err)
	}
	defer rows.Close()
	out := make([]StrategyProposal, 0, 16)
	for rows.Next() {
		var p StrategyProposal
		var payload []byte
		if err := rows.Scan(&p.ID, &p.StrategyID, &p.Route, &payload, &p.Status, &p.Reason,
			&p.DecidedReason, &p.CreatedBy, &p.DecidedBy, &p.DecidedAt, &p.CreatedAt); err != nil {
			return nil, fmt.Errorf("列策略提议失败: %w", err)
		}
		p.Payload = json.RawMessage(payload)
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("列策略提议失败: %w", err)
	}
	return out, nil
}

// DecideStrategyProposal 裁决一条提议。
//
// 裁决是**裁决写**，只从宿主侧调用，且必须写明裁决人与理由 —— 理由与裁决人
// 一起进 audit_events 哈希链（事后可查「谁批的、为什么批」）。
//
// ⚠️ 只接受 approved / rejected：pending 与 superseded 不是裁决结果。
func (d *DB) DecideStrategyProposal(id int64, status, decidedBy, reason string) error {
	if err := d.ensureStrategyProposalSchema(); err != nil {
		return err
	}
	switch status {
	case StrategyProposalApproved, StrategyProposalRejected:
	default:
		return fmt.Errorf("裁决结果只能是 %s 或 %s（收到 %q）",
			StrategyProposalApproved, StrategyProposalRejected, status)
	}
	if strings.TrimSpace(decidedBy) == "" {
		return errors.New("裁决必须写明裁决人")
	}
	if strings.TrimSpace(reason) == "" {
		return errors.New("裁决必须写明理由")
	}
	var route string
	var strategyID int64
	if err := d.QueryRow(`UPDATE strategy_proposals
SET status=$2, decided_by=$3, decided_reason=$4, decided_at=now()
WHERE id=$1 AND status=$5
RETURNING route, COALESCE(strategy_id,0)`, id, status, decidedBy, reason,
		StrategyProposalPending).Scan(&route, &strategyID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("提议 %d 不存在或已裁决", id)
		}
		return fmt.Errorf("裁决策略提议失败: %w", err)
	}
	payload, _ := json.Marshal(map[string]any{
		"proposal_id": id,
		"route":       route,
		"strategy_id": strategyID,
		"status":      status,
	})
	if _, err := d.AppendAuditEvent(AuditEvent{
		Actor:    decidedBy,
		ToolName: "strategy_proposal",
		Action:   "decide:" + status,
		Reason:   reason,
		Payload:  payload,
	}); err != nil {
		return fmt.Errorf("裁决写审计链失败: %w", err)
	}
	return nil
}

// RecordStrategyUsage 记一次策略注入与运行结果。
//
// 它同时是三件事的真相源：retrieval_count 的分母、归因器的「策略上下文」、
// 以及里程碑命中的记录处。同一策略 / 同一任务 / 同一意图上只有一行 ——
// 重复记录是**更新**而不是插新行（注入时写 pending，结算时改成 hit / miss）。
//
// 【结算只认第一次】：更新带 WHERE strategy_usage.outcome='pending' ——
// 已经结算过的行不再被改写。归因挂在意图完成上，同一个意图可能被归因多次；
// 若第二次能把 outcome 从 hit 翻成 miss，票与结论就对不上了。
func (d *DB) RecordStrategyUsage(u StrategyUsage) error {
	if err := d.ensureStrategyProposalSchema(); err != nil {
		return err
	}
	if u.StrategyID <= 0 || u.TaskID <= 0 {
		return errors.New("策略使用明细必须带策略与任务")
	}
	switch u.Outcome {
	case "", StrategyUsagePending, StrategyUsageHit, StrategyUsageMiss, StrategyUsageSkipped:
	default:
		return fmt.Errorf("未知的策略使用结果 %q", u.Outcome)
	}
	if u.Outcome == "" {
		u.Outcome = StrategyUsagePending
	}
	hits, err := json.Marshal(nonNilStrings(u.MilestoneHits))
	if err != nil {
		return fmt.Errorf("序列化里程碑命中失败: %w", err)
	}
	_, err = d.Exec(`
INSERT INTO strategy_usage(strategy_id, task_id, exploration_id, intent_id, round_no, outcome, classification, milestone_hits)
VALUES($1,$2,$3,$4,$5,$6,$7,$8)
ON CONFLICT (strategy_id, task_id, COALESCE(intent_id,0)) DO UPDATE
SET outcome=EXCLUDED.outcome, classification=EXCLUDED.classification,
    milestone_hits=EXCLUDED.milestone_hits, round_no=EXCLUDED.round_no,
    exploration_id=EXCLUDED.exploration_id, updated_at=now()
WHERE strategy_usage.outcome='pending'`,
		u.StrategyID, u.TaskID, u.ExplorationID, nullStrategyID(u.IntentID), u.RoundNo,
		u.Outcome, u.Classification, string(hits))
	if err != nil {
		return fmt.Errorf("记策略使用明细失败: %w", err)
	}
	return nil
}

// StrategyUsageForTask 列出某任务的全部策略使用明细。
//
// 归因器用它还原「每个意图的策略上下文」—— 一次读全量，避免按意图逐条查。
func (d *DB) StrategyUsageForTask(taskID int64) ([]StrategyUsage, error) {
	if err := d.ensureStrategyProposalSchema(); err != nil {
		return nil, err
	}
	rows, err := d.Query(`
SELECT strategy_id, task_id, exploration_id, COALESCE(intent_id,0), round_no, outcome,
       classification, milestone_hits
FROM strategy_usage WHERE task_id=$1 ORDER BY id`, taskID)
	if err != nil {
		return nil, fmt.Errorf("读策略使用明细失败: %w", err)
	}
	defer rows.Close()
	out := make([]StrategyUsage, 0, 16)
	for rows.Next() {
		var u StrategyUsage
		var hits []byte
		if err := rows.Scan(&u.StrategyID, &u.TaskID, &u.ExplorationID, &u.IntentID, &u.RoundNo,
			&u.Outcome, &u.Classification, &hits); err != nil {
			return nil, fmt.Errorf("读策略使用明细失败: %w", err)
		}
		u.MilestoneHits = decodeStringArray(hits)
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("读策略使用明细失败: %w", err)
	}
	return out, nil
}

// ApplyStrategyVotes 把一批投票一次性应用到策略上。
//
// 【这是本层唯一能改 strategies 列的函数】—— 其余写入路径只碰
// strategy_proposals / strategy_usage / strategy_replay_corpus。
//
// 三道结构性闸门：
//  1. 分类闸门：只接受 skill_guided。independent（自主探索成功）与
//     environment（不可归因）不给策略加分（反模式 10：一次成功不能当通用能力）。
//  2. 状态闸门（**结构性的关键**）：投票必须建立在一次**已结算且命中**的
//     使用明细上 —— 这里从库里把 strategy_usage.outcome 读回来核对。
//     失败子任务在库里留下的是 outcome='miss' ⇒ 即使调用方伪造了分类，
//     闸门也会按库里持久化的结论把它挡下来。
//  3. 幂等闸门：每一票只能被**认领**一次（把 usage 行的 voted_at 从 NULL
//     写成 now()）。归因挂在意图完成上，同一个意图会被反复归因；没有这道
//     闸门，一次成功会被算成很多次票。
//
// 聚合后再改库：调用方必须先把整批投票攒齐再调一次本函数（本函数内部是
// 一个事务，逐条应用）。
func (d *DB) ApplyStrategyVotes(votes []StrategyVote) error {
	if len(votes) == 0 {
		return nil
	}
	if err := d.ensureStrategyProposalSchema(); err != nil {
		return err
	}
	tx, err := d.Begin()
	if err != nil {
		return fmt.Errorf("开事务失败: %w", err)
	}
	defer tx.Rollback()
	for _, v := range votes {
		if v.Classification != StrategyAttributionSkillGuided {
			return fmt.Errorf("只有 %s 的票能改库（收到 %q）：自主探索的成功与不可归因的子任务都不给策略记功",
				StrategyAttributionSkillGuided, v.Classification)
		}
		if v.StrategyID <= 0 || v.TaskID <= 0 || v.IntentID <= 0 {
			return errors.New("投票必须带策略、任务与意图标识")
		}
		var outcome string
		err := tx.QueryRow(`
SELECT outcome FROM strategy_usage
WHERE strategy_id=$1 AND task_id=$2 AND COALESCE(intent_id,0)=$3`,
			v.StrategyID, v.TaskID, v.IntentID).Scan(&outcome)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("策略 %d 没有对应的使用明细：投票必须建立在一次已结算的运行上", v.StrategyID)
		}
		if err != nil {
			return fmt.Errorf("读策略使用明细失败: %w", err)
		}
		if outcome != StrategyUsageHit {
			return fmt.Errorf("策略 %d 的运行结果是 %q（不是 %q）：失败子任务永不直接改库",
				v.StrategyID, outcome, StrategyUsageHit)
		}
		// 认领这票：只有把 voted_at 从 NULL 改成 now() 的那一方才继续计分。
		var usageID int64
		err = tx.QueryRow(`
UPDATE strategy_usage SET voted_at=now(), updated_at=now()
WHERE strategy_id=$1 AND task_id=$2 AND COALESCE(intent_id,0)=$3 AND voted_at IS NULL
RETURNING id`, v.StrategyID, v.TaskID, v.IntentID).Scan(&usageID)
		if errors.Is(err, sql.ErrNoRows) {
			continue // 这一票已经投过了 ⇒ 幂等，不重复计分
		}
		if err != nil {
			return fmt.Errorf("认领策略投票失败: %w", err)
		}
		if v.EvidenceRefType != "" && v.EvidenceRefID != "" {
			digest := v.ContentDigest
			if digest == "" {
				digest = "sha256:" + fmt.Sprint(v.EvidenceRefType, ":", v.EvidenceRefID)
			}
			if _, err := tx.Exec(`
INSERT INTO strategy_evidence(strategy_id, ref_type, ref_id, content_digest, role)
VALUES($1,$2,$3,$4,'validated_by') ON CONFLICT DO NOTHING`,
				v.StrategyID, v.EvidenceRefType, v.EvidenceRefID, digest); err != nil {
				return fmt.Errorf("挂策略证据失败: %w", err)
			}
		}
		// ⚠️ 这条 UPDATE 里**没有 state 列** —— 归因器没有晋级权。
		if _, err := tx.Exec(`
UPDATE strategies
SET confidence = confidence + 1, run_utility = run_utility + 1,
    usage_count = usage_count + 1, hit_count = hit_count + 1,
    updated_at = now()
WHERE id=$1 AND state IN ($2,$3)`, v.StrategyID,
			StrategyQuarantined, StrategyActive); err != nil {
			return fmt.Errorf("应用策略投票失败: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交策略投票失败: %w", err)
	}
	return nil
}

// FreezeReplayCorpus 把一次运行的资产签名与里程碑结果**冻结**成语料案例。
//
// 冻结 = 不可变：重放闸门要比的就是「当时的世界」，语料一旦被后来结果改写，
// 闸门就失去意义。⇒ 用 ON CONFLICT DO NOTHING —— 第二次冻结**不覆盖**已有案例。
//
// sig 是任务的资产图签名（db.AssetSignature，Task 2 定义）：它的键直接取自
// 现有资产图的列（asset_type / service_type / category / record_type），
// 不另造资产类型体系（设计 §1.1）。
func (d *DB) FreezeReplayCorpus(taskID int64, sig AssetSignature, milestones []string, outcomes []string) error {
	if err := d.ensureStrategyProposalSchema(); err != nil {
		return err
	}
	if taskID <= 0 {
		return errors.New("冻结语料必须带任务标识")
	}
	raw, err := json.Marshal(sig)
	if err != nil {
		return fmt.Errorf("序列化资产签名失败: %w", err)
	}
	signature := normalizeReplaySignature(raw)
	hits, err := json.Marshal(nonNilStrings(milestones))
	if err != nil {
		return fmt.Errorf("序列化里程碑失败: %w", err)
	}
	outs, err := json.Marshal(nonNilStrings(outcomes))
	if err != nil {
		return fmt.Errorf("序列化漏洞结局失败: %w", err)
	}
	// ON CONFLICT DO NOTHING ⇒ 语料只写一次，之后永不覆盖。
	if _, err := d.Exec(`
INSERT INTO strategy_replay_corpus(task_id, asset_signature, milestone_hits, finding_outcomes)
VALUES($1,$2,$3,$4)
ON CONFLICT (task_id, md5(asset_signature::text)) DO NOTHING`,
		taskID, string(signature), string(hits), string(outs)); err != nil {
		return fmt.Errorf("冻结重放语料失败: %w", err)
	}
	return nil
}

// ListReplayCorpus 列出某个任务（或全部任务）的冻结语料。
func (d *DB) ListReplayCorpus(taskID int64) ([]ReplayCorpusRow, error) {
	if err := d.ensureStrategyProposalSchema(); err != nil {
		return nil, err
	}
	q := `SELECT id, task_id, asset_signature, milestone_hits, finding_outcomes, frozen_at
FROM strategy_replay_corpus`
	var args []any
	if taskID > 0 {
		q += ` WHERE task_id=$1`
		args = append(args, taskID)
	}
	q += ` ORDER BY id`
	rows, err := d.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("读重放语料失败: %w", err)
	}
	defer rows.Close()
	out := make([]ReplayCorpusRow, 0, 8)
	for rows.Next() {
		var r ReplayCorpusRow
		var sig, hits, outs []byte
		if err := rows.Scan(&r.ID, &r.TaskID, &sig, &hits, &outs, &r.FrozenAt); err != nil {
			return nil, fmt.Errorf("读重放语料失败: %w", err)
		}
		r.AssetSignature = json.RawMessage(sig)
		r.MilestoneHits = decodeStringArray(hits)
		r.FindingOutcomes = decodeStringArray(outs)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("读重放语料失败: %w", err)
	}
	return out, nil
}

// ReplayScore 在冻结语料上给一份候选打分。
//
// 打分口径（确定性，不经 LLM）：
//   - 命中案例 = 语料案例的资产签名与候选 activation 相交（与检索同一套谓词：
//     候选 activation 里出现的每个键，都必须与案例资产签名的同名键有交集）；
//   - score = 「命中案例里，候选里程碑至少命中一个的比例」。
//
// comparable=false ⇒ **没有可比语料** ⇒ 保守默认 = 不采纳（design §4.3：
// 「没有冻结语料可比时 → 不采纳」）。
func (d *DB) ReplayScore(candidate ReplayCandidate) (float64, bool, error) {
	if err := d.ensureStrategyProposalSchema(); err != nil {
		return 0, false, err
	}
	rows, err := d.Query(`
SELECT asset_signature, milestone_hits FROM strategy_replay_corpus ORDER BY id`)
	if err != nil {
		return 0, false, fmt.Errorf("读重放语料失败: %w", err)
	}
	defer rows.Close()
	activation := parseActivationSets(candidate.Activation)
	comparable, matched := 0, 0
	for rows.Next() {
		var sig, hits []byte
		if err := rows.Scan(&sig, &hits); err != nil {
			return 0, false, fmt.Errorf("读重放语料失败: %w", err)
		}
		if !activationIntersects(activation, parseActivationSets(sig)) {
			continue
		}
		comparable++
		if intersectsStrings(candidate.Milestones, decodeStringArray(hits)) {
			matched++
		}
	}
	if err := rows.Err(); err != nil {
		return 0, false, fmt.Errorf("读重放语料失败: %w", err)
	}
	if comparable == 0 {
		return 0, false, nil
	}
	return float64(matched) / float64(comparable), true, nil
}

// ReplaySignatureJSON 把「资产类型 / 服务类型 / 类别 / 记录类型」四组键值拼成
// 冻结语料的资产签名 JSONB。
//
// 为什么需要它：db.AssetSignature（Task 2 定义）与这里的 JSON 形状一致，
// 但本单元不引用那个类型（并行开发中，重复声明同名类型会让整个 db 包编译失败）。
// ⇒ 调用方要么用本函数拼，要么 json.Marshal 自己那份。
func ReplaySignatureJSON(assetTypes, serviceTypes, categories, recordTypes []string) json.RawMessage {
	raw, err := json.Marshal(map[string][]string{
		"asset_type":   nonNilStrings(assetTypes),
		"service_type": nonNilStrings(serviceTypes),
		"category":     nonNilStrings(categories),
		"record_type":  nonNilStrings(recordTypes),
	})
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return raw
}

// normalizeReplaySignature 把资产签名归一成「四个已知键都在」的形状。
//
// 为什么归一：语料的唯一键是 md5(asset_signature::text)，不归一的话
// 「同样的资产集合、键序或空值不同」会算成两个 md5 ⇒ 语料被写两遍，
// 重放闸门拿到的可比集里混着同一条案例的多个副本。
func normalizeReplaySignature(sig json.RawMessage) json.RawMessage {
	sets := parseActivationSets(sig)
	return ReplaySignatureJSON(sets["asset_type"], sets["service_type"],
		sets["category"], sets["record_type"])
}

// parseActivationSets 把 activation / 资产签名 JSON 解析成「键 → 值集合」。
//
// 值的形状有两种：单个字符串（{"asset_type":"site"}）或字符串数组
// （{"asset_type":["site"]}）—— 与检索侧的谓词解析同形。
//
// 解析失败或不是对象 ⇒ 空集合（没有约束）：宁可少一条可比案例，
// 也不让一份坏 JSON 变成「匹配一切」。
func parseActivationSets(raw json.RawMessage) map[string][]string {
	out := map[string][]string{}
	if len(raw) == 0 {
		return out
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return out
	}
	for k, v := range m {
		out[k] = decodeActivationValues(v)
	}
	return out
}

// decodeActivationValues 读一个 activation 键的取值（单个字符串或字符串数组）。
func decodeActivationValues(raw json.RawMessage) []string {
	var one string
	if err := json.Unmarshal(raw, &one); err == nil {
		if one == "" {
			return nil
		}
		return []string{one}
	}
	var many []string
	if err := json.Unmarshal(raw, &many); err != nil {
		return nil
	}
	return many
}

// activationIntersects 判定候选 activation 与案例资产签名是否可比。
//
// 口径与检索侧的结构谓词同形：没声明的键不约束；声明了的键必须有交集；
// 声明了但取值为空 ⇒ 不匹配（空谓词不是「匹配一切」，是「没写清楚」）。
func activationIntersects(activation, signature map[string][]string) bool {
	for key, want := range activation {
		if len(want) == 0 {
			return false
		}
		if !intersectsStrings(want, signature[key]) {
			return false
		}
	}
	return true
}

// intersectsStrings 判定两个字符串集合是否有交集。
func intersectsStrings(a, b []string) bool {
	if len(a) == 0 || len(b) == 0 {
		return false
	}
	set := make(map[string]bool, len(b))
	for _, v := range b {
		set[v] = true
	}
	for _, v := range a {
		if set[v] {
			return true
		}
	}
	return false
}

// nonNilStrings 把空切片换成空数组（JSON 里是 [] 而不是 null）。
func nonNilStrings(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}

// decodeStringArray 读一个 JSON 字符串数组；坏数据退化成空数组（fail closed）。
func decodeStringArray(raw []byte) []string {
	if len(raw) == 0 {
		return []string{}
	}
	var out []string
	if err := json.Unmarshal(raw, &out); err != nil {
		return []string{}
	}
	return nonNilStrings(out)
}

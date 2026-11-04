// 本文件是策略库（L4）的【宿主侧接线】层：任务共享的策略版本快照 + operator
// 裁决写所需的窄访问面。
//
// 为什么与 db/strategies.go（策略节点本体：建、读、有界检索、证据链、跨层遍历）
// 分开：那个文件由策略库主线的单元持有（Task 1/2），本单元不改它。快照与
// operator 支撑是 Task 6 的产物，落在这里可避免并行开发时改同一个文件。
//
// 为什么快照按 task_id 存：一个群 = 一个 task ⇒ 组内成员天然用同一份策略版本
// （design §3.3）。组内成员用不同版本的策略互相污染，比「用旧策略」更糟。
//
// allow: SIZE_OK —— 本单元的文件域被任务书钉死（快照 + operator 支撑同属「宿主侧
// 接线」这一件事：快照的读者就是 operator 侧与注入侧）；拆开会把「快照只带摘要」
// 这条约束的检查点切散。

package db

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
)

// ---------- 任务共享的策略版本快照 ----------

// StrategySnapshotEntry 是快照里的一条策略摘要。
//
// ⚠️ 这个类型【故意没有 steps 字段】：快照只承载摘要，策略正文按需加载。
// 「注入上下文只带摘要」这条约束因此是**结构性的** —— 快照类型里根本没有正文
// 可带，注入路径拿不到 steps。反模式 8：把可执行产物塞进上下文。
type StrategySnapshotEntry struct {
	StrategyID int64
	Name       string
	Summary    string
	Version    int64
	Utility    float64
}

// strategySnapshotSchemaSQL 是本层的幂等 DDL。
//
// 为什么单独建一张「快照收据」表：task_strategy_snapshot 按 (task_id, strategy_id)
// 存，只能表达「快照里有这些策略」。库里一条 active 策略都没有时，快照是空的，
// 而空快照与「还没生成」在表里长得一模一样 ⇒ 每次调用都会重算 ⇒ planner 看到的
// 策略集会随库的变动漂移，与「组内同版本」直接冲突。这张表把「已生成」记下来。
const strategySnapshotSchemaSQL = `
-- ---------- task_strategy_snapshot_meta ----------
-- 快照的生成收据：记下「这个任务的快照已经算过」，让空快照也只算一次。
CREATE TABLE IF NOT EXISTS task_strategy_snapshot_meta (
    task_id        BIGINT PRIMARY KEY REFERENCES tasks(id) ON DELETE CASCADE,
    strategy_count INTEGER NOT NULL DEFAULT 0,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
`

// strategySnapshotSchemaReady 记录某个连接已经应用过本层的 DDL。
//
// 键是底层 *sql.DB 指针（与 db/strategies.go 的同款做法）：每个测试包/实例各连
// 各的库，互不干扰。
var strategySnapshotSchemaReady sync.Map

// EnsureStrategySnapshotSchema 应用本层 DDL（幂等）。
func (d *DB) EnsureStrategySnapshotSchema() error {
	if err := d.EnsureStrategyProposalSchema(); err != nil {
		return err
	}
	if _, ok := strategySnapshotSchemaReady.Load(d.DB); ok {
		return nil
	}
	if _, err := d.Exec(strategySnapshotSchemaSQL); err != nil {
		return fmt.Errorf("应用策略快照层 schema 失败: %w", err)
	}
	strategySnapshotSchemaReady.Store(d.DB, struct{}{})
	return nil
}

// ensureStrategySnapshotSchema 是访问层的幂等入口。
func (d *DB) ensureStrategySnapshotSchema() error { return d.EnsureStrategySnapshotSchema() }

// TaskStrategySnapshot 返回任务共享的策略快照（组内同版本）。
//
// 摘要取自**快照行**（生成那一刻的 summary 与 version）：策略后来改了正文或退役，
// 快照仍可读 —— 审计与复盘需要「当时那一份」。
func (d *DB) TaskStrategySnapshot(taskID int64) ([]StrategySnapshotEntry, error) {
	if err := d.ensureStrategySnapshotSchema(); err != nil {
		return nil, err
	}
	rows, err := d.Query(`
SELECT sn.strategy_id, st.name, sn.summary, sn.version, COALESCE(u.utility_raw, st.utility)
  FROM task_strategy_snapshot sn
  JOIN strategies st ON st.id = sn.strategy_id
  LEFT JOIN strategy_utility u ON u.strategy_id = sn.strategy_id
 WHERE sn.task_id = $1
 ORDER BY sn.strategy_id`, taskID)
	if err != nil {
		return nil, fmt.Errorf("读任务策略快照失败: %w", err)
	}
	defer rows.Close()
	out := []StrategySnapshotEntry{}
	for rows.Next() {
		var e StrategySnapshotEntry
		if err := rows.Scan(&e.StrategyID, &e.Name, &e.Summary, &e.Version, &e.Utility); err != nil {
			return nil, fmt.Errorf("读任务策略快照失败: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("读任务策略快照失败: %w", err)
	}
	return out, nil
}

// EnsureTaskStrategySnapshot 为任务生成（或复用）策略版本快照。
//
// 幂等：已有快照就直接复用（组内成员必须用**同一版本**的策略，否则会用不同版本
// 的策略互相污染 —— design §3.3）。
//
// 快照只存**摘要**；steps 不进快照（正文按需加载）。
//
// 资产签名走 scope 注入版查询（TaskAssetSignature）：直接查 assets 全表会让范围外
// 资产进签名，策略匹配就成了一条范围外的旁路（Phase 0 的读面隔离白做）。
// 这是**安全前提**，不是性能优化。
func (d *DB) EnsureTaskStrategySnapshot(taskID int64) ([]StrategySnapshotEntry, error) {
	if taskID <= 0 {
		return nil, errors.New("任务 id 不合法")
	}
	if err := d.ensureStrategySnapshotSchema(); err != nil {
		return nil, err
	}
	entries, err := d.TaskStrategySnapshot(taskID)
	if err != nil {
		return nil, err
	}
	if len(entries) > 0 {
		return entries, nil
	}
	// 空快照：查收据，区分「已生成（当时库里没有可用策略）」与「还没生成」。
	taken, err := d.strategySnapshotTaken(taskID)
	if err != nil {
		return nil, err
	}
	if taken {
		return []StrategySnapshotEntry{}, nil
	}
	sig, err := d.Assets().TaskAssetSignature(taskID)
	if err != nil {
		return nil, fmt.Errorf("算任务资产签名失败: %w", err)
	}
	matches, err := d.SearchActiveStrategies(taskID, sig, StrategySearchTopK, StrategyMinRelevance)
	if err != nil {
		return nil, err
	}
	if err := d.writeTaskStrategySnapshot(taskID, matches); err != nil {
		return nil, err
	}
	return d.TaskStrategySnapshot(taskID)
}

// strategySnapshotTaken 报告该任务的快照是否已经生成过（含「生成时是空的」）。
func (d *DB) strategySnapshotTaken(taskID int64) (bool, error) {
	var exists bool
	err := d.QueryRow(`SELECT EXISTS (SELECT 1 FROM task_strategy_snapshot_meta WHERE task_id=$1)`, taskID).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("读策略快照收据失败: %w", err)
	}
	return exists, nil
}

// writeTaskStrategySnapshot 把一批候选写进快照并落收据（一个事务）。
//
// 只写摘要与版本：快照里没有正文，注入路径也就没有正文可带。
func (d *DB) writeTaskStrategySnapshot(taskID int64, matches []StrategyMatch) error {
	tx, err := d.Begin()
	if err != nil {
		return fmt.Errorf("开事务失败: %w", err)
	}
	defer tx.Rollback()
	for _, m := range matches {
		if _, err := tx.Exec(`
INSERT INTO task_strategy_snapshot(task_id, strategy_id, version, summary)
VALUES($1,$2,$3,$4) ON CONFLICT (task_id, strategy_id) DO NOTHING`,
			taskID, m.ID, m.Version, m.Summary); err != nil {
			return fmt.Errorf("写任务策略快照失败: %w", err)
		}
	}
	if _, err := tx.Exec(`
INSERT INTO task_strategy_snapshot_meta(task_id, strategy_count)
VALUES($1,$2) ON CONFLICT (task_id) DO NOTHING`, taskID, len(matches)); err != nil {
		return fmt.Errorf("写任务策略快照收据失败: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交任务策略快照失败: %w", err)
	}
	return nil
}

// ---------- operator 裁决写所需的窄访问面 ----------

// SetStrategyPinned 人工置位 / 解除 pin。
//
// 解除必须留理由与操作人，并进 audit_events 哈希链 —— 「解 pin」是能断掉漏洞报告
// 可复现性的动作，不能没有痕迹。
//
// ⚠️ 本函数【只改 pinned 一列】：pin 的置位与解除条件本应由确定性判据驱动
// （Task 5 的 EvaluatePin），这里是 operator 的人工通道，裁决权在宿主侧。
func (d *DB) SetStrategyPinned(id int64, pinned bool, actor, reason string) error {
	if err := d.ensureStrategySchema(); err != nil {
		return err
	}
	if strings.TrimSpace(actor) == "" {
		return errors.New("人工置位 / 解除 pin 必须写明操作人")
	}
	if strings.TrimSpace(reason) == "" {
		return errors.New("人工置位 / 解除 pin 必须写明理由")
	}
	res, err := d.Exec(`UPDATE strategies SET pinned=$2, updated_at=now() WHERE id=$1`, id, pinned)
	if err != nil {
		return fmt.Errorf("设置策略 pin 失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("策略 %d 不存在", id)
	}
	payload, _ := json.Marshal(map[string]any{"strategy_id": id, "pinned": pinned})
	action := "unpin"
	if pinned {
		action = "pin"
	}
	if _, err := d.AppendAuditEvent(AuditEvent{
		Actor:    actor,
		ToolName: "strategy_pin",
		Action:   action,
		Reason:   reason,
		Payload:  payload,
	}); err != nil {
		return fmt.Errorf("写 pin 审计链失败: %w", err)
	}
	return nil
}

// RetireStrategy 人工退役一条策略。
//
// 退役 ≠ 删除（反模式 5 的正面对策）：state 置 retired、retired_at 落时间、
// retired_reason 留理由，证据链整条留着，墓园可复活。
//
// 只允许从 quarantined / active 退役：已经 retired 的再退役一次没有意义，
// tombstone 是终态（硬 TTL 到了），人工不该把它往前拨。
func (d *DB) RetireStrategy(id int64, actor, reason string) error {
	if err := d.ensureStrategySchema(); err != nil {
		return err
	}
	if strings.TrimSpace(actor) == "" {
		return errors.New("人工退役必须写明操作人")
	}
	if strings.TrimSpace(reason) == "" {
		return errors.New("人工退役必须写明理由")
	}
	res, err := d.Exec(`
UPDATE strategies
   SET state='retired', retired_at=now(), retired_reason=$2, updated_at=now()
 WHERE id=$1 AND state IN ($3,$4)`, id, reason, StrategyQuarantined, StrategyActive)
	if err != nil {
		return fmt.Errorf("人工退役策略失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("策略 %d 不存在或当前状态不可退役", id)
	}
	payload, _ := json.Marshal(map[string]any{"strategy_id": id, "state": StrategyRetired})
	if _, err := d.AppendAuditEvent(AuditEvent{
		Actor:    actor,
		ToolName: "strategy_retire",
		Action:   "retire",
		Reason:   reason,
		Payload:  payload,
	}); err != nil {
		return fmt.Errorf("写退役审计链失败: %w", err)
	}
	return nil
}

// PinnedStrategies 列出被 pin 的策略（唯一证人保护的那批）。
//
// **只读**：这个函数不写任何东西 —— 复审队列永远只是 advisory（自动解 pin 会砸掉
// 漏洞报告的可复现性）。
func (d *DB) PinnedStrategies(limit int) ([]*Strategy, error) {
	if err := d.ensureStrategySchema(); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 200
	}
	rows, err := d.Query(`SELECT `+strategyCols+` FROM strategies WHERE pinned ORDER BY id LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("列 pin 队列失败: %w", err)
	}
	defer rows.Close()
	out := make([]*Strategy, 0, 8)
	for rows.Next() {
		s, err := scanStrategy(rows)
		if err != nil {
			return nil, fmt.Errorf("列 pin 队列失败: %w", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("列 pin 队列失败: %w", err)
	}
	return out, nil
}

// RefreshStrategyUtility 手动刷新 utility 物化视图。
//
// 权威 utility 在物化视图里（静态列只是缓存）；curator 平时会自己刷新，
// 这个入口给 operator 一个「立刻对齐」的手段。
//
// 为什么优先走迁移连接（admin）：REFRESH MATERIALIZED VIEW 需要物化视图的
// **属主**权限，而视图由迁移连接（超级用户）建（db/schema_strategy.sql）；
// 运行期角色（artex_app）刻意没有任何对象的所有权（db/role.go）⇒ 直接在
// 运行期连接上刷必然报「must be owner of materialized view」。admin 池缺失
// （单 DSN 部署）时运行期连接就是属主，直刷即可。
func (d *DB) RefreshStrategyUtility() error {
	if err := d.ensureStrategySchema(); err != nil {
		return err
	}
	pool := d.DB
	if d.admin != nil {
		pool = d.admin
	}
	if _, err := pool.Exec(`REFRESH MATERIALIZED VIEW strategy_utility`); err != nil {
		return fmt.Errorf("刷新策略 utility 物化视图失败: %w", err)
	}
	return nil
}

// ---------- 跨层访问器 ----------

// DB 返回底层连接句柄。
//
// 为什么需要它：agent 侧的策略工具（strategy_search / strategy_get /
// strategy_propose）要读写策略库，而它们手里只有任务绑定的 ExplorationStore。
// 这个访问器是那条接线的唯一入口。
//
// 为什么落在本文件：db/exploration.go 由别的单元并行持有，本单元不改它。
func (s *ExplorationStore) DB() *DB { return s.db }

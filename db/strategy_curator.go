// 本文件是策略库（L4）「裁决写」那一层的访问层。
//
// 裁决写 = 晋级 / 淘汰 / TTL / 唯一证人 pin 的**状态迁移**。分工：
//   - server/strategy_curator.go：**判定**（要不要迁），可被单测直接调用；
//   - 本文件：**迁移原语**，每条都带状态守卫（条件更新），于是并发下第二个
//     执行者影响 0 行，自动幂等。
//
// 为什么阈值常量在这里而不是 db/strategies.go：db/strategies.go 由 Task 1/2 的
// 单元持有（并行开发中），本单元不改它。常量仍然落在 **db 包**里 ⇒ 调用方写
// db.StrategyTauU，与写在 db/strategies.go 里完全等价，单一真相源不变。
//
// 为什么阈值必须是【代码常量】：DB 规则行可被 DELETE、settings 可被持有 JWT 的
// 人热改 —— 与 guard/shortcircuit.go、internal/scopepolicy/policy.go 同一条理由。
// 阈值一旦能被数据改写，「淘汰」就变成「谁能把阈值改成负数谁就能淘汰所有策略」。
package db

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// 淘汰与治理判据的代码常量（单一真相源）。
const (
	// StrategyTauU 是效用阈值 τ_u：utility 低于它即触发淘汰评估。
	//
	// ⚠️ 取值必须在 [0,1]：权威 utility 来自 strategy_utility 物化视图，它的
	// utility_raw = avg(CASE WHEN f.status='confirmed' THEN 1.0 ELSE 0.0 END)
	// 恒在 [0,1]。取负数则判据永远不触发。
	//
	// 默认 **0.05**（裁决 2026-10-30）：
	//   - **Selm 论文的超参表默认就是 0.05** ⇒ 本阶段与 Selm 对齐；
	//   - utility 是 [0,1] 上的 EMA，0.05 的含义是「只有**持续表现很差**的策略
	//     才被清退」，而不是「平均表现不佳」就被清退；
	//   - **保守性由 confidence 腿（StrategyConfidenceRetireThreshold）承担，
	//     不由 utility 阈值承担** —— 早先建议的 0.2 在冷启动期（冻结语料还没
	//     积累起来、utility 本身不稳）会把只是暂时表现不佳的策略提前推进退役评估。
	//     ⇒ 0.2 已作废，**不要**再把它写进任何地方（含注释与测试里的字面量）。
	StrategyTauU = 0.05

	// StrategyConfidenceRetireThreshold 是**退役的 confidence 腿阈值**。
	//
	// Selm 的判据是 `v(s) < 0`；本阶段取**更严的负值 -0.3**，含义是
	// 「**只有被反复 DOWNVOTE 到明显负值才退役**」。
	//
	// ⚠️ **-0.3 这个具体取值是分析员依据「保守性由 confidence 腿承担」这条原则
	// 推导出来的建议值，不是论文里引用的数值** —— 论文只给了 `v(s) < 0`。
	// 记录这一点是为了避免后人把它当成「论文口径」来引用或校对。
	//
	// 这是「淘汰太激进 / 太宽松」的**调节旋钮**：若语料显示**漏退役**
	// （本该退的没退），可调回 0；若显示**误杀**，可调得更负（如 -0.5）。
	//
	// ⚠️ 单一真相源：与 StrategyTauU **同处 db 包常量**，不得进 settings、
	// 不得进 DB 可写表、不得由 agent 申报。
	StrategyConfidenceRetireThreshold = -0.3

	// StrategyMinObservations 是最小观测前置 n：观测次数低于它不进入淘汰评估
	// （效用估计偏差过大）。默认 5（design §10 第 3 条）。
	//
	// ⚠️ 计的是 **usage_count（被实际使用次数）**，**不是** retrieval_count：
	// 检索到 ≠ 用上了。被检索多次但从未被采纳的策略不应过早进入退役评估
	// —— 它连一次真实执行都没发生过，utility 无从谈起。
	// ① Selm 本身没有强制的最小观测退役闸门（它的 Nslow=20 是「慢更新样本量」，
	//    不是退役阈值）；② 这道闸门是我们自己加的，拦的是「一次坏观测就触发评估」；
	// ③ 因此它必须数真正发生过的事；④ usage_count 与 Selm 的口径一致 ——
	//    utility 来自执行结果，不是来自「被检索到」这件事。
	//
	// ⚠️ **不改 retrieval_count 本身**：它仍是合法指标，仍在 schema 里，也仍用于
	// **有界近因衰减**项（StrategyDecay 的 recency 分量）；只有**退役 / 晋级前置门**
	// 改用 usage_count。
	StrategyMinObservations = 5

	// StrategyPinQueueThreshold 是 pin 复审队列的触发阈值。
	//
	// ⚠️ 队列**只提示，不自动解**（design §4.4 末段）：自动解 pin 会砸掉漏洞报告
	// 的可复现性 —— 当通往某个漏洞的唯一已知路径被放行，报告就再也复现不出来。
	// ⚠️ **禁止按时间自动放行**：策略的价值与年龄无关，只与「是否还有别的路径」有关。
	StrategyPinQueueThreshold = 50
)

// StrategyCuratorScanLimit 是 curator 一轮扫库的上限。
//
// 为什么要有界：策略库是**跨任务、跨会话**累积的，无界扫描会让一次仲裁变成一次
// 全表遍历，把 PostgreSQL 拖死。超出上限的策略留到下一轮。
const StrategyCuratorScanLimit = 1000

// StrategyFindingLink 是「策略 → finding」的一条链接及其**当前**验证状态。
type StrategyFindingLink struct {
	FindingID int64
	// Status 是 findings.status 的当前值（空串按 pending 处理）。
	Status string
}

// StrategyAuthoritativeUtility 读物化视图里的**权威** utility。
//
// 第二个返回值为 false 表示该策略在视图里没有行 ⇒ utility **无定义**（不是 0）。
//
// ⚠️ 为什么不给「取不到就当 0」的兜底：无定义与 0 是两件事 —— 当成 0 会让一条
// 刚建、还没有任何证据的策略直接踩中淘汰判据。
func (d *DB) StrategyAuthoritativeUtility(strategyID int64) (float64, bool, error) {
	if err := d.ensureStrategySchema(); err != nil {
		return 0, false, err
	}
	var value float64
	err := d.QueryRow(`SELECT utility_raw FROM strategy_utility WHERE strategy_id=$1`, strategyID).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("读权威 utility 失败: %w", err)
	}
	return value, true, nil
}

// StrategyHasFindingEvidence 报告该策略有没有 **finding 类**证据。
//
// 为什么单独判定：没有 finding 类证据时 utility 是**无定义**而不是 0 —— 把「没证据」
// 当成「效用为零」会把刚建的策略直接推进退役评估。
func (d *DB) StrategyHasFindingEvidence(strategyID int64) (bool, error) {
	if err := d.ensureStrategySchema(); err != nil {
		return false, err
	}
	var exists bool
	err := d.QueryRow(`SELECT EXISTS (SELECT 1 FROM strategy_evidence
		WHERE strategy_id=$1 AND ref_type='finding')`, strategyID).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("读策略证据失败: %w", err)
	}
	return exists, nil
}

// StrategyObservedMilestones 读该策略在**真实运行**里被命中过的里程碑。
//
// 数据来源是 strategy_usage.milestone_hits（归因器写的运行明细），不是
// success_criteria —— 后者是**声明**的里程碑，前者是**发生过**的命中。
func (d *DB) StrategyObservedMilestones(strategyID int64) ([]string, error) {
	if err := d.ensureStrategySchema(); err != nil {
		return nil, err
	}
	rows, err := d.Query(`
SELECT DISTINCT m
  FROM strategy_usage su
  CROSS JOIN LATERAL jsonb_array_elements_text(
      CASE WHEN jsonb_typeof(su.milestone_hits) = 'array'
           THEN su.milestone_hits ELSE '[]'::jsonb END) AS m
 WHERE su.strategy_id = $1
 ORDER BY m`, strategyID)
	if err != nil {
		return nil, fmt.Errorf("读策略命中里程碑失败: %w", err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			return nil, fmt.Errorf("读策略命中里程碑失败: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("读策略命中里程碑失败: %w", err)
	}
	return out, nil
}

// StrategyEvidenceSnapshotURLs 读策略证据里的**流量快照 url 集合**。
//
// 这是 pin 解除条件①的基准：策略所依据的是哪几条路径（url 才是路径的身份，
// 快照 id 只是某次抓取的编号）。
func (d *DB) StrategyEvidenceSnapshotURLs(strategyID int64) ([]string, error) {
	if err := d.ensureStrategySchema(); err != nil {
		return nil, err
	}
	rows, err := d.Query(`
SELECT DISTINCT t.url
  FROM strategy_evidence se
  JOIN traffic_evidence_snapshots t ON t.id = se.ref_id
 WHERE se.strategy_id = $1 AND se.ref_type = 'traffic_snapshot'
 ORDER BY t.url`, strategyID)
	if err != nil {
		return nil, fmt.Errorf("读策略流量证据失败: %w", err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var url string
		if err := rows.Scan(&url); err != nil {
			return nil, fmt.Errorf("读策略流量证据失败: %w", err)
		}
		out = append(out, url)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("读策略流量证据失败: %w", err)
	}
	return out, nil
}

// StrategyFindingSnapshotURLs 读某 finding 绑定的**全部**流量快照 url 集合。
func (d *DB) StrategyFindingSnapshotURLs(findingID int64) ([]string, error) {
	if err := d.ensureStrategySchema(); err != nil {
		return nil, err
	}
	rows, err := d.Query(`
SELECT DISTINCT t.url
  FROM finding_traffic_bindings fb
  JOIN traffic_evidence_snapshots t ON t.id = fb.snapshot_id
 WHERE fb.finding_id = $1
 ORDER BY t.url`, findingID)
	if err != nil {
		return nil, fmt.Errorf("读 finding 流量证据失败: %w", err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var url string
		if err := rows.Scan(&url); err != nil {
			return nil, fmt.Errorf("读 finding 流量证据失败: %w", err)
		}
		out = append(out, url)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("读 finding 流量证据失败: %w", err)
	}
	return out, nil
}

// StrategyLinkedFindings 读该策略 **validated_by** 过的 finding 及其当前状态。
//
// 这是 pin 解除条件②的判据来源：finding 不再 confirmed ⇒ 那条唯一路径已经
// 不再需要被保护。
func (d *DB) StrategyLinkedFindings(strategyID int64) ([]StrategyFindingLink, error) {
	if err := d.ensureStrategySchema(); err != nil {
		return nil, err
	}
	rows, err := d.Query(`
SELECT f.id, COALESCE(f.status, 'pending')
  FROM strategy_evidence se
  JOIN findings f ON f.id::text = se.ref_id
 WHERE se.strategy_id = $1 AND se.ref_type = 'finding' AND se.role = 'validated_by'
 ORDER BY f.id`, strategyID)
	if err != nil {
		return nil, fmt.Errorf("读策略关联的漏洞失败: %w", err)
	}
	defer rows.Close()
	out := []StrategyFindingLink{}
	for rows.Next() {
		var link StrategyFindingLink
		if err := rows.Scan(&link.FindingID, &link.Status); err != nil {
			return nil, fmt.Errorf("读策略关联的漏洞失败: %w", err)
		}
		out = append(out, link)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("读策略关联的漏洞失败: %w", err)
	}
	return out, nil
}

// StrategySoleWitnessFindings 读「以该策略为**唯一** validated_by 路径」的
// confirmed finding。
//
// 唯一性按**不同的策略**算：同一条策略对同一个 finding 只能有一条 validated_by
// 证据（主键保证），所以「唯一路径」= 没有第二条策略也验证了同一个 finding。
//
// 为什么只取 confirmed：pin 保护的是「通往某个**已确认**漏洞的唯一已知路径」，
// 还没确认的 finding 不构成需要保护的复现性。
func (d *DB) StrategySoleWitnessFindings(strategyID int64) ([]StrategyFindingLink, error) {
	if err := d.ensureStrategySchema(); err != nil {
		return nil, err
	}
	rows, err := d.Query(`
SELECT f.id, COALESCE(f.status, 'pending')
  FROM strategy_evidence se
  JOIN findings f ON f.id::text = se.ref_id
 WHERE se.strategy_id = $1 AND se.ref_type = 'finding' AND se.role = 'validated_by'
   AND COALESCE(f.status, 'pending') = 'confirmed'
   AND NOT EXISTS (
        SELECT 1 FROM strategy_evidence se2
         WHERE se2.ref_type = 'finding' AND se2.ref_id = se.ref_id
           AND se2.role = 'validated_by' AND se2.strategy_id <> $1)
 ORDER BY f.id`, strategyID)
	if err != nil {
		return nil, fmt.Errorf("读唯一证人路径失败: %w", err)
	}
	defer rows.Close()
	out := []StrategyFindingLink{}
	for rows.Next() {
		var link StrategyFindingLink
		if err := rows.Scan(&link.FindingID, &link.Status); err != nil {
			return nil, fmt.Errorf("读唯一证人路径失败: %w", err)
		}
		out = append(out, link)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("读唯一证人路径失败: %w", err)
	}
	return out, nil
}

// PromoteStrategyToActive 把一条**隔离态**策略晋级为 active。
//
// 条件更新（WHERE state='quarantined'）⇒ 并发下第二个执行者影响 0 行，
// 自动幂等；也保证「隔离态之外的策略不会被这条语句顺手改掉」。
//
// ⚠️ 这是**迁移原语**，不是判定：要不要晋级由 server.EvaluatePromotion 的三个
// 条件决定，调用方（宿主侧的 curator）必须先过那道判定。
func (d *DB) PromoteStrategyToActive(strategyID int64) (bool, error) {
	if err := d.ensureStrategySchema(); err != nil {
		return false, err
	}
	res, err := d.Exec(`UPDATE strategies SET state=$2 WHERE id=$1 AND state=$3`,
		strategyID, StrategyActive, StrategyQuarantined)
	if err != nil {
		return false, fmt.Errorf("晋级策略 %d 失败: %w", strategyID, err)
	}
	return rowsAffected(res)
}

// AutoRetireStrategy 把一条 active 策略**自动**退役。
//
// 【退役 ≠ 删除】：行留着，retired_reason 留着，证据链留着 —— 墓碑与墓园是
// 「可复活」的前提。条件更新额外守住「pinned 的策略不参与任何自动淘汰」。
//
// ⚠️ 这是**迁移原语**，不是判定：要不要退役由 server.EvaluateRetirement 的
// 两条腿 + 共同前置决定。
func (d *DB) AutoRetireStrategy(strategyID int64, reason string) (bool, error) {
	if err := d.ensureStrategySchema(); err != nil {
		return false, err
	}
	res, err := d.Exec(`
UPDATE strategies
   SET state=$2, retired_at=now(), retired_reason=$3
 WHERE id=$1 AND state=$4 AND pinned=false`,
		strategyID, StrategyRetired, reason, StrategyActive)
	if err != nil {
		return false, fmt.Errorf("退役策略 %d 失败: %w", strategyID, err)
	}
	return rowsAffected(res)
}

// TombstoneStrategyByTTL 把一条到期策略墓碑化（**软删除**：行与证据都留着）。
//
// 硬 TTL 是独立的第二层：到期就墓碑，与 utility / confidence 无关 ——
// 「过期」是时间事实，「没价值」是价值判断，两件事不许混。
func (d *DB) TombstoneStrategyByTTL(strategyID int64) (bool, error) {
	if err := d.ensureStrategySchema(); err != nil {
		return false, err
	}
	res, err := d.Exec(`UPDATE strategies SET state=$2 WHERE id=$1 AND state<>$2`,
		strategyID, StrategyTombstone)
	if err != nil {
		return false, fmt.Errorf("墓碑化策略 %d 失败: %w", strategyID, err)
	}
	return rowsAffected(res)
}

// ApplyStrategyPin 置位 / 解除 pin（**自动**通道：条件更新，无操作人、无审计）。
//
// ⚠️ 与 operator 侧的人工通道（db.SetStrategyPinned，带操作人与理由、进审计链）
// 是两回事：这条是**裁决写**的一部分，判定条件由确定性判据给出，操作人是 curator。
//
// 条件更新（pinned<>目标值）⇒ 重复调用影响 0 行，自动幂等。
func (d *DB) ApplyStrategyPin(strategyID int64, pinned bool) (bool, error) {
	if err := d.ensureStrategySchema(); err != nil {
		return false, err
	}
	res, err := d.Exec(`UPDATE strategies SET pinned=$2 WHERE id=$1 AND pinned<>$2`,
		strategyID, pinned)
	if err != nil {
		return false, fmt.Errorf("改策略 %d 的 pin 失败: %w", strategyID, err)
	}
	return rowsAffected(res)
}

// PinnedStrategyIDs 列被 pin 的策略 id（升序）。
func (d *DB) PinnedStrategyIDs(limit int) ([]int64, error) {
	if err := d.ensureStrategySchema(); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = StrategyCuratorScanLimit
	}
	rows, err := d.Query(`SELECT id FROM strategies WHERE pinned=true ORDER BY id LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("列被 pin 的策略失败: %w", err)
	}
	defer rows.Close()
	out := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("列被 pin 的策略失败: %w", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("列被 pin 的策略失败: %w", err)
	}
	return out, nil
}

// CountPinnedStrategies 数被 pin 的策略**总数**（不看分页上限）。
//
// 为什么单独数：复审队列的触发条件是「总数超过阈值」，而队列内容是有界的。
func (d *DB) CountPinnedStrategies() (int64, error) {
	if err := d.ensureStrategySchema(); err != nil {
		return 0, err
	}
	var n int64
	if err := d.QueryRow(`SELECT count(*) FROM strategies WHERE pinned=true`).Scan(&n); err != nil {
		return 0, fmt.Errorf("数被 pin 的策略失败: %w", err)
	}
	return n, nil
}

// ExpiredStrategies 列出 TTL 到期、尚未墓碑化的策略。
func (d *DB) ExpiredStrategies(now time.Time, limit int) ([]*Strategy, error) {
	if err := d.ensureStrategySchema(); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = StrategyCuratorScanLimit
	}
	rows, err := d.Query(`
SELECT `+strategyCols+`
  FROM strategies
 WHERE expired_at IS NOT NULL AND expired_at <= $1 AND state <> $2
 ORDER BY id
 LIMIT $3`, now, StrategyTombstone, limit)
	if err != nil {
		return nil, fmt.Errorf("列到期策略失败: %w", err)
	}
	defer rows.Close()
	out := []*Strategy{}
	for rows.Next() {
		s, err := scanStrategy(rows)
		if err != nil {
			return nil, fmt.Errorf("列到期策略失败: %w", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("列到期策略失败: %w", err)
	}
	return out, nil
}

// rowsAffected 报告一次条件更新是否真的改了行。
//
// 「改了行」= 状态确实迁移了；没改行 = 有人已经迁过（并发）或守卫拦住了。
// 裁决报告要区分这两者，所以这里返回行数而不是只返回错误。
func rowsAffected(res sql.Result) (bool, error) {
	if res == nil {
		return false, nil
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("读更新行数失败: %w", err)
	}
	return n > 0, nil
}

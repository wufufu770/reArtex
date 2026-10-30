// 本文件是策略库（L4）的**裁决写**：晋级 / 淘汰 / TTL / 唯一证人 pin。
//
// 【裁决写只在宿主侧发生，LLM 一行都不参与】
//   - 晋级 / 淘汰 / 解 pin 的判据全部是**关系查询 + 状态比 + 数值阈值**；
//   - 三个判定函数的入参只有 (*db.DB, db.Strategy) —— 拿不到模型、provider、
//     planner 的任何句柄 ⇒ 结构上就不可能问模型「这条该不该晋级」；
//   - 本文件**不 import** agent / llm / llmrec / llmpool 任何一个包
//     （TestCuratorNeverConsultsAModel 用 go/parser 把这条钉死）。
//
// 【为什么不能有粗糙 judge】（反模式 4）：让模型判「该不该晋级 / 该不该退役」，
// 同一批证据在不同轮次会得到不同结论 ⇒ 裁决不可复现 ⇒ 事后无法复盘、无法追责。
//
// 【退役 ≠ 删除】：淘汰写的是 state='retired' + retired_reason，证据链一行不少，
// 墓园与复活的前提就在这里。
//
// allow: SIZE_OK —— 本单元的文件域被任务书钉死为 server 下的两个文件
// （裁决 + 测试）；晋级 / 淘汰 / pin / 水位四件事共用同一条「先判定、后条件更新」
// 的链路，拆开会把「判定与写入必须同源」这道结构闸门切散。
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/Autumn-27/artex/db"
)

// CuratorReport 是一次仲裁的结果摘要（用于日志与 UI）。
type CuratorReport struct {
	Promoted       []int64
	Retired        []int64
	Tombstoned     []int64
	Pinned         []int64
	Unpinned       []int64
	PinQueue       []int64
	UtilityRefresh bool
}

// Curator 跑策略库的仲裁。
//
// **裁决写只在宿主侧发生**（auto-permission-tiers §4.2）：agent 侧没有任何
// 工具能把策略标成 active / retired / tombstone，也没有工具能解 pin。
type Curator struct {
	pg    *db.DB
	clock func() time.Time
}

// NewCurator 建一个 curator。
func NewCurator(pg *db.DB) *Curator {
	return &Curator{pg: pg, clock: time.Now}
}

// now 读当前时刻（可注入，便于测试）。
func (c *Curator) now() time.Time {
	if c == nil || c.clock == nil {
		return time.Now()
	}
	return c.clock()
}

// PromotionDecision 是一次晋级的判定明细。三个条件**全过**才晋级。
type PromotionDecision struct {
	StrategyID       int64
	ReplayPassed     bool
	ReplayComparable bool
	ReplayBefore     float64
	ReplayAfter      float64
	// UsageCount 是被实际使用的次数（晋级前置门 n 数的就是它，
	// 不是 retrieval_count —— 检索到 ≠ 用上了）。
	UsageCount   int64
	MilestoneHit bool
	Eligible     bool
	Reason       string
}

// EvaluatePromotion 判定一条 quarantined 策略能否晋级。
//
// 三个条件（全部确定性，design §3.4）：
//  1. 冻结重放闸门通过：有可比语料，且候选在语料上有命中；
//  2. usage_count >= n（最小观测前置，**按被实际使用次数计**）；
//  3. 客观里程碑命中（success_criteria 里的里程碑在真实运行中被命中过）。
//
// 任何一条不满足 ⇒ 不晋级。**没有可比语料 ⇒ 不晋级**（保守默认）。
func EvaluatePromotion(pg *db.DB, s db.Strategy) (PromotionDecision, error) {
	d := PromotionDecision{StrategyID: s.ID, UsageCount: s.UsageCount}

	// ① 冻结重放闸门。
	//
	// ⚠️ **晋级不改内容**（晋级只是状态迁移），所以「拟生效的候选」就是现状本身
	//    ⇒ before == after，严格的 after > before 永远不成立。
	//    ⇒ 闸门退化成计划写明的退化口径：「**有可比语料 且 该候选在语料上有命中**」
	//       （after > 0）。
	//    这道闸门**不许被默默跳过**：跳过闸门 = 反模式 4 的变种（裁决不经过证据）。
	//    若将来晋级允许携带内容变更，再把「候选」接进来 —— 那时 before 与 after
	//    才是两个不同的值。
	declared := declaredMilestones(s.SuccessCriteria)
	candidate := db.ReplayCandidate{Activation: s.Activation, Milestones: declared}
	score, comparable, err := pg.ReplayScore(candidate)
	if err != nil {
		return d, fmt.Errorf("重放打分失败: %w", err)
	}
	d.ReplayBefore = score
	d.ReplayAfter = score
	d.ReplayComparable = comparable
	d.ReplayPassed = comparable && score > 0

	// ② 最小观测前置：usage_count（被实际使用次数）>= n。
	//    ⚠️ 计的是 usage_count 而**不是** retrieval_count：检索到 ≠ 用上了。
	//    被检索多次却从未被采纳的策略连一次真实执行都没发生过。
	//
	// ③ 客观里程碑命中：success_criteria 里声明的里程碑在真实运行中被命中过。
	observed, err := pg.StrategyObservedMilestones(s.ID)
	if err != nil {
		return d, fmt.Errorf("读策略命中里程碑失败: %w", err)
	}
	d.MilestoneHit = curatorStringsIntersect(declared, observed)

	d.Eligible = d.ReplayPassed && d.UsageCount >= db.StrategyMinObservations && d.MilestoneHit
	d.Reason = promotionReason(d)
	return d, nil
}

// RetirementDecision 是一次淘汰判定。
type RetirementDecision struct {
	StrategyID int64
	Utility    float64
	Confidence float64
	TauU       float64
	// ConfidenceThreshold 是本次判定实际用的 confidence 腿阈值
	// （db.StrategyConfidenceRetireThreshold），记进判定明细以便复盘。
	ConfidenceThreshold float64
	HasEvidence         bool
	UsageCount          int64
	Pinned              bool
	Eligible            bool
	Retire              bool
	Reason              string
}

// EvaluateRetirement 判定一条 active 策略是否该退役。
//
// 判据（任一命中即退役）：utility(s) < τ_u 或 confidence(s) < confidence 腿阈值。
// 共同前置（缺一不可）：usage_count >= n（**按被实际使用次数计**）、有 finding 类
// 证据（否则 utility 无定义而不是 0）、权威 utility 有定义、有可比冻结语料、
// 且**未被 pin**。
func EvaluateRetirement(pg *db.DB, s db.Strategy) (RetirementDecision, error) {
	d := RetirementDecision{
		StrategyID:          s.ID,
		Confidence:          s.Confidence,
		TauU:                db.StrategyTauU,
		ConfidenceThreshold: db.StrategyConfidenceRetireThreshold,
		UsageCount:          s.UsageCount,
		Pinned:              s.Pinned,
	}

	// 共同前置 ①：有 finding 类证据（否则 utility 无定义，而不是 0）。
	hasEvidence, err := pg.StrategyHasFindingEvidence(s.ID)
	if err != nil {
		return d, fmt.Errorf("读策略证据失败: %w", err)
	}
	d.HasEvidence = hasEvidence

	// 共同前置 ②：有可比冻结语料（没有语料就没有可比判据 ⇒ 不评估）。
	declared := declaredMilestones(s.SuccessCriteria)
	candidate := db.ReplayCandidate{Activation: s.Activation, Milestones: declared}
	_, comparable, err := pg.ReplayScore(candidate)
	if err != nil {
		return d, fmt.Errorf("重放打分失败: %w", err)
	}

	// utility 读**物化视图**的权威值（静态列只是缓存）——
	// 这正是「证据链变化自动反映到 utility」的收益所在（design §3.5）。
	utility, defined, err := pg.StrategyAuthoritativeUtility(s.ID)
	if err != nil {
		return d, fmt.Errorf("读权威 utility 失败: %w", err)
	}
	d.Utility = utility

	// 共同前置 ③：最小观测前置（按 usage_count 计）。
	// 共同前置 ④：未被 pin（唯一证人不参与任何自动淘汰）。
	d.Eligible = d.UsageCount >= db.StrategyMinObservations &&
		hasEvidence && defined && comparable && !s.Pinned
	if !d.Eligible {
		d.Reason = retirementBlockedReason(d, comparable, defined)
		return d, nil
	}

	utilityLeg := utility < db.StrategyTauU
	confidenceLeg := s.Confidence < db.StrategyConfidenceRetireThreshold
	d.Retire = utilityLeg || confidenceLeg
	switch {
	case utilityLeg && confidenceLeg:
		d.Reason = fmt.Sprintf("utility=%.4f < τ_u=%.2f 且 confidence=%.4f < %.2f",
			utility, db.StrategyTauU, s.Confidence, db.StrategyConfidenceRetireThreshold)
	case utilityLeg:
		d.Reason = fmt.Sprintf("utility=%.4f < τ_u=%.2f", utility, db.StrategyTauU)
	case confidenceLeg:
		d.Reason = fmt.Sprintf("confidence=%.4f < %.2f（被反复 down-vote）",
			s.Confidence, db.StrategyConfidenceRetireThreshold)
	default:
		d.Reason = fmt.Sprintf("两条腿都不成立：utility=%.4f ≥ %.2f 且 confidence=%.4f ≥ %.2f",
			utility, db.StrategyTauU, s.Confidence, db.StrategyConfidenceRetireThreshold)
	}
	return d, nil
}

// PinDecision 是一次 pin 置位 / 解除的判定。
type PinDecision struct {
	StrategyID  int64
	ShouldPin   bool
	ShouldUnpin bool
	Reason      string
}

// EvaluatePin 判定唯一证人 pin 的置位与解除。**全部确定性**。
//
// 置位：某 strategy 是某个 confirmed finding 的**唯一** validated_by 路径
//
// 解除（自动的只有两条，写死为可机械判定的条件，design §4.4）：
//
//	① 同一 finding 下出现**新的**流量证据，且其 url 与被 pin 策略所依据的**不同**；
//	② 该 finding 不再是 confirmed（status 不是 confirmed）。
//
// 人工解除（第三条）走 operator 路径：必须留理由与操作人，进 audit_events 哈希链。
//
// ❌ 明确禁止：用模型判定「是否已有替代路径」—— 那是粗糙 judge（反模式 4）。
// ❌ 明确禁止：按时间自动解 pin —— 策略的价值与年龄无关。
func EvaluatePin(pg *db.DB, s db.Strategy) (PinDecision, error) {
	d := PinDecision{StrategyID: s.ID}

	sole, err := pg.StrategySoleWitnessFindings(s.ID)
	if err != nil {
		return d, fmt.Errorf("读唯一证人路径失败: %w", err)
	}
	d.ShouldPin = len(sole) > 0

	if !s.Pinned {
		if d.ShouldPin {
			d.Reason = "是某个 confirmed 漏洞的唯一 validated_by 路径：应当置位"
		} else {
			d.Reason = "不是任何 confirmed 漏洞的唯一 validated_by 路径：不置位"
		}
		return d, nil
	}

	// 已置位 ⇒ 只可能由两条**自动**条件解除。
	linked, err := pg.StrategyLinkedFindings(s.ID)
	if err != nil {
		return d, fmt.Errorf("读策略关联的漏洞失败: %w", err)
	}
	strategyURLs, err := pg.StrategyEvidenceSnapshotURLs(s.ID)
	if err != nil {
		return d, fmt.Errorf("读策略流量证据失败: %w", err)
	}
	for _, link := range linked {
		// 条件②：finding 不再 confirmed（resolved / fixed / false_positive / ignored …）。
		if link.Status != db.FindingConfirmed {
			d.ShouldUnpin = true
			d.Reason = fmt.Sprintf("漏洞 %d 的状态已不是 confirmed（现在是 %s）", link.FindingID, link.Status)
			return d, nil
		}
		// 条件①：同一 finding 下出现指向**不同 url** 的新流量证据。
		//
		// ⚠️ 前置：策略自己必须**有**流量快照证据 —— 否则它压根不是「一条路径」，
		//    无从谈「出现了替代路径」。只有 finding 类证据引用时条件①不成立。
		if len(strategyURLs) == 0 {
			continue
		}
		findingURLs, err := pg.StrategyFindingSnapshotURLs(link.FindingID)
		if err != nil {
			return d, fmt.Errorf("读漏洞流量证据失败: %w", err)
		}
		if fresh := curatorFirstNewURL(strategyURLs, findingURLs); fresh != "" {
			d.ShouldUnpin = true
			d.Reason = fmt.Sprintf("漏洞 %d 下出现了指向不同 url 的新流量证据（%s）", link.FindingID, fresh)
			return d, nil
		}
	}
	d.Reason = "已置位：两条自动解除条件都不成立，pin 保持"
	return d, nil
}

// RunOnce 跑一轮仲裁。**幂等**：重复调用不产生额外副作用。
//
// 顺序（每一步都可独立失败而不污染下一步）：
//  1. 刷新 utility 物化视图（权威值）；
//  2. 晋级：quarantined → active（三条件全过）；
//  3. 淘汰：active → retired（判据命中 + 前置满足 + 未 pin）；
//  4. TTL：expired_at 到期 → tombstone；
//  5. pin：置位 / 解除（确定性）；
//  6. pin 复审队列：超过阈值时生成提示条目（**不自动解**）。
func (c *Curator) RunOnce(ctx context.Context) (CuratorReport, error) {
	rep := CuratorReport{}
	if err := ctx.Err(); err != nil {
		return rep, fmt.Errorf("curator 运行被取消: %w", err)
	}

	// 1) 刷新 utility 物化视图（权威值）。刷新失败**不阻断**后续 —— 缓存值仍可用。
	if err := c.pg.RefreshStrategyUtility(); err != nil {
		log.Printf("[curator] 刷新 utility 物化视图失败（本轮继续用缓存值）: %v", err)
	} else {
		rep.UtilityRefresh = true
	}

	// 2) 晋级：quarantined → active（三个条件全过才动）。
	quarantined, err := c.pg.ListStrategies(db.StrategyQuarantined, db.StrategyCuratorScanLimit)
	if err != nil {
		return rep, fmt.Errorf("列隔离态策略失败: %w", err)
	}
	for _, s := range quarantined {
		dec, err := EvaluatePromotion(c.pg, *s)
		if err != nil {
			return rep, err
		}
		if !dec.Eligible {
			continue
		}
		ok, err := c.pg.PromoteStrategyToActive(s.ID)
		if err != nil {
			return rep, fmt.Errorf("晋级策略 %d 失败: %w", s.ID, err)
		}
		if ok {
			rep.Promoted = append(rep.Promoted, s.ID)
		}
	}

	// 3) 淘汰：active → retired（退役 ≠ 删除：留 retired_reason 与证据链）。
	active, err := c.pg.ListStrategies(db.StrategyActive, db.StrategyCuratorScanLimit)
	if err != nil {
		return rep, fmt.Errorf("列在役策略失败: %w", err)
	}
	for _, s := range active {
		dec, err := EvaluateRetirement(c.pg, *s)
		if err != nil {
			return rep, err
		}
		if !dec.Retire {
			continue
		}
		ok, err := c.pg.AutoRetireStrategy(s.ID, dec.Reason)
		if err != nil {
			return rep, fmt.Errorf("退役策略 %d 失败: %w", s.ID, err)
		}
		if ok {
			rep.Retired = append(rep.Retired, s.ID)
		}
	}

	// 4) TTL：expired_at 到期 → tombstone（软删除，行与证据都留着）。
	expired, err := c.pg.ExpiredStrategies(c.now(), db.StrategyCuratorScanLimit)
	if err != nil {
		return rep, fmt.Errorf("列到期策略失败: %w", err)
	}
	for _, s := range expired {
		ok, err := c.pg.TombstoneStrategyByTTL(s.ID)
		if err != nil {
			return rep, fmt.Errorf("墓碑化策略 %d 失败: %w", s.ID, err)
		}
		if ok {
			rep.Tombstoned = append(rep.Tombstoned, s.ID)
		}
	}

	// 5) pin：置位 / 解除（确定性，不含任何模型判断）。
	live, err := c.liveStrategies()
	if err != nil {
		return rep, err
	}
	for _, s := range live {
		dec, err := EvaluatePin(c.pg, *s)
		if err != nil {
			return rep, err
		}
		switch {
		case dec.ShouldPin && !s.Pinned:
			ok, err := c.pg.ApplyStrategyPin(s.ID, true)
			if err != nil {
				return rep, fmt.Errorf("置 pin %d 失败: %w", s.ID, err)
			}
			if ok {
				rep.Pinned = append(rep.Pinned, s.ID)
			}
		case dec.ShouldUnpin && s.Pinned:
			ok, err := c.pg.ApplyStrategyPin(s.ID, false)
			if err != nil {
				return rep, fmt.Errorf("解 pin %d 失败: %w", s.ID, err)
			}
			if ok {
				rep.Unpinned = append(rep.Unpinned, s.ID)
			}
		}
	}

	// 6) pin 复审队列：**只提示，不自动解**。
	pinnedCount, err := c.pg.CountPinnedStrategies()
	if err != nil {
		return rep, fmt.Errorf("数被 pin 的策略失败: %w", err)
	}
	if pinnedCount > db.StrategyPinQueueThreshold {
		queue, err := c.pg.PinnedStrategyIDs(db.StrategyCuratorScanLimit)
		if err != nil {
			return rep, fmt.Errorf("列复审队列失败: %w", err)
		}
		rep.PinQueue = queue
	}
	return rep, nil
}

// liveStrategies 列出「活着的」策略（隔离态 + 在役态）—— pin 判定的对象。
//
// 为什么不含 retired / tombstone：它们不再参与检索注入，也不该再被置位。
func (c *Curator) liveStrategies() ([]*db.Strategy, error) {
	quarantined, err := c.pg.ListStrategies(db.StrategyQuarantined, db.StrategyCuratorScanLimit)
	if err != nil {
		return nil, fmt.Errorf("列隔离态策略失败: %w", err)
	}
	active, err := c.pg.ListStrategies(db.StrategyActive, db.StrategyCuratorScanLimit)
	if err != nil {
		return nil, fmt.Errorf("列在役策略失败: %w", err)
	}
	return append(quarantined, active...), nil
}

// appendPinAudit 写一条「人工解除 pin」的审计事件。
//
// 进 audit_events 哈希链，带操作人与理由 —— 「解 pin」是能断掉漏洞报告可复现性
// 的动作，不能没有痕迹。
func appendPinAudit(pg *db.DB, actor, reason string, strategyID int64) error {
	payload, err := json.Marshal(map[string]any{"strategy_id": strategyID})
	if err != nil {
		return fmt.Errorf("序列化解 pin 审计载荷失败: %w", err)
	}
	if _, err := pg.AppendAuditEvent(db.AuditEvent{
		Actor:    actor,
		ToolName: "strategy_unpin",
		Action:   "strategy_pin_released",
		Reason:   reason,
		Payload:  payload,
	}); err != nil {
		return fmt.Errorf("写解 pin 审计事件失败: %w", err)
	}
	return nil
}

// ReleasePin 人工解除一条策略的 pin。
//
// 这是自动两条之外的**第三条**解除路径（operator 入口，由 operator API 接线）：
// 必须写明操作人与理由，且进审计哈希链。缺操作人或缺理由一律拒绝 —— 没有留痕的
// 解 pin 等于没发生过。
//
// ⚠️ 审计事件**总是**写：即便策略本来就没被 pin（这次操作没有改变状态），
// 操作者的动作也要留痕。
//
// ⚠️ 为什么是 *Curator 的方法而不是自由函数：手动解 pin 属于「裁决写」这一族，
// 挂在 curator 上就不会与 operator API 那一族的名字撞车。
func (c *Curator) ReleasePin(actor, reason string, strategyID int64) error {
	if strings.TrimSpace(actor) == "" {
		return errors.New("人工解除 pin 必须写明操作人")
	}
	if strings.TrimSpace(reason) == "" {
		return errors.New("人工解除 pin 必须写明理由")
	}
	if strategyID <= 0 {
		return errors.New("策略 id 不合法")
	}
	ok, err := c.pg.ApplyStrategyPin(strategyID, false)
	if err != nil {
		return err
	}
	if err := appendPinAudit(c.pg, actor, reason, strategyID); err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("策略 %d 本来就没有被 pin（本次操作已记入审计链）", strategyID)
	}
	return nil
}

// ---------- 运行频率：小时级水位 ----------

// SchedKeyStrategyCurator 是 curator 的上次运行水位（unix 秒）。
//
// 与既有 5 个 watermark 同一套机制（scheduler_state 表）—— 不另起调度器。
const SchedKeyStrategyCurator = "strategy_curator_last_run"

// CuratorInterval 是 curator 的运行间隔。
//
// 为什么是小时级：scheduler 的 tick 是 5 秒，而晋级 / 淘汰是**重裁决**（要刷
// 物化视图 + 扫一遍全库）。5 秒一次会把 PostgreSQL 拖死（60:1 的频率差）。
const CuratorInterval = time.Hour

// ShouldRunCurator 判断现在是否该跑一轮 curator。
//
// 水位读不出来 / 读坏了 ⇒ 当成「该跑」：宁可多跑一次，也不要永远不跑。
func ShouldRunCurator(pg *db.DB, now time.Time) (bool, error) {
	raw, err := pg.GetSchedState(SchedKeyStrategyCurator)
	if err != nil {
		return false, fmt.Errorf("读 curator 水位失败: %w", err)
	}
	if raw == "" {
		return true, nil
	}
	last, perr := strconv.ParseInt(raw, 10, 64)
	if perr != nil {
		return true, nil
	}
	return now.Sub(time.Unix(last, 0)) >= CuratorInterval, nil
}

// RunCuratorIfDue 按水位跑一轮 curator。
//
// ⚠️ **水位在跑完之后才写**：写在跑之前，崩溃重启会立刻再跑一轮全库淘汰 ——
// 每轮都要刷物化视图 + 扫全库，PG 会被拖死。跑失败**不写**水位 ⇒ 下一轮重试。
func RunCuratorIfDue(ctx context.Context, pg *db.DB, now time.Time) (CuratorReport, bool, error) {
	due, err := ShouldRunCurator(pg, now)
	if err != nil {
		return CuratorReport{}, false, err
	}
	if !due {
		return CuratorReport{}, false, nil
	}
	rep, runErr := NewCurator(pg).RunOnce(ctx)
	if runErr != nil {
		return rep, true, runErr
	}
	if err := pg.SetSchedState(SchedKeyStrategyCurator, strconv.FormatInt(now.Unix(), 10)); err != nil {
		return rep, true, fmt.Errorf("写 curator 水位失败: %w", err)
	}
	return rep, true, nil
}

// shouldRunCurator 判断 scheduler 这一 tick 是否该跑 curator。
func (sc *Scheduler) shouldRunCurator() bool {
	if sc == nil || sc.pg == nil {
		return false
	}
	due, err := ShouldRunCurator(sc.pg, time.Now())
	if err != nil {
		log.Printf("[scheduler] 读 curator 水位失败: %v", err)
		return false
	}
	return due
}

// runStrategyCurator 跑一轮 curator 并把结果打进日志。
//
// ⚠️ curator 不在 scheduler 包里建 —— 装配点由 server 侧负责，这里只调它。
func (sc *Scheduler) runStrategyCurator() {
	if sc == nil || sc.pg == nil {
		return
	}
	ctx := context.Background()
	if sc.s != nil && sc.s.ctx != nil {
		ctx = sc.s.ctx
	}
	rep, ran, err := RunCuratorIfDue(ctx, sc.pg, time.Now())
	if err != nil {
		log.Printf("[scheduler] curator 运行失败: %v", err)
		return
	}
	if !ran {
		return
	}
	log.Printf("[scheduler] curator 完成：晋级 %d、退役 %d、墓碑 %d、置位 %d、解 pin %d、复审队列 %d（utility 视图已刷新=%v）",
		len(rep.Promoted), len(rep.Retired), len(rep.Tombstoned), len(rep.Pinned),
		len(rep.Unpinned), len(rep.PinQueue), rep.UtilityRefresh)
}

// ---------- 判定辅助 ----------

// declaredMilestones 取策略 success_criteria 里**声明**的里程碑 id。
//
// 为什么复用 parseMilestoneCriteria：里程碑判据的形状（id / where / match / kind）
// 只能有一个定义处 —— 归因器用它判命中，这里用它取「声明了哪些里程碑」；
// 两处各写一份解析器，形状迟早漂移。
func declaredMilestones(criteria json.RawMessage) []string {
	items := parseMilestoneCriteria(criteria)
	out := make([]string, 0, len(items))
	for _, it := range items {
		if it.ID == "" {
			continue
		}
		out = append(out, it.ID)
	}
	return out
}

// curatorStringsIntersect 判定两个字符串集合是否有交集。
func curatorStringsIntersect(a, b []string) bool {
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

// curatorFirstNewURL 找出 finding 下「不在策略证据集合里」的第一个 url。
//
// 返回空串表示没有新路径 —— 也就是 pin 解除条件①不成立。
func curatorFirstNewURL(strategyURLs, findingURLs []string) string {
	known := make(map[string]bool, len(strategyURLs))
	for _, u := range strategyURLs {
		known[u] = true
	}
	for _, u := range findingURLs {
		if !known[u] {
			return u
		}
	}
	return ""
}

// promotionReason 给出晋级判定的理由（中文，进日志可读）。
func promotionReason(d PromotionDecision) string {
	switch {
	case !d.ReplayComparable:
		return "没有可比冻结语料：保守默认，不晋级"
	case !d.ReplayPassed:
		return "冻结重放闸门未通过：候选在可比语料上没有命中"
	case d.UsageCount < db.StrategyMinObservations:
		return fmt.Sprintf("观测次数不足（usage_count=%d < %d）：检索到不等于用上了",
			d.UsageCount, db.StrategyMinObservations)
	case !d.MilestoneHit:
		return "真实运行里没有命中过该策略声明的里程碑"
	default:
		return "三个条件全过：晋级"
	}
}

// retirementBlockedReason 给出「不进入淘汰评估」的理由。
func retirementBlockedReason(d RetirementDecision, comparable, defined bool) string {
	switch {
	case d.Pinned:
		return "已被 pin（唯一证人）：不参与任何自动淘汰"
	case d.UsageCount < db.StrategyMinObservations:
		return fmt.Sprintf("观测次数不足（usage_count=%d < %d）", d.UsageCount, db.StrategyMinObservations)
	case !d.HasEvidence:
		return "没有 finding 类证据：utility 无定义而不是 0"
	case !defined:
		return "权威 utility 无定义（物化视图里没有该策略的行）"
	case !comparable:
		return "没有可比冻结语料：没有判据就不评估"
	default:
		return "前置不满足，不进入淘汰评估"
	}
}

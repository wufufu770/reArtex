package taskintelligence

// collaboration.go —— v2.1 统一架构优化增补 §0.4 偏锋探索与业务建模双向协作
// 规则的确定性控制面（0.15.62，批次 TI-04）。
//
// 三条铁律（0.4「双向协作的控制原则」原文口径）：
//  1. 能力之间不直接创建执行任务：本文件的全部输出是「建议」，实际执行
//     仍由主 Agent 经统一 scope 与闸门（scope/SoD/审批/审计）完成；
//  2. 每次协作必须可记录：触发原因、输入对象版本、输出对象、耗时、Token
//     与结果状态由 CollabRecord 承载（落库归 TI-05 的 CapabilityExecution）；
//  3. 防循环触发：同一未变化对象（causation）对同一目标能力的自动反馈有
//     硬上限；对象变化（新 causation id）才重置预算。
//
// 本文件是纯确定性规则 + 进程内台账，不做任何 IO 与工具执行。

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"
)

// ---------- 偏锋 → 业务建模：业务模型新鲜度检查 ----------

// StaleBizModelTTL 业务模型新鲜度缺省窗口。超过该窗口未更新即视为过期，
// 偏锋假设若依赖业务语义应建议补建模（建议，非强制）。
const StaleBizModelTTL = 24 * time.Hour

// EdgeNeedsBusinessModel 判断「偏锋探索是否应请求业务建模补充上下文」。
// 只读快照判断，输出是建议（suggest modeling.business），不创建任何执行：
//   - 业务模型分节不可用（provider 失败/未接线）→ 建议（宁补勿猜）；
//   - 尚无建模快照（Version==0）→ 建议；
//   - 模型过期（超过 StaleBizModelTTL 未更新）→ 建议；
//   - 未收敛差异过多（> maxOpenDiffs）→ 建议先收敛再探索；
//   - 其余情况 → 不建议。
func EdgeNeedsBusinessModel(snap *TaskContextSnapshot) (bool, string) {
	const maxOpenDiffs = 20
	if snap == nil {
		return true, "快照不可用，宁补勿猜"
	}
	biz := snap.BizModel
	if biz.Err != nil {
		return true, "业务模型分节不可用（" + biz.Err.Error() + "），建议补建模后再出假设"
	}
	if biz.Version == 0 {
		return true, "尚无业务建模快照，依赖业务语义的假设应先补 Design/Behavior"
	}
	if !biz.UpdatedAt.IsZero() && time.Since(biz.UpdatedAt) > StaleBizModelTTL {
		return true, fmt.Sprintf("业务模型已过期（%s 未更新，窗口 %s），建议增量补建模",
			biz.UpdatedAt.Format("2006-01-02 15:04"), StaleBizModelTTL)
	}
	if biz.LayerDiffs > maxOpenDiffs {
		return true, fmt.Sprintf("未收敛差异 %d 条（上限 %d），建议先收敛现有差异", biz.LayerDiffs, maxOpenDiffs)
	}
	return false, ""
}

// ---------- 业务建模 → 偏锋：差异转待验证假设 ----------

// ModelDeltaRef 指向 biz_layer_diffs 一行的最小引用视图（不复制正文）。
type ModelDeltaRef struct {
	ID       int64  `json:"id"`        // biz_layer_diffs.id
	TaskID   int64  `json:"task_id"`   // 归属任务
	DiffType string `json:"diff_type"` // 差异类型（biz_layer_diffs.diff_type）
	Severity string `json:"severity"`  // critical/high/medium/low
	InScope  bool   `json:"in_scope"`  // 差异锚定的资产/接口是否在任务范围内
}

// delta 建议面阈值：仅 critical/high 差异值得转待验证假设（与发现准入的
// 保守口径一致）。medium/low 留在模型层观察，不进假设队列。
func deltaSeverityRank(s string) int {
	switch s {
	case "critical":
		return 4
	case "high":
		return 3
	case "medium":
		return 2
	case "low":
		return 1
	}
	return 0
}

// DeltaShouldBecomeHypothesis 判断「模型差异是否值得转成待验证假设」。
// 返回的只是建议语句（Hypothesis 建议 ≠ 漏洞结论）：
//   - 严重度达 high 及以上；
//   - 且差异锚定对象在任务范围内（越权锚定直接拒绝——INV-1）；
//   - 不满足则给出一句话理由。
func DeltaShouldBecomeHypothesis(d ModelDeltaRef) (bool, string) {
	if d.ID <= 0 {
		return false, "差异引用缺少 id，拒绝转假设"
	}
	if !d.InScope {
		return false, "差异锚定对象不在任务范围内：不生成假设（范围外问题走范围治理，不是探测目标）"
	}
	if deltaSeverityRank(d.Severity) < deltaSeverityRank("high") {
		return false, "严重度未达 high，留在模型层观察"
	}
	return true, fmt.Sprintf("建议假设：模型差异 #%d（%s/%s）的预期行为与观察行为不一致，待验证", d.ID, d.DiffType, d.Severity)
}

// ---------- 假设指纹：跨能力去重合并的基础 ----------

// fingerprintNoiseRe 去掉不影响语义的字符：空白与全半角标点折叠后再哈希。
var fingerprintNoiseRe = regexp.MustCompile(`[\s，。：；、！？（）()\[\]{}"'“”‘’,.:;!?\\|/]+`)

// HypothesisFingerprint 生成假设语句的规范化指纹（同一假设被不同能力重复
// 提出时可据此合并证据、不丢来源）。稳定：同输入恒同输出。
func HypothesisFingerprint(taskID int64, statement string) string {
	norm := fingerprintNoiseRe.ReplaceAllString(strings.ToLower(strings.TrimSpace(statement)), "")
	sum := sha256.Sum256([]byte(fmt.Sprintf("t%d|%s", taskID, norm)))
	return hex.EncodeToString(sum[:8]) // 16 hex，碰撞面足够小且可读
}

// ---------- 防循环台账 ----------

// MaxAutoFeedbackPerKey 同一 causation 对同一目标能力的自动反馈硬上限。
// 超限后拒绝并给理由；这是 0.4「同一未变化模型差异不能无限触发同一探索」
// 的确定性兜底（事件层闭环归 TI-06，此处先给进程内控制面）。
const MaxAutoFeedbackPerKey = 2

// CollabRecord 一次协作建议的记录契约（0.4「每次协作必须记录」的载体）。
// 落库归 TI-05；本包提供结构定义与内存台账。
type CollabRecord struct {
	CausationID    string        `json:"causation_id"`    // 触发源对象标识（如 biz_layer_diffs#12）
	CorrelationID  string        `json:"correlation_id"`  // 本次协作链路标识
	FromCapability string        `json:"from_capability"` // 建议来源能力（可为空=系统）
	ToCapability   string        `json:"to_capability"`   // 建议目标能力
	Reason         string        `json:"reason"`          // 触发原因一句话
	Accepted       bool          `json:"accepted"`        // 主 Agent 是否采纳
	OutputID       string        `json:"output_id"`       // 采纳后的产出对象 ID（假设/意图…）
	Duration       time.Duration `json:"duration"`        // 耗时
	Tokens         int64         `json:"tokens"`          // Token 消耗
	Status         string        `json:"status"`          // suggested/accepted/rejected/blocked
}

// CollabLedger 进程内防循环台账 + 最近协作记录（容量受限，只做控制面）。
type CollabLedger struct {
	mu      sync.Mutex
	counts  map[string]int // key = causationID + "|" + toCapability
	recent  []CollabRecord // 环形保留最近记录，防止无界增长
	maxKeep int
}

// NewCollabLedger 产出空台账。
func NewCollabLedger() *CollabLedger {
	return &CollabLedger{counts: map[string]int{}, maxKeep: 256}
}

// feedbackKey 幂等键： causation（未变化对象）× 目标能力。
func feedbackKey(causationID, toCapability string) string {
	return causationID + "|" + toCapability
}

// AllowAutoFeedback 判断是否允许对 (causation, target) 再发一次自动反馈。
// 允许时占用一次预算并返回 correlation id；拒绝时返回原因，且**不占预算**。
func (l *CollabLedger) AllowAutoFeedback(causationID, toCapability string) (string, bool, string) {
	if causationID == "" || toCapability == "" {
		return "", false, "causation 或目标能力为空，拒绝自动反馈"
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	key := feedbackKey(causationID, toCapability)
	if l.counts[key] >= MaxAutoFeedbackPerKey {
		return "", false, fmt.Sprintf("自动反馈已达上限（%s → %s 共 %d 次）：同一未变化对象不再触发，等对象更新出新 causation", causationID, toCapability, MaxAutoFeedbackPerKey)
	}
	l.counts[key]++
	correlation := fmt.Sprintf("corr-%s-%d", causationID, l.counts[key])
	return correlation, true, ""
}

// Record 追加协作记录（容量上限后丢弃最旧记录）。
func (l *CollabLedger) Record(rec CollabRecord) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.maxKeep <= 0 {
		l.maxKeep = 256
	}
	if len(l.recent) >= l.maxKeep {
		l.recent = l.recent[1:]
	}
	l.recent = append(l.recent, rec)
}

// Recent 返回最近协作记录副本（读面）。
func (l *CollabLedger) Recent() []CollabRecord {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]CollabRecord, len(l.recent))
	copy(out, l.recent)
	return out
}

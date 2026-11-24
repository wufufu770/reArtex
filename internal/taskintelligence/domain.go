package taskintelligence

// domain.go —— v2.1 统一架构优化增补 §0.5「统一领域对象与数据关系」的领域契约
//（0.15.63，批次 TI-05）。
//
// 定位（§0.10「建议的 Go 模块边界」落位说明）：方案建议的 domain/ 目录不单独
// 建包——本仓库的既有惯例是 db 行结构在 db 包（存储投影）、领域契约在
// internal/taskintelligence（本文件）。两套结构字段同名同义，转换在接线层
// （server，TI-11 及后续）完成，避免互相 import 形成平行分类学。
//
// 存储映射（只指向，不复制主体数据；DDL 在 db/schema_task_intelligence.sql）：
//   Hypothesis           → hypotheses 表（(task_id, fingerprint) 唯一去重）
//   Fact                 → facts 表（observed / inferred 分kind）
//   EvidenceRef          → evidence_refs 表（正文留在 traffic/blob，只存引用）
//   ModelDelta           → 不另建对象：biz_layer_diffs 已是差异事实源，
//                          本包 ModelDeltaRef（collaboration.go）是其最小引用视图
//   Finding              → 复用 findings 表，不建平行漏洞实体（§0.5 原文）
//   CapabilityExecution  → capability_executions 表（execution_id 是回指锚点）
//
// 本文件只定义契约与确定性规则（校验、状态机），不做 IO。

import (
	"fmt"
	"time"
)

// ---------- Hypothesis：可证伪的测试假设（不是漏洞结论） ----------

// 假设状态（与 db 包白名单一致；两处常量必须同步演进）。
const (
	HypStatusProposed   = "proposed"
	HypStatusValidating = "validating"
	HypStatusValidated  = "validated"
	HypStatusRefuted    = "refuted"
	HypStatusDropped    = "dropped"
)

// Hypothesis 领域契约（§0.5 领域对象表字段）。
type Hypothesis struct {
	ID          int64   `json:"id"`
	TaskID      int64   `json:"task_id"`
	Fingerprint string  `json:"fingerprint"`
	Statement   string  `json:"statement"`
	Source      string  `json:"source"`
	Status      string  `json:"status"`
	Confidence  float64 `json:"confidence"`
	// created_by_execution_id：提出本假设的能力执行（capability_executions.execution_id）。
	CreatedByExecutionID string    `json:"created_by_execution_id"`
	ScopeVersion         int64     `json:"scope_version"`
	ModelVersion         int64     `json:"model_version"`
	CreatedAt            time.Time `json:"created_at"`
}

// ValidateHypothesis 校验假设契约：语句必填、状态在白名单内、置信度 ∈ [0,1]。
func ValidateHypothesis(h Hypothesis) error {
	if h.TaskID <= 0 {
		return fmt.Errorf("假设缺 task_id")
	}
	if h.Statement == "" {
		return fmt.Errorf("假设语句为必填")
	}
	switch h.Status {
	case "", HypStatusProposed, HypStatusValidating, HypStatusValidated, HypStatusRefuted, HypStatusDropped:
	default:
		return fmt.Errorf("非法假设状态 %q", h.Status)
	}
	if h.Confidence < 0 || h.Confidence > 1 {
		return fmt.Errorf("置信度必须在 [0,1]，got %v", h.Confidence)
	}
	return nil
}

// hypothesisTransitions 假设状态机（确定性规则，§0.5「可证伪」的推进面）：
//
//	proposed → validating（进入验证）
//	validating → proposed（证据不足打回，可重新排队）
//	validating → validated / refuted（证据裁决——只有这里能出终态结论）
//	proposed/validating → dropped（人工放弃）
//
// 禁止跳步（proposed → validated 直接晋级非法：没有经过验证的「确认」不是
// 假设的语义）；终态（validated/refuted/dropped）不可离开。
var hypothesisTransitions = map[string][]string{
	HypStatusProposed:   {HypStatusValidating, HypStatusDropped},
	HypStatusValidating: {HypStatusProposed, HypStatusValidated, HypStatusRefuted, HypStatusDropped},
	HypStatusValidated:  {},
	HypStatusRefuted:    {},
	HypStatusDropped:    {},
}

// CanTransitionHypothesis 判断状态迁移是否合法；不合法返回一句话原因。
func CanTransitionHypothesis(from, to string) (bool, string) {
	if from == to {
		return false, "状态未变化，无需迁移"
	}
	for _, next := range hypothesisTransitions[from] {
		if next == to {
			return true, ""
		}
	}
	return false, fmt.Sprintf("非法迁移 %s → %s：终态不可离开，非终态不可跳步（验证结论只能从 validating 出）", from, to)
}

// ---------- Fact：有来源的观察事实 ----------

// 事实种类：observed=工具/流量直接观察；inferred=推导（§0.5 要求区分）。
const (
	FactKindObserved = "observed"
	FactKindInferred = "inferred"
)

// Fact 领域契约（subject-predicate-object 三元组 + 来源）。
type Fact struct {
	ID         int64     `json:"id"`
	TaskID     int64     `json:"task_id"`
	Subject    string    `json:"subject"`
	Predicate  string    `json:"predicate"`
	Object     string    `json:"object"`
	FactKind   string    `json:"fact_kind"`
	SourceType string    `json:"source_type"`
	SourceRef  string    `json:"source_ref"`
	Confidence float64   `json:"confidence"`
	ObservedAt time.Time `json:"observed_at"`
}

// ValidateFact 校验事实契约：来源必填（无来源不算事实）、种类白名单、置信度域。
func ValidateFact(f Fact) error {
	if f.TaskID <= 0 {
		return fmt.Errorf("事实缺 task_id")
	}
	if f.SourceType == "" && f.SourceRef == "" {
		return fmt.Errorf("事实缺来源（source_type/source_ref 至少一项）——无来源不算事实")
	}
	switch f.FactKind {
	case "", FactKindObserved, FactKindInferred:
	default:
		return fmt.Errorf("非法事实种类 %q", f.FactKind)
	}
	if f.Confidence < 0 || f.Confidence > 1 {
		return fmt.Errorf("置信度必须在 [0,1]，got %v", f.Confidence)
	}
	return nil
}

// ---------- EvidenceRef：统一证据引用 ----------

// 证据对假设的支持方向。
const (
	EvidenceDirSupports = "supports"
	EvidenceDirRefutes  = "refutes"
	EvidenceDirNeutral  = "neutral"
)

// EvidenceRef 领域契约：只存受控引用（content_ref + hash），正文不进本对象
// （§0.12：大内容留在 traffic/blob，敏感正文不得进普通对象 payload）。
type EvidenceRef struct {
	ID          int64     `json:"id"`
	TaskID      int64     `json:"task_id"`
	AssetID     int64     `json:"asset_id"`
	Kind        string    `json:"kind"`
	ContentRef  string    `json:"content_ref"`
	ContentHash string    `json:"content_hash"`
	Provenance  string    `json:"provenance"`
	Support     string    `json:"support"`
	ObservedAt  time.Time `json:"observed_at"`
}

// ValidateEvidenceRef 校验证据契约：引用或 hash 至少一项（不可追溯的引用拒绝）、
// 支持方向白名单。
func ValidateEvidenceRef(e EvidenceRef) error {
	if e.TaskID <= 0 {
		return fmt.Errorf("证据缺 task_id")
	}
	if e.ContentRef == "" && e.ContentHash == "" {
		return fmt.Errorf("证据缺 content_ref/content_hash——不可追溯的引用拒绝")
	}
	switch e.Support {
	case "", EvidenceDirSupports, EvidenceDirRefutes, EvidenceDirNeutral:
	default:
		return fmt.Errorf("非法支持方向 %q", e.Support)
	}
	return nil
}

// ---------- CapabilityExecution：能力执行记录 ----------

// 执行状态：running 是唯一非终态；终态不可复活（重试=新执行，幂等键去重）。
const (
	ExecStatusRunning   = "running"
	ExecStatusCompleted = "completed"
	ExecStatusFailed    = "failed"
	ExecStatusCancelled = "cancelled"
	ExecStatusBlocked   = "blocked"
)

// CapabilityExecution 领域契约（§0.5 领域对象表字段）。
type CapabilityExecution struct {
	ID            int64          `json:"id"`
	TaskID        int64          `json:"task_id"`
	CapabilityID  string         `json:"capability_id"`
	InputRefs     map[string]any `json:"input_refs"`
	OutputRefs    map[string]any `json:"output_refs"`
	Status        string         `json:"status"`
	CostTokens    int64          `json:"cost_tokens"`
	ExecutionID   string         `json:"execution_id"`
	CorrelationID string         `json:"correlation_id"`
	CausationID   string         `json:"causation_id"`
	// scope_version 调用前后的水位：不一致 = 长调用期间授权范围被变更（§0.12
	// 「执行前再次校验」的取证面；裁决权在确定性代码，不在本对象）。
	ScopeVersionStart int64     `json:"scope_version_at_start"`
	ScopeVersionEnd   int64     `json:"scope_version_at_end"`
	CreatedAt         time.Time `json:"created_at"`
}

// ValidateCapabilityExecution 校验执行契约：能力 ID / 任务 / envelope 标识必填、
// 状态白名单。
func ValidateCapabilityExecution(e CapabilityExecution) error {
	if e.TaskID <= 0 {
		return fmt.Errorf("能力执行缺 task_id")
	}
	if e.CapabilityID == "" {
		return fmt.Errorf("能力执行缺 capability_id")
	}
	if e.ExecutionID == "" {
		return fmt.Errorf("能力执行缺 execution_id（其他对象用它回指）")
	}
	switch e.Status {
	case "", ExecStatusRunning, ExecStatusCompleted, ExecStatusFailed, ExecStatusCancelled, ExecStatusBlocked:
	default:
		return fmt.Errorf("非法执行状态 %q", e.Status)
	}
	return nil
}

// ScopeDrifted 判断执行前后范围水位是否漂移（两个水位都非零才可比——零值表示
// 该侧未记录，不能断言漂移）。
func ScopeDrifted(e CapabilityExecution) bool {
	return e.ScopeVersionStart > 0 && e.ScopeVersionEnd > 0 && e.ScopeVersionStart != e.ScopeVersionEnd
}

// ---------- TI-04 台账 → §0.5 执行对象的桥 ----------

// CollabRecordToExecution 把 TI-04 的协作台账记录映射为能力执行契约（0.4
// 「每次协作必须可记录」的落库形态：correlation/causation/来源/目标能力一一
// 对应）。状态映射：suggested→running（建议已受理）、accepted→completed、
// rejected/blocked→blocked。
func CollabRecordToExecution(rec CollabRecord, taskID int64) CapabilityExecution {
	status := ExecStatusRunning
	switch rec.Status {
	case "accepted":
		status = ExecStatusCompleted
	case "rejected", "blocked":
		status = ExecStatusBlocked
	}
	return CapabilityExecution{
		TaskID:        taskID,
		CapabilityID:  rec.ToCapability,
		Status:        status,
		ExecutionID:   rec.CorrelationID,
		CorrelationID: rec.CorrelationID,
		CausationID:   rec.CausationID,
		CostTokens:    rec.Tokens,
	}
}

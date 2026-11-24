package db

import (
	"database/sql"
	_ "embed"
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

// Task Intelligence 统一领域对象访问层（v2.1 统一架构优化增补 §0.5，0.15.63，
// 批次 TI-05）：hypotheses / facts / evidence_refs / capability_executions 四表
// + edge_runs / biz_layer_diffs 的统一身份引用列。
//
// 分工与纪律（与 edge_run.go 同款）：
//   - DDL 走独立幂等文件 db/schema_task_intelligence.sql（db/schema.sql 是
//     RESERVED），由 EnsureTaskIntelligenceSchema 在启动期应用；
//   - ModelDelta 不另建表：biz_layer_diffs 是差异事实源；Finding 复用 findings
//     表——本层只提供引用列（evidence_refs.finding_id），不复制主体数据；
//   - scope_version 是任务范围水位（task_scope 的 MAX(id)）：任何范围插入都会
//     抬升水位，调用方在写对象前取一次、能力执行收尾再取一次，两值不一致即
//     「长调用期间 scope 被变更」的取证面（§0.12）；
//   - 假设的库层去重靠 (task_id, fingerprint) 唯一索引（指纹算法在
//     internal/taskintelligence.HypothesisFingerprint）：重复提案拿到已存在行，
//     不产生第二行——这是 §0.4「同一假设被不同能力重复提出时合并证据、不丢
//     来源」的确定性兜底。

//go:embed schema_task_intelligence.sql
var taskIntelligenceSchemaSQL string

// tiSchemaReady 记录某个连接已经应用过 TI 领域对象 DDL（与 loopTrackSchemaReady 同款）。
var tiSchemaReady sync.Map

// EnsureTaskIntelligenceSchema 应用 Task Intelligence 领域对象的幂等 DDL。
func (d *DB) EnsureTaskIntelligenceSchema() error {
	if _, ok := tiSchemaReady.Load(d.DB); ok {
		return nil
	}
	if _, err := d.Exec(taskIntelligenceSchemaSQL); err != nil {
		return fmt.Errorf("应用 task intelligence schema 失败: %w", err)
	}
	tiSchemaReady.Store(d.DB, struct{}{})
	return nil
}

// ensureTaskIntelligenceSchema 是访问层的幂等入口。
func (d *DB) ensureTaskIntelligenceSchema() error { return d.EnsureTaskIntelligenceSchema() }

// ---------- hypotheses ----------

// 假设状态白名单。假设 ≠ 漏洞结论（§0.5）：validated/refuted 只能由证据链推进。
const (
	HypothesisProposed   = "proposed"
	HypothesisValidating = "validating"
	HypothesisValidated  = "validated"
	HypothesisRefuted    = "refuted"
	HypothesisDropped    = "dropped"
)

// Hypothesis 是 hypotheses 表的一行（§0.5 领域对象：可证伪的测试假设）。
type Hypothesis struct {
	ID                   int64     `json:"id"`
	TaskID               int64     `json:"task_id"`
	CompanyID            int64     `json:"company_id"` // 0=NULL
	Fingerprint          string    `json:"fingerprint"`
	Statement            string    `json:"statement"`
	Status               string    `json:"status"`
	Source               string    `json:"source"`
	SourceType           string    `json:"source_type"`
	SourceRef            string    `json:"source_ref"`
	Confidence           float64   `json:"confidence"`
	CreatedByExecutionID string    `json:"created_by_execution_id"`
	CapabilityID         string    `json:"capability_id"`
	CorrelationID        string    `json:"correlation_id"`
	CausationID          string    `json:"causation_id"`
	ScopeVersion         int64     `json:"scope_version"`
	ModelVersion         int64     `json:"model_version"`
	MethodologyVersion   string    `json:"methodology_version"`
	CreatedAt            time.Time `json:"created_at"`
	UpdatedAt            time.Time `json:"updated_at"`
}

// ScopeVersionForTask 返回任务的范围水位（task_scope 的 MAX(id)，无范围=0）。
func (d *DB) ScopeVersionForTask(taskID int64) (int64, error) {
	var v int64
	err := d.QueryRow(`SELECT COALESCE(MAX(id),0) FROM task_scope WHERE task_id=$1`, taskID).Scan(&v)
	return v, err
}

// InsertHypothesis 插入一条假设；指纹重复（同任务同语句）时不产生第二行，
// 返回已存在行的 id 与 deduped=true——调用方据此把新来源合并到既有假设上。
func (d *DB) InsertHypothesis(h *Hypothesis) (id int64, deduped bool, err error) {
	if err := d.ensureTaskIntelligenceSchema(); err != nil {
		return 0, false, err
	}
	if h.Statement == "" {
		return 0, false, fmt.Errorf("假设 statement 为必填")
	}
	res, err := d.Exec(`
INSERT INTO hypotheses(task_id, company_id, fingerprint, statement, source, source_type,
                       source_ref, confidence, created_by_execution_id, capability_id,
                       correlation_id, causation_id, scope_version, model_version,
                       methodology_version)
VALUES ($1,NULLIF($2,0),$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)
ON CONFLICT (task_id, fingerprint) WHERE fingerprint <> '' DO NOTHING`,
		h.TaskID, h.CompanyID, h.Fingerprint, h.Statement, h.Source, h.SourceType,
		h.SourceRef, h.Confidence, h.CreatedByExecutionID, h.CapabilityID,
		h.CorrelationID, h.CausationID, h.ScopeVersion, h.ModelVersion,
		h.MethodologyVersion)
	if err != nil {
		return 0, false, fmt.Errorf("insert hypothesis: %w", err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		// 新插入：取回自增 id（RETURNING 在 ON CONFLICT 分支拿不到，二次查询）。
		err := d.QueryRow(`SELECT id FROM hypotheses WHERE task_id=$1 AND fingerprint=$2`,
			h.TaskID, h.Fingerprint).Scan(&id)
		if err != nil {
			return 0, false, err
		}
		return id, false, nil
	}
	// 指纹命中已存在行：返回既有 id（合并语义），不覆盖既有内容。
	err = d.QueryRow(`SELECT id FROM hypotheses WHERE task_id=$1 AND fingerprint=$2`,
		h.TaskID, h.Fingerprint).Scan(&id)
	if err != nil {
		return 0, false, err
	}
	return id, true, nil
}

const hypothesisCols = `id, task_id, COALESCE(company_id,0), fingerprint, statement, status,
	source, source_type, source_ref, confidence, created_by_execution_id, capability_id,
	correlation_id, causation_id, scope_version, model_version, methodology_version,
	created_at, updated_at`

func scanHypothesis(row *sql.Row) (*Hypothesis, error) {
	h := &Hypothesis{}
	err := row.Scan(&h.ID, &h.TaskID, &h.CompanyID, &h.Fingerprint, &h.Statement, &h.Status,
		&h.Source, &h.SourceType, &h.SourceRef, &h.Confidence, &h.CreatedByExecutionID,
		&h.CapabilityID, &h.CorrelationID, &h.CausationID, &h.ScopeVersion, &h.ModelVersion,
		&h.MethodologyVersion, &h.CreatedAt, &h.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return h, nil
}

// GetHypothesis 按 id 取一条假设；不存在返回 (nil, nil)。
func (d *DB) GetHypothesis(id int64) (*Hypothesis, error) {
	if err := d.ensureTaskIntelligenceSchema(); err != nil {
		return nil, err
	}
	h, err := scanHypothesis(d.QueryRow(
		`SELECT `+hypothesisCols+` FROM hypotheses WHERE id=$1`, id))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return h, err
}

// ListHypothesesByTask 列任务的假设（新→旧，limit 兜底 200）。
func (d *DB) ListHypothesesByTask(taskID int64, limit int) ([]*Hypothesis, error) {
	if err := d.ensureTaskIntelligenceSchema(); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 200 {
		limit = 200
	}
	rows, err := d.Query(`SELECT `+hypothesisCols+` FROM hypotheses WHERE task_id=$1 ORDER BY id DESC LIMIT $2`,
		taskID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Hypothesis
	for rows.Next() {
		h := &Hypothesis{}
		if err := rows.Scan(&h.ID, &h.TaskID, &h.CompanyID, &h.Fingerprint, &h.Statement, &h.Status,
			&h.Source, &h.SourceType, &h.SourceRef, &h.Confidence, &h.CreatedByExecutionID,
			&h.CapabilityID, &h.CorrelationID, &h.CausationID, &h.ScopeVersion, &h.ModelVersion,
			&h.MethodologyVersion, &h.CreatedAt, &h.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	if out == nil {
		out = []*Hypothesis{}
	}
	return out, nil
}

// UpdateHypothesisStatus 推进假设状态（白名单校验；谁推进由调用方的证据链决定，
// 本层只保证状态值合法并留 updated_at）。
func (d *DB) UpdateHypothesisStatus(id int64, status string) error {
	if err := d.ensureTaskIntelligenceSchema(); err != nil {
		return err
	}
	switch status {
	case HypothesisProposed, HypothesisValidating, HypothesisValidated, HypothesisRefuted, HypothesisDropped:
	default:
		return fmt.Errorf("非法假设状态 %q", status)
	}
	res, err := d.Exec(`UPDATE hypotheses SET status=$2, updated_at=now() WHERE id=$1`, id, status)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("假设 #%d 不存在", id)
	}
	return nil
}

// ---------- facts ----------

// 事实种类：observed=工具/流量直接观察；inferred=由事实/模型推导（§0.5 要求区分）。
const (
	FactObserved = "observed"
	FactInferred = "inferred"
)

// Fact 是 facts 表的一行（有来源的观察事实）。
type Fact struct {
	ID                 int64     `json:"id"`
	TaskID             int64     `json:"task_id"`
	CompanyID          int64     `json:"company_id"` // 0=NULL
	Subject            string    `json:"subject"`
	Predicate          string    `json:"predicate"`
	Object             string    `json:"object"`
	FactKind           string    `json:"fact_kind"`
	SourceType         string    `json:"source_type"`
	SourceRef          string    `json:"source_ref"`
	Confidence         float64   `json:"confidence"`
	HypothesisID       int64     `json:"hypothesis_id"` // 0=NULL
	ExecutionID        string    `json:"execution_id"`
	CapabilityID       string    `json:"capability_id"`
	CorrelationID      string    `json:"correlation_id"`
	CausationID        string    `json:"causation_id"`
	ScopeVersion       int64     `json:"scope_version"`
	ModelVersion       int64     `json:"model_version"`
	MethodologyVersion string    `json:"methodology_version"`
	ObservedAt         time.Time `json:"observed_at"`
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt          time.Time `json:"updated_at"`
}

// InsertFact 落一条观察事实（主体-谓词-对象三元组；来源必填——无来源不算事实）。
func (d *DB) InsertFact(f *Fact) (int64, error) {
	if err := d.ensureTaskIntelligenceSchema(); err != nil {
		return 0, err
	}
	if f.SourceType == "" && f.SourceRef == "" {
		return 0, fmt.Errorf("事实缺来源（source_type/source_ref 至少一项）——无来源不算事实")
	}
	var id int64
	err := d.QueryRow(`
INSERT INTO facts(task_id, company_id, subject, predicate, object, fact_kind,
                  source_type, source_ref, confidence, hypothesis_id, execution_id,
                  capability_id, correlation_id, causation_id, scope_version,
                  model_version, methodology_version, observed_at)
VALUES ($1,NULLIF($2,0),$3,$4,$5,$6,$7,$8,$9,NULLIF($10,0),$11,$12,$13,$14,$15,$16,$17,$18)
RETURNING id`,
		f.TaskID, f.CompanyID, f.Subject, f.Predicate, f.Object, f.FactKind,
		f.SourceType, f.SourceRef, f.Confidence, f.HypothesisID, f.ExecutionID,
		f.CapabilityID, f.CorrelationID, f.CausationID, f.ScopeVersion,
		f.ModelVersion, f.MethodologyVersion, f.ObservedAt).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("insert fact: %w", err)
	}
	return id, nil
}

// ListFactsByTask 列任务的观察事实（新→旧，limit 兜底 200）。
func (d *DB) ListFactsByTask(taskID int64, limit int) ([]*Fact, error) {
	if err := d.ensureTaskIntelligenceSchema(); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 200 {
		limit = 200
	}
	rows, err := d.Query(`
SELECT id, task_id, COALESCE(company_id,0), subject, predicate, object, fact_kind,
       source_type, source_ref, confidence, COALESCE(hypothesis_id,0), execution_id,
       capability_id, correlation_id, causation_id, scope_version, model_version,
       methodology_version, observed_at, created_at, updated_at
FROM facts WHERE task_id=$1 ORDER BY id DESC LIMIT $2`, taskID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Fact
	for rows.Next() {
		f := &Fact{}
		if err := rows.Scan(&f.ID, &f.TaskID, &f.CompanyID, &f.Subject, &f.Predicate, &f.Object,
			&f.FactKind, &f.SourceType, &f.SourceRef, &f.Confidence, &f.HypothesisID,
			&f.ExecutionID, &f.CapabilityID, &f.CorrelationID, &f.CausationID,
			&f.ScopeVersion, &f.ModelVersion, &f.MethodologyVersion,
			&f.ObservedAt, &f.CreatedAt, &f.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	if out == nil {
		out = []*Fact{}
	}
	return out, nil
}

// ---------- evidence_refs ----------

// 证据对假设的支持方向（§0.6 evidence.recorded：支持或反驳假设）。
const (
	EvidenceSupports = "supports"
	EvidenceRefutes  = "refutes"
	EvidenceNeutral  = "neutral"
)

// EvidenceRef 是 evidence_refs 表的一行（原始证据引用；正文留在 traffic/blob）。
type EvidenceRef struct {
	ID            int64     `json:"id"`
	TaskID        int64     `json:"task_id"`
	CompanyID     int64     `json:"company_id"` // 0=NULL
	AssetID       int64     `json:"asset_id"`   // 0=NULL
	Kind          string    `json:"kind"`
	ContentRef    string    `json:"content_ref"`
	ContentHash   string    `json:"content_hash"`
	Provenance    string    `json:"provenance"`
	Support       string    `json:"support"`
	HypothesisID  int64     `json:"hypothesis_id"` // 0=NULL
	FindingID     int64     `json:"finding_id"`    // 0=NULL
	ExecutionID   string    `json:"execution_id"`
	CapabilityID  string    `json:"capability_id"`
	CorrelationID string    `json:"correlation_id"`
	CausationID   string    `json:"causation_id"`
	ScopeVersion  int64     `json:"scope_version"`
	ObservedAt    time.Time `json:"observed_at"`
	CreatedAt     time.Time `json:"created_at"`
}

// InsertEvidenceRef 登记一条证据引用（content_ref / content_hash 至少一项——
// 既无引用也无 hash 的「证据」不可追溯，直接拒绝）。
func (d *DB) InsertEvidenceRef(e *EvidenceRef) (int64, error) {
	if err := d.ensureTaskIntelligenceSchema(); err != nil {
		return 0, err
	}
	if e.ContentRef == "" && e.ContentHash == "" {
		return 0, fmt.Errorf("证据缺 content_ref/content_hash——不可追溯的引用不落库")
	}
	var id int64
	err := d.QueryRow(`
INSERT INTO evidence_refs(task_id, company_id, asset_id, kind, content_ref, content_hash,
                          provenance, support, hypothesis_id, finding_id, execution_id,
                          capability_id, correlation_id, causation_id, scope_version, observed_at)
VALUES ($1,NULLIF($2,0),NULLIF($3,0),$4,$5,$6,$7,$8,NULLIF($9,0),NULLIF($10,0),$11,$12,$13,$14,$15,$16)
RETURNING id`,
		e.TaskID, e.CompanyID, e.AssetID, e.Kind, e.ContentRef, e.ContentHash,
		e.Provenance, e.Support, e.HypothesisID, e.FindingID, e.ExecutionID,
		e.CapabilityID, e.CorrelationID, e.CausationID, e.ScopeVersion, e.ObservedAt).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("insert evidence ref: %w", err)
	}
	return id, nil
}

// ListEvidenceByTask 列任务的证据引用（新→旧，limit 兜底 200）。
func (d *DB) ListEvidenceByTask(taskID int64, limit int) ([]*EvidenceRef, error) {
	if err := d.ensureTaskIntelligenceSchema(); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 200 {
		limit = 200
	}
	rows, err := d.Query(`
SELECT id, task_id, COALESCE(company_id,0), COALESCE(asset_id,0), kind, content_ref,
       content_hash, provenance, support, COALESCE(hypothesis_id,0), COALESCE(finding_id,0),
       execution_id, capability_id, correlation_id, causation_id, scope_version,
       observed_at, created_at
FROM evidence_refs WHERE task_id=$1 ORDER BY id DESC LIMIT $2`, taskID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*EvidenceRef
	for rows.Next() {
		e := &EvidenceRef{}
		if err := rows.Scan(&e.ID, &e.TaskID, &e.CompanyID, &e.AssetID, &e.Kind, &e.ContentRef,
			&e.ContentHash, &e.Provenance, &e.Support, &e.HypothesisID, &e.FindingID,
			&e.ExecutionID, &e.CapabilityID, &e.CorrelationID, &e.CausationID,
			&e.ScopeVersion, &e.ObservedAt, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	if out == nil {
		out = []*EvidenceRef{}
	}
	return out, nil
}

// ---------- capability_executions ----------

// 能力执行状态：running 为唯一非终态；终态不可复活（重试 = 新执行行，靠
// idempotency_key 去重）。
const (
	CapExecRunning   = "running"
	CapExecCompleted = "completed"
	CapExecFailed    = "failed"
	CapExecCancelled = "cancelled"
	CapExecBlocked   = "blocked"
)

// CapabilityExecution 是 capability_executions 表的一行（§0.5：每次能力调用的
// 可追踪记录）。
type CapabilityExecution struct {
	ID                int64           `json:"id"`
	TaskID            int64           `json:"task_id"`
	CompanyID         int64           `json:"company_id"` // 0=NULL
	CapabilityID      string          `json:"capability_id"`
	ToolName          string          `json:"tool_name"`
	Status            string          `json:"status"`
	InputRefs         json.RawMessage `json:"input_refs"`
	OutputRefs        json.RawMessage `json:"output_refs"`
	CostTokens        int64           `json:"cost_tokens"`
	DurationMs        int64           `json:"duration_ms"`
	Error             string          `json:"error"`
	ExecutionID       string          `json:"execution_id"`
	CorrelationID     string          `json:"correlation_id"`
	CausationID       string          `json:"causation_id"`
	IdempotencyKey    string          `json:"idempotency_key"`
	ScopeVersionStart int64           `json:"scope_version_at_start"`
	ScopeVersionEnd   int64           `json:"scope_version_at_end"`
	CreatedAt         time.Time       `json:"created_at"`
	UpdatedAt         time.Time       `json:"updated_at"`
}

// StartCapabilityExecution 开一条能力执行记录（scope_version 取当前水位落
// at_start 列）。幂等键重复时**不新开执行**，返回已存在行——这是 §0.11「幂等键
// 防止模型重试造成重复运行」的库层兑现，调用方应复用返回的既有执行而不是再跑一次。
func (d *DB) StartCapabilityExecution(e *CapabilityExecution) (id int64, deduped bool, err error) {
	if err := d.ensureTaskIntelligenceSchema(); err != nil {
		return 0, false, err
	}
	if e.CapabilityID == "" {
		return 0, false, fmt.Errorf("capability_id 为必填")
	}
	if e.ExecutionID == "" {
		return 0, false, fmt.Errorf("execution_id 为必填（envelope 标识，其他对象用它回指）")
	}
	inputRefs, _ := json.Marshal(e.InputRefs) // nil → "null"；统一落 {} 语义由调用方保证
	outputRefs, _ := json.Marshal(e.OutputRefs)
	res, err := d.Exec(`
INSERT INTO capability_executions(task_id, company_id, capability_id, tool_name, status,
                                  input_refs, output_refs, execution_id, correlation_id,
                                  causation_id, idempotency_key, scope_version_at_start)
VALUES ($1,NULLIF($2,0),$3,$4,$5,$6::jsonb,$7::jsonb,$8,$9,$10,$11,$12)
ON CONFLICT (idempotency_key) WHERE idempotency_key <> '' DO NOTHING`,
		e.TaskID, e.CompanyID, e.CapabilityID, e.ToolName, CapExecRunning,
		string(inputRefs), string(outputRefs), e.ExecutionID, e.CorrelationID,
		e.CausationID, e.IdempotencyKey, e.ScopeVersionStart)
	if err != nil {
		return 0, false, fmt.Errorf("start capability execution: %w", err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		if err := d.QueryRow(`SELECT id FROM capability_executions WHERE execution_id=$1`,
			e.ExecutionID).Scan(&id); err != nil {
			return 0, false, err
		}
		return id, false, nil
	}
	// 幂等键命中：返回既有执行（重试面），不覆盖既有内容。
	err = d.QueryRow(`SELECT id FROM capability_executions WHERE idempotency_key=$1`,
		e.IdempotencyKey).Scan(&id)
	if err != nil {
		return 0, false, err
	}
	return id, true, nil
}

// CompleteCapabilityExecution 收尾执行：status（终态白名单）+ 产出引用 + 成本 +
// 收尾时的 scope 水位（at_end）。调用方对比 at_start/at_end 判断 scope 是否中途漂移。
func (d *DB) CompleteCapabilityExecution(executionID, status string, outputRefs json.RawMessage,
	costTokens, durationMs int64, execErr string, scopeVersionEnd int64) error {
	if err := d.ensureTaskIntelligenceSchema(); err != nil {
		return err
	}
	switch status {
	case CapExecCompleted, CapExecFailed, CapExecCancelled, CapExecBlocked:
	default:
		return fmt.Errorf("非法能力执行终态 %q（running 只能被终态收尾，不可回写）", status)
	}
	if outputRefs == nil {
		outputRefs = json.RawMessage("{}")
	}
	res, err := d.Exec(`
UPDATE capability_executions
SET status=$2, output_refs=$3::jsonb, cost_tokens=$4, duration_ms=$5, error=$6,
    scope_version_at_end=$7, updated_at=now()
WHERE execution_id=$1`, executionID, status, string(outputRefs), costTokens, durationMs,
		execErr, scopeVersionEnd)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("能力执行 %s 不存在", executionID)
	}
	return nil
}

// ListCapabilityExecutionsByTask 列任务的能力执行记录（新→旧，limit 兜底 200）。
func (d *DB) ListCapabilityExecutionsByTask(taskID int64, limit int) ([]*CapabilityExecution, error) {
	if err := d.ensureTaskIntelligenceSchema(); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 200 {
		limit = 200
	}
	rows, err := d.Query(`
SELECT id, task_id, COALESCE(company_id,0), capability_id, tool_name, status,
       input_refs, output_refs, cost_tokens, duration_ms, error, execution_id,
       correlation_id, causation_id, idempotency_key, scope_version_at_start,
       scope_version_at_end, created_at, updated_at
FROM capability_executions WHERE task_id=$1 ORDER BY id DESC LIMIT $2`, taskID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*CapabilityExecution
	for rows.Next() {
		e := &CapabilityExecution{}
		if err := rows.Scan(&e.ID, &e.TaskID, &e.CompanyID, &e.CapabilityID, &e.ToolName,
			&e.Status, &e.InputRefs, &e.OutputRefs, &e.CostTokens, &e.DurationMs, &e.Error,
			&e.ExecutionID, &e.CorrelationID, &e.CausationID, &e.IdempotencyKey,
			&e.ScopeVersionStart, &e.ScopeVersionEnd, &e.CreatedAt, &e.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	if out == nil {
		out = []*CapabilityExecution{}
	}
	return out, nil
}

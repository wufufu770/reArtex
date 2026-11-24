// Package taskintelligence 实现 v2.1 统一架构优化增补 §0.2 的 Task Intelligence
// 统一任务智能层（0.15.60，批次 TI-02）：任务上下文快照聚合。
//
// 职责边界（§0.2 原文口径，代码即契约）：
//
// 负责：
//   - 生成任务上下文快照：统一聚合授权范围、资产、探索图、业务模型、事实、
//     假设、证据、发现和历史执行；
//   - 快照是只读视图，供主 Agent 与后续能力路由（TI-07）消费。
//
// 不负责：
//   - 不直接执行 shell、HTTP、浏览器或其他底层工具；
//   - 不替代 scope / SoD / netguard / pathguard / 审批 / 删除人工确认
//     （见 docs/安全不变量清单.md INV-10：模型只提议，确定性代码裁决）；
//   - 不拥有第二套任务状态机或独立漏洞库；
//   - 不让 LLM 自行决定授权范围、发现终态或安全策略。
//
// 接线说明：各 Provider 由 server 侧在装配期注入（归 TI-11「API 与工具契约」）；
// 未注入的分节在快照里如实标记 Unavailable，不伪造空数据。
package taskintelligence

import (
	"context"
	"fmt"
	"time"
)

// ContextContractVersion 是上下文快照的版本化契约号。字段语义变更时递增，
// 消费方按契约号判读（0.2「同一上下文来源」的版本化落点）。
const ContextContractVersion = "ti.context.v1"

// TaskContextSnapshot 是一次任务上下文的只读聚合视图。
type TaskContextSnapshot struct {
	ContractVersion string    `json:"contract_version"` // 恒为 ContextContractVersion
	TaskID          int64     `json:"task_id"`
	GeneratedAt     time.Time `json:"generated_at"`

	Scope       ScopeSummary       `json:"scope"`
	Assets      AssetSummary       `json:"assets"`
	Exploration ExplorationSummary `json:"exploration"`
	BizModel    BizModelSummary    `json:"biz_model"`
	Facts       FactsSummary       `json:"facts"`
	Evidence    EvidenceSummary    `json:"evidence"`
	Findings    FindingsSummary    `json:"findings"`
	History     HistorySummary     `json:"history"`
}

// Unavailable 返回快照中不可用分节及原因，供调用方决定降级策略。
// 返回空 map = 全分节可用。
func (s *TaskContextSnapshot) Unavailable() map[string]string {
	bad := map[string]string{}
	if s.Scope.Err != nil {
		bad["scope"] = s.Scope.Err.Error()
	}
	if s.Assets.Err != nil {
		bad["assets"] = s.Assets.Err.Error()
	}
	if s.Exploration.Err != nil {
		bad["exploration"] = s.Exploration.Err.Error()
	}
	if s.BizModel.Err != nil {
		bad["biz_model"] = s.BizModel.Err.Error()
	}
	if s.Facts.Err != nil {
		bad["facts"] = s.Facts.Err.Error()
	}
	if s.Evidence.Err != nil {
		bad["evidence"] = s.Evidence.Err.Error()
	}
	if s.Findings.Err != nil {
		bad["findings"] = s.Findings.Err.Error()
	}
	if s.History.Err != nil {
		bad["history"] = s.History.Err.Error()
	}
	return bad
}

// ---------- 各分节摘要（只计数与引用，不搬运大文本） ----------

// ScopeSummary 授权范围分节。Version 为空表示当前范围源没有版本化快照——
// 这是已登记缺口（TI-01 审计：scope_version 全库零落库），不伪造值。
type ScopeSummary struct {
	Version   string `json:"version"`
	AssetRows int    `json:"asset_rows"` // 范围内条目数
	Err       error  `json:"-"`
}

// AssetSummary 资产图分节。
type AssetSummary struct {
	Total   int   `json:"total"`
	InScope int   `json:"in_scope"`
	Err     error `json:"-"`
}

// ExplorationSummary 探索图（偏锋）分节：格点状态计数。
type ExplorationSummary struct {
	Open       int   `json:"open"`
	Dispatched int   `json:"dispatched"`
	Confirmed  int   `json:"confirmed"`
	Disproven  int   `json:"disproven"`
	Folded     int   `json:"folded"`
	Err        error `json:"-"`
}

// BizModelSummary 业务模型分节。Version=0 表示尚无建模快照（biz_model_versions 无行）。
type BizModelSummary struct {
	Version    int64     `json:"version"`
	UpdatedAt  time.Time `json:"updated_at"`
	LayerDiffs int       `json:"layer_diffs"` // biz_layer_diffs 未收敛差异计数
	Err        error     `json:"-"`
}

// FactsSummary 事实与假设分节。
type FactsSummary struct {
	Total      int   `json:"total"`      // facts 总数
	Hypotheses int   `json:"hypotheses"` // 待验证假设数（open intents/格点假设口径）
	Err        error `json:"-"`
}

// EvidenceSummary 证据分节。
type EvidenceSummary struct {
	Count int   `json:"count"`
	Err   error `json:"-"`
}

// FindingsSummary 发现分节。
type FindingsSummary struct {
	Open      int   `json:"open"`      // 未终态发现
	Confirmed int   `json:"confirmed"` // 已确认
	Err       error `json:"-"`
}

// HistorySummary 历史执行分节：该任务能力调用/意图执行次数。
type HistorySummary struct {
	Executions int   `json:"executions"`
	Err        error `json:"-"`
}

// ---------- 窄接口：server/db 侧适配（TI-11 接线），本包不做 SQL ----------

// ScopeProvider 提供授权范围摘要。
type ScopeProvider interface {
	ScopeSummary(ctx context.Context, taskID int64) (ScopeSummary, error)
}

// AssetProvider 提供资产图摘要。
type AssetProvider interface {
	AssetSummary(ctx context.Context, taskID int64) (AssetSummary, error)
}

// ExplorationProvider 提供偏锋探索图摘要。
type ExplorationProvider interface {
	ExplorationSummary(ctx context.Context, taskID int64) (ExplorationSummary, error)
}

// BizModelProvider 提供业务模型摘要。
type BizModelProvider interface {
	BizModelSummary(ctx context.Context, taskID int64) (BizModelSummary, error)
}

// FactProvider 提供事实与假设摘要。
type FactProvider interface {
	FactsSummary(ctx context.Context, taskID int64) (FactsSummary, error)
}

// EvidenceProvider 提供证据摘要。
type EvidenceProvider interface {
	EvidenceSummary(ctx context.Context, taskID int64) (EvidenceSummary, error)
}

// FindingProvider 提供发现摘要。
type FindingProvider interface {
	FindingsSummary(ctx context.Context, taskID int64) (FindingsSummary, error)
}

// HistoryProvider 提供历史执行摘要。
type HistoryProvider interface {
	HistorySummary(ctx context.Context, taskID int64) (HistorySummary, error)
}

// SnapshotBuilder 聚合各 Provider 产出快照。字段可为 nil：对应分节标记
// Unavailable（接线缺失是显式状态，不是空数据）。
type SnapshotBuilder struct {
	Scope       ScopeProvider
	Assets      AssetProvider
	Exploration ExplorationProvider
	BizModel    BizModelProvider
	Facts       FactProvider
	Evidence    EvidenceProvider
	Findings    FindingProvider
	History     HistoryProvider
}

// unavailable 填一个"接线缺失"的分节错误。
func unavailable(section string) error {
	return fmt.Errorf("provider 未接线（%s）：分节不可用，见 TI-11 接线任务", section)
}

// Build 聚合快照。单分节失败不短路整份快照（失败记入该分节 Err）；
// 仅当 ctx 已取消/超时或 taskID 非法时返回错误。
func (b *SnapshotBuilder) Build(ctx context.Context, taskID int64) (*TaskContextSnapshot, error) {
	if taskID <= 0 {
		return nil, fmt.Errorf("task id 必须为正数，得到 %d", taskID)
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("构建上下文快照被取消: %w", err)
	}
	snap := &TaskContextSnapshot{
		ContractVersion: ContextContractVersion,
		TaskID:          taskID,
		GeneratedAt:     time.Now(),
	}
	fill := func(err error, target *error, section string) {
		if err != nil {
			*target = fmt.Errorf("%s: %w", section, err)
		}
	}
	if b.Scope == nil {
		snap.Scope.Err = unavailable("scope")
	} else if v, err := b.Scope.ScopeSummary(ctx, taskID); err != nil {
		fill(err, &snap.Scope.Err, "scope")
	} else {
		snap.Scope = v
	}
	if b.Assets == nil {
		snap.Assets.Err = unavailable("assets")
	} else if v, err := b.Assets.AssetSummary(ctx, taskID); err != nil {
		fill(err, &snap.Assets.Err, "assets")
	} else {
		snap.Assets = v
	}
	if b.Exploration == nil {
		snap.Exploration.Err = unavailable("exploration")
	} else if v, err := b.Exploration.ExplorationSummary(ctx, taskID); err != nil {
		fill(err, &snap.Exploration.Err, "exploration")
	} else {
		snap.Exploration = v
	}
	if b.BizModel == nil {
		snap.BizModel.Err = unavailable("biz_model")
	} else if v, err := b.BizModel.BizModelSummary(ctx, taskID); err != nil {
		fill(err, &snap.BizModel.Err, "biz_model")
	} else {
		snap.BizModel = v
	}
	if b.Facts == nil {
		snap.Facts.Err = unavailable("facts")
	} else if v, err := b.Facts.FactsSummary(ctx, taskID); err != nil {
		fill(err, &snap.Facts.Err, "facts")
	} else {
		snap.Facts = v
	}
	if b.Evidence == nil {
		snap.Evidence.Err = unavailable("evidence")
	} else if v, err := b.Evidence.EvidenceSummary(ctx, taskID); err != nil {
		fill(err, &snap.Evidence.Err, "evidence")
	} else {
		snap.Evidence = v
	}
	if b.Findings == nil {
		snap.Findings.Err = unavailable("findings")
	} else if v, err := b.Findings.FindingsSummary(ctx, taskID); err != nil {
		fill(err, &snap.Findings.Err, "findings")
	} else {
		snap.Findings = v
	}
	if b.History == nil {
		snap.History.Err = unavailable("history")
	} else if v, err := b.History.HistorySummary(ctx, taskID); err != nil {
		fill(err, &snap.History.Err, "history")
	} else {
		snap.History = v
	}
	return snap, nil
}

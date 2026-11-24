package taskintelligence

// capability.go —— v2.1 统一架构优化增补 §0.3 能力模型（0.15.61，批次 TI-03）。
//
// 统一能力注册表：模块是可调用能力（capability），不是独立 Agent 产品。
// 五个标准 capability 以稳定 ID 注册，映射到仓库现有实现（工具族作为绑定提示，
// 真正的调用接线归 TI-11； Specialist 委派由 AUTO 按需决定（TI-07），注册表
// 本身不创建任何 Agent——「不得因为存在一个独立页面，就默认创建一个常驻
// Agent」是本文件的名注释铁律。

import (
	"fmt"
	"regexp"
	"sort"
	"sync"
)

// RiskClass 风险档：能力触达面越接近不可逆/出网/删除，档位越高。
type RiskClass string

const (
	RiskLow    RiskClass = "low"
	RiskMedium RiskClass = "medium"
	RiskHigh   RiskClass = "high"
)

// CostClass 成本档：以 token/时间预算的粗粒度分级。
type CostClass string

const (
	CostLow    CostClass = "low"
	CostMedium CostClass = "medium"
	CostHigh   CostClass = "high"
)

// CapabilityDescriptor 能力登记项（§0.3 字段口径）。
type CapabilityDescriptor struct {
	// ID 稳定能力标识（如 "exploration.edge"）。注册后作为跨模块引用主键。
	ID string `json:"id"`
	// Version 能力实现版本（随实现演进递增，变更需显式 Replace）。
	Version string `json:"version"`
	// Description 一句话职责。
	Description string `json:"description"`
	// InputSchema / OutputSchema 输入输出的 JSON Schema 形状描述（v1 期为
	// 描述性文本；正式 schema 校验归 TI-11 接线时启用）。
	InputSchema  string `json:"input_schema"`
	OutputSchema string `json:"output_schema"`
	// RequiredContext 声明依赖的上下文分节（与 TaskContextSnapshot 分节同名：
	// scope/assets/exploration/biz_model/facts/evidence/findings/history）。
	RequiredContext []string `json:"required_context"`
	// RequiredScopes 声明依赖的授权范围要素（如 "task_scope"、"asset_scope"）。
	RequiredScopes []string `json:"required_scopes"`
	// RiskClass / CostClass 风险与成本档。
	RiskClass RiskClass `json:"risk_class"`
	CostClass CostClass `json:"cost_class"`
	// DefaultPolicy §0.3 的默认启用策略（自然语言口径，AUTO 判定归 TI-07）。
	DefaultPolicy string `json:"default_policy"`
	// BoundTools 仓库中承载该能力的现有工具族（登记映射，供审计与 TI-11 接线
	// 对照；注册表不直接调用这些工具）。
	BoundTools []string `json:"bound_tools"`
	// Enabled 是否参与路由建议。
	Enabled bool `json:"enabled"`
}

var capabilityIDRe = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)+$`)

// Validate 校验登记项必填面与取值域。
func (d CapabilityDescriptor) Validate() error {
	if !capabilityIDRe.MatchString(d.ID) {
		return fmt.Errorf("capability id %q 不合法（需形如 domain.name 的小写点分格式）", d.ID)
	}
	if d.Version == "" {
		return fmt.Errorf("capability %s 缺少 version", d.ID)
	}
	switch d.RiskClass {
	case RiskLow, RiskMedium, RiskHigh:
	default:
		return fmt.Errorf("capability %s risk_class 非法: %q", d.ID, d.RiskClass)
	}
	switch d.CostClass {
	case CostLow, CostMedium, CostHigh:
	default:
		return fmt.Errorf("capability %s cost_class 非法: %q", d.ID, d.CostClass)
	}
	for _, ctx := range d.RequiredContext {
		if !knownContextSection(ctx) {
			return fmt.Errorf("capability %s 声明了未知上下文分节 %q", d.ID, ctx)
		}
	}
	return nil
}

// knownContextSection 与 TaskContextSnapshot 分节一一对应，防止登记项引用
// 不存在的上下文面。
func knownContextSection(s string) bool {
	switch s {
	case "scope", "assets", "exploration", "biz_model", "facts", "evidence", "findings", "history":
		return true
	}
	return false
}

// Registry 能力注册表。并发安全；不持久化（注册表是进程内确定性数据，
// 持久化与运行时启用开关归 TI-11/TI-07）。
type Registry struct {
	mu   sync.RWMutex
	caps map[string]CapabilityDescriptor
}

// StandardCapabilityID 五个标准能力 ID（§0.3 表格一字不改）。
const (
	CapTestingStandard  = "testing.standard"
	CapExplorationEdge  = "exploration.edge"
	CapModelingBusiness = "modeling.business"
	CapValidationDeep   = "validation.deep"
	CapReportingResult  = "reporting.result"
)

// NewRegistry 产出已注册五个标准能力的注册表。绑定映射以仓库当前实现为准：
//   - testing.standard  → 常规 worker 意图执行与领域工具族（add_intent/insert_assets 等）
//   - exploration.edge  → 偏锋探索工具族（server/edge_tools.go：edge_init_cells/
//     edge_dispatch/edge_switch/edge_consult/edge_conclude…）
//   - modeling.business → 业务建模工具族（server/biz_api.go + db/business_graph.go：
//     biz_anchors/biz_nodes/biz_edges/biz_layer_diffs）
//   - validation.deep   → 发现验证与复测面（finding_retests/finding validation 状态机）
//   - reporting.result  → 报告中心（server/report_gen.go，reartex:report）
func NewRegistry() *Registry {
	r := &Registry{caps: map[string]CapabilityDescriptor{}}
	for _, d := range []CapabilityDescriptor{
		{
			ID:              CapTestingStandard,
			Version:         "1.0.0",
			Description:     "常规资产、服务和漏洞测试：按任务范围派发意图、登记资产、产出事实与发现",
			OutputSchema:    "facts,evidence,findings,intents",
			RequiredContext: []string{"scope", "assets", "findings"},
			RequiredScopes:  []string{"task_scope"},
			RiskClass:       RiskMedium,
			CostClass:       CostMedium,
			DefaultPolicy:   "AUTO 按任务启用",
			BoundTools:      []string{"add_intent", "insert_assets", "record_fact", "report_finding"},
			Enabled:         true,
		},
		{
			ID:              CapExplorationEdge,
			Version:         "1.0.0",
			Description:     "偏锋探索、非典型假设生成：方法论驱动的格点假设与验证",
			InputSchema:     "assets,interfaces,methodology",
			OutputSchema:    "hypotheses,intents,evidence",
			RequiredContext: []string{"scope", "exploration", "biz_model"},
			RequiredScopes:  []string{"task_scope"},
			RiskClass:       RiskMedium,
			CostClass:       CostHigh,
			DefaultPolicy:   "有探索价值时调用",
			BoundTools:      []string{"edge_init_cells", "edge_dispatch", "edge_switch", "edge_consult", "edge_conclude"},
			Enabled:         true,
		},
		{
			ID:              CapModelingBusiness,
			Version:         "1.0.0",
			Description:     "业务流程、角色、权限和状态建模：Design/Behavior 双层与差异登记",
			InputSchema:     "interfaces,traffic,role_hints",
			OutputSchema:    "design,behavior,model_deltas",
			RequiredContext: []string{"scope", "assets", "biz_model", "evidence"},
			RequiredScopes:  []string{"task_scope"},
			RiskClass:       RiskLow,
			CostClass:       CostMedium,
			DefaultPolicy:   "业务型目标或上下文不足时调用",
			BoundTools:      []string{"biz_model_upsert", "biz_layer_diff_upsert"},
			Enabled:         true,
		},
		{
			ID:              CapValidationDeep,
			Version:         "1.0.0",
			Description:     "对候选发现进行深入验证：复测与对抗性核验（产出 validation_result/finding_update）",
			InputSchema:     "hypothesis_or_finding,evidence",
			OutputSchema:    "validation_result,finding_update",
			RequiredContext: []string{"scope", "facts", "evidence", "findings"},
			RequiredScopes:  []string{"task_scope"},
			RiskClass:       RiskHigh,
			CostClass:       CostHigh,
			DefaultPolicy:   "有明确验证对象时调用",
			BoundTools:      []string{"report_finding", "finding_retest"},
			Enabled:         true,
		},
		{
			ID:              CapReportingResult,
			Version:         "1.0.0",
			Description:     "结果整理与报告：从统一发现/证据生成报告工件（reartex:report）",
			InputSchema:     "findings,evidence,coverage",
			OutputSchema:    "report_artifact",
			RequiredContext: []string{"findings", "evidence", "history"},
			RequiredScopes:  []string{"task_scope"},
			RiskClass:       RiskLow,
			CostClass:       CostLow,
			DefaultPolicy:   "任务收口时调用",
			BoundTools:      []string{"report_gen"},
			Enabled:         true,
		},
	} {
		if err := r.Register(d); err != nil {
			// 标准能力是编译期常量数据，注册失败只可能是代码错误，直接 panic。
			panic(fmt.Sprintf("taskintelligence: 标准能力 %s 注册失败: %v", d.ID, err))
		}
	}
	return r
}

// Register 登记能力；ID 重复或登记项不合法时报错（升级走 Replace）。
func (r *Registry) Register(d CapabilityDescriptor) error {
	if err := d.Validate(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.caps[d.ID]; dup {
		return fmt.Errorf("capability %s 已注册，升级请用 Replace 显式换版", d.ID)
	}
	r.caps[d.ID] = d
	return nil
}

// Replace 以显式换版语义覆盖登记项（调用方负责递增 Version）。
func (r *Registry) Replace(d CapabilityDescriptor) error {
	if err := d.Validate(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.caps[d.ID] = d
	return nil
}

// Lookup 按 ID 查能力。
func (r *Registry) Lookup(id string) (CapabilityDescriptor, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	d, ok := r.caps[id]
	return d, ok
}

// ListEnabled 返回启用的能力，按 ID 字典序稳定输出。
func (r *Registry) ListEnabled() []CapabilityDescriptor {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]CapabilityDescriptor, 0, len(r.caps))
	for _, d := range r.caps {
		if d.Enabled {
			out = append(out, d)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Disable / Enable 运行时开关（建议面开关，不是权限面：真正的执行闸门仍走
// scope/SoD，见 INV-10）。
func (r *Registry) Disable(id string) error { return r.setEnabled(id, false) }

// Enable 重新启用能力。
func (r *Registry) Enable(id string) error { return r.setEnabled(id, true) }

func (r *Registry) setEnabled(id string, on bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	d, ok := r.caps[id]
	if !ok {
		return fmt.Errorf("capability %s 未注册", id)
	}
	d.Enabled = on
	r.caps[id] = d
	return nil
}

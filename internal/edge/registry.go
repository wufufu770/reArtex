// Package edge 是偏锋探索的方法论注册表与判据实现（EDGE E2）。
//
// 注册表的三条硬约束（方案二 §5.2，全部是注册期的确定性校验，不是运行时自觉）：
//   - 【三级条目不得自带完成判据】：隐喻不可证伪，允许隐喻自带判据会产生一批
//     永远产出内容、永远无法被评测淘汰的 doctrine。CompletionCriterion 来自
//     一级或二级方法论且不可为空；三级只有 HeuristicTags。
//   - 【上限参数是判据表达式的一部分】：每条判据注册时必须声明上限参数——
//     缺了它会朝「永不完成」方向退化，而退化本身不报错（最后被人设兜底掐断，
//     指标上误读成「方法论有效但目标太大」）。缺上限参数 ⇒ 注册被拒。
//   - 【EvidenceGrade 必填】：实证 / 标准 / 隐喻三级证据等级，评测聚合层的
//     分散度评估按它分层。
//
// 注册表是代码常量（Enabled/Priority 由人改代码发布，不让 agent 自选方法论——
// 自我确认偏误的处置：方法论由人指定或按轮换表盲选）。
package edge

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
)

// CriterionKind 是五类判据类型（方案二 §5.3）。
type CriterionKind string

const (
	// CriterionCoverage 覆盖型：每个相关组合至少一条假设且各有结论
	//（STPA/UCA、ATT&CK、FMEA 矩阵覆盖）。
	CriterionCoverage CriterionKind = "coverage"
	// CriterionConvergence 收敛型：每个原因已确认或已证伪（pre-mortem）。
	CriterionConvergence CriterionKind = "convergence"
	// CriterionExhaust 穷举型：每条否定项已转成假设（逆向/负空间）。
	CriterionExhaust CriterionKind = "exhaust"
	// CriterionDeplete 耗尽型：每个映射已判定成立或不成立（跨域类比迁移）。
	CriterionDeplete CriterionKind = "deplete"
	// CriterionDecay 衰减型：连续 N 轮信息增益低于阈值。
	CriterionDecay CriterionKind = "decay"
)

// EvidenceGrade 是方法论的证据等级（评测聚合层按它分层）。
type EvidenceGrade string

const (
	GradeEmpirical EvidenceGrade = "empirical" // 实证：有对照实验或生产证据
	GradeStandard  EvidenceGrade = "standard"  // 标准：行业方法论文本（STPA/FMEA 等）
	GradeMetaphor  EvidenceGrade = "metaphor"  // 隐喻：兵法/思维工具，启发式
)

// UpperBound 是判据的一条上限参数（防退化必备项，不是调优项）。
type UpperBound struct {
	// Kind 是上限的语义维度：target_set（目标集合规模）/ cause_set（原因集冻结点）/
	// asset_set（资产清单规模）/ mapping_set（类比源数量）/ decay（衰减窗口）。
	Kind  string `json:"kind"`
	Limit int    `json:"limit"`
}

// CompletionCriterion 是一条完成判据：类型 + 上限参数。
type CompletionCriterion struct {
	Kind   CriterionKind `json:"kind"`
	Bounds []UpperBound  `json:"bounds"`
}

// Validate 校验判据的注册约束：类型合法 + 至少一条上限参数。
func (c *CompletionCriterion) Validate() error {
	if c == nil {
		return errors.New("判据为空：一级/二级方法论必须自带 CompletionCriterion")
	}
	switch c.Kind {
	case CriterionCoverage, CriterionConvergence, CriterionExhaust, CriterionDeplete, CriterionDecay:
	default:
		return fmt.Errorf("未知判据类型 %q（五类之外不注册）", c.Kind)
	}
	if len(c.Bounds) == 0 {
		return fmt.Errorf("判据 %q 缺上限参数：上限是判据表达式的一部分，缺了它会朝「永不完成」退化且不报错", c.Kind)
	}
	for _, b := range c.Bounds {
		if b.Limit <= 0 {
			return fmt.Errorf("判据 %q 的上限参数 %s 非法（limit 必须 > 0）", c.Kind, b.Kind)
		}
	}
	return nil
}

// Methodology 是注册表的一条方法论。
type Methodology struct {
	ID            string              // 稳定 id（tasks.methodology_id 存它）
	Name          string              // 中文显示名
	Tier          int                 // 1=机制与判据都可机器判定；2=需改造；3=隐喻
	EvidenceGrade EvidenceGrade       // 证据等级（必填）
	Summary       string              // 一句话说明（进提示词与前端）
	Criterion     *CompletionCriterion // 一/二级必填；三级必须为 nil
	HeuristicTags []string            // 启发式：只参与提示词拼装与人工复盘，不参与判据
	Enabled       bool                // 由人配置；agent 不得自选未启用的方法论
	Priority      int                 // 轮换表盲选的排序（人配）
}

// registry 是进程级注册表。注册全部发生在 init（代码常量），无运行时注册入口
// ——「三级条目不得自带判据」这类约束在编译期就定死，不给运行期留口子。
var registry = map[string]*Methodology{}

// ErrInvalidMethodology 是 Register 的统一错误前缀。
var ErrInvalidMethodology = errors.New("方法论注册被拒")

// Register 注册一条方法论，注册期校验全部硬约束。返回错误即拒绝（E2 验收口径：
// 空 CompletionCriterion 的条目无法注册；上限参数缺失无法注册）。
func Register(m Methodology) error {
	if m.ID == "" || m.Name == "" {
		return fmt.Errorf("%w: id/name 必填", ErrInvalidMethodology)
	}
	if m.EvidenceGrade == "" {
		return fmt.Errorf("%w: %s 缺 EvidenceGrade", ErrInvalidMethodology, m.ID)
	}
	switch m.Tier {
	case 1, 2:
		if err := m.Criterion.Validate(); err != nil {
			return fmt.Errorf("%w: %s: %v", ErrInvalidMethodology, m.ID, err)
		}
	case 3:
		if m.Criterion != nil {
			return fmt.Errorf("%w: %s 是隐喻级条目，不得自带完成判据（隐喻不可证伪——允许它自带判据会产生永远无法被评测淘汰的 doctrine）", ErrInvalidMethodology, m.ID)
		}
	default:
		return fmt.Errorf("%w: %s tier 必须是 1/2/3", ErrInvalidMethodology, m.ID)
	}
	if _, dup := registry[m.ID]; dup {
		return fmt.Errorf("%w: %s 重复注册", ErrInvalidMethodology, m.ID)
	}
	cp := m
	registry[m.ID] = &cp
	return nil
}

// Get 按 id 取方法论；未启用也返回（调用方按 Enabled 决定是否可用——前端要能
// 显示「已停用」）。
func Get(id string) (*Methodology, bool) {
	m, ok := registry[id]
	return m, ok
}

// ListEnabled 返回启用的方法论，按 Priority 降序（轮换表盲选的顺序）。
func ListEnabled() []*Methodology {
	var out []*Methodology
	for _, m := range registry {
		if m.Enabled {
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Priority > out[j].Priority })
	return out
}

// ListAll 返回全部注册条目（含停用），同样按 Priority 降序。
func ListAll() []*Methodology {
	var out []*Methodology
	for _, m := range registry {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Priority > out[j].Priority })
	return out
}

// CriterionHash 是判据状态的规范化哈希（熔断③「判据停滞」的判定基础）：
// canonical JSON（map 键排序）→ sha256 前十六位。状态连续 M 轮哈希不变 ⇒ 停滞。
func CriterionHash(state map[string]any) string {
	b, err := json.Marshal(canonicalize(state))
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return fmt.Sprintf("%x", sum[:8])
}

// canonicalize 把 map[string]any 转成键有序的形状（json.Marshal 对 map 键本就
// 按字典序输出，这里只做一层防御性的数值归一：float64 整数转 int64，避免
// 1 与 1.0 的序列化差异造成假停滞/假推进）。
func canonicalize(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, vv := range t {
			out[k] = canonicalize(vv)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, vv := range t {
			out[i] = canonicalize(vv)
		}
		return out
	case float64:
		if t == float64(int64(t)) {
			return int64(t)
		}
		return t
	default:
		return v
	}
}

// builtin 注册全部内置方法论。三级隐喻条目只有 HeuristicTags（原文出处见
// 各条注释——启发式给方向，判据给终止条件，两者来源必须分开）。
func init() {
	must := func(err error) {
		if err != nil {
			panic(fmt.Sprintf("edge registry init: %v", err))
		}
	}
	// ---- 一级：机制与判据都可机器判定 ----
	must(Register(Methodology{
		ID: "stpa-uca", Name: "STPA 不安全控制动作", Tier: 1, EvidenceGrade: GradeStandard,
		Summary: "把目标系统当控制器：枚举控制动作 × 四类 UCA（未给/给了/过早过晚/停止过快过慢），每格至少一条假设且各有结论。",
		Criterion: &CompletionCriterion{Kind: CriterionCoverage, Bounds: []UpperBound{
			{Kind: "target_set", Limit: 42},
		}},
		Enabled: true, Priority: 90,
	}))
	must(Register(Methodology{
		ID: "pre-mortem", Name: "事前验尸", Tier: 1, EvidenceGrade: GradeEmpirical,
		Summary: "假设攻击已成功，反推每个可能原因；每个原因必须被确认或证伪后判据才收敛。",
		Criterion: &CompletionCriterion{Kind: CriterionConvergence, Bounds: []UpperBound{
			{Kind: "cause_set", Limit: 24},
		}},
		Enabled: true, Priority: 80,
	}))
	must(Register(Methodology{
		ID: "info-gain", Name: "信息增益衰减", Tier: 1, EvidenceGrade: GradeEmpirical,
		Summary: "按「这一轮探索改变了多少已知」选题；连续 N 轮增益低于阈值即判据成立（已到收益递减点）。",
		Criterion: &CompletionCriterion{Kind: CriterionDecay, Bounds: []UpperBound{
			{Kind: "decay", Limit: 3},
		}},
		Enabled: true, Priority: 70,
	}))
	must(Register(Methodology{
		ID: "fmea-components", Name: "FMEA 组件级枚举", Tier: 1, EvidenceGrade: GradeStandard,
		Summary: "逐组件枚举失效模式 × 探测/响应控制，矩阵覆盖：每格至少一条假设且各有结论。",
		Criterion: &CompletionCriterion{Kind: CriterionCoverage, Bounds: []UpperBound{
			{Kind: "target_set", Limit: 60},
		}},
		Enabled: true, Priority: 60,
	}))
	must(Register(Methodology{
		ID: "reverse-negative", Name: "逆向与负空间", Tier: 1, EvidenceGrade: GradeStandard,
		Summary: "枚举「系统绝对不该允许的事」的否定清单，每条否定项转成一条待验证假设。",
		Criterion: &CompletionCriterion{Kind: CriterionExhaust, Bounds: []UpperBound{
			{Kind: "asset_set", Limit: 50},
		}},
		Enabled: true, Priority: 50,
	}))
	// ---- 二级：需要改造才可判定 ----
	must(Register(Methodology{
		ID: "cross-domain-analogy", Name: "跨域类比迁移", Tier: 2, EvidenceGrade: GradeEmpirical,
		Summary: "从其他领域（工控/金融/游戏反作弊）迁移攻击模式；每个类比映射必须判定成立或不成立。",
		Criterion: &CompletionCriterion{Kind: CriterionDeplete, Bounds: []UpperBound{
			{Kind: "mapping_set", Limit: 12},
		}},
		Enabled: true, Priority: 40,
	}))
	must(Register(Methodology{
		ID: "attacker-spectrum", Name: "攻击者模型谱系", Tier: 2, EvidenceGrade: GradeStandard,
		Summary: "按攻击者能力谱系（外部无凭据 → 低权 → 高权 → 内部协作方）逐档推演，谱系覆盖。",
		Criterion: &CompletionCriterion{Kind: CriterionCoverage, Bounds: []UpperBound{
			{Kind: "target_set", Limit: 8},
		}},
		Enabled: true, Priority: 30,
	}))
	must(Register(Methodology{
		ID: "diverge-converge", Name: "先发散后收敛", Tier: 2, EvidenceGrade: GradeEmpirical,
		Summary: "编排提示：先无评判发散出候选面，再按判据收敛；两阶段各有终点。",
		Criterion: &CompletionCriterion{Kind: CriterionConvergence, Bounds: []UpperBound{
			{Kind: "cause_set", Limit: 30},
		}},
		Enabled: false, Priority: 20,
	}))
	// ---- 三级：隐喻（只有启发式，判据字段必须为空）----
	must(Register(Methodology{
		ID: "sunzi-virtual-real", Name: "避实击虚", Tier: 3, EvidenceGrade: GradeMetaphor,
		Summary: "「避实而击虚」（《虚实篇》）：检查未设防的信任边界——防守强的地方绕开，找防守语义没覆盖的面。",
		HeuristicTags: []string{"信任边界", "未设防面", "绕开正面"},
		Enabled:       true, Priority: 10,
	}))
	must(Register(Methodology{
		ID: "sunzi-surprise", Name: "攻其无备", Tier: 3, EvidenceGrade: GradeMetaphor,
		Summary: "「攻其无备，出其不意」（《始计篇》）：检查默认配置下的越权路径——安装默认值、示例端点、未改的初始凭据语义。",
		HeuristicTags: []string{"默认配置", "示例端点", "初始化语义"},
		Enabled:       true, Priority: 9,
	}))
	must(Register(Methodology{
		ID: "sunzi-detour", Name: "迂直之计", Tier: 3, EvidenceGrade: GradeMetaphor,
		Summary: "「先知迂直之计者胜」（《军争篇》）：从资产持有者的角度反查目标资产的职责边界——谁的资产、替谁服务、越权面在哪。",
		HeuristicTags: []string{"资产职责", "持有者视角", "边界反查"},
		Enabled:       true, Priority: 8,
	}))
	must(Register(Methodology{
		ID: "sunzi-subdue", Name: "不战屈人", Tier: 3, EvidenceGrade: GradeMetaphor,
		Summary: "「不战而屈人之兵」（《谋攻篇》）：优先尝试让目标自行暴露（配置端点、调试面、健康检查泄密）而不是主动破坏。",
		HeuristicTags: []string{"自行暴露", "调试面", "低接触优先"},
		Enabled:       true, Priority: 7,
	}))
}

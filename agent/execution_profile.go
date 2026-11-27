package agent

// 任务执行档位（0.15.17，外部实现工具包 phase2 的本仓映射）：
//
//	难度 Simple/Medium/Hard —— 决定资源帽（Simple 收紧上下文窗口）；
//	模式 Fast/Balanced/SuperHeavy —— 决定请求级推理强度与（SuperHeavy）熔断预算。
//
// 生效纪律（与 0.15.12 缓存纪律同一条线）：切档【只动请求级参数】——
// ReasoningEffort 走请求级覆盖通道（provider 前缀哈希不含它，切档零缓存代价），
// 压缩窗口帽走每轮现取的 resolver；system 提示词与 tools 数组零改动。
// SuperHeavy 的 HardTokenBudget 只作 AutoScaler 的降档信号（70% 触发降档），
// 不做硬暂停——预算熔断属额度子系统口径，不在这里越权。
//
// 监督角色锁定：档位不改变任何 lane 的 LLM profile 身份（profile 归属由
// agents.llm_profile_id 绑定裁定），因此工具包 LockedSupervisorRoles 的
// 「监督角色不得自选模型」在本仓天然成立，无需新增闸。

import (
	"strings"
	"sync"

	"github.com/Autumn-27/norma/agentcore"
)

// TaskDifficulty 任务难度：auto 由 AssessAutoProfile 判定。
type TaskDifficulty string

const (
	DifficultyAuto   TaskDifficulty = "auto"
	DifficultySimple TaskDifficulty = "simple"
	DifficultyMedium TaskDifficulty = "medium"
	DifficultyHard   TaskDifficulty = "hard"
)

// ExecutionMode 执行模式：auto 由 AssessAutoProfile 判定。
type ExecutionMode string

const (
	ModeAuto          ExecutionMode  = "auto"
	ModeFast          ExecutionMode  = "fast"
	ModeBalanced      ExecutionMode  = "balanced"
	ModeSuperHeavy    ExecutionMode  = "super_heavy"
	DefaultDifficulty TaskDifficulty = DifficultyMedium
	DefaultMode       ExecutionMode  = ModeBalanced
)

// SimpleModeWindowCapK 是 Simple 难度的上下文窗口帽（K tokens）：单目标轻任务
// 不需要 200K 全窗，收紧压缩阈值让上下文更快进入消化区间。
const SimpleModeWindowCapK = 256

// SuperHeavyHardTokenBudget 是 SuperHeavy 模式的熔断预算（tokens）：仅作
// AutoScaler 的降档信号（用量达 70% 触发降档），不是硬暂停。
const SuperHeavyHardTokenBudget = 800_000

// autoScaleCooldownTurns 是自动升降档的冷却滞回（轮）：防止模式抖动。
const autoScaleCooldownTurns = 3

// TaskExecutionProfile 是一个任务的生效档位。
type TaskExecutionProfile struct {
	Difficulty        TaskDifficulty `json:"difficulty"`
	Mode              ExecutionMode  `json:"mode"`
	ReasoningEffort   string         `json:"reasoning_effort"`     // 请求级覆盖；""=不发（跟随 profile 默认）
	ContextWindowCapK int            `json:"context_window_cap_k"` // 压缩窗口帽（K）；0=不设帽
	HardTokenBudget   int64          `json:"hard_token_budget"`    // tokens；0=无（仅 AutoScaler 信号）
}

// effortRank 给推理强度定序（difficulty 下限与 mode 档位取强者）。
func effortRank(e string) int {
	switch e {
	case "low":
		return 1
	case "medium":
		return 2
	case "high", "xhigh", "max":
		return 3
	}
	return 0
}

func strongerEffort(a, b string) string {
	if effortRank(a) >= effortRank(b) {
		return a
	}
	return b
}

// ResolveExecutionProfile 把（难度, 模式）解析成生效档位。非法值回落默认
// （medium/balanced）——档位是运营旋钮，坏输入不该炸任务。
func ResolveExecutionProfile(diff TaskDifficulty, mode ExecutionMode) TaskExecutionProfile {
	switch diff {
	case DifficultySimple, DifficultyMedium, DifficultyHard:
	default:
		diff = DefaultDifficulty
	}
	switch mode {
	case ModeFast, ModeBalanced, ModeSuperHeavy:
	default:
		mode = DefaultMode
	}
	p := TaskExecutionProfile{Difficulty: diff, Mode: mode}
	// 模式 → 推理强度与熔断预算。
	switch mode {
	case ModeFast:
		p.ReasoningEffort = "low"
	case ModeSuperHeavy:
		p.ReasoningEffort = "high"
		p.HardTokenBudget = SuperHeavyHardTokenBudget
	default:
		p.ReasoningEffort = "medium"
	}
	// 难度 → 资源帽与强度下限。
	switch diff {
	case DifficultySimple:
		p.ContextWindowCapK = SimpleModeWindowCapK
	case DifficultyHard:
		p.ReasoningEffort = strongerEffort(p.ReasoningEffort, "medium")
	}
	return p
}

// AssessAutoProfile 在难度/模式为 auto（或空）时按任务书文本自动判定。
// 目标数/资产图规模由调用方供给（创建时点通常为 0——文本启发式兜底）。
// 判据是保守的文本启发式：宁去中间档，不激进升降。
func AssessAutoProfile(goal, description string, targetCount, assetGraphNodes int, diff TaskDifficulty, mode ExecutionMode) (TaskDifficulty, ExecutionMode) {
	text := strings.ToLower(goal + " " + description)
	if diff == "" || diff == DifficultyAuto {
		switch {
		case targetCount <= 1 && assetGraphNodes < 10 &&
			(strings.Contains(text, "quick") || strings.Contains(text, "check") || strings.Contains(text, "single") ||
				strings.Contains(text, "单个") || strings.Contains(text, "快速")):
			diff = DifficultySimple
		case targetCount >= 4 || assetGraphNodes >= 50 ||
			strings.Contains(text, "full audit") || strings.Contains(text, "payment") || strings.Contains(text, "auth") ||
			strings.Contains(text, "全面") || strings.Contains(text, "支付") || strings.Contains(text, "鉴权"):
			diff = DifficultyHard
		default:
			diff = DefaultDifficulty
		}
	}
	if mode == "" || mode == ModeAuto {
		switch {
		case strings.Contains(text, "quick") || strings.Contains(text, "fast") || strings.Contains(text, "passive") ||
			strings.Contains(text, "快速") || strings.Contains(text, "被动"):
			mode = ModeFast
		case strings.Contains(text, "deep") || strings.Contains(text, "thorough") || strings.Contains(text, "full audit") ||
			strings.Contains(text, "深度") || strings.Contains(text, "彻底"):
			mode = ModeSuperHeavy
		default:
			mode = DefaultMode
		}
	}
	return diff, mode
}

// ---------- 进程内注册表（切档即时生效；跨重启经 server 侧 settings KV 水合） ----------

var profileRegistry = struct {
	mu sync.RWMutex
	m  map[int64]TaskExecutionProfile
}{m: map[int64]TaskExecutionProfile{}}

// SetExecutionProfile 覆写一个任务的档位（PATCH 面与 AutoScaler 共用）。
func SetExecutionProfile(taskID int64, p TaskExecutionProfile) {
	profileRegistry.mu.Lock()
	defer profileRegistry.mu.Unlock()
	profileRegistry.m[taskID] = p
}

// ExecutionProfileFor 读取任务档位（miss = 未切过档）。
func ExecutionProfileFor(taskID int64) (TaskExecutionProfile, bool) {
	profileRegistry.mu.RLock()
	defer profileRegistry.mu.RUnlock()
	p, ok := profileRegistry.m[taskID]
	return p, ok
}

// DeleteExecutionProfile 清掉任务档位（任务删除时的伴生清理；miss 幂等）。
func DeleteExecutionProfile(taskID int64) {
	profileRegistry.mu.Lock()
	defer profileRegistry.mu.Unlock()
	delete(profileRegistry.m, taskID)
}

// ApplyExecutionProfileEffort 把档位的推理强度作为【请求级覆盖】塞进 session
// Options——仅当调用方未预设时生效；DB 级 agents.thinking_effort 覆盖（
// applyThinkingOverride）在其之后调用、非空才覆盖 ⇒ 优先级：DB 覆盖 > 档位 >
// profile 默认。ReasoningEffort 是请求顶层参数，不进前缀哈希 ⇒ 切档零缓存代价。
func ApplyExecutionProfileEffort(opts *agentcore.Options, taskID int64) {
	if opts == nil {
		return
	}
	p, ok := ExecutionProfileFor(taskID)
	if !ok || p.ReasoningEffort == "" || opts.ReasoningEffort != "" {
		return
	}
	opts.ReasoningEffort = p.ReasoningEffort
}

// ClampCompactionWindow 用档位帽收紧压缩窗口（每轮现取；Simple 256K 帽）。
// 帽低于压缩下限（minWindowK）时不动——帽是收紧旋钮，不该把窗口压进非法区。
func ClampCompactionWindow(taskID int64, base int) int {
	p, ok := ExecutionProfileFor(taskID)
	if !ok || p.ContextWindowCapK <= 0 || p.ContextWindowCapK < minWindowK {
		return base
	}
	if base <= p.ContextWindowCapK {
		return base
	}
	return p.ContextWindowCapK
}

// ---------- AutoScaler：双向自动升降档（冷却滞回；只动请求级参数） ----------

// TurnSignals 是一轮评估的确定性信号（调用方从任务态采集）。
type TurnSignals struct {
	TurnIndex             int
	TokensUsed            int64
	ConsecutiveFailures   int
	ConsecutiveNoNewAsset int
	CriticalSignalFound   bool // 高危发现（auth/payment/RCE/SSRF 等）中途出现
}

// AutoScaleDecision 是一次评估的结论；Notice 供 UI/轨迹展示（不进 system）。
type AutoScaleDecision struct {
	Changed      bool
	PreviousMode ExecutionMode
	NewMode      ExecutionMode
	Notice       string
	NewProfile   TaskExecutionProfile
}

// AutoScaler 按轮信号自动升降档：升档看「遇到硬仗」（高危信号/连续失败），
// 降档看「预算吃紧/边际收益归零」。冷却滞回防抖；切档只改请求级参数。
type AutoScaler struct {
	mu             sync.Mutex
	cooldownTurns  int
	lastSwitchTurn int
}

func NewAutoScaler(cooldownTurns int) *AutoScaler {
	if cooldownTurns <= 0 {
		cooldownTurns = autoScaleCooldownTurns
	}
	return &AutoScaler{cooldownTurns: cooldownTurns, lastSwitchTurn: -100}
}

func (s *AutoScaler) EvaluateTurn(current TaskExecutionProfile, sig TurnSignals) AutoScaleDecision {
	s.mu.Lock()
	defer s.mu.Unlock()
	noChange := AutoScaleDecision{PreviousMode: current.Mode, NewMode: current.Mode, NewProfile: current}
	if sig.TurnIndex-s.lastSwitchTurn < s.cooldownTurns {
		return noChange
	}
	next := current.Mode
	// 升档：遇到硬仗（高危信号/连续失败）。
	if current.Mode == ModeFast && (sig.CriticalSignalFound || sig.ConsecutiveFailures >= 2) {
		next = ModeBalanced
	} else if current.Mode == ModeBalanced && (sig.CriticalSignalFound || sig.ConsecutiveFailures >= 3) {
		next = ModeSuperHeavy
	}
	// 降档：预算吃紧（达熔断预算 70%）或边际收益归零且无硬仗。
	budgetPressure := current.HardTokenBudget > 0 && float64(sig.TokensUsed) >= 0.70*float64(current.HardTokenBudget)
	diminishing := sig.ConsecutiveNoNewAsset >= 3 && !sig.CriticalSignalFound
	if current.Mode == ModeSuperHeavy && (budgetPressure || diminishing) {
		next = ModeBalanced
	} else if current.Mode == ModeBalanced && (budgetPressure || diminishing) {
		next = ModeFast
	}
	if next == current.Mode {
		return noChange
	}
	s.lastSwitchTurn = sig.TurnIndex
	updated := ResolveExecutionProfile(current.Difficulty, next)
	modeName := map[ExecutionMode]string{ModeFast: "轻快", ModeBalanced: "均衡", ModeSuperHeavy: "重火力"}
	return AutoScaleDecision{
		Changed:      true,
		PreviousMode: current.Mode,
		NewMode:      next,
		Notice: "[自动缩放] 执行模式 " + modeName[current.Mode] + " → " + modeName[next] +
			"（只调请求级推理强度与资源帽，模型上下文与工具面不变）",
		NewProfile: updated,
	}
}

// ---------- C-02A 任务难度 → 资源画像 ----------
//
// 上面的 TaskExecutionProfile / ResolveExecutionProfile 是 0.15.17 的请求级档位
// （推理强度、压缩窗口、AutoScaler 熔断信号），输入是执行档 auto/simple/medium/hard。
// 下面这张表是另一件事：把 C-01 的任务难度 easy/medium/hard/expert 翻译成
// 预算、并发、planner 轮次和复测范围。两张表共用 ModeFast / ModeBalanced /
// ModeSuperHeavy 这三个字符串，但不共用解析函数，也不写 profileRegistry。
//
// 本批只提供纯函数。tasks.execution_mode 列、PATCH、planner/worker/复测接线都还没做：
// 现有轮次与 token 没有可对齐的绝对常量（见各字段注释），硬接到派发上会改掉
// 「maxTurns=0 不限」的现行行为。

// 复测范围。oracle 未落地（E-02）时 RequireOracle 只是标注，不代替复测。
const (
	RetestScopeKeyOnly  = "key_only"
	RetestScopeHighPlus = "high_plus"
	RetestScopeAll      = "all"
)

// ExecutionProfile 是一张确定性资源画像。
// TokenBudget 与 PlannerRounds 用的是卡面相对档位，不是 token 数或已生效的轮次上限。
type ExecutionProfile struct {
	Mode              ExecutionMode
	WorkerConcurrency int
	TokenBudget       int64
	PlannerRounds     int
	RetestScope       string
	RequireOracle     bool
}

// DefaultExecutionMode 把任务难度映射成默认资源模式。
// 空值、空白和执行档的 auto/simple 都不是任务难度，回落 balanced。
// 大小写敏感，与 ValidDifficulty 同一口径。
// 不能叫 DefaultMode：那个名字已经被 0.15.17 的常量占用（恒为 balanced）。
func DefaultExecutionMode(level string) ExecutionMode {
	switch strings.TrimSpace(level) {
	case LevelEasy:
		return ModeFast
	case LevelMedium, LevelHard:
		return ModeBalanced
	case LevelExpert:
		return ModeSuperHeavy
	default:
		return ModeBalanced
	}
}

// ValidResourceMode 报告 s 是否为可手动覆盖的资源模式。
// 空串不是覆盖（空 = 跟随难度）。ModeAuto 是 0.15.17 的执行档，不是本表的覆盖值。
func ValidResourceMode(s string) bool {
	switch ExecutionMode(strings.TrimSpace(s)) {
	case ModeFast, ModeBalanced, ModeSuperHeavy:
		return true
	}
	return false
}

// ResolveProfile 按「手动模式优先，空则跟难度」解析资源画像。
// 纯函数：不读库、不写 profileRegistry、不调 LLM。
// executionMode 去空白后非空但不在白名单 → 回落 balanced 画像（与卡面 switch 的默认支一致）。
// 这种值不能当覆盖落库；拒绝写入是后续 API 批的事，本函数不产生错误。
func ResolveProfile(difficulty, executionMode string) ExecutionProfile {
	mode := ExecutionMode(strings.TrimSpace(executionMode))
	if mode == "" {
		mode = DefaultExecutionMode(difficulty)
	}
	return resourceProfileFor(mode)
}

// resourceProfileFor 是映射表本身。改数只改这里，解析顺序在 ResolveProfile。
//
// TokenBudget 1/2/3、PlannerRounds 3/6/10 是相对档位，不是绝对 token / 轮次：
// planner 的 MaxTurns 来自 agent 管理配置（0 = 不限，planner.go），没有 3/6/10 这组常量；
// 唯一的 token 常量 SuperHeavyHardTokenBudget（800_000）是 0.15.17 AutoScaler 的降档信号，
// 不是按难度分的每轮预算。绝对数留给 G-06 / H-05，决策 #7 之前不在这里换算。
func resourceProfileFor(mode ExecutionMode) ExecutionProfile {
	switch mode {
	case ModeFast:
		// 并发卡面给的是 1–2，取上限 2。
		return ExecutionProfile{
			Mode:              ModeFast,
			WorkerConcurrency: 2,
			TokenBudget:       1,
			PlannerRounds:     3,
			RetestScope:       RetestScopeKeyOnly,
			RequireOracle:     false,
		}
	case ModeBalanced:
		return ExecutionProfile{
			Mode:              ModeBalanced,
			WorkerConcurrency: 3,
			TokenBudget:       2,
			PlannerRounds:     6,
			RetestScope:       RetestScopeHighPlus,
			RequireOracle:     false,
		}
	case ModeSuperHeavy:
		return ExecutionProfile{
			Mode:              ModeSuperHeavy,
			WorkerConcurrency: 5,
			TokenBudget:       3,
			PlannerRounds:     10,
			RetestScope:       RetestScopeAll,
			RequireOracle:     true,
		}
	default:
		return ExecutionProfile{
			Mode:              ModeBalanced,
			WorkerConcurrency: 3,
			TokenBudget:       2,
			PlannerRounds:     6,
			RetestScope:       RetestScopeHighPlus,
			RequireOracle:     false,
		}
	}
}

// ScaledPlannerTurns 把 agent 管理配置里的 MaxTurns 按资源画像的相对轮次缩放。
// configured <= 0 表示不限，原样返回（0 仍是不限）。
// 以 balanced 的 6 轮为 1 倍：fast 乘 3/6，super_heavy 乘 10/6。四舍五入，至少 1。
// TokenBudget 不参与换算，1/2/3 不是 token 数。
func ScaledPlannerTurns(configured, plannerRounds int) int {
	if configured <= 0 {
		return configured
	}
	if plannerRounds <= 0 {
		plannerRounds = 6
	}
	scaled := (configured*plannerRounds + 3) / 6
	if scaled < 1 {
		return 1
	}
	return scaled
}

// RetestSuggested 按复测范围判断这个严重度算不算「建议复测」。
// key_only 只建议 critical；high_plus 建议 high 与 critical；all 建议四档。
// 这只是建议标记，不自动派发，也不因为 RequireOracle 拒绝人工复测。
func RetestSuggested(scope, severity string) bool {
	switch strings.ToLower(strings.TrimSpace(severity)) {
	case "critical":
		return scope == RetestScopeKeyOnly || scope == RetestScopeHighPlus || scope == RetestScopeAll
	case "high":
		return scope == RetestScopeHighPlus || scope == RetestScopeAll
	case "medium", "low":
		return scope == RetestScopeAll
	default:
		return false
	}
}

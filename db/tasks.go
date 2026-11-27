package db

import (
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Task is a row in the task registry (1:1 with an exploration).
type Task struct {
	ID            int64      `json:"id"`
	Name          string     `json:"name"` // 可选任务名称;空=未命名
	CategoryID    *int64     `json:"category_id,omitempty"`
	CategoryName  string     `json:"category_name,omitempty"`
	Pinned        bool       `json:"pinned"`
	PinnedAt      *time.Time `json:"pinned_at,omitempty"`
	Description   string     `json:"description"`
	Goal          string     `json:"goal"`
	ExplorationID int64      `json:"exploration_id"`
	Status        string     `json:"status"`
	Paused        bool       `json:"paused"`
	Queued        bool       `json:"queued"`
	QueuedAt      *time.Time `json:"queued_at,omitempty"`
	QueueMode     string     `json:"queue_mode,omitempty"`
	LLMProfileID  *int64     `json:"llm_profile_id,omitempty"`
	// Task-level ordered LLM chain. LLMProfileID remains the compatibility alias
	// for ActiveLLMProfileID while older API clients still send one profile id.
	LLMProfileIDs      []int64    `json:"llm_profile_ids,omitempty"`
	ActiveLLMProfileID *int64     `json:"active_llm_profile_id,omitempty"`
	LLMChainRevision   int64      `json:"-"`
	LLMFailoverState   string     `json:"llm_failover_state,omitempty"`
	LLMFailoverReason  string     `json:"llm_failover_reason,omitempty"`
	SourceTaskIDs      []int64    `json:"source_task_ids,omitempty"`
	CompanyIDs         []int64    `json:"company_ids,omitempty"`
	ParentRef          string     `json:"parent_ref,omitempty"` // 父任务 id(编排 spawn 记录;空=顶层)
	CreatedAt          time.Time  `json:"created_at"`
	CompletedAt        *time.Time `json:"completed_at,omitempty"` // 进入终态(done/failed/timeout)的时刻;非终态为 nil
	// 任务级超时(见 docs/任务级超时与收尾设计.md)。
	TimeoutSeconds int        `json:"timeout_seconds"`        // 0=不限时
	FirstRunAt     *time.Time `json:"first_run_at,omitempty"` // 首次真正开始运行的时刻(非 created_at);nil=尚未运行
	DeadlineAt     *time.Time `json:"deadline_at,omitempty"`  // = first_run_at + timeout_seconds;nil=不限或未运行
	// planner 心跳触发间隔(秒)：距上轮 plan 结束/任务开始满该值且期间无触发 → 触发一轮。
	// 下限=默认=300(5min)，低于一律抬到 300(在 CreateTask 归一)。见 docs/planner-trigger-impl-plan.md
	PlanHeartbeatSeconds int `json:"plan_heartbeat_seconds"`
	// CoverageEnabled 是「资产覆盖度功能」总开关(默认 true)。false 时：不计算/不展示测试
	// 覆盖度、不自动累积 task_scope(source=auto)、不给 agent 开放 add_task_scope/
	// list_untested_assets、态势里不注入 coverage 块(scope 字段仍保留)。company 关联
	// (task_scope kind=company)与此开关无关，永不受影响。见 db/task_scope.go。
	CoverageEnabled bool `json:"coverage_enabled"`
	// ScopeConfigured 是「seed 是否给任务配好了 scope」(方案 v3 缺陷 H1)。默认 true;
	// seed 解析不出目标且任务无 company scope 时置 false(任务详情横幅数据源)。
	// 后续任何 scope 写入(task_scope 行 / 任务资产挂靠)自动翻回 true。
	ScopeConfigured bool `json:"scope_configured"`
	// Difficulty 是任务难度（C-01）：easy / medium / hard / expert。
	// 不是 0.15.17 执行档位的 difficulty（auto/simple/medium/hard，只调算力、
	// 存在进程内档位里）。两套同名不同义，本列只存四值白名单。
	// 创建时不传落默认 medium。改本列不回写已存在的意图节点。
	Difficulty string `json:"difficulty"`
	// DifficultyReason 是一句话理由。空串合法（默认）。
	DifficultyReason string `json:"difficulty_reason"`
	// ExecutionMode 是资源模式覆盖（C-02B）：fast / balanced / super_heavy。
	// 空串 = 跟随任务难度的默认模式，不在创建时把默认模式写进列里——
	// 难度以后改了，空列仍然跟新难度走。不是 0.15.17 执行档的
	// auto/simple/medium/hard。非法值拒绝写入，不静默改成 balanced。
	ExecutionMode string `json:"execution_mode"`
	// Grade 是终态评级（C-03）：S / A / B / C。空串 = 还没评。
	// 终态结算写一次，不随后来的 finding 改动变化；重算只走显式重新评级。
	// 不是执行档，也不是任务难度。
	Grade        string `json:"grade"`
	GradeSummary string `json:"grade_summary"`
	// GradeScore 未评时为 nil（列是 NULL）。已评时是公式分，可以是 0，所以不能用 0 当未评。
	GradeScore *float64 `json:"grade_score,omitempty"`
}

// TaskDeleteResult reports optional related-data cleanup performed in the same
// transaction as the task/exploration delete.
type TaskDeleteResult struct {
	AssetsDeleted     int64
	AssetsDetached    int64
	FindingsDeleted   int64
	LLMRecordsDeleted int64
}

// TaskDeletePreparation is produced inside the PostgreSQL deletion transaction
// after asset and anchor writers have been excluded. Prepare callbacks may use
// TrafficHosts to stage an external traffic deletion before PostgreSQL commits.
type TaskDeletePreparation struct {
	ExplorationID int64
	TrafficHosts  []string
}

// IsTerminal reports whether a task status is a terminal (finished) state.
// 单一真源，替换散落各处的 done/failed 硬编码判定。
func IsTerminal(status string) bool {
	return status == "done" || status == "failed" || status == "timeout"
}

// CreateTask creates an exploration + task in one transaction and returns the task.
// timeoutSeconds is the task-level wall-clock budget (0 = 不限时); deadline_at is
// stamped later at first real run (see engine), not here.
// MinPlanHeartbeatSeconds 是 planner 心跳间隔的下限 = 默认 = 10min。
// 低于它(含缺省 0 / 负值 / 误配的小值)一律抬到 10min，防止把 planner 打爆。
const MinPlanHeartbeatSeconds = 600

// MaxTaskSourceCount bounds the amount of live inherited context one task can
// pull into every planner/main-agent prompt. Inheritance is intentionally direct
// only; keeping the fan-in bounded also prevents a single create request from
// multiplying graph and asset-context queries without limit.
const MaxTaskSourceCount = 8

// MaxTaskCompanyCount bounds the number of company asset scopes attached to a
// task. Company scopes are prompt context, and their currently attributed
// assets are snapshotted into the task at creation time.
const MaxTaskCompanyCount = 32

const taskCompanyAssetSource = "company"

var (
	ErrTaskCompanyIDsInvalid = errors.New("invalid task company ids")
	ErrTaskCompanyNotFound   = errors.New("task company not found")
	// ErrTaskDifficultyInvalid 表示难度不在四值白名单。非法值拒绝写入，
	// 不静默回落 medium。HTTP 400 映射留给 C-01B。
	ErrTaskDifficultyInvalid = errors.New("task difficulty must be easy, medium, hard, or expert")
	// ErrTaskExecutionModeInvalid 表示资源模式不在白名单。空串合法（跟随难度）。
	// 非法值拒绝写入，不静默改成 balanced。HTTP 400 留给 C-02C。
	ErrTaskExecutionModeInvalid = errors.New("task execution_mode must be empty, fast, balanced, or super_heavy")
	// ErrTaskGradeNotTerminal 表示任务还不是终态，不能写入评级。
	ErrTaskGradeNotTerminal = errors.New("只有终态任务可以评级")
	// ErrTaskGradeInvalid 表示等级不在 S/A/B/C。空串不是等级，清除走 ClearTaskGrade。
	ErrTaskGradeInvalid = errors.New("task grade must be S, A, B, or C")
)

// 任务难度四值（C-01）。与执行档位的 simple/auto 不是同一套枚举。
const (
	TaskDifficultyEasy   = "easy"
	TaskDifficultyMedium = "medium"
	TaskDifficultyHard   = "hard"
	TaskDifficultyExpert = "expert"
	// 资源模式三值（C-02B）。必须与 agent.ModeFast / ModeBalanced / ModeSuperHeavy
	// 的字符串一致。db 不能 import agent（agent 已经 import db）。
	// 空串不在这三个常量里：空是「未覆盖」，由 ValidTaskExecutionMode 单独接受。
	TaskExecutionModeFast       = "fast"
	TaskExecutionModeBalanced   = "balanced"
	TaskExecutionModeSuperHeavy = "super_heavy"
	// 任务评级四值（C-03）。空串不在这里：空是「还没评」。
	// 与 agent.GradeS/A/B/C 的字符串一致。db 不能 import agent。
	TaskGradeS = "S"
	TaskGradeA = "A"
	TaskGradeB = "B"
	TaskGradeC = "C"
)

//go:embed schema_task_difficulty.sql
var taskDifficultySchemaSQL string

//go:embed schema_task_execution_mode.sql
var taskExecutionModeSchemaSQL string

//go:embed schema_task_grade.sql
var taskGradeSchemaSQL string

// taskDifficultySchemaReady 按连接池标记 DDL 已就位（markRuntimeSchemasReady 同键）。
var taskDifficultySchemaReady sync.Map

// taskExecutionModeSchemaReady 同上，键是运行期连接池指针。
var taskExecutionModeSchemaReady sync.Map

// taskGradeSchemaReady 同上，键是运行期连接池指针。
var taskGradeSchemaReady sync.Map

// EnsureTaskDifficultySchema 幂等补上 tasks.difficulty / difficulty_reason。
// 启动期用迁移连接调用；运行期连接被 markRuntimeSchemasReady 标成就位后不再执行 DDL。
func (d *DB) EnsureTaskDifficultySchema() error {
	if d == nil || d.DB == nil {
		return errors.New("数据库未装配")
	}
	if _, ok := taskDifficultySchemaReady.Load(d.DB); ok {
		return nil
	}
	if _, err := d.Exec(taskDifficultySchemaSQL); err != nil {
		return fmt.Errorf("应用任务难度 schema 失败: %w", err)
	}
	taskDifficultySchemaReady.Store(d.DB, struct{}{})
	return nil
}

// EnsureTaskExecutionModeSchema 幂等补上 tasks.execution_mode。
// 启动期用迁移连接调用；运行期连接被 markRuntimeSchemasReady 标成就位后不再执行 DDL。
func (d *DB) EnsureTaskExecutionModeSchema() error {
	if d == nil || d.DB == nil {
		return errors.New("数据库未装配")
	}
	if _, ok := taskExecutionModeSchemaReady.Load(d.DB); ok {
		return nil
	}
	if _, err := d.Exec(taskExecutionModeSchemaSQL); err != nil {
		return fmt.Errorf("应用任务执行模式 schema 失败: %w", err)
	}
	taskExecutionModeSchemaReady.Store(d.DB, struct{}{})
	return nil
}

// EnsureTaskGradeSchema 幂等补上 tasks.grade / grade_summary / grade_score。
// 启动期用迁移连接调用；运行期连接被 markRuntimeSchemasReady 标成就位后不再执行 DDL。
func (d *DB) EnsureTaskGradeSchema() error {
	if d == nil || d.DB == nil {
		return errors.New("数据库未装配")
	}
	if _, ok := taskGradeSchemaReady.Load(d.DB); ok {
		return nil
	}
	if _, err := d.Exec(taskGradeSchemaSQL); err != nil {
		return fmt.Errorf("应用任务评级 schema 失败: %w", err)
	}
	taskGradeSchemaReady.Store(d.DB, struct{}{})
	return nil
}

// ValidTaskGrade 报告 s 是否为 S/A/B/C。空串不是等级。大小写敏感。
func ValidTaskGrade(s string) bool {
	switch s {
	case TaskGradeS, TaskGradeA, TaskGradeB, TaskGradeC:
		return true
	}
	return false
}

// ValidTaskDifficulty 报告 s 是否为四值白名单（大小写敏感，不接受执行档位的 auto/simple）。
func ValidTaskDifficulty(s string) bool {
	switch s {
	case TaskDifficultyEasy, TaskDifficultyMedium, TaskDifficultyHard, TaskDifficultyExpert:
		return true
	}
	return false
}

// resolveCreateDifficulty 把创建选项收成落库值。nil = 不传 = 默认 medium。
func resolveCreateDifficulty(p *string) (string, error) {
	if p == nil {
		return TaskDifficultyMedium, nil
	}
	v := strings.TrimSpace(*p)
	if !ValidTaskDifficulty(v) {
		return "", fmt.Errorf("%w: %q", ErrTaskDifficultyInvalid, v)
	}
	return v, nil
}

// ValidTaskExecutionMode 报告 s 是否可落库。空串合法（跟随难度）。
// 三值与 agent.ValidResourceMode 相同，且大小写敏感。
// auto/simple/medium/hard 是 0.15.17 执行档，不是本列的值。
// 不 TrimSpace——调用方先规范化。
func ValidTaskExecutionMode(s string) bool {
	switch s {
	case "", TaskExecutionModeFast, TaskExecutionModeBalanced, TaskExecutionModeSuperHeavy:
		return true
	}
	return false
}

// normalizeTaskExecutionMode 把写入值收成落库值。空白折成空串（跟随难度），
// 不是改成某一档。其余非法值拒绝。
func normalizeTaskExecutionMode(raw string) (string, error) {
	v := strings.TrimSpace(raw)
	if !ValidTaskExecutionMode(v) {
		return "", fmt.Errorf("%w: %q", ErrTaskExecutionModeInvalid, v)
	}
	return v, nil
}

// NormalizeTaskCompanyIDs validates IDs and removes duplicates while retaining
// the user's first-seen order.
func NormalizeTaskCompanyIDs(ids []int64) ([]int64, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	seen := make(map[int64]struct{}, min(len(ids), MaxTaskCompanyCount))
	normalized := make([]int64, 0, min(len(ids), MaxTaskCompanyCount))
	for _, id := range ids {
		if id <= 0 {
			return nil, fmt.Errorf("%w: company id must be positive", ErrTaskCompanyIDsInvalid)
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		normalized = append(normalized, id)
		if len(normalized) > MaxTaskCompanyCount {
			return nil, fmt.Errorf("%w: got more than %d unique companies", ErrTaskCompanyIDsInvalid, MaxTaskCompanyCount)
		}
	}
	return normalized, nil
}

func normalizeHeartbeat(sec int) int {
	if sec < MinPlanHeartbeatSeconds {
		return MinPlanHeartbeatSeconds
	}
	return sec
}

func (d *DB) CreateTask(description, goal string, llmProfileID *int64, timeoutSeconds, planHeartbeatSeconds int) (*Task, error) {
	var ids []int64
	if llmProfileID != nil {
		ids = []int64{*llmProfileID}
	}
	return d.CreateTaskWithOptions(description, goal, TaskCreateOptions{
		LLMProfileIDs: ids, TimeoutSeconds: timeoutSeconds, PlanHeartbeatSeconds: planHeartbeatSeconds,
	})
}

// TaskCreateOptions contains the task data that must be committed atomically
// with the task/exploration row.
type TaskCreateOptions struct {
	Name                 string // 可选任务名称;空=未命名
	CategoryID           *int64
	SourceTaskIDs        []int64
	CompanyIDs           []int64
	LLMProfileIDs        []int64
	TimeoutSeconds       int
	PlanHeartbeatSeconds int
	// CoverageEnabled 是「资产覆盖度功能」开关;nil=默认开(true)，让不关心该开关的创建
	// 路径(编排 spawn、老 API)沿用原行为。仅 web 创建任务时可显式传 false 关闭。
	CoverageEnabled *bool
	// InterceptRules 是任务级资产拦截规则,创建时随任务在同一事务内写入 task_intercept_rules。
	InterceptRules []TaskInterceptRuleInput
	// Difficulty 是任务难度。nil = 默认 medium；非 nil 必须过四值白名单
	// （空白与非法值都拒绝，不静默回落）。不是执行档位的 difficulty。
	Difficulty *string
	// DifficultyReason 是一句话理由。写入前 TrimSpace；空串合法。
	DifficultyReason string
	// ExecutionMode 是资源模式覆盖。空串 = 跟随难度；非空必须是
	// fast/balanced/super_heavy。写入前 TrimSpace。纯空白折成空串，
	// 不是改成某一档。非法值拒绝，不静默改成 balanced。
	ExecutionMode string
}

// SetToolTableFallback 注册「工具表不可读但仍有上一份可用快照」的判定。
//
// 由 server 装配时调用（见 server/assembly.go 的 wireTools）。刻意做成 per-DB
// 而不是包级钩子：包级钩子捕获的是某个 DB 指针，而测试里前一个用例关库后，
// 钩子就会指向已关闭的库 —— 后续用例的建任务会被无端拒绝。
func (d *DB) SetToolTableFallback(f func() bool) { d.toolTableFallback = f }

// toolTableUnavailable 报「工具表不可读且无快照」。
//
// 工具表可读 → 工具白名单已知，放行；不可读但有上一份可用快照 → 仍可按快照
// 划边界，放行；两者都没有 → 拒绝：白名单未知时开工等于无边界放行。
//
// 工具表始终按**当前这个库**读，不借用别的 DB 的结果。
func (d *DB) toolTableUnavailable() error {
	read := d.listTools
	if read == nil {
		read = d.ListTools
	}
	return toolTableUnavailable(read, d.toolTableFallback)
}

// toolTableUnavailable 是判定的纯逻辑部分：readTools 报「工具表能否读」，
// fallback 报「是否还有上一份可用快照」。拆出来是为了能脱离数据库单测这条闸。
func toolTableUnavailable(readTools func() ([]*Tool, error), fallback func() bool) error {
	if _, err := readTools(); err == nil {
		return nil
	}
	if fallback != nil && fallback() {
		return nil
	}
	return errors.New("工具表不可读且无可用快照，拒绝创建新任务：工具白名单未知，此时开工等于无边界放行")
}

// CreateTaskWithOptions creates an exploration, task, direct source relations,
// and the ordered task LLM chain in one transaction.
func (d *DB) CreateTaskWithOptions(description, goal string, opts TaskCreateOptions) (*Task, error) {
	// 先做纯参数校验（不碰数据库），再做需要读库的闸门，最后才开事务 ——
	// 顺序有讲究：参数错误应当以参数错误的形式返回，而不是被闸门的错误盖住。
	if len(opts.SourceTaskIDs) > MaxTaskSourceCount {
		return nil, fmt.Errorf("too many source tasks: got %d, maximum is %d", len(opts.SourceTaskIDs), MaxTaskSourceCount)
	}
	companyIDs, err := NormalizeTaskCompanyIDs(opts.CompanyIDs)
	if err != nil {
		return nil, err
	}
	opts.CompanyIDs = companyIDs
	difficulty, err := resolveCreateDifficulty(opts.Difficulty)
	if err != nil {
		return nil, err
	}
	difficultyReason := strings.TrimSpace(opts.DifficultyReason)
	executionMode, err := normalizeTaskExecutionMode(opts.ExecutionMode)
	if err != nil {
		return nil, err
	}
	// 工具表不可读且无快照 → 拒绝创建新 run。
	//
	// 降级放行只是「少给几个工具」，但冷启动那一次恰恰是无人值守的时刻：
	// 工具白名单未知，贸然开工等于无边界放行。此时宁可建不了任务。
	//
	// 闸门放在事务之前 —— 拒绝创建不该留下半截的 exploration 行。
	if err := d.toolTableUnavailable(); err != nil {
		return nil, err
	}
	if err := d.EnsureTaskDifficultySchema(); err != nil {
		return nil, err
	}
	if err := d.EnsureTaskExecutionModeSchema(); err != nil {
		return nil, err
	}
	if err := d.EnsureTaskGradeSchema(); err != nil {
		return nil, err
	}
	tx, err := d.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var expID int64
	if err := tx.QueryRow(`INSERT INTO explorations(description, goal) VALUES ($1,$2) RETURNING id`, description, goal).Scan(&expID); err != nil {
		return nil, err
	}
	// origin fact: the exploration graph's root, a KindFact node (state='origin')
	// holding the original task description. Goals/intents/findings descend from
	// it, and the seeded target asset is anchored to it as lineage (the asset graph
	// is global and shared, not isolated per task). Being a fact (not a special 'begin' kind) lets every
	// intent uniformly connect to a fact node, including the first ones.
	originPayload, _ := json.Marshal(map[string]any{
		"summary":     "任务起点：" + description + "；目标：" + goal,
		"description": description,
		"goal":        goal,
	})
	if _, err := tx.Exec(`
INSERT INTO exploration_nodes(exploration_id, kind, payload, priority, state, origin)
VALUES ($1, 'fact', $2, 0, 'origin', 'system')`, expID, string(originPayload)); err != nil {
		return nil, err
	}
	if opts.TimeoutSeconds < 0 {
		opts.TimeoutSeconds = 0
	}
	opts.PlanHeartbeatSeconds = normalizeHeartbeat(opts.PlanHeartbeatSeconds)
	coverageEnabled := opts.CoverageEnabled == nil || *opts.CoverageEnabled
	var categoryName string
	if opts.CategoryID != nil {
		if *opts.CategoryID <= 0 {
			return nil, fmt.Errorf("%w: category id must be positive", ErrTaskCategoryInvalid)
		}
		if err := tx.QueryRow(`SELECT name FROM task_categories WHERE id=$1`, *opts.CategoryID).Scan(&categoryName); err != nil {
			if err == sql.ErrNoRows {
				return nil, ErrTaskCategoryNotFound
			}
			return nil, err
		}
	}
	var active *int64
	if len(opts.LLMProfileIDs) > 0 {
		id := opts.LLMProfileIDs[0]
		active = &id
	}
	t := &Task{
		Name: opts.Name, CategoryID: opts.CategoryID, CategoryName: categoryName,
		Description: description, Goal: goal, ExplorationID: expID,
		LLMProfileID: active, ActiveLLMProfileID: active,
		LLMProfileIDs:  append([]int64(nil), opts.LLMProfileIDs...),
		SourceTaskIDs:  append([]int64(nil), opts.SourceTaskIDs...),
		CompanyIDs:     append([]int64(nil), opts.CompanyIDs...),
		TimeoutSeconds: opts.TimeoutSeconds, PlanHeartbeatSeconds: opts.PlanHeartbeatSeconds,
		CoverageEnabled: coverageEnabled,
		Difficulty:      difficulty, DifficultyReason: difficultyReason,
		ExecutionMode: executionMode,
	}
	if err := tx.QueryRow(`
INSERT INTO tasks(name, category_id, description, goal, exploration_id, llm_profile_id, active_llm_profile_id, timeout_seconds, plan_heartbeat_seconds, coverage_enabled, difficulty, difficulty_reason, execution_mode)
VALUES ($1,$2,$3,$4,$5,$6,$6,$7,$8,$9,$10,$11,$12)
RETURNING id, status, paused, created_at`, opts.Name, opts.CategoryID, description, goal, expID, active, opts.TimeoutSeconds, opts.PlanHeartbeatSeconds, coverageEnabled, difficulty, difficultyReason, executionMode).Scan(&t.ID, &t.Status, &t.Paused, &t.CreatedAt); err != nil {
		return nil, err
	}
	if err := insertTaskRelations(tx, t.ID, opts.SourceTaskIDs); err != nil {
		return nil, err
	}
	if err := insertTaskCompanies(tx, t.ID, opts.CompanyIDs); err != nil {
		return nil, err
	}
	if err := insertTaskLLMProfiles(tx, t.ID, opts.LLMProfileIDs); err != nil {
		return nil, err
	}
	if err := insertTaskInterceptRules(tx, t.ID, opts.InterceptRules); err != nil {
		return nil, err
	}
	if len(opts.LLMProfileIDs) == 0 {
		t.LLMFailoverState = "default"
	} else {
		t.LLMFailoverState = "ready"
	}
	return t, tx.Commit()
}

func insertTaskCompanies(tx *sql.Tx, taskID int64, companyIDs []int64) error {
	if len(companyIDs) == 0 {
		return nil
	}
	// Company scope edits rebuild assets.company_id. Serialize the creation-time
	// snapshot with those edits so the task sees one committed attribution state.
	if err := lockCompanyScopeMutation(tx); err != nil {
		return err
	}
	var inserted int
	err := tx.QueryRow(`
WITH requested(company_id, position) AS (
    SELECT company_id, position
    FROM unnest($2::bigint[]) WITH ORDINALITY AS requested(company_id, position)
), inserted AS (
    INSERT INTO task_scope(task_id, kind, company_id, source, reason)
    SELECT $1, 'company', companies.id, 'manual', '任务创建时关联企业'
    FROM requested
    JOIN companies ON companies.id=requested.company_id
    ORDER BY requested.position
    RETURNING company_id
)
SELECT count(*) FROM inserted`, taskID, companyIDs).Scan(&inserted)
	if err != nil {
		return err
	}
	if inserted != len(companyIDs) {
		return fmt.Errorf("%w: one or more companies do not exist", ErrTaskCompanyNotFound)
	}

	// Updating task_ids fires sync_task_asset_links, which first creates generic
	// source rows. The provenance upsert must therefore run afterwards so the
	// company name and creation-time reason remain visible to operators.
	if _, err := tx.Exec(`
UPDATE assets
SET task_ids=CASE
    WHEN $1=ANY(task_ids) THEN task_ids
    ELSE array_append(task_ids, $1)
END
WHERE company_id=ANY($2::bigint[])`, taskID, companyIDs); err != nil {
		return err
	}
	if _, err := tx.Exec(`
INSERT INTO task_asset_links(task_id, asset_id, source, source_summary)
SELECT $1, asset.id, $3, '任务创建时关联企业：' || company.name
FROM assets asset
JOIN companies company ON company.id=asset.company_id
WHERE asset.company_id=ANY($2::bigint[])
  AND $1=ANY(asset.task_ids)
ON CONFLICT (task_id, asset_id) DO UPDATE
SET source=EXCLUDED.source,
    source_summary=EXCLUDED.source_summary,
    source_node_id=NULL`, taskID, companyIDs, taskCompanyAssetSource); err != nil {
		return err
	}
	return nil
}

func insertTaskRelations(tx *sql.Tx, taskID int64, sourceIDs []int64) error {
	seen := map[int64]bool{}
	for _, sourceID := range sourceIDs {
		if sourceID <= 0 || sourceID == taskID || seen[sourceID] {
			return fmt.Errorf("invalid or duplicate source task id %d", sourceID)
		}
		seen[sourceID] = true
		res, err := tx.Exec(`INSERT INTO task_relations(task_id, source_task_id)
SELECT $1, id FROM tasks WHERE id=$2 AND deleted_at IS NULL`, taskID, sourceID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return fmt.Errorf("source task %d not found", sourceID)
		}
	}
	return nil
}

func insertTaskLLMProfiles(tx *sql.Tx, taskID int64, profileIDs []int64) error {
	seen := map[int64]bool{}
	for position, profileID := range profileIDs {
		if profileID <= 0 || seen[profileID] {
			return fmt.Errorf("invalid or duplicate LLM profile id %d", profileID)
		}
		seen[profileID] = true
		if _, err := tx.Exec(`INSERT INTO task_llm_profiles(task_id, profile_id, position) VALUES ($1,$2,$3)`, taskID, profileID, position); err != nil {
			return err
		}
	}
	return nil
}

const taskCols = `id, COALESCE(name,''), category_id,
COALESCE((SELECT category.name FROM task_categories category WHERE category.id=tasks.category_id),''),
description, goal, exploration_id, status, paused, queued, queued_at, COALESCE(queue_mode,''), llm_profile_id, active_llm_profile_id, COALESCE(parent_ref,''), pinned_at, created_at, completed_at, COALESCE(timeout_seconds,0), COALESCE(plan_heartbeat_seconds,300), COALESCE(coverage_enabled,true), COALESCE(scope_configured,true), first_run_at, deadline_at, COALESCE(difficulty,'medium'), COALESCE(difficulty_reason,''), COALESCE(execution_mode,''), COALESCE(grade,''), COALESCE(grade_summary,''), grade_score`

func scanTask(sc interface{ Scan(...any) error }) (*Task, error) {
	var t Task
	var score sql.NullFloat64
	if err := sc.Scan(&t.ID, &t.Name, &t.CategoryID, &t.CategoryName, &t.Description, &t.Goal, &t.ExplorationID, &t.Status, &t.Paused, &t.Queued, &t.QueuedAt, &t.QueueMode, &t.LLMProfileID, &t.ActiveLLMProfileID, &t.ParentRef, &t.PinnedAt, &t.CreatedAt, &t.CompletedAt, &t.TimeoutSeconds, &t.PlanHeartbeatSeconds, &t.CoverageEnabled, &t.ScopeConfigured, &t.FirstRunAt, &t.DeadlineAt, &t.Difficulty, &t.DifficultyReason, &t.ExecutionMode, &t.Grade, &t.GradeSummary, &score); err != nil {
		return nil, err
	}
	if score.Valid {
		v := score.Float64
		t.GradeScore = &v
	}
	t.Pinned = t.PinnedAt != nil
	return &t, nil
}

// SetParentRef records a task's parent task id (编排 agent spawn_task 关联).
func (d *DB) SetParentRef(id int64, parentRef string) error {
	_, err := d.Exec(`UPDATE tasks SET parent_ref=NULLIF($2,'') WHERE id=$1`, id, parentRef)
	return err
}

// MarkTaskScopeConfigured 把任务的 scope_configured 翻回 true（方案 v3 缺陷 H1）。
//
// seed 失败置 false 后，任何后续 scope 写入（task_scope 行 / 任务资产挂靠）都调
// 这里消横幅——横幅只说「创建时没配上 scope」，人补配后必须自动消失。
// best-effort：翻不动不影响主写（横幅多显示一次不是数据错误）。
func (d *DB) MarkTaskScopeConfigured(taskID int64) {
	if taskID <= 0 {
		return
	}
	_, _ = d.Exec(`UPDATE tasks SET scope_configured=true WHERE id=$1 AND NOT scope_configured`, taskID)
}

// SetTaskScopeConfigured 写 scope_configured 全值（seed 失败路径用 false；
// MarkTaskScopeConfigured 是它的单向 true 特例，供 scope 写入点复用）。
func (d *DB) SetTaskScopeConfigured(taskID int64, configured bool) error {
	if taskID <= 0 {
		return fmt.Errorf("task id 必须为正")
	}
	_, err := d.Exec(`UPDATE tasks SET scope_configured=$2 WHERE id=$1`, taskID, configured)
	return err
}

// ListTasks returns alive tasks with pinned tasks first, then newest ids.
func (d *DB) ListTasks() ([]*Task, error) {
	if err := d.EnsureTaskDifficultySchema(); err != nil {
		return nil, err
	}
	if err := d.EnsureTaskExecutionModeSchema(); err != nil {
		return nil, err
	}
	if err := d.EnsureTaskGradeSchema(); err != nil {
		return nil, err
	}
	// id 是 BIGSERIAL，同一时刻创建的任务也有稳定且唯一的顺序。
	rows, err := d.Query(`SELECT ` + taskCols + ` FROM tasks WHERE deleted_at IS NULL
ORDER BY (pinned_at IS NOT NULL) DESC, pinned_at DESC NULLS LAST, id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	if err := d.hydrateTasksContext(out); err != nil {
		return nil, err
	}
	return out, nil
}

// TaskPatch updates task list metadata without changing execution state.
// 难度字段同样是元数据：nil = 不改。改难度只更新 tasks 行，不碰已有意图。
// ExecutionMode 同理：nil = 不改；指向空串 = 改回跟随难度。
type TaskPatch struct {
	Name             *string
	Pinned           *bool
	Difficulty       *string
	DifficultyReason *string
	ExecutionMode    *string
}

// UpdateTask applies a partial task name/pin/difficulty mutation and returns the updated row.
// Re-pinning an already pinned task preserves its original position.
// 难度每次现读现写，不在调用方缓存快照；非法难度在 SQL 之前拒绝，行保持原值。
func (d *DB) UpdateTask(id int64, patch TaskPatch) (*Task, error) {
	if err := d.EnsureTaskDifficultySchema(); err != nil {
		return nil, err
	}
	if err := d.EnsureTaskExecutionModeSchema(); err != nil {
		return nil, err
	}
	if err := d.EnsureTaskGradeSchema(); err != nil {
		return nil, err
	}
	var difficulty string
	if patch.Difficulty != nil {
		difficulty = strings.TrimSpace(*patch.Difficulty)
		if !ValidTaskDifficulty(difficulty) {
			return nil, fmt.Errorf("%w: %q", ErrTaskDifficultyInvalid, difficulty)
		}
	}
	var reason string
	if patch.DifficultyReason != nil {
		reason = strings.TrimSpace(*patch.DifficultyReason)
	}
	var executionMode string
	if patch.ExecutionMode != nil {
		var err error
		executionMode, err = normalizeTaskExecutionMode(*patch.ExecutionMode)
		if err != nil {
			return nil, err
		}
	}
	task, err := scanTask(d.QueryRow(`UPDATE tasks SET
	name = CASE WHEN $2::boolean THEN $3 ELSE name END,
	pinned_at = CASE
		WHEN $4::boolean IS NULL THEN pinned_at
		WHEN $4::boolean THEN COALESCE(pinned_at, now())
		ELSE NULL
	END,
	difficulty = CASE WHEN $5::boolean THEN $6 ELSE difficulty END,
	difficulty_reason = CASE WHEN $7::boolean THEN $8 ELSE difficulty_reason END,
	execution_mode = CASE WHEN $9::boolean THEN $10 ELSE execution_mode END
WHERE id=$1 AND deleted_at IS NULL
RETURNING `+taskCols, id, patch.Name != nil, patch.Name, patch.Pinned, patch.Difficulty != nil, difficulty, patch.DifficultyReason != nil, reason, patch.ExecutionMode != nil, executionMode))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return task, nil
}

// GetTask returns one alive task (nil if not found/deleted).
func (d *DB) GetTask(id int64) (*Task, error) {
	if err := d.EnsureTaskDifficultySchema(); err != nil {
		return nil, err
	}
	if err := d.EnsureTaskExecutionModeSchema(); err != nil {
		return nil, err
	}
	if err := d.EnsureTaskGradeSchema(); err != nil {
		return nil, err
	}
	t, err := scanTask(d.QueryRow(`SELECT `+taskCols+` FROM tasks WHERE id=$1 AND deleted_at IS NULL`, id))
	if err != nil {
		if err.Error() == "sql: no rows in result set" {
			return nil, nil
		}
		return nil, err
	}
	if err := d.hydrateTaskContext(t); err != nil {
		return nil, err
	}
	return t, nil
}

// SetPaused persists a task's paused flag.
func (d *DB) SetPaused(id int64, paused bool) error {
	_, err := d.Exec(`UPDATE tasks SET paused=$1 WHERE id=$2`, paused, id)
	return err
}

// Enqueue places a task at the tail of the persistent admission queue. Repeating
// the operation while it is already queued keeps its original FIFO position.
func (d *DB) Enqueue(id int64, mode string) error {
	if mode != "bootstrap" && mode != "resume" {
		return fmt.Errorf("invalid queue mode %q", mode)
	}
	_, err := d.Exec(`UPDATE tasks
SET queued=true,
    queued_at=CASE WHEN queued THEN COALESCE(queued_at, now()) ELSE now() END,
    queue_mode=CASE
        WHEN queue_mode='bootstrap' OR $2='bootstrap' THEN 'bootstrap'
        ELSE 'resume'
    END
WHERE id=$1`, id, mode)
	return err
}

// Dequeue removes the concurrency hold. clearMode is false when a user pauses a
// queued task, preserving whether its next admission must bootstrap or resume.
func (d *DB) Dequeue(id int64, clearMode bool) error {
	_, err := d.Exec(`UPDATE tasks
SET queued=false,
    queued_at=NULL,
    queue_mode=CASE WHEN $2 THEN '' ELSE queue_mode END
WHERE id=$1`, id, clearMode)
	return err
}

// SetQueued is the compatibility helper used by older callers and tests. New
// scheduling code should use Enqueue/Dequeue so FIFO metadata is explicit.
func (d *DB) SetQueued(id int64, queued bool) error {
	if queued {
		return d.Enqueue(id, "bootstrap")
	}
	return d.Dequeue(id, true)
}

// SetStatus updates a task's lifecycle status. Entering a terminal state
// (done/failed/timeout) stamps completed_at once (COALESCE keeps the first stamp
// stable); moving back to a non-terminal state clears it, so a re-run has no stale
// finish time.
func (d *DB) SetStatus(id int64, status string) error {
	_, err := d.Exec(`
UPDATE tasks
   SET status = $1,
       completed_at = CASE WHEN $1 IN ('done','failed','timeout') THEN COALESCE(completed_at, now()) ELSE NULL END
 WHERE id = $2`, status, id)
	return err
}

// SetTerminalStatusGuarded sets a terminal status only when the task is NOT already
// terminal, so a completed↔timeout race resolves to the first writer (won=true).
// Returns won=false (no error) when another terminal status already stuck — the
// caller then leaves it alone. Non-terminal transitions / re-run still use SetStatus.
func (d *DB) SetTerminalStatusGuarded(id int64, status string) (won bool, err error) {
	res, err := d.Exec(`
UPDATE tasks
   SET status = $1,
       completed_at = COALESCE(completed_at, now())
 WHERE id = $2 AND status NOT IN ('done','failed','timeout')`, status, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// StampFirstRun records a task's first-real-run moment and computes its absolute
// deadline (= now + timeoutSeconds). Idempotent: only stamps when first_run_at is
// still NULL, so restarts / re-entries keep the original clock. timeoutSeconds<=0
// leaves deadline_at NULL (不限时). Returns the resulting deadline (nil = 不限/未变).
func (d *DB) StampFirstRun(id int64, timeoutSeconds int) (*time.Time, error) {
	var deadline *time.Time
	err := d.QueryRow(`
UPDATE tasks
   SET first_run_at = COALESCE(first_run_at, now()),
       deadline_at  = CASE
           WHEN first_run_at IS NOT NULL THEN deadline_at            -- 已盖过章：不动
           WHEN $2 > 0 THEN now() + make_interval(secs => $2)
           ELSE NULL END
 WHERE id = $1
 RETURNING deadline_at`, id, timeoutSeconds).Scan(&deadline)
	return deadline, err
}

// DeleteTask preserves the historical behavior: remove the task and its
// exploration subgraph while retaining global assets.
func (d *DB) DeleteTask(id int64) error {
	_, err := d.DeleteTaskCascade(id, false, false)
	return err
}

// DeleteTaskCascade hard-deletes a task and its exploration subgraph
// (nodes/edges/activity via ON DELETE CASCADE). Optional standalone findings and
// assets owned only by this task are deleted in the same transaction; shared
// assets are retained and only have this task id detached. deleteLLMRecords is
// optional to preserve callers of the original two-option API.
func (d *DB) DeleteTaskCascade(id int64, deleteAssets, deleteFindings bool, deleteLLMRecords ...bool) (TaskDeleteResult, error) {
	deleteRecords := len(deleteLLMRecords) > 0 && deleteLLMRecords[0]
	return d.DeleteTaskCascadePrepared(id, deleteAssets, deleteFindings, deleteRecords, nil)
}

// DeleteTaskCascadePrepared coordinates reversible external deletion with the
// PostgreSQL cascade. When prepare is non-nil, host ownership is resolved and
// prepare is invoked inside this transaction while asset and anchor writers are
// excluded. The locks remain held through commit, closing the window where a
// host could become shared after its traffic had already been staged.
//
// prepare must only stage reversible work. Any returned error or later database
// error rolls PostgreSQL back; the caller remains responsible for rolling back
// external work that its callback staged successfully.
func (d *DB) DeleteTaskCascadePrepared(
	id int64,
	deleteAssets, deleteFindings, deleteLLMRecords bool,
	prepare func(TaskDeletePreparation) error,
) (TaskDeleteResult, error) {
	var result TaskDeleteResult
	tx, err := d.Begin()
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	var expID int64
	if err := tx.QueryRow(`SELECT exploration_id FROM tasks WHERE id=$1 FOR UPDATE`, id).Scan(&expID); err != nil {
		if err == sql.ErrNoRows {
			return result, nil // already gone
		}
		return result, err
	}

	// SHARE ROW EXCLUSIVE conflicts with every INSERT/UPDATE/DELETE on these
	// tables and with another coordinated deletion. Taking both in one fixed order
	// prevents phantoms (a distinct asset row for the same host) as well as new
	// ownership/anchor references until the deletion transaction commits.
	if deleteAssets || prepare != nil {
		if _, err := tx.Exec(`LOCK TABLE assets, exploration_anchors IN SHARE ROW EXCLUSIVE MODE`); err != nil {
			return result, err
		}
	}
	if prepare != nil {
		hosts, err := hostsForTaskDeletion(tx, id, expID)
		if err != nil {
			return result, err
		}
		if err := prepare(TaskDeletePreparation{
			ExplorationID: expID,
			TrafficHosts:  hosts,
		}); err != nil {
			return result, err
		}
	}
	if deleteAssets {
		// Ownership is the union of explicit task_ids and this exploration's anchors.
		// An asset is deletable only when no other live task references it through
		// either mechanism. This covers legacy anchor-only seeds without destroying
		// evidence anchored by another task.
		res, err := tx.Exec(`
WITH candidate_assets AS (
  SELECT id FROM assets WHERE $1 = ANY(task_ids)
  UNION
  SELECT ea.asset_id
  FROM exploration_anchors ea
  JOIN exploration_nodes n ON n.id=ea.node_id
  WHERE n.exploration_id=$2
),
deletable AS (
  SELECT a.id
  FROM assets a
  JOIN candidate_assets c ON c.id=a.id
  WHERE NOT EXISTS (
    SELECT 1 FROM tasks t
    WHERE t.id<>$1 AND t.deleted_at IS NULL AND t.id=ANY(a.task_ids)
  ) AND NOT EXISTS (
    SELECT 1
    FROM exploration_anchors ea
    JOIN exploration_nodes n ON n.id=ea.node_id
    JOIN tasks t ON t.exploration_id=n.exploration_id
    WHERE ea.asset_id=a.id AND t.id<>$1 AND t.deleted_at IS NULL
  )
)
DELETE FROM assets a USING deletable d WHERE a.id=d.id`, id, expID)
		if err != nil {
			return result, err
		}
		result.AssetsDeleted, _ = res.RowsAffected()

		res, err = tx.Exec(`UPDATE assets SET task_ids = array_remove(task_ids, $1) WHERE $1 = ANY(task_ids)`, id)
		if err != nil {
			return result, err
		}
		result.AssetsDetached, _ = res.RowsAffected()
	}
	if deleteFindings {
		res, err := tx.Exec(`DELETE FROM findings WHERE task_id = $1`, id)
		if err != nil {
			return result, err
		}
		result.FindingsDeleted, _ = res.RowsAffected()
	}
	if deleteLLMRecords {
		res, err := tx.Exec(`DELETE FROM llm_records WHERE COALESCE(task_id,'')=$1`, strconv.FormatInt(id, 10))
		if err != nil {
			return result, err
		}
		result.LLMRecordsDeleted, _ = res.RowsAffected()
	}
	// llm_usage (the token metering ledger) is intentionally NOT deleted with the
	// task — it is kept as historical accounting even after the task is gone.
	// coverage_gaps 先清（0.15.40 修复）：0.14.3 建表时三条外键全部裸露（无
	// ON DELETE 子句），gap 行会以 RESTRICT 拦死 tasks/explorations 删除
	//（SQLSTATE 23503，远程部署实锤）。无条件删本任务 gap 账——无论删除选项
	// 如何组合，gap 是任务级派生账目，随任务消亡（schema 迁移的 CASCADE 为
	// 另一层保险）。
	if _, err := tx.Exec(`DELETE FROM coverage_gaps WHERE task_id=$1`, id); err != nil {
		return result, err
	}
	if _, err := tx.Exec(`DELETE FROM tasks WHERE id=$1`, id); err != nil {
		return result, err
	}
	// intent_conflicts 先清（0.15.40 修复，与 coverage_gaps 同款）：两条 node
	// 外键裸露（v0.15.39 及此前无 ON DELETE），DELETE explorations 级联删
	// nodes 时 RESTRICT 拦死。防御性清理 + schema 迁移 CASCADE 双保险。
	if _, err := tx.Exec(`DELETE FROM intent_conflicts WHERE exploration_id=$1`, expID); err != nil {
		return result, err
	}
	if _, err := tx.Exec(`DELETE FROM explorations WHERE id=$1`, expID); err != nil {
		return result, err
	}
	return result, tx.Commit()
}

// TaskGradeFinding 是评级取证用的一条发现。只带结论和证据正文，
// 证据是否完整由 agent.CheckFindingGate 判定（db 不能 import agent）。
type TaskGradeFinding struct {
	Summary  string
	Evidence string
}

// TaskGradeFacts 是一次评级要用的确定性计数。
// 目标口径与 GoalCountsAll 相同：kind='goal' 的总数，以及其中 state='met'。
// 已验证发现是 status='confirmed'。覆盖率是 TaskCoverage 的分母 / 已测。
// abandoned 的目标算进总数、不算达成。
type TaskGradeFacts struct {
	GoalsMet, GoalsTotal            int
	VerifiedFindings, TotalFindings int
	CoveredAssets, ScopedAssets     int
	Findings                        []TaskGradeFinding
}

// LoadTaskGradeFacts 为单任务读取评级输入。任务不存在时返回 sql.ErrNoRows。
// 不调用 GoalCountsAll：那是全表汇总，这里用同一谓词只查这一个探索。
func (d *DB) LoadTaskGradeFacts(taskID int64) (TaskGradeFacts, error) {
	var facts TaskGradeFacts
	if err := d.EnsureTaskGradeSchema(); err != nil {
		return facts, err
	}
	var expID int64
	err := d.QueryRow(`SELECT exploration_id FROM tasks WHERE id=$1 AND deleted_at IS NULL`, taskID).Scan(&expID)
	if err != nil {
		return facts, err
	}
	if err := d.QueryRow(`
SELECT COUNT(*), COUNT(*) FILTER (WHERE state='met')
FROM exploration_nodes
WHERE exploration_id=$1 AND kind='goal'`, expID).Scan(&facts.GoalsTotal, &facts.GoalsMet); err != nil {
		return facts, err
	}
	if err := d.QueryRow(`
SELECT COUNT(*), COUNT(*) FILTER (WHERE status=$2)
FROM findings
WHERE task_id=$1`, taskID, FindingConfirmed).Scan(&facts.TotalFindings, &facts.VerifiedFindings); err != nil {
		return facts, err
	}
	rows, err := d.Query(`
SELECT COALESCE(summary,''), COALESCE(evidence,'')
FROM findings
WHERE task_id=$1
ORDER BY id`, taskID)
	if err != nil {
		return facts, err
	}
	defer rows.Close()
	for rows.Next() {
		var f TaskGradeFinding
		if err := rows.Scan(&f.Summary, &f.Evidence); err != nil {
			return facts, err
		}
		facts.Findings = append(facts.Findings, f)
	}
	if err := rows.Err(); err != nil {
		return facts, err
	}
	cov, err := d.Assets().TaskCoverage(taskID, expID)
	if err != nil {
		return facts, err
	}
	if cov != nil {
		facts.ScopedAssets = cov.Denominator
		facts.CoveredAssets = cov.Tested
	}
	return facts, nil
}

// SaveTaskGrade 把一次评级写入任务行。force=false 时只在还没评（grade=”）
// 且任务已是终态时写入；已经评过则返回 false, nil，不覆盖。
// force=true 时终态任务一律覆盖。非终态返回 ErrTaskGradeNotTerminal，不写。
// 等级必须是 S/A/B/C。分数与摘要同一次 UPDATE 写入。
func (d *DB) SaveTaskGrade(id int64, grade, summary string, score float64, force bool) (bool, error) {
	if !ValidTaskGrade(grade) {
		return false, fmt.Errorf("%w: %q", ErrTaskGradeInvalid, grade)
	}
	if err := d.EnsureTaskGradeSchema(); err != nil {
		return false, err
	}
	q := `UPDATE tasks
SET grade=$2, grade_summary=$3, grade_score=$4
WHERE id=$1 AND deleted_at IS NULL AND status IN ('done','failed','timeout')`
	if !force {
		q += ` AND grade=''`
	}
	res, err := d.Exec(q, id, grade, summary, score)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		return true, nil
	}
	var status, existing string
	err = d.QueryRow(`SELECT status, grade FROM tasks WHERE id=$1 AND deleted_at IS NULL`, id).Scan(&status, &existing)
	if err == sql.ErrNoRows {
		return false, sql.ErrNoRows
	}
	if err != nil {
		return false, err
	}
	if !IsTerminal(status) {
		return false, ErrTaskGradeNotTerminal
	}
	if !force && existing != "" {
		return false, nil
	}
	return false, fmt.Errorf("保存任务评级失败")
}

// ClearTaskGrade 把评级清回未评。离开终态时调用，让下一次进入终态重新计算。
// 已经是空的则不动。
func (d *DB) ClearTaskGrade(id int64) error {
	if err := d.EnsureTaskGradeSchema(); err != nil {
		return err
	}
	_, err := d.Exec(`UPDATE tasks SET grade='', grade_summary='', grade_score=NULL WHERE id=$1 AND grade<>''`, id)
	return err
}

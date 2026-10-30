package db

import (
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
)

// TaskInterceptRuleInput is one task-level rule supplied at task creation.
// Action: 'block'=拦截 'allow'=允许(白名单)；空视为 'block'。
type TaskInterceptRuleInput struct {
	Enabled bool   `json:"enabled"`
	Action  string `json:"action"`
	Kind    string `json:"kind"`
	Pattern string `json:"pattern"`
	Note    string `json:"note"`
}

const taskInterceptRuleCols = `id, enabled, action, kind, pattern, note, created_at, updated_at`

func scanTaskInterceptRule(row interface{ Scan(...any) error }) (AssetInterceptRule, error) {
	var r AssetInterceptRule
	err := row.Scan(&r.ID, &r.Enabled, &r.Action, &r.Kind, &r.Pattern, &r.Note, &r.CreatedAt, &r.UpdatedAt)
	return r, err
}

// ListTaskInterceptRules returns a task's rules (both block and allow) as
// AssetInterceptRule (Builtin always false; Action carries block/allow). taskID
// <= 0 returns nothing.
func (s *AssetStore) ListTaskInterceptRules(taskID int64) ([]AssetInterceptRule, error) {
	if taskID <= 0 {
		return nil, nil
	}
	rows, err := s.db.Query(`SELECT `+taskInterceptRuleCols+` FROM task_intercept_rules WHERE task_id=$1 ORDER BY action, id`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AssetInterceptRule
	for rows.Next() {
		r, err := scanTaskInterceptRule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// TaskInterceptRulesSplit loads a task's rules and splits them into block and
// allow sets, for the enforcement gate.
func (s *AssetStore) TaskInterceptRulesSplit(taskID int64) (block, allow []AssetInterceptRule, err error) {
	rules, err := s.ListTaskInterceptRules(taskID)
	if err != nil {
		return nil, nil, err
	}
	for _, r := range rules {
		if r.Action == "allow" {
			allow = append(allow, r)
		} else {
			block = append(block, r)
		}
	}
	return block, allow, nil
}

func normalizeRuleAction(action string) string {
	if action == "allow" {
		return "allow"
	}
	return "block"
}

// CreateTaskInterceptRule inserts a rule under a task.
func (s *AssetStore) CreateTaskInterceptRule(taskID int64, action, kind, pattern, note string, enabled bool) (AssetInterceptRule, error) {
	row := s.db.QueryRow(`
INSERT INTO task_intercept_rules(task_id, enabled, action, kind, pattern, note)
VALUES ($1,$2,$3,$4,$5,$6)
RETURNING `+taskInterceptRuleCols,
		taskID, enabled, normalizeRuleAction(action), kind, pattern, note)
	return scanTaskInterceptRule(row)
}

// UpdateTaskInterceptRule replaces the editable fields of a task's rule (scoped
// by task_id so a rule can only be edited through its owning task).
func (s *AssetStore) UpdateTaskInterceptRule(taskID, ruleID int64, action, kind, pattern, note string, enabled bool) (AssetInterceptRule, error) {
	row := s.db.QueryRow(`
UPDATE task_intercept_rules
   SET enabled=$3, action=$4, kind=$5, pattern=$6, note=$7
WHERE id=$1 AND task_id=$2
RETURNING `+taskInterceptRuleCols,
		ruleID, taskID, enabled, normalizeRuleAction(action), kind, pattern, note)
	return scanTaskInterceptRule(row)
}

// DeleteTaskInterceptRule removes a task's rule. Returns false if not found.
func (s *AssetStore) DeleteTaskInterceptRule(taskID, ruleID int64) (bool, error) {
	res, err := s.db.Exec(`DELETE FROM task_intercept_rules WHERE id=$1 AND task_id=$2`, ruleID, taskID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ToggleTaskInterceptRule flips the enabled state of a task's rule.
func (s *AssetStore) ToggleTaskInterceptRule(taskID, ruleID int64, enabled bool) error {
	_, err := s.db.Exec(`UPDATE task_intercept_rules SET enabled=$3 WHERE id=$1 AND task_id=$2`, ruleID, taskID, enabled)
	return err
}

// TaskCommandRuleKind 是 task_intercept_rules 里「命令级」规则的 kind。
//
// 与资产级 kind（域名/IP/URL/CIDR）的分工：资产级规则判的是「这个目标能不能测」
// （资产闸门，db.EvaluateAssetGate），命令级规则判的是「这条工具调用能不能执行」
// （改日志、清历史、抽 journal 这类会毁掉取证证据的动作）。两类规则同表不同质，
// 所以用 kind 区分。
const TaskCommandRuleKind = "command_regex"

// taskCommandRuleKindDDL 放宽 task_intercept_rules.kind 的枚举约束（多出
// 'command_regex'），并给命令级规则加一条唯一索引。
//
// 为什么需要放宽：这张表原本只为资产闸门设计，kind 的 CHECK 里只有域名/IP/URL/
// CIDR 四个维度，装不下命令级规则（对工具入参做正则匹配）。
//
// 为什么 DDL 写在这里而不是 db/schema.sql：schema.sql 由别的单元并行持有（见计划书
// Global Constraints）。本单元按 db/sod.go / db/llm_usage.go 的既有范式把幂等 DDL
// 放在访问层、首次写入前补齐；schema 属主把它并进 schema.sql 之后，这里重复应用
// 也无副作用。
//
// 幂等性：每次执行都先把 kind 列上的 CHECK 约束全删掉、再统一加一条带新枚举的，
// 重复执行后的最终状态一致。唯一索引保证「同一个任务 + 同一条 pattern」只有一行
// ⇒ 安装动作天然幂等，并发安装也不会堆出重复行。
const taskCommandRuleKindDDL = `
  DO $$
  DECLARE cname text;
  BEGIN
    IF to_regclass('task_intercept_rules') IS NULL THEN
      RETURN;
    END IF;
    -- 已有约束若已经放行 'command_regex' 就**立刻**返回，后面一概不做。
    --
    -- 为什么这个检查必须排在最前面：反复 DROP/ADD 会白白拿表级锁
    -- （ACCESS EXCLUSIVE），而这张表是热表 ⇒ 同库并发时锁会排队，甚至互等。
    -- 先看约束在不在，一眼就能判断「这次什么都不用做」。
    IF EXISTS (
      SELECT 1 FROM pg_constraint
       WHERE conrelid = 'task_intercept_rules'::regclass
         AND contype = 'c'
         AND pg_get_constraintdef(oid) LIKE '%command_regex%'
    ) THEN
      RETURN;
    END IF;
    CREATE UNIQUE INDEX IF NOT EXISTS idx_task_intercept_command_unique
        ON task_intercept_rules(task_id, kind, pattern)
        WHERE kind = 'command_regex';
    FOR cname IN
      SELECT conname FROM pg_constraint
       WHERE conrelid = 'task_intercept_rules'::regclass
         AND contype = 'c'
         AND pg_get_constraintdef(oid) LIKE '%kind%'
    LOOP
      EXECUTE format('ALTER TABLE task_intercept_rules DROP CONSTRAINT %I', cname);
    END LOOP;
    ALTER TABLE task_intercept_rules
      ADD CONSTRAINT task_intercept_rules_kind_check
      CHECK (kind IN ('exact_domain','exact_ip','exact_url',
                      'fuzzy_domain','fuzzy_ip','fuzzy_url',
                      'cidr','command_regex'));
  END $$;
  `

// taskCommandRuleSchemaReady 记录某个连接已经补过 kind 约束。
//
// 键是底层 *sql.DB 指针。
//
// ⚠️ 这个 memo **不能**当作「约束已就位」的依据：同一个库可以被多个连接
// 打开（每个测试自建 Manager 就是如此），指针不同 ⇒ memo 互不命中 ⇒ 每次
// 都会重跑 DDL。而重跑 DDL 会拿这张热表的表级锁 ⇒ 同库并发时互相排队，
// 表现为整包测试卡到超时。
//
// 所以真正的保证在 **db.Open**：那里统一补一次约束（见 EnsureTaskInterceptSchema）。
// 这个 memo 只用于「同一个连接内不要重复执行」这一层便宜的优化。
var taskCommandRuleSchemaReady sync.Map

// EnsureTaskInterceptSchema 放宽任务拦截规则的 kind 枚举，并加命令级唯一索引。
//
// 在 db.Open 里调用（与 EnsureChatGroupSchema / EnsureQuotaSchema /
//  EnsureStrategySchema 同范式）：DDL 只在进程启动时补一次，不在写路径上懒执行 ——
// 写路径上懒执行会在同库并发时反复拿表级锁。
//
// 幂等：约束已放行 'command_regex' 时立刻返回，什么都不做。
func (d *DB) EnsureTaskInterceptSchema() error {
	if _, err := d.Exec(taskCommandRuleKindDDL); err != nil {
		return fmt.Errorf("放宽任务拦截规则 kind 约束失败: %w", err)
	}
	return nil
}

// ensureCommandRuleKind 幂等补齐 kind 约束（含命令级唯一索引）。
func (s *AssetStore) ensureCommandRuleKind() error {
	if _, ok := taskCommandRuleSchemaReady.Load(s.db.DB); ok {
		return nil
	}
	// 兜底：约束没就位时才补，且补的动作在 DDL 里先看约束在不在 ——
	// 已经在就直接返回，不碰表锁。真正的保证在 db.Open（统一补一次）。
	//
	// 为什么写路径上不能无条件重跑 DDL：这张表是热表，重建 CHECK 约束要拿
	// ACCESS EXCLUSIVE 表级锁；同库并发时互相排队，整包测试会卡到超时。
	if err := s.db.EnsureTaskInterceptSchema(); err != nil {
		return err
	}
	taskCommandRuleSchemaReady.Store(s.db.DB, struct{}{})
	return nil
}

// AddTaskInterceptRule 给任务装一条命令级拦截规则（证据保全策略用）。
//
// 幂等：同一个任务上同一条 pattern 只装一行（唯一索引 + ON CONFLICT DO NOTHING），
// 重复安装不堆重复行。
//
// 写入前先编译一遍正则：平台的拦截器遇到编译不过的正则会**静默跳过这条规则**
// （不报错、也不拦）。一条「装上了却不生效」的规则比没装更坏 —— 它会让人以为
// 证据已经被保全了。
func (s *AssetStore) AddTaskInterceptRule(taskID int64, name, pattern, message string) error {
	if taskID <= 0 {
		return fmt.Errorf("任务 id 非法: %d", taskID)
	}
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return errors.New("拦截规则 pattern 不能为空")
	}
	if _, err := regexp.Compile(pattern); err != nil {
		return fmt.Errorf("拦截规则 pattern 不是合法正则: %w", err)
	}
	if err := s.ensureCommandRuleKind(); err != nil {
		return err
	}
	// note 是这张表唯一的自由文本列（没有单独的 name 列）⇒ 规则名与拒绝理由合并存。
	note := strings.TrimSpace(name)
	if msg := strings.TrimSpace(message); msg != "" {
		if note != "" {
			note += "："
		}
		note += msg
	}
	if _, err := s.db.Exec(`
INSERT INTO task_intercept_rules(task_id, enabled, action, kind, pattern, note)
VALUES ($1, true, 'block', $2, $3, $4)
ON CONFLICT DO NOTHING`,
		taskID, TaskCommandRuleKind, pattern, note); err != nil {
		return fmt.Errorf("写入任务拦截规则失败: %w", err)
	}
	return nil
}

// ListTaskCommandRules 返回任务上的命令级拦截规则（证据保全策略装的那批）。
//
// 与 ListTaskInterceptRules 的区别：那个返回任务的全部规则（资产级 + 命令级），
// 这个只挑命令级 —— 执行侧要判的是「这条工具调用能不能执行」。
func (s *AssetStore) ListTaskCommandRules(taskID int64) ([]AssetInterceptRule, error) {
	rules, err := s.ListTaskInterceptRules(taskID)
	if err != nil {
		return nil, err
	}
	out := make([]AssetInterceptRule, 0, len(rules))
	for _, r := range rules {
		if r.Kind == TaskCommandRuleKind {
			out = append(out, r)
		}
	}
	return out, nil
}

// insertTaskInterceptRules inserts task-level rules within the task-creation
// transaction (mirrors insertTaskCompanies).
func insertTaskInterceptRules(tx *sql.Tx, taskID int64, rules []TaskInterceptRuleInput) error {
	for _, r := range rules {
		if _, err := tx.Exec(`
INSERT INTO task_intercept_rules(task_id, enabled, action, kind, pattern, note)
VALUES ($1,$2,$3,$4,$5,$6)`, taskID, r.Enabled, normalizeRuleAction(r.Action), r.Kind, r.Pattern, r.Note); err != nil {
			return fmt.Errorf("insert task intercept rule %q: %w", r.Pattern, err)
		}
	}
	return nil
}

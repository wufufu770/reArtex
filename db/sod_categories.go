package db

// ---------- 拦截种类表 / 影子对照 / 建议队列 的存取（方案 v4.1）----------
//
// 种类表的读路径是**每次工具调用都要走的热路径**：ListSodCategories 由 server
// 侧挂缓存（TTL 秒级），这里只保证单次查询便宜（enabled 索引 + 全表极小）。
// 写路径全部 bump settings `sod_categories_version`——server 缓存靠它失效。

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/Autumn-27/artex/internal/sodpolicy"
)

// SodCategoryRow 是 sod_categories 的一行（与 sodpolicy.SodCategory 同构，
// 加审计时间戳）。
type SodCategoryRow struct {
	sodpolicy.SodCategory
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

const sodCategoriesVersionKey = "sod_categories_version"

// ListSodCategories 列全部种类（含停用——UI 要能看见并重新启用）。
func (d *DB) ListSodCategories() ([]SodCategoryRow, error) {
	rows, err := d.Query(`
		SELECT id, name, kind, pattern, action, cap_per_task, fallback, locked, enabled, priority, reason, created_by, created_at, updated_at
		FROM sod_categories ORDER BY priority DESC, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SodCategoryRow
	for rows.Next() {
		var r SodCategoryRow
		if err := rows.Scan(&r.ID, &r.Name, &r.Kind, &r.Pattern, &r.Action, &r.CapPerTask, &r.Fallback,
			&r.Locked, &r.Enabled, &r.Priority, &r.Reason, &r.CreatedBy, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// CreateSodCategory 新建种类（custom 或 builtin 引用）；同名冲突返回错误。
func (d *DB) CreateSodCategory(cat sodpolicy.SodCategory) (SodCategoryRow, error) {
	var id int64
	err := d.QueryRow(`
		INSERT INTO sod_categories (name, kind, pattern, action, cap_per_task, fallback, locked, enabled, priority, reason, created_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11) RETURNING id`,
		cat.Name, cat.Kind, cat.Pattern, cat.Action, cat.CapPerTask, cat.Fallback, cat.Locked, cat.Enabled, cat.Priority, cat.Reason, cat.CreatedBy,
	).Scan(&id)
	if err != nil {
		return SodCategoryRow{}, fmt.Errorf("创建种类失败: %w", err)
	}
	_ = d.bumpSodCategoriesVersion()
	return d.GetSodCategory(id)
}

// GetSodCategory 取单行。
func (d *DB) GetSodCategory(id int64) (SodCategoryRow, error) {
	var r SodCategoryRow
	err := d.QueryRow(`
		SELECT id, name, kind, pattern, action, cap_per_task, fallback, locked, enabled, priority, reason, created_by, created_at, updated_at
		FROM sod_categories WHERE id=$1`, id).
		Scan(&r.ID, &r.Name, &r.Kind, &r.Pattern, &r.Action, &r.CapPerTask, &r.Fallback,
			&r.Locked, &r.Enabled, &r.Priority, &r.Reason, &r.CreatedBy, &r.CreatedAt, &r.UpdatedAt)
	if err != nil {
		return SodCategoryRow{}, err
	}
	return r, nil
}

// UpdateSodCategory 更新可变字段；locked 行拒绝改 action（安全地板）。
func (d *DB) UpdateSodCategory(id int64, action string, capPerTask int, fallback string, enabled *bool, priority *int, reason *string) (SodCategoryRow, error) {
	cur, err := d.GetSodCategory(id)
	if err != nil {
		return SodCategoryRow{}, err
	}
	if cur.Locked && action != cur.Action {
		return SodCategoryRow{}, fmt.Errorf("种类「%s」是锁定基线，动作不可修改", cur.Name)
	}
	if _, err := d.Exec(`
		UPDATE sod_categories
		SET action=$2, cap_per_task=$3, fallback=$4,
		    enabled=COALESCE($5, enabled),
		    priority=COALESCE($6, priority),
		    reason=COALESCE($7, reason),
		    updated_at=NOW()
		WHERE id=$1`, id, action, capPerTask, fallback, enabled, priority, reason); err != nil {
		return SodCategoryRow{}, err
	}
	_ = d.bumpSodCategoriesVersion()
	return d.GetSodCategory(id)
}

// DeleteSodCategory 删除种类；locked 行不可删。
func (d *DB) DeleteSodCategory(id int64) error {
	cur, err := d.GetSodCategory(id)
	if err != nil {
		return err
	}
	if cur.Locked {
		return fmt.Errorf("种类「%s」是锁定基线，不可删除", cur.Name)
	}
	if _, err := d.Exec(`DELETE FROM sod_categories WHERE id=$1`, id); err != nil {
		return err
	}
	_ = d.bumpSodCategoriesVersion()
	return nil
}

// bumpSodCategoriesVersion 让 server 侧种类快照缓存失效（best-effort）。
func (d *DB) bumpSodCategoriesVersion() error {
	return d.SetSetting(sodCategoriesVersionKey, fmt.Sprintf("%d", time.Now().UnixNano()))
}

// SodCategoriesVersion 读当前版本号（server 缓存对账用；读失败返回空串）。
func (d *DB) SodCategoriesVersion() string {
	v, _, _ := d.GetSetting(sodCategoriesVersionKey)
	return v
}

// SeedSodCategories 幂等种子（方案 v4.1 §2 表）：settings 旗标防重播 +
// ON CONFLICT DO NOTHING（用户手建同名行不覆盖）。删除的行不复活——旗标
// 只在从未种过时落一次。
func (d *DB) SeedSodCategories() error {
	// 表可能还没建（seedBuiltins 跑在启动期 schema 应用之前——agent 测试直呼
	// seedBuiltins 的路径就是实证）：幂等 DDL 先行，播种才不会 42P01。
	if err := d.EnsureSoDSchema(); err != nil {
		return fmt.Errorf("sod schema 兜底应用失败: %w", err)
	}
	if v, _, _ := d.GetSetting("sod_categories_seed_v1"); v == "done" {
		return nil
	}
	seeds := []sodpolicy.SodCategory{
		{Name: "删除/破坏", Kind: "builtin", Pattern: sodpolicy.RecognizeDestructive, Action: sodpolicy.CategoryActionHardDeny, Fallback: "deny", Locked: true, Enabled: true, Priority: 100, Reason: "锁定基线：用户拍板「只拦删除等动作」的删除/破坏面"},
		{Name: "平台自身配置", Kind: "builtin", Pattern: sodpolicy.RecognizePlatformCfg, Action: sodpolicy.CategoryActionHardDeny, Fallback: "deny", Locked: true, Enabled: true, Priority: 100, Reason: "锁定基线：create/update_mcp、自定义工具/技能文件、按主机删资产"},
		{Name: "任务目录内路径", Kind: "builtin", Pattern: sodpolicy.RecognizeTaskDirLocal, Action: sodpolicy.CategoryActionPass, Fallback: "ask", Enabled: true, Priority: 80, Reason: "任务工作区是任务自己的授权领地（B5 泛化）"},
		{Name: "情报源GET", Kind: "builtin", Pattern: sodpolicy.RecognizeIntelSource, Action: sodpolicy.CategoryActionPass, Fallback: "ask", Enabled: true, Priority: 80, Reason: "被动情报获取不触碰目标（crt.sh 等 17 源白名单）"},
		{Name: "DNS与whois查询", Kind: "builtin", Pattern: sodpolicy.RecognizeDNSWhois, Action: sodpolicy.CategoryActionPass, Fallback: "ask", Enabled: true, Priority: 80, Reason: "dig/host/nslookup/whois 是资产收集的标准动作"},
		{Name: "良性无目标动作", Kind: "builtin", Pattern: sodpolicy.RecognizeBenignNoop, Action: sodpolicy.CategoryActionPass, Fallback: "ask", Enabled: true, Priority: 80, Reason: "echo/sleep/工具库检索等无外部目标的良性动作"},
		{Name: "资产库写入", Kind: "builtin", Pattern: sodpolicy.RecognizeAssetInsert, Action: sodpolicy.CategoryActionPass, Fallback: "ask", Enabled: true, Priority: 70, Reason: "写平台自己的资产库（落库即 unverified，探测才需授权）"},
		{Name: "finding登记", Kind: "builtin", Pattern: sodpolicy.RecognizeFindingWrite, Action: sodpolicy.CategoryActionPass, Fallback: "ask", Enabled: true, Priority: 70, Reason: "写平台 findings 表，事后 verifier/三态验证兜底（任务 21 教训）"},
		{Name: "目录外写", Kind: "builtin", Pattern: sodpolicy.RecognizeOutsideWrite, Action: sodpolicy.CategoryActionAuditor, CapPerTask: 5, Fallback: "ask", Enabled: true, Priority: 60, Reason: "写宿主 FS 任务目录外（/tmp、/app/data/sessions 除外）"},
		{Name: "敏感面读取", Kind: "builtin", Pattern: sodpolicy.RecognizeSensitiveRead, Action: sodpolicy.CategoryActionAuditor, CapPerTask: 10, Fallback: "ask", Enabled: true, Priority: 65, Reason: "读宿主敏感面（/etc/shadow、~/.ssh 等；平台凭据由 pathguard 硬拦）"},
		{Name: "状态迁移", Kind: "builtin", Pattern: sodpolicy.RecognizeStateChange, Action: sodpolicy.CategoryActionAuditor, CapPerTask: 3, Fallback: "ask", Enabled: true, Priority: 60, Reason: "prove_goal/kill_work/goal_met 任务状态迁移"},
		{Name: "大扩权", Kind: "builtin", Pattern: sodpolicy.RecognizeScopeExpansion, Action: sodpolicy.CategoryActionAuditor, CapPerTask: 8, Fallback: "ask", Enabled: true, Priority: 60, Reason: "root_domain/company 扩权（subdomain 级仍走预算内直写）"},
		{Name: "出网新主机", Kind: "builtin", Pattern: sodpolicy.RecognizeEgressNewHost, Action: sodpolicy.CategoryActionAuditor, CapPerTask: 10, Fallback: "ask", Enabled: true, Priority: 60, Reason: "非情报源、非目标段的出网（决策点 1：deny→auditor/ask）"},
		{Name: "MCP调用", Kind: "builtin", Pattern: sodpolicy.RecognizeMCPCall, Action: sodpolicy.CategoryActionAuditor, CapPerTask: 0, Fallback: "ask", Enabled: true, Priority: 50, Reason: "MCP 任意名工具——不整体豁免，逐调用裁决"},
	}
	for i := range seeds {
		if err := d.CreateSodCategorySeed(seeds[i]); err != nil {
			return err
		}
	}
	return d.SetSetting("sod_categories_seed_v1", "done")
}

// CreateSodCategorySeed 是种子的静默插入（同名已存在则跳过，不 bump 版本）。
func (d *DB) CreateSodCategorySeed(cat sodpolicy.SodCategory) error {
	_, err := d.Exec(`
		INSERT INTO sod_categories (name, kind, pattern, action, cap_per_task, fallback, locked, enabled, priority, reason, created_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,'system')
		ON CONFLICT (name) DO NOTHING`,
		cat.Name, cat.Kind, cat.Pattern, cat.Action, cat.CapPerTask, cat.Fallback, cat.Locked, cat.Enabled, cat.Priority, cat.Reason)
	return err
}

// RecordSodShadowDiff 落一行影子对照（best-effort 由调用方保证）。
func (d *DB) RecordSodShadowDiff(diff sodShadowRow) error {
	_, err := d.Exec(`
		INSERT INTO sod_gate_shadow (task_id, tool_name, payload_digest, old_verdict, old_reason, new_verdict, new_reason)
		VALUES (NULLIF($1,0)::bigint, $2, $3, $4, $5, $6, $7)`,
		diff.TaskID, diff.ToolName, diff.PayloadDigest, diff.OldVerdict, diff.OldReason, diff.NewVerdict, diff.NewReason)
	return err
}

// sodShadowRow 与 internal/sod.ShadowDiff 同构（db 不 import internal 包）。
type sodShadowRow struct {
	TaskID        int64
	ToolName      string
	PayloadDigest string
	OldVerdict    string
	OldReason     string
	NewVerdict    string
	NewReason     string
}

// ListSodShadowDiffs 近期影子对照（倒序限量）。
func (d *DB) ListSodShadowDiffs(limit int) ([]map[string]any, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := d.Query(`
		SELECT id, COALESCE(task_id,0), tool_name, payload_digest, old_verdict, old_reason, new_verdict, new_reason, created_at
		FROM sod_gate_shadow ORDER BY id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var id, taskID int64
		var tool, digest, ov, or, nv, nr string
		var ts time.Time
		if err := rows.Scan(&id, &taskID, &tool, &digest, &ov, &or, &nv, &nr, &ts); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{
			"id": id, "task_id": taskID, "tool_name": tool, "payload_digest": digest,
			"old_verdict": ov, "old_reason": or, "new_verdict": nv, "new_reason": nr,
			"created_at": ts,
		})
	}
	return out, rows.Err()
}

// SodCategorySuggestion 是 auditor 的一条种类建议。
type SodCategorySuggestion struct {
	ID        int64            `json:"id"`
	Name      string           `json:"name"`
	Kind      string           `json:"kind"`
	Pattern   string           `json:"pattern"`
	Action    string           `json:"action"`
	Evidence  []map[string]any `json:"evidence"`
	Status    string           `json:"status"`
	Reason    string           `json:"reason"`
	CreatedAt time.Time        `json:"created_at"`
	DecidedAt *time.Time       `json:"decided_at"`
	DecidedBy string           `json:"decided_by"`
}

// CreateSodCategorySuggestion 落一条建议（auditor 裁决时 best-effort 调用）。
func (d *DB) CreateSodCategorySuggestion(name, kind, pattern, action, reason string, evidence []map[string]any) error {
	ev, err := json.Marshal(evidence)
	if err != nil {
		ev = []byte("[]")
	}
	_, err = d.Exec(`
		INSERT INTO sod_category_suggestions (name, kind, pattern, action, evidence, reason)
		VALUES ($1,$2,$3,$4,$5::jsonb,$6)`,
		name, kind, pattern, action, string(ev), reason)
	return err
}

// ListSodCategorySuggestions 列建议（status 空 = 全部）。
func (d *DB) ListSodCategorySuggestions(status string, limit int) ([]SodCategorySuggestion, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	q := `SELECT id, name, kind, pattern, action, COALESCE(evidence,'[]'::jsonb), status, reason, created_at, decided_at, decided_by
		FROM sod_category_suggestions`
	var args []any
	if status != "" {
		q += ` WHERE status=$1`
		args = append(args, status)
	}
	q += fmt.Sprintf(` ORDER BY id DESC LIMIT %d`, limit)
	rows, err := d.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SodCategorySuggestion
	for rows.Next() {
		var s SodCategorySuggestion
		var ev []byte
		if err := rows.Scan(&s.ID, &s.Name, &s.Kind, &s.Pattern, &s.Action, &ev, &s.Status, &s.Reason, &s.CreatedAt, &s.DecidedAt, &s.DecidedBy); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(ev, &s.Evidence)
		out = append(out, s)
	}
	return out, rows.Err()
}

// DecideSodCategorySuggestion 采纳/忽略一条建议。
func (d *DB) DecideSodCategorySuggestion(id int64, status, by string) error {
	if status != "adopted" && status != "dismissed" {
		return fmt.Errorf("status 只能是 adopted|dismissed")
	}
	_, err := d.Exec(`
		UPDATE sod_category_suggestions SET status=$2, decided_at=NOW(), decided_by=$3 WHERE id=$1 AND status='pending'`,
		id, status, by)
	return err
}

// CountSodCategoryUsed 统计某任务某类别的已用次数（cap 口径，账本即计数器）。
// 口径：sod_decisions 里 checks->category 等于该名且 created_at 在本任务窗口内
// 的行数（裁决即消耗一次配额，无论放行与否）。
func (d *DB) CountSodCategoryUsed(taskID int64, category string) (int, error) {
	var n int
	err := d.QueryRow(`
		SELECT count(*) FROM sod_decisions
		WHERE task_id=$1 AND created_at > now()-interval '7 days'
		  AND checks->>'category' = $2`,
		taskID, category).Scan(&n)
	return n, err
}

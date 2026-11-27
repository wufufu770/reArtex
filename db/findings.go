package db

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// DBFinding is a row in the standalone findings table. It persists across task
// deletion unless the caller explicitly requests related finding cleanup.
type DBFinding struct {
	TrafficCount          int
	EvidenceVersion       int64
	ReportEvidenceVersion int64
	TrafficBindings       []FindingTrafficBinding // populated only for export

	ID              int64
	TaskID          *int64
	NodeID          *int64
	VulnClass       string
	Name            string // 漏洞名称(可读标题);为空时前端回退展示 VulnClass
	Severity        string
	Summary         string
	Evidence        string
	Worker          string
	AssetIDs        []int64
	Status          string
	Report          string // 详细报告(Markdown);仅 GetFinding 填充,列表查询不带
	EvidenceStatus  string // verified | unverified | na（0.15.35 证据交叉验证）
	RootCauseKey    string // 同根因合并标记键（0.15.35，空=自由文本无 URL 未标记）
	CreatedAt       time.Time
	TaskDescription string // populated via LEFT JOIN on tasks

	// C-04 三维分级。严重度仍是 Severity，这两列不替代它。
	// ImpactLevel：1 信息 / 2 认证·越权 / 3 控制 / 4 数据·资金。
	// EvidenceLevel：1 推断 / 2 复现 / 3 完整证据链 / 4 独立复核通过。
	// EvidenceLevelRequested 是自报值；与 EvidenceLevel 不同时看 EvidenceLevelReason。
	ImpactLevel            int    `json:"impact_level"`
	EvidenceLevel          int    `json:"evidence_level"`
	EvidenceLevelRequested int    `json:"evidence_level_requested"`
	EvidenceLevelReason    string `json:"evidence_level_reason"`
}

// Finding triage states (findings.status).
const (
	FindingPending       = "pending"        // 待处理
	FindingInProgress    = "in_progress"    // 处理中
	FindingConfirmed     = "confirmed"      // 已确认(真实漏洞,未修复)
	FindingResolved      = "resolved"       // 已处理
	FindingFixed         = "fixed"          // 已修复
	FindingFalsePositive = "false_positive" // 误报
	FindingIgnored       = "ignored"        // 忽略
	FindingDuplicate     = "duplicate"      // 重复
	FindingRiskAccepted  = "risk_accepted"  // 风险接受
)

// ValidFindingStatus reports whether s is a known triage state.
func ValidFindingStatus(s string) bool {
	switch s {
	case FindingPending, FindingInProgress, FindingConfirmed, FindingResolved, FindingFixed,
		FindingFalsePositive, FindingIgnored, FindingDuplicate, FindingRiskAccepted:
		return true
	}
	return false
}

// Finding severity levels (findings.severity).
const (
	SeverityCritical = "critical" // 严重
	SeverityHigh     = "high"     // 高
	SeverityMedium   = "medium"   // 中
	SeverityLow      = "low"      // 低
	// SeverityInfo 信息级（E1-7，方案 v10 §9）：未证实候选面/侦察线索/无直接
	// 影响的暴露——大量低危实为信息级，虚高的主因就是缺这一档。
	SeverityInfo = "info" // 信息
)

// ValidSeverity reports whether s is a known severity level.
func ValidSeverity(s string) bool {
	switch s {
	case SeverityCritical, SeverityHigh, SeverityMedium, SeverityLow, SeverityInfo:
		return true
	}
	return false
}

//go:embed schema_finding_levels.sql
var findingLevelsSchemaSQL string

// findingLevelsSchemaReady 按连接池标记 DDL 已就位（markRuntimeSchemasReady 同键）。
var findingLevelsSchemaReady sync.Map

// ErrFindingLevelInvalid 表示影响层级或证据等级不在 1–4。
var ErrFindingLevelInvalid = errors.New("影响层级与证据等级必须是 1-4")

// EnsureFindingLevelsSchema 幂等补上 findings 的影响层级、证据等级、自报值与降级原因。
// 启动期用迁移连接调用；运行期连接被 markRuntimeSchemasReady 标成就位后不再执行 DDL。
func (d *DB) EnsureFindingLevelsSchema() error {
	if d == nil || d.DB == nil {
		return errors.New("数据库未装配")
	}
	if _, ok := findingLevelsSchemaReady.Load(d.DB); ok {
		return nil
	}
	if _, err := d.Exec(findingLevelsSchemaSQL); err != nil {
		return fmt.Errorf("应用漏洞分级 schema 失败: %w", err)
	}
	findingLevelsSchemaReady.Store(d.DB, struct{}{})
	return nil
}

// ValidFindingLevel 报告 n 是否在 1–4。0 不是合法等级（省略由调用方折成 1）。
func ValidFindingLevel(n int) bool {
	return n >= 1 && n <= 4
}

// EvidenceSufficient 判断这条严重度是否算「证据够用」。
// critical / high 必须证据等级 ≥ L3；其余严重度（含 info 与空串）不强制。
// 这是展示和统计口径，不负责拒绝入库。
func EvidenceSufficient(severity string, evidenceLevel int) bool {
	if severity == SeverityCritical || severity == SeverityHigh {
		return evidenceLevel >= 3
	}
	return true
}

// 证据等级上限的近似规则（E-01 的 finding_gate 落地前）。
// L3：去空白后不少于 20 个字符，且能看出引用了工具输出。
// L4：在 L3 之上，还要有独立复测 verdict=reproduced。
// 空证据最高 L1；有正文但够不上 L3 的最高 L2。
const evidenceChainMinRunes = 20

var (
	evidenceFencedOutput = regexp.MustCompile("(?s)```[^\\n]*\\n[[:space:]]*\\S.*?```")
	evidenceShellPrompt  = regexp.MustCompile("(?m)^[[:space:]]*[$#][[:space:]]+\\S")
)

// EvidenceCitesToolOutput 判断证据正文是否「够长且引用了工具输出」。
// 引用只认三种确定性痕迹：markdown 围栏代码块、HTTP 状态行、shell 提示符后的命令。
// 不查活动流，也不把任意 20 字散文当成完整证据链。E-03 落地后应改成逐字比对。
func EvidenceCitesToolOutput(evidence string) bool {
	text := strings.TrimSpace(evidence)
	if utf8.RuneCountInString(text) < evidenceChainMinRunes {
		return false
	}
	if evidenceFencedOutput.MatchString(text) {
		return true
	}
	if strings.Contains(text, "HTTP/1.") || strings.Contains(text, "HTTP/2") {
		return true
	}
	return evidenceShellPrompt.MatchString(text)
}

// EvidenceCeiling 是当前证据正文和复测状态下，证据等级允许写下的最高档。
func EvidenceCeiling(evidence string, reproduced bool) int {
	if strings.TrimSpace(evidence) == "" {
		return 1
	}
	if !EvidenceCitesToolOutput(evidence) {
		return 2
	}
	if reproduced {
		return 4
	}
	return 3
}

// CapEvidenceLevel 把自报证据等级收进可验证上限。
// requested 为 0 视为省略，按 L1 处理，不记降级。
// 超出 1–4 返回 ErrFindingLevelInvalid，调用方不得落库。
// 自报高于上限时 applied 是上限，reason 非空；没有降级时 reason 为空。
// 只降不升：自报低于上限时保持自报值。
func CapEvidenceLevel(requested int, evidence string, reproduced bool) (applied int, reason string, err error) {
	if requested == 0 {
		requested = 1
	}
	if !ValidFindingLevel(requested) {
		return 0, "", ErrFindingLevelInvalid
	}
	ceiling := EvidenceCeiling(evidence, reproduced)
	if requested <= ceiling {
		return requested, "", nil
	}
	reason = fmt.Sprintf("自报证据等级 L%d，可验证上限为 L%d", requested, ceiling)
	switch {
	case requested >= 4 && !reproduced && !EvidenceCitesToolOutput(evidence):
		reason += "（尚无独立复测 verdict=reproduced，且证据未达到长度≥20并含工具输出引用）"
	case requested >= 4 && !reproduced:
		reason += "（尚无独立复测 verdict=reproduced）"
	case !EvidenceCitesToolOutput(evidence):
		reason += "（证据未达到长度≥20并含工具输出引用）"
	}
	return ceiling, reason, nil
}

// SetFindingLevels 在一个事务里写下影响层级和证据等级，并把同样的值镜像到
// 来源探索节点的 payload。本函数只做 1–4 白名单，不按证据正文降级——
// agent 自报要先过 CapEvidenceLevel，再把结果交进来。
// 显式写入会把自报值改成这次的证据等级，并清空降级原因。
// 找不到行返回 ErrFindingNotFound。
func (d *DB) SetFindingLevels(id int64, impact, evidence int) error {
	if !ValidFindingLevel(impact) || !ValidFindingLevel(evidence) {
		return ErrFindingLevelInvalid
	}
	return d.writeFindingLevels(id, impact, evidence, evidence, "")
}

// writeFindingLevels 把四列一起写入。requested 必须已是 1–4。
func (d *DB) writeFindingLevels(id int64, impact, evidence, requested int, reason string) error {
	if err := d.EnsureFindingLevelsSchema(); err != nil {
		return err
	}
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var nodeID sql.NullInt64
	err = tx.QueryRow(`UPDATE findings
		SET impact_level=$1, evidence_level=$2, evidence_level_requested=$3, evidence_level_reason=$4
		WHERE id=$5 RETURNING node_id`, impact, evidence, requested, reason, id).Scan(&nodeID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrFindingNotFound
	}
	if err != nil {
		return err
	}
	if nodeID.Valid {
		if _, err := tx.Exec(`UPDATE exploration_nodes
			SET payload = jsonb_set(
				jsonb_set(
					jsonb_set(
						jsonb_set(COALESCE(payload, '{}'::jsonb), '{impact_level}', to_jsonb($1::int), true),
						'{evidence_level}', to_jsonb($2::int), true),
					'{evidence_level_requested}', to_jsonb($3::int), true),
				'{evidence_level_reason}', to_jsonb($4::text), true)
			WHERE id=$5`, impact, evidence, requested, reason, nodeID.Int64); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// resolveFindingLevels 把自报的两维收成可落库的四元组。
// 影响层级只做 1–4 白名单（0 视为省略，按 L1）。证据等级走 CapEvidenceLevel。
func resolveFindingLevels(impact, evidence int, evidenceText string, reproduced bool) (impactOut, applied, requested int, reason string, err error) {
	if impact == 0 {
		impact = 1
	}
	if !ValidFindingLevel(impact) {
		return 0, 0, 0, "", ErrFindingLevelInvalid
	}
	applied, reason, err = CapEvidenceLevel(evidence, evidenceText, reproduced)
	if err != nil {
		return 0, 0, 0, "", err
	}
	requested = evidence
	if requested == 0 {
		requested = 1
	}
	return impact, applied, requested, reason, nil
}

// ApplyFindingLevels 按调用方给出的维度更新一条 finding。
// nil 表示这一维不改。给出的值必须是 1–4。证据等级仍走上限，不能绕过降级。
func (d *DB) ApplyFindingLevels(id int64, impact, evidence *int) error {
	if impact != nil && !ValidFindingLevel(*impact) {
		return ErrFindingLevelInvalid
	}
	if evidence != nil && !ValidFindingLevel(*evidence) {
		return ErrFindingLevelInvalid
	}
	if impact == nil && evidence == nil {
		return nil
	}
	if err := d.EnsureFindingLevelsSchema(); err != nil {
		return err
	}
	var curImpact, requested int
	var evidenceText string
	err := d.QueryRow(`SELECT impact_level, evidence_level_requested, COALESCE(evidence,'')
		FROM findings WHERE id=$1`, id).Scan(&curImpact, &requested, &evidenceText)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrFindingNotFound
	}
	if err != nil {
		return err
	}
	if impact != nil {
		curImpact = *impact
	}
	if evidence != nil {
		requested = *evidence
	}
	var reproduced bool
	if err := d.QueryRow(`SELECT EXISTS(SELECT 1 FROM finding_retests WHERE finding_id=$1 AND verdict='reproduced')`, id).Scan(&reproduced); err != nil {
		return err
	}
	applied, reason, err := CapEvidenceLevel(requested, evidenceText, reproduced)
	if err != nil {
		return err
	}
	return d.writeFindingLevels(id, curImpact, applied, requested, reason)
}

// RefreshFindingEvidenceLevel 按当前证据正文和是否已有 reproduced 复测，
// 重新套用上限。只把证据等级往自报值回升或维持，不会升过自报，也不会改影响层级。
// 存量行自报默认是 1，所以复测通过不会把没人申请过 L4 的旧记录抬上去。
func (d *DB) RefreshFindingEvidenceLevel(findingID int64) error {
	if findingID <= 0 {
		return nil
	}
	if err := d.EnsureFindingLevelsSchema(); err != nil {
		return err
	}
	var evidence string
	var impact, current, requested int
	var reason string
	err := d.QueryRow(`SELECT COALESCE(evidence,''), impact_level, evidence_level, evidence_level_requested, COALESCE(evidence_level_reason,'')
		FROM findings WHERE id=$1`, findingID).Scan(&evidence, &impact, &current, &requested, &reason)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var reproduced bool
	if err := d.QueryRow(`SELECT EXISTS(SELECT 1 FROM finding_retests WHERE finding_id=$1 AND verdict='reproduced')`, findingID).Scan(&reproduced); err != nil {
		return err
	}
	applied, newReason, err := CapEvidenceLevel(requested, evidence, reproduced)
	if err != nil {
		return err
	}
	if applied == current && newReason == reason {
		return nil
	}
	return d.writeFindingLevels(findingID, impact, applied, requested, newReason)
}

// MaxImpactLevel 返回任务下 finding 的最高影响层级。没有 finding 时返回 0。
func (d *DB) MaxImpactLevel(taskID int64) (int, error) {
	if taskID <= 0 {
		return 0, nil
	}
	if err := d.EnsureFindingLevelsSchema(); err != nil {
		return 0, err
	}
	var n int
	err := d.QueryRow(`SELECT COALESCE(MAX(impact_level), 0) FROM findings WHERE task_id=$1`, taskID).Scan(&n)
	return n, err
}

// AddFinding inserts a finding into the standalone findings table. taskID and
// nodeID may be 0 (stored as NULL). name may be "" (frontend falls back to
// vulnclass). Returns the new finding id.
func (d *DB) AddFinding(taskID, nodeID int64, vulnclass, name, severity, summary, evidence, worker string, assetIDs []int64) (int64, error) {
	aidsJSON, _ := json.Marshal(assetIDs)
	if assetIDs == nil {
		aidsJSON = []byte("[]")
	}
	var tid, nid *int64
	if taskID > 0 {
		tid = &taskID
	}
	if nodeID > 0 {
		nid = &nodeID
	}
	var id int64
	err := d.QueryRow(
		`INSERT INTO findings (task_id, node_id, vulnclass, name, severity, summary, evidence, worker, asset_ids)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING id`,
		tid, nid, vulnclass, name, severity, summary, evidence, worker, string(aidsJSON),
	).Scan(&id)
	return id, err
}

// findingSelectCols is the column list (with task_description join) every finding
// list query selects, so scanFinding stays in sync across callers.
const findingSelectCols = `f.id, f.task_id, f.node_id, f.vulnclass, COALESCE(f.name, ''), f.severity, f.summary,
	       f.evidence, f.worker, f.asset_ids, COALESCE(f.status, 'pending'), f.created_at,
	       COALESCE(t.description, '') AS task_description, f.evidence_version, f.report_evidence_version,
	       COALESCE(f.evidence_status, 'na'), COALESCE(f.root_cause_key, ''),
 (SELECT count(*) FROM finding_traffic_bindings b WHERE b.finding_id=f.id),
 f.impact_level, f.evidence_level, f.evidence_level_requested, COALESCE(f.evidence_level_reason, '')`

// scanFindingRow 按 findingSelectCols 的顺序读一行。withReport 为真时再读末尾的 report。
func scanFindingRow(scanner interface{ Scan(...any) error }, withReport bool) (*DBFinding, error) {
	f := &DBFinding{}
	var aidsJSON string
	dest := []any{
		&f.ID, &f.TaskID, &f.NodeID, &f.VulnClass, &f.Name, &f.Severity,
		&f.Summary, &f.Evidence, &f.Worker, &aidsJSON, &f.Status, &f.CreatedAt,
		&f.TaskDescription, &f.EvidenceVersion, &f.ReportEvidenceVersion,
		&f.EvidenceStatus, &f.RootCauseKey, &f.TrafficCount,
		&f.ImpactLevel, &f.EvidenceLevel, &f.EvidenceLevelRequested, &f.EvidenceLevelReason,
	}
	if withReport {
		dest = append(dest, &f.Report)
	}
	if err := scanner.Scan(dest...); err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(aidsJSON), &f.AssetIDs)
	return f, nil
}

// scanFindings materializes rows selected via findingSelectCols.
func scanFindings(rows interface {
	Next() bool
	Scan(...any) error
	Err() error
}) ([]*DBFinding, error) {
	var out []*DBFinding
	for rows.Next() {
		f, err := scanFindingRow(rows, false)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// ListFindings returns all findings (newest first), joined with task description.
// Kept for the dashboard's summary; the paginated 发现 page uses ListFindingsPage.
func (d *DB) ListFindings(limit int) ([]*DBFinding, error) {
	if err := d.EnsureFindingLevelsSchema(); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 500
	}
	rows, err := d.Query(`
		SELECT `+findingSelectCols+`
		FROM findings f
		LEFT JOIN tasks t ON f.task_id = t.id
		ORDER BY f.created_at DESC
		LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanFindings(rows)
}

// FindingFilter narrows a paginated findings query. Empty-string fields mean "no
// filter on that column". Sort is "severity" (severity desc, then newest) or
// anything else (newest first).
type FindingFilter struct {
	Severity  string // high | medium | low
	Status    string // pending | false_positive | ignored | resolved
	VulnClass string
	TaskID    string // 任务 id(逗号分隔可多值——0.15.43 多任务合并导出;空/全非法 = 不按任务筛选)
	Query     string // 名称/类型/摘要/证据/报告正文的模糊检索关键词
	Sort      string // "severity" | "time"
	// AssetScope 是资产树的节点 key(a:<id> / c:<id> / r:<domain> / __none__),
	// 选中一个节点等于选中它的整棵子树。空 = 不按资产筛选。
	AssetScope string

	// 下面三个由 applyAssetScope 从 AssetScope 解析而来,调用方不用设置。
	assetIDs  []int64 // 子树里所有资产 id
	assetNone bool    // 只要「未关联资产」的发现
	assetMiss bool    // 选中的节点在当前筛选下不存在 → 结果恒空
}

// FindingUnassignedTask is the task filter sentinel for findings whose task is
// absent. That includes rows created without a task and rows retained after their
// originating task was deleted (the findings FK is ON DELETE SET NULL).
const FindingUnassignedTask = "__unassigned__"

// where builds the WHERE clause (shared by the page and count queries) plus its
// positional args. All values are parameterized; Query also escapes ILIKE
// wildcards so user input is always matched literally.
func (f FindingFilter) where() (string, []any) {
	var conds []string
	var args []any
	add := func(col, val string) {
		if val == "" {
			return
		}
		args = append(args, val)
		conds = append(conds, fmt.Sprintf("f.%s = $%d", col, len(args)))
	}
	add("severity", f.Severity)
	add("status", f.Status)
	add("vulnclass", f.VulnClass)
	// task_id 是 bigint 列,按整数比较(不能走上面的文本 add);空/全非法值忽略。
	// 0.15.43 #8 多任务合并导出：逗号分隔多值（向后兼容单值）——每个值可为
	// 任务号或 __unassigned__ 哨兵，非法项忽略，全部 OR 连接。
	if f.TaskID == FindingUnassignedTask {
		conds = append(conds, "(f.task_id IS NULL OR t.id IS NULL)")
	} else if f.TaskID != "" {
		var ors []string
		unassigned := false
		for _, part := range strings.Split(f.TaskID, ",") {
			part = strings.TrimSpace(part)
			if part == FindingUnassignedTask {
				unassigned = true
				continue
			}
			if tid, err := strconv.ParseInt(part, 10, 64); err == nil && tid > 0 {
				args = append(args, tid)
				ors = append(ors, fmt.Sprintf("f.task_id = $%d", len(args)))
			}
		}
		if unassigned {
			ors = append(ors, "(f.task_id IS NULL OR t.id IS NULL)")
		}
		if len(ors) > 0 {
			conds = append(conds, "("+strings.Join(ors, " OR ")+")")
		}
	}
	// 资产筛选:asset_ids 是 jsonb 数组,@> ANY(...) 能走 idx_findings_asset_ids。
	switch {
	case f.assetMiss:
		conds = append(conds, "FALSE")
	case f.assetNone:
		// 「未关联资产」= asset_ids 为空,或者里面的 id 一个都不在 assets 表里
		// (资产已被删除)。两类都进资产树的未关联桶,这里必须同样收下,否则桶上
		// 的计数会大于点开后能查到的条数。
		conds = append(conds, `(
			jsonb_array_length(COALESCE(f.asset_ids, '[]'::jsonb)) = 0
			OR NOT EXISTS (
				SELECT 1 FROM jsonb_array_elements_text(f.asset_ids) e(v)
				JOIN assets a ON a.id = e.v::bigint
			)
		)`)
	case len(f.assetIDs) > 0:
		args = append(args, assetIDContainments(f.assetIDs))
		conds = append(conds, fmt.Sprintf("f.asset_ids @> ANY($%d::jsonb[])", len(args)))
	}
	if query := strings.TrimSpace(f.Query); query != "" {
		escaped := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(query)
		args = append(args, "%"+escaped+"%")
		placeholder := fmt.Sprintf("$%d", len(args))
		conds = append(conds, fmt.Sprintf(`(
			COALESCE(f.name, '') ILIKE %s ESCAPE '\' OR
			f.vulnclass ILIKE %s ESCAPE '\' OR
			f.summary ILIKE %s ESCAPE '\' OR
			f.evidence ILIKE %s ESCAPE '\' OR
			COALESCE(f.report, '') ILIKE %s ESCAPE '\'
		)`, placeholder, placeholder, placeholder, placeholder, placeholder))
	}
	if len(conds) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

// ListFindingsPage returns one page of findings matching the filter, plus the
// total count of matching rows (for the frontend pager). page is 1-based.
func (d *DB) ListFindingsPage(f FindingFilter, page, pageSize int) ([]*DBFinding, int, error) {
	if err := d.EnsureFindingLevelsSchema(); err != nil {
		return nil, 0, err
	}
	if page <= 0 {
		page = 1
	}
	if pageSize <= 0 {
		pageSize = 20
	}
	f, err := d.applyAssetScope(f)
	if err != nil {
		return nil, 0, err
	}
	where, args := f.where()

	var total int
	if err := d.QueryRow(`SELECT COUNT(*) FROM findings f LEFT JOIN tasks t ON f.task_id=t.id`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	// Avoid overflowing (page-1)*pageSize for an arbitrarily large page number.
	// Once the requested page is beyond the exact count, no data query is needed.
	if total == 0 || page > (total-1)/pageSize+1 {
		return []*DBFinding{}, total, nil
	}

	order := "f.created_at DESC, f.id DESC"
	if f.Sort == "severity" {
		// critical > high > medium > low > 其它, then newest first.
		order = `CASE f.severity WHEN 'critical' THEN 4 WHEN 'high' THEN 3 WHEN 'medium' THEN 2 WHEN 'low' THEN 1 WHEN 'info' THEN 0 ELSE 0 END DESC, f.created_at DESC, f.id DESC`
	}
	pageArgs := append(append([]any{}, args...), pageSize, (page-1)*pageSize)
	q := fmt.Sprintf(`
		SELECT %s
		FROM findings f
		LEFT JOIN tasks t ON f.task_id = t.id%s
		ORDER BY %s
		LIMIT $%d OFFSET $%d`, findingSelectCols, where, order, len(args)+1, len(args)+2)
	rows, err := d.Query(q, pageArgs...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out, err := scanFindings(rows)
	return out, total, err
}

// FindingGroup is one task-level bucket in the global findings view. TaskID is
// nil for both findings that never had a task and findings retained after task
// deletion; those records intentionally share one "unassigned/deleted" bucket.
type FindingGroup struct {
	TaskID          *int64    `json:"task_id"`
	TaskName        string    `json:"task_name"` // 可选任务名称;空=未命名
	TaskDescription string    `json:"task_description"`
	TaskStatus      string    `json:"task_status"`
	Count           int       `json:"count"`
	Critical        int       `json:"critical"`
	High            int       `json:"high"`
	Medium          int       `json:"medium"`
	Low             int       `json:"low"`
	Info            int       `json:"info"`
	LastFoundAt     time.Time `json:"last_found_at"`
}

// ListFindingGroups returns a page of task groups matching the same filters as
// ListFindingsPage. The group count and finding count are independent totals so
// clients can page groups without losing the exact export/selection count.
func (d *DB) ListFindingGroups(f FindingFilter, page, pageSize int) ([]FindingGroup, int, int, error) {
	if page <= 0 {
		page = 1
	}
	if pageSize <= 0 {
		pageSize = 10
	}
	f, err := d.applyAssetScope(f)
	if err != nil {
		return nil, 0, 0, err
	}
	where, args := f.where()
	grouped := ` FROM findings f LEFT JOIN tasks t ON f.task_id=t.id` + where +
		` GROUP BY t.id, t.name, t.description, t.status, t.paused, t.queued`

	var groupTotal, findingTotal int
	countQuery := `SELECT COUNT(*), COALESCE(SUM(finding_count),0) FROM (` +
		`SELECT COUNT(*) AS finding_count` + grouped + `) grouped_findings`
	if err := d.QueryRow(countQuery, args...).Scan(&groupTotal, &findingTotal); err != nil {
		return nil, 0, 0, err
	}
	if groupTotal == 0 || page > (groupTotal-1)/pageSize+1 {
		return []FindingGroup{}, groupTotal, findingTotal, nil
	}

	order := "MAX(f.created_at) DESC, t.id DESC NULLS LAST"
	if f.Sort == "severity" {
		order = `MAX(CASE f.severity WHEN 'critical' THEN 4 WHEN 'high' THEN 3 WHEN 'medium' THEN 2 WHEN 'low' THEN 1 WHEN 'info' THEN 0 ELSE 0 END) DESC, MAX(f.created_at) DESC, t.id DESC NULLS LAST`
	}
	pageArgs := append(append([]any{}, args...), pageSize, (page-1)*pageSize)
	query := fmt.Sprintf(`SELECT t.id, COALESCE(t.name,''), COALESCE(t.description,''), COALESCE(
		CASE
			WHEN t.status IN ('done','failed','timeout') THEN t.status
			WHEN t.queued THEN 'queued'
			WHEN t.paused THEN 'paused'
			ELSE t.status
		END, ''),
		COUNT(*),
		COUNT(*) FILTER (WHERE f.severity='critical'),
		COUNT(*) FILTER (WHERE f.severity='high'),
		COUNT(*) FILTER (WHERE f.severity='medium'),
		COUNT(*) FILTER (WHERE f.severity='low'),
		COUNT(*) FILTER (WHERE f.severity='info'),
		MAX(f.created_at)%s
		ORDER BY %s LIMIT $%d OFFSET $%d`, grouped, order, len(args)+1, len(args)+2)
	rows, err := d.Query(query, pageArgs...)
	if err != nil {
		return nil, 0, 0, err
	}
	defer rows.Close()
	groups := []FindingGroup{}
	for rows.Next() {
		var group FindingGroup
		var taskID sql.NullInt64
		if err := rows.Scan(&taskID, &group.TaskName, &group.TaskDescription, &group.TaskStatus, &group.Count,
			&group.Critical, &group.High, &group.Medium, &group.Low, &group.Info, &group.LastFoundAt); err != nil {
			return nil, 0, 0, err
		}
		if taskID.Valid {
			id := taskID.Int64
			group.TaskID = &id
		}
		groups = append(groups, group)
	}
	return groups, groupTotal, findingTotal, rows.Err()
}

// ErrFindingOriginUnavailable means a retained finding no longer has a live
// owning task and finding node from which a follow-up intent can be derived.
var ErrFindingOriginUnavailable = errors.New("finding origin is no longer available")

// AddFindingFollowUpIntent atomically creates a priority-10 human intent from a
// live finding node, copies that finding's asset anchors, records the
// finding --derived_from--> intent lineage edge, and persists its audit activity.
// The returned activity is the committed row and can be broadcast as-is without
// calling AppendActivity again.
func (s *ExplorationStore) AddFindingFollowUpIntent(findingID, findingNodeID int64, description string, audit Activity) (int64, Activity, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, Activity{}, err
	}
	defer tx.Rollback()

	var liveNodeID int64
	err = tx.QueryRow(`SELECT n.id
		FROM findings f
		JOIN tasks t ON t.id=f.task_id
		JOIN exploration_nodes n ON n.id=f.node_id AND n.exploration_id=t.exploration_id
		WHERE f.id=$1 AND f.node_id=$2 AND t.exploration_id=$3 AND n.kind='finding'
		FOR SHARE OF f, t, n`, findingID, findingNodeID, s.expID).Scan(&liveNodeID)
	if err == sql.ErrNoRows {
		return 0, Activity{}, ErrFindingOriginUnavailable
	}
	if err != nil {
		return 0, Activity{}, err
	}

	anchors := []int64{}
	anchorRows, err := tx.Query(`SELECT asset_id FROM exploration_anchors WHERE node_id=$1 ORDER BY asset_id`, liveNodeID)
	if err != nil {
		return 0, Activity{}, err
	}
	for anchorRows.Next() {
		var assetID int64
		if err := anchorRows.Scan(&assetID); err != nil {
			anchorRows.Close()
			return 0, Activity{}, err
		}
		anchors = append(anchors, assetID)
	}
	if err := anchorRows.Err(); err != nil {
		anchorRows.Close()
		return 0, Activity{}, err
	}
	if err := anchorRows.Close(); err != nil {
		return 0, Activity{}, err
	}

	payload := map[string]any{
		"summary":                description,
		"source_finding_id":      findingID,
		"source_finding_node_id": liveNodeID,
	}
	if len(anchors) > 0 {
		payload["asset_ids"] = anchors
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return 0, Activity{}, err
	}
	var intentID int64
	if err := tx.QueryRow(`INSERT INTO exploration_nodes(exploration_id,kind,payload,priority,state,origin)
		VALUES ($1,'intent',$2,10,'open','human') RETURNING id`, s.expID, raw).Scan(&intentID); err != nil {
		return 0, Activity{}, err
	}
	if _, err := tx.Exec(`INSERT INTO exploration_anchors(node_id,asset_id)
		SELECT $1, asset_id FROM exploration_anchors WHERE node_id=$2
		ON CONFLICT DO NOTHING`, intentID, liveNodeID); err != nil {
		return 0, Activity{}, err
	}
	if _, err := tx.Exec(`INSERT INTO exploration_edges(exploration_id,src_id,rel,dst_id)
		VALUES ($1,$2,$3,$4)`, s.expID, liveNodeID, RelDerivedFrom, intentID); err != nil {
		return 0, Activity{}, err
	}

	audit.NodeID = &intentID
	if summary := strings.TrimSpace(audit.Summary); summary != "" {
		audit.Summary = fmt.Sprintf("%s #%d", summary, intentID)
	} else {
		audit.Summary = ""
	}
	metadata := audit.Metadata
	if len(metadata) == 0 {
		metadata = json.RawMessage(`{}`)
	}
	if err := tx.QueryRow(`
INSERT INTO activity(exploration_id, node_id, worker, kind, tool, tool_use_id, is_error, summary, detail, metadata, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens)
VALUES ($1,$2,NULLIF($3,''),NULLIF($4,''),NULLIF($5,''),NULLIF($6,''),$7,NULLIF($8,''),NULLIF($9,''),$10,$11,$12,$13,$14)
RETURNING id, created_at`, s.expID, audit.NodeID, utf8Clean(audit.Worker), utf8Clean(audit.Kind), utf8Clean(audit.Tool), utf8Clean(audit.ToolUseID), audit.IsError,
		utf8Clean(audit.Summary), utf8Clean(audit.Detail), metadata, audit.InputTokens, audit.OutputTokens, audit.CacheReadTokens, audit.CacheWriteTokens).
		Scan(&audit.ID, &audit.CreatedAt); err != nil {
		return 0, Activity{}, err
	}
	audit.Metadata = metadata
	if err := tx.Commit(); err != nil {
		return 0, Activity{}, err
	}
	return intentID, audit, nil
}

// ListFindingsForExport returns findings for the 发现 page 导出功能，携带完整
// report 字段、不分页。ids 非空时按这批 finding id 精确导出(勾选导出),忽略
// filter;ids 为空时按 filter 导出(导出当前筛选/全部)。结果按严重等级降序、
// 再按时间倒序,与「导出汇总报告」的分组顺序一致。
func (d *DB) ListFindingsForExport(f FindingFilter, ids []int64) ([]*DBFinding, error) {
	if err := d.EnsureFindingLevelsSchema(); err != nil {
		return nil, err
	}
	const order = `ORDER BY CASE f.severity WHEN 'critical' THEN 4 WHEN 'high' THEN 3 WHEN 'medium' THEN 2 WHEN 'low' THEN 1 WHEN 'info' THEN 0 ELSE 0 END DESC, f.created_at DESC`
	cols := findingSelectCols + `, COALESCE(f.report, '')`

	var q string
	var args []any
	if len(ids) > 0 {
		ph := make([]string, len(ids))
		for i, id := range ids {
			ph[i] = fmt.Sprintf("$%d", i+1)
			args = append(args, id)
		}
		q = `SELECT ` + cols + `
			FROM findings f
			LEFT JOIN tasks t ON f.task_id = t.id
			WHERE f.id IN (` + strings.Join(ph, ",") + `)
			` + order
	} else {
		scoped, err := d.applyAssetScope(f)
		if err != nil {
			return nil, err
		}
		where, wargs := scoped.where()
		q = `SELECT ` + cols + `
			FROM findings f
			LEFT JOIN tasks t ON f.task_id = t.id` + where + `
			` + order
		args = wargs
	}

	rows, err := d.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*DBFinding
	for rows.Next() {
		f, err := scanFindingRow(rows, true)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// FindingStats is the whole-table aggregate powering the 发现 page's stat cards
// and vuln-class filter — computed server-side so it stays exact regardless of
// pagination.
type FindingStats struct {
	Total       int                 `json:"total"`
	Pending     int                 `json:"pending"`
	Critical    int                 `json:"critical"`
	High        int                 `json:"high"`
	Medium      int                 `json:"medium"`
	Low         int                 `json:"low"`
	Info        int                 `json:"info"`
	VulnClasses []string            `json:"vulnclasses"`
	Tasks       []FindingTaskOption `json:"tasks"` // 有漏洞的任务(供「按任务」下拉)
}

// FindingTaskOption is one entry in the 发现 page's 任务 filter: a task that has at
// least one finding, with its description and finding count. Description is empty when
// the task has since been deleted (finding rows persist), so the frontend falls back to
// the id.
type FindingTaskOption struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"` // 可选任务名称;空=未命名
	Description string `json:"description"`
	Count       int    `json:"count"`
}

// FindingStats returns whole-table counts (by severity + pending) and the sorted
// set of distinct vuln classes.
func (d *DB) FindingStats() (*FindingStats, error) {
	st := &FindingStats{VulnClasses: []string{}, Tasks: []FindingTaskOption{}}
	err := d.QueryRow(`SELECT
		COUNT(*),
		COUNT(*) FILTER (WHERE status = 'pending'),
		COUNT(*) FILTER (WHERE severity = 'critical'),
		COUNT(*) FILTER (WHERE severity = 'high'),
		COUNT(*) FILTER (WHERE severity = 'medium'),
		COUNT(*) FILTER (WHERE severity = 'low'),
		COUNT(*) FILTER (WHERE severity = 'info')
		FROM findings`).Scan(&st.Total, &st.Pending, &st.Critical, &st.High, &st.Medium, &st.Low, &st.Info)
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT DISTINCT vulnclass FROM findings WHERE vulnclass <> '' ORDER BY vulnclass`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var vc string
		if err := rows.Scan(&vc); err != nil {
			return nil, err
		}
		st.VulnClasses = append(st.VulnClasses, vc)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// 任务下拉:有漏洞的任务,带描述(任务删除后为空,前端回退 id)和条数,最新有漏洞的排前。
	trows, err := d.Query(`SELECT f.task_id, COALESCE(t.name, ''), COALESCE(t.description, ''), COUNT(*)
		FROM findings f
		LEFT JOIN tasks t ON f.task_id = t.id
		WHERE f.task_id IS NOT NULL
		GROUP BY f.task_id, t.name, t.description
		ORDER BY MAX(f.created_at) DESC`)
	if err != nil {
		return nil, err
	}
	defer trows.Close()
	for trows.Next() {
		var opt FindingTaskOption
		if err := trows.Scan(&opt.ID, &opt.Name, &opt.Description, &opt.Count); err != nil {
			return nil, err
		}
		st.Tasks = append(st.Tasks, opt)
	}
	if err := trows.Err(); err != nil {
		return nil, err
	}
	archived, err := d.archivedTaskAggregates()
	if err != nil {
		return nil, err
	}
	vulnclasses := make(map[string]bool, len(st.VulnClasses))
	for _, vulnclass := range st.VulnClasses {
		vulnclasses[vulnclass] = true
	}
	for _, aggregate := range archived {
		cold := aggregate.FindingStats
		st.Total += cold.Total
		st.Pending += cold.Pending
		st.Critical += cold.Critical
		st.High += cold.High
		st.Medium += cold.Medium
		st.Low += cold.Low
		for _, vulnclass := range cold.VulnClasses {
			if vulnclass != "" {
				vulnclasses[vulnclass] = true
			}
		}
	}
	st.VulnClasses = st.VulnClasses[:0]
	for vulnclass := range vulnclasses {
		st.VulnClasses = append(st.VulnClasses, vulnclass)
	}
	sort.Strings(st.VulnClasses)
	return st, nil
}

// GetFinding returns a single finding row (with task_description joined and the
// full Markdown report), or nil when no row has that id. Unlike the list queries
// it also selects `report` — that column is only needed on the detail page.
func (d *DB) GetFinding(id int64) (*DBFinding, error) {
	if err := d.EnsureFindingLevelsSchema(); err != nil {
		return nil, err
	}
	row := d.QueryRow(`SELECT `+findingSelectCols+`, COALESCE(f.report, '')
		FROM findings f
		LEFT JOIN tasks t ON f.task_id = t.id
		WHERE f.id = $1`, id)
	f, err := scanFindingRow(row, true)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return f, nil
}

// DeleteFinding removes a finding entirely: the standalone findings row and its
// originating exploration node (kind='finding'), so it disappears from the findings
// list, the per-task 发现 Tab, and the exploration graph alike. Deleting the node
// cascades its edges + node_assets and nulls any activity referencing it. Returns
// rows affected (0 = no finding with that id).
func (d *DB) DeleteFinding(id int64) (n int64, err error) {
	err = d.WithEvidenceTx(context.Background(), func(tx *sql.Tx) error {
		if err := LockFindingEvidenceTx(tx, id, nil); err != nil {
			if errors.Is(err, ErrFindingNotFound) {
				return nil
			}
			return err
		}
		var nodeID sql.NullInt64
		if err := tx.QueryRow(`DELETE FROM findings WHERE id=$1 RETURNING node_id`, id).Scan(&nodeID); err != nil {
			return err
		}
		if nodeID.Valid {
			if _, err := tx.Exec(`DELETE FROM exploration_nodes WHERE id=$1 AND kind='finding'`, nodeID.Int64); err != nil {
				return err
			}
		}
		n = 1
		return nil
	})
	return
}

// DeleteFindingsByTask removes all findings rows of a task. The originating
// exploration finding nodes are cascade-deleted separately when the task's
// exploration subgraph is dropped. Returns rows deleted.
func (d *DB) DeleteFindingsByTask(taskID int64) (int64, error) {
	res, err := d.Exec(`DELETE FROM findings WHERE task_id=$1`, taskID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// SetFindingStatus updates one finding's triage state. Returns rows affected.
//
// 底层 setter：只改状态、不登记推送事件。生产代码改状态请走
// SetFindingStatusWithNotify —— 直接调本函数会让「状态变更推送」静默失效。
// 保留它是为了让不关心通知的用例（参数校验、复测流程）能单独驱动状态。
func (d *DB) SetFindingStatus(id int64, status string) (int64, error) {
	res, err := d.Exec(`UPDATE findings SET status=$1 WHERE id=$2`, status, id)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// SetFindingReportByNodeID sets the Markdown report on the standalone finding row
// whose node_id matches — report_finding returns that node id, so an agent tool
// can address the finding it just created. Returns rows affected (0 when no row).
func (d *DB) SetFindingReportByNodeID(nodeID int64, report string) (int64, error) {
	return d.SetFindingReportVersionByNodeID(context.Background(), nodeID, report, nil)
}

// setFindingCol updates one text column on the standalone finding row AND mirrors
// the new value into the originating exploration node's payload under jsonKey, so the
// per-task 发现 Tab (which reads the node payload, not this table) stays in sync.
// Returns rows affected (0 when no finding has that id); the node sync is best-effort.
// col and jsonKey MUST be trusted constants (they are interpolated into SQL) — never
// pass user input.
func (d *DB) setFindingCol(id int64, col, jsonKey, val string) (int64, error) {
	var nodeID *int64
	err := d.QueryRow(`UPDATE findings SET `+col+`=$1 WHERE id=$2 RETURNING node_id`, val, id).Scan(&nodeID)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if nodeID != nil {
		_, _ = d.Exec(`UPDATE exploration_nodes
			SET payload = jsonb_set(payload, '{`+jsonKey+`}', to_jsonb($1::text))
			WHERE id = $2`, val, *nodeID)
	}
	return 1, nil
}

// SetFindingSeverity updates one finding's severity (+ node payload sync). Returns
// rows affected (0 when no finding has that id).
func (d *DB) SetFindingSeverity(id int64, severity string) (int64, error) {
	return d.setFindingCol(id, "severity", "severity", severity)
}

// SetFindingName updates one finding's 漏洞名称 (+ node payload sync). Empty name is
// allowed — the frontend falls back to the vuln class for display.
func (d *DB) SetFindingName(id int64, name string) (int64, error) {
	return d.setFindingCol(id, "name", "name", name)
}

// SetFindingVulnClass updates one finding's 漏洞类别 (+ node payload sync).
func (d *DB) SetFindingVulnClass(id int64, vulnclass string) (int64, error) {
	return d.setFindingCol(id, "vulnclass", "vulnclass", vulnclass)
}

// ErrFindingReplayEvidenceMissing 表示这条漏洞还没有可重放证据。
//
// 为什么要有这个错误：db/sod.go 的 GetFindingReplayEvidence 对「没有可重放证据」
// 返回的是一个**空字符串**（ReplayEvidenceID == ""）—— 闸门（internal/sod/gate.go 的
// tier2 检查）正是靠这个空值把动作判成 tier2_inconsistent，所以那条读路径不能改：
// 改了闸门的原因码就从 tier2_inconsistent 变成 tier2_check_unavailable，与
// docs/SoD链覆盖边界.md 登记的口径不符。
//
// 而对「我需要知道这条漏洞到底能不能重放」的调用方，空字符串是个**静默**的缺口：
// 调用方必须自己记得判空，忘了就是拿一条不可重放的结论去放行。本错误把那个缺口
// 变成一个**可区分**的失败。
var ErrFindingReplayEvidenceMissing = errors.New("该漏洞尚无可重放证据")

// GetFindingReplayEvidenceStrict 读 finding 的重放证据；缺证据时报错。
//
// finding 不存在 ⇒ (nil, nil)（与 GetFindingReplayEvidence 一致：调用方按「不适用」
// 处理）。finding 存在但没有可重放证据 ⇒ ErrFindingReplayEvidenceMissing。
func (d *DB) GetFindingReplayEvidenceStrict(findingID int64) (*ReplayEvidence, error) {
	ev, err := d.GetFindingReplayEvidence(findingID)
	if err != nil {
		return nil, err
	}
	if ev == nil {
		return nil, nil
	}
	if ev.ReplayEvidenceID == "" {
		return nil, fmt.Errorf("%w: finding %d", ErrFindingReplayEvidenceMissing, findingID)
	}
	return ev, nil
}

// FindingMeta is the standalone-row data (id, triage state, anchored assets) the
// per-task view grafts onto its exploration-node findings.
type FindingMeta struct {
	TrafficCount int

	ID       int64
	Status   string
	AssetIDs []int64
	// ValidationState 是这条漏洞的**验证态**（exploration_nodes.state）：
	// pending（已提出，未验证）/ confirmed（验证通过）/ dismissed（不成立）。
	// 空 = 这条漏洞没有对应的探索节点（或节点已被删）。
	//
	// ⚠️ 与 Status 不是一个东西：Status 是 findings.status 的**处置**状态（待处理 /
	// 已确认 / 已修复…），ValidationState 回答的是「这条结论有没有被独立复核过」。
	// 两组语义混在一起正是本单元要消灭的混淆。
	ValidationState string
}

// FindingMetaByNodeID maps a task's finding node ids to their standalone-row
// metadata (status + anchored asset ids) via the asset store, so callers holding
// only an AssetStore (e.g. the agent ToolSet) can reach it without a raw *DB.
func (a *AssetStore) FindingMetaByNodeID(taskID int64) (map[int64]FindingMeta, error) {
	return a.db.FindingMetaByNodeID(taskID)
}

// FindingMetaByNodeID maps a task's finding node ids to their standalone-row
// metadata, so the per-task view (which reads exploration nodes) can show and
// edit the same status — and the same anchored assets — as the global 发现 page.
func (d *DB) FindingMetaByNodeID(taskID int64) (map[int64]FindingMeta, error) {
	out := map[int64]FindingMeta{}
	if taskID <= 0 {
		return out, nil
	}
	rows, err := d.Query(`SELECT f.node_id, f.id, COALESCE(f.status,'pending'), f.asset_ids,
 (SELECT count(*) FROM finding_traffic_bindings b WHERE b.finding_id=f.id),
 COALESCE(n.state,'')
FROM findings f LEFT JOIN exploration_nodes n ON n.id = f.node_id
WHERE f.task_id=$1 AND f.node_id IS NOT NULL`, taskID)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var nid int64
		var m FindingMeta
		var aidsJSON string
		if err := rows.Scan(&nid, &m.ID, &m.Status, &aidsJSON, &m.TrafficCount, &m.ValidationState); err != nil {
			return out, err
		}
		_ = json.Unmarshal([]byte(aidsJSON), &m.AssetIDs)
		out[nid] = m
	}
	return out, rows.Err()
}

// ---------- E1-7 存量重评（评级基准显式化后的收尾） ----------
//
// 背景：severity 枚举校验与 info 档是 v0.10.1 才上的——此前的存量行里有
// ① 乱值 severity（当时的透传通道）②「纯信息泄露却给了 low/medium」的虚高行。
// 这里只出**候选**不改库：应用走 SetFindingSeverity，apply=true 由人拍板。

// RerateCandidate 是一条重评候选：Current→Proposed 的改法与触发规则。
type RerateCandidate struct {
	ID        int64  `json:"id"`
	TaskID    int64  `json:"task_id"`
	VulnClass string `json:"vulnclass"`
	Name      string `json:"name"`
	Current   string `json:"current"`
	Proposed  string `json:"proposed"`
	Rule      string `json:"rule"`
}

// rerateCredentialRe 是「凭据/利用链特征」标记：命中其一 ⇒ 这条 info_disclosure
// 可能真有升级链，**不**进候选（留给人工/复核判——确定性层只收无争议的）。
var rerateCredentialRe = regexp.MustCompile(
	`(?i)(password|passwd|pwd=|token|secret|api[_-]?key|access[_-]?key|private[_-]?key|cookie|sessionid|jwt|authorization|注入成功|接管|rce|shell|凭据|口令|密码|利用链)`)

// rerateMaxRows 单次重评扫描的上限（无界扫描没有意义：候选是给人看的）。
const rerateMaxRows = 500

// RerateFindingCandidates 列出评级基准（E1-7）下需要重评的存量 finding 候选：
// ① severity 乱值（不在枚举内）→ info（无判级依据，只能落信息级）；
// ② 纯信息泄露（vulnclass=info_disclosure）且现级 > info，且证据/摘要均无
//
//	凭据链特征 → info（基准：纯信息泄露与暴露类一律 info）。
//
// taskID>0 时只扫该任务。只读，不改任何行。
func (d *DB) RerateFindingCandidates(taskID int64) ([]RerateCandidate, error) {
	q := `SELECT id, COALESCE(task_id,0), COALESCE(vulnclass,''), COALESCE(name,''), severity,
		     COALESCE(summary,''), COALESCE(evidence,'')
		FROM findings`
	args := []any{}
	if taskID > 0 {
		q += ` WHERE task_id=$1`
		args = append(args, taskID)
	}
	q += fmt.Sprintf(` ORDER BY id LIMIT %d`, rerateMaxRows)
	rows, err := d.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RerateCandidate{}
	for rows.Next() {
		var c RerateCandidate
		var summary, evidence string
		if err := rows.Scan(&c.ID, &c.TaskID, &c.VulnClass, &c.Name, &c.Current, &summary, &evidence); err != nil {
			return nil, err
		}
		if proposed, rule := reratePropose(c.VulnClass, c.Current, summary, evidence); rule != "" {
			c.Proposed, c.Rule = proposed, rule
			out = append(out, c)
		}
	}
	return out, rows.Err()
}

// reratePropose 按确定性规则给改评建议；rule 为空 = 不需要重评。
func reratePropose(vulnclass, severity, summary, evidence string) (proposed, rule string) {
	if !ValidSeverity(severity) {
		return SeverityInfo, "severity 乱值（历史行早于枚举校验），无判级依据 → info"
	}
	if vulnclass == "info_disclosure" && severity != SeverityInfo &&
		!rerateCredentialRe.MatchString(summary) && !rerateCredentialRe.MatchString(evidence) {
		return SeverityInfo, "纯信息泄露且证据/摘要未见凭据或利用链特征 → info（E1-7：一律 info）"
	}
	return "", ""
}

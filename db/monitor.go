package db

// monitor.go：Monitor 仓库监测存储模型（B-09B）——monitor_repos / monitor_findings
// 两张表的最小 CRUD（B-09A 核实结论落地：auth_ref 只存凭据引用，token 明文经
// db/crypto.go 加密面另行管理，本层零 token 字段）。
//
// 纪律：
//   - DDL 幂等独立文件（schema_monitor.sql，db/schema.sql 是 RESERVED），
//     EnsureMonitorSchema 访问层首次使用时应用，重复应用零副作用，不改既有表；
//   - 入参全部 fail-closed 校验：repo_url / github_base_url 只收 https 且 host
//     必填（GHE 自建域名合法）、branch 过 git ref 安全子集、last_seen_sha 只收
//     40 位小写十六进制或空串（游标绝不被污染值推进）；
//   - monitor_findings 状态白名单 new/pending_review/reproduced/ignored，其中
//     pending_review 是「模型失败/无法判断」的 fail-closed 落点（绝不把判不出
//     静默等价为「不是安全更新」）；CHECK 约束是数据层终闸，代码白名单先行
//     快速失败；
//   - (repo_id, sha) 唯一是发现去重的存储层最小闸（重判/重跑幂等收敛）；
//   - 零 API、零 GitHub 客户端、零模型调用——本层只是存储（拉取属 B-09D/F，
//     判定属 B-09E，操作属 B-09G）。
//
// 单租户模型：与 tasks/assets 既有面一致，无 tenant 列；created_by 是操作者
// 标识（审计溯源用自由串）。

import (
	"database/sql"
	_ "embed"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"
)

//go:embed schema_monitor.sql
var monitorSchemaSQL string

// monitorSchemaReady 每个 *DB 连接只应用一次 DDL（bizSchemaReady 同款）。
var monitorSchemaReady sync.Map

// EnsureMonitorSchema 应用 monitor 两张表的幂等 DDL（schema_monitor.sql）。
func (d *DB) EnsureMonitorSchema() error {
	if _, ok := monitorSchemaReady.Load(d.DB); ok {
		return nil
	}
	if _, err := d.Exec(monitorSchemaSQL); err != nil {
		return fmt.Errorf("应用 monitor schema 失败: %w", err)
	}
	monitorSchemaReady.Store(d.DB, struct{}{})
	return nil
}

// MonitorStatus 是 monitor_findings 的发现状态白名单。PendingReview 是模型
// 失败/无法判断的 fail-closed 落点（B-09B 在原卡三态上加列，如实申报）。
const (
	MonitorStatusNew           = "new"
	MonitorStatusPendingReview = "pending_review"
	MonitorStatusReproduced    = "reproduced"
	MonitorStatusIgnored       = "ignored"
)

// monitorStatuses 与 schema CHECK 约束同值（代码白名单快速失败，数据层终闸）。
var monitorStatuses = map[string]bool{
	MonitorStatusNew:           true,
	MonitorStatusPendingReview: true,
	MonitorStatusReproduced:    true,
	MonitorStatusIgnored:       true,
}

// MonitorRepo 是一个被监测的仓库配置。
type MonitorRepo struct {
	ID            int64
	RepoURL       string
	Branch        string
	GitHubBaseURL string
	AuthRef       string // 凭据引用（opaque），绝不携带 token 明文
	Enabled       bool
	LastSeenSHA   string // 40 位小写十六进制；空串=尚未拉取
	CreatedBy     string
	CreatedAt     time.Time
}

// MonitorFinding 是一条监测发现（被 Monitor 判为安全更新的提交）。
type MonitorFinding struct {
	ID        int64
	RepoID    int64
	SHA       string
	Title     string
	CVEID     string
	GHSAID    string
	Status    string
	CreatedAt time.Time
}

var (
	monitorSHARe    = regexp.MustCompile(`^[0-9a-f]{40}$`)
	monitorBranchRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,127}$`)
	monitorCVERe    = regexp.MustCompile(`^CVE-[0-9]{4}-[0-9]{4,}$`)
	monitorGHSARe   = regexp.MustCompile(`^GHSA-[A-Za-z0-9]{4}-[A-Za-z0-9]{4}-[A-Za-z0-9]{4}$`)
)

// monitorURLForm 校验 https URL 基本形态：https、host 必填、无 userinfo/
// 查询串/片段。GHE 自建域名合法（host 不做白名单——企业内网 GHE 是设计目标，
// 出网管控属运行环境层）。返回归一化 URL（去尾斜杠）。
func monitorURLForm(raw string) (string, error) {
	u := strings.TrimSpace(raw)
	if u == "" {
		return "", fmt.Errorf("URL 不能为空")
	}
	if !strings.HasPrefix(u, "https://") {
		return "", fmt.Errorf("URL 必须是 https://（got %q）", u)
	}
	rest := strings.TrimPrefix(u, "https://")
	if rest == "" || strings.HasPrefix(rest, "/") {
		return "", fmt.Errorf("URL 缺少 host（got %q）", u)
	}
	if strings.ContainsAny(rest, "?#") || strings.Contains(rest, "@") {
		return "", fmt.Errorf("URL 不允许 userinfo/查询串/片段（got %q）", u)
	}
	host := rest
	if i := strings.IndexAny(rest, "/"); i >= 0 {
		host = rest[:i]
	}
	if host == "" || strings.Contains(host, "..") {
		return "", fmt.Errorf("URL host 非法（got %q）", u)
	}
	return strings.TrimRight(u, "/"), nil
}

// monitorRepoURLForm 校验仓库 URL：https URL 基本形态之外要求 path 至少
// owner/repo 两段（GitHub 仓库最小形态）。
func monitorRepoURLForm(raw string) (string, error) {
	u, err := monitorURLForm(raw)
	if err != nil {
		return "", err
	}
	path := strings.TrimPrefix(u, "https://")
	if i := strings.Index(path, "/"); i >= 0 {
		path = path[i+1:]
	}
	segs := strings.Split(strings.Trim(path, "/"), "/")
	if len(segs) != 2 || segs[0] == "" || segs[1] == "" {
		return "", fmt.Errorf("仓库 URL 必须恰为 owner/repo 两段（got %q）", raw)
	}
	return u, nil
}

// monitorBaseURLForm 校验 GitHub API base URL：https URL 基本形态；允许
// 子路径前缀（GHE 形态 https://ghe.example.com/api/v3）。返回归一化 URL
// （去尾斜杠）。
func monitorBaseURLForm(raw string) (string, error) {
	return monitorURLForm(raw)
}

// monitorBranchForm 校验分支名：git ref 安全子集（字母/数字开头，1-128 字节，
// 字符集 [A-Za-z0-9._/-]，无 .. 序列——路径穿越与歧义 ref 拒绝）。
func monitorBranchForm(b string) (string, error) {
	b = strings.TrimSpace(b)
	if b == "" {
		return "", fmt.Errorf("分支名不能为空")
	}
	if !monitorBranchRe.MatchString(b) || strings.Contains(b, "..") || strings.HasSuffix(b, ".lock") {
		return "", fmt.Errorf("分支名非法（got %q）", b)
	}
	return b, nil
}

// monitorSHAForm 校验 commit sha：40 位小写十六进制，或空串（游标重置语义）。
func monitorSHAForm(sha string) error {
	if sha == "" || monitorSHARe.MatchString(sha) {
		return nil
	}
	return fmt.Errorf("sha 必须是 40 位小写十六进制（got %q）", sha)
}

// UpsertMonitorRepo 新建或更新仓库配置（按 (repo_url, branch) 幂等：已存在
// 则更新 enabled/auth_ref/github_base_url/created_by，绝不重置 last_seen_sha
// 游标）。authRef 只收引用串——调用方绝不传 token 明文（参数名即纪律）。
func (d *DB) UpsertMonitorRepo(repoURL, branch, baseURL, authRef, createdBy string, enabled bool) (int64, error) {
	u, err := monitorRepoURLForm(repoURL)
	if err != nil {
		return 0, err
	}
	b, err := monitorBranchForm(branch)
	if err != nil {
		return 0, err
	}
	if baseURL == "" {
		baseURL = "https://api.github.com"
	}
	bu, err := monitorBaseURLForm(baseURL)
	if err != nil {
		return 0, err
	}
	authRef = strings.TrimSpace(authRef)
	if authRef != "" && !monitorAuthRefForm(authRef) {
		return 0, fmt.Errorf("auth_ref 只允许引用形态（[A-Za-z0-9._:-]{1,128}），绝不存 token 明文")
	}
	if err := d.EnsureMonitorSchema(); err != nil {
		return 0, err
	}
	var id int64
	err = d.QueryRow(`
INSERT INTO monitor_repos(repo_url, branch, github_base_url, auth_ref, enabled, created_by)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (repo_url, branch) DO UPDATE SET
    github_base_url = EXCLUDED.github_base_url,
    auth_ref        = EXCLUDED.auth_ref,
    enabled         = EXCLUDED.enabled,
    created_by      = EXCLUDED.created_by
RETURNING id`, u, b, bu, authRef, enabled, strings.TrimSpace(createdBy)).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("upsert monitor repo: %w", err)
	}
	return id, nil
}

// monitorTokenPrefixes 是已知 GitHub token 明文前缀族。auth_ref 只收引用串
// ——token 形态出现在引用字段 = 调用方传错了值，fail-closed 拒绝而不是当
// 引用存下来（charset 恰好合法的值必须点名拒绝）。
var monitorTokenPrefixes = []string{"ghp_", "gho_", "ghu_", "ghs_", "ghr_", "github_pat_"}

// monitorAuthRefForm 是引用形态闸（opaque reference：字母数字与 . _ : -），
// 且显式拒绝 token 明文形态（前缀族 + 经典 40 位小写 hex PAT）。
func monitorAuthRefForm(s string) bool {
	if len(s) > 128 {
		return false
	}
	low := strings.ToLower(s)
	for _, p := range monitorTokenPrefixes {
		if strings.HasPrefix(low, p) {
			return false
		}
	}
	if monitorSHARe.MatchString(low) { // 经典 40 位小写 hex PAT 形态
		return false
	}
	if len(s) > 128 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.' || r == '_' || r == ':' || r == '-':
		default:
			return false
		}
	}
	return true
}

// scanMonitorRepo 是行扫描器（列清单与 SELECT 逐一对应）。
func scanMonitorRepo(row interface{ Scan(...any) error }) (*MonitorRepo, error) {
	var r MonitorRepo
	err := row.Scan(&r.ID, &r.RepoURL, &r.Branch, &r.GitHubBaseURL, &r.AuthRef,
		&r.Enabled, &r.LastSeenSHA, &r.CreatedBy, &r.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &r, nil
}

const monitorRepoCols = `id, repo_url, branch, github_base_url, auth_ref, enabled, last_seen_sha, created_by, created_at`

// GetMonitorRepo 按 id 取配置；不存在返回 (nil, nil)（not-found ≠ 错误）。
func (d *DB) GetMonitorRepo(id int64) (*MonitorRepo, error) {
	if id <= 0 {
		return nil, fmt.Errorf("monitor: repo id 必须为正")
	}
	if err := d.EnsureMonitorSchema(); err != nil {
		return nil, err
	}
	r, err := scanMonitorRepo(d.QueryRow(`SELECT `+monitorRepoCols+` FROM monitor_repos WHERE id=$1`, id))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return r, err
}

// ListMonitorRepos 列配置；enabledOnly=true 只返回启用中的（poller 输入面）。
// 确定性排序 id ASC。
func (d *DB) ListMonitorRepos(enabledOnly bool) ([]*MonitorRepo, error) {
	if err := d.EnsureMonitorSchema(); err != nil {
		return nil, err
	}
	q := `SELECT ` + monitorRepoCols + ` FROM monitor_repos`
	if enabledOnly {
		q += ` WHERE enabled`
	}
	q += ` ORDER BY id ASC`
	rows, err := d.Query(q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*MonitorRepo
	for rows.Next() {
		r, err := scanMonitorRepo(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SetMonitorRepoEnabled 启停配置（游标与配置零触碰）。
func (d *DB) SetMonitorRepoEnabled(id int64, enabled bool) error {
	if id <= 0 {
		return fmt.Errorf("monitor: repo id 必须为正")
	}
	if err := d.EnsureMonitorSchema(); err != nil {
		return err
	}
	res, err := d.Exec(`UPDATE monitor_repos SET enabled=$2 WHERE id=$1`, id, enabled)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("monitor: repo %d 不存在", id)
	}
	return nil
}

// UpdateMonitorRepoCursor 推进拉取游标（last_seen_sha）。sha 只收 40 位小写
// 十六进制；空串是显式重置（重拉语义，由运维显式触发）。
func (d *DB) UpdateMonitorRepoCursor(id int64, sha string) error {
	if id <= 0 {
		return fmt.Errorf("monitor: repo id 必须为正")
	}
	if err := monitorSHAForm(sha); err != nil {
		return err
	}
	if err := d.EnsureMonitorSchema(); err != nil {
		return err
	}
	res, err := d.Exec(`UPDATE monitor_repos SET last_seen_sha=$2 WHERE id=$1`, id, sha)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("monitor: repo %d 不存在", id)
	}
	return nil
}

// DeleteMonitorRepo 删除配置。有 findings 挂靠时被外键 RESTRICT 拒绝——
// 处置历史绝不随仓库删除被隐式清空（先显式处置 findings 再删仓库）。
func (d *DB) DeleteMonitorRepo(id int64) error {
	if id <= 0 {
		return fmt.Errorf("monitor: repo id 必须为正")
	}
	if err := d.EnsureMonitorSchema(); err != nil {
		return err
	}
	res, err := d.Exec(`DELETE FROM monitor_repos WHERE id=$1`, id)
	if err != nil {
		return fmt.Errorf("删除 monitor repo 失败（若存在 findings 挂靠需先显式处置）: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("monitor: repo %d 不存在", id)
	}
	return nil
}

// RecordMonitorFinding 落一条监测发现。(repo_id, sha) 幂等：重复落返回
// inserted=false 且首见行保持不变（重判/重跑收敛，绝不覆盖原状态）。
// status 允许 new / pending_review（模型失败 fail-closed 落点）；其余两态是
// 人工处置态（B-09G 操作面），判定层绝不直接写入。
func (d *DB) RecordMonitorFinding(repoID int64, sha, title, cveID, ghsaID, status string) (bool, error) {
	if repoID <= 0 {
		return false, fmt.Errorf("monitor: repo id 必须为正")
	}
	if err := monitorSHAForm(sha); err != nil || sha == "" {
		return false, fmt.Errorf("monitor: finding 的 sha 必填且为 40 位小写十六进制")
	}
	title = strings.TrimSpace(title)
	if title == "" || len(title) > 512 {
		return false, fmt.Errorf("monitor: title 必填且 ≤512 字节")
	}
	cveID, ghsaID = strings.TrimSpace(cveID), strings.TrimSpace(ghsaID)
	if cveID != "" && !monitorCVERe.MatchString(cveID) {
		return false, fmt.Errorf("monitor: cve_id 形态非法（got %q）", cveID)
	}
	if ghsaID != "" && !monitorGHSARe.MatchString(ghsaID) {
		return false, fmt.Errorf("monitor: ghsa_id 形态非法（got %q）", ghsaID)
	}
	if status == "" {
		status = MonitorStatusNew
	}
	if status != MonitorStatusNew && status != MonitorStatusPendingReview {
		return false, fmt.Errorf("monitor: 判定层只允许落 new/pending_review（got %q）——reproduced/ignored 是人工处置态", status)
	}
	if err := d.EnsureMonitorSchema(); err != nil {
		return false, err
	}
	var id int64
	err := d.QueryRow(`
INSERT INTO monitor_findings(repo_id, sha, title, cve_id, ghsa_id, status)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (repo_id, sha) DO NOTHING
RETURNING id`, repoID, sha, title, cveID, ghsaID, status).Scan(&id)
	if err == nil {
		return true, nil
	}
	if err == sql.ErrNoRows {
		return false, nil
	}
	return false, fmt.Errorf("record monitor finding: %w", err)
}

// scanMonitorFinding 是行扫描器。
func scanMonitorFinding(row interface{ Scan(...any) error }) (*MonitorFinding, error) {
	var f MonitorFinding
	err := row.Scan(&f.ID, &f.RepoID, &f.SHA, &f.Title, &f.CVEID, &f.GHSAID, &f.Status, &f.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &f, nil
}

const monitorFindingCols = `id, repo_id, sha, title, cve_id, ghsa_id, status, created_at`

// ListMonitorFindings 列发现。status 过滤（空=全部，白名单校验）；确定性
// 排序 id DESC（新发现在前）；limit 必填 >0 且 ≤500（零默认夹带）。
func (d *DB) ListMonitorFindings(status string, limit int) ([]*MonitorFinding, error) {
	if status != "" && !monitorStatuses[status] {
		return nil, fmt.Errorf("monitor: status 过滤值非法（got %q）", status)
	}
	if limit <= 0 || limit > 500 {
		return nil, fmt.Errorf("monitor: limit 必须 >0 且 ≤500（got %d）", limit)
	}
	if err := d.EnsureMonitorSchema(); err != nil {
		return nil, err
	}
	q := `SELECT ` + monitorFindingCols + ` FROM monitor_findings`
	args := []any{}
	if status != "" {
		q += ` WHERE status=$1`
		args = append(args, status)
	}
	q += ` ORDER BY id DESC LIMIT $` + fmt.Sprint(len(args)+1)
	args = append(args, limit)
	rows, err := d.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*MonitorFinding
	for rows.Next() {
		f, err := scanMonitorFinding(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// SetMonitorFindingStatus 人工处置状态转移（B-09G 操作面的存储入口）。白名单
// 四态（CHECK 约束数据层终闸）；行不存在报错（不静默）。
func (d *DB) SetMonitorFindingStatus(id int64, status string) error {
	if id <= 0 {
		return fmt.Errorf("monitor: finding id 必须为正")
	}
	if !monitorStatuses[status] {
		return fmt.Errorf("monitor: status 非法（got %q，白名单 new/pending_review/reproduced/ignored）", status)
	}
	if err := d.EnsureMonitorSchema(); err != nil {
		return err
	}
	res, err := d.Exec(`UPDATE monitor_findings SET status=$2 WHERE id=$1`, id, status)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("monitor: finding %d 不存在", id)
	}
	return nil
}

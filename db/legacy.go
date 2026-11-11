package db

// ---------- 传承图（P5，第三张图）访问层 ----------
//
// 三操作闭环（方案 v3.1 §3.3）：归档蒸馏（写 draft）→ 人工评审（accepted/retired）
// → 检索注入（只注 accepted，确定性 KV/文本重叠打分）。无自动晋升、无 embedding。

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

//go:embed schema_legacy.sql
var legacySchemaSQL string

var legacySchemaReady sync.Map

// EnsureLegacySchema 建三张表（幂等；Open 迁移期用 admin 连接调用，运行期空操作）。
func (d *DB) EnsureLegacySchema() error {
	if _, ok := legacySchemaReady.Load(d.DB); ok {
		return nil
	}
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	if err := coordinateWithSchemaMigration(tx); err != nil {
		return err
	}
	if _, err := tx.Exec(legacySchemaSQL); err != nil {
		return fmt.Errorf("应用传承图 schema 失败: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	legacySchemaReady.Store(d.DB, struct{}{})
	return nil
}

// fingerprintKeyRe 是指纹 KV 的 key 白名单（方案 v3.1 §3.3②——新写校验，
// 形状出处是 sapper facts.data：扁平标量 KV）。
// key 允许 CJK（模型产出的自然语言键，如「技术栈」），仍禁止空白/冒号/引号——
// 传承页的 k=v 徽章渲染依赖其单 token 性。上限 64 rune。
var fingerprintKeyRe = regexp.MustCompile(`^[\p{Han}A-Za-z0-9_.-]{1,64}$`)

// ValidateFingerprint 校验指纹 KV：key 白名单、值限标量（str/int/float/bool）、
// 值长 ≤256、键数 ≤32。
func ValidateFingerprint(fp map[string]any) error {
	if len(fp) > 32 {
		return fmt.Errorf("指纹键数超过 32")
	}
	keys := make([]string, 0, len(fp))
	for k := range fp {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !fingerprintKeyRe.MatchString(k) {
			return fmt.Errorf("指纹 key %q 不合法（限 ≤64 字的汉字/字母/数字/._-，禁空白）", k)
		}
		switch v := fp[k].(type) {
		case string:
			if len(v) > 256 {
				return fmt.Errorf("指纹 %s 的值超过 256 字符", k)
			}
		case float64:
		case bool:
		default:
			return fmt.Errorf("指纹 %s 的值必须是标量（str/int/float/bool）", k)
		}
	}
	return nil
}

// LegacyNode 是一条经验。
type LegacyNode struct {
	ID            int64          `json:"id"`
	CompanyID     *int64         `json:"company_id,omitempty"`
	Kind          string         `json:"kind"`
	Title         string         `json:"title"`
	Summary       string         `json:"summary"`
	Detail        string         `json:"detail,omitempty"`
	Fingerprint   map[string]any `json:"fingerprint"`
	State         string         `json:"state"`
	SourceTaskID  *int64         `json:"source_task_id,omitempty"`
	SourceTask    string         `json:"source_task,omitempty"`
	SourceSession string         `json:"source_session,omitempty"`
	CreatedAt     time.Time      `json:"created_at"`
	ReviewedAt    *time.Time     `json:"reviewed_at,omitempty"`
	ReviewedBy    string         `json:"reviewed_by,omitempty"`
}

const legacyCols = `id, company_id, kind, title, summary, detail, fingerprint, state,
 source_task_id, created_at, reviewed_at, reviewed_by`

func scanLegacyNode(row interface{ Scan(...any) error }) (*LegacyNode, error) {
	var n LegacyNode
	var fp []byte
	var srcTask *int64
	if err := row.Scan(&n.ID, &n.CompanyID, &n.Kind, &n.Title, &n.Summary, &n.Detail,
		&fp, &n.State, &srcTask, &n.CreatedAt, &n.ReviewedAt, &n.ReviewedBy); err != nil {
		return nil, err
	}
	_ = json.Unmarshal(fp, &n.Fingerprint)
	if n.Fingerprint == nil {
		n.Fingerprint = map[string]any{}
	}
	if srcTask != nil {
		id := *srcTask
		n.SourceTaskID = &id
		n.SourceTask = fmt.Sprint(id)
	}
	return &n, nil
}

// CreateLegacyNode 归档蒸馏的写入口（state 固定 draft——人工评审才可晋升）。
func (d *DB) CreateLegacyNode(n *LegacyNode) (int64, error) {
	if err := d.EnsureLegacySchema(); err != nil {
		return 0, err
	}
	if strings.TrimSpace(n.Title) == "" || strings.TrimSpace(n.Summary) == "" {
		return 0, fmt.Errorf("title/summary 不能为空")
	}
	if runeLen(n.Summary) > 4000 {
		return 0, fmt.Errorf("summary 超过 4000 字符上限")
	}
	if err := ValidateFingerprint(n.Fingerprint); err != nil {
		return 0, err
	}
	fp, _ := json.Marshal(n.Fingerprint)
	state := n.State
	if state == "" {
		state = "draft"
	}
	if state != "draft" {
		return 0, fmt.Errorf("归档写入只允许 draft 态（评审走 ReviewLegacyNode）")
	}
	var id int64
	err := d.QueryRow(`INSERT INTO legacy_nodes(company_id, kind, title, summary, detail, fingerprint, state, source_task_id, source_session)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING id`,
		n.CompanyID, n.Kind, n.Title, n.Summary, n.Detail, fp, state, n.SourceTaskID, n.SourceSession).Scan(&id)
	return id, err
}

// ErrLegacyAcceptedPin 表示要删除的经验处于 accepted 态——accepted 是注入池的
// 在用经验，删除必须先走评审退役（防误删正在被新任务检索注入的知识）。
var ErrLegacyAcceptedPin = fmt.Errorf("accepted 经验不可直接删除，请先评审退役")

// ReviewLegacyNode 人工评审。转移矩阵（方案 v3 缺陷 D2——退役必须可撤销）：
//
//	draft    → accepted | retired   （首评）
//	retired  → draft                （恢复待评审：退役错了/误操作可回）
//	accepted → retired              （在用经验退役：注入池立即失去它）
//
// UPDATE 带 `state = 期望前态集合` 守卫：非预期前态的行不被改写（并发评审安全）；
// 目标态与当前态相同视为幂等成功（重复点击不报错）。
func (d *DB) ReviewLegacyNode(id int64, state, reviewer string) error {
	if err := d.EnsureLegacySchema(); err != nil {
		return err
	}
	var prev []string
	switch state {
	case "accepted":
		prev = []string{"draft"}
	case "retired":
		prev = []string{"draft", "accepted"}
	case "draft":
		prev = []string{"retired"}
	default:
		return fmt.Errorf("评审目标态只能是 accepted|retired|draft（恢复），收到 %q", state)
	}
	if strings.TrimSpace(reviewer) == "" {
		return fmt.Errorf("评审必须留操作者身份")
	}
	in := func() string {
		q := `state='` + prev[0] + `'`
		for _, p := range prev[1:] {
			q += ` OR state='` + p + `'`
		}
		return q
	}()
	res, err := d.Exec(`UPDATE legacy_nodes SET state=$2, reviewed_at=now(), reviewed_by=$3
WHERE id=$1 AND (`+in+`)`, id, state, reviewer)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// 幂等：已在目标态（重复评审）。前态校验靠重读当前行。
		var cur string
		if err := d.QueryRow(`SELECT state FROM legacy_nodes WHERE id=$1`, id).Scan(&cur); err != nil {
			return fmt.Errorf("经验 #%d 不存在", id)
		}
		if cur == state {
			return nil
		}
		return fmt.Errorf("经验 #%d 当前为 %s 态，不允许转移到 %s", id, cur, state)
	}
	return nil
}

// DeleteLegacyNode 删除一条经验（缺陷 D2：退役/草稿可删，accepted 拒绝）。
// 删前把节点全量快照 + legacy_usage 行写进 legacy_audit——删除不可逆，但审计
// 不能蒸发（红队口径：破坏性操作必须留痕）。
func (d *DB) DeleteLegacyNode(id int64, operator string) error {
	if err := d.EnsureLegacySchema(); err != nil {
		return err
	}
	node, err := d.GetLegacyNode(id)
	if err != nil {
		return err
	}
	if node.State == "accepted" {
		return ErrLegacyAcceptedPin
	}
	// 快照：节点 + 采纳留痕行（usage 的 task_id 列表）。
	usageRows, _ := d.Query(`SELECT task_id, used_at FROM legacy_usage WHERE experience_id=$1 ORDER BY id`, id)
	type usageRow struct {
		TaskID int64     `json:"task_id"`
		UsedAt time.Time `json:"used_at"`
	}
	usages := []usageRow{}
	if usageRows != nil {
		defer usageRows.Close()
		for usageRows.Next() {
			var u usageRow
			if err := usageRows.Scan(&u.TaskID, &u.UsedAt); err == nil {
				usages = append(usages, u)
			}
		}
		usageRows.Close()
	}
	snapshot, err := json.Marshal(map[string]any{"node": node, "usages": usages})
	if err != nil {
		return err
	}
	if _, err := d.Exec(`INSERT INTO legacy_audit(action, node_id, payload, operator) VALUES ('delete',$1,$2,$3)`,
		id, string(snapshot), strings.TrimSpace(operator)); err != nil {
		return fmt.Errorf("删除前审计快照失败（拒绝删除）: %w", err)
	}
	res, err := d.Exec(`DELETE FROM legacy_nodes WHERE id=$1 AND state IN ('draft','retired')`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// 并发下状态变成了 accepted——审计快照已写但不回滚（多一条无害快照）。
		return ErrLegacyAcceptedPin
	}
	return nil
}

// GetLegacyNode 读单条（不存在返回可区分错误）。
func (d *DB) GetLegacyNode(id int64) (*LegacyNode, error) {
	if err := d.EnsureLegacySchema(); err != nil {
		return nil, err
	}
	row := d.QueryRow(`SELECT `+legacyCols+`, source_session FROM legacy_nodes WHERE id=$1`, id)
	var n LegacyNode
	var fp []byte
	var srcTask *int64
	if err := row.Scan(&n.ID, &n.CompanyID, &n.Kind, &n.Title, &n.Summary, &n.Detail,
		&fp, &n.State, &srcTask, &n.CreatedAt, &n.ReviewedAt, &n.ReviewedBy, &n.SourceSession); err != nil {
		return nil, fmt.Errorf("经验 #%d 不存在: %w", id, err)
	}
	_ = json.Unmarshal(fp, &n.Fingerprint)
	if n.Fingerprint == nil {
		n.Fingerprint = map[string]any{}
	}
	if srcTask != nil {
		idv := *srcTask
		n.SourceTaskID = &idv
		n.SourceTask = fmt.Sprint(idv)
	}
	return &n, nil
}

// ListLegacyNodes 列经验（state 过滤可选；默认按 id 降序）。
func (d *DB) ListLegacyNodes(state string, limit int) ([]*LegacyNode, error) {
	if err := d.EnsureLegacySchema(); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	q := `SELECT ` + legacyCols + ` FROM legacy_nodes`
	args := []any{}
	if state != "" {
		q += ` WHERE state=$1`
		args = append(args, state)
	}
	q += fmt.Sprintf(` ORDER BY id DESC LIMIT %d`, limit)
	rows, err := d.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*LegacyNode{}
	for rows.Next() {
		n, err := scanLegacyNode(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// acceptedExperiences 是注入候选池的行（注入只取列出的字段，预算内截断）。
type acceptedExperience struct {
	ID      int64
	Title   string
	Summary string
	Fingerp map[string]any
	// CreatedAt 是经验的归档时间：注入面按它算时效分（权重行没有 last_win_at
	// 时的回落基准，见 MatchExperiences）。
	CreatedAt time.Time
}

// ScoredExperience 是带归一化重叠分的注入候选（X1：注入排序
// 0.6×重叠 + 0.2×prior + 0.2×时效，重叠分从这里来）。
type ScoredExperience struct {
	acceptedExperience
	// Overlap 是文本重叠分（0..1）＝ 原始匹配分 / 候选池最高原始分。
	// 相对归一口径：池内可比、跨池稳定（同池排序确定性不变）。
	Overlap float64
}

// AcceptedExperiences 读注入候选池（state=accepted）。
func (d *DB) AcceptedExperiences(limit int) ([]acceptedExperience, error) {
	if err := d.EnsureLegacySchema(); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 50
	}
	rows, err := d.Query(`SELECT id, title, summary, fingerprint, created_at FROM legacy_nodes
WHERE state='accepted' ORDER BY id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []acceptedExperience{}
	for rows.Next() {
		var e acceptedExperience
		var fp []byte
		if err := rows.Scan(&e.ID, &e.Title, &e.Summary, &fp, &e.CreatedAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(fp, &e.Fingerp)
		out = append(out, e)
	}
	return out, rows.Err()
}

// MatchExperiences 按 goal 文本 token 重叠 + 种子 KV 重叠给 accepted 经验打分取 top-N。
// 查询侧没有历史指纹（指纹属蒸馏产物）⇒ 匹配退化为文本/资产重叠——如实口径
// （方案 v3.1 §3.3③）。token 化照抄 sapper recall 思路：小写化 + 连续字母数字切分
// + CJK 单字切分；打分 = 标题×2 + 摘要 + 指纹值命中。
//
// X1（方案 v10 §6 T1）：返回带归一化重叠分的候选 —— 注入面按
// 0.6×重叠 + 0.2×prior + 0.2×时效 加权重排，重叠分在这里归一化
// （原始分 / 池内最高分，0..1），排序面不再各自算一份。
func (d *DB) MatchExperiences(goalText string, seedKVs map[string]any, topN int) ([]ScoredExperience, error) {
	pool, err := d.AcceptedExperiences(100)
	if err != nil {
		return nil, err
	}
	if len(pool) == 0 {
		return []ScoredExperience{}, nil
	}
	qt := TokenizeForMatch(goalText)
	scored := make([]struct {
		e acceptedExperience
		s float64
	}, 0, len(pool))
	for _, e := range pool {
		score := 0.0
		title := strings.ToLower(e.Title)
		summary := strings.ToLower(e.Summary)
		for tok := range qt {
			if strings.Contains(title, tok) {
				score += 2
			}
			if strings.Contains(summary, tok) {
				score++
			}
		}
		// 种子 KV 与指纹的键/值重叠（资产族命中是强信号）。
		for k, v := range seedKVs {
			vs := strings.ToLower(fmt.Sprint(v))
			if _, ok := e.Fingerp[k]; ok {
				score += 1.5
			}
			if fv, ok := e.Fingerp[k]; ok && strings.Contains(strings.ToLower(fmt.Sprint(fv)), vs) && vs != "" {
				score += 3
			}
		}
		if score > 0 {
			scored = append(scored, struct {
				e acceptedExperience
				s float64
			}{e, score})
		}
	}
	sort.Slice(scored, func(i, j int) bool { return scored[i].s > scored[j].s })
	if topN <= 0 || topN > 5 {
		topN = 5
	}
	// ⚠️ v0.10.1：池子非空但**全员 0 分**（goal 词表与池零重叠，或 goal 为空）
	// 时 scored 为空——此前 scored[0] 直接 panic。这条 panic 不只打挂注入拼装
	// 预览：launchTask 的真实种子路径走同一个函数 ⇒ 新任务目标与 accepted 池
	// 零词重叠时**建任务请求会直接崩**。空集 = 无命中 = 空串不注入，正常返回。
	if len(scored) == 0 {
		return []ScoredExperience{}, nil
	}
	// 归一化基准：池内最高原始分（候选们都过同一条匹配函数，相对分可比）。
	maxScore := scored[0].s
	out := []ScoredExperience{}
	for i, sc := range scored {
		if i >= topN {
			break
		}
		overlap := 0.0
		if maxScore > 0 {
			overlap = sc.s / maxScore
		}
		out = append(out, ScoredExperience{acceptedExperience: sc.e, Overlap: overlap})
	}
	return out, nil
}

// TokenizeForMatch 小写化 + 连续字母数字段 + CJK 单字（sapper.py:1013-1027 思路）。
// 词集口径的唯一定义处：匹配打分（MatchExperiences）、归因因果校验
// （experience_weight.go）、晋级复盘门（server/promotion.go）共用它。
func TokenizeForMatch(text string) map[string]bool {
	text = strings.ToLower(text)
	out := map[string]bool{}
	var cur strings.Builder
	for _, r := range text {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_', r == '.', r == '-':
			cur.WriteRune(r)
		case r >= 0x4e00 && r <= 0x9fff:
			if cur.Len() > 0 {
				out[cur.String()] = true
				cur.Reset()
			}
			out[string(r)] = true
		default:
			if cur.Len() > 0 {
				out[cur.String()] = true
				cur.Reset()
			}
		}
	}
	if cur.Len() > 0 {
		out[cur.String()] = true
	}
	// 单字符 ASCII token 噪声大，剔除（CJK 单字保留——中文检索主力）。
	for tok := range out {
		if len(tok) == 1 && tok[0] < 0x80 {
			delete(out, tok)
		}
	}
	return out
}

// RecordLegacyUsage 记一次注入采纳留痕（人工评审参考，不做自动权重）。
func (d *DB) RecordLegacyUsage(experienceID, taskID int64) {
	if err := d.EnsureLegacySchema(); err != nil {
		return
	}
	_, _ = d.Exec(`INSERT INTO legacy_usage(experience_id, task_id) VALUES ($1,$2)`, experienceID, taskID)
}

// runeLen 是 rune 长度（包内小工具；不与 agent 层的同名函数冲突）。
func runeLen(s string) int { return len([]rune(s)) }

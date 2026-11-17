package db

// 跨任务同目标参考（0.15.12）：兄弟任务摘要注入 planner 态势的数据源。
//
// 纪律：这里只做机械查询（零 LLM、零判断）；「仅供参照、不做主线、不得据此直接
// 提交发现」的框架纪律由注入方（server/situation.go）负责。范围口径 = 公司重叠：
// tasks.company_id 直配或 task_scope(kind=company) 任一相同即视为同族目标——
// 同族 ⇒ 同一授权边，注入不越权（兄弟任务产出的都是本任务授权范围内的资产）。

// SiblingTaskDigest 是一条兄弟任务参考摘要。
type SiblingTaskDigest struct {
	ID          int64    `json:"id"`
	Goal        string   `json:"goal"`
	Status      string   `json:"status"`
	CompanyID   int64    `json:"company_id"`
	Findings    int      `json:"findings"`
	TopFindings []string `json:"top_findings,omitempty"`
}

// SiblingTaskDigests 返回与本任务共享公司范围的其他未删任务（新→旧，≤limit），
// 各带 goal/status/发现数/最新发现摘要（每任务 ≤3 条，过滤 dismissed/deleted）。
// 本任务没有公司范围、或没有兄弟任务时返回空切片（调用方不注入）。参考段是
// 增值面不是主线：查询失败返回 err，由调用方降级为不注入。
func (d *DB) SiblingTaskDigests(taskID int64, limit int) ([]SiblingTaskDigest, error) {
	if taskID <= 0 {
		return []SiblingTaskDigest{}, nil
	}
	if limit <= 0 {
		limit = 5
	}
	// 本任务的公司集合：tasks.company_id 直配 ∪ task_scope(kind=company)。
	companyIDs := []int64{}
	var directCompany int64
	_ = d.QueryRow(`SELECT COALESCE(company_id, 0) FROM tasks WHERE id=$1`, taskID).Scan(&directCompany)
	if directCompany > 0 {
		companyIDs = append(companyIDs, directCompany)
	}
	if scoped, err := d.TaskCompanyIDs(taskID); err == nil {
		companyIDs = append(companyIDs, scoped...)
	}
	if len(companyIDs) == 0 {
		return []SiblingTaskDigest{}, nil
	}
	rows, err := d.Query(`
SELECT t.id, COALESCE(t.company_id, 0), t.goal, t.status
FROM tasks t
WHERE t.deleted_at IS NULL AND t.id <> $1
  AND (
    COALESCE(t.company_id, 0) = ANY($2)
    OR EXISTS (
      SELECT 1 FROM task_scope s
      WHERE s.task_id = t.id AND s.kind = 'company' AND s.company_id = ANY($2)
    )
  )
ORDER BY t.id DESC LIMIT $3`, taskID, companyIDs, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SiblingTaskDigest{}
	for rows.Next() {
		var t SiblingTaskDigest
		if err := rows.Scan(&t.ID, &t.CompanyID, &t.Goal, &t.Status); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return out, nil
	}
	// 每个兄弟任务的最新发现摘要（≤3 条；finding 计数与摘要一并带出）。
	ids := make([]int64, 0, len(out))
	for _, t := range out {
		ids = append(ids, t.ID)
	}
	frows, err := d.Query(`
SELECT t.id, n.payload->>'summary'
FROM tasks t
JOIN exploration_nodes n ON n.exploration_id = t.exploration_id
WHERE t.id = ANY($1::bigint[]) AND n.kind='finding' AND n.state NOT IN ('dismissed','deleted')
  AND COALESCE(n.payload->>'summary','') <> ''
ORDER BY n.id DESC`, ids)
	if err != nil {
		return nil, err
	}
	defer frows.Close()
	counts := map[int64]int{}
	tops := map[int64][]string{}
	for frows.Next() {
		var id int64
		var summary string
		if err := frows.Scan(&id, &summary); err != nil {
			return nil, err
		}
		counts[id]++
		if len(tops[id]) < 3 {
			tops[id] = append(tops[id], summary)
		}
	}
	if err := frows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Findings = counts[out[i].ID]
		out[i].TopFindings = tops[out[i].ID]
	}
	return out, nil
}

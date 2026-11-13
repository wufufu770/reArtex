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

// 业务建模（第三张图）访问层：biz_nodes / biz_edges / biz_anchors /
// biz_layer_diffs / biz_model_versions。
//
// 分工与纪律（这是本文件的设计核心）：
//   - DDL 走独立幂等文件 db/schema_biz.sql（db/schema.sql 是 RESERVED），由
//     EnsureBizSchema 在启动期应用——与 EnsureLoopSchema 同款模式：库里没表这件事
//     要在启动期暴露，而不是等建模 agent 运行期报「关系不存在」（v0.9.16 事故的
//     同款修法，见 db/db.go EnsureExperienceWeightSchema 注释）。
//   - 写路径只有 InScope 一族：业务节点强制锚点 + 锚点先判范围（复用
//     anchorAssetsInScope），越界整单拒绝，错误文案不区分「不存在」与「不在范围
//     内」（同 ErrAnchorOutOfScope 口径，拒绝不构成存在性探测器）。
//   - 读路径全部按锚点范围过滤（bizScopedVisibility）：节点只有在它锚定的每一个
//     资产都落在调用者范围内时才可见；范围判定只走 AssetStore.ScopeFragmentFor
//     （全平台唯一入口），判定与取数在同一条 SQL 里发生。
//   - superseded_by 只由宿主侧写函数触碰（MarkBizNodeSuperseded /
//     MarkBizEdgeSuperseded），普通 upsert 不碰它——与 assets.superseded_by 同款
//     「追加合并与失效指针分离」纪律。

//go:embed schema_biz.sql
var bizSchemaSQL string

// bizSchemaReady 记录某个连接已经应用过业务图 DDL（键是底层 *sql.DB 指针，
// 与 loopTrackSchemaReady 同款：每个测试包/实例各连各的库，互不干扰）。
var bizSchemaReady sync.Map

// EnsureBizSchema 应用业务建模的幂等 DDL。
func (d *DB) EnsureBizSchema() error {
	if _, ok := bizSchemaReady.Load(d.DB); ok {
		return nil
	}
	if _, err := d.Exec(bizSchemaSQL); err != nil {
		return fmt.Errorf("应用业务建模 schema 失败: %w", err)
	}
	bizSchemaReady.Store(d.DB, struct{}{})
	return nil
}

// ensureBizSchema 是访问层的幂等入口：首次调用时补 DDL，之后只是一次 map 查表。
func (d *DB) ensureBizSchema() error { return d.EnsureBizSchema() }

// ---------- 业务节点类型（biz_nodes.node_type）----------

const (
	BizNodeEntity    = "biz_entity"    // 业务实体：账户、订单、商品、凭证……
	BizNodeAction    = "biz_action"    // 业务动作：下单、退款、审批、转账……
	BizNodeState     = "biz_state"     // 业务状态：待支付/已支付/已退款……
	BizNodeRole      = "biz_role"      // 业务角色：管理员/商家/买家……
	BizNodeTrust     = "biz_trust"     // 信任边界：内部服务/第三方回调/管理面……
	BizNodeDataflow  = "biz_dataflow"  // 数据流：同步/导出/通知……
	BizNodeExternal  = "biz_external"  // 外部系统：支付网关/短信/OAuth IdP……
)

// 业务图层（biz_nodes.layer）：design=系统声称自己是什么（前端产物/路由/权限矩阵），
// behavior=系统实际上是什么（观测证据/fact/流量）。
const (
	BizLayerDesign   = "design"
	BizLayerBehavior = "behavior"
)

// ---------- 差异类型（biz_layer_diffs.diff_type）与外部标准映射 ----------

// BizDiffRule 是八条异常规则的一条（纯规则段，WSTG/STRIDE 映射内置；
// internal 侧检测器与本表同源，词表漂移会让产出对不上外部标准）。
type BizDiffRule struct {
	DiffType string
	WSTG     string
	STRIDE   string
	Severity string
}

// BizDiffRules 是八类差异的规范映射表（方案一 §5.3）。代码常量不进 settings：
// severity 是审计与报告的口径，热改会让历史 diff 与新 diff 不可比。
var BizDiffRules = map[string]BizDiffRule{
	"missing_validation":    {DiffType: "missing_validation", WSTG: "WSTG-BUSL-03", STRIDE: "E", Severity: "critical"},
	"extra_permission":      {DiffType: "extra_permission", WSTG: "WSTG-BUSL-03", STRIDE: "E", Severity: "critical"},
	"state_bypass":          {DiffType: "state_bypass", WSTG: "WSTG-BUSL-02", STRIDE: "T", Severity: "critical"},
	"trust_violation":       {DiffType: "trust_violation", WSTG: "WSTG-BUSL-03", STRIDE: "S", Severity: "high"},
	"unexpected_flow":       {DiffType: "unexpected_flow", WSTG: "WSTG-BUSL-06", STRIDE: "I", Severity: "high"},
	"timing_anomaly":        {DiffType: "timing_anomaly", WSTG: "WSTG-BUSL-02", STRIDE: "T", Severity: "medium"},
	"unbounded_consumption": {DiffType: "unbounded_consumption", WSTG: "WSTG-BUSL-01", STRIDE: "D", Severity: "high"},
	"workflow_skip":         {DiffType: "workflow_skip", WSTG: "WSTG-BUSL-06", STRIDE: "I", Severity: "low"},
}

// ---------- 数据结构 ----------

// BizNode 是 biz_nodes 的一行。
type BizNode struct {
	ID           int64             `json:"id"`
	CompanyID    int64             `json:"company_id"`
	TaskID       sql.NullInt64     `json:"-"`
	TaskIDVal    int64             `json:"task_id"` // 0 = 无归属任务
	Layer        string            `json:"layer"`
	NodeType     string            `json:"node_type"`
	Name         string            `json:"name"`
	RawName      string            `json:"raw_name"`
	Description  string            `json:"description"`
	Props        json.RawMessage   `json:"props"`
	Confidence   string            `json:"confidence"`
	Source       string            `json:"source"`
	SupersededBy sql.NullInt64     `json:"-"`
	Superseded   int64             `json:"superseded_by"` // 0 = 未失效
	AssetIDs     []int64           `json:"asset_ids"`     // 锚点（查询时填充）
	CreatedAt    time.Time         `json:"created_at"`
	UpdatedAt    time.Time         `json:"updated_at"`
}

// BizEdge 是 biz_edges 的一行。
type BizEdge struct {
	ID           int64           `json:"id"`
	CompanyID    int64           `json:"company_id"`
	TaskIDVal    int64           `json:"task_id"`
	Layer        string          `json:"layer"`
	SrcID        int64           `json:"src_id"`
	DstID        int64           `json:"dst_id"`
	Rel          string          `json:"rel"`
	Props        json.RawMessage `json:"props"`
	Confidence   string          `json:"confidence"`
	Superseded   int64           `json:"superseded_by"`
	CreatedAt    time.Time       `json:"created_at"`
	UpdatedAt    time.Time       `json:"updated_at"`
}

// BizLayerDiff 是 biz_layer_diffs 的一行。
type BizLayerDiff struct {
	ID           int64           `json:"id"`
	CompanyID    int64           `json:"company_id"`
	TaskIDVal    int64           `json:"task_id"`
	DiffType     string          `json:"diff_type"`
	Severity     string          `json:"severity"`
	DesignRef    json.RawMessage `json:"design_ref"`
	BehaviorRef  json.RawMessage `json:"behavior_ref"`
	Description  string          `json:"description"`
	WSTGRef      string          `json:"wstg_ref"`
	StrIDERef    string          `json:"stride_ref"`
	Status       string          `json:"status"`
	CreatedAt    time.Time       `json:"created_at"`
}

// BizModelVersion 是 biz_model_versions 的一行。
type BizModelVersion struct {
	ID            int64           `json:"id"`
	CompanyID     int64           `json:"company_id"`
	TaskIDVal     int64           `json:"task_id"`
	Version       int             `json:"version"`
	Snapshot      json.RawMessage `json:"snapshot"`
	ChangeSummary string          `json:"change_summary"`
	CreatedAt     time.Time       `json:"created_at"`
}

// BizGraphData 是前端业务图视图的一次取数（节点+边，节点带锚点）。
type BizGraphData struct {
	Nodes []*BizNode `json:"nodes"`
	Edges []*BizEdge `json:"edges"`
}

// ---------- 写路径（agent 可达，全部 scope 判定前置）----------

// BizNodeInput 是 UpsertBizNodeInScope 的一条输入。
type BizNodeInput struct {
	Layer       string
	NodeType    string
	Name        string
	RawName     string
	Description string
	Props       map[string]any
	Confidence  string // observed|inferred；空=inferred
	Anchors     []int64
}

// upsert biz_nodes 的去重键（部分唯一索引，见 schema_biz.sql）：
// (company_id, layer, node_type, lower(name)) WHERE superseded_by IS NULL。
// 同名同层同类的节点只留一行，重复建模是 no-op 更新——增量建模靠它。

// UpsertBizNodeInScope 写一个业务节点（agent 可达版本）。
//
// 强制锚点：anchors 为空整单拒（业务节点必须能回答「这个推断是从哪条资产来的」；
// 不写锚点的裸节点会在范围读里整体不可见，写它等于造不可审计的数据）。
// 锚点先判范围（复用 anchorAssetsInScope），taskID<=0 直接 ErrAnchorNoTaskContext
// —— 与探索图 AddNodeInScope 同一 fail-closed 语义，会话路径无任务上下文时
// 写 biz_* 一律拒绝。
func (d *DB) UpsertBizNodeInScope(taskID, companyID int64, in BizNodeInput) (int64, error) {
	if err := d.ensureBizSchema(); err != nil {
		return 0, err
	}
	if companyID <= 0 {
		return 0, errors.New("业务节点必须归属一个公司（company_id 必填）")
	}
	if len(in.Anchors) == 0 {
		return 0, ErrAnchorNoTaskContext
	}
	if taskID <= 0 {
		return 0, ErrAnchorNoTaskContext
	}
	if err := d.anchorAssetsInScope(taskID, in.Anchors); err != nil {
		return 0, err
	}
	confidence := strings.TrimSpace(in.Confidence)
	if confidence != "observed" {
		confidence = "inferred"
	}
	props, err := json.Marshal(in.Props)
	if err != nil {
		return 0, fmt.Errorf("props: %w", err)
	}
	var id int64
	err = d.QueryRow(`
INSERT INTO biz_nodes(company_id, task_id, layer, node_type, name, raw_name, description, props, confidence, source)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,'agent')
ON CONFLICT (company_id, layer, node_type, lower(name)) WHERE superseded_by IS NULL
DO UPDATE SET
  raw_name    = EXCLUDED.raw_name,
  description = EXCLUDED.description,
  props       = biz_nodes.props || EXCLUDED.props,
  confidence  = EXCLUDED.confidence,
  task_id     = COALESCE(EXCLUDED.task_id, biz_nodes.task_id)
RETURNING id`,
		companyID, nullInt64(taskID), in.Layer, in.NodeType, in.Name, in.RawName,
		in.Description, props, confidence).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("upsert biz node: %w", err)
	}
	for _, aid := range in.Anchors {
		if _, err := d.Exec(`INSERT INTO biz_anchors(node_id, asset_id) VALUES ($1,$2) ON CONFLICT DO NOTHING`, id, aid); err != nil {
			return 0, fmt.Errorf("biz anchor: %w", err)
		}
	}
	return id, nil
}

// BizEdgeInput 是 UpsertBizEdgeInScope 的一条输入。
type BizEdgeInput struct {
	Layer      string
	SrcID      int64
	DstID      int64
	Rel        string
	Props      map[string]any
	Confidence string
}

// UpsertBizEdgeInScope 写一条业务边（agent 可达版本）。
//
// 两端节点的锚点必须都落在调用者范围内：先读两端的锚点做范围判定（任一端越界
// 整单拒，文案不区分不存在与越界），再落边。边自身不锚资产——它的授权边界
// 完全继承两端节点。
func (d *DB) UpsertBizEdgeInScope(taskID, companyID int64, in BizEdgeInput) (int64, error) {
	if err := d.ensureBizSchema(); err != nil {
		return 0, err
	}
	if taskID <= 0 {
		return 0, ErrAnchorNoTaskContext
	}
	if in.SrcID == in.DstID {
		return 0, errors.New("业务边两端不能是同一个节点")
	}
	anchors, err := d.bizNodesAnchors([]int64{in.SrcID, in.DstID})
	if err != nil {
		return 0, err
	}
	// 两端的锚点合集先判范围：任一 id 越界/不存在 ⇒ 整单拒（同一错误文案）。
	all := make([]int64, 0, len(anchors[in.SrcID])+len(anchors[in.DstID]))
	all = append(all, anchors[in.SrcID]...)
	all = append(all, anchors[in.DstID]...)
	if len(all) == 0 {
		// 端点不存在或都没有锚点：同一文案，不构成存在性探测器。
		return 0, ErrAnchorOutOfScope
	}
	if err := d.anchorAssetsInScope(taskID, all); err != nil {
		return 0, err
	}
	props, err := json.Marshal(in.Props)
	if err != nil {
		return 0, fmt.Errorf("props: %w", err)
	}
	confidence := strings.TrimSpace(in.Confidence)
	if confidence != "observed" {
		confidence = "inferred"
	}
	var id int64
	err = d.QueryRow(`
INSERT INTO biz_edges(company_id, task_id, layer, src_id, dst_id, rel, props, confidence)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
ON CONFLICT (company_id, layer, src_id, dst_id, rel) WHERE superseded_by IS NULL
DO UPDATE SET props = biz_edges.props || EXCLUDED.props, confidence = EXCLUDED.confidence
RETURNING id`,
		companyID, nullInt64(taskID), in.Layer, in.SrcID, in.DstID, in.Rel, props, confidence).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("upsert biz edge: %w", err)
	}
	return id, nil
}

// RecordBizDiffInScope 登记一条双层差异（agent 可达版本）。
//
// taskID 必须大于 0（差异是「本任务建模」的产出，无任务上下文不落账）；
// design_ref / behavior_ref 里引用的业务节点 id 若提供（node_ids 键），
// 逐个判范围，越界整单拒。WSTG/STRIDE/severity 由规则表补全（调用方给
// diff_type 即可，给错的映射以规则表为准——映射是审计口径，不许模型自造）。
func (d *DB) RecordBizDiffInScope(taskID, companyID int64, diffType, description string, designRef, behaviorRef map[string]any) (int64, error) {
	if err := d.ensureBizSchema(); err != nil {
		return 0, err
	}
	if taskID <= 0 {
		return 0, ErrAnchorNoTaskContext
	}
	rule, ok := BizDiffRules[diffType]
	if !ok {
		return 0, fmt.Errorf("未知差异类型 %q：必须是八类之一（见 BizDiffRules）", diffType)
	}
	// behavior_ref / design_ref 里带 node_ids 时逐个判范围。
	var refIDs []int64
	for _, ref := range []map[string]any{designRef, behaviorRef} {
		if ref == nil {
			continue
		}
		if raw, ok := ref["node_ids"].([]any); ok {
			for _, v := range raw {
				if f, ok := v.(float64); ok && f > 0 {
					refIDs = append(refIDs, int64(f))
				}
			}
		}
	}
	if len(refIDs) > 0 {
		anchors, err := d.bizNodesAnchors(refIDs)
		if err != nil {
			return 0, err
		}
		var all []int64
		for _, ids := range anchors {
			all = append(all, ids...)
		}
		if len(all) == 0 {
			return 0, ErrAnchorOutOfScope
		}
		if err := d.anchorAssetsInScope(taskID, all); err != nil {
			return 0, err
		}
	}
	designJSON, _ := json.Marshal(designRef)
	behaviorJSON, _ := json.Marshal(behaviorRef)
	var id int64
	err := d.QueryRow(`
INSERT INTO biz_layer_diffs(company_id, task_id, diff_type, severity, design_ref, behavior_ref, description, wstg_ref, stride_ref)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING id`,
		companyID, nullInt64(taskID), rule.DiffType, rule.Severity,
		designJSON, behaviorJSON, description, rule.WSTG, rule.STRIDE).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("record biz diff: %w", err)
	}
	return id, nil
}

// ---------- 读路径（范围过滤）----------

// bizScopedVisibility 是业务节点的范围可见性关系（与探索图 scopedNodeVisibility
// 同款语义，锚点表换成 biz_anchors）：节点必须至少锚在一个范围内资产上，且不能
// 锚到任何范围外资产。业务节点强制锚点（写路径保证），所以不需要探索图的
// NoOutOfScope 宽松版——fail-closed 是唯一口径。
const bizScopedVisibility = `EXISTS (
    SELECT 1 FROM biz_anchors ba
    WHERE ba.node_id = biz_nodes.id AND ba.asset_id IN (SELECT id FROM target)
  )
  AND NOT EXISTS (
    SELECT 1 FROM biz_anchors ba
    WHERE ba.node_id = biz_nodes.id AND ba.asset_id NOT IN (SELECT id FROM target)
  )`

// ListBizGraphScoped 按公司取业务图（节点+边），按调用者范围过滤。
//
// taskID > 0 → 该任务的范围；taskID <= 0 → 平台范围（默认空 ⇒ 什么都看不到，
// 不回退全局——与 ListByKindScoped 同一口径）。layer/type 为空串 = 不过滤。
func (d *DB) ListBizGraphScoped(taskID, companyID int64, layer, nodeType string, limit int) (*BizGraphData, error) {
	if err := d.ensureBizSchema(); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 1000
	}
	fragment, err := d.Assets().ScopeFragmentFor(taskID)
	if err != nil {
		return nil, err
	}
	args := append([]any{}, fragment.Args...)
	if companyID > 0 {
		args = append(args, companyID)
	}
	companyPos := len(args)
	if layer != "" {
		args = append(args, layer)
	}
	layerPos := len(args)
	if nodeType != "" {
		args = append(args, nodeType)
	}
	typePos := len(args)
	args = append(args, limit)
	limitPos := len(args)

	q := `WITH ` + fragment.CTE + `
SELECT id, company_id, COALESCE(task_id,0), layer, node_type, name, raw_name, description,
       props, confidence, source, COALESCE(superseded_by,0), created_at, updated_at
FROM biz_nodes
WHERE company_id = $` + strconv.Itoa(companyPos) + `
  AND superseded_by IS NULL
  AND ` + bizScopedVisibility
	if layer != "" {
		q += ` AND layer = $` + strconv.Itoa(layerPos)
	}
	if nodeType != "" {
		q += ` AND node_type = $` + strconv.Itoa(typePos)
	}
	q += ` ORDER BY id LIMIT $` + strconv.Itoa(limitPos)
	rows, err := d.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	nodes, err := scanBizNodes(rows)
	if err != nil {
		return nil, err
	}
	if len(nodes) == 0 {
		return &BizGraphData{Nodes: []*BizNode{}, Edges: []*BizEdge{}}, nil
	}
	ids := make([]int64, len(nodes))
	for i, n := range nodes {
		ids[i] = n.ID
	}
	edges, err := d.bizEdgesBetween(companyID, ids)
	if err != nil {
		return nil, err
	}
	if err := d.fillBizAnchors(nodes); err != nil {
		return nil, err
	}
	return &BizGraphData{Nodes: nodes, Edges: edges}, nil
}

// bizEdgesBetween 取两端都在给定节点集合内的未失效边（可见节点的边才可见——
// 一端越界的边跟着节点一起消失，半截保留才是泄露）。
func (d *DB) bizEdgesBetween(companyID int64, nodeIDs []int64) ([]*BizEdge, error) {
	q := `
SELECT id, company_id, COALESCE(task_id,0), layer, src_id, dst_id, rel, props, confidence,
       COALESCE(superseded_by,0), created_at, updated_at
FROM biz_edges
WHERE company_id=$1 AND superseded_by IS NULL
  AND src_id = ANY($2::bigint[]) AND dst_id = ANY($2::bigint[])`
	rows, err := d.Query(q, companyID, nodeIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*BizEdge
	for rows.Next() {
		e := &BizEdge{}
		if err := rows.Scan(&e.ID, &e.CompanyID, &e.TaskIDVal, &e.Layer, &e.SrcID, &e.DstID,
			&e.Rel, &e.Props, &e.Confidence, &e.Superseded, &e.CreatedAt, &e.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	if out == nil {
		out = []*BizEdge{}
	}
	return out, nil
}

// fillBizAnchors 批量填充节点的锚点资产 id。
func (d *DB) fillBizAnchors(nodes []*BizNode) error {
	if len(nodes) == 0 {
		return nil
	}
	ids := make([]int64, len(nodes))
	for i, n := range nodes {
		ids[i] = n.ID
	}
	rows, err := d.Query(`SELECT node_id, asset_id FROM biz_anchors WHERE node_id = ANY($1::bigint[]) ORDER BY asset_id`, ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	byNode := map[int64][]int64{}
	for rows.Next() {
		var nid, aid int64
		if err := rows.Scan(&nid, &aid); err != nil {
			return err
		}
		byNode[nid] = append(byNode[nid], aid)
	}
	for _, n := range nodes {
		n.AssetIDs = byNode[n.ID]
		if n.AssetIDs == nil {
			n.AssetIDs = []int64{}
		}
	}
	return nil
}

// bizNodesAnchors 读给定业务节点的锚点合集（按节点分组）。节点不存在时该节点
// 的键缺失——调用方负责「空即拒」的口径（不区分不存在与无锚点）。
func (d *DB) bizNodesAnchors(nodeIDs []int64) (map[int64][]int64, error) {
	out := map[int64][]int64{}
	if len(nodeIDs) == 0 {
		return out, nil
	}
	rows, err := d.Query(`SELECT node_id, asset_id FROM biz_anchors WHERE node_id = ANY($1::bigint[])`, nodeIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var nid, aid int64
		if err := rows.Scan(&nid, &aid); err != nil {
			return nil, err
		}
		out[nid] = append(out[nid], aid)
	}
	return out, nil
}

// ---------- biz_layer_diffs 读面 ----------

// ListBizDiffs 按公司/任务列差异（宿主与前端面板用；diff 不锚资产，按 task 归属
// 过滤而不是锚点过滤——它的授权边界是建模任务本身）。
func (d *DB) ListBizDiffs(companyID, taskID int64, status, severity string, limit int) ([]*BizLayerDiff, error) {
	if err := d.ensureBizSchema(); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 200
	}
	q := `SELECT id, company_id, COALESCE(task_id,0), diff_type, severity, design_ref, behavior_ref,
	      description, wstg_ref, stride_ref, status, created_at
FROM biz_layer_diffs WHERE company_id=$1`
	args := []any{companyID}
	if taskID > 0 {
		q += ` AND task_id=$2`
		args = append(args, taskID)
	}
	if status != "" {
		q += fmt.Sprintf(` AND status=$%d`, len(args)+1)
		args = append(args, status)
	}
	if severity != "" {
		q += fmt.Sprintf(` AND severity=$%d`, len(args)+1)
		args = append(args, severity)
	}
	q += fmt.Sprintf(` ORDER BY id DESC LIMIT $%d`, len(args)+1)
	args = append(args, limit)
	rows, err := d.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*BizLayerDiff
	for rows.Next() {
		df := &BizLayerDiff{}
		if err := rows.Scan(&df.ID, &df.CompanyID, &df.TaskIDVal, &df.DiffType, &df.Severity,
			&df.DesignRef, &df.BehaviorRef, &df.Description, &df.WSTGRef, &df.StrIDERef,
			&df.Status, &df.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, df)
	}
	if out == nil {
		out = []*BizLayerDiff{}
	}
	return out, nil
}

// UpdateBizDiffStatus 是差异状态的宿主侧裁决写（pushed 由派生路径写；
// confirmed / false_positive 由人工裁决 API 写）。唯一写入口，调用方负责落审计。
func (d *DB) UpdateBizDiffStatus(id int64, status string, behaviorRef map[string]any) error {
	if err := d.ensureBizSchema(); err != nil {
		return err
	}
	switch status {
	case "open", "pushed", "confirmed", "false_positive":
	default:
		return fmt.Errorf("未知差异状态 %q", status)
	}
	if behaviorRef != nil {
		b, err := json.Marshal(behaviorRef)
		if err != nil {
			return err
		}
		_, err = d.Exec(`UPDATE biz_layer_diffs SET status=$2, behavior_ref=$3 WHERE id=$1`, id, status, b)
		return err
	}
	_, err := d.Exec(`UPDATE biz_layer_diffs SET status=$2 WHERE id=$1`, id, status)
	return err
}

// ---------- 快照版本（增量建模的回滚点）----------

// SnapshotBizModel 落一份建模快照，返回版本号。snapshot 是调用方序列化好的
// 全量图 JSON（节点+边+差异计数）；version 按 (company_id) 单调 +1。
func (d *DB) SnapshotBizModel(companyID, taskID int64, snapshot json.RawMessage, changeSummary string) (int, error) {
	if err := d.ensureBizSchema(); err != nil {
		return 0, err
	}
	var version int
	err := d.QueryRow(`
INSERT INTO biz_model_versions(company_id, task_id, version, snapshot, change_summary)
VALUES ($1,$2,
  COALESCE((SELECT MAX(version)+1 FROM biz_model_versions WHERE company_id=$1), 1),
  $3,$4)
RETURNING version`,
		companyID, nullInt64(taskID), snapshot, changeSummary).Scan(&version)
	if err != nil {
		return 0, fmt.Errorf("snapshot biz model: %w", err)
	}
	return version, nil
}

// ListBizModelVersions 列建模快照（不含 snapshot 正文，列表面）。
func (d *DB) ListBizModelVersions(companyID int64, limit int) ([]*BizModelVersion, error) {
	if err := d.ensureBizSchema(); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 50
	}
	rows, err := d.Query(`
SELECT id, company_id, COALESCE(task_id,0), version, '{}'::jsonb, change_summary, created_at
FROM biz_model_versions WHERE company_id=$1 ORDER BY version DESC LIMIT $2`, companyID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*BizModelVersion
	for rows.Next() {
		v := &BizModelVersion{}
		if err := rows.Scan(&v.ID, &v.CompanyID, &v.TaskIDVal, &v.Version, &v.Snapshot,
			&v.ChangeSummary, &v.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	if out == nil {
		out = []*BizModelVersion{}
	}
	return out, nil
}

// ---------- supersede（宿主侧失效指针）----------

// MarkBizNodeSuperseded 把旧业务节点标失效并指向新节点。宿主侧唯一写入口：
// 增量建模发现「关系被证伪/被更细节点取代」时由 server 层调用（调用方负责
// AppendAuditEvent）。普通 upsert 不碰这一列。
func (d *DB) MarkBizNodeSuperseded(oldID, newID int64) error {
	if err := d.ensureBizSchema(); err != nil {
		return err
	}
	_, err := d.Exec(`UPDATE biz_nodes SET superseded_by=$2 WHERE id=$1 AND superseded_by IS NULL`, oldID, newID)
	return err
}

// MarkBizEdgeSuperseded 同上，边的失效指针。
func (d *DB) MarkBizEdgeSuperseded(oldID, newID int64) error {
	if err := d.ensureBizSchema(); err != nil {
		return err
	}
	_, err := d.Exec(`UPDATE biz_edges SET superseded_by=$2 WHERE id=$1 AND superseded_by IS NULL`, oldID, newID)
	return err
}

// ---------- 资产图 supersede（宿主侧唯一写路径，BIZ 裁决）----------

// SupersedeAsset 把旧资产标失效并指向新记录。
//
// 为什么是显式路径而不是 upsert 的一部分：assets 的 upsert 语义是【追加合并】
// （auth/params/technologies 走 assets.xxx || EXCLUDED.xxx 合并去重），「把旧记录
// 标失效」这个动作在合并语义里根本不存在——既没有可写的字段（superseded_by 是
// 本次施工新加的列），也没有可用的写路径。所以失效是叠加在合并之上的显式动作：
// 唯一写入口是本函数（普通 upsert 不碰这一列），调用方负责 AppendAuditEvent。
//
// 幂等保护：旧行已有失效指针时拒绝（第一次的裁决是权威，不被覆盖）；
// 「不存在」与「已失效」同一文案，不构成存在性探测器。
func (d *DB) SupersedeAsset(oldID, newID int64) error {
	if oldID <= 0 || newID <= 0 || oldID == newID {
		return errors.New("supersede 参数非法：old/new 必须为正且不同")
	}
	tag, err := d.Exec(`
UPDATE assets SET superseded_by=$2 WHERE id=$1 AND superseded_by IS NULL`, oldID, newID)
	if err != nil {
		return fmt.Errorf("supersede asset: %w", err)
	}
	if n, _ := tag.RowsAffected(); n == 0 {
		return errors.New("旧资产不存在或已被其他裁决失效")
	}
	return nil
}

// ---------- 摘要（planner 态势注入用，G-19）----------

// BizGraphSummary 返回一个任务业务图的机械摘要（planner 态势段的数据源；
// 反模式 8：只给计数与要点，不给全图）。
func (d *DB) BizGraphSummary(taskID int64) (designN, behaviorN, openDiffs int, err error) {
	if err := d.ensureBizSchema(); err != nil {
		return 0, 0, 0, err
	}
	err = d.QueryRow(`SELECT
  COALESCE(SUM(CASE WHEN layer='design' THEN 1 ELSE 0 END),0),
  COALESCE(SUM(CASE WHEN layer='behavior' THEN 1 ELSE 0 END),0)
FROM biz_nodes WHERE task_id=$1 AND superseded_by IS NULL`, taskID).Scan(&designN, &behaviorN)
	if err != nil {
		return 0, 0, 0, err
	}
	err = d.QueryRow(`SELECT COUNT(*) FROM biz_layer_diffs WHERE task_id=$1 AND status IN ('open','pushed')`, taskID).Scan(&openDiffs)
	if err != nil {
		return 0, 0, 0, err
	}
	return designN, behaviorN, openDiffs, nil
}

// ---------- scan / 小工具 ----------

func scanBizNodes(rows *sql.Rows) ([]*BizNode, error) {
	var out []*BizNode
	for rows.Next() {
		n := &BizNode{}
		if err := rows.Scan(&n.ID, &n.CompanyID, &n.TaskIDVal, &n.Layer, &n.NodeType, &n.Name,
			&n.RawName, &n.Description, &n.Props, &n.Confidence, &n.Source, &n.Superseded,
			&n.CreatedAt, &n.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	if out == nil {
		out = []*BizNode{}
	}
	return out, nil
}

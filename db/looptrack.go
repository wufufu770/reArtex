// 本文件是奇点循环执行层（方案 v10 §12 + §16，批次 F2-F5）的访问层：
// intent_predictions（意图预测镜像）/ contradictions（矛盾台账）/ gaps（空缺账本）。
//
// 分工（这是本文件的设计核心）：
//   - intent_predictions 是 F1「许愿纪律」的**读侧投影**：预测三字段已进意图
//     payload（agent/tools.go 的 addOneIntent），这里再镜像一行结构化行 —— F2
//     矛盾检测器做「预测 vs 实际」的确定性 diff 时按 intent_id 一次取预测，
//     不必解析每个意图的 payload JSON。主键 = intent_id，重复写是 no-op。
//   - contradictions 是 F2 确定性 diff 的**输出账**：不靠模型自省，谁的预测没
//     兑现、出现了什么预测外的发现，全部机器可判。resolved 由 F5 的人工裁决
//     通道（strategy_proposals 的 approved 分支）翻转。
//   - gaps 是 F4 空缺驱动唤醒的**账本**：open 的空缺触发一次定向唤醒
//     （authorization 类除外——授权空缺必须停，只能由人补，见 server/gapwake.go
//     的硬停止注释）；意图兑现（F5）或人工裁决通过时闭合。
//
// DDL 单独成文件（db/schema_loop.sql，照 db/schema_memory.sql 的 ensureSchema
// 模式）：db/schema.sql 由别的单元持有（RESERVED），本单元不改它。幂等：
// CREATE TABLE IF NOT EXISTS，重复应用零副作用。
package db

import (
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

//go:embed schema_loop.sql
var loopTrackSchemaSQL string

// loopTrackSchemaReady 记录某个连接已经应用过奇点循环 DDL。
//
// 与 db/finding_validation.go 的 findingValidationSchemaReady 同款：键是底层
// *sql.DB 指针（每个测试包/实例各连各的库，互不干扰）。
var loopTrackSchemaReady sync.Map

// EnsureLoopSchema 应用奇点循环执行层的幂等 DDL。
func (d *DB) EnsureLoopSchema() error {
	if _, ok := loopTrackSchemaReady.Load(d.DB); ok {
		return nil
	}
	if _, err := d.Exec(loopTrackSchemaSQL); err != nil {
		return fmt.Errorf("应用奇点循环 schema 失败: %w", err)
	}
	loopTrackSchemaReady.Store(d.DB, struct{}{})
	return nil
}

// ensureLoopSchema 是访问层的幂等入口：首次调用时补 DDL，之后只是一次 map 查表。
func (d *DB) ensureLoopSchema() error { return d.EnsureLoopSchema() }

// ---------- 矛盾种类（contradictions.kind）----------
//
// F2 的确定性 diff 只产出其中两类（evidence / path / permission）；其余是 §16
// 分类表的完整枚举，预留给后续批次的检测面。常量放本包，server 层引用同值，
// 免得同一套词在两个包里各写一遍。
const (
	// ContradictionEvidence 证据矛盾：预测的漏洞类别与实际产出不符 / 预测外发现。
	ContradictionEvidence   = "evidence"
	ContradictionPath       = "path"       // 路径矛盾：预测路径走不通（如目标出界）
	ContradictionTool       = "tool"       // 工具矛盾：预计工具与实际可用工具不符
	ContradictionPermission = "permission" // 权限矛盾：动作被越权/cap 拦截
	ContradictionTime       = "time"       // 时间矛盾：预算耗尽与预期不符
	ContradictionRisk       = "risk"       // 风险矛盾：风险口径与预期不符
)

// ---------- 空缺种类（gaps.kind，方案 §16 层①八类）----------
const (
	GapCognitive     = "cognitive"     // 认知空缺：不知道下一步该试什么
	GapEvidence      = "evidence"      // 证据空缺：预测与实际不符，缺定论
	GapAction        = "action"        // 动作空缺：想做但被拦/没做成
	GapTool          = "tool"          // 工具空缺：缺趁手的工具
	GapVerify        = "verify"        // 验证空缺：结论缺独立复核
	GapImpact        = "impact"        // 影响空缺：危害证明不充分
	GapReport        = "report"        // 汇报空缺：产出没有落成正式结论
	GapAuthorization = "authorization" // 授权空缺：需要人工扩权（只能停，不能自动补）
)

// IntentPrediction 是 intent_predictions 的一行（F1 写入，F2 消费）。
type IntentPrediction struct {
	IntentID           int64     `json:"intent_id"`
	PredictedVulnclass string    `json:"predicted_vulnclass"`
	PredictedActions   []string  `json:"predicted_actions"`
	PredictedTools     []string  `json:"predicted_tools"`
	CreatedAt          time.Time `json:"created_at"`
}

// HasPrediction 报这行预测里有没有「可证伪的愿望」：三个字段全空 ⇒ 没有
// （F1 的写入侧就不会给它建行，这里是防御性双保险）。
func (p *IntentPrediction) HasPrediction() bool {
	return strings.TrimSpace(p.PredictedVulnclass) != "" ||
		len(p.PredictedActions) > 0 || len(p.PredictedTools) > 0
}

// Contradiction 是 contradictions 的一行（F2 写入，F3 路由，F5 消账）。
type Contradiction struct {
	ID         int64     `json:"id"`
	TaskID     int64     `json:"task_id"`
	IntentID   int64     `json:"intent_id"`
	Kind       string    `json:"kind"`
	Detail     string    `json:"detail"`
	Resolved   bool      `json:"resolved"`
	Resolution string    `json:"resolution"`
	CreatedAt  time.Time `json:"created_at"`
}

// Gap 是 gaps 的一行（F4 写入/唤醒，F5 闭合）。
type Gap struct {
	ID             int64      `json:"id"`
	TaskID         int64      `json:"task_id"`
	Kind           string     `json:"kind"`
	Detail         string     `json:"detail"`
	Status         string     `json:"status"`
	OpenedAt       time.Time  `json:"opened_at"`
	ClosedAt       *time.Time `json:"closed_at,omitempty"`
	ClosedByIntent int64      `json:"closed_by_intent"` // 0 = 非意图兑现闭合
}

// gapDetailMaxRune 是 gap detail 的截断上限（字符）。uq_gaps_open 部分唯一索引
// 建在 (task_id, kind, detail) 上，btree 索引行有 ~2.7KB 的硬上限 —— 500 字符
// （CJK 最多 ~1.5KB）留足余量，超长 detail 会让 INSERT 整条失败。
const gapDetailMaxRune = 500

// contradictionDetailMaxRune 是矛盾 detail 的截断上限（无索引，纯防失控）。
const contradictionDetailMaxRune = 2000

func truncateRunesLoop(s string, n int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// InsertIntentPrediction 落一行意图预测（F1 的写入点：addOneIntent 落图后调用）。
//
// 主键 = intent_id，ON CONFLICT DO NOTHING ⇒ 同一意图重复落图是 no-op。
// 三个预测字段全空时直接跳过（无预测不写行——F1 的口径）。
func (d *DB) InsertIntentPrediction(intentID int64, predictedVulnclass string, predictedActions, predictedTools []string) (bool, error) {
	if err := d.ensureLoopSchema(); err != nil {
		return false, err
	}
	if intentID <= 0 {
		return false, nil
	}
	pred := &IntentPrediction{
		IntentID:           intentID,
		PredictedVulnclass: strings.TrimSpace(predictedVulnclass),
		PredictedActions:   predictedActions,
		PredictedTools:     predictedTools,
	}
	if !pred.HasPrediction() {
		return false, nil
	}
	actions, tools := pred.PredictedActions, pred.PredictedTools
	if actions == nil {
		actions = []string{}
	}
	if tools == nil {
		tools = []string{}
	}
	actionsJSON, _ := json.Marshal(actions)
	toolsJSON, _ := json.Marshal(tools)
	res, err := d.Exec(`
INSERT INTO intent_predictions(intent_id, predicted_vulnclass, predicted_actions, predicted_tools)
VALUES($1,$2,$3,$4) ON CONFLICT (intent_id) DO NOTHING`,
		pred.IntentID, pred.PredictedVulnclass, string(actionsJSON), string(toolsJSON))
	if err != nil {
		return false, fmt.Errorf("落意图预测失败: %w", err)
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// GetIntentPrediction 取一条意图预测。没有预测行 ⇒ (nil, nil)（F2 据此跳过）。
func (d *DB) GetIntentPrediction(intentID int64) (*IntentPrediction, error) {
	if err := d.ensureLoopSchema(); err != nil {
		return nil, err
	}
	if intentID <= 0 {
		return nil, nil
	}
	var p IntentPrediction
	var actions, tools []byte
	err := d.QueryRow(`
SELECT intent_id, predicted_vulnclass, predicted_actions, predicted_tools, created_at
FROM intent_predictions WHERE intent_id=$1`, intentID).
		Scan(&p.IntentID, &p.PredictedVulnclass, &actions, &tools, &p.CreatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("读意图预测失败: %w", err)
	}
	p.PredictedActions = decodeStringArray(actions)
	p.PredictedTools = decodeStringArray(tools)
	return &p, nil
}

// InsertContradiction 记一条矛盾（F2 的确定性 diff 输出；resolved=false 起步）。
func (d *DB) InsertContradiction(c *Contradiction) (int64, error) {
	if err := d.ensureLoopSchema(); err != nil {
		return 0, err
	}
	if c == nil || c.TaskID <= 0 {
		return 0, fmt.Errorf("矛盾必须带任务 id")
	}
	kind := strings.TrimSpace(c.Kind)
	if kind == "" {
		kind = ContradictionEvidence
	}
	detail := truncateRunesLoop(c.Detail, contradictionDetailMaxRune)
	var id int64
	if err := d.QueryRow(`
INSERT INTO contradictions(task_id, intent_id, kind, detail)
VALUES($1,$2,$3,$4) RETURNING id`,
		c.TaskID, c.IntentID, kind, detail).Scan(&id); err != nil {
		return 0, fmt.Errorf("记矛盾失败: %w", err)
	}
	c.ID, c.Kind, c.Detail, c.Resolved = id, kind, detail, false
	return id, nil
}

// MarkContradictionResolved 置一条矛盾为已消解（F5：人工裁决通过后调用）。
// 只翻未消解的行（幂等：已 resolved 的行不再改写 resolution）。
func (d *DB) MarkContradictionResolved(id int64, resolution string) error {
	if err := d.ensureLoopSchema(); err != nil {
		return err
	}
	if id <= 0 {
		return fmt.Errorf("矛盾 id 无效")
	}
	res, err := d.Exec(`
UPDATE contradictions SET resolved=true, resolution=$2 WHERE id=$1 AND resolved=false`,
		id, truncateRunesLoop(resolution, contradictionDetailMaxRune))
	if err != nil {
		return fmt.Errorf("消解矛盾失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("矛盾 %d 不存在或已消解", id)
	}
	return nil
}

// GetContradiction 取一条矛盾（F5 人工裁决通道用：从提议 payload 里的
// contradiction_id 找回 task/kind 以闭合对应空缺）。
func (d *DB) GetContradiction(id int64) (*Contradiction, error) {
	if err := d.ensureLoopSchema(); err != nil {
		return nil, err
	}
	if id <= 0 {
		return nil, nil
	}
	return scanContradiction(d.QueryRow(`
SELECT id, task_id, intent_id, kind, detail, resolved, resolution, created_at
FROM contradictions WHERE id=$1`, id))
}

// ListUnresolvedContradictions 列本任务未消解的矛盾（F3：legacy 蒸馏原料，
// ≤10 条；F5 人工裁决面同用）。
func (d *DB) ListUnresolvedContradictions(taskID int64, limit int) ([]Contradiction, error) {
	if err := d.ensureLoopSchema(); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 10
	}
	return d.listContradictions(
		`SELECT id, task_id, intent_id, kind, detail, resolved, resolution, created_at
FROM contradictions WHERE task_id=$1 AND resolved=false ORDER BY id DESC LIMIT $2`,
		taskID, limit)
}

// ListRecentContradictions 列本任务最近的矛盾（含已消解，治理视图暴露用）。
func (d *DB) ListRecentContradictions(taskID int64, limit int) ([]Contradiction, error) {
	if err := d.ensureLoopSchema(); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 20
	}
	return d.listContradictions(
		`SELECT id, task_id, intent_id, kind, detail, resolved, resolution, created_at
FROM contradictions WHERE task_id=$1 ORDER BY id DESC LIMIT $2`,
		taskID, limit)
}

func (d *DB) listContradictions(q string, args ...any) ([]Contradiction, error) {
	rows, err := d.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("列矛盾失败: %w", err)
	}
	defer rows.Close()
	out := []Contradiction{}
	for rows.Next() {
		var c Contradiction
		if err := rows.Scan(&c.ID, &c.TaskID, &c.IntentID, &c.Kind, &c.Detail,
			&c.Resolved, &c.Resolution, &c.CreatedAt); err != nil {
			return nil, fmt.Errorf("列矛盾失败: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func scanContradiction(sc interface{ Scan(...any) error }) (*Contradiction, error) {
	var c Contradiction
	if err := sc.Scan(&c.ID, &c.TaskID, &c.IntentID, &c.Kind, &c.Detail,
		&c.Resolved, &c.Resolution, &c.CreatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("读矛盾失败: %w", err)
	}
	return &c, nil
}

// InsertGap 开一条空缺（F4）。同 (task, kind, detail) 已有 open 行 ⇒ 去重
// （不插新行），返回既有行 id 与 inserted=false —— 唤醒只发生在 inserted=true
// 上，库级唯一索引（uq_gaps_open）兜住并发双写。
func (d *DB) InsertGap(taskID int64, kind, detail string) (id int64, inserted bool, err error) {
	if err := d.ensureLoopSchema(); err != nil {
		return 0, false, err
	}
	if taskID <= 0 {
		return 0, false, fmt.Errorf("空缺必须带任务 id")
	}
	kind = strings.TrimSpace(kind)
	if kind == "" {
		kind = GapAction
	}
	detail = truncateRunesLoop(detail, gapDetailMaxRune)
	var newID int64
	ierr := d.QueryRow(`
INSERT INTO gaps(task_id, kind, detail) VALUES($1,$2,$3)
ON CONFLICT (task_id, kind, detail) WHERE status='open' DO NOTHING
RETURNING id`, taskID, kind, detail).Scan(&newID)
	if ierr == nil {
		return newID, true, nil // 新 open 行：调用方据此触发唤醒
	}
	if !errors.Is(ierr, sql.ErrNoRows) {
		return 0, false, fmt.Errorf("记空缺失败: %w", ierr)
	}
	// 撞了 open 去重行（DO NOTHING ⇒ RETURNING 无行）：把既有行 id 带回
	// （便于观测；唤醒不触发）。
	var existing int64
	qerr := d.QueryRow(`SELECT id FROM gaps
WHERE task_id=$1 AND kind=$2 AND detail=$3 AND status='open' ORDER BY id DESC LIMIT 1`,
		taskID, kind, detail).Scan(&existing)
	if qerr != nil {
		return 0, false, nil // 去重行读丢（并发闭合）——按未插入处理即可
	}
	return existing, false, nil
}

// ListOpenGaps 列本任务 open 的空缺（F4 唤醒面 / F5 消账面 / 治理视图）。
func (d *DB) ListOpenGaps(taskID int64) ([]Gap, error) {
	if err := d.ensureLoopSchema(); err != nil {
		return nil, err
	}
	rows, err := d.Query(`
SELECT id, task_id, kind, detail, status, opened_at, closed_at, COALESCE(closed_by_intent,0)
FROM gaps WHERE task_id=$1 AND status='open' ORDER BY id DESC`, taskID)
	if err != nil {
		return nil, fmt.Errorf("列空缺失败: %w", err)
	}
	defer rows.Close()
	out := []Gap{}
	for rows.Next() {
		var g Gap
		if err := rows.Scan(&g.ID, &g.TaskID, &g.Kind, &g.Detail, &g.Status,
			&g.OpenedAt, &g.ClosedAt, &g.ClosedByIntent); err != nil {
			return nil, fmt.Errorf("列空缺失败: %w", err)
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// CloseGap 闭合一条 open 空缺（F5）。幂等：只翻 open 行；note 追加进 detail
// （消账出处可读——「由意图 #N 兑现闭合」这类注释走这里）。
func (d *DB) CloseGap(gapID, closedByIntent int64, note string) (bool, error) {
	if err := d.ensureLoopSchema(); err != nil {
		return false, err
	}
	if gapID <= 0 {
		return false, nil
	}
	if closedByIntent <= 0 {
		closedByIntent = 0
	}
	note = truncateRunesLoop(note, 200)
	res, err := d.Exec(`
UPDATE gaps SET status='closed', closed_at=now(), closed_by_intent=$2,
       detail=CASE WHEN $3='' THEN detail ELSE left(detail, 300) || $3 END
WHERE id=$1 AND status='open'`, gapID, closedByIntent, note)
	if err != nil {
		return false, fmt.Errorf("闭合空缺失败: %w", err)
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// CloseLatestOpenGap 按「本任务该 kind 最新 open 空缺」闭合（F5 的 best-effort
// 消账：意图与空缺做不到精确关联时，按方案口径闭最新一条并在 detail 注明）。
func (d *DB) CloseLatestOpenGap(taskID int64, kind string, closedByIntent int64, note string) (bool, error) {
	if err := d.ensureLoopSchema(); err != nil {
		return false, err
	}
	if taskID <= 0 {
		return false, nil
	}
	kind = strings.TrimSpace(kind)
	if kind == "" {
		kind = GapAction
	}
	note = truncateRunesLoop(note, 200)
	res, err := d.Exec(`
UPDATE gaps SET status='closed', closed_at=now(), closed_by_intent=$3,
       detail=CASE WHEN $4='' THEN detail ELSE left(detail, 300) || $4 END
WHERE id=(SELECT id FROM gaps WHERE task_id=$1 AND kind=$2 AND status='open'
          ORDER BY id DESC LIMIT 1) AND status='open'`,
		taskID, kind, closedByIntent, note)
	if err != nil {
		return false, fmt.Errorf("闭合空缺失败: %w", err)
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// YieldedFinding 是「意图 yields 的 finding 节点」的类别投影（F2 的确定性 diff
// 输入）。
type YieldedFinding struct {
	NodeID    int64  `json:"node_id"`
	Vulnclass string `json:"vulnclass"`
}

// YieldedFindingVulnclasses 按意图读它 yields 的 finding 类别。
//
// 为什么不按 findings 表 JOIN：finding 节点的 payload 在落库时与 findings 行写
// 同一个 vulnclass（db/finding_traffic.go 的 RecordFindingTx），图侧单查就能拿全；
// dismissed（复核不成立）的 finding 不算「实际产出」，排除。
func (d *DB) YieldedFindingVulnclasses(expID, intentID int64) ([]YieldedFinding, error) {
	rows, err := d.Query(`
SELECT n.id, COALESCE(n.payload->>'vulnclass','')
FROM exploration_edges e
JOIN exploration_nodes n ON n.id=e.dst_id AND n.exploration_id=e.exploration_id
WHERE e.exploration_id=$1 AND e.src_id=$2 AND e.rel='yields'
  AND n.kind='finding' AND n.state <> 'dismissed'
ORDER BY n.id`, expID, intentID)
	if err != nil {
		return nil, fmt.Errorf("读意图产出类别失败: %w", err)
	}
	defer rows.Close()
	out := []YieldedFinding{}
	for rows.Next() {
		var f YieldedFinding
		if err := rows.Scan(&f.NodeID, &f.Vulnclass); err != nil {
			return nil, fmt.Errorf("读意图产出类别失败: %w", err)
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

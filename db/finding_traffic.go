package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/Autumn-27/artex/notify"
)

var (
	ErrEvidenceConflict = errors.New("流量证据已变更，请刷新后重试")
	ErrFindingNotFound  = errors.New("漏洞不存在")
	ErrEvidenceNotFound = errors.New("流量证据不存在")
	// ErrFindingOutOfScope 表示目标漏洞不在调用者的任务范围内。
	//
	// 「目标不存在」与「目标存在但不在范围内」共用这一句文案：拒绝本身不该变成
	// 一个存在性探测器 —— 否则调用方可以靠「报错文案不同」把别人的漏洞探出来。
	ErrFindingOutOfScope = errors.New("漏洞不存在或不在当前任务范围内")
)

// This lock covers the evidence filesystem as well as its SQL references. All
// processes sharing the database use it, including readers, exports and GC.
const findingEvidenceLockKey int64 = 7337741004

func (d *DB) WithEvidenceTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, findingEvidenceLockKey); err != nil {
		return err
	}
	if err = fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

type TrafficRef struct {
	TrafficID string `json:"traffic_id"`
	Role      string `json:"role"`
	Note      string `json:"note"`
}

func NormalizeTrafficRefs(refs []TrafficRef) ([]TrafficRef, error) {
	out := make([]TrafficRef, 0, len(refs))
	seen := map[string]bool{}
	for _, ref := range refs {
		ref.TrafficID = strings.TrimSpace(ref.TrafficID)
		if ref.TrafficID == "" {
			return nil, errors.New("traffic_id 不能为空")
		}
		if ref.Role == "" {
			ref.Role = "supporting"
		}
		if !ValidTrafficRole(ref.Role) {
			return nil, fmt.Errorf("无效的流量用途 %q", ref.Role)
		}
		if !seen[ref.TrafficID] {
			out = append(out, ref)
			seen[ref.TrafficID] = true
		}
	}
	return out, nil
}

func ValidTrafficRole(role string) bool {
	return role == "baseline" || role == "proof" || role == "verification" || role == "supporting"
}

type TrafficEvidenceSnapshot struct {
	ID              string    `json:"id"`
	SourceTrafficID string    `json:"source_traffic_id"`
	CapturedAt      int64     `json:"captured_at"`
	URL             string    `json:"url"`
	Method          string    `json:"method"`
	Status          int       `json:"status"`
	ContentType     string    `json:"content_type"`
	ReqHead         string    `json:"req_head,omitempty"`
	RespHead        string    `json:"resp_head,omitempty"`
	ReqHash         string    `json:"req_hash"`
	RespHash        string    `json:"resp_hash"`
	ReqLen          int64     `json:"req_len"`
	RespLen         int64     `json:"resp_len"`
	CreatedAt       time.Time `json:"created_at"`
}

type FindingTrafficBinding struct {
	ID         int64                   `json:"id,string"`
	FindingID  int64                   `json:"finding_id,string"`
	SnapshotID string                  `json:"snapshot_id"`
	Role       string                  `json:"role"`
	Note       string                  `json:"note"`
	Position   int                     `json:"position"`
	CreatedAt  time.Time               `json:"created_at"`
	Snapshot   TrafficEvidenceSnapshot `json:"snapshot"`
}

type FindingTraffic struct {
	FindingID     int64                   `json:"finding_id,string"`
	Version       int64                   `json:"version"`
	ReportVersion int64                   `json:"report_version"`
	Bindings      []FindingTrafficBinding `json:"bindings"`
}

type PreparedTrafficEvidence struct {
	Ref      TrafficRef
	Snapshot TrafficEvidenceSnapshot
}

// Normalize makes the snapshot's text columns safe for PostgreSQL. URL and the
// head blocks come straight off the wire, so a target answering with a non-UTF-8
// header (a GBK `Content-Disposition: filename=…`, a NUL byte) would otherwise
// abort the INSERT and roll back the whole finding — losing a confirmed finding
// over a malformed response header. Applied before hashing so the ID always
// matches the bytes that actually land in the table.
func (s TrafficEvidenceSnapshot) Normalize() TrafficEvidenceSnapshot {
	s.SourceTrafficID = utf8Clean(s.SourceTrafficID)
	s.URL = utf8Clean(s.URL)
	s.Method = utf8Clean(s.Method)
	s.ContentType = utf8Clean(s.ContentType)
	s.ReqHead = utf8Clean(s.ReqHead)
	s.RespHead = utf8Clean(s.RespHead)
	return s
}

func TrafficSnapshotID(snapshot TrafficEvidenceSnapshot) string {
	snapshot = snapshot.Normalize()
	snapshot.ID = ""
	snapshot.CreatedAt = time.Time{}
	raw, _ := json.Marshal(snapshot)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// LockTaskEvidenceTx serializes writes with archive queueing (which locks the
// same task row). Once queued, its snapshot must not acquire new evidence.
func LockTaskEvidenceTx(tx *sql.Tx, taskID int64) error {
	if taskID == 0 {
		return nil
	}
	var deleted sql.NullTime
	if err := tx.QueryRow(`SELECT deleted_at FROM tasks WHERE id=$1 FOR UPDATE`, taskID).Scan(&deleted); err != nil {
		return err
	}
	if deleted.Valid {
		return ErrTaskArchiveState
	}
	var state string
	err := tx.QueryRow(`SELECT state FROM task_archives WHERE task_id=$1`, taskID).Scan(&state)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil && state != ArchiveFailed {
		return ErrTaskArchiveState
	}
	return nil
}

func LockFindingEvidenceTx(tx *sql.Tx, findingID int64, version *int64) error {
	var taskID sql.NullInt64
	if err := tx.QueryRow(`SELECT task_id FROM findings WHERE id=$1`, findingID).Scan(&taskID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrFindingNotFound
		}
		return err
	}
	if err := LockTaskEvidenceTx(tx, taskID.Int64); err != nil {
		return err
	}
	var current int64
	if err := tx.QueryRow(`SELECT evidence_version FROM findings WHERE id=$1 FOR UPDATE`, findingID).Scan(&current); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrFindingNotFound
		}
		return err
	}
	if version != nil && *version != current {
		return ErrEvidenceConflict
	}
	return nil
}

func InsertEvidenceSnapshotTx(tx *sql.Tx, s TrafficEvidenceSnapshot) error {
	if s.ID != TrafficSnapshotID(s) {
		return errors.New("证据快照元数据哈希不匹配")
	}
	// The ID was computed over the normalized form; store those same bytes.
	id := s.ID
	s = s.Normalize()
	s.ID = id
	if s.CreatedAt.IsZero() {
		s.CreatedAt = time.Now().UTC()
	}
	_, err := tx.Exec(`INSERT INTO traffic_evidence_snapshots
(id,source_traffic_id,captured_at,url,method,status,content_type,req_head,resp_head,req_hash,resp_hash,req_len,resp_len,created_at)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14) ON CONFLICT(id) DO NOTHING`,
		s.ID, s.SourceTrafficID, s.CapturedAt, s.URL, s.Method, s.Status, s.ContentType, s.ReqHead, s.RespHead, s.ReqHash, s.RespHash, s.ReqLen, s.RespLen, s.CreatedAt)
	return err
}

func AddFindingTrafficTx(tx *sql.Tx, findingID int64, prepared []PreparedTrafficEvidence) error {
	var position int
	if err := tx.QueryRow(`SELECT COALESCE(MAX(position)+1,0) FROM finding_traffic_bindings WHERE finding_id=$1`, findingID).Scan(&position); err != nil {
		return err
	}
	changed := false
	for _, item := range prepared {
		if err := InsertEvidenceSnapshotTx(tx, item.Snapshot); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE traffic_evidence_snapshots SET unreferenced_at=NULL WHERE id=$1`, item.Snapshot.ID); err != nil {
			return err
		}
		res, err := tx.Exec(`INSERT INTO finding_traffic_bindings(finding_id,snapshot_id,role,note,position)
VALUES($1,$2,$3,$4,$5) ON CONFLICT(finding_id,snapshot_id) DO NOTHING`, findingID, item.Snapshot.ID, item.Ref.Role, item.Ref.Note, position)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n > 0 {
			changed = true
			position++
		}
	}
	if changed {
		return bumpEvidenceVersionTx(tx, findingID)
	}
	return nil
}

func bumpEvidenceVersionTx(tx *sql.Tx, findingID int64) error {
	_, err := tx.Exec(`UPDATE findings SET evidence_version=evidence_version+1 WHERE id=$1`, findingID)
	return err
}

func FindingTrafficTx(tx *sql.Tx, findingID int64) (*FindingTraffic, error) {
	out := &FindingTraffic{FindingID: findingID, Bindings: []FindingTrafficBinding{}}
	if err := tx.QueryRow(`SELECT evidence_version,report_evidence_version FROM findings WHERE id=$1`, findingID).Scan(&out.Version, &out.ReportVersion); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrFindingNotFound
		}
		return nil, err
	}
	rows, err := tx.Query(`SELECT b.id,b.finding_id,b.snapshot_id,b.role,b.note,b.position,b.created_at,to_jsonb(s)
FROM finding_traffic_bindings b JOIN traffic_evidence_snapshots s ON s.id=b.snapshot_id WHERE b.finding_id=$1 ORDER BY b.position,b.id`, findingID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var b FindingTrafficBinding
		var raw []byte
		if err := rows.Scan(&b.ID, &b.FindingID, &b.SnapshotID, &b.Role, &b.Note, &b.Position, &b.CreatedAt, &raw); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(raw, &b.Snapshot); err != nil {
			return nil, err
		}
		out.Bindings = append(out.Bindings, b)
	}
	return out, rows.Err()
}

func (d *DB) GetFindingTraffic(ctx context.Context, findingID int64) (out *FindingTraffic, err error) {
	err = d.WithEvidenceTx(ctx, func(tx *sql.Tx) error { var e error; out, e = FindingTrafficTx(tx, findingID); return e })
	return
}

func (d *DB) EditFindingTraffic(ctx context.Context, findingID, bindingID, version int64, role, note *string, remove bool, order []int64) error {
	if role != nil && !ValidTrafficRole(*role) {
		return errors.New("无效的流量用途")
	}
	return d.WithEvidenceTx(ctx, func(tx *sql.Tx) error {
		if err := LockFindingEvidenceTx(tx, findingID, &version); err != nil {
			return err
		}
		if order != nil {
			current, err := FindingTrafficTx(tx, findingID)
			if err != nil {
				return err
			}
			if len(order) != len(current.Bindings) {
				return ErrEvidenceConflict
			}
			ids := map[int64]bool{}
			for _, b := range current.Bindings {
				ids[b.ID] = true
			}
			for position, id := range order {
				if !ids[id] {
					return ErrEvidenceConflict
				}
				delete(ids, id)
				if _, err := tx.Exec(`UPDATE finding_traffic_bindings SET position=$1 WHERE finding_id=$2 AND id=$3`, position, findingID, id); err != nil {
					return err
				}
			}
		} else {
			var res sql.Result
			var err error
			if remove {
				res, err = tx.Exec(`DELETE FROM finding_traffic_bindings WHERE finding_id=$1 AND id=$2`, findingID, bindingID)
			} else {
				res, err = tx.Exec(`UPDATE finding_traffic_bindings SET role=COALESCE($3,role),note=COALESCE($4,note) WHERE finding_id=$1 AND id=$2`, findingID, bindingID, role, note)
			}
			if err != nil {
				return err
			}
			n, err := res.RowsAffected()
			if err != nil {
				return err
			}
			if n == 0 {
				return ErrEvidenceNotFound
			}
		}
		return bumpEvidenceVersionTx(tx, findingID)
	})
}

type RecordFindingInput struct {
	TaskID, ExplorationID, IntentID                      int64
	VulnClass, Name, Severity, Summary, Evidence, Worker string
	AssetIDs                                             []int64
	// ReplayEvidenceID 是这条漏洞的「可重放证据」标识（重放语料 id / 流量快照 id /
	// 证据链 id）。
	//
	// 为什么要有它：findings.replay_evidence_id 这一列一直存在（db/schema_sod.sql
	// 引入）却**没有任何写入方** ⇒ 闸门（internal/sod/gate.go 的 tier2 检查）永远
	// 判成「关联漏洞缺少可重放证据」⇒ 每一条与 finding 关联的动作都被拒。
	//
	// 留空 ⇒ 由记录路径按流量证据推导（见 replayEvidenceID）；两者都拿不到时仍写空
	// —— 空是**合法状态**（schema_sod.sql 明写「空 = 尚无可重放证据 ⇒
	// tier2_inconsistent」），由闸门按 fail-closed 拒掉，而不是在登记时炸掉整条
	// 上报路径。
	ReplayEvidenceID string
}

type RecordedFinding struct {
	FindingID int64           `json:"finding_id,string"`
	NodeID    int64           `json:"finding_node_id,string"`
	Traffic   *FindingTraffic `json:"traffic"`
}

// replayEvidenceID 解析这条 finding 的「可重放证据」标识。
//
// 优先级：
//  1. 调用方显式给的（重放语料 id / 证据链 id / 流量快照 id 皆可）；
//  2. 绑定的流量证据里 role='proof' 的那条快照 id —— 快照 id 是快照元数据的
//     内容哈希（见 TrafficSnapshotID），它本身就是一个可重放的把手；
//  3. role='verification' 的那条快照 id；
//  4. 都没有 ⇒ 空串。
//
// ⚠️ 第 4 条不是错误：schema_sod.sql 明写「空 = 该 finding 尚无可重放证据 ⇒ tier2
// 一致性检查判定为 tier2_inconsistent」—— 空是一个**合法状态**，由闸门 fail-closed
// 拒掉；在登记时炸掉整条上报路径只会逼调用方谎报证据。要区分「这条漏洞到底能不能
// 重放」的调用方用 GetFindingReplayEvidenceStrict（返回可区分的错误）。
func replayEvidenceID(in RecordFindingInput, prepared []PreparedTrafficEvidence) string {
	if id := strings.TrimSpace(in.ReplayEvidenceID); id != "" {
		return id
	}
	for _, role := range []string{"proof", "verification"} {
		for _, item := range prepared {
			if item.Ref.Role == role && item.Snapshot.ID != "" {
				return item.Snapshot.ID
			}
		}
	}
	return ""
}

// ctx 由调用方传入本次事务所用的上下文（而非在内部取 context.Background）：
// 事务内新加的推送事件写入同样应受调用方的取消与超时约束。
//
// d 是平台句柄：锚点写入前的范围判定要走 AssetStore.ScopedAssetIDs（全平台
// 唯一的范围判定入口），而判定需要资产库句柄 —— 所以这条路径挂在 *DB 上。
func (d *DB) RecordFindingTx(ctx context.Context, tx *sql.Tx, in RecordFindingInput, prepared []PreparedTrafficEvidence) (*RecordedFinding, error) {
	if err := LockTaskEvidenceTx(tx, in.TaskID); err != nil {
		return nil, err
	}
	if in.TaskID > 0 {
		var expID int64
		if err := tx.QueryRow(`SELECT exploration_id FROM tasks WHERE id=$1`, in.TaskID).Scan(&expID); err != nil {
			return nil, err
		}
		if expID != in.ExplorationID {
			return nil, errors.New("漏洞所属任务与探索记录不匹配")
		}
	}
	// 锚点写入前先判范围（唯一判定入口：AssetStore.ScopedAssetIDs）。
	//
	// 整单拒绝而不是「跳过那一个越界资产、其余照写」：同一个越界 id 会同时落进
	// findings.asset_ids 与推送事件的 payload，逐处过滤漏一处就是泄露；拒绝则
	// 一个字节都不写。错误文案不区分「不存在」与「不在范围内」，因此拒绝不构成
	// 存在性探测器。
	//
	// 没有任务上下文（TaskID<=0）⇒ 没有授权边 ⇒ 不建立任何锚点（fail-closed）。
	if in.TaskID > 0 {
		if err := d.anchorAssetsInScope(in.TaskID, in.AssetIDs); err != nil {
			return nil, err
		}
	}
	if in.IntentID > 0 {
		var ok bool
		if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM exploration_nodes WHERE id=$1 AND exploration_id=$2 AND kind='intent')`, in.IntentID, in.ExplorationID).Scan(&ok); err != nil {
			return nil, err
		}
		if !ok {
			return nil, errors.New("intent_id 必须是本任务的意图（关联任务意图只读）")
		}
	}
	payload, _ := json.Marshal(map[string]any{"vulnclass": in.VulnClass, "name": in.Name, "severity": in.Severity, "summary": in.Summary, "evidence": map[string]any{"by": in.Worker, "poc": in.Evidence}})
	out := &RecordedFinding{}
	// 出生态是 **pending**（已提出，未验证），不是 confirmed。
	//
	// 为什么：「提出」被当成了「验证」⇒ 看不出这条结论有没有被独立复核过，而且
	// 复核不通过时没有退路（既不能关掉，也不能退回给提出它的人）。验证态由
	// db/finding_validation.go 的 ValidateFinding 决定（confirmed / dismissed 结案，
	// rework 打回且状态留在 pending）。
	//
	// ⚠️ 落库前先确保 DDL 到位：ck_node_state 在放行 'pending' 之前只认
	// ('confirmed','dismissed') ⇒ 老库上直接写 pending 会被 CHECK 挡下（整条上报
	// 路径失败）。ensureFindingValidationSchema 是幂等的（第二次起只是一次 map 查表）。
	if err := d.ensureFindingValidationSchema(); err != nil {
		return nil, err
	}
	if err := tx.QueryRow(`INSERT INTO exploration_nodes(exploration_id,kind,payload,priority,state,origin)
VALUES($1,'finding',$2,9,'pending',$3) RETURNING id`, in.ExplorationID, string(payload), in.Worker).Scan(&out.NodeID); err != nil {
		return nil, err
	}
	// 锚点：只有落在任务范围内的资产才建立关系（范围判定见上）。无任务上下文时
	// 一条都不建 —— 没有授权边就没有锚点。
	if in.TaskID > 0 {
		for _, asset := range in.AssetIDs {
			if _, err := tx.Exec(`INSERT INTO exploration_anchors(node_id,asset_id) VALUES($1,$2) ON CONFLICT DO NOTHING`, out.NodeID, asset); err != nil {
				return nil, err
			}
		}
	}
	if in.IntentID > 0 {
		if _, err := tx.Exec(`INSERT INTO exploration_edges(exploration_id,src_id,rel,dst_id) VALUES($1,$2,$3,$4)`, in.ExplorationID, in.IntentID, RelYields, out.NodeID); err != nil {
			return nil, err
		}
	}
	assets := in.AssetIDs
	if assets == nil {
		assets = []int64{}
	}
	raw, _ := json.Marshal(assets)
	replayID := replayEvidenceID(in, prepared)
	if err := tx.QueryRow(`INSERT INTO findings(task_id,node_id,vulnclass,name,severity,summary,evidence,worker,asset_ids,replay_evidence_id)
VALUES(NULLIF($1,0),$2,$3,$4,$5,$6,$7,$8,$9,$10) RETURNING id`, in.TaskID, out.NodeID, in.VulnClass, in.Name, in.Severity, in.Summary, in.Evidence, in.Worker, string(raw), replayID).Scan(&out.FindingID); err != nil {
		return nil, err
	}
	// 在**同一事务**里登记一条推送事件：提交即保证「漏洞落库」与「推送任务存在」
	// 原子一致，不存在提交成功却没入队、消息永久丢失的窗口。
	// 这里的失败被隔离在保存点上、不影响漏洞写入（见函数注释），因此忽略返回值。
	RecordNotificationEventTx(ctx, tx, notify.EventFindingCreated, out.FindingID, notify.Snapshot{
		Kind:      notify.EventFindingCreated,
		FindingID: out.FindingID,
		TaskID:    in.TaskID,
		VulnClass: in.VulnClass,
		Name:      in.Name,
		Severity:  in.Severity,
		Summary:   in.Summary,
		AssetIDs:  assets,
	})
	if err := AddFindingTrafficTx(tx, out.FindingID, prepared); err != nil {
		return nil, err
	}
	var err error
	out.Traffic, err = FindingTrafficTx(tx, out.FindingID)
	if err != nil {
		return nil, err
	}
	// 回写 L3 反向索引的原料：finding id + 它绑定的流量快照 id。
	//
	// ⚠️ 必须在**同一个事务**里：否则会出现「漏洞已落库、节点还没回写」的窗口，
	// 反向索引就静默漏掉一条证据，而且没有任何报错。
	//
	// 位置在 AddFindingTrafficTx **之后**：流量绑定先落库，这里才读得到
	// snapshot_id（放在插入漏洞行之后立刻写，traffic_ids 会永远是空的）。
	snapshotIDs := make([]string, 0, len(out.Traffic.Bindings))
	for _, b := range out.Traffic.Bindings {
		snapshotIDs = append(snapshotIDs, b.SnapshotID)
	}
	if err := linkNodeToFindingTx(tx, in.ExplorationID, out.NodeID, out.FindingID, snapshotIDs); err != nil {
		return nil, err
	}
	return out, nil
}

// RecordFinding is the atomic legacy/no-recorder path used by tools and tests.
func (s *ExplorationStore) RecordFinding(ctx context.Context, in RecordFindingInput) (out *RecordedFinding, err error) {
	in.ExplorationID = s.expID
	err = s.db.WithEvidenceTx(ctx, func(tx *sql.Tx) error { var e error; out, e = s.db.RecordFindingTx(ctx, tx, in, nil); return e })
	return
}

func (d *DB) FindingIDByNodeID(nodeID int64) (id int64, err error) {
	err = d.QueryRow(`SELECT id FROM findings WHERE node_id=$1`, nodeID).Scan(&id)
	return
}

// PopulateFindingTrafficIDs only enriches already-visible nodes. It performs no
// discovery or ID guessing, and leaves the legacy node ID unchanged.
func (s *ExplorationStore) PopulateFindingTrafficIDs(nodes []*Node) error {
	byID := map[int64]*Node{}
	var args []any
	var placeholders []string
	for _, n := range nodes {
		if n == nil || n.Kind != KindFinding || byID[n.ID] != nil {
			continue
		}
		byID[n.ID] = n
		args = append(args, n.ID)
		placeholders = append(placeholders, fmt.Sprintf("$%d", len(args)))
	}
	if len(args) == 0 {
		return nil
	}
	rows, err := s.db.Query(`SELECT f.id,f.node_id,(SELECT count(*) FROM finding_traffic_bindings b WHERE b.finding_id=f.id) FROM findings f WHERE f.node_id IN (`+strings.Join(placeholders, ",")+`)`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var findingID, nodeID int64
		var count int
		if err := rows.Scan(&findingID, &nodeID, &count); err != nil {
			return err
		}
		n := byID[nodeID]
		n.FindingID, n.FindingNodeID, n.TrafficCount = findingID, nodeID, count
	}
	return rows.Err()
}

// SetFindingReportVersionByNodeID 是宿主/UI 的入口：没有任务上下文，写哪条漏洞
// 都行（平台操作者可信）。agent 可达的那条写路径必须用
// SetFindingReportVersionByNodeIDInScope（按漏洞归属判定范围）。
func (d *DB) SetFindingReportVersionByNodeID(ctx context.Context, nodeID int64, report string, version *int64) (n int64, err error) {
	err = d.WithEvidenceTx(ctx, func(tx *sql.Tx) error {
		var id int64
		if err := tx.QueryRow(`SELECT id FROM findings WHERE node_id=$1`, nodeID).Scan(&id); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			return err
		}
		if err := LockFindingEvidenceTx(tx, id, version); err != nil {
			return err
		}
		res, err := tx.Exec(`UPDATE findings SET report=$2,report_evidence_version=COALESCE($3::bigint,CASE WHEN evidence_version=0 THEN 0 ELSE -1 END) WHERE id=$1`, id, report, version)
		if err != nil {
			return err
		}
		n, err = res.RowsAffected()
		return err
	})
	return
}

// SetFindingReportVersionByNodeIDInScope 写入漏洞报告，但只在目标漏洞属于 taskID
// 时才允许 —— 这是 agent 可达的那条写路径。
//
// 跨任务写守卫：findings.task_id 是「这条漏洞属于哪个任务」的唯一列（可为 NULL）。
// 归属不符（含 NULL，即归属不明）一律拒绝：拿到一个 node_id 不等于有权改它的
// 报告。不设任务上下文（taskID<=0）同样拒绝 —— 没有授权边界就没有写权限。
//
// 与 SetFindingReportVersionByNodeID 的区别只在于「有没有任务上下文」这一个判据：
// 无上下文的那个是宿主/UI 的显式入口（平台操作者可信），本函数是任务侧入口。
func (d *DB) SetFindingReportVersionByNodeIDInScope(ctx context.Context, taskID, nodeID int64, report string, version *int64) (n int64, err error) {
	if taskID <= 0 {
		return 0, errors.New("缺少任务上下文：写入漏洞报告必须绑定到具体任务")
	}
	err = d.WithEvidenceTx(ctx, func(tx *sql.Tx) error {
		var id int64
		var owner sql.NullInt64
		if err := tx.QueryRow(`SELECT id,task_id FROM findings WHERE node_id=$1`, nodeID).Scan(&id, &owner); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrFindingOutOfScope
			}
			return err
		}
		if !owner.Valid || owner.Int64 != taskID {
			return ErrFindingOutOfScope
		}
		if err := LockFindingEvidenceTx(tx, id, version); err != nil {
			return err
		}
		res, err := tx.Exec(`UPDATE findings SET report=$2,report_evidence_version=COALESCE($3::bigint,CASE WHEN evidence_version=0 THEN 0 ELSE -1 END) WHERE id=$1`, id, report, version)
		if err != nil {
			return err
		}
		n, err = res.RowsAffected()
		return err
	})
	return
}

// TrafficScopeHosts 返回「绑定到 taskID 的调用者可以读取哪些 host 的流量」。
//
// 关系选择：流量按 host 存储（traffic exchanges.host），资产也带 domain/ip ⇒
// 流量与资产的桥就是 host。这正是实际 schema 支持的关系；流量行本身没有 task
// 维度（流量库全局共享，采集端是同一个代理），findings→asset_ids 那条链只能覆盖
// 「已绑定到漏洞」的那部分流量，撑不起 traffic_search 这种「先找候选再绑定」的用法。
//
// 范围判定复用平台唯一的范围入口（AssetStore.ScopeFragmentFor）：taskID>0 → 该
// 任务声明的范围；taskID<=0 → 平台范围（默认空）⇒ 什么都看不到。
//
// 只取 domain / ip / url 的主机名：能读到哪些 host，就等于能读到那个 host 上抓到
// 的全部凭据。
func (d *DB) TrafficScopeHosts(taskID int64) ([]string, error) {
	fragment, err := d.Assets().ScopeFragmentFor(taskID)
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`WITH `+fragment.CTE+`
SELECT DISTINCT COALESCE(a.domain,''), COALESCE(a.ip,''), COALESCE(a.url,'')
FROM assets a WHERE `+fragment.Predicate, fragment.Args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	hosts := []string{}
	seen := map[string]bool{}
	add := func(host string) {
		host = strings.ToLower(strings.TrimSpace(host))
		if host == "" || seen[host] {
			return
		}
		seen[host] = true
		hosts = append(hosts, host)
	}
	for rows.Next() {
		var domain, ip, rawURL string
		if err := rows.Scan(&domain, &ip, &rawURL); err != nil {
			return nil, err
		}
		add(domain)
		add(ip)
		if parsed, err := url.Parse(rawURL); err == nil {
			add(parsed.Hostname())
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Strings(hosts)
	return hosts, nil
}

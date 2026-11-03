// 本文件是 finding 级验证态（pending/confirmed/dismissed）+ 返工边（rework）的访问层。
//
// 背景：finding 节点**落库即 confirmed** ⇒「提出」被当成了「验证」：没人看得出
// 一条结论有没有被独立复核过；而且复核不通过时没有退路（既不能关掉，也不能退回
// 给提出它的人）。
//
// 状态与边的分工（这是本文件的设计核心）：
//   - 状态回答「这条结论现在处于哪一步」：pending（已提出，未验证）/ confirmed
//     （验证通过，结案）/ dismissed（不成立，结案）。
//   - 返工回答「这次打回给谁」—— 那是**一次动作**，可以发生多次（打回 → 补充 →
//     再打回），状态机表达不了 ⇒ 落成一条边 finding --rework--> intent。
//
// 为什么返工不用第四个状态、也不用第二条 derived_from：
//   - 血缘边 derived_from 在建 finding 时**已经存在**（finding --derived_from-->
//     intent），而边的主键是 (exploration_id, src_id, rel, dst_id) ⇒ 再插一条
//     derived_from 会被 ON CONFLICT DO NOTHING 变成 no-op，返工信息会**静默丢失**。
//
// **验证者 ≠ 提出者**：finding 节点的 origin 列存的是提出者（上报它的那次 worker
// 执行），复核必须由另一个人做 —— 这与 db/sod.go 的 ErrSoDReviewSelfCertification
// 同一条纪律（对抗证伪必须由对抗者做）。裁决在**确定性层**做出：模型只提议。
//
// db/schema.sql 由别的单元持有（RESERVED）⇒ 本单元的 DDL 单独成文件
// （db/schema_finding_validation.sql），由这里幂等补齐。
package db

import (
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"strings"
	"sync"
)

//go:embed schema_finding_validation.sql
var findingValidationSchemaSQL string

// findingValidationSchemaReady 记录某个连接已经应用过 finding 验证态 DDL。
//
// 与 db/sod.go 的 sodSchemaReady 同款：键是底层 *sql.DB 指针（每个测试包/实例各连
// 各的库，互不干扰）。
var findingValidationSchemaReady sync.Map

// EnsureFindingValidationSchema 应用 finding 验证态 + 返工边的幂等 DDL。
//
// 幂等：DDL 自身用 DO 块比对当前约束定义，已经是目标样子就什么都不做 ⇒ 重复应用
// 安全，也不会因为「约束已存在」而漏改别的 kind 的取值。
func (d *DB) EnsureFindingValidationSchema() error {
	if _, ok := findingValidationSchemaReady.Load(d.DB); ok {
		return nil
	}
	if _, err := d.Exec(findingValidationSchemaSQL); err != nil {
		return fmt.Errorf("应用 finding 验证 schema 失败: %w", err)
	}
	findingValidationSchemaReady.Store(d.DB, struct{}{})
	return nil
}

// ensureFindingValidationSchema 是访问层的幂等入口：首次调用时补 DDL，之后只是一次
// map 查表。
func (d *DB) ensureFindingValidationSchema() error { return d.EnsureFindingValidationSchema() }

// FindingValidationVerdict 是确定性层对一条 finding 的裁决（只有三种取值）。
type FindingValidationVerdict string

const (
	// FindingValidationConfirmed 验证通过：结案，节点状态转 confirmed。
	FindingValidationConfirmed FindingValidationVerdict = "confirmed"
	// FindingValidationDismissed 结论不成立：结案，节点状态转 dismissed。
	FindingValidationDismissed FindingValidationVerdict = "dismissed"
	// FindingValidationRework 结论需要返工：**不结案**，状态留在 pending，同时落一条
	// finding --rework--> intent 的返工边。
	FindingValidationRework FindingValidationVerdict = "rework"
)

// finding 节点的验证态（kind='finding' 的 state 取值）。
//
// ⚠️ 与 db/findings.go 的 FindingPending / FindingConfirmed 不是一回事：那两个是
// findings.status 的**处置**状态（待处理 / 已确认 / 已修复…），这里回答的是
// 「这条结论有没有被独立复核过」。两组常量必须分开命名，否则调用方会把「处置」
// 当成「验证」，而那正是本单元要消灭的混淆。
const (
	FindingValidationStatePending   = "pending"
	FindingValidationStateConfirmed = "confirmed"
	FindingValidationStateDismissed = "dismissed"
)

// RelRework 是返工边：finding --rework--> intent（这次打回给谁）。
//
// ⚠️ 与 db/constants.go 的 Rel* 家族同款常量；放在本文件是因为本单元不碰
// db/constants.go（并行单元可能同时改它）。
const RelRework = "rework"

var (
	// ErrFindingValidationSelfVerification 表示验证者就是提出者 —— 自证。
	//
	// 为什么必须有这条：这条 finding 的验证结论会决定「它算不算数」。提出者自己
	// 盖章 ⇒ 「被独立复核过」这件事从未发生，于是整条验证链只剩提示词层面的默契。
	// 与 db/sod.go 的 ErrSoDReviewSelfCertification 同一条纪律。
	ErrFindingValidationSelfVerification = errors.New("验证者不得是自己提出那条结论（复核必须由复核者做）")
	// ErrFindingValidationReasonRequired 表示打回或不成立时没有给理由。
	//
	// 为什么必须有这条：「不成立」与「打回」都是**结论**，没有理由的结论无法被
	// 复核、无法被追溯 ⇒ 不写理由就不许落库。
	ErrFindingValidationReasonRequired = errors.New("打回或不成立必须给出理由")
	// ErrFindingValidationNotPending 表示这条 finding 已经结案，不能再改验证态。
	//
	// 为什么不能重开：验证是一次性裁决。已结案的结论想再来一次，应该**重新提一条
	// finding**，而不是把旧结论改回 pending —— 否则「它到底被验证过没有」这个问题
	// 永远没有确定答案。
	ErrFindingValidationNotPending = errors.New("该 finding 已结案，不能再变更验证态")
	// ErrFindingValidationNotFinding 表示目标节点不存在、或不是 finding 节点。
	ErrFindingValidationNotFinding = errors.New("目标节点不是 finding")
	// ErrFindingValidationReworkTarget 表示返工目标缺失或无效（不是本探索的 intent）。
	ErrFindingValidationReworkTarget = errors.New("返工目标必须是本探索的 intent 节点")
	// ErrFindingValidationVerifierRequired 表示没有给出验证者身份。
	//
	// 为什么单独一条：没有身份的验证**无法**与提出者比对 ⇒ 也就无法证明它不是
	// 自证。放行一个查不出结果的检查等于把它当成通过 —— 与本仓库 fail-closed 的
	// 口径相反。
	ErrFindingValidationVerifierRequired = errors.New("验证者身份不能为空")
)

// FindingValidationInput 是一次「验证一条 finding」的输入。
type FindingValidationInput struct {
	// ExplorationID 是这条 finding 所属的探索（>0）。store 入口会自动填成本 store
	// 的探索；直接调 db 层时必须自己给 —— 验证是**写**动作，不能落到别的探索上。
	ExplorationID int64
	// NodeID 是 finding 节点 id（exploration_nodes.id）。
	NodeID int64
	// Verdict 是裁决：confirmed / dismissed / rework。
	Verdict FindingValidationVerdict
	// Verifier 是复核者身份（agent key / worker 名）：不得等于提出者。
	Verifier string
	// Reason 是理由：dismissed / rework 必填（confirmed 可空）。
	Reason string
	// ReworkTarget 是返工目标 intent 节点 id：rework 必填。
	ReworkTarget int64
}

// ValidateFinding 验证一条 finding 并落库裁决（确定性层）。
//
// 一个事务里做完，顺序不可换：
//  1. 锁住节点行（FOR UPDATE）⇒ 并发的两次验证不会互相覆盖；
//  2. 断言 kind='finding'（不是漏洞的节点不许走这条路径）；
//  3. 断言 state='pending'（已结案的结论不许被重新打开）；
//  4. 断言验证者 ≠ 提出者（origin）。
//
// 然后按裁决落地：
//   - confirmed → state='confirmed'，清掉 blocked_reason；
//   - dismissed → state='dismissed'，理由记进 blocked_reason；
//   - rework    → 状态**留在 pending**（返工不是结案），理由记进 blocked_reason，
//     并落一条 finding --rework--> intent 的返工边。
//
// 失败一律返回**可区分**的错误（errors.Is）：调用方要能分辨「自证」「缺理由」
// 「已结案」「不是 finding」「返工目标不对」—— 它们的处置完全不同（换人复核 /
// 补理由 / 重新提一条 / 传对节点 / 传对 intent）。
func (d *DB) ValidateFinding(in FindingValidationInput) error {
	if err := d.ensureFindingValidationSchema(); err != nil {
		return err
	}
	verifier := strings.TrimSpace(in.Verifier)
	if verifier == "" {
		return ErrFindingValidationVerifierRequired
	}
	reason := strings.TrimSpace(in.Reason)
	switch in.Verdict {
	case FindingValidationConfirmed, FindingValidationDismissed, FindingValidationRework:
	default:
		return fmt.Errorf("验证裁决只能是 confirmed/dismissed/rework，收到 %q", in.Verdict)
	}
	if in.Verdict != FindingValidationConfirmed && reason == "" {
		return ErrFindingValidationReasonRequired
	}
	if in.Verdict == FindingValidationRework && in.ReworkTarget <= 0 {
		return fmt.Errorf("%w: 未指定返工目标", ErrFindingValidationReworkTarget)
	}
	if in.ExplorationID <= 0 || in.NodeID <= 0 {
		return fmt.Errorf("%w: 缺少探索或节点 id", ErrFindingValidationNotFinding)
	}

	tx, err := d.Begin()
	if err != nil {
		return fmt.Errorf("开启验证事务失败: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	// 作用域与锁一起落在 WHERE 上：别的探索的节点 id 一律当「不是 finding」处理
	// （错误文案不区分「不存在」与「不在本探索内」⇒ 不构成存在性探测器）。
	var (
		kind   string
		state  string
		origin string
	)
	err = tx.QueryRow(`SELECT kind, state, COALESCE(origin,'') FROM exploration_nodes
 WHERE id=$1 AND exploration_id=$2 FOR UPDATE`, in.NodeID, in.ExplorationID).
		Scan(&kind, &state, &origin)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: 节点 %d 不在本探索内", ErrFindingValidationNotFinding, in.NodeID)
		}
		return fmt.Errorf("读取 finding 节点失败: %w", err)
	}
	if kind != KindFinding {
		return fmt.Errorf("%w: 节点 %d 是 %s", ErrFindingValidationNotFinding, in.NodeID, kind)
	}
	if state != FindingValidationStatePending {
		return fmt.Errorf("%w: 当前状态 %q", ErrFindingValidationNotPending, state)
	}
	// 提出者没记身份（origin 为空）时**不**算自证 —— 那是「拿不到身份」的降级情形，
	// 不是「自己给自己盖章」。判据与 db/sod.go 的自证守卫一字不差。
	if origin != "" && origin == verifier {
		return fmt.Errorf("%w: 验证者 %q", ErrFindingValidationSelfVerification, verifier)
	}
	if in.Verdict == FindingValidationRework {
		var ok bool
		if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM exploration_nodes
 WHERE id=$1 AND exploration_id=$2 AND kind='intent')`, in.ReworkTarget, in.ExplorationID).Scan(&ok); err != nil {
			return fmt.Errorf("校验返工目标失败: %w", err)
		}
		if !ok {
			return fmt.Errorf("%w: intent %d 不属于本探索", ErrFindingValidationReworkTarget, in.ReworkTarget)
		}
	}

	switch in.Verdict {
	case FindingValidationConfirmed:
		if _, err := tx.Exec(`UPDATE exploration_nodes
   SET state=$1, blocked_reason=NULL, content_version=content_version+1
 WHERE id=$2`, FindingValidationStateConfirmed, in.NodeID); err != nil {
			return fmt.Errorf("写验证态失败: %w", err)
		}
	case FindingValidationDismissed:
		if _, err := tx.Exec(`UPDATE exploration_nodes
   SET state=$1, blocked_reason=$2, content_version=content_version+1
 WHERE id=$3`, FindingValidationStateDismissed, reason, in.NodeID); err != nil {
			return fmt.Errorf("写验证态失败: %w", err)
		}
	case FindingValidationRework:
		// 返工**不结案**：状态留在 pending，等提出方补充后重新验证。
		if _, err := tx.Exec(`UPDATE exploration_nodes
   SET blocked_reason=$1, content_version=content_version+1
 WHERE id=$2`, reason, in.NodeID); err != nil {
			return fmt.Errorf("写返工理由失败: %w", err)
		}
		// 返工边：finding --rework--> intent（这次打回给谁）。
		// ON CONFLICT DO NOTHING：同一条 (finding, intent) 只留一条边 —— 边的语义是
		// 「这条结论被打回给过这条意图」，不是次数（次数记在理由列里）。
		if _, err := tx.Exec(`INSERT INTO exploration_edges(exploration_id,src_id,rel,dst_id)
VALUES($1,$2,$3,$4) ON CONFLICT (exploration_id,src_id,rel,dst_id) DO NOTHING`,
			in.ExplorationID, in.NodeID, RelRework, in.ReworkTarget); err != nil {
			return fmt.Errorf("写返工边失败: %w", err)
		}
	default:
		return fmt.Errorf("验证裁决无法落库: %q", in.Verdict)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交验证事务失败: %w", err)
	}
	return nil
}

// ValidateFinding 验证**本探索**里的一条 finding。
//
// 与 db 层唯一的差别是**作用域**：store 的探索是固定的 ⇒ 节点必须属于它。跨探索
// 的节点 id 一律按「不是 finding」处理（不区分「不存在」与「在别处」⇒ 不构成存在
// 性探测器）。
func (s *ExplorationStore) ValidateFinding(in FindingValidationInput) error {
	scoped := in
	scoped.ExplorationID = s.expID
	return s.db.ValidateFinding(scoped)
}

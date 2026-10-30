package sod

import (
	"context"
	"errors"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/internal/sodpolicy"
)

// Gate 是确定性闸门：五道检查 + 短路顺序 + 原因码。
//
// ⚠️ Gate **不解析 shell**、**不读原始动作**、**不调模型**。它只读规整器产出的
// 结构化字段 + 宿主查到的实时状态。裁决只有这一处来源 —— 「模型只提议，
// 确定性层裁决」在这里落地。
//
// ⚠️ 顺序不可改：scope → tool_health → quota → tier2 → anomaly。
// ① scope 是授权边界（越界是最高等级的越权，必须最先说）；② 熔断是工具可用性；
// ③ 额度是资源边界；④ tier2 是证据完整性；⑤ anomaly 只是观察信号（规格明写
// anomaly **不是**拒绝，是 needs_review —— 拒掉会丢掉「目标侧对抗行为」的
// 检测信号）。
//
// 每一道检查【查不出来】都按**拒绝**处理（fail-closed）：查不出来不等于没有
// 限制。这与 db/quota.go 的 GroupQuotaState 契约一字不差。
type Gate struct {
	assets *db.AssetStore
	pg     *db.DB
	// deps 是五道检查的取数口。生产路径上是 dbGateDeps（读 assets/pg）；
	// 测试里换成替身 ⇒ 每一道检查都能被单独驱动并计数。
	//
	// 为什么留这道缝：短路顺序是本闸门最要紧的性质（「scope 失败 ⇒ 后面四道
	// 检查一次都没跑」），而对着真 DB 无法断言「没跑」—— 只有替身能计数。
	deps gateDeps
}

// gateDeps 是闸门取实时状态的口子。
//
// 只暴露闸门真正需要的六项查询，不暴露 db 句柄 —— 闸门拿到 db 句柄就能写库，
// 而闸门**只该读**。
type gateDeps interface {
	scopeHostInScope(taskID int64, host string) (bool, error)
	toolCircuitState(toolKey string) (string, error)
	groupIDForScope(taskID, conversationID int64) (int64, error)
	groupQuotaState(groupID int64) (db.QuotaState, error)
	findingReplayEvidence(findingID int64) (*db.ReplayEvidence, error)
	scopedAssetIDs(taskID int64, ids []int64) ([]int64, error)
}

// dbGateDeps 是生产实现：直接读 db 包。
//
// ⚠️ 每个方法都先判空：闸门被装配成零值（没给资产库 / 没给 DB）时，检查必须
// **报不可用**而不是 panic —— 不可用 ⇒ fail-closed 拒绝，这是唯一安全的落法。
type dbGateDeps struct {
	assets *db.AssetStore
	pg     *db.DB
}

func (d dbGateDeps) scopeHostInScope(taskID int64, host string) (bool, error) {
	if d.assets == nil {
		return false, errors.New("资产库未装配，范围判定不可用")
	}
	return d.assets.ScopeHostInScope(taskID, host)
}

func (d dbGateDeps) toolCircuitState(toolKey string) (string, error) {
	if d.pg == nil {
		return "", errors.New("数据库未装配，工具健康状态不可用")
	}
	return d.pg.ToolCircuitState(toolKey)
}

func (d dbGateDeps) groupIDForScope(taskID, conversationID int64) (int64, error) {
	if d.pg == nil {
		return 0, errors.New("数据库未装配，群组归属不可用")
	}
	return d.pg.GroupIDForScope(taskID, conversationID)
}

func (d dbGateDeps) groupQuotaState(groupID int64) (db.QuotaState, error) {
	if d.pg == nil {
		return db.QuotaStateUnset, errors.New("数据库未装配，群组额度状态不可用")
	}
	return d.pg.GroupQuotaState(groupID)
}

func (d dbGateDeps) findingReplayEvidence(findingID int64) (*db.ReplayEvidence, error) {
	if d.pg == nil {
		return nil, errors.New("数据库未装配，finding 证据不可用")
	}
	return d.pg.GetFindingReplayEvidence(findingID)
}

func (d dbGateDeps) scopedAssetIDs(taskID int64, ids []int64) ([]int64, error) {
	if d.assets == nil {
		return nil, errors.New("资产库未装配，资产归属不可判定")
	}
	return d.assets.ScopedAssetIDs(taskID, ids)
}

// NewGate 构造闸门。
func NewGate(assets *db.AssetStore, pg *db.DB) *Gate {
	return &Gate{assets: assets, pg: pg, deps: dbGateDeps{assets: assets, pg: pg}}
}

// GateInput 是闸门的输入（全部由【代码】准备，不含任何模型自由文本的解释权）。
type GateInput struct {
	ToolName       string
	SourceAgent    string
	TaskID         int64
	ConversationID int64
	PayloadDigest  string
	ScopeDigest    string
	Normalized     NormalizerResult
	// FindingID 是本次调用关联的 finding（0 = 不关联）。
	FindingID int64
}

// GateOutput 是闸门的产出。
type GateOutput struct {
	Verdict    string
	ReasonCode string
	Checks     map[string]string
	Note       string
}

// Evaluate 跑五道检查（短路）。
//
// 顺序（不可改）：scope → tool_health → quota → tier2 → anomaly。
// 任何一道检查【查不出来】⇒ **拒绝**（fail-closed），原因码是 xxx_unavailable。
//
// ctx 目前不参与判定：db 访问层的签名不带 ctx（它是既有契约，本单元不改），
// ctx 留给后续把查询改成可取消时使用。
func (g *Gate) Evaluate(ctx context.Context, in GateInput) GateOutput {
	out := GateOutput{Checks: map[string]string{}}
	deps := g.deps
	if deps == nil {
		deps = dbGateDeps{assets: g.assets, pg: g.pg}
	}

	// ① scope —— 授权边界，最高优先级。
	out.Checks["scope_check"] = "fail"
	if in.Normalized.StructuredIntent.TargetHint == "" {
		// 没有目标线索 ⇒ 无法判定范围 ⇒ **不放行**。
		out.Verdict = sodpolicy.VerdictDeny
		out.ReasonCode = sodpolicy.ReasonOutOfScope
		out.Note = "本次动作没有可判定的目标线索，无法确认其在授权范围内"
		return out
	}
	ok, err := deps.scopeHostInScope(in.TaskID, in.Normalized.StructuredIntent.TargetHint)
	if err != nil {
		out.Verdict = sodpolicy.VerdictDeny
		out.ReasonCode = sodpolicy.ReasonScopeUnavailable
		out.Note = "范围判定不可用，按失败关闭处理"
		return out
	}
	if !ok {
		out.Verdict = sodpolicy.VerdictDeny
		out.ReasonCode = sodpolicy.ReasonOutOfScope
		out.Note = "目标不在本任务的授权范围内"
		return out
	}
	out.Checks["scope_check"] = "pass"

	// ② tool health —— 工具熔断。
	state, err := deps.toolCircuitState(in.ToolName)
	if err != nil {
		out.Verdict = sodpolicy.VerdictDeny
		out.ReasonCode = sodpolicy.ReasonToolHealthUnavailable
		out.Note = "工具健康状态不可用，按失败关闭处理"
		return out
	}
	if state == sodpolicy.CircuitOpen {
		out.Verdict = sodpolicy.VerdictDeny
		out.ReasonCode = sodpolicy.ReasonToolCircuitOpen
		out.Note = "该工具当前处于熔断状态"
		return out
	}
	out.Checks["tool_health_check"] = "pass"

	// ③ quota —— 群组额度。
	groupID, err := deps.groupIDForScope(in.TaskID, in.ConversationID)
	if err != nil {
		out.Verdict = sodpolicy.VerdictDeny
		out.ReasonCode = sodpolicy.ReasonQuotaUnavailable
		out.Note = "群组额度状态不可用，按失败关闭处理"
		return out
	}
	if groupID > 0 {
		qstate, err := deps.groupQuotaState(groupID)
		if err != nil {
			out.Verdict = sodpolicy.VerdictDeny
			out.ReasonCode = sodpolicy.ReasonQuotaUnavailable
			out.Note = "群组额度状态不可用，按失败关闭处理"
			return out
		}
		if db.QuotaStateDeniesNewCalls(qstate) {
			out.Verdict = sodpolicy.VerdictDeny
			out.ReasonCode = sodpolicy.ReasonQuotaExhausted
			out.Note = "群组额度处于拒绝态"
			return out
		}
	}
	out.Checks["quota_check"] = "pass"

	// ④ tier2 —— 证据一致性（不关联 finding ⇒ not_applicable）。
	if in.FindingID <= 0 {
		out.Checks["tier2_check"] = "not_applicable"
	} else {
		ev, err := deps.findingReplayEvidence(in.FindingID)
		if err != nil {
			out.Verdict = sodpolicy.VerdictDeny
			out.ReasonCode = sodpolicy.ReasonTier2Unavailable
			out.Note = "finding 证据不可读，按失败关闭处理"
			return out
		}
		if ev == nil || ev.ReplayEvidenceID == "" {
			out.Verdict = sodpolicy.VerdictDeny
			out.ReasonCode = sodpolicy.ReasonTier2Inconsistent
			out.Note = "关联漏洞缺少可重放证据"
			return out
		}
		if len(ev.AssetIDs) > 0 {
			inScope, err := deps.scopedAssetIDs(in.TaskID, ev.AssetIDs)
			if err != nil {
				out.Verdict = sodpolicy.VerdictDeny
				out.ReasonCode = sodpolicy.ReasonTier2Unavailable
				out.Note = "finding 资产归属不可判定，按失败关闭处理"
				return out
			}
			if len(inScope) != len(ev.AssetIDs) {
				out.Verdict = sodpolicy.VerdictDeny
				out.ReasonCode = sodpolicy.ReasonTier2Inconsistent
				out.Note = "关联漏洞的资产归属与本任务范围不一致"
				return out
			}
		}
		out.Checks["tier2_check"] = "pass"
	}

	// ⑤ anomaly —— 观察信号，**不是拒绝**。
	if in.Normalized.AnomalyFlag {
		out.Verdict = sodpolicy.VerdictNeedsReview
		out.ReasonCode = sodpolicy.ReasonAnomalyFlagged
		out.Note = "动作存在异常特征，转入对抗复核"
		out.Checks["anomaly_check"] = "flagged"
		return out
	}
	out.Checks["anomaly_check"] = "clean"

	out.Verdict = sodpolicy.VerdictApprove
	out.ReasonCode = sodpolicy.ReasonAllChecksPassed
	out.Note = "全部确定性检查通过"
	return out
}

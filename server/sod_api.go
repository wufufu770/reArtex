package server

// 本文件是 SoD 链（职责分离）三个**只读**观测面 + 人工复核改判（F3）的 HTTP 出口：
//
//   - 决策账本（sod_decisions）：每一次裁决的完整留痕；
//   - 对抗复核队列（sod_adversarial_review_pending）：交给 redteam 证伪的清单；
//   - 工具熔断状态（sod_tool_circuit）：此刻哪些工具是关着的。
//
// 【为什么这三个面必须有对外的读通道】
//   - 决策账本是「每次决策都留痕」的落点。读不出来 = 没留痕 ⇒ 读失败必须报错
//     （fail closed），绝不能报成「没有决策」。
//   - 对抗复核队列是 needs_review 的**唯一**去处：它交给 redteam 证伪，不是
//     「请人看一眼」—— 人工审批队列是 intercept_pending，另一张表。所以响应里
//     必须把「advisory、不是审批」写明。
//   - 工具熔断的作用是「把坏掉的工具关掉」。关掉了却没人看得见，它就只是一次
//     静默的失败。
//
// 【只读】三个观测面一个字节都不写；唯一例外是 F3 的人工复核改判（写
// sod_manual_overrides 放行凭据——它是人工意愿的登记，不是观测）。

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/internal/sodpolicy"
)

// 三个只读面的单页上限。人工复核面不需要无界返回（与 server/strategy_api.go 的
// strategyListLimit 同一量级）。
const (
	sodDecisionListLimit    = 200
	sodReviewQueueListLimit = 200
	sodCircuitListLimit     = 200
)

// sodDecisionDTO 是决策账本一行的对外形状。
//
// 为什么要 DTO：db.SoDDecision 没有 json tag ⇒ 直接序列化会吐出 Go 字段名
// （ID / TaskID / …），与本仓库其它 API 的 snake_case 口径不一致。
type sodDecisionDTO struct {
	ID             int64           `json:"id"`
	TaskID         *int64          `json:"task_id,omitempty"`
	ConversationID *int64          `json:"conversation_id,omitempty"`
	SourceAgent    string          `json:"source_agent"`
	ToolName       string          `json:"tool_name"`
	Verdict        string          `json:"verdict"`
	ReasonCode     string          `json:"reason_code"`
	PayloadDigest  string          `json:"payload_digest"`
	PayloadPreview string          `json:"payload_preview"`
	IntentDigest   string          `json:"intent_digest"`
	AnomalyFlag    bool            `json:"anomaly_flag"`
	AnomalyReason  string          `json:"anomaly_reason"`
	Checks         json.RawMessage `json:"checks"`
	TokenDigest    string          `json:"token_digest"`
	ScopeDigest    string          `json:"scope_digest"`
	CacheHit       bool            `json:"cache_hit"`
	LatencyMs      int             `json:"latency_ms"`
	CreatedAt      string          `json:"created_at"`

	// 分层信息（自动权限分层，agent/tier_judge.go）。
	Tier         string `json:"tier"`
	TierCategory string `json:"tier_category"`
	TierHostOnly bool   `json:"tier_host_only"`
	// TierScopeIn 为 null = 账本里没有范围结论（未判定）。
	//
	// 为什么用可空而不是 false：「未判定」与「判定为不在范围内」在分层的
	// 含义上不同（前者 fail closed 拒绝，后者是确凿的越界），糊成一个 false
	// 会让这两种情况长得一样 ⇒ 事后无法排查。
	TierScopeIn *bool  `json:"tier_scope_in"`
	TierReason  string `json:"tier_reason"`
	// TierDerived = true = 这一层的结论是**读时推导**的，不是账本原文。
	TierDerived bool `json:"tier_derived"`
}

// sodReviewItemDTO 是对抗复核队列一项的对外形状（理由同 sodDecisionDTO）。
type sodReviewItemDTO struct {
	ID               int64           `json:"id"`
	TaskID           *int64          `json:"task_id,omitempty"`
	ConversationID   *int64          `json:"conversation_id,omitempty"`
	SourceAgent      string          `json:"source_agent"`
	ToolName         string          `json:"tool_name"`
	PayloadDigest    string          `json:"payload_digest"`
	PayloadPreview   string          `json:"payload_preview"`
	AnomalyFlag      bool            `json:"anomaly_flag"`
	AnomalyReason    string          `json:"anomaly_reason"`
	ReasonCode       string          `json:"reason_code"`
	StructuredIntent json.RawMessage `json:"structured_intent"`
	Status           string          `json:"status"`
	ClaimedBy        string          `json:"claimed_by"`
	Resolution       string          `json:"resolution"`
	CreatedAt        string          `json:"created_at"`
	UpdatedAt        string          `json:"updated_at"`
}

// sodCircuitDTO 是一个工具的熔断状态。
//
// State 是**有效状态**（闸门此刻实际执行的那个）：开路超过
// sodpolicy.CircuitOpenDurationSeconds 之后，行里仍写着 open，但闸门已按
// half_open 处理。RecordedState 是行里的原始值 —— 两个都要给，否则看的人会
// 以为「行里写 open 就是还在熔断期」。
type sodCircuitDTO struct {
	ToolKey       string `json:"tool_key"`
	State         string `json:"state"`
	RecordedState string `json:"recorded_state"`
	Present       bool   `json:"present"`
	FailCount     int    `json:"fail_count"`
	OpenedAt      string `json:"opened_at,omitempty"`
	Reason        string `json:"reason"`
	UpdatedAt     string `json:"updated_at,omitempty"`
}

// sodDecisionList 读决策账本。
//
// ?task_id=<id> 只列该任务的决策；不传 = 跨任务看账。?limit= 限单页条数。
//
// ⚠️ **读不出来必须报错**：账本是审计记录，返回 200 + 空列表等于告诉操作者
// 「没人做过裁决」—— 那是最危险的假象（fail closed）。
func (s *Server) sodDecisionList(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	taskID, ok := sodOptionalInt64(w, r.URL.Query().Get("task_id"), "task_id")
	if !ok {
		return
	}
	limit := sodLimitParam(r, sodDecisionListLimit)
	items, err := pg.ListSoDDecisions(taskID, limit)
	if err != nil {
		writeErr(w, 500, "读决策账本失败: "+err.Error())
		return
	}
	out := make([]sodDecisionDTO, 0, len(items))
	for _, d := range items {
		out = append(out, sodDecisionDTOOf(d))
	}
	writeJSON(w, 200, map[string]any{
		"decisions": out,
		"count":     len(out),
		"limit":     limit,
		// 分层结论是**读时推导**的（原始载荷不入库 ⇒ 推导值是下界）。
		// 把这句话写进响应，操作者才不会把它当成账本原文。
		"tier_note": "tier 为读时推导值（依据：工具 key + 账本里的脱敏载荷预览 + 账本记下的范围检查结论）；" +
			"原始载荷不入库、调用点的写语义也不在账本里 ⇒ 推导值是下界，真值只可能更高。",
	})
}

// sodDecisionDTOOf 把一行决策组装成对外形状（含分层信息）。
func sodDecisionDTOOf(d *db.SoDDecision) sodDecisionDTO {
	tier := sodDecisionTier(d)
	out := sodDecisionDTO{
		ID:             d.ID,
		TaskID:         d.TaskID,
		ConversationID: d.ConversationID,
		SourceAgent:    d.SourceAgent,
		ToolName:       d.ToolName,
		Verdict:        d.Verdict,
		ReasonCode:     d.ReasonCode,
		PayloadDigest:  d.PayloadDigest,
		PayloadPreview: d.PayloadPreview,
		IntentDigest:   d.IntentDigest,
		AnomalyFlag:    d.AnomalyFlag,
		AnomalyReason:  d.AnomalyReason,
		Checks:         d.Checks,
		TokenDigest:    d.TokenDigest,
		ScopeDigest:    d.ScopeDigest,
		CacheHit:       d.CacheHit,
		LatencyMs:      d.LatencyMs,
		CreatedAt:      rfc3339(d.CreatedAt),
		Tier:           tier.Tier,
		TierCategory:   tier.Category,
		TierHostOnly:   tier.HostOnly,
		TierScopeIn:    tier.ScopeIn,
		TierReason:     tier.Reason,
		TierDerived:    true,
	}
	if len(out.Checks) == 0 {
		out.Checks = json.RawMessage(`{}`)
	}
	return out
}

// sodTierVerdict 是推导出来的分层结论。
type sodTierVerdict struct {
	Tier     string
	Category string
	HostOnly bool
	// ScopeIn 可空：null = 账本里没有范围结论。
	ScopeIn *bool
	Reason  string
}

// sodDecisionTier 推导一条决策的权限层级。
//
// 【为什么读时推导，不落列】
//
//	落列要**写路径**把层级写进账本，而写路径在 internal/sod 的审计器里
//	（db.RecordSoDDecision）。那一侧不归本单元管；在这里加一列 ⇒ 永远没有人
//	会填它 ⇒ 一列恒为空的数据比没有这列更坏（它让人以为账本里有层级）。
//	读时推导的另一个好处：判定器本身是**纯函数**（agent.TierOfAction），
//	推导结果不会与判定器漂移 —— 分层清单改了，推导结果跟着改。
//
// 【推导出来的是下界，不是真值】
//
//	账本只存载荷的**摘要 + 脱敏预览**（原始载荷永不入库），而层级判定要读入参
//	（asset_ids / owner / state）与调用点声明的写语义（出网 / 扩权 / 提议）。
//	拿不到的那些一律按「没有发生」处理 ⇒ 推出来的层级只可能**比真值低**，
//	不会比真值高。方向是安全的：Tier3 不会被报成「可自动放行」的 Tier2。
//
//	范围结论取自账本自己记下的 scope 检查（宿主侧闸门的判定，不是 agent 可写的
//	数据）；查不出来的那次（原因码 scope_check_unavailable）算「未判定」。
func sodDecisionTier(d *db.SoDDecision) sodTierVerdict {
	verdict := agent.TierOfAction(agent.TierAction{
		ToolKey: d.ToolName,
		Args:    sodDecisionArgs(d),
		Scope:   sodDecisionScope(d),
	})
	out := sodTierVerdict{
		Tier:     verdict.Tier.String(),
		Category: string(verdict.Category),
		HostOnly: verdict.HostOnly,
		Reason:   verdict.Reason,
	}
	if scopeIn, known := sodDecisionScopeIn(d); known {
		out.ScopeIn = &scopeIn
	}
	return out
}

// sodDecisionArgs 从账本里的脱敏载荷预览还原入参。
//
// 预览是**脱敏后**的载荷前缀（可能被截断）：脱敏只碰凭据，不会碰层级判定要读
// 的 asset_ids / owner / state ⇒ 对分层判定来说它是个可用的近似。解不出 JSON
// 对象就当没有入参（判定器按「没给条件」处理 ⇒ 落到 Tier3，方向安全）。
func sodDecisionArgs(d *db.SoDDecision) json.RawMessage {
	preview := strings.TrimSpace(d.PayloadPreview)
	if !strings.HasPrefix(preview, "{") {
		return nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(preview), &obj); err != nil {
		return nil
	}
	return json.RawMessage(preview)
}

// sodDecisionScope 把账本记下的范围检查结论翻成分层判定要的范围判定。
//
// 三种情况必须分清：判过且在范围内 / 判过且不在范围内 / 没判过。
// 「没判过」在分层判定里是 fail closed（不放行），不能当成「在范围内」。
func sodDecisionScope(d *db.SoDDecision) agent.ScopeVerdict {
	if inScope, known := sodDecisionScopeIn(d); known {
		return agent.ScopeVerdict{Assessed: true, AssetInScope: inScope}
	}
	return agent.ScopeVerdict{Assessed: false}
}

// sodDecisionScopeIn 读账本里的范围检查结论。known=false = 没判过。
func sodDecisionScopeIn(d *db.SoDDecision) (inScope, known bool) {
	checks := map[string]string{}
	if len(d.Checks) > 0 {
		_ = json.Unmarshal(d.Checks, &checks)
	}
	switch checks["scope_check"] {
	case "pass":
		return true, true
	case "fail":
		// 「fail」有两种：确定越界（判过）与查不出来（没判过）。
		if d.ReasonCode == sodpolicy.ReasonScopeUnavailable {
			return false, false
		}
		return false, true
	default:
		return false, false
	}
}

// sodReviewQueue 读对抗复核队列。
//
// ⚠️ 这个队列**不是**人工审批队列：needs_review 的唯一去处是这张表，它由 redteam
// 消费（对抗证伪）。人工审批队列是 intercept_pending，另一张表。响应里必须把
// 「advisory、不是审批」写明，否则操作者会把它当成「等人点头的清单」。
func (s *Server) sodReviewQueue(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	taskID, ok := sodOptionalInt64(w, r.URL.Query().Get("task_id"), "task_id")
	if !ok {
		return
	}
	limit := sodLimitParam(r, sodReviewQueueListLimit)
	items, err := pg.ListAdversarialReviewPending(taskID, limit)
	if err != nil {
		writeErr(w, 500, "读对抗复核队列失败: "+err.Error())
		return
	}
	out := make([]sodReviewItemDTO, 0, len(items))
	for _, it := range items {
		out = append(out, sodReviewItemDTOOf(it))
	}
	writeJSON(w, 200, map[string]any{
		"queue":  out,
		"count":  len(out),
		"limit":  limit,
		"status": "pending",
		// 下面三个字段是**语义声明**，不是装饰：前端据此把这一页标成
		// 「对抗证伪清单」而不是「待人工审批」。
		"advisory_only":        true,
		"human_approval_queue": false,
		"consumer":             "redteam",
		"purpose":              "adversarial_falsification",
		"note": "本队列是交给对抗者（redteam）证伪的清单，不是人工审批队列；" +
			"在这里点「同意 / 拒绝」不构成任何裁决（人工审批走 intercept_pending）。",
	})
}

// sodReviewItemDTOOf 把一项待复核组装成对外形状。
func sodReviewItemDTOOf(it *db.AdversarialReviewItem) sodReviewItemDTO {
	out := sodReviewItemDTO{
		ID:               it.ID,
		TaskID:           it.TaskID,
		ConversationID:   it.ConversationID,
		SourceAgent:      it.SourceAgent,
		ToolName:         it.ToolName,
		PayloadDigest:    it.PayloadDigest,
		PayloadPreview:   it.PayloadPreview,
		AnomalyFlag:      it.AnomalyFlag,
		AnomalyReason:    it.AnomalyReason,
		ReasonCode:       it.ReasonCode,
		StructuredIntent: it.StructuredIntent,
		Status:           it.Status,
		ClaimedBy:        it.ClaimedBy,
		Resolution:       it.Resolution,
		CreatedAt:        rfc3339(it.CreatedAt),
		UpdatedAt:        rfc3339(it.UpdatedAt),
	}
	if len(out.StructuredIntent) == 0 {
		out.StructuredIntent = json.RawMessage(`{}`)
	}
	return out
}

// sodCircuitList 读全部工具的熔断状态。
//
// 熔断只影响**已经出过事**的工具，所以这张表通常很短。逐条走
// db.ToolCircuitState（而不是在 server 侧重算折算）—— 折算规则
// （开路超期 ⇒ half_open）是安全边界，必须只有一份。
func (s *Server) sodCircuitList(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	if err := pg.EnsureSoDSchema(); err != nil {
		writeErr(w, 500, "读工具熔断状态失败: "+err.Error())
		return
	}
	rows, err := pg.Query(`SELECT tool_key, state, fail_count, opened_at, reason, updated_at
FROM sod_tool_circuit ORDER BY tool_key LIMIT $1`, sodCircuitListLimit)
	if err != nil {
		writeErr(w, 500, "读工具熔断状态失败: "+err.Error())
		return
	}
	defer rows.Close()
	out := []sodCircuitDTO{}
	counts := map[string]int{}
	for rows.Next() {
		var (
			key      string
			recorded string
			fails    int
			openedAt *time.Time
			reason   string
			updated  *time.Time
		)
		if err := rows.Scan(&key, &recorded, &fails, &openedAt, &reason, &updated); err != nil {
			writeErr(w, 500, "读工具熔断状态失败: "+err.Error())
			return
		}
		state, err := pg.ToolCircuitState(key)
		if err != nil {
			// 状态解析不出来 ⇒ 报错（fail closed），不跳过这一行：跳过去就等于
			// 「这个工具没熔断」，而闸门那边恰恰是拒绝。
			writeErr(w, 500, "读工具熔断状态失败: "+err.Error())
			return
		}
		counts[state]++
		out = append(out, sodCircuitDTO{
			ToolKey:       key,
			State:         state,
			RecordedState: recorded,
			Present:       true,
			FailCount:     fails,
			OpenedAt:      sodTimeRFC(openedAt),
			Reason:        reason,
			UpdatedAt:     sodTimeRFC(updated),
		})
	}
	if err := rows.Err(); err != nil {
		writeErr(w, 500, "读工具熔断状态失败: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{
		"circuits": out,
		"count":    len(out),
		"states":   counts,
		"note": "state 是闸门此刻实际执行的有效状态（开路超期后按 half_open 处理）；" +
			"recorded_state 是行里记着的原始值。",
	})
}

// sodCircuitGet 读一个工具的熔断状态。
//
// 没有行 ⇒ closed（这个工具还没出过事），不是「查不到」。
func (s *Server) sodCircuitGet(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	toolKey := strings.TrimSpace(r.PathValue("tool_key"))
	if toolKey == "" {
		writeErr(w, 400, "工具 key 不能为空")
		return
	}
	if err := pg.EnsureSoDSchema(); err != nil {
		writeErr(w, 500, "读工具熔断状态失败: "+err.Error())
		return
	}
	state, err := pg.ToolCircuitState(toolKey)
	if err != nil {
		writeErr(w, 500, "读工具熔断状态失败: "+err.Error())
		return
	}
	dto := sodCircuitDTO{ToolKey: toolKey, State: state}
	var (
		recorded string
		fails    int
		openedAt *time.Time
		reason   string
		updated  *time.Time
	)
	err = pg.QueryRow(`SELECT state, fail_count, opened_at, reason, updated_at
FROM sod_tool_circuit WHERE tool_key=$1`, toolKey).
		Scan(&recorded, &fails, &openedAt, &reason, &updated)
	switch {
	case err == nil:
		dto.RecordedState = recorded
		dto.Present = true
		dto.FailCount = fails
		dto.OpenedAt = sodTimeRFC(openedAt)
		dto.Reason = reason
		dto.UpdatedAt = sodTimeRFC(updated)
	case errors.Is(err, sql.ErrNoRows):
		// 没有行：工具还没出过事 ⇒ closed。
	default:
		writeErr(w, 500, "读工具熔断状态失败: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"circuit": dto})
}

// sodOptionalInt64 读一个可选的整型查询参数。空串 = 不筛；非法值 = 400。
func sodOptionalInt64(w http.ResponseWriter, raw, name string) (int64, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, true
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || v < 0 {
		writeErr(w, 400, name+" 必须是正整数")
		return 0, false
	}
	return v, true
}

// sodLimitParam 读 limit 查询参数：缺省用 def，越界截到 def。
func sodLimitParam(r *http.Request, def int) int {
	n := atoiDefault(r.URL.Query().Get("limit"), def)
	if n <= 0 {
		return def
	}
	if n > def {
		return def
	}
	return n
}

// sodTimeRFC 把可空时间戳渲染成 RFC3339（空 = 空串）。
func sodTimeRFC(t *time.Time) string {
	if t == nil {
		return ""
	}
	return rfc3339(*t)
}

// sodDecisionReview 人工复核改判（方案 v3 缺陷 F3）：对一条 deny/needs_review
// 裁决发放「call-precision 放行凭据」。
//
// POST /api/sod/decisions/{id}/review
// 体：{"scope":"once|task","reason":"...","operator":"..."}
//
// 语义与边界：
//   - 只允许对 deny / needs_review 裁决改判；approve 裁决本就放行，无需改判；
//   - Tier3 保护面（agent.IsTier3ProtectedTool）拒绝改判 allow——四类高危任何
//     机制都不可豁免（§4 不变式 1），只可转 ask（走拦截规则）；
//   - 凭据四元组 (tool, task, agent, payload_digest) 全等才放行——换一个字节
//     就不命中（call-precision）；
//   - once 用后即焚；task 级随任务终态过期；
//   - 同一 (task, tool) 第 ≥3 次改判时在响应里提示沉淀信任规则（F2）。
func (s *Server) sodDecisionReview(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, ok := pathInt(r, "id")
	if !ok || id <= 0 {
		writeErr(w, 400, "id 不合法")
		return
	}
	var req struct {
		Scope    string `json:"scope"`
		Reason   string `json:"reason"`
		Operator string `json:"operator"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, "参数解析失败")
		return
	}
	req.Scope = strings.TrimSpace(req.Scope)
	if req.Scope == "" {
		req.Scope = "once"
	}
	if req.Scope != "once" && req.Scope != "task" {
		writeErr(w, 400, "scope 只能是 once|task")
		return
	}
	if strings.TrimSpace(req.Operator) == "" {
		writeErr(w, 400, "复核必须留操作者身份（operator）")
		return
	}
	dec, err := pg.GetSoDDecision(id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if dec == nil {
		writeErr(w, 404, "裁决不存在")
		return
	}
	if dec.Verdict == sodpolicy.VerdictApprove {
		writeErr(w, 409, "该裁决本就是放行，无需改判")
		return
	}
	if dec.TaskID == nil || *dec.TaskID <= 0 {
		writeErr(w, 409, "该裁决没有任务上下文，无法发放任务级放行凭据")
		return
	}
	if agent.IsTier3ProtectedTool(dec.ToolName) {
		writeErr(w, 409, "Tier3 高危工具不可改判放行（只可转 ask 拦截规则）——出网/写宿主/裁决写/状态迁移任何机制都不可豁免")
		return
	}
	// v4.1 §10-F1：锁定基线的拒绝不存在人工改判放行——删除/破坏、平台自身
	// 配置是安全地板，override 机制对它们同样无权（与 guard 基线预检同源）。
	if dec.ReasonCode == sodpolicy.ReasonDestructiveAction || dec.ReasonCode == sodpolicy.ReasonPlatformBaseline {
		writeErr(w, 409, "锁定基线拒绝不可改判放行——删除/破坏与平台自身配置不可被任何机制豁免")
		return
	}
	ovr := &db.SodManualOverride{
		DecisionID:    dec.ID,
		PayloadDigest: dec.PayloadDigest,
		ToolKey:       dec.ToolName,
		TaskID:        *dec.TaskID,
		AgentKey:      dec.SourceAgent,
		Scope:         req.Scope,
		Reason:        strings.TrimSpace(req.Reason),
		CreatedBy:     strings.TrimSpace(req.Operator),
	}
	oid, err := pg.CreateSodManualOverride(ovr)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	// 改判后给任务一条提示：planner/worker 下轮可读，worker 会重试同一动作。
	if t, exists := s.m.Task(strconv.FormatInt(*dec.TaskID, 10)); exists && t != nil && t.Store != nil {
		_, _ = t.Store.AddNodeInScope(*dec.TaskID, db.KindHint,
			map[string]any{"text": fmt.Sprintf("已人工批准重试：%s（载荷摘要 %s…）。请原样重试该调用，不要改动入参。", dec.ToolName, trunc(dec.PayloadDigest, 12))},
			0, "active", "human", nil)
	}
	// 同一 (task, tool) 改判次数 ≥3 ⇒ 提示沉淀信任规则（F2 回放预览路径）。
	prior, _ := pg.ListSodManualOverrides(*dec.TaskID, 200)
	sameTool := 0
	for _, o := range prior {
		if o.ToolKey == dec.ToolName {
			sameTool++
		}
	}
	writeJSON(w, 200, map[string]any{
		"ok": true, "override_id": oid, "scope": ovr.Scope,
		"override_count_same_tool": sameTool,
		"suggest_trust_rule":       sameTool >= 3,
	})
}

// sodOverrideList 人工改判历史（职责分离页的复核历史过滤）。
// GET /api/sod/overrides?task_id=
func (s *Server) sodOverrideList(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	taskID, ok := sodOptionalInt64(w, r.URL.Query().Get("task_id"), "task_id")
	if !ok {
		return
	}
	items, err := pg.ListSodManualOverrides(taskID, 100)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"overrides": items, "count": len(items)})
}

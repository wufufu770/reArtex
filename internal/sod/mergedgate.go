package sod

// ---------- 合并闸管线（方案 v4.1 §1）----------
//
// enforce 态的裁决管线：分类 → 种类表 → auditor → 闸门硬检查 → 凭证。
// 影子态复用 mergedVerdict（纯裁决、零副作用）做新旧链对照，最终动作仍由
// 旧链（normalizer+gate）给出——这是 §11-G1 的灰度方案。
//
// 与旧链的关系：本管线**不替代**闸门的确定性硬检查（额度/熔断/tier2/anomaly
// 照跑），它替换的只是旧链里「LLM 规整器 + scope 单一判定」的那一段。

import (
	"context"
	"time"

	"github.com/Autumn-27/artex/internal/sodpolicy"
)

// mergedDecision 是 mergedVerdict 的产出：下一步做什么。
type mergedDecision struct {
	kind    string // "gate"（跑闸门，gateIn 已备好）| "deny" | "review"（对抗复核）
	gateIn  GateInput
	reason  string // deny 的 reason_code
	note    string
	review  *DecisionRecord
	markers map[string]string // 追加进审计 Checks 的合并闸标记
}

// enforceGate 是 enforce 态的入口：纯裁决 → 副作用（闸门/凭证/审计）。
func (c *Chain) enforceGate(ctx context.Context, call chainCall, name string) (bool, string, []byte) {
	d := c.mergedVerdict(ctx, call, name)
	switch d.kind {
	case "gate":
		if c.deps.Gate == nil {
			return c.deny(ctx, call, sodpolicy.ReasonScopeUnavailable, "闸门未装配，按失败关闭处理")
		}
		out := c.deps.Gate.Evaluate(ctx, d.gateIn)
		// 出段出网的特殊通路：evaluate 判 out_of_scope ⇒ 不再静默拒，转
		// auditor 代批 / 人工 ask（v4.1 决策点 1，cap 挂 egress_new_host 行）。
		if out.Verdict == sodpolicy.VerdictDeny && out.ReasonCode == sodpolicy.ReasonOutOfScope {
			return c.outOfScopeRoute(ctx, call, d)
		}
		out.Checks = mergeChecks(out.Checks, d.markers)
		return c.finalize(ctx, call, out, d.gateIn.Normalized, false, false, d.markers, nil)
	case "review":
		c.record(ctx, d.review)
		c.notifyReview(ctx, d.review, []byte("{}"))
		return true, blockMessage(d.review), nil
	default: // deny
		return c.denyWithChecks(ctx, call, d.reason, d.note, d.markers)
	}
}

// denyWithChecks 是 deny 的合并闸变体：审计行带上管线标记（category/auditor/
// fallback 原因）——§4 不变式 4 的边缘： deny 也必须能回放「为什么」。
func (c *Chain) denyWithChecks(ctx context.Context, call chainCall, reasonCode, note string, markers map[string]string) (bool, string, []byte) {
	rec := &DecisionRecord{
		TaskID:         call.taskID,
		ConversationID: call.conversationID,
		SourceAgent:    call.sourceAgent,
		ToolName:       call.toolName,
		Verdict:        sodpolicy.VerdictDeny,
		ReasonCode:     reasonCode,
		Note:           note,
		PayloadDigest:  call.payloadDigest,
		PayloadPreview: RedactPreview(call.payload, sodpolicy.PayloadPreviewMaxBytes),
		ScopeDigest:    call.scopeDigest,
		Checks:         mergeChecks(map[string]string{}, markers),
		LatencyMs:      int(time.Since(call.started) / time.Millisecond),
	}
	c.record(ctx, rec)
	return true, blockMessage(rec), nil
}

// mergedVerdict 是纯裁决（无副作用）：分类 → 种类表 → auditor。
// 影子态也走它（拿到「假如 enforce 会怎么判」），但不铸凭证不落 sod 账本。
func (c *Chain) mergedVerdict(ctx context.Context, call chainCall, name string) mergedDecision {
	mk := func(kind string) mergedDecision { return mergedDecision{kind: kind, markers: map[string]string{}} }
	intelSource := c.deps.IntelSource
	cls := Classify(call.taskID, name, call.payload, intelSource)

	var cats []sodpolicy.SodCategory
	if c.deps.Categories != nil {
		cats, _ = c.deps.Categories()
	}
	cat := MatchCategory(cats, name, cls, call.payload)

	used := func(catName string) int {
		if c.deps.CategoryUsed == nil || catName == "" {
			return 0
		}
		n, _ := c.deps.CategoryUsed(ctx, call.taskID, catName)
		return n
	}

	// --- 种类表命中 ---
	if cat != nil {
		switch cat.Action {
		case sodpolicy.CategoryActionPass:
			d := mk("gate")
			d.gateIn = c.gateInput(call, NormalizerResult{}, "category:"+cat.Name)
			d.markers["category"] = cat.Name
			return d
		case sodpolicy.CategoryActionHardDeny:
			d := mk("deny")
			d.reason = sodpolicy.ReasonCategoryDeny
			d.note = "拦截种类「" + cat.Name + "」配置为硬拒"
			if cat.Reason != "" {
				d.note += "：" + cat.Reason
			}
			d.markers["category"] = cat.Name
			return d
		case sodpolicy.CategoryActionAsk:
			d := mk("gate")
			if !c.categoryCapOK(ctx, call, cat, used, &d) {
				return d
			}
			if c.elevateAsk(ctx, call, "拦截种类「"+cat.Name+"」请求放行本次调用") {
				d.gateIn = c.gateInput(call, NormalizerResult{}, "elevated_ask")
				d.markers["category"] = cat.Name
				d.markers["elevated_ask"] = "approved"
				return d
			}
			dd := mk("deny")
			dd.reason = sodpolicy.ReasonAskDeclined
			dd.note = "拦截种类「" + cat.Name + "」的放行请求未被批准（拒答或超时）"
			dd.markers["category"] = cat.Name
			return dd
		case sodpolicy.CategoryActionAuditor:
			return c.auditorRoute(ctx, call, cat, cls, used, mk)
		}
	}

	// --- 未命中种类 ---
	if cls.Kind == ClassTargetKnown && len(cls.Hosts) > 0 {
		// 有明确目标：交给闸门做真实范围判定（在段内 → 照常放行；出段 →
		// evaluate 返回 out_of_scope，由 enforceGate 转 outOfScopeRoute）。
		d := mk("gate")
		nr := NormalizerResult{}
		nr.StructuredIntent.TargetHint = cls.Hosts[0]
		d.gateIn = c.gateInput(call, nr, "")
		d.markers["classified_target"] = cls.Hosts[0]
		return d
	}
	// 其余（unclear / 种类行被停用的敏感面等）→ auditor 默认裁量岗。
	return c.auditorRoute(ctx, call, nil, cls, used, mk)
}

// auditorRoute 是 action=auditor 的种类与未命中调用的共同通路：
// cap 检查 → auditor 裁决 → pass/deny/ask；故障 → 种类兜底。
func (c *Chain) auditorRoute(ctx context.Context, call chainCall, cat *sodpolicy.SodCategory, cls ClassResult, used func(string) int, mk func(string) mergedDecision) mergedDecision {
	catName := ""
	if cat != nil {
		catName = cat.Name
		// cap 超限 → 直接升级人工（auditor 也不再问——上限的意义就是封顶）。
		// categoryCapOK 未通过时 d 已是最终裁决。
		d := mk("gate")
		if !c.categoryCapOK(ctx, call, cat, used, &d) {
			return d
		}
	}

	reason := catName
	if reason == "" {
		reason = "uncategorized"
	}
	if c.deps.Adjudicator == nil {
		return c.auditorFallback(ctx, call, cat, mk, "auditor 未装配")
	}
	verdict, err := c.deps.Adjudicator(ctx, AuditorRequest{
		TaskID:         call.taskID,
		ConversationID: call.conversationID,
		ToolName:       call.toolName,
		SourceAgent:    call.sourceAgent,
		PayloadPreview: RedactPreview(call.payload, sodpolicy.PayloadPreviewMaxBytes),
		PayloadDigest:  call.payloadDigest,
		ScopeDigest:    call.scopeDigest,
		Reason:         reason,
	})
	if err != nil {
		return c.auditorFallback(ctx, call, cat, mk, err.Error())
	}

	d := mk("gate")
	d.markers["category"] = catName
	d.markers["auditor"] = verdict.Verdict
	d.markers["auditor_reason"] = clipStr(verdict.Reason, 120)
	switch verdict.Verdict {
	case "pass":
		if verdict.AnomalyFlag {
			// 高危疑似：不执行，走既有对抗复核通路（⑨ 的合并闸版本）。
			dd := mk("review")
			dd.review = &DecisionRecord{
				TaskID: call.taskID, ConversationID: call.conversationID,
				SourceAgent: call.sourceAgent, ToolName: call.toolName,
				Verdict: "needs_review", ReasonCode: sodpolicy.ReasonAnomalyFlagged,
				Note:           "auditor 标记异常：" + verdict.AnomalyReason,
				PayloadDigest:  call.payloadDigest,
				PayloadPreview: RedactPreview(call.payload, sodpolicy.PayloadPreviewMaxBytes),
				AnomalyFlag:    true, AnomalyReason: verdict.AnomalyReason,
				Checks:      mergeChecks(map[string]string{}, d.markers),
				ScopeDigest: call.scopeDigest,
				LatencyMs:   verdict.LatencyMs,
			}
			return dd
		}
		d.gateIn = c.gateInput(call, NormalizerResult{}, "auditor")
		return d
	case "deny":
		dd := mk("deny")
		dd.reason = sodpolicy.ReasonAuditorDeny
		dd.note = "auditor 否决本次调用：" + verdict.Reason
		return dd
	case "ask":
		if c.elevateAsk(ctx, call, "auditor 建议人工确认："+verdict.Reason) {
			d.gateIn = c.gateInput(call, NormalizerResult{}, "elevated_ask")
			d.markers["elevated_ask"] = "approved_auditor_suggest"
			return d
		}
		dd := mk("deny")
		dd.reason = sodpolicy.ReasonAskDeclined
		dd.note = "auditor 建议人工确认，未被批准（拒答或超时）：" + verdict.Reason
		return dd
	}
	dd := mk("deny")
	dd.reason = sodpolicy.ReasonAuditorDeny
	dd.note = "auditor 裁决不可识别（verdict=" + verdict.Verdict + "）"
	return dd
}

// auditorFallback 是 auditor 不可用/超时的兜底：按种类 fallback_action
// （默认 ask；绝不静默放行、也绝不变墙——v4.1 §4）。
func (c *Chain) auditorFallback(ctx context.Context, call chainCall, cat *sodpolicy.SodCategory, mk func(string) mergedDecision, cause string) mergedDecision {
	fallback := sodpolicy.CategoryFallbackAsk
	catName := ""
	if cat != nil {
		fallback = cat.Fallback
		catName = cat.Name
	}
	if fallback == "" {
		fallback = sodpolicy.CategoryFallbackAsk
	}
	d := mk("gate")
	d.markers["auditor_fallback"] = fallback
	d.markers["auditor_fallback_cause"] = clipStr(cause, 120)
	switch fallback {
	case sodpolicy.CategoryFallbackPass:
		d.gateIn = c.gateInput(call, NormalizerResult{}, "fallback_pass")
		return d
	case sodpolicy.CategoryFallbackDeny:
		dd := mk("deny")
		dd.reason = sodpolicy.ReasonAuditorUnavailable
		dd.note = "auditor 不可用且种类「" + catName + "」兜底为拒绝：" + cause
		return dd
	default: // ask
		if c.elevateAsk(ctx, call, "auditor 暂不可用（"+cause+"），请求人工裁决本次调用") {
			d.gateIn = c.gateInput(call, NormalizerResult{}, "elevated_ask")
			d.markers["elevated_ask"] = "approved_fallback"
			return d
		}
		dd := mk("deny")
		dd.reason = sodpolicy.ReasonAuditorUnavailable
		dd.note = "auditor 不可用且人工未响应：" + cause
		return dd
	}
}

// outOfScopeRoute 是 enforce 态下 out_of_scope 的转换通路（v4.1 决策点 1：
// 非目标段出网 deny → auditor 代批 / 人工 ask；都不成 → 保持 out_of_scope
// 拒绝，reason 语义留给 B6 升级提示）。
func (c *Chain) outOfScopeRoute(ctx context.Context, call chainCall, d mergedDecision) (bool, string, []byte) {
	// cap 挂 egress_new_host 种类行（表被停用/删除时视为不限——auditor 裁决仍在）。
	capLimit := 0
	if c.deps.Categories != nil {
		if cats, _ := c.deps.Categories(); cats != nil {
			for _, cat := range cats {
				if cat.Enabled && cat.Kind == "builtin" && cat.Pattern == sodpolicy.RecognizeEgressNewHost && cat.CapPerTask > 0 {
					capLimit = cat.CapPerTask
				}
			}
		}
	}
	if capLimit > 0 && c.deps.CategoryUsed != nil {
		if n, _ := c.deps.CategoryUsed(ctx, call.taskID, sodpolicy.RecognizeEgressNewHost); n >= capLimit {
			return c.deny(ctx, call, sodpolicy.ReasonCategoryCapExceeded,
				"非目标段出网的本任务配额已用尽且人工未批准")
		}
	}
	reason := "out_of_scope"
	if d.gateIn.Normalized.StructuredIntent.TargetHint != "" {
		reason += "（目标 " + d.gateIn.Normalized.StructuredIntent.TargetHint + "）"
	}
	ask := func() bool {
		return c.elevateAsk(ctx, call, "目标不在任务范围，请求放行本次出网："+reason)
	}
	if c.deps.Adjudicator != nil {
		verdict, err := c.deps.Adjudicator(ctx, AuditorRequest{
			TaskID: call.taskID, ConversationID: call.conversationID,
			ToolName: call.toolName, SourceAgent: call.sourceAgent,
			PayloadPreview: RedactPreview(call.payload, sodpolicy.PayloadPreviewMaxBytes),
			PayloadDigest:  call.payloadDigest,
			ScopeDigest:    call.scopeDigest,
			Reason:         "out_of_scope",
		})
		if err == nil && verdict.Verdict == "pass" {
			out := c.deps.Gate.Evaluate(ctx, withScopePass(d.gateIn, "auditor"))
			out.Checks = mergeChecks(out.Checks, map[string]string{"auditor": "pass", "out_of_scope_escalated": "true"})
			return c.finalize(ctx, call, out, NormalizerResult{}, false, false, map[string]string{"out_of_scope_escalated": "true"}, nil)
		}
		if err == nil && verdict.Verdict == "deny" {
			return c.deny(ctx, call, sodpolicy.ReasonAuditorDeny, "auditor 否决越界出网："+verdict.Reason)
		}
	}
	if ask() {
		out := c.deps.Gate.Evaluate(ctx, withScopePass(d.gateIn, "elevated_ask"))
		out.Checks = mergeChecks(out.Checks, map[string]string{"elevated_ask": "approved", "out_of_scope_escalated": "true"})
		return c.finalize(ctx, call, out, NormalizerResult{}, false, false, map[string]string{"out_of_scope_escalated": "true"}, nil)
	}
	return c.deny(ctx, call, sodpolicy.ReasonOutOfScope, d.note)
}

// shadowCompare 是影子态的对照：新链纯裁决 vs 旧链实际裁决，只记差异。
// best-effort：对照失败不影响旧链已经给出的动作。异步跑，不拖慢 worker。
func (c *Chain) shadowCompare(call chainCall, oldOut GateOutput) {
	if c.deps.RecordShadowDiff == nil || c.deps.MergedGateMode == nil {
		return
	}
	if c.deps.MergedGateMode() != sodpolicy.MergedGateShadow {
		return
	}
	// 复制载荷：hook 调用方可能复用底层缓冲。
	payloadCopy := make([]byte, len(call.payload))
	copy(payloadCopy, call.payload)
	callCopy := call
	callCopy.payload = payloadCopy

	go func() {
		defer func() { _ = recover() }() // 对照是旁路：任何 panic 都吞掉
		ctx := context.Background()
		d := c.mergedVerdict(ctx, callCopy, callCopy.toolName)
		newVerdict, newReason := d.verdictSummary()
		if newVerdict == "" {
			return
		}
		c.deps.RecordShadowDiff(ShadowDiff{
			TaskID: callCopy.taskID, ToolName: callCopy.toolName,
			PayloadDigest: callCopy.payloadDigest,
			OldVerdict:    oldOut.Verdict, OldReason: oldOut.ReasonCode,
			NewVerdict: newVerdict, NewReason: newReason,
		})
	}()
}

// verdictSummary 把 mergedDecision 压成 (verdict, reason) 供影子对照。
func (d *mergedDecision) verdictSummary() (string, string) {
	switch d.kind {
	case "gate":
		if d.gateIn.ScopePassReason != "" {
			return "approve", "scope_pass(" + d.gateIn.ScopePassReason + ")"
		}
		// 交给闸门的真实判定（TargetKnown 在段内/熔断/额度等）——影子态不再
		// 真跑闸门（要查 DB），以「gate_pending_scope」标注，差异统计以
		// deny/approve 类为主。
		return "gate", "scope_check_pending"
	case "review":
		return "needs_review", d.review.ReasonCode
	default:
		return "deny", d.reason
	}
}

// ---------- 小工具 ----------

// gateInput 组装 GateInput（合并闸路径；scopePassReason 非空 ⇒ scope 构造通过）。
func (c *Chain) gateInput(call chainCall, normalized NormalizerResult, scopePassReason string) GateInput {
	in := GateInput{
		ToolName: call.toolName, SourceAgent: call.sourceAgent,
		TaskID: call.taskID, ConversationID: call.conversationID,
		PayloadDigest: call.payloadDigest, ScopeDigest: call.scopeDigest,
		Normalized: normalized, FindingID: findingIDFromInput(call.payload),
		ScopePassReason: scopePassReason,
	}
	return in
}

// withScopePass 复制 GateInput 并改写 ScopePassReason（outOfScopeRoute 用）。
func withScopePass(in GateInput, reason string) GateInput {
	in.ScopePassReason = reason
	return in
}

// elevateAsk 走人工 ask 通路；未装配/出错 = 未批准（fail-safe 拒绝）。
func (c *Chain) elevateAsk(ctx context.Context, call chainCall, reason string) bool {
	if c.deps.ElevateAsk == nil {
		return false
	}
	approved, _, err := c.deps.ElevateAsk(ctx, AskRequest{
		TaskID: call.taskID, ConversationID: call.conversationID,
		ToolName: call.toolName, Reason: reason,
		PayloadPreview: RedactPreview(call.payload, sodpolicy.PayloadPreviewMaxBytes),
	})
	return err == nil && approved
}

// categoryCapOK 检查种类配额；配额内返回 true。超限时在 *d 上做「auditor
// 严格裁决」并返回 false（v6.1：cap 的意义是防 auditor 橡皮图章洪泛，不是把
// 人变成审批员——auditor pass 就放、deny 就拒、ask 才升级人工；auditor 不可
// 用再按种类兜底）。返回 false 时 *d 已是最终裁决。
func (c *Chain) categoryCapOK(ctx context.Context, call chainCall, cat *sodpolicy.SodCategory, used func(string) int, d *mergedDecision) bool {
	if cat.CapPerTask <= 0 {
		return true
	}
	if used(cat.Name) < cat.CapPerTask {
		return true
	}
	d.markers["category"] = cat.Name
	d.markers["cap_exceeded"] = "true"
	// 事件直触（v9 批次1⑥）：cap 命中即刻通知宿主，监督轮不再等轮询空窗。
	// 同任务多次命中由宿主侧节流合并。
	if c.deps.OnCapHit != nil {
		c.deps.OnCapHit(call.taskID, cat.Name)
	}
	if c.deps.Adjudicator == nil {
		// 无裁决岗：保持旧语义（转人工一次，不批则拒）。
		if c.elevateAsk(ctx, call, "拦截种类「"+cat.Name+"」本任务配额已用尽，请求人工放行本次调用") {
			d.kind = "gate"
			d.gateIn = c.gateInput(call, NormalizerResult{}, "elevated_ask")
			d.markers["elevated_ask"] = "approved_cap_exceeded"
			return false
		}
		d.kind = "deny"
		d.reason = sodpolicy.ReasonCategoryCapExceeded
		d.note = "拦截种类「" + cat.Name + "」本任务配额已用尽且人工未批准"
		return false
	}
	verdict, err := c.deps.Adjudicator(ctx, AuditorRequest{
		TaskID: call.taskID, ConversationID: call.conversationID,
		ToolName: call.toolName, SourceAgent: call.sourceAgent,
		PayloadPreview: RedactPreview(call.payload, sodpolicy.PayloadPreviewMaxBytes),
		PayloadDigest:  call.payloadDigest,
		ScopeDigest:    call.scopeDigest,
		Reason:         "cap_exceeded_strict:" + cat.Name,
	})
	if err != nil {
		// auditor 挂：按种类兜底（默认 ask；ask 无人则拒）。
		if c.elevateAsk(ctx, call, "拦截种类「"+cat.Name+"」配额已用尽且 auditor 暂不可用（"+err.Error()+"），请求人工裁决") {
			d.kind = "gate"
			d.gateIn = c.gateInput(call, NormalizerResult{}, "elevated_ask")
			d.markers["elevated_ask"] = "approved_cap_fallback"
			return false
		}
		d.kind = "deny"
		d.reason = sodpolicy.ReasonAuditorUnavailable
		d.note = "拦截种类「" + cat.Name + "」配额已用尽，auditor 不可用且人工未响应：" + err.Error()
		return false
	}
	switch verdict.Verdict {
	case "pass":
		d.kind = "gate"
		d.gateIn = c.gateInput(call, NormalizerResult{}, "auditor")
		d.markers["auditor"] = "pass"
		d.markers["auditor_reason"] = clipStr(verdict.Reason, 120)
		return false
	case "deny":
		d.kind = "deny"
		d.reason = sodpolicy.ReasonAuditorDeny
		d.note = "拦截种类「" + cat.Name + "」配额已用尽且 auditor 否决：" + verdict.Reason
		return false
	default: // ask——auditor 也拿不准，转人工
		if c.elevateAsk(ctx, call, "拦截种类「"+cat.Name+"」配额已用尽且 auditor 建议人工确认："+verdict.Reason) {
			d.kind = "gate"
			d.gateIn = c.gateInput(call, NormalizerResult{}, "elevated_ask")
			d.markers["elevated_ask"] = "approved_cap_strict"
			return false
		}
		d.kind = "deny"
		d.reason = sodpolicy.ReasonAskDeclined
		d.note = "拦截种类「" + cat.Name + "」配额已用尽，auditor 建议人工确认但未被批准：" + verdict.Reason
		return false
	}
}

func mergeChecks(base, extra map[string]string) map[string]string {
	if base == nil {
		base = map[string]string{}
	}
	for k, v := range extra {
		if v != "" || base[k] == "" {
			base[k] = v
		}
	}
	return base
}

func clipStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

package agent

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/Autumn-27/artex/db"
	actool "github.com/Autumn-27/norma/tool"
)

// 产出级复核器（方案书 §11.6 的 P1 项：把「对抗终止触发」从工具层搬到【产出层】）。
//
// 为什么要有这一层：此前 finding **落库即 confirmed** ⇒「提出」被当成了「验证」。
// 于是「结论有没有被独立复核过」这件事在数据上根本看不出来，复核不通过时也没有
// 退路（既不能关掉，也不能退回给提出它的人）。
//
// 现在的分工：
//   - **产出层**：worker 提出结论（落库态 = pending）；
//   - **复核层**：复核者给出裁决（confirmed / dismissed / rework）；
//   - **确定性层**：db 层的 ValidateFinding 裁决「谁能裁决」「能不能翻案」
//     （提出人不得自验、已结案不得重开、dismissed/rework 必须给理由）。
//
// 模型只提议，确定性层裁决 ⇒ 本文件只负责【把裁决送进确定性层】，不做任何裁决。

// 裁决三态（与 db 层的 finding 节点三态一一对应）。
const (
	// FindingVerdictConfirmed = 验证通过（结案）。
	FindingVerdictConfirmed = "confirmed"
	// FindingVerdictDismissed = 判定不成立（结案）。
	FindingVerdictDismissed = "dismissed"
	// FindingVerdictRework = 验证不通过、退回返工（非结案）。
	FindingVerdictRework = "rework"
)

// FindingValidator 是 db 层验证裁决的**注入点**。
//
// 为什么用注入而不是直接调 db 包：db 层的签名还在演进（裁决值是字符串还是类型、
// 参数顺序），直接依赖会把 agent 包与 db 包焊死。这里定义窄接口，编排层在接线
// 时把它绑到 db 实现上（db/finding_validation.go 的 ValidateFinding）。
type FindingValidator func(in db.FindingValidationInput) error

// findingVerdictValues 是合法裁决值（给校验用）。
var findingVerdictValues = map[string]bool{
	FindingVerdictConfirmed: true,
	FindingVerdictDismissed: true,
	FindingVerdictRework:    true,
}

// SetFindingValidator 注入 db 层的裁决实现（nil = 不校验复核）。
func (t *ToolSet) SetFindingValidator(fn FindingValidator) { t.findingValidator = fn }

// validateFindingTool 返回「裁决一条 finding」的复核工具。
//
// 这个工具**不做裁决**：它把裁决送进确定性层（db），由 db 挡下「提出人自验」
// 「已结案翻案」「没写理由」三类非法操作。
func (t *ToolSet) validateFindingTool() actool.CoreTool {
	return t.writeExpTool("validate_finding",
		"【复核】给一条 finding 下验证裁决。\\n"+
			"裁决三态：confirmed（验证通过，结案）/ dismissed（判定不成立，结案）/ rework（验证不通过、退回返工，非结案）。\\n"+
			"finding_node_id 必填（finding 节点的 id）；verdict 必填；dismissed 与 rework 必须写 reason（说明为什么）。\\n"+
			"origin_intent_id：返工时必填（退回到产出它的那条意图）。\\n"+
			"⚠️你不能裁决自己提出的结论：确定性层会挡下「提出人自验」。被挡时换一个人复核，别绕过。",
		map[string]any{
			"type": "object",
			"properties": map[string]any{
				"finding_node_id":  map[string]any{"type": "integer", "description": "finding 节点的 id。"},
				"verdict":          map[string]any{"type": "string", "description": "confirmed / dismissed / rework。"},
				"reason":           map[string]any{"type": "string", "description": "理由；dismissed 与 rework 必填。"},
				"origin_intent_id": map[string]any{"type": "integer", "description": "返工时退回的意图 id（rework 必填）。"},
			},
			"required": []any{"finding_node_id", "verdict"},
		},
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.findingValidator == nil {
				return actool.Text("复核层未接线（确定性裁决器不可用）"), nil
			}
			var a struct {
				FindingNodeID int64  `json:"finding_node_id"`
				Verdict       string `json:"verdict"`
				Reason        string `json:"reason"`
				OriginIntent  int64  `json:"origin_intent_id"`
			}
			if err := json.Unmarshal(in, &a); err != nil {
				return actool.Errorf("参数不合法: " + err.Error()), nil
			}
			verdict := strings.TrimSpace(a.Verdict)
			if !findingVerdictValues[verdict] {
				return actool.Errorf("裁决值非法：只能是 confirmed / dismissed / rework"), nil
			}
			reason := strings.TrimSpace(a.Reason)
			if (verdict == FindingVerdictDismissed || verdict == FindingVerdictRework) && reason == "" {
				return actool.Errorf("dismissed 与 rework 必须写明理由"), nil
			}
			if verdict == FindingVerdictRework && a.OriginIntent <= 0 {
				return actool.Errorf("返工必须指明要退回的意图（origin_intent_id）"), nil
			}
			verifier := strings.TrimSpace(t.memberAgentKey)
			if verifier == "" {
				verifier = strings.TrimSpace(t.worker)
			}
			if verifier == "" {
				return actool.Errorf("复核人身份未知：无法裁决"), nil
			}
			if err := t.findingValidator(db.FindingValidationInput{
				NodeID:       a.FindingNodeID,
				Verdict:      db.FindingValidationVerdict(verdict),
				Verifier:     verifier,
				Reason:       reason,
				ReworkTarget: a.OriginIntent,
			}); err != nil {
				return actool.Text("裁决被确定性层拒绝：" + err.Error()), nil
			}
			return actool.Text("verdict recorded: " + verdict), nil
		})
}

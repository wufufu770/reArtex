package server

// 连续拒绝升级路径（方案 v3 缺陷 B6）：同一 (task, tool, reason_code) 连续被拒
// ≥3 次 ⇒ 在跑的意图转 blocked（带拒绝摘要）+ 绑定会话播报「需人工处理」。
//
// 为什么要有它：被拦后 worker 看到的只是一条错误字符串，它会换措辞重试——
// 同一堵墙撞 N 次毫无产出（远程实测任务 15 卡死 10+ 分钟）。升级到 blocked 后
// 意图停在等人处理的形态（可经 rerun-blocked 重跑），且群里有播报。
//
// out_of_scope 首拒附带 add_company_scope 建议（提示里带上，worker 学得会走
// 扩权请求而不是硬撞）。
//
// 计数是进程内的（重启即清）：升级是「止损」不是「记账」——重启后 worker 再撞
// 墙照样会重新升级，漏不了。

import (
	"fmt"
	"log"
	"regexp"
	"strconv"
	"sync"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/internal/sodpolicy"
)

// denialEscalationThreshold 是触发升级的连续同类拒绝次数。
const denialEscalationThreshold = 3

// denialKey 是计数键：task \x00 tool \x00 reason_code。
type denialKey struct{ task, tool, reason string }

// reasonCodeFromDenialMessage 从拒绝消息里抠 reason_code：
// "[SoD] 拒绝执行：reason_code=out_of_scope；..." → out_of_scope；
// 拦截规则的拒绝没有 reason_code ⇒ "rule_deny"。
var reasonCodeRe = regexp.MustCompile(`reason_code=([a-z0-9_]+)`)

func reasonCodeFromDenialMessage(msg string) string {
	if m := reasonCodeRe.FindStringSubmatch(msg); m != nil {
		return m[1]
	}
	return "rule_deny"
}

// environmentalDenialCodes 是「环境类失败」的原因码（方案 v6 D3，d2d 失败
// 七分类的精简版）：基础设施/额度/裁决通道的抖动——不是对 worker 行为的
// 实质性拒绝，不计入 B6 连续拒绝计数（否则目标站抖两下就把正要出成果的
// 意图升到 blocked）。
var environmentalDenialCodes = map[string]bool{
	"normalizer_unavailable": true, "scope_check_unavailable": true,
	"tool_health_unavailable": true, "quota_check_unavailable": true,
	"auditor_unavailable": true, "execution_token_invalid": true,
}

// denialEscalation 记录每任务的连续拒绝计数（引擎侧持有）。
type denialEscalation struct {
	mu    sync.Mutex
	count map[denialKey]int
}

func newDenialEscalation() *denialEscalation {
	return &denialEscalation{count: map[denialKey]int{}}
}

// recordDenial 记一次拒绝；达到阈值返回 true（调用方执行升级动作）。
func (d *denialEscalation) recordDenial(taskID, toolName, reasonCode string) bool {
	if taskID == "" {
		return false
	}
	k := denialKey{taskID, toolName, reasonCode}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.count[k]++
	return d.count[k] >= denialEscalationThreshold
}

// resetTool 清掉该 (task, tool) 的计数（同工具的其它 reason_code 保留——
// 换了原因码本来就是新的一类）。
func (d *denialEscalation) resetTool(taskID, toolName, reasonCode string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.count, denialKey{taskID, toolName, reasonCode})
}

// onToolDenial 是 B6 的观察点：工具调用被拒（拦截规则 / SoD 链）时由引擎回灌
// 处调用。达到阈值 ⇒ 在跑意图转 blocked + 会话播报。
func (s *Server) onToolDenial(taskID, toolName, denialMessage string) {
	reasonCode := reasonCodeFromDenialMessage(denialMessage)
	if environmentalDenialCodes[reasonCode] {
		return // D3 环境类失败：不计数（实质拒绝才升 blocked）
	}
	// F4 空缺源③（方案 v10 §16）：授权类硬停（锁定基线/删除破坏）的拒绝逐次入账
	// kind=authorization。authorization 类只入账不唤醒（授权空缺必须停——只能由人
	// 补范围/给额度，自动唤醒产意图等于让 agent 自己给自己扩权，见 server/gapwake.go
	// 的硬停止注释）。同 (task, detail) 的 open 行由 gap 层去重，重复撞墙不刷账。
	if reasonCode == sodpolicy.ReasonPlatformBaseline || reasonCode == sodpolicy.ReasonDestructiveAction {
		if numeric, perr := strconv.ParseInt(taskID, 10, 64); perr == nil && numeric > 0 {
			s.openGap(numeric, db.GapAuthorization, trunc(denialMessage, 300))
		}
	}
	if !s.denials.recordDenial(taskID, toolName, reasonCode) {
		return
	}
	s.denials.resetTool(taskID, toolName, reasonCode)
	numeric, _ := strconv.ParseInt(taskID, 10, 64)
	if numeric <= 0 || s.m == nil {
		return
	}
	t, ok := s.m.Task(taskID)
	if !ok || t == nil || t.Store == nil {
		return
	}
	summary := fmt.Sprintf("%s 被 %s 连续拒绝 %d 次（reason_code=%s）",
		toolName, "平台", denialEscalationThreshold, reasonCode)
	hint := denialHintFor(reasonCode)
	blockedReason := trunc(denialMessage, 300)
	if ids, err := t.Store.RunningIntentIDs(); err == nil {
		for _, id := range ids {
			if _, err := t.Store.SetIntentBlockedWithReason(id, blockedReason); err == nil {
				summary += fmt.Sprintf("；意图 #%d 已转 blocked", id)
			}
		}
	}
	s.engine.emitActivity(t, db.Activity{Worker: "system", Kind: "text", IsError: true,
		Summary: summary + "，需人工处理" + suffixHint(hint)})
	// F4 空缺源①（方案 v10 §16）：B6 升级（意图转 blocked）= 一条动作空缺——
	// worker 反复撞同一堵墙说明「想做的动作做不成」，入账 kind=action 并触发一次
	// 定向唤醒（让监督者产出绕行意图或收口判断）。
	s.openGap(numeric, db.GapAction, trunc(denialMessage, 300))
	// 绑定会话播报（总控/群聊里看得见）。
	if sess, err := s.m.pg.SessionByTask(numeric); err == nil && sess != nil {
		body := fmt.Sprintf("【需人工处理】任务 #%s：%s%s", taskID, summary, suffixHint(hint))
		s.say(sess.ID, numeric, "system", "system", body)
	}
	log.Printf("[escalation] task %s %s", taskID, summary)
}

func suffixHint(hint string) string {
	if hint == "" {
		return ""
	}
	return "。" + hint
}

// denialHintFor 给各 reason_code 生成改道提示（B6 升级播报与 deny 消息共用）；
// v4.1 新增四个合并闸原因码的口径。
func denialHintFor(reasonCode string) string {
	switch reasonCode {
	case "out_of_scope":
		return "目标不在任务范围内——请用 add_task_scope/add_company_scope 走扩权请求，或调整任务范围后重跑；若本次是文件操作：相对路径会按解析后的绝对路径判定出界，请先 cd 到目标目录再操作，或直接使用绝对路径（方案 v9.2 §3.5-F6 hint 案）"
	case "destructive_action":
		return "删除/破坏类动作被锁定基线硬拦且不可放行——请改用非破坏方式达成目的（例如查询代替删除）"
	case "auditor_deny":
		return "auditor 否决了本次调用（见拒绝理由）——请按理由调整方式后重试，或把诉求写进任务提示让人工裁决"
	case "category_deny":
		return "拦截种类配置为硬拒——如属误配请联系操作者在拦截页调整该种类"
	case "ask_declined":
		return "放行请求未被批准——请改用获批范围内的替代动作"
	case "auditor_unavailable":
		return "审计裁决暂时不可用——稍后重试或等待人工审批"
	case "category_cap_exceeded":
		return "该类动作的本任务配额已用尽——收拢同类动作或请求人工扩额"
	}
	return ""
}

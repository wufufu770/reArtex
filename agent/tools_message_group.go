package agent

import (
	"context"
	"encoding/json"
	"log"
	"strings"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/internal/ingestsanitize"
	actool "github.com/Autumn-27/norma/tool"
)

// message_group（设计 §1 Tier 2 ④）：把一条消息写进本任务绑定的任务大厅群信箱。
//
// 为什么放在 worker 面：大厅任务的执行进展原来只活在任务详情页；群里的成员（和
// 人）看不到「正在发生什么」。worker 用这个工具把关键进展/请求决策投进大厅，
// 成员下一轮收信即读到（UnreadChatGroupMessages），人打开大厅消息箱即看见。
//
// 护栏三道（与既有纪律对齐）：
//  1. SoD Tier2 —— approval_policy.go 的 tier2 表已按本 key 映射（tier2Mailbox），
//     internal/sod/chain.go 的免规整白名单也早已列有本 key：工具落地后这两处
//     不再是「按一个不存在的工具决策」。
//  2. 准入在存储层 —— AddChatGroupMessage 内建 chatGroupAdmissionReason（任务
//     一致性/意图归属/收件人必须是群成员），被拦消息落库标 blocked 不进上下文。
//  3. T7 出口消毒 —— 群消息经收信进入【其他成员】的上下文，是跨上下文通道；
//     worker 读过的外部数据可能夹带注入载荷，故在出口过 ingestsanitize。
func (t *ToolSet) messageGroupTool() actool.CoreTool {
	return writeTool("message_group",
		"把一条消息写进本任务绑定的任务大厅群信箱（默认广播全体成员，可定向 to_agent）。\n"+
			"用途：向大厅里的其他成员和人汇报关键进展、请求决策、回答人类的定向提问。人人可见，不要刷屏。\n"+
			"返回 {id, blocked}：blocked=true 表示被准入规则拦截（附 blocked_reason），消息不会进入任何成员的上下文。",
		obj(map[string]any{
			"body":     str("消息正文（必填，建议 ≤2000 字，一件事一条）"),
			"to_agent": str("定向收件成员的 agent key（可选；留空=广播全群。收件人必须是本群成员，否则被拦）"),
			"kind":     str("消息类型：note（默认，进展/汇报）或 answer（回答人类的定向提问）"),
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				Body    string `json:"body"`
				ToAgent string `json:"to_agent"`
				Kind    string `json:"kind"`
			}
			_ = json.Unmarshal(in, &a)
			body := strings.TrimSpace(a.Body)
			if body == "" {
				return actool.Errorf("body 不能为空"), nil
			}
			kind := strings.TrimSpace(a.Kind)
			if kind == "" {
				kind = "note"
			}
			if kind != "note" && kind != "answer" {
				return actool.Errorf("kind 只能是 note 或 answer（request/escalation/finding 各有专用通道，不从这里走）"), nil
			}
			if t.memoryDB == nil || t.taskID <= 0 {
				return actool.Errorf("message_group 需要任务大厅上下文：当前执行体没有绑定任务或库句柄未接线，该工具不可用"), nil
			}
			g, err := t.memoryDB.GroupByTask(t.taskID)
			if err != nil {
				return actool.Errorf("查询任务大厅失败: "+err.Error()), nil
			}
			if g == nil {
				return actool.Errorf("本任务没有绑定任务大厅群，无法投递群消息"), nil
			}
			// T7：出口消毒（fail-closed：HIGH 丢弃；SOFT 消毒放行并留痕日志）。
			clean, action, serr := ingestsanitize.Sanitize("message_group", []byte(body))
			if action == ingestsanitize.ActionDropped {
				return actool.Errorf("消息命中注入特征被拦截（"+serr.Error()+"）"), nil
			}
			if action == ingestsanitize.ActionWarned {
				log.Printf("[message_group] %s 消息命中注入软特征，消毒后放行", ingestsanitize.Marker("message_group"))
			}
			body = string(clean)
			if len([]rune(body)) > 2000 {
				body = truncateRunesAgent(body, 2000)
			}
			tid := t.taskID
			m := &db.ChatGroupMessage{
				GroupID:   g.ID,
				FromAgent: t.worker, // 执行体身份（work#N / planner）；成员名册校验只对 ToAgent 做
				ToAgent:   strings.TrimSpace(a.ToAgent),
				Kind:      kind,
				Body:      body,
				TaskID:    &tid,
			}
			id, err := t.memoryDB.AddChatGroupMessage(m)
			if err != nil {
				return actool.Errorf("投递失败: "+err.Error()), nil
			}
			if m.Status == db.ChatGroupMessageBlocked {
				return jsonResult(map[string]any{"id": id, "blocked": true, "blocked_reason": m.BlockedReason})
			}
			return jsonResult(map[string]any{"id": id, "blocked": false})
		})
}

// MessageGroupTool 供工具目录（tools 表种子）取本工具的描述与 schema。
func (t *ToolSet) MessageGroupTool() actool.CoreTool { return t.messageGroupTool() }

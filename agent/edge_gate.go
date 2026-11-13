package agent

// chat lane 的偏锋触发词闸门辅助（EDGE E6，合并施工）。
//
// 这里只有两件机械事：工具前置与审计落账。词表、两级判定、注入文案都在
// internal/edge（代码常量可单测）；chat.go 的 Chat() 只调 edge.Scan。

import (
	"encoding/json"
	"log"

	actool "github.com/Autumn-27/norma/tool"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/internal/edge"
)

// promoteEdgeTools 把 edge_* 工具移动到工具集头部（硬命中时调用——模型先看见
// 先想起）。稳定排序：其余工具相对顺序不变，不影响既有提示缓存语义（工具清单
// 本就是每 run 重建的）。
func promoteEdgeTools(tools []actool.CoreTool) []actool.CoreTool {
	edgeFirst := make([]actool.CoreTool, 0, 4)
	rest := make([]actool.CoreTool, 0, len(tools))
	for _, t := range tools {
		if t == nil {
			continue
		}
		name := t.Name()
		if len(name) > 5 && name[:5] == "edge_" {
			edgeFirst = append(edgeFirst, t)
		} else {
			rest = append(rest, t)
		}
	}
	return append(edgeFirst, rest...)
}

// auditTriggerGate 把闸门命中落审计链（四要素：词/级/agent/时间——随后调了
// 哪个工具由 agent 自选，活动流里有完整工具调用记录可对账）。
// 审计失败只记日志：闸门是提示层机制，不因审计抖动阻塞对话。
func (c *ChatAgent) auditTriggerGate(agentKey string, hit *edge.TriggerHit) {
	if c.memoryDB == nil {
		return
	}
	level := "soft"
	if hit.Hard {
		level = "hard"
	}
	payload, _ := json.Marshal(map[string]string{"word": hit.Word, "level": level})
	if _, err := c.memoryDB.AppendAuditEvent(db.AuditEvent{
		Actor:    agentKey,
		ToolName: "edge-trigger-gate",
		Action:   "edge.trigger_scan",
		Reason:   "用户消息命中偏锋触发词（" + level + "）",
		Payload:  payload,
		Source:   "edge-trigger-gate",
	}); err != nil {
		log.Printf("[edge] 触发闸门审计写入失败（忽略）: %v", err)
	}
}

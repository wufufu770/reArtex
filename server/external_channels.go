package server

// 外部通道广播（0.15.19 飞书 + 0.15.20 企业微信/邮箱）：审批要素出向钩子。
//
// 架构：外部侧是【独立侧车】——飞书桥（feishu-bridge/，纯 WS 长连接客户端 +
// 出向 REST，回环监听唯一广播端点）与邮箱摄取（email-intake/，IMAP 轮询侧车）。
// reArtex 只在 ask pending 行落库后把审批要素异步扇出到已配置的目标：
//   ① 飞书桥：POST feishu_bridge_url（JSON 要素；桥渲染审批卡，文本命令裁决）
//   ② 企微群：POST wecom_webhook_url（群机器人 markdown 卡；裁决在审批页/飞书）
// 两目标相互独立、各自开关（settings 空=关闭）；裁决回调统一走既有
//   POST /api/intercept/pending/{id}/decide  {decision: allowed|denied}
// ——审批决策面零新增权限面（与审批页/群聊卡同一入口，SoD 口径不变）。
//
// 配置（settings KV，SQL/运维通道；空=该目标关闭）：
//   feishu_bridge_url    桥的接收端点
//   feishu_bridge_secret 共享口令（X-Bridge-Secret 头；桥侧校验）
//   wecom_webhook_url    企微群机器人 webhook（notify 系统的 wecom 渠道亦复用此形态）
// 凭据（飞书 app_id/secret、IMAP 口令、allowlist）只在侧车环境变量里——不进
// reArtex 进程、不进库、不进日志。

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	settingFeishuBridgeURL    = "feishu_bridge_url"
	settingFeishuBridgeSecret = "feishu_bridge_secret"
	settingWeComWebhookURL    = "wecom_webhook_url"
	channelBroadcastTimeout   = 5 * time.Second
	// weComMarkdownLimit：企微群机器人 markdown content 硬上限 4096 字节
	// （超长整条被拒——与 notify/wecom.go 同一口径），广播卡按 3600 收紧留余量。
	weComCardLimit = 3600
	// inputPreviewLimit 是广播载荷里工具入参的截断点（审批卡要看得见上下文，
	// 但不整段灌给外部通道）。
	inputPreviewLimit = 1024
)

// askBroadcast 是一次审批广播的要素（多目标共享同一份）。
type askBroadcast struct {
	PendingID int64           `json:"pending_id"`
	ConvID    int64           `json:"conv_id"`
	TaskID    string          `json:"task_id"`
	Agent     string          `json:"agent"`
	Tool      string          `json:"tool"`
	Reason    string          `json:"reason"`
	Input     json.RawMessage `json:"input"`
}

// externalChannelListener 是 SetAskListener 的实现：组装要素后扇出到所有已配置
// 目标。广播是增值面——任何失败只记日志，不影响审批主链。
func (s *Server) externalChannelListener(pendingID int64, convID int64, taskID, agentName, toolName string, input []byte, reason string) {
	if s.m == nil || s.m.pg == nil {
		return
	}
	preview := input
	if len(preview) > inputPreviewLimit {
		preview = preview[:inputPreviewLimit]
	}
	b := askBroadcast{
		PendingID: pendingID, ConvID: convID, TaskID: taskID,
		Agent: agentName, Tool: toolName, Reason: reason, Input: json.RawMessage(preview),
	}
	// ① 飞书桥（JSON 要素；空 URL=关闭）。
	if u := strings.TrimSpace(s.settingValue(settingFeishuBridgeURL)); u != "" {
		secret := s.settingValue(settingFeishuBridgeSecret)
		body, err := json.Marshal(b)
		if err == nil {
			s.postExternal(u, secret, body, "feishu", pendingID)
		} else {
			log.Printf("[channels] 飞书广播组包失败 pending=%d: %v", pendingID, err)
		}
	}
	// ② 企微群机器人（markdown 卡；空 URL=关闭）。
	if hook := strings.TrimSpace(s.settingValue(settingWeComWebhookURL)); hook != "" {
		body, err := json.Marshal(map[string]any{
			"msgtype":  "markdown",
			"markdown": map[string]any{"content": weComApprovalCard(b)},
		})
		if err == nil {
			s.postExternal(hook, "", body, "wecom", pendingID)
		} else {
			log.Printf("[channels] 企微广播组包失败 pending=%d: %v", pendingID, err)
		}
	}
}

// weComApprovalCard 渲染企微 markdown 审批卡（4096 字节内收紧）。
func weComApprovalCard(b askBroadcast) string {
	preview := strings.TrimSpace(string(b.Input))
	if len(preview) > 600 {
		preview = preview[:600] + " …"
	}
	card := "**[reArtex 审批待决 #" + itoa64c(b.PendingID) + "]**\n" +
		"> 任务：`" + b.TaskID + "`　执行体：`" + b.Agent + "`\n" +
		"> 工具：`" + b.Tool + "`\n" +
		"> 原因：" + b.Reason + "\n" +
		"> 入参预览：\n> ```" + preview + "```\n" +
		"> 裁决请到平台「审批」页（或已配置的飞书命令通道）。"
	if len(card) > weComCardLimit {
		card = card[:weComCardLimit]
	}
	return card
}

// itoa64c 是广播卡用的 int64→string。
func itoa64c(n int64) string {
	return strconv.FormatInt(n, 10)
}

// settingValue 现取 settings（每条广播各取一次：键值少、热改即刻生效）。
func (s *Server) settingValue(key string) string {
	if s.m == nil || s.m.pg == nil {
		return ""
	}
	v, ok, _ := s.m.pg.GetSetting(key)
	if !ok {
		return ""
	}
	return v
}

// postExternal 统一出向：http(s) scheme 强制（防误配成非 http 面）、共享口令可选、
// 有界超时、失败只记日志（增值面不为外部通道买单）。
func (s *Server) postExternal(url, secret string, body []byte, label string, pendingID int64) {
	url = strings.TrimSpace(url)
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		log.Printf("[channels] %s 目标 URL 非法（必须是 http/https，当前 %q），跳过", label, url)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), channelBroadcastTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		log.Printf("[channels] %s 广播请求构造失败 pending=%d: %v", label, pendingID, err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if secret != "" {
		req.Header.Set("X-Bridge-Secret", secret)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Printf("[channels] %s 广播失败 pending=%d（侧车未启动/网络不可达，不影响审批主链）: %v", label, pendingID, err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		log.Printf("[channels] %s 广播被拒 pending=%d: http %d", label, pendingID, resp.StatusCode)
	}
}

// wireExternalChannels 装配广播钩子（server 启动时调用一次）。task_id 只作展示
// 要素透传；pending 行本体仍以 pending_id 为唯一句柄（裁决回调解码用它）。
func (s *Server) wireExternalChannels() {
	if s.m == nil || s.m.interceptor == nil {
		return
	}
	s.m.interceptor.SetAskListener(s.externalChannelListener)
}

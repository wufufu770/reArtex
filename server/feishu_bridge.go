package server

// 飞书通道（0.15.19，批次 D-①）：审批广播出向钩子。
//
// 架构：飞书侧是【独立 Python 桥】（feishu-bridge/，纯 WebSocket 长连接客户端 +
// 出向 REST，零监听端口零攻击面）。reArtex 只在 ask pending 行落库后把审批
// 要素异步推给桥（本文件）；桥收到后渲染审批卡到飞书，人在飞书里用文本命令
// （/approve /deny）裁决，桥回调既有 REST：
//   POST /api/intercept/pending/{id}/decide  {decision: allowed|denied}
// ——审批决策面零新增权限面（与审批页/群聊卡同一入口，SoD 口径不变）。
//
// 配置（settings KV，SQL/运维通道；空=通道关闭）：
//   feishu_bridge_url    桥的接收端点（如 http://127.0.0.1:8791/internal/approval）
//   feishu_bridge_secret 共享口令（X-Bridge-Secret 头；桥侧校验）
// 凭据（飞书 app_id/secret、allowlist）只在桥侧环境变量里——不进 reArtex 进程、
// 不进库、不进日志。

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"
)

const (
	settingFeishuBridgeURL    = "feishu_bridge_url"
	settingFeishuBridgeSecret = "feishu_bridge_secret"
	feishuBroadcastTimeout    = 5 * time.Second
)

// feishuBridgeListener 是 SetAskListener 的实现：现取 settings（空=通道关闭），
// 组装审批要素异步推桥。广播是增值面——任何失败只记日志，不影响审批主链。
func (s *Server) feishuBridgeListener(pendingID int64, convID int64, taskID, agentName, toolName string, input []byte, reason string) {
	if s.m == nil || s.m.pg == nil {
		return
	}
	bridgeURL, ok, _ := s.m.pg.GetSetting(settingFeishuBridgeURL)
	if !ok || strings.TrimSpace(bridgeURL) == "" {
		return
	}
	// URL 只认 http(s)——防 file:/gopher: 等奇异 scheme 被运维误配成 SSRF 面。
	u := strings.TrimSpace(bridgeURL)
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		log.Printf("[feishu] bridge_url 非法（必须是 http/https，当前 %q），通道关闭", bridgeURL)
		return
	}
	secret, _, _ := s.m.pg.GetSetting(settingFeishuBridgeSecret)
	// 输入预览：截断到 1KB——审批卡要看得见上下文，但不整段灌给外部通道。
	preview := input
	if len(preview) > 1024 {
		preview = preview[:1024]
	}
	payload, err := json.Marshal(map[string]any{
		"pending_id": pendingID,
		"conv_id":    convID,
		"task_id":    taskID,
		"agent":      agentName,
		"tool":       toolName,
		"reason":     reason,
		"input":      json.RawMessage(preview),
	})
	if err != nil {
		log.Printf("[feishu] 审批广播组包失败 pending=%d: %v", pendingID, err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), feishuBroadcastTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSpace(bridgeURL), bytes.NewReader(payload))
	if err != nil {
		log.Printf("[feishu] 审计广播请求构造失败 pending=%d: %v", pendingID, err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if secret != "" {
		req.Header.Set("X-Bridge-Secret", secret)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Printf("[feishu] 审批广播失败 pending=%d（桥未启动/网络不可达，不影响审批主链）: %v", pendingID, err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		log.Printf("[feishu] 审批广播被桥拒 pending=%d: http %d", pendingID, resp.StatusCode)
	}
}

// wireFeishuBridge 装配广播钩子（server 启动时调用一次）。task_id 只作展示要素
// 透传；pending 行本体仍以 pending_id 为唯一句柄（桥回调解码用它）。
func (s *Server) wireFeishuBridge() {
	if s.m == nil || s.m.interceptor == nil {
		return
	}
	s.m.interceptor.SetAskListener(s.feishuBridgeListener)
}

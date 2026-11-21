package server

// Amber 外置记忆库接入（0.15.39，批次 J1）。
//
// 0.15.18 裁决的更新：用户提供了 Amber v0.5.0 包（117/117 全绿），裁决由
// 「不引入外部记忆库」更新为「可选组件引入」——功能开关默认关（amber_enabled
// settings 键），未部署/未开启时零行为变化；开启且服务在位时渐进接入。
//
// 信任模型（Amber v0.5.0 原生语义，零改动复用）：
//   - worker 经 REST /api/memory/write 写入 → agent 通道强制进隔离区（quarantine）；
//   - 晋升走 curator（批次 J2，观察模式起步）或人工（Amber Web UI）；
//   - worker 无晋升权限（权限矩阵硬保证——agent 通道无 review 权限）。
//
// 降级纪律：连接失败/超时 → best-effort 记日志，功能静默（不影响主链路）。

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Autumn-27/artex/internal/sodpolicy"
)

// amberEnabled 读功能开关（settings 键 amber_enabled，默认 false）。
func (s *Server) amberEnabled() bool {
	if s.m == nil || s.m.pg == nil {
		return false
	}
	return s.m.pg.GetBool("amber_enabled", false)
}

// amberBaseURL 读服务地址（settings 键 amber_base_url，默认 loopback:8002）。
func (s *Server) amberBaseURL() string {
	if s.m == nil || s.m.pg == nil {
		return ""
	}
	v, _, _ := s.m.pg.GetSetting("amber_base_url")
	if strings.TrimSpace(v) == "" {
		return "http://127.0.0.1:8002"
	}
	return strings.TrimRight(v, "/")
}

// amberToken 读 worker Bearer（环境变量 AMBER_WORKER_TOKEN——worker 令牌只写
// 隔离区；key 只存环境变量）。
func (s *Server) amberToken() string {
	return strings.TrimSpace(osGetenv("AMBER_WORKER_TOKEN"))
}

// amberWrite 写一条记忆（agent 通道 → 隔离区；scope 按 reartex:<target_fp> 命名）。
// best-effort：失败只记日志。body 先过脱敏面（平台凭据形态拒绝外带——与
// evidence 脱敏同口径）。
//
// ⚠️ 0.15.41 通道修复：此前走 REST /api/memory/write + worker 令牌——但 Amber
// 的 REST 面 worker 令牌一律 403（worker 只许 /mcp），auth 开启时写入全挂。
// 改走 /mcp JSON-RPC memory_write（worker 令牌 → agent:worker → agent 通道，
// Amber 侧强制隔离区——信任模型由通道绑定，调用方无法自称 human）。
func (s *Server) amberWrite(scope, body, session string) (string, error) {
	if !s.amberEnabled() {
		return "", fmt.Errorf("amber 未启用")
	}
	if hit := sodpolicy.SensitiveHit([]byte(body)); hit != "" {
		return "", fmt.Errorf("记忆正文命中敏感面（%s），拒绝外带", hit)
	}
	rpc := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name": "memory_write",
			"arguments": map[string]any{
				"scope":   scope,
				"body":    body,
				"session": session,
			},
		},
	}
	buf, _ := json.Marshal(rpc)
	req, _ := http.NewRequest(http.MethodPost, s.amberBaseURL()+"/mcp", bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	if tok := s.amberToken(); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	// 结果形态：{"jsonrpc":"2.0","id":1,"result":{"item_id":"..."}} 或
	// {"jsonrpc":"2.0","id":1,"error":{"code":..,"message":".."}}。
	var out struct {
		Result struct {
			ItemID string `json:"item_id"`
		} `json:"result"`
		Err *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if out.Err != nil {
		return "", fmt.Errorf("amber write mcp 错误: %s", out.Err.Message)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("amber write HTTP %d", resp.StatusCode)
	}
	return out.Result.ItemID, nil
}

// amberSearch 检索（read 面，owner 令牌走 REST——worker 令牌在 REST 面无读
// 权限；worker 召回经 Amber 自身 inject 面 V2，不经此函数）。
func (s *Server) amberSearch(q string, k int) ([]map[string]any, error) {
	if !s.amberEnabled() {
		return nil, fmt.Errorf("amber 未启用")
	}
	body, status, err := s.amberGet("/api/memory/search?q=" + urlQueryEscape(q) + "&k=" + strconv.Itoa(k))
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("amber search HTTP %d", status)
	}
	var out struct {
		Hits []map[string]any `json:"hits"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, err
	}
	return out.Hits, nil
}

// ── 局部助手 ────────────────────────────────────────────────────────────────

func osGetenv(k string) string { return os.Getenv(k) }

func urlQueryEscape(q string) string { return url.QueryEscape(q) }
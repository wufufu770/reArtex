package server

import (
	"context"
	"fmt"
	"log"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/mcphttp"
	"github.com/Autumn-27/norma/mcp"
	actool "github.com/Autumn-27/norma/tool"
)

// stdioPackageRunners 从 registry 拉取代码执行，等价于任意第三方代码执行，
// 且包名可抢注、无版本 pin 可供应链翻转。比对 base name，允许路径前缀。
var stdioPackageRunners = map[string]bool{
	"npx": true, "uvx": true, "bunx": true, "pnpx": true,
	"pipx": true, "dlx": true, "docker": true, "podman": true,
	"sh": true, "bash": true, "zsh": true, "dash": true,
	"python": true, "python3": true, "perl": true, "ruby": true, "node": true,
}

// validateStdioCommand 拒绝包执行器，且要求命令解析为绝对路径。裸命令名允许
// （V2 字面要求绝对路径，但既有 stdio MCP 用裸名强制绝对路径会打断），
// 但必须在 PATH 解析到真实可执行文件。
func validateStdioCommand(cmd string) (string, error) {
	if strings.TrimSpace(cmd) == "" {
		return "", fmt.Errorf("stdio 传输缺少命令")
	}
	base := filepath.Base(cmd)
	if stdioPackageRunners[base] {
		return "", fmt.Errorf(
			"stdio MCP 禁止使用包执行器 %q：它等价于任意第三方代码执行且包名可被抢注。"+
				"请改用已安装的本地二进制绝对路径", base)
	}
	resolved := cmd
	if !filepath.IsAbs(cmd) {
		p, err := exec.LookPath(cmd)
		if err != nil {
			return "", fmt.Errorf("stdio MCP 命令 %q 无法在 PATH 中解析: %w", cmd, err)
		}
		resolved = p
	}
	if !filepath.IsAbs(resolved) {
		return "", fmt.Errorf("stdio MCP 命令必须解析为绝对路径，得到 %q", resolved)
	}
	return resolved, nil
}

// mcpClient is the shared surface of a connected MCP server (stdio, Streamable HTTP,
// or legacy SSE),
// so tools/list and cleanup are handled uniformly regardless of transport.
type mcpClient interface {
	Tools(context.Context) ([]actool.CoreTool, error)
	Close() error
}

// mcpMissingURLWarned 记录已就「缺 URL」告警过的 MCP 名（F5，方案 v9.2 §3.5）。
// 占位行（如 schema 播种的 ScopeSentry：enabled=true、URL 空）会让每次会话装配
// 都走一遍必然失败的拨号；去重后同名只告警一次，配置补全即自动恢复发现。
var mcpMissingURLWarned sync.Map

// warnMCPMissingURLOnce 报告是否应跳过该 server 的发现（true=跳过），并在首次
// 命中时打一条告警。之后命中静默跳过。
func warnMCPMissingURLOnce(name string) bool {
	if _, loaded := mcpMissingURLWarned.LoadOrStore(name, struct{}{}); loaded {
		return true
	}
	log.Printf("[mcp] %s 已启用但未配置 URL——跳过发现（补上 URL 后自动恢复；在 MCP 设置页配置）", name)
	return true
}

// ---------- 连接失败负缓存（v0.9.17，task#9「workers 空跑」根治之一） ----------
//
// G1 装配缓存下，任何一台 server 连接失败都会把整份装配标记为不可缓存
// （cacheable=false）⇒ 此后**每个** worker 认领都重新拨号**全部** server。
// burp 上游 504 挂死（nginx ~60s 才回超时）时，每次认领都被拖住一分多钟，
// worker 一轮都跑不满——这正是「worker 会话在转、0 执行步」的空跑签名。
//
// 负缓存：失败后 TTL 内跳过该 server（本轮没有它的工具，其余 server 正常），
// 装配保持可缓存；TTL 到期自动重试拨号，恢复即自动回归。配置改动/手动刷新
// 会清掉冷却（pgSaveMCP / pgRefreshMCP），改完配置立即生效。
var mcpFailUntil sync.Map // server name → time.Time（冷却到期时刻）

// mcpFailTTL 是连接失败后的重试冷却。短到「恢复后一两轮就能用」，长到能扛住
// 上游几十秒的超时窗口。
const mcpFailTTL = 2 * time.Minute

// mcpDown 报告该 server 是否处于连接失败冷却期。
func mcpDown(name string) bool {
	v, ok := mcpFailUntil.Load(name)
	if !ok {
		return false
	}
	until, _ := v.(time.Time)
	return time.Now().Before(until)
}

// mcpMarkFailed 把该 server 打入失败冷却（到期自动重试）。
func mcpMarkFailed(name string) {
	mcpFailUntil.Store(name, time.Now().Add(mcpFailTTL))
}

// mcpMarkHealthy 清除该 server 的失败冷却（拨号成功即调，恢复立即生效）。
func mcpMarkHealthy(name string) {
	mcpFailUntil.Delete(name)
}

// mcpClearFailCache 清空全部冷却（MCP 配置被人工改动/手动刷新时调用）。
func mcpClearFailCache() {
	mcpFailUntil.Range(func(k, _ any) bool {
		mcpFailUntil.Delete(k)
		return true
	})
}

// connectMCP dials one MCP server per its transport. Callers must Close the client.
func connectMCP(ctx context.Context, m *db.MCPServer) (mcpClient, error) {
	switch m.Transport {
	case "stdio":
		resolved, err := validateStdioCommand(m.Command)
		if err != nil {
			return nil, err
		}
		return mcp.NewStdioClient(ctx, m.Name, resolved, jsonStrMap(m.Env), jsonStrSlice(m.Args)...)
	case "http":
		if m.URL == "" {
			return nil, fmt.Errorf("http 传输缺少 URL")
		}
		// env map doubles as HTTP headers (e.g. Authorization).
		return mcphttp.New(ctx, m.Name, m.URL, jsonStrMap(m.Env), m.Insecure)
	case "sse":
		if m.URL == "" {
			return nil, fmt.Errorf("sse 传输缺少 URL")
		}
		return mcphttp.NewSSE(ctx, m.Name, m.URL, jsonStrMap(m.Env), m.Insecure)
	default:
		return nil, fmt.Errorf("未知传输方式 %q", m.Transport)
	}
}

// discoverAndCacheMCP connects to one MCP, lists its tools, and persists the tool
// names to mcp_tools_cache so the UI shows them without a live connection.
func (s *Server) discoverAndCacheMCP(ctx context.Context, m *db.MCPServer) error {
	// F5：URL 没配的 http/sse 占位行在此同样跳过（启动自动发现也走这里），
	// 「连接失败」的周期日志不再出现，只保留一次告警。
	if (m.Transport == "http" || m.Transport == "sse") && m.URL == "" && warnMCPMissingURLOnce(m.Name) {
		return fmt.Errorf("http/sse 传输缺少 URL")
	}
	cl, err := connectMCP(ctx, m)
	if err != nil {
		return err
	}
	defer cl.Close()
	ts, err := cl.Tools(ctx)
	if err != nil {
		return err
	}
	tools := make([]db.MCPTool, 0, len(ts))
	for _, t := range ts {
		tools = append(tools, db.MCPTool{Name: t.Name(), Description: t.Description()})
	}
	if err := s.m.pg.SaveMCPTools(m.ID, tools); err != nil {
		return err
	}
	log.Printf("[mcp] %s 发现 %d 个工具并已缓存", m.Name, len(tools))
	return nil
}

// discoverEmptyMCPsOnStartup fills the tool cache for any enabled MCP that has none
// yet (notably the seeded browser MCP on first run). Runs sequentially in one
// goroutine so we never spawn many stdio servers (npx) at once, and never blocks
// startup. Best-effort: a failure leaves the cache empty to retry next start.
func (s *Server) discoverEmptyMCPsOnStartup() {
	servers, err := s.m.pg.ListMCP()
	if err != nil {
		log.Printf("[mcp] 启动自动发现: 读取列表失败: %v", err)
		return
	}
	for _, m := range servers {
		if !m.Enabled || len(m.Tools) > 0 {
			continue
		}
		ctx, cancel := context.WithTimeout(s.ctx, 90*time.Second)
		if err := s.discoverAndCacheMCP(ctx, m); err != nil {
			log.Printf("[mcp] 启动自动发现 %s 失败: %v", m.Name, err)
		}
		cancel()
	}
}

package server

import (
	"context"
	"fmt"
	"log"
	"os/exec"
	"path/filepath"
	"strings"
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

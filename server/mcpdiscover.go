package server

import (
	"context"
	"fmt"
	"log"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/mcphttp"
	"github.com/Autumn-27/norma/mcp"
	actool "github.com/Autumn-27/norma/tool"
)

// stdioRunnerNamePatterns 是包执行器/解释器的名字模式（A-07，0.15.81）。
// 从 registry 拉取代码执行等价于任意第三方代码执行，且包名可抢注、无版本 pin
// 可供应链翻转。比旧版 stdioPackageRunners 名字集合多盖两类：版本化名字
// （python3.11 / node18 / ruby3.2——改个后缀就想绕过名字黑名单）与 uv/pip/
// conda/poetry（同为包生态入口）。
var stdioRunnerNamePatterns = []*regexp.Regexp{
	regexp.MustCompile(`^(?:python|python3|ruby|node|perl)(?:\d+(?:\.\d+)?)?$`),
	regexp.MustCompile(`^(?:npx|uvx|bunx|pnpx|pipx|dlx|uv|pip|conda|poetry)$`),
	regexp.MustCompile(`^(?:sh|bash|zsh|dash|docker|podman)$`),
}

// stdioInterpreterDirs 是解释器/包执行器的标准安装目录（真实路径比对用）。
// 只认这些前缀、不认任意目录：自定义二进制装在别处（含 /tmp 的 PoC 工具）
// 必须照常放行——渗透工作流不收敛。
var stdioInterpreterDirs = []string{
	"/bin/", "/usr/bin/", "/usr/local/bin/", "/usr/sbin/", "/usr/local/sbin/",
	"/snap/bin/", "/opt/homebrew/bin/", "/opt/homebrew/sbin/",
}

func matchRunnerName(base string) bool {
	for _, re := range stdioRunnerNamePatterns {
		if re.MatchString(base) {
			return true
		}
	}
	return false
}

// isKnownInterpreterPath 判定真实路径是否落在标准解释器目录内且名字是执行器形态
// （如 /usr/bin/python3.12——软链解析后现形的最终落点）。
func isKnownInterpreterPath(real string) bool {
	for _, dir := range stdioInterpreterDirs {
		if strings.HasPrefix(real, dir) && matchRunnerName(filepath.Base(real)) {
			return true
		}
	}
	return false
}

// envAssignRe 匹配 `env` 命令的 VAR=value 赋值前缀。
var envAssignRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

// stripEnvPrefix 跳过 `env` 之后的旗标（-i/-0/…）与 VAR=value 赋值，返回真正的
// 命令字段。全部被跳过（没有真命令）时返回 nil。
func stripEnvPrefix(fields []string) []string {
	for i, f := range fields {
		if strings.HasPrefix(f, "-") || envAssignRe.MatchString(f) {
			continue
		}
		return fields[i:]
	}
	return nil
}

// validateStdioCommand 拒绝包执行器，且要求命令解析为绝对路径（A-07，0.15.81）。
//
// 判定改两步走：名字模式先拦版本化执行器（python3.11 不再漏网）；再
// LookPath + EvalSymlinks 解析到真实路径比对——`ln -s $(which python3) /tmp/mypy`
// 这类改名软链现形（最终落点还是解释器）。`env python3` 形态剥掉 env 前缀后
// 按真命令判定。返回值是真实绝对路径，登记侧落库用它（运行期不再重复解析）。
//
// 已知盲区（如实记录）：对解释器二进制的【复制】（cp /usr/bin/python3 /tmp/x）
// 无能为力——复制体的真实路径就是它自己，路径层判不出血统；那要靠文件哈希/
// 行为层，不在本卡范围。软链形态（常见逃逸）已封。
func validateStdioCommand(cmd string) (string, error) {
	if strings.TrimSpace(cmd) == "" {
		return "", fmt.Errorf("stdio 传输缺少命令")
	}
	fields := strings.Fields(cmd)
	if len(fields) == 0 {
		return "", fmt.Errorf("stdio MCP 命令解析为空")
	}
	if fields[0] == "env" {
		fields = stripEnvPrefix(fields[1:])
	}
	if len(fields) == 0 {
		return "", fmt.Errorf("stdio MCP 命令解析为空（env 形态未解析出真命令）")
	}
	name := fields[0]
	// 1. 名字模式先拦：裸名与路径尾段都算（/usr/bin/python3.11 的尾段即命中）。
	if base := filepath.Base(name); matchRunnerName(base) {
		return "", fmt.Errorf(
			"stdio MCP 禁止使用包执行器 %q：它等价于任意第三方代码执行且包名可被抢注。"+
				"请改用已安装的本地二进制绝对路径", base)
	}
	// 2. 解析到真实绝对路径再比对：改名/软链的二进制现形。
	resolved := name
	if !filepath.IsAbs(resolved) {
		p, err := exec.LookPath(resolved)
		if err != nil {
			return "", fmt.Errorf("stdio MCP 命令 %q 无法在 PATH 中解析: %w", name, err)
		}
		resolved = p
	}
	real, err := filepath.EvalSymlinks(resolved)
	if err != nil {
		return "", fmt.Errorf("stdio MCP 命令 %q 无法解析真实路径: %w", resolved, err)
	}
	if matchRunnerName(filepath.Base(real)) || isKnownInterpreterPath(real) {
		return "", fmt.Errorf(
			"stdio MCP 禁止使用包执行器（真实路径 %q）：软链/改名不改变它是什么。"+
				"请改用已安装的本地二进制绝对路径", real)
	}
	return real, nil
}

// mcpStore 是启动重验需要的最小存储面（*db.DB 满足）；拆出来让重验逻辑可直接单测。
type mcpStore interface {
	ListMCP() ([]*db.MCPServer, error)
	SaveMCP(*db.MCPServer) (int64, error)
}

// reverifyStdioMCPs 是存量 stdio MCP 的启动期重验（A-07）：登记时落库的是当时的
// 解析结果，二进制后来被删/被换成解释器软链的存量行在此现形——记警告并禁用，
// 不静默放行。登记侧写真实路径后，新行的重验只是确认落点仍在。
func reverifyStdioMCPs(store mcpStore) {
	servers, err := store.ListMCP()
	if err != nil {
		log.Printf("[mcp] 启动重验: 读取列表失败: %v", err)
		return
	}
	for _, m := range servers {
		if !m.Enabled || m.Transport != "stdio" {
			continue
		}
		if _, err := validateStdioCommand(m.Command); err != nil {
			log.Printf("[mcp] 存量 stdio MCP %q 重验不通过，已禁用: %v", m.Name, err)
			m.Enabled = false
			if _, err := store.SaveMCP(m); err != nil {
				log.Printf("[mcp] %q 禁用落库失败: %v", m.Name, err)
			}
		}
	}
}

func (s *Server) reverifyStdioMCPsOnStartup() { reverifyStdioMCPs(s.m.pg) }

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
		return mcphttp.New(ctx, m.Name, m.URL, jsonStrMap(m.Env), m.Insecure, mcphttp.WithCredProfile(m.ID))
	case "sse":
		if m.URL == "" {
			return nil, fmt.Errorf("sse 传输缺少 URL")
		}
		return mcphttp.NewSSE(ctx, m.Name, m.URL, jsonStrMap(m.Env), m.Insecure, mcphttp.WithCredProfile(m.ID))
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

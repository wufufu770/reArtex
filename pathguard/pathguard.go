// Package pathguard blocks agent file tools from reaching ARTEX's own control-plane
// secrets.
//
// 设计取舍（与 guard/shortcircuit.go 的短路表互补，不是替代）: 这里用真实路径解析
// （EvalSymlinks）而非字符串匹配，因此能挡住短路表自己写明挡不住的那类逃逸——
// `ln -s /app/jwt.key /tmp/x` 之后 Read /tmp/x。反过来，本包刻意【不做】根目录收敛:
// 渗透 agent 的工作区是 /app/data/sessions/<key>，而目标在别处，收敛会打断每一个
// 「把证据读到本地再分析」的流程。所以策略是平台秘密拒绝列表，其余一律放行。
package pathguard

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// secretRoots 是 ARTEX 自身的控制面秘密，agent 工具一律不得读写。
//
// 保持为 Go 常量而非 DB/settings，理由同 guard/shortcircuit.go: 安全边界不能挂在
// 任何可热改的存储上，否则一次 PUT /api/settings 就能整体关闭。
var secretRoots = []string{
	"/app/jwt.key",          // JWT 签名密钥
	"/app/.env",             // 容器环境变量落盘（含 DSN 与 LLM key）
	"/app/data/traffic/_ca", // MITM CA 私钥
}

// procSecrets 是 /proc 下会泄露宿主/容器进程环境的入口。形态特殊（PID 段可变），
// 单独判定而非放进 secretRoots。
var procSecrets = []string{"environ", "cmdline", "mem", "maps", "mounts"}

// pathParams 给出各内置文件工具承载路径的参数名。取自 norma v0.4.3 的 Spec.Schema。
var pathParams = map[string][]string{
	"Read":      {"file_path"},
	"Write":     {"file_path"},
	"Edit":      {"file_path"},
	"MultiEdit": {"file_path"},
	"LS":        {"path"},
	"Glob":      {"path"},
	"Grep":      {"path"},
}

// Check 判定一次工具调用是否可以放行。nil 表示放行。
//
// 只覆盖路径类工具：Bash 的命令串里路径要经 shell 展开才现形，字符串层判不准，
// 那部分仍由 guard/shortcircuit.go 的短路表按 best-effort 处理；WebFetch 的 url
// 属出网策略，由 netguard 在拨号期判定，不在这里。
func Check(toolName string, input []byte) error {
	params, ok := pathParams[toolName]
	if !ok {
		return nil
	}
	var in map[string]any
	if err := json.Unmarshal(input, &in); err != nil {
		return nil // 解析不了交给工具自己报错，这里不因格式问题误伤
	}
	for _, key := range params {
		raw, ok := in[key].(string)
		if !ok || raw == "" {
			continue // 参数缺省时工具回落到 WorkingDir，不构成越界
		}
		resolved, err := resolveExisting(raw)
		if err != nil {
			// 解析不出真实路径时不猜，按拒绝处理更安全；代价是极少数畸形路径会误伤。
			return fmt.Errorf("拒绝 %s %s=%q: 无法解析真实路径: %w", toolName, key, raw, err)
		}
		if err := denyReason(resolved); err != nil {
			return fmt.Errorf("拒绝 %s %s=%q: %w", toolName, key, raw, err)
		}
	}
	return nil
}

func denyReason(real string) error {
	for _, root := range secretRoots {
		if real == root || strings.HasPrefix(real, root+"/") {
			return fmt.Errorf("目标是 ARTEX 平台自身的凭据/控制面文件 (%s)", root)
		}
	}
	if procSecretMatch(real) {
		return fmt.Errorf("目标是进程环境入口 %s（可读到 ARTEX_PG_DSN 与 LLM key）", real)
	}
	return nil
}

// procSecretMatch 匹配 /proc/<pid-or-self>/{environ,cmdline,...}。
func procSecretMatch(real string) bool {
	if !strings.HasPrefix(real, "/proc/") {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(real, "/proc/"), "/")
	if len(parts) < 2 {
		return false
	}
	entry := parts[1]
	for _, s := range procSecrets {
		if entry == s {
			return true
		}
	}
	return false
}

// resolveExisting 返回 p 的真实路径。p 不存在时（写入新文件的常见情形）改为解析
// 最深的已存在祖先再拼回剩余段——否则对新文件会解析失败而误伤正常写入。
func resolveExisting(p string) (string, error) {
	if strings.HasPrefix(p, "~") {
		if home, err := os.UserHomeDir(); err == nil {
			p = filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	if r, err := filepath.EvalSymlinks(abs); err == nil {
		return r, nil
	}
	dir, tail := abs, ""
	for {
		parent := filepath.Dir(dir)
		if parent == dir {
			return abs, nil // 一路到根都没解析出可用的祖先
		}
		if tail == "" {
			tail = filepath.Base(dir)
		} else {
			tail = filepath.Join(filepath.Base(dir), tail)
		}
		dir = parent
		if r, err := filepath.EvalSymlinks(dir); err == nil {
			return filepath.Join(r, tail), nil
		}
	}
}

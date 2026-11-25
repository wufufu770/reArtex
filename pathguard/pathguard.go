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
	"sync"
)

// secretRoots 是 ARTEX 自身的控制面秘密，agent 工具一律不得读写。
//
// 保持为 Go 常量而非 DB/settings，理由同 guard/shortcircuit.go: 安全边界不能挂在
// 任何可热改的存储上，否则一次 PUT /api/settings 就能整体关闭。
var secretRoots = []string{
	"/app/jwt.key",          // JWT 签名密钥
	"/app/.env",             // 容器环境变量落盘（含 DSN 与 LLM key）
	"/app/data/traffic/_ca", // MITM CA 私钥
	"/run/secrets",          // docker secret（数据库连接串等）：只读挂载，删不掉也清不掉
}

// procSecrets 是 /proc 下会泄露宿主/容器进程环境的入口。形态特殊（PID 段可变），
// 单独判定而非放进 secretRoots。
var procSecrets = []string{"environ", "cmdline", "mem", "maps", "mounts"}

// secretConfigPath 是本实例 config.json 的解析后绝对路径（A-09，0.15.79）。
//
// config.json 可能含 database.dsn 明文连接串（含口令），按控制面秘密对待：
// agent 的 Read/Grep 读到即等于泄密。路径不在编译期写死——config.Path() 有三个
// 候选位置（cwd / BaseDir / 可执行文件旁，ARTEX_CONFIG 可显式覆盖），字面清单
// 只会漏掉部署者自定义的位置。启动期由 main 用 config.Path() 注入一次。
//
// 并发口径：启动期写一次、此后只读；RWMutex 兜住理论上「启动早期并发工具调用」
// 的窗口，代价可忽略。
var (
	secretConfigMu   sync.RWMutex
	secretConfigPath string
)

// SetSecretConfigPath 登记本实例 config.json 的绝对路径（启动期调用一次；空串
// 忽略——config.json 不存在时 Path() 返回的候选路径无秘密可守，不登记）。
func SetSecretConfigPath(p string) {
	p = strings.TrimSpace(p)
	if p == "" {
		return
	}
	secretConfigMu.Lock()
	secretConfigPath = p
	secretConfigMu.Unlock()
}

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
//
// workingDir 是工具进程的工作目录（norma permission.Context.WorkingDir，A-11）：
// 入参里的相对路径按它而不是进程 CWD 解析——工具真正打开文件时用的就是它。
// 为空时回落进程 CWD（与旧版一致）。
func Check(toolName string, input []byte, workingDir string) error {
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
		resolved, err := resolveReal(raw, workingDir)
		if err != nil {
			// 解析不出真实路径（含悬空符号链接）时不猜，按拒绝处理更安全；
			// 代价是极少数畸形路径会误伤。
			return fmt.Errorf("拒绝 %s %s=%q: %w", toolName, key, raw, err)
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
	// A-09：config.json（可能含明文 DSN）。路径启动期注入；未登记（测试环境/
	// 无配置文件）时不拦，行为与旧版一致。
	secretConfigMu.RLock()
	cfg := secretConfigPath
	secretConfigMu.RUnlock()
	if cfg != "" && (real == cfg || strings.HasPrefix(real, cfg+"/")) {
		return fmt.Errorf("目标是 ARTEX 平台自身的配置文件（可能含明文数据库口令）(%s)", cfg)
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

// resolveReal 返回 p 的真实路径（A-11，0.15.80）。语义：
//   - 相对路径按 workingDir 拼全（为空回落进程 CWD）；~ 先展开；
//   - 路径存在 → EvalSymlinks 直解（整条链接链都算进去）；
//   - 路径不存在（写新文件的常态）→ 沿父目录向上找已存在的锚点，把剩余段拼回；
//   - 悬空符号链接（Lstat 能看到链接本身、跟随目标不存在/成环）→ 直接报错拒绝，
//     不尝试跟随：读它必然失败，写它会在链接目标处落盘，绕开工作目录判定。
//
// 全包的路径解析统一走这里（Check 与 tokens.go 的真实路径层共用），不再各写各的。
func resolveReal(p, workingDir string) (string, error) {
	if strings.HasPrefix(p, "~") {
		if home, err := os.UserHomeDir(); err == nil {
			p = filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	if !filepath.IsAbs(p) && workingDir != "" {
		p = filepath.Join(workingDir, p)
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return real, nil
	}
	// 路径不存在或链接链断掉：自 abs 起逐层向上，Lstat 抓悬空符号链接，
	// EvalSymlinks 找已存在的锚点。每一层都被 Lstat 查过，锚点以下的剩余段
	// 不含未决符号链接，拼回即得真实落点。
	cur := abs
	for {
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", fmt.Errorf("无法解析真实路径 %q（已走到根）", abs)
		}
		if fi, err := os.Lstat(cur); err == nil && fi.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("悬空符号链接 %q（跟随目标不存在或成环）——按拒绝处理", cur)
		}
		if real, err := filepath.EvalSymlinks(parent); err == nil {
			rel, err := filepath.Rel(parent, abs)
			if err != nil {
				rel = strings.TrimPrefix(abs, parent+"/")
			}
			return filepath.Join(real, rel), nil
		}
		cur = parent
	}
}

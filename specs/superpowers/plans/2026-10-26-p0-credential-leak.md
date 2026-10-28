# P0 凭据泄露链修复 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让 artex 进程的 `/proc/<pid>/environ` 内不再存在任何平台秘密，堵住「agent 经 /proc 读父进程环境块 → 拿到 DB 口令 → 接管平台」的接管链。

**Architecture:** 三层。①**供给侧消除**（根治）：DSN 改由容器内文件供给，compose 的 entrypoint wrapper 在 `exec` 前 `unset` 环境变量、artex 启动读入文件后立刻清零并删除 → env、`/proc`、文件系统三处同时没有秘密。②**纵深防御**：`guard` 的平台凭据正则由「只认字面数字 pid」放宽为「覆盖 `/proc/<任意>/environ` 的 glob 与展开形态」。③**启动自检**：启动时读 `/proc/self/environ`，发现残留即告警，`ARTEX_REQUIRE_ENV_SCRUB=1` 时拒绝启动 —— 防止「制造了已加固的错觉」。

**Tech Stack:** Go 1.26.8（`regexp` RE2，无新依赖）· PostgreSQL 16 · Docker Compose v2 · Bash（pre-push 钩子）

**设计文档：** `specs/superpowers/specs/2026-10-26-p0-credential-leak-design.md`（commit `68460fd`）

---

## Global Constraints

这些约束来自设计文档与 Sir 的决策，**每个 task 都隐含包含**：

1. **分支**：`local/a2-hardening`（基线 `1b29664`）。不切分支、不动 `main`。
2. **范围**：仅 P0 凭据泄露链。不碰 G-17 / G-04④ / G-03 全字段 / CI 门 / 交付源改指。
3. **正则边界**：只收紧 `environ`。`/proc/\d+/cmdline` 与 `/proc/\d+/mem` **原样保留**（字面数字形态仍拦，glob 形态仍放过）。
4. **提交信息**：中文 + SEMANTIC 风格（`type(scope): 说明`），与仓库既有 19/30 一致。
5. **提交粒度**：实现与其直接测试必须在同一 commit；不同目录分开提交。
6. **安全规则纪律**：每条 deny/ask 必须配「不许误伤渗透正事」的**负向**测试用例（仓库既有纪律，见 `guard/shortcircuit.go` 顶部注释）。
7. **不引入新依赖**：`go.mod` 不动。
8. **不重建 pg 数据卷**：`findings` / `assets` / `conversations` 必须零丢失。
9. **部署侧改动只进本地文件**：`docker-compose.override.yml` 已被 `.git/info/exclude` 排除，永不提交、永不推送。
10. **提交前自查**：pre-push 钩子会拦个人路径 / 个人 MCP / 凭据，其规则清单是唯一权威
    （见本机 `.git/hooks/pre-push` 的 `FORBIDDEN_CONTENT`，该文件本地专用、不入库）。
    提交前跑 `GIT_MASTER=1 git push --dry-run reArtex local/a2-hardening`，退出码须为 0（放行）。
11. **git 命令前缀**：所有 git 命令带 `GIT_MASTER=1`。
12. **既有失败基线**：`server` 包整体测试在基线 `1b29664` 上即因缺 DB 配置而失败（环境性失败，非本批引入）。判定标准是**失败集合不变**，不是「全绿」。

---

## File Structure

| 文件 | 动作 | 职责 |
| --- | --- | --- |
| `config/config.go` | 修改 | 新增 DSN 文件来源 `readAndBurnDSNFile`；新增自检 `SecretEnvLeak` / `leakSecretEnvKeys` / `secretEnvPolicy` / `CheckSecretEnv` / `SecretEnvReport` |
| `config/config_test.go` | 修改 | 扩展 `TestPostgresDSNPrecedence`；新增文件来源、读入即删、错误路径、自检策略四组用例 |
| `server/server.go` | 修改 | 启动序列调用 `config.CheckSecretEnv()`，按 `Fatal` 决定告警或拒绝启动 |
| `guard/shortcircuit.go` | 修改 | `platformCredentialRe` 的 environ 规则由两条字面量合并为一条覆盖 glob/展开形态的规则 |
| `guard/shortcircuit_test.go` | 修改 | 新增 4 条正向（glob/展开形态）+ 3 条负向（本机侦察不误伤） |
| `CHANGELOG.md` | 修改 | 补 `[Unreleased]` 段（A2 全量 + 本批）与「已知残留」小节 |
| `README.md` | 修改 | 补 `ARTEX_PG_DSN_FILE` 用法与部署说明 |
| `docker-compose.override.yml` | 修改（**本地，不提交**） | entrypoint wrapper + `ARTEX_PG_DSN_FILE` 环境变量 |

---

## Task 1: `config` —— DSN 走文件，读入即清零并删除

**Files:**
- Modify: `config/config.go`（`PostgresDSN()` 位于 137-149 行；新增 `readAndBurnDSNFile`）
- Test: `config/config_test.go`

**Interfaces:**
- Consumes: 无（首个 task）
- Produces:
  - `readAndBurnDSNFile(path string) (string, error)` —— 包内私有，读文件后清零并删除，返回 DSN 文本
  - `PostgresDSN() (dsn, source string, err error)` —— **签名不变**，但新增 `ARTEX_PG_DSN_FILE` 一档来源，优先级 `env ARTEX_PG_DSN` > `file ARTEX_PG_DSN_FILE` > 配置文件

- [ ] **Step 1: 写失败的测试**

在 `config/config_test.go` 末尾追加（需在 import 块补 `"strings"`）：

```go
func TestPostgresDSNFromFile(t *testing.T) {
	dir := t.TempDir()
	dsnPath := filepath.Join(dir, "pg_dsn")
	const want = "postgres://u:p@db:5432/artex?sslmode=disable"
	if err := os.WriteFile(dsnPath, []byte(want), 0o600); err != nil {
		t.Fatalf("写 DSN 文件失败: %v", err)
	}

	t.Setenv("ARTEX_PG_DSN", "")
	t.Setenv("ARTEX_PG_DSN_FILE", dsnPath)

	got, source, err := PostgresDSN()
	if err != nil {
		t.Fatalf("从文件读 DSN 应成功: %v", err)
	}
	if got != want {
		t.Fatalf("从文件读 DSN: got %q want %q", got, want)
	}
	if !strings.Contains(source, dsnPath) {
		t.Fatalf("来源说明应含文件路径，得到 %q", source)
	}

	// 读入后文件必须被删除：否则秘密留在盘上等 agent 来捡（agent 与 artex 同 UID）。
	if _, err := os.Stat(dsnPath); !os.IsNotExist(err) {
		t.Fatalf("DSN 文件读入后应被删除，stat err=%v", err)
	}
}

func TestPostgresDSNFileSourceErrors(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ARTEX_PG_DSN", "")

	// 文件不存在 → 明确报错，不静默 fallback 到配置文件
	t.Setenv("ARTEX_PG_DSN_FILE", filepath.Join(dir, "missing"))
	if _, _, err := PostgresDSN(); err == nil {
		t.Fatal("DSN 文件不存在时应报错")
	}

	// 文件为空 → 明确报错
	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, []byte("   \n"), 0o600); err != nil {
		t.Fatalf("写空 DSN 文件失败: %v", err)
	}
	t.Setenv("ARTEX_PG_DSN_FILE", empty)
	if _, _, err := PostgresDSN(); err == nil {
		t.Fatal("DSN 文件为空时应报错")
	}
}

func TestPostgresDSNEnvWinsOverFile(t *testing.T) {
	dir := t.TempDir()
	dsnPath := filepath.Join(dir, "pg_dsn")
	if err := os.WriteFile(dsnPath, []byte("postgres://fromfile/x"), 0o600); err != nil {
		t.Fatalf("写 DSN 文件失败: %v", err)
	}
	t.Setenv("ARTEX_PG_DSN", "postgres://fromenv/x")
	t.Setenv("ARTEX_PG_DSN_FILE", dsnPath)

	got, _, err := PostgresDSN()
	if err != nil || got != "postgres://fromenv/x" {
		t.Fatalf("env 应优先于文件，got %q err %v", got, err)
	}
	// env 生效时文件不该被动：它属于下一次启动的输入
	if _, err := os.Stat(dsnPath); err != nil {
		t.Fatalf("env 优先时不应动文件：%v", err)
	}
}
```

同时把 import 块改为：

```go
import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)
```

- [ ] **Step 2: 运行测试确认失败**

```bash
cd "$(git rev-parse --show-toplevel)"
GIT_MASTER=1 git checkout -q local/a2-hardening
go test ./config/... -run 'TestPostgresDSN' -v 2>&1 | tail -20
```

预期：`TestPostgresDSNFromFile` FAIL（`ARTEX_PG_DSN_FILE` 未被识别，`PostgresDSN()` 会落到配置文件分支或报错），`TestPostgresDSNFileSourceErrors` FAIL。

- [ ] **Step 3: 实现**

在 `config/config.go` 的 import 块补 `"errors"`：

```go
import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)
```

把 `PostgresDSN()` 的文档注释与函数体改为：

```go
// PostgresDSN resolves the connection string with precedence:
//
//	env ARTEX_PG_DSN  >  file ARTEX_PG_DSN_FILE  >  config file (database.dsn, or assembled from fields)
//
// There is NO built-in fallback: when no source supplies a database config,
// it returns an error naming the config path it inspected, so startup fails loudly
// instead of silently connecting to a wrong default. source describes where the
// DSN came from (for startup logging).
func PostgresDSN() (dsn, source string, err error) {
	if v := strings.TrimSpace(os.Getenv("ARTEX_PG_DSN")); v != "" {
		return v, "环境变量 ARTEX_PG_DSN", nil
	}
	if p := strings.TrimSpace(os.Getenv("ARTEX_PG_DSN_FILE")); p != "" {
		d, err := readAndBurnDSNFile(p)
		if err != nil {
			return "", "", fmt.Errorf("读取 ARTEX_PG_DSN_FILE %q 失败: %w", p, err)
		}
		return d, "文件 " + p + " (读入后已删除)", nil
	}
	db := Load().Database
	if d := strings.TrimSpace(db.DSN); d != "" {
		return d, "配置文件 " + Path() + " (database.dsn)", nil
	}
	if db.Host != "" || db.DBName != "" || db.User != "" {
		return db.buildDSN(), "配置文件 " + Path() + " (database 字段)", nil
	}
	return "", "", fmt.Errorf("未找到数据库配置：环境变量 ARTEX_PG_DSN 未设置，且配置文件 %s 未提供 database（dsn 或 host/user/dbname）。请创建该配置文件或设置环境变量后重试", Path())
}

// readAndBurnDSNFile 读连接串后立刻把磁盘上的那份抹掉。
//
// 只读不删等于把秘密留在盘上等 agent 来捡：agent 的 Bash 与 artex 同 UID，
// 且 /app/data 是它的工作区根。内容已在内存里，磁盘上的副本没有存在价值。
//
// 顺序是先清零再删：删除失败时至少内容不在了。
func readAndBurnDSNFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if f, err := os.OpenFile(path, os.O_WRONLY, 0); err == nil {
		_ = f.Truncate(0)
		_ = f.Close()
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("删除 DSN 文件失败（内容已清零）: %w", err)
	}
	dsn := strings.TrimSpace(string(b))
	if dsn == "" {
		return "", errors.New("DSN 文件为空")
	}
	return dsn, nil
}
```

- [ ] **Step 4: 运行测试确认通过**

```bash
cd "$(git rev-parse --show-toplevel)"
go test ./config/... -v 2>&1 | tail -20
go build ./... && echo "build ok"
```

预期：`TestPostgresDSNPrecedence` / `TestPostgresDSNFromFile` / `TestPostgresDSNFileSourceErrors` / `TestPostgresDSNEnvWinsOverFile` 全 PASS，`build ok`。

- [ ] **Step 5: 提交**

```bash
cd "$(git rev-parse --show-toplevel)"
GIT_MASTER=1 git push --dry-run reArtex local/a2-hardening   # 退出码须为 0
GIT_MASTER=1 git add config/config.go config/config_test.go
GIT_MASTER=1 git diff --cached --stat
GIT_MASTER=1 git commit -m "feat(config): 新增 ARTEX_PG_DSN_FILE 来源，读入即清零并删除文件"
```

自查命令须 0 命中（无输出）。

---

## Task 2: `config` + `server` —— 启动自检平台凭据是否仍可从环境块读到

**Files:**
- Modify: `config/config.go`（新增自检相关函数）
- Modify: `server/server.go`（`agent.ScrubSecretEnv()` 调用点之后，约 257 行；import 块补 `config`）
- Test: `config/config_test.go`

**Interfaces:**
- Consumes: Task 1 的 `config` 包（同一包内调用，无需新增依赖）
- Produces:
  - `SecretEnvReport struct { Leaks []string; Fatal bool; Reason string }`
  - `CheckSecretEnv() SecretEnvReport`
  - `SecretEnvLeak() []string`
  - 包内私有：`leakSecretEnvKeys(block []byte) []string`、`secretEnvPolicy(leaks []string, requireEnvScrub string) (fatal bool, reason string)`

- [ ] **Step 1: 写失败的测试**

在 `config/config_test.go` 末尾追加：

```go
func TestLeakSecretEnvKeys(t *testing.T) {
	block := []byte("PATH=/usr/bin\x00ARTEX_PG_DSN=postgres://u:p@db/x\x00ANTHROPIC_API_KEY=\x00OPENAI_API_KEY=sk-xxx\x00HOME=/root\x00")
	got := leakSecretEnvKeys(block)
	want := []string{"ARTEX_PG_DSN", "OPENAI_API_KEY"}
	if len(got) != len(want) {
		t.Fatalf("泄露键: got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("泄露键: got %v want %v", got, want)
		}
	}
}

func TestSecretEnvPolicy(t *testing.T) {
	// 无残留 → 不致命、无理由
	if fatal, reason := secretEnvPolicy(nil, "1"); fatal || reason != "" {
		t.Fatalf("无残留时应不致命且无理由，得到 fatal=%v reason=%q", fatal, reason)
	}
	// 有残留但未开启 → 只告警
	fatal, reason := secretEnvPolicy([]string{"ARTEX_PG_DSN"}, "")
	if fatal {
		t.Fatal("未开启 ARTEX_REQUIRE_ENV_SCRUB 时不应致命")
	}
	if !strings.Contains(reason, "ARTEX_PG_DSN") {
		t.Fatalf("理由应含泄露键名，得到 %q", reason)
	}
	// 有残留且开启 → 致命
	if fatal, _ := secretEnvPolicy([]string{"ARTEX_PG_DSN"}, "1"); !fatal {
		t.Fatal("开启 ARTEX_REQUIRE_ENV_SCRUB=1 后应致命")
	}
}
```

> `ANTHROPIC_API_KEY=`（空值）不算泄露 —— 没有秘密就没有损失。这条断言在上面 `want` 里不含它，是刻意的。

- [ ] **Step 2: 运行测试确认失败**

```bash
cd "$(git rev-parse --show-toplevel)"
go test ./config/... -run 'TestLeakSecretEnvKeys|TestSecretEnvPolicy' -v 2>&1 | tail -15
```

预期：编译失败，`leakSecretEnvKeys` / `secretEnvPolicy` 未定义。

- [ ] **Step 3: 实现**

在 `config/config.go` 末尾追加：

```go
// secretEnvKeys 是平台自身凭据在环境块里的键名。
var secretEnvKeys = []string{"ARTEX_PG_DSN", "ANTHROPIC_API_KEY", "OPENAI_API_KEY"}

// SecretEnvReport 是启动自检的结论。
type SecretEnvReport struct {
	Leaks  []string // 仍可从 /proc/self/environ 读到的平台秘密键名
	Fatal  bool     // 是否应拒绝启动
	Reason string   // 人类可读的结论（无泄露时为空）
}

// leakSecretEnvKeys 从一段环境块字节里挑出仍带值的平台秘密键名。
//
// 空值不算泄露：键在但没值，agent 读走也没有秘密。
func leakSecretEnvKeys(block []byte) []string {
	want := map[string]bool{}
	for _, k := range secretEnvKeys {
		want[k] = true
	}
	var hits []string
	for _, kv := range strings.Split(string(block), "\x00") {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || !want[k] || strings.TrimSpace(v) == "" {
			continue
		}
		hits = append(hits, k)
	}
	return hits
}

// SecretEnvLeak 返回仍能从 /proc/self/environ 读到的平台秘密键名。
//
// 平台自己的 exec 期环境块是 agent 子进程够得着的（同 UID）：os.Unsetenv 只改
// Go 进程内的副本，改不动内核那块。所以凭据一旦进了容器环境，agent 一条
// `cat /proc/*/environ` 就能读走。这里读的就是那块，用来在启动时把问题喊出来。
//
// 读不到（非 Linux / 权限不足）时返回 nil：无法判定就不谎报。
func SecretEnvLeak() []string {
	b, err := os.ReadFile("/proc/self/environ")
	if err != nil {
		return nil
	}
	return leakSecretEnvKeys(b)
}

// secretEnvPolicy 决定发现残留凭据时的动作：致命，还是仅告警。
//
// requireEnvScrub 来自 ARTEX_REQUIRE_ENV_SCRUB：只有显式设为 "1" 才致命。
// 默认只告警 —— 官方镜像等既有部署可能合法地用 env 传参，硬失败会让它们起不来。
func secretEnvPolicy(leaks []string, requireEnvScrub string) (fatal bool, reason string) {
	if len(leaks) == 0 {
		return false, ""
	}
	reason = fmt.Sprintf("平台凭据仍可经 /proc/self/environ 读取（%v）："+
		"同 UID 的 agent 子进程可用 `cat /proc/*/environ` 读走。"+
		"请改用 ARTEX_PG_DSN_FILE，或把密钥移出容器环境。", leaks)
	return requireEnvScrub == "1", reason
}

// CheckSecretEnv 读进程自身的环境块，判断平台凭据是否仍可被 agent 子进程读到。
func CheckSecretEnv() SecretEnvReport {
	leaks := SecretEnvLeak()
	fatal, reason := secretEnvPolicy(leaks, os.Getenv("ARTEX_REQUIRE_ENV_SCRUB"))
	return SecretEnvReport{Leaks: leaks, Fatal: fatal, Reason: reason}
}
```

在 `server/server.go` 的 import 块补一行（该块已有 `fmt` / `log` / `os`，无需新增）：

```go
	"github.com/Autumn-27/artex/config"
```

把 `agent.ScrubSecretEnv()` 那一行之后改为：

```go
	agent.ScrubSecretEnv()
	rep := config.CheckSecretEnv()
	if len(rep.Leaks) == 0 {
		log.Printf("[security] 平台凭据未残留在进程环境块中")
	} else if rep.Fatal {
		log.Fatalf("[security] %s", rep.Reason)
	} else {
		log.Printf("[security] %s", rep.Reason)
	}
```

- [ ] **Step 4: 运行测试确认通过**

```bash
cd "$(git rev-parse --show-toplevel)"
go test ./config/... -v 2>&1 | tail -15
go build ./... && echo "build ok"
go vet ./config/... ./server/... && echo "vet ok"
```

预期：4 组 config 测试全 PASS，`build ok`，`vet ok`。

- [ ] **Step 5: 提交**

```bash
cd "$(git rev-parse --show-toplevel)"
GIT_MASTER=1 git push --dry-run reArtex local/a2-hardening   # 退出码须为 0
GIT_MASTER=1 git add config/config.go server/server.go config/config_test.go
GIT_MASTER=1 git diff --cached --stat
GIT_MASTER=1 git commit -m "feat(config): 启动自检平台凭据是否仍可经 /proc/self/environ 读取"
```

---

## Task 3: `guard` —— 平台环境块拦截覆盖 glob 与展开形态

**Files:**
- Modify: `guard/shortcircuit.go`（`platformCredentialRe`，96-112 行）
- Test: `guard/shortcircuit_test.go`

**Interfaces:**
- Consumes: 无
- Produces: 无对外接口变更。`shortCircuit(tool string, input []byte) *intercept.Decision` 行为变化：
  - 新增被 deny 的输入形态：`/proc/*/environ`、`/proc/$$/environ`、`/proc/[0-9]*/environ`、`/proc/1/envi*on`
  - 保持不拦的输入：`/proc/net/tcp`、`/proc/cpuinfo`、`/proc/*/cmdline`、`/proc/self/status`

- [ ] **Step 1: 写失败的测试**

在 `guard/shortcircuit_test.go` 的 `TestShortCircuitPlatformCredentialsDeny` 的 `deny` 切片里，`{"proc self environ", ...}` 之后追加 4 条：

```go
		{"proc environ glob", `{"command":"cat /proc/*/environ"}`},
		{"proc environ 变量展开", `{"command":"cat /proc/$$/environ"}`},
		{"proc environ 字符类 glob", `{"command":"cat /proc/[0-9]*/environ"}`},
		{"proc environ 文件名 glob", `{"command":"cat /proc/1/envi*on"}`},
```

在 `TestShortCircuitMustNotBlockPentestWork` 的 `allow` 切片里，`{"本机侦察 /proc/net/tcp", ...}` 之后追加 3 条：

```go
		{"本机侦察 /proc/cpuinfo", `{"command":"cat /proc/cpuinfo"}`},
		{"本机侦察 /proc/*/cmdline", `{"command":"cat /proc/*/cmdline"}`},
		{"本机侦察 /proc/self/status", `{"command":"cat /proc/self/status"}`},
```

> **不要**把 `/proc/1234/cmdline` 写成负向用例：既有的 `/proc/\d+/cmdline` 规则会拦它（字面数字形态维持现状是 Sir 的决定）。负向用例只能用 glob 形态。

- [ ] **Step 2: 运行测试确认失败**

```bash
cd "$(git rev-parse --show-toplevel)"
go test ./guard/... -run 'TestShortCircuitPlatformCredentialsDeny|TestShortCircuitMustNotBlockPentestWork' -v 2>&1 | tail -20
```

预期：4 条新的 environ glob 用例 FAIL（当前正则只认 `/proc/self/environ` 与 `/proc/\d+/environ`）。

- [ ] **Step 3: 实现**

在 `guard/shortcircuit.go` 的 `platformCredentialRe` 里，把这两行：

```go
		`|/proc/self/environ` +
		`|/proc/\d+/environ` +
```

替换为一行：

```go
		`|/proc/[^\s"']*envi[^\s"']*` +
```

并把该正则上方的注释块（`/proc 族：任意 PID 的环境块 / 命令行 / 内存` 那段）改为：

```go
		// /proc 族：任意 PID 的环境块 / 命令行 / 内存。
		// environ 用一条覆盖形态的规则收口：pid 段可以是数字、self、*、
		// [0-9]*、$$ 等任意写法，文件名段也可能是 glob（envi*on）。
		// 只认字面形态的话，cat /proc/*/environ 就绕过去了。
		//
		// 这是纵深防御，不是闭合：运行时才成形的路径（find /proc -name environ
		// -exec cat {} \;）字符串预筛挡不住。真正的闭合靠供给侧 —— 凭据根本
		// 不进进程环境块（ARTEX_PG_DSN_FILE + 启动即读入即删除）。
		`|/proc/[^\s"']*envi[^\s"']*` +
```

`cmdline` 与 `mem` 两条规则**原样保留**（字面数字形态仍拦，glob 形态仍放过）。

- [ ] **Step 4: 运行测试确认通过（验收 A3 / A4）**

```bash
cd "$(git rev-parse --show-toplevel)"
go test ./guard/... -v 2>&1 | tail -25
go build ./... && echo "build ok"
```

预期：guard 包全部用例 PASS，`build ok`。逐项对照验收：

| 验收 | 判据 | 对应用例 |
| --- | --- | --- |
| **A3** | 5 种 glob/展开形态全部 `deny` | `TestShortCircuitPlatformCredentialsDeny` 里的 `proc environ glob` / `proc environ 变量展开` / `proc environ 字符类 glob` / `proc environ 文件名 glob` / `proc self environ` |
| **A4** | 本机侦察 4 类全部**不拦** | `TestShortCircuitMustNotBlockPentestWork` 里的 `本机侦察 /proc/net/tcp` / `本机侦察 /proc/cpuinfo` / `本机侦察 /proc/*/cmdline` / `本机侦察 /proc/self/status` |


- [ ] **Step 5: 提交**

```bash
cd "$(git rev-parse --show-toplevel)"
GIT_MASTER=1 git push --dry-run reArtex local/a2-hardening   # 退出码须为 0
GIT_MASTER=1 git add guard/shortcircuit.go guard/shortcircuit_test.go
GIT_MASTER=1 git diff --cached --stat
GIT_MASTER=1 git commit -m "fix(guard): 收紧平台环境块拦截，覆盖 /proc/*/environ 的 glob 与展开形态"
```

---

## Task 4: 部署侧 wrapper + 集成验收

**Files:**
- Modify: `docker-compose.override.yml`（**本地文件，不提交、不推送**）
- 不产出 commit

**Interfaces:**
- Consumes: Task 1 的 `ARTEX_PG_DSN_FILE`；Task 2 的 `config.CheckSecretEnv()`
- Produces: 运行环境里 `/proc/<artex-pid>/environ` 无 DSN

> 本 task 必须先重建镜像：当前 `artex:local` 镜像是 10-20 从 A2 工作区构建的，**不含 Task 1–3 的新代码**。

- [ ] **Step 1: 记录基线行数（用于验收 A5）**

```bash
docker exec artex-postgres-1 psql -U artex -d artex -t -c "
select 'findings', count(*) from findings
union all select 'assets', count(*) from assets
union all select 'conversations', count(*) from conversations;"
```

把三行数值记下，Step 6 要比对。

- [ ] **Step 2: 改 `docker-compose.override.yml`**

把 `services.artex` 段落改成（**保留已有的三个个人 skill 只读挂载**）：

```yaml
services:
  artex:
    image: artex:local
    pull_policy: never
    # DSN 供给侧消除：把带口令的 DSN 从「进程环境块」挪到「一次性文件」。
    #
    # 关键在于 unset 之后 exec —— exec 会用 shell 的当前环境重建进程映像，
    # 所以 exec 出来的 artex 的 /proc/<pid>/environ 里没有 DSN。只 unset 不 exec
    # 是没用的：unset 只改 shell 内部变量表，改不动内核保存的 exec 期环境块。
    #
    # 顺序不能反：先写文件，再 unset，最后 exec。
    entrypoint:
      - /bin/sh
      - -c
      - >-
        umask 077; mkdir -p /run/artex;
        printf '%s' "$$ARTEX_PG_DSN" > /run/artex/pg_dsn;
        unset ARTEX_PG_DSN;
        exec /app/start.sh "$$@"
      - --
    environment:
      ARTEX_PG_DSN_FILE: /run/artex/pg_dsn
    volumes:
      # 已有的个人自建 recon skill 只读挂载保持不变（它们不在版本库里，
      # 本计划不复制其清单 —— 需要时看本机 docker-compose.override.yml）。
```

> `$$` 是 compose 的转义，实际传给容器的是 `$ARTEX_PG_DSN` 与 `$@`。
> 数组末位 `--` 让 `sh -c` 把后续参数放进 `$@`；镜像的 `CMD ["-addr", ":8787", "-proxy", ":8788"]` 会由 compose 追加。
> **`volumes` 一节不要动**：那些挂载指向仓库外的个人 skill，动了会打断本机的 recon 流程。

- [ ] **Step 3: 重建镜像**

```bash
cd "$(git rev-parse --show-toplevel)"
ARTEX_SKIP_FRONTEND=1 ./build.sh
docker build --build-arg TARGETARCH=amd64 -t artex:local .
```

预期：产出新的 `dist/amd64/artex`，镜像 `artex:local` 重建成功。
（`ARTEX_SKIP_FRONTEND=1` 复用已就绪的 `server/webui/dist`，跳过 npm。）

- [ ] **Step 4: 重建容器**

```bash
cd "$(git rev-parse --show-toplevel)"
docker compose -p artex up -d
for i in $(seq 1 40); do
  code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 3 http://127.0.0.1:8787/ 2>/dev/null)
  [ "$code" = "200" ] && break
  sleep 2
done
echo "8787 HTTP=$code"
```

预期：`8787 HTTP=200`。**pg 容器不重建**（`docker compose up -d` 只重建 artex 服务）。

- [ ] **Step 5: 跑验收 A1 / A2 / A8**

```bash
echo "--- A1：artex 进程环境块里的平台秘密（期望 0 行）---"
docker exec artex-artex-1 sh -c 'tr "\000" "\n" < /proc/1/environ | grep -E "ARTEX_PG_DSN=|_API_KEY="' || echo "  0 行 ✓"

echo "--- A2：glob 形态读父进程环境块（期望 0）---"
docker exec artex-artex-1 sh -c 'cat /proc/*/environ | tr "\000" "\n" | grep -c "^ARTEX_PG_DSN=postgres"' || echo "  0 ✓"

echo "--- A8：启动日志的自检结论 ---"
docker logs artex-artex-1 2>&1 | grep -E '\[security\]' | tail -3
```

预期：
- A1 输出 `0 行 ✓`
- A2 输出 `0`
- A8 出现 `[security] 平台凭据未残留在进程环境块中`
- 启动日志**不应**出现 `[security] 平台凭据仍可经 ...` 的告警行

- [ ] **Step 6: 跑验收 A5（数据零丢失）**

```bash
docker exec artex-postgres-1 psql -U artex -d artex -t -c "
select 'findings', count(*) from findings
union all select 'assets', count(*) from assets
union all select 'conversations', count(*) from conversations;"
```

三个数值必须与 Step 1 记录的一致。

- [ ] **Step 7: 负向验证 —— 自检确实会喊出来**

临时绕过 wrapper（在 override 的 `environment` 里加回 `ARTEX_PG_DSN`、去掉 `entrypoint`），重建容器：

```bash
cd "$(git rev-parse --show-toplevel)"
cp docker-compose.override.yml /tmp/opencode/ov.good.yml
# 编辑 override：加回 environment: ARTEX_PG_DSN: postgres://artex:x@postgres:5432/artex?sslmode=disable，并删除 entrypoint 段
docker compose -p artex up -d
sleep 6
docker logs artex-artex-1 2>&1 | grep -E '\[security\]' | tail -3
```

预期：出现 `[security] 平台凭据仍可经 /proc/self/environ 读取（[ARTEX_PG_DSN]）` 告警行，且**进程仍正常启动**（默认只告警）。

再加 `ARTEX_REQUIRE_ENV_SCRUB=1` 重建一次：

```bash
# 在 override 的 environment 里加 ARTEX_REQUIRE_ENV_SCRUB: "1"
docker compose -p artex up -d
sleep 6
docker logs artex-artex-1 2>&1 | grep -E '\[security\]' | tail -3
```

预期：出现 `log.Fatalf` 的致命日志，且容器进入重启循环（`[artex] 异常退出 (code=1)`）。

验证完恢复正确配置：

```bash
cd "$(git rev-parse --show-toplevel)"
cp /tmp/opencode/ov.good.yml docker-compose.override.yml
docker compose -p artex up -d
for i in $(seq 1 40); do
  code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 3 http://127.0.0.1:8787/ 2>/dev/null)
  [ "$code" = "200" ] && break
  sleep 2
done
echo "恢复后 8787 HTTP=$code"
docker logs artex-artex-1 2>&1 | grep -E '\[security\]' | tail -2
```

- [ ] **Step 8: 确认个人 MCP 与 skill 未受影响**

```bash
docker exec artex-artex-1 ls /app/skills/
docker exec artex-postgres-1 psql -U artex -d artex -t -c "select name||' | '||transport||' | '||coalesce(url,'-') from mcp_servers order by id;"
```

预期：6 个 skill 齐全；5 个 MCP 全部在库、端点不变。

- [ ] **Step 9: 确认工作区干净、无个人内容入库**

```bash
cd "$(git rev-parse --show-toplevel)"
GIT_MASTER=1 git status --porcelain -uall
GIT_MASTER=1 git push --dry-run reArtex local/a2-hardening; echo "钩子退出码=$?"
```

预期：`git status` 为空（`docker-compose.override.yml` 被 exclude 排除，不显示）；钩子退出码 0（放行 —— 钩子按整棵树扫描新分支，是「无个人内容入库」的权威判据）。

> **本 task 不产生 commit。** `docker-compose.override.yml` 由 `.git/info/exclude` 排除，永不进入版本库。

---

## Task 5: 文档同步

**Files:**
- Modify: `CHANGELOG.md`（`## [Unreleased]` 段，当前为空）
- Modify: `README.md`
- Modify: `specs/superpowers/specs/2026-10-26-p0-credential-leak-design.md`（若 §10 的判定需按实测结果微调）

**Interfaces:**
- Consumes: Task 1–4 的实测结果
- Produces: 无代码接口

- [ ] **Step 1: 补 `CHANGELOG.md` 的 `[Unreleased]` 段**

把 `## [Unreleased]` 替换为：

```markdown
## [Unreleased]

### 安全

#### 修复的问题

- **平台凭据不再残留在进程环境块**：`ARTEX_PG_DSN` 原先经容器环境变量下发，
  而环境变量会留在进程的 exec 期环境块里，`/proc/<pid>/environ` 对同 UID 的
  agent 子进程可读 —— 一条 `cat /proc/*/environ` 就能取走数据库口令，进而
  读到 LLM key 与全部会话/流量/漏洞数据。
  新增 `ARTEX_PG_DSN_FILE`：DSN 由一次性文件供给，进程启动读入后立刻清零并
  删除文件；部署侧在 `exec` 前 `unset` 环境变量，使进程环境块里不再有秘密。
- **平台环境块拦截覆盖 glob 与展开形态**：原规则只认 `/proc/self/environ`
  与 `/proc/<数字>/environ` 字面形态，`/proc/*/environ`、`/proc/$$/environ`、
  `/proc/[0-9]*/environ`、`/proc/1/envi*on` 均可绕过。现改为一条覆盖形态的规则。
- **MCP 注册工具由 ask 收紧为 deny**：stdio 传输会以容器 root 拉起任意二进制，
  http/sse 传输则把 env 字段当 HTTP header 回放。新建 MCP 等价于「让平台对任意
  地址带任意 header 发请求」，不应由 agent 在测评中途自行开通。
- **MCP SSE 端点同源校验**：端点由被连接的 MCP 服务端自报，校验 scheme/host/port
  并拒绝带 userinfo 的端点，阻断 Authorization / X-API-Key 向任意主机回放。
- **控制面凭据读入即从进程环境抹除**：`ARTEX_PG_DSN` 与 `ANTHROPIC_API_KEY` /
  `OPENAI_API_KEY` 读入后立即 `os.Unsetenv`，避免子进程 `env` / `printenv` 直接读走。
- **自定义脚本工具改白名单继承宿主环境**：黑名单追不上新增的 `*_TOKEN` 一类变量。
- **skill 信任分档**：记录 skill 的写入来源，agent 写入的 skill 正文降权注入，
  失败一律 fail-closed。

#### 新增

- `ARTEX_PG_DSN_FILE`：DSN 文件来源（读入即清零并删除），优先级低于
  `ARTEX_PG_DSN`、高于配置文件。
- `ARTEX_REQUIRE_ENV_SCRUB`：设为 `1` 时，若启动自检发现平台凭据仍残留在
  `/proc/self/environ`，拒绝启动。默认只告警。
- 启动自检：读取 `/proc/self/environ` 并在日志中给出「未残留 / 仍残留」的明确结论。

### 已知残留

- `/proc/<pid>/mem` 读父进程内存仍可取到 DSN 字符串（需 ptrace 权限）。本批未处理。
- 运行时才成形的路径（如 `find /proc -name environ -exec cat {} \;`）仍能绕过
  字符串预筛。正则只是纵深防御，闭合靠的是凭据不进环境块这一供给侧措施。
- `docker inspect` 仍可见容器配置里的 DSN（需宿主 / docker socket 权限）。
- 平台环境块规则作用在整个工具入参 JSON 上，故「把含 `/proc/…/environ` 字样的
  内容写进文件」也会被拦。
- `llm_profiles.api_key` 仍以明文存于数据库（需数据库读权限才能取到）。
```

- [ ] **Step 2: 补 `README.md` 的 `ARTEX_PG_DSN_FILE` 说明**

在 README 的部署/环境变量章节追加：

````markdown
### 平台凭据不出现在进程环境块

`ARTEX_PG_DSN` 经容器环境变量下发时，会留在进程的 exec 期环境块里，
`/proc/<pid>/environ` 对同 UID 的 agent 子进程可读。若希望凭据不出现在环境块，
改用 `ARTEX_PG_DSN_FILE` 指向一次性文件：

```yaml
services:
  artex:
    environment:
      ARTEX_PG_DSN_FILE: /run/artex/pg_dsn
    entrypoint:
      - /bin/sh
      - -c
      - >-
        umask 077; mkdir -p /run/artex;
        printf '%s' "$$ARTEX_PG_DSN" > /run/artex/pg_dsn;
        unset ARTEX_PG_DSN;
        exec /app/start.sh "$$@"
      - --
```

进程启动后会读入该文件并立即清零、删除它；`exec` 之前的那次 `unset` 是必需的
（`exec` 会用当前环境重建进程映像，只 `unset` 不 `exec` 不会改变环境块）。

启动时平台会自检 `/proc/self/environ` 是否仍残留平台凭据并给出结论。
若希望残留时直接拒绝启动，设 `ARTEX_REQUIRE_ENV_SCRUB=1`。
````

- [ ] **Step 3: 自查文档不含个人环境信息**

```bash
cd "$(git rev-parse --show-toplevel)"
GIT_MASTER=1 git push --dry-run reArtex local/a2-hardening   # 退出码须为 0
```

预期：0 命中（无输出）。

- [ ] **Step 4: 提交**

```bash
cd "$(git rev-parse --show-toplevel)"
GIT_MASTER=1 git add CHANGELOG.md README.md
GIT_MASTER=1 git diff --cached --stat
GIT_MASTER=1 git commit -m "docs: 补 CHANGELOG 残留清单与 DSN 文件源说明"
```

---

## 全局验收（全部 task 完成后）

- [ ] **Step 1: 单元测试全量**

```bash
cd "$(git rev-parse --show-toplevel)"
go build ./... && echo "build ok"
go test ./config/... ./guard/... ./db/... ./agent/... ./mcphttp/... ./pathguard/... ./netguard/...
```

预期：这些包全绿。**`./server/...` 单独跑**：它在基线 `1b29664` 上即因缺 DB 配置而失败（环境性失败），本批**不要求它变绿**，只要求失败集合与基线一致。

- [ ] **Step 2: 与基线对比 server 包失败集合**

```bash
REPO="$(git rev-parse --show-toplevel)"
cd /tmp/opencode && rm -rf baseline-verify && GIT_MASTER=1 git clone -q --no-checkout "$REPO" baseline-verify && cd baseline-verify && GIT_MASTER=1 git checkout -q 1b29664 && go test ./server/ 2>&1 | grep -E '^(--- )?FAIL' | sort > /tmp/opencode/base-fails.txt; wc -l /tmp/opencode/base-fails.txt
cd "$REPO" && go test ./server/ 2>&1 | grep -E '^(--- )?FAIL' | sort > /tmp/opencode/head-fails.txt; wc -l /tmp/opencode/head-fails.txt
diff /tmp/opencode/base-fails.txt /tmp/opencode/head-fails.txt && echo "失败集合与基线一致 ✓"
```

- [ ] **Step 3: 确认 pre-push 钩子放行本分支**

```bash
cd "$(git rev-parse --show-toplevel)"
GIT_MASTER=1 git push --dry-run reArtex local/a2-hardening; echo "退出码=$?"
```

预期：退出码 0（放行）。**不要执行真实 push**，除非 Sir 明确要求。

- [ ] **Step 4: 分支树个人标识终检**

以钩子为权威判据 —— 它按整棵树扫描新分支，规则清单见本机 `.git/hooks/pre-push` 的
`FORBIDDEN_CONTENT`（本地专用、不入库）。

```bash
cd "$(git rev-parse --show-toplevel)"
GIT_MASTER=1 git push --dry-run reArtex local/a2-hardening; echo "钩子退出码=$?"
```

预期：退出码 0（放行）。若非 0，钩子会打印具体命中项与规则名，照它报的改。

> 附带确认：`pathguard/pathguard_test.go` 里的家目录占位夹具（如 `/home/analyst/...`）
> 属于通用占位，钩子已按占位用户名放行，不会误报。

- [ ] **Step 5: 更新审计账本**

把 `reatrex.md`（仓库外的本地文件）中 G-02①-a 的判定更新为：

```
| G-02①-a 平台凭据可读 | worker 与 server 同 UID，可读 /app/jwt.key、原始环境块、MITM CA 私钥 |
| **部分解决（残留收窄）** | 环境继承通道已断：DSN 改由 ARTEX_PG_DSN_FILE 供给、读入即清零并删除，
  部署侧在 exec 前 unset，进程环境块内不再有秘密（实测 /proc/1/environ 无 DSN）；
  guard 侧覆盖 /proc/*/environ 的 glob 与展开形态；启动自检在部署退化时当场告警。
  残留：/proc/<pid>/mem 形态、运行时成形的路径、docker inspect 可见的容器配置面。 |
```

同时在 `reatrex.md` 的计分板里把 G-02①-a 从「残留加重」改为「残留收窄」。

- [ ] **Step 6: 归档**

```bash
# WORKSPACE_OUTPUT 设为你的工作区产出归档目录（本机约定的归档路径）。
WORKSPACE_OUTPUT="${WORKSPACE_OUTPUT:?请先设置工作区产出归档目录}"
REPO="$(git rev-parse --show-toplevel)"
cp "$REPO/specs/superpowers/specs/2026-10-26-p0-credential-leak-design.md" \
   "$WORKSPACE_OUTPUT/artex-p0-credential-leak-design-20261026.md"
cp "$REPO/specs/superpowers/plans/2026-10-26-p0-credential-leak.md" \
   "$WORKSPACE_OUTPUT/artex-p0-credential-leak-plan-20261026.md"
```

---

## 回滚

| 层 | 方式 |
| --- | --- |
| 代码 | `GIT_MASTER=1 git revert <commit>`，或 `GIT_MASTER=1 git checkout main`（回到 `1b29664`） |
| 部署 | 恢复 `docker-compose.override.yml` 的备份（Task 4 Step 7 存了 `/tmp/opencode/ov.good.yml`），移除 `entrypoint` 与 `ARTEX_PG_DSN_FILE` → 回到 env 供给 |
| 镜像 | `docker build` 重建回上一个 `artex:local`（或 `docker tag` 指回旧镜像 ID） |
| 数据 | 本批不重建 pg 卷，`findings`/`assets`/`conversations` 不受影响 |

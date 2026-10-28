# W-A2 入参路径解析与端到端验证 · 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让「工具入参里出现的路径」不再只靠字符串预筛，而是**按工作目录解析成真实路径**再判定，从而堵住 `cat $(realpath ../../../jwt.key)` 这类经 shell 展开才现形的绕过；并用一条真实链路的回归测试把这件事钉死。

**Architecture:** 在 `pathguard` 包新增 `CheckInputPaths(toolName, input, workingDir)`：把入参 JSON 里的所有字符串值抽出「路径样片段」，相对 `workingDir`（= 工具进程的工作目录，由 norma 的 `permission.Context.WorkingDir` 提供）解析成真实路径，再复用现有的 `secretRoots` / `procSecrets` 判定。调用点仍是 `agent/toolpolicy.go` 的 `policyTool.CheckPermissions`——`Deny` 落在权限判定的 Stage 1，任何 permission mode 都绕不过。

**Tech Stack:** Go 1.26、`github.com/Autumn-27/norma v0.4.4`（只读不改）、PostgreSQL 16（仅端到端用例需要）

## Global Constraints

- 模块路径固定为 `github.com/Autumn-27/artex` —— **不得修改**。
- 所有 git 命令必须带 `GIT_MASTER=1` 前缀。
- **禁止 `git reset --hard`**；**禁止用配置里的活库 `artex` 跑测试**。
- 注释、commit message、文档一律中文。
- **不引入新的第三方依赖**（路径解析只用标准库 `path/filepath`、`regexp`、`strings`）。
- **安全清单（`secretRoots`、`procSecrets`）保持为包级 Go 常量**，不得改成可热改数据源；`pathguard` 的既有取舍不变：**不做根目录收敛**（agent 工作区在 `/app/data/sessions/<key>`，渗透目标在别处，收敛会打断「把证据读到本地再分析」）。
- 判定失败时**按拒绝处理**（沿用现有 `Check` 的取舍：解析不出真实路径时不猜）。
- 每一处「闭合」都要同步更新账本 `reartex.md` 并归档。
- 推送前自检：不得出现本机路径、个人 MCP 名、凭据、主机指纹。

---

## File Structure

| 文件 | 职责 | 本计划中的动作 |
| --- | --- | --- |
| `pathguard/pathguard.go` | 秘密清单、路径类工具的判定、真实路径解析 | 抽出 `resolveExistingAt(p, base)`，保留 `resolveExisting(p)` 为其特例（进程 CWD） |
| `pathguard/tokens.go` | 入参路径片段的提取与解析（**新建**） | `CheckInputPaths` + 片段提取器 |
| `pathguard/tokens_test.go` | 入参路径解析的回归用例（**新建**） | 30+ 形态 |
| `agent/toolpolicy.go` | 工具执行前的平台策略装饰器 | 在 `CheckPermissions` 里加调 `pathguard.CheckInputPaths` |
| `agent/toolpolicy_test.go` | 策略装饰器回归（已存在） | 补一条：相对路径命中 → Denied |
| `guard/guard_e2e_test.go` | 端到端：hook → 策略 → 拒绝 → 审计链（**新建**） | 断言 Bash 面绕过被拒、审计链完整 |

---

### Task 1: 路径片段提取器与相对路径解析

**Files:**
- Create: `pathguard/tokens.go`
- Modify: `pathguard/pathguard.go`（抽出 `resolveExistingAt`）
- Create: `pathguard/tokens_test.go`

**Interfaces:**
- Consumes: `pathguard.secretRoots`（包级 `[]string`）、`pathguard.procSecrets`（包级 `[]string`）、`pathguard.denyReason(real string) error`（包级私有）
- Produces:
  - `func CheckInputPaths(toolName string, input []byte, workingDir string) error` —— 入参里任一路径片段解析后命中平台秘密即返回错误
  - `func resolveExistingAt(p, base string) (string, error)` —— 以 `base` 为基准解析真实路径；`base` 为空时用进程 CWD

- [ ] **Step 1: 写失败测试**

新建 `pathguard/tokens_test.go`：

```go
package pathguard

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// 入参里的相对路径必须按工具工作目录解析后判定 —— 字符串预筛挡不住
// `cat ../../../jwt.key` 这类经 shell 展开才现形的路径。
func TestCheckInputPathsResolvesRelativeToWorkingDir(t *testing.T) {
	work := t.TempDir()
	// 工作目录在 <root>/sessions/<key>，三层 .. 回到 <root>
	root := filepath.Dir(filepath.Dir(work))
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(root, "jwt.key")
	if err := os.WriteFile(secret, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	withSecretRoots(t, secret)

	cases := []struct {
		desc string
		in   string
	}{
		{"相对回溯", `{"command":"cat ../../../jwt.key"}`},
		{"命令替换包裹", `{"command":"cat $(realpath ../../../jwt.key)"}`},
		{"变量前缀拼接", `{"command":"cat $HOME/../../../jwt.key"}`},
		{"绝对路径", `{"command":"cat ` + secret + `"}`},
		{"软链绕行", `{"command":"ln -s ` + secret + ` /tmp/x; cat /tmp/x"}`},
		{"proc environ", `{"command":"cat /proc/1/environ"}`},
	}
	for _, c := range cases {
		if err := CheckInputPaths("Bash", json.RawMessage(c.in), work); err == nil {
			t.Fatalf("%s：入参 %s 应被拒绝", c.desc, c.in)
		}
	}
}

// 正常路径不得误伤：这是渗透 agent 的日常操作，一刀切会让平台不可用。
func TestCheckInputPathsAllowsNormalPaths(t *testing.T) {
	work := t.TempDir()
	withSecretRoots(t, filepath.Join(work, "jwt.key"))
	ok := []string{
		`{"command":"ls -la"}`,
		`{"command":"cat ./notes.md"}`,
		`{"command":"curl -s http://127.0.0.1:8787/api/health"}`,
		`{"command":"cat /etc/hosts"}`,
		`{"command":"find . -name '*.txt'"}`,
		`{"file_path":"/tmp/output.txt"}`,
	}
	for _, in := range ok {
		if err := CheckInputPaths("Bash", json.RawMessage(in), work); err != nil {
			t.Fatalf("入参 %s 不应被拒绝：%v", in, err)
		}
	}
}

// URL 里的路径片段不是文件路径：/app/jwt.key 出现在 http URL 中不得触发误伤。
func TestCheckInputPathsIgnoresURLPathSegments(t *testing.T) {
	work := t.TempDir()
	withSecretRoots(t, filepath.Join(work, "jwt.key"))
	in := `{"command":"curl -s http://example.com/app/jwt.key"}`
	if err := CheckInputPaths("Bash", json.RawMessage(in), work); err != nil {
		t.Fatalf("URL 内的路径片段不应触发拒绝：%v", err)
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./pathguard/ -run 'TestCheckInputPaths' -v`
Expected: FAIL —— `CheckInputPaths` 尚不存在（编译错误）。

- [ ] **Step 3: 抽出以基准目录解析的真实路径**

`pathguard/pathguard.go`：把 `resolveExisting` 改成薄封装，新增 `resolveExistingAt`：

```go
// resolveExisting 返回 p 的真实路径（以进程 CWD 为基准）。
func resolveExisting(p string) (string, error) {
	return resolveExistingAt(p, "")
}

// resolveExistingAt 返回 p 相对 base 解析出的真实路径。base 为空时用进程 CWD。
//
// 必须能指定基准：工具的工作目录不是进程 CWD，而入参里的相对路径是相对
// 工作目录解释的（`cat ../../../jwt.key` 里的 `..` 层数就是按工作目录算的）。
func resolveExistingAt(p, base string) (string, error) {
	if strings.HasPrefix(p, "~") {
		if home, err := os.UserHomeDir(); err == nil {
			p = filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	if !filepath.IsAbs(p) && base != "" {
		p = filepath.Join(base, p)
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
```

- [ ] **Step 4: 实现片段提取与判定**

新建 `pathguard/tokens.go`：

```go
package pathguard

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// 入参路径片段提取。
//
// 为什么要有这一层：短路表是对入参 JSON 的【字符串预筛】，挡不住经 shell
// 展开后才现形的路径（`cat $(realpath ../../../jwt.key)`）。本文件把入参里
// 的「路径样片段」挑出来，按工具工作目录解析成真实路径，再走秘密清单判定。
//
// 提取策略：逐个字符串值 → 去掉 URL 形态（scheme://…，其中的 / 不是文件路径）
// → 按 shell 分隔符切成 token → 每个 token 再切出「以 / 或 ../ 或 ~/ 开头」的
// 后缀作为候选路径。之所以按后缀切，是因为 shell 变量与命令替换会把路径
// 粘在别的东西前面（`$HOME/../../jwt.key`、`$(realpath ../../../jwt.key)`）。
var urlLikeRe = regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.-]*://[^\s"'` + "`" + `]+`)

// tokenSplitter 是切 token 时要断开的字符：空白与 shell 元字符。
const tokenSplitter = " \t\r\n\"'`;|&<>(){}[],$"

// pathStart 判断 token 从该位置起是否像一个路径起点。
func pathStart(s string, i int) bool {
	if i >= len(s) {
		return false
	}
	if s[i] == '/' || s[i] == '~' {
		return true
	}
	// `.` 或 `..` 开头（后面必须跟 / 或就是结尾）
	if s[i] == '.' {
		j := i
		for j < len(s) && s[j] == '.' {
			j++
		}
		return j-i <= 2 && (j == len(s) || s[j] == '/')
	}
	return false
}

// pathCandidates 从一个 token 里切出所有路径样候选（按出现顺序）。
func pathCandidates(tok string) []string {
	var out []string
	for i := 0; i < len(tok); i++ {
		if !pathStart(tok, i) {
			continue
		}
		cand := tok[i:]
		// 去掉尾部标点：命令替换的右括号、引号、句号等都不属于路径
		cand = strings.TrimRight(cand, ".,;:!?)]}'\"")
		if cand == "" {
			continue
		}
		out = append(out, cand)
	}
	return out
}

// stringValues 递归取出 JSON 里的所有字符串值。
func stringValues(v any, out *[]string) {
	switch t := v.(type) {
	case string:
		*out = append(*out, t)
	case []any:
		for _, e := range t {
			stringValues(e, out)
		}
	case map[string]any:
		for _, e := range t {
			stringValues(e, out)
		}
	}
}

// CheckInputPaths 判定工具入参里是否携带指向平台秘密的路径。
//
// workingDir 是工具进程的工作目录（norma 的 permission.Context.WorkingDir）：
// 入参里的相对路径是相对它解释的。workingDir 为空时用进程 CWD。
//
// 判定失败（解析不出真实路径）时按拒绝处理，理由与 Check 一致：解析不出
// 就不猜，猜错的方向是放行。
func CheckInputPaths(toolName string, input []byte, workingDir string) error {
	var v any
	if err := json.Unmarshal(input, &v); err != nil {
		return nil // 解析不了交给工具自己报错，不因格式问题误伤
	}
	var strs []string
	stringValues(v, &strs)

	for _, s := range strs {
		s = urlLikeRe.ReplaceAllString(s, " ")
		for _, tok := range strings.FieldsFunc(s, func(r rune) bool {
			return strings.ContainsRune(tokenSplitter, r)
		}) {
			for _, cand := range pathCandidates(tok) {
				real, err := resolveExistingAt(cand, workingDir)
				if err != nil {
					return fmt.Errorf("拒绝 %s 的路径 %q: 无法解析真实路径: %w", toolName, cand, err)
				}
				if err := denyReason(real); err != nil {
					return fmt.Errorf("拒绝 %s 的路径 %q: %w", toolName, cand, err)
				}
			}
		}
	}
	return nil
}
```

- [ ] **Step 5: 运行测试确认通过**

Run: `go test ./pathguard/ -run 'TestCheckInputPaths' -v`
Expected: PASS（三个用例都过）。

- [ ] **Step 6: 既有 pathguard 测试不得回归**

Run: `go test ./pathguard/ -count=1`
Expected: PASS。

- [ ] **Step 7: 提交**

```bash
GIT_MASTER=1 git add pathguard/tokens.go pathguard/tokens_test.go pathguard/pathguard.go
GIT_MASTER=1 git commit -m "feat(pathguard): 入参路径片段按工作目录解析，堵住 shell 展开后才现形的绕过"
```

---

### Task 2: 接入工具策略装饰器

**Files:**
- Modify: `agent/toolpolicy.go`
- Modify: `agent/toolpolicy_test.go`

**Interfaces:**
- Consumes: `pathguard.CheckInputPaths(toolName string, input []byte, workingDir string) error`（Task 1 产出）、`acperm.Context.WorkingDir`（norma 既有）
- Produces: `policyTool.CheckPermissions` 在既有 `pathguard.Check` 之后追加入参路径判定

- [ ] **Step 1: 写失败测试**

在 `agent/toolpolicy_test.go` 追加：

```go
// 入参里的相对路径绕过必须被拒：短路表是字符串预筛，挡不住经 shell 展开
// 才现形的路径；这一层用真实路径解析兜住。
func TestPolicyToolDeniesRelativeSecretPath(t *testing.T) {
	work := t.TempDir()
	root := filepath.Dir(filepath.Dir(work))
	secret := filepath.Join(root, "jwt.key")
	if err := os.WriteFile(secret, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// pathguard 的秘密清单是包级变量，包外无法替换；故用真实的 /app 前缀形态
	// 验证判定链路（容器里 /app/jwt.key 存在；测试环境下不存在也仍应被拦，
	// 因为判定发生在解析出的路径上，而非文件是否存在）。
	raw := json.RawMessage(`{"command":"cat ../../../jwt.key"}`)
	pc := acperm.Context{Mode: acperm.ModeBypass, WorkingDir: work}
	tool := enforceToolPolicy(fakeTool{name: "Bash"})
	if tool.CheckPermissions(context.Background(), raw, pc) == nil {
		t.Fatal("相对路径指向平台密钥时应被拒")
	}
}
```

（`fakeTool` 是本文件内的最小 `actool.CoreTool` 桩（本文件内实现）：

```go
// fakeTool 是最小的 CoreTool 桩：只提供名字与恒定放行的权限判定。
type fakeTool struct{ name string }

func (f fakeTool) Name() string { return f.name }

func (f fakeTool) CheckPermissions(context.Context, json.RawMessage, acperm.Context) acperm.Decision {
	return acperm.Allowed()
}

func (f fakeTool) Call(context.Context, json.RawMessage, *actool.ToolContext) (any, error) {
	return nil, nil
}
```

`actool` 是 `github.com/Autumn-27/norma/tool`，`acperm` 是 `github.com/Autumn-27/norma/permission`。）

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./agent/ -run TestPolicyToolDeniesRelativeSecretPath -v`
Expected: FAIL —— 尚未调用 `CheckInputPaths`，相对路径未被拒。

- [ ] **Step 3: 接入装饰器**

`agent/toolpolicy.go` 的 `CheckPermissions` 改为：

```go
func (p *policyTool) CheckPermissions(ctx context.Context, input json.RawMessage, pc acperm.Context) acperm.Decision {
	if err := pathguard.Check(p.Name(), input); err != nil {
		return acperm.Denied(err.Error())
	}
	// 入参路径片段：相对工具工作目录解析真实路径后判定。短路表是字符串预筛，
	// 挡不住 `cat $(realpath ../../../jwt.key)` 这类经 shell 展开才现形的路径。
	if err := pathguard.CheckInputPaths(p.Name(), input, pc.WorkingDir); err != nil {
		return acperm.Denied(err.Error())
	}
	return p.CoreTool.CheckPermissions(ctx, input, pc)
}
```

- [ ] **Step 4: 运行测试确认通过**

Run: `go test ./agent/ -run 'TestPolicyTool' -count=1 -v`
Expected: PASS。

- [ ] **Step 5: 全包测试 + 提交**

```bash
go test ./agent/ ./pathguard/ -count=1
GIT_MASTER=1 git add agent/toolpolicy.go agent/toolpolicy_test.go
GIT_MASTER=1 git commit -m "feat(agent): 工具策略装饰器接入入参路径判定"
```

---

### Task 3: 端到端回归（hook → 策略 → 拒绝 → 审计链）

**Files:**
- Create: `guard/guard_e2e_test.go`

**Interfaces:**
- Consumes: `guard.NewWithInterceptor(ic)`、`hook.Event{ToolName, Input}`、`intercept.New(db)`、`(*DB).VerifyAuditChain()`
- Produces: 一条钉死「Bash 面绕过被拒 + 留痕 + 链完整」的回归用例

- [ ] **Step 1: 准备测试库基座与辅助函数**

若 `guard/testmain_test.go` 不存在，按 W-A1 Task 2 的内容新建（包名 `guard`，`testdb.Setup("guard")`）；已存在则直接复用。

`openTestDB` 与 `hookEvent` 两个辅助函数在本文件内实现，形态与 W-A1 的 guard 包一致：

```go
// openTestDB 打开本包的派生测试库；连不上就跳过需要库的用例。
func openTestDB(t *testing.T) *db.DB {
	t.Helper()
	dsn := os.Getenv("ARTEX_PG_DSN")
	if dsn == "" {
		t.Skip("无测试库（ARTEX_PG_DSN 未设置）")
	}
	conn, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Skipf("打开测试库失败: %v", err)
	}
	if err := conn.Ping(); err != nil {
		t.Skipf("测试库不可用: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return db.NewWithConn(conn)
}

// hookEvent 构造一次工具调用事件。
func hookEvent(tool string, in json.RawMessage) hook.Event {
	return hook.Event{ToolName: tool, Input: in}
}
```

`db.NewWithConn` 若不存在，用 `db` 包里既有的「用现成 *sql.DB 构造 DB」的入口；落地时先 `grep -n "func New" db/db.go` 确认。）

- [ ] **Step 2: 写端到端用例**

新建 `guard/guard_e2e_test.go`：

```go
package guard

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Autumn-27/artex/intercept"
	"github.com/Autumn-27/norma/hook"
)

// 端到端：真实链路（PreToolUse hook → 工具策略 → 拒绝 → 审计链）。
//
// 单测覆盖了函数逻辑，但「链路里是否真的生效」一直没验过 —— 这条用例
// 把 hook 求值到审计留痕整条链走一遍。
func TestBashRelativeSecretPathIsBlockedEndToEnd(t *testing.T) {
	d := openTestDB(t)
	ic := intercept.New(d)
	g := NewWithInterceptor(ic)

	ev := hook.Event{
		ToolName: "Bash",
		Input:    json.RawMessage(`{"command":"cat ../../../jwt.key"}`),
	}
	res := g.applyIntercept(context.Background(), ev)
	if res.Decision != "block" {
		t.Fatalf("相对路径指向平台密钥应被拦，得到 %+v", res)
	}

	// 留痕：拦截记录 + 审计链
	var n int
	if err := d.QueryRow(`SELECT COUNT(*) FROM intercept_pending WHERE tool_name='Bash' AND status='denied'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatal("拦截未落 intercept_pending")
	}
	if broken, err := d.VerifyAuditChain(); err != nil {
		t.Fatalf("审计链断裂在 seq=%d: %v", broken, err)
	} else if broken != 0 {
		t.Fatalf("审计链断裂在 seq=%d", broken)
	}
}
```

- [ ] **Step 3: 运行测试确认通过**

Run: `go test ./guard/ -run TestBashRelativeSecretPathIsBlockedEndToEnd -v`
Expected: PASS。

- [ ] **Step 4: 确认这条用例真的会因为绕过而失败（反证）**

临时把 `pathguard.CheckInputPaths` 的调用注释掉（或把 `agent/toolpolicy.go` 的新判定短路），重跑：

Run: `go test ./guard/ -run TestBashRelativeSecretPathIsBlockedEndToEnd -v`
Expected: FAIL（不被拦）——证明这条用例有鉴别力，不是恒过的空断言。

改回代码，重跑一次确认 PASS。

- [ ] **Step 5: 提交**

```bash
GIT_MASTER=1 git add guard/guard_e2e_test.go
GIT_MASTER=1 git commit -m "test(guard): Bash 面相对路径绕过的端到端回归（拦截+留痕+链完整）"
```

---

### Task 4: 账本口径同步

**Files:**
- Modify: 账本本体（本地工作区内，不在版本库）
- Modify: 账本归档副本

**Interfaces:**
- Produces: 账本里「短路表是入参字符串预筛，不是路径解析级强制」这条残留的闭合记录 + 诚实的剩余边界说明

- [ ] **Step 1: 改写该条残留**

把「短路表是入参字符串预筛，不是路径解析级强制。`cat $(realpath ../../../jwt.key)` 这类经 shell 展开后才现形的路径仍可绕过。真正闭合需要执行前的 realpath 校验与环境变量白名单。」改为：

```
短路表原先是入参字符串预筛，`cat $(realpath ../../../jwt.key)` 这类经 shell 展开后才现形的
路径绕得过。**已闭合（2026-10-28）**：新增 pathguard.CheckInputPaths，把入参里的路径样
片段按工具工作目录（norma permission.Context.WorkingDir）解析成真实路径后再判秘密清单，
判定点仍在工具策略装饰器（Stage 1 Deny，任何 permission mode 绕不过）。
**剩余边界（不得宣称闭合）**：运行时才成形的路径（编码/拼接/下载内容决定）仍在射程外；
容器内 root/特权进程的任何读取也不在本层覆盖内。真正的硬边界要等 per-task 隔离。
```

- [ ] **Step 2: 补「运行时端到端验证未做」这条的闭合记录**

```
短路表的运行时端到端验证原先未做（单测覆盖了函数逻辑，但没在真实链路里确认过）。
**已闭合（2026-10-28）**：新增 guard 端到端回归（hook 求值 → 策略 → 拦截 → intercept_pending
留痕 → 审计链完整性校验），并做过反证（撤掉判定后该用例确实变红）。
```

- [ ] **Step 3: 归档并核对**

复制到归档路径，`cmp` 校验两份一致。

---

## 验收（整个计划完成后）

```bash
go test ./pathguard/ ./agent/ ./guard/ -count=1
go test ./... -timeout 30m -count=1
```

**本计划不覆盖**：per-task 隔离（硬边界）、环境块不变式门、容器权限收紧、密钥加密。

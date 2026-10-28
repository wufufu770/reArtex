# W-A1 判定层三档化与审批治理 · 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 把平台工具的审批从「一律 ask」改成 **deny / ask / 显式 allow 三档**，并给「反复请求审批」的 agent 加防疲劳计数与默认超时，使低危日常操作零摩擦、高危面仍有人工把关。

**Architecture:** 短路表（`guard/shortcircuit.go`，代码常量）扩成三张表：`platformDenyTools`（不变）、`platformAskTools`（去掉 `create_skill`）、`platformAllowTools`（新增，只含 `create_skill`）。三张表**都在 `applyIntercept` 最前求值**——「显式 allow」是主动判定放行，不是「不返回 Decision」的漏判，因此不会重新打开 G-01① 那个「平台工具绕白名单直接放行」的洞。审批防疲劳计数落在 `intercept` 包（`AskBurstExceeded`），数据源是 `intercept_pending` 表（进程重启不丢）。DB 规则的 ask 类补默认超时（60s → deny），使「审批无限等待」窗口关闭。

**Tech Stack:** Go 1.26、PostgreSQL 16（pgx/v5）、`github.com/Autumn-27/norma v0.4.4`（外部 module，只读不改）

## Global Constraints

- 模块路径固定为 `github.com/Autumn-27/artex` —— **不得修改**（改了就编不过，且与上游 fork 关系断掉）。
- 所有 git 命令必须带 `GIT_MASTER=1` 前缀。
- **禁止 `git reset --hard`**；**禁止用配置里的活库 `artex` 跑测试**（一律用 `internal/testdb` 派生库）。
- 新增测试库接入方式：照抄 `db/testmain_test.go` 的形状（`testdb.Setup("<pkg>")` → `os.Setenv("ARTEX_PG_DSN", dsn)` → `code := runSuite(m, dsn); release(); os.Exit(code)`；`os.Exit` 不执行 defer，收尾必须显式做完）。
- 注释、commit message、文档一律中文，与既有代码风格一致。
- **不引入新的第三方依赖**（加密/密钥相关的依赖留给 W-A4 单独决策）。
- 安全边界（deny/ask 档、秘密清单、默认超时）**必须是代码常量**，不得挂到 DB/settings 热改面；只有**可用性参数**（计数阈值、窗口长度）可热改。
- CI 门不得带 `-skip` / 排除项。
- 每一处「闭合」都要同步更新账本 `reartex.md`（账本与归档副本都在本地工作区，不在版本库），口径与实现必须一致。
- 推送前自检：不得出现本机路径（`/home/<用户名>/...`、`Desktop/...`）、个人 MCP 名、凭据、主机指纹（pre-push 钩子会拦，但不要依赖它兜底）。

---

## File Structure

| 文件 | 职责 | 本计划中的动作 |
| --- | --- | --- |
| `guard/shortcircuit.go` | 代码常量短路表：工具名 → 判定档位 | 新增 `platformAllowTools`；`platformAskTools` 去掉 `create_skill`；`shortCircuit` 命中 allow 档时返回 `Action:"allow"` 的 Decision |
| `guard/guard.go` | 短路表判定结果的落地：ask→人工、deny→拦截 | 新增 `allow` 分支（落库 + 审计链 + 放行）；新增「审批请求超阈值 → 直接拒绝」分支 |
| `intercept/intercept.go` | 判定动作的求值、审批、审计链 | 新增 `AgentNameFromContext`；新增 `AskBurstExceeded`；`Match` 对 ask 类规则补默认超时；导出 `AppendDecisionEvent` |
| `db/intercept.go` | `intercept_rules` / `intercept_pending` 的读写 | 新增 `CountRecentInterceptAsks` |
| `guard/shortcircuit_test.go` | 短路表分档回归 | 改写：原「5 件全 ask」断言拆成 deny/ask/allow 三档 |
| `guard/tiers_test.go` | 三档判定与求值顺序回归 | 新建 |
| `guard/testmain_test.go` | 本包独立测试库 | 新建（照抄 `db/testmain_test.go`） |
| `guard/audit_tier_test.go` | allow/deny 档的审计留痕回归（需 DB） | 新建 |
| `intercept/asktimeout_test.go` | DB 规则 ask 默认超时回归 | 新建 |
| `intercept/askburst_test.go` | 审批防疲劳计数回归（需 DB） | 新建 |
| `intercept/testmain_test.go` | 本包独立测试库 | 新建（照抄 `db/testmain_test.go`） |

---

### Task 1: 短路表三档化

**Files:**
- Modify: `guard/shortcircuit.go`
- Modify: `guard/guard.go`
- Modify: `guard/shortcircuit_test.go`

**Interfaces:**
- Consumes: `intercept.Decision{Action, RuleName, Message, TimeoutEnabled, TimeoutSeconds, TimeoutAction}`（已有，字段名不变）
- Produces:
  - `guard.platformAllowTools map[string]bool`（包级常量表，仅 `create_skill`）
  - `shortCircuit(toolName string, input []byte) *intercept.Decision` 的返回语义扩展：`Action` 现在可能是 `"allow"`
  - `guard.Guard.applyIntercept` 认识 `Action == "allow"`

- [ ] **Step 1: 写失败测试（改写既有断言为三档）**

`guard/shortcircuit_test.go` 里把 `TestShortCircuitPlatformToolsAsk` 拆成两个：

```go
func TestShortCircuitPlatformToolsAsk(t *testing.T) {
	for _, tool := range []string{
		"create_custom_tool", "update_custom_tool",
		"update_skill_file", "delete_assets_by_host",
	} {
		dec := shortCircuit(tool, []byte(`{"command":"http://127.0.0.1"}`))
		if dec == nil {
			t.Fatalf("%s 应命中短路表", tool)
		}
		if dec.Action != "ask" {
			t.Fatalf("%s 应为 ask，得到 %s", tool, dec.Action)
		}
		if !dec.TimeoutEnabled || dec.TimeoutSeconds != 60 || dec.TimeoutAction != "deny" {
			t.Fatalf("%s 的 ask 超时须为 60s/deny，得到 %+v", tool, dec)
		}
	}
}

// 低危面（新建技能文件）不再打断日常操作：短路表【显式判定放行】，
// 而不是「不命中短路表」——后者等于漏判，会重开 G-01① 的洞。
func TestShortCircuitLowRiskToolExplicitAllow(t *testing.T) {
	dec := shortCircuit("create_skill", []byte(`{"name":"x"}`))
	if dec == nil {
		t.Fatal("create_skill 应命中短路表（显式 allow），不应落回漏判")
	}
	if dec.Action != "allow" {
		t.Fatalf("create_skill 应为 allow，得到 %s", dec.Action)
	}
	if dec.RuleName == "" {
		t.Fatal("allow 档也必须带 RuleName：审计链靠它区分来源")
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./guard/ -run 'TestShortCircuit' -v`
Expected: FAIL —— `create_skill` 当前命中 `platformAskTools`，`dec.Action` 是 `"ask"` 而非 `"allow"`。

- [ ] **Step 3: 短路表加 allow 档**

`guard/shortcircuit.go`：把 `create_skill` 从 `platformAskTools` 移出，新增

```go
// platformAllowTools 是低危面：代码常量【显式判定放行】。
//
// 与「不命中短路表」的区别是本表存在的全部意义：不命中 = 漏判，平台工具
// 就能绕开判官（G-01① 原漏洞）；显式 allow = 平台主动判了「这件事无害」，
// 判定动作照样落审计链。
//
// 收录标准：① 不覆盖既有行为（新建不同于改写）；② 其后果由动作层护栏兜住
// （护栏拦行为不拦内容，故写进 skill 的内容无法绕过工具层策略）。
var platformAllowTools = map[string]bool{
	"create_skill": true,
}
```

并在 `shortCircuit()` 里，`platformAskTools` 判定**之后**、凭据正则判定**之前**插入：

```go
	// 低危面：显式放行（仍是代码常量判定，不是漏判）
	if platformAllowTools[toolName] {
		return &intercept.Decision{
			Action:  "allow",
			RuleName: "[代码常量] 平台低危工具放行",
			Message:  "新建技能文件不改变既有行为，按平台策略放行。",
		}
	}
```

- [ ] **Step 4: guard 落地 allow 分支**

`guard/guard.go` 的 `applyIntercept` 里，短路表分支改为三分支（把现在的 `if dec.Action == "ask" { ... }` 改成 `switch`）：

```go
	if dec := shortCircuit(ev.ToolName, ev.Input); dec != nil {
		switch dec.Action {
		case "allow":
			// 显式放行也要留痕：平台工具调用量小，进审计链无压力；
			// 事后要能回答「谁在何时建过什么」。
			if g.interceptor != nil {
				logCtx := intercept.WithCall(ctx, ev.ToolName, ev.Input)
				g.interceptor.Log(logCtx, intercept.ConvIDFromContext(logCtx), *dec, ev.ToolName, ev.Input, "allowed")
			}
			g.record(ev.ToolName, "allow", dec.Message, "")
			return hook.Result{}
		case "ask":
			if g.interceptor == nil {
				return g.block(ev.ToolName, systemBlockMessage("人工审批通道不可用（"+dec.Message+"）"), "")
			}
			ctx = intercept.WithCall(ctx, ev.ToolName, ev.Input)
			if !g.interceptor.HandleAsk(ctx, intercept.ConvIDFromContext(ctx), *dec, ev.ToolName, ev.Input) {
				return g.block(ev.ToolName, systemBlockMessage("人工审批未通过（用户拒绝或审批超时）"), "")
			}
			return hook.Result{}
		default:
			// deny：短路表的 deny 也要落库。此前它只进内存 audit 切片
			//（上限 2000、进程退出即失），于是「谁在何时想干什么」查无实据。
			if g.interceptor != nil {
				logCtx := intercept.WithCall(ctx, ev.ToolName, ev.Input)
				g.interceptor.Log(logCtx, intercept.ConvIDFromContext(logCtx), *dec, ev.ToolName, ev.Input, "denied")
			}
			g.record(ev.ToolName, "block", dec.Message, "")
			return g.block(ev.ToolName, systemBlockMessage(dec.Message), "")
		}
	}
```

- [ ] **Step 5: 运行测试确认通过**

Run: `go test ./guard/ -run 'TestShortCircuit' -v`
Expected: PASS（`TestShortCircuitPlatformToolsAsk` 4 件全过、`TestShortCircuitLowRiskToolExplicitAllow` 过、`TestShortCircuitMCPDeny` 与 `TestShortCircuitPlatformCredentialsDeny` 不受影响）。

- [ ] **Step 6: 跑全包测试确认没打破别的**

Run: `go test ./guard/ -count=1`
Expected: PASS，无 skip。

- [ ] **Step 7: 提交**

```bash
GIT_MASTER=1 git add guard/shortcircuit.go guard/guard.go guard/shortcircuit_test.go
GIT_MASTER=1 git commit -m "feat(guard): 短路表三档化，低危面显式放行不再打断日常操作"
```

---

### Task 2: allow 档的审计留痕（进哈希链）

**Files:**
- Modify: `intercept/intercept.go`（导出 `AppendDecisionEvent`）
- Modify: `guard/guard.go`（allow 分支追加链事件）
- Create: `guard/testmain_test.go`
- Create: `guard/audit_tier_test.go`

**Interfaces:**
- Consumes: `(*Interceptor).Log(ctx, convID, dec, toolName, input, status)`（已有）、`(*DB).AppendAuditEvent(ev db.AuditEvent) (string, error)`（已有）
- Produces: `func (i *Interceptor) AppendDecisionEvent(ctx context.Context, dec Decision, toolName string, input []byte, action string)` —— 把一条判定动作追加进审计链

- [ ] **Step 1: 写失败测试（需要 DB）**

新建 `guard/testmain_test.go`（照抄 `db/testmain_test.go` 的形状，包名改 `guard`，包名参数传 `"guard"`）：

```go
package guard

import (
	"database/sql"
	"os"
	"testing"

	"github.com/Autumn-27/artex/internal/testdb"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// TestMain 给本包准备一个独立的测试库（绝不碰配置里的活库）。
func TestMain(m *testing.M) {
	dsn, release, err := testdb.Setup("guard")
	if err != nil {
		os.Exit(m.Run())
	}
	_ = os.Setenv("ARTEX_PG_DSN", dsn)

	code := runSuite(m, dsn)
	release()
	os.Exit(code)
}

func runSuite(m *testing.M, dsn string) int {
	conn, err := sql.Open("pgx", dsn)
	if err != nil || conn.Ping() != nil {
		return m.Run()
	}
	defer conn.Close()
	return m.Run()
}
```

新建 `guard/audit_tier_test.go`：

```go
package guard

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Autumn-27/artex/intercept"
)

// allow 档必须留痕：平台工具调用量小，进审计链无压力；
// 没有留痕就回答不了「谁在何时建过什么」。
func TestAllowTierIsAudited(t *testing.T) {
	d := openTestDB(t)
	ic := intercept.New(d)
	g := NewWithInterceptor(ic)

	ev := hookEvent("create_skill", json.RawMessage(`{"name":"x"}`))
	res := g.applyIntercept(context.Background(), ev)
	if res.Decision == "block" {
		t.Fatalf("create_skill 不应被拦，得到 %+v", res)
	}

	rows, err := d.Query(`SELECT action, tool_name FROM audit_events WHERE tool_name='create_skill' ORDER BY seq DESC LIMIT 1`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatal("allow 档未落审计链")
	}
	var action, tool string
	if err := rows.Scan(&action, &tool); err != nil {
		t.Fatal(err)
	}
	if action != "allow" || tool != "create_skill" {
		t.Fatalf("审计行不对：action=%q tool=%q", action, tool)
	}
}
```

辅助函数（本文件内实现，勿留空）：

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

（`db.NewWithConn` 若不存在，用 `db` 包里既有的「用现成 *sql.DB 构造 DB」的入口；该入口名以 `db/db.go` 实际为准，落地时先 `grep -n "func New" db/db.go` 确认。`hook` 包是 `github.com/Autumn-27/norma/hook`。）

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./guard/ -run TestAllowTierIsAudited -v`
Expected: FAIL —— `applyIntercept` 的 allow 分支只调 `Log`（进 `intercept_pending`），不进 `audit_events` 链。

- [ ] **Step 3: 导出链写入方法**

`intercept/intercept.go` 增加：

```go
// AppendDecisionEvent 把一条判定动作追加进审计链。
//
// 与 Log 的区别：Log 只在 status=="denied" 时进链（普通 allow 量太大），
// 平台工具的 allow 量小、需要可追责，故单独暴露这个方法。
func (i *Interceptor) AppendDecisionEvent(ctx context.Context, dec Decision, toolName string, input []byte, action string) {
	i.appendAuditEvent(ctx, dec, toolName, input, action)
}
```

- [ ] **Step 4: allow 分支同时进链**

`guard/guard.go` 的 allow 分支改为：

```go
		case "allow":
			if g.interceptor != nil {
				logCtx := intercept.WithCall(ctx, ev.ToolName, ev.Input)
				g.interceptor.Log(logCtx, intercept.ConvIDFromContext(logCtx), *dec, ev.ToolName, ev.Input, "allowed")
				g.interceptor.AppendDecisionEvent(logCtx, *dec, ev.ToolName, ev.Input, "allow")
			}
			g.record(ev.ToolName, "allow", dec.Message, "")
			return hook.Result{}
```

- [ ] **Step 5: 运行测试确认通过**

Run: `go test ./guard/ -run TestAllowTierIsAudited -v`
Expected: PASS。

- [ ] **Step 6: 全包测试 + 提交**

```bash
go test ./guard/ ./intercept/ -count=1
GIT_MASTER=1 git add guard/guard.go guard/testmain_test.go guard/audit_tier_test.go intercept/intercept.go
GIT_MASTER=1 git commit -m "feat(guard): allow 档判定也进审计链，平台工具调用可追责"
```

---

### Task 3: DB 规则的 ask 类补默认超时

**Files:**
- Modify: `intercept/intercept.go`（`Match`）
- Create: `intercept/asktimeout_test.go`

**Interfaces:**
- Consumes: `db.InterceptRule{Action, TimeoutEnabled, TimeoutSeconds, TimeoutAction}`（已有）
- Produces: 常量 `defaultAskTimeoutSecs = 60`；`Match` 对 `Action=="ask"` 且 `TimeoutEnabled==false` 的规则补上 `TimeoutEnabled=true / TimeoutSeconds=60 / TimeoutAction="deny"`

- [ ] **Step 1: 写失败测试**

新建 `intercept/asktimeout_test.go`：

```go
package intercept

import (
	"testing"

	"github.com/Autumn-27/artex/db"
)

// 种子规则照抄了 timeout_enabled=false —— 照抄即「审批无限等待」。
// 命中 ask 的规则必须带默认超时，否则审批能一直挂着。
func TestMatchAskRuleGetsDefaultTimeout(t *testing.T) {
	dec := matchRule(db.InterceptRule{
		Name:           "种子规则",
		Action:         "ask",
		TimeoutEnabled: false,
		TimeoutSeconds: 0,
		TimeoutAction:  "",
	})
	if !dec.TimeoutEnabled {
		t.Fatal("ask 类规则必须带超时（否则审批无限等待）")
	}
	if dec.TimeoutSeconds != defaultAskTimeoutSecs {
		t.Fatalf("默认超时应为 %d 秒，得到 %d", defaultAskTimeoutSecs, dec.TimeoutSeconds)
	}
	if dec.TimeoutAction != "deny" {
		t.Fatalf("默认超时动作应为 deny，得到 %q", dec.TimeoutAction)
	}
}
```

（`matchRule` 是纯函数，不碰 DB，故本用例无需测试库。）

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./intercept/ -run TestMatchAskRuleGetsDefaultTimeout -v`
Expected: FAIL —— `matchRule` 尚不存在（编译错误）。

- [ ] **Step 3: 抽出纯函数并补默认超时**

`intercept/intercept.go`：

```go
// DB 规则的 ask 类默认超时。
//
// 种子规则照抄了 timeout_enabled=false，照抄即「审批无限等待」——
// 审批挂着不回来，人不在的时候这条面就永远卡住。默认给 60s 且超时即拒
// （超时放行等于「不在就通过」）。
const defaultAskTimeoutSecs = 60
```

把 `Match` 里的构造抽成纯函数（便于无 DB 测试）：

```go
// matchRule 把一条命中的 DB 规则变成 Decision，并补齐 ask 类的默认超时。
func matchRule(r db.InterceptRule) Decision {
	msg := r.Message
	if msg == "" {
		msg = defaultMessage(r.Action, r.Name)
	}
	dec := Decision{
		RuleName: r.Name, ConfigDigest: digestInput(configJSON),
		Action:         r.Action,
		Message:        msg,
		RuleID:         r.ID,
		TimeoutEnabled: r.TimeoutEnabled,
		TimeoutSeconds: r.TimeoutSeconds,
		TimeoutAction:  r.TimeoutAction,
	}
	if dec.Action == "ask" && !dec.TimeoutEnabled {
		dec.TimeoutEnabled = true
		dec.TimeoutSeconds = defaultAskTimeoutSecs
		dec.TimeoutAction = "deny"
	}
	return dec
}
```

（`Match` 改为调用 `matchRule(r.InterceptRule)`。`configJSON` 沿用原有写法：`json.Marshal(r)` 失败时忽略错误即可 —— 它只用来算配置摘要。）

- [ ] **Step 4: 运行测试确认通过**

Run: `go test ./intercept/ -run 'TestMatchAskRuleGetsDefaultTimeout' -v`
Expected: PASS。

- [ ] **Step 5: 确认判官侧默认值不变量已被测试固定**

Run: `go test ./intercept/ -run 'TestJudge' -v`
Expected: PASS（`intercept/judge_test.go` 已有 `defaultJudgeFailAction != "deny"` 与 `defaultJudgeAskTimeoutAction != "deny"` 的断言——判官侧「不可用即拒」本来就已经是默认，本任务只补 DB 规则这一侧）。

- [ ] **Step 6: 全量测试 + 提交**

```bash
go test ./intercept/ ./guard/ ./db/ -count=1
GIT_MASTER=1 git add intercept/intercept.go intercept/asktimeout_test.go
GIT_MASTER=1 git commit -m "fix(intercept): DB 规则的 ask 类补默认超时 60s→deny，关闭审批无限等待窗口"
```

---

### Task 4: 审批防疲劳计数（ask 队列治理）

**Files:**
- Modify: `db/intercept.go`（新增计数查询）
- Modify: `intercept/intercept.go`（`AgentNameFromContext`、`AskBurstExceeded`、settings 键）
- Modify: `guard/guard.go`（超阈值 → 直接拒绝）
- Create: `intercept/testmain_test.go`
- Create: `intercept/askburst_test.go`

**Interfaces:**
- Consumes: `intercept_pending(agent_name, tool_name, created_at)`（已有列）
- Produces:
  - `func (d *DB) CountRecentInterceptAsks(agentName, toolName string, since time.Time) (int, error)`
  - `func AgentNameFromContext(ctx context.Context) string`
  - `func (i *Interceptor) AskBurstExceeded(ctx context.Context, toolName string) bool`
  - settings 键：`intercept_ask_burst_threshold`（默认 5）、`intercept_ask_burst_window_seconds`（默认 600）

- [ ] **Step 1: 写失败测试（DB 计数 + 阈值判定）**

新建 `intercept/testmain_test.go`（照抄 `db/testmain_test.go`，包名 `intercept`，包名参数 `"intercept"`）。

新建 `intercept/askburst_test.go`：

```go
package intercept

import (
	"context"
	"testing"
	"time"
)

// 同一 agent 在窗口内反复对同一工具请求审批 → 达阈值后不再问人，直接拒。
// 治的是「疲劳点 allow」的无限循环；单次/少量请求完全不受影响。
func TestAskBurstExceeded(t *testing.T) {
	d := openTestDB(t)
	ic := New(d)
	ctx := WithTaskContext(context.Background(), "task-1", "worker-1", nil)

	// 窗口内已有 5 条同 agent 同工具的审批记录
	for i := 0; i < 5; i++ {
		if _, err := d.CreateInterceptPending(0, 0, "task-1", "worker-1", "update_custom_tool", []byte(`{}`), "ask"); err != nil {
			t.Fatal(err)
		}
	}

	if !ic.AskBurstExceeded(ctx, "update_custom_tool") {
		t.Fatal("窗口内已有 5 次同工具审批，应判定为超阈值")
	}
	if ic.AskBurstExceeded(ctx, "create_skill") {
		t.Fatal("别的工具不应受牵连")
	}

	// 无 agent 归属（会话侧）不计数
	if ic.AskBurstExceeded(context.Background(), "update_custom_tool") {
		t.Fatal("无 agent 归属时不应升级")
	}
}

// 窗口外的旧记录不计数：治理只针对「当前这一波」的反复请求。
func TestAskBurstIgnoresOldRequests(t *testing.T) {
	d := openTestDB(t)
	ic := New(d)
	ctx := WithTaskContext(context.Background(), "task-2", "worker-2", nil)

	for i := 0; i < 5; i++ {
		if _, err := d.CreateInterceptPending(0, 0, "task-2", "worker-2", "update_custom_tool", []byte(`{}`), "ask"); err != nil {
			t.Fatal(err)
		}
	}
	// 把窗口调到 1 秒并等它过去
	_ = d.SetSetting(settingAskBurstWindowSecs, "1")
	time.Sleep(1100 * time.Millisecond)

	if ic.AskBurstExceeded(ctx, "update_custom_tool") {
		t.Fatal("窗口外的请求不应计入")
	}
}
```

（`openTestDB` 辅助函数（本文件内实现）：

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
```

`db.NewWithConn` 若不存在，用 `db` 包里既有的「用现成 *sql.DB 构造 DB」的入口；落地时先 `grep -n "func New" db/db.go` 确认。`intercept` 包已 import `github.com/Autumn-27/artex/db` ✓。）

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./intercept/ -run 'TestAskBurst' -v`
Expected: FAIL —— `ic.AskBurstExceeded` / `settingAskBurstWindowSecs` 尚不存在（编译错误）。

- [ ] **Step 3: DB 计数查询**

`db/intercept.go` 新增：

```go
// CountRecentInterceptAsks 统计某 agent 在 since 之后对某工具的审批请求数。
//
// 只数「真正问过人」的请求：decision_source='shortcircuit' 且状态不是 denied
// （被拒的行从没问过人）；DB 规则发起的 ask 不计入 —— 那是管理员自己配的面。
func (d *DB) CountRecentInterceptAsks(agentName, toolName string, since time.Time) (int, error) {
	var n int
	err := d.QueryRow(`SELECT COUNT(*) FROM intercept_pending
		WHERE agent_name=$1 AND tool_name=$2 AND created_at >= $3
		  AND decision_source='shortcircuit' AND status <> 'denied'`,
		agentName, toolName, since).Scan(&n)
	return n, err
}
```

- [ ] **Step 4: intercept 侧阈值判定**

`intercept/intercept.go` 新增：

```go
// 审批防疲劳（可用性参数，故可热改；安全边界不在这里）。
const (
	settingAskBurstThreshold = "intercept_ask_burst_threshold"
	settingAskBurstWindowSecs = "intercept_ask_burst_window_seconds"
)

const (
	// defaultAskBurstThreshold 是窗口内「允许进入人工审批」的最大次数。
	// 第 N+1 次起直接拒绝（不再打扰人），治「疲劳点 allow」的无限循环。
	defaultAskBurstThreshold = 5
	// defaultAskBurstWindowSecs 是计数窗口（秒）。
	defaultAskBurstWindowSecs = 600
)

// 取 agent 名直接用包内私有的 taskInfoFromCtx —— 只有同包的 AskBurstExceeded
// 需要它，导出只会平白扩大 API 面。

// AskBurstExceeded 判断该 agent 在时间窗内对某工具的审批请求是否已达阈值。
//
// 只对【有 agent 归属】的请求计数：会话侧没有 agent 归属，计了会误伤人。
// 计数失败时返回 false（不升级）：这个计数是防疲劳措施、不是安全边界，
// 误拒的代价（正常任务被卡死）高于漏拒的代价（人多点一次 allow）。
func (i *Interceptor) AskBurstExceeded(ctx context.Context, toolName string) bool {
	agentName := AgentNameFromContext(ctx)
	if agentName == "" {
		return false
	}
	threshold := i.getSettingInt(settingAskBurstThreshold, defaultAskBurstThreshold)
	if threshold <= 0 {
		return false
	}
	window := i.getSettingInt(settingAskBurstWindowSecs, defaultAskBurstWindowSecs)
	if window <= 0 {
		window = defaultAskBurstWindowSecs
	}
	since := time.Now().Add(-time.Duration(window) * time.Second)
	n, err := i.db.CountRecentInterceptAsks(agentName, toolName, since)
	if err != nil {
		return false
	}
	return n >= threshold
}
```

- [ ] **Step 5: guard 侧升级为直接拒绝**

`guard/guard.go` 的 ask 分支，在调 `HandleAsk` **之前**插入：

```go
		case "ask":
			// 审批防疲劳：同一 agent 在窗口内反复对同一高危面请求审批，
			// 达阈值后直接拒绝，不再打扰人（也堵住「疲劳点 allow」的循环）。
			if g.interceptor != nil && g.interceptor.AskBurstExceeded(ctx, ev.ToolName) {
				esc := *dec
				esc.Action = "deny"
				esc.RuleName = "[代码常量] 审批请求超阈值（同一 agent 重复请求）"
				esc.Message = "同一 agent 在时间窗内对本工具的审批请求已超阈值，按防疲劳策略直接拒绝。"
				logCtx := intercept.WithCall(ctx, ev.ToolName, ev.Input)
				g.interceptor.Log(logCtx, intercept.ConvIDFromContext(logCtx), esc, ev.ToolName, ev.Input, "denied")
				g.interceptor.AppendDecisionEvent(logCtx, esc, ev.ToolName, ev.Input, "denied")
				g.record(ev.ToolName, "block", esc.Message, "")
				return g.block(ev.ToolName, systemBlockMessage(esc.Message), "")
			}
			if g.interceptor == nil {
				return g.block(ev.ToolName, systemBlockMessage("人工审批通道不可用（"+dec.Message+"）"), "")
			}
			ctx = intercept.WithCall(ctx, ev.ToolName, ev.Input)
			if !g.interceptor.HandleAsk(ctx, intercept.ConvIDFromContext(ctx), *dec, ev.ToolName, ev.Input) {
				return g.block(ev.ToolName, systemBlockMessage("人工审批未通过（用户拒绝或审批超时）"), "")
			}
			return hook.Result{}
```

- [ ] **Step 6: 运行测试确认通过**

Run: `go test ./intercept/ -run 'TestAskBurst' -v`
Expected: PASS（两个用例都过）。

- [ ] **Step 7: 全量测试 + 提交**

```bash
go test ./intercept/ ./guard/ ./db/ ./server/ -count=1
GIT_MASTER=1 git add db/intercept.go intercept/intercept.go intercept/testmain_test.go intercept/askburst_test.go guard/guard.go
GIT_MASTER=1 git commit -m "feat(intercept): 审批防疲劳计数，同一 agent 重复请求超阈值即拒绝"
```

---

### Task 5: 账本口径同步（G-01① 降级）

**Files:**
- Modify: 账本本体（本地工作区内，不在版本库；路径见下方说明）
- Modify: 账本归档副本（与账本本体同目录的 output/ 下）

**Interfaces:**
- Consumes: 本计划前三项的实际实现与测试结果
- Produces: 与实现一致的账本口径（G-01① 从「完全闭合」降为「高危面闭合 + 低危面代码级显式放行」；新增 G-09④⑥ 已闭合的记录；G-04①② 的 judge 侧默认 deny 本就存在、已补 DB 规则侧）

- [ ] **Step 1: 定位账本文件**

账本本体在本地工作区（不在版本库内，避免个人路径进仓库）。用 `ls` 确认其存在与路径后再编辑。

- [ ] **Step 2: 改 G-01① 的判定口径**

把「G-01① 平台工具绕白名单 → 短路表人工审批 ｜ 判定：已解决（唯一完全闭合）」改为记录三档现状：

```
#### G-01① 平台工具绕白名单 → 短路表三档判定 ｜ 判定：已解决（覆盖面降级，见下）
- 现状：短路表扩成三档 —— deny（create_mcp/update_mcp、读平台控制面秘密、平台环境块）、
  ask（update_custom_tool/create_custom_tool/update_skill_file/delete_assets_by_host）、
  显式 allow（create_skill）。
- 关键性质：「显式 allow」是代码常量【主动判定放行】，仍在 applyIntercept 最前求值，
  与「不命中短路表」的漏判有本质区别 —— G-01① 原漏洞（平台工具绕白名单直接放行）不因此重开。
- 口径变化：为降低日常摩擦，create_skill 从 ask 降为显式 allow，故覆盖面由
  「7 件全部人工确认」降为「4 件人工确认 + 1 件显式放行 + 2 件硬拦」。
  账本不再称此项「完全闭合」。
```

- [ ] **Step 3: 补 G-09④⑥ 与 G-04①② 的闭合记录**

```
| G-09④⑥ ask 队列治理 | 未解决 | **已解决** | 计数维度 (agent, 工具, 时间窗)，仅 ask 档、仅有 agent 归属的请求；窗口内第 N+1 次起直接拒绝（默认 N=5、窗口 600s，可经 settings 热改 —— 属可用性参数非安全边界）；计数失败不升级（宁可多问一次人，不误拒正常任务） |
| G-04①② 判官分侧 + ask 默认超时 | 未解决 | **已解决** | 判官侧「不可用即拒」本就是默认值（defaultJudgeFailAction=deny / defaultJudgeAskTimeoutAction=deny，已有测试固定）；本次补上 DB 规则侧：命中 ask 的规则若未配 timeout_enabled，自动补 60s 超时且超时动作 deny —— 关闭「审批无限等待」窗口 |
```

- [ ] **Step 4: 归档并核对**

把账本复制到归档路径，两份内容一致（`cmp` 校验）。

- [ ] **Step 5: 提交（账本不在版本库则跳过）**

若账本确实不在版本库内，本步无提交动作，只需在最终汇报里说明账本已更新与归档位置。

---

## 验收（整个计划完成后）

```bash
# 1. 三档行为
go test ./guard/ ./intercept/ -count=1 -v | grep -E '^(=== RUN|--- (PASS|FAIL))' | tail -30
# 2. 全量 Go 测试无跳过
go test ./... -timeout 30m -count=1
# 3. 派生测试库跑完归零（不残留 artex_test_* 空壳）
# 4. CI 全绿（推送后观察 GitHub Actions 的 ci run）
```

**本计划不覆盖**（留给后续波次，勿在本计划里顺手做）：W-B1 签名与 provenance、W-A2 路径解析、W-A3 部署面收口、W-A4 密钥加密、per-task 隔离架构。

---

## 实施偏差记录（2026-10-28 执行后回写）

计划与代码不一致时，**以代码为准并回写计划**（本节即为回写结果）：

| 计划原写法 | 实际实现 | 原因 |
| --- | --- | --- |
| `matchRule(r, toolName, input) (Decision, bool)` | `matchRule(r db.InterceptRule) Decision` | 匹配已在上游完成，这两个参数在函数体里用不到，留着是噪声；`bool` 恒为 true 也是噪声 |
| 导出 `AgentNameFromContext` | 用包内私有 `taskInfoFromCtx` | 只有同包的 `AskBurstExceeded` 需要，导出扩大 API 面无收益 |
| 测试助手用 `db.NewWithConn` | 用 `db.Open(dsn)` | 仓库里 `db` 包只有 `Open`；它会跑 schema 与种子数据，测试库因此自动建表 |
| 计数只按 (agent, 工具, 窗口) | 追加 `decision_source='shortcircuit' AND status <> 'denied'` | 「只数真正问过人的请求」——被拒的行从没问过人；DB 规则发起的 ask 属管理员自配面，不该被短路表的治理计数 |
| `dbInterceptRule` 测试辅助类型 | 直接用 `db.InterceptRule` | 不该为测试另造类型 |

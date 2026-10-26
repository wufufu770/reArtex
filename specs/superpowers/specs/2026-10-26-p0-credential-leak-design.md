# P0 凭据泄露链修复 · 设计文档

| 项 | 值 |
| --- | --- |
| 日期 | 2026-10-26 |
| 仓库 | 本地克隆的 `ARTEX`（module `github.com/Autumn-27/artex`） |
| 分支 | `local/a2-hardening`（基线 `1b29664`，本批在其上） |
| 状态 | 待实施 |
| 范围决策 | 仅 P0 凭据泄露链（Sir 2026-10-26 决定） |
| 路线决策 | B2 = DSN 走文件、读入即 unlink（Sir 2026-10-26 决定） |
| 误伤边界决策 | 正则只收紧 environ，cmdline/mem 维持现状（Sir 2026-10-26 决定） |

---

## 0. 背景与证据

### 0.1 问题

ARTEX 容器以 `ARTEX_PG_DSN` 环境变量携带数据库连接串。实测容器内 `/proc/1/environ`：

```
ARTEX_PG_DSN      值长度 = 81   → postgres://artex:<PASSWORD>@postgres:5432/artex?sslmode=disable
OPENAI_API_KEY    值长度 = 0
ANTHROPIC_API_KEY 值长度 = 0
```

平台自己的 exec 期环境块对同 UID 的 agent 子进程可读。实测 glob 形态可读出明文：

```
docker exec artex-artex-1 sh -c 'cat /proc/*/environ | tr "\000" "\n" | grep -c "^ARTEX_PG_DSN=postgres"'
→ 8
```

### 0.2 为什么 `os.Unsetenv` 不够

A2 已在 `db/db.go` 与 `agent/provider.go` 做了 `os.Unsetenv`。它只改 Go 进程内的环境副本，**改不动内核保存的 exec 期环境块**（`/proc/<pid>/environ` 是那份原始块的只读视图）。

代码注释里已写明这一约束（`agent/provider.go:151-155`），但当时只做了「读入即 unset」，没有把秘密从环境里挪走。

### 0.3 完整接管链

```
agent Bash → cat /proc/*/environ        （实测命中 8 条明文 DSN）
           → 得到 DB 口令
           → 连 postgres，读 llm_profiles.api_key（125 字符 LLM key）
           → 再读全部会话 / 流量 / 资产 / 漏洞
```

容器内无 `psql`、无 `psycopg`，但有 `pip3`（实测），且渗透 agent 自建 postgres 线协议客户端是常规动作。

### 0.4 守卫正则的已知绕过

还原 `guard/shortcircuit.go:96-112` 的 `platformCredentialRe` 后逐个试探：

| 写法 | 结果 |
| --- | --- |
| `cat /proc/1/environ` | BLOCKED |
| `cat /proc/self/environ` | BLOCKED |
| `cat /proc/1/cmdline` | BLOCKED |
| `x=/proc/1/environ; cat $x` | BLOCKED |
| `cat /app/jwt.key` | BLOCKED |
| `cat /proc/*/environ` | **BYPASS** |
| `cat /proc/$$/environ` | **BYPASS** |
| `cat /proc/[0-9]*/environ` | **BYPASS** |
| `cat /proc/*/cmdline` | **BYPASS** |
| `cat /proc/1/envi*on` | **BYPASS** |

### 0.5 一个重要的正面事实

`server/server.go:239-246` 的 LLM 配置优先级是 **DB profile 优先于 env**：

```go
// precedence: persisted DB config > env.
if cfg, ok := s.loadLLMConfig(); ok {   // → "LLM configured from DB"
} else if cfg, ok := agent.FromEnv(); ok {
```

实测 `llm_profiles` 有 1 条记录（`is_default=t`，`api_key` 125 字符），且 env 里两个 key 都是空值。
**结论：LLM key 从来不在 env 里，env 里的只有 DSN。** 因此本次修复的唯一实质目标是 DSN。

---

## 1. 目标与非目标

### 1.1 目标

1. artex 进程的 `/proc/<pid>/environ` 内不再存在任何平台秘密（DSN、API key）。
2. 「读父进程环境块」这一类攻击无密可读 —— 闭合靠供给侧，不靠字符串过滤。
3. 守卫正则覆盖 `/proc/*/environ` 的 glob 与展开形态，作为纵深防御。
4. 部署形态退化时（有人把 key 塞回 env）能被启动自检当场喊出来，而不是默默「看起来加固了」。
5. 渗透 agent 的常规 `/proc` 侦察（`/proc/net/tcp`、`/proc/cpuinfo`、`/proc/1/cmdline`）不被误伤。

### 1.2 非目标

- 不闭合 `/proc/<pid>/mem` 读父进程内存（需 ptrace 权限）。Sir 决定「其余照旧」，本批明确留作已知残留。
- 不闭合运行时构造路径的读取（`find /proc -name environ -exec cat {} \;`）。字符串预筛本质挡不住；本批在文档中明说，不宣称「已闭合」。
- 不处理 `docker inspect` 可见 DSN 的容器配置面（需宿主/docker socket 权限，已 root 等价）。
- 不动 `llm_profiles.api_key` 的存储形态（那是 DB 读权限问题，与本项无关）。
- 不做权限隔离 / 容器拆分（路线图 G-02③、G-13④，另一个立项）。

---

## 2. 验收标准（全部可实测）

| # | 命令 | 期望 | 现状 |
| --- | --- | --- | --- |
| A1 | `docker exec artex-artex-1 sh -c 'tr "\000" "\n" < /proc/1/environ \| grep -E "ARTEX_PG_DSN=\|_API_KEY="'` | 0 行 | 1 行 |
| A2 | `docker exec artex-artex-1 sh -c 'cat /proc/*/environ \| tr "\000" "\n" \| grep -c "^ARTEX_PG_DSN=postgres"'` | 0 | 8 |
| A3 | 5 种 glob/展开形态喂 `shortCircuit("Bash", …)` | 全部 `deny` | 5 种 BYPASS |
| A4 | `/proc/net/tcp`、`/proc/cpuinfo`、`/proc/*/cmdline`、`/proc/self/status` | 不拦 | 不拦（须保持） |
| A5 | 重建前后 `findings` / `assets` / `conversations` 行数 | 不变 | — |
| A6 | `go build ./...` | exit 0 | 0 |
| A7 | `go test ./config/... ./guard/... ./db/...` | 全绿 | 全绿 |
| A8 | 平台启动日志 | 出现自检结论行（干净时明确「未发现残留」） | 无此机制 |

---

## 3. 方案总览

### 3.1 改后数据流

```
compose environment: ARTEX_PG_DSN=…          ← 只在【容器配置面】（docker inspect 可见）
  ↓ entrypoint wrapper（PID 1）
     umask 077
     mkdir -p /run/artex
     printf '%s' "$ARTEX_PG_DSN" > /run/artex/pg_dsn    (0600)
     unset ARTEX_PG_DSN
     exec /app/start.sh "$@"                ← exec 用【当前环境】重建进程映像
  ↓
start.sh（environ 已无 DSN） → artex（environ 已无 DSN）
     ★ 此后 /proc/<artex-pid>/environ 内不再有任何秘密
  ↓ artex 启动读文件
config.PostgresDSN() → 读入内存 → Truncate(0) → os.Remove
  ↓
DSN 只存在于 Go 变量与连接池里
```

### 3.2 为什么 `unset` + `exec` 是必须的

`unset` 只改 shell 内部变量表，**不改内核保存的 exec 期环境块**。但 `exec` 会用 shell 的**当前**环境重新调用 `execve`，新进程映像的 `/proc/<pid>/environ` 就是 execve 传入的那份。

因此：先 `unset` 再 `exec`，exec 出来的 artex 的环境块里就没有 DSN。顺序反了（先 exec 再 unset）则无效。

### 3.3 窗口期分析

- wrapper shell 持有 DSN 的时间：从容器启动到 `exec`，约数十毫秒。
- 该窗口内 artex 尚未启动、HTTP 端口未监听、agent 不存在 → **无任何进程能读它**。
- 容器配置面（`docker inspect`）全程可见 DSN —— 这是刻意取舍，见 §5。

---

## 4. 详细设计

### 4.1 `config/config.go` —— 新增 `ARTEX_PG_DSN_FILE` 来源

**改动点**：`config/config.go:137` `PostgresDSN()`

来源优先级扩展为：

```
env ARTEX_PG_DSN  >  file ARTEX_PG_DSN_FILE  >  config file (database.dsn / database 字段)
```

新增两个函数：

```go
// readAndBurnDSNFile 读连接串后立刻把磁盘上的那份抹掉。
//
// 只读不删等于把秘密留在盘上等 agent 来捡：agent 的 Bash 与 artex 同 UID，
// 且 /app/data 是它的工作区根。内容已在内存里，磁盘上的副本没有存在价值。
func readAndBurnDSNFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	// 先清零再删：删除失败时至少内容不在了。
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

`PostgresDSN()` 中插入：

```go
	if p := strings.TrimSpace(os.Getenv("ARTEX_PG_DSN_FILE")); p != "" {
		d, err := readAndBurnDSNFile(p)
		if err != nil {
			return "", "", fmt.Errorf("读取 ARTEX_PG_DSN_FILE %q 失败: %w", p, err)
		}
		return d, "文件 " + p + " (读入后已删除)", nil
	}
```

**设计约束**：

- 文件来源失败时**明确报错、不静默 fallback**。理由同 `PostgresDSN()` 既有注释：「no built-in fallback … startup fails loudly instead of silently connecting to a wrong default」。
- `ARTEX_PG_DSN`（env）优先级保持最高。已有部署若不设 `ARTEX_PG_DSN_FILE`，行为与今天完全一致 → 向后兼容。
- `db/db.go` 里既有的 `os.Unsetenv("ARTEX_PG_DSN")` 保留 —— env 路径下它仍有用。
- `config.Redact(dsn)` 已有，日志里照旧用它遮蔽口令。
- **测试落点**：`config/config_test.go` 已有 `TestPostgresDSNPrecedence`，本批**扩展它**（加入 file 来源一档）而不是另起炉灶，与仓库「同一关注点同一测试文件」的纪律一致。新增用例需覆盖：文件来源生效、读入后文件被删除且内容清零、文件为空时报错而非静默 fallback、env 仍优先于 file。

### 4.2 `config/config.go` + `server/server.go` —— 启动自检

**新增** `config/config.go`：

```go
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
	want := map[string]bool{
		"ARTEX_PG_DSN": true, "ANTHROPIC_API_KEY": true, "OPENAI_API_KEY": true,
	}
	var hits []string
	for _, kv := range strings.Split(string(b), "\x00") {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || !want[k] || strings.TrimSpace(v) == "" {
			continue
		}
		hits = append(hits, k)
	}
	return hits
}
```

**调用点**：`server/server.go:257`（紧挨既有的 `agent.ScrubSecretEnv()` 之后）

**前置改动**：`server/server.go` 当前未 import `config` 包（其 import 块已有 `fmt` / `log` / `os`，无需新增），需补一行：

```go
	"github.com/Autumn-27/artex/config"
```

`config` 包只依赖标准库，不存在 import 环。

```go
	agent.ScrubSecretEnv()
	if leaks := config.SecretEnvLeak(); len(leaks) > 0 {
		msg := fmt.Sprintf("[security] 平台凭据仍可经 /proc/self/environ 读取（%v）："+
			"同 UID 的 agent 子进程可用 `cat /proc/*/environ` 读走。"+
			"请改用 ARTEX_PG_DSN_FILE，或把密钥移出容器环境。", leaks)
		if os.Getenv("ARTEX_REQUIRE_ENV_SCRUB") == "1" {
			log.Fatalf("%s", msg)
		}
		log.Printf("%s", msg)
	} else {
		log.Printf("[security] 平台凭据未残留在进程环境块中")
	}
```

**语义说明**：

- 默认（`ARTEX_REQUIRE_ENV_SCRUB` 未设或非 `1`）→ **只告警**，不挡启动。理由：官方镜像与既有部署可能合法地用 env 传参，硬失败会让它们直接起不来。
- `ARTEX_REQUIRE_ENV_SCRUB=1` → `log.Fatalf` 拒绝启动。`server.New()` 无 error 返回路径，故用 Fatal；start.sh 的守护循环会重启它 → 形成带醒目日志的崩溃循环，这是「拒绝启动」在现有结构下的表达方式。
- 干净时也打一行明确结论（验收 A8），避免「没输出」被误读成「没检查」。

### 4.3 `guard/shortcircuit.go` —— 正则收紧（只保 environ）

**改动点**：`guard/shortcircuit.go:96-112` `platformCredentialRe`

把两条 environ 规则

```
`|/proc/self/environ` +
`|/proc/\d+/environ` +
```

替换为一条覆盖任意形态的规则

```
`|/proc/[^\s"']*envi[^\s"']*` +
```

**覆盖验证**（逐字对 `(?i)` 生效）：

| 输入 | 匹配 | 说明 |
| --- | --- | --- |
| `/proc/self/environ` | ✓ | `self/` 由 `[^\s"']*` 吃掉 |
| `/proc/1/environ` | ✓ | |
| `/proc/*/environ` | ✓ | `*/` 被吃掉 |
| `/proc/$$/environ` | ✓ | `$$/` 被吃掉 |
| `/proc/[0-9]*/environ` | ✓ | `[0-9]*/` 被吃掉 |
| `/proc/1/envi*on` | ✓ | `envi` + `*on` |
| `/proc/net/tcp` | ✗ | 不含 `envi` |
| `/proc/cpuinfo` | ✗ | 不含 `envi` |
| `/proc/self/status` | ✗ | 不含 `envi` |
| `/proc/meminfo` | ✗ | 不含 `envi` |

**cmdline / mem 按 Sir 决定维持现状**：`/proc/\d+/cmdline`、`/proc/\d+/mem` 两条原样保留（字面数字形态仍拦，glob 形态仍放过）。

**已知代价（须写进 CHANGELOG 残留清单）**：这条规则作用在整个工具入参 JSON 上，因此「把含 `/proc/…/environ` 字样的内容写进文件」也会被拦（例如 `echo "cat /proc/self/environ" > notes.md`）。这是字符串预筛的固有代价，与既有的 `/app/jwt.key` 规则同类。

**定位声明**：本条是**纵深防御**，不是闭合。闭合靠 §4.1 + §4.5 的供给侧。代码注释里必须写明这一点，不得宣称「已阻断」。

### 4.4 `guard/shortcircuit_test.go` —— 回归用例

沿用仓库既有风格（表驱动 + 中文断言 + 正向/负向成对）。

**正向**（必须 `deny`）：

```
{"environ glob",        `{"command":"cat /proc/*/environ"}`},
{"environ 变量展开",     `{"command":"cat /proc/$$/environ"}`},
{"environ 字符类 glob",  `{"command":"cat /proc/[0-9]*/environ"}`},
{"environ 文件名 glob",  `{"command":"cat /proc/1/envi*on"}`},
{"environ self",        `{"command":"cat /proc/self/environ"}`},
```

**负向**（必须**不拦**，守住渗透正事）：

```
{"本机网络侦察",        `{"command":"cat /proc/net/tcp"}`},      // 既有 allow 表已有
{"本机 cpu 信息",        `{"command":"cat /proc/cpuinfo"}`},
{"进程命令行 glob 形态",   `{"command":"cat /proc/*/cmdline"}`},
{"进程状态侦察",          `{"command":"cat /proc/self/status"}`},
```

**注意 cmdline 的口径**（与 §4.3 一致）：按 Sir 决定 cmdline/mem 维持现状，故
- `/proc/<数字>/cmdline`（字面数字形态）**仍被拦** —— 既有规则 `/proc/\d+/cmdline` 不动；
- `/proc/*/cmdline`（glob 形态）**放过** —— 这才是本批新增的 environ 规则之外的行为。

因此负向用例只能用 glob 形态；把字面数字形态写成负向用例会直接失败。

负向用例的必要性来自仓库既有纪律（`guard/shortcircuit.go` 顶部注释：每条安全规则必须配「不许误伤渗透正事」的反向用例）。

### 4.5 部署侧 wrapper（本地文件，不进版本库）

改动文件：仓库根目录的 `docker-compose.override.yml`（已被 `.git/info/exclude` 排除，永不推送）。

```yaml
services:
  artex:
    image: artex:local
    pull_policy: never
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
```

要点：

- `$$` 是 compose 的转义，实际传给容器的是 `$ARTEX_PG_DSN` / `$@`。
- 数组末位 `--` 让 `sh -c` 把后续参数放进 `$@`；镜像 CMD 的 `-addr :8787 -proxy :8788` 会由 compose 追加 → `exec /app/start.sh -addr :8787 -proxy :8788`。
- `umask 077` → 文件 0600、目录 0700。
- `/run` 已在容器内存在且可写（实测），不在 agent 的工作区（`/app/data/sessions/*`）内。

**为什么放在本地 override 而不是仓库的 `docker-compose.yml`**：仓库 base compose 配的是官方镜像 `autumn27/artex`，其二进制不认识 `ARTEX_PG_DSN_FILE`；若在 base compose 里启用 wrapper，官方镜像会因找不到 DSN 而启动失败。等 reArtex 发布自有镜像时，再把这段并入 base compose（属路线图 A1「发布身份」的工作范围）。

---

## 5. 残留风险（诚实清单）

| 残留 | 性质 | 本批处理 |
| --- | --- | --- |
| `docker inspect` 可见 DSN | 容器配置面 | 刻意保留。需宿主/docker socket 权限 = 已 root 等价 |
| `/proc/<pid>/mem` 读父进程内存可捞到 DSN 字符串 | 需 ptrace 权限 | **明确留作已知残留**（Sir 决定「其余照旧」），写入 CHANGELOG 残留清单 |
| 运行时构造路径（`find /proc -name environ`） | 字符串预筛本质 | 明说未闭合；不靠正则，靠供给侧 |
| 写入含 `/proc/…/environ` 字样的文件被拦 | 误伤 | 已知代价，写入 CHANGELOG |
| DSN 字节留在 Go 堆内存 | 内存残留 | 不做密码学擦除；超出本批范围 |
| `llm_profiles.api_key` 可被有 DB 读权限者读 | 需 DB 权限 | 与本项无关 |

---

## 6. 提交拆分

遵循 git-master 的多提交规则（不同目录/关注点分开；实现与测试同 commit）。

| # | 提交信息 | 文件 | 理由 |
| --- | --- | --- | --- |
| C1 | `feat(config): 新增 ARTEX_PG_DSN_FILE 来源，读入即清零并删除文件` | `config/config.go`、`config/config_test.go` | 实现 + 其直接测试 |
| C2 | `feat(config): 启动自检平台凭据是否仍可经 /proc/self/environ 读取` | `config/config.go`、`server/server.go` | 自检实现 + 其唯一调用点，同一关注点 |
| C3 | `fix(guard): 收紧平台环境块拦截，覆盖 /proc/*/environ 的 glob 与展开形态` | `guard/shortcircuit.go`、`guard/shortcircuit_test.go` | 实现 + 测试 |
| C4 | `docs: 补 CHANGELOG 残留清单与 DSN 文件源说明` | `CHANGELOG.md`、`README.md` | 文档同步 |

最小提交数 `ceil(12/3) = 4`，计划 4 → 满足。

---

## 7. 验证方案

**单元层**
- `go test ./config/... -run 'DSN|SecretEnv'` —— 文件来源、读入即删、失败即报错、自检命中/不命中
- `go test ./guard/... -run 'Credential'` —— 5 正向 + 4 负向
- `go test ./config/... ./guard/... ./db/...` 全绿
- `go build ./...` exit 0

**集成层（重建 artex1 后）**
1. 跑验收 A1–A5、A8
2. 平台能正常连库、会话/资产/漏洞行数不变
3. `docker logs` 无 `[security]` 告警行、有「未残留」结论行
4. 故意把 `ARTEX_PG_DSN` 直接塞回 compose 环境（绕过 wrapper）→ 启动日志应出现告警；加 `ARTEX_REQUIRE_ENV_SCRUB=1` 应拒绝启动（负向验证）

**回归**
- `go test ./...` 全量（已知 `server` 包在基线 `1b29664` 上即因缺 DB 配置而失败，属既有环境性失败，不是本批引入；须与基线对比确认失败集合不变）

---

## 8. 回滚

| 层 | 回滚方式 |
| --- | --- |
| 代码 | `git revert` 对应 commit，或 `git checkout main`（回到 `1b29664`） |
| 部署 | 移除 override 的 `entrypoint` / `ARTEX_PG_DSN_FILE` 两项 → 恢复 env 供给（回到今天的行为） |
| 数据 | 本批不重建 pg 数据卷，`findings`/`assets`/`conversations` 不受影响 |

---

## 9. 文档同步

| 文档 | 改动 |
| --- | --- |
| `CHANGELOG.md` | `## [Unreleased]` 段（当前为空）补：A2 全部 15 个提交 + 本批 4 个提交的摘要；新增「已知残留」小节，写明 §5 的 5 条 |
| `README.md` | 补 `ARTEX_PG_DSN_FILE` 的用法与部署说明（不含任何个人环境信息） |
| 审计账本 `reatrex.md`（仓库外的本地文件） | G-02①-a 状态更新（对应账本 34 项复审结论） |
| 归档输出目录（仓库外的本地工作区） | 本设计文档副本（工作区归档） |

---

## 10. 与审计账本的对应

本批完成后，`reatrex.md` 中 G-02①-a 的判定更新为：

- **原判定**：部分解决△（残留加重：自定义 Python/SDK Bash/stdio MCP 继承 os.Environ）
- **新判定**：**部分解决（残留收窄）** —— 环境继承通道（env 供给）已断；剩余残留为 §5 的 /proc mem 形态与容器配置面
- **依据**：`config/config.go` 的 `ARTEX_PG_DSN_FILE` 来源 + `guard/shortcircuit.go` 的 environ 规则收紧 + 验收 A1/A2 实测为 0

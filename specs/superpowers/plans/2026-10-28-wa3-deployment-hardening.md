# W-A3 部署面收口 · 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 把三条**部署面**的活路径逐条闭掉：容器配置面能看到数据库口令（`docker inspect`）、同 UID 进程能读父进程内存（`/proc/<pid>/mem`）、平台秘密一旦进了环境块就无人守（一次性自检没有门）。这三项都不改应用逻辑，改的是 compose 形态与验收脚本。

**Architecture:** ① 数据库口令改由 **docker secret** 文件供给（`/run/secrets/pg_dsn`），compose 不再把口令放进 `environment` → `docker inspect` 看不到；配套让 `readAndBurnDSNFile` 容忍**只读挂载**（secret 文件不可删也不可清零，但它在 `/run/secrets`，不在 agent 工作区内）。② 用 `cap_drop: [SYS_PTRACE]` 让 Docker 默认 seccomp 拦掉 `ptrace` 系统调用（默认 seccomp 配置对 `ptrace` 的放行条件是「容器持有 CAP_SYS_PTRACE」，去掉该能力即被拦）→ `/proc/<pid>/mem` 读不到。③ 把「进程树环境块无非空平台秘密」固化成可执行脚本 `verify-secret-env.sh`，进镜像、进 release 冒烟 job —— 从「一次性自检」变成「每次发布都验的门」。

**Tech Stack:** Docker Compose（file 型 secret）、`seccomp`（Docker 默认 profile）、POSIX shell、Go 1.26（仅 config 包一处小改）

## Global Constraints

- 所有 git 命令必须带 `GIT_MASTER=1` 前缀。
- **禁止 `git reset --hard`**；**禁止用配置里的活库 `artex` 跑测试**。
- 注释、commit message、文档一律中文；shell 脚本沿用既有风格（`set -u`、中文注释说明「为什么」）。
- **不引入新的第三方依赖**（脚本只用 POSIX sh + 核心工具）。
- 部署面改动必须**保留可回退路径**：compose 的改动要能一条命令退回旧形态（文档写清）。
- 验收脚本的判定必须是**可执行断言**（退出码），不得只打印信息。
- 每一处「闭合」都要同步更新账本 `reartex.md` 并归档。
- 推送前自检：不得出现本机路径、个人 MCP 名、凭据、主机指纹。**注意**：本计划会新增 `secrets/` 目录，必须写进 `.gitignore`，且**绝不能**把真实口令文件提交进仓库。

---

## File Structure

| 文件 | 职责 | 本计划中的动作 |
| --- | --- | --- |
| `config/config.go` | DSN 解析与「读入即烧毁」 | `readAndBurnDSNFile` 容忍只读挂载（限 `/run/secrets` 前缀） |
| `config/config_test.go` | config 包单测（已存在） | 补一条：只读文件不被烧毁也能读出 DSN |
| `docker-compose.yml` | 部署形态 | 去掉 `ARTEX_PG_DSN` 环境变量；加 `secrets:` 段与 `secrets:` 挂载；加 `cap_drop` |
| `init-pg-dsn-secret.sh` | 从 `.env` 生成 secret 文件（**新建**） | 写 `./secrets/pg_dsn`，权限 0600 |
| `verify-secret-env.sh` | 进程树环境块洁净性验收（**新建**） | 扫描进程树，断言无非空平台秘密 |
| `verify-ptrace-blocked.sh` | ptrace 封闭验收（**新建**） | 断言读父进程内存失败 |
| `Dockerfile` | 运行镜像 | COPY 两个验收脚本到 `/usr/local/bin/` |
| `.github/workflows/release.yml` | 发布流水线 | 冒烟步骤追加两条验收脚本 |
| `.gitignore` | 版本库忽略 | 加 `secrets/` |

---

### Task 1: DSN 文件读取容忍只读挂载

**Files:**
- Modify: `config/config.go`
- Modify: `config/config_test.go`

**Interfaces:**
- Consumes: `os.ReadFile` / `os.OpenFile` / `os.Remove`
- Produces: `readAndBurnDSNFile(path string) (string, error)` 的新语义——**只读挂载**（`/run/secrets` 前缀）下不烧毁、不报错；其余路径保持「烧毁失败即报错」

- [ ] **Step 1: 写失败测试**

在 `config/config_test.go` 追加：

```go
// docker secret 是只读挂载：既不能清零也不能删。但它落在 /run/secrets，
// 不在 agent 的工作区（/app/data）里，agent 够不着 —— 故容忍不烧毁。
func TestPostgresDSNFileReadOnlyMountIsTolerated(t *testing.T) {
	dir := t.TempDir()
	// 只读 secret 目录是包级变量，测试临时指向临时目录（手法与 pathguard 的
	// withSecretRoots 一致：替换包级变量 + Cleanup 还原，生产代码不留测试钩子）。
	orig := readOnlySecretDir
	readOnlySecretDir = dir
	t.Cleanup(func() { readOnlySecretDir = orig })

	p := filepath.Join(dir, "pg_dsn")
	if err := os.WriteFile(p, []byte("postgres://u:p@h:5432/d"), 0o400); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ARTEX_PG_DSN_FILE", p)

	dsn, source, err := PostgresDSN()
	if err != nil {
		t.Fatalf("只读 secret 应能读出 DSN: %v", err)
	}
	if dsn != "postgres://u:p@h:5432/d" {
		t.Fatalf("DSN 不对: %q", dsn)
	}
	if source == "" {
		t.Fatal("source 不应为空")
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./config/ -run TestPostgresDSNFileReadOnlyMountIsTolerated -v`
Expected: FAIL —— 现状下 `os.Remove` 失败会返回错误，DSN 读不出来。

- [ ] **Step 3: 实现容忍分支**

`config/config.go`：把 `readAndBurnDSNFile` 改成

```go
// readOnlySecretDir 是只读 secret 的目录前缀。docker secret 以只读方式挂到
// 这里：既不能清零也不能删。它不在 agent 的工作区（/app/data）内，agent 够不着，
// 故可容忍「不烧毁」。
//
// 做成包级变量是为了让测试能临时指向临时目录（手法与 pathguard 的
// withSecretRoots 一致），生产代码不留测试专用钩子。
var readOnlySecretDir = "/run/secrets"

// readAndBurnDSNFile 读连接串后立刻把磁盘上的那份抹掉。
//
// 只读不删等于把秘密留在盘上等 agent 来捡：agent 的 Bash 与 artex 同 UID，
// 且 /app/data 是它的工作区根。内容已在内存里，磁盘上的副本没有存在价值。
//
// 例外：只读 secret（/run/secrets 下的 docker secret）无法清零也无法删除。
// 那条路径不在 agent 工作区内，故容忍「不烧毁」而不是让启动失败 ——
// 否则 compose 用 secret 供给 DSN 的部署根本起不来。
func readAndBurnDSNFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if readOnlySecret(path) {
		return strings.TrimSpace(string(b)), nil
	}
	// 顺序是先清零再删：删除失败时至少内容不在了。
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

// readOnlySecret 判断路径是否落在只读 secret 目录内。
func readOnlySecret(path string) bool {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	return strings.HasPrefix(abs, readOnlySecretDir+string(filepath.Separator)) || abs == readOnlySecretDir
}
```

> 注：`readOnlySecretDir` 已是包级变量（上面的实现即如此），测试用
> `t.Cleanup` 还原即可 —— 与 `pathguard` 包既有的 `withSecretRoots` 手法一致，
> 生产代码不留任何测试专用钩子。

- [ ] **Step 4: 运行测试确认通过**

Run: `go test ./config/ -count=1 -v`
Expected: PASS（新用例与既有 `TestPostgresDSNFileSourceErrors` 等都过）。

- [ ] **Step 5: 提交**

```bash
GIT_MASTER=1 git add config/config.go config/config_test.go
GIT_MASTER=1 git commit -m "fix(config): 只读 secret 供给的 DSN 文件不再要求可烧毁"
```

---

### Task 2: compose 改用 docker secret 供给 DSN

**Files:**
- Modify: `docker-compose.yml`
- Create: `init-pg-dsn-secret.sh`
- Modify: `.gitignore`

**Interfaces:**
- Consumes: `.env` 里的 `POSTGRES_USER` / `POSTGRES_PASSWORD` / `POSTGRES_DB`（已存在）
- Produces: `./secrets/pg_dsn`（0600，内容是完整 DSN）；compose 用 `ARTEX_PG_DSN_FILE=/run/secrets/pg_dsn` 供给

- [ ] **Step 1: 新建 secret 生成脚本**

新建 `init-pg-dsn-secret.sh`：

```sh
#!/bin/sh
# 生成 docker secret 文件（数据库连接串）。
#
# 为什么不用 environment 变量供给：容器配置面（docker inspect）会原样列出
# environment 的值，口令就在里面。secret 以文件挂载（/run/secrets/pg_dsn），
# 不进容器配置面，inspect 看不到。
#
# 口令来源仍是 .env（本地私有、不进版本库）。本脚本把 .env 里的三项拼成
# 完整 DSN 写进 secrets/pg_dsn，权限 0600。
set -eu

cd "$(dirname "$0")"

# 读 .env（不 source：避免执行其中的任意语句）
getenv() {
  key="$1"
  [ -f .env ] || return 1
  sed -n "s/^${key}=//p" .env | tail -n 1 | sed 's/^"//; s/"$//'
}

USER_NAME="${POSTGRES_USER:-$(getenv POSTGRES_USER || true)}"
PASSWORD="${POSTGRES_PASSWORD:-$(getenv POSTGRES_PASSWORD || true)}"
DB_NAME="${POSTGRES_DB:-$(getenv POSTGRES_DB || true)}"

[ -n "$USER_NAME" ] || { echo "缺少 POSTGRES_USER（.env 或环境变量）" >&2; exit 1; }
[ -n "$PASSWORD" ] || { echo "缺少 POSTGRES_PASSWORD（.env 或环境变量）" >&2; exit 1; }
DB_NAME="${DB_NAME:-artex}"

mkdir -p secrets
umask 077
printf 'postgres://%s:%s@postgres:5432/%s?sslmode=disable' "$USER_NAME" "$PASSWORD" "$DB_NAME" > secrets/pg_dsn
chmod 600 secrets/pg_dsn
echo "已生成 secrets/pg_dsn（0600），内容不在此显示。"
```

- [ ] **Step 2: 改 compose**

`docker-compose.yml`：

```yaml
services:
  postgres:
    image: postgres:16-alpine
    environment:
      POSTGRES_USER: ${POSTGRES_USER:-artex}
      POSTGRES_PASSWORD: ${POSTGRES_PASSWORD:?请在 .env 设置 POSTGRES_PASSWORD}
      POSTGRES_DB: ${POSTGRES_DB:-artex}
    volumes:
      - pgdata:/var/lib/postgresql/data
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U ${POSTGRES_USER:-artex} -d ${POSTGRES_DB:-artex}"]
      interval: 5s
      timeout: 3s
      retries: 10
    restart: unless-stopped

  artex:
    image: ghcr.io/wufufu770/reartex:${ARTEX_TAG:-latest}
    depends_on:
      postgres:
        condition: service_healthy
    # 去掉 CAP_SYS_PTRACE：Docker 默认 seccomp 配置对 ptrace 的放行条件是
    # 「容器持有 CAP_SYS_PTRACE」，去掉该能力即被拦 → 同 UID 进程也读不到
    # 父进程内存（/proc/<pid>/mem）。
    cap_drop:
      - SYS_PTRACE
    secrets:
      - pg_dsn
    environment:
      ARTEX_DISABLE_SHORT_CIRCUIT: ${ARTEX_DISABLE_SHORT_CIRCUIT:-}
      # 口令走 secret 文件，不进容器配置面：docker inspect 看不到。
      ARTEX_PG_DSN_FILE: /run/secrets/pg_dsn
      ANTHROPIC_API_KEY: ${ANTHROPIC_API_KEY:-}
      OPENAI_API_KEY: ${OPENAI_API_KEY:-}
      ARTEX_LLM_PROVIDER: ${ARTEX_LLM_PROVIDER:-}
      ARTEX_LLM_MODEL: ${ARTEX_LLM_MODEL:-}
      ARTEX_LLM_BASE_URL: ${ARTEX_LLM_BASE_URL:-}
      ARTEX_LLM_PROXY: ${ARTEX_LLM_PROXY:-}
    ports:
      - "8787:8787"
      - "127.0.0.1:8788:8788"
    volumes:
      # 把数据目录（含 jwt.key、SQLite 等）绑定到项目根目录的 data/，
      # 方便在宿主机直接查看/备份，容器重建也不丢。
      - ./data:/app/data
      # 把项目根目录的 skills 挂进容器（覆盖镜像内置的 /app/skills）；
      # 改 skill 无需重建镜像，容器读的就是宿主机这份。
      - ./skills:/app/skills
    restart: unless-stopped

secrets:
  pg_dsn:
    file: ${PG_DSN_SECRET_FILE:-./secrets/pg_dsn}

volumes:
  pgdata:
```

- [ ] **Step 3: 忽略 secrets 目录**

`.gitignore` 追加：

```
# 数据库连接串 secret（由 init-pg-dsn-secret.sh 生成，绝不进版本库）
secrets/
```

- [ ] **Step 4: 生成 secret 并起容器验证**

```bash
chmod +x init-pg-dsn-secret.sh
./init-pg-dsn-secret.sh
docker compose up -d --force-recreate artex
# 等就绪
for i in $(seq 1 60); do
  code=$(docker exec artex-artex-1 sh -c 'curl -s -m 3 -o /dev/null -w "%{http_code}" http://127.0.0.1:8787/' 2>/dev/null)
  [ "$code" = "200" ] && break
  sleep 3
done
```

Expected: 容器起来（`/api/health` 返回 200，`version` 字段有值）。

- [ ] **Step 5: 断言容器配置面不再有口令**

```bash
docker inspect artex-artex-1 --format '{{range .Config.Env}}{{println .}}{{end}}' | grep -c 'ARTEX_PG_DSN='
```
Expected: 输出 `0`。

```bash
docker inspect artex-artex-1 --format '{{range .Config.Env}}{{println .}}{{end}}' | grep 'ARTEX_PG_DSN'
```
Expected: 只出现 `ARTEX_PG_DSN_FILE=/run/secrets/pg_dsn`。

- [ ] **Step 6: 断言进程环境块仍然干净**

```bash
docker exec artex-artex-1 sh -c 'tr "\0" "\n" < /proc/1/environ | grep -c "^ARTEX_PG_DSN=postgres"'
```
Expected: 输出 `0`。

- [ ] **Step 7: 提交**

```bash
GIT_MASTER=1 git add docker-compose.yml init-pg-dsn-secret.sh .gitignore
GIT_MASTER=1 git commit -m "feat(deploy): 数据库口令改由 docker secret 供给，容器配置面不再可见"
```

---

### Task 3: ptrace 封闭验收

**Files:**
- Create: `verify-ptrace-blocked.sh`

**Interfaces:**
- Consumes: 容器内 `/proc/<pid>/mem` 的读取结果
- Produces: 退出码 0 = 封闭生效（读不到）；非 0 = 仍可读（活路径还在）

- [ ] **Step 1: 新建验收脚本**

`verify-ptrace-blocked.sh`：

```sh
#!/bin/sh
# 断言同 UID 进程读不到父进程内存。
#
# 为什么需要：/proc/<pid>/mem 能读出父进程的内存内容（含平台口令）。
# 平台层的路径/命令过滤挡不住这一条 —— 它不走工具入参。真正的闭合靠
# 部署面收紧：去掉 CAP_SYS_PTRACE 后，Docker 默认 seccomp 配置会拦掉
# ptrace 系统调用（该配置的 ptrace 放行条件是「容器持有 CAP_SYS_PTRACE」）。
#
# 判定：读父进程内存必须失败。读得到 = 活路径还在 = 非 0 退出。
set -u

target="${1:-1}"

if [ ! -r "/proc/$target/mem" ]; then
  echo "OK: /proc/$target/mem 不可读（权限或文件不存在），ptrace 封闭生效"
  exit 0
fi

# 文件可读不代表能读出内容：真正要试的是实际读一次。
if dd if="/proc/$target/mem" of=/dev/null bs=1 count=1 skip=0 >/dev/null 2>&1; then
  echo "FAIL: 仍能读 /proc/$target/mem —— ptrace 未封闭，同 UID 进程可取走父进程内存" >&2
  exit 1
fi

echo "OK: /proc/$target/mem 存在但读取失败，ptrace 封闭生效"
exit 0
```

- [ ] **Step 2: 进镜像**

`Dockerfile` 增两行（放在 `COPY verify-binary-version.sh` 之后）：

```dockerfile
COPY verify-secret-env.sh /usr/local/bin/verify-secret-env.sh
COPY verify-ptrace-blocked.sh /usr/local/bin/verify-ptrace-blocked.sh
RUN chmod +x /app/artex /app/start.sh /usr/local/bin/verify-binary-version.sh \
    /usr/local/bin/verify-secret-env.sh /usr/local/bin/verify-ptrace-blocked.sh
```

- [ ] **Step 3: 在运行中的容器里验**

```bash
docker exec artex-artex-1 /usr/local/bin/verify-ptrace-blocked.sh 1
```
Expected: 退出码 0，输出 `OK: ...`。

- [ ] **Step 4: 反证：去掉 cap_drop 后应失败**

临时把 compose 里的 `cap_drop` 段注释掉，`docker compose up -d --force-recreate artex`，再跑 Step 3 的命令：
Expected: **非 0 退出**，输出 `FAIL: 仍能读 /proc/<pid>/mem ...` —— 证明这条验收真的有鉴别力。

恢复 compose（`git checkout -- docker-compose.yml`），重建容器，Step 3 再次通过。

- [ ] **Step 5: 确认没有功能回归（浏览器工具仍可用）**

```bash
docker exec artex-artex-1 sh -c 'chromium --version || npx playwright --version' 2>&1 | tail -2
```
Expected: 有版本号输出（说明容器里的浏览器工具链没被 cap_drop 影响）。

- [ ] **Step 6: 提交**

```bash
chmod +x verify-ptrace-blocked.sh
GIT_MASTER=1 git add verify-ptrace-blocked.sh Dockerfile
GIT_MASTER=1 git commit -m "feat(deploy): 收紧 CAP_SYS_PTRACE 并加 ptrace 封闭验收脚本"
```

---

### Task 4: 环境块洁净性成为可回归的门

**Files:**
- Create: `verify-secret-env.sh`
- Modify: `Dockerfile`
- Modify: `.github/workflows/release.yml`

**Interfaces:**
- Consumes: `/proc/<pid>/environ` 的内容
- Produces: 退出码 0 = 进程树内无非空平台秘密；非 0 = 有泄露

- [ ] **Step 1: 新建验收脚本**

`verify-secret-env.sh`：

```sh
#!/bin/sh
# 断言应用进程树的环境块里没有非空的平台秘密。
#
# 为什么要有这道门：平台凭据一旦进了环境块，同 UID 的 agent 子进程一条
# `cat /proc/*/environ` 就能读走（os.Unsetenv 改不动内核保存的 exec 期环境块）。
# 现状是「启动时自检 + 人工核对」—— 一次性的结论没有门守着，下一次有人把
# 密钥改成 env 注入就悄悄回归。本脚本把那条结论固化成可执行的断言。
#
# 判定：进程树内任何进程的 environ 里出现「带值」的平台秘密键 = 泄露 = 非 0。
set -u

# 平台自身的凭据键。空值不算泄露（键在但没值，读走也没有秘密）。
SECRET_KEYS="ARTEX_PG_DSN ANTHROPIC_API_KEY OPENAI_API_KEY ARTEX_MASTER_KEY"

leak=0

# 遍历本进程及其所有后代
walk() {
  pid="$1"
  if [ -r "/proc/$pid/environ" ]; then
    for key in $SECRET_KEYS; do
      # 取该键的值长度：非 0 即视为带值
      n=$(tr '\0' '\n' < "/proc/$pid/environ" 2>/dev/null | grep "^${key}=" | head -n 1 | sed "s/^${key}=//" | wc -c)
      if [ "$n" -gt 1 ]; then
        echo "LEAK: pid=$pid 的环境块里 $key 仍带值" >&2
        leak=1
      fi
    done
  fi
  for child in $(cat "/proc/$pid/task/$pid/children" 2>/dev/null); do
    walk "$child"
  done
}

walk "${1:-1}"

if [ "$leak" -ne 0 ]; then
  echo "FAIL: 应用进程树的环境块内仍有平台秘密" >&2
  exit 1
fi

echo "OK: 应用进程树的环境块内无非空平台秘密"
exit 0
```

- [ ] **Step 2: 进镜像**

`Dockerfile` 的 `COPY`/`RUN chmod` 行按 Task 2 Step 2 的写法补上 `verify-secret-env.sh`。

- [ ] **Step 3: 在运行中的容器里验**

```bash
docker exec artex-artex-1 /usr/local/bin/verify-secret-env.sh 1
```
Expected: 退出码 0，输出 `OK: 应用进程树的环境块内无非空平台秘密`。

- [ ] **Step 4: 反证：把口令塞回环境后应失败**

```bash
docker exec -e ARTEX_PG_DSN=postgres://artex:x@postgres:5432/artex artex-artex-1 /usr/local/bin/verify-secret-env.sh 1
```
Expected: **非 0 退出**，输出含 `LEAK: pid=... 的环境块里 ARTEX_PG_DSN 仍带值`。

- [ ] **Step 5: 接进 release 冒烟**

`.github/workflows/release.yml` 的「冒烟刚发布的镜像」步骤，在版本核对通过之后追加：

```yaml
          # 交付前的环境块洁净性门：刚发布的镜像里，进程树的环境块内不得
          # 有非空的平台秘密。此前这条只是人工核对，没有门守着。
          - name: 验收环境块洁净性
            run: |
              docker exec "$cid" /usr/local/bin/verify-secret-env.sh 1
              docker exec "$cid" /usr/local/bin/verify-ptrace-blocked.sh 1
```

（注意放在 `docker rm -f "$cid"` **之前** —— 容器删了就验不到了。）

- [ ] **Step 6: 提交**

```bash
chmod +x verify-secret-env.sh
GIT_MASTER=1 git add verify-secret-env.sh Dockerfile .github/workflows/release.yml
GIT_MASTER=1 git commit -m "ci(release): 发布前验收环境块洁净性与 ptrace 封闭"
```

---

### Task 5: 账本口径同步

**Files:**
- Modify: 账本本体（本地工作区）
- Modify: 账本归档副本

- [ ] **Step 1: 改写「docker inspect 仍可见容器配置里的 DSN」这条**

```
2. ~~`docker inspect` 仍可见容器配置里的 DSN~~ → **已闭合（2026-10-28）**：数据库连接串
   改由 docker secret 文件供给（/run/secrets/pg_dsn），compose 不再把口令放进
   environment → `docker inspect` 的 Env 里只有文件路径，看不到口令。配套让
   readAndBurnDSNFile 容忍只读挂载（secret 不可删也不可清零，但该路径不在 agent
   工作区内）。
```

- [ ] **Step 2: 改写「/proc/<pid>/mem 读父进程内存」这条**

```
3. ~~`/proc/<pid>/mem` 读父进程内存仍可取到 DSN 字符串~~ → **已闭合（2026-10-28，部署面）**：
   compose 去掉 CAP_SYS_PTRACE 后，Docker 默认 seccomp 配置拦掉 ptrace 系统调用
   （该配置的 ptrace 放行条件是「容器持有 CAP_SYS_PTRACE」）→ 同 UID 进程也读不到
   父进程内存。验收脚本 verify-ptrace-blocked.sh 已进 release 冒烟（带反证：
   去掉 cap_drop 后该验收确实变红）。**边界**：宿主上的 root 仍可读容器内进程内存，
   属宿主权限面，不在本仓库范围。
```

- [ ] **Step 3: 补「环境块不变式没有门」这条**

```
平台环境块规则同样是字符串预筛（运行时才成形的路径仍能绕过），真正的闭合靠供给侧
—— 凭据根本不进进程环境块。这条供给侧已闭合（DSN 走 secret 文件、启动即读入即删），
本次再把它固化成**门**：verify-secret-env.sh 扫进程树断言无非空平台秘密，已进 release
冒烟 job，每次发布都验。
```

- [ ] **Step 4: 归档并核对**

复制账本到归档路径，`cmp` 校验一致。

---

## 验收（整个计划完成后）

```bash
# 本地部署形态
./init-pg-dsn-secret.sh
docker compose up -d --force-recreate artex
docker inspect artex-artex-1 --format '{{range .Config.Env}}{{println .}}{{end}}' | grep -c 'ARTEX_PG_DSN='   # 期望 0
docker exec artex-artex-1 /usr/local/bin/verify-secret-env.sh 1                                        # 期望 0
docker exec artex-artex-1 /usr/local/bin/verify-ptrace-blocked.sh 1                                   # 期望 0
go test ./config/ -count=1
```

**回退路径**（写在 README 的运维段）：把 compose 的 `secrets:` 与 `ARTEX_PG_DSN_FILE`
换回 `ARTEX_PG_DSN: postgres://...` 即可回到旧形态（代价是 `docker inspect` 又能看到口令）。

**本计划不覆盖**：per-task 隔离、路径解析、密钥加密、签名与 provenance。

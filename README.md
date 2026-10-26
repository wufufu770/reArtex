<div align="center">

# ARTEX

AI 自主渗透测试系统（Go 后端 + Next.js 前端）

🌐 **在线 Demo**： [https://artex-demo.vercel.app/](https://artex-demo.vercel.app/)

> **reArtex = ARTEX 安全加固分支。** 本仓库在上游基础上修了一批 agent 越权与失效开放（fail-open）缺陷，核心是把安全约束从「可热改的 DB / settings」移到「代码常量 + 编译期」。改动清单与验证证据见下方 [安全加固](#安全加固)。

</div>

---

## 安全加固

本仓库相对上游的改动。

### 为什么改

原实现里，**决定 agent 能做什么的那几张表和设置项，本身可被持有 JWT 的人改**——`settings.intercept_enabled_tools` 决定哪些工具进拦截器，`intercept_rules` 是规则本身。一次 `PUT /api/settings` 或一条 `DELETE` 就能把整套防线整体关闭，且无审计留痕。

所以加固的第一原则是：**安全边界不能挂在任何可热改的存储上**。本仓库所有新增约束都写进 Go 源码，关闭途径只剩编译期开关 + 重启。

### 修复的缺陷

以下 8 项均已直读源码逐条验证属实（非推测），编号沿用《ARTEX限制强化方案-终版V2》。

| 编号 | 缺陷 | 原行为 | 现行为 |
|---|---|---|---|
| G-01① | 平台工具绕过白名单 | 改平台配置的工具若不在 `intercept_enabled_tools` 名单内，在 `IsToolEnabled` 处直接放行，规则根本轮不到求值 | 短路表插在 `applyIntercept` **最前面**，先于 `IsToolEnabled` |
| G-01③ | stdio MCP 无任何白名单 | 命令任意指定，等价于任意第三方代码执行；包名可抢注、无版本 pin 可供应链翻转 | 拒 17 个包执行器（`npx`/`uvx`/`bunx`/`pnpx`/`pipx`/`dlx`/`docker`/`podman`/`sh`/`bash`/`zsh`/`dash`/`python*`/`perl`/`ruby`/`node`），且命令须 PATH 解析成功 |
| G-02①-a | agent 可读平台自身凭据 | worker 的 Bash 与 server 同 UID，可读 `/app/jwt.key` 与 server 原始环境块（含 `ANTHROPIC_API_KEY`、`ARTEX_PG_DSN`） | 硬拦平台签名密钥、MITM CA 私钥与 `/proc/*/{environ,cmdline,mem}`；控制面凭据读入即从进程环境抹除；DSN 另可走 `ARTEX_PG_DSN_FILE`，读入即清零并删除文件，使进程环境块内不再有秘密 |
| G-03 | 拦截规则可被 JSON 转义绕过 | `Match` 只对原始入参字节做正则，`"r\u0065ad /app/jwt.key"` 匹配不上 `read` | 匹配前 JSON 深解码，raw 与 decoded 双形态都试 |
| G-04④ | 工具表故障时全量放行 | `ListTools` 失败 → 打条日志 → `return tools`，DB 一抖动所有 agent 立刻拿到完整工具面 | 沿用上一份已验证快照；冷启动无快照时降级为 SDK 基础件 8 件 |
| G-07 | 工作区符号链接可越界 | `wsResolve` 只做词法 `Clean`，注释称"爬不出根"——但那对软链不成立；`saveUpload` 跟随符号链接 | `EvalSymlinks` 复核真实路径 + `O_NOFOLLOW` |
| G-09 | steer 消息无上限 | 纠偏消息直入 worker 上下文且不过闸，队列无界 | 限 2000 字符、队列 cap 10 |
| G-04①② | 模型兜底审批 fail-open | 判官超时、报错或输出不可解析时回落 `allow` —— 让判官失效即可绕过全部规则；ask 类规则默认无超时，审批可无限等待 | 失败/超时一律回落 `deny`；ask 默认 300s 超时后 `deny`；前端表单初值与后端默认对齐，避免保存一次就把 `allow` 写回 |

### 改动清单

第一批（相对上游 `e6ec569`）：

```
a  guard/shortcircuit.go          +160   代码常量短路表（新文件）
a  guard/shortcircuit_test.go     +102
a  intercept/normalize_test.go    +51
a  server/nofollow_unix.go        +10    O_NOFOLLOW 常量
a  server/nofollow_windows.go     +7
m  intercept/intercept.go         +57/-7
m  server/mcpdiscover.go          +43/-3
m  server/assembly.go             +36/-2
m  server/workspace.go            +32/-1
m  guard/guard.go                 +16
m  server/engine.go               +10
m  docker-compose.yml             +1     env 开关透传
```

第二批（相对 `1b29664`）：30 files changed, 1899 insertions(+), 34 deletions(-)

```
a  pathguard/pathguard.go         +136/-0   真实路径解析 + 控制面秘密拒绝列表（新文件）
a  pathguard/pathguard_test.go    +153/-0
a  netguard/netguard.go           +166/-0   拨号期出网目的地守卫（新文件）
a  netguard/netguard_test.go      +153/-0
a  agent/toolpolicy.go            +42/-0    工具执行前的平台侧安全策略（新文件）
a  agent/toolpolicy_test.go       +110/-0
a  server/skill_trust.go          +108/-0   skill 信任分档（新文件）
a  server/skill_trust_test.go     +129/-0
a  server/skill_demote.go         +108/-0   agent 写入的 skill 正文降权注入（新文件）
a  server/skill_demote_test.go    +108/-0
a  config/config_test.go          +102/-0
a  agent/provider_env_test.go     +41/-0
a  db/pgdsn_env_test.go           +40/-0
a  server/customtool_env_test.go  +39/-0
a  server/platform_tools_test.go  +47/-0
a  mcphttp/client_sse_test.go     +42/-0
m  config/config.go               +100/-2   ARTEX_PG_DSN_FILE 来源 + 启动自检
m  db/db.go                       +23/-1    DSN 读入即从进程环境抹除
m  agent/provider.go              +33/-2    API key 读入即抹除
m  agent/assembly.go              +5/-3     策略装饰器包裹全量工具
m  server/customtool.go           +44/-2    自定义脚本改白名单继承宿主环境
m  server/platform_tools.go       +49/-0    MCP 定义写库侧校验
m  mcphttp/client.go              +29/-7    MCP SSE 端点同源校验
m  guard/shortcircuit.go          +32/-12   environ 规则覆盖 glob 与展开形态
m  server/skill_usage.go          +9/-3     skill 计量接入信任档
m  server/server_mgmt.go          +6/-0     skill 写操作同步信任档
m  server/server.go               +14/-0    启动自检调用点
m  server/assembly.go             +1/-1     计量链路传入 skill 目录
```

被转人工审批的 7 件平台工具：`create_mcp`、`update_mcp`、`create_custom_tool`、`update_custom_tool`、`create_skill`、`update_skill_file`、`delete_assets_by_host`（60 秒不确认即拒绝）。**这是有意的行为变更**——auto agent 不再能自主建 MCP / skill。

### 审计过程中额外发现并修掉的三个问题

1. **MITM CA 私钥未受保护**：原注释声称覆盖 `*.pem`，正则里根本没有；而容器里确实存在 `/app/data/traffic/_ca/mitmproxy-ca.pem`（MITM CA 私钥）。已补进拦截表。
2. **规则过宽误伤渗透正常工作**：初版按目录前缀一刀切（`/app/data/**/*.key`、裸 `/\.env\b`）。但 agent 工作目录正是 `/app/data/sessions/<key>`，工作区就落在 data 目录里——**目标的 `.env` / `.key` / `jwt.key` 全被误拦**，而 `.env` 恰恰是渗透最常要读的东西。已改为按具体凭据文件列举。
3. **应急开关是假的**：注释写着 `ARTEX_DISABLE_SHORT_CIRCUIT=1` 可停用短路表，代码里从没读过这个环境变量。已实现（`init()` 读取，启动日志留痕），并把开关内核抽成可测函数。

### 已知残留（未做）

- **短路表是入参字符串预筛，不是路径解析级强制。** `cat $(realpath ../../../jwt.key)` 这类经 shell 展开后才现形的路径仍可绕过。真正闭合需要执行前的 `realpath` 校验与环境变量白名单。
- **平台环境块规则同样是字符串预筛。** 运行时才成形的路径（`find /proc -name environ -exec cat {} \;`）仍能绕过。闭合靠的是供给侧 —— 凭据根本不进进程环境块，见下一节的 `ARTEX_PG_DSN_FILE`。
- **`/proc/<pid>/mem` 读父进程内存仍可取到 DSN 字符串**（需 ptrace 权限）。本批未处理。
- **`docker inspect` 仍可见容器配置里的 DSN**（需宿主 / docker socket 权限）。同理 `docker exec` 拉起的进程会继承容器配置面的 env —— 但 agent 的 Bash 是 artex 的子孙进程，继承的是 artex 的（干净的）环境块，不在此列。
- **平台环境块规则作用在整个工具入参 JSON 上**，故「把含 `/proc/…/environ` 字样的内容写进文件」也会被拦。
- **`llm_profiles.api_key` 仍以明文存于数据库**（需数据库读权限才能取到）。
- G-17 审计事件骨架与 digest 链（需改表结构）—— 短路表 deny 目前仍只落内存切片，进程退出即失。
- G-09 ask 队列治理（需 per-agent 聚合计数）。
- G-04④ 的「拒绝创建新 run」目前以最小工具面降级代替，不是硬拒绝。
- **尚未做运行时端到端验证**：单测覆盖了函数逻辑，但未通过真实 agent 会话发起工具调用确认短路表在链路中生效。

### 应急回滚

短路表整体停用（重启生效，无需重新构建镜像）：

```bash
# docker-compose.yml 里设，或临时注入
ARTEX_DISABLE_SHORT_CIRCUIT=1 docker compose up -d
```

启动日志出现 `[guard] ⚠ 短路表已停用` 即生效。接受 `1/true/yes/on`。停用后平台退化为「仅 DB 规则」防线——而 DB 规则本身可被持 JWT 者热改。

### 平台凭据不出现在进程环境块

`ARTEX_PG_DSN` 经容器环境变量下发时，会留在进程的 exec 期环境块里，而 `/proc/<pid>/environ` 对同 UID 的 agent 子进程可读。若希望凭据不出现在环境块，改用 `ARTEX_PG_DSN_FILE` 指向一次性文件：

````yaml
services:
  artex:
    entrypoint:
      - /bin/sh
      - -c
      - >-
        umask 077; mkdir -p /run/artex;
        printf '%s' "$$ARTEX_PG_DSN" > /run/artex/pg_dsn;
        unset ARTEX_PG_DSN;
        exec /app/start.sh "$$@"
      - --
    # 必须显式写出：覆盖 entrypoint 后 compose 不再带镜像的 CMD，
    # 而 wrapper 的 "$@" 就空了 —— 结果只剩默认的 :8787，录制代理 :8788 不会起。
    command: ["-addr", ":8787", "-proxy", ":8788"]
    environment:
      ARTEX_PG_DSN_FILE: /run/artex/pg_dsn
````

三点说明：

- **`unset` 必须在 `exec` 之前。** `unset` 只改 shell 内部变量表，改不动内核保存的 exec 期环境块；而 `exec` 会用当前环境**重建**进程映像，所以 exec 出来的进程环境块里才没有 DSN。顺序反了无效。
- **进程启动后会读入该文件并立即清零、删除它**，磁盘上不留副本。
- **`command` 不能省。** 覆盖 `entrypoint` 后 compose 不再带镜像的 `CMD`，wrapper 的 `"$@"` 会是空的。

启动时平台会自检 `/proc/self/environ` 是否仍残留平台凭据并给出结论（`[security] 平台凭据未残留在进程环境块中`）。若希望残留时直接拒绝启动，设 `ARTEX_REQUIRE_ENV_SCRUB=1`。

---

## 截图预览

> 完整交互见[在线 Demo](https://artex-demo.vercel.app/)。

| 仪表盘（总览 / Token 消耗 / 活动流） | 任务列表 |
| :---: | :---: |
| ![仪表盘](screenshots/dashboard.png) | ![任务](screenshots/tasks.png) |

| 任务 · 执行过程（会话 / 工具调用） | 探索链路 |
| :---: | :---: |
| ![执行过程](screenshots/sessions.png) | ![探索链路](screenshots/graph.png) |

| 发现 | 资产 |
| :---: | :---: |
| ![发现](screenshots/findings.png) | ![资产](screenshots/assets.png) |

| 资产覆盖图（力导向布局 · 已测高亮 · 节点折叠展开） |
| :---: |
| ![资产覆盖图](screenshots/assets_test.png) |

| 流量录制 | 人在环路对话 |
| :---: | :---: |
| ![流量](screenshots/traffic.png) | ![对话](screenshots/chat.png) |

| Agent 管理 | LLM 配置 |
| :---: | :---: |
| ![Agent](screenshots/agents.png) | ![LLM](screenshots/llm.png) |

| 拦截审批 | 后端日志 |
| :---: | :---: |
| ![拦截](screenshots/intercept.png) | ![日志](screenshots/logs.png) |


---

## 审批记录详情

全局「审批记录」、任务内「拦截审批」及对话中的审批卡片均支持展开查看详情。展示结构参考
[AegisHook 的审批详情组件](https://github.com/RuoJi6/AegisHook/blob/main/web/src/components/CallDetail.vue)，沿用 ARTEX 的组件和主题：


## 资产同步（ScopeSentry）

支持从 [ScopeSentry](https://github.com/Autumn-27/ScopeSentry) 直接同步资产数据，免去重复收集：

- 在「**资产同步**」页填 ScopeSentry 的地址与 API Key，接入数据源；
- 按**项目**或**任务**维度选择要同步的目标与资产类型（域名 / 子域 / IP / 端口 / 站点 / 端点…）；
- 一键导入并按公司资产范围归并，直接进入 ARTEX 的资产图供 agent 探索使用。

---

## 安装

> 依赖数据库 **PostgreSQL**；探索需配置 **LLM**（`ANTHROPIC_API_KEY` 或 `OPENAI_API_KEY`，也可在 UI 里配）。

### 方式一：一键安装脚本（推荐）

```bash
git clone https://github.com/Autumn-27/ARTEX.git
cd ARTEX
./install.sh
```

脚本会：检测 / 自动安装 Docker → 让你选 **① 全部 Docker** 或 **② 本地编译运行**：

- **① 全部 Docker**：填一个 Postgres 密码（可回车随机）→ 自动写 `.env` → `docker compose up -d`。
- **② 本地运行**：选数据库（连已有 / 用 Docker 起一个）→ 生成 `config.json` → `go` 编译内嵌单二进制 → 启动。

装好后打开 **http://localhost:8787**（首次进入 `/setup` 设置管理员密码）。

### 方式二：Docker Compose（手动）

```bash
git clone https://github.com/Autumn-27/ARTEX.git
cd ARTEX
cp .env.example .env          # 填 POSTGRES_PASSWORD、可选 ANTHROPIC_API_KEY
docker compose up -d          # 拉取 autumn27/artex 镜像 + postgres
# → http://localhost:8787
```

镜像已含常用工具（ripgrep/curl/vim/npm/nmap…）；`./skills` 与 `./data` 以绑定挂载持久化。

远程 MCP 可在系统设置中选择 `http`（Streamable HTTP）或 `sse`（旧版 SSE）。
旧版 SSE 服务通常使用 `GET /sse` 建立事件流，再通过服务返回的
`/message?sessionId=...` 接收 JSON-RPC 请求；配置时将 URL 填为 `/sse`，请求头按
`Authorization=Bearer <token>` 填写。

### 方式三：下载预编译二进制（Releases）

到 [Releases](https://github.com/Autumn-27/ARTEX/releases) 下载对应平台的 zip，解压后得到 `artex` + `start.sh`（Windows 为 `start.bat`）+ `skills/` + `config.example.json`：

```bash
cp config.example.json config.json   # 填好 database 连接
./start.sh                           # → http://localhost:8787
```

> 请用 `start.sh` / `start.bat` 启动，而不是直接跑 `./artex`。它是个守护脚本：程序退出后按退出码决定是否重新拉起，**页面上的[一键更新](#方式一页面一键更新推荐)靠它完成换装**。直接运行 `./artex` 时更新完就不会被拉起了。
> 后台常驻：`nohup ./start.sh >artex.log 2>&1 &`。

### 方式四：从源码编译单二进制

```bash
# 1) 前端静态导出
cd web && npm ci && npm run build:static && cd ..
# 2) 拷进内嵌目录
cp -r web/out server/webui/dist
# 3) 编译（-tags embedui 才内嵌前端）
CGO_ENABLED=0 go build -tags embedui -o artex ./cmd/artex
./start.sh
```

### 方式五：构建跨平台 Release 压缩包

`build.sh` 会先构建并嵌入前端，再使用 Go linker 去除调试信息，并将发布文件压缩为 zip。Release 模式默认生成 Linux amd64/arm64、macOS amd64/arm64 和 Windows amd64 的 zip 包：

```bash
./build.sh --release
# 产物：dist/artex-0.3.3-*.zip
```

UPX 自解压二进制可能与部分 Linux 内核、虚拟化环境或安全策略不兼容，因此默认不启用。可用 `ARTEX_TARGETS` 自定义目标；确认目标运行环境兼容时，可显式传入 `--upx` 进一步缩小二进制：

```bash
ARTEX_TARGETS=linux/amd64,windows/amd64 ./build.sh --release
./build.sh --target linux/amd64 --upx
```

---

## 更新升级

> 升级只换程序、不动数据：Postgres 数据卷 `pgdata`、`./data`（jwt.key / SQLite 等）、`./skills` 都会保留。**数据库迁移无需手动执行**——`artex` 每次启动会幂等重跑 `schema.sql`（含 `ADD COLUMN` / `CREATE INDEX IF NOT EXISTS`），即“重启即迁移”。升级前仍建议先备份 `./data` 与数据库。

### 方式一：页面一键更新（推荐）

在 **系统配置** 页（侧边栏「系统配置」→ `/system/settings`）的**版本与更新**卡片里，可以直接检查并安装新版本，无需登录服务器。

点「更新」后：下载当前平台的发布包 → 比对 Release 的 `SHA256SUMS` → 用 `-h` 冒烟测试新二进制 → 暂存为 `artex.new` → 程序退出，由 `start.sh` / `start.bat` 重新拉起并完成换装。页面会自动等到新版本上线后刷新。

- **失败不会留下坏程序**：校验或冒烟不通过就丢弃暂存件、继续跑当前版本；换装后的新版若连续 3 次启动失败，会自动回滚到 `artex.old`（失败的那个留作 `artex.failed` 供排查）。
- **随时可回退**：上一版本保留为 `artex.old`，卡片上有「回滚到上一版本」。注意数据库结构不会回退。
- **更新会中断正在运行的任务**——更新即重启，请在空闲时进行。
- **开发构建不给更新**：版本号是 `dev` 或 `git describe` 带后缀时禁用，避免正式版覆盖掉本地调试的二进制。
- **Docker 下只换程序、不换镜像**：镜像里的 playwright / nmap 等工具链不会跟着升级，且 `docker compose up -d` 重建容器后会退回镜像自带的版本。要连镜像一起升级仍请用 `docker compose pull artex && docker compose up -d artex`。
- 访问 GitHub 需要代理时，在同一页面配置**全局代理**即可，更新链路会走它。更新只从 GitHub 域名下载并强制 HTTPS。

### 方式二：一键更新脚本

```bash
cd ARTEX
./update.sh
```

脚本先可选 `git pull` 拉取最新代码，再让你选 **① Docker 更新** 或 **② 本地编译更新**（与 `install.sh` 对应）：

- **① Docker**：可指定目标镜像 tag（回车沿用 `.env` 的 `ARTEX_TAG`，缺省 `latest`）→ `docker compose pull` → `docker compose up -d`（换新镜像重启即自动迁移）。
- **② 本地**：重建前端静态产物 → 重新编译 `./artex`（完成后重启进程生效）。

### 方式三：Docker Compose（手动）

```bash
cd ARTEX
git pull                       # 更新 compose / 脚本（可选）
# 指定版本：在 .env 设 ARTEX_TAG=v0.2.0；不设则用 latest
docker compose pull artex
docker compose up -d artex     # 换新镜像重启 → 自动迁移 schema
docker image prune -f          # 清理旧镜像（可选）
```

### 方式四：预编译二进制（Releases）

到 [Releases](https://github.com/Autumn-27/ARTEX/releases) 下载新版本 zip，停掉旧进程后覆盖 `artex` 与 `skills/`（保留你的 `config.json` 与 `data/`），重启即可：

```bash
cp -r <解压目录>/skills ./ && cp <解压目录>/artex ./
./start.sh
```

### 方式五：从源码编译

```bash
git pull
cd web && npm ci && npm run build:static && cd ..
cp -r web/out server/webui/dist
CGO_ENABLED=0 go build -tags embedui -o artex ./cmd/artex
# 重启 ./start.sh
```

---

## 配置

**数据库**（`config.json`，或用环境变量 `ARTEX_PG_DSN` 覆盖）：

```json
{
  "database": {
    "host": "127.0.0.1", "port": 5432,
    "user": "artex", "password": "yourpass",
    "dbname": "artex", "sslmode": "disable"
  }
}
```

**LLM**：`export ANTHROPIC_API_KEY=sk-...`（或 `OPENAI_API_KEY`），也可在 UI 的「LLM 配置」页填写。
可选：`ARTEX_LLM_PROVIDER` / `ARTEX_LLM_MODEL` / `ARTEX_LLM_BASE_URL` / `ARTEX_LLM_PROXY`。

**并发**：每个任务的 work agent 数在「系统设置」里配置（默认 3）。

**常用参数**：`./start.sh -addr :8787 -proxy :8788`（`-addr` 前端+API，`-proxy` 流量录制代理）。启动脚本会把参数原样透传给 `artex`。

### 反向代理部署（HTTPS / 只开放 443）

前端和 API/SSE 都由同一个后端端口（默认 `:8787`）提供，实时活动流默认走**同源**地址，因此**无需配置 `NEXT_PUBLIC_SSE_BASE`**，公网只开放 443、把 8787 留在内网即可。

SSE 是长连接 + 持续推送，反代**必须关闭缓冲**，否则浏览器能连上却收不到事件（表现为活动流一直转圈）。Nginx 示例：

```nginx
server {
    listen 443 ssl;
    server_name your.domain.com;
    # ssl_certificate / ssl_certificate_key ...

    location / {
        proxy_pass http://127.0.0.1:8787;
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-Proto $scheme;

        # SSE 关键项：关缓冲、长超时、HTTP/1.1
        proxy_buffering off;
        proxy_cache off;
        proxy_read_timeout 3600s;
        proxy_http_version 1.1;
        proxy_set_header Connection "";
    }
}
```

> 仅当 SSE 需要走与页面不同的来源（如独立子域）时，才在**构建期**设置 `NEXT_PUBLIC_SSE_BASE`（该变量在 `next build` 时固化进静态包，容器运行时再设无效）。

---



## 开发

### 手动漏洞复测

任务详情的「复测」页签可分页选择本任务的漏洞、查看历次结论和证据，并手动发起复测。启动后保留当前页签，显示转圈图标和「复测中」；确认修复后同步更新漏洞状态。

在漏洞列表每行操作区点击「复测」，或在漏洞详情的「漏洞复测」区域点击「发起复测」，填写可选的修复版本、测试条件或限制，系统会创建独立的复测 Agent 会话，启动后保留当前页面。列表的平铺、按任务分组和资产视图均支持该入口；复测运行时显示转圈图标和「复测中」，需要查看时点击进入对应会话，结束后恢复「复测」。复测无需重新启动原扫描任务，结论分为「仍可复现」「已修复」「无法确认」，每次的结论、证据和会话链接保存在漏洞详情中。

新版后端首次启动会预置可编辑的「漏洞复测」（`retester`）Agent，可在 Agent 管理中配置提示词、LLM、运行预算和工具。默认使用其绑定的 LLM，未绑定则使用全局激活配置。复测会话成功完成且结论为「已修复」时，系统自动将漏洞处置状态改为「已修复」；执行中、失败、停止或其他结论保留原状态。原始证据和报告始终保留。也可在状态下拉菜单中手动选择「已修复」。同一漏洞正在复测时复用已有会话，停止、失败或服务重启后可重新发起。

本版历史记录通过漏洞详情和会话查看，暂未纳入漏洞报告导出或任务归档包，也未自动关联流量包。演示模式只生成明确标注的模拟记录，不请求真实目标。

### 本地运行与测试

```bash
./dev.sh    # 后端(:8787) + 流量代理(:8788) + 前端 next dev(:5173) → http://localhost:5173
```

- 后端：`go run ./cmd/artex`（不带 `-tags embedui` 则不内嵌前端）
- 前端：`cd web && npm run dev`（`/api` 反代到后端，带热更新）
- 测试：`go test ./...`
- Mock 预览（无后端）：`cd web && NEXT_PUBLIC_MOCK=1 npm run dev`

---

## 系统技术架构

ARTEX 是一套 **LLM 多 agent 驱动的自主渗透系统**：Go 单体后端（内嵌 Next.js 前端）+ PostgreSQL，agent 能力由 [`norma`](https://github.com/Autumn-27/norma) SDK 提供（`agentcore` / `tool` / `permission` / `harness` / `memory` / `transcript`）。核心是**双图架构**，以及围绕它的两条自主性机制：**worker 间过程级信息交换**与 **planner 多轮共享 todolist 稳定攻击链路**。

### 总体分层

```mermaid
flowchart TB
  subgraph FE["前端 Next.js（go:embed 内嵌单二进制）"]
    UI["仪表盘 · 任务 · 资产 · 覆盖图 · 流量 · 工作空间 · 系统配置"]
  end
  subgraph SRV["server（Go net/http）"]
    API["REST /api/*　JWT 鉴权　SSE"]
    ENG["engine 调度循环"]
    MGR["Manager　任务/引擎/store 生命周期"]
  end
  subgraph AG["agent（norma SDK）"]
    GO["goals　目标分解 + 提取范围"]
    PL["planner　规划者（唯一意图生成者）"]
    WK["worker　执行者 ×N"]
    MA["mainagent　人在环路"]
  end
  subgraph DB["PostgreSQL"]
    AGRAPH["资产图　assets / companies / task_scope"]
    EGRAPH["探索图　exploration_nodes / anchors / activity"]
  end
  subgraph SUB["支撑子系统"]
    PROXY["流量记录代理　MITM + CA 留痕"]
    GUARD["guard / intercept　工具审批门"]
    ENR["enrich　DNS / HTTP 异步补全"]
    EXT["MCP · skills · memory · report"]
  end

  UI -->|HTTP| API
  API --> MGR --> ENG
  ENG --> PL
  ENG --> WK
  API --> MA
  API --> GO
  PL --> DB
  WK --> DB
  MA --> DB
  GO --> DB
  WK -->|"Bash / HTTP 全程留痕"| PROXY
  WK --> GUARD
  WK --> ENR
  PL -.-> EXT
  WK -.-> EXT
  MA -.-> EXT
```

| 层 | 职责 |
| --- | --- |
| **前端** | Next.js 静态导出，`go:embed` 内嵌进单二进制；可视化任务/资产/探索链路/覆盖图，人在环路对话 |
| **server** | `net/http` 路由 + JWT 鉴权 + SSE；`Manager` 托管任务、引擎、DB store 的生命周期 |
| **engine** | 每任务一个 `plannerLoop` + N 个 worker goroutine；意图领取、超时/暂停/drain |
| **agent** | goals / planner / worker / mainagent，`ToolSet` 把双图暴露成 LLM 工具 |
| **db** | 双图的 Postgres 落地（pgx）；schema 随 `go:embed` 每次启动幂等建表 |
| **支撑** | 记录型 MITM 代理、审批门、异步补全、MCP/技能/记忆/报告 |

### 双图架构：探索图 + 资产图

系统把「**目标是什么**」和「**测到了什么程度**」拆成两张相互独立、又通过锚点相连的图：

- **资产图（Asset Graph，全局共享）**：跨任务同一份的资产真值库。节点为 `root_domain / subdomain / ip / service / app / endpoint`，归属公司；域名→子域→服务→端点的父子关系与去重 key 全部由程序计算，agent 只提交原始信息。
- **探索图（Exploration Graph，每任务独立）**：一次任务的“思考与推进”过程。节点为 `goal（目标）/ intent（意图）/ fact（事实）/ finding（漏洞）/ hint（提示）`，靠 `spawns / derived_from / yields / proves` 等边连成**血缘链**，回答“哪个方向派生自哪些事实、产出了什么”。
- **两图靠锚点相连**：`exploration_anchors(node_id, asset_id)` 把意图/事实/漏洞锚定到具体资产上——于是既能从“探索方向”看它打的是哪些资产，也能从“某个资产”反查它在本任务被哪些意图测过、得出过哪些事实。这也支撑了**资产测试覆盖度**与**资产覆盖图**（范围内资产 + 已测高亮）。

```mermaid
flowchart LR
  subgraph EG["探索图（每任务独立 · 推进链）"]
    direction TB
    G["goal 目标"]
    I1["intent 意图 A"]
    F1["fact 事实"]
    I2["intent 意图 B"]
    FD["finding 漏洞"]
    G -->|spawns| I1
    I1 -->|yields| F1
    F1 -->|derived_from| I2
    I2 -->|proves| FD
  end
  subgraph AG["资产图（全局共享 · 真值库）"]
    direction TB
    RD["root_domain"]
    SD["subdomain"]
    SV["service"]
    EP["endpoint"]
    RD --> SD --> SV --> EP
  end
  I1 -. anchor .-> SD
  F1 -. anchor .-> SV
  I2 -. anchor .-> EP
  FD -. anchor .-> EP
```

> 分工：**planner** 读探索图态势、判目标、只在有未覆盖的新方向时派**意图**进 frontier；**worker** 领**一条意图**、用真实工具执行、把新资产/事实/漏洞写回两图后即停。资产图是共享事实，探索图是每任务的推进链。

### 引擎与意图生命周期（一次探索的闭环）

引擎是**事件驱动**的闭环：图一变就唤醒 planner，planner 派意图，worker 领意图执行并写回，写回又触发下一轮——直到目标被证明（`prove_goal`）。

```mermaid
sequenceDiagram
  autonumber
  participant EV as 图变更 debounce
  participant P as planner
  participant FR as frontier 意图队列
  participant W as worker
  participant PX as 记录代理
  participant DB as 双图 + activity

  EV-->>P: 唤醒
  P->>DB: 读态势(graph_overview 预取 + coverage/scope)
  P->>FR: 派 0..N 个意图(带 asset_ids)
  Note over P,FR: 大多数唤醒派 0 个——无新方向即结束
  W->>FR: claimNext 领一条意图
  W->>DB: 取意图 asset_ids 的原始资产作为初始信息
  W->>PX: 真实工具执行(Kali / Bash / HTTP)
  PX-->>W: 响应(全程留痕 + CA 验证)
  W->>DB: 写回 fact / asset / finding + 每步 activity
  DB-->>EV: 图变更
  EV-->>P: 再次唤醒(闭环)
```

### worker 间的过程级信息交换

一次深入的探索里，很多有价值的观察（某个报错、某段响应、某个隐藏参数）出现在一个 worker 的**执行过程**中，却未必被写成正式 fact。为避免重复劳动、让链路上的 worker 能站在彼此的肩膀上，worker 具备**跨 work 检索过程**的能力：

- `search_all_worker_traces(q)`：在**本任务其他 work 的执行过程**里按关键字检索（自动排除自己这条意图的步骤），命中项带 `intent_id`；
- `list_worker_traces` / `get_worker_trace(intent_id, step_ids=[…])`：先看有哪些 work 跑过，再取某个 work 具体几步的完整内容做细节交换。

这样即便探索图上还没有对应的 fact，后续 worker 也能复用他人过程中的观察——**信息在 worker 之间以“执行过程”为粒度流动**，而边界不变（每个 worker 仍只做自己领到的那条意图）。

```mermaid
flowchart LR
  WA["worker A（意图 #12）"] -->|"每步 activity"| ACT[("探索图 · activity 过程库")]
  WB["worker B（意图 #34）"] -->|"每步 activity"| ACT
  WC["worker C（意图 #56）"] ==>|"1) search_all_worker_traces(q)"| ACT
  ACT ==>|"2) 命中 A/B 的步骤（排除自己）"| WC
  WC ==>|"3) get_worker_trace(id, step_ids)"| ACT
  ACT ==>|"4) 返回完整过程内容"| WC
```

### planner 多轮共享 todolist → 稳定的攻击链路

真实攻击链往往是**有前后依赖的多步序列**（如：发现注入点 → 拿到凭据 → 横向 → 提权），一次性把这些并行派下去只会乱套。planner 因此持有一份**按任务保留、跨唤醒共享的规划待办（todolist）**：

- planner 是事件驱动的——图一变就被唤醒，但**每次唤醒是全新会话**；共享的 todolist 让它把一条串行利用链**记录一次**、然后在后续多轮里**按依赖逐步派意图**，而不是把整条链在一轮里全部前置展开；
- 每轮只对「前置步骤已完成、其依赖的 fact 已存在」的下一步派意图，并随进展更新清单（把已被 fact 满足的步骤标完成）。

```mermaid
flowchart TB
  subgraph TODO["共享 todolist（按任务保留 · 跨唤醒常驻）"]
    direction LR
    T1["1 注入点　[已完成]"]
    T2["2 取凭据　[进行中]"]
    T3["3 横向　[待前置]"]
    T4["4 提权　[待前置]"]
    T1 -.前置满足.-> T2 -.-> T3 -.-> T4
  end
  R1["第 1 轮唤醒　派意图①"] --> T1
  R2["第 2 轮（①产出 fact）　派意图②"] --> T2
  R3["第 3 轮（②产出 fact）　派意图③"] --> T3
```

于是攻击链在“事件驱动 + 无状态会话”的环境下依然**稳定推进、不重复、不错序**——这是 ARTEX 能自主走完多步利用链的关键。

---

## 交流群

扫码关注微信公众号 **SecSentry**，在公众号后台私信即可入群交流。

<div align="center">

<img src="screenshots/wx.png" alt="微信公众号 SecSentry" width="480" />

</div>

---
## 参考

https://github.com/oritera/Cairn


## 许可与免责声明

### 开源协议

本项目采用 **GNU Affero General Public License v3.0（AGPL-3.0）** 授权，完整条款见仓库根目录的 [LICENSE](LICENSE) 文件。

这意味着任何人都可以自由使用、修改和分发本项目，但**衍生作品必须同样以 AGPL-3.0 开源**；特别地，**若你修改本项目并通过网络（如部署为在线服务）向用户提供，也必须向这些用户公开对应的完整源码**。

> ⚠️ **重要提示**：开源协议本身不限制软件的使用用途。以下的「使用限制」与「免责声明」是作者对使用者的额外约定与郑重声明，请务必遵守。

**ARTEX 仅供个人学习、代码研究与本地技术验证使用，不得用于对任何线上系统或网站发起实际测试。**

### 允许使用范围

- 仅可用于**阅读、学习与研究本项目源码**，以及在**本地隔离环境**中进行技术原理验证；
- 适用于个人学习、学术研究、代码审阅等非攻击性用途。

### 禁止事项

- **严禁使用本工具对任何网站、线上服务或联网系统发起扫描、探测、利用或攻击**（无论是否获得授权、是否为自有资产）；
- 严禁将本工具用于任何实际的渗透测试、攻防对抗或生产环境；
- 严禁将本工具用于非法入侵、数据窃取、勒索、拒绝服务或任何破坏性、犯罪性活动；
- 严禁利用本工具从事违反所在国家/地区法律法规的行为。

### 合规责任

使用者须自行遵守所在国家/地区关于网络安全、数据保护与计算机犯罪的全部法律法规（在中国大陆包括但不限于《网络安全法》《数据安全法》《个人信息保护法》及相关司法解释）。**因使用本工具产生的一切法律责任与后果，均由使用者自行承担。**

### 免责声明

本项目按“现状（AS IS）”提供，不附带任何明示或默示的担保。作者及贡献者不对使用本工具（无论使用方式是否得当）所导致的任何直接或间接损失、数据丢失、系统损坏或法律纠纷承担责任。**下载、安装或使用本项目，即表示你已阅读、理解并同意上述全部条款。**

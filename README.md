<div align="center">

# reArtex

**AI 自主渗透测试系统** · Go 后端 + Next.js 前端 + PostgreSQL

> 上游 ARTEX 的**安全加固分支**，独立发布线。
>
> | | |
> |---|---|
> | **上游** | [wufufu770/ARTEX](https://github.com/wufufu770/ARTEX)（原作者仓库）—— reArtex **不向它推送任何改动** |
> | **本仓库的发布线** | 镜像 `ghcr.io/wufufu770/reartex`、二进制 [Releases](https://github.com/wufufu770/reArtex/releases) |
> | **更新策略** | 页面「一键更新」只跟随 **reArtex 自己的 Release**；上游的后续变更按需择取回合，**不自动同步** |
> | **许可** | AGPL-3.0（详见文末「许可与免责声明」）|

</div>

> ⚠️ **授权测试专用**：本系统只用于已获明确授权的渗透测试场景（自有资产 / 授权靶场）。
> 它不内置目标清单，不会替你决定打谁，也不在未经授权的目标上工作。

[![ci](https://github.com/wufufu770/reArtex/actions/workflows/ci.yml/badge.svg)](https://github.com/wufufu770/reArtex/actions/workflows/ci.yml)

---

## 这是什么

reArtex 是一套**跑在你自己机器上**的 AI 自主渗透测试系统：你描述一个目标，它调度一组
agent 去做资产发现、漏洞探测、证据留存与复测，全程在 Web 界面里推进，你能看每一步、
能看到 token 花在哪。

形态是「**一个后端 + 一个前端 + 一个 Postgres**」：后端负责调度、裁决、记账；前端是
完整的操作台；数据全在本地数据库里。

### 它能做什么

| 能力 | 说明 |
|---|---|
| **探索引擎** | 投一个目标，引擎自动拆解意图、并行派发 worker、把结论写回图里，形成一条可复现的攻击链路 |
| **双图** | **探索图**记录目标、意图、事实、漏洞与血缘；**资产图**记录域名、子域、IP、服务、端点与归属公司；两图靠锚点表相连 |
| **策略库** | 把一次成功的打法沉淀成**策略**（含证据链）；新策略先进隔离态，晋级要过 curator 的三条件（重放闸门 + 观测数 + 里程碑命中） |
| **职责分离（SoD）** | 每次工具调用都要过一道判定：谁、哪一层权限、命中哪条策略、为什么放行或拒绝 —— 全部落成可校验的决策账本 |
| **发现验证态** | 每条发现有三个验证态（待确认 / 已确认 / 已驳回）与一条**返工边**（发现 → 产出它的那条意图），裁决由确定性层做 |
| **会话一等对象** | 会话是数据库里的一等实体：一个会话可以挂多个 agent、可关联任务 / 策略 / 资产 / 漏洞、可热加入成员 |
| **任务大厅** | 一个任务一个大厅，多 agent 共享一张探索图；大厅有额度、并发上限、成员花名册与消息箱 |
| **追加式记忆层** | 记忆以追加 + 版本化方式落库，可查看、可导出、可退役；**dream 只提交 diff**，不整体重写索引 |
| **出网治理** | 出网按**任务范围**判定；平台基础设施类目的地走**范围豁免清单**；未覆盖通道公开列出 |
| **流量录制** | 内置 MITM 录制代理，请求/响应全程留痕，可绑定为漏洞证据并下载完整正文 |
| **MCP / Skill / 工具** | 三种传输的外部 MCP 服务端、agentskills.io 规范的技能库、系统工具与自定义工具，均可按 agent 授权可见 |

### 它**不**做什么

- **不替你做授权判断**：目标由你给，范围由你定。
- **默认失败关闭**：判官不可用、策略读不到、范围判定不了、裁决链查不到状态 —— 一律**拒绝**，不是放行。
- **不替你做决定**：agent 只提议，确定性层裁决；LLM 的裁决输出不能直接驱动状态机。

---

## 核心能力

| 类 | 位置 | 说明 |
|----|------|------|
| 会话 | `db/schema_session.sql`、`server/session_api.go`、`web/src/components/session/` | `sessions` / `session_members` / `session_attachments` / `session_messages` 四表 + 11 个 HTTP 接口 + 会话化前端；默认按「置顶 / 今天 / 更早」分组，**按 agent 分组是可选项且默认关闭** |
| 任务大厅 | `db/schema_chat_group.sql`、`web/src/app/(main)/task-hall/` | 群组容器（额度、并发、成员、消息箱、工具禁列表）；一个任务一个大厅 |
| 探索图 | `db/schema.sql`（`exploration_nodes` / `exploration_anchors` / activity）、`web/src/components/exploration-graph.tsx` | 节点种类 `begin/goal/hint/intent/fact/finding/digest`；关系 `spawns / derived_from / yields / proves`；ReactFlow 渲染 |
| 资产图 | `db/schema.sql`（`assets` / `companies` / `task_scope`）、`web/src/app/(main)/function/tasks/detail/_tabs/coverage-graph-tab.tsx` | 节点种类 `root_domain/subdomain/ip/service/app/endpoint`；去重 key 由程序计算；覆盖图用 AntV G6 渲染 |
| 策略图 | `db/schema_strategy.sql`、`db/strategy_curator.go`、`web/src/app/(main)/strategies/` | 策略状态 `quarantined/active/retired/tombstone`；提议 `pending/approved/rejected/superseded`；`exploration_nodes.strategy_id` 是真外键（`ON DELETE SET NULL`） |
| SoD 链 | `internal/sod/`、`internal/sodpolicy/`、`server/sod_chain.go` | 四段式：规整器 → 确定性闸门 → 一次性执行凭证 → 执行器；五道检查按序 `scope → tool_health → quota → tier2 → anomaly`；判定落 `sod_decisions` |
| 记忆层 | `db/schema_memory.sql`、`db/memory.go`、`agent/` | 追加式版本化索引 + 作用域 + 写操作日志；dream 只提 diff；**不设 per-agent 个人记忆**；目前无独立 UI 页面，经 agent 侧工具访问 |
| 进度回执 | `agent/progress_receipt.go`、`db/schema.sql`（activity） | `record_progress` / `list_progress_receipts` 两个工具 + 运行结束时的确定性回执；落在 `activity.kind='progress'`（自由文本列，无需新 DDL）；**阻塞不走回执**，走 `intercept_pending` |
| 产出级校验 | `agent/verifier.go`、`db/finding_validation.go` | `validate_finding` 工具只把裁决送进确定性层；复核器用注入回调（零编译期耦合） |
| 出网治理 | `traffic/egress.go`、`traffic/websearch_scope.go`、`internal/sodpolicy/egress_scope.go`、`server/egress_governance.go` | 范围判定 + 任务身份 + 豁免清单三层；豁免清单是**编译期常量**，不是 DB 行、不是 settings 项 |
| 流量与证据 | `traffic/`、`evidence/`、`web/src/components/traffic-evidence-viewer.tsx` | MITM 录制（`:8788`）、流量检索、漏洞证据绑定（正常对照 / 漏洞证明 / 补充验证）、完整正文下载 |
| MCP / Skill / 工具 | `mcphttp/`、`web/src/app/(main)/system/{mcp,skills,tools}/`、`web/src/components/agent-editor.tsx` | MCP 支持 `stdio` / `http` / `sse`；skill 根目录下每个子目录一个 skill（`SKILL.md`）；按 agent 授权可见 |

---

## 架构一览

```
┌───────────────────────────────────────────────────────────────────────┐
│ 前端 Next.js（静态导出，go:embed 内嵌进单二进制）                        │
│ 会话 · 任务大厅 · 任务 · 探索链路 · 资产覆盖图 · 策略库 · 流量 · SoD    │
└─────────────────────────────────┬─────────────────────────────────────┘
                                  │ REST /api/* · JWT 鉴权 · SSE 活动流
┌─────────────────────────────────▼─────────────────────────────────────┐
│ server (Go net/http)                                                  │
│ 会话与消息 · 任务大厅 · 图与策略 · 流量检索 · 审批门 · 更新与自检        │
└──────────────┬──────────────────────────────────┬─────────────────────┘
               │ 调度循环                          │ 判定与留痕
┌──────────────▼─────────────────┐  ┌──────────────▼────────────────────┐
│ agent（norma SDK）                │  │ internal/sod · internal/sodpolicy  │
│ goals 目标分解                     │  │ 规整器 → 确定性闸门 → 执行凭证 → 执行器│
│ planner 规划者（唯一意图生成者）     │  │ 阈值是代码常量 · 判定写 sod_decisions │
│ worker 执行者 ×N                   │  └──────────────┬────────────────────┘
│ mainagent 人在环路                 │                 │
└──────────────┬─────────────────┘                 │
┌──────────────▼─────────────────────────────────▼────────────────────┐
│ PostgreSQL                                                             │
│ 探索图 exploration_nodes / exploration_anchors / activity              │
│ 资产图 assets / companies / task_scope                                 │
│ 策略图 strategies / strategy_evidence · 记忆层 memory_index / …        │
│ 会话 sessions / session_members / session_attachments / session_messages│
│ 账本 sod_decisions · intercept_pending · audit_events（哈希链）          │
└──────────────┬────────────────────────────────────────────────────────┘
               │
┌──────────────▼────────────────────────────────────────────────────────┐
│ :8788 流量录制代理（MITM + CA 留痕）→ 流量库 → 漏洞证据库                │
└───────────────────────────────────────────────────────────────────────┘
```

- **图即状态**：目标、意图、事实、漏洞、提示都是图上的节点，血缘是边；planner 读态势、只在有
  未覆盖的新方向时派意图，worker 领一条意图执行完就写回并停下。
- **worker 之间的过程级信息交换**：`search_all_worker_traces` / `list_worker_traces` /
  `get_worker_trace` 让一个 worker 能检索同任务其它 worker 的执行过程（结果只落在 activity 里），
  避免重复劳动。
- **planner 多轮共享 todolist**：待办按任务保留、跨唤醒共享，每轮只对「前置已满足」的步骤派意图，
  把已被事实满足的步骤标完成 —— 串行利用链因此能被逐步拆开执行。
- **判定与执行分离**：LLM 只提议，确定性层裁决；安全阈值一律是 `internal/sodpolicy` 里的代码常量，
  不落在可热改的 DB / settings。
- **收敛由确定性层做**：上下文压缩的触发点、发现验证态的流转、SoD 的放行与否，都不靠模型自觉。

---

## 快速开始

> 需要 **PostgreSQL**；跑探索需要配 **LLM**（`ANTHROPIC_API_KEY` 或 `OPENAI_API_KEY`，也可在 UI 里配）。
>
> 下面所有产物都来自 **reArtex 自己的发布线**（`ghcr.io/wufufu770/reartex` 与本仓库的
> Releases），不跟上游 ARTEX 的发布。

### 方式一：Docker Compose（推荐）

```bash
git clone https://github.com/wufufu770/reArtex.git
cd reArtex
cp .env.example .env          # 填 POSTGRES_PASSWORD（必填）、可选 LLM key
./init-pg-dsn-secret.sh       # 由 .env 生成数据库连接串 secret（0600，不进版本库）
docker compose up -d          # 拉取 ghcr.io/wufufu770/reartex 镜像 + postgres
# → http://localhost:8787
```

镜像已含常用工具（ripgrep / curl / vim / npm / nmap…）；`./skills` 与 `./data` 以绑定挂载持久化。

> **`init-pg-dsn-secret.sh` 不能省**：数据库口令走 **docker secret 文件**供给
> （`/run/secrets/pg_dsn`），这样 `docker inspect` 看不到口令 —— 口令留在容器的
> `environment` 里就等于把它摊开给任何能看容器配置的人。该脚本从 `.env` 拼出连接串
> 写进 `secrets/pg_dsn`（权限 0600，`secrets/` 已被 `.gitignore` 排除）。

指定版本：`.env` 里设 `ARTEX_TAG=v0.4.6`（不设则用 `latest`）。**建议固定版本**，不要用
可变的 `latest` —— 出了问题没法确定跑的是哪一版。

> **想要拉取的那个 tag 还没发布时**：本仓库的 Release 随 `v*` tag 生成，ghcr 镜像与之同批
> 推送。若你要的版本尚未发布，镜像就还不存在，上面那条 `docker compose up -d` 会拉取失败。
> 两条走法：
>
> - **等发布**：打 tag 时 release workflow 会自动构建多架构镜像推上 ghcr.io，并生成带
>   `SHA256SUMS` 的 GitHub Release。
> - **先本地构建**（需要 Go ≥ 1.26、Node ≥ 20、rsync、zip）：
>   ```bash
>   ./build-image.sh                                  # 编前端 + 编二进制 + 构建镜像 artex:local
>   echo "image: artex:local" > docker-compose.override.yml
>   docker compose up -d
>   ```
>
>   `build-image.sh` 会在构建期核对镜像里的二进制版本与本次构建是否一致，
>   装错旧二进制会直接构建失败，不会让你拿到一个「看着新、其实是旧」的镜像。

### 干净实例：新版本从一个空库起

`docker-compose.fresh.yml` 是「干净实例」专用覆盖：它用**新的 project 名**起实例，compose 就会
新建卷，**绝不吸收旧实例的任何内容**（旧 schema、旧种子、旧提示词、旧任务、旧发现、旧策略都不进来）。

```bash
docker compose -p artexdev2 \
  -f docker-compose.yml -f docker-compose.override.yml -f docker-compose.fresh.yml \
  up -d
```

- 端口换成 `19787` / `19788`，与现役实例互不冲突；网段 `172.30.0.0/16`。
- 镜像 `artex-dev:local`，由本仓库 `Dockerfile` 本地构建。
- 该覆盖额外做两件现役不做的事：保留 `CAP_SYS_ADMIN`、解除 AppArmor 对 `mount(2)` 的封禁
  （仅用于本地隔离实验；现役 compose 一律不加）。
- 旧实例与它的卷**原封不动保留**（不 down、不 `-v`、不删），两者之间不做任何数据搬运。

### 方式二：下载预编译二进制（reArtex Releases）

到 [Releases](https://github.com/wufufu770/reArtex/releases) 下载对应平台的 zip，解压后得到
`artex` + `start.sh`（Windows 为 `start.bat`）+ `skills/` + `config.example.json`：

```bash
cp config.example.json config.json   # 填好 database 连接
./start.sh                           # → http://localhost:8787
```

> 请用 `start.sh` / `start.bat` 启动，而不是直接跑 `./artex`。它是个守护脚本：程序退出后按退出码决定是否重新拉起，
> **页面上的「一键更新」靠它完成换装**。直接运行 `./artex` 时更新完就不会被拉起了。
> 后台常驻：`nohup ./start.sh >artex.log 2>&1 &`。

### 方式三：一键安装脚本

```bash
git clone https://github.com/wufufu770/reArtex.git
cd reArtex
./install.sh
```

脚本会：检测 / 自动安装 Docker → 让你选 **① 全部 Docker** 或 **② 本地编译运行**：

- **① 全部 Docker**：填一个 Postgres 密码（可回车随机）→ 自动写 `.env` → `docker compose up -d`。
- **② 本地运行**：选数据库（连已有 / 用 Docker 起一个）→ 生成 `config.json` → `go` 编译内嵌单二进制 → 启动。

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

### 本地开发

```bash
./dev.sh    # 后端(:8787) + 流量代理(:8788) + 前端 next dev(:5173) → http://localhost:5173
```

- 后端：`go run ./cmd/artex`（不带 `-tags embedui` 则不内嵌前端）
- 前端：`cd web && npm run dev`（`/api` 反代到后端，带热更新）

---

## 端口

| 端口 | 默认绑定 | 用途 |
|---|---|---|
| `8787` | 全部接口 | 前端页面 + REST API + SSE 活动流（同一个进程、同源） |
| `8788` | `127.0.0.1` | 流量录制代理（`docker-compose.yml` 里显式绑到回环，避免默认成为开放代理） |

改端口：`./start.sh -addr :9000 -proxy :9001`（`start.sh` 把参数原样透传给 `artex`）。

---

## 配置

### `config.json`

本文件只含 `database` 与 `skill_dir` 两项；LLM 等其余配置走环境变量或应用内设置。

```json
{
  "database": {
    "host": "127.0.0.1", "port": 5432,
    "user": "artex", "password": "yourpass",
    "dbname": "artex", "sslmode": "disable"
  },
  "skill_dir": "/opt/artex/skills"
}
```

- `database` 也可以只填一个完整 `dsn`（`postgres://user:pass@host:5432/artex?sslmode=require`），
  非空时忽略分字段。
- `skill_dir` 是 skill 根目录（每个子目录一个 skill，含 `SKILL.md`）。留空或删除即用默认
  （可执行文件同级的 `skills/`）。

### 环境变量

| 变量 | 作用 |
|---|---|
| `ARTEX_CONFIG` | 配置文件路径（默认取可执行文件同级的 `config.json`） |
| `ARTEX_SKILL_DIR` | skill 根目录，优先级高于配置文件 |
| `ARTEX_PG_DSN` | 数据库连接串，优先级最高 |
| `ARTEX_PG_DSN_FILE` | 数据库连接串文件路径（读入即清零并删除该文件）；优先级低于 `ARTEX_PG_DSN`、高于配置文件 |
| `ARTEX_REQUIRE_ENV_SCRUB` | 设为 `1` 时，启动自检发现平台凭据仍残留在进程环境块即拒绝启动（默认只告警） |
| `ARTEX_MASTER_KEY_FILE` | LLM key 加密的主密钥文件（可选）。不配置则 `llm_profiles.api_key` 行为与从前一致 |
| `ARTEX_DISABLE_SHORT_CIRCUIT` | 设为 `1` 停用代码常量短路表（应急回滚用；DB 规则侧仍拦得住） |
| `ARTEX_LLM_PROVIDER` / `ARTEX_LLM_MODEL` / `ARTEX_LLM_BASE_URL` / `ARTEX_LLM_PROXY` / `ARTEX_LLM_STREAM` | LLM 端点与传输方式 |
| `ANTHROPIC_API_KEY` / `OPENAI_API_KEY` | LLM 密钥（读入后立即从进程环境抹除） |
| `ARTEX_COMPACT_TRIGGER_RATIO` | 上下文压缩触发比率（0~1）。**只能提前、不能推后**；填 0 或非法值 = 保持 SDK 默认 |
| `AGENT_CORE_TOOL_TIMEOUT_SEC` | 前台命令超时（秒），钳制在 5s–600s。超时后命令被移到后台，agent 立刻拿回控制权 |
| `ARTEX_TAG` | docker compose 使用的镜像 tag（缺省 `latest`） |
| `POSTGRES_USER` / `POSTGRES_PASSWORD` / `POSTGRES_DB` | compose 里的 Postgres 账号与库；**`POSTGRES_PASSWORD` 必填** |
| `PG_DSN_SECRET_FILE` | 数据库 secret 文件位置（默认 `./secrets/pg_dsn`） |
| `NEXT_PUBLIC_SSE_BASE` | SSE 基地址。**仅在 SSE 需与页面不同源时才设**，且是构建期变量（`next build` 时固化进静态包，容器运行时再设无效） |

### 启动参数

| 参数 | 默认 | 说明 |
|---|---|---|
| `-addr` | `:8787` | HTTP 监听地址（前端 + API + SSE） |
| `-proxy` | `127.0.0.1:8788` | 流量录制代理地址（留空则关闭） |
| `-data` | 可执行文件同级的 `data/` | SQLite 类本地存储目录（`jwt.key` 等） |
| `-version` | — | 打印构建版本后退出 |

### 运行时上限

| 项 | 值 |
|---|---|
| 每个任务的 work agent 数 | 「系统设置」里配置（默认 3） |
| 压缩触发点 | SDK 默认窗口比例；`ARTEX_COMPACT_TRIGGER_RATIO` 只能提前 |
| 前台命令超时 | 默认 60s（干净实例显式写明以便审计），上限 600s |
| SoD 闸门阈值 | 全部在 `internal/sodpolicy` 的代码常量里，DB 里没有任何阈值列 |

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

---

## 更新与升级

> **更新源只有本仓库自己的 Release**（`wufufu770/reArtex`）—— 不跟上游 ARTEX 的发布线。
> 拉到的包会先比对同批 Release 的 `SHA256SUMS`，再冒烟测试、换装；失败自动回滚。
>
> 升级只换程序、不动数据：Postgres 数据卷 `pgdata`、`./data`（jwt.key / SQLite 等）、`./skills` 都会保留。
> **数据库迁移无需手动执行** —— `artex` 每次启动会幂等重跑 `schema.sql`（含 `ADD COLUMN` /
> `CREATE INDEX IF NOT EXISTS`），即「重启即迁移」。升级前仍建议先备份 `./data` 与数据库。

### 方式一：页面一键更新（推荐）

在 **系统配置** 页（`/system/settings`）的**版本与更新**卡片里，可以直接检查并安装新版本，无需登录服务器。

点「更新」后：下载当前平台的发布包 → 比对 Release 的 `SHA256SUMS` → 用 `-h` 冒烟测试新二进制 → 暂存为 `artex.new` → 程序退出，由 `start.sh` / `start.bat` 重新拉起并完成换装。页面会自动等到新版本上线后刷新。

- **失败不会留下坏程序**：校验或冒烟不通过就丢弃暂存件、继续跑当前版本；换装后的新版若连续 3 次启动失败，会自动回滚到 `artex.old`（失败的那个留作 `artex.failed` 供排查）。
- **随时可回退**：上一版本保留为 `artex.old`，卡片上有「回滚到上一版本」。注意数据库结构不会回退。
- **更新会中断正在运行的任务** —— 更新即重启，请在空闲时进行。
- **开发构建不给更新**：版本号是 `dev` 或 `git describe` 带后缀时禁用。
- **Docker 下只换程序、不换镜像**：要连镜像一起升级请用 `docker compose pull artex && docker compose up -d artex`。

### 方式二：一键更新脚本

```bash
cd reArtex
./update.sh
```

先可选 `git pull` 拉取最新代码，再让你选 **① Docker 更新** 或 **② 本地编译更新**（与 `install.sh` 对应）。

### 方式三：Docker Compose（手动）

```bash
cd reArtex
git pull                       # 更新 compose / 脚本（可选）
docker compose pull artex
docker compose up -d artex     # 换新镜像重启 → 自动迁移 schema
docker image prune -f          # 清理旧镜像（可选）
```

### 方式四：预编译二进制（Releases）

到 [Releases](https://github.com/wufufu770/reArtex/releases) 下载新版本 zip，停掉旧进程后覆盖 `artex` 与 `skills/`（保留你的 `config.json` 与 `data/`），重启即可。

---

## 安全模型

- **默认失败关闭（fail-closed）**：判官不可用、策略读不到、范围判定不了、裁决链状态读不到 —— 一律拒绝并留痕，不放行。
- **判定与执行分离**：LLM 只提议，确定性层裁决；模型裁决超时 / 报错 / 输出不可解析时按配置的失败策略（默认 `deny`）处理，不是回落放行。
- **SoD 决策链**：规整器把自由文本意图规整成结构化意图 → 确定性闸门按序跑
  `scope_check` / `tool_health_check` / `quota_check` / `tier2_check` / `anomaly_check` →
  放行则签发一次性执行凭证 → 执行器按凭证放行。判定与理由写 `sod_decisions`；`needs_review` 进
  对抗复核队列。**提出人不得给自己盖章**（`ErrSoDReviewSelfCertification`），已结案不得翻案。
- **工具面三档**：代码常量短路表分 `deny`（凭据汇 / 读控制面秘密）、`ask`（真高危，**强制 60s 超时**，
  到点按拒绝处理）、**显式 allow**（低危日常）。DB 规则构成第二层拦截；短路表被停用时写库侧仍拦得住。
- **审批防疲劳**：同一 agent 在时间窗内反复对同一高危面请求审批，超阈值才自动拒绝（默认 5 次 / 600 秒，可热改）。
- **工具执行前的路径判定**：入参里的路径片段按工具工作目录**解析成真实路径**再判定
  （`pathguard.CheckInputPaths`），软链与父目录软链也覆盖；判定点在工具策略层，任何 permission mode 绕不过。
- **凭据不进进程环境块**：数据库口令走 docker secret 文件（`/run/secrets/pg_dsn`）或一次性
  `ARTEX_PG_DSN_FILE`（读入即清零并删除）；LLM key 读入后立即从环境抹除。启动自检读
  `/proc/self/environ` 并给出明确结论，`ARTEX_REQUIRE_ENV_SCRUB=1` 时可直接拒绝启动。
- **LLM key 可选加密落库**：`llm_profiles.api_key` 支持 AES-256-GCM，主密钥由
  `ARTEX_MASTER_KEY_FILE` 供给（不进环境块、不进库），AAD 绑定行名，改名触发重加密。
- **出网治理三层**：范围判定（`SetEgressScope` 接线）+ 任务身份（planner / 主 agent / 对话 agent /
  worker 四类角色都带令牌）+ **范围豁免清单**（`internal/sodpolicy.EgressScopeExemptHostList`，
  编译期常量：搜索服务商 / 模型 API / 镜像仓库等平台基础设施）。清单外目的地仍放行但按
  「未归属出网」留痕；**判不出目的地则拒绝**。
- **MCP 双层拦截**：定义写库侧校验 + 短路表；stdio 传输属「让平台拉起任意二进制」、http/sse
  传输属「把 env 字段当 header 回放」，两者默认 `deny`；SSE 端点做同源校验并拒绝带 userinfo 的端点。
- **审计链**：`sod_decisions` 决策账本 + `intercept_pending` 拦截记录 + `audit_events` 哈希链
  （可校验被改动位置）。

### 已知边界（**刻意公开**）

- **`/proc/<pid>/mem` 读同 UID 进程内存仍可做到**：内核对同 UID 的 ptrace 不要求
  `CAP_SYS_PTRACE`，`cap_drop` 挡不住。真正的闭合要让工具进程与 artex 不同 UID（属 per-task 隔离那一档）。
  镜像里留了 `verify-ptrace-blocked.sh` 供自查，它**故意不进发布门**。
- **出网治理有未覆盖通道**：清单在「系统配置」页可见（`GET /settings/egress-governance`），
  例如 MCP 服务端自身的出网、宿主侧出网、流量捕获关闭时走的全局代理路径。这些通道**不受任务范围约束**。
- **平台环境块规则是字符串预筛**：运行时才成形的路径仍可能绕过；闭合靠的是凭据不进环境块这一供给侧措施。
- **发布产物无独立签名**：只有同批 Release 的 `SHA256SUMS`，没有 GPG/签名链。
- **前端 lint 是棘轮门**：只查相对基线改动的文件（仓库历史 lint 基线本身是红的）。

---

## 文档

| 文档 | 内容 |
|---|---|
| [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) | 系统分层、双图架构、引擎与意图生命周期、worker 间信息交换、planner 共享 todolist |
| [docs/SECURITY-HARDENING.md](docs/SECURITY-HARDENING.md) | 安全加固的设计决策：安全边界为何要移到代码常量、fail-closed 的落点、凭据治理、已知残留与应急回滚 |
| [docs/SoD链覆盖边界.md](docs/SoD链覆盖边界.md) | SoD 链的已覆盖 / 未覆盖 / 明确不在范围内、原因码全集、阈值所在位置 |
| [docs/ROADMAP-会话群聊化与记忆层.md](docs/ROADMAP-会话群聊化与记忆层.md) | 会话实体化、任务大厅优化、出网治理补齐、记忆层调研与实施结果回写 |
| [docs/漏洞流量证据.md](docs/漏洞流量证据.md) | 流量证据的绑定与消费方式 |
| [docs/RELEASE-NOTES-v0.4.6.md](docs/RELEASE-NOTES-v0.4.6.md) | v0.4.6 发布说明与仓库简介文案 |
| [CHANGELOG.md](CHANGELOG.md) | 版本变更流水 |
| [RELEASE_NOTES.md](RELEASE_NOTES.md) | 当前发布说明（release workflow 取它作为 Release 正文） |

---

## 交流群

扫码关注微信公众号 **SecSentry**，在公众号后台私信即可入群交流。

<div align="center">

<img src="screenshots/wx.png" alt="微信公众号 SecSentry" width="480" />

</div>

## 参考

https://github.com/oritera/Cairn

---

## 许可与免责声明

### 开源协议

本项目采用 **GNU Affero General Public License v3.0（AGPL-3.0）** 授权，完整条款见仓库根目录的 [LICENSE](LICENSE) 文件。

这意味着任何人都可以自由使用、修改和分发本项目，但**衍生作品必须同样以 AGPL-3.0 开源**；特别地，**若你修改本项目并通过网络（如部署为在线服务）向用户提供，也必须向这些用户公开对应的完整源码**。

> ⚠️ **重要提示**：开源协议本身不限制软件的使用用途。以下的「使用限制」与「免责声明」是作者对使用者的额外约定与郑重声明，请务必遵守。
>
> **ARTEX 仅供个人学习、代码研究与本地技术验证使用，不得用于对任何线上系统或网站发起实际测试。**

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

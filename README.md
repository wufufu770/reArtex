<div align="center">

# reArtex

**AI 自主渗透测试系统** · Go 后端 + Next.js 前端

> 上游 ARTEX 的**安全加固分支**，独立发布线。
>
> | | |
> |---|---|
> | **上游** | [wufufu770/ARTEX](https://github.com/wufufu770/ARTEX)（原作者仓库）—— reArtex **不向它推送任何改动** |
> | **本仓库的发布线** | 镜像 `ghcr.io/wufufu770/reartex`、二进制 [Releases](https://github.com/wufufu770/reArtex/releases) |
> | **更新策略** | 页面「一键更新」只跟随 **reArtex 自己的 Release**；上游的后续变更按需择取回合，**不自动同步** |
> | **许可** | AGPL-3.0（详见文末「许可与免责声明」）|

</div>

---

## 这是什么

reArtex 是一套**跑在你自己机器上**的 AI 自主渗透测试系统：你描述一个目标，它调度一组
agent 去做资产发现、漏洞探测、证据留存与复测，全程在 Web 界面里推进，你随时能插话、
能看每一步、能看到钱花在哪。

它的形态是「**一个后端 + 一个前端 + 一个 Postgres**」：后端负责调度、裁决、记账；前端是
完整的操作台；数据全在本地数据库里。

### 你能用它做什么

| 能力 | 说明 |
|---|---|
| **探索任务** | 投一个目标，引擎自动拆解意图、并行派发 worker、汇总线索，形成一条可复现的攻击链路 |
| **任务大厅** | 把多个 agent 组成**群组**来协作：定群额度、限并发、看「谁认领了什么」、随时在群里插话 |
| **策略库** | 把一次成功的打法沉淀成**策略**（含证据链），可被后续任务引用；策略可提议、可裁决、可退役 |
| **职责分离（SoD）** | 每次 agent 调工具都要过一道裁决：谁、哪一层权限、命中哪条策略、为什么放行或拒绝 —— 全部落成**可校验的审计账本** |
| **资产同步** | 可选接 ScopeSentry 之类的资产源，把资产并入探索 |
| **MCP 接入** | 支持 `stdio` / `http` / `sse` 三种传输的外部 MCP 服务端（stdio 命令、http 流式端点或 sse 事件流均可），按 agent 粒度授权可见性 |
| **漏洞复测** | 对已报漏洞发起独立复测会话，结论分为「仍可复现 / 已修复 / 无法确认」，证据全程留存 |

### 它**不**做什么

reArtex 是一套**需要你自己有授权**才该用的工具。它不内置目标清单，不会替你决定打谁，
也不会在未经授权的目标上工作。**默认失败关闭**：判官不可用、策略读不到、范围判定不了，
一律拒绝，而不是放行。

---

## 安装

> 需要 **PostgreSQL**；跑探索需要配 **LLM**（`ANTHROPIC_API_KEY` 或 `OPENAI_API_KEY`，也可在 UI 里配）。
>
> 下面所有产物都来自 **reArtex 自己的发布线**（`ghcr.io/wufufu770/reartex` 与本仓库的
> Releases），不跟上游 ARTEX 的发布。

### 方式一：Docker Compose（推荐）

```bash
git clone https://github.com/wufufu770/reArtex.git
cd reArtex
cp .env.example .env          # 填 POSTGRES_PASSWORD、可选 ANTHROPIC_API_KEY
./init-pg-dsn-secret.sh       # 由 .env 生成数据库连接串 secret（0600，不进版本库）
docker compose up -d          # 拉取 ghcr.io/wufufu770/reartex 镜像 + postgres
# → http://localhost:8787
```

镜像已含常用工具（ripgrep / curl / vim / npm / nmap…）；`./skills` 与 `./data` 以绑定挂载持久化。

> **`init-pg-dsn-secret.sh` 不能省**：数据库口令走 **docker secret 文件**供给
> （`/run/secrets/pg_dsn`），这样 `docker inspect` 看不到口令 —— 口令留在容器的
> `environment` 里就等于把它摊开给任何能看容器配置的人。该脚本从 `.env` 拼出连接串
> 写进 `secrets/pg_dsn`（权限 0600，`secrets/` 已被 .gitignore 排除）。
> 该形态需要 **v0.4.1 及以后**的镜像。

指定版本：`.env` 里设 `ARTEX_TAG=v0.4.1`（不设则用 `latest`）。**建议固定版本**，不要用
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

### 方式五：构建跨平台 Release 压缩包

`build.sh` 会先构建并嵌入前端，再使用 Go linker 去除调试信息，并将发布文件压缩为 zip。
Release 模式默认生成 Linux amd64/arm64、macOS amd64/arm64 和 Windows amd64 的 zip 包：

```bash
./build.sh --release
# 产物：dist/artex-0.4.0-*.zip
```

UPX 自解压二进制可能与部分 Linux 内核、虚拟化环境或安全策略不兼容，因此默认不启用。
可用 `ARTEX_TARGETS` 自定义目标；确认目标运行环境兼容时，可显式传入 `--upx` 进一步缩小二进制：

```bash
ARTEX_TARGETS=linux/amd64,windows/amd64 ./build.sh --release
./build.sh --target linux/amd64 --upx
```

---

## 第一次用

1. **打开 <http://localhost:8787>** —— 首次访问会跳到 `/setup`，**由你设置管理员密码**
   （账号固定为 `ARTEX`）。系统不预置任何账号，第一次访问的人就是管理员。
2. **配 LLM**：进「系统 → LLM」，填一个 openai 兼容端点与密钥，激活它。也可以用
   `ANTHROPIC_API_KEY` / `OPENAI_API_KEY` 环境变量代替。
   - ⚠️ 有些厂商的「通用 API 端点」和「编程套餐端点」是**两套计费**，返回 429「余额不足」
     时多半是连错了端点，不是密钥错。
3. **配 MCP（可选）**：进「系统 → MCP」，按 `stdio` / `http` / `sse` 填外部服务端的
   命令、参数或 URL。旧版 SSE 服务通常用 `GET /sse` 建立事件流，配置时 URL 填 `/sse`，
   请求头按 `Authorization=Bearer <token>` 填。
4. **建任务**：进「任务大厅」建一个群组（可勾选多个 agent，或交给 auto 分发），定好群额度
   与并发上限，然后发布任务。

---

## 更新升级

> **更新源只有本仓库自己的 Release**（`wufufu770/reArtex`）—— 不跟上游 ARTEX 的发布线。
> 拉到的包会先比对同批 Release 的 `SHA256SUMS`，再冒烟测试、换装；失败自动回滚。
>
> 升级只换程序、不动数据：Postgres 数据卷 `pgdata`、`./data`（jwt.key / SQLite 等）、`./skills` 都会保留。
> **数据库迁移无需手动执行** —— `artex` 每次启动会幂等重跑 `schema.sql`（含 `ADD COLUMN` /
> `CREATE INDEX IF NOT EXISTS`），即「重启即迁移」。升级前仍建议先备份 `./data` 与数据库。

### 方式一：页面一键更新（推荐）

在 **系统配置** 页（侧边栏「系统配置」→ `/system/settings`）的**版本与更新**卡片里，可以直接检查并安装新版本，无需登录服务器。

点「更新」后：下载当前平台的发布包 → 比对 Release 的 `SHA256SUMS` → 用 `-h` 冒烟测试新二进制 → 暂存为 `artex.new` → 程序退出，由 `start.sh` / `start.bat` 重新拉起并完成换装。页面会自动等到新版本上线后刷新。

- **失败不会留下坏程序**：校验或冒烟不通过就丢弃暂存件、继续跑当前版本；换装后的新版若连续 3 次启动失败，会自动回滚到 `artex.old`（失败的那个留作 `artex.failed` 供排查）。
- **随时可回退**：上一版本保留为 `artex.old`，卡片上有「回滚到上一版本」。注意数据库结构不会回退。
- **更新会中断正在运行的任务** —— 更新即重启，请在空闲时进行。
- **开发构建不给更新**：版本号是 `dev` 或 `git describe` 带后缀时禁用，避免正式版覆盖掉本地调试的二进制。
- **Docker 下只换程序、不换镜像**：镜像里的 playwright / nmap 等工具链不会跟着升级，且 `docker compose up -d` 重建容器后会退回镜像自带的版本。要连镜像一起升级仍请用 `docker compose pull artex && docker compose up -d artex`。
- 访问 GitHub 需要代理时，在同一页面配置**全局代理**即可，更新链路会走它。更新只从 GitHub 域名下载并强制 HTTPS。

### 方式二：一键更新脚本

```bash
cd reArtex
./update.sh
```

脚本先可选 `git pull` 拉取最新代码，再让你选 **① Docker 更新** 或 **② 本地编译更新**（与 `install.sh` 对应）：

- **① Docker**：可指定目标镜像 tag（回车沿用 `.env` 的 `ARTEX_TAG`，缺省 `latest`）→ `docker compose pull` → `docker compose up -d`（换新镜像重启即自动迁移）。
- **② 本地**：重建前端静态产物 → 重新编译 `./artex`（完成后重启进程生效）。

### 方式三：Docker Compose（手动）

```bash
cd reArtex
git pull                       # 更新 compose / 脚本（可选）
# 指定版本：在 .env 设 ARTEX_TAG=v0.4.1；不设则用 latest
docker compose pull artex
docker compose up -d artex     # 换新镜像重启 → 自动迁移 schema
docker image prune -f          # 清理旧镜像（可选）
```

### 方式四：预编译二进制（Releases）

到 [Releases](https://github.com/wufufu770/reArtex/releases) 下载新版本 zip，停掉旧进程后覆盖 `artex` 与 `skills/`（保留你的 `config.json` 与 `data/`），重启即可：

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

> 仅当 SSE 需要走与页面不同的来源（如独立子域）时，才在**构建期**设置 `NEXT_PUBLIC_SSE_BASE`
> （该变量在 `next build` 时固化进静态包，容器运行时再设无效）。

---

## 常见问题

**Q：打开页面一直跳 `/setup`？**
首次访问没有账号是正常的 —— 系统不预置任何账号，`/setup` 就是初始化入口，由你设管理员密码。

**Q：端口 8787 被占了？**
`./start.sh -addr :8899`（Docker 下改 compose 里的 `ports` 映射）。

**Q：升级后数据还在吗？**
在。升级只换程序，不动 `pgdata` / `./data` / `./skills`；数据库结构在启动时自动迁移。

**Q：任务大厅的「额度」和「并发」是什么关系？**
额度是**总量**上限（这个群总共能烧多少 token），并发是**同时在跑**的 agent 数上限。
两者独立生效，都可以在建群时定，之后也能改。

**Q：SoD 账本里看到「deny」，是我的配置有问题吗？**
不一定。裁决是**默认拒绝**的：判官不可用、策略读不到、工具不在允许范围，都会记成 deny
并留下原因码。账本里的每条记录都带权限层级与理由，可以按 `source_agent` / 工具名筛选。

**Q：出网范围校验覆盖不到全部通道。**
是已知且**刻意公开**的边界。出网治理状态在「系统配置」页里能看到它列出的**未覆盖通道清单**
（例如 web_fetch 不走流量代理、MCP 服务端自身的出网、宿主侧出网等）。这些通道**不受任务范围
约束**，请自行评估影响面。

---

## 文档

面向开发者的材料已从本文拆出，避免把使用说明淹没在实现细节里：

| 文档 | 内容 |
|---|---|
| [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) | 系统分层、双图架构、引擎与意图生命周期、worker 间信息交换、planner 共享 todolist |
| [docs/SECURITY-HARDENING.md](docs/SECURITY-HARDENING.md) | 安全加固的历史与设计决策：安全边界为何要移到代码常量、fail-closed 的落点、凭据治理、已知残留与应急回滚 |
| [CHANGELOG.md](CHANGELOG.md) | 版本变更流水 |
| [RELEASE_NOTES.md](RELEASE_NOTES.md) | 当前发布说明 |

本地开发：

```bash
./dev.sh    # 后端(:8787) + 流量代理(:8788) + 前端 next dev(:5173) → http://localhost:5173
```

- 后端：`go run ./cmd/artex`（不带 `-tags embedui` 则不内嵌前端）
- 前端：`cd web && npm run dev`（`/api` 反代到后端，带热更新）

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

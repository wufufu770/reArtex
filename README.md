# reArtex

这个项目是对artex的二次开发，在限制更严格的安全边界后，也融入了一些自己的理解。

# Build in Public

如果你在时间线上刷到了这个项目，欢迎停下来聊聊——无论是架构层面的争论、对某个具体缺陷的补充验证，还是"你这里其实可以更简单"的一句提醒，对我来说都是宝贵的输入。欢迎师傅们提pr和issue

## 是什么

reArtex 是一套 LLM 多 agent 驱动的自主渗透测试系统：Go 单体后端（内嵌 Next.js 前端）+ PostgreSQL，agent 能力由 norma SDK 提供。

## 干什么

- **目标拆解**：planner 读探索图态势、判目标、派意图；worker 领一条意图用真实工具执行，把新资产、事实与漏洞写回后即停。
- **双图**：探索图（每任务的思考与推进链）+ 资产图（跨任务共享的资产真值库），两者靠锚点相连 —— 既能从探索方向看它测了哪些资产，也能从资产反查它被哪些意图测过、得出过什么。
- **人在环路**：对话干预、审批门、steer 纠偏、流量录制与证据留存。
- **漏洞复测**：独立的复测 Agent 会话，结论分「仍可复现 / 已修复 / 无法确认」。
- **资产同步**：从 ScopeSentry 导入资产，按公司资产范围归并进资产图。

## 安装

依赖 PostgreSQL；探索需配置 LLM（`ANTHROPIC_API_KEY` 或 `OPENAI_API_KEY`，也可在 UI 里配）。

### Docker Compose（推荐）

```bash
git clone https://github.com/wufufu770/reArtex.git
cd reArtex
cp .env.example .env          # 填 POSTGRES_PASSWORD、可选 ANTHROPIC_API_KEY
./init-pg-dsn-secret.sh       # 由 .env 生成数据库连接串 secret（0600，不进版本库）
docker compose up -d
# → http://localhost:8787
```

指定版本：`.env` 里设 `ARTEX_TAG=v0.9.12`（不设则用 `latest`）。建议固定版本，不要用可变的 `latest` —— 出问题没法确定跑的是哪一版。

### 预编译二进制

到 [Releases](https://github.com/wufufu770/reArtex/releases) 下载对应平台的 zip，解压后得到 `artex` + `start.sh`（Windows 为 `start.bat`）+ `skills/`：

```bash
cp config.example.json config.json   # 填好 database 连接
./start.sh                           # → http://localhost:8787
```

用 `start.sh` / `start.bat` 启动，不要直接跑 `./artex`：它是守护脚本，程序退出后按退出码决定是否重新拉起，页面上的一键更新靠它完成换装。

### 一键脚本

```bash
git clone https://github.com/wufufu770/reArtex.git
cd reArtex
./install.sh
```

### 从源码编译

```bash
cd web && npm ci && npm run build:static && cd ..
cp -r web/out server/webui/dist
CGO_ENABLED=0 go build -tags embedui -o artex ./cmd/artex
./start.sh
```

### CVE 情报服务（sidecar，可选）

`reartex-cve-mcp/` 是独立的只读 CVE 情报服务（MCP stdio / Streamable HTTP + 只读 REST，SQLite 本地库），作为 sidecar 随平台一起运行。**它挂了或没部署，平台照常启动**：单体经环境变量 `CVE_MCP_URL`（冻结地址）连接，不可达即按降级处理、不阻塞任务（B-01E 纪律：服务不可用绝不伪装成「没有漏洞」，单体侧消费随 B 阶段后续卡点亮）。

**预编译二进制模式**（`start.sh` 已内置接线）：

```bash
cd reartex-cve-mcp && go build -o reartex-cve-mcp . && cd ..
./start.sh
```

`start.sh` 检测到 `./reartex-cve-mcp/reartex-cve-mcp` 就会在 `127.0.0.1:8400` 拉起 sidecar（独立数据文件 `data/cve-mcp.db`），并把 `CVE_MCP_URL=http://127.0.0.1:8400` 传给 artex；二进制不存在则跳过、只打一行提示。已有外部/远端 CVE 服务时，显式设置 `CVE_MCP_URL` 再启动，脚本检测到就不拉本地实例。

**Docker Compose 模式**：compose 里的 `cve-mcp` 服务同样取宿主机预编译二进制（只读挂载模块目录），**不发布任何端口**——只在 compose 内网按 `http://cve-mcp:8400` 可达（artex 容器已注入该 `CVE_MCP_URL`），宿主机与公网摸不到；数据在独立卷 `cvedata`，与单体 PostgreSQL 完全隔离（SQLite 不搬进主库）。二进制缺失时该服务起不来（unhealthy 如实显示），artex 不受影响。

- 数据同步：serve 模式默认每 6h 自动同步三源（`CVE_MCP_SYNC_INTERVAL` 可调，`off` 关闭）；也可手动一次性同步：二进制模式 `./reartex-cve-mcp/reartex-cve-mcp sync-once`，compose 模式 `docker compose run --rm cve-mcp sync-once`。
- 同步凭据（可选）用环境变量 `NVD_API_KEY` / `GITHUB_TOKEN` 注入（compose 下写进 `.env`，不写进 compose/config 明文），永不入库不落日志。
- 资源上限：compose 侧 0.5 CPU / 256M 内存；本地模式与 artex 同机共存，无额外限制。

## 使用

- 首次进入 `/setup` 设置管理员密码，之后访问 http://localhost:8787 。
- 左侧功能：仪表盘、任务列表、执行过程、发现、资产、流量录制、对话、Agent 管理、LLM 配置、拦截审批、审批记录、后端日志、系统配置。
- 每个任务：planner 派意图、worker 执行并写回；任务详情的「复测」页签可分页选择本任务的漏洞、查看历次结论与证据，并手动发起复测（结论会同步更新漏洞处置状态）。
- 配置：数据库走 `config.json` 或环境变量 `ARTEX_PG_DSN`；LLM 可用环境变量或在「LLM 配置」页填写；每个任务的 work agent 数在「系统设置」里配置（默认 3）。**不要把数据库连接串写进 config 文件**（`database.dsn` 明文落盘即待治理秘密）：优先用环境变量 `ARTEX_PG_DSN`，或 `ARTEX_PG_DSN_FILE` 指向一次性文件（读入即清零删除）；config.json 若含明文 dsn，会被列入平台秘密清单（agent 工具禁读）且部署检查脚本会告警，迁移用 `scripts/migrate-dsn-to-file.sh`。
- 常用参数：`./start.sh -addr :8787 -proxy :8788`（`-addr` 前端+API，`-proxy` 流量录制代理）。
- 出网治理（A-01/A-15）：带任务身份的出网经录制代理按任务范围判定；未携带任务身份默认拒绝（仅启动期白名单的平台通道例外，见 `ARTEX_PLATFORM_CHANNELS`）；planner/worker 的 WebFetch 与 Bash 出网 = 经录制代理 + 任务身份，代理缺失或不可达即禁用/拒绝启动，不直连。当前未覆盖的通道清单见系统设置页的「出网治理」提示。
- 反向代理：只开放 443、8787 留内网；SSE 必须关缓冲（`proxy_buffering off;`）并给长超时，否则浏览器连得上却收不到事件。
- 远程 MCP：在系统设置里选 `http`（Streamable HTTP）或 `sse`（旧版 SSE）。

## 允许使用范围

- 仅可用于阅读、学习与研究本项目源码；
- 适用于个人学习、学术研究、代码审阅等非攻击性用途；
- 仅限在本地隔离环境中进行技术原理验证。

## 禁止事项

- 严禁对任何网站、线上服务或联网系统发起扫描、探测、利用或攻击（无论是否获得授权、是否为自有资产）；
- 严禁用于任何实际的渗透测试、攻防对抗或生产环境；
- 严禁用于非法入侵、数据窃取、勒索、拒绝服务或任何破坏性、犯罪性活动；
- 严禁利用本工具从事违反所在国家/地区法律法规的行为。

## 合规责任

使用者须自行遵守所在国家/地区关于网络安全、数据保护与计算机犯罪的全部法律法规（在中国大陆包括但不限于《网络安全法》《数据安全法》《个人信息保护法》及相关司法解释）。因使用本工具产生的一切法律责任与后果，均由使用者自行承担。

## 免责声明

本项目按「现状（AS IS）」提供，不附带任何明示或默示的担保。作者及贡献者不对使用本工具（无论使用方式是否得当）所导致的任何直接或间接损失、数据丢失、系统损坏或法律纠纷承担责任。下载、安装或使用本项目，即表示你已阅读、理解并同意上述全部条款。

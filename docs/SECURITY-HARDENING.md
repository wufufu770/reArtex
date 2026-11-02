# reArtex 安全加固说明（历史与设计决策）

> 本文从原 README 拆出，面向开发者与审阅者。面向用户的安装/使用/更新请看仓库根目录的 [README.md](../README.md)。

---

## 重点改了什么

### 第一批 · 安全边界与失效开放

以下 9 项均已直读源码逐条验证属实（非推测），编号沿用《ARTEX限制强化方案-终版V2》。

| 编号 | 缺陷 | 原行为 | 现行为 |
|---|---|---|---|
| G-01① | 平台工具绕过白名单 | 改平台配置的工具若不在 `intercept_enabled_tools` 名单内，在 `IsToolEnabled` 处直接放行，规则根本轮不到求值 | 短路表插在 `applyIntercept` **最前面**，先于 `IsToolEnabled` |
| G-01③ | stdio MCP 无任何白名单 | 命令任意指定，等价于任意第三方代码执行；包名可抢注、无版本 pin 可供应链翻转 | 拒 17 个包执行器（`npx`/`uvx`/`bunx`/`pnpx`/`pipx`/`dlx`/`docker`/`podman`/`sh`/`bash`/`zsh`/`dash`/`python*`/`perl`/`ruby`/`node`），且命令须 PATH 解析成功 |
| G-02①-a | agent 可读平台自身凭据 | worker 的 Bash 与 server 同 UID，可读 `/app/jwt.key` 与 server 原始环境块（含 `ANTHROPIC_API_KEY`、`ARTEX_PG_DSN`） | 硬拦平台签名密钥、MITM CA 私钥与 `/proc/*/{environ,cmdline,mem}`；控制面凭据读入即从进程环境抹除；DSN 另可走 `ARTEX_PG_DSN_FILE`，读入即清零并删除文件，使进程环境块内不再有秘密 |
| G-03 | 拦截规则可被 JSON 转义绕过 | `Match` 只对原始入参字节做正则，`"r\u0065ad /app/jwt.key"` 匹配不上 `read` | 匹配前 JSON 深解码，raw 与 decoded 双形态都试 |
| G-04④ | 工具表故障时全量放行 | `ListTools` 失败 → 打条日志 → `return tools`，DB 一抖动所有 agent 立刻拿到完整工具面 | 沿用上一份已验证快照；冷启动无快照时降级为 SDK 基础件 8 件，**且直接拒绝创建新 run**（白名单未知时开工等于无边界放行） |
| G-07 | 工作区符号链接可越界 | `wsResolve` 只做词法 `Clean`，注释称"爬不出根"——但那对软链不成立；`saveUpload` 跟随符号链接 | `EvalSymlinks` 复核真实路径 + `O_NOFOLLOW` |
| G-09 | steer 消息无上限 | 纠偏消息直入 worker 上下文且不过闸，队列无界 | 限 2000 字符、队列 cap 10 |
| G-04①② | 模型兜底审批 fail-open | 判官超时、报错或输出不可解析时回落 `allow` —— 让判官失效即可绕过全部规则；ask 类规则默认无超时，审批可无限等待 | 失败/超时一律回落 `deny`；ask 规则强制带超时（默认 60s，到点 `deny`）；前端表单初值与后端默认对齐，避免保存一次就把 `allow` 写回 |
| G-17 | 拦截动作无留痕 | 短路表的 deny 只落内存切片（上限 2000、进程退出即失），且与 DB 规则/模型判定混作一谈 | 短路表 deny 写入 `intercept_pending` 已决行，「审批记录」页与任务拦截列表可查；来源标识区分 `rule` / `model` / `shortcircuit` |

### 第二批 · 凭据治理、审计链、门禁与交付链

| 编号 | 缺陷 | 原行为 | 现行为 |
|---|---|---|---|
| G-02①-a | 平台凭据残留在进程环境块 | `ARTEX_PG_DSN` 与平台 API key 留在 exec 期环境块，同 UID 的 agent 子进程可从 `/proc/<pid>/environ` 读走 | 控制面凭据读入即从进程环境抹除；DSN 另可走 `ARTEX_PG_DSN_FILE`（一次性文件，读入即清零删除）；启动自检读 `/proc/self/environ` 给出明确结论，`ARTEX_REQUIRE_ENV_SCRUB=1` 时残留即拒绝启动 |
| G-17′ | 已决判定无法防篡改 | 短路表 deny 只落内存切片（上限 2000、进程退出即失），与 DB 规则/模型判定混作一谈 | 判定写入 `intercept_pending` 已决行（页面可查），来源标识区分 `rule` / `model` / `shortcircuit`；另加 **digest 链**：每条把上一条的 digest 算进自己的 digest，`VerifyAuditChain` 能报出被改动的位置 |
| 类别五 | 无测试 CI | 发布 workflow 只编译五个平台，测试只能本地跑；仓库现有 lint 基线本身是红的，全量设为门等于永久失败 | `ci.yml`：编译 + vet + **全量测试（真实 Postgres，排除项为零）** + 前端构建 + **lint 棘轮门**（只查相对基线改动的文件）；tag 发布前也须过门 |
| — | 测试套件不可重复 | 各测试包共用一个库，前一个包留下的数据让后一个包的用例随机挂（基线上一次挂 10 个、一次挂 16 个，**测试名都不同**），门只能开成红的 | 每个测试包用**自己派生的库**（`internal/testdb`），互不可见；顺带测试不再往开发库灌数据。`llm_usage` 这类不在 `schema.sql` 里、由应用启动时单独建表的，也一并补上 |
| 类别六 | 交付源 404 / 产物无校验 | 镜像与产物指向原作者命名空间（fork 无权推）；「装进镜像的是不是刚编出来的那个」无人把关 | 交付源全部指向本仓库；镜像构建**必填** `--build-arg ARTEX_BINARY_VERSION`，构建时跑一次装进去的二进制核对版本（旧产物 → **构建失败**）；release 的 docker job **发布后冒烟**，把刚推上去的镜像拉回来真跑一次 |
| 类别四 | norma SDK 上游缺陷 | `go.mod` 锁 v0.4.3，原样传导三个缺陷（流解析丢 input usage、丢 Gemini thought_signature、长任务/大输出边角） | 升至 **v0.4.4**，上游已修并自带回归测试 |
| — | `graph_overview` 不展开公司范围 | 返回的 `coverage.scope` 只有任务自身范围 | 展开 `company_scope` 的 domain/ip/cidr/icp/keyword 规则与 `company_keywords`（取值用 `raw` 原文而非规范化列） |

### 第三批 · W-A 波次（分级审核、路径解析、凭据供给面、密钥加密）

这一批做的是把**已登记的活路径**逐条闭合或如实降级。功能面（执行模型、任务编排、探索引擎、对外 API）仍保持上游原样。

| 编号 | 缺陷 | 原行为 | 现行为 |
|---|---|---|---|
| G-01①′ | 平台工具一律要人工确认 | 7 件平台工具全部 ask —— 连「新建技能文件」这种日常操作也要打断 | **三档**：2 件 `deny`（MCP 注册）、4 件 `ask`（改可执行面 / 改既有行为 / 删证据库）、**1 件显式 allow**（新建技能）。显式 allow 是代码常量**主动判定放行**、仍插在判定最前 → 不重开「平台工具绕白名单」的原洞；allow 档判定也进审计链 |
| G-04①②′ | DB 规则的 ask 类可无限等待 | 种子规则照抄 `timeout_enabled=false` → 审批挂着不回来，人不在时该面永远卡住 | 命中 ask 的规则自动补 **60s 超时且超时即拒**（判官侧「不可用即拒」本就是默认值，第一批表里已成立） |
| G-09④⑥ | 审批无防疲劳机制 | 同一 agent 反复请求审批时，人只能一次次点 —— 疲劳点 allow 等于无限循环 | 计数维度 (agent, 工具, 时间窗)，**仅超阈值才**自动拒绝（默认 5 次 / 600 秒，可热改）；单次或少量请求**完全不受影响**；计数失败不升级（宁可多问一次人，不误拒正常任务） |
| G-02①-a′ | 入参路径只做字符串预筛 | `cat $(realpath ../../../jwt.key)` 这类经 shell 展开才现形的路径绕得过短路表 | 入参里的路径片段**按工具工作目录解析成真实路径**再判秘密清单（相对回溯 / 命令替换 / 软链 / 父目录软链都覆盖）；判定点仍在工具策略层（Stage 1 Deny，任何 permission mode 绕不过） |
| — | 工具层拒绝不留痕 | 工具层（Stage 1）早于 `PreToolUse` hook 求值 → hook 根本不跑 → 拿不到短路表那条留痕路径 | 由 server 注入的记录器把工具层拒绝写进审计链（顺带补上 Read/Write/Edit 面既有的老缺口） |
| — | 容器配置面暴露数据库口令 | `docker inspect` 能看到 environment 里的 DSN | 口令改由 **docker secret 文件**供给（`/run/secrets/pg_dsn`），配置面只剩文件路径；`/run/secrets` 进平台秘密清单 |
| — | 平台秘密只靠一次性自检 | 「进程环境块里没有秘密」是一次性结论，没有门守着 | 固化成**发布阻断门**（`verify-secret-env.sh` 进 release 冒烟，每次发布都验） |
| — | LLM key 明文存库 | 拿到数据库口令即可读出全部 LLM key | 改为 **AES-256-GCM 加密落库**（明文列置空），主密钥由文件供给（不进环境块、不进库；放数据目录内拒绝启动），历史明文行启动时自动迁移；AAD 绑定行名 → 改名触发重加密 |
| — | 运行时端到端验证缺失 | 单测覆盖了函数逻辑，但没在真实链路里验过 | 真实装配 → 判定 → 拒绝 → 审计链整条链有回归用例，并做过**反证**（撤掉判定后该用例确实变红） |

> **这一批新查实的两件事**（都写进了口径）：① 工具层（Stage 1）**早于** `PreToolUse` hook 求值 → 工具层的拒绝拿不到短路表那条留痕路径，故须另有留痕通道；② **`cap_drop: [SYS_PTRACE]` 是空操作** —— Docker 默认能力集本就不含它，而**同 UID 的 ptrace 并不需要该能力**，实测容器内读同 UID 进程的已映射内存依然成功。故「`/proc/<pid>/mem`」这条残留**仍然开放**（见下）。

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

第三批（W-A 波次）：31 files changed, 1553 insertions(+), 38 deletions(-)

```
a  pathguard/tokens.go             +145/-0   入参路径片段提取与解析（新文件）
a  pathguard/tokens_test.go        +123/-0
a  config/masterkey.go             +77/-0    主密钥加载与位置校验（新文件）
a  config/masterkey_test.go        +91/-0
a  db/crypto.go                    +77/-0    AES-256-GCM 加解密，AAD 绑定行名（新文件）
a  db/crypto_test.go               +85/-0
a  db/llmkey_migration.go          +54/-0    历史明文 key 迁移（新文件）
a  db/llmkey_test.go               +166/-0
a  server/toolpolicy_e2e_test.go   +71/-0    端到端回归：装配→拒绝→审计链（新文件）
a  verify-secret-env.sh            +45/-0    环境块洁净性发布门（新文件）
a  verify-ptrace-blocked.sh        +42/-0    ptrace 封闭诊断（该条未闭合，供自查）（新文件）
a  init-pg-dsn-secret.sh           +35/-0    由 .env 生成 secret 文件（新文件）
m  db/config.go                    +123/-22  profile 读写密文化 + 改名重加密
m  config/config.go                +39/-2    只读 secret 供给容忍 + 错误信息如实化
m  agent/toolpolicy_test.go        +95/-0
m  agent/toolpolicy.go             +18/-0    接入入参路径判定 + 拒绝留痕
m  agent/assembly.go               +11/-0    工具层留痕注入缝
m  server/assembly.go              +16/-0    留痕接线（audit 链写入）
m  server/manager.go               +14/-0    主密钥位置校验 + 迁移调用
m  docker-compose.yml              +18/-1    secret 供给 + cap_drop
m  .github/workflows/release.yml   +16/-1    冒烟改 secret 供给 + 环境块洁净性门
m  Dockerfile                      +6/-1     两个验收脚本进镜像
m  guard/shortcircuit_test.go      +14/-0    三档断言
m  pathguard/pathguard_test.go     +9/-0     /run/secrets 秘密清单断言
m  db/schema.sql                   +3/-0     api_key_cipher 列
m  .gitignore                      +3/-0     secrets/
m  guard/shortcircuit.go           +2/-0     allow 档
m  pathguard/pathguard.go          +1/-0     /run/secrets 进秘密清单
```

**平台工具的三档归属**（第三批调整后）：`create_mcp` / `update_mcp` = **硬拦**（凭据汇，无人工放行路径）；`create_custom_tool` / `update_custom_tool` / `update_skill_file` / `delete_assets_by_host` = **转人工审批**（60 秒不确认即拒绝）；`create_skill` = **代码级显式放行**（新建技能不覆盖既有行为，日常操作不再被打断）。相对第二批的变化：建 MCP 仍是硬拦，但**建技能从「人工审批」放宽为「显式放行」** —— 这是**有意的行为变更**（用覆盖面换日常可用性），且「显式放行」仍是代码常量在判定最前主动求值，不是漏判。

### 审计过程中额外发现并修掉的三个问题

1. **MITM CA 私钥未受保护**：原注释声称覆盖 `*.pem`，正则里根本没有；而容器里确实存在 `/app/data/traffic/_ca/mitmproxy-ca.pem`（MITM CA 私钥）。已补进拦截表。
2. **规则过宽误伤渗透正常工作**：初版按目录前缀一刀切（`/app/data/**/*.key`、裸 `/\.env\b`）。但 agent 工作目录正是 `/app/data/sessions/<key>`，工作区就落在 data 目录里——**目标的 `.env` / `.key` / `jwt.key` 全被误拦**，而 `.env` 恰恰是渗透最常要读的东西。已改为按具体凭据文件列举。
3. **应急开关是假的**：注释写着 `ARTEX_DISABLE_SHORT_CIRCUIT=1` 可停用短路表，代码里从没读过这个环境变量。已实现（`init()` 读取，启动日志留痕），并把开关内核抽成可测函数。

### 已知残留（未做）

- **平台环境块规则是字符串预筛。** 运行时才成形的路径（`find /proc -name environ -exec cat {} \;`）仍能绕过。闭合靠的是供给侧 —— 凭据根本不进进程环境块，见下一节。
- **`/proc/<pid>/mem` 读同 UID 进程的内存仍可做到**（实测在容器里能读）。这里有个容易想错的点：**同 UID 的 ptrace 不需要 `CAP_SYS_PTRACE`**，所以 `cap_drop: [SYS_PTRACE]`（Docker 默认能力集本就不含它）**挡不住这条**。真正的闭合要让工具进程与 artex **不同 UID**（属 per-task 隔离那一档），或让内存里不留明文秘密。镜像里留了 `verify-ptrace-blocked.sh` 供运维自查（它**故意不进发布门**，因为这条尚未闭合）。
- **平台环境块规则作用在整个工具入参 JSON 上**，故「把含 `/proc/…/environ` 字样的内容写进文件」也会被拦。
- **产物无独立签名**：Release 的 `SHA256SUMS` 与二进制同源（同一个 Release），能篡改 Release 的人可以同时替换两者。当前信任模型是「信任 GitHub Release」。
- **lint 棘轮门是「按文件」判定的**：碰到带着既有 lint 债的文件会让门变红（触碰过的文件已清干净）。这不是 bug 而是该门的语义 —— **碰了脏文件就得顺手清干净**。

本批已闭合（原先也登记在上面，现已消解）：

- ~~短路表是入参字符串预筛~~ → 入参里的路径片段现在**按工具工作目录解析成真实路径**再判定（`pathguard.CheckInputPaths`），`cat $(realpath ../../../jwt.key)` 这类经 shell 展开才现形的路径已被拒；判定点仍在工具策略层（Stage 1 Deny，任何 permission mode 绕不过）。**剩余边界**：运行时才成形的路径（编码/拼接/下载内容决定）仍在射程外，容器内 root 的读取也不在本层覆盖内。
- ~~`docker inspect` 仍可见容器配置里的 DSN~~ → 口令改由 **docker secret 文件**供给，`docker inspect` 的 Env 里只有文件路径。
- ~~`llm_profiles.api_key` 明文存库~~ → 改为 **AES-256-GCM 加密落库**，主密钥由文件供给（不进环境块、不进库），历史明文行在启动时自动迁移。详见[加密落库的 LLM key](#加密落库的-llm-key)。
- ~~G-09 ask 队列治理未做~~ → 已做：计数维度 (agent, 工具, 时间窗)，**只在同一 agent 反复请求同一高危面超阈值时**才自动拒绝（默认 5 次 / 600 秒，可热改）；单次或少量请求完全不受影响。
- ~~尚未做运行时端到端验证~~ → 已做：真实装配（`AugmentTools`）→ 工具层判定 → 拒绝 → 审计链留痕整条链有回归用例，并做过反证（撤掉判定后用例确实变红）。

### 应急回滚

短路表整体停用（重启生效，无需重新构建镜像）：

```bash
# docker-compose.yml 里设，或临时注入
ARTEX_DISABLE_SHORT_CIRCUIT=1 docker compose up -d
```

启动日志出现 `[guard] ⚠ 短路表已停用` 即生效。接受 `1/true/yes/on`。停用后平台退化为「仅 DB 规则」防线——而 DB 规则本身可被持 JWT 者热改。

### 平台凭据不出现在进程环境块

`ARTEX_PG_DSN` 经容器环境变量下发时，会留在进程的 exec 期环境块里，而 `/proc/<pid>/environ` 对同 UID 的 agent 子进程可读；更要紧的是它**还留在容器的配置面**（`docker inspect` 能看到）。

**推荐形态（v0.4.1+）**：口令走 **docker secret 文件**，容器配置面里根本没有口令：

```bash
./init-pg-dsn-secret.sh   # 由 .env 生成 secrets/pg_dsn（0600，不进版本库）
docker compose up -d
```

secret 以只读方式挂到 `/run/secrets/pg_dsn`，`/run/secrets` 已在平台的秘密清单里
（agent 读不到）。**只读挂载的文件删不掉也清不掉**，所以这条路径不适用「读入即销毁」；
它靠的是「不在 environment 里、不在 agent 工作区内」这两点。

若不使用 compose（比如裸跑二进制），可用 `ARTEX_PG_DSN_FILE` 指向一次性文件：

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

## 加密落库的 LLM key

`llm_profiles.api_key` 可以用主密钥加密后落库（拿到数据库口令的人，没有主密钥就读不出 key）。**默认不启用** —— 不配主密钥时行为与从前完全一致（明文落库），只是没有这层保护。

启用方式：

1. 生成主密钥，放到**数据目录之外**（数据目录是 agent 的工作区根，放进去等于没加密）：

   ```bash
   mkdir -p /run/artex
   openssl rand -hex 32 > /run/artex/master.key
   chmod 600 /run/artex/master.key
   ```

2. 在 compose 的 `artex.environment` 里加一行（**只放路径，不放密钥本身**）：

   ```yaml
   ARTEX_MASTER_KEY_FILE: /run/artex/master.key
   ```

3. 重启后，历史明文行会在启动时自动迁移成密文（日志里会打印迁移了几行）。

四点说明：

- **主密钥文件放错位置会被拒绝启动**（在数据目录内即报错），因为那等于把钥匙挂在 agent 够得着的地方。
- **丢了主密钥 = 所有已加密的 LLM key 都解不开**（只能重新填）。请备份该文件。
- 密文的 **AAD 绑定行名**：不同 profile 的密文不能互换（有库写权限的人不能把 A 行的 key 挪到 B 行再配上自己可控的 `base_url` 把 key 送出去）。因此**改名会触发重加密**。
- 平台**不会**把主密钥写进环境块或数据库，只留在进程内存里。

---

## 未来打算改什么

按「还欠着什么」排，不承诺时间。每项都对应上文或[已知残留](#已知残留未做)里登记的东西。

| 计划 | 目的 | 现状 |
|---|---|---|
| **运行时权限边界**（可信授权 + 统一入口 + 任务隔离 + 强制网络出口） | 现在的硬边界主要靠**代码常量**与**凭据不落环境块**；真正的"每个任务一个受限身份、出网只走允许的通道"还没建。计划形态：per-task runner + 最小 env/netns + 默认拒绝直连 | **进行中**：执行层收口已落地（入参路径按工作目录解析并留痕、口令走 secret 文件不进配置面、平台工具分级审核），但 per-task 隔离与净网未做 —— 工作量最大的一项 |
| **产物签名与来源证明** | Release 的 `SHA256SUMS` 与二进制同源，能改 Release 就能同时换掉两者。计划：产物签名（cosign / minisign）+ SBOM + 校验 buildx provenance | 规划中 |
| **测试隔离下沉到用例级** | 现在是**每包一个独立库**，包内用例仍共享一个库（顺序敏感）。计划：按用例隔离或补统一的清理钩子，让用例可任意顺序、可并行 | 规划中 |
| **发布后冒烟扩到多架构 + 不可变标签** | 现在冒烟只验 amd64；Compose 默认用 `latest` 这个可变标签。计划：冒烟覆盖 arm64、Compose 默认改用不可变 tag 或 digest | **部分**：发布冒烟已加**环境块洁净性**阻断门（每次发布都验），arm64 冒烟与不可变标签未做 |
| **上游择取回合的可重复流程** | reArtex **不自动同步**上游（更新只跟自己的 Release），但上游后续仍有变更。计划：把"评估一个上游提交要不要回合"做成有据可依的固定流程，而不是临时手工挑 | 规划中 |
| **生态补齐**（skills 同步 / 更新即重启 / 回滚数据 / 复测导出） | 上游登记的部署与生态类问题：多端 skill 同步、页面更新后的容器换装、数据回滚、复测结果导出 | 规划中 |

一个刻意的选择：**这一批只做边界与门禁，不动功能面**。agent 的执行模型、任务编排、探索引擎与对外 API 形态都保持上游原样 —— 加固应当让系统更可被信任，而不是更难用。

---

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


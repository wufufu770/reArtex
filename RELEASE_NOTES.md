**reArtex** —— 自 v0.15.44 以来的累积发布（0.15.45 → 0.15.95，共 51 个版本）：平台安全硬化 A 批次（Tier-0 边界锁死 / 出网默认拒绝 / 审计链锚定）+ CVE 情报面全家（独立 MCP sidecar 三源同步 / 三态版本区间判定 / CVE-Feed / PoC 情报）+ npm 依赖审计 + 仓库安全监测 Monitor + LLM 轮询与生命周期修缮。

> 维护纪律：本文件描述「下一个 tag」要发布的内容（自 v0.15.44 起的全跨度累积）。
> 标题**不钉版本号**——release 页的 tag 自带版本显示，钉版本就是 v0.15.44 描述
> 陈旧那次的根源（本文件在 0.15.12~0.15.44 期间未更新，tag 一打就原样发布了
> v0.15.11 的文案）。每次发版有新内容时同步更新本文件。

## 平台安全硬化（A 批次 16 卡）

- **Tier-0 不可变安全边界锁死**（0.15.92，门禁参数修正 0.15.93）：guard/netguard/pathguard/SoD 审计等安全核心进不可变清单，rsi_guard Tier-0 硬锁强制化；例外走显式理由 + 提交信息 + SECURITY-HARDENING.md 登记。
- **审计链防篡改**（0.15.89）：audit_events HMAC-SHA256 密钥哈希链 + 外部锚点——挡「改一条」，异常可验证。
- **未归属出网翻默认拒绝**（0.15.86/0.15.88）：planner/worker 联网收口到执行层强制（webAccessFor 门：代理地址与任务身份缺一不可，缺失即禁用/拒绝启动，不给「看似在跑、实际无网」）；netguard 拒绝进审计链 + Docker 形态 netns 内强制层（A-01C）；web_search 装配门四路同口径（A-15B）。
- **skill 信任绑定**（0.15.90/0.15.91）：目录树哈希绑定 + 判定翻转；operator 授信端点 + 启动对账 + 批量创建速率告警。
- **统一受管执行判据**（0.15.95 SEC-01 / A-03B）：判据从 Intent 单源改为三路 SoD 接线，决策账本/审批记录/UI 三面一致。
- **凭据熔断器全家**（0.15.82/0.15.83）：internal/credbreaker（键=profile，连续失败开路）+ mcphttp/ScopeSentry 接线 + 审计恢复事件 + 前端横幅。
- 路径与执行面硬化：pathguard 统一真实路径解析（A-11，0.15.80）；执行器黑名单改真实路径比对（A-07，0.15.81）；MCP stdio 校验对象升级为已打开执行对象身份，TOCTOU 关闭（A-07B fd-exec）；config.json 明文 DSN 纳入密钥清单 + verify-secret-env / migrate-dsn-to-file 工具（A-09，0.15.79）；SoD 降级开关移出热改面 + 装配缺失翻默认拒绝（A-06，0.15.85）。

## CVE 情报面（B-01~B-04、B-06、B-07）

- **独立 CVE sidecar 升级**：sync-once CLI 一次性三源同步（B-01A）；MCP 无状态 Streamable HTTP 传输（`POST /mcp`，B-01B）；REST 路径与成功响应形状零变化核查（B-01C）；compose 独立服务零端口发布 + start.sh 可选拉起（B-01F）。
- **三源同步驱动器**：NVD 全量分页游标续跑（B-02C）；GHSA 多生态 Link 头游标（B-02D）；KEV 快通道日粒度判定线（B-02E）；上游 HTTP 有界重试 + Retry-After（B-02B）；同步错误持久化 + stale 判定（B-02A）；按源互斥/跨源并行调度器（B-02F）。
- **冻结契约与只读面**：`cve_intel.v1` JSON Schema 契约 + IntelV1 DTO（B-03A）；lookup not-found 语义 / search 参数校验 + published_after（B-03B/C）；cve_sync_status 缺源显式 unknown（B-03E）；CWE→真实审计技能显式白名单映射（B-03F）；单体四只读工具经 cveintel 客户端接入统一管道（B-03G/B-01D）。
- **CVE→资产匹配链**：技术栈 token 规范化（B-04A）→ 粗筛候选（B-04B）→ 反向精判（B-04C，B-05 纯求值注入）→ 未终态任务归属（B-04D）→ CVE-hint 幂等台账 + 发出器（B-04E/F）→ 新 CVE 批次触发双水位调度（B-04G）。
- **PoC 情报**：poc_links 聚合存储（复合主键去重 + https://github.com 形态闸，B-06A）；GitHub PoC 元数据搜索双路径客户端（B-06B）；日常≤每日一次 + 每 CVE 小时预算节流（B-06C）；cve_lookup 输出可选 poc_links（B-06D）。
- **CVE Feed 只读面**：`GET /api/cve/feed`（days 白名单 + KEV/CVSS 筛选，B-07A）；显式挂 hint dispatch（幂等台账 + 走既有安全链，B-07B）；web 只读页面 `/function/cve-feed` 五态渲染（B-07C）+ 行内挂 hint 操作（B-07D）。

## 版本区间三态判定（B-05）

- **三态判定器**：affected / not_affected / maybe 纯函数比较器——任何无法确定的结果保留 maybe；OSV events 与 CVE 配置区间统一适配（B-08C 零复制透传）。平台纪律逐字：**CVE/OSV/Monitor/PoC 结果只是情报或 hint，绝不直接形成 finding、授权利用或扩大任务范围**。

## npm 依赖审计（B-08）

- 依赖发现与锁文件优先（workspaces 递归，零网络纯发现，B-08A）；OSV QueryBatch 有界批次客户端 + 三态重试 + 非 npm 生态防御（B-08B）；区间适配三态结果（B-08C）；命中任务时间线事件持久化与去重（B-08D）；上传完成=源码就绪异步审计一次 + 手动重跑入口（B-08E）。

## 仓库安全监测 Monitor（B-09）

- 两张表增量迁移 monitor_repos / monitor_findings（幂等 DDL + fail-closed 校验，B-09B）；仓库配置 CRUD API（requireAuth 内 + SSRF 主机策略 + auth_ref 只收引用，B-09C）；GitHub/GHE commits 只读拉取客户端（增量分页 + 请求时 DNS/拨号双闸，B-09D）；LLM 安全更新二分类（复用现有 LLM pool，不可判 → pending_review fail-closed，B-09E）；增量轮询与游标（15min 节流 + 逐提交游标安全边界，绝不静默跳过，B-09F）；人工处置 API（复现只建未启动任务绝不自动执行 / 重判失败继续 pending_review / 忽略理由必填入审计链，B-09G）；前端页面（四态绝不互相冒充 + pending_review 明确突出，B-09H）。

## LLM 配置修缮

- 轮询手动配置：`POST /api/llm/pool/members` 有序名单重写 + 轮询抽屉直接编辑，激活恒为链首；生命周期四处缺陷修复：删除激活配置即全栈 teardown + 会话取消留痕 + 轮询池显式 opt-in + 启动期一次性迁移。

## 交付质量（验证口径）

- gofmt / go vet / go build 全绿；server / db / agent 三包串行全量回归 ok（795.7s / 427.6s / 113.0s）。
- web：tsc --noEmit、biome lint/format、next build 三绿；e2e 冒烟既有面零回归。
- 敏感信息闸 `scan-sensitive.sh all` 全历史干净（3 枚内容钉定良性 blob 豁免入档 `scripts/scan-exemptions.list`，阳性对照实测仍抓红）。
- Mimosa 深度安全扫描入档（seal sha256:38f1f35d…；B-09 monitor 面 0 发现）。

## 兼容与升级

- 数据库迁移全自动（`monitor_repos`、`monitor_findings`、`cve_hints`、`poc_links`、`cve_hints` 发表时间列、`sync_state` 等幂等迁移），无手工步骤。
- reartex-cve-mcp 保留既有 REST 路径与 stdio 传输；新增 Streamable HTTP `POST /mcp`（B-01B）与 sync-once 子命令（B-01A）；`/api/cve/*` 路径与成功响应形状零变化（B-01C）。
- 默认值翻转需部署侧注意（A 批次硬化语义）：未归属出网默认拒绝（配好出网代理与平台通道白名单即与原行为一致）、SoD 装配缺失/账本不可读翻默认拒绝（0.15.85/0.15.86）；LLM 轮询链新建配置缺省不进链，需在轮询抽屉显式加入。
- CVE / OSV / Monitor / PoC 各面只落情报表，绝不写漏洞 findings、绝不自动建任务/扩 scope（复现操作只经现有人工建任务入口创建未启动任务）。
- 外部凭据只存环境变量或引用（auth_ref），永不入库、不落日志。

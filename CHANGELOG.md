# 更新日志

本项目的重要变更记录在此文件中，格式参考 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)。

> 本文件记录 **reArtex**（ARTEX 的安全加固分支）的变更。reArtex 走自己的发布线，
> 不自动同步上游 ARTEX 的变更。

## [0.15.94] - 2026-11-25

### 文档（TI-05 第二轮核对：删除/归档语义与数据保留方案补档）

- 背景：`02-task-intelligence-next` 提示词包要求对 TI-05 做第二轮差异核对（先读原卡、以当前代码为准）。核对结论：**0.15.63（b0fa426）已实现本卡唯一目标**——hypotheses/facts/evidence_refs/capability_executions 四领域对象落库、ModelDelta 复用 biz_layer_diffs、Finding 复用 findings、统一身份引用列（correlation_id/causation_id/capability_id/scope_version）、假设状态机、`CollabRecordToExecution` 桥（TI-04 台账→执行对象，测试钉住），且 TI-11 第一阶段（`server/ti_api.go`）已在消费该 DAO。
- 唯一缺口（卡面实施步骤 3「删除/归档语义」的归档面 + 步骤 5「数据保留方案」）：本轮以**纯注释**补进 `db/schema_task_intelligence.sql` 头部，无运行时改动——归档 = 假设终态（validated/refuted/dropped，状态机禁止离开）+ append-only 观测面退出活跃查询，不改写历史；保留方案 = 保留期属取证/审计合规决策必须人工拍板、最小领域模型不定 TTL、清理/分区/冷归档留后续运维卡，且须与审计链保留口径对齐（provenance 链不得比 `audit_events` 先消失）。
- 【核对时如实记录的差异说明（不改代码）】`CollabRecordToExecution` 以台账 correlation id 兼作 `execution_id`——契约层 1:1 映射（每条台账记录 correlation 唯一）且测试钉住；§0.11 的 uuid envelope 执行标识由 TI-11 第二阶段接线引入，届时桥应改为显式接收 executionID 参数。现在改无人消费的契约属投机，留报告记录。
- 验证：`go test ./db/ -run TaskIntelligence -count=1` 真库 13.2s 全绿（注释改动 embed SQL 应用无损）；`go test -race ./internal/taskintelligence/` 全绿；`go vet` 干净；gofmt 仅既有漂移（本卡未触碰 Go 文件）；`rsi_guard --staged --enforce --tier 2` 通过（0.15.93 修正后的门）。

## [0.15.93] - 2026-11-25

### 修复（A-16 门禁参数缺陷：`--tier 1` 误拦全部 Tier-2 常规提交）

- `.github/workflows/ci.yml` + `scripts/precommit-test.sh`：rsi_guard 调用参数 `--tier 1` 改 **`--tier 2`**。`--tier` 的语义是**允许层级上限**（0=最严档），0.15.92 落地 A-16 时误写 `--tier 1`——任何触碰 Tier-2（agent/server/db/web 等常规开发面）的提交都会被判「[Tier 2 越层]」拦截，而 A-16 的目标只是**锁死 Tier-0**。实测 A 批区间（92d79c4..HEAD）：`--tier 1` 报 48 条 Tier-2 误报（每次普通推送必红，例外台账「此后的常规推送区间不含 Tier-0 触碰，门自动回绿」的承诺在该参数下不成立）；`--tier 2` 同区间报 0 条 Tier-2、仅剩 15 条已登记基线的 Tier-0 命中——**Tier-0 触碰在任何档位下都违规，锁死强度不变**。
- 缺陷暴露途径（如实交代）：TI-05 第二轮核对（0.15.94）需要改 `db/schema_task_intelligence.sql`（Tier-2），本地门 `--tier 1` 会拦下 TI-05 自己——「门挡住自己的修复」正是 A-16 立法时要防的事。
- 两文件均不在 Tier-0 清单（`scripts/rsi_guard.py` 才是），本修复无需例外通道；参数旁加注释说明 tier 语义，防再犯。

## [0.15.92] - 2026-11-25

### 变更（A-16：Tier-0 不可变安全边界锁死——先修完 A 类、再锁死）

- `scripts/rsi_guard.py`：**强制面收口**。`--tier` 支持最严档 `0`——Tier-1/2 触碰也一律违规（0.15.18 的观察模式允许按层级放行，锁死后最严档下任何清单内文件都不可改）。新增 **`--allow-tier0 <理由>`** 例外通道：仅 `--enforce` 下可用且理由必须非空（人工改 Tier-0 也必须显式留痕——「谁来改」在文件系统层面无法区分，唯一可区分的是改动是否留痕、是否经过显式人类裁决）；例外放行逐条列明并提示登记 `docs/SECURITY-HARDENING.md`。**例外留痕校验**：`--base`（CI/push）模式下校验理由确实出现在区间提交信息里（`git log base..HEAD`，区间不可得退回最新一条——校验从宽但绝不静默跳过）；理由只出现在命令行参数上不算留痕——留痕在提交信息里，任何人 `git log` 可查。
- `.github/workflows/ci.yml`：新增独立 **`rsi-guard` job**（push/PR/workflow_call 全触发，`fetch-depth: 0`，`python3 scripts/rsi_guard.py --base "${{ github.event.before }}" --enforce --tier 1`，命中即失败；`event.before` 为空/全零——首推或历史缺失——跳过并明示），取代 secret-scan job 里 0.15.18 的观察模式步骤——**观察期结束，A-01…A-15 修复批次落地后锁死生效**（卡面执行约束第 5 条的时点要求）。
- `scripts/precommit-test.sh`：新增 **[1/6] RSI 门禁步**（`--staged --enforce --tier 1`，与 CI 同款，先本地拦再 CI 拦）；例外通道走环境变量 `RSI_ALLOW_TIER0="理由"`；其余步骤顺延为 2/6–6/6。
- `docs/SECURITY-HARDENING.md`：新增 **「Tier-0 例外登记」** 节——例外流程四步（PR 描述写明原因 + `tier0-exception` 标签 + 第二人复核/无第二人时由维护者显式确认 + append-only 登记表）；基线整批登记 0.15.74–0.15.91 A 类卡面明载改动的 23 个 Tier-0 清单内文件（**不追溯拦截**——锁死前的修复批次是任务本身，不是例外）；并注明锁死提交所在首推区间会让 rsi-guard 首跑红，人工以登记原句 `--allow-tier0` 放行一次后常规推送自动回绿。
- 【与卡面差异（如实说明）】① CI 命令未带 `--require-evo-id`：本仓全部提交均为主导人工的常规提交（无一处带 EVO 编号），CI 强制校验会让每次推送都失败——evo-id 校验保留给自主改进流水线显式启用（guard 的 `--require-evo-id` 随时可用）；② 卡面称 rsi_guard.py「不存在」已过时（0.15.18 批次 C 已落地观察模式），本项在其基础上收口强制面而非新建。
- 自证：guard 对本提交自身即 Tier-0 命中（`scripts/rsi_guard.py` 在 TIER_0_EXACT 清单内），本次提交经例外通道落地（理由已含于提交信息，`--base` 留痕校验可匹配），锁死自此生效。
- 验证：`bash -n scripts/precommit-test.sh` 语法通过；ci.yml YAML 解析通过；guard 狗粮自测——暂存区 `--staged --enforce --tier 1` 对本批次正确报 Tier-0 命中（退出码 1），带 `--allow-tier0` 后放行（退出码 0，例外逐条列明）。

## [0.15.91] - 2026-11-25

### 变更（A-02 第二批：operator 授信端点 + 启动对账 + 批量创建速率告警）

- `server/skill_trust_admin.go`（新增）：**`POST /api/skills/{name}/trust`**——树哈希失配降级后唯一能把档位升回 trusted 的显式入口。走全局 `requireAuth`；body `{"tier":"trusted"|"untrusted","reason":...}`；先重算树哈希落 operator 记录，再写审计链（`skill.trust`，actor/source=`operator`，理由与 tier 可查）；非法 tier 400、skill 不存在/目录空 404、被拒绝的请求不改档位。
- `server/skill_trust_admin.go`：**`reconcileSkillTrust` 启动期一次性对账**（`server/server.go` 装配在 seedPrompts 之后）。三分支：①无记录 skill 补钉 `treeHash|trusted`——升级前「无记录 = 随镜像分发 = operator 布置」按 trusted 展示，补钉保持列表页展示一致（**无静默翻转**），树同时被钉住，此后记录之外的改动照常失配降级（迁移是加固不是放水）；②存量单文件哈希记录（值恰等于 `sha256(SKILL.md)`）静默换算为树哈希、**档位原样保留**——格式迁移不是重新裁决；③已是树哈希/未知格式的记录一律不动——reconcile 不掩盖任何已发生的改动，失配交给判定路径降级+审计。幂等（迁移后不再命中 legacy 口径，重复启动零开销）；失败不阻断启动（判定路径对无记录 fail-closed，最坏按不可信注入而非放水）。
- `server/skill_trust_admin.go` + `server/platform_tools.go`：**create_skill 批量创建速率告警**——滑动 1 小时窗口进程内计数（重启归零是该口径已知边界，与调度面计数器一致）；阈值 settings 热改（`skill_trust.create_alert_threshold`，默认 10 次/小时；非法值回落默认并在日志说明——配置错误不得变成「关掉告警」）；超阈值第一次记审计事件 `skill.create_rate_alert` + 告警日志，同窗口不重复记链，滑回阈值内重新武装——批量自撰 skill 是 prompt-injection 面的量变信号，值得一条显式留痕。【实现披露】卡片原文「复用 A-08 的计数口径」：本任务包的 A-08（0.15.75）是 config.json 入秘密清单，仓内无可复用的计数机制，故取与调度面计数器同款「进程内窗口 + 重启归零」口径并如实注明。
- 测试（`server/skill_trust_admin_test.go` 新增）：`TestSkillTrustEndpoint`（降级 → 端点授信 → 恢复 trusted + 审计留痕的完整闭环；非法 tier 拒绝且不改档位）、`TestReconcileSkillTrust`（三分支 + 幂等复跑零变化）、`TestSkillCreateRateAlert`（阈值内不告警 / 超阈恰好一条 / 同窗口不刷链 / 非法阈值回落默认）、`TestSkillTreeHashIgnoresGitAndTemp`（`.git` 与临时文件不进摘要——它们的出现与改动不构成 skill 内容变化）。
- 验证：skill 信任/目录树/端点/对账/告警 12 用例 + 平台工具与降权注入关联面全绿（24.9s）；`go build` + `go vet` 干净。

## [0.15.90] - 2026-11-25

### 变更（A-02 第一批：skill 信任绑定从单文件哈希改为目录树哈希，判定翻转）

- `server/skill_trust.go`：`skillContentHash`（只读 SKILL.md）升级为 `skillTreeHash`——`filepath.WalkDir` 按字典序遍历 `skills/<name>/` 整棵目录树，每个文件以「相对路径 + 长度 + 内容」进 SHA-256；忽略 `.git` 目录与临时文件（`*~`/`*.tmp`）；目录不存在、遍历失败、目录为空（只有 .git/临时文件也算空）一律返回错误——没有可复核的内容就不得产出「可信」的哈希记录。**判定翻转**：树哈希失配不再默认 trusted。旧口径的前提是「ARTEX 之外的改动 = operator 亲手编辑」，但同 UID 部署里 agent 与平台共享文件系统，agent 用 Write 工具直改文件与 operator 宿主机编辑在文件系统层面无法区分——失配默认可信等于把「谁能改文件」升级成「谁能授信」。新语义：失配 ⇒ 降级 `untrusted` + 审计事件（`skill.demoted`，actor/source=`platform`，0.15.89 起进 HMAC 链），等 operator 显式复核；失配状态重钉为 `cur|untrusted`——同一次改动的后续注入走稳定路径，审计链只在内容真变时多一条，不按注入次数刷屏。无库可查/无目录/无记录/记录格式坏一律 `untrusted`（fail-closed）。
- `server/skill_trust.go`：`recordSkillTrust` 增加**写入方身份**参数（`operator`/`agent`）：operator 侧（管理接口/信任端点/启动 reconcile）按传入 tier 落记录；agent 身份写入 tier 一律强制 `untrusted`——「信任记录只能由 operator 侧写入」落在函数里，不靠调用方自觉。
- `server/platform_tools.go`：`update_skill_file` 写**任何**文件（不只 SKILL.md）都重钉信任记录——树哈希把 `scripts/*.py` 等引用文件也算进摘要，「只绑定 SKILL.md」的缺口（agent 用工具加引用脚本、SKILL.md 哈希不变而记录不动）就此消除；`create_skill` 落 `agent` 身份（固定 untrusted 不变）。`create_skill` 的短路表显式 allow 保留（新建不覆盖既有文件，行为由动作层护栏兜住；内容信任由本机制收口）。
- `server/server_mgmt.go`：operator 管理接口三处写入面（改前言 / 装 zip / 写文件）重钉 `trusted`；其中写文件从「仅 SKILL.md」扩到任意文件——树哈希下 operator 写 `scripts/` 不重钉会被平台自己的写路径打成不可信；operator 显式动作 = 复核过的新基线。
- `server/skill_trust_test.go`：按新语义重写（原「无记录默认 trusted」「宿主机编辑变 trusted」两条用例钉的是被推翻的旧语义，改为钉 fail-closed 与失配降级）。新增：`TestSkillTrustTreeHashSensitivity`（验收四路：改 SKILL.md / 改 scripts/*.py / 新增文件 / 删文件，树哈希全变、档位全落 untrusted、重钉后稳定）、`TestSkillTrustTierMismatchDemotesAndAudits`（攻击复现：绕开 ARTEX 写入路径直改 SKILL.md ⇒ 下一次判定 untrusted（降权注入由 `skill_demote.go` 触发）+ 恰好一条审计事件；同一次改动不重复审计；内容再变再记一条）、`TestSkillTrustTierAgentWriterNeverTrusted`（agent 传 trusted 落库仍 untrusted）、`TestSkillTreeHashEmptyDirErrors`（空目录/缺目录/仅 .git+临时文件都拒绝）。
- 验证：`go test ./server/ -run 'TestSkillTrust|TestSkillTreeHash'` 8 用例全绿；`TestSkill|TestDemote|TestPlatformTool` 关联面全绿；`go build` + `go vet` 干净。遗留（第二批 0.15.91）：operator 信任端点 `POST /api/skills/{name}/trust`、启动 reconcile（存量单文件哈希记录静默迁移 + 无记录 skill 补标记，无静默翻转）、批量创建速率告警。

## [0.15.89] - 2026-11-25

### 变更（A-05：审计链从无密钥 SHA-256 升级为 HMAC-SHA256，配套外部锚点第二介质）

- `db/audit_chain.go`：**HMAC 升级**。无密钥哈希链挡得住「改一条」（digest 依赖前一条），挡不住「持 DB 写权限者从断点起整条重算」——重算不需要密钥。升级为 HMAC-SHA256：密钥=主密钥的域分离派生值（`DeriveAuditKey`：常量后缀 + SHA-256——同一把主密钥还加密 LLM key，一把钥开两把锁不是加固，审计域拿自己的派生值，泄露互不连带），只进进程内存，不落库、不进 settings、不进日志（`WithAuditKey` 启动装配一次，进程生命周期内不变）。**新旧行共存**：存量 SHA-256 行保持原口径可验，新行 HMAC 且 `source` 纳入摘要（来源标记也是审计事实，不该有「换了来源摘要不变」的重写空间），按行 `key_id`（`''`=旧口径 / `hmac-v1`）逐行选口径复算——链在边界处自然衔接，无需搬数据。`VerifyAuditChain` 在逐行复算之外核对最近锚点：链尾被整段截断时逐行仍验得通，锚点正是为此存在；报「断裂 seq」或「锚点不一致 seq」两类告警，链断裂优先；未配钥实例链上有 HMAC 行 ⇒ 如实报「无法复算」（不是跳过）。
- `db/audit_chain.go`：**启动期一次性重锚** `MigrateAuditChainToHMAC`——旧链逐行验通才把链尾 digest 写成创世锚点（`audit_anchors` 表，`key_id=''` 标记新旧口径边界，此后首条 HMAC 行的 prev 就是它）；幂等（已有锚点 no-op）、链空 no-op；旧链已断 ⇒ 拒绝写锚点并返回断裂位置——断裂是必须人工看的结论，锚点位选在哪都掩盖不了；新行照常追加（链的增长不被历史损坏阻塞）。
- `db/db.go` + `db/schema.sql`：`audit_events` 加 `key_id` 列（行口径标记，存量行默认 `''`）；新表 `audit_anchors`（seq/digest/key_id/anchored_at/note，append-only）。
- `scripts/audit-anchor.sh`（新增）：把链头（seq/digest/key_id/时间）追加写到 `ANCHOR_DIR`——必须在备份目录之外的**第二介质**（另一块盘/对象存储挂载点；空值=拒绝运行，写进备份目录等于没做外部锚定）。理由：锚点只落在同一个数据库里，防得住「改行/重写」，防不住「连同锚点表一起重写」；只有落在 DB 写权限够不到的地方，「持 DB 写权限者单方面重算/截断整条链」才会与外部介质对不上而现形。幂等（同 seq 已锚定则跳过）、链空明确跳过、库不可达非零退出（cron 留痕）。配套 `GET /api/audit/anchor` 只读对账端点（`server/audit_api.go` + `server/server.go` 路由）：无锚点 ⇒ `anchor=null` 且不算不一致（诚实回报而非报错吓人）；锚点与链对不上 ⇒ `consistent=false` + 可读 error；GET 绝不写锚点、绝不「修复」任何东西。
- `server/manager.go`：启动装配——主密钥可用 ⇒ 派生审计钥注入 + 执行迁移；读取失败/未配置 ⇒ 沿旧无密钥口径并打告警日志（不是静默降级）。
- `intercept/audit_chain.go` + `server/scheduler_metrics.go`：审计链追加失败不再无声——error 级日志 + `artex_audit_append_failures_total` 计数器（`/api/metrics` 暴露）。best-effort 语义不变（链写失败不改工具调用结果），但「链缺口」从一行普通日志升级为可观测指标。
- 测试：`TestDeriveAuditKeyDomainSeparated`（确定性/域分离/派生值≠主密钥原文）、`TestAuditChainHMACMixedVerify`（新旧口径混合链自然衔接、未配钥实例验 HMAC 行如实报无法复算、改 HMAC 行的 source 必须现形）、`TestAuditAnchorLifecycle`（Record→Latest→Verify 闭环、锚点被改报不一致、链尾截断逐行验得通但锚点现形）、`TestMigrateAuditChainToHMANGenesis`（创世锚点落链尾、重复迁移幂等、首条 HMAC 行 prev=锚点位 digest 的直接衔接证据、断链拒绝迁移）、`TestAuditAnchorRouteRegistered/Endpoint`（三态：无锚点 null+一致 / 真实锚点一致带明细 / 篡改报不一致+error，全程只读）。
- 【顺带修复（披露）】`db/audit_chain_test.go` 的 `TestAuditChainDetectsTampering`：清理 DELETE 落在 `t.Cleanup` 里而连接池关闭是 `defer`——Go 语义 defer 先于 Cleanup 执行 ⇒ 删行跑在已关闭的池上被 `_, _ =` 吞掉 ⇒ 被篡改的行残留同一测试进程 ⇒ 排在后面的整链校验用例（本次 A-05 三个新用例当场踩中）齐报「断裂在 seq=1」。把 Close 折进 cleanup（先删行再关池）修好；`TestAuditChainEmptyIsIntact` 也因此从「被残留行顶成永远 SKIP」恢复真跑。测试基建实况随记：db 包 `TestMain`（`internal/testdb`）每轮派生独立空库 `artex_test_db` 并在收尾删除——环境变量里的 `ARTEX_PG_DSN` 只作派生基准，测试数据不会出现在该变量指向的库里。
- 验证：db 全包 397.7s 绿；server 审计链 3 用例 + 锚点 2 用例 + 调度指标渲染绿；intercept 全包 31.6s 绿；`go build` + `go vet` 干净。密钥轮换（key_id 预留）为后续项，本版未含。


### 变更（A-15：planner 联网收口——执行层强制取代「默认开启 + 提示词约束」，与 A-01 联动）

- `agent/planner.go`：新增 `webAccessFor(taskID)` 联网门与 `checkProxyReachable` 启动探测。**联网门**：录制代理地址与任务身份缺一不可——任一缺失，`EnableWebFetch=false`（WebFetch 不装配，不再「开着但没人管」）；联网搜索的独立出口代理（`WebSearchProxy`）缺失即禁用 `EnableWebSearch`——「空则直连」是本项要堵的第二条直连旁路：搜索流量不经录制代理，任务范围判定与留痕都看不见。三者齐全时行为与原先一致（WebFetch 经录制代理 + 任务身份）。**启动期 preflight**：代理配置了但不可达（TCP 探测 2 秒，只探可达、不做 HTTP 往返）⇒ 本轮规划拒绝启动并返回可读错误——fail-closed 到轮次启动期，不给「看似在跑、实际无网」的状态；禁用态每轮记一条 A-15 日志（说明缺代理还是缺身份）。
- `agent/runinfo.go`：`egressTaskID` 的陈旧注释随 A-01 翻转修正——「未归属 ⇒ 放行、不受范围校验」改为「未归属 ⇒ 拒绝（例外仅启动期平台通道白名单）」。
- `README.md`：配置节补「出网治理」实况一行（带任务身份 = 经录制代理按任务范围判定；未携带身份默认拒绝、仅启动期白名单的平台通道例外；planner/worker 的 WebFetch 与 Bash 出网 = 经录制代理 + 任务身份，代理缺失或不可达即禁用/拒绝启动，不直连）。【如实说明】原 README 并没有「本地隔离」的功能性声明——只有合规用语（「仅限在本地隔离环境中进行技术原理验证」），该表述本就与实际行为一致；本次补的是行为描述而非纠正错误声明。
- 【与卡片骨架的差异（如实）】① `webAccessFor` 略去未使用的 `ctx` 参数；② 卡片示例里 `BashEnv: proxyEnv(...)` 本就带任务身份，未改；③ `chat.go` / `mainagent.go` 的 `EnableWebFetch: true` 按卡片范围（仅 planner）不动——chat 的无身份出网已由 A-01 在代理侧拒绝（调用点已加注释），mainagent 属任务道、恒带任务身份。
- 测试：`TestPlannerWebAccessGate`（六矩阵：无代理 / 无任务身份 / 负任务号 / 代理+任务但搜索关 / 搜索开但无独立代理 / 三者齐全）、`TestCheckProxyReachable`（监听地址与 `http://` 前缀形态可达、空地址与不可解析地址报错）。`go test ./agent/ -run TestPlannerWeb` 绿（0.9s）、agent 全包绿（113.4s）；`go build ./...` + vet 干净。【验收口径】卡片「停掉录制代理后新建任务，planner 轮次拒绝启动」的机制已实现（Plan 头 `fetchOK → checkProxyReachable → 可读错误`），单测钉在探测函数与门矩阵上；平台级手动场景（整平台起停代理）留人工验收。

## [0.15.87] - 2026-11-25

### 修复（A-07 遗留补丁：TestMgmtAPI 的 stdio 命令适配真实路径校验）

- `server/mgmt_test.go`：`TestMgmtAPI` 的 `command: "x"` 在 0.15.81（A-07 真实路径校验）落地后被 `pgSaveMCP` 400 拒绝——该用例自 A-07 起未随全量 server 套件重跑，坏到本次 A-01（0.15.86）全包验证才现形。本用例只验「登记 → planner 可见性 → 资源侧查询 → 删除」管线、命令从不执行，改用 `/bin/echo`（LookPath + EvalSymlinks 可解析、非包执行器）并加注释说明校验口径；单测恢复绿（14.6s）。
- 【如实记录】全量 server 套件首跑同时发现 `TestDeliverAgentReplyInlinesQuotedMessage` 的 `TempDir RemoveAll cleanup: directory not empty` 偶发竞态——单测复跑即绿，属测试环境 flake（异步 goroutine 落盘与清理竞争），不在本卡处理，留待后续专门收敛。

## [0.15.86] - 2026-11-25

### 变更（A-01：未携带任务身份的出网翻默认拒绝 + 启动期平台通道白名单 + worker 代理环境启动期校验）

- `traffic/egress.go`：`egressDecide` 的「未归属」分支从「放行但标记」翻转为**默认拒绝**——旧语义等于给 agent 留了「不带身份就绕过任务范围校验」的后门，治理被这个口子架空。拒绝文案点名「未携带任务身份」，fail-closed。函数头注释如实记录由此带来的**能力变化**：纯聊天会话（未绑任务）的 WebFetch 走本代理且无身份 ⇒ 被拒，要联网就把会话绑定任务；`agent/chat.go` 调用点同步注释。
- 新增 `traffic/platform_channels.go`：平台通道白名单——「未归属」出网的唯一例外面。`ARTEX_PLATFORM_CHANNELS` 环境变量指向 JSON 文件（`[{"name":"enrich","hosts":[".assets.example.com"]}]`），operator **启动期**配置、不进 settings 热改面（与 A-06 降级开关同一原则：绕过范围校验的口子不能靠点鼠标开）。fail-closed 口径：文件不存在 = 空白名单全拒（不回落放行）；读失败/解析失败 = 空白名单 + 错误交还启动日志。解析归一化（小写/去空白）、空 name 拒绝、重名条目后者为准。裁决规则：`X-Artex-Channel` 头只是**声明**（明文代理协议下可伪造，注释如实标注这不是防伪边界），**主机表**才是裁决——后缀条目（前导点）命中自身与任意子域，精确条目只做精确比对；通道对而主机不命中、通道名未登记、空白名单，一律拒绝。放行理由点名通道（走哪条通道一目了然）。
- `traffic/traffic.go`：白名单放行的**留痕管线**——闸门判定通过后把通道名写进 `X-Artex-Channel-Decided` 内部头（写的就是闸门刚判过的同一请求对象，不经 agent 之手；伪造它拿不到任何放行，至多影响已放行流量的标签），`Requestheaders` 先读后摘（与身份头同口径，绝不转发给目标），按 flow / CONNECT 隧道继承，`exchanges` 表新增取值 `egress_decision=channel`。**语义钉子**：翻转后新记录里出现 `unattributed` 即为泄漏线索（正常路径要么 allow、要么 channel、要么 deny），注释写明。
- `agent/worker.go` + `server/task_llm.go`：卡片一期——`validateProxyEnv` 启动期校验：录制代理在用时，`HTTP_PROXY/HTTPS_PROXY/http_proxy/https_proxy/ALL_PROXY/all_proxy` 六个变量必须逐个等于「带本任务身份令牌」的代理 URL 且 `NODE_USE_ENV_PROXY=1`；被清空/改写/冒用他任务令牌 ⇒ **本次 run 拒绝启动** + 审计事件 `egress.proxy_env_violation`（`SetEgressAudit` 由宿主注入审计链；审计失败只记日志，拒绝不依赖审计）。注释如实标注边界：agent 在命令里临时 `unset` 后**直连**的流量一期尚不能被平台看见——这正是二期 per-task netns 要封的口，本版按卡片分期暂缓（需 `CAP_SYS_ADMIN`/`CAP_NET_ADMIN`，部署可行性待核实，需人工决策）。
- `server/manager.go`：启动装配时加载白名单（`traffic.LoadPlatformChannels(os.Getenv(...))` + `SetPlatformChannels`），日志两行：装载失败按空白名单（未归属出网将全部拒绝）、在册通道数。
- `server/egress_governance.go`：未覆盖通道清单删掉已失真的旧条目「未归属的出网按放行留痕」，换成「平台通道白名单：启动期登记的无任务平台通道按主机表放行，不经任务范围校验（A-01 预期例外，放行逐条留痕）」——按「平台必须说清哪里还没管住」的纪律，例外也如实列出。`governed` 由清单派生的不变式不变；前端横幅是清单驱动渲染，无需改动。
- 【与卡片骨架的差异（如实）】① 白名单注入用独立的 `Traffic.SetPlatformChannels` 而非「随 SetEgressScope 一并注入」——两个配置源解耦，空白名单语义（全拒）不因是否配置过范围判定而漂移；② 通道名取自请求头而非「请求上下文」——录制代理的 auth 钩子只有 `*http.Request`，头是同一请求对象上的传递载体；③ 卡片骨架返回序 `(bool, PlatformChannel)` 调整为 `(PlatformChannel, bool)` 以配合惯用 `ch, ok :=`。
- 测试：`traffic`——`TestEgressDecideUnattributedDeniesByDefault`（默认拒绝+点名原因）、`TestEgressDecideUnattributedPlatformChannel`（命中放行+理由点名通道/主机不命中拒/未登记通道拒/空白名单全拒）、`TestEgressGateDeniesUnattributedWithTrace`（卡片验收 gate 级：403 + `recordEgressDenial` 留痕 task=0 decision=deny + 审计链判定）、`TestEgressGateChannelAllowRecordsChannelDecision`（端到端：白名单通道放行，exchange 记 `channel`，内部头不泄漏到目标）、`TestParsePlatformChannels`/`TestLoadPlatformChannels`（fail-closed 矩阵：缺文件/坏 JSON/目录读失败）、`TestMatchPlatformHost`（七例）、`TestSetPlatformChannelsNilBecomesEmpty`；`agent`——`TestValidateProxyEnvAcceptsWellFormedEnv`/`TestValidateProxyEnvRejectsClearedOrRewritten`（清空/改写/错任务令牌/缺 NODE 变量点名拒）；`server`——治理清单新条目三断言 + 旧说法「按『未经范围校验』放行」禁回归。`go test ./traffic/ ./agent/ -count=1` 绿（10.5s/112.7s）；`go test ./server/ -run TestEgress` 绿（4 项）；`go build ./...` + vet 干净。

## [0.15.85] - 2026-11-25

### 变更（A-06：SoD 降级开关移出热改面 + 装配缺失/配额账本不可读翻默认拒绝）

- `server/sod_chain.go`：降级档开关从 settings 热改项改为**启动期配置**——新增 `ARTEX_SOD_DEGRADE_ENABLED` 环境变量，`sodDegradeEnabledAtBoot()` 仅接受 `1/true`（其余一律 false，不给模糊解释空间），装配链时读一次快照进 `ChainDeps.DegradeEnabled`，进程生命周期内不变。旧 settings 键 `sod.degrade_enabled` 废弃为**只读展示位**（GET /api/settings 回显当前生效值）；`putSettings` 拒绝写入并返回 400 + 明确迁移文案（静默忽略会让前端以为改成功、运维以为关掉了降级）。启动日志新增两行：降级开关生效态、合并闸三态（off/shadow/enforce）；settings 里遗留 `true` 而启动期未开时打迁移提示（不静默吞掉口径差异）。`notifySodDegrade`（OnDegradeChange 通知）不变——降级档进/出仍发通知事件。
- `internal/sod/chain.go`：`ChainDeps.DegradeEnabled` 从 `func() bool`（每轮现读）改为 `bool` 启动期快照；`degradeTracker` 三方法签名随翻——快照 false 恒不降级、关闭期失败不推进计数（`degradeThreshold = 5` 保持代码常量不变）。**`AgentKeyOf == nil` 分支翻转为拒绝**：旧语义「链没装到受管路径上 ⇒ 不 gate（放行）」与整套 fail-closed 方向相反；现在 `PreToolUse` 直接返回拒绝 + error 日志，且连放行清单里的工具一并拒（装配缺失是链级故障）。`RegisterSoDChain` 加装配校验：`AgentKeyOf` 为 nil 即返回错误、不装链、不发布执行器凭证表——fail-closed 推到启动期，服务起不来比不设防好。
- `internal/sod/mergedgate.go`：配额账本不可读（`CategoryUsed == nil` 或读取失败）按「**配额已用尽**」处理——走既有的超限严格裁决通路（auditor 严格裁决 / 人工 ask / 都不成则拒），不再「视为 0 已用 = 不限」；`outOfScopeRoute` 的出网 cap 同口径（账本 nil → 拒绝并注明 fail-closed）。翻转后账本故障最多把调用送去人工/auditor 裁决，绝不静默放宽。
- 【如实差异】卡片验收的「前端 SoD 页降级开关只读」：仓库前端**本就没有**降级开关 UI（grep 全前端无 `sod_degrade_enabled` 引用；写路径只有通用 settings API）——写接口已拒绝并返回明确文案，展示位回显生效值，无 UI 可改；不做无中生有的页面。
- 测试：`TestSodDegradeBootEnvParsing`（12 值解析口径：1/true/TRUE/True/带空白 → 开；0/false/yes/on/2/空 → 关）、`TestSodDegradeSettingWriteRejected`（PUT 400 + 点名环境变量 + 确认不落库）、`TestSoDRegisterSoDChainRejectsNilAgentKeyOf`（nil 拒绝 + 不发布凭证表 + 补齐后可装）、`TestSoDPreToolUseDeniesWhenAgentKeyOfNil`（Bash 与放行清单工具双拒）、`TestDegradeTrackerLifecycle`（bool 化重写 + 关态不计数）、`TestMergedVerdictUnreadableLedgerTreatedAsCapExceeded`（nil 账本走 `cap_exceeded_strict` 严格裁决；无裁决岗无 ask ⇒ 拒）。`go test ./internal/sod/ -count=1` 全绿（15.8s）；server 子集（Register/Wrap/Install/Schema/Degrade）10 项全绿；`go build ./...` + vet 干净。

## [0.15.84] - 2026-11-25

### 变更（A-14：notify 拨号阶段 SSRF 防护 + 凭据脱敏——现状核实为「大部分已存在」，本次补钉测试）

- **现状核实（已实现，证据在库）**：三层 SSRF 防护齐备——`validateHTTPURL`（协议白名单 + 字面 IP 保存期拦截，dingtalk/webhook 同用）、`blockInternalDial`（Dialer `Control` 钩子按**连接时刻** IP 判定，HTTP 系与 `emailDial` 共挂）、`httpClient` 拒跨主机重定向（5 次上限；钉钉 access_token / 企微 key / Telegram bot token 都在 URL 里，跨主机跳转等于交出凭据）；飞书/企微/Telegram 全部走共用 `doJSON`→`httpClient`，无自建 client 绕守卫。错误脱敏比卡片骨架更收敛：4xx/5xx 错误文案**根本不含 URL**（只带状态码 + 响应体 200 字节 snippet），传输层错误经 `redactTransportError` 剥 URL 保底层原因，兜底 `redactURLsInText`；`redactRequestTarget`（= 卡片的 `redactURL`，落在 http.go 而非新建 redact.go——规则一致：只留 scheme://host）。markdown 渲染出口单规则已成立：`writeItem` 全部不可信字段过 `markdownText`（单行化 + 转义），Telegram/HTML 各自输出端转义。
- **本次补缺口（钉测试）**：`notify/rebind_test.go`（新）——① `TestBlockInternalDialJudgesConnectTimeAddress` 直钉 Control 钩子收到连接时刻地址：`127.0.0.1:2379`、`169.254.169.254:80`、`[::1]`、IPv4-mapped 环回、非 IP 地址全拒；公网放行；逃生口 `ARTEX_NOTIFY_ALLOW_LOCAL=1` 在连接层同样生效。② `TestDNSRebindingCaughtAtDialStage`：A-14 验收主体——配置阶段域名目标畅通（字面 IP 判定天然不覆盖域名 = 重绑定的打点），DialContext 夹具把「解析结果」改写到 127.0.0.1，Control 钩子必须在 TCP 握手前以「拒绝投递到本机」真实拒绝（改写只替代 DNS 解析这一步，判定走生产同一路径）。③ `TestRebindFixturePositiveControl` 反向对照：同改写手法 + 显式放开 → 可达，证明拒绝出自守卫判定而非夹具堵路。`pack_test.go` 补 A-14 验收样例的组合注入行：`**critical** [x](http://evil)\n# …` → 粗体/链接/井号全转义、换行折叠，渲染结果无新 markdown 结构。
- 【覆盖口径如实说明】卡片验收「last_error/服务端日志/投递历史三处 grep 零出现」：凭据不外泄的不变量在**错误文本源头**钉住（`redact_test.go` 逐渠道真发必然失败请求 + 本次连接层用例）——last_error、日志、投递历史接口存的都是这同一串错误文本，源头干净则三处全干净；server 侧不另起 DB 级用例。
- 测试：`go test ./notify/ -count=1` 全绿（1.234s，含既有 ssrf/redact/render/pack/channels/email/filter/mask/sign/webhook_template 全量回归）；`go vet ./notify/` 干净。

## [0.15.83] - 2026-11-25

### 新增（A-13 第二批：凭据熔断接 mcphttp/ScopeSentry + 审计/恢复事件 + metrics + 前端横幅）

- `mcphttp/client.go`：凭据熔断接入 MCP 出站链——包级 `credBreaker` + `SetCredBreaker`（server 启动注入，包级单例避免逐实例接线侵入）；`Option`/`WithCredProfile(id)` 按实例携带所属 profile（`New`/`NewSSE` 增变参，未携带 = 不参与凭据记账，stdlib 退路不变）；`roundTrip`/`legacyRoundTrip`/`NewSSE` 握手 GET 三路统一 **Allow → 请求 → Record(status)**：断路中的凭据本地即拒（`credDenied`，不发请求——「先看红绿灯再上路」），响应状态码回灌记账（401/403 累计、2xx 复位、其余码不动）。
- `server`：启动 `SetCredBreaker(s.credBreaker)`；`connectMCP` 与 `sync_scopesentry` 两处 `New`/`NewSSE` 全部携带 `WithCredProfile(m.ID)`——MCP 凭据撞墙也走同一断路口径。
- `server/credbreaker.go`：补 `onRecover`（窗口内一次成功 2xx → 提前结束断路）与断路/恢复双事件——各记一条 `audit_chain`（`Source: "credbreaker"`，Actor=platform）+ 一条 `llm_records`（`Worker: "governance"`，后端日志页可见），对齐 A-13「断路有留痕、恢复也有留痕」。
- `server/metrics_api.go`：`/api/metrics` 增 `credbreaker_open{profile_id,host} 1` gauge（仅窗口内键输出，HELP/TYPE 首键才写）。
- `server/llmpool.go`：`llmPoolStatus` 增 `cred_breakers` 数组（profile_id/host/consecutive/last_reason/open_until，仅窗口内；自然过期键下次快照自动消失）。
- 前端：`types.ts` 增 `LLMCredBreaker`；LLM 页顶部琥珀横幅（与「无备用链」同款）——`配置名 → host（401 ×2，已熔断至 HH:MM）`，让「换凭据还是等恢复」直接可判。
- 测试：`internal/credbreaker` 四用例 + `llmpool` 两用例 + `mcphttp` 全包 + `server` 子集（`TestValidateStdioCommand|TestReverifyStdio|TestPolicy`）全绿；`go build ./...` 通过；`tsc --noEmit` 干净；`build:static` 全页面预渲染成功。

## [0.15.82] - 2026-11-25

### 新增（A-13 第一批：凭据熔断器核心 + DB 持久化 + LLM 链挂接）

- `internal/credbreaker`（新包）：凭据维度熔断器，键 = `profile + host`（host 归一化小写，空 host = profile 全局维度）。策略为代码常量（不进 settings，安全阈值不热改——pathguard `secretRoots` 同款原则）：**连续 2 次 401/403 → 断路 30 分钟**；一次 2xx 即复位；429/5xx 不推进也不清零（只有「连续」的凭据失败才累积）。`Record` 返回 `tripped`（断路瞬间恰好由这次触发）；`onOpen` 回调供宿主记审计/activity；`persist`/`forget` 回调复用 llmpool/health.go 模式（进程内状态权威，DB 镜像）。
- `db/schema_credbreaker.sql` + `db/credbreaker.go`（新增）：`cred_breaker_state` 表（profile+host 主键）+ `SaveCredBreakerState`/`ClearCredBreakerState`/`LoadCredBreakerState`（只取未过期窗口——冷却完毕的凭据重启后视为健康，与 `LoadLLMHealth` 同口径）；幂等 DDL 走启动迁移块（`EnsureCredBreakerSchema`）+ `markRuntimeSchemasReady` 就位标记。
- `llmpool`：`Member.Host`（`HostOf` 从 BaseURL 提取，空 = SDK 默认端点退化为 profile 全局维度）；`Pool.cred`（`New` 增第三参，nil 安全）；`order()` 跳过凭据断路中的成员；成功 `Record(200)` 复位、failover 失败按 `statusOf` 计数（401/403 累计，其余码不改）；断路瞬间进程日志。与池 health 互补：health 管「profile 还能用吗」（首败即硬熔断+冷却阶梯），cred 管「这份凭据对这个端点还要不要撞」（跨冷却探测累计、固定 30min、重启存活）。
- `server`：`Server.credBreaker` 字段 + `newCredBreakerRegistry`（DB 镜像 + 启动 Restore；`m.pg=nil` 退化纯内存）；断路审计/activity 留痕与第二批挂接点（mcphttp / sync_scopesentry）、`credbreaker_open` 指标、LLM 页横幅在 0.15.83 落地。
- 测试：core 四用例（两次 401 断路/成功复位/键独立+大小写归一/重启 Restore 后仍拒绝且过期放行）；pool 集成两用例（`health.Reset` 模拟冷却到期——401 硬失败第一次即池熔断，凭据维度的增量正是跨冷却累计；成功复位后恰好两次 401 断路）。llmpool/internal/credbreaker/sidequestion/db 全绿；`go build ./...` 通过。

## [0.15.81] - 2026-11-25

### 新增（A-07：stdio MCP 执行器黑名单改真实路径比对）

- `server/mcpdiscover.go`：`validateStdioCommand` 从「base 名字比对 17 个执行器名」改为**名字模式 + 真实路径**双规则——名字模式先拦版本化执行器（`python3.11`/`node18`/`ruby3.2` 旧版漏网）与 `uv`/`pip`/`conda`/`poetry`；再 `LookPath` + `EvalSymlinks` 解析真实路径比对（`ln -s $(which python3) /tmp/mypy` 改名软链现形）；`env python3` 形态剥掉 env 旗标/`VAR=` 赋值后按真命令判定。返回值即真实绝对路径。
- 【如实盲区】解释器二进制的**复制体**（`cp /usr/bin/python3 /tmp/x`）路径层判不出血统——复制体的真实路径就是它自己，需文件哈希/行为层，不在本卡范围；软链形态（常见逃逸）已封。
- 登记接口落库真实绝对路径：agent 工具侧 `toolCreateMCP`/`toolUpdateMCP` 已有（复用 `validateMCPDefinition`）；本次补上 **UI 路由 `pgSaveMCP`**——此前完全不校验，执行器命令能存进库只在拨号期失败，现校验失败返回 400、成功以解析结果落库。
- 启动期存量重验：`reverifyStdioMCPs`（`server.go` 启动接线，异步）对全部启用中的 stdio 行重跑校验，不过的记警告并 `enabled=false` 落库——不静默放行等拨号失败冷却。测试用 `onlyNameStore` 收窄重验视野，避免动到共享测试库的种子行。
- 语义变化（如实记录）：登记期起**拒绝解析不到真实路径的命令**（含不存在的绝对路径）——旧逻辑只查形态，坏配置会留到拨号期才炸；`platform_tools_test` 夹具从写死的 `/usr/bin/playwright-mcp`（多数环境不存在）改为真实临时二进制。
- 测试：`TestValidateStdioCommand`（验收四夹具：版本化裸名/绝对路径/env 前缀/改名软链 + 裸名 PATH 解析 + node-exporter 不得过宽）+ `TestReverifyStdioMCPs*`（坏行禁用/好行保持）+ MCP 面回归全绿（server 包 `TestValidateMCP|TestValidateStdioCommand|TestReverifyStdioMCPs|TestShortCircuitMCP|MCP` 16.2s）；guard 的 create_mcp 硬拦回归绿。

## [0.15.80] - 2026-11-25

### 新增（A-11：pathguard 真实路径解析收口——workingDir 口径 + 悬空符号链接拒绝）

- `pathguard/pathguard.go`：新增 `resolveReal(p, workingDir)` 统一全包路径解析（取代 `resolveExisting`）——相对路径按工具 WorkingDir 拼全（此前按进程 CWD，口径与工具实际打开文件的基准不一致）；悬空符号链接（Lstat 可见链接本身、跟随目标不存在/成环）直接拒绝，不再解析成「父目录+链接名」放行；不存在路径仍沿父目录找已存在锚点拼回（写新文件常态不受影响）；workingDir 为空回落进程 CWD。`Check` 签名增 `workingDir` 参数（唯一调用方 `agent/toolpolicy.go` 传 `pc.WorkingDir`，Stage-1 语义不变）。
- `pathguard/tokens.go`：`CheckInputPaths` 真实路径层改调统一 `resolveReal`（删去「不存在就跳过」的 Lstat 短路）；字符串层（Clean 比对）保留作第一道。新增裸文件名候选：无任何路径起点的 token 整体按 WorkingDir 相对名判定——`cd /app && cat jwt.key`、`cat .env` 这类此前整段漏掉的形态补上；超 96 字符或含 `/` 的杂物 token 不进文件系统层（控 syscall 面）。
- `pathguard/resolve_real_test.go`（新增）：卡片四种符号链接夹具（工作区内目录软链读秘密 / 经软链父目录写新文件 / 工作区外秘密软链 / 悬空符号链接，后两种同时盖 tokens 层）+ workingDir 相对路径口径（拒绝与放行双向）+ 裸文件名误伤反例（非秘密同名、正文裸词）。
- 【如实差异】卡片验收原文「软链指向工作区外目录→拒绝」：拒绝面保持**平台秘密清单**——指向非秘密外部路径的软链依设计放行（本包刻意不做根目录收敛，包注释与 Oracle「不要做根目录收敛」红线；收敛会打断「把证据读到本地再分析」的渗透工作流）。已按仓库现状落地并在测试注释中标注。
- 测试：`go test ./pathguard/` 全绿（0.097s，含既有 symlink/tokens 回归）；`go test ./agent/ -run 'TestPolicy|TestAugment'` Stage-1 全绿；`go build ./...` 通过。

## [0.15.79] - 2026-11-25

### 新增（A-09：config.json 明文 DSN 纳入密钥清单）

- `pathguard`：新增 `SetSecretConfigPath`（`RWMutex` 保护，空串忽略）；`cmd/artex/main.go` 启动期检测到 config 文件存在即注入。deny 侧按真实路径比对（== config 或位于其下），拒绝理由点名「平台自身配置文件（可能含明文数据库口令）」。`TestBlocksInjectedConfigPath`：直读 / Grep / 软链现形三路拒绝，未登记（config 不存在）时行为不变。
- `guard` 短路表：凭据形态补 config.json 三形态（`/app/config.json`、`/app/data/config.json`、`../../config.json` 相对回溯）；`/proc/\d+/cmdline|mem` 扩为 `(?:\d+|self)`。放行表回归确认：会话工作区内的目标 config.json 不受影响（渗透工作流零误伤）。
- `scripts/verify-secret-env.sh`（新增）：部署面检查——config.json 含明文 dsn 即警告并 exit 1（CI/巡检可挂）；只检查不硬拦存量启动。
- `scripts/migrate-dsn-to-file.sh`（新增）：python3 标准库完成「提取 dsn → 0600 一次性文件 → `os.replace` 原子删除字段」；提示 operator 设置 `ARTEX_PG_DSN_FILE`（读入即清零删除，readAndBurnDSNFile 语义；未设置则下次启动 fail-closed）。
- `scripts/scan-sensitive.sh`：FORBIDDEN_CONTENT 增加「dsn 值含 @（userinfo@host）」形态规则。偏差说明：任务卡建议字面量 `database.dsn`，实际采用值形态——代码注释与设计文档合法引用该字段名，字面规则会把基线打红（闸门不能拦自己）。
- `config.example.json`：占位 DSN 字面块折进 `_comment_database_dsn` 说明文字——含口令形状的样例与新规则互相打红；规则保持最大敏感度，不为样例开负向豁免。附带修正：`pathguard` 测试夹具不放 dsn 形态字面量（同一互打问题）。
- 【如实披露】`docs/实施基线-v2.1差异清单.md`：真实家目录绝对路径改写为通用形式。该命中系 0.15.57 提交的**既有基线红**（本机路径进库，正是扫描门要拦的类别）；此前闸门只在 push/CI 触发、本地提交不跑，故一直未暴露。本次 `tree HEAD` 验证为 A-09 首次本地跑闸，顺带补漏。
- `README.md`「使用 → 配置」：明确「不要把数据库连接串写进 config 文件」，给出 `ARTEX_PG_DSN` / `ARTEX_PG_DSN_FILE` 优先序与迁移脚本指引。
- 测试：pathguard（含新用例）0.044s 绿、guard 绿、`go build ./...` 通过；两脚本冒烟（verify 对含明文 dsn 的真实配置警告 + exit 1；migrate 完成提取→0600 文件→字段删除）；`scan-sensitive tree HEAD` 干净（exit 0）。

## [0.15.78] - 2026-11-25

### 修复（A-12：guard DSL OR 括号化）

- `db/asset_dsl.go`：`buildDSLWhereBase` 顶层片段一律包括号（此前裸返回的顶层 OR 与调用方范围条件拼接会因优先级错位变成 `X OR (Y AND 约束…)`，X 分支脱离 type/scope 约束）；`whereBuilder.build` 的 and/or 分支 join 结果再整体包一层（双保险：解析/编译/拼接三处各一道括号，单点失效仍有后手）。
- 拼接点核实：四处 `ParseDSL` 调用——`ValidateDSL`（只编译）、`CountDSL`/`QueryDSL`（`AND type=`、`AND $n=ANY(task_ids)`）、`QueryDSLInScope`（`AND` 范围谓词）；括号化后均为原子拼接，无需调用点改动。
- `db/asset_dsl_brackets_test.go`（新增）：确定性用例（顶层 OR 不得脱离约束）+ 属性对照测试（200 组随机 AND/OR/括号表达式，SQL 求值 == 内存 AST 求值，含类型与范围过滤）。
- 测试：db 包 DSL/Query/Asset/Scope 面 43.5s 全绿。仓库内唯一 AND/OR DSL 已 grep 核实为资产 DSL（guard/ 内无第二套）。

## [0.15.77] - 2026-11-24

### 新增（A-10 前端：拦截规则编辑页 tool_scope）

- `web/src/lib/types.ts`：`InterceptRule.tool_scope?: string[]`。
- `web/src/app/(main)/system/intercept/page.tsx`：规则编辑器「作用工具」多选（候选 = 全部已知工具 ∪ 拦截范围工具，支持手填未注册的 MCP 工具名，留空 = 全局）；规则表新增「作用域」列（全局 / N 个工具，悬停看全量）；`openNew`/`openEdit` 表单口径补 `tool_scope`。
- 验证链：`tsc --noEmit` 通过；`build:static` 通过。biome check 因仓内配置 schema（2.5.1）与本地 biome 二进制（2.6.8）版本不匹配报配置错误——既有环境差异，非本次改动引入，未顺手改 biome.json（属无关重构）。

## [0.15.76] - 2026-11-24

### 新增（A-10 后端：intercept 规则加 tool_scope）

- `db/schema_intercept_scope.sql`（新增）+ `db/intercept_scope.go`（新增）：`intercept_rules.tool_scope TEXT[] NOT NULL DEFAULT '{}'`（空数组=全局，存量规则语义不变）+ GIN 索引；幂等 DDL 走启动迁移（`EnsureInterceptToolScopeSchema`，迁移块 + `markRuntimeSchemasReady` 就位标记，v0.9.16 模式）。
- `db/intercept.go`：`InterceptRule.ToolScope` 字段；`CreateInterceptRule`/`UpdateInterceptRule` 增 `toolScope` 参数（`normalizeToolScope` 归一：非 nil/去空串/去重）；TEXT[] 经 `array_to_json` 读出（pgx stdlib 兼容）。
- `intercept/intercept.go`：`ruleMatches` 先判 scope（非空且工具不在 scope 内整体跳过）；`toolInScope` 支持精确名与组前缀 `前缀:*`（同前缀且后随 `_ - .` 分隔符，防 `shellx` 误命中）。
- `server/intercept.go`：`interceptRuleReq.ToolScope` 透传 CRUD；`server/trust_rules.go`、`server/task_scope_rules_test.go` 调用点补参数。
- `intercept/toolscope_test.go`（新增）：scope 收窄不误伤 / 空 scope 全局 / 组前缀边界 / db 往返与归一 / 端到端 Match / scope 含未注册工具名不报错。
- 测试：intercept 31.9s + db 全包 399s + guard 14.4s + server（Intercept/Trust/TaskScope/Evidence 面）21.4s 全绿。前端编辑器（tool_scope 多选）为下一小版本 0.15.77。

## [0.15.75] - 2026-11-24

### 变更（A-08：审批防疲劳扩大到三档 ask 来源）

- `intercept/intercept.go`：`AskBurstExceededFor`（三档统一口径，按 `decision_source` 分档计数、互不挤占）；`HandleAsk` 入口对 DB 规则/判官 ask 在落 pending 前检查（拒绝不产生新 pending 行、不进入等待分支，落注明防疲劳的 denied 观测行）；`decisionSourceOf` 与 `db.interceptSource` 同口径；`New` 启动日志打印阈值与覆盖三档。
- `db/intercept.go`：`CountRecentInterceptAsksBySource`（按来源计数；原单来源函数保留，陈旧的「DB 规则不计入」设计注释按 A-08 决策更正）。
- `guard/guard.go`：短路 ask 分支显式走三档口径的 shortcircuit 档。
- `docs/SECURITY-HARDENING.md`：G-09 覆盖面更新为三档。
- `intercept/askburst_sources_test.go`（新增）：三档独立计数 / 旧口径不变 / 入口拒绝不等待 / 工具隔离 / 来源推导同口径。
- 测试：intercept 防疲劳面 23.2s + guard 回归 12.1s + db 拦截面 18.8s 全绿。

## [0.15.74] - 2026-11-24

### 变更（A-04：Audit #42 两项移植）

- `intercept/decode.go`（新增）：入参匹配面全量递归解码——全部字符串值（含未知键嵌套、键名、字符串内再嵌 JSON 文本的双重编码）进规范化文本；结构深度上限 8，超限子树停止展开（raw 原文始终兜底匹配，解码只加宽不收窄；标量沿用历史 re-marshal 口径避免存量规则匹配面收窄）。
- `intercept/intercept.go`：`NormalizeToolInput` 从 9 个已知顶层字段改为全量递归 walk；判官失败默认锁死（Audit #42 第二条）——`judgeFailActionEffective` 把存量 `llm_judge_fail_action=allow` 在确认标记缺失时运行时按 deny 执行（不自动改库），`New` 启动期迁移检查记警告（进程内一次）；`SetJudgeConfig` 写 allow 需显式 `confirm_fail_open=true`（`ErrFailOpenConfirmRequired`），确认落标记、离开 allow 清标记；`JudgeConfig` 增 `fail_action_effective`/`fail_open_unconfirmed`/`confirm_fail_open` 三个字段（读出/写入口径）。
- `server/intercept.go`：未确认写 allow 按 400 返回（`errors.Is(ErrFailOpenConfirmRequired)`）。
- `intercept/audit42_test.go`（新增）：转义绕过矩阵（`\u0063url`/未知键嵌套/键名/双重编码/非法 JSON 回落/深度帽）+ 判官三态（出错/超时/不可解析 × FailAction 默认）+ 存量 allow 运行时翻转 + 确认门。与 XIB-Artex CHANGELOG Audit #42 两条逐条对齐。
- 测试：`go test ./intercept/` 全包 22.2s 通过；`go test ./guard/` 回归 12.4s 通过。

## [0.15.73] - 2026-11-24

### 新增（TI-15：本次优化的完成定义·九条件验收清单）

- `docs/完成定义-0.15-验收清单.md`（新增）：§0.15 九条件逐条对账（3 ✅ / 5 🔶 / 1 ❌，每条带证据与缺口）；后续任务依赖序（TI-11 第二阶段 → 事件生产方注册 → TI-08-a/b/c → CRSI 接入）；**0.16.0 中版本边界建议**（TI 批次完成或 TI-11 第二阶段完成，由用户点名，不自行切换）。总结论如实：骨架就位、端到端闭环未达成，「功能割裂真正解决」以运行链接线后的端到端测试为准。

## [0.15.72] - 2026-11-24

### 新增（TI-14：对原 118 项和现有模块的映射）

- `docs/118项映射-0.14-复用适配关联.md`（新增）：方案映射表 × 仓库落位（D-13 调度难度与算力档术语红线、E-* 晋级对接假设状态机、F-* 报告数据源扩展、偏锋/建模/Traffic 三行的「保留+写入方后续」口径）；映射纪律四条（复用优先/适配不重写/关联靠引用列/调整项显式登记到任务）。不推翻任何原模块。

## [0.15.71] - 2026-11-24

### 新增（TI-13：分阶段迁移与验收·对账）

- `docs/分阶段迁移与验收-0.13-对账.md`（新增）：TI-0~TI-6 七阶段对账表（TI-0 ✅；TI-1~TI-5 🔶 数据/规则/契约面就位、运行链接线显式标记后续，各自带版本证据；TI-6 CRSI ❌ 未开始、接入硬约束已成文）；§0.13 八条集成测试覆盖映射（#5 幂等投递、#6 环路阻止已有确定性测试 ✅；#4/#7 部分覆盖（取证面有、拦截面待接线）；#1/2/3 待接线、#8 未开始）；验收口径锁定「表/规则/契约落地 ≠ 阶段完成」，不提前写 mock 假绿测试。

## [0.15.70] - 2026-11-24

### 新增（TI-12：安全与数据隔离·事件 payload 消毒 + 不变量扩展）

- `db/sanitize.go`（新增）：§0.12「事件 outbox 不得写入凭据/Cookie/Authorization/原始敏感正文」的**确定性消毒门**——凭据类键清单（`EventCredentialKeys`，白名单非启发式）叶子值改写 `[redacted:use evidence_ref]` 占位符（键名保留、改写路径可观测）；payload 超 64 KiB 或非 JSON 整体拒绝（大内容走 evidence_refs 受控引用）。执法点选在**存储单点** `db.EnqueueEvent`——生产方无法绕过，且避开 events↔db import 环。
- `db/events.go`：EnqueueEvent 接入消毒门（空 payload 默认 {} 后消毒）。
- `docs/安全不变量清单.md`：追加 INV-12（事件 payload 消毒，含执法点/回归入口/键清单评审边界）与 INV-13（能力执行 scope 水位取证面——at_start/end 双水位 + ScopeDrifted；运行时拦截点随 TI-11 第二阶段接入，如实标注当前只覆盖取证）。
- 测试：`db/sanitize_test.go` 4 用例（凭据改写三处+标记块/干净直通不加壳+数组嵌套命中/非 JSON+超限拒绝/EnqueueEvent 落库单点强制）全绿。

## [0.15.69] - 2026-11-24

### 新增（TI-11：API 与工具契约·第一阶段 task-scoped API）

- `server/ti_api.go`（新增）：§0.11 按卡自带的分阶段口径落地第一阶段——六个 task-scoped 端点：`GET /api/tasks/{id}/intelligence`（ti.context.v1 契约形状，Provider 未接线时分节显式 unavailable + `providers_wired=false` 如实声明，不伪造空态）、`GET/POST /api/tasks/{id}/hypotheses`（写操作服务器端重载：任务存在性校验→服务器算指纹→服务器取 scope 水位，请求体不携带不信任任何 scope 字段；指纹命中返回既有 id）、`GET /api/tasks/{id}/model-deltas`（复用 biz_layer_diffs 任务维度查询，不另建库）、`GET /api/tasks/{id}/capability-runs`、`GET /api/tasks/{id}/timeline`（假设/能力执行/证据/事实四类对象按时间合并，封顶 200）。
- `server/server.go`：tiRoutes 注册（bizRoutes 之后一行）。
- 契约边界（文件头成文）：task identity 只来自路径；`capability_run` 等执行类端点与 9 个 Agent 工具（task_context_get/capability_list/…）**本阶段不开放**——执行必须走统一工具安全管道，接线归 TI-11 第二阶段，宁缺毋假；删除类动作不进本 API。
- 测试：`server/ti_api_test.go` 2 用例（六端点往返+去重语义+时间线读回 / 404 先于 400 的重载校验顺序+缺 statement 400）全绿。

## [0.15.68] - 2026-11-24

### 新增（TI-10：建议的 Go 模块边界·现状映射）

- `docs/Go模块边界-0.10-现状映射.md`（新增）：§0.10 建议树逐行落位表（context/capability/events/db 四处一致；envelope 语义拆 collaboration+domain；coordinator/adapter 标记接线期再建）；边界纪律成文化（依赖方向 events→db、SQL 全留 db 包、taskintelligence 零 db 依赖、不预建空目录、Tier-0 不涉）。不建 domain/、exploration/、businessmodel/ 空包，不迁移现有包。

## [0.15.67] - 2026-11-24

### 新增（TI-09：Knowledge Engine 与 SIM/CRSI 的边界）

- `docs/知识引擎与SIM-CRSI-0.9-边界契约.md`（新增）：三系统×现状载体映射（KE=edge_methodologies+策略库+PeakSearch；BizModel=biz_* 五表双维度隔离；SIM/CRSI 本体不存在，eval/ 仅黄金基线前身）；§0.9 五条禁止事项逐条落位到现有确定性防线（rsi_guard Tier-0、fact_kind observed/inferred、TI-04 差异转假设门槛）；SIM/CRSI 接入的 4 条硬约束（数据最小化走受控引用、经验复用边界、事件口径、Tier-0 红线）。SIM/CRSI 本体实施显式标记为 §0.13 TI-6 阶段后续。

## [0.15.66] - 2026-11-24

### 新增（TI-08：统一任务工作台·前端信息架构审计与映射）

- `docs/任务工作台-0.8-信息架构审计与映射.md`（新增）：任务详情页现状盘点（顶部态势 + 12 tab + 数据面）、§0.8 四区布局逐区映射表（顶部态势栏缺 AUTO 难度/已调用能力展示、左侧导航缺假设/时间线数据面、中间活动流缺能力选择原因、右侧缺三对象互链）、功能入口保留要求逐条核对（偏锋启动结构性任务绑定 ✅ / 业务建模新鲜度显示 🔶 / 方法论运行时 version/rationale 痕迹 🔶）、关系链串联缺失分析。
- **工作台实施显式标记为后续任务**（依赖 TI-11 API 生产方接线 + 事件闭环接线，预计 >5 前端文件，按卡约束拆分为 TI-08-a/b/c 三张子卡），本批不实施、不重构现有 tab、不动 api.ts 既有接口。

## [0.15.65] - 2026-11-24

### 新增（TI-07：AUTO 与 Single-Agent 的统一调度规则）

- `internal/taskintelligence/scheduling.go`（新增）：§0.7 调度规则的**确定性决策层**（纯函数，零 IO/零工具/零 LLM）——`AutoDecision`（方案字段一字对应）+ `DecideAuto` 按快照八分节信号产出建议：目标规模→难度（easy/expert 双硬信号）、业务缺口→建议补建模（复用 TI-04 `EdgeNeedsBusinessModel`）、证据覆盖比<0.2→建议 exploration.edge、假设积压>10→建议 validation.deep+深度抬升、发现清零→建议 reporting.result、历史执行量>100→预算收紧只降不升；`NormalizeAutoDecision` 收口（能力×原因同步排序保持一一对应，剔除未注册/停用能力并留尾注）；`ClampDepthMax` 风险上调时的**唯一下调入口**（刻意没有上调函数）。
- 安全结构化：原则 5「AUTO 不能修改授权范围/审批/策略」落在结构体上——`AutoDecision` 无 Scope/Approval/Policy 字段，测试用反射字段白名单守卫设计回退。术语边界：本层 Difficulty 是调度目标难度，非 execution_profile 算力档，两档映射归接线任务（TI-14 D-13 登记项）。
- 测试：`scheduling_test.go` 6 用例（信号映射七态/降级三态/注册表过滤/只降不升/字段白名单）全绿含 -race；修复 Normalize 排序拆散能力-原因配对的真实 bug。

## [0.15.64] - 2026-11-24

### 新增（TI-06：统一事件流与反馈闭环）

- `events/event.go`（新增）：§0.6 统一事件契约——`DomainEvent` envelope（方案字段一字对应，TenantID/ScopeVersion 与仓库 company_id/水位 bigint 的投影语义写在字段注释）+ 方案表格全部 12 个事件类型常量（finding.created/updated 拆两值）+ `ValidateEvent` 入口契约门（缺 ID/Type、schema_version≠当前契约、nil payload 拒绝）。包注释载明三铁律：先落库再派发 / 幂等可重试可观测 / 事件连接而非硬编码。
- `db/schema_task_intelligence.sql`（追加）：`domain_events` outbox 表（event_id 唯一=生产端幂等键、status pending/dispatched/failed/dead、attempts/next_attempt_at/last_error 可观测列）+ `domain_event_consumers` 消费幂等台账（event_id+consumer 主键）。
- `db/events.go`（新增）：outbox 存储层——EnqueueEvent（ON CONFLICT 去重返回既有行）、ClaimDueEvents（事务内 FOR UPDATE SKIP LOCKED 多实例安全，崩溃遗留行保持可认领=at-least-once）、MarkEventDispatched/MarkEventFailed（退避时间由派发器算，存储层不算策略）、MarkConsumerProcessed/HasConsumerProcessed。
- `events/dispatcher.go`（新增）：派发器——窄接口 OutboxStore（*db.DB 结构性满足）、RegisterConsumer（同名唯一=幂等键，类型过滤，稳定执行序）、PollOnce/Start 轮询循环、逐消费者隔离（panic recover 按失败处理）、失败指数退避（30s×2^n 封顶 10m）、超 MaxEventAttempts=5 入 dead；`ToRecord`/`envelope` 双向投影。接线边界：生产方与 server 启动装配归 TI-11 及后续。
- 测试：`events/dispatcher_test.go` 7 用例（happy+类型过滤/重投幂等跳过/重试到 dead/panic 隔离/注册规则/无消费者事件照常收尾/双向投影+契约门）全绿含 -race；`db/events_test.go` 2 用例（真实 PG：入队去重/认领字段/退避不可认领/dead 死信/消费台账）全绿。

## [0.15.63] - 2026-11-24

### 新增（TI-05：统一领域对象与数据关系）

- `db/schema_task_intelligence.sql`（新增，幂等）：§0.5 四类缺失领域对象——`hypotheses`（(task_id,fingerprint) 唯一去重=跨能力合并的库层兑现）/`facts`（observed/inferred 分kind，无来源不算事实）/`evidence_refs`（只存受控引用+hash，正文留 traffic）/`capability_executions`（execution_id 回指锚点+idempotency_key 幂等+scope_version_at_start/end 取证面）；另按 §0.13 阶段 TI-1「统一身份与引用」给 `edge_runs`/`biz_layer_diffs` 补 correlation_id/causation_id/capability_id/scope_version 列与索引（全加法，可回滚=DROP）。ModelDelta 不另建表（biz_layer_diffs 已是事实源），Finding 复用 findings 表（不建平行漏洞实体）。
- `db/task_intelligence.go`（新增）：EnsureTaskIntelligenceSchema（启动期接入 db.go 迁移块+role.go 运行期免建表双登记）+ 访问层——ScopeVersionForTask 范围水位（task_scope MAX(id)，任何范围插入抬升）、InsertHypothesis（指纹冲突返回既有行 deduped=true）、状态白名单推进、InsertFact/InsertEvidenceRef 必填守卫、StartCapabilityExecution（幂等键命中复用既有执行）/CompleteCapabilityExecution（终态白名单）/三个 List。
- `internal/taskintelligence/domain.go`（新增）：§0.5 领域契约与确定性规则——Hypothesis/Fact/EvidenceRef/CapabilityExecution 契约+校验、假设状态机（proposed→validating→validated/refuted/dropped，禁止跳步、终态不可离开）、ScopeDrifted 水位漂移判定、`CollabRecordToExecution` 桥（TI-04 台账 → §0.5 执行对象）。方案建议的 domain/ 目录不单独建包（落位理由见文件头）。
- 接线边界：本层只有 schema+访问+契约，无 server/db 生产调用方——Provider 接线与 API 归 TI-11，事件闭环归 TI-06。
- 测试：`db/task_intelligence_test.go` 3 用例（四对象往返/守卫路径去重与拒绝/scope 水位抬升）全绿；`domain_test.go` 6 用例（契约校验/状态机/漂移/台账桥）全绿；全 db 包套件 382s 全绿。

## [0.15.62] - 2026-11-24

### 新增（TI-04：偏锋探索与业务建模双向协作控制面）

- `internal/taskintelligence/collaboration.go`：§0.4 双向协作的**确定性规则层**（纯函数+进程内台账，零 IO、零工具执行）：
  - 偏锋→建模：`EdgeNeedsBusinessModel` 按快照判新鲜度/覆盖（无模型、分节失败、超 24h 过期、未收敛差异>20 → 建议补建模，宁补勿猜）；
  - 建模→偏锋：`DeltaShouldBecomeHypothesis` 仅范围内且 severity≥high 的 `biz_layer_diffs` 差异建议转**待验证假设**（范围外直接拒绝，对齐 INV-1；建议 ≠ 漏洞结论）；
  - 去重：`HypothesisFingerprint` 规范化指纹（任务隔离，空白/标点无关）；
  - 防循环：`CollabLedger.AllowAutoFeedback` 以 causation×目标能力为幂等键，`MaxAutoFeedbackPerKey=2` 硬上限，拒绝不占预算，新 causation 重置；`CollabRecord` 承载触发原因/耗时/Token/结果状态（落库归 TI-05）。
- 测试：`collaboration_test.go` 8 用例（新鲜度四态/差异转假设四态/指纹稳定性/循环上限/新预算/环形容量/-race 并发冒烟）全绿。
- 验证链：`gofmt`/`go vet` 干净；`go test -race ./internal/taskintelligence/` 通过；安全六包回归（guard/intercept/sod/sodpolicy/pathguard/netguard）全绿。事件闭环归 TI-06，server 接线归 TI-11。

## [0.15.61] - 2026-11-24

### 新增（TI-03：统一能力注册表）

- `internal/taskintelligence/capability.go`：`CapabilityDescriptor`（§0.3 字段口径：ID/Version/Input-OutputSchema/RequiredContext/RequiredScopes/RiskClass/CostClass/DefaultPolicy）+ 并发安全 `Registry`（Register 拒重/Lookup/ListEnabled/Disable/Enable/Replace 显式换版）。
- 五个标准能力按方案表格一字注册：`testing.standard` / `exploration.edge` / `modeling.business` / `validation.deep` / `reporting.result`，各自映射仓库现有工具族（BoundTools 登记映射，非调用接线）。
- 注册项校验：ID 点分小写格式、version 必填、风险/成本档枚举、RequiredContext 必须引用 `TaskContextSnapshot` 真实分节名——登记项引用不存在的上下文面直接拒绝。
- 测试：`capability_test.go` 7 用例（标准能力齐全性/登记查询/重复拒绝+显式换版/5 种非法登记项/开关往返）全绿；接线与 Specialist 委派按卡边界归 TI-11/TI-07。

## [0.15.60] - 2026-11-24

### 新增（TI-02：Task Intelligence 上下文快照层）

- 新增 `internal/taskintelligence` 包：`TaskContextSnapshot` 只读聚合视图（契约号 `ti.context.v1`）+ 8 个窄 Provider 接口（scope/assets/exploration/biz_model/facts/evidence/findings/history）+ `SnapshotBuilder.Build`。
- 边界即契约：包注释明示 TI 层只读聚合与建议——不执行工具、不替代 scope/SoD/审批/审计、不拥有第二套状态机、不让 LLM 裁决安全策略（对齐 `docs/安全不变量清单.md` INV-10）。
- 失败语义：单分节失败不拖垮整份快照，未接线分节显式标记 Unavailable（不伪造空数据）；`taskID<=0` 与已取消 ctx 拒绝构建。
- 测试：`context_test.go` 5 用例（主路径/单分节失败隔离/nil provider 显式可见/非法输入/cancel 传播）全绿。
- server 侧 Provider 接线与 API 暴露按任务卡边界**明确归 TI-11**，本版不含。

## [0.15.59] - 2026-11-24

### 文档（TI-01：Task Intelligence 割裂审计与四条件映射）

- 新增 `docs/任务智能层-0.1-割裂审计与四条件映射.md`：对 0.1 节四个孤岛逐条核对现状（偏锋探索任务化但无事件回写、biz_layer_diffs 与假设/证据无引用关联、全库无 correlation/causation/execution_id 与 scope 版本快照、前端 10 张独立功能卡）；四条件（同一身份/同一上下文/同一结果契约/同一反馈闭环）逐条映射到归属任务卡（TI-02/03/04 本轮、TI-05/06/07/08/11 后续）。
- 关联 ID 审计结论：`correlation_id`/`causation_id`/`execution_id` 全库零落库，任务身份现仅 task_id + chat 群绑 RunInfo（`server/conversations.go:570`）。

## [0.15.58] - 2026-11-24

### 文档（BASE-01：全局安全不变量清单）

- 新增 `docs/安全不变量清单.md`：11 条不可绕过安全不变量（授权范围 fail-closed、任务/工作区隔离、出网治理、SoD 链、命令拦截与审批、删除类动作管控、审计链 append-only、凭据与密钥保护、Tier-0 不可变边界、确定性裁决、额度与熔断 fail-closed），每条给代码入口、受保护文件、回归测试入口与当前缺口。
- 落定 rsi_guard Tier-0 精确清单（8 个整目录 + 29 个单文件硬锁 + intercept/prompt.go Tier-2 例外）作为受保护文件基线。
- 新增后续任务卡通用「安全核对清单 7 问」与安全回归测试统一入口（安全包全套 + 审计链 + rsi_guard --staged + scan-sensitive）。
- 差异声明：A-01/03/05/06/09/12/13/15 与 DeleteApproval 等未落地条目按缺口如实标注，本清单只保证「安全不回退」，不声称「安全已覆盖」。

## [0.15.57] - 2026-11-24

### 文档（BASE-00：实施基线与差异清单）

- 新增 `docs/实施基线-v2.1差异清单.md`：对 `feature/biz-edge-modeling` @ `4196442`（0.15.56）做只读核对，把 v2.1 全量总方案 118 项 + TI/AUTO/CRSI 章节逐项标注 已实现/部分实现/未实现/不适用，全部结论带文件+符号证据。
- 关键结论：仓库已先于方案落地单循环、loopguard、四审计 skill、偏锋探索/业务建模、execution profile、cve-mcp、amber/veto/peaksearch/sidequestion、IM 推送、DOCX/PDF/SARIF 报告等（H/I/K/L/F/B 类大半）；方案排序最高的 A 类安全修复 16 项中 A-01/02/03/05/06/09/10/12/15 明确未实现；TI/CRSI 统一层未建。
- 登记需人工决策事项 5 条（A 类是否插队、算力 Difficulty 与用户难度术语分离、goal_met 语义与 task_conclude 治理线合并、rsi_guard 强制化时机、行号漂移以符号定位）。

## [0.15.56] - 2026-11-24

### 修复（out_of_scope 拒绝风暴：发现被自锁 + 升级提示把 agent 往死路上指）

- **实测现象**：task 9（ali-1124）范围只有一条精确子域行 `www.lingguang.com`
  （agent 只能自行沉淀精确行），worker 每碰一个兄弟子域（cognihome/render/
  agi-static/aabbcc…）就 out_of_scope 拒一次：深夜 03:49-03:56 每分钟一次
  「连续拒绝 3 次，需人工处理」，人工逐域复核苦不堪言。
- **自锁根因**：平台本来就「发现即纳范围」（insert_assets 自动加精确子域行），
  但发现这些主机需要的解析探测被 scope 拒在前面——不许探测就登记不了、
  不登记就不许探测。
- **修复①（同根域 DNS 发现切面）**：纯解析形命令（dig/nslookup/whois/host
  单命令，复用快路径的严格形态白名单）且被查名字与任务范围中某条域名行同
  eTLD+1（db.RootDomain/publicsuffix，多段后缀不会算错）⇒ 解析随已授权根域
  放行，审计理由如实可回放（`pass(sibling_domain_dns(<基域>))`）。**触碰**
  （curl/nmap 等）兄弟子域仍按越界拒绝——发现随根域放行，触碰必须有人工的
  根域授权行。复合命令进不了解析形态判定，切面不放宽；资产库未装配/查询失败
  一律 fail-closed。
- **修复②（升级提示如实 + 一次沉淀指令）**：旧提示让 agent 「用 add_task_scope
  走扩权」——但 root_domain/company 刻意不在 agent 扩权白名单里
  （db/task_scope_expansion.go：整域授权=边界整体推翻，必须人工确认），agent
  照做只会被拒，然后逐子域硬撞。静态文案改写为如实分面（解析已放行/触碰要
  人工根域授权/agent 只能加精确行）；被拒目标与已授权域同根域时升级播报改为
  动态生成，点名那条「一次沉淀」的人工授权：登记 `root_domain=<基域>` 一次，
  全子域放行、不再逐域询问。
- **边界不动**：root_domain/company 仍不在 agent 扩权白名单（v6 用户拍板「授权
  边界变更由人工拍板」）；闸门五道检查顺序不变；合并闸三态不变（本次切面走
  旧链 scopePassOverride 既有机制，gateDeps 方法集零变更）。
- **操作沉淀（同日人工授权）**：task 9 登记人工行 `root_domain=lingguang.com`
  （source=manual，理由引用用户 2026-11-24 裁决「除删除动作和行为静止外均可」），
  原.www 精确行保留。
- **测试**：TestScopeSiblingBase（同根域判定/多段后缀/跨任务隔离）、
  TestDNSResolveOnly（解析形白名单 15 形态）、TestSiblingDNSBaseFailClosed、
  TestSiblingDNSDiscoveryAllowedForResolve（真实路径：dig 兄弟子域 → approve
  + 审计回放 + 凭证消费）、TestSiblingDNSTouchStillDenied（curl 兄弟子域照拒）、
  TestOutOfScopeHintNamesRootDomainSettle / TestOutOfScopeHintFallbacks。

## [0.15.55] - 2026-11-23

### 完善（0.15.52-54 修复面的收尾补齐——无新语义，只补一致性与可观察性）

- **终态回写补漏**：任务超时收尾（exhausted）、任务已完成取消（stopped）、
  planner 终止（stopped）三条终态路径此前没挂格点回写钩子——偏锋假设走这几条
  路会留下滞留 dispatched 的格点。全部补上（与主收尾路径同一钩子）。
- **edge_conclude 机械门与调度器 doctrine 守卫同口径**：全终态但假设验证还在
  飞 ⇒ 第四门不成立（此前只查格点终态，封顶 falsity 会被当判据成立）。
- **软截止收尾播报（0.15.55）**：完成判据是【意图清零】（不是「只剩 running」）
  ——清零时播报一次「软截止收尾完成：可走 task_conclude 或解除软截止」，
  监督者/人据此收口；软截止每次翻转重新计。
- **封格文案对齐实际行为**：edge_dispatch 描述写明「同格点同时只允许一条假设
  在飞；2 次尝试落定仍未确证后第 3 次下发时封格（disproven）」（旧文案写的是
  「满 2 次强制封格（cell_exhausted）」，与实现不符）。
- **测试**：TestDrainCompleteNotifiedOnce（播报一次/在途不播/翻转重计）、
  TestEdgeConcludeRefusesWhileInFlight（在飞拒收/落定后可收）。

## [0.15.54] - 2026-11-23

### 修复（任务整面静止：软截止语义把已派发工作冻死 + 偏锋封顶风暴两处）

- **实测现象**：监督者 18:06-07 以「交付物已齐」对 task 5/6 下软截止后，任务
  整面静止——40 条**已派发**意图无人可领、永不再动（task 5 尚有 open goal
  766「最终漏洞清单报告」、task 6 尚有 open goal 807「四主机测试矩阵」）。
- **根因（语义缺陷）**：runWorkerStep 里 `Drained ⇒ return false` 把
  「停派新意图」实际做成了「全停」——软截止把**已下发未认领**的工作也永久
  冻结，「在飞工作自然收尾」只覆盖认领前就 running 的意图。监督者评估时只数
  running（「仅剩 2 条在飞」），把 open 积压当不存在，于是判定可以收口。
- **修复（语义对齐工具描述）**：drain 只停【规划面】（新意图不再产生），
  已派发未认领的工作照旧被认领自然收尾——收尾完成的判据是意图清零而不是
  「只剩 running」。**完全冻结（含已派发工作）是暂停（pause）的语义**——
  task_drain 工具描述同步写明两个开关的分工，并提醒监督者判定收口前先看
  open 意图数（open>0 = 已派发工作没做完）。
- **偏锋连带两处**（0.15.53 换装实测：task 6 一轮 20 格被封顶风暴全灭 falsity
  后收出 doctrine_complete）：
  - edge_dispatch 增**在飞门**：同格点同时只允许一条假设在飞，复投必须等上一条
    验证落定——此前控制器对着还在飞的格点连投，尝试数被瞬间烧到封顶、格点
    全被判 disproven；
  - decideEdgeWake 增 **doctrine 在飞守卫**：全终态但假设验证还在飞时不机械收
    （等回写落定），封顶 falsity 不再被当判据成立。
- **回执如实**：去重分支不再谎称「本格点改判 disproven」（实际根本没改），
  如实记「跳过重复下发，格点保持原状」。
- **测试**：TestDrainedTaskStillClaimsDispatchedWork（软截止认领语义钉死）、
  TestEdgeDispatchInFlightGate（在飞门：拦截/不计尝试/落定后可复投）、
  TestDecideEdgeWake 扩 doctrine 在飞分支。

## [0.15.53] - 2026-11-23

### 修复（偏锋判据闭环：格点回写缺失 + 停滞熔断两处误杀面）

- **0.15.52 换装实测暴露的三个深层缺口**（建格修好后运行照旧全灭在
  criterion_stalled——re-open 的 task 6 运行 5 分钟内被收）：
  1. **格点回写路径不存在**：edge_dispatch 的「判据进度随其终态更新」只是一句
     回执文案——`UpdateEdgeCellCriterion` 零调用者、意图终态不回写格点，
     格点永远停在 dispatched、判据哈希建完格就冻结；
  2. **停滞哈希只吃 criterion_state**：下发记账（attempts/state 变化）不动哈希
     ⇒ 建完格必然冻结 ⇒ 连续 4 个对账周期后熔断**必然**误杀；
  3. **排队被当停滞**：edge_exec 是串行单飞（serial+by_task+1）——re-open 的
     运行排在别的任务长轮后面时哈希当然不动，4 分钟后被 criterion_stalled 收掉。
- **修复**：
  - 意图终态回写格点（worker 收尾与治理停止两个口都挂）：done + 预测兑现
    （yields finding 命中 predicted_vulnclass，与 settleGap 同源判定）⇒
    confirmed；done 未兑现 ⇒ disproven（负空间结论也是结论）；exhausted/
    stopped/blocked ⇒ 回 open 待复投（尝试封顶照旧由 dispatch 记账把关）；
    无论哪个分支都写 criterion_state；
  - 停滞哈希拓宽为 判据状态+格点状态+尝试数（下发与终态都算推进）；
  - decideEdgeWake 增两道守卫：**控制轮在飞/在排队** ⇒ 本 tick 不算对账周期
    （计数不动、不叠唤醒）；**假设在飞**（格点意图 open/running/paused）⇒
    熔断不成立、照常唤醒（可继续对其他格点下发）。新增
    artex_edge_controller_skips_total 指标；
  - edge_dispatch 补登记 intent_predictions（F1 登记面此前只接了 add_intent
    路径——偏锋假设不登记 ⇒ 兑现判定/矛盾对照拿不到预测）；回写钩子对存量
    未登记假设回退读载荷 predicted_vulnclass。
- **测试**：TestSettleEdgeCellOnIntentDone（回写四口径）、
  TestEdgeCellHashesMovesOnBookkeeping（哈希拓宽）、TestDecideEdgeWake 扩
  排队/在飞分支、dispatch 预测登记断言。

## [0.15.52] - 2026-11-23

### 修复（偏锋探索三处缺口：建格死锁 + 延迟形态闸门漏判 + 偏锋工具家族空目标误拒）

- **缺口一（建格死锁，核心）**：`StartEdgeExploration` 的启动词让 edge_exec
  「读 edge_coverage 建立格点坐标系」，但它的工具箱里**没有任何建格入口**
  ——`edge_activate`（带 cells 参数）只绑 mainagent，`edge_dispatch` 要求格点
  已存在（不存在直接报「先用 edge_activate 初始化坐标系」）。实测 3 条偏锋
  运行（task 3/4/6）全部卡死在 cells_total=0：判据哈希恒定 ⇒ 连续 4 个对账
  周期无推进 ⇒ `criterion_stalled` 熔断收场，edge_exec 不再唤醒；worker 判据
  轨迹里留下「尝试通过现有工具初始化或获取初始化入口」的失败记录。
- **修复**：
  - 新工具 `edge_init_cells`（绑定 edge_exec/auto/mainagent）：幂等建格/扩格，
    只建格不下发、不动模式不动运行行；
  - `edge_dispatch` 对不存在的格点**自动建格**（坐标系随下发逐步建立，死锁
    不可能复发；回执如实标注「格点不存在已自动建格」）；
  - 启动词/总控轮唤醒词同步指向 edge_init_cells。
- **缺口二（0.15.50 修了一半）**：任务内表的**规整跳过面**按解包内层名识别，
  但**闸门入参**仍按外层名判 TaskInternalWrite ⇒ 延迟形态（ExecuteExtraTool
  载荷 tool_name=record_cell 等）跳过了规整却在闸门侧落到「无可判定目标」
  fail-closed。生产实锤（sod_decisions）：worker 经 ExecuteExtraTool 调
  edge_consult（params 只有 task_id）被拒 ×2、update_finding_report 靠载荷里
  恰好出现目标主机名（规整器从报告正文里抓出 mcp.mcd.cn 当目标）才侥幸通过。
  - 修复：evaluate 的 TaskInternalWrite 与 enforceGate 同一口径按解包内层名
    判定（deferredInnerName），端到端用例钉住正反两面。
- **缺口三（偏锋工具家族空目标误拒）**：edge_consult/edge_coverage/
  edge_methodologies（读侧）与 edge_init_cells/edge_dispatch/edge_switch
  （格点与假设记账，写目标在本任务格点/意图图）目标是任务/图/账本引用而非
  主机，规整器产不出目标线索 ⇒ 成批误拒。全部入任务内表；`update_finding_report`
  （report_finding 的孪生写侧）同族入表。**边界不变**：edge_conclude/edge_run/
  edge_activate 是生命周期动作（收尾/建任务/切模式）照旧进规整过闸，worker
  不得经延迟转发直收运行、建任务；假设落图仍走 AddIntentInScope 的既有范围
  校验，dispatch 自带溯因门禁/去重/封顶。
- **测试**：TestEdgeInitCellsAndDispatchAutoCreate（建格幂等/自动建格/封顶
  记账）、TestDeferredTaskInternalWritePassesGate + TestDeferredHostTargetStillGated
  （延迟形态闸门正反面）、task_internal 矩阵扩边（偏锋家族命中、生命周期
  工具不命中）。

## [0.15.51] - 2026-11-23

### 修复（过早收尾连发两起：task_conclude 三门不查 goal 状态——补「open goal 门」）

- **实测两起过早收尾**：
  1. task 2（09:02）：goal 达成自动收尾——goal 5「报告落库」被判 met 但
     reports 表为空（达成误报），全部目标达成 → 自动 conclude；
  2. task 4（09:52）：**监督者 task_conclude 治理收尾**——「三门满足（无
     running/open 意图 + 有产出）」直接 conclude，但 goal 55 仍 **open**、
     规划者正在派新意图（09:52:36 add_intent）；残留 9 条 blocked 意图被
     settle 成 stopped（blocked 不计入三门）。
- **根因（设计缺口）**：ConcludeTask 的三门只查「意图 frontier + 产出」，
  **不查 goal 状态**——人工/规划立的收口条件（open goal）可被 agent 的
  「目标实质达成」判定静默绕过。goal 达成自动路径与 goalless 路径都有
  goal 状态兜底，唯独监督者直收路径没有。
- **修复（门5）**：ConcludeTask 在额度门（门4）后加「open goal 未闭合 ⇒
  不直接收，返回需人工确认」——与既有「三门未满足 ⇒ 需人工确认（可发起
  人工 ask）」同款双签语义。task_conclude 工具描述同步标注（明示 open goal
  是收口条件，不要试图绕过）。
- **方向**：人类立的目标闭合与否由人类裁决（或 prove_goal 显式证明），
  agent 的实质达成判断降级为「建议人工确认」——SoD 口径一致。
- **测试**：TestConcludeTaskRefusesWithOpenGoal（产出门/意图门均满足、
  open goal 在——必须拒到「第5门」；若无门5 用例会直接收尾，正反面都钉住）。
- 注：goal 达成误报（报告落库 goal 无报告却判 met）为独立问题记档，后续修
  达成判定的落库核实。

## [0.15.50] - 2026-11-23

### 修复（实战实锤：平台内部工具被「无可判定目标」成批误拒——任务内表解包识别 + 空目标面补齐）

- **问题（08:49 escalation 实锤）**：闸门空目标 fail-closed 分支
  （「没有可判定的目标线索 ⇒ 不放行」）误伤**目标不是主机**的平台内部工具：
  - `sleep`（{"seconds":45} 被拒——等待工具天然无目标）；
  - `ExecuteExtraTool` 内层 `record_cell` / `graph_overview` / `list_facts`
    （业务图/事实账本操作，task 3 建模线连拒 3 次触发 escalation，gap 详情
    实锤「本次动作没有可判定的目标线索（工具=ExecuteExtraTool）」）；
  - `TaskOutput`（{"task_id":"task_8"}——任务引用被当网络主机做 scope 裁决，
    host 口径对任务引用是范畴错误；连合法任务引用也会被拒）。
  根因两条：① 任务内表按**外层名**查——延迟工具外层是 ExecuteExtraTool，
  内层名没解包；② 内建等待/跨任务读与事实图面工具不在任务内表，空目标
  fail-closed 对「目标=任务/图/账本引用」的工具是误拒。
- **修复**：
  - 任务内识别改按 **deferredInnerName 解包后的内层名**（与桌面识别、
    intercept.UnwrapDeferredName 同一语义；外层 ExecuteExtraTool 本身不命中
    ——内层可能是网络工具，解包后逐名判定）；
  - 任务内表补齐平台内部数据面工具（目标是任务/图/账本引用，不触网）：
    sleep / TaskOutput / add_task_hint / add_intent / record_cell /
    graph_overview / list_facts / bizmodeler 全家 12 名（read/upsert/record/
    propose/adjudicate/list/get）/ bind_finding_traffic / arbiter_resolve；
  - **边界不变**：扩权工具（insert_assets/add_task_scope/add_company_scope/
    create_mcp/create_custom_tool/create_skill）与以主机为真实目标的工具
    （delete_assets_by_host/Bash/WebFetch/…）照旧进规整过 scope。
  - 放行语义沿 B2：跳过规整、scope 按构造通过（pass(task_internal_write)），
    熔断/额度/tier2 检查照跑，审计带 bypassed_normalizer 标记。
- **测试**：TestTaskInternalWriteUnwrapsDeferred（解包识别/内建命中/内层网络
  工具不命中/扩权工具不命中 15 例）+ 既有 gate 级 bypass 钉子回归。
- 验证：go vet + sod/sodpolicy 全包绿。

## [0.15.49] - 2026-11-22

### 修复（0.15.48 判别粒度实测修正——混合复合命令本地子段被连坐 + 输出件扩展名漏白名单）

- **实测甄别（0.15.48 上线后 5 条 out_of_scope 逐条溯源）**：3 条是真越界
  该拒（worker 试图访问 tcadp.cloud.tencent.com 静态 CDN——任务 scope 只授权
  了 adp.tencentcloud.com，worker 自己的笔记确认「扩范围被 SoD 以 out_of_scope
  拒绝」；另 1 条是 TaskStop 指向幻觉 task id）；**2 条是 v1 判别器的粒度缺口**：
  - #1304/#1318：混合复合命令 `cd && curl … && cat bGet.body`——v1 的
    networkAPIRe 扫**整载荷**，curl 子段把整条命令染成「有出网能力」⇒ cat
    子段的文件形目标（bGet.body/hGet.headers）被连坐拒绝；
  - 输出件扩展名 .body/.headers 不在 v1 白名单 ⇒ 裸名判成主机形。
- **v2 判别器（按子意图自身三分类）**：
  - **网络工具**（curl/wget/ssh/dig/…）：目标一律按网络目标照常裁决（hint
    是主机不是文件）；
  - **解释器/间接执行器**（python/bash/find/xargs/awk/sed/…）：出网能力取决
    于代码体——内联代码（-c / stdin / heredoc）按**载荷**过 networkAPIRe
    （代码体可见：正则字面量本地分析照常放行、urlopen/curl 代码照常裁决）；
    **外部脚本执行**（`bash probe.sh`）代码体不可见 ⇒ 保守照常裁决（工作区
    脚本走 B5 绝对路径豁免是设计通道；路径less 形态从严是 B5 同款取舍）；
  - **纯本地工具**（cat/grep/jq/…）：无法出网 ⇒ 文件形目标按构造通过
    （混合复合命令的本地子段不再被别的子段连坐）。
- **白名单补**：body/headers/hdr/dump/trace/snap/pid/lock/part/sig/asc/sha/
  sum/meta/idx/plist/strings/session（curl -o/-D 输出件实测形态）。
- **/dev/tcp、/dev/udp 网络端点守卫**：形态是路径实际出网（`>/dev/tcp/evil/80`），
  路径形豁免明确排除（该口子在 0.15.5 的 /path 豁免里就存在，顺手补上）。
- **测试**：判定分界补 3 实测形态（bGet.body/hGet.headers//dev/tcp）；
  TestScopePassFileTarget 重写为 9 案例子意图粒度矩阵（混合复合放行/脚本
  写入放行/内联本地分析放行/内联出网拒绝/heredoc 出网拒绝/外部脚本拒绝/
  网络工具拒绝/主机形拒绝/快路径拒绝）。
- 验证：go vet + sod/sodpolicy 全包绿；server 包全量回归 **686s 全绿**
  （0.15.48 首跑的 FAIL 系漏配 ARTEX_PG_DSN 测试库的环境性失败，非代码问题，
  已按测试纪律带库复跑实证）。

## [0.15.48] - 2026-11-22

### 修复（真实渗透实战实锤：本地分析命令被 out_of_scope 成批误拒——文件形 target_hint 构造放行）

- **问题（实战首日 4 分钟内 4 起 escalation 实锤）**：worker 的本地分析命令
  （python 读下载的 HTML/JSON 做正则扫描）被 SoD 闸门以 out_of_scope 连拒：
  - 被拒载荷全部是**纯本地操作**（`python3 - <<'EOF'` 扫描 page*.html、
    `cd /workdir && python3 -c` 解析 catalog.html）；
  - 根因链：脚本里的正则字面量含 URL 形字符串（`inet=re.compile(r'https?://…')`）
    → networkIndicatorRe 误判「命令在碰网络」→ B5 本地任务目录豁免被反向
    守卫杀掉 + 复合动作 compound_local_segment 被跳过 → 规整器塞进
    target_hint 的本地文件名（hostmap.py / page__.html 逗号列表）被当主机名
    对 scope 裁决 → 文件名不在授权范围 → 拒绝。
- **修复（文件形目标构造放行 + 出网能力判别）**：新增
  `scopePassFileTarget`（链侧单意图/复合子段共用）——三重前置齐备才放行：
  ① 非快路径（快路径提取的 hint 一定是主机，照常裁决）；② 载荷**无任何
  出网能力**（新增 networkAPIRe：只认 urllib/requests/curl/socket 等出网
  API/工具调用形态——URL 形字符串只是数据/正则）；③ 目标是**文件形**
  （新增 localFileTargetHint：路径形/裸名扩展名白名单/逗号列表逐段判定，
  混入主机段保守从严）。放行理由 `local_file_target` 如实进审计。
- **方向安全**：主机形目标（console.bce.baidu.com 这类真正越界的）照常
  scope 裁决拒绝；有出网能力的载荷（urllib/curl）不享受豁免；形态不明
  （无扩展名/白名单外扩展名）保守按网络目标裁决。networkAPIRe 误中只是
  少一条豁免（与 networkIndicatorRe 同款取舍）。
- **测试**：TestLocalFileTargetHint（实测误拒形态 vs 真实主机形态 22 例）、
  TestNetworkAPIRe（正则字面量 vs 出网 API 13 例）、TestScopePassFileTarget
  （三重前置逐条钉）。
- 注：既有已知口子留档——复合动作载荷无网络指示符时全段按构造通过
  （compound_local_segment），两段裸域名（evil.com 形态）不出 networkIndicatorRe，
  该路径的 scope 旁路是 0.15.5 以来的既有形态，本次未动（改它会把
  `grep 'evil.com' file` 这类本地检索误拒回来）；follow-up 另议。

## [0.15.47] - 2026-11-22

### 修复（真实渗透开战前体检实锤：凭据熔断全工具记账会误伤主通道——收窄为注册制）

- **问题（0.15.44 #6 的范围缺陷）**：凭据熔断在 PostToolUse 对**所有工具**
  无差别记账，匹配模式含裸词 `Unauthorized|Forbidden` 与 `HTTP/1.1 401`。
  真实渗透里**目标返回的 401/403 是核心数据**（鉴权探测正是靠它），同一
  工具（curl/HTTP 请求/浏览器）连续两次 401 形态结果就会触发熔断——该工具
  被确定性闸拦 30 分钟，渗透主通道被平台自己打残。
- **修复（注册制收窄）**：新增 settings 键 `sod_credential_tools`（逗号分隔
  精确工具名）。只有**注册进凭据面的工具**才进熔断记账与 PreToolUse ⑤.0
  拦截；未注册的工具无论撞多少次 401 都不可达状态机（`credCircuit` 连条目
  都不落）。**默认空 = 熔断完全休眠**。注册表 60s TTL 缓存重读，运行期改键
  无需重启。
- **为什么默认休眠是对的**：当前代理面没有任何承载平台凭据的工具（Amber 走
  服务端内部客户端不经工具链；NVD/GitHub 令牌尚无消费者；ScopeSentry 未配
  URL 未发现工具）。注册表是给未来接入的凭据集成（reartex-cve-mcp、
  ScopeSentry 等）预留的显式入口——出现即注册，语义精准。
- **配套**：`credToolRegistered`（TTL 缓存读 settings）+ PreToolUse ⑤.0
  条件加注册检查（注册表移除的工具即使熔断态残留也不再拦）+ credAuthFailRe
  注释更新（宽松匹配的误伤面已由注册表挡住）。
- **测试**：存量 4 例改注册表注入；新增
  `TestCredentialCircuitDormantForUnregisteredTools`（未注册工具撞 3 次 401
  不熔断、状态表零条目、注册后同链照常工作的回归钉）。
- 验证：`go vet` + `go test ./internal/sod/ ./internal/sodpolicy/` 全绿。

## [0.15.46] - 2026-11-22

### 修复（v0.15.44 release 描述陈旧——RELEASE_NOTES.md 机制性欠账）

- **现场**：GitHub 上 v0.15.44 release 的描述还是 v0.15.11 的文案。根因：
  release.yml 用 `body_path: RELEASE_NOTES.md` 发布描述，而该文件在
  0.15.12~0.15.44 共 33 个版本期间无人更新——第一个 tag 就把 11 代前的
  旧文案原样发布（发布内容本身不受影响，产物/资产全部正确）。
- **live 修复**：v0.15.44 release 描述已用 `gh release edit` 重写为自
  v0.15.11 以来的累积发布摘要（0.15.12 → 0.15.44 全跨度，按主题组织：
  任务干预/外部通道/RSI 自进化/尖峰探索/发现报告/SoD 安全/交付质量/
  兼容升级）。
- **机制修复**：仓库 RELEASE_NOTES.md 重写为同款累积摘要，并且标题**不钉
  版本号**（release 页 tag 自带版本显示；钉版本正是这次陈旧的根源）；
  文件头加维护纪律注释（描述「下一个 tag」的内容，发版有新内容即同步）。
  0.15.45 的 lint 门修复一并入「交付质量」节——下一个 tag 发布时读到的
  即当前跨度。

## [0.15.45] - 2026-11-22

### 修复（0.15.44 首次推送实测：分支 CI lint 门 2 处 error 清零）

- 推送 0.15.44 后分支 ci.yml 的 frontend lint 门（biome 棘轮，只查相对基线
  改动文件）红了 2 处 error——文件因 0.15.44 改动进入棘轮集合，连带暴露：
  - **api.ts organizeImports**（error 级）：0.15.44 新增的
    `TaskEscalationProposal` import 插错位置（排在 Asset 后），违反导入名
    字母序。挪到 TaskConstraint 与 TaskGoal 之间（净位置修正，零行为变化）。
  - **reports 页 noStaticElementInteractions**（error 级）：行内操作区 span
    的 biome-ignore 抑制注释用的是 `//` 行注释形态且写在 JSX 开标签属性区
    内——biome 2.5.1 对该形态判「抑制无效果」，抑制形同虚设。改为与
    system/mcp 页同款且实测有效的形态：`{/* … */}` JSX 注释 + 紧贴元素
    之前的子节点位。被抑制的元素与行为零变化。
- **本机复现门**：CI 的 `npx biome check $changed` 依赖 bash 分词；本机
  zsh 不分词导致多路径被并成单参数、出现「全部路径被忽略」的假象——
  本地复现须走 xargs 等价形态。修正后 19 文件集 0 error（38 warnings/
  171 infos 为棘轮容忍的既有旧账）。
- 附带发现（未改）：web/node_modules 里 biome 残留 2.6.8（早前 pnpm 漂移
  沉淀），与权威 npm lock 的 2.5.1 不符——npx @biomejs/biome@2.5.1 钉版
  验证不受影响；node_modules 属未跟踪产物，不推不修。

## [0.15.44] - 2026-11-22

### 功能（#12 single_loop 硬锁 + 升级提议卡 + #6 两小项——vNext 用户裁决三项落地）

- **① #12 会话单循环硬锁（按推荐落法：会话粒度 opt-in，不砍既有能力）**：
  conversations.single_loop 列（幂等迁移）+ POST /api/conversations/{id}/
  single-loop 开关 + 对话页会话菜单项。开 = ToolResolve 确定性剥 spawn_task
  （工具面物理不可见 = 比提示词纪律强一档；30s 缓存）；既有会话（recon 派
  任务等设计能力）零影响，锁是逐会话开关。
- **② #12 propose_task_escalation 升级提议卡（LLM 只提议，人工批准才建）**：
  task_escalation_proposals 表（pending→approved/rejected，CAS 防重复裁决）；
  工具只写提议**不执行**（绑 auto/mainagent，一次性补绑迁移）——mainagent
  此前无 spawn 面，提议卡正是它与被锁会话的确定性出口；批准端点用提议
  payload 重放 spawnTaskCore（与 spawn_task 工具共用同一套校验/建任务/绑
  会话/launch——零平行实现）；任务页「升级提议」卡组（pending 时渲染，
  批准建任务/驳回）。SoD 口径全程成立：提议不建任务，批准动作是人工。
- **③ #6 凭据熔断（C-T1 真增量）**：SoD 链 PostToolUse 记凭据面——同一工具
  连续 2 次 401/403 形态响应（宽松正则：HTTP 状态行/SDK 错误串/鉴权失败字样）
  → 开路 30 分钟（进程内存态，重启清零=重新探测）。PreToolUse ⑤.0 位 deny
  （reason=credential_circuit_open，落账本；正常裁决 deny——有意不进
  vetoSystemFailureReasons 链级故障集）；非命中响应清零（一次正常读数说明
  凭据可能已恢复）；执行层错误归工具健康熔断不进凭据面。守卫测试禁词
  "Credential" 精确为 "SysProcAttr.Credential"（裁决不依赖进程特权的意图
  不变，行为腿测试不动——裸词误伤 HTTP 凭据面语境）。
- **④ #6 thinking_fallback（C-T1 真增量）**：llmpool Complete/Stream 双挂点
  ——400 且请求带 thinking/reasoning 参数 → 剥参本成员重放一次（安全窗口内
  零输出已发；后续 failover 成员同样用剥参请求）；无参可剥 → 400 原样上抛
  （不重放不熔断，与既有 400 语义一致）。回拨依赖裁决：参数是宿主加的，
  网关不支持时剥离重放是宿主权责（norma 零改动，不碰 patch 链）。
- 验证：llmpool 3 新测试 + sod 4 状态机测试 + server 3 全链测试（锁开关/
  物理剥工具/提议驳回+批准建任务）+ wiring 全绿；tsc/biome(2.5.1)/全包回归
  见提交。#3 基线录制按用户指令【不做】——等手动启动。

## [0.15.43] - 2026-11-21

### 功能（vNext #8 收尾：导出 9 格式齐 + 报告中心标签化 + 多任务合并 + 脱敏诊断包）

- **① DOC / 打印即 PDF（导出补全收尾，现 9 格式齐）**：
  - `format=doc`：Word 兼容单文件（ProgId=Word.Document——HTML 存 .doc 的
    业界惯用形态，Word/WPS 直接打开编辑）；二进制 OLE 老格式手写不现实，
    本形态为标准做法；
  - `format=pdf`：打印就绪视图（@page A4 分页 + 自动唤起打印对话框，浏览器
    「另存为 PDF」）——纯标准库纪律裁决留档：中文 PDF 需嵌 CJK 字体
    （≥10MB 二进制不入库），浏览器引擎承担排版导出。
- **② 报告中心标签化（C-T2 `reartex:report`）**：reports 表加 tags 列
  （幂等迁移）；任务生成初稿自动打系统标签 `reartex:report`；
  **forward-only**：人工更新不可移除系统标签（db 层强制并入）；列表
  `?tag=` 精确过滤（string_to_array ANY，参数化）；前端标签徽章列 +
  过滤框 + 编辑器标签输入。
- **③ 多任务合并导出**：FindingFilter.TaskID 支持逗号多值（向后兼容单值；
  `__unassigned__` 哨兵可混用；全非法 = 不筛选保持原语义）；发现页任务
  筛选改多选面板（合并导出直接挑多任务）。
- **④ 脱敏诊断包**：GET /api/diagnostics/bundle → 排障 zip（manifest/
  config-safe 白名单键/tasks 摘要/SoD 裁决摘要/最近活动摘要）。所有文本过
  sensitivePathRes 脱敏；**打码文本不回显命中片段**（片段即敏感特征，
  本条在测试中实钉：初版回显 "id_rsa" 被测试抓住即改）；payload/detail
  blob/凭据位永不入包。
- 验证：report 渲染器 2 新测试 + db 报告/Finding 回归 + 诊断包 3 测试
  （含脱敏泄漏钉子）+ wiring 全绿；tsc/biome(2.5.1)/前端检查过。

## [0.15.42] - 2026-11-21

### 修复（远程交接第三份实锤：「软截止卡死」三层修复——drain 不再是无解陷阱）

- **① 核查结论（0.15.41 现场复核）**：三层根因**全部仍在**——drain 双闸
  （planner 静默返回 + worker 停派）是设计语义；但解除面为零：全仓
  `engine.Drain` 唯一调用方是 `task_drain` 工具（只绑 auto），监督环
  fireSupervision/fireSupervisionEvent 对 drained 任务直接 return（永久
  失联），drained 又是进程内存态——一旦 drain 除重启外无解；批量重开
  blocked 意图在 drain 中是静默无效操作。本版按交接方案 A1+A2+A3+B 全量修复。
- **② A1 监督环降频审查（打破永久失联）**：fireSupervision 对软截止任务从
  「永久跳过」改「10 分钟一轮只读评估」（drainedReviewDue 水位）——快照
  明确标注任务处于软截止，auto 判断诱因已消失即 task_drain(false) 解除；
  收口理由仍成立则零动作维持（保留「不打扰」本意）。事件轮保持不打扰
  （headline 多为 cap 命中等收口相关，解除线索由轮询环兜底）。
- **③ A2 人工双入口（确定性解除）**：
  - a. 新端点 `POST /api/tasks/{id}/drain` body `{"drain":bool,"reason"}`——
    终态任务 drain=true 拒 409；解除幂等；动作走 govRecord 审计（人工留痕）；
  - b. resume 顺带清 drain：用户要它跑 = 清除停派（旧版 resume 不清 drain，
    任务显示已恢复但双闸仍在——误导源之一；幂等，未 drain 零副作用）。
- **④ A3 软截止可见化**：任务列表/详情 DTO 加 `drained` 字段；列表页
  「软截止中」琥珀徽章；详情页徽章 + 一键「恢复派发」按钮（调 a 端点）。
  旧版软截止对用户完全不可见，只表现为「卡住」。
- **⑤ B1 重开陷阱修复**：软截止中的任务 POST rerun-blocked → 409 明确
  报「先恢复派发」（旧版静默接受，重开的意图晾在 open 无人认领——现场
  13:40:26 批量重开 7 条无效的根因）。
- **⑥ B2 drain 决策可追溯**：task_drain(true) 落审计/活动时并上「诱因：
  最近闸门拒绝摘要」（SoD 账本 deny 的 reason_code 计数，前 4 类）——
  现场那句「进入软截止」没写为什么，用户无从下手。
- **⑦ B3 注释留档**：事件轮跳过语义 + 解除线索路径注明，防后人再踩。
- 验证：新回归 5 条全绿（端点 on/off/400/404、重开 409→解除放行、
  resume 清 drain 全链、水位降频、诱因摘要计数、DTO 字段）+ 路由接线；
  Go 全包回归绿（agent/db/evidence/guard/intercept…+ server 598s）；
  tsc/biome(2.5.1 权威)/build:static 过；e2e 47 页全 200（health=0.15.42）。
- 注：交接文档 D 表提到的 engine_timeout settle-drain 在本仓现行版本无
  对应调用（`.Drain(` 全仓唯一调用方为治理工具），正常收尾路径回归不受影响；
  第 3 层诱因（scope 缺子域 → out_of_scope 连拒）属任务运行期配置，代码面
  由 A1（监督环可自主解除）+ 既有 scope 编辑面缓解，无需新代码。

## [0.15.41] - 2026-11-21

### 功能（RSI 可见化：自进化面板）+ 改名 + 集成 bug 修复 + 全量体检

- **① 「自进化（RSI）」面板（新页面 /system/self-evolve）**：批次 E/J 的两个
  外挂组件从黑盒变 UI 可查——Amber 外置永久记忆库（开关/地址/探活/令牌位 +
  正式记忆/隔离区/语义检索三视图，**全部只读**——晋升仍走 curator 观察态与
  人工，J3 前零自动落写）+ Veto agent 测评器（终态任务 reartex_pentest_v1
  回执清单：六字段 + SoD 链有效徽章）+ curator 观察水位 + **从哪里启动**
  指引卡（Amber 启动命令/环境变量、veto-eval 评分 CLI、J3 推迟裁决留档）。
  后端 6 端点（/api/self-evolve/status|amber/items|quarantine|search|
  veto/exports|exports/{taskID}），路由接线测试钉住。
- **② 更名：Peak Search → 尖峰探索**（用户命名）：导航/页面标题/错误文案/
  注释全改；协议标识有意保持稳定（API 路径 /api/peak-search、UI 守卫头
  X-Peak-Search-UI、包名）——显示名与协议解耦，改名零风险。
- **③ Amber 写入通道修复（真 bug，体检实锤）**：amberWrite 此前走 REST
  /api/memory/write + worker 令牌——但 Amber REST 面 worker 令牌一律 403
  （worker 只许 /mcp，权限矩阵硬保证），API 鉴权开启时经验写入全挂。改走
  /mcp JSON-RPC memory_write（worker 令牌 → agent:worker → agent 通道，
  隔离区语义由 Amber 侧通道绑定强制）；amberSearch 同理改 owner 令牌读。
  契约单测钉死（/mcp + Bearer + memory_write 三断言）。
- **④ amberGet 短读修复**：单次 Body.Read 可能短读静默截断大响应——改
  io.ReadAll + 2MB 上限（curator/代理共用受益）。
- **⑤ 前端 lint 环境漂移排查（体检查明：权威工具链本身是好的）**：本机
  biome 报配置 schema 错——根因是本机 node_modules 被 pnpm 装过（biome
  2.6.8 + 流浪的 web/pnpm-lock.yaml，未跟踪），而仓的权威锁是 npm
  package-lock.json（biome 2.5.1，与提交的 biome.json schema 一致）。
  处置：删除流浪 pnpm-lock.yaml（不进仓）、**回滚** biome.json 迁移试验、
  以权威 2.5.1 复验（改动文件 0 错误 0 警告）。顺带修 2.5.1 也翻出的存量
  问题：尖峰探索 RadioGroup label 无 htmlFor（a11y）+ 三个 pre-existing
  unused import（peak-search 的 api / notify 的 cn / api.ts 的
  BizLayerDiff）。
- **⑥ 发现页导出全格式 500 修复（全量体检实锤的真 bug）**：
  ListFindingsForExport 的 Scan 少了 EvidenceStatus/RootCauseKey 两列
  （G1 给 findingSelectCols 加列时漏了这个 Scan——19 列 vs 17 目标，
  sql: expected 19 destination arguments in Scan, not 17），发现页导出
  （md-single/md-zip/csv/json/txt/sarif/html 全部格式）一律 500。补齐两列 +
  findingSelectCols 全部消费点普化复查（scanFindings/GetFinding/本处，唯此一处）。
- **⑦ e2e 冒烟路由清单补缺**：/function/peak-search 与 /system/self-evolve
  入清单；reartex-cve-mcp 本地构建产物入 .gitignore。
- 验证：go build/vet 全模块过；db/evidence/agent/sidequestion/traffic 等全绿；
  server 包全量 588s 绿（⑤⑥修复后；曾以 540s 预算误判超时——单例最长 7.4s，
  非挂死，纯预算问题）；死锁/通道契约用例 -race 过；tsc/biome/build:static 过；
  e2e 冒烟 47 页全 200；实例级冒烟 health=0.15.41、/api/self-evolve/status 正确
  走鉴权、尖峰探索与自进化页面 200。

## [0.15.40b] - 2026-11-21

### 功能（前后端对齐补缺 + Peak Search 交互增强）

- **① Amber 设置 UI 化（J1 对齐缺口补齐）**：消息推送页新增「外置记忆库（Amber）」
  卡——amber_enabled 开关（默认关）+ amber_base_url（http(s) 校验复用外部通道
  同一函数）+ 令牌已配置布尔位（AMBER_*_TOKEN 是环境变量面，永不回显）。此前
  两键只能 SQL 配——批次 J 的前端缺口补齐。
- **② Peak Search edit_approve 前端**：草稿卡新增「修改后批准」——标题/正文
  可编辑后提交（后端 0.15.38 已有 edit_approve 语义，前端此前只暴露
  approve/reject），人工蒸馏「改完再收」的常见流程不再绕手。
- **③ 对齐复扫（0.15.32-40 新面）**：Peak Search 三端点前端已接 ✓、证据徽标/
  同根因组/三导出格式/授权面 UI ✓；**veto-export GET 前端零消费**（运维面，
  CLI 等价可代，留档不补）；amber 两键即①补齐。
- 验证：tsc/biome/build:static 过。

## [0.15.40] - 2026-11-21

### 修复（远程部署交接文档实锤两 bug：0.15.39 仍存在，双修 + 普化排查）

- **① Engine 自我死锁修复（交接文档根因实锤，0.15.39 复核确认仍在）**：
  recentConclusion 函数体内 Lock stagnationMu，而唯一调用方
  noteIntentWriteBack **持同一把锁**调用——不可重入互斥锁重入 = 永久死锁
  （sync.Mutex 无所有者概念）。触发条件：任一任务连续 6 个零产出意图收尾
  （LLM 配额风暴即凑齐）→ Engine 单例锁 → **全平台 planner/worker 永久阻塞**
  （远程 goroutine 转储 12 组循环卡死 1475 分钟）。修复：函数改纯读 + 更名
  recentConclusionLocked 明示「必须在持锁状态调用」。回归测试 2 条（持锁同
  goroutine 调用带超时断言 / 7 连零产出全链不阻塞）。
- **② 删除链漏接双修（与 coverage_gaps 同款普化）**：
  a) **coverage_gaps**（0.14.3 建表三 FK 全裸露无 ON DELETE）——任务删除时
  gap 行以 RESTRICT 拦死 assets/tasks/explorations 任一方向（SQLSTATE 23503，
  远程任务 #13 删除实锤）。修复：schema 迁移（task_id/asset_id CASCADE、
  targeted_by SET NULL——缺口是任务级账目，锚点删除保留 gap）+
  DeleteTaskCascadePrepared 无条件先删本任务 gap 账（防御性双保险）。
  b) **intent_conflicts**（普化扫描发现的同类漏配）：两条 node 外键裸露——
  DELETE explorations 级联删 nodes 时 RESTRICT 拦死。修复：schema 迁移
  （CASCADE）+ 删除链先清（exploration_id 维度）。
- **③ 普化排查结论**：
  - 锁重入：静态分析全部 Engine 方法（Lock 集合 × 调用闭包），6 个疑点逐一
    核实——5 个为「先解锁再调 / go 异步 / 异锁」安全形态，唯一重入点即①已修；
  - 裸外键：全 schema 扫描 + 运行库实查——finding_traffic_bindings.snapshot_id
    的裸 FK 为**有意设计**（证据快照随 evidence GC 管生命周期，不随 finding
    静默消失，留档）；其余两条（coverage_gaps/intent_conflicts）即②已修。
- 测试：死锁回归 2 条 + db Delete/Gap/Conflict 全绿。

## [0.15.39] - 2026-11-21

### 功能（批次 J1+J2：Amber 记忆库接入（可选组件）+ curator 观察模式）

- **① 裁决更新（0.15.18 的延续）**：用户交付 Amber v0.5.0 包（117/117 全绿），
  「不引入外部记忆库」裁决更新为**可选组件引入**——功能开关默认关
  （amber_enabled settings 键 + amber_base_url），未部署/未开启时零行为变化。
- **② worker 经验写入**（server/amber_client.go + engine 终态挂点）：任务终态
  把结构化经验摘要（终态/确认漏洞类别/复测确认/工具调用/token/SoD 链有效）写
  Amber 隔离区——agent 通道强制隔离（Amber 原生权限矩阵，worker 无晋升权限）；
  正文过 SensitiveHit 敏感面（平台凭据形态拒绝外带）；best-effort 失败静默。
- **③ curator 观察模式**（server/amber_curator.go + scheduler 每小时）：扫隔离区
  → 按规则评估（specialize:* 拟自动晋升 / generalize:* 需同 scope active ≥2 /
  冲突交人工）→ **拟晋升清单写 sched_state——观察态不落写**（不调 review 接口）。
  观察数据积累后人工确认规则行为再切 enforce（J3 期）。
- **④ 部署形态**：amber-memory/ 落仓（v0.5.0）+ venv 部署（127.0.0.1:8002 loopback，
  AMBER_BOOTSTRAP=1 首次授权；AMBER_WORKER_TOKEN/AMBER_OWNER_TOKEN 角色令牌只存
  环境变量）。
- **⑤ J3（Tier-1 RSI 自动闭环）推迟留档**：依赖 E2 真实 baseline（无 LLM+靶场
  未录）+ J2 观察数据（触发条件 ≥15 泛化 trace）；自动改提示词+自动 revert 是
  最高风险面，基线与观察数据齐备后再开。闭环的门禁本体（Veto paired_exact
  gate）已在 E1 就位。

## [0.15.38] - 2026-11-21

### 功能（批次 I：Peak Search——人工专属知识蒸馏器）

- **① peaksearch 包**（4 路径检索故障链）：SourceDocument 归一化契约 +
  Retriever 接口 + Chain 优先级故障切换（失败/空结果自动下探，连续 3 次失败开
  5 分钟熔断窗）。V1 落地路径 1（内置 WebFetch——URL 抓取，egress 治理复用）+
  路径 4（手动离线摄入——粘贴文本零网络兜底）；路径 2（外部 MCP 提供者）/
  路径 3（域名直取器）框架位留档 V2。
- **② 人工专属纪律落地**（C 方案 5.1）：全部端点要求显式 UI 头
  X-Peak-Search-UI: 1（缺失 → 403——agent/侧车常规调用不携带）；不注册任何
  agent 工具定义（零暴露面）；写面硬绑 strategies/legacy 双库。
- **③ 三端点**：POST search（检索→蒸馏草稿→提议入队；LLM 缺席时回落原文摘录
  草稿，审批流不断）/ GET proposals / POST decide（approve/edit_approve/reject，
  批准才原子写 data/knowledge/{repo}/<slug>.md + PG knowledge_entries 索引）。
  单测：无头 403 / 有头通过 / 手动路径全链（提议→批准→文件落库→重复裁决 404）。
- **④ 前端**：Peak Search 页（检索框+目标库选择+手动粘贴+待审提议卡列表，
  批准/丢弃）+ sidebar 新项（Telescope 图标）。
- 测试：peaksearch API 2 条全绿；tsc/biome/build:static 过。

## [0.15.37] - 2026-11-21

### 功能（批次 H：reartex-cve-mcp 独立 CVE 情报服务）

- **① 独立 Go 模块**（reartex-cve-mcp/，module github.com/Autumn-27/reartex-cve-mcp，
  唯一依赖 modernc.org/sqlite）：MCP stdio（4 工具）+ 只读 REST（:8400 默认）双面；
  SQLite WAL + auto_vacuum=incremental + FTS5（traffic 同款纪律）本地缓存库，
  离线毫秒级查询。
- **② 4 工具面**（<30 活跃定义纪律）：cve_lookup（精确查）/ cve_match_component
  （ecosystem+package+version 命中）/ cve_search（FTS5+结构化过滤）/ cve_sync_status
  （水位）。MCP stdio 协议实测通过（initialize/tools/list 冒烟）。
- **③ cve_intel.v1 契约**：含 related_skills[]（CWE→0.15.30 四技能路由，读取时
  派生不落库）、source_provenance 多源标注、affected_ranges/fixed_versions。
- **④ 三源同步**（后台/显式触发，永不阻塞 agent 轮；凭据只存环境变量）：
  CISA KEV 全量 feed（最简）/ NVD API 2.0 增量（lastMod 窗口+分页）/ GHSA
  ecosystem advisories（REST）。单源失败互不影响，水位各自记录。
- **⑤ 三源合并语义（两个真 bug 修复过程中钉死）**：a) nil slice 经 json.Marshal
  是字面 "null"（非 "[]"）——UPSERT 合并 CASE 失效、反序列化打回 nil；
  b) []byte 参数绑定为 BLOB，与 TEXT 的 <> 比较恒真（类型不同）——CASE 全失效。
  修复：nil 归一化 + string 绑定 + CASE 双排除；KEV 位取 OR、字段空值不覆盖
  旧值、来源追加标注。**这个语义（BLOB vs TEXT 类型分离）已进测试钉死**。
- **⑥ 版本命中判定**：GHSA vulnerable_version_range 带操作符（>= 1.0.0, < 1.3.0
  形态）——剥操作符后数字段比较（零依赖简化 semver；预发/元数据边角留档 V2）。
- 测试：store 合并语义/三过滤/组件匹配全绿；go vet 过。

## [0.15.36] - 2026-11-21

### 功能（批次 G2：findings 导出补全——TXT / SARIF 2.1.0 / HTML 三渲染器）

- **① report 包三渲染器**（纯标准库零依赖，docx 同款纪律）：
  - FindingsTXT：分节纯文本（邮件/工单/CLI 审阅形态，分隔线+中文字段名）；
  - FindingsSARIF：OASIS SARIF 2.1.0（GitHub Code Scanning / VS Code 兼容）——
    vulnclass 作 ruleId、severity 映射（critical/high→error、medium→warning、
    low/info→note）、finding id 作 partialFingerprints、automationDetails 按
    时间戳；
  - FindingsHTML：自包含 HTML（内联 CSS 零外链，可直接浏览器打开或打印为
    PDF——PDF 导出即此路径的打印动作）；**证据 HTML 转义钉死**（&lt;script&gt;
    不得逃逸）。
- **② findingsExport 扩三格式**（txt/sarif/html，现 7 格式：md-single/md-zip/
  csv/json/txt/sarif/html）+ 前端导出对话框三选项。
- **③ 裁决留档**：PDF/DOC 独立格式推迟（HTML 打印覆盖 PDF 场景、docx 覆盖
  Word 场景，独立渲染器性价比低）；报告中心标签化（reartex:report 系统标签 +
  Combine 合并）体量为新批次，随后续排期。
- 测试：TXT 分节/SARIF shape+level 映射/HTML 转义 3 条全绿。

## [0.15.35] - 2026-11-21

### 功能（批次 G1：证据逐字交叉验证 + root-cause 同根因标记）

- **① 证据交叉验证**（evidence/store.go VerifyEvidenceAgainstActivity，finding 落库
  同事务 best-effort）：evidence 的证据行（≥8 rune 行，最多 8 行，空白折叠后）
  与活动流最近 500 条 tool_result 输出逐字比对——任一行命中 = verified；
  活动流有输出但无一命中 = **unverified（声称的输出没发生过）**；无可解析行/
  活动流空 = na。findings.evidence_status 列（schema 迁移）+ 发现页徽标
  （绿「证据已核实」/琥珀「证据未核实」）。
- **② root_cause_key 同根因标记**（evidence/finding_dedup.go）：键 =
  hash(vulnclass | host | 归一化路径模式)——query 全剥、数字/UUID/长 token 段
  归一化 {var}（/item/123 与 /item/456 同根因两个位置）；host 参与（跨 host 不
  合并）。**自动标记，不自动合并**（裁决：合并是高后果写，折叠已验证记录破坏
  复核流——标记是提议，合并是人权）；同键组在发现页标「同根因组」徽标。
  evidence 无 URL → 空键不标记（自由文本归因不可靠，宁缺勿错）。
- **③ 数据面**：findingSelectCols 扩两列（GetFinding/scanFindings 列序同步——
  全量 db/evidence 测试抓出两处 Scan 错位即修）；FindingDTO/Finding 类型 +
  前端徽标。
- 测试：evidence 三态判定/抽行纪律/归一化键 3 条 + db/evidence 全量回归绿
  （152s）。

## [0.15.34] - 2026-11-21

### 功能（批次 F：loopguard 重复调用拦截 + 两项裁决留档）

- **① loopguard（agent/loopguard.go + loopguard_runner.go）**：同签名工具调用
  3× 硬拦——签名 = 工具名 + 参数键排序 canonical 的 SHA-256（键序变换不构成
  「换了方法」）；拦截消息给换策略指引；累计拦 4 次后消息升级为收尾指引（连续
  撞墙 = 本轮策略整体失效）。挂 worker PreToolUse 边界、SoD 合并闸外层
  （空转拦截不消耗 SoD 账本行——流程缺陷不是裁决事件；内层 deny 后换参数重试
  属正常路径不计入）。per-run 生命周期（换意图/续跑 = 新守卫）。阈值挂源码
  常量，不进热改面。
- **② 振荡检测（A→B→A→B）不移植（留档）**：振荡对渗透工作流是正常形态
  （两探测交替是常见节奏），误拦率不可控；相同签名 3× 才是纯空转信号。
- **③ 凭据熔断（C-Track1 加固④）不做（留档裁决）**：对渗透 agent，401/403 是
  测试信号不是故障信号——「凭据熔断」语义不成立；防 retry storm/账号锁死由
  recon 纪律 5（遇 429/封锁就退避）+ loopguard 覆盖（反复用同一失效凭据打同一
  host = 同签名重复调用，3× 即拦）。
- **④ thinking_fallback（网关 400 参数剥离重试）推迟（留档裁决）**：norma 是
  vendored 深度面（Tier0 清单），改动风险高；当前 GLM 网关实测兼容 thinking
  参数（0.15.x 注释链），400 参数拒绝是边缘场景；LLM profile 链故障转移已有
  兜底。待真实出现该故障形态再评估。
- 测试：loopguard 6 条（3× 拦/键序不变性/换参不拦/消息升级/透传内层/拦在
  内层前）；agent+server 定向回归绿。

## [0.15.33] - 2026-11-21

### 功能（批次 E2：golden holdout 套件 + baseline 录制工具链）

- **① eval/suite.json**：15 个 golden 任务定义（reartex_pentest_v1，**Veto schema
  校验 15/15 通过**），四维度与 0.15.30 四审计技能同构：污点 ×4（SQLi/SSTI/SSRF/
  路径穿越）/ 越权认证 ×4（IDOR/提权/会话固定/批量赋值）/ 配置暴露 ×4（密钥/CORS/
  调试面/错误泄露）/ 综合复测 ×3（组合链/跨报告去重/定级复核）。expected 全零
  守卫容忍（max_guard_violations=0）+ 按任务类型的复测/工具/token 帽。
- **② eval/collect_baseline.py**：终态导出聚合器——data/veto/<taskID>/responses.json
  → Veto responses + identity；套件 id 对齐（golden 任务按套件 id 命名）、未终态
  任务清单退出、跨构建混录拒绝（基线必须同一 prompt_version）。
- **③ eval/README.md**：完整录制流程（建任务→等终态→聚合→score→归档→回归对照
  的 veto compare 命令全链）+ 现状说明（首次录制待 LLM+授权靶场条件具备）。
- **④ CLI 全链已验证**：空响应探针 score 跑通（ungraded 15/15=schema 合法）；
  E1 端到端（score→compare release_eligible=true）已验。发布前回归门：后续
  一切改动 compare 本基线 n_10=0 才算无回归。

## [0.15.32] - 2026-11-21

### 功能（批次 E1：Veto 评估器集成——exporter + 跨语言签名对齐）

- **① Veto v0.5.0 落仓**（veto-eval/ 侧车目录，feishu-bridge 同款形态）：Python
  包 + venv 部署（127.0.0.1:8001 预留），76/76 测试本机全绿（含 4 条
  reartex_pentest_v1 专项）。Amber v0.5.0 同步落仓（amber-memory/，117/117
  全绿），批次 J 接入时启用。
- **② reartex_pentest_v1 exporter**（server/veto_export.go）：任务终态（四个落点：
  停滞/goalless/超时/常规，挂 emitTaskTokenTotal 旁全覆盖）自动导出
  responses.json + identity.json 到 <dataDir>/veto/<taskID>/——六字段映射：
  confirmed_vuln_classes（findings 确认态去重）/ retest_confirmed_count
  （retest verdict=reproduced）/ guard_violations（SoD 账本 deny 计数）/
  sod_chain_valid（无链级系统故障原因码）/ tool_calls（activity 计数，
  db.CountTaskToolCalls 任务→探索 id 翻译）/ total_tokens。best-effort 不影响
  终态；GET /api/tasks/{id}/veto-export 手动重导出面。
- **③ trusted_record 跨语言签名对齐（逐字节）**：server/trustedrecord.go 自写
  canonical JSON 编码器（复刻 Python json.dumps 语义：ensure_ascii=False 转义集、
  sort_keys 字节序、UseNumber 保大整数字面、拒 NaN）+ HMAC-SHA256（剥离自身
  signature 字段）；向量由 Python 侧生成（veto-eval/testdata/），Go 测试四条
  全部逐字节通过（canonical/digest/signature/verify + 篡改必失败）。
- **④ 端到端验收**：exporter 形态 responses → veto score → compare
  --require-recompute --assume-independent → release_eligible=true /
  recompute_verified=true / non_inferior。E2 基线录制复用同一出口。
- 测试：跨语言向量 4 条 + exporter 六字段映射 + 落盘形态；server 定向回归绿。

## [0.15.31] - 2026-11-21

### 修复（全量扫描轮：前后端对齐复检 + 2 个真问题修复）

- **① guard 短路表凭据正则接归一化匹配（真缺口）**：G-02①-a 平台凭据 deny 此前只匹配原始字节——模型把路径藏在 JSON 转义里（{"command":"\u002fapp/jwt.key"}）时 raw 无字面路径即漏；normalizeToolInput 递归解码（0.15.24）提升为导出 NormalizeToolInput 后，shortCircuit 凭据匹配加归一化主体（raw + 解码文本双匹配，非 JSON 回落纯 raw 零行为差异）。单测补 3 条转义形态（一层/双层/proc 族转义）+ 零误伤面回归保持。
- **② execution-profile PATCH 语义修复（顺带修掉 0.15.17 既有 bug）**：只传 engagement_profile 的请求此前会①无条件 SetExecutionProfile 把「未显式切档」写成「已切默认档」（破坏 ExecutionProfileFor 的 ok=false 语义与 AutoScaler 依据）②无条件播报「执行档位切换」误导活动流。修复：profileChanged（请求真带 difficulty/mode）才写入与播报；只切授权面时算力档零触碰。
- **③ 前后端对齐复检**（383 条后端路由 × 前端全量调用双向匹配）：本批新增面（engagement 授权面 GET/PATCH/createTask、report.docx、exportFindingDocx）全部有映射；report.docx 实例实测路由注册（401=auth 拦截非 404）；前端无后端断链（提取噪声如模板串尾巴已人工核对）。
- **④ 其余面复核留档**：技能部署面（build.sh:191 打包 skills/ → 运行实例 BaseDir/skills ✓）；TooltipProvider 根 layout 全局装配 ✓；engagementElevate 仅 enforce 态接线（shadow 旧链权威不收紧 ✓）；createTask 授权面写入在 launchTask 之前 ✓；docx/finding_gate/iLink 侧车无前后端对齐面。
- 验证：guard/intercept/server 定向回归全绿；go vet 过。

## [0.15.30] - 2026-11-21

### 功能（审计专项技能 4 件——渐进披露的深钻手册落库）

- **① 四个审计技能**（`skills/` 目录，norma skill.LoadDir 解析验证过）：
  - `audit-taint`（污点追踪）：定源→定汇→追链→坐实四步循环；正向/反向双追、
    二级链（stored）两段各自验证；「净化了什么字符集」才是链成立的关键。
  - `audit-auth-access`（认证与访问控制）：垂直越权/水平越权（IDOR）/会话管理/
    认证流程/租户隔离五核查面；**双账号双坐实**标配；非 ID 引用面与批量接口归属过滤。
  - `audit-config-hardening`（配置与暴露面）：密钥泄露/CORS/TLS/调试面/不安全
    默认值/错误泄露六盘点；**组合升权是核心产出**（配置缺陷单看 info、链起来 high）；
    零元项不单独报（与 report_finding 不报清单一致）。
  - `finding-synthesis`（多线索综合与定级）：收全证据→拼链（组合升权对照表）→
    定级三问→落笔；每一环独立证据、缺环如实记 inferred；降级原则优先。
- **② 与既有体系的分工（刻意设计）**：四技能是「候选面铺开后的**专项深钻手册**」，
  通过 Skill 元工具按需渐进加载——不重复 pentest/worker 提示词的常驻心法，不与
  report_finding 的评级基准重复（引用同一口径）；每步都与平台工具配合
  （record_fact/report_finding/traffic_search）。
- 验证：norma skill.LoadDir 四技能全解析通过（repo 外独立程序实测）。

## [0.15.29] - 2026-11-21

### 功能（finding 报告导出 Word——纯标准库 docx 生成器，零依赖）

- **① `server/docx.go` 纯标准库 OOXML 生成器**：docx = ZIP + XML，Markdown 子集
  （标题 H1-3 / 有序无序列表 / 围栏代码块 / `**加粗**` / `` `行内代码` `` / 普通段落）
  → 最小 OOXML 包（Content_Types + rels + document + styles 五 part）。go.mod 零新增；
  子集外语法原样落段落（宁朴素勿丢内容）；全部文本过 XML 转义（报告证据里的
  `< > &` 与非法 XML 字符）；reporter 报告模板（概述/影响/范围/复现/证据/PoC/根因/
  修复）正好全部落在子集内。
- **② `GET /api/exploration/findings/{id}/report.docx`**：GetFinding → Report 为空
  404（没报告没有可导出的东西）→ 渲染下发（Content-Disposition 附件名）。
- **③ 前端**：发现页行内展开的详细报告旁新增「Word」导出按钮——fetch blob + 认证
  头 + Content-Disposition 文件名（与 exportFindings 同款模式，认证部署不 401）。
- 测试：docx 单测 4 条（最小包完整性/子集转换钉死/行内 runs 切分/1200 行大报告）；
  finding_workflow 既有 fixture 补 evidence 适配（finding_gate 空证据预检，语义保真：
  must-fail 用例的失败点保持在 hint 绑定层）；server 全量回归 531s 全绿。

## [0.15.28] - 2026-11-21

### 功能（微信 iLink 任务通道——个人微信扫码接入，长轮询零公网依赖）

- **① `wechat-ilink/` Python 侧车**（参照 email-intake 架构）：扫码登录（二维码
  内容终端打印 + 轮询确认 → bot_token 持久化）→ `getupdates` 长轮询（35s hold，
  游标持久化防重复）→ **allowlist user_id 信任边界**（与飞书 `/start`、邮箱通道
  同层；空集拒绝启动，宁漏勿误）→ 命令面（`/tasks` 列任务 / `/create 描述|目标`
  / `/help` / 裸文本直建任务 ≤300 字）→ 经飞书桥 `/internal/notice` 推创建通知（可选）。
- **② 协议字段集中映射**：基于 2026-03 社区逆向文档（openclaw-weixin 1.0.2），
  字段名全部集中在 `ilink.py` 的 `FIELD` 映射区——实测漂移时只改一处。README
  留档腾讯条款风险（限制/拦截/终止权）与群聊权限限制（本侧车只接 1v1）。
- **③ 零公网依赖**：长轮询拉模式（无回调 URL），与 email-intake 同架构；
  reArtex 主程序零改动（复用 `/api/tasks`）。
- 验证：py_compile 过；协议层冒烟（登录态 roundtrip/UIN 头/updates 解析容错）
  + 命令层冒烟（/help//tasks 降级路径）；通道文档矩阵补 iLink 行。

## [0.15.27] - 2026-11-21

### 功能（Tier B LLM 裁决器——授权面收紧消化进 auditor 裁决岗，零新增 LLM 面）

- **① 收紧命中先经 auditor 裁决**（`internal/sod/mergedgate.go` engagementElevate 升级）：passive_recon/strict_manual 档命中收紧后，先交 **auditor LLM 三态裁决**（复用 deps.Adjudicator——缓存/并发槽/90s 超时/失败审计全复用，**零新增 LLM 面**）：pass=放行（常规出网动作不落人工带宽）、deny=拒（note 带裁决理由）、ask/裁决失败=人工兜底（elevateAsk）。auditor 未装配时整段回落 elevateAsk（宁问人不断链）。
- **② 裁决请求带授权面语境**：Reason 前缀 `engagement:<档>`，auditor（或未来任何 Adjudicator 实现）按任务授权面语境裁决；裁决理由进审计 marker（`engagement_reason`，120 rune 截断）。
- **③ 与铁律的对齐**：auditor 裁决的是「放不放行这次工具调用」（权限面），fail-closed 三态 + ask 人工兜底 + deny 可回放——它不是业务结论裁决（finding 确认/意图仲裁仍是确定性层），SoD 铁律语义不变。
- 测试：engagement 测试扩到 6 条（默认零收紧/passive 出网 ask/strict 非安全 ask/拒答 fail-closed/auditor pass/deny/ask/失败四路）；sod 全量回归绿。

## [0.15.26] - 2026-11-21

### 功能（任务授权面 EngagementPolicy——预授权三档，XIB engagement policy 本仓化）

- **① tasks.engagement_profile 列**（`db/engagement.go` 新文件 + schema.sql 迁移）：三档——`authorized_active`（默认=现状全部行为，零行为变化）/ `passive_recon`（对目标的出网动作先经人工确认）/ `strict_manual`（除平台内安全读写外一律先确认）。CHECK 枚举约束 + `NormalizeEngagementProfile` 写侧拒绝非法值（授权面坏值不静默回落）；读侧查不到/失败一律回落默认档（不收紧不放宽，收紧是显式写入的结果）。与执行档位（difficulty/mode）正交：那管算力，本列管「允许自动做什么」。
- **② 合并闸放行出口按档收紧**（`internal/sod/mergedgate.go` engagementElevate + ChainDeps.EngagementProfileOf 注入）：闸门即将自动放行时按档复核——合同内放行照旧，合同外先敲门；deny/auditor-route/review 路径不受影响（那些本来就不自动放行）。passive_recon 的「出网」判定用分类器结论（远端主机/GET/DNS/目标写形态）——分不出「这次 curl 只读还是写目标」按最保守解释宁可多问；strict_manual 放行 ClassBenign/ClassLocalSafe（平台内安全动作问人纯属骚扰）。
- **③ server 面**：createTask 接收 `engagement_profile`（非法 400）+ execution-profile GET/PATCH 扩展（切档 activity 播报）；wireMergedGateDeps 注入（db 读失败回落默认档）。
- **④ 前端**：建任务对话框授权面下拉（默认授权/被动侦察/全人工）+ 执行档位卡授权面三 chip（Tooltip 带语义说明，非默认档琥珀色边提示）。
- 测试：db 读写回归 + sod 三态收紧 6 条；tsc/biome/build:static 过；server 定向回归绿。

## [0.15.25] - 2026-11-21

### 功能（finding_gate 启发式预检——注定被打回的上报不再占用复核带宽）

- **① report_finding 落库前确定性预检**（`agent/finding_gate.go` 新文件，XIB-Artex finding_gate.go 的本仓化）：三条保守规则——R1 证据空白（TrimSpace 空）；R2 summary 结论自否（「不是(一个)漏洞/误报/不成立/false positive…」）；R3 summary 自认未验证（「未验证/未触发/无 PoC/待验证/存疑…」）。命中即当场拒绝（与 severity 归一化同一形态：当场拒 + 告知改道 `record_fact` 标 inferred），违规上报不再落 pending 占探索图节点与复核带宽。正则/词表挂源码常量（同 shortcircuit 纪律：不进 DB/settings 热改面）。
- **② 判定面刻意收窄（误伤最低裁决）**：R2/R3 只查 **summary**（结论陈述），不查 evidence——证据文本引用上下文（复测记录、目标回复「不存在漏洞」等）出现这些词是正常的；只查 summary 同样能拦住「注入面存在（未验证）」这类自认候选。
- **③ 两条 XIB 原版规则不移植（留档裁决）**：「缺 HTTP 交互标记即拒」——本仓明文支持 TCP 等非 HTTP 漏洞省略流量绑定，会误伤合法 TCP 类上报；「decompiled 字面拦截」——小程序渗透的 wxapkg 反编译产物凭据泄露是合法上报场景（key 泄露本身是洞）。
- **④ 接线钉死**：addFinding 的 Run 在 severity 校验后调用 `CheckFindingGate(a.Summary, a.Evidence)`；既有合约测试 fixture 补 evidence 适配（fixture 是原子性合约测试，输入本就该过预检）。
- 测试：finding_gate 单测 4 条（空证据/自否/合法零误伤含 TCP+反编译场景/接线钉死）；agent 全量回归绿（83s）；db 全量绿。

## [0.15.24] - 2026-11-21

### 修复（自治计划 Phase 0 细项复核轮：1 缺口补齐 + 2 条无缺口留档）

- **① 双层转义绕过补齐（真缺口，intercept 归一化）**：`normalizeToolInput` 此前只做一层 `json.Unmarshal`——传输层 `\u0072m` 能拦，但 payload 自身再带一层转义（`\\u0072m`，解码后仍是字面 `\u0072m`）时词表正则永不命中。补递归解码：字符串字段循环「包引号 Unmarshal」解到不再变化（上限 4 层防深嵌套兜圈），复合字段（args 数组/对象）经 `decodeValue` 递归走遍；解不开的非法形态（字面换行/悬空反斜杠）保持原样参与匹配（fail-safe：少解，raw 原文始终兜底，不产生误放行）。单测 4 条：双重转义命中 / 非法形态保持 / 多层转义解净 / 嵌套结构走遍。
- **② 破坏性路径 deny 清单：无缺口，不移植（留档裁决）**：XIB-Artex 的 `/delete` 等破坏性路径 deny 在本仓架构下由 netguard 拨号面等价闭合——agent 直调平台 API 的面在 `AgentEgress` 拨号时刻被挡（环回默认不放行、`dialControl` 建连时校验），且对目标系统的 DELETE 是渗透测试合法面（REST 越权删除测试），本就不该拦。移植反而引入误拦面。
- **③ dial-stage SSRF：已完备，零改动（复核留档）**：`netguard.dialControl`（Control 回调=最终建连地址时刻校验）+ DNS 重绑定覆盖（校验时解析到公网、连接时解析到内网）+ `ResolveOnceDialContext`（解析一次拿 IP 建连，重连不重解析）——XIB-Artex 的 dial-stage 防护在本仓已全部等价存在。
- 验证：intercept 包测试全绿（含新 4 条）+ go vet 过。

## [0.15.23] - 2026-11-20

### 功能（会话 Todo 悬浮框统一入口——Worker 执行会话可见）

- **① Todo 入口统一移到会话标题栏**：任务详情会话页的 Todo 悬浮框（TodoPopover）此前只挂在非 worker 会话的输入区旁——**Worker 执行会话没有任何 Todo 入口**（worker 分支输入区是三档干预框，没有留位置）。现在挂在会话面板标题栏右侧（token 指标旁），主 Agent / 规划 Planner / Workers / 系统审计全会话类型统一入口；数据源不变（本会话活动流里最近一次 TodoWrite 调用的 detail 全 JSON，capture 管道无条件落库）。
- **② TodoPopover 增强**：无 TodoWrite 记录的会话从「灰死按钮」改为可点开的说明面板（「该会话没有用 TodoWrite 记录过进度」）；打开期间每 5s 轻量轮询（worker 在跑时进度实时可见，关闭即停）；头部加 `完成 N/M` 进度统计。
- **背景澄清（对应「planner 启用的 worker 是不是预设 agent」之问）**：任务引擎的 worker 不是从预设 agent 列表里挑的——`Engine.Run` 按 worker 池数量（settings `workers`，默认 3）起 `work#1..#N` 个同构执行循环，全部用同一份可编辑的通用 worker 提示词（agent_prompts key=worker）+ 各自领到的意图上下文。会话顶部那排 chips（Auto/渗透测试/资产收集等）是聊天会话成员（会话型 agent），其中 reporter（报告撰写）/retester（漏洞复测）/intent_normalizer（意图规整）等特种 agent 按事件（report_finding/复测/意图规整阶段）自动参与任务流程，与 work#N 是两套角色。
- 验证：tsc 过；biome 0 errors；build:static 过；e2e 路由冒烟过。

## [0.15.22] - 2026-11-20

### 修复（全量 debug 轮：-race 实锤数据竞态 + 门禁面错误清零）

- **① 数据竞态修复（-race 实锤，snapshotFor 读 e.resolve vs Set*Resolver 写）**：`restoreTaskRuntimes` 在 `New()` 期间就拉起 planner/worker goroutine 并发读任务路由解析器，而 `SetAgentResolver`/`SetAuthoritativeAgentResolver`（测试换桩/运行时重装）可能在其后发生——裸字段读写是数据竞态（此前靠「启动前接线」的时序约定兜着，-race 全量跑才暴露）。修复：`resolvePtr atomic.Pointer[agentResolver]` 原子注入（fn + authoritative 标志打包快照），「注入晚于读」从时序约定变成结构保证。复跑：server 全量 `-race` 0 DATA RACE。
- **② 门禁面错误清零**：消息推送页等 8 个近期改动文件 biome 错误（useTemplate + 排序类）自动修复——错误级清零，剩 13 条既有 warning 不拦门。
- **③ debug 方法论留档**：server 包「并行多包测试 FAIL」两度为 debug 方法自扰（-race 批次未结束时并发启动同包测试 → 同名派生测试库 DDL 死锁/半套 schema）——干净序列复跑 0 FAIL（560s）确证非代码缺陷。**测试纪律：同库测试不得并发跑两份**。
- **④ norma vendored 树测试留档（不修，Tier0 硬锁目录）**：noa 2 条 + noaadapter 5 条规格测试为 vendored 入库时（b2fe80d）即存在的预存失败，此前从未在本仓全量跑过——与本仓代码零关联（v0.15.11 起 norma 仅 llm/mcp/stdio 4 文件因 0.15.13/19 改动），CI 主模块测试不含 norma，发布门不受影响。已知失败清单：llm TestDoStreamHonorsConfiguredRetries（环境性）、noa TestAdaptiveGrowthTracksTheWindow/TestSuppressionReleaseFitsInsideTheWindow、noaadapter 5 条 bounded 系。
- 验证：go vet 全绿；server 全量 -race 0 竞态 + 常规全绿（547s）；agent/intercept/mcphttp/guard -race 全绿；norma 其余包全绿；前端 tsc/test:mentions/build:static 过；e2e 45 路由冒烟过（0.15.22 实测）。


## [0.15.21] - 2026-11-20

### 功能（消息推送页优化 + 前后端对齐补缺——外部通道 UI 化 + 建任务档位）

- **① 消息推送页新增「外部通道（审批广播）」卡**：飞书桥接收端点 + 共享口令、企微群机器人 webhook——0.15.19/20 仅 SQL 可配的三个 settings 键升级为页面可配（URL 明文回显；口令/webhook 永不回显，只回「已配置」布尔位，掩码语义：留空=保持原值）。后端 settings GET/PUT 补 `feishu_bridge_url`/`feishu_bridge_secret`/`wecom_webhook_url` 三字段（URL 统一 http(s) 校验 + 去尾斜杠，校验复用广播面同一函数）。
- **② 渠道卡快捷测试**：渠道卡直接「发送测试消息」（免进编辑弹窗）；编辑弹窗内测试按钮不变。
- **③ 建任务执行档位补缺（前后端对齐复检发现）**：0.15.17 的 createTask `difficulty`/`mode` 入参此前无表单映射——创建对话框高级设置新增「执行难度」（自动/轻量/标准/困难）与「执行模式」（自动/轻快/均衡/重火力）两档选择（auto=后端按任务书文本判定），创建后仍可在任务总览「执行档位」卡中途切换。
- **④ 前后端全量对齐复检**（382 条后端路由 × 前端全量调用双向匹配）：前端调用零断链（22 条疑点全为提取噪声）；后端未消费 33 条 = 既有裁决（sessions context 族等排期项 + 文档豁免运维面）+ 4 条 0.15.15 死方法删除后悬空的路由（`/agents/X/prompts`、`/agents/X/variables`、`/skills/X/files`、`/conversations/delete/batch`——路由保留供未来接线，与前述 metrics/prices 同类）。0.15.16-20 全部新后端面均有前端映射。
- 验证：tsc/biome/build:static 过；server 定向回归（外部通道/wiring/settings/notify）绿。
- 对齐检查：settings 请求体扩展 3 个可选字段（既有端点）。


## [0.15.20] - 2026-11-20

### 功能（批次 D-②：企业微信审批广播 + 邮箱任务通道——外部通道面补全）

- **① 企微审批广播（出向）**：广播监听器泛化为多目标扇出（`server/external_channels.go`，原 feishu_bridge.go 重构）——ask pending 落库后同一次事件扇出飞书桥（JSON 要素）与企微群机器人（markdown 卡，`wecom_webhook_url` settings 键，4096 字节限制内收紧渲染），各目标独立开关、独立失败互不影响；URL 强制 http(s)；裁决统一回审批页/飞书命令（webhook 无入向面，企微卡只展示）。共享口令仅飞书桥需要（企微 webhook 地址本身即凭据，与 notify/wecom.go 同口径）。
- **② 邮箱任务通道**（`email-intake/` 独立侧车）：IMAP 轮询（标准库 imaplib/email，零额外依赖）→ allowlist（发件人/整域，**宁漏勿误**：空集拒绝启动）→ 直建任务（主题+正文要点 ≤300 字符，来源标注在任务描述）→ 可选经飞书桥 `/internal/notice` 推创建通知。已知 UID 落本地状态文件防重复处理；轮询异常不退出。信任语义与飞书 `/start` 同层（allowlist=可信操作者）。`REARTEX_API_TOKEN` 只存在于侧车环境变量。
- **③ 飞书桥补 /internal/notice**：通用文本通知端点（共享口令鉴权、推 allowlist 私聊）——邮箱通道等侧车共用的通知入口。
- **④ notify 系统零代码面（重要澄清）**：平台 notify 子系统**早已原生支持** feishu/wecom/email/dingtalk/telegram/webhook 渠道（群机器人出向通知）——任务/发现等通知在设置页建渠道即可，本批交互通道与它互补不重叠。
- 文档：docs/飞书通道.md → **docs/外部通道.md**（通道总览矩阵 + 契约差异 + 安全边界 + 改动清单）；两个侧车 README 各自完整运行手册。
- 测试：`TestExternalChannelsListener` 重写（双目标扇出/载荷与口令/非法 scheme 只拒对应目标）+ 桥与 intake Python 编译与逻辑验证（首响锁/allowlist/goal 构建）；敏感扫描干净。
- 对齐检查：无新端点（企微为 settings 键出向；notice 为桥内部面）。可选组件——不配置时平台行为与 0.15.19 逐字节一致。


## [0.15.19] - 2026-11-20

### 功能（批次 D-①：飞书通道——审批广播 + 远程指令，零公网依赖）

外部实现工具包 phase3 channel-bridge 的本仓映射（按真实 REST 契约适配，非移植）：

- **① 出向广播钩子**：`intercept.Interceptor.SetAskListener`——ask pending 行落库后异步触发（best-effort，广播是增值面：桥挂了审批页照常裁决，worker 等待不受影响）。`server/feishu_bridge.go`：广播客户端（settings 键 `feishu_bridge_url`/`feishu_bridge_secret`，空=通道关闭；URL 强制 http(s) scheme 防误配 SSRF；输入预览截 1KB；5s 超时只记日志）。启动接线 `wireFeishuBridge`。
- **② feishu-bridge/ 独立服务**（不进 Go 单体）：飞书 WebSocket 长连接收指令（**零回调 URL**——长连接模式，内网部署无需公网面）+ 回环监听的审批广播端点（X-Bridge-Secret 鉴权，唯一监听面）。能力：`/status` `/start`（含 0.15.17 档位入参）`/pause` `/resume` `/approve` `/deny` `/help`；审批卡推 allowlist 私聊，裁决走文本命令+首响锁（多渠道并发只接受第一响，平台侧 ErrAlreadyDecided 409 双保险）。
- **③ 与工具包原型的契约差异全适配**：resolve→`/api/intercept/pending/{id}/decide`（allowed/denied）；tasks `target`→`description`+`goal`+档位；edge stop→`control/batch`（普通任务）；交互按钮→文本命令（按钮回调需公网面，v1 不开）。
- **④ 安全边界**：凭据（飞书 app_id/secret、平台 JWT、allowlist）只在桥环境变量——不进 reArtex 进程/库/日志；reArtex 侧仅桥 URL+共享口令两个 settings 键（运维 SQL 通道）；allowlist 宁漏勿误（空=拒收所有指令，审批卡仍投递）；决策面零新增权限面（既有 decide 入口，审计链不变）。`feishu-bridge/README.md` 含飞书应用创建步骤（长连接订阅+权限）与运行手册；`docs/飞书通道.md` 记录架构/契约差异/安全边界。
- 测试：`TestFeishuBridgeListener`（未配置静默/广播载荷+口令/非法 scheme 拒绝）；桥 Python 编译过+首响锁逻辑验证；敏感扫描干净；无 Go 行为面变更外的新端点（广播钩子为拦截层内部挂点）。
- 配置说明：桥为可选组件——不部署、不配置 URL 时平台行为与 0.15.18 逐字节一致。


## [0.15.18] - 2026-11-20

### 功能（批次 C：RSI 修改标准门禁——观察模式上线）

外部实现工具包 phase4 治理面的本仓化（标准文档重写 + 门禁脚本适配；**不引入外部记忆库**）：

- **① docs/RSI修改标准.md**：Tier0/1/2 分层路径权限矩阵按当前文件树核定（工具包清单逐文件实测——26 个 Tier0 单文件全部存在；补充 internal/sod、internal/sodpolicy、agent/toolpolicy.go 本仓独有面；patches/ 实存目录照锁）。职责分离四角色表 + 记忆库裁决（**不引入 mcp-memory/pgvector 三表**——传承图+经验权重卡+strategies+memory 已是单一体系，RSI 经验沉淀走平台内链路，避免双记忆库分裂）+ 演化门前置条件留档。
- **② scripts/rsi_guard.py**（适配版，~200 行）：Tier0 硬锁 / Tier1-Tier2 越层 / 膨胀帽（SKILL.md ≤300 行、prompts 相对基线 ≤+15% 增幅）/ 脱敏门（硬编码 IPv4——TEST-NET 除外——与 secret 形状）/ [EVO-YYYYMMDD-NNN] 溯源（可选）。**观察模式默认**（只打印基线报告、退出码恒 0），`--enforce` 切强制（启用 RSI 时的切换开关）；支持 --staged（pre-commit 场景）与 --base <ref>（CI 区间场景）。本文件与标准文档互为 Tier0 自保护清单成员。
- **③ CI 接线**：secret-scan job 追加「RSI 修改标准门禁（观察模式）」步——跑 `--base ${{ github.event.before }}`（首推空基线自动跳过），输出分层基线报告进 CI 日志，不阻断。
- **④ 预置的切换路径**：未来 RSI 实体落地时，pre-commit/CI 把命令加 `--enforce --tier 1` 即完成观察→强制切换，脚本语义零改动。
- 验证：脚本三形态实测（观察=报告+exit 0 且批次 B 的 Tier2 文件被正确分类进基线；强制=exit 1；staged 空集=干净）；CI yaml 解析过。本批无 Go/前端代码变更，全量回归沿用 0.15.17 绿基线。
- 对齐检查：无新端点（治理面为脚本+文档+CI 步）。


## [0.15.17] - 2026-11-20

### 功能（批次 B：任务执行档位——难度×模式矩阵 / 中途切档 / 自动缩放）

外部实现工具包 phase2 的本仓映射（适配而非移植；import 路径与既有请求级覆盖通道融合）：

- **① 档位矩阵**（`agent/execution_profile.go`）：难度 Simple/Medium/Hard × 模式 Fast/Balanced/SuperHeavy。模式定请求级推理强度（fast=low / balanced=medium / super_heavy=high）+ SuperHeavy 800K token 熔断预算；难度定资源帽（Simple 256K 压缩窗口帽）与强度下限（hard ≥ medium）。非法值整体回落默认（medium/balanced）——档位是运营旋钮，坏输入不炸任务。
- **② 生效纪律（与 0.15.12 缓存纪律同一条线）**：切档【只动请求级参数】——ReasoningEffort 是请求顶层参数不进前缀哈希（切档零缓存代价）；压缩窗口帽走每轮现取的 resolver（worker/planner/mainagent 三 lane 经 ClampCompactionWindow 钳制，帽低于压缩下限不钳）。system 提示词与 tools 数组零改动——工具包「Tool Masking over Tool Removal」的缓存结论在本仓由架构直接满足。
- **③ 优先级**：请求级 effort 链 = DB agents.thinking_effort（非空才覆盖）> 任务执行档位 > profile 默认。档位不改变任何 lane 的 LLM profile 身份（归属由 agents.llm_profile_id 绑定裁定）——工具包 LockedSupervisorRoles 的「监督角色不得自选模型」在本仓天然成立。
- **④ 中途切档**：`GET/PATCH /api/tasks/{id}/execution-profile`（PATCH difficulty/mode 可传 auto=按任务书文本启发式判定）；注册表进程内即时生效 + settings KV（`exec_profile:<taskID>`）跨重启水合；切档落一条 system 活动供 UI/轨迹可见。createTask 新增 difficulty/mode 可选入参（auto 判定在建任务时点定档）。
- **⑤ 自动缩放**（settings 键 `auto_scaling`，默认 off——零开销直到显式开启）：planner 轮界评估（agent.AutoScaler）：高危发现/连续失败升档（3 轮冷却滞回防抖），SuperHeavy 预算 70% 降档；变更更新档位 + 落 system 活动。信号取任务 token 总量（ExplorationStore.TokenTotal 现成）与 high/critical 发现存在性。
- **UI**：任务详情总览页「执行档位」卡——当前难度/模式/窗口帽/熔断预算展示 + 四模式一键切换（重新拉全量档位）；端点不可用（旧后端）时整卡隐去。
- **明确不采纳**（与路线图一致）：wakebus（引擎已是 Notify+debounce 事件驱动）、artex_headless_run（demo 骨架）、ParallelHypotheses/组聊/建模开关字段（既有任务级配置已覆盖）。
- 测试：agent 6 用例（矩阵解析/auto 判定/effort 优先级/窗口帽钳制/缩放冷却与双向/注册表并发）+ server REST 1 用例（默认档/切档/跨 Engine 水合/auto 判定/非法回落/坏 JSON）全绿。
- 对齐检查：新增 2 条路由（GET/PATCH execution-profile），wiring 测试钉住；createTask 请求体扩展可选字段。


## [0.15.16] - 2026-11-19

### 功能（批次 A：任务级三档消息投递 + 保缓存硬打断——外部实现工具包 phase2 的本仓映射）

- **语义映射裁决（适配而非移植）**：工具包的 SessionMessageQueue/CancelActiveTurn 不移植——本仓干预基建更完善（按 intent 落库防崩副本 + 容量闸 + 消费标记 + KillWork 宽限取消 + 暂停→续跑链），三档全部映射到既有验证过的路径，**零 norma/harness 改动**。
- **① 软纠偏（steer，默认档）**：既有 SteerWork→PreToolUse 通道（下一个计划动作不执行、实时指令注入）。norma hooks 的 PostToolUse 无注入面，但 PreToolUse 时刻 = 上一个工具完成后、下一个动作执行前——语义等价工具包的 steer_soft。
- **② 硬打断（barge_in，新档）**：`bargeInWork` 一步完成「ControlWork 暂停（取消在飞 LLM 流/非关键工具，有界等待收场）→ runDetachedIntent 同 session 自动续跑并注入消息」——把 UI 原有「暂停→发消息」两步自动化。历史 append-only 安全性由 norma cleanResumed 丢弃悬空 tool_use 保证（= 工具包合成 tool_result 的本仓等价物），前缀缓存不破。
- **③ 排队（follow_up，新档）**：`FollowUpWork` → 独立 followUpBox → `steerHooks.Stop` 在 run 自然收场边界取空整队合并注入（【排队指令】包装）——本轮工作完全不受影响，消息只在「本来要停」的边界生效；独立于空转续跑配额。落库副本带 mode 字段（旧节点无 mode 按 steer 兜底，向后兼容），重放按各自通道回队。
- **REST**：`POST /api/tasks/{id}/steer` {intent_id, message, mode: steer(默认)|barge_in|follow_up}，公共校验 + 逐档既有 409 语义；route_wiring_test 补行。
- **UI**：任务详情 worker 会话输入区（运行态）新增档位 chips（软纠偏/硬打断/排队，硬打断选中时红色警示「将立即中断当前执行」）+ 常规消息发送按钮——运行中的意图免暂停直达干预；暂停态原有「直接继续/发消息续跑」链不变。
- 测试：新增 4 用例（follow_up 落库形状与双通道隔离+重放路由 / Stop 边界注入+inner 硬停优先+空转回归 / barge_in 暂停→续跑组合链含意图状态断言 / REST 校验矩阵经真实 mux）；既有 steer/kill/空转回归全绿。
- 对齐检查：无新端点面变化（/api/tasks/{id}/steer 为新增 1 条路由，wiring 测试钉住）。


## [0.15.15] - 2026-11-18

### 清理（全量审查 P3——纯删减零行为变更，双重铁证才删）

- **删除原则**：只删「deadcode 可达性分析不达 + 全仓含测试 grep 零引用」双重确认的条目；引用计数 >0、注释声明保留（deleteTaskFiles 测试契约 / isSodDegradableTool 测试+将来加白 / registerSensitiveResultTool 扩展点 / guardTaskScope* 大厅通道）、排期承诺（前端 edgeMethodologyImport、assetImportFile 方法保留待 UI 接线）的一律不动。
- **Go 侧删除（~90 行）**：db/nkey.go 旧层级编码键簇 8 函数（IPKey/PortKey/SiteKey/ServiceKey/ParameterKey/TechKey/SplitURL/NormalizeParamName/itoa——图存储迁 BIGSERIAL 主键后的遗留，保留可达的 DomainKey/RootDomain/TemplatePath/defaultPort）；enrich.isBlank；sod/normalizer.loosenScalarStrings（被 loosenScalarsSchemaFree 取代后的遗留）；server/biz_tools parseBizCtx+bizCtx 类型；edge_methodology_api.edgeMustName（edgeMustAll 在用）。
- **前端删除（~50 行）**：15 个零引用 api 方法（getLLM/setLLM——被 llmProfiles 家族替代；setActive/updateAgent/updateTask/updateConversation/deleteConversations——UI 走专一/批量路径；tokenDaily/runMetrics/skillFiles/sessionsForAttachment/taskHallDisabledTools/getAgentVisibility/agentPromptVersions/agentVariables——只读面无 UI）+ 3 个孤儿类型导入。**保留** edgeMethodologyImport/assetImportFile（盘点排期承诺）。
- 验证：go build/vet 全绿；tsc + biome + build:static 过；db/agent/server 全量回归绿；e2e 45 路由冒烟过。
- 其余 deadcode 报告项（~40 条）逐条裁决为有意保留或测试专用，留档不动。


## [0.15.14] - 2026-11-18

### 功能（全量审查 P1+P2 落地：手动蒸馏入口 / 确定性回执接线 / 单价表暴露）

- **① 传承图手动蒸馏入口**：`POST /api/legacy/distill/{taskID}` 此前无前端入口——蒸馏失败后「允许重试」的服务端语义悬空（0.15.14 全量审查实锤的真缺口）。传承图页头新增「手动蒸馏」：输入终态任务号即触发后台异步蒸馏（202），后端终态校验（409）与一次性可重入去重不变；触发后延迟刷新让新草稿可见。
- **② 确定性运行收尾回执接线**：`recordRunProgressReceipt` 设计上由运行循环在 run 结束时落一条「本次执行写回了什么」（内容来自 WriteCounts 确定性事实，非模型自述），但从未被调用——读侧工具（list_progress_receipts）在、写侧悬空。现在 worker.execute 唯一返回点前落回执（空计数不落，函数自带 guard；压缩后模型看不到自己写过什么，这条回执可回读）。
- **③ 模型单价表暴露**：`GET/PUT /api/metrics/prices` 此前仅供脚本 curl（注释自认「前端暂无页面」）。LLM 录制页新增「模型单价」卡：每 profile 一行，币种/输入/输出/缓存读（每百万 token）行内编辑、dirty 才出现保存按钮、保存后拉新表；单价是用量核算基准（runMetrics 的 $ 列由此驱动），此前只能 SQL 改。
- 测试：agent 包 worker/progress 相关用例通过；前端 tsc + biome（错误级零）+ build:static 过。
- 对齐检查：无新端点（legacy/distill 与 metrics/prices 均为既有路由首次接线；盘点「待接线」清单销账 2 项）。


## [0.15.13] - 2026-11-18

### 修复（外部审查核实后落地四项——zombie run / guard 覆盖缺口 / MCP 握手挂死 / thinking 覆盖雷）

- **① 偏锋人工停止修 zombie run（实锤）**：`POST /api/edge/tasks/{id}/stop` 此前只把 edge_runs 行翻成 cancelled，底层任务原样在跑——worker 继续领意图烧 token。现在联动正常任务控制链暂停任务（取消在飞运行 + paused 落库，与人工暂停同一条链；任务已终态/已暂停时幂等无关只记日志）。顺带硬化：请求体 MaxBytesReader 64KB、goal/description 修剪空白、错误分类（无 open run=404 经 `db.ErrNoOpenEdgeRun` 哨兵判定，不再靠错误文案字符串匹配；后端错误=500 不裸泄 err.Error()）。
- **② planner/mainagent 补挂 guard hook（SoD 覆盖边界文档自认的「最大已知缺口」）**：此前 `opts.Hooks` 只挂 chat 与 worker——这两个 lane 都带 Bash/Write，其工具调用全部绕过基线预检/短路表/拦截规则/审计链。现在 planner（engine 的 wireStrategyContext 统一装配 + 终局轮同款）与 mainagent（task_llm 构造点按任务注入）均经 SetGuardProvider 挂上任务 guard；拦截名单外的工具在 IsToolEnabled 门即刻放行，不给 planner 的 DB 写工具加判官延迟。**SoD 链仍只挂 worker**（保持文档边界：链的 scope 判定依赖任务范围，planner 的 DB 写工具过链只会平添规整器延迟；chat 的 task_id=0 场景链会 fail-closed 全拒，不能开）。
- **③ MCP 握手挂死修复（实锤）**：a) `NewSSE` 的流 ctx 此前裸 context.Background()——卡死的 server（重启中的容器/半开 TCP）会把 GET /sse、端点公告、initialize 全部无限挂死，agent 装配路径不返回就永远进不了 mcpFailTTL 失败冷却，worker 领取队列被堵死。现在握手 30s 看门狗 + 调用方 ctx 取消转发（仅握手期，成功后解除——流本体独立于调用方 ctx 是刻意的：连接跨 run 复用），initialize 单独 30s 预算；看门狗与握手完成竞态时以 streamCtx.Err() 复核按失败处理。b) stdio 传输的 initialize 同加 30s 预算（此前只靠通常无 deadline 的调用方 ctx）。c) `newHTTPClient` 改 clone `http.DefaultTransport`——裸 `&http.Transport{}` 丢失 TLS 握手超时/空闲连接回收/ExpectContinue 超时（拨号超时由 netguard guardedDialer 已有，10s）。
- **④ agent/thinking.go 三修（部分属实，雷潜而未爆）**：① 返回值 "" 的字段不再覆盖——旧实现无条件赋值，解析器返回全空时会把调用方预设的 Thinking/ReasoningEffort 清掉，与「"" = 不覆盖」契约矛盾（现网调用方未预设故未实际发生，属潜在雷）；② opts == nil 直接返回；③ 裸全局可变函数变量 `ThinkingOverride` 换 `atomic.Pointer`（SetThinkingOverride 原子注入——装配重入路径不再有数据竞态）。
- **审查误判留档（不改）**：`get_finding_traffic` 并非缺 seed（走 seedFindingTrafficTools 专属通道，tools 表有行有绑定）；「MCP 在 SoD 链外」对工具调用而言不成立（PreToolUse 对所有 CoreTool 生效，UnwrapDeferredName 专门处理延迟 MCP）——「MCP server 子进程/http 传输不在链内」是文档留档的 by-design 边界；normalizer 无 ToolChoice 为 0.15.11 已留档取舍；TestDoStreamHonorsConfiguredRetries 为环境性已知项。
- 测试：mcphttp 三条新用例（卡死 server 握手看门狗/调用方取消中止握手/Transport clone 语义保留）；agent thinking 两条（全空不清预设/nil 安全）；db 集成测试补 StopOpenEdgeRun 哨兵断言；全量构建+vet 过。
- 对齐检查：无新端点（stop 语义增强属于既有路由）。


## [0.15.12] - 2026-11-18

### 功能（四项：建模图布局排序 / 偏锋卡片化 / 缓存命中率 / 跨任务参考复用）

- **① 业务建模布局排序**：语义图此前只有 d3-force 一种力导向，节点一多互相缠绕看不清结构。新增四档一键切换——力导向（默认）/层次排序（antv-dagre，rankdir=TB）/网格排序/径向排序（后两档按 node_type 等级序再按名称排，同类聚拢、排序口径稳定），工具栏另有「一键重排」按钮把手动拖乱的图归位。静态布局下拖拽改用 drag-element（力导向仍 drag-element-force）。切换走重建图路径（同主题切换既有机制），数据由 applyData 通道自动灌回。
- **② 偏锋探索卡片化 + 滚动容器**：方法论从手风琴行改为响应式卡片网格（md 两列 / xl 三列，档位圆点+名称+档级+判据+标签+摘要两行截断+底部快捷操作），点卡片展开完整内容与编辑（一次一条不变）；方法论区 max-h-[60vh]、运行记录区 max-h-[36vh] 滚动容器——条目增多不再把页面拉长，滚轮上下滑与其他 reArtex 列表一致。发起区与全部 PUT/DELETE/轮询逻辑不动。
- **③ 缓存命中率（95%→更高）**：a) **分 worker 命中率可观测**——/api/llm/records/stats 的 by_worker 增加 input_tokens/cache_read/cache_hit_rate（口径同全局：cache_read/input），LLM 录制页按 worker 渲染命中率徽标，「哪里在漏」从猜测变成可见；b) **chat 记忆块改道**——原走 SystemReminderFunc，在 harness 里每轮前插为 messages[0]，记忆一变整个会话前缀从第 0 字节发散、缓存全失；改拼进本轮输入尾部（与 edge 触发词同通道），历史轮次字节稳定；c) **MCP 工具确定性排序**——ListMCP 无序查询序 + tools/list 枚举序会让装配重建/冷却恢复后工具数组换位、请求前缀发散；server 按 ID 序遍历、工具按 (server,tool) 序收敛，全量与缓存重建两条路径同序；d) **消息尾缓存断点（默认关）**——norma llm Config.CacheMessages + agent.Config 透传 + settings 键 `cache_messages`，开启时最后一条消息追加 cache_control:ephemeral（Anthropic 官方增量缓存模式；对 MiniMax 隐式前缀缓存无害）。结构性 miss（planner 每轮新会话的态势输入、一次性小调用）是真实新 token 不可消除，现实目标 ~97-98%。
- **④ 跨任务同目标参考复用（仅参考、不做主线）**：P5 传承图（终局蒸馏→评审→accepted→新任务注入）已存在，本批修两个缺口+扩一个通道：a) **修活指纹匹配**——launchTask 真实注入路径 seedKVs 此前恒传 nil，资产指纹强信号通道（host/url 重叠）是死的；现从任务书 parseTarget 解析 host/url 喂进匹配器，且蒸馏契约要求 fingerprint 必须带 host（取自材料不得编造），同目标不同措辞的任务经资产 KV 即可匹配到历史经验；b) **planner 同目标参考段**——situationAppendProvider 新增②段：db.SiblingTaskDigests（同公司范围兄弟任务 ≤5 个，goal/状态/发现数/最新发现摘要 ≤3 条×120 字，预算 ≤8K），框架纪律写死在块内（仅供参照/不做主线/不得据此直接提交发现/历史结论可能过时）；settings 键 `cross_task_reference`（默认开）；c) **评审瓶颈缓解（默认关）**——settings 键 `legacy_auto_accept`：蒸馏产物中 fingerprint_summary（事实性目标画像）直接 accepted 入池，experience/pitfall 仍走 /legacy 人工评审。
- 测试：norma-llm 三条新用例（尾断点 copy-on-write/空历史透传/开关两态请求体）；db TestSiblingTaskDigests（同公司可见/异公司排除/自身排除/方向无关/无公司空集）；db/agent/server 全量回归通过；前端 tsc 过。
- 对齐检查：无新端点（stats 扩展字段与 settings 键为既有面）。


## [0.15.11] - 2026-11-17

### 修复（跨模型过拟合审计——按用户裁定全面调研"换其他模型还会不会出这些问题"）
- **审计范围**：全部 14 处 llm.CompletionRequest 构造点 + provider 映射层（anthropic/openai/responses）+ llmpool 失败分类 + thinking 能力注册表 + SoD 契约语义。结论：请求形状已全部由 profile 配置驱动（ThinkingType/ReasoningEffort/Stream/MaxTokensField/ContextWindowK），代码层无模型名特化分支；MiniMax 实测暴露的六类问题（思考开关 400/禁思考空响应/标量字符串/数字数组元素/多意图拆分/配额 429 语义）对应的修复全部是**契约放宽或配置驱动**，对其他模型无副作用。
- **修 ①openai 格式 temperature 自动省略（reasoning 族兼容）**：o1/o3/gpt-5 族拒绝 temperature 参数（只认默认值）——判据用 profile 已有的 MaxTokensField==max_completion_tokens（正是这族模型的配置信号），该分支下不再透传 temperature；常规模型行为不回归，无模型名硬编码。normalizer/裁判/策略学习的 temp=0 在 Anthropic 与常规 OpenAI 模型上继续生效。
- **修 ②llmpool 硬熔断按类别给准确指引**：OnHardTrip 增加 kind 实参（auth/quota/missing）——此前 429 配额耗尽也喊「鉴权失效请轮换 Key」，误导运维方向；现在 quota→「请充值/等待重置」、missing→「核对模型名/base_url」、auth→「轮换 Key」。多厂商配额 regex（MiniMax/OpenAI/Anthropic 形态）已在位，匹配不到保守回退瞬态。
- **审计留档（不改动的裁决）**：①SoD 规整器无 ToolChoice 强制——倾向散文回答的模型会 fail-closed（malformed）而非误放行，属设计取舍，未来可加 ToolChoice 支持；②thinking 能力注册表是设计好的三层能力发现（注册表+探测+手动兜底，未收录模型回退保守全集），非过拟合；③标量矫正/数字元素矫正只在解码失败路径触发，对输出规范类型的模型零影响；④多意图契约 1..N 对单意图模型同样成立；⑤中文提示词全主流模型可用。
- 验证：go build 全绿；llmpool/norma-llm/compaction 测试全过（norma llm 有一个环境性 pre-existing 失败 TestDoStreamHonorsConfiguredRetries，干净树同样失败，与本批无关）。
- 对齐检查：无新端点。

## [0.15.10] - 2026-11-17

### 修复（发现涨至 28 条含 3 critical + 压缩链 thinking 硬编码全面清除）
- **发现 20→28**（高 10/中 9/低 2/信息 4）：新增 **RCE（critical）**——/command/vul1、/command/vul2 已认证命令执行实证；**SSRF（high）**——/ssrf/vul?url= 服务端按攻击者 URL 发起请求；**验证码绕过（high）**——/loginProcess URL 编码路径变体绕过图形验证码；**支付金额篡改（critical×2）**——由业务建模双层差异链自动产出（design 层声明"支付金额完整性边界"→behavior 层实测缺失→差异转发现，建模→发现全链闭环的首次实战验证）。
- **压缩链 thinking 硬编码清除（4 处）**：cold_digest.compress / norma compaction summarizer（[compaction] 日志实锤——上下文压缩整条瘫、会话越滚越肥）/ server/intercept 裁判 / sidequestion 摘要——「必须 adaptive thinking」的模型（MiniMax-M3.1-Flash）对 thinking.type="disabled" 直接 400 invalid params。全部改为不发送该字段走网关默认；sidequestion 测试判别器从 r.Thinking 改按系统提示词区分摘要/回答调用（isSummaryCall 助手）。
- 对齐检查：无新端点。

## [0.15.9] - 2026-11-17

### 修复（靶场持续运行暴露）+ 发现涨至 20 条
- **arguments 数组混入数字元素**：MiniMax 偶发把 ["-c", 30] 这类数字叶子混进 string 数组 ⇒ 严格 []string 反序列化炸 ⇒ normalizer_malformed。修=loosenAgainstSchema 补 "string" 分支（schema 声明为 string 的位置把数字/布尔叶子 coerce 成字符串——与 boolean/number 分支对称；无 schema 位置依旧绝不触碰，TestLoosenFixesNumericArrayElements 钉住）。实机：此前 23:45-00:32 三批任务 10/8 的 grep/ls 复合命令被此错法拒掉。
- **发现 15→20**（高危 8/中危 8/低危 2/信息 2）：新增 **IDOR 水平越权**（/logic/idor/horizontal/getUserInfo?username=X 任意用户密码泄露，实测 admin/test 均回显明文口令——高危）、**会话劫持**（/druid/websession.json 未鉴权导出 1000 条活跃 Tomcat 会话——高危）、**stored XSS 链**（上传 .html/.svg 落地可回读）、**access_control_bypass**（XFF 变体）、信息级（/api/init.json 全站 51 条路由树泄露等）。流量证据绑定已生效：#19/#20 各绑 4 条录制交换，累计录制 822 条。
- **已知限制（记录不动刀）**：本机 fake-ip DNS（198.18.0.0/15）下代理按解析后 IP 判出网范围，非标端口（如 :9090）的域名目标会被保守拒绝——80/443 主链路不受影响（688+ 条已录）；如需测非标端口可显式加 ip/cidr 范围行。
- 对齐检查：无新端点。

## [0.15.8] - 2026-11-16

### 修复（遗留清单三件 + 靶场第三轮实测又抓两条阻断）
- **流量证据链打通（遗留①）**：录制代理与 worker 出网注入的基础设施本就完整（proxyEnv 注入 HTTP_PROXY+任务身份令牌 → egress 归属+范围校验 → 录制），只是 `traffic_capture` 开关默认关。运行时打开（PUT /api/settings，不入仓库）后实测：经代理的请求即被录制（GET robots.txt 302 入账）；此后 worker 的 HTTP 动作自动带任务归属，发现可绑流量证据。
- **对抗复核积压清理（遗留②）**：24h TTL 过期闸本就存在且在工作（18 条已 dismissed）；补 `sod_review_ttl_hours` 设置（小时，默认 24、下限 1）让运维按积压承受度调——db 新增 GetInt 助手，调度器每轮读。
- **bizmodeler 会话消息（遗留③自证解决）**：之前"0 条消息"是配额死亡期 turn 无产出；真实跑通后 conversation_activities 749 条可读，messages API 无缺陷。
- **复合动作整段复核推翻子段裁决（新抓，阻断级）**：子段循环逐条通过后，整段复核仍对 StructuredIntent（=首段，常为本地段）跑 scope → 「无可判定目标」把已通过的复合动作整个推翻；叠加模式缓存复用放大成批误拒（干净单 curl 被缓存里三段复合结果拖死）。修=①整段复核跳过 scope（compound_evaluated，子段已逐条裁决，其余检查照跑）②多意图结果只落精确缓存键不落模式键（子意图拆分与具体命令结构强相关，跨载荷复用必然错位）。回归测试 TestSoDGateCompoundEvaluatedScopePass 钉住。实机：探针 worker 的 curl（范围内 授权测试靶场）此前被拒，修复后放行可期。
- 对齐检查：无新端点；新设置键 sod_review_ttl_hours 为行为面。

## [0.15.7] - 2026-11-16

### 修复（业务建模图读侧双 bug——bizmodeler 实际已建 64 节点/96 边/27 差异，API 恒空）
- **bizTaskGraphHandler 读错列**：读 tasks.company_id（恒 0——公司绑定实际存于 task_scope kind=company 行）⇒ companyID=0 传入。
- **ListBizGraphScoped 位置参数错位**：companyID<=0 时 companyPos 仍被引用，占位符指到 limit 实参 ⇒ company_id=2000 恒 0 行。
- 修=handler 从 task_scope 取公司绑定 + company 过滤按需追加 + 范围 fragment 三态（任务范围/公司范围/平台 fail-closed）——无任务上下文的公司视图不再误入平台空集；显式 company_id 即授权上下文，无上下文依旧 fail-closed 纪律不变。实机：task10 64/96/14 open diffs、公司级 64/96 可读，页面渲染完整业务图（截图目验）。

## [0.15.6] - 2026-11-16

### 修复（靶场第二轮实测暴露）+ 冒烟脚本口径同步
- **btw 侧问 checkpoint 毒丸无限重试**（每秒数条刷屏）：约束/外键类永久错误+超限重试即出队，丢弃时打 key+父 id（此前零上下文）；sideParent 在 ExpID≤0 时拒绝开会话（父组合注定违反 side_question_sessions_check）。
- **requiresTarget 改集合语义**：复合命令本地子段无 target_hint 是正确输出，任一段带目标即满足——逐段强制错杀「payload 含 URL+首段本地命令」的正常拆分。
- **Bash 无网络指示符时 scope 按构造通过**（单意图 no_egress_local/复合 compound_local_segment 全段）：纯本地命令（grep/tshark/排序）被「无可判定目标」拒绝的实测堵点；其余检查照跑。
- **SearchExtraTools 入 NotGatedTools**（平台只读检索）+ **越界拒绝理由带上被拒目标**（此前「无声被拒」无法定性）。
- **e2e 冒烟脚本 legacy 期望同步 0.15.1 语义**（pianfeng 无斜杠 200、edge-exploration 308→pianfeng）——此前沿用 0.14.9 旧口径，CI 一跑必红。实跑 45 页全过。

## [0.15.5] - 2026-11-16

### 修复（真 LLM 靶场实战暴露的四层阻断——平台首次真实任务全链测试）
布置真实任务（授权测试靶场 授权靶场，MiniMax-M3.1-Flash）后依次暴露并修复：
- **① 意图规整器契约与模型天然行为冲突（最高优先级阻断）**：模型对复合命令（`cd && pwd && ls`）按子动作拆成 N 条 `emit_structured_intent`——语义正确、提示词「失败处理」段也写着「输出**多个** structured_intent」，但宿主代码要求「恰好一个」⇒ 全部 malformed ⇒ 一切 Bash 被 SoD 拒绝（3 分钟失败 #1→#20，worker 全瘫）。修复：接受 1..N 条（NormalizerResult.MultiIntent），每条独立过同套校验，**闸门逐条裁决、全部放行才放行原始动作**（SoD 不变式不松）；混入异名工具/任一条校验失败照旧 fail-closed（单测 TestSoDNormalizerAcceptsMultiIntent / MultiIntentFailClosed）。
- **② 标量矫正对嵌套字段失效**：tools 表 schema 的 shell_features 行只有 `{type:object, description}` 无 properties——schema 引导矫正走不进嵌套，`"has_background":"false"` 永远修不了。修复：schema 引导无效时的 schema-free 兜底矫正（loosenScalarsSchemaFree，仅在该濒临拒绝的路径上生效）。
- **③ 任务工作区前缀硬编码 `/app/data/tasks`（部署可移植性 bug）**：本机部署 data_dir=<本机数据目录> ⇒ B5 本地任务目录豁免永不命中 ⇒ 本地文件分析（grep/排序/读改写）全量被「无可判定目标」out_of_scope 拒绝（107 次）。修复：taskDirBase 包级变量 + SetTaskDirPrefix（main.go 启动注入 <data_dir>/tasks），classifier 同源函数一并统一；Write/Edit 文件工具落任务目录内同样豁免（FileToolLocalWrite，写文件动作本身不触网，反向守卫不适用）；复合动作的本地子段（无 target_hint）scope 按构造通过（compound_local_segment，理由进审计）——目标段照常逐条裁决。
- **④ report_finding 不在任务内写清单**：发现清单是任务自己的账本，与 record_fact 同性质；实测 LFI/上传证据被标「evidence 里嵌了 shell 命令」→ needs_review → 复核拒 ⇒ 平台核心产出物丢失。修复：入 taskInternalWriteTools（熔断/额度/tier2 照跑），钉住测试同步更新。
- **⑤ 规整器提示词领域校准**：anomaly_flag 判定对象=「绕过审查的企图与任务无关的破坏」，不是授权目标上的攻击载荷（注入/编码/穿越是渗透任务的正常内容）——过度标记会把一切正常工作送进复核队列。文件与活库提示词同步升级 v3（DB 首插生效，PUT /api/agents/intent_normalizer/prompt 推送）。
- **实机验证**：修复期间真流量裁决 approve 613+ 条、复合动作逐条裁决日志坐实（拆 2/3/4/5 条均有）、本地豁免后 out_of_scope 归零；worker 在配额耗尽前产出真实验证过的漏洞证据（未授权任意文件读取 /etc/shadow + 未授权文件上传，PoC 带 /safe 对照组）。测试环境问题另记：playwright-mcp 二进制缺失（浏览器工具 2min 冷却循环）、MiniMax Token Plan 配额耗尽（429，任务停摆——外部资源限制）。
- 对齐检查：无新端点；行为面=SOD 多意图裁决/本地豁免扩展（审计标记 multi_intent/bypass_reason 可回放）。



## [0.15.4] - 2026-11-15

### 改进（方法论全量可编 + 配置写穿运行时；业务建模页 UI 风格对齐全站主题）
- **每一条方法论都可全量编辑（内置与自定义同一套）**：内置条目此前只可调启停/优先级（0.14.7 红线），按用户裁定放开为全量可编——名称/档位/证据级/判据类型与上限/说明/标签/优先级，走注册期同套硬约束（五类判据/上限强制/隐喻无判据），存 builtin-override 行，「恢复默认」随时回退代码常量本体（registry 新增 builtinBackup + Upsert/RestoreBuiltin/IsBuiltin/UpsertValidate）。批量导入面维持只落启停/排序：外部数据不注入本体。
- **配置写穿运行时（修"虚"的最后一块）**：0.15.3 及之前 UI 写的启停/优先级只进展示合并视图，轮换（edge.ListEnabled）、判定（edge.Get）、agent 工具（ListAll）读的是纯代码常量——停用不停、排序不乱；自定义条目更进不了轮换、发起后被 dispatch 以"未注册或未启用"拒绝。现在：启动时 syncEdgeRegistry 全量写穿（orchestration 播种序列），PUT/DELETE/import 后即时写穿；自定义条目删除补 Unregister（写穿反向）。轮换/判定/工具读到即合并态。
- **业务建模页 UI 风格对齐全站主题**（用户指出的违和深蓝块）：G6 画布背景不再硬编码 #111827/#f0f2f7，改画布透明 + 外层容器 bg-background 承担底色——暗色下画布与全站同底色，仅文字/边按明暗两套；功能零改动。
- 验证：go build/vet 全绿；registry 单测 TestUpsertOverrideRestore（内置覆盖/硬约束拒/恢复默认/自定义注册移除/内置拒 Unregister）全过；前端 tsc/biome 全绿；实机——内置条目全量编辑→合并视图/运行时同现新值→恢复默认回退全链（见下）。
- 对齐检查：无新端点（PUT/DELETE 语义升级）；前端入口=偏锋页方法论展开编辑器（所有条目统一）。

## [0.15.3] - 2026-11-15

### 改进（偏锋页二改，按用户反馈：可点开完整内容、交互落地、运行记录变实）
- **方法论可点开看完整内容（0.15.2 未落实的要求补正）**：行点击展开（手风琴）——完整说明正文/档位语义（一级·机器可判/二级·需改造/三级·隐喻 + 证据级）/判据全称与上限/全部启发标签直接可读；0.15.2 把 summary 折进 hover title 是错误实现（悬停不可发现，点开看完整内容才是要求本身）。
- **轮换优先级落地**：数字含义双处可读（行 title 与展开档位行："大者优先，决定轮换顺序"）；行内失焦/回车自动保存，砍掉逐行"保存"按钮。合并视图对覆盖行输出 `source=builtin-override`（导入侧不读 source，无回环影响），前端据此把破坏性操作分为「恢复默认」（内置覆盖行）与「删除」（自定义行），内置原样行不显示删。
- **内置红线如实呈现**：展开区只读展示内置本体与判据（代码常量不可改，只可调启停/优先级）；自定义条目展开区全量可编辑（名称/档位/证据级/判据类型与上限/说明/标签/优先级），新增与编辑走 PUT 同一套注册期校验，拒绝原因（后端原文）直接进 toast。
- **运行记录变实**：展示目标/状态徽章（进行中/排队中/已完成/已停止/失败）/判据进度（终态格点/总格点+进度条）/停止史（break×n 等）/方法论；可跳任务详情页（/function/tasks/detail?id=）、运行中可停止；有进行中运行时 5s 轻轮询。发起区选中方法论时显示其说明（发起前知道注入什么视角）；下拉默认项「轮换表首位」改为「自动轮换」。
- 验证：go build/vet 全绿；前端 build/tsc/biome 全绿；实机——点开/收起、优先级自动保存、启停、恢复默认、运行记录进度与跳转全链过。
- 对齐检查：无新端点（复用 0.14.7 PUT/DELETE 与 0.15.2 run/stop）；合并视图 source 字段为前端消费的新输出。

## [0.15.2] - 2026-11-15

### 改进（偏锋页重写，按用户裁定：可点击、可编辑、能直接布置任务、从简布局）
- **能直接布置任务**：POST /api/edge/run——发起偏锋探索（goal/范围说明/方法论下拉），与 edge_run 工具共用宿主链 StartEdgeExploration（0.15.2 抽取：CreateTask(edge 类别,不限时)→edge_mode→StartEdgeRun→绑会话播报→launchTask→执行体排队，不另造第二条）；**实机全链验证**：REST 发起→真实建任务+开运行（run_id=1）→edge/tasks 出现新运行→人工停止（cancelled）→清理。
- **可交互**：POST /api/edge/tasks/{id}/stop（人工停止写审计）+ 运行记录块（判据进度/stop 分布/停止按钮）。
- **可编辑**（0.14.7 的 PUT/DELETE 落地到 UI）：方法论紧凑行——启停一键（PUT 内置覆盖/自定义）、优先级行内编辑保存、删（custom 删/内置恢复默认）、新增自定义表单（tier/判据类型/上限，导入通道同一套校验拒绝非法条目）。
- **从简布局**（砍标语/提示文字/次要标签）：方法论改紧凑行——色点=证据等级、T{tier} 缩写、判据缩写（覆盖/收敛/穷举/耗尽/衰减+上限）、summary 折进 title 悬停、隐喻不显示"无判据"、无引导文字；tier≤3 上限输入在新增表单。
- 验证：go build/vet 全绿；REST 全链实机五步全过（见上）；前端 build/tsc/biome 全绿。
- 对齐检查：POST /api/edge/run 与 /api/edge/tasks/{id}/stop 的新前端入口=偏锋页发起区与运行记录块；方法论编辑=偏锋页紧凑行（0.14.7 行为面转正）。

## [0.15.1] - 2026-11-15

### 修复 + 拆分（用户报障根治：偏锋探索"页面渲染出错"的序列化回归 + 两入口拆分独立页面）
- **偏锋探索渲染崩溃根因（实证修复）**：错误信息 `Cannot read properties of undefined (reading 'length')` 的根因是 **0.14.7 合并视图的序列化回归**——`methodologyPayload.HeuristicTags` 带 `omitempty`，内置一二级条目（8 条）无启发式标签时字段被省略 → 前端 `m.heuristic_tags` undefined → `EdgeOverviewPanel` 的 `.length` 崩溃。0.14.7 前的旧 handler 直接 marshal 结构体（无 json tag、字段恒在）故不崩。**不是技术栈问题**。修复三层：① 后端去 omitempty 且 nil→[]（实机：12 条全部输出 list）；② 前端类型放宽 + `(m.heuristic_tags ?? [])` 防御；③ 0.13.5 error boundary 兜底（本次报障正是它把白屏变成了带错误信息的可定位页面）。
- **页面拆分（用户裁定还原 0.11.8 的"并页"结构）**：偏锋探索独立为 `/function/pianfeng` 真页面（sidebar 直达，复用 EdgeOverviewPanel）；business-graph 页移除"业务图/偏锋探索"tab 机制回归纯业务建模；legacy 链反转——pianfeng 恢复真路由（200），edge-exploration 308 直达 pianfeng；URL 矩阵测试同步更新（新增 pianfeng 200 两例 + legacy 链头带 query 用例）。F7 同 URL 消歧逻辑保留（通用机制，URL 不同后自然不触发）。
- 验证：go build/vet/URL 矩阵测试全绿；前端 build/tsc/biome 全绿；pianfeng 页静态产物导出确认；实机——heuristic_tags 12 条全 list、pianfeng 200、business-graph 无 tab 残留、legacy 308 → pianfeng。
- 对齐检查：偏锋探索前端入口从"页内模式"变回独立页（sidebar URL 更新）；无新后端功能点。

## [0.15.0] - 2026-11-15

### 移除（用户裁定：版本错位提示弹窗——换装提醒是一次性场景，不需要常驻轮询机制）
- 移除 0.14.6 F12 的版本错位检测（client-error-boot 的 /api/health 轮询比对 + 无限期 toast + build.sh 的 NEXT_PUBLIC_ARTEX_VERSION 注入）。错误上报（0.13.5）不受影响。
- 验证：build/tsc/biome 全绿；产物中提示串清零。
- 对齐检查：无新功能点；/api/health 既有面不受影响。

## [0.14.9] - 2026-11-15

### 新增（v0.14 批次八收官：交付质量——e2e 进 CI / precommit 执行化 / 备份脚本 / 供应链可信）
- **H1 e2e 冒烟进 CI**：scripts/e2e_smoke.sh（起内嵌前端真服务 → 45 条路由 HTTP 200 → 尾斜杠 200 serve + legacy 308 链 + 登录页平台标识）；ci.yml 新增独立 e2e-smoke job（frontend 产物 + PG service，本地已实测通过）。渲染层回归从此在 CI 里跑，不再靠人肉。
- **H2 precommit 执行化**：scripts/precommit-test.sh（go build/vet + 全量测试 CI 同款 + tsc + biome 相对基线；--quick 跳全量）——开发纪律 §1「先运行后提交」从纪律变成一条命令。
- **H3 备份脚本**：scripts/backup.sh（pg_dump + 工作目录打包排除 logs + **审计链头单独导出**——哈希链的跨介质防篡改闭环）。compose 一键栈核对确认已在位（secret 供给/cap_drop/迁移连接分离，不重复施工）。
- **E11 供应链可信**：release.yml 增 syft SBOM（SPDX JSON，挂 release 资产）+ GitHub artifact build provenance（attestation 可验证产物由本仓库本 workflow 构建）。
- **B2/B3 核对确认**：建群 UI 无套餐/证据保全写入口（写入口仅 API 且显式拒绝带明确文案）——UI 置灰实际已在位，不重复施工。
- 验证：e2e 冒烟本地实跑通过（45 页/URL 语义/标识全过）；脚本可执行位赋权；YAML 合法性校验。
- 对齐检查：无新用户功能面；交付质量设施（CI/脚本）为行为面，入盘点表。



### 改进（v0.14 批次七：前端可维护性——温和拆分试点；大改按「不能弄坏任何东西」纪律分批推进）
- **G1 试点：findings 页统计卡提取**（1300+ 行页文件 → `_components/finding-stats.tsx` 纯展示组件）——零行为变化（stats 进、卡片网格出）；页文件后续拆分按同款「独立可读的展示块」粒度分批推进。tasks 页（3200 行）拆分排后续批次（拆分是机械工作但验证成本高，一次全拆与红线冲突）。
- **G3 核对确认**：ScopeSentry 未配置引导已在位（未就绪 → 配置卡 + 引导文案；502→503 语义修正已在 0.13.2）——审查判定的缺口核对后确认已闭合，不重复施工。
- **如实降级**：批量操作乐观更新与虚拟滚动（@tanstack/react-virtual 引入+表格改造）移入延伸池——依赖引入与表格改造的回归面在连续施工批次里无法充分验证，不做半成品。
- 验证：npm build/tsc/biome 全绿（拆分前后产物同构）。
- 对齐检查：无新功能点；纯结构重构。



### 新增（v0.14 批次六：方法论 DB 化——内置本体留代码常量，DB 存覆盖与自定义）
- **数据层**：`db/schema_edge2.sql`（edge_methodologies：source=custom|builtin-override；tier/grade CHECK）+ db/edge_methodology.go（Upsert/Delete（custom 删，builtin-override 恢复默认）/List）+ EnsureEdge2Schema 启动接线。**红线**：内置 12 条的本体与判据在代码常量（internal/edge 注册表编译期钉死），DB 只持久化内置条目的启停/排序覆盖与用户自定义全量。
- **internal/edge 增量**：Register 增 initDone/builtinRegistered 标记（init 注册=内置，运行期注册=自定义）+ Unregister（内置 id 拒绝移除）——自定义条目经同一套注册期校验（五类判据/上限强制/隐喻不带判据）后落 DB，不进本轮内置轮换 map（读取侧重新校验合成，无重启前后行为漂移）。
- **REST 五条**：GET 合并视图（内置 ∪ 自定义，覆盖行生效）/ PUT /{id}（内置=覆盖行本体从代码常量补齐）/ DELETE /{id}（custom 删 / 内置=恢复默认）/ export / import（逐条过同套校验，rejected 回传）。旧只读 handler 替换为合并视图。
- 运行抓出并修复两处：PUT 内置覆盖行本体字段空撞 CHECK（从代码常量补齐）；List 的 heuristic_tags jsonb 直接 Scan []string 失败（RawMessage 中转 Unmarshal）。
- 验证：build/vet/edge 包回归全绿；实机八步全链——合并视图 12 内置→内置覆盖生效（enabled=False/priority=5）→恢复默认（True/90）→非法导入拒（隐喻带判据 rejected）→合法导入→清理。
- 对齐检查：合并视图=偏锋总览页既有数据源（前端零改动自动获得自定义条目）；import/export 为行为面（运维通道，入盘点表）。



### 新增（v0.14 批次五：平台健壮性——五件套）
- **E1 getTask 只读回源**：内存任务注册表 miss 时回 DB 读任务元信息（readonly_backfill=true 标记 + engine_mode=readonly，不重建 manager 态）——直插 DB 的任务行此前对运行中服务不可见（全量审查实测的管理面 404）。实机验证 ✓。
- **E2 凭据强制加密模式**：settings 键 `require_master_key`（默认关）——开启后未配置主密钥时，mp token 保存与 llm_profiles api_key 写入 409 并明确文案；加密状态端点回传开关值。降级从默认变显式选择。
- **E3 启动诊断清单**：GET /api/diagnostics——master_key / agent_desktop / chrome_devtools / wxa_runner / mp_runtime_url / unpack_backend / scopesentry / master_key_file 八项实时求值（warn 附原因与去向）。实机 warn=4 与环境实况吻合。
- **F12 版本错位提示**：构建注入 NEXT_PUBLIC_ARTEX_VERSION（build.sh 从 VERSION 注入），前端每 5 分钟比对 /api/health——错位即无限期 toast + 一键刷新（会话内只提醒一次）。
- **B5 任务终局会话播报**：mirrorDecision 放行 system 的【治理收尾】行（精确前缀，不泛化 text 防洪水）——收口 0.11 时代的 v1 妥协。
- 运行抓出并修复：/api/reports/generate 双重注册路由 panic（server.go 与分组注册各挂一次，启动即 panic 被守护退避抓住）。
- 验证：build/vet 全绿；前端 build/tsc/biome 全绿；实机三项（诊断/E1 回源/加密端点回传）全过。
- 对齐检查：/api/diagnostics 前端入口排后续（数据面先落，设置页聚合展示随批次七）；其余为行为面。



### 新增（v0.14 批次四：报告生成器——任务维度初稿的确定性拼装；终审永远人工）
- **POST /api/reports/generate**（task_id+可选标题）：全量来自库内事实的确定性拼装——执行概览（目标/发现四级计数/差异状态计数/缺口计数/偏锋运行分布）、发现清单表、业务差异表、未闭合缺口、任务相关审计摘要（尾部 10 条）；生成稿 created_by=report-gen 标记初稿来源，落报告中心（可编辑/可导出 .md）。不引入模型判断（LLM 润色为后续可选，终审永远人工）。
- **报告中心入口**：头部「从任务生成初稿」（任务 id 输入 + 生成按钮），生成即刷新列表。
- 验证：build/vet 全绿；前端 build/tsc/biome 全绿；实机——夹具任务+confirmed 高危 finding → 生成稿（概览计数/发现表/终审声明完整）→ REST 清单可见 → 夹具清理。
- 对齐检查：生成入口=报告中心头部（任务 id 输入）；生成稿在报告中心既有展示/编辑/导出面直接可用（无新 UI 面）。



### 新增（v0.14 批次三：biz 差异反向消费——关差建议闭环 + 批量裁决 + 筛选分页）
- **suggest_close 建议关差**：pushed 差异的派生意图（behavior_ref.intent_id）所在探索产生了 confirmed 高危 finding ⇒ 差异清单带建议关差标记——**裁决永远人工**（不自动落），前端琥珀色徽章提示。BizDiffSuggestCloseByTask 单 SQL EXISTS 判定。
- **批量裁决**：POST /api/business-graph/diffs/batch-decision（ids+status+理由必填；逐条容错 applied/failed 分账——不存在的 id 进 failed，RowsAffected 校验；一次审计落账）。前端复选框 + 批量状态/理由模板（5 个常用模板一键填入可改）。
- **ListBizDiffs 参数收敛**：opts 结构化（wstg/stride 前缀筛选 + offset 分页 + CountBizDiffs 总数面）；旧签名保留委托。差异面板加 WSTG/STRIDE 输入筛选、上一页/下一页与 total 显示。
- 验证：build/vet/真库集成测试 TestBizDiffBatchAndSuggest（批量 applied/failed 分账 + suggest 两态判定 + wstg 筛选）全绿；前端 build/tsc/biome + 产物 chunk 回验（"建议关差"在产物）；实机——批量端点校验（ids 必填 400）/wstg 筛选 200。
- 对齐检查：差异面板新增的批量条/suggest 徽章/筛选/分页与后端参数一一对应；新端点 batch-decision 前端入口=面板批量条。



### 新增（v0.14 批次二收官：F4 gap 实体——覆盖缺口对账 + 派生候选消费面；pending_items 清零）
- **数据层**：`db/schema_gap.sql`（coverage_gaps：task×asset 唯一，状态机 open→targeted→closed 原位演进）+ `db/gap.go`（ComputeCoverageGaps 确定性对账 / CloseCoverageGap 宿主闭合 / ListCoverageGaps）+ EnsureGapSchema 启动接线。缺口判据（宁漏勿误）：链接到任务且无 open 意图锚的资产 = 缺口；获得锚 → targeted；closed 不被重扫复活。真库集成测试 TestCoverageGapLifecycle 全链 PASS（open 计算/重算幂等/targeted 演进/closed 恒定/二次闭合拒/清单过滤）。
- **派生决策纪律**：缺口面只作数据与消费面（planner/bizmodeler 读缺口自主派生意图——LLM 提议、确定性层闸门的既有纪律），平台不做自动派生（防意图洪水）。
- **对账覆盖面**（运行抓出并修正）：调度器 60s 重扫轮对全部链接了资产的活动任务做缺口对账——初版只扫有 open 意图的探索，而无意图任务恰是缺口主体，实测修正为独立任务集（ListTasksWithLinkedAssets）。
- **REST**：GET /api/tasks/{id}/gaps + POST /api/tasks/{id}/gaps/{gid}/close（理由必填写审计链 source=coverage-gap）；实测 400 校验/200 落账/二次闭合拒/审计落账。
- **metrics/loop pending_items 清零**：F2（0.14.1 arbiter）与 F4（本版）落地，待办清单空——仪表盘指标行不再显示待落地项。
- **前端**：biz-tab 覆盖缺口块（未闭合计数 + 清单 + 人工闭合入口，理由必填）；api/types 接线。
- 验证：build/vet/真库测试/前端三件全绿；实机——重扫自动对账产出缺口（task×asset open）→ 闭合 → 审计链 seq13 coverage-gap ✓；夹具测后即清。
- 对齐检查：缺口块=biz-tab + REST；metrics/loop 仪表盘行为变化如实记录（pending_items 恒空）。



### 新增（v0.14 批次二下：仲裁闭环收尾——状态联动 + 周期重扫 + 前端冲突块；实机全链验证）
- **意图状态联动**：仲裁落笔压制败方（open → blocked + blocked_reason=arbiter_deferred，只动 open 态不回溯终态）；reopen 反向释放（只解 arbiter_deferred）。**实机全链验证**：verify×exploit 意图对 → 检测登记 → host-rule 直裁 → 败方 blocked=arbiter_deferred ✓；reopen → 两方回 open ✓。
- **周期重扫统一三条派生入路**（B）：fireConflictArbitration 60s 全局水位对全部持有 open 意图的探索重跑检测——planner（agent 包不能挂 server 钩子）/edge_dispatch/bizmodeler 三线统一覆盖，写入点挂点（propose_biz_intent）只做即时加速。运行抓出并修复两个 bug：①重扫水位首见空键永不触发（else 挂错层）；②规则直裁分支缺败方压制联动。
- **重扫堆积修复**：RecordConflict 同对已有非 closed 行即幂等跳过——此前每 60s 对已裁决对重复 INSERT（实测 #7/#8/#9 同对堆积）；reopen 是唯一再登记通道（回 open 后幂等跳过、等 arbiter 判定——重开语义=绕过规则直裁）。70s 复测零新行 ✓。
- **前端冲突块**（biz-tab）：冲突对清单（规则/状态/verdict/理由/重开数）+ frozen/adjudicated 行「重开」按钮（行内理由必填，写审计链）——人工覆盖与 frozen 兜底的 UI 入口。
- 验证：go build/vet/真库测试（生命周期回归含堆积幂等）/前端 build+tsc+biome 全绿；实机三链全过（见上）。
- 对齐检查：冲突块=biz-tab（任务详情「业务建模」tab）+ REST 双入口；arbiter agent（Agent 页）/arbiter_resolve（工具页）既有入口复用。



### 新增（v0.14 批次二上：意图冲突仲裁——确定性检测 + arbiter agent 自行调研判定；减少人的参与，人只兜底熔断）
- **数据层**：`db/schema_intent2.sql`（intent_conflicts 一表：归一对 open 唯一 / verdict 枚举 / state 机 open→adjudicated→reopen / open→frozen）+ `db/intent_conflicts.go`（RecordConflict 幂等登记 / ResolveConflict 宿主落笔（枚举+理由校验）/ FreezeConflict / ReopenConflict（reopen_count+1）/ ListConflictsByExploration）+ EnsureIntent2Schema 启动接线。真库集成测试 TestIntentConflictLifecycle 全链 PASS（归一幂等/校验拒/状态机迁移与拒绝路径/清单面）。
- **确定性检测器**（`internal/intent`，代码常量规则带稳定 ID）：三条起步规则——asset_mutex（同资产 verify×exploit 互斥，tie-break 判验证先行=host-rule 直裁）、reverse_predicate（同资产+同漏洞类+对偶词摘要，交 arbiter）、resource_clash（双 mutating 共工具，交 arbiter）。宁漏勿误：无锚/缺预测字段的意图不参与检测；任务资产范围是权威基线（锚点写入已 fail-closed）。单测 5 例（三规则正反例+无锚跳过+归一口径）。
- **arbiter agent**（平台一等 agent，chat_visible=false 不进选择器）：种子链全套——prompts/arbiter.md（调研纪律/判定纪律「宁 defer 勿瞎裁」/输出枚举）走 SeedPromptIfEmpty（可编辑可重置）；arbiter_resolve 唯一写工具（宿主落笔，绑定只给 arbiter，roledispatch 钉死允许集=只读调研面+resolve）；serial 单轮 15min。**红线不动**：候选集是已过闸的合法意图，只决定谁先做，不触碰闸门层。
- **调度唤醒**：fireConflictArbitration 每 tick 扫 open 冲突按水位（60s/冲突）唤醒 arbiter 调研判定（与 fireEdgeWake 同款水位语义）；defer_both 连续 3 次→FreezeConflict 冻结等人（唯一进人路径，UI 重开/人工覆盖在 0.14.2 接前端块）。
- **派生挂点**：propose_biz_intent 派生成功后异步 scanAndRecordConflicts（规则直裁落账 host-rule + 审计；检测失败不影响派生）。planner/edge_dispatch 线挂点与意图状态联动（被压制方 blocked）+ 前端冲突块排 0.14.2。
- **REST**：GET /api/intents/conflicts?task_id、POST .../{id}/reopen（理由必填写审计链）；实测 404/校验。
- 验证：build/vet 全绿；新增测试全过；实机——intent_conflicts 表建好、arbiter agent 落库（chat_visible=false）、arbiter_resolve 播种绑定、REST 就绪。
- 对齐检查：冲突清单/重开的前端块在 0.14.2（下一版）；arbiter agent 在 Agent 页可见可编辑（提示词可改可重置）；arbiter_resolve 工具页可见。



### 新增（v0.14 批次一：调度可观测性与测试基建——自主循环的质量必须可观测）
- **调度器可注入时钟**：Scheduler 增 `now func() time.Time`（生产 time.Now，测试 fake 控制水位/熔断推进节奏，不再靠真实等待）。
- **fireEdgeWake 决策核心抽纯函数**（A1）：`decideEdgeWake(hasCells, doctrineReady, inWatermark, stallAdvances, stallRounds)` ——doctrine > 水位 skip > 熔断 > fire 四路径与优先级钉死；熔断要求 stallAdvances=true（本对账轮哈希与上轮相同——重构时抓出并修正了一个语义缺口：纯计数判断会在判据刚动过的轮误熔断）。`TestDecideEdgeWake` 8 例。
- **调度面过程计数器 + GET /api/metrics**（A2）：Prometheus 文本格式——tick_total / edge_wake_fired / watermark_skips / circuit_breaks / doctrine_concludes / task_missing 六计数器（进程内原子，重启归零为已知口径边界）；挂 requireAuth 之内。停滞熔断死代码（0.13.2 修复）当时零测试零指标——此类回归从此先被测试和指标抓住。
- **core_test 目标播种窗口加固**：3s→10s——LLM 拆解超时回退链路在套件满载下实测可超 3s（三度假失败均为同因，单跑恒 PASS、CI 同码全绿）。
- 验证：build/vet/新增测试（decide 8 例+metrics 渲染+stallAdvance+WebUI 矩阵回归）全绿；/api/metrics 实机验证见运行核查。
- 对齐检查：GET /api/metrics 为行为面（脚本/curl 消费，Prometheus 抓取口径），无独立 UI（入盘点表）；仪表盘既有 loop 行不受影响。

## [0.13.7] - 2026-11-14

### 修复（外部审查批 P1——导航一致性与死代码清除）
- **F7 高亮歧义修复**：「业务建模」与「偏锋探索」共享 /function/business-graph，此前按 pathname 判定两者同时高亮。现 NavMain 记录最近点击的入口 id（点击 Link 时 setState），同 URL 组只亮点击项；刚进入（未点击，如刷新/直达）只亮同 URL 组的第一个（业务建模）。URL 不动——维持 0.12.3 去查询参数的裁定。
- **F11 删除 proxy.ts**：静态导出（output:export）从不运行 Next middleware，该文件是死代码且误导维护者以为服务端有鉴权；全库零 import 核实后删除，layout.tsx 注释同步更新（客户端守卫=唯一鉴权入口成文）。
- 验证：npm build:static + tsc + biome 全绿；构建产物回验（chunk 含高亮消歧逻辑）。
- 对齐检查：无新功能点；sidebar 高亮为既有 UI 行为修正。

## [0.13.6] - 2026-11-14

### 修复（外部审查批 P0 服务端半边——URL 形态统一 + B7 尾巴清理）
- **尾斜杠直接 200 serve，不再 308**（审查 P0①/B1）：`webui_embed.go` 的 serve 逻辑抽到无 build-tag 的 `server/webui_serve.go`（`serveWebUIFromFS(fsys)`——embedui 构建与 CI 测试共用同一实现），归一化改为：`path.Clean` 后先查 legacy（原始路径与 Clean 后路径都查——`/pianfeng/` 带尾斜杠的收藏也断链成功），然后 **serve `<path>/index.html` 直接 200**。此前「尾斜杠 308 到无尾斜杠」与 next.config `trailingSlash:true` 方向相反，prefetch/软导航取 RSC 分片遇 308 不跟随即导航挂起（0.12.3 排查记录的"浏览器侧挂起"与此吻合）；serve 优于 redirect：零跳、无永久缓存语义。规范 URL 仍是干净路径，HTML no-cache 无缓存键漂移。
- **路径矩阵单测**（`server/webui_serve_test.go`，fstest.MapFS 注入导出产物）：干净路径/尾斜杠/双斜杠/根路径/_next 资源/SPA 兜底六类 200 语义 + legacy 三形态 308（含 query 保留）+ HTML no-cache 缓存语义，11 用例钉死——CI（无 embedui tag）全量覆盖。
- **bizDiffsHandler 冗余回填清除**（B7 尾巴）：company=0 时「从任务行补 company_id」的回填查询已由 0.13.2 的 ListBizDiffs `WHERE TRUE` 分支取代，删除冗余 `QueryRow`。
- 验证：go build 双形态（含 -tags embedui）/vet/矩阵测试全绿；换装实测见运行核查（尾斜杠 200、legacy 308 链不断）。
- 对齐检查：无新前端功能点；URL 语义变化对前端透明（fetch 一律干净路径，尾斜杠行为仅影响外部书签/软导航）。



### 修复（外部审查批 P0 前端半边——渲染健壮性；审查报告基于 0.13.1，逐项核实后施工）
- **error boundary 落地**（审查 P0②）：新增 `app/error.tsx`（路由段级，重试/返回仪表盘）与 `app/global-error.tsx`（根级，自带 html/body 内联样式）——此前客户端渲染抛错即整树卸载纯白屏。
- **前端错误上报管道**（审查 P0③ 配套 + 6.7 错误上报延伸）：新 `POST /api/logs/client`（server/client_log_api.go）——走既有 logsink（日志页/SSE 自动带出，tag=webclient），每 IP 令牌桶限流 60/min 突发 10，字段硬截断，payload 上限 8KB；前端 `lib/client-error-reporter.ts`（同 message 60s 去重 + 会话上限 20 条，上报永不比业务错误更吵）+ `app/client-error-boot.tsx` 挂 window.onerror/unhandledrejection。
- **removeConsole 保留 error/warn**（审查 P0③）：next.config 改 `{ exclude: ["error", "warn"] }`——此前 prod 整批删除，渲染出错时控制台一片空白无从排查。
- **business-graph 渲染防御**（审查 P0 F1/F2）：TYPE_META/TYPE_ICON 全部经 nodeMeta/nodeIcon 兜底（枚举外 node_type 曾直接崩渲染）；G6 动态 import 补 catch（大 chunk 加载失败曾静默不初始化）——失败态画布内提示 + 重试按钮（graphEpoch 代数重建）+ 上报。
- **F4 空公司指引**：公司清单为空时给出明确去向（"先在资产页创建公司并配置范围"），不再是静默空图。
- 验证：go build/vet + 前端 build:static/tsc/biome 全绿；产物 chunk 回验；人为抛错实测 boundary 兜底与上报落账见运行核查。
- 对齐检查：新增后端功能点 POST /api/logs/client 的前端入口=上报器（行为面，非 UI）；日志页既有面自动消费。

## [0.13.5] - 2026-11-14

### 修复（外部审查批 P0 前端半边——渲染健壮性；标题行因编辑失误被吞，正文即上方紧邻条目，按序列修回）

## [0.13.4] - 2026-11-14

### 修复（补交缺口——0.13.2 声明的前端改动有两个文件漏出提交清单）
- **补交** `web/src/app/(main)/function/miniprogram/page.tsx`（未配置主密钥的持久告警条 + 保存 toast 加密状态区分 + 「凭据值（有主密钥时加密落库）」标签修正）与 `web/src/app/(main)/system/settings/page.tsx`（「加密主密钥」状态卡 + masterKeyConfigured 加载）。两文件的变更在 0.13.2 的 CHANGELOG 已声明、服务与构建产物一直包含（工作区代码在跑），但 0.13.2/0.13.3 两次提交的暂存清单均漏了它们——终核复查抓出，本提交补齐。运行行为无变化（代码与已运行版本一致），补的是 git 完整性。
- `artex.log`（start.sh 守护的运行时日志）加入 .gitignore。
- 对齐检查：无新功能点；diff 内容与 0.13.2 声明逐条对应（告警条/toast/标签/状态卡）。

## [0.13.3] - 2026-11-14

### 新增（前端对齐批——审查报告的 BE→FE 缺口按分类处置；补 UI 或入盘点表）
- **记忆治理页**（/system/memory + sidebar「记忆治理」）：X4+X2 宿主侧最小治理面——分页列表（active/quarantined 筛选）+ 晋级/退役按钮；操作人与理由必填（后端强制，写审计哈希链 source=memory-curation）；promote 被三门评估拒绝时如实展示确定性缺口清单（409）。
- **发现页存量重评入口**（E1-7）：两段式——「存量重评」先 dry-run 列候选（未落库），确认后「应用改判（N 条）」才写库；应用后刷新列表与统计。
- **大厅群详情状态控制**：StatusBadge 旁「状态」按钮展开七态选择 + 变更理由（状态机=代码常量，理由必填），应用即 PUT /api/task-hall/groups/{id}/status 并刷新。
- **LLM 页探索累计总量**：头部显示全探索聚合的输入/输出 token 总量（W9 /llm/totals）。
- **仪表盘调度循环指标行**：任务卡头部显示意图 done/blocked/exhausted、空转率与 pending_items 待落地项（M3 /metrics/loop，15s 慢轮询面）——F2/F4 待落地项从此在仪表盘可见。
- **Mimosa L3 加固**：theme 生成脚本路径内敛校验（readdirSync 产物再入 join 前钉住解析结果落 presetDir 内）+ chatUpload id 白名单（与服务端 safeChatID 同口径，本地先手校验）+ 查询串 URLSearchParams 化——消除 L3 高危拦截，剩 4 个跨文件污点 medium（上传目标 id 源自服务端响应属正常数据流，见提交说明）。
- **docs/前端对齐盘点.md（新）**：全量基线快照——有入口的功能面全清单、行为面标注（无独立 UI 的理由逐条写明）、已知排期项（sessions 会话面/策略库三面/窗口级精控等 6 项必须排批次）。此前审查发现的 21 条 BE→FE 缺口全部处置完毕：5 个补了 UI（memory/rerate/status/totals/loop），其余按行为面或排期项入表。
- 验证：npm build:static + tsc + biome 全绿；构建产物回验（记忆治理页 chunk 存在）。
- 对齐检查：本轮新增前端功能点（记忆治理页/重评入口/状态控制/总量行/指标行）与后端端点一一对应；21 条缺口处置结果全部落盘点表。

## [0.13.2] - 2026-11-14

### 修复（全量审查批——P1 熔断死代码 + 两个 P2 + 三个 P3；实测驱动）
- **P1 偏锋停滞熔断是死代码（实测坐实并修复）**：fireEdgeWake 正常路径推进 StallRounds 后从不落库（只有熔断分支才 save）——loadEdgeBreaker 每次读回空 lastHash，stallAdvance 归零，StallRounds 永远到不了阈值 4，判据停滞熔断（EDGE E7 三熔断之一）永不触发。审查期经 API 建任务+插入 open edge_run 实测 35 秒（7 个 tick）不熔断坐实。**修复**：停滞对账移入水位 gate 之后（每 60s 推进一次 ≈ 一轮对账语义，与 StallTracker 的「轮」对齐）且推进即落库；判据成立机械收尾（主路径）保持在水位前每 tick 判定不被延迟。同时钉住反向风险：若只补 save 不改节奏，5s tick 会变成 20s 误熔断。新增 TestStallAdvance 钉死计数语义；实机验证：夹具任务 4 个对账周期后 criterion_stalled 熔断 + 审计落账。
- **P2 差异按任务维度查询对未归属公司任务恒空（实测坐实并修复）**：bizDiffsHandler 的 company_id 回退对 company_id NULL 的任务补出 0，ListBizDiffs `WHERE company_id=0` 恒空——差异面板按任务维度查不到该类差异（diff 行确凿在库，`?task_id=2` 空、`?company_id=2&task_id=2` 命中）。**修复**：ListBizDiffs 在 company=0 且 task>0 时跳过公司过滤按任务维度查全库；双零调用显式拒绝（补全表扫口子）。真库集成测试 TestListBizDiffsNullCompany 钉死。
- **P2 token 加密告知**：无主密钥时凭据明文落 token_plain 过渡列（既有文档化降级），但操作者无感知。现：db 层返回加密状态；REST 保存响应带 `encrypted` 字段；mp_token_save 工具响应如实报加密状态；GET /api/miniprogram/encryption 暴露主密钥配置状态；小程序页未配置主密钥时挂持久告警条 + 保存 toast 区分两种状态；系统设置页新增「加密主密钥」状态卡。
- **P3 host-targets 校验任务存在**：此前对不存在任务也 200 并写审计链（审计留孤儿事件，实测坐实）；现先查任务行（含 deleted_at 过滤），不存在 404。
- **P3 ScopeSentry 构造失败 502→503**：数据源不存在/未配置 URL 是依赖面缺口，503 才是「没配置」的语义（此前 502 伪装成「上游坏了」）；上游调用失败保持 502。
- **P3 死代码清除**：miniprogram_api.go ensureMpArtifactsDir 的 `_ = strings.TrimSpace("")` 遗留行删除。
- 验证：go build/vet 全绿；TestStallAdvance 过；实机观测见上；全量 go test（CI 同款环境）后跑。
- 对齐检查：新增前端功能点——加密告警条（小程序页）、加密主密钥卡（设置页）、GET /api/miniprogram/encryption（小程序页+设置页双入口）。

## [0.13.1] - 2026-11-14

### 修剪（前端小动作：砍标语/提示文字/次要标签——目标·架构·设计·实现全部从简）
- 五个自有界面（业务建模图 / 差异面板 / 偏锋总览 / 小程序渗透 / 任务建模 tab）的界面文案修剪：长引导句、教程式空态、次要徽章（severity 描边说明等）、toast 后缀说明全部砍掉，空态只留一句事实（"暂无业务图数据"）；数据徽章（指纹/标签/severity）与全部功能按钮、tab、链接保留。
- **零逻辑改动**：纯 JSX 文本删减——构建/tsc/biome 全绿即证明功能未动。
- **回滚**：单提交 `git revert <本提交>` 即整体回到 0.13.0 文案形态；SKILL.md 的 agent 指引文本未动（AX 保持）。
- 对齐检查：纯文本修剪无新功能点；产物 chunk 回验（verbose 串已不在构建产物中）。

## [0.13.0] - 2026-11-14

### 新增（小程序渗透增强批次——专用 agent + 内嵌浏览器 + chrome-devtools 注册；按用户需求）
- **专用小程序渗透 agent**（builtin，`miniprogram`，单次 run 不限时）：六阶段专用提示词（阶段→kind 映射、两层身份矩阵、包获取人工口子、证据纪律）注册进 prompts_files；wechat-miniprogram skill 与 mp_token 工具绑定补上 miniprogram agent；桌面控制经 desktop-ops 的全量绑定天然可用（驱动微信 PC 客户端）。
- **chrome-devtools MCP 种子**：LookPath 解析绝对路径（未安装跳过不落旗标）；可见性=全部 agent——接口发现阶段的浏览器操控入口。
- **内嵌浏览器（小程序运行时反代）**：`/mp-runtime/` 反代到 wxa-runner dev server（settings `miniprogram_runtime_url`，操作者经 REST `PUT /api/miniprogram/runtime` 设置，**agent 不可改**）——小程序渗透页新增「内嵌浏览器」面板（同源 iframe，直接点击调试页面）；SSRF 面关闭：scheme 仅 http/https、单固定目标（路径转发 host 恒等配置值）、目标为部署配置非 agent 可控输入。iframe 公共路由说明：代理的是被测应用自身 dev server（与直接访问运行时等价），artex JWT 不适用于 iframe 请求头。
- 前端：小程序页「内嵌浏览器」面板（地址配置 + iframe + 未配置引导）；api/types 接线。
- 验证：build/vet/tsc/biome 全绿；实跑 0.13.0。
- 对齐检查：miniprogram agent（对话选择器）、chrome-devtools（MCP 页）、内嵌浏览器面板（小程序页）、mp 工具（工具页）——后端功能点全部有前端入口。

## [0.12.3] - 2026-11-14

### 修复（偏锋探索入口在用户设备反复"无法载入"——移除查询参数变量）
- sidebar「偏锋探索」入口改为 `/function/business-graph`（不带 `?mode=edge`）：与用户实测可打开的业务建模页**完全同一网络路径**，模式由页内「业务图 / 偏锋探索」标签切换（query 参数机制保留作兼容，不再承担入口职责）。
- 排查记录：服务端 HTML+33 子资源并行全 200（毫秒级）；headless Chromium 导航挂起可复现（含此前正常的 dashboard）——挂起在浏览器侧加载序列（JS/连接层），非 HTTP 层。入口去查询参数移除最后一个 URL 级变量；若业务建模页本身也打不开，问题在设备侧网络（代理/缓存），需无痕窗口或核查代理对该内网地址的规则。

## [0.12.2] - 2026-11-14

### 修复（前后端对齐审计——0.12.0 的 sidebar 补丁静默失配 + skill 绑定缺口）
- **sidebar 补「小程序渗透」入口**：0.12.0 的补丁因锚串与文件实际内容失配而静默未打上（脚本无断言）——本轮修复并立规矩：**前端补丁必须带断言 + 构建产物必须回验**（本次构建产物 chunk 中 grep 到「小程序渗透」才算过）。
- **skill 绑定种子**（更严重的功能缺口）：agent_skill_visibility 此前只有 api-recon→3 agent——desktop-ops 与 wechat-miniprogram 两个 skill 没绑任何 agent ⇒ **没有任何 agent 能解锁这两个 skill**（桌面动作面/小程序链路不可用）。builtinSkillVisibility 补两行：desktop-ops→全部 agent（"*" 通配，roledispatch 钉死的纪律 agent 由白名单继续兜）；wechat-miniprogram→auto/pentest/worker/mainagent。
- 对齐审计全量重过（0.11.9-0.12.1 后端功能点 vs 前端入口）：桌面控制（MCP 页/拦截页/审批页/工具页 + biz-tab 授权编辑）✓；小程序渗透（sidebar+页+token 工具+REST）✓（本轮修复）；偏锋/业务建模既有入口 ✓。

## [0.12.1] - 2026-11-14

### 新增（桌面控制 A4——闸门第五分支 HostLocalAction，任务路径桌面动作解锁）
- **闸门第五分支**：`GateInput.HostLocalAction`——桌面控制面动作（`db.IsDesktopToolName`，解包后的内层名）且任务宿主授权集合（tasks.host_targets）非空时，scope 按构造通过（`pass(host_local_action)`），其余四道检查照跑。集合未配置的桌面动作在规整之前以**独立理由**拒绝（与「无可判定目标」区分）。
- **跳过意图规整**（照 fastpath 纪律：豁免规整不豁免裁决）：桌面载荷只有 ref，规整器产不出目标线索——编造目标反而制造不稳定放行；审计带 bypassed_normalizer + bypass_reason=host_local_action。
- **tasks.host_targets 列**（D9）+ db SetTaskHostTargets/TaskHostTargets + REST `PUT /api/tasks/{id}/host-targets`（写审计链）+ 任务详情「业务建模」tab 授权清单编辑器。
- **解包一致性钉**：sod 的 deferredInnerName 与 guard 侧 intercept.UnwrapDeferredName 同载荷同产出（双包各持实现，行为漂移在此被钉死）；desktop_gate_test 三钉（分支通过/未授权区分/前缀唯一语义源）+ db 往返集成测试。
- 语义边界（如实）：授权粒度是任务级粗粒度 + guard 层逐动作人工审批的组合——ref→窗口映射在 agent-desktop 会话态里，闸门解析不到；窗口级精控排后续。
- 验证：build/vet 全绿；sod 包测试全过（含 0.11.9 既有链测试回归）；三钉 + db 往返真库通过；实跑 0.12.1。
- 对齐检查：无新前端路由；授权清单编辑落在既有任务详情 tab。

## [0.12.0] - 2026-11-14

### 新增（小程序渗透功能点，施工方案 B0-B4；包获取人工口子按 D7）
- **token 池**（§四-B3）：`db/schema_miniprogram.sql`（miniprogram_tokens，UNIQUE(task,app,label)）+ `EnsureMiniprogramSchema` 启动接线 + db/miniprogram_tokens.go（加密落库照 llm_profiles 的 MasterKey 口径，AAD 绑 app:label——换库挪用解密失败）；transcript/审计面只有标签与指纹，明文仅在 reveal 时刻进工具结果（构造 Burp replacement）。
- **作用域工具**：mp_token_save / mp_token_list / mp_token_get(reveal)（server 侧参数化，task_id 必填；保存与 reveal 走审计链，凭据值不进审计载荷）；绑定 pentest/worker/auto/mainagent（任务路径与会话驱动两线）。
- **REST**：GET/POST /api/miniprogram/tokens、DELETE .../{id}、GET /api/miniprogram/artifacts（列 artifacts/miniprogram/<appid>/ 产物）；保存走审计链。
- **read_design_artifacts 扩类**（§四-B4）：扫 artifacts/miniprogram/<appid>/ 的 app.json（页面注册树摘要：主包/分包页面数）与 project.config.json（编译开关核对）；建模 agent 可直接消费解包源码做设计层。
- **skills/wechat-miniprogram**：SKILL.md（阶段→kind 映射、两层身份矩阵、包获取人工口子、桌面控制调用口径、证据纪律）+ 五脚本（probe_unpackers 探针 / discover_package 版本分支发现 / unpack.sh 适配器（后端可换）+ fix_miniprogram_config.py（五关两开+componentFramework/plugins/pages 矫正）+ validate_unpack.py（内容级三判据，退出码即判定））。
- **前端**：/function/miniprogram 页（token 池 CRUD + 产物浏览；**Safari 实测为交付验收首项，失败降级 tab 模式**——pianfeng 教训）+ sidebar「小程序渗透」入口。
- 验证：build/vet/tsc/biome 全绿；脚本三件实测行为正确（探针诚实报告本机未装解包后端=人工口子的一部分；fix/validate 对无产物目录按退出码 1+失败清单报告）；实跑 0.12.0（schema 落库、工具播种、REST 就绪）。
- 对齐检查：后端功能点（token 池 REST+工具、产物 API、read_design_artifacts 扩类）前端入口齐备；桌面控制为共有能力，小程序经「桌面操作」skill 调用（B-5 口径）。

## [0.11.9] - 2026-11-14

### 新增（桌面控制功能点——全 agent 共有，施工方案 A1+A2+A3 阶段）
- **agent-desktop（vercel-labs，读无障碍树 + 稳定 ref）以 stdio MCP 注册**：种子按 LookPath 解析绝对路径（未安装跳过且不落旗标，装好后下次启动补种）、`args=["mcp","--headed"]`（物理输入为注册期决策，D6）、可见性=全部 agent（用户裁定：平台共有能力）。
- **ExecuteExtraTool 转发的治理闭环（平台级）**：延迟工具经外层转发器调用时，guard 按载荷内层【实际执行】的工具名与参数求值（intercept.UnwrapDeferredName）——启用名单、规则匹配、审计与审批记录全部回归真实身份；载荷畸形回退外层名（执行侧同拒，无旁路窗口）。安全钉三条路径单测钉死。
- **桌面工具进拦截系统**（治理三段之一）：启用名单逐项合并（8 默认 + 75 桌面 = 83）；四族规则按命令族正则落库——观测类 allow（12）、动作类 ask（33）、敏感读取类 ask（3：截图/剪贴板/通知）、高危类 deny（16：launch/close-app/剪贴板改写/系统界面/batch 元执行器/sandbox exec·install 等）。分族以 1.3.0 实测的 75 个真实工具名快照钉死覆盖（族间互斥 + 全覆盖 + 正则级互命中验证 + 优先级严格降序），上游加新命令时测试失败提醒分族。
- **敏感输出元信息化**（留存策略 §6.3）：desktop_screenshot / clipboard_get / list_notifications 的 tool_result 正文在持久层（activity.detail）替换为元信息（工具名/字节数/时间），模型上下文不受影响；机制按类别注册可扩展。
- **「桌面操作」skill**（mcps: agent-desktop 门控）：命令族说明、ref 重快照纪律、屏幕文本不可信警示、渗透场景用法（GUI-only 面/客户端越权/微信 PC 客户端小程序窗口）、证据边界如实声明。
- 验证：go build/vet 全绿；intercept 解包钉 + db 分族覆盖钉单测过；实跑 0.11.9：MCP 注册（75 工具发现并缓存）、四族规则与 83 项名单落库、分族与实测全集程序化比对通过。
- 对齐检查：桌面控制为平台共有能力，无新增独立前端路由（配置面用既有 MCP/工具/拦截页）；roledispatch 钉死 agent 的动作面默认剔除（D8'：动作类不进"只提议"纪律），观测类入白名单排阶段二可选。

## [0.11.8] - 2026-11-13

### 重构（偏锋探索功能点 0.11.7 后仍报"无法载入"——按用户指令重构功能点本身）
- **取消独立静态路由**：偏锋总览不再有自己的页面路径（pianfeng 路由删除），**并入业务建模页作为第二种模式**——sidebar「偏锋探索」= `/function/business-graph?mode=edge`，页内顶部「业务图 / 偏锋探索」Tabs 切换。业务建模页是用户实测可正常打开的页面；独立路由是该功能点与其它可达页面唯一的结构差异，重构后该差异不复存在。
- 总览面板抽为共享组件 `components/edge-overview-panel.tsx`（方法论注册表 + 偏锋任务判据进度 + stop 统计），数据走既有 API（`/api/edge/methodologies`、`/api/edge/tasks`——API 层从未失败）。
- 兼容链：`/function/pianfeng` → 308 → `/function/business-graph?mode=edge`；`/function/edge-exploration`（含尾斜杠）→ 308 链式到达同页。收藏不断链。
- 对齐检查：无新后端功能点；偏锋功能的前端入口由"独立页"变为"页内模式"，功能面（总览/方法论/判据进度/stop 统计）一个不少。

## [0.11.7] - 2026-11-13

### 修复（偏锋总览页 Safari"无法载入"——用户二次报障根治）
- **页面路径改名**：`/function/edge-exploration` → `/function/pianfeng`（sidebar 标题不变）。排查结论：服务端 HTML/JS chunk/RSC payload/尾斜杠响应全部 200、字节数精确、响应头干净，同一服务器其它页面正常 ⇒ 唯一残留在用户侧（代理按 URL 关键词分流——"edge" 是常见分流规则词，或 Safari 对失败 URL 的连接级缓存）。服务端釜底抽薪：路径去关键词 + 老路径 308 兼容。
- **静态服务规范 URL 归一**：尾斜杠路径 308 到无尾斜杠（此前目录形态与文件形态返回同一份 index.html，但浏览器缓存键不同）；改名页老路径 308 兼容（收藏/历史不断链，保留 query）。
- 对齐检查：无新后端功能点；sidebar 三入口（业务建模/差异面板/偏锋探索）与后端功能面对照无缺口。

## [0.11.6] - 2026-11-13

### 新增（业务建模 × 偏锋探索 显示与体验对齐）
- **业务建模图画布跟随页面主题**：G6 画布背景/标签/连线配色按 resolvedThemeMode 双套（暗色不再刺眼）；建图 effect 依赖改为主题（数据变化不再重建整图）。
- **工作目录固化**（开发纪律 §2）：`config.json` 支持 `data_dir`（优先级：`-data` flag > `ARTEX_DATA_DIR` env > config `data_dir` > 可执行文件同级 `data/`）；版本更新只换二进制、永不删工作目录；预置 `traffic/`、`artifacts/`（功能点产物根）、`workspace/` 子目录与 `WORKDIR.md` 说明。
- **版本纪律落地**：`VERSION` 文件为版本权威（当前 0.11.6），`build.sh` 优先读它注入 `main.version`；纪律成文 `docs/开发纪律.md`（版本步进/运行后提交/前后端对齐/推送规范/工作目录）。

### 修复（0.11.5 显示缺口复查）
- 偏锋探索总览页 `/function/edge-exploration` 报"无法载入"——排查为服务重启空窗 + 浏览器缓存旧构建 chunk（服务端 HTML/JS chunk/尾斜杠全部 200），随本轮重发布刷新 chunk 后消除；无代码缺陷。

## [0.11.5] - 2026-11-13

### 新增（前后端显示对齐）
- sidebar 三入口：**业务建模**（原业务图改名）、**差异面板**、**偏锋探索**。
- 新页 `/function/edge-exploration`：方法论注册表（判据类型/上限参数/证据等级/启用状态）+ 偏锋任务判据进度与 stop_reason 统计。
- 新 REST：`GET /api/edge/methodologies`、`GET /api/edge/tasks`（db.ListEdgeTasksOverview：edge_mode 任务+判据机械汇总+未收尾运行+stop 分组）；`TestEdgeTasksOverview` 真库集成测试。

## [0.11.4] - 2026-11-13

### 新增（业务建模 × 偏锋探索合并施工，M0-M8 全量落地；验证记录见分支 feature/biz-edge-modeling 提交 96ba5d9）
- 业务建模（第三张图）：biz 五表 + 范围读写 + 八类差异规则表（WSTG/STRIDE 内置映射）+ 快照版本 + 资产 supersede（宿主侧唯一写函数 + 审计链）；bizmodeler 自定义 agent（六段流程，触发器默认关）；八工具（含 propose_biz_intent 的归一去重/积压水位/可证伪预测纪律）；REST 四条 + 前端 G6 三视图与差异面板 + 任务详情「业务建模」tab。
- 偏锋探索：internal/edge 方法论注册表（五类判据+上限参数强制+隐喻不带判据）、两级触发词闸门（本轮注入+工具前置+审计落账）、edge/edge_exec 双 agent（执行体 chat_visible=false）、八工具（绑定矩阵：edge_run 不绑 mainagent）、scheduler fireEdgeWake（轮间接力+判据停滞熔断+判据成立机械收尾）、planner 态势追加 provider、worker「偏锋探索」纪律段、edge-coverage REST（stop_reason 统计=判据触发率口径）。
- 基础设施：`agents.chat_visible` 列（选择器可见性，与 builtin/role 正交）；`tasks.methodology_id/edge_mode`；`assets.superseded_by`（宿主侧唯一写路径）；独立 DDL 文件 `db/schema_biz.sql`/`db/schema_edge.sql` 走 Ensure*Schema 启动接线。
- 验证：真库集成测试（TestBizEdgeIntegration）、db 全包、server 两轮回归（失败集合=基线既有）、HTTP 全链路（agents/工具绑定/范围读 fail-closed/差异裁决→审计链 intact）。运行抓出并修复：schema_biz 缺 ON CONFLICT 唯一索引（42P10）、bizDiffsHandler 只传 task_id 时 company_id=0 恒空。

## [0.11.3] - 2026-11-13

### 修复（v0.11.2 遗留两项收尾）
- **会话轮硬顶 15min**（AbortChatTurnTimeout）：LLM 流式连接挂死（无数据也不断开）会让 turn 永久占用 busyKey——群唤醒/人工对话全部静默失效（复测实证 conv 卡 running 240s+ 无输出无失败日志）。人工 stop 仍可提前收束。
- **规整输入按 schema 矫正标量类型**：只动 schema 声明的 boolean/number 位置的字符串值（实测 MiniMax "has_background":"false" ⇒ normalizer_malformed ⇒ 意图 blocked）；string 字段与数组内容绝不被启发式误伤（单测钉死）。矫正后仍失败才拒绝。

## [0.11.2] - 2026-11-13

### 修复（群聊发命令无人认领——用户报障根治）
- **大厅群消息入库即驱动行动**：此前消息只躺信箱、零唤醒（无人认领的根因之一）。现在定向命令唤醒对应成员会话、广播唤醒 auto 分发者，各跑一轮；回复镜像回群信箱（大厅消息流可见）。busy 冲突静默放弃（消息在信箱不丢）。
- **群成员会话的任务身份注入**（根因之二）：server 侧此前从不注入 RunInfo ⇒ 成员被唤醒也反查不到群（收信为空、探索图工具不可用）。现按 conv→群成员→群→任务自动注入；独立聊天不受影响。
- **建群自动开通默认群预算 500k/日**（根因之三）：group-quota 闸在未配置预算时拒群上一切 LLM 调用（planner/成员唤醒全被 [[group-quota]] 拒）——建群即哑巴。已有群在配额页手工配置。
- **bg_task 启动期 DDL 接线补缺**（memo 名单有而启动序列漏接的第三例）：主控后台命令在受限角色下 42P01。
- 附带：唤醒用现有 runConversationSync 通道（busyKey 与 stop/状态页口径一致）；wakeHallAgent 入口日志。
- 已知开放项（v0.11.3 候选）：① auto 的 chat 轮在群会话上偶发挂起无响应（conv15 实测 240s+ 无输出无失败日志——chat lane 的 LLM 挂起无超时）；② 规整器对 MiniMax 输出容错不足（structured_intent.shell_features.has_background 类型错 → normalizer_malformed → 意图 blocked）。

## [0.11.1] - 2026-11-13

### 修复（P0：幻觉收口缺口）
- **planner 不再能登记漏洞**：planner 工具面移除 report_finding——LLM 可在未执行任何工具的情况下凭空登记编造的漏洞并宣布完成（复测实证：20 秒假完成）。规划者的发现一律走 add_intent / 交 worker 验证登记。
- **planner 补 findingValidator 接线**（防御双保险）。
- **goalless 零产出守卫**：直投任务抽干但探索图无任何 fact/finding 时不落 done，转自动暂停等人确认。
- 附带：绑定快照同步；db 新增 CountKindsUnscoped（不经 scope 过滤的客观产出计数，排除 origin 根）。

## [0.11.0] - 2026-11-12

跨过 v0.9.16 → v0.10.x 的一个大版本：治理面收口 + 现场事故根治 + 两个新功能面（报告中心/消息化）。**升级无破坏性**：schema 全部幂等、启动期自动应用；`llm_pool_enabled` 默认翻开（行为变化见下）。

### 新增
- **报告中心**（/system/reports，独立侧边栏）：最终报告统一管理——正文整篇存库、列表/新建/点击展示/**实时编辑**、下载导出 .md（与工作空间下载同体验）、**从工作空间导入**（扫描 *.md/*.txt/*.html 点选入库，原文件不动）。
- **message_group 工具**（Tier2）：worker 可向任务大厅群信箱投递进展/请求决策；出口过消毒链，准入走存储层拦截规则。
- **存量漏洞重评**：`POST /api/exploration/findings/rerate`——确定性出候选（乱值→info；纯信息泄露无凭据链→info），默认 dry-run，apply 才落库。
- **大厅语义化状态**进群详情 API（hall_status：人话档位+卡住成员），总览页渲染。
- **拦截详情**：任务规则 API 单列 command_rules；前端命令级行「证据保全·不可改」。
- **initdb.d 兜底**：全新库冷启动自动创建受限角色 artex_app（口令与 pg_dsn secret 同源）。
- **web_search 出网判定**装进请求前钩子（范围判定+留痕）。

### 修复（现场事故）
- 裁决大屏恒 0 条：feed SQL 引用不存在列 + 吞错（改 fail-closed）。
- 拼装 API panic（经验空集越界）。
- **竖向放权死路径**：审批 pending 行与「同类放行」凭据改按 SoD 口径键落库，与执行器查表逐字节同源——「同类放行」从此真的生效。
- 意图 blocked 不阻断 done 的假完成路径（额度耗尽自动暂停人工恢复）。
- validate_finding 复核层接线补缺；playwright-cli 种子改绝对路径。
- 主控调 validate_finding「复核层未接线」；pending 查询 jsonb 转型错。

### 变更（治理/行为）
- **llm_pool_enabled 默认开**：多 profile 故障/额度耗尽自动落链（单 profile 等价直连）；绑定回退仍默认关。
- **评级基准显式化**：纯信息泄露/无凭据暴露/指纹/未证实候选面一律 info；基准注入 worker/planner 提示词（唯一语义源）。
- **E1-4 判定精确化**：范围内出网 → Tier2·egress_in_scope（未判定/越界维持 Tier3）。
- **E1-8 实况化**：生效 hook 链读 sod_categories 实表；跳转指向真实配置页。
- **E1-2/E1-3**：决策账本页人工改判徽章+「最近人工改判」卡；DTO 对齐真实列名（anomaly_reason），截断 120→400。
- **T7 消毒链补出口**：record_fact 三字段与 attach_assets source_summary 过消毒（现共 4 出口）。
- **三门以人为本**：复盘/证据门可带理由越级；**额度耗尽任务自动暂停**（人工恢复）。
- **系统事件消息化**：worker=system 文本整条消息渲染（不再折叠）；需要人工处理的事件内联操作（重跑/恢复）。
- **蒸馏容错解析**：三态解析（直通→修复→逐对象打捞）。
- **UI**：侧边栏紧凑化（240px/行高 30px）；/system/mcp 卡片紧凑化；裁决大屏窄屏并排。

### 安全
- **推送闸门**：push 模式补扫祖先历史 blob（漏检修复）；误报修复（裸词）。
- 历史：本地开发线历史在本仓库发布时即净（0 泄漏可达对象）；台账 docs/ 保持本地不入库。

## [0.4.6] - 2026-11-03

会话实体化、记忆层、产出级校验的一批落地。**功能面扩张**，执行模型（意图/工种模型）、
对外 API 形态与部署形态保持上一版原样。

### 会话

#### 新增的功能

- **会话成为一等对象**：新增 `sessions` / `session_members` / `session_attachments` /
  `session_messages` 四张表（`db/schema_session.sql`）+ 访问层 + HTTP 接口 + 前端会话化界面。
  兼容做法是**不做破坏性迁移**：`conversations` 不动结构，只加一列 `session_id`；现有会话每条
  自成一个 session 并回填一行成员；`chat_groups` 保留为 `kind='task_hall'` 的 session。
- **会话列表默认不再按 agent 分组**：默认分组轴改为「置顶 / 今天 / 更早」；「按 agent 分组」
  降级为**可选视图且默认关闭**，agent 从分组轴变成**筛选维度**。旧对话列表仍可切回旧视图。
- **命名三轨**：用户显式名 > 生成标题 > 默认显示名（`目录名-2位后缀`）。生成标题**永不覆盖**
  用户显式名；自动命名从「首行截 40 字符」升级为「模型摘要 + 首行兜底」。
- **成员花名册与在场状态**：`session_members` 记 `role`（dispatcher / member / observer）与
  `presence`（idle / listening / working / blocked / left），支持会话中热加入 / 退出。
- **消息与收据条**：消息 `kind` 分 note / request / answer / finding / escalation / receipt；
  收据条（`receipt_of` + `receipt_action`：已接收 / 已记下 / 已推迟）贴在原消息下、**不占气泡**。
- **会话 ↔ 工件双向关联**：`session_attachments` 支持 task / task_hall / strategy / asset /
  finding / pr / external，并可由工件反查会话。

### 发现

#### 新增的功能

- **finding 级验证态 + 返工边**：finding 节点的三态收敛为 `pending` / `confirmed` /
  `dismissed`；新增 `rework` 边（finding → 产出它的那条 intent）。
- **裁决归确定性层**：提出人不得自验（`ErrSoDReviewSelfCertification`）、已结案不得翻案、
  `dismissed → rework` 必须给理由。
- **`replay_evidence_id` 落库**：该列早已存在但**全仓库无写者**，永远为空 ⇒ SoD 的 tier2 检查
  只能判它不一致；现在记录路径会写入。

### 记忆

#### 新增的功能

- **追加式记忆层（方案 B）**：新增 `memory_index` / `memory_topic` / `memory_write_log`
  （`db/schema_memory.sql`）。索引 **append-only + 版本化**（库层证明不覆盖）、带作用域、
  可导出为纯文本（可查看 / 可编辑 / 可禁用）、可退役。
- **dream 只提交 diff**：不整体重写索引 —— 记忆也是摘要，用更好的摘要去修一个摘要往往不работает。
- **不做 per-agent 个人记忆层**：记忆归会话 / 任务大厅所有；`written_by` 只作审计，不是归属。
  per-agent 记忆 = N 份发散副本 + 写冲突。

### Agent

#### 新增的功能

- **任务级进度回执**：两个工具 `record_progress`（写）与 `list_progress_receipts`（读），
  外加运行结束时的确定性回执。回执落在 `activity.kind='progress'`（自由文本列 ⇒ **不需要新
  DDL**）。**阻塞不走回执**，走 `intercept_pending` 那条路 —— 阻塞是状态机，归确定性层。
- **产出级复核（对抗终止从工具层搬到产出层）**：新增 `validate_finding` 工具，它**只把裁决送进
  确定性层**，裁决本身由 db 做；复核器用**注入回调**（零编译期耦合）。
- **压缩后提醒模型写进度**：`patches/norma-compact-progress.patch` 让压缩后的 preamble 点名
  真实工具键 `record_progress` —— vendored SDK 是生成树，只能走补丁。
- **Team 值得性闸**：`agent/team.go` 的 `JudgeTeamWorthiness` 是纯函数（不查库 / 不调模型 /
  不出网 / 不读可变全局状态），输入全部是图上事实。**不拆硬底线** —— 闸只决定并行形态
  （不值得时 `MaxParallel=1`），不决定要不要干活。

### 出网治理

#### 修复的问题

- **范围校验从未真正接线**：`traffic.SetEgressScope` 在整个仓库里只被 traffic 包的测试调用过，
  生产路径上范围判定恒为「未注入」⇒ worker 的出网一直是全拒，那套范围校验形同虚设。
  `server/manager.go` 现在在 `traffic.Open` 成功后注入 `DBEgressScope`。**这只放宽不收紧**：
  范围判定不可用时仍然拒绝（fail-closed 不变）。
- **三类角色此前不带任务身份**：planner / 主 agent / 对话 agent 的出网不受任务范围校验
  （只有 worker 带令牌）。现在统一走 `agent/egressTaskID`：显式 > 0 ⇒ 用它；否则回落 ctx 的
  `RunInfo.TaskID`；都没有 ⇒ 0 ⇒ **不伪造身份**。
- **web_fetch 也没带身份**：新增 `agent/egressProxyURL`（把令牌放进代理 URL 的 userinfo），
  四类角色的 `WebFetchProxy` 全部改用它。

#### 新增的功能

- **web_search 范围豁免清单**：搜索走自己的出口代理，不经录制代理也不带任务身份 ⇒ 完全不受
  任务范围校验。治理办法不是让它受范围校验（搜索服务商的域名不在任务资产范围里，会把工具
  直接打死），而是引入**范围豁免清单**（`internal/sodpolicy.EgressScopeExemptHostList`，
  **编译期常量**，不是 DB 行、不是 settings 项）：清单内 ⇒ 平台基础设施、不受任务范围管辖；
  清单外 ⇒ 仍放行（工具不能坏）但按「未归属出网」留痕；**判不出目的地 ⇒ fail-closed**。
- **未覆盖通道清单公开**：`GET /settings/egress-governance` 列出当前未被范围校验覆盖的通道，
  「系统配置」页可见。

### 运行时

#### 新增的功能

- **压缩触发点可提前**：新增 `ARTEX_COMPACT_TRIGGER_RATIO`（0~1）→ `Config.AutoCompactRatio`，
  触发点变为 `min(默认阈值, effectiveWindow × ratio)`。**只能提前、不能推后**（实现里只在
  t < base 时才采用 ⇒ 放松默认策略必须改代码，不可能靠改环境变量悄悄生效）。默认 ratio=0 ⇒
  行为完全不变。
- **前台命令上限 60s**：`AGENT_CORE_TOOL_TIMEOUT_SEC` 落地，钳制在 5s–600s。工具描述与提示词里
  的数字**动态生成** —— 改行为却让模型看到过期数字等于骗模型。
- **总预算不重置钉成回归测试**：复核结论是系统本来就做到了（绝对 deadline + 夹逼剩余时间），
  但此前无测试保护，本次补 6 组用例钉住。
- **web_fetch 直连兜底改为显式开启**：SDK 在代理**连接层**出错时会直连重试一次。改为
  `NORMA_WEBFETCH_DIRECT_FALLBACK=1` 才开启，默认配置下代理失败**必须失败**、不得静默直连。

### 策略

#### 新增的功能

- **节点级策略真外键**：`exploration_nodes.strategy_id` 改为真 FK（`ON DELETE SET NULL`：策略删了
  只丢链接、不删节点），打通探索图与策略图 —— 「哪条策略奏效」从此查得到。

### CI

#### 修复的问题

- **干净克隆里 Go 门禁形同虚设**：`go.mod` 用 `replace ... => ./third_party/norma`，而
  `third_party/` 被 gitignore ⇒ 干净克隆里 `go build` 第一步就报「replacement directory does
  not exist」，**后面的 vet / 测试全被跳过**。`scripts/prepare-norma.sh` 现在在模块缓存缺原版时
  自己下载，并在 `ci.yml` / `release.yml` 的 Go 构建之前各插入一步。
- **只改文档的提交被误判成 lint 失败**：lint 门改为自己算改动文件（`git diff --name-only`），
  空集明确跳过；`git diff` 在仓库根目录跑，避免 `cd web` 后 pathspec 变成 `web/web` 匹配不到。
- **前端满足 biome 门禁**：会话化改造后的前端文件通过仓库的 biome 棘轮门（class 排序 /
  冗余解构 / 布尔 `||` 的逐行抑制并写明理由）。

### 待落地（**不在本版内**）

- **（占位，安全加固批次）** 后台命令跨 run 持久（`ARTEX_TASK_DIR` + 进程级注册表 + 回收者）、
  受限运行角色（数据库侧最小权限）、跨会话共享上下文、会话收件条与线程、记忆混合检索面等条目
  **正在并行落地中，尚未形成提交**。合入后再补写本节；在此之前它们**不计为本版内容**。
- **Phase 0 Task 4（DB 直连靠 DB role 挡）** 改的是数据库权限模型 ⇒ 需要停机窗口 + 凭据轮换
  配套，按既有红线不擅自执行。
- **P0-④(c) 后台任务跨 run 持久** 需要宿主目录、跨 session 注册表、任务表当真相来源与回收者
  四件同时到位 —— 缺回收者只会换来一个进程泄漏 ⇒ 与上一条同批处理。

## [0.4.1] - 2026-10-28

W-A 波次：把「已登记的活路径」逐条闭合或如实降级。**功能面不变**，动的是边界、
留痕与交付面。

### 判定层：分级审核 + 审批治理

- **短路表三档化**：`deny`（凭据汇 / 读控制面秘密）、`ask`（真高危：改可执行面、改既有
  行为、删证据库）、**显式 allow**（低危日常：新建技能文件）。显式 allow 是代码常量的
  **主动判定放行**，仍插在 `applyIntercept` 最前求值 —— 与「不命中短路表」的漏判有本质
  区别，故 G-01① 原漏洞不重开；allow 档判定也进审计链。
- **DB 规则的 ask 类补默认超时**（60s，超时即拒）：种子规则照抄 `timeout_enabled=false`
  就是「审批无限等待」，该窗口关闭。
- **审批防疲劳计数**：同一 agent 在时间窗内反复对同一高危面请求审批，超阈值才自动拒绝
  （默认 5 次 / 600 秒，可热改）。单次或少量请求**完全不受影响**。

### 路径判定：Bash 面不再只靠字符串预筛

- 入参里的路径片段现在**按工具工作目录解析成真实路径**再判定（`pathguard.CheckInputPaths`）：
  `cat $(realpath ../../../jwt.key)`、`cat ../../../jwt.key` 这类经 shell 展开才现形的路径
  已被拒；软链与父目录软链也覆盖。判定点仍在工具策略层（Stage 1 Deny，任何 permission
  mode 绕不过）。
- **工具层的拒绝接入审计链**：该层早于 `PreToolUse` hook 求值（hook 不会被调用），原先
  拿不到留痕；现由 server 注入的记录器补一条，链的规范化与串行化与其他判定动作一致。

### 凭据供给面

- **数据库口令改由 docker secret 文件供给**（`/run/secrets/pg_dsn`）：`docker inspect` 不再
  看得到口令。配套：`/run/secrets` 进平台秘密清单（agent 读不到）、只读挂载的 DSN 文件
  不再要求「读入即销毁」。
- **`llm_profiles.api_key` 可加密落库**（可选）：AES-256-GCM，主密钥由
  `ARTEX_MASTER_KEY_FILE` 供给（不进环境块、不进库），历史明文行启动时自动迁移；AAD 绑定
  行名，改名触发重加密。未配主密钥时行为与从前完全一致。

### 门禁与交付

- **发布冒烟新增环境块洁净性阻断门**：每次发布都验「应用进程树的环境块内无非空平台秘密」。
- 工具层拒绝的留痕缺口补齐后，「谁在何时想干什么」在审计链上可查。

### 如实降级（未闭合）

- **`/proc/<pid>/mem` 读同 UID 进程内存仍可做到**：同 UID 的 ptrace **不需要**
  `CAP_SYS_PTRACE`，故 `cap_drop` 挡不住它。真正的闭合要让工具进程与 artex 不同 UID
  （属 per-task 隔离那一档）。镜像里留了 `verify-ptrace-blocked.sh` 供自查，它**故意
  不进发布门**。

## [0.4.0] - 2026-10-28

reArtex 的首个正式发布版。相对上游 ARTEX，本版集中做的是**安全边界与门禁**，
功能面（执行模型、任务编排、探索引擎、对外 API）保持上游原样。逐项差异与目的见
[README](https://github.com/wufufu770/reArtex#重点改了什么)。

### 交付

- **reArtex 自己的发布线**：镜像 `ghcr.io/wufufu770/reartex`（linux/amd64 + linux/arm64）、
  五平台预编译二进制 zip、附 `SHA256SUMS` 的 GitHub Release。页面「一键更新」只跟随
  本仓库的 Release，不跟上游。
- **镜像构建期校验二进制版本**：构建必填 `ARTEX_BINARY_VERSION`，构建时跑一次装进去的
  二进制核对它自报的版本 —— 旧产物不再被静默装进镜像。
- **发布后冒烟**：release 把刚推上去的镜像拉回来真跑一次，核对版本与 tag 一致。
- **产物布局归一**：本地构建与 CI 落到同一处，不再有两套路径各说各话。

### 工程门禁

- 新增 CI 门禁：编译 + vet + **全量测试（真实 Postgres，排除项为零）** + 前端构建 +
  前端 lint 棘轮门；tag 发布前也须过门。
- **测试隔离**：各测试包改用自己派生的库，套件可重复（此前共用一个库，每次全量跑挂的
  用例都不一样），且测试不再往开发库灌数据。


### 安全

#### 修复的问题

- **平台凭据不再残留在进程环境块**：`ARTEX_PG_DSN` 原先经容器环境变量下发，而环境变量会留在进程的 exec 期环境块里 —— `/proc/<pid>/environ` 对同 UID 的 agent 子进程可读，一条 `cat /proc/*/environ` 就能取走数据库口令，进而读到 LLM key 与全部会话/流量/漏洞数据。
  新增 `ARTEX_PG_DSN_FILE`：DSN 由一次性文件供给，进程启动读入后立刻清零并删除该文件；部署侧在 `exec` 之前 `unset` 环境变量，使进程环境块内不再有任何秘密。
  配套的启动自检会读取 `/proc/self/environ` 并给出「未残留 / 仍残留」的明确结论 —— 部署形态退化时当场喊出来，而不是默默「看起来加固了」。
- **平台环境块拦截覆盖 glob 与展开形态**：原规则只认 `/proc/self/environ` 与 `/proc/<数字>/environ` 两种字面形态，`/proc/*/environ`、`/proc/$$/environ`、`/proc/[0-9]*/environ`、`/proc/1/envi*on` 均可绕过。现改为一条覆盖形态的规则。
- **MCP 注册工具由 ask 收紧为 deny**：stdio 传输会以容器 root 拉起任意二进制，http/sse 传输则把 env 字段当 HTTP header 回放。新建 MCP 等价于「让平台对任意地址带任意 header 发请求」，不应由 agent 在测评中途自行开通。
- **MCP 定义写库侧校验**：与短路表构成双层拦截；短路表被 `ARTEX_DISABLE_SHORT_CIRCUIT=1` 停用时，写库侧仍拦得住。
- **MCP SSE 端点同源校验**：端点由被连接的 MCP 服务端自行公告，校验 scheme/host/port 并拒绝带 userinfo 的端点，阻断 Authorization / X-API-Key 向任意主机回放。
- **控制面凭据读入即从进程环境抹除**：`ARTEX_PG_DSN` 与 `ANTHROPIC_API_KEY` / `OPENAI_API_KEY` 读入后立即 `os.Unsetenv`，避免子进程 `env` / `printenv` 直接读走。
- **自定义脚本工具改白名单继承宿主环境**：黑名单追不上新增的 `*_TOKEN` 一类变量。
- **工具执行前的平台侧安全策略**：在工具真正执行前按解析后的真实参数判定，短路表负责「该不该做」，策略装饰器负责「够不够得着」。
- **skill 信任分档**：记录 skill 的写入来源 —— operator 写入的照常注入，agent 写入的降权注入；失败一律 fail-closed。
- **模型兜底审批改为 fail-closed**：判官（LLM 兜底审批）超时、报错或输出不可解析时，原先回落 `allow` —— 等于「只要让判官失效就能绕过全部规则」。现默认回落 `deny`。管理员仍可在「系统 → 命令拦截」里显式改成 `allow` / `ask`，那是有意的放宽。
  同时把前端配置表单的初值与后端默认对齐 —— 否则打开页面直接保存就会把 `allow` 写回库，静默恢复 fail-open。
- **ask 类规则强制超时**：ask 规则若不配超时，人工审批一直没人确认就是「无限等待」——而 ask 的语义是「拿不准就问人」，问不到人等于变相放行。现 ask 规则一律带 60s 超时（到点按 `deny`），管理员仍可调等待时长与到点后的动作。
  超时对 ask 是**强制**的：`timeout_enabled` 是布尔值，JSON 里「未指定」与「显式 false」不可区分，与其猜不如不允许出现无限等待。
- **短路表的 deny 现在落库**：此前它只进内存审计切片（上限 2000、进程退出即失），于是「谁在何时想干什么」查无实据 —— 而短路表恰恰是唯一一道不受 DB 热改面影响的防线，最需要留痕。现写入 `intercept_pending`（已决行），在「审批记录」页与任务拦截列表里可查。
- **工具表不可读且无快照时拒绝创建新 run**：降级为最小工具面只是「少给几个工具」，但冷启动那一次恰恰是无人值守的时刻 —— 白名单未知时开工等于无边界放行。现任务创建入口（API / 自动拆子任务 / 管理接口三条路的汇合点）直接拒绝。
- **交付源改指本仓库**：原 compose 镜像、selfupdate 更新源、README 克隆与 Releases 链接、CHANGELOG 对比链接全部指向已 404 的上游 `Autumn-27/ARTEX`。现指向本仓库；镜像发布从 Docker Hub（上游作者的命名空间，fork 无权推）迁到 `ghcr.io/wufufu770/reartex`，与 GitHub 账号天然绑定。

#### 新增

- `ARTEX_PG_DSN_FILE`：DSN 文件来源（读入即清零并删除），优先级低于 `ARTEX_PG_DSN`、高于配置文件。
- `ARTEX_REQUIRE_ENV_SCRUB`：设为 `1` 时，若启动自检发现平台凭据仍残留在 `/proc/self/environ`，拒绝启动。默认只告警。
- 启动自检：读取 `/proc/self/environ` 并在日志中给出明确结论。
- **CI 安全测试门**：新增 `.github/workflows/ci.yml`（编译 + vet + 全量测试 + 前端构建），PR 与分支推送必跑，tag 发布前也必须过。此前本仓库唯一的 workflow 只有 release，其中 `go test` / `npm test` / `security` 命中数为 0 —— 没有任何测试门，坏掉的测试可以一直烂在那里没人发现。
  测试带真实 Postgres：server / agent / db / evidence 等包的测试要建库、跑真实读写并清理数据，没有库时那些用例会因「未找到数据库配置」而失败，门就成了摆设。
- **拦截记录来源标识**：新增 `shortcircuit` 来源，区分「代码常量短路表判定」与「DB 规则 / 模型判定」。此前短路表的记录来源显示为「未知」，看不出是哪条内置防线拦的。

### 已知残留

- `/proc/<pid>/mem` 读父进程内存仍可取到 DSN 字符串（需 ptrace 权限）。本批未处理。
- 运行时才成形的路径（如 `find /proc -name environ -exec cat {} \;`）仍能绕过字符串预筛。正则只是纵深防御，闭合靠的是凭据不进环境块这一供给侧措施。
- `docker inspect` 仍可见容器配置里的 DSN（需宿主 / docker socket 权限）。同理，`docker exec` 拉起的进程会继承容器配置面的 env —— 但 agent 的 Bash 是 artex 的子孙进程，继承的是 artex 的（干净的）环境块，不在此列。
- 平台环境块规则作用在整个工具入参 JSON 上，故「把含 `/proc/…/environ` 字样的内容写进文件」也会被拦。
- `llm_profiles.api_key` 仍以明文存于数据库（需数据库读权限才能取到）。
- G-17 的审计事件骨架与 digest 链（需改表结构）仍未做 —— 短路表 deny 已落库，但只有结论与理由，没有 digest 链式的防篡改。
- G-09 ask 队列治理（需 per-agent 聚合计数）。
- **CI 里有一个已知断测被单独排除**：`TestGraphOverviewExpandsAssociatedCompanyScope` 断言 `graph_overview` 的 `coverage.scope` 含公司范围展开，而生产代码尚未接线该字段（测试超前于实现，基线即红）。排除项与理由写在 workflow 里，修好该功能后应把排除去掉。
- **CI 前端 lint 未设为门**：仓库现有 lint 基线本身是红的（集中在既有文件），设为门等于永久失败、无人再看；目前只保证前端能构建。
- **判官 fail-closed 带来的运维耦合**：判官一旦启用，超时（默认 15s）或报错都会直接拒绝该次工具调用 —— 判官侧不可用等于 agent 无法工作。这是刻意的取舍（fail-open 等于审批形同虚设），但启用前应确认判官链路的稳定性与额度。
- 尚未做运行时端到端验证：单测覆盖了函数逻辑，但未通过真实 agent 会话发起工具调用确认防线在链路中生效。

## [0.3.15] - 2026-10-07

### 拦截

#### 新增的功能

- **新增内置拦截规则「删除类接口路径」**：此前内置的 HTTP 破坏性规则只认 DELETE **方法**，而多数应用的删除接口用 GET/POST 就能触发，`curl 'http://t/api/user/delete?id=1'` 这类调用会真实删掉目标数据。现补一条 `deny` 规则覆盖 `/delete /del /remove /unlink /erase /destroy`（动词后必须跟分隔符，`/delivery`、`/details`、`/delta` 不会误拦）。已有实例升级后也会拿到，可在「系统 → 命令拦截」里停用或删除。
- **模型兜底审批的 token 消耗可单独查看**：审批调用此前没有独立的计量归属，花了多少只能靠猜。现在它单独计量，「系统 → 命令拦截」的「模型兜底审批」开关下方新增用量卡片——调用次数、输入、输出、缓存读写五项累计统计，加一张近 30 天每日消耗的迷你柱图。

### 漏洞推送

#### 新增的功能

- **漏洞发现支持推送到 IM**：新增「系统 → 通知推送」页，可把扫描发现推送到**钉钉、飞书、企业微信、通用 Webhook、Telegram、邮件**六种渠道。同一类型可配任意多个实例，每个实例独立设启停、限流与过滤规则。
- **推送时机分实时与汇总两种模式**：实时模式命中即逐条发送；汇总模式按全局周期（默认 30 分钟）合并成一条消息，开头给出新增数量与级别分布。想做「高危实时、其余汇总」就建两个渠道分别配置。
- **过滤规则支持四个维度**：最低严重级别、限定任务 / 资产范围、漏洞类型关键词包含与排除（排除优先）、是否接收处置状态变更（默认关）。消息带「查看详情」按钮，目标地址由全局「回链地址」配置，留空则不带按钮。
- **投递历史与手动重发**：列出每条投递的状态、尝试次数、失败原因与所属渠道，可按渠道与状态筛选；失败项可一键重发（重发会清零重试计数）。
- **渠道凭据掩码回显**：Webhook 地址、加签密钥、Bot Token、SMTP 密码等只回显带尾号提示的掩码值；提交时原样回传表示「不修改」，清空输入框则删除该字段。

#### 修复的问题

> 本节是功能初版完成后一轮安全审计的结论，每条都先复现、再修、再补回归测试。

- **修复掩码可被「改目标地址、保留凭据」绕过（严重）**：配置合并对「未提及的键」一律保留库中原值，于是只改地址、对凭据避而不谈，就能让服务端把库里的真凭据发到任意地址，且完全静默。现规定目标地址一旦变化，每个凭据字段都必须显式表态——给新值，或显式留空表示不再需要。
- **修复 markdown 系渠道对不可信内容零转义**：漏洞标题与摘要来自模型输出、资产 URL 来自扫描结果，构造的标题会在钉钉 / 企业微信 / 飞书里渲染成可点击外链，图片语法则会在渲染时被客户端拉取并泄露阅读者 IP。现统一做单行化 + markdown 元字符转义，且转义发生在各渠道的渲染出口而非共用的标题函数。
- **修复投递地址的 SSRF 面**：此前环回、内网与云元数据地址一律可投递，而失败响应体前 200 字节会进 `last_error` 并被投递历史回显，构成一条半盲读原语。现改在**拨号阶段**设防，覆盖 DNS 重绑定，并拒绝跨主机重定向；环回与链路本地地址需显式设置 `ARTEX_NOTIFY_ALLOW_LOCAL=1`，RFC1918 私网仍放行（内网自建 SMTP 中继很常见）。
- **修复推送凭据经错误信息外泄**：钉钉的 `access_token`、企业微信的 `key`、Telegram 的 bot token 都在 URL 里，而投递失败时完整 URL 会流入 `last_error`、投递历史、服务端日志与前端提示四处。现统一脱敏，只保留 `scheme://host` 与底层原因。
- **修复汇总消息被截断后整批标记已送达导致的静默丢失**：被截掉的漏洞既不在消息里、也不在失败列表里，投递历史却显示成功。现改为按**整条**打包，只标记装进本条的那些，其余回队列等下一条续发且不消耗重试次数，消息头部也写明还有多少条将在下一条继续。
- **修复最低级别门槛打错字会让过滤器静默失效**：`min_severity` 写成 `hgih` 这类笔误时未知级别序数为 0，判定退化成恒真，用户以为只收高危、实际全部漏洞都被灌进群里。现于写入路径校验取值并列出可选值。
- **修复 `rate_per_min = 0`（不限流）不可达**：写库这一层把显式 0 悄悄改成了默认值，操作者以为放开了限流、实际仍被卡着。默认值改到接口层在字段缺省时填。
- **修复汇总渠道完全绕过令牌桶**：`rate_per_min` 对 digest 模式此前没有任何作用。现在领取条数同时受本轮额度与内存上界两个约束。
- **修复失败处置按整批最大尝试次数判断，让老投递连坐新投递**：一个已重试两次的老投递会把同批里全新的投递一起拖进 failed，新漏洞一次重试都没用上就永久丢失。现改为逐条决定。
- **修复汇总批次中快照无法解析的投递被静默标记成功**：这类条目会被渲染阶段跳过，却随整批一起算作成功。现在显式判失败并给出原因。
- **修复单渠道每轮投递条数可能超出租约时长**：多实例部署下，租约过期的行会被重新领走、重复发送并双份递增尝试次数。现按「租约 / 单次超时」倒推每轮上限，并有断言把这几个常量的关系钉死。
- **修复 Telegram 截断可能切断 HTML 实体**：截断只避开了半截标签，没避开 `&amp` 这类实体残片，而解析器可能因此拒收**整条**消息。现在两者都回避。
- **修复邮件渠道把临时性 SMTP 失败判成永久失败**：灰名单 `450` 这类 4xx 是临时拒绝、本该稍后重试，此前一律判死，启用灰名单的服务器会让每条推送都在首次尝试后失败。现按应答码首位区分，4xx 可重试、5xx 永久失败。
- **修复提交嵌套结构时会真的把掩码字面量写进库**：像 `webhook.headers` 这种对象字段只能整体掩码或整体提交，把掩码哨兵塞进对象内部会被当真实值存下来、导致鉴权静默失效。现在这种提交被显式拒绝。

#### 设计说明

- **写漏洞的事务只做一次盲 INSERT**：`notification_events` 与漏洞在同一事务内写入，提交即保证两者原子一致。这条 INSERT 刻意不读渠道表、不跑用户的过滤规则，并由 `SAVEPOINT` 包住——否则一条配错的过滤条件就能中止事务，让高危漏洞存不进库。
- **投递用租约领取而非长事务**：`FOR UPDATE SKIP LOCKED` 领取后把行置为 `sending` 并把 `next_attempt_at` 推到未来作为租约，投递期间不持有数据库锁；进程崩溃留下的行会在租约到期后被重新领取，自愈且不会无限重试。
- **限流不消耗重试预算**：先按令牌桶算出本轮额度，再按这个数量去领取。顺序反过来会让被限流挡下的投递白计一次尝试次数，三次预算被纯等待耗光后落入失败。

### 测试基础设施

#### 修复的问题

- **修复公司 ICP 归属用例的清理顺序导致的资产永久残留**：`defer d.Close()` 在函数返回时先执行、`t.Cleanup` 在其后，于是清理语句全部落在已关闭的连接上，错误又被丢弃，测试资产与公司永久残留在库里，还会让其他按资产计数断言的用例莫名失败。现改为关闭连接也走 `t.Cleanup` 并注册在前，同时让清理失败显形。

### 流量

#### 新增的功能

- **流量列表支持内容检索、路径 / 状态码 / 长度筛选与列排序**：筛选行支持按**响应内容**（走全文索引）、**路径**、**状态码**（精确值或 `2xx` 这类分桶）、**响应长度范围**过滤，表头可按时间、状态码、响应长度排序，排序偏好记在本地。新增两个索引，已有实例升级后自动生效；Agent 工具的 query 契约未改动。
- **流量列表新增「清空全部」**：一次删除全部流量记录（忽略当前筛选），并清理索引已不再记录的历史 host 目录，清空后顺带做一次全量压实并提示实际释放了多少空间。已绑定到漏洞的流量证据存在独立的证据库中，不受影响。

#### 修复的问题

- **修复删除流量后磁盘空间不被释放**：索引库建库时没有启用 `auto_vacuum`，文件永不收缩；全文索引又是 `contentless_delete`，`DELETE` 只写 tombstone 而不回收原 postings——删流量反而让索引变大（实测抓 6MB 正文 → 索引 16MB，删光后仍是 16MB）。现在新建索引库启用 `auto_vacuum=incremental`，每次删除后在后台分块执行全文索引合并与增量回收，逐步把空间还给系统（同一场景回落到 104KB）；回收在块间释放写锁，不阻塞流量录制。

> 升级说明：`auto_vacuum` 只能在建库时设定，因此**已有实例的索引库仍是旧模式**，增量回收在其上是空操作（启动时会打印一行提示）。这类库升级后 tombstone 的增长已经止住，而已占用的体积用「清空全部」回收一次即可——那一步会把库转成增量回收模式，此后日常删除自行生效。

### LLM

#### 修复的问题

- **修复自定义会话头在「一次性 LLM 调用」路径上不生效**：目标拆解（第 0 轮）与冷节点压缩都是不挂 transcript store 的一次性调用，context 上没有 session id，网关侧因此读不到该头——对 opencode zen 这类缺头直接 400 的端点，表现为「第 0 轮失败、后续 planner 轮次却完全正常」。现为这两条路径显式挂上按探索稳定的 session id，顺带让 token 用量能正确归因到对应探索（此前完全记不到）。

### 工具

#### 修复的问题

- **修复 MCP 与自定义工具的输出截断不生效，大结果整段灌爆上下文**：MCP 工具的返回此前完全不经截断，HTTP 类自定义工具则因传入空的工具上下文，一直用写死的 6000 字符而非会话配置的上限。现两条路径统一走 `actool.Capture`：按会话 `MaxOutputChars`（默认 30000）截断，配了 `ToolOutputDir` 时把全量输出溢写到磁盘、消息里只留开头片段与文件指针。
- **升级 norma 到 v0.4.3**：harness 层统一执行 `CaptureOnce`，任何工具（含今后新增的）都不会再因为漏接截断而溢爆上下文。

### 部署

#### 修复的问题

- **修复反向代理部署下活动流连不上**：SSE 地址此前无条件拼成 `${hostname}:8787`，反代到 443 的部署里页面从 `https://域名/` 加载、SSE 却被指向未对公网开放的 8787 端口，表现为活动流反复重连。现按 `NODE_ENV` 区分：生产默认同源，仅 `next dev` 保留 `:8787` 以绕过 Next.js 对 SSE 的缓冲；`NEXT_PUBLIC_SSE_BASE` 覆盖口保留。README 补充了反向代理部署说明。

### 发现

#### 修复的问题

- **修复「按资产」视图资产列表溢出后没有滚动条**：外层卡片只给了 `max-height` 而没有确定高度，滚动视口的百分比高度解析不出来，于是资产多时列表要么撑破卡片、要么被截断且无法滚动。现把高度上限直接落到资产树的原生滚动容器上，随窗口高度自适应。

### 数据库

#### 修复的问题

- **为连接池设置上限**：`database/sql` 默认不限制应用自身可打开的连接数，池里没有空闲连接时会无条件新建，一路顶到 PostgreSQL 的 `max_connections`（默认 100）才被拒。现封顶 32 条，超额查询改为等待空闲连接，同样的负载下表现为变慢而不是报错；若 `max_connections` 调低过需同步下调。
- **提升连接复用率**：`MaxIdleConns` 的默认值是 2，并发超过 2 的连接用完即关、下次请求要重做 TCP 与 PostgreSQL 认证握手。现将空闲上限提到与总上限一致，并补上 30 分钟连接寿命与 5 分钟空闲淘汰，让池能自然换掉长期存活的坏连接。

### 贡献者

- [@Autumn-27](https://github.com/Autumn-27)

## [0.3.14] - 2026-09-24

### 任务列表

#### 新增的功能

- **任务列表新增「漏洞」列**：按严重度分档显示 严 / 高 / 中 / 低 的数量，非零档位按严重度着色，一眼看清每个任务的漏洞规模与分布。

#### 修改的功能

- **「描述 / 目标」两列宽度收窄**：超长内容省略显示，鼠标悬浮可查看全文，减少长文本挤占列表横向空间。

### 探索图与规划态势概览

#### 修改的功能

- **精简规划态势概览（graph_overview）并为各列表设定数量上限**：最近事实、已结束意图、待办意图、冷区摘要与确认漏洞明细均改为「最新一窗 + 计数兜底」，未展示的可按需查询；显著降低每轮 LLM 上下文体积、避免长任务上下文膨胀（关联任务概览的冷区一并限流）。

#### 修复的问题

- **折叠冷节点后消除探索图中 digest 与 finding 之间的反向重复边**。

### 贡献者

- [@Autumn-27](https://github.com/Autumn-27)

## [0.3.13] - 2026-09-19

### 资产拦截

#### 新增的功能

- **新增全局资产拦截规则（黑名单）**：支持录入全等与模糊匹配的域名 / IP / URL 以及 CIDR 网段，提供规则的增删改查与启用/停用，管理页位于「系统 → 资产拦截」；默认内置模糊拦截政府（`.gov` / `.gov.cn`）与教育（`.edu` / `.edu.cn`）网站。
- **资产拦截接入执行链**：Agent 在下发意图（`add_intent`）与插入资产（`insert_assets`）前先对目标资产做拦截判定——命中拦截的意图不下发、命中拦截的资产不插入，并向 Agent 返回资产信息与拦截原因。
- **新增任务级拦截 / 允许（白名单）规则**：独立于全局规则、仅对本任务生效，判定顺序为「先拦截后允许」——命中拦截即禁止；未命中拦截但本任务配置了允许规则且都不命中，则「不允许测试」；未配置允许规则时不启用白名单。可在创建任务时录入，也可在任务详情「总览」中增删改与启用/停用。
- **任务模板支持预设分类与任务级拦截/允许规则**：模板可保存任务分类与一组任务级规则，应用模板时一并带入新任务表单。

### 操作审查

#### 修复的问题

- **收紧模型裁判输出协议，减少截断导致的误放行**：将裁决说明（comment）上限由 500 汉字下调至 120 汉字并强制「只输出 JSON、不带前言或代码块」，避免裁决被 `MaxTokens` 截断后无法解析、进而按模型失败策略放行（fail-open）。

### 任务归档

#### 修复的问题

- **归档遇到符号链接时跳过而非整包失败**：归档格式端到端仅支持普通文件与目录，此前工作目录中出现任意符号链接都会导致整个任务归档失败；现改为跳过该符号链接并记录日志，其余文件正常归档（不跟随链接、不越出目录树）。

### 账户与合规

#### 新增的功能

- **登录前新增「使用须知与免责声明」弹窗**：需勾选同意后方可登录。

### 许可与依赖

#### 修改的功能

- **项目采用 AGPL-3.0 开源协议**，并完善 README 的许可与免责声明说明。
- **升级 norma 至 v0.4.1**。

### 贡献者

- [@Autumn-27](https://github.com/Autumn-27)

## [0.3.12] - 2026-09-17

### 探索链路播报板

#### 新增的功能

- **任务详情页新增「探索链路播报板」**（#144）：以时间轴流水视角呈现任务的探索节点（起点/目标/意图/事实/漏洞/提示/压缩），支持按类型筛选、关键字搜索、正序/倒序、分页与自动刷新；处于「最新在前的第一页」时为直播位、每轮刷新，离开该位时只累计未读计数、不打扰当前阅读，并以「N 条新播报 · 回到最新」一键返回。按天分组，长任务翻页也能认出「这是哪天的事」。
- **播报板每行显示节点 id**（#145），便于对照探索链路图与定位具体节点。
- **播报板节点详情展开显示上下游与锚定资产**（#147）：展开一条播报可见该节点的上游（由此而来）/下游（由此产生）关系，鼠标悬停关联条目弹出对应节点的名片（类型/状态/来源/时间/摘要/payload 片段）；并顺带列出该节点锚定的资产（类型标签 + 可辨识文本）。相关数据随播报页一并下发，展开不额外发请求。
- **播报板搜索支持按节点 id 筛选**（#150）：搜索框在「内容 / 来源」之外新增节点 id 匹配，输入纯数字或界面展示的「#41」形式即可精确定位对应节点。

### 意图管理

#### 新增的功能

- **意图删除支持「假删除 / 真删除」两种模式**（#149）：待领、运行中、已暂停的意图均可删除，删除需填写原因；删除模式在确认弹窗中选择。
  - **假删除（默认）**：意图置为「已删除」并把原因记入独立字段，保留意图节点与其全部产出、血缘。
  - **真删除**：物理移除该意图，以及「仅由它支撑」的独占子孙节点（沿产出 / 意图链级联到叶子），避免留下孤立数据；被删意图的 Token 计量按原始日期归档保留；共享节点（还被其它意图引用）、目标与任务根事实一律保留，真删除弹窗会展示预计级联影响的节点数。

  两种模式都会通知规划者「该意图由用户删除 + 原因」并据此重新规划。

### 操作审查

#### 新增的功能

- **审批记录支持按状态与判定来源筛选**（#139）：可按审批状态、判定来源过滤审批记录，快速定位目标记录。

#### 修复的问题

- **待处理审批请求独立于历史分页加载**（#133）：待处理请求不再受历史列表分页影响，翻阅历史记录时仍完整可见。

### Agent

#### 修改的功能

- **Worker 角色描述改为通用网络安全平台表述**（#138）。

#### 修复的问题

- **修复冷节点压缩从不触发**：给按任务运行的规划器接上 Compactor，冷节点压缩（cold-digest）此前因未接入而从不执行，现已恢复。

### 聊天

#### 新增的功能

- **聊天支持多类型记录的 @ 引用及滚动分页**（#135）：可在聊天中 @ 引用多种类型的记录，引用候选支持滚动分页加载。

#### 修复的问题

- **长消息气泡约束在会话面板内**（#137）：过长的消息气泡不再溢出会话区域。
- **修复新对话未创建时上传文件报「缺少上传文件」**：Composer 的文件选择在清空 input 之前先把 `FileList` 快照为数组再回调；此前草稿态需先异步创建会话，恢复执行时与 input 活绑定的 `FileList` 已被清空，导致上传缺少文件字段、后端返回 400。

### 流量

#### 修复的问题

- **流量录制代理默认只监听 127.0.0.1**（#129、#130）：避免默认配置下开放代理暴露在其它网络接口。

### Web

#### 修复的问题

- **修复静态导出版任务列表页丢失全局头部**。
- **补齐 demo 漏洞详情页关联流量 mock，修复白屏**。

### 贡献者

- [@Autumn-27](https://github.com/Autumn-27)
- [@RuoJi6](https://github.com/RuoJi6)
- [@dingpotian](https://github.com/dingpotian)

## [0.3.11] - 2026-09-15

### 操作审查

#### 新增的功能

- **审批记录支持来源定位**（#125）：点审批记录的「来源」可跳回触发该次审批的那一次工具执行——打开原始完整会话、自动分页加载到目标位置、就地展开命令与结果并在滚动区居中高亮，前后消息保持可读；用户手动滚动时停止自动校正。跨普通对话、任务 Worker、规划器与主 Agent 分段会话，均通过持久化的 `tool_use_id` 与任务映射精确定位（新增作用域内调用 ID 索引），缺失、重复或关联不明确时显式提示而非跳到其他执行。会话被删除或任务归档/记录缺失时给出明确提示。
- **审批记录支持翻页**（#115）。

#### 修改的功能

- **精简模型审查输入**（#124、#125）：模型审查（规则未命中后的 LLM 裁判）输入收敛为「当前这一次完整工具调用 + 明确选定的简短背景 + 本机工作目录」的版本化 JSON。背景只取真实用户消息（聊天/任务主 Agent 的当前用户消息），Worker 不再附带意图摘要、也清除从上级 Agent 继承的背景；规划器与自动触发会话不补造用户消息。不再附带任务描述、目标、操作约束、全局探索态势、历史调用或 Worker 完整意图——这些仍按原逻辑用于 Agent 执行与独立会话审计，只是不进入操作审查。所有裁决要求输出 JSON 的 `decision`/`comment`，说明必须包含「实际操作、成功后的后果、命中规则」；参数或背景中的指令不能改变审查策略。保存实际发送给模型的输入快照及指纹，旧版快照保留并标注版本，不用当前数据补造旧输入。

#### 修复的问题

- **模型裁决被代码块包裹时不再静默放行**（#126）：模型返回的裁决 JSON 若被 ``` 代码块包裹，此前解析失败会经模型失败策略被静默放行；现在先剥离代码块围栏再解析，解析仍失败才按已配置的失败策略处理。

### Agent

#### 新增的功能

- **新增实验性 noa 上下文压缩**（norma 升级至 v0.4.0）：系统设置中可开启的实验功能，默认关闭。开启后由 noa（模型驱动的上下文压缩）接管主 Agent、规划器、Worker、聊天四类 Agent 的上下文压缩，替代内置压缩；压缩原文持久化归档，集中落在 `<workDir>/noa/<会话ID>/` 下（不分散在各任务目录内），按会话 ID 全局唯一分目录。接入失败自动回退内置压缩，不中断真实任务；开关每次运行读取一次，切换只影响之后启动的运行。

#### 修复的问题

- **探索图工具在无任务上下文时拒绝而非 nil 崩溃**：非任务上下文调用探索图相关工具时返回明确错误，不再因空存储解引用崩溃。

### MCP

#### 新增的功能

- **支持旧版 SSE MCP 服务**（#117）：兼容仅提供旧式 SSE 传输的 MCP 服务端。

### 流量

#### 修复的问题

- **流量检索按端口感知匹配记录的主机**（#114）：`traffic_search` 的主机匹配纳入端口，避免不同端口的同主机记录相互串扰。
- **`traffic_search` 描述迁移出错不再中断后续 reporter 迁移**：单条迁移失败被隔离，不影响后续迁移执行。

### Web

#### 修改的功能

- **任务复测漏洞选项增加分割线**（#122）：复测漏洞选择项之间增加分隔，视觉更清晰。

#### 修复的问题

- **修复 demo 任务详情页整页崩溃**：mock 模式下任务详情「会话」标签会去拉 `GET /api/tasks/<id>/side-questions`（旁路提问历史），但 mock handler 没有这条路由，被读兜底按“路径以 s 结尾即集合”返回了 `[]`，导致 `data.items` 为 `undefined`，旁路 hook 的 `merge()` 对其迭代抛 `TypeError: t is not iterable`；该异常发生在 `setItems` 的 updater 里、被 React 推迟到 render 阶段重抛，调用方 `catch` 接不住，整页被错误边界接管显示 “This page couldn't load”。现在 mock handler 显式返回空的旁路历史，`sideAPI.history` 也对返回值做归一化（`items` 非数组一律兜成 `[]`）作为防御纵深。

### 贡献者

- [@Autumn-27](https://github.com/Autumn-27)
- [@RuoJi6](https://github.com/RuoJi6)

## [0.3.10] - 2026-09-13

### 网络

#### 新增的功能

- **网络搜索新增 DeepSeek 官方来源**：直接复用当前激活的 LLM 配置。它与其余三个来源性质不同——DeepSeek 没有可直接调用的搜索接口，搜索只存在于其 Anthropic 兼容接口内部（`web_search_20250305` 服务端工具），由 DeepSeek 服务端执行。因此它**仅支持 DeepSeek 官方端点 + anthropic 协议**（OpenAI 协议端点会直接拒绝服务端工具），且每次搜索会**额外消耗一次模型调用**、请求**不经过搜索出口代理**、也**不计入流量留痕**，返回结果**只有标题与链接**（无摘要，需正文时由 WebFetch 抓取）。设置页说明上述限制但**不做校验拦截**，是否满足由用户自行确认，可用「测试搜索」按钮实跑一次验证。

### Agent

#### 新增的功能

- **worker 获得跨 work 回看能力**：新增 `search_all_worker_traces`（不必先知道 intent_id，按关键字在本任务所有 work 的执行过程里全局检索命中步骤）与 `get_worker_trace`（锁定某条 work 后列步骤流、就地按关键字搜、按 step_id 取完整内容），用于复用别的 work 见过却没写进 fact 的观察、避免重复劳动。
- **`node_detail` 下放给 worker**：配合上面的回看工具，worker 拿到 intent_id / 节点 id 后可直接查该节点的完整详情。
- **`add_hint` 触发的规划轮会被显式播报**：此前新增提示只把 hint 折进态势总览、让规划者自己发现；现在一次 `add_hint` 会给规划者记一条「人新增了 N 条战略提示：…」触发（批量算一条，不逐条刷屏），规划者被明确告知“本轮由新增 hint 触发”并直接看到提示内容。

#### 修改的功能

- **全局态势提示改为“松绑”口吻**：鼓励探索发散、跨意图线索及时上报，而非过早收敛。
- **精简 worker 边界措辞**：初次受阻不代表探透，聚焦“把本意图内的绕过手段走完再下结论”。
- `insert_assets` 移除每条资产的 `related` 入参：它唯一的作用是决定要不要把这条资产加进本任务范围，值又不落库（同一资产被再次登记就作废、界面上也看不出谁被判为无关），实际只是让模型多做一次留不住的判断。
- **测试范围(`task_scope`)不再受资产覆盖度开关影响**：`insert_assets` 的自动入范围(`source='auto'`)无论覆盖度开启与否都执行，`add_task_scope` 也始终提供给 planner / 任务内主 Agent / 目标拆解 Agent。范围是任务的授权边界、资产查询的过滤基准，覆盖度开关只决定要不要拿它当分母算指标，不该决定要不要累积范围本身。此前关闭覆盖度会让 `auto`/`agent` 两条写入路径同时失效，`task_scope` 只剩界面手工添加的行。`list_untested_assets` 仍随覆盖度关闭而隐藏（它本身就是纯覆盖度视角）。

#### 修复的问题

- **修复任务间资产互串**（#59）：`list_assets` 此前把任务 id 写死为 0、裸查整个共享资产库，模型又无从写出范围过滤条件，于是把别的任务的资产（尤其 IP）当成本任务目标，测试方向被带偏。现在它只返回落在【本任务及直接关联任务】测试范围(`task_scope`)内的资产，按**归属**匹配而非字面值：范围里有某根域即可查到其名下全部子域/服务/接口，有某网段即可查到段内主机及服务；按 id 直取范围外资产同样取不到。非任务上下文(Auto/pentest)无范围可依，仍回退全库。顺带修复 IP 直连主机(如 `http://1.2.3.4/api`)无法被网段范围命中的归属盲区。UI 的「测试资产」视图（按任务生产者过滤）不受影响。
- **修复 worker 跨 work 回看工具被误删**：`search_all_worker_traces` / `get_worker_trace`（以及 `node_detail`）加进 worker 默认工具集后，被一段“收敛 worker 工具面”的旧迁移在启动时又解绑掉，导致 worker 实际拿不到这些工具。已把它们移出该解绑清单，并对已跑过旧迁移的库一次性补绑回 worker。
- **修复 `/btw` 旁路提问偶发入库失败**：side_question checkpoint 落库前剥除 JSONB 不支持的 NUL(`\u0000`) 转义，避免含该字符的内容写入报错。

### 触发器

#### 修复的问题

- **合并触发会话按任务去重任务描述/目标**：多任务合并触发时，同一任务的任务描述/目标不再重复拼进消息，避免长目标反复堆叠把上下文撑爆。

### 贡献者

- [@Autumn-27](https://github.com/Autumn-27)

## [0.3.9] - 2026-09-11

### Agent

#### 新增的功能

- 任务内主 Agent 支持**多会话**：可新建、切换、独立重置上下文，每个会话都能交互。
- 支持持久化的 `/btw` **旁路提问**：不打断主线追问，问答落库，重启后仍在。
- `spawn_task` 新增 `source_task_ids`，新任务可**只读继承**来源任务的资产与发现。

#### 修改的功能

- 关闭 worker 的跨 engagement 记忆并收敛其默认工具面：读上下文、跨 work 复盘属规划职责，worker 只管单条意图的执行与写回。
- 精简 worker 默认提示词；`get_worker_output` 移除 `terminated` 与 `worker_name` 字段；精简 `insert_assets` / `list_assets` 的描述。

#### 修复的问题

- 修复 work 因**空转回合**提前中断的问题：模型有时会用一整轮只输出思考，既不给正文也不调工具，此时 harness 看到的是一次自然结束（`end_turn` 且无 `tool_use`），直接以 `completed` + 空总结收场——一条还没做完的意图就断在半路，表现为「任务正常结束但没有任何文字总结」。五层 LLM 重试一层都够不着它：它不是错误，同 provider 安全窗口重试的入口是"流失败"，熔断在 `err == nil` 时直接判成功，意图重跑只认 `model_error`；SDK 的空响应重试也够不着，因为那层以"有没有 yield 过事件"判空，而思考增量本身就是事件。现在 work 的 Stop 钩子会识别这种回合并注入一条续跑指令，让模型带着已经产出的思考继续执行下一步——刻意不选"原样重发"，因为这类空转通常由提示词与上下文形状决定，是稳定行为而非随机抽风，重发只会让模型再想一遍。
- 次数复用 LLM 页「重试与退避」里的**空响应重试次数**（两者都在回答"模型完成了却没产出实质内容"，只是判据与手段不同），默认 2 次，填 `-1` 即关掉、退回原先"空转即收场"的行为。它是一条意图的**总量**上限而非连续次数：harness 自身已限死连续空转只推一次，只有真正发生过工具回合配额才刷新，所以这个数挡的是「工具 → 空转 → 推 → 工具 → 空转」这类循环把意图预算耗光。触发时日志记 `[work <worker> · #<意图>] 空转回合…注入续跑指令 (n/N)`。设计见 `docs/LLM重试设计.md` §1.1。
- 修复长对话下 `/btw` 的上下文预算与输入布局；非安全上下文（非 HTTPS 访问）降级生成旁路请求 ID。

### 流量

#### 新增的功能

- 漏洞支持**关联多条流量证据**，可排序、加备注、按角色区分（请求/响应/佐证）。
- 报告 Agent 会在写报告前**自动关联**相关流量证据。
- 新增 Agent **流量绑定开关**，并补齐证据在 Agent 之间的交接。

### 任务

#### 新增的功能

- 支持**会话级漏洞复测**：复测跑在独立 Agent 会话里，列表与详情显示运行状态。

### 拦截

#### 新增的功能

- 审批记录新增**详情与执行审计**：可查看工具请求上下文、模型/规则初判、执行输出与参数指纹。

### 资产

#### 新增的功能

- 任务测试资产支持 **DSL 搜索**。

### LLM

#### 修复的问题

- 修复测试连接未补发自定义会话头，导致 opencode zen 返回 400 的问题。

### 网络

#### 新增的功能

- MCP 的 HTTP 传输支持**跳过 TLS 证书校验**，便于接入自签证书的服务。

### UI

#### 新增的功能

- 会话列表按 Agent 分组，支持展开收起与独立置顶；对话支持按 Agent 筛选。
- 攻击链路图渲染压缩（digest）节点，并收缩其成员。

#### 修复的问题

- 修复登录凭据不同步导致的白屏。
- 拦截提示文案改为明确标示「平台管控」，避免被误读成目标侧的防御。

### 部署与更新

#### 新增的功能

- 支持**页面一键更新**：系统配置页新增「版本与更新」卡片，顶栏在有新版时亮出提示。流程为下载发布包 → 校验 `SHA256SUMS` → 冒烟测试 → 暂存 → 退出由守护脚本重新拉起完成换装，页面自动刷新。
- 新增守护启动脚本 `start.sh` / `start.bat` 作为正式启动入口（发布包与 Docker 镜像均已带上，`install.sh` 不变），按退出码决定是否重新拉起，并负责把 SIGTERM 转发给 artex。校验与换装逻辑都在 Go 里，脚本保持傻瓜化。
- 失败自动兜底：校验或冒烟不通过就丢弃、继续跑当前版本；新版连续 3 次启动失败则回滚上一版本。设置页另有手动回滚（注意数据库结构不会回退）。
- 更新只认 GitHub 域名且强制 HTTPS，发布源不可配置；开发构建禁用一键更新。GitHub 查询结果缓存 30 分钟，避免顶栏提示耗尽 API 配额。

#### 已知限制

- Docker 下只换程序、不换镜像：工具链不会跟着升级，且重建容器会退回镜像自带版本，需要时仍用 `docker compose pull artex`。
- 不同步发布包里的 `skills/`，新版新增的内置 skill 不会自动生效。
- 更新即重启，会中断正在运行的任务。

### 依赖

#### 修改的功能

- 升级 norma 至 v0.3.7。

### 贡献者

- [@Autumn-27](https://github.com/Autumn-27)
- [@RuoJi6](https://github.com/RuoJi6)

## [0.3.8] - 2026-09-09

### LLM

#### 新增的功能

- LLM 页新增「重试与退避」标签页：五层重试的**次数**与**间隔**都可以配。一次模型调用的失败由内到外经过建连重试（SDK，流开始前的连接重置 / 超时 / 429 / 5xx）、空响应重试（SDK，正常结束却没有任何内容，仅 openai 格式）、同 provider 安全窗口重试（未向调用方交付任何输出前的断流重放）、轮询熔断（连续失败到阈值就冷却跳过）、意图重跑（worker 以 model_error 收场后整条意图重跑）五层，内层用尽才轮到外层。每层两个旋钮，语义统一：留空 = 用原本的默认次数与指数退避；填次数就用该次数；填间隔就把指数退避换成固定间隔；填 -1 = 关掉这层重试。前三层跟着端点走，可在每个模型配置里逐字段覆盖全局默认（只钉间隔的仍继承全局次数）；熔断与意图重跑是进程级语义，只有全局一份。保存后热生效，不需要重启。全部留空即当前行为，旧库升级后逐字节不变（新列默认 0、settings 键不存在即全默认）。连接测试刻意不带这些参数——它有 30s 硬超时，叠上用户配的重试只会把能用的端点测成超时失败。设计见 `docs/LLM重试设计.md`。
- 每个 LLM 配置支持自定义**会话头**（`session_header_key`）：非空时每次 LLM 请求都会带上该 HTTP 头，头值为当前运行的 session id（chat 会话为 `conv-<id>`、worker 为 `exp<x>-worker-i<intent>` 等），用于某些按 session-id 头做提示缓存 / 粘性路由的网关。实现上从请求 context 里读取 session id 注入，无需改 norma，同一共享 provider 也能按会话发出不同头值。旧库带迁移，前端 LLM 配置弹窗新增输入框。

#### 修复的问题

- 修复保存 LLM 配置时因会话头字段为空直接报 `23502` 的问题：该列是 `NOT NULL DEFAULT ''`，此前误套 `NULLIF($n,'')` 把未填写的会话头写成 NULL 触发非空约束，现按空串语义直接传参。

### Agent

#### 新增的功能

- 墙钟超时改为**就地收尾**：依赖 norma v0.3.6，`MaxDuration` 到点会打断正在跑的工具并在活 ctx 上按收尾轮数续跑（写回已识别内容 + 总结，终态为 timeout），不再依赖 worker/planner 各自的 `maxDur+90s` 外部硬 ctx 把卡住的 run 直接杀成 `aborted_tools`。chat 走同一 harness 自动获得同样行为，mainagent 无 `MaxDuration` 不受影响。
- 停摆兜底：planner 在心跳 / 无变动唤醒且全图已无任何 open 或 running 意图时，开场白改为停摆告警，明确告知无 worker 在跑、无排队方向，本轮必须产出一个或多个互不重复的新意图（不得产出 0 意图）。

#### 修改的功能

- worker 提示词重构：意图 / 启动指令 / 锚定资产原始 JSON 移入 system prompt，每轮重拼、绝不被 compaction 压掉，续跑也不依赖 transcript 首消息留存；启动 user 消息瘦身为仅全局态势 overview（可降级、容忍 stale）。代价是 system 混入 per-intent 数据、失去跨意图缓存复用，是「不丢意图」的刻意取舍。
- 精简 planner / worker 默认正文并修正若干实战问题：planner 对 `recent_done` 按状态区分（blocked/exhausted 先查 trace 再决定，不当死路也不无脑重跑）、否定结论改为「观察 / 存疑、非定论」且采信前看 evidence、目标未达成且无 open/running 意图必须产出（硬底线）、深度优先于覆盖度；worker 否定类结论只写「观察 + 试探性读法」、判决权归 planner，跨意图线索写进 fact 的 summary 交规划者、不自己追。reseed 会把新默认作为新版本追加并切过去，用户自定义 / 旧版本保留在历史可回滚。
- 收敛 work agent 默认工具集与资产写回提示：worker 只负责单条意图的执行与写回，读上下文 / 跨 work 复盘属规划职责，故从其默认工具移除 `list_facts` / `node_detail` / `list_companies` 及跨 work 检索（`search_all_worker_traces` / `list_worker_traces` / `get_worker_trace`），只留 `list_findings`（报漏洞前查重）+ `add_finding` / `record_fact` + `insert_assets` / `list_assets`；提示词删掉与 `insert_assets` schema 冲突的陈旧字段说明（`type=tech`/`on_url`/`props`）。旧库带一次性迁移剥离对应绑定，planner/main 的同名绑定不动。

### 探索图

#### 新增的功能

- **探索图冷节点压缩（cold-digest）**：把老且长期不活跃的意图 / 事实折叠成 digest 节点在 `graph_overview` 里展示，原始节点永久保留、按 id 可完整还原（存储无损、只压呈现、折叠可逆）。冷热判定按逆向可达 + 任一活分支即热，配 R=6 轮防抖与连通分量分组；后台压缩（minor 折未覆盖冷块、major 回源重压合并碎片）带活跃度复核与冷却互斥，绝不上热路径也绝不覆盖已复活节点。概览侧提供 `cold_digests` 与按资产索引的 `cold_index`，`expand_digest` / `expand_index` 负责还原。关联 / 继承任务的概览同样复用其自身折叠视图，`expand_digest` 支持跨任务只读还原。`expand_digest` / `expand_index` 只给 planner 与 main agent，不给 worker。均带旧库迁移。
- `graph_overview` 全量输出 `finding_list`：findings 是任务最高价值产物且单任务通常不多，改为在概览里全量带出（不像 facts 只给最近窗口），planner/worker 每轮即可一眼看全所有确认漏洞，无需再调 `list_findings`。每条精简为 `{id, summary, evidence?, from_intent?, assets?}`，其中受影响资产直接给可读内容（url / 域名 / ip:port）而非裸 id。

#### 修改的功能

- `graph_overview` 不再平铺 `hosts`：coverage 块移除 host 列表（大范围任务里每轮最多携带 500 个 host 字符串，对规划决策价值有限），只保留 `host_count`，具体主机按需 `list_assets` 查。新增顶层 `done_intents_total`（已结束意图总数），与被截断到 ≤15 的 `recent_done_intents` 平行，让 planner 去重时知道有无被截。

### 工具

#### 修改的功能

- `add_company_scope` 默认绑定由 worker 改为 planner：定义企业资产范围属规划 / 主控 / Auto 的职责，worker 只执行探索。新库 seed 默认绑定为 mainagent/planner/auto，老库带一次性迁移。
- planner 默认绑定 `list_assets`：现同时具备 `list_assets`（DSL 全库检索）与 `list_untested_assets`（范围内未测），老库一次性回填、不覆盖用户解绑。

### 任务

#### 新增的功能

- 任务列表新增「运行中 Worker」列：统计该任务下 `state='running'` 的意图节点数，口径与任务详情页概览的「运行中 Worker」完全一致，无运行中 Worker 时显示 0。

### 网络

#### 新增的功能

- 新增**全局出口代理**配置：所有目标流量可经统一的全局代理出网（http/https/socks5，支持 `user:pass`）。开启流量捕获时作为 MITM 记录代理的上游（流量照录再经代理出网，拦截与透传两条路径都走上游、不泄露源 IP）；关闭捕获时直接注入 agent 的 bash 环境与 WebFetch（`proxyEnv` 增加 `ALL_PROXY` 支持 socks5）。配置存于 settings KV 表（无需迁移），前端系统配置页新增「全局代理」卡片，与网络搜索代理、LLM 代理相互独立。

### UI

#### 修复的问题

- 修正意图状态标签的语义错误：`exhausted`「已穷尽」→「预算耗尽」（实为达步数 / 时间预算被中途掐断、只写回部分结果，并非该方向已探尽）、`blocked`「被拦截」→「执行出错」（实为模型 / API / 网络故障重试用尽、意图基本没真正探成，并非目标 / WAF 拦截），并补上此前缺失的 `stopped`「已停止」（用户手动停止的 work）。

### 依赖

#### 修改的功能

- 升级 norma 至 v0.3.4：MCP 工具输出增加截断与落盘（见 `651b961`），后续 v0.3.6 支撑墙钟就地收尾。

### 贡献者

- [@Autumn-27](https://github.com/Autumn-27)

## [0.3.7] - 2026-08-31

### LLM

#### 新增的功能

- 模型配置新增「输出上限」，并可选择上限用哪个请求字段名：上限限制单次回复最多生成多少 token，随每次请求发出，0（默认）= 不发送该字段、由服务端默认值决定；它与「上下文窗口」是两回事——后者是模型总容量，只在本地用来算压缩阈值，不出现在请求里。字段名开关仅对 `openai`（Chat Completions）格式有意义：空（默认）发 `max_tokens`，绝大多数兼容网关只认它；OpenAI 官方推理模型（o 系列 / GPT-5）反过来只认 `max_completion_tokens`，收到 `max_tokens` 会直接报 `unsupported_parameter`，这类端点需手动切到新字段。两种格式各自定死了字段名（Anthropic 为 `max_tokens`、Responses API 为 `max_output_tokens`），故非 openai 格式该开关置灰并在保存时清空。同时补上一条此前从未接通的链路：planner / worker / chat / 主 Agent / 目标拆解五处都没设过输出上限，openai 系压根不发、Anthropic 走 SDK 的 8192 兜底，光加配置项而不接这条线，填了数字也不会生效；现在取值与「流式输出」一样按配置每轮解析，故障转移切换后下一轮即生效。旧库升级后行为完全不变（新列默认 0 与空字段名）。
- 后台点「测试」时把每次 HTTP 尝试的状态码与网关原始响应体打进服务端日志（响应体裁到 4K），便于诊断 401、额度文案、空帧、返回 HTML 页面等情况，不再只能看到 UI 折叠后的 ok / err。该日志不依赖「LLM 记录」开关。

#### 修改的功能

- 任务 LLM 配置链改为任何状态都可修改，不再限制「运行中 / 暂停 / 配置链耗尽」：任务结束（done / failed / timeout）后主 Agent 对话仍走这条链，链上模型出问题时原先既不能改也就没法继续交互。后端 HTTP 与 DB 事务两处终态拦截一并去掉，前端弹窗对终态任务也开放编辑与保存。终态任务保存时不再重开额度阻塞意图（那些意图会被挪到 open，既没有 worker 执行、也不再满足「重跑意图」的条件），想继续跑仍走重跑意图 / 新增目标，由它们把任务重新拉回运行态。

### Agent

#### 修改的功能

- 「给运行中的 Worker 发消息调方向」改为复用已有的暂停 / 恢复 + transcript resume 机制，行为与主 Agent 对话一致：此前走的是一套自建的介入持久化协议，牵动调度屏障、恢复流程与十余处活动查询过滤。现在消息经下一轮输入注入，意图在专属 goroutine 里直接跑，不受 3 槽 worker 池限制、随发随跑；前端保留消息框并恢复「直接继续」按钮，发送改由 SSE 承载。无 DB schema 改动。取舍：消息仅存内存、不做崩溃恢复，且发消息时该任务可能瞬时超并发一个 worker（低频场景，可接受）。

### 任务

#### 修复的问题

- 修复归档大型任务时内存耗尽（OOM）崩溃的问题：归档改为流式写入快照，不再把整个任务一次性读进内存；同时补齐冷归档路径上的三处恢复缺口——现代安装的流量只存在 SQLite 里，此前在 PostgreSQL 提交与 SQLite 提交之间崩溃会让已归档任务的流量滞留在热存储且无从恢复，现在无论有无历史目录都会写暂存日志，缺失的 `journal.json` 也按可丢弃处理。
- 修复任务较多时系统卡顿的问题：加载对话与加载界面存在明显延迟。任务列表、任务上下文与探索记录的查询一并优化，前端的仪表盘、对话页与任务详情页同步减少了重复请求。

### 资产

#### 修改的功能

- 去掉企业资产范围「最多 256 条规则」的限制：逐个 IP / 域名录入范围的企业很容易撞上这个上限，撞上之后只能拆成多个企业，而范围本身并不因为条数多就更慢。前后端与 demo mock 三处上限一并移除，单条规则仍限 1024 字符，请求正文 2 MiB 的上限保留作兜底（约合四五万条规则）。

### 技能

#### 修复的问题

- 修复上传技能压缩包报「上传失败：zip: unsupported compression」的问题：Go 标准库只内置 Store / Deflate 两种解压器，压缩软件在非默认档位下写出的 bzip2、Zstandard 包一律解不开。现在补上这两种解压器（纯 Go 实现，不引新的外部依赖）；Deflate64 / LZMA / XZ / PPMd 等确实解不了的方式，以及加密压缩包，改为在解压前就报出中文提示，直接点名是哪个文件用了哪种压缩方式、该怎么重新打包，不再把底层英文错误甩给用户。
- 修复技能内文件名校验把中文文件名判为非法的问题：路径校验原先是 `[A-Za-z0-9-_./]` 的 ASCII 白名单，压缩包里只要有一个中文命名的文件（`参考/说明.md` 之类），整包上传就会以「压缩包含非法路径」失败。改为 Unicode 黑名单：允许各种语言的文件名与空格，仍然拒绝控制字符、非法 UTF-8、零宽与双向控制字符（RLO 文件名伪装）、`\ % # ? * : " < > |` 以及 `..` / 绝对路径 / 空路径段，防目录穿越的行为不变。技能名同样放开——ASCII 仍限小写字母、数字与连字符（agentskills.io 规范），中文等非 ASCII 字母可直接作技能名，但不接受空格、点和路径分隔符。
- 修复 Windows 压缩软件打出的中文包解压后文件名乱码或整包被拒的问题：这类 zip 不置 UTF-8 标志位、按 GBK 写文件名，现在按 GBK 兜底解码后再做路径校验。同时 `name: "中文技能"` 这种带引号的 frontmatter 也能正确取到技能名。

### UI

#### 新增的功能

- 发现页新增「全部发现」平铺视图并设为默认，与原「按任务分组」视图通过页头 Tab 切换：分组视图要逐个任务展开才看得到漏洞，只想跨任务扫一眼列表时反而绕路。平铺视图是一张跨任务大表（每页 10/20/50/100 条），行为与分组视图完全一致——勾选导出、行内展开看证据与详细报告、行内改名称/类别/严重度、改处置状态、深入、删除。两个视图共用统计卡片与筛选条，切换不丢筛选，当前视图与筛选项一并记在本地；轮询只打当前视图，行内改动会同步写两边缓存，切过去不会看到过期数据。
- 发现页新增「按资产」视图：左侧资产树（企业 → 根域名 / IP → 子域名 → 服务 → 接口，企业层只在资产确实有归属时才出现），右侧是选中节点整棵子树下的发现，与另外两个视图共用同一张表和同一批筛选条件。树只收有发现的资产，祖先链按需补齐（发现只挂在最深的接口上时，上面几层照样能拼出来），节点上的计数是子树聚合并按发现去重的；资产已删除或本就没关联资产的发现进「未关联资产」桶。与另外两个视图不同，资产视图不轮询——进入视图、改筛选、页内增删改发现、或点树上的刷新按钮时才查询，左树是导航结构，没必要每 5 秒重算。树上每层只显示相对上一层的增量（子域名去掉根域名后缀、服务显示 `https :443`、接口只显示路径），完整值在悬停提示与面包屑里；节点过多时整层丢弃接口/服务层级并给出提示，计数仍计入上层。

#### 修复的问题

- 修复手机端会话详情里会话记录被挤压看不全的问题：窄屏下折叠会话列表，把宽度让给记录本身。

### 依赖

#### 修改的功能

- 升级 norma v0.3.2 → v0.3.3：新增 `Config.MaxTokensField`，让 OpenAI Chat Completions 的输出上限可以改发 `max_completion_tokens`（推理模型只认这个键）。两个键互斥、只发其中一个，默认仍是 `max_tokens`。
- 升级 `golang.org/x/mod` v0.37.0 → v0.40.0，修复两处依赖安全告警。

### 贡献者

- [@Autumn-27](https://github.com/Autumn-27)
- [@neouks](https://github.com/neouks)
- [@begininvoke](https://github.com/begininvoke)

## [0.3.6] - 2026-08-27

### LLM

#### 修复的问题

- 修复「非流式」开关保存后重新打开又变回「流式」的问题（#69）：配置列表接口的 DTO 漏了 `streaming` 字段，响应从不返回它，前端读到 `undefined` 后一律回落成默认流式；实际写入与 DB 存储本无问题，只是读不回来。DTO 补回该字段（不加 `omitempty`，`false` 也必须出现在响应里）。
- 测试连接改为校验模型确有回复、并按该配置真实的收发模式来测（#65）：此前只判 HTTP 是否成功，请求通但模型零回复（思考烧光预算 / 正文被安全策略吞掉 / 兼容层丢 `content`）也报「连接成功」，与会话里「不回话」的表现割裂；且测试恒走流式，非流式配置测的其实是另一条通道。现在空回复直接判失败、成功时在提示里展示模型回复，并把当前流式开关一并带入测试，让「测试通过」与「会话跑得通」保持一致。

### Agent

#### 修改的功能

- `list_facts` 改为分页 + 关键词过滤，避免事实多时单次调用撑爆上下文（#74）：默认返回最新 20 条，支持 `limit`（上限 100）/ `before` 游标 / `q` 摘要关键词，返回 `{facts, total, has_more, next_before}`；单条 summary 过长按字数截断（全文仍走 `node_detail`）。worker / planner 提示词同步为分页语义。旧库通过一次性迁移把新参数 schema 刷进工具目录表（`SeedTool` 首插入only，否则工具管理页显示「无参数」）。

### 工具

#### 新增的功能

- 「工具执行」页新增工具调用次数统计（#72）：工具栏「统计」按钮打开弹窗，按工具展示调用次数、占比与失败次数（次数降序）；沿用列表的任务 / 关键词筛选，统计整个结果集而非当前页，弹窗打开时才拉取。

### 依赖

#### 修改的功能

- 升级 norma v0.3.1 → v0.3.2：修复 OpenAI 系响应里被静默丢弃的 reasoning/refusal（Responses 补 `reasoning_text`）、reasoning 字段三名去重、空响应（零内容块）有界重试、压缩边界切断工具配对导致的网关 400（同时修复已烤进 transcript 的存量孤儿）。

### 贡献者

- [@Autumn-27](https://github.com/Autumn-27)

## [0.3.5] - 2026-08-25

### LLM

#### 新增的功能

- 每个 LLM 配置支持流式/非流式切换（默认流式）：开走流式 SSE；关走真·非流式（`stream:false`、一次性返回完整 JSON），可绕开部分网关糟糕的 SSE 实现（空帧、思考字段丢帧），代价是失去运行中的实时进度与实时 Token 计数。worker / planner / mainagent / chat / goals 均按当前激活配置动态取值；`llmpool` / `llmrec` / 任务运行时三层 Provider 包装均兼容非流式；`llm_profiles` 新增 `streaming` 列并 `ALTER` 补旧库（默认 `true`，旧配置无感）。
- 支持 OpenAI Responses API 格式的 LLM 配置：每个配置新增第三种格式 `openai-responses`（打 `POST /v1/responses`），与 Chat Completions / Anthropic 并列；`BaseURL` 归一化、默认模型 `gpt-5`；`llm_profiles` 的 `format` 约束加入 `openai-responses` 并幂等迁移补旧库；前端格式下拉新增「OpenAI (Responses API)」。依赖升级 norma v0.3.1（含 `reasoning_content` 回传修复）。
- 录制 LLM 请求/响应 HTTP 原文：在 HTTP transport 层捕获真实 wire body，保留归一化视图看不到的工具 schema、`tool_use` 块与原始 SSE 帧；norma 内部重试的多次尝试逐次保留；`llm_records` 新增 `raw_request` / `raw_response` 两列并 `ALTER` 补旧库；录制页详情面板增加「原文」视图切换与请求/响应复制按钮（兼容非安全上下文的 `execCommand` 回退）。

### 对话

#### 修改的功能

- 会话列表多选改为「多选」模式开关：默认列表不再常驻每行勾选框（观感更干净），表头改为「共 N 个 + 多选」按钮；点「多选」进入选择模式（勾选框、全选、批量删除），「完成」退出并清空选择，批量删除全部成功后自动退出（有失败则留在选择模式便于重试）；单条重命名 / 置顶 / 删除仍走每行 ⋯ 菜单。

### UI

#### 修改的功能

- 任务管理、会话操作与流量查看体验增强（#57）：任务列表列排序偏好可持久化记忆、行内重命名改用图标直接触发（不再经菜单）、抽屉（sheet）交互与流量查看细节优化。

#### 修复的问题

- Worker 资产标签只显示域名与 IP，并正确处理空标签的场景。

### Agent

#### 新增的功能

- planner 判目标时新增「量化验收核对」：目标含可量化验收条件（资产测试覆盖度达到 X%、拿到 N 个 flag、获得某权限）时，`prove_goal` 前必须核对 `graph_overview` 的实测值（`coverage.pct`、findings 计数等），实测未达标则禁止 `prove_goal`、改为派意图补足差距，不得以「主要部分已完成/大体达成」为由提前标 met。修复了目标要求覆盖度 100%、实测仅 40% 却被判完成的问题。

#### 修改的功能

- 重写 planner 提示词中「本轮 0 意图」的判定依据：原文将 0 意图描述为「最常见、最重要的原则」，会让规划者在目标未达成、范围内仍有未测面时过早收手。现改为只在两种具体情况下才应产出 0 意图——① 想到的方向都已被 open/running/recent_done 意图覆盖；② 下一步依赖当前正在运行的 work 的产出、而产出尚未出现（应等其跑完、图更新后的下一次唤醒再规划）。并补充反向约束：确有未被覆盖且不依赖在跑 work 的新方向、或目标未达成且仍有未测面时，不要因「0 意图常见」而收手。
- 强化 worker 提示词中「否定结论的证据门槛」：对「不可注入/端口关闭/无登录入口」等可能让规划者放弃一整条方向的否定结论，要求下结论前先穷尽该意图内的合理手段（换编码/参数/路径/方法）；手段未走完或证据偏弱一律标 `confidence=inferred`，避免用轻率的 `observed` 否定把整条路线焊死（任务早期的错误否定尤其会把方向带偏且难以自愈）。
- 上述 planner / worker 默认提示词变更通过一次性迁移（`reseedPlannerPrompt` / `reseedWorkerPrompt`，各带 settings flag 守卫）追加为新版本并切为当前版本；旧版本保留在版本历史中，自定义过提示词的用户可在版本记录里恢复。

### 运维

#### 新增的功能

- 新增 `reset-password.sh` 重置管理员（用户名固定 `ARTEX`）密码：支持 local / docker 两种部署，连接信息可显式指定或自动从 `--dsn`/`$ARTEX_PG_DSN`/`config.json` 读取；在库内用 `pgcrypto` 生成与后端登录兼容的 bcrypt 哈希并写回 `settings.auth.password_hash`，重置后无需重启服务。密码经环境变量传入、不进入进程 argv，并做转义防注入。

### 贡献者

- [@Autumn-27](https://github.com/Autumn-27)
- [@neouks](https://github.com/neouks)

## [0.3.4] - 2026-08-24

### UI

#### 新增的功能

- 任务列表勾选多个任务后可批量修改分类，目标分类支持「未分类」以将任务移出当前分类；整批写入在一个事务内完成，失败项只会是勾选后已被删除的任务。

#### 修改的功能

- 新建任务的「任务分类」由下拉选择改为可搜索输入框：输入即过滤已有分类，库里没有的名称回车（或点下拉里的「创建」）即时新建并选中，选中项以可移除的 tag 展示；仍限单个分类。

### 对话

#### 修复的问题

- 修复会话已选定 LLM 配置、但全局无激活配置时，发送消息被「LLM 未配置，无法对话」拦截的问题。发送前的检查原本只看全局激活配置，而实际执行会优先用会话所选配置，两处解析逻辑不一致；现统一为同一套（会话/Agent 绑定优先，全局兜底）。
- 无法对话时的提示按状态细分：完全没有配置提示「添加配置」，有配置但未激活提示「激活或为本会话指定一个配置」，不再一律显示「未配置」，便于定位。

### Agent

#### 新增的功能

- 目标已全部达成的任务，主 Agent 经 `add_intent` 下发的意图可被 Worker 直接领取执行、跑完即止：此时 Planner 不再运行（任务无 open 目标即不进规划），避免其重判目标达成而取消刚下发的意图；frontier 抽干后任务回到已完成。主 Agent 下发意图前会依据意图内容反问用户是否将其登记为正式目标，登记则任务恢复常规自主规划。

#### 修改的功能

- `get_worker_trace` / `get_task_worker_trace` 的 `step_ids` 超过一次上限（5 个）时不再直接报错，改为返回前 5 步的完整内容，并在结果中通过 `returned_step_ids` / `omitted_step_ids` / `notice` 告知本次取了哪些、还有哪些未取，并提示「若已足够则无需再取」。传入的重复与非法 id 会先去重、剔除后再计数。

#### 修复的问题

- 任务列表改为按任务 id 降序（最新创建在最前），替换原先的按创建时间排序。同一时刻创建的多个任务时间戳相同、排序不稳定，配合每 10 秒轮询和内存 map 的随机遍历，会导致它们在列表里频繁互换位置。

### 资产

#### 修复的问题

- 修复企业归属重算被单行脏数据打挂：`assets.ip` 存了主机名（Agent 或资产 API 写入）时，`a.ip::inet` 会抛 `22P02`，导致新增/修改企业资产范围和删除企业全部失败并回滚。改用安全转换 `try_inet()`，非法值跳过而不是中断整条语句。
- 无法解析的 `ip` 不再静默跳过：企业范围保存后会明确提示有多少条资产的 `ip` 不是合法 IP（含具体 id 和取值），提醒这些资产不会被 IP/CIDR 规则归属；同一信息也写入服务端日志，覆盖没有前端响应的路径（删除企业、scopesentry 同步、Agent 写入）。
- 任务测试范围的 IP/CIDR 匹配同步改用 `try_inet()`，替换原先只检查字符集的正则守卫（`abc.def` 这类全 hex 字母的主机名此前仍会漏过并触发同样的报错）。

#### 修改的功能

- `ip`、`service`、`endpoint` 资产的 `ip` 字段不再接受主机名：`insert_assets` 和资产 API 会逐条返回带 `index` 的错误，并直接给出改正方法（改用 `type=subdomain` 填 `domain`，或先解析 A/AAAA 记录），Agent 可据此自行修正后重插。同批次中的其他资产不受影响，照常入库。

### 贡献者

- [@neouks](https://github.com/neouks)

## [0.3.3] - 2026-08-23

### Worker

#### 新增的功能

- 运行中的 Worker 支持单项暂停、恢复和取消；暂停保留意图、事实与漏洞，取消会在执行退出后事务性清理当前意图及其直接产物。
- 增加 `paused` 意图状态、执行栅栏和具名终止原因，防止暂停、取消或任务删除后出现迟到黑板写入。
- 增加可选任务并发上限；新建、恢复、漏洞深入和队列补位统一通过持久化 FIFO 准入路径。

#### 修改的功能

- Worker 单次运行墙钟默认时长由 600 秒调整为 1200 秒；超时任务复活后在下一次真实执行时重新计时。
- Worker、Planner 和主 Agent 的真实执行状态统一驱动任务状态徽标，修复 Worker 运行时仍显示“空闲”的问题。
- Worker 会话保留单项控制和当前会话 Token 汇总，模型名称移动到当前会话标题右侧展示。

#### 删除的功能

- 删除 Worker 多选、全选及批量暂停/继续界面，同时移除 Worker 批量控制 API 和 Mock 契约。
- 删除 Worker 列表行中的 Token 徽标与 Tooltip，底层完整 Token 账本和任务聚合接口继续保留。

### LLM

#### 新增的功能

- 任务支持有序 LLM 配置链；明确识别供应商额度不足时自动切换下一配置，并持久化当前配置、耗尽状态和错误摘要。
- 运行中和暂停任务支持编辑完整配置链、调整顺序和手动切换当前配置，从下一次 LLM 调用生效。
- 自动切换、手动切换和全链耗尽写入结构化系统活动，并通过任务活动流弹出可去重提醒。
- 增加任务角色模型解析接口，统一解析 Main Agent、Planner 和 Worker 下一次调用使用的配置与模型。
- 增加可选的全局 LLM Pool，支持配置调用顺序、指定模型失败兜底、健康状态、冷却恢复和手动重置。
- 增加始终开启的 LLM 用量账本，按任务、会话、模型和配置聚合输入、输出及缓存 Token。

#### 修改的功能

- 目标拆解、Planner、Worker 和主 Agent 共享任务级 LLM runtime（解析优先级见下）。
- 故障转移只响应明确的额度、余额或账单错误；普通限流、鉴权、网络、服务端和上下文错误不会错误切换。
- LLM 配置页改为配置卡片列表和右侧抽屉编辑，并提供 Pool 轮询顺序、优先级、排除项、健康状态和恢复操作。
- 配置链耗尽信息支持窄屏换行；当前模型由会话列表图标改为当前会话标题旁的文字标签和完整 Tooltip。
- 任务内各角色解析 LLM 的顺序调整为「Agent 绑定 → 任务配置链 → 全局」：显式绑定模型的角色始终跑在该模型上，未绑定角色才落到任务链，任务链为空时再落到全局。
- 出站代理留空即直连，不再回退 `HTTP_PROXY`/`HTTPS_PROXY` 环境变量（显式 `ARTEX_LLM_PROXY` 不受影响）；代理输入支持带账号密码的 `socks5://user:pass@host:port`。

#### 修复的问题

- 提交前（尚未产出任何输出）的瞬时流式失败在同一 provider 上安全退避重试，显著减少「只跑了一两个工具就中断、无总结、终态 `model_error`」的情况；额度耗尽、上下文过长和 4xx 确定性错误不重试，仍分别交由故障转移与压缩恢复处理。

#### 删除的功能

- 删除会话列表中的 LLM 图标式标识，避免与 Worker 状态图标混淆。

### UI

#### 新增的功能

- 任务支持全局分类的创建、重命名、删除和筛选；新建任务可直接选择分类。
- 增加全局任务模板 CRUD 和右侧管理抽屉；新建任务可加载预设描述与目标，也可将当前内容另存为模板。
- 新建任务支持关联多个来源任务和多个企业资产范围；企业范围通过单个多行文本框自动识别域名、URL、IP、CIDR、ICP 和企业关键词。
- 任务测试资产支持实时新增和删除，记录人工、企业、继承或 Agent 发现等来源；Worker 会话标题旁显示当前测试资产及来源摘要。
- 发现页按任务分组并提供组内独立分页，支持为漏洞填写描述并创建高优先级深入意图。
- 对话支持重命名、置顶、取消置顶和删除；任务列表支持当前页批量暂停与继续。
- 流量详情和工具执行详情改为右侧抽屉；HTTP 报文补齐 `Host` 并高亮请求行、状态码、Header、JSON 和标记语言正文。
- 任务列表操作列新增「详情」与「暂停/继续」按钮；会话发送键位可在系统设置中配置（`Enter` / `Cmd+Enter` 等），Web 搜索区新增代理输入框。
- 会话标题右侧徽标改为显示 LLM 配置名称，模型 ID 移至 hover Tooltip；任务可设置可选名称，任务列表新增名称列与搜索（空则回退到描述）。
- 技能页增加使用次数、最近使用时间和缺失依赖统计，便于定位未生效或未安装的技能。

#### 修改的功能

- 任务标题改为可聚焦详情链接；任务资产、企业资产、发现任务组和组内漏洞使用服务端真实分页、稳定排序与精确总数。
- 新增企业改用右侧抽屉，范围录入统一为多行文本并提供实时识别、校验和类型预览。
- 主 Agent 输入框支持自动增长、`Enter` 发送、`Shift+Enter` 换行，并避免中文输入法组合态误发送。
- Agent 预览、任务报告和相关详情统一使用共享 Markdown 渲染；删除确认、长错误和移动端抽屉宽度统一修复。
- 任务模板选择框以模板名称显示和搜索、以模板 ID 提交；恢复系统原始字体尺寸，应用版本统一显示为 `0.3.3`。
- 任务总计和当前会话 Token 只高亮输入、缓存读取和输出数值；发送按钮使用更简洁的上箭头图标。
- 任务总览的测试范围显示企业名称而非 ID。

#### 修复的问题

- 含点号的普通文本不再被误判为 ICP 备案号。
- 修复变量目录中与全局运行时变量（如 `{{.Now}}`）撞名，导致 Agent 编辑器变量列表渲染出重复 key 的问题。

#### 删除的功能

- 删除任务卡片独立“进入”按钮，统一点击任务标题进入详情。
- 删除仪表盘“新建任务”按钮、新增企业的 Logo URL 输入项，以及发现页顶部和任务分组中的漏洞数量 Badge。
- 撤销全局字体增大 10% 的样式，并删除会话列表中的 Worker 选择框、模型图标和行级 Token 统计。

### Agent

#### 新增的功能

- 新任务可直接关联多个已有任务，实时只读继承直接来源任务的目标、事实、漏洞、已完成意图、资产范围和黑板上下文。
- 黑板读取工具支持按需查询来源任务节点、事实、漏洞和执行轨迹；继承节点带来源标记且所有写工具拒绝修改。
- 企业范围工具支持域名、URL、IP、CIDR、ICP 和企业关键词；关键词仅作为 Agent 范围提示，不参与资产自动归属。
- 漏洞深入操作会在原任务创建带资产锚点和 `derived_from` 边的高优先级人工 Worker 意图。
- 总览新增「目标管理」卡片，支持人工查看、新增、修改和删除目标；新增或修改会通知 Planner 并复活任务，删除硬删目标节点（级联删边与锚点）但不复活。
- 主 Agent 默认获得 `steer_work` 工具，可对运行中的 Worker 实时注入纠偏指令而不打断、不丢失进展（仍校验意图归属本任务）。

#### 修改的功能

- 来源任务意图不进入新任务 frontier，新任务继续使用独立 exploration、执行队列、工作目录和对话历史。
- 删除运行中的意图不再销毁数据：改为停到 `stopped`、要求填写删除原因、把原因作为事实挂到该意图并写入 payload，规划者按 `cancelled` 触发感知（保留意图内容与原因）。
- 主 Agent 对话改为服务端活动流驱动，发送失败可恢复输入；打开会话时自动贴底并在详情懒加载后保持最后回复可见。
- 任务暂停会终止当前 Main Agent、Planner 和 Worker 调用，但不阻止暂停期间用户主动发起新的主 Agent 编排会话。
- 取消、关闭和流式中断统一保留真实终止原因、已生成内容、运行轮数、耗时、Token 与未返回工具调用。

#### 删除的功能

- 删除主 Agent 客户端乐观消息回显，避免暂停、失败或并发活动下出现重复消息和跨会话串流。

### 约束

#### 新增的功能

- 目标拆解阶段先抽取操作约束（allow/deny）再拆目标，主 Agent 运行时可补充；约束按最高优先级注入 Planner 与 Worker 系统提示，去偏后多样性与拓面探索显式服从约束。
- 总览新增「约束管理」卡片与增删改接口；约束注入范围可按 Planner / Worker 分别开关（默认都开，每轮读取、即时生效）。新表 `task_constraints` 使用 `CREATE TABLE IF NOT EXISTS`，旧库自动补建。

### 拦截

#### 新增的功能

- 命令拦截在正则/字符串规则之外新增模型兜底审批：一条规则都未命中时由模型做 `ALLOW`/`ASK`/`DENY` 语义判定，失败回退动作与审批超时动作可配置；拦截页拆分「拦截规则 / 模型配置」两个 Tab，模型判定结果标注 `[模型]` 前缀与理由。

#### 修复的问题

- 加固裁判输出解析，避免正确的 `DENY` 判定被误当作放行。

### 流量

#### 修改的功能

- 流量录制改为整条 exchange 落 SQLite（大正文溢出到按 hash 去重的 blob 桶），文本正文建 trigram 全文索引，支持任意子串与中文搜索；删除退化为单条 SQL 事务，从数小时降到毫秒级且期间不再停摆录制。新增大 body 流式下载与 `traffic_search` 全文参数；旧文件树数据免迁移，仍可读、可搜、可删。

### 任务与资产

#### 新增的功能

- 创建任务新增「资产覆盖度功能」开关（默认开）：关闭后不计算与展示覆盖度、态势图只显示资产、不自动累积 `task_scope`，相关工具从 Planner / 主 Agent 剔除；企业关联不受此开关影响。
- `insert_assets` 每条资产新增 `related` 标记（默认 `true`）：仅在开启覆盖度时生效，`false` 只入共享资产库、不计入本任务覆盖度（如顺带发现的旁站或无关资产）。
- 企业范围和任务测试资产统一支持域名、URL、IP、CIDR、ICP 与关键词文本识别；域名/IP 可创建或复用全局资产，CIDR/ICP/关键词保留为任务范围上下文。

### 构建

- `build.sh --release` 支持一次构建 Linux amd64/arm64、macOS amd64/arm64 和 Windows amd64，并生成带 `skills/`、配置示例和 README 的 zip 发布包。
- 使用 Go linker 去除调试信息并生成 zip 发布包；UPX 改为显式可选，避免其自解压 ELF 在部分 Linux 环境启动时发生段错误。
- Release Workflow 对 Linux amd64 二进制执行启动冒烟测试，并随发布包提供 `SHA256SUMS`。

### 贡献者

- [@neouks](https://github.com/neouks)

## [0.3.2] - 2026-08-20

### Added

- 新建任务支持关联多个已有任务，实时、只读继承直接来源任务的事实、漏洞、已完成意图、资产范围和黑板上下文；新任务仍使用独立的执行队列、工作目录和会话历史。
- 任务支持有序 LLM 配置链。明确识别到供应商额度不足时自动切换到下一配置，并持久化当前配置、耗尽状态和结构化审计活动。
- 运行中和暂停任务支持编辑 LLM 配置链、调整顺序及手动切换当前配置；自动切换、手动切换和全链耗尽会在任务详情页提示。
- 运行中的 Worker 支持暂停、恢复和取消。暂停保留黑板数据，取消会在 Worker 停止写入后事务性清理该意图及其直接产出的事实、漏洞和执行记录。
- 删除任务可选择同时清理关联资产、流量、漏洞和任务落地文件，并增加删除屏障、并发保护和可审计的删除统计。
- 增加可选的任务并发上限；达到上限的新任务按 FIFO 排队，并在运行槽位释放后自动启动。
- 增加任务资产分页、公司资产分页、任务分类统计及分页意图 Mock 契约。
- 增加 `build.sh` 单二进制构建脚本，支持静态前端导出、资源嵌入、跨平台目标和构建版本注入。

### Changed

- 任务级显式 LLM 配置链与现有全局 LLM Pool 并存；显式链继续采用严格的额度故障转移语义，无显式链时沿用 Agent 绑定和全局配置规则。
- 主 Agent 输入框支持自动增长的多行输入，`Enter` 发送、`Shift+Enter` 换行，并避免中文输入法组合态误发送。
- Planner、Worker 和主 Agent 共享任务级 LLM runtime；任务链使用候选模型中最小上下文窗口作为安全压缩阈值。
- 任务详情页按真实 LLM 调用状态展示运行、空闲和暂停状态；来源任务的事实、漏洞、意图、资产引用和图谱节点统一标记来源并保持只读。
- Agent 预览、任务报告和相关详情视图统一使用共享 Markdown 渲染组件。
- LLM 模型配置采用卡片和抽屉交互，模型列表在抽屉内支持独立滚动。
- 任务暂停不再拦截主 Agent 对话：主 Agent 编排会话独立于任务暂停，暂停中仍可继续发送新消息（暂停只终止当前进行中的那一轮）。
- Worker 单次 run 的墙钟默认时长由 600 秒调整为 1200 秒。

### Fixed

- 移除主 Agent 控制台的乐观回显，改为纯服务端数据渲染，修复暂停或发送失败等场景下消息错乱、串入其他会话内容的问题。
- 打开主 Agent 会话默认滚动到底并完整显示最后一条回复：最后一条回复的完整内容懒加载展开后自动贴底，不再被顶出屏幕。
- 修复暂停任务后主 Agent 会话仍继续运行，以及编排 Agent 暂停任务时未同步停止主 Agent 的问题。
- 修复任务完成、Planner、Worker 或主 Agent 实际运行时，任务状态徽标与操作按钮状态不同步的问题。
- 修复 Worker 取消、任务删除和并发写入之间可能产生迟到黑板写入或残留文件的问题。
- 修复 Markdown 预览失效、删除确认长文本溢出、移动端宽度和部分任务详情按钮未对齐的问题。
- 为所有 Agent 取消路径增加具名终止原因，活动详情可显示取消方、终态、运行轮数、耗时、Token 使用量和未返回的工具调用。
- 修复后端关闭时父 context 可能抢先覆盖具名 `shutdown` 原因的竞态，以及中文活动摘要按字节截断导致乱码的问题。
- 修复带部分流式输出的取消事件丢失真实终止原因的问题，同时保留取消前已生成的内容。

### 贡献者

- [@Autumn-27](https://github.com/Autumn-27)
- [@neouks](https://github.com/neouks)

[Unreleased]: https://github.com/wufufu770/reArtex/compare/v0.3.10...HEAD
[0.3.10]: https://github.com/wufufu770/reArtex/compare/v0.3.9...v0.3.10
[0.3.9]: https://github.com/wufufu770/reArtex/compare/v0.3.8...v0.3.9
[0.3.8]: https://github.com/wufufu770/reArtex/compare/v0.3.7...v0.3.8
[0.3.7]: https://github.com/wufufu770/reArtex/compare/v0.3.6...v0.3.7
[0.3.6]: https://github.com/wufufu770/reArtex/compare/v0.3.5...v0.3.6
[0.3.5]: https://github.com/wufufu770/reArtex/compare/v0.3.4...v0.3.5
[0.3.4]: https://github.com/wufufu770/reArtex/compare/v0.3.3...v0.3.4
[0.3.3]: https://github.com/wufufu770/reArtex/compare/v0.3.2...v0.3.3
[0.3.2]: https://github.com/wufufu770/reArtex/compare/v0.3.1...v0.3.2

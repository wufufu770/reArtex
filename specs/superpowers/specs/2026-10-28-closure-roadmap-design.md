# reArtex 闭合路线图 · 总纲设计文档

| 项 | 值 |
| --- | --- |
| 日期 | 2026-10-28 |
| 仓库 | 本地克隆的 `ARTEX`（module `github.com/Autumn-27/artex`） |
| 分支 | `main`（= `reArtex/main` = GitHub `wufufu770/reArtex`），基线 `8165abf` |
| 状态 | 待实施（总纲；每个波次另立 spec 与实施计划） |
| 范围决策 | 用户列出的 6 项「未来打算改什么」+ 8 项「已知残留」全做；**建议补入 1 项**（G-04①② 判官分侧，账本长期挂「未解决」，且与 G-09④⑥ 同属 V2 W1 前置步）；**本次新决策 1 项**（A0 分级审核改造，Sir 2026-10-28）。算式：`6 + 8 + 1 + 1 − 1 = 15`——减的 1 是「无独立签名」与「产物签名与来源证明」实为同一件事，合并为 B1。合计 **15 项**（A0–A8 共 9、B1–B5 共 5、C1 共 1） |
| 路线决策 | **两层**（Sir 2026-10-28 决定）：W-A/W-B 不动容器权限模型、不改上游 norma 契约；per-task 隔离作为 W-C 独立阶段，先出设计再单独授权 |
| 审核决策 | **分级审核**（Sir 2026-10-28 决定）：人工审核只加在「特别危害」的面；低危面按 ARTEX 原样（不审核、可继续循环）。实现上把短路表从「ask 为主」改为 **deny / ask / 显式 allow 三档** |
| 交付形态 | reArtex 自有发布线（不跟上游 tag），每波次独立 spec → 实施计划 → 验收 → 提交 |

---

## 0. 背景与证据

### 0.1 本轮实测到的事实（写进计划前先核过，不凭印象）

| # | 事实 | 证据 |
| --- | --- | --- |
| E1 | **平台工具的判定插在工具表最前**，`Deny` 落在权限判定的 Stage 1，任何 permission mode 都绕不过 | `agent/toolpolicy.go`：`policyTool.CheckPermissions` → `pathguard.Check` → `acperm.Denied`；六个 agent 会话全部 `ModeBypass`，而 ModeBypass 只短路 Stage 2 |
| E2 | **入参真路径解析已存在，但只覆盖路径类工具**（Read/Write/Edit/MultiEdit/LS/Glob/Grep），Bash 不在其中 | `pathguard/pathguard.go`：`pathParams` 无 Bash；注释自陈「Bash 的命令串里路径要经 shell 展开才现形，字符串层判不准」 |
| E3 | **相对路径解析基准可从框架层拿到**，无需改上游 | norma `permission.Context` 有 `WorkingDir` 字段；`policyTool.CheckPermissions(ctx, input, pc)` 的 `pc` 即该 Context |
| E4 | **norma 是外部 module，无 vendor**（`github.com/Autumn-27/norma v0.4.4`），改其源码需 fork/replace | `go.mod`；仓库内无 `vendor/` |
| E5 | **进程树环境块里已无非空平台秘密**：LLM key 在 env 中为空值，DSN 走 `ARTEX_PG_DSN_FILE` 且启动即读入即删 | 账本 A1/A2 验收；本轮 `docker exec` 复核：`OPENAI_API_KEY`/`ANTHROPIC_API_KEY` 长度 0 |
| E6 | **但容器配置面仍带 DSN** → `docker inspect` 可见口令 | 本轮 `docker inspect`：`ARTEX_PG_DSN=postgres://artex:<口令>@postgres:5432/artex?sslmode=disable` |
| E7 | **`llm_profiles.api_key` 是明文 TEXT 列** | `db/schema.sql:369`、`db/config.go:168`（旁有 `api_key_hint` 用于掩码展示） |
| E8 | **真实口令只落在未跟踪的 `.env`**，版本库跟踪文件里只有测试夹具与 CI 一次性凭据 | `git grep` 命中仅 `db/pgdsn_env_test.go`、`server/customtool_env_test.go`、CI 的 `artex:artex`；`.env` 已被 `.gitignore:52` 忽略 |
| E9 | **G-17 审计链已闭合**（①判定动作落 `intercept_pending` ②`audit_events` 哈希链，可验断点） | 账本 §D.9 与 `db/audit_chain.go` |
| E10 | **G-04①②（判官分侧 fail→deny、ask 默认超时）与 G-09④⑥（ask 队列治理）仍为「未解决」** | 账本 §5.2 裁定表 |
| E11 | `release.yml` 无签名/SBOM/provenance；冒烟只验 linux/amd64；Compose 默认 `latest`（可变标签） | `.github/workflows/release.yml` |
| E12 | 测试隔离**只到包级**（每包一个独立库），包内用例仍共享一库 | `internal/testdb/testdb.go` |

### 0.2 为什么要总纲而不是一份实施计划

这 15 项里有 4 组**互相独立**的子系统（安全收口 / 交付可信 / 工程隔离 / 架构级隔离），任一组都有独立的失败模式与回滚面。按 brainstorming 的分解原则，**总纲只定"分几波、每波闭合什么、怎么验证、依赖谁"**，每波次另立 spec 与实施计划；混成一份计划会让每一步都无法独立验收。

---

## 1. 目标与成功标准

**总目标**：把 reArtex 从「以纵深防御为主、留有若干已登记活路径」推进到「已登记残留逐条闭合或明确降级为已接受的残余风险」，且**不牺牲 agent 的日常可用性**。

**成功标准（可断言）**：

1. §3 表中 15 项**每项都有明确的"闭合手段 + 验证断言 + 成本"**，且验证断言是可执行的命令或测试，不是"检查一下"。
2. 每一项闭合都留下**可回归的门**（测试 / CI 步骤 / 自检脚本），不是一次性的人工核对。
3. 账本口径与实现一致：任何"闭合"都有断言支撑；任何"降级"都改口径并说明理由。
4. **agent 日常路径的摩擦不增加**：低危操作按 ARTEX 原样放行、可继续循环（§2 的分级审核是硬约束）。
5. 任何"闭合"都不宣称"绝对安全"——本方案的最外层（容器/特权面）不由本方案覆盖（§6 诚实声明）。

---

## 2. 设计原则：分级审核（Sir 2026-10-28 决定）

### 2.1 问题

原短路表把**改平台配置的工具一律 ask**（60s 超时 deny）。这些工具里既有"改既有 agent 行为/删证据/新增可执行面"（确实高危），也有"新建一个技能文件"（日常运维）。一律 ask 会让 agent 在正常任务里频繁被打断——**用户体验下降，而拦住的并不是真正的高危面**。

同时 G-09④⑥ 的原始设想（per-agent × 时间窗聚合计数 → 超阈值转 deny + 暂停派发）如果做成"所有 ask 都计数并逐步升级"，效果同样是给日常路径加摩擦。

### 2.2 三档判定

| 档 | 语义 | 实现位置 | 人工审核 |
| --- | --- | --- | --- |
| **deny** | 硬拦，无放行路径 | 短路表最前（`guard/shortcircuit.go`） | 无 |
| **ask** | 转人工确认，带超时兜底 | 同上 | 有 |
| **显式 allow** | 代码常量**显式判定为放行** | 同上，**仍在最前求值** | 无 |

**关键区别**：「显式 allow」与「漏判」是两件事。

- 漏判 = 工具不在任何规则里 → 判官根本不求值 → 这就是 G-01① 原漏洞（平台工具绕白名单直接放行），**已修**。
- 显式 allow = 短路表**主动求值并判定放行**（代码常量决定，附理由）→ 不产生漏判，且判定动作进审计链（§2.4）。

因此分级**不重新打开 G-01①**，只把"该 ask 的面"缩到真正高危的那部分。

### 2.3 各档归属（提案，待 Sir 复核）

| 工具 / 行为 | 现档 | 新档 | 理由 |
| --- | --- | --- | --- |
| `create_mcp` / `update_mcp` | deny | **deny** | 凭据汇：stdio 以容器 root 拉任意二进制、http/sse 把 env 当 header 回放。**不改** |
| 读平台控制面秘密（`/app/jwt.key`、`/app/.env`、MITM CA、`/proc/*/{environ,cmdline,mem,maps,mounts}`） | deny | **deny** | 控制面秘密，非渗透目标资产。**不改** |
| 平台环境块规则（工具入参层面） | deny | **deny** | 纵深防御的第二道。**不改** |
| `update_custom_tool`（改写既有可执行面） | ask | **ask** | 自定义工具 = 任意代码执行面，改写既有面影响面大 |
| `create_custom_tool`（新增可执行面） | ask | **ask** | 同为任意代码执行面；平台无法在事前证明其无害 |
| `update_skill_file`（改写既有 agent 行为） | ask | **ask** | 持久化行为注入，影响后续所有轮次 |
| `delete_assets_by_host`（删证据库） | ask | **ask** | 不可逆、影响已有结论 |
| **`create_skill`（新建技能文件）** | ask | **显式 allow** | 新建不覆盖既有行为；其后的动作仍受工具层护栏约束（护栏在**动作**层，不在**内容**层 → 恶意 skill 内容并不能绕过） |
| 其它普通工具 | 走 DB 规则/判官 | **不变** | ARTEX 原样 |

### 2.4 审计与计数（配套）

- 三档判定（deny / ask / **allow**）**全部**写审计事件。平台工具调用量小，进 `audit_events` 哈希链无压力；普通工具的 allow 量太大，仍不进链（沿用现口径）。
- 保留 60s ask 超时 → deny 的兜底（人在远处时默认拒，不默认放）。

### 2.5 G-09④⑥ ask 队列治理的新形态

**不改成"多拦"，改成"重复才升级"**：

- 计数维度：`(agent_id, 目标面, 时间窗)` 的 ask 次数。
- 单次 / 少量 ask：**完全走现流程**（人工确认，60s 超时 deny）→ 体验零变化。
- 超过阈值（同一 agent 在窗口内对**高危面**反复 ask，形态上像"疲劳点 allow 的循环"）→ 该 agent 在该面上升级为 deny + 暂停派发 + 落审计 + 告警。
- 阈值、窗口、是否可配：默认保守（宁可不升级）；可经 settings 热改（这是**可用性**参数，不是安全边界，故可热改；安全边界仍在代码常量里）。
- 只对 §2.3 的 **ask 档**计数；deny 档本来就不需要计数；普通工具的 ask 走 DB 规则，不进本计数（避免把"正常业务提问"误判成疲劳）。

### 2.6 口径后果（诚实记录）

G-01① 的账本判定从「**已解决（唯一完全闭合）**」降为「**高危面闭合 + 低危面代码级显式放行**」。这不是倒退——原先的"完全"是把低危面也 ask 换来的；但账本必须改，否则记录会高估覆盖面。**该项的回归测试相应从"7 件工具全部 ask"改为"2 件 deny、4 件 ask、1 件显式 allow，且三者都在最前求值"**。

---

## 3. 波次分解

### 3.1 依赖图（波次内串行，波次间可并行）

```
W-A（执行层收口）                     W-B（交付可信与工程）
  A0 分级审核改造 ──┬─→ A6 ask 队列治理  B1 签名+SBOM+provenance
  A1 入参路径解析   │                     B2 多架构冒烟 + 不可变标签
  A2 环境块不变式门 ├─→ A8 端到端验证      B3 每用例级测试隔离
  A3 ptrace 收紧    │                     B4 上游回合流程（文档）
  A4 容器面 DSN     │                     B5 生态补齐（4 小项）
  A5 key 加密落库   │
  A7 判官分侧/超时 ─┘
                    W-C（架构级，需单独授权）
                      C1 per-task runner + 净网 + 最小 env
```

### 3.2 总表

| # | 项 | 所属波次 | 闭合手段（摘要） | 验证断言 | 成本 | 依赖 |
| --- | --- | --- | --- | --- | --- | --- |
| A0 | 分级审核改造 | W-A | 短路表三档化：deny / ask / 显式 allow；低危面改显式 allow；判定全进审计链 | 回归测试断言 2 deny + 4 ask + 1 allow，且三者均在 `applyIntercept` 最前求值 | S | — |
| A1 | 入参路径 token 的 cwd 相对解析 | W-A | 在 `pathguard` 增加"入参路径片段解析"：提取入参里的路径样 token，相对 `pc.WorkingDir` 解析，命中秘密清单即 deny | 单元测试覆盖 `../../../jwt.key`、`$(realpath ../../../jwt.key)`、`${HOME}/../../jwt.key` 等形态；**真实 agent 会话**发起 `Bash(cat ../../../jwt.key)` 必须被 deny 并落审计 | M | 无硬依赖（建议排在 A0 之后） |
| A2 | 环境块不变式成为可回归门 | W-A | 把"进程树环境块无非空平台秘密"从一次性自检固化为 CI/验收脚本；覆盖工具子进程继承面 | 验收脚本在 CI 与本地各跑一次，断言进程树内无非空平台秘密 | S | — |
| A3 | `/proc/<pid>/mem` 读取 | W-A | 部署侧：`cap_drop: [SYS_PTRACE]` + seccomp 禁 ptrace；容器内非 root | 容器内尝试读父进程 mem **必须失败**（EACCES/EPERM），且不影响工具正常执行 | S | — |
| A4 | 容器配置面 DSN 可见 | W-A | compose 改用 docker secret（`/run/secrets/pg_dsn`），入口脚本从 secret 文件取 DSN 写 DSN_FILE | `docker inspect` 输出中**不含** `ARTEX_PG_DSN`；容器起来且 `/api/health` 正常 | S | — |
| A5 | `llm_profiles.api_key` 明文 | W-A | 应用层 AES-GCM 加密落库（新增 `api_key_cipher` 列），主密钥走**文件供给**（与 DSN 同模式：读入即删）；读路径解密；旧明文行读时就地迁移 | 库里查 `api_key` 列为空/密文、接口行为不变、旧行自动迁移；主密钥文件用完即删 | M | — |
| A6 | G-09④⑥ ask 队列治理 | W-A | 按 §2.5：`(agent, 面, 窗口)` 计数，仅 ask 档，仅超阈值才升级 deny+暂停派发+告警 | 同一 agent 窗口内第 N+1 次 ask 被自动 deny 并落审计；第 1 次仍走人工；低危面不计数 | M | A0 |
| A7 | G-04①② 判官分侧 + ask 默认超时 | W-A | 任务侧 judge 失败/超时 → deny（快速失败，不放行）；会话侧保留 ask；DB 规则的 ask 类默认 60s 超时 → deny | 注入 judge 不可用 → 任务侧 deny；DB 规则 ask 无 timeout 配置时 60s 后 deny | M | — |
| A8 | 短路表运行时端到端验证 | W-A | 起真实 agent 会话发起工具调用，断言 deny/ask 落审计、页面可见、审批流可用 | 可重复脚本（进 CI 的安全回归 job；不依赖 LLM key 的部分单独成 job） | S-M | A1 |
| B1 | 产物签名 + SBOM + provenance | W-B | 用现成路径：GitHub Artifact Attestations（`attestations: write`）+ `actions/attest-build-provenance` + syft 生成 SBOM 作为 Release 资产；**不自建密钥体系** | `gh attestation verify` 对 zip 与镜像通过；SBOM 在 Release 资产里 | M | — |
| B2 | 多架构冒烟 + 不可变标签 | W-B | 冒烟扩到 arm64（QEMU 已在）；Compose 默认改不可变 tag/digest，`latest` 保留但标注不建议 | 两个架构的镜像冒烟都过；Compose 默认拉到的 digest 与 Release 一致 | S-M | — |
| B3 | 每用例级测试隔离 | W-B | `internal/testdb` 增 schema 级隔离（每用例一个 schema + `search_path`），不每用例建库（库数量与耗时不可接受） | 同一包内用例**任意顺序 + 并行**跑，结果与顺序无关；重复跑 3 遍结果一致 | M | — |
| B4 | 上游择取回合流程 | W-B | 写成 `docs/upstream-backport.md`：判定清单（安全类必回合 / 功能类按需 / 冲突处理）+ 每回合的记录模板（进账本） | 文档含"如何判定一个上游提交要不要回合"的可操作步骤 + 一个已回合案例回填 | S | — |
| B5 | 生态补齐（4 小项） | W-B | ① skills 同步 ② 更新即重启 ③ 数据回滚 ④ 复测结果导出 | 每小项各自有验收断言（见 §4.6） | M-H（合） | B1（签名与回滚相关） |
| C1 | per-task runner + 净网 + 最小 env | W-C | 需架构级授权：容器权限模型 + 上游 norma 契约（工具执行搬进 per-task 侧车） | 需先出独立设计；本总纲只登记范围 | H | 需单独授权 |

### 3.3 波次成本与产出

| 波次 | 项数 | 量级 | 产出 |
| --- | --- | --- | --- |
| W-A | 9 | 中（约 1/3 改配置、1/3 改应用代码、1/3 补测试与门） | 安全活路径逐条闭合 + 分级审核落地 |
| W-B | 5 | 中 | 交付物可验证 + 工程门更硬 + 生态补齐 |
| W-C | 1 | 高 | 真正的硬边界（需单独授权） |

---

## 4. 逐项闭合方案

### 4.1 A0 分级审核改造

- **现状**：`guard/shortcircuit.go` 的 `platformAskTools` 含 5 件（`create_custom_tool`/`update_custom_tool`/`create_skill`/`update_skill_file`/`delete_assets_by_host`），一律 ask + 60s 超时 deny。
- **手段**：拆成三张表——`platformDenyTools`（不变）、`platformAskTools`（去掉 `create_skill`）、`platformAllowTools`（新增，只含 `create_skill`）。三张表都在 `shortCircuit()` 最前求值，命中即返回 `Decision{Action:"allow"}`（**显式判定**，不是"不返回"）。
- **审计**：`allow` 档的判定也要落审计事件（平台工具量小，可进哈希链）。
- **风险**：误放行。缓解：只有 `create_skill` 进 allow 档；且 skill 的**内容**仍受动作层护栏约束（护栏拦的是行为，不是内容）。
- **验证**：回归测试断言三档各自命中；断言求值顺序（`applyIntercept` 最前，先于 `IsToolEnabled`）。

### 4.2 A1 入参路径 token 的 cwd 相对解析

- **现状**：E2 —— Bash 不在 pathguard 覆盖内，短路表是字符串预筛，故 `cat $(realpath ../../../jwt.key)` 绕得过（已登记残留）。
- **手段**：
  1. 在 `pathguard` 增加一个**入参路径片段提取器**：从工具入参 JSON 的字符串值中提取"路径样片段"（含 `/`、`.`、`..`、`~`、反引号/`$()` 内的路径子串）。
  2. 对每个片段：若为绝对路径 → 直接解析；相对路径 → **以 `pc.WorkingDir` 为基准解析**（E3 保证拿得到，无需改上游）。
  3. 解析结果（`EvalSymlinks`，沿用现有 `resolveExisting` 对不存在路径取最深已存在祖先的策略）命中 `secretRoots` / `procSecrets` → **Deny**。
- **为何能挡住已登记的那条绕过**：`cat $(realpath ../../../jwt.key)` 的入参里**字面量就含 `../../../jwt.key`**，相对 `WorkingDir`（= `/app/data/sessions/<key>`）解析三层即落在 `/app/jwt.key` → 命中 → deny。`${HOME}/../../jwt.key` 同理命中。
- **仍挡不住的（必须写进账本，不得宣称闭合）**：运行时才成形的路径（`x=$(printf '\x2fapp\x2fjwt.key'); cat $x` 这类编码/拼接形态）、由外部下载内容决定的路径、**以及容器内 root/特权进程的任何读取**。这仍是**应用层收口**，不是强制边界。
- **误伤面**：只命中秘密精确清单，**不做根目录收敛**（沿用 pathguard 现取舍：渗透工作区在 `/app/data/sessions/<key>`，目标在别处，收敛会打断"读证据到本地"流程）。
- **成本**：M（新解析器 + 提取器 + ~30 组用例 + 端到端脚本）。

### 4.3 A2 环境块不变式成为可回归门

- **现状**：E5 —— 平台秘密已不进环境块；但这是**一次性自检**的结论，没有门守着，下一次有人把 key 改成 env 注入就悄悄回归。
- **手段**：把验收脚本固化（进 CI 的安全回归 job + 本地一条命令）：遍历应用进程树，逐个读 `/proc/<pid>/environ`，断言无非空平台秘密（`ARTEX_PG_DSN`/`ARTEX_MASTER_KEY`/`*_API_KEY` 等）。
- **附带**：断言工具子进程（agent 的 Bash）继承到的环境块同样干净——这是"继承通道"的可回归形式（norma 侧 `cmd.Env = withEnv(tc.Env)`，最终来源是平台进程，故在进程树层面断言即覆盖）。
- **成本**：S。

### 4.4 A3 `/proc/<pid>/mem`

- **现状**：属**宿主/容器权限面**的活路径（需 ptrace 权限），应用层拦不住。
- **手段**：部署侧收紧——compose 加 `cap_drop: [SYS_PTRACE]`、seccomp profile 禁 `ptrace`、容器内以非 root 运行（若现状已是 root，需同时改 `/app` 属主与数据目录权限 → 该子项成本会上升，须先核实现状）。
- **验证**：容器内 `cat /proc/<ppid>/mem` 必须失败（EACCES/EPERM）；同时工具正常执行不受影响。
- **诚实边界**：容器内 root + 未 drop 能力时仍可读；本项闭合的是"同 UID 任意进程可读"这一条。
- **成本**：S（若已是非 root）/ M（需改属主与权限）。

### 4.5 A4 容器配置面 DSN

- **现状**：E6 —— `ARTEX_PG_DSN` 仍在容器配置的 Env 里 → `docker inspect` 可见口令（虽然进程环境块已干净）。
- **手段**：compose 用 `secrets:` 挂载 `/run/secrets/pg_dsn`（`docker compose` 对非 swarm 也支持 file 型 secret），入口脚本从该文件读 DSN → 写 `ARTEX_PG_DSN_FILE` → 沿用现有"读入即删"链路。**不再把口令放进 Env**。
- **验证**：`docker inspect` 的 Env 里不含 `ARTEX_PG_DSN`；容器起来、`/api/health` 正常；口令仍不进进程环境块（A2 门继续绿）。
- **成本**：S（compose + 入口脚本 + 一条验收命令）。

### 4.6 A5 `llm_profiles.api_key` 加密落库

- **现状**：E7 —— 明文 TEXT 列。拿到 DB 口令即可读出全部 LLM key。
- **手段**：
  1. 新增列 `api_key_cipher`（bytea 或 base64 TEXT），应用层 **AES-GCM** 加密（每行随机 nonce）。
  2. 主密钥走**文件供给**（与 DSN 同模式）：`ARTEX_MASTER_KEY_FILE` → 启动读入 → 用完即删（文件本身也 unlink）。
  3. 读路径解密；写路径加密；`api_key_hint` 保持掩码展示不变。
  4. **迁移兼容**：读到旧明文行时解密分支识别为"明文"→ 就地加密回写（一次迁移脚本 + 启动时惰性迁移兜底）。
- **风险**：主密钥丢失 = 所有 profile 不可用 → 需要明确的备份与轮换说明（写进 README 的运维段）。
- **验证**：库里 `api_key` 列无明文；接口行为不变；旧行迁移后仍能调用；主密钥文件用完即删。
- **成本**：M。

### 4.7 A6 ask 队列治理（见 §2.5）

- **成本**：M。依赖 A0（先有分级，再谈治理）。

### 4.8 A7 判官分侧 + ask 默认超时（G-04①②，建议补入）

- **现状**：E10 —— 判官失败/超时的行为未按任务侧/会话侧分治；DB 规则的 ask 类仍可"无限等待"（种子规则 `timeout_enabled=false` 照抄即无超时），只有短路表自带的 ask 配了 60s。
- **手段**：任务侧 judge 失败/超时 → **deny**（快速失败，不放行）；会话侧 ask 保留；DB 规则的 ask 类补默认 60s 超时 → deny。
- **可用性代价（如实登记）**：判官不可用时任务直接失败，而不是放行。这是有意的取舍（放行 = 边界失效）。
- **成本**：M。

### 4.9 A8 端到端验证

- **手段**：起真实 agent 会话 → 发起工具调用 → 断言 deny/ask 落审计 + 页面可见 + 审批流可用；写成可重复脚本。
- **约束**：CI 无 LLM key → 拆成两个 job：不依赖 LLM 的（直接走工具层 + 审计断言）进 CI；依赖真实会话的部分作为**部署后验收脚本**（本地/预发跑，留档）。
- **成本**：S-M。依赖 A1（否则测不出 Bash 面的真实闭合）。

### 4.10 B1 产物签名 + SBOM + provenance

- **手段**（全部用现成路径，不自建密钥体系）：
  - `actions/attest-build-provenance` 生成构建来源证明（OIDC 无密钥）；
  - GitHub Artifact Attestations 签 zip 与镜像（`attestations: write` 权限）；
  - syft 生成 SBOM，作为 Release 资产（每个平台一份或合并一份）。
- **闭合的残留**：「产物无独立签名，SHA256SUMS 与二进制同源」→ 变为"可离线验证的来源证明 + 独立于 Release 资产的签名"。
- **验证**：`gh attestation verify <zip>` / `gh attestation verify oci://ghcr.io/...:v0.4.0` 通过。
- **成本**：M。**信任模型变化**：从"信任 GitHub Release"升级为"信任 GitHub OIDC + attestation 链"，须在 README 更新措辞。

### 4.11 B2 多架构冒烟 + 不可变标签

- **手段**：冒烟 job 扩到 `linux/arm64`（QEMU 已在 `docker/setup-qemu-action`）；Compose 默认镜像改为**不可变 tag 或 digest**（`${ARTEX_TAG:-v0.4.0}` 形态 + README 建议 pin digest）；`latest` 保留但标注不建议用于生产。
- **验证**：两架构冒烟都过；`docker compose up` 拉到的 digest 与 Release 资产一致。
- **成本**：S-M。

### 4.12 B3 每用例级测试隔离

- **现状**：E12 —— 包内用例共享一库 → 顺序敏感（基线上"每次挂的用例都不一样"的成因之一）。
- **手段**：`internal/testdb` 增加 schema 级隔离：每个用例一个 schema（`test_<pkg>_<n>`）+ 会话 `search_path`；**不**每用例建库（库数量与建库耗时不可接受）。
- **需注意**：跨 schema 的全局对象（`public` 里的扩展/函数）与并发用例的 schema 清理顺序需在实施时核。
- **验证**：同一包内用例**任意顺序 + `-shuffle=on` + 并行**跑，结果与顺序无关；重复 3 遍结果一致；`go test ./... -count=1` 退出码 0。
- **成本**：M。

### 4.13 B4 上游择取回合流程

- **手段**：文档化（`docs/upstream-backport.md`）：上游提交分类（安全类 / 功能类 / 依赖升级 / 与 reArtex 本地改动冲突）→ 判定清单 → 回合步骤（含本 fork 已改过的文件要人工合并的提示）→ 每次回合在账本留条目。附一个已回合案例回填（例：norma v0.4.3→v0.4.4）。
- **验证**：文档可被第三方按步骤复现一次回合（自检：拿一个已知上游提交走一遍流程）。
- **成本**：S。

### 4.14 B5 生态补齐

拆为四个可独立交付的小项：

| 小项 | 手段要点 | 验收 |
| --- | --- | --- |
| ① skills 同步 | 一条命令把本仓库 `skills/` 同步到运行中的容器（并刷新缓存） | 同步后容器内 skill 列表与仓库一致；不重启主进程 |
| ② 更新即重启 | 自更新器（`selfupdate/bootstrap.go`）下载新版本后，安全重启进程（保留数据与状态） | 更新后进程版本号变化、`/api/health` 正常、数据无损 |
| ③ 数据回滚 | 发布/迁移前自动快照（`pg_dump`）+ 一条回滚命令 | 能把库恢复到快照点，且有演练记录 |
| ④ 复测结果导出 | 把复测（retest）结果导出为可归档格式（JSON/CSV） | 导出文件含结论、证据引用、时间戳 |

- **成本**：M-H（合）；建议按 ④ → ① → ③ → ② 的顺序（先只读、后写、再动进程）。

### 4.15 C1 per-task 隔离（架构级，后置）

- **范围登记**：per-task 受限身份（最小 env + 独立 uid）、出网只走允许通道（netns + 默认拒绝直连）、任务级资源与工作目录隔离。
- **前置**：需独立设计（架构图 + 迁移路径 + 对上游 norma 的改动面评估）+ 单独授权。
- **不做什么**：本总纲不承诺其形态；只承诺"它是唯一能把边界从应用层推到强制层的路径"。

---

## 5. 验证方法总表

| 类别 | 门 | 落点 |
| --- | --- | --- |
| 分级审核 | 三档命中回归 + 求值顺序断言 | Go 单测（A0） |
| Bash 面路径解析 | 30+ 形态用例 + 真实 agent 会话 | Go 单测 + 部署后验收脚本（A1/A8） |
| 环境块不变式 | 进程树扫描断言 | CI 安全回归 job（A2） |
| ptrace | 容器内读父进程 mem 必须失败 | 部署验收脚本（A3） |
| 容器配置面 | `docker inspect` 无 DSN | 部署验收脚本（A4） |
| 密钥落库 | 库内无明文 + 迁移正确 | Go 单测 + 验收脚本（A5） |
| ask 治理 | 阈值前后行为差异断言 | Go 单测（A6） |
| 判官分侧 | 注入 judge 不可用 → deny | Go 单测（A7） |
| 交付可信 | `gh attestation verify` + SBOM 存在 | CI（B1） |
| 多架构/不可变 | 双架构冒烟 + digest 一致 | CI（B2） |
| 用例隔离 | `-shuffle=on` + 并行 + 3 遍一致 | CI（B3） |
| 回合流程 | 第三方可复现一次回合 | 文档自检（B4） |
| 生态四项 | 各见 §4.14 | 部署后验收（B5） |

**通用约束**：任何一项的"验证断言"必须是**可执行**的命令或测试；写不出断言的项不进入实施。

---

## 6. 风险与取舍（诚实声明）

1. **本方案全部落在应用层/部署配置层**。真正不可绕过的边界（容器内 root、特权、宿主 docker socket、内核 ptrace 未收紧）**不在本方案覆盖内**。做完 W-A/W-B 只能说"已登记的活路径逐条闭合或降级为已接受残余"，**不能说"安全了"**。
2. **A1 仍是预筛**：它把"已知形态"（含 shell 展开的相对回溯）从绕不过变成绕不过，但运行时成形的路径（编码/拼接/下载内容）仍在射程外。要真闭合必须 C1。
3. **A7 牺牲可用性**：判官不可用时任务失败而非放行。这是有意的。
4. **A0 会让账本口径变差**：G-01① 从"完全闭合"降为"高危面闭合 + 低危面显式放行"。这是用覆盖面换体验，**必须改口径并留证据**，不得悄悄放松。
5. **B1 改信任模型**：从"信任 GitHub Release"变为"信任 GitHub attestation 链"。需在 README 同步措辞，并确认下游用户接受该信任假设。
6. **A5 引入新的单点**：主密钥丢失 = 全部 LLM profile 不可用。需要备份/轮换说明。
7. **B3 会有性能代价**：schema 级隔离比包级慢（每用例建/删 schema），需在 CI 上量一次；若超出预算，退回"包级 + 用例级清理钩子"的折中。
8. **上游漂移**：reArtex 不自动同步上游（更新只跟自己的 Release），上游后续仍有变更 → B4 是唯一对冲手段；不做 B4，则回合靠临时手工挑，覆盖度不可控。

---

## 7. 明确不做（out of scope）

- 容器内 root 提权链、宿主 docker socket 权限、宿主内核加固——属宿主面，不在本仓库范围。
- 端到端加密（TLS/mTLS）与证书体系——属部署面，另案。
- 把 reArtex 与上游 tag 线合并（决策：走自有发布线）。
- 为渗透目标做白名单式目录收敛（会打断"读证据到本地"的工作流，明确不做）。

---

## 8. 执行顺序与检查点

1. **W-A 第一步**：A0（分级改造）→ 立刻可验证"低危面不再打断"，同时给 A6 打好底。
2. **W-A 第二步**：A1 + A8（Bash 面真路径解析 + 端到端验证）——这是本批**唯一直接闭合一条已登记活路径**的高价值项。
3. **W-A 第三步**：A4 → A3 → A2（部署面收口，成本低、见效快，且互相独立可并行）。
4. **W-A 第四步**：A5、A6、A7（需要动数据模型与判官行为，风险略高）。
5. **W-B**：B1 → B2（交付可信，先让发布物可验证）→ B3（工程门）→ B4（流程）→ B5（生态，按 §4.14 顺序）。
6. **W-C**：仅在 W-A/W-B 完成、账本口径稳定后再启动设计评审。

**每个波次结束时的检查点**：账本口径更新 + 归档 + 一个可复现的验收脚本 + 一次 CI 全绿（禁止带 `-skip` 的门）。

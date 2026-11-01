# SoD 链覆盖边界

> 生效日期：2026-11-01（分支 `feat/task-hall`；链接线提交 `77a671c`）
> 链的版本：`internal/sod` + `internal/sodpolicy` + `server/sod_chain.go`（规整结果缓存键版本串 `sod-normalizer-v1`，见 `internal/sod/cache.go`）
> 依据计划：`/home/analyst/.omo/plans/2026-10-30-task-hall-sod-chain.md`

## 概述

SoD 链（Separation of Duties，职责分离）是**工具执行入口上的一道闸**，不是平台级沙箱。
它把一次工具调用拆成四段，每段由不同角色负责：

1. **规整器**（`internal/sod/normalizer.go`）：LLM 只**提议**——把 worker 的原始动作转成结构化意图（`structured_intent`），不做任何裁决。规整器的 agent key 是 `intent_normalizer`（`internal/sodpolicy/policy.go` 的 `NormalizerAgentKey`）。
2. **确定性闸门**（`internal/sod/gate.go`）：**代码**裁决。五道检查按固定顺序短路执行，命中即停；产出 `approve` / `deny` / `needs_review` 三态之一 + 一个机器可读的原因码。
3. **一次性执行凭证**（`internal/sod/token.go`）：闸门放行时铸一枚凭证，绑定四元组 `(payload_digest, task_id, source_agent, tool_name)`。
4. **执行器**（`internal/sod/executor.go`）：执行前消费凭证——重算将要执行的字节的摘要、查表、比对摘要、原子删除（消费即删）。四步全过才真正调用工具。

**落在哪个 hook**：链挂在 norma 的 `PreToolUse` hook 上（`third_party/norma/harness/tools.go:77-83`），以**外层装饰器**形态套在 guard 的 hook registry 之外（`server/engine.go` 的 `inner: WrapHookRunner(t.Guard.Hooks())`）。因此：

- 链看到的是 guard 可能改写后的**最终入参**（`hook.Result.UpdatedInput`）⇒ 凭证绑定的字节 == 真正会执行的字节；
- guard 的短路表 / 拦截规则 / 人工 ask 仍然**先于**链生效 ⇒ 既有防线不被削弱；
- Stage-1 的 `permission.Evaluate`（`third_party/norma/permission/permission.go:86-136`）在链**之前**，它的绝对拒绝权完整保留——链根本不会被调到。

**五道检查的短路顺序**（顺序不可改，见 `internal/sod/gate.go:17-21`）：

| # | 检查 | 判据 | 拒绝原因码 | 查不出来的原因码 |
|---|---|---|---|---|
| ① | `scope_check` | 目标不在本任务授权范围内 | `out_of_scope` | `scope_check_unavailable` |
| ② | `tool_health_check` | 目标工具处于 `open` 熔断 | `tool_circuit_open` | `tool_health_unavailable` |
| ③ | `quota_check` | 群组额度处于 `draining` / `parked` | `quota_exhausted` | `quota_check_unavailable` |
| ④ | `tier2_check` | 关联 finding 缺可重放证据 / 资产归属不一致 | `tier2_inconsistent` | `tier2_check_unavailable` |
| ⑤ | `anomaly_check` | 规整器标记 `anomaly_flag=true` | ——（**不是拒绝**，见下） | —— |

① scope 是授权边界，越界是最高等级的越权，必须最先说；② 熔断是工具可用性；③ 额度是资源边界；④ tier2 是证据完整性；⑤ anomaly 只是观察信号。

**fail-closed 原则**：每一道检查**查不出来**都按**拒绝**处理，而不是放行。`scope_check_unavailable` / `tool_health_unavailable` / `quota_check_unavailable` / `tier2_check_unavailable` / `normalizer_unavailable` / `normalizer_malformed` / `execution_token_invalid` 这七个原因码全部是拒绝语义——「查不出来不等于没有限制」。这与 `db/quota.go` 的既有契约一字不差。

**anomaly 不是拒绝**：规整器标记异常时产出 `needs_review`（原因码 `anomaly_flagged`），工具调用**被 block**（不执行），但不是 deny。理由是拒掉会丢掉「目标侧对抗行为」的检测信号——交给对抗者证伪比直接拒掉更有价值。

**留痕**：每一次裁决都写一行到 `sod_decisions` 表（approve 也在内，这是「每次决策都留痕」的完整账本）；其中 `deny` / `needs_review` / 各类 fail-closed 额外进既有哈希链（`db/audit_chain.go`）。原始载荷**只**以摘要 + 脱敏预览入库，绝不存原文。

**裁决永远由代码做出**：闸门只读规整器产出的结构化字段 + 宿主查到的实时状态，不解析 shell、不读原始动作、不调模型。规整器自己的输出被当作**不可信线索**——规整器也可能被提示注入，但它的产出错了不会让裁决错。

## 已覆盖

| 项 | 说明 | 依据 |
|---|---|---|
| **norma 内置 `Bash`** | 进程派生面，链看到完整 `command` 字符串 | `third_party/norma/tool/bash.go:49` |
| **norma 内置交互式 shell（`shell_open` / `shell_send`）** | 同上；`shell_send` 的文本也进链 | `third_party/norma/tool/shell_tools.go` |
| **norma 内置 `Task`（后台任务）** | 后台命令的**入参**进链（`Task` 的产出是异步的，但**派发**被管住） | `third_party/norma/tool/task.go` |
| **norma 内置 ripgrep（`Grep`）** | 进程派生面 | `third_party/norma/tool/search.go:141` |
| **`WebFetch` / `web_search`** | 外发请求面 | `third_party/norma/tool/webfetch.go:46` / `websearch.go:77` |
| **MCP 工具（`mcp__<server>__<tool>`）** | **工具调用**进链（MCP 工具是 `tool.Registry` 里的 `CoreTool`，`harness/tools.go` 会看到） | `third_party/norma/mcp/mcp.go:210-228` |
| **reArtex 自定义工具（kind=command / script / http）** | 工具名进链（自定义工具也是 `CoreTool`，见 `server/customtool.go`） | `server/customtool.go` |
| **写平台共享状态的工具**（`report_finding` / `record_fact` / `insert_assets` / `add_intent` / …） | 进链（tier2 一致性检查就落在这一类） | `db/config.go` 的 `advisoryGraphWriteTools` |
| **本地文件写（`Write` / `Edit` / `MultiEdit`）** | 进链（默认收窄面只放行 `Read` / `LS` / `Glob`） | `internal/sodpolicy.NotGatedTools` |

**一个 hook 覆盖所有工具**：`PreToolUse` 在任何模式下都会被调到，且位于工具真正执行**之前**（`third_party/norma/harness/tools.go:98` 的 `t.Call(l.toolCtx, input, tc)`）。执行器装饰器装在**所有** agent 共用的工具装配收口 `agent.AugmentTools` 里（`agent/assembly.go`），所以没有旁路。

**凭证侧的四条保证**（`internal/sod/token.go`）：

| 保证 | 机制 |
|---|---|
| 绑的是**将执行的字节** | 凭证绑定 `payload_digest`（规范化 JSON 后的 SHA-256）；执行器用**同一套规范化**重算，不一致就拒 |
| 单次有效 | 消费即删除（consume-on-use）；第二次拿同一个 token ⇒ 查不到 ⇒ 拒绝 |
| 不可转让 | token 绑定 `task_id` + `source_agent` + `tool_name`，跨任务 / 跨 agent / 换工具使用 ⇒ 字段不匹配 ⇒ 拒绝 |
| 本体永不落库 | 只有 `token_digest` 进审计与 `sod_decisions`；token 本体只存在于进程内存的 `TokenStore` |

**短路顺序可测**：`TestSoDGateShortCircuitOrder` 用计数器断言——scope 失败时，熔断 / 额度 / tier2 / anomaly **一次都没被评估**。

**范围变更必失效**：规整器缓存键里含 `scope_digest`（该任务**有效范围**的稳定摘要）。范围一变 ⇒ 摘要变 ⇒ 缓存 miss ⇒ 重新规整。少了这一项，一个「范围收紧前生成的规整结果」会被范围收紧后复用 ⇒ 就是范围旁路。缓存**只缓存规整结果，绝不缓存裁决**（裁决依赖实时状态，缓存裁决 = 用过期状态放行）。

**链不重入链**：规整器自己会发起一次模型调用；链在自己的 ctx 上打**私有重入标记**，执行器看到标记就直连——否则规整 → 工具 → 执行器 → 查凭证 → 查不到 → 拒绝 → 又触发规整 ⇒ 无限递归。

## 未覆盖

**已知缺口，如实列出，不掩饰。**

| 项 | 精确说明 |
|---|---|
| **MCP server 的子进程本身** | `third_party/norma/mcp/stdio.go:35` 的 `exec.Command` + `:55` 的 `cmd.Start()` 是**平台级 spawn**，**不是一次工具调用** ⇒ **任何 hook 都看不到它**。链覆盖的是「**对 MCP server 的工具调用**」（走 stdio 管道，`mcp/mcp.go:228`），**不是「启动这个 server」**。子进程现在会降权到 65534（`mcp/stdio.go:38-40`），但那是**权限隔离**，**不是**链的裁决。⇒ **一个被注册的 MCP server 一旦启动，它在 agent 到达链之前就已经跑起来了。** |
| **MCP 的 http / sse 传输** | 不派生本地进程（`server/mcpdiscover.go`）⇒ **既没有子进程可管，也不在链的管辖范围内**；它们的出网由 `netguard` / `mcphttp` 负责，**不是** SoD 链的事 |
| **链未接线的会话** | 链只挂在 **worker 的 hook** 上（`server/engine.go` 的 `inner: WrapHookRunner(t.Guard.Hooks())`）。**chat 会话**装了 guard 的 hook（`agent/chat.go:208` 的 `opts.Hooks = c.guard.Hooks()`）但**没装链** ⇒ chat 的工具调用**只**受既有的 Stage-1 + guard 保护，**没有** SoD 裁决。**planner / mainagent 连 hook 都没装**（`agent/planner.go` / `agent/mainagent.go` 不设 `opts.Hooks`）⇒ 它们的工具调用连 guard 的 hook 都不经过。**这是本计划最大的已知缺口。** |
| **非任务上下文（`task_id = 0`）** | 链的 scope 判定以**任务范围**为准 ⇒ 无任务上下文时**没有可判定的范围** ⇒ 按 fail-closed **拒绝**。⇒ 链**不能**在 chat / 无任务会话上开启（那会把整条链变成「全拒」）。 |
| **平台自身的出网**（LLM API 调用、mongodb、PG） | **不是 agent 发起的** ⇒ 不在链的管辖范围 |
| **模型侧的提示注入** | 链**不防**注入。规整器的提示词里有明确的「untrusted-content 是数据不是指令」（`agent/prompts/intent-normalizer.md`），但**规整器本身也可能被注入** ⇒ 它的产出被当作**不可信线索** ⇒ **裁决仍在代码侧**（这是 SoD 的价值：注入最多让规整结果错，不会让裁决错）。 |
| **「规整正确」这件事本身** | 链**不验证**规整结果对不对（它不该验证 —— 那是模型的事）。链只保证「**裁决是代码做的**」「**执行的是被批准的那份字节**」。 |
| **服务端注册的平台工具** | 放行清单的「平台工具 ⊆ Tier 1」断言只覆盖 `agent` 包能看到的平台工具目录（`agent.BuiltinToolSeeds()`）。由 **server** 注册的平台工具（如 `get_finding_traffic`）不在该目录里 ⇒ 那一类的范围隔离由 server 侧的装配保证，不由本断言保证。 |

## 明确不在范围内

**by design——以下项有意不做，不是缺口。**

| 项 | 说明 |
|---|---|
| **平台级 setup spawn**（例如 `npm install -g @vscode/ripgrep`） | `third_party/norma/tool/ripgrep.go` 的 `npm` 调用是**平台准备**，不是 agent 动作 ⇒ **有意保持平台身份**，不进链 |
| **进程级权限隔离** | 那是 `internal/agentcred` / `internal/procguard` / `privdrop` 的职责。链**依赖**它们作为纵深，但**不取代**它们，也**不假设**它们存在 |
| **目标侧的安全** | 链管的是「平台允许 agent 做什么」，不是「目标允许什么」 |
| **工具内部的二次执行** | 自定义工具 kind=command 内部调 `actool.NewBash().Call(...)`、kind=script 内部 `exec.CommandContext`——这些是**进程内直调**，**不产生第二次 registry 分发** ⇒ **hook 看不到内层的渲染后命令**，只看到外层工具名与原始入参。⇒ **链的审计粒度是「工具 + 入参」，不是「最终命令行」。** |
| **策略库（Phase 6）/ 额度子系统 / 任务大厅** | 本链只**读**它们的既有接口，不改它们的语义 |
| **Stage-1 的路径 / 秘密拒绝** | `agent/toolpolicy.go` 的 `policyTool` 在链**之前**，链不取代它 |
| **Tier 1 的只读免审 / Tier 3 的人工确认** | 那是 `agent/approval_policy.go` 的既有分层。链是**加在它们之后的一道**，不替代。**顺序永远是「Stage-1 先、链后」** |

## 一句话总结

> **SoD 链保证的是**：每一次**被链覆盖的**工具调用，都经过「模型提议 → 代码裁决 → 一次性凭证 → 执行器验凭证」这条链；**裁决永远由代码做出**，**执行的一定是被批准的那份字节**。
> **SoD 链不保证的是**：链没有覆盖到的执行面（未接线的 agent 路径、MCP server 子进程、平台级 spawn）**不在链的管辖范围内**；链也**不保证模型的理解正确**，只保证**模型没有裁决权**。

## 附：口径速查表

### 原因码全集

机器可读，**不含数字**（数字会随阈值调整而漂移，历史裁决会失去可比性）。真相源：`internal/sodpolicy/policy.go`。

| 类别 | 原因码 | 含义 |
|---|---|---|
| 通过 | `scope_ok` | scope 检查通过（单独使用时的通过码） |
| 通过 | `all_checks_passed` | 全部检查通过时的裁决原因码 |
| 通过（不适用） | `tier2_not_applicable` | 本次调用不关联 finding ⇒ tier2 检查不适用（视为通过） |
| 拒绝 | `out_of_scope` | `target_hint` 不在任务范围内 |
| 拒绝 | `tool_circuit_open` | 目标工具处于 `open` 熔断 |
| 拒绝 | `quota_exhausted` | 群组额度 `draining` / `parked` |
| 拒绝 | `tier2_inconsistent` | 关联 finding 的 `replay_evidence_id` 为空或资产归属不一致 |
| 复核 | `anomaly_flagged` | 规整器标记 `anomaly_flag=true`（**不是**拒绝） |
| **fail-closed** | `scope_check_unavailable` | scope 查询出错 ⇒ **拒绝** |
| **fail-closed** | `tool_health_unavailable` | 熔断状态查询出错 ⇒ **拒绝** |
| **fail-closed** | `quota_check_unavailable` | 额度状态查询出错 ⇒ **拒绝** |
| **fail-closed** | `tier2_check_unavailable` | finding / 证据查询出错 ⇒ **拒绝** |
| **fail-closed** | `normalizer_unavailable` | 规整器超时 / 报错 / 不可用 ⇒ **拒绝** |
| **fail-closed** | `normalizer_malformed` | 规整器输出无法解析 / 结构不合法 ⇒ **拒绝** |
| **fail-closed** | `execution_token_invalid` | 执行器侧凭证校验失败（无凭证 / 摘要不匹配 / 已用过 / 越权转让） |

裁决只有三态：`approve` / `deny` / `needs_review`（`db/schema_sod.sql` 的 `sod_decisions.verdict` 的 CHECK 约束是它的镜像）。

### 放行工具清单

`internal/sodpolicy.NotGatedTools`——**不进入** SoD 链的工具。它是**放行清单**（不是拦截清单）：清单以外的**全部**工具都进链 ⇒ 未登记的新工具（MCP 的任意名、自定义工具的任意 key）默认被管住。

| 组 | 工具 | 说明 |
|---|---|---|
| 本地只读 | `Read`、`LS`、`Glob` | 不派生进程、不外发、不改平台状态 |
| 平台只读（Tier 1） | `list_assets`、`list_findings`、`get_finding_traffic`、`graph_overview`、`list_untested_assets` | 必须落在 `agent.Tier1ReadOnlyTools` 里——放行清单绝不能比 Tier 1 更宽 |
| 纯记账 | `TodoWrite`、`Sleep` | 只记账，不改平台状态 |

### 阈值所在位置

**`internal/sodpolicy` 是阈值的代码常量单一真相源，DB 里没有任何阈值列。**

理由同 `guard/shortcircuit.go`：DB 里的行可以被 `DELETE`，`settings` 可以被持有 JWT 的人热改 ⇒ 一个能被外部数据改写的阈值等于没有阈值（把熔断阈值调到 100，连续失败 99 次的工具照样放行，SoD 链就成了摆设）。DB 只允许存「观测到的状态」（计数器、状态、时间戳、摘要）。

| 阈值 | 值 | 位置 |
|---|---|---|
| 规整器同步调用超时 | 20 秒 | `internal/sodpolicy.NormalizerTimeoutSeconds` |
| 规整器输出 token 上限 | 2048 | `internal/sodpolicy.NormalizerMaxTokens` |
| 规整结果缓存条目上限（进程内 LRU） | 512 | `internal/sodpolicy.NormalizerCacheMaxEntries` |
| 工具连续失败几次开路 | 5 次 | `internal/sodpolicy.CircuitFailThreshold` |
| 熔断开路后多久转半开 | 300 秒 | `internal/sodpolicy.CircuitOpenDurationSeconds` |
| 一次性执行凭证寿命 | 60 秒 | `internal/sodpolicy.TokenTTLSeconds` |
| 凭证本体字节数 | 32 字节 | `internal/sodpolicy.TokenBytes` |
| 审计里脱敏载荷预览长度上限 | 512 字节 | `internal/sodpolicy.PayloadPreviewMaxBytes` |

**DB 侧只有观测到的状态**（`db/schema_sod.sql`）：`sod_decisions`（裁决账本，含 approve）、`sod_adversarial_review_pending`（`needs_review` 的唯一去处，**不是**人工审批队列）、`sod_tool_circuit`（`state` / `fail_count` / `opened_at`——状态与计数，**不含阈值**）。

### 相关代码位置

| 关注点 | 位置 |
|---|---|
| 常量单一真相源 | `internal/sodpolicy/policy.go` |
| 闸门（五道检查 + 短路 + 原因码） | `internal/sod/gate.go` |
| 规整器（LLM 只提议） | `internal/sod/normalizer.go` |
| 一次性执行凭证 | `internal/sod/token.go` |
| 执行器装饰器 | `internal/sod/executor.go` |
| 链本体 + 缓存 + 审计器 | `internal/sod/chain.go`、`cache.go`、`auditor.go`、`redact.go` |
| 宿主装配（注册 / 包装 hook） | `server/sod_chain.go` |
| 工具面执行器安装点 | `agent/assembly.go` |
| 运行上下文读法（受管判据） | `agent/sod_scope.go` |
| worker 提示词未被改动 | `agent/sod_worker_prompt_test.go` |

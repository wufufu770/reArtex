<!--
本文件是「会话群聊化 + 记忆层 + 出网治理补齐」的方案书（设计之记录）。
与交付目录里的 session-groupchat-proposal.md 同源；仓库内这一份把本机绝对路径与个人
私有标识换成通用占位，以便公开评审。
-->

# 方案书 · 会话群聊化 + 任务大厅优化 + 出网治理补齐

- 日期：2026-11-02
- 状态：**待审阅**（会话与任务大厅部分**未动任何代码**）
- 依据：本轮 5 个调研单元（3 份代码勘察 + 2 份外部产品调研）+ 现役库实测
- 涉及代码坐标均为 `wufufu770/reArtex` @ `5df9aea`（`feat/task-hall`）

---

## 零、一句话结论

您要的「一个 session 就是一群聊、可以自动命名、可以关联任务/资产/策略、可以热加入 agent」
在 ARTEX 里**已经有一个 80% 的骨架**（`chat_groups` 群模型），但它**绑死在 task 上**、
**agent 只能在群里当哑巴**（`message_group` 工具预留未实现）、**群里没有实时性**（邮箱只在
agent 自己 turn 开始时读）。而「会话按 agent 分组」的根因更底层：**`conversations.agent_key`
是单值列** ⇒ 一个会话只能跑一个 agent。

⇒ 所以真正的改造不是「改前端分组」，而是**把 `agent_key` 从「会话的属性」升格为「会话成员的属性」**。
推荐做法：**新增 `sessions` 实体**，让 `conversations` 变成 session 下的一个 agent 线程。

---

## 一、现状诊断

### 1.1 会话按 agent 类型分组 —— 根因在数据层，不在前端

| 层 | 坐标 | 事实 |
|---|---|---|
| 前端 | `web/src/app/(main)/chat/page.tsx:115` `groupByAgent()` | 按 `conversation.agent_key` 分桶；`:1186-1199` 组装；`:1436-1490` 渲染成「置顶块 + 折叠 agent 分组」 |
| 前端 | `chat/page.tsx:1200-1203` `chatAgents` | 只留 `role === "assistant"` 的内置 agent（auto/pentest/curator/verifier/redteam/creative/intent_normalizer） |
| **数据层（根因）** | **`db/schema.sql:1003-1017`** | **`conversations.agent_key TEXT NOT NULL` 是单值列** ⇒ 一个会话只能跑一个 agent |
| 服务端 | `db/chat_group.go:3-7` 的注释 | 官方口径已写明：「`agent_key` 是单值列 ⇒ 一个会话只能跑一个 agent ⇒ 群聊落地为『1 大厅会话 + N 成员会话』」 |

⇒ 现在的「群聊」其实是 **1 个大厅会话（agent=auto）+ N 个成员会话（每成员一个，各跑各的）**。
成员之间**没有共享上下文**，只是共享同一个 task 的探索图。

⇒ 命名现状：`server/conversations.go:443-449` 的「自动命名」只是**首条消息首行截 40 字符**，
不是真命名；手动重命名走 `PATCH /api/conversations/{id}`（`:173`）+ 前端双击
（`chat/page.tsx:1332-1345`）。

### 1.2 群聊模型已存在，但有三个硬伤

已存在（`db/schema_chat_group.sql`）：

```
chat_groups            id, conversation_id UNIQUE, task_id NOT NULL, name, status,
                       concurrency_limit, plan_id, budget_tokens, drained_at, parked_at, resumed_at
chat_group_members     PK(group_id, agent_key), agent_key → agents(key), conversation_id, role,
                       joined_at            ⚠️ 无 left_at
chat_group_messages    from_agent, to_agent(''=广播), kind, body, task_id, intent_id, finding_id,
                       status(unread/read/acked/blocked), blocked_reason, claims_authorization
chat_group_disabled_tools  只挡不禁（deny-list，结构上无法放宽权限）✅
```

三个硬伤：

| # | 硬伤 | 后果 | 坐标 |
|---|---|---|---|
| **①** | `chat_groups.task_id NOT NULL` + `UNIQUE(task_id)` | **不绑 task 就当不了群** ⇒ 纯对话型 session 无处安放 | `db/schema_chat_group.sql:21,45-50` |
| **②** | 邮箱**只在 agent 自己 turn 开始时读**（`agent/chat.go:168` → `agent/group_mailbox.go:71-95`） | **没有唤醒/推送** ⇒ 群里发消息，agent 不主动跑就永远看不到 ⇒ **不是实时群聊** | 同左 |
| **③** | `message_group` 工具**键已预留但未实现**（`agent/approval_policy.go:104,121` 有常量，`agent/tools.go` 无实现） | **agent 在群里不能发言** ⇒ 只有人能说话 | 同左 |

另外：`chat_group_members` 的 PK 是 `(group_id, agent_key)` ⇒ 同一 agent 只能入群一次；
`UNIQUE(conversation_id)` ⇒ **一个会话不能被两个群共享**。

### 1.3 任务大厅的不足（现存实现）

已有四件套（群列表 / 建群 / 群详情 / 消息箱 / 禁用工具表 / 额度 / 并发），
但按「用户找人」的角度有这几处硬伤：

1. **群与 task 一对一且不可空** ⇒ 列表里出现的每一行背后都挂着一个 task ⇒ 想建一个
   「先聊聊、还没决定做哪个 task」的群都做不到。
2. **intent 看板只对 worker 有意义**（`exploration_nodes.owner`）⇒ 群里若只有 planner/main
   agent，看板是空的。
3. **额度进度与并发占用是数字，不是状态** ⇒ 看不出「谁快烧完了」「现在卡在谁手里」。
4. **消息箱是群的附属物，不是会话** ⇒ 一个 agent 在群外单独开的会话，在大厅里找不着。

### 1.4 出网治理的两条实洞（您让我补的那两条）

| 洞 | 现状 | 影响 |
|---|---|---|
| (a) planner / 主 agent / 对话 agent 的 Bash 不带任务身份 | 只有 worker 带令牌 | 这三类 agent 的出网**不受任务范围校验** |
| (b) web_fetch / web_search 不走 netguard transport、也不吃子进程 http_proxy | 旁路 | 工具出网**不受任务范围校验** |

⇒ 代码定位单元正在跑（`bg_4cfec35f`），结果回来后按最小正确改法实施。

---

## 二、外部调研：形态学（跨 12 个产品）

调研对象：Grok Bot (xAI)、OpenAI dots、Cue by Manus、DotCollab、Douchat、Poe、
Character.AI、Slack、Discord、ChatGPT Projects/Space、Codex CLI/app、Claude Code、
Cursor、VS Code chat sessions、Cline、Windsurf、Devin、Linear Agent、Notion AI、Hermes。

### 2.1 六条跨产品共识（这是方案的地基）

1. **会话列表是「一个会话一行」的扁平列表**。Codex / VS Code / Cline / Windsurf / Devin /
   Cursor **全部**如此。**没有任何一个主流产品把 agent 类型做成列表的分组轴。**
2. **agent 身份是「行上的字段」，不是「行的分组轴」**。Codex 的 `thread/list` 返回里，
   `agentNickname` / `agentRole` / `source(subAgent*)` / `parentThreadId` 都是**每行的列**；
   分组用 `section`（用户自己建的分区）。VS Code 的 session 行显示 `harness`（哪个 agent 在跑）
   同样是一个**字段**。
3. **命名是双轨的**：用户显式名 > 生成标题 > 默认名。Codex 的 `/rename` 会「建议一个可编辑的标题」；
   Claude Code 明确区分三种标签（用户设的名字 / `目录名-2位后缀` 的默认显示名 / 后台小模型写的生成标题），
   并规定**只有用户设的名字能当 resume 句柄**。
4. **群聊形态的四个必备件**：① @提及路由 ② agent 侧栏状态机 ③ agent 独立身份（名字/头像/颜色）
   ④ 会话↔工件双向链接。
5. **热加入有两条路**：
   - **Grok Bot 式（人在环路）**：群里正常说话，**Bot 自己决定谁响应**；要指定就 `@Bot名`；
     要全体就 `@everyone`；Bot 之间交接时**只在 Bot 自己的聊天里可见**，群里看不到。
   - **DotCollab 式（agent 在环路）**：agent 用 `join` 工具带 role/@handle/skills **主动加入**，
     presence 状态 `listening / working / blocked(+etaMinutes, blockedOn)`，
     卡住时**冒泡到 owner 的「待你决策」托盘**。
6. **工件链接是双向、typed、可枚举**。Codex 有专门的
   `thread/attachment/{add,list,remove,owner/list}`：正向「session → 工件」加
   `(attachment_type, identity_key, payload)`，反向「工件 → 有哪些 session」用
   `owner/list` 查。Devin 的 PR hover card 反向跳会话、Claude Code 的 `--from-pr`
   按 PR 过滤会话，都是同一套东西的 UI 化。

### 2.2 关于您说的「bot 或者 dot」——三个候选，判定如下

| 候选 | 是什么 | 与您描述的吻合度 |
|---|---|---|
| **OpenAI dots**（2026-09-29 DevDay 发布） | 「always-on agent」，有自己的云端电脑/浏览器/记忆/名字/头像，profile 页分 **In progress / Scheduled / Completed** 三个标签页 | 名字对得上，但**目前不能群聊**：官方原话「over time, we envision teams of dots」；且「dot 不能参与群聊短信，只有一对一」⇒ **形态参考它，能力不能照搬** |
| **DotCollab**（dotcollab.ai） | 「workspace = 一群 agent 协作的单位，**由目标定义，而不是由外部工单定义**」；agent 用 `join` 加入，presence 三态 + 阻塞冒泡 + `acknowledge` 渲染成 ✓✓ 收据条（**不打消息气泡**）+ 重连保持同一 session + 同名 agent 活体时自动变 `ABC(2)` 共实例 | **吻合度最高**（您要的「热加入」「群里选 agent」「关联任务」都在），但它是**开发者向 MCP 工具 API**，不是终端用户 UI ⇒ **形态可抄，交互要自己设计** |
| **Grok Bot**（xAI, 2026-08-11） | Bot 是有名字/头像/颜色的「队友」，各自有云端电脑；**放进群聊后 Bot 之间自己协调、互相交接活儿、只在需要人判断时才拉你进来**；群聊自动生成名字可改；侧边栏有「需要关注 / 未读 / 正在工作」三态；Cmd+K 搜索；`Cmd+1..9` 跳 Bot | **产品化程度最高**（已 GA）⇒ **交互形态直接对标它** |

⇒ **建议**：**交互形态照抄 Grok Bot**（它的群聊、状态三态、@路由、命名都是已验证的），
**数据模型照抄 Codex**（session 树 + `parentThreadId` + typed attachment），
**「加入/退出/阻塞」语义照抄 DotCollab**（presence 三态 + 收据条 + 共实例），
**命名规则照抄 Claude Code**（用户名 > 生成标题 > 默认名，只有用户名作句柄）。

### 2.3 值得抄的具体做法（Top 8）

| # | 做法 | 出处 | 为什么值得抄 |
|---|---|---|---|
| 1 | 会话列表扁平；agent 作为行内字段；分组用**用户自建 section** | Codex `thread/list` + `thread_sections` | 一举解掉「按 agent 分类难找」 |
| 2 | 折叠行展开显示：Session ID / 创建时间 / 更新时间 / 目录 / git 分支 / 首条消息 | Codex resume picker（实测渲染） | 折叠时一眼可扫，展开时信息全 |
| 3 | 列表筛选器：`Filter: Cwd|All|Repo` + `Status: Active|Archived` + `Sort: Updated|Created` + 输入即搜 | Codex resume picker（实测） | 把「找会话」从「分类浏览」变成「检索」 |
| 4 | 一个 session 树共享 `sessionId`，每个 agent 线程有自己的 `id` 与 `parentThreadId` | Codex `thread/list` 模型 | **这正是「一个 session 就是一群聊」的标准结构** |
| 5 | `@Bot名` 定向提问 + `@everyone` 广播 + Bot 自己决定谁响应 | Grok Bot | 人不必每次点名 |
| 6 | 侧边栏三态：需要关注 / 未读 / 工作中（含 typing） | Grok Bot、Claude Code agent view | 让「谁在忙、谁卡住了」可见 |
| 7 | 「已完成」类消息渲染成 ✓✓ 收据条，**不打气泡**；补充意见折叠在线程里 | DotCollab | 群聊最大的噪音源是「收到」「好的」 |
| 8 | 会话↔工件双向 typed 链接 + 反向查询 | Codex `thread/attachment/*`、Devin PR hover card | 「这个 PR 是哪个会话做的」必须能反查 |

---

## 三、方案选型

| | **A · 只改列表** | **B · Session 实体化（推荐）** | **C · 群组泛化** |
|---|---|---|---|
| 做什么 | 前端去掉 `groupByAgent`，改扁平列表 + 折叠分区；命名与关联只做前端本地 | 新增 `sessions` 实体；`conversations` 变成 session 下的 agent 线程；群能力复用并补齐唤醒/发言/presence | 把 `chat_groups.task_id` 变可选、去掉 UNIQUE，群升级为「可脱离 task 的会话容器」 |
| 满足「一 session 一群聊」 | ❌（仍是 1 会话 1 agent） | ✅ | ✅（但 agent 仍各跑各的会话） |
| 满足「会话内多 agent」 | ❌ | ✅ | ⚠️ 成员仍是独立会话，无共享上下文 |
| 满足「热加入」 | ❌ | ✅（presence + 唤醒） | ⚠️ 可加成员，但无唤醒 |
| 满足「关联任务/大厅/策略/资产」 | ⚠️ 只能存前端 | ✅（DB 外键） | ⚠️ 只能绑 task |
| 满足「可搜索可排序」 | ✅（前端） | ✅（后端查询） | ✅ |
| 改动量 | 小（~1 天） | 中（~2 周） | 中（~1 周） |
| 迁移风险 | 无 | 需迁移现有会话归属 | 需放开约束，可能影响额度/状态机 |
| 会不会推翻已有群实现 | 否 | **复用**（群 = session 的一种） | 是（改群语义） |

⇒ **推荐 B**：它是唯一真正兑现「一个 session 就是一群聊」的形状，且**不推翻**已有的
群/额度/并发/禁用工具实现 —— 只是把 `chat_groups` 提升成 `sessions` 的一个特例。

---

## 四、推荐方案 B 的详细设计

### 4.1 数据模型

```sql
-- ① 会话（群聊容器）：一个 session = 一群聊
CREATE TABLE sessions (
    id            BIGSERIAL PRIMARY KEY,
    name          TEXT NOT NULL DEFAULT '',        -- 用户显式名（唯一句柄来源）
    title         TEXT NOT NULL DEFAULT '',        -- 生成标题（模型摘要）
    name_source   TEXT NOT NULL DEFAULT 'auto'     -- user | generated | default
                  CHECK (name_source IN ('user','generated','default')),
    kind          TEXT NOT NULL DEFAULT 'chat'
                  CHECK (kind IN ('chat','task_hall','side_question')),
    task_id       BIGINT REFERENCES tasks(id) ON DELETE SET NULL,   -- 可空！
    strategy_id   BIGINT REFERENCES strategies(id) ON DELETE SET NULL,
    asset_id      BIGINT REFERENCES assets(id) ON DELETE SET NULL,
    pinned_at     TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_sessions_updated ON sessions(updated_at DESC);
CREATE INDEX idx_sessions_task    ON sessions(task_id) WHERE task_id IS NOT NULL;

-- ② 会话成员：一个 agent 在会话里的席位（可热加入/退出）
CREATE TABLE session_members (
    session_id  BIGINT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    agent_key   TEXT   NOT NULL REFERENCES agents(key) ON DELETE CASCADE,
    role        TEXT   NOT NULL DEFAULT 'member'
                CHECK (role IN ('dispatcher','member','observer')),
    presence    TEXT   NOT NULL DEFAULT 'idle'
                CHECK (presence IN ('idle','listening','working','blocked','left')),
    eta_minutes INTEGER,
    blocked_on  TEXT,
    joined_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    left_at     TIMESTAMPTZ,                      -- 新增：支持退出
    PRIMARY KEY (session_id, agent_key)
);

-- ③ 会话↔工件：typed + 双向（抄 Codex 的 attachment 模型）
CREATE TABLE session_attachments (
    id             BIGSERIAL PRIMARY KEY,
    session_id     BIGINT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    attachment_type TEXT NOT NULL,   -- 'task' | 'strategy' | 'asset' | 'pr' | 'finding' | 'external'
    identity_key   TEXT NOT NULL,    -- 稳定标识（PR 用 'owner/repo#12'）
    payload        JSONB NOT NULL DEFAULT '{}',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (session_id, attachment_type, identity_key)
);
CREATE INDEX idx_sess_attach_owner ON session_attachments(attachment_type, identity_key);

-- ④ 会话消息：把现有 chat_group_messages 泛化（from_agent/to_agent/broadcast 保留）
--    chat_group_messages 保留为兼容视图，或直接改名 + 加 session_id 列
CREATE TABLE session_messages (
    id             BIGSERIAL PRIMARY KEY,
    session_id     BIGINT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    parent_message_id BIGINT REFERENCES session_messages(id) ON DELETE CASCADE,  -- 线程/回复层（episode 的载体）
    from_agent     TEXT NOT NULL DEFAULT '',   -- '' = 人
    to_agent       TEXT NOT NULL DEFAULT '',   -- '' = 广播给全会话
    kind           TEXT NOT NULL DEFAULT 'note'
                   CHECK (kind IN ('note','request','answer','finding','escalation','receipt')),
    body           TEXT NOT NULL DEFAULT '',
    task_id        BIGINT REFERENCES tasks(id) ON DELETE SET NULL,
    intent_id      BIGINT REFERENCES exploration_nodes(id) ON DELETE SET NULL,
    finding_id     BIGINT REFERENCES findings(id) ON DELETE SET NULL,
    status         TEXT NOT NULL DEFAULT 'unread'
                   CHECK (status IN ('unread','read','acked','blocked')),
    blocked_reason TEXT NOT NULL DEFAULT '',
    claims_authorization BOOLEAN NOT NULL DEFAULT false,
    -- 收据条：agent 的「收到 / 好的」类消息只落这里，不进消息流（避免群聊被
    -- 确认噪音淹没）。UI 渲染成 ✓✓ 贴在原消息下方（抄 DotCollab 的 acknowledge）。
    receipt_of     BIGINT REFERENCES session_messages(id) ON DELETE CASCADE,
    receipt_action TEXT NOT NULL DEFAULT ''
                   CHECK (receipt_action IN ('','taken','noted','deferred')),
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    read_at        TIMESTAMPTZ
);
CREATE INDEX idx_session_messages_session ON session_messages(session_id, id);
CREATE INDEX idx_session_messages_parent  ON session_messages(parent_message_id) WHERE parent_message_id IS NOT NULL;
CREATE INDEX idx_session_messages_receipt ON session_messages(receipt_of) WHERE receipt_of IS NOT NULL;
```

⚠️ **三处引用这张表的地方必须对齐**（实施时最容易漏）：
- §10.7 的 `agent_message(parent_message_id)` ⇒ 对应本表的 `parent_message_id`（episode 层的载体）
- 迁移表里的「群消息同时写 `session_messages`」⇒ 本表是**双写**目标（`chat_group_messages` 保留兼容视图）
- 收件条语义（§10.2 的做法 7）⇒ 本表的 `kind='receipt'` + `receipt_of` + `receipt_action`
  ⇒ **`receipt_of` 是必填**：没有它就分不清「一条普通消息」和「贴在某条消息下的收据」，
  UI 只能靠 `kind` 判断，而 `kind='receipt'` 的消息体为空 ⇒ 挂不到原消息上。

```

**兼容策略（关键：不做破坏性迁移）**

| 现状 | 处置 |
|---|---|
| `conversations` 表 | **不动结构**，只加一列 `session_id BIGINT REFERENCES sessions(id)` |
| 现有会话（`agent_key` 单值） | 迁移时**每条会话自成一个 session**，`session_members` 插一行 `agent_key` 席位 |
| `chat_groups` | 保留；建 session 时 `kind='task_hall'` 并回填 `session_id`；`task_id` 继续用（可空化） |
| `chat_group_members` / `chat_group_messages` | 保留为 group 专属视图；群消息同时写 `session_messages`（新表），避免破坏额度/状态机 |

### 4.2 命名规则（抄 Claude Code 的三标签）

| 标签 | 来源 | 何时用 | 能否当检索句柄 |
|---|---|---|---|
| **用户显式名** | 用户手改 / 建群时填 | 一旦设置永不被覆盖 | ✅ 唯一句柄 |
| **生成标题** | 后台小模型摘要首轮对话 | 未设名时 | ✅ 可搜 |
| **默认显示名** | `目录名-2位后缀` | 连生成标题都还没有 | ❌ 仅显示 |

规则：
- 生成标题**永不覆盖**用户显式名（ChatGPT 有这个 bug，别学）。
- 自动命名从「首行截 40 字符」升级为「模型摘要 + 首行兜底」。
- 建群时可预填；生成标题走异步，列表上先显示占位。

### 4.3 Agent 花名册与热加入（抄 Grok Bot + DotCollab）

**花名册**：每个 agent 有独立身份（名字 / 头像 / 颜色）。agent 名下可登记 MCP 可见性
（`agent_visibility` 已有）。

**加入方式（两条都支持，用户可任选）**：

| 方式 | 时机 | 交互 |
|---|---|---|
| **建会话时选** | 创建 session 时 | 多选 agent（≥1），选谁是 dispatcher（默认 auto） |
| **会话中热加入** | 任何时候 | ① 在会话里 `@Agent名` ⇒ 若该 agent 尚未在席，**先加入再被定向**；② 侧边栏「+ 添加 agent」 |

**加入后的行为**：
1. 写 `session_members` 一行（`presence='listening'`）。
2. **唤醒**：这是现在最大的缺口（邮箱只在 turn 开始时读）⇒ 必须加一个
   **会话级唤醒器**：session 有新消息且成员 `presence='listening'` ⇒ 起一个 turn。
   ⚠️ 这会引入新的并发与额度消耗路径 ⇒ 需明确「被 @ 的 agent 才自动醒」还是「所有 listening 成员都醒」
   ⇒ **建议前者**（Grok Bot 的语义：Bot 自己决定谁响应 ⇒ 但 ARTEX 是确定性系统 ⇒ 用
   「@ 定向 ⇒ 必醒；非 @ ⇒ 成员按需自取」的两段式）。
3. `presence` 三态：`listening` / `working` / `blocked(+eta,blockedOn)`；blocked 冒泡到
   「待你决策」托盘（DotCollab）。

**群内发言（修硬伤 ③）**：实现 `message_group` 工具（键已在
`agent/approval_policy.go:104` 预留）⇒ agent 能在群里发 `note/request/answer/finding/escalation`，
且 `to_agent` 支持定向与广播。

**噪音控制**：agent 的「收到/好的」类消息渲染成 ✓✓ 收据条（`acknowledge` 语义），**不打气泡**；
补充意见折叠在线程里（`comment_message` 语义）。

### 4.4 关联模型（任务 / 任务大厅 / 策略库 / 资产）

- **正向**（session → 工件）：`session_attachments`。
- **反向**（工件 → session）：`GET /api/sessions/attachments/{type}/{identity_key}/sessions`
  ⇒ 「这个 PR / 这个漏洞是哪个会话做的」能反查（Devin PR hover card / Codex `owner/list`）。
- **内置工件类型**：`task`（任务）、`task_hall`（任务大厅群）、`strategy`（策略）、
  `asset`（资产）、`finding`（漏洞）、`pr`（外部）。
- 会话详情页加一个「关联」区块：已关联的工件 + 「添加关联」入口（可搜索任务/策略/资产）。

### 4.5 API 变更（新增为主，尽量不动旧的）

```
# 会话（新的第一类对象）
GET    /api/sessions                      # 扁平列表：按 updated_at，附 agent 名册、状态、关联摘要
POST   /api/sessions                      # 建会话：name? agents[] dispatcher? attachments[]
GET    /api/sessions/{id}                 # 详情：成员、presence、关联、状态
PATCH  /api/sessions/{id}                 # 改名 / pin / 关联增删
DELETE /api/sessions/{id}
GET    /api/sessions/{id}/messages        # 会话消息（含 agent 消息、收据、线程）
POST   /api/sessions/{id}/messages        # 发言（@Agent名 路由）
POST   /api/sessions/{id}/members         # 热加入 agent
DELETE /api/sessions/{id}/members/{agent} # 退出
GET    /api/sessions/{id}/presence        # 成员状态
GET    /api/sessions/attachments/{type}/{key}/sessions   # 反查

# 保留并兼容
GET    /api/conversations                 # 旧列表继续可用（映射到 session 视图）
POST   /api/conversations                 # 旧建会话 → 自动建 session + 单成员
```

### 4.6 前端信息架构

**侧边栏**（对话入口改名「会话」）：

```
▾ 置顶
   ▸ 漏洞复测 — 攻击面梳理            [2 agent]  ●工作中
▾ 最近
   今天
     ▸ 资产同步排查                   [auto, pentest]  ●需关注
     ▸ 策略库评审                      [curator]      ○
   更早
     ▸ ...
```

**列表行字段**（抄 Codex 折叠行）：
`标题/生成标题` · `相对时间` · `agent 名册（头像组 + 数量）` · `状态点` · `关联工件小图标`
· `钉住` · `↗ 关联对象`。
点击展开：`Session ID / 创建时间 / 更新时间 / 目录 / 关联工件 / 参与 agent 与其 presence`。

**工具条**：`搜索` · `筛选（全部 / 有未读 / 工作中 / 需关注 / 已归档）` · `排序（最近 / 创建）`
· `视图（扁平 / 按工件分组 / 按 agent 过滤）` ⇒ **agent 变成一个筛选维度，而不是分组轴**。

**会话页**：
- 顶部：会话名（可改）+ agent 花名册（头像 + presence 状态点 + 「+ 添加」）
- 主体：共享消息流（人 / agent 同一条流；agent 消息带头像与名字；收据条不占行）
- 输入框：`@` 唤出 agent 选择器；`@everyone`；附件；工具调用按组折叠
- 右侧抽屉：关联的工件（任务 / 策略 / 资产）、参与 agent 的状态与配额

### 4.7 迁移与兼容

1. **不动** `conversations` 结构，只加 `session_id` 列（幂等 `ADD COLUMN IF NOT EXISTS`）。
2. 启动时一次性回填：每条 `conversations` ⇒ 自成一个 session + 一行成员。
3. `chat_groups` 建 session（`kind='task_hall'`）并回填 `chat_groups.session_id`。
4. 旧前端仍可用 `/api/conversations`；新前端优先用 `/api/sessions`。
5. **可回退**：回填是加列 + 加行 ⇒ 回退只需停用新前端，旧数据无损。

---

## 五、任务大厅优化（在方案 B 之上）

| # | 改动 | 目的 |
|---|---|---|
| 1 | 群 = session 的一种（`kind='task_hall'`）⇒ 大厅列表与会话列表**同一套列表机制**，可统一搜索 | 一处找全 |
| 2 | 群详情把「额度进度条」升级为**状态语义**：剩余额度、预计耗尽时间、并发占用（谁占着） | 看数字不如看状态 |
| 3 | intent 看板在无 worker 时显示「本群未派发意图」而非空白 | 消除空态困惑 |
| 4 | 群成员区加 **presence 三态** + 「待你决策」托盘 | 谁卡住了 |
| 5 | 大厅里每个群可直接「升为普通会话」或「从会话创建群」 | 两种心智互转 |
| 6 | 关联面板：群 ↔ 任务 / 策略 / 资产 | 打通资产面 |

---

## 六、出网治理补齐（**已完成**，2026-11-03）

> 本节原为待办。实际做下来比原计划多挖出一层根因：范围校验**从未真正生效**。
> 三个提交：`0017915`（接线 + 三类角色带身份）、`feff84d`（web_fetch 带身份 + 清单修正）、
> `1fb6f49`（CI 门禁修复，见 §6.4）。

### 6.1 根因：范围校验从未接线

`traffic/egress.go` 的语义是「未注入范围判定 ⇒ **带任务身份的请求一律拒绝**」。
而 `SetEgressScope` **在整个仓库里只被 traffic 包的测试调用过** ⇒ 生产路径上
`egressScope()` 恒为 nil ⇒ **worker 的出网一直是全拒的，那套范围校验形同虚设**。
适配器 `DBEgressScope` 本来就写好了（用 `PG.TrafficScopeHosts` + `AppendAuditEvent`），
只是没人调用。修法一行：manager.go 在 `traffic.Open` 成功后注入。
**这只放宽不收紧**：未接线是「带身份⇒全拒」，接上后是「带身份⇒范围内放行、范围外拒绝」，
且范围判定不可用时仍然拒绝（fail-closed 不变）。

### 6.2 三类角色补上任务身份

worker 一直是 `proxyEnv(addr, ca, taskID)`（带身份），planner / 主 agent / chat agent
都是 `proxyEnv(addr, ca)`（不带）⇒ 这三个角色的出网按「未归属」放行，完全不受范围校验。

三处 taskID 来源不同（planner/main 从函数参数拿，chat agent 没这个参数），散着写容易再漏，
故统一成一个函数 `agent/egressTaskID(ctx, taskID)`：显式 > 0 ⇒ 用它；否则回落 ctx 的
`RunInfo.TaskID`（群聊成员会话带着任务身份）；都没有 ⇒ 0 ⇒ **不伪造身份**（没有任务就没有
范围边界，装个假身份比不带更危险）。

### 6.3 web_fetch 也带上身份

web_fetch 一直在走录制代理（`WebFetchProxy` = 录制代理地址）⇒ 它并不是「不走 netguard」，
真正缺的是**任务身份**：代理地址里没有令牌 ⇒ 判不出归属 ⇒ 不受范围校验。
新增 `agent/egressProxyURL(proxyAddr, taskID)`（把令牌放进代理 URL 的 userinfo，
与 `proxyEnv` 同法），四个角色的 `WebFetchProxy` 全部改用它；`proxyEnv` 也改为复用它
⇒ 两条出网路径的身份口径从此只有一处。

**web_search 没一起改**：它走独立出口代理（socks5/全局代理），不是录制代理 ⇒ 没有可塞令牌的
http 代理地址（socks5 的 userinfo 是另一套认证）；改走录制代理也不行 —— 搜索服务商的域名不在
任务资产范围里，会被拒 ⇒ 工具直接不可用。要真正覆盖它得先引入「范围豁免清单」，
那是独立的一项改造，**不夹在这次修复里悄悄做掉** ⇒ 它继续留在未覆盖清单里，但原因改准了。

### 6.4 清单同步 + CI 门禁修复

- `server/egress_governance.go` 的未覆盖通道清单是**代码事实**，必须与代码同步：删掉
  「web_fetch / web_search 都不受校验」那条合并项（web_fetch 已受校验）；新增「web_fetch 的
  直连兜底」（SDK 在代理**连接层**出错时会直连重试一次，那条路径不经范围判定 ——
  但范围拒绝本身不触发它，代理返回的是状态码，而 SDK 的「像代理故障」判定只匹配传输层错误）。
- ⚠️ 推送后发现 `feat/task-hall` 分支的 CI 连着三次红，断在 `go build ./...`。查下去发现
  **这条分支从来就没绿过**：`go.mod` 用 `replace ... => ./third_party/norma`（`27270e6` 引入），
  而 `third_party/` 被 gitignore（git 一个文件都不跟踪）⇒ 干净克隆里那个目录不存在 ⇒
  `go build` 第一步就报「replacement directory does not exist」，**后面的 vet / 测试全被跳过 ——
  门禁形同虚设**。main 不受影响（它的 go.mod 没有 replace 指令）。
  修法：`scripts/prepare-norma.sh` 在模块缓存缺原版时自己下载（带显式版本才绕得开 replace），
  并在 `ci.yml` / `release.yml` 的 Go 构建之前各插入一步。
  **验证**（干净克隆 + 空模块缓存 = CI 的真实条件）：prepare ✓、build ✓、vet ✓、版本校验 ✓✓
  ⇒ 现在 CI 的 `go test` 真的在跑（之前是 `skipped`）。

### 6.5 尚未覆盖的（清单里的 7 条，逐条对应）

1. web_search：走独立出口代理，不经录制代理也不带任务身份（要覆盖需先引入范围豁免清单）
2. web_fetch 的直连兜底：SDK 在代理连接层出错时直连重试一次
3. MCP 的外部通道：MCP 服务端自己发出的请求不由本平台策略管辖
4. 宿主侧出网：容器与宿主进程自身的网络请求
5. 全局出网代理路径：流量捕获关闭时 Bash 走全局代理，那条路径没有目的地判定
6. 未归属的出网：请求不带任务身份时按「未经范围校验」放行并留痕
7. 20 条内置 deny 规则只挡破坏性命令，不挡范围外出网（其中「数据外泄管道」规则出厂即 disabled）

### 6.6 交付验证

交付镜像 `f3bb867cae95`（`0.4.1-110-gfeff84d`）的二进制里直接 grep：
新清单条目在、旧条目不在、新符号 `egressProxyURL` / `egressTaskID` 都在 ⇒ 修复确实进了交付物。

| 洞 | 目标状态 | 验收 |
|---|---|---|
| (a) planner / 主 agent / 对话 agent 的 Bash 不带任务身份 | 三类角色的 Bash 执行路径**全部带上任务身份令牌** ⇒ 出网进入 netguard 范围判定 | 构造一个「范围外 host」的请求 ⇒ 三类角色都被拒并留痕 |
| (b) web_fetch / web_search 旁路 | 二者**接入 netguard transport** 或**改走子进程 http_proxy** | 同上 ⇒ 被拒并留痕 |

原则不变：**fail-closed**、阈值进代码常量、判定写 `sod_decisions` 账本。

⚠️ 代码定位结果回来后，我会先给出**最小正确改法**再动手；若 (b) 确认不可在本仓库内闭环
（例如工具实现在 provider 侧），我会**如实说明**并给替代方案（把该工具从 agent 可用清单里移除，
或改走带代理的 HTTP 客户端），不做假修复。

---

## 七、分期与验收（已按第十节的记忆层结论重排）

> ⚠️ 本表原先把记忆层当成独立的一期，那是**第十节调研之前**的排法。第十节的结论是
> **记忆层不构成独立的第五期，而是第四期（sessions 实体化）的延伸** —— 因为
> `memory_index.source_session_ids` / `agent_session.parent_session_id` / `scope_type=scope_id`
> 说的就是 sessions 本身，另起一套 session 概念会出现两套。所以记忆层的表与检索并进 P2/P4。

| 期 | 内容 | 验收口径 |
|---|---|---|
| **P0** ✅**已完成** | 出网治理补齐：`SetEgressScope` 接线（范围校验此前从未生效）+ 三类角色带任务身份 + web_fetch 带身份 + 清单同步 + CI 门禁修复 | 提交 `0017915` / `feff84d` / `1fb6f49`；范围外请求被拒且留痕；CI 的 `go test` 真的在跑（此前被跳过） |
| **P1** | 会话列表扁平化（不改数据结构的前端版）+ 命名规则（用户名 > 生成标题 > 默认名）+ 搜索/排序/筛选（agent 变成**筛选维度**而非分组轴） | 「找上周某个会话」≤ 3 次点击；现有会话全部可见；老视图仍可切回 |
| **P2** | `sessions` 实体 + `session_members`（presence 三态 + `left_at`）+ `session_attachments`（typed 双向）+ **记忆层核心三表**（`memory_index` / `memory_topic` / `memory_write_log`）+ 迁移回填 | 老会话零丢失；老 API 兼容一个版本周期；记忆表 `scope_type/scope_id` **NOT NULL** 且未 scope 的读写用 typed error 拒绝 |
| **P3** | 群聊化：会话内多 agent、@路由（@ 定向必醒 + 非 @ 按需自取）、热加入、`message_group` 实现、会话级唤醒器 | 一个会话里两个 agent 能互相看见并接力；@某人必醒；群里 agent 能发言 |
| **P4** | 任务大厅并入会话体系 + 状态语义化（额度/并发从数字变状态）+ 关联面板 + **记忆检索与 dream 任务**（索引上限写时拒绝、绝不静默截断；`tsvector`/pgvector 混合检索用 RRF 融合） | 大厅与会话同一套检索；索引超限时**写入被拒**而非截断；检索上下文按 COMPLETE/PARTIAL/INSUFFICIENT 分级 |
| **P5** | 收件条/线程折叠等噪音治理 + 跨 session 共享上下文（**已定稿：推到 P5**，见 §9.1 第 10 条） | 群消息气泡数下降可量化 |

### 各期与红线的关系（不变）

- 新增的一切安全边界进**代码常量**（`internal/sodpolicy`），不进可热改的 DB/settings。
- 模型只提议、确定性层裁决：生成标题、摘要这类可以用模型；**安全判定不行**。
- 记忆层不是安全旁路：从记忆读入的内容一律当**不可信输入**（可能来自工具输出、可能是被污染的经验）⇒
  记忆不能扩大工具权限，不能绕过 SoD 裁决 ⇒ 写路径必须有出处校验（§10.5 的第 8 条）。
- `db` 不 import `agent`；`internal/sod` 不 import `agent`/`server`；DDL 幂等。
- **记忆是「提示」，不是「安全机制」**：记忆可查看、可编辑、可禁用（见 §11.1）；
  必须强制的规则属于代码常量与 hook，不属于记忆文件。两者不可互相替代。

## 八、红线（沿用既有纪律）

1. **不改** `pentest / reporter / planner / auto` 的既有提示词（您的原话：每个文件独立存在、
   不修改、不引用、不覆盖）。
2. **模型只提议，确定性层裁决** —— 生成标题、摘要这类可以用模型；安全判定不行。
3. **一切新增安全边界进代码常量**，不进可热改的 DB/settings。
4. **fail-closed**：新链路读不到状态 ⇒ 拒绝，不是放行。
5. **不删数据**：迁移只加列加行；旧 API 保留一个版本周期。
6. **DDL 幂等**，`db` 不 import `agent`，`internal/sod` 不 import `agent`/`server`。

---

## 九、决策定稿（2026-11-02 · Sir 已授权按推荐执行）

原先挂在您这里的 5 个待定项，按下表的推荐**全部定稿**；第 6 条是记忆层调研之后新出现
的同一类问题（§10.6 的报告建议），也按推荐定稿。定稿即写进实现口径，后续实施不再逐条
回头确认；若实施中发现某条不可行，会带证据来找您，不静默改。

| # | 议题 | **定稿** | 理由 |
|---|---|---|---|
| 1 | 群聊里谁被唤醒 | **@ 定向 ⇒ 必醒；非 @ 成员 ⇒ 按需自取**（两段式） | Grok Bot 的语义是「Bot 自己决定谁响应」，但 ARTEX 是确定性系统且额度是硬约束 ⇒ 全员自动醒会让额度被自动烧穿。两段式既保证「@得到答复」，又不产生意外消耗 |
| 2 | 新会话默认带哪些 agent | **默认只带 `auto`（调度者）一个**；建群时可多选；会话中可随时热加 | 单 agent 起步最省额度，也贴合现有「auto 是分发者」的既有角色；多选留给用户主动表达 |
| 3 | 一个 session 树是否跨 session 共享上下文 | **P3 只做「同一 session 内共享」；跨 session 共享推后到 P5** | 跨 session 共享会把权限边界、额度归属、审计留痕三件事同时复杂化 ⇒ 先在 session 内把共享语义做干净，再谈跨 session |
| 4 | 老会话列表是否保留「按 agent 分组」 | **保留为可选视图开关，默认关闭** | 不强制改变习惯；数据模型上「按 agent 过滤」比「按 agent 分组」更该是一等公民（agent 是列，不是轴） |
| 5 | 关联对象是否支持外链（GitHub PR / 外部工单） | **支持**（`attachment_type='pr'|'external'`） | 「这个 PR 是哪个会话做的」必须能反查 —— 这是 Codex `thread/attachment/owner/list` 与 Devin PR hover card 的共同做法，也是把会话接回工作流的关键 |

| 6 | 持久记忆归谁所有：agent 还是 task-hall？ | **task-hall**（per-agent 记忆 = N 份发散副本 + 写冲突；task-hall 范围 + agent 归属写入才有单一真相与审计痕迹） | 调研报告 §7 的建议；与 §10.6「记忆层的 scope 就是 session」自洽 |

**§9 第 6 条的落地含义**：`memory_index.scope_type/scope_id` 指向 session（`kind='task_hall'` 或
`'chat'`），**不指向 agent**；`written_by` 记录是哪个 agent 写的（审计用，不是归属）。
「X 日那天为真是什么」这个问题**现在不需要回答** ⇒ 不进方案 C（时序知识图）。
但 `supersedes_id` / `valid_from` / `valid_to` **从 P2 第一天就在** ⇒ 将来要进 C 也不用迁移。

### 由这 6 条派生的实现约束（写进代码前先钉住）

1. **唤醒器只对「显式 @」负责**：会话级唤醒器只唤醒被 @ 的成员；未 @ 的成员由
   「取件」语义（自己在下一轮取）驱动 ⇒ 唤醒路径与「自取」路径必须分开实现、分开计量。
2. **`auto` 在 session 内是 dispatcher 角色**，不与成员抢活；成员发消息用 `message_group`
   （键已在 `agent/approval_policy.go:104` 预留）。
3. **presence 三态语义**：`listening`（在席可被唤醒）/ `working`（正在跑）/ `blocked(+eta,blocked_on)`；
   退出写 `left_at`，不改历史消息的归属。
4. **「按 agent 过滤」是一等筛选维度**，与「按 agent 分组」的旧视图并存但默认关；
   两者共用同一份后端列表数据 ⇒ 前端不额外请求。
5. **外链附件必须有稳定 identity_key**（如 `owner/repo#12`）⇒ 反查走
   `GET /api/sessions/attachments/{type}/{identity_key}/sessions`。

---

### 9.1 第二批拍板项（2026-11-03 · Sir 授权「全部按推荐定稿」，不再逐条回头确认）

| # | 议题 | **定稿** | 理由 |
|---|---|---|---|
| 7 | `llm_usage` / `llm_records` 两张表不在 `db/schema.sql`（懒执行）⇒ 全新部署「先读后写」的统计路径在首次写入前报「relation does not exist」 | **给建表用一把独立的 advisory lock key**（不动 `db/schema.sql` 这个 RESERVED 文件） | ① 在 `db.Open` 里建表会与并发测试**自我死锁**（会话级锁 + 同 key 的事务级锁嵌套），已实测；② 换一把独立 key 即可避开嵌套；③ `db/schema.sql` 是 RESERVED 文件，改它需要额外授权，不该为一张统计表消耗；④ 兜底：在读路径上先 Ensure（幂等） |
| 8 | ✅**已完成** web_fetch 的「直连兜底」缺口（SDK 在代理**连接层**出错时直连重试一次，该路径不经范围判定） | **加一个 patch**（`patches/norma-egress-scope.patch`）并让 `scripts/prepare-norma.sh` 支持按序应用多个 patch | ① 直连重试是 **SDK 内部行为**，本仓库改不了；② 已有 `patches/norma-privilege-drop.patch` + `prepare-norma.sh` 这套机制，扩展它成本最低；③ 不改上游、不改 gitignore 的生成目录 ⇒ 幂等可重放；④ 注意：范围拒绝**不会**触发它（代理返状态码，SDK 只匹配传输层错误）⇒ 补上后是「正常路径已受控 + 兜底路径也受控」 | |
| 9 | web_search 走独立 socks5 出口代理 ⇒ 不经录制代理、不带任务身份 ⇒ 不受范围校验 | **引入「范围豁免清单」**（平台基础设施类主机的白名单，如搜索服务商、模型 API 端点、镜像仓库），并**只对豁免清单内的主机放行**，其余搜索请求仍按「未归属」留痕放行 | ① 改走录制代理会让搜索服务商的域名因「不在任务资产范围」被拒 ⇒ 工具直接不可用；② socks5 的 userinfo 是另一套认证，塞令牌会弄坏代理；③ 「豁免清单」是通用的基础设施白名单机制（不只是为 web_search），可复用到模型 API / 镜像源；④ **豁免清单本身必须是代码常量**，不能落 DB/settings（否则又变成可热改的安全边界） |
| 10 | 跨 session 共享上下文（原 P5 保留待定） | **仍然推到 P5**，P3 只做「同一 session 内共享」 | 跨 session 共享会同时复杂化三件事：权限边界、额度归属、审计留痕 ⇒ 先把同 session 内的共享语义做干净，再谈跨 session |
| 11 | Phase 0 Task 4 的「DB 直连靠 DB role 挡」那半 | **保留为独立项，需要生产停机窗口，不擅自做** | ① 计划红线要求不得因「API 侧校验已完成」而降级它；② 它改的是 DB 权限模型 ⇒ 必须停机窗口 + 凭据轮换配套 |
| 12 | 现役 8787 实例（`artex-artex-1` / `artex-postgres-1`，仍 Exited） | **保持停机**，等 P1–P5 落地并在 19787 干净实例上验收通过后，再由 Sir 决定切流时点与方式 | ① 现役是旧代码（`505b196`），缺任务大厅/策略库/SoD/出网治理全部新面；② 先切会让人在旧代码上继续用；③ 切流需要停机窗口 + 数据迁移评估 |

⇒ 至此，本方案书不再保留「待拍板」项：所有决策均已定稿，实施时按本节与 §9 执行；
若实施中发现某条不可行，会带证据来找 Sir，不静默改。

## 十、记忆层与「第三层图」的调研结论（2026-11-03）

> 调研来源：① Claude Code 记忆机制（逆向 Claude Code 二进制 + 官方文档 + 第三方逆向文章）；
> ② 图形态 agent 记忆业界做法（Graphiti/Zep、Letta/MemGPT、Mem0、Cognee/GraphRAG）；
> ③ ARTEX 现有图谱的 schema 勘察。
> ⚠️ ② 的完整报告在 `~/.omo/ulw-research/20261103-001446/REPORT.md`（788 行），
> 已交付副本 `~/<交付目录>/claude/output/reArtex-agent-memory-research-2026-11-03.md`。
> ⚠️ ①③ 两个单元的最终结论文本未被持久化（只留下原始证据），下述内容是**从原始证据
> 重建**的结论，不是原报告原话 —— 引用时按「重建结论」对待。

### 10.1 「Claude 的五层记忆」到底指哪五层

**关键消歧**：官方文档（`code.claude.com/docs/en/memory`）只承认**两种**跨会话机制
（CLAUDE.md 与 auto memory）；「五层」是**第三方对 Claude Code 二进制的逆向分级**，
不是官方口径。按该分级：

| 层 | 机制 | 存储 | 生命周期 | 硬上限（逆向出的常量） |
|---|---|---|---|---|
| **T1** | Project Memory：`MEMORY.md` 索引 + 每条记忆一个主题文件 | `~/.claude/projects/<sanitized-git-root>/memory/` | 持久、跨会话 | `MAX_ENTRYPOINT_LINES=200`、`MAX_ENTRYPOINT_BYTES=25_000`；**超限后写入仍成功，但下次加载时尾部被静默丢弃**，并提示 Claude 重写索引 |
| **T2** | Session Memory：每会话一份 scratchpad | 会话目录 | 单会话；喂给压缩 | 初始化阈值 10000 token、两次更新间隔 5000 token、3 次工具调用；上限 12K token |
| **T3** | End-of-Turn Extraction（`extractMemories.ts`） | auto-memory 目录的主题文件 | 每项目持久 | forked agent、`maxTurns=5`、`skipTranscript=true`、独立沙箱 |
| **T4** | Dream Consolidation（`autoDream`） | 重写 T1 索引 + T3 主题文件 | 归档型 | **三重门**：≥24h（`minHours`）+ ≥5 次会话（`minSessions`，背后 10 分钟扫描节流）+ 锁文件；功能旗标默认关；`/dream` 自 v2.1.88 可见 |
| **T5** | Team Memory Sync | 服务端，镜像到磁盘 | 团队级 | `GET/PUT /api/claude_code/team_memory?repo=owner/repo`；拉取服务端优先、推送本地优先；单文件 250KB 上限 + 密钥扫描 + 路径穿越校验；功能旗标默认关 |

**官方文档里值得逐字抄下来的规则**（可移植）：
- auto memory 索引加载「**前 200 行或 25KB，取先到者**」；超限 → 写入仍成功但尾部被丢弃。
- CLAUDE.md 是**拼接不是覆盖**：「从文件系统根到工作目录排序，越靠近启动目录的越晚读」；
  同目录内 `CLAUDE.local.md` 追加在 `CLAUDE.md` 之后；`@path` import **最多递归四跳**。
- 「每个 CLAUDE.md 控制在 200 行以内」；「两条指令冲突时 Claude 可能任意选一条」。
- 记忆文件模板：frontmatter `name`（短 kebab slug）/ `description`（一行摘要，用于未来判断相关性）/ `metadata.type ∈ {user, feedback, project, reference}`；正文「先写规则/事实，再写 **Why:** 与 **How to apply:**」；相关记忆用 `[[name]]` 互链，且「指向尚不存在的记忆不算错」。
- 四类型的**排他性**定义：`project` = 代码或 git 历史推不出来的进行中工作/目标/约束（相对日期要转绝对）；`reference` = 外部资源指针。
- **写者沙箱**逐层收窄：T1 主 agent 全工具但只能显式保存；T2 只有一个工具且只准改一个路径；T3 只读 + 只能写 memory 目录；T4 同 T3 再加锁；T5 上传前密钥扫描。`autoMemoryDirectory` 只允许来自 policy/local/user 设置，**显式排除 projectSettings**（否则恶意仓库可把它指向 `~/.ssh`）。

### 10.2 「加一层图」的调研结论 —— **与直觉相反**

**业界正在离开图**：
- **Mem0 v3 把图记忆从 OSS SDK 里删了**：`enable_graph`、`graph_store` 与全部外部驱动
  （Neo4j / Memgraph / Kuzu / Apache AGE / Neptune）约 4000 行移除，图记忆变成闭源 Platform 功能。
  替代方案 = 只增不删的抽取 + 单独的时间推理 pass（给每条记忆打时间签名 + 时间意图重排 + 衰减系数）。
- **Letta（最像图的运行时）2026-08-16 砍掉约 367,000 行 V1 server**，转向 `letta-ai/letta-code`，
  新记忆模型 = **MemFS**：每 agent 一个 git 仓库，`MEMORY.md` 索引 + 每主题一个 Markdown，
  根文件进系统提示，深目录读到才进上下文。
- ⇒ **「是张图」不是差异点**。Graphiti/Zep 真正买到的是三样：**episode 出处**、
  **双时态有效性**（`valid_at/invalid_at` vs `created_at/expired_at`）、**用「关闭区间」代替删除
  来表达取代**。这三样在 Postgres 里都能做。真正贵的是**实体消解**（一次错误合并会污染所有邻近事实）。

**Graphiti/Zep 的代价**：LLM 成本在负载下打到配额的 **3–5 倍**；读己之写最坏 120 秒。
**Mem0 的代价**：只增不删（原文：「Your sole operation is ADD」）⇒ 记忆量只增不减，矛盾事实共存。
**Cognee 的代价**：普通文本入图后体积是原文的 **8–14 倍**，约 23–26 次查询才回本。
**检索预算饱和**：20/20 是拐点，30/30 只多 0.26 分却多花 45% token。
**投毒实测**（MemSecBench）：持久率 **84.2%**；**8 个模型里 6 个**会把工具输出直接写进
`MEMORY.md`；加一句提示能让比例从 40% 升到 80%。

**结论（报告原话）**：「**do not add a third graph layer to reArtex yet**」。
理由不是「图不好」，而是**第三张图不是瓶颈**，真正的三处瓶颈是：
1. **现有图层没有时态语义** ⇒ 在**既有**表上加 `valid_from` / `valid_to` / `superseded_by`；
2. **没有 episode 层** ⇒ 把 planner↔worker↔tool 的原始会话存成第一等行，其余都是带指针的派生投影；
3. **没人决定记忆归谁所有** ⇒ per-agent 记忆会得到 N 份发散副本 + 写冲突；
   task-hall 范围 + agent 归属写入才有单一真相 + 审计痕迹。

### 10.3 ARTEX 现状：「第三张图」可能已经有了

**`strategies` 已经是一张独立的顶层图谱层（L4）**，且**已经带双时态列**
（`valid_from` / `invalid_at` / `expired_at` / `retired_at`）⇒ 「加一层图」这个诉求在策略层
**已经落地**；真正缺的是**记忆 / episode 层**，不是又一张图。

关系矩阵（从 verbatim DDL 派生；原报告的矩阵未持久化）：

| 关系对 | 状态 | schema 事实 |
|---|---|---|
| findings ↔ assets | **PARTIAL** | `findings.asset_ids JSONB` —— JSON 数组，无外键、无引用完整性 |
| findings ↔ intents | **EXISTS** | `findings.node_id BIGINT REFERENCES exploration_nodes(id)` + 边 `intent --yields--> finding` |
| findings ↔ strategies | **PARTIAL** | `strategy_evidence(strategy_id → strategies(id) ON DELETE CASCADE, ref_type, ref_id TEXT, content_digest, role)` —— **无 FK 的是 `ref_id`（裸 TEXT）**，`strategy_id` 本身有级联 FK ⇒ **任务归档会连带删掉策略的证据链，策略变成「凭空提权」** |
| intents ↔ assets | **EXISTS** | `exploration_anchors(node_id, asset_id)` PK + `idx_anchor_asset` |
| intents ↔ sessions | **ABSENT** | intent 挂在 `exploration_id` 上，**没有任何东西指向会话** |
| strategies ↔ sessions | **ABSENT** | `strategies` 无 session 列；会话侧只有 conversations / chat_groups / chat_group_members / chat_group_messages |
| strategies ↔ tasks | **ABSENT** | 策略是必须跨任务存活的顶层图，租户维度是 `company_id` 而非 `task_id` |
| strategies ↔ strategies | **EXISTS** | `strategy_edges(from_id, to_id, relation ∈ requires/variant_of/blocks/supersedes)` |

⚠️ 由此得到一条**与第十节 4.4 节直接相关**的结论：会话与既有图谱之间**目前完全没有边**
⇒ 第四节的 `session_attachments` 不只是「关联工件」，它是**把会话接进图谱的唯一入口**。

另注：`third_party/norma/memory` 存在（MemFS 形态：`tools.go relevant.go memory.go`），且 `norma/agentcore` **import 了它并接进每个 session** —— 但 **ARTex 从不配置 `Options.Memory`** ⇒ 这条路径是「死的」而非「不存在」⇒ 有一条现成但未启用的记忆库路径。⚠️ 但它是**错误的**捐赠者（无索引上限 / 无 scope / 无 supersession / 截断不响亮）⇒ 方案 B 仍按「自建」估算工作量。见 §11.7。

### 10.4 三个记忆方案（工作量/代价是工程判断，非厂商数据）

| 方案 | 工作量 | 数据模型 | 判断 |
|---|---|---|---|
| **A** 现有图 + 摘要行 | **S**（1–2 人日） | `memory_summary(scope_type, scope_id, content, topics[], embedding, source_session_ids[], revision)` + 既有资产/探索表加 `valid_from/valid_to/superseded_by` + 新节点类型 `session_summary` | 单一摘要点 ⇒ 信息损失；无主题粒度；只在会话关闭时新鲜；范围问题被推迟 |
| **B** 每会话记忆索引 + 主题指针 | **M**（3–5 人周） | `memory_index`（`topic_key`、`title ≤120`、`summary_line ≤200`、`topic_id→memory_topic`、`tags[]`、embedding、revision、**`supersedes_id`**、`valid_from`、`valid_to`、confidence、`source_session_ids`、`written_by`，UNIQUE(scope_type,scope_id,topic_key)）+ `memory_topic`（正文 markdown）+ `memory_write_log`（op/actor/prev_hash/new_hash）+ `agent_session(parent_session_id, task_hall_id, agent_role)` + `agent_message(parent_message_id)` | **推荐**。热路径：`memory_write`（对 revision 做 CAS → 冲突抛 typed error）、`memory_link`、`memory_retire`（**永不硬删**）。冷路径：dream 任务强制索引上限（如 150 行 / 20KB）并整合。检索：常驻被上限夹住的索引行 + 按需 `memory_read(topic_key)` + `tsvector`/pgvector 混合检索用 RRF 融合，**scope 过滤强制** |
| **C** episode 上的时序知识图 | **L**（2–4 人月） | `episode(group_id NOT NULL, reference_time, created_at, content_hash)` + `entity(canonical_name, name_hash, merged_into)` + `entity_edge(valid_at, invalid_at, created_at, expired_at, invalidated_by)` + `entity_edge_episode` + 可选 `entity_community` | 主导成本是**每 episode 一次 LLM**；实体合并的爆炸半径（Graphiti 没有拆分路径 ⇒ 要补 `merged_into`）；读己之写延迟；scope 强制是你自己的活 |

### 10.5 要抄的顺序（报告原话，优先级排序）

1. **scope key 在代码里强制**，未 scope 的读写用 typed error 拒绝（抄 Mem0 的 `VALIDATION_001`）；
2. **索引 + 明细，索引有硬上限，写入时拒绝，绝不静默截断**；
3. **episode 层**（不可变，`reference_time` 与 `created_at` 分开）；
4. **取代（supersession）是显式操作**：`supersedes_id` + `valid_to` + 只追加的日志；
5. **多写者 scope 用 CAS**；
6. 双通道写（dreaming 63.5% > tools 53.9% > 两者 66.1%）；
7. 混合检索 + 确定性融合；
8. 写路径校验 + 出处（provenance）；
9. 检索到的上下文分级（COMPLETE/PARTIAL/INSUFFICIENT）与答案正确性分开评。

**不要抄**：把第三张图当第一步；默认挂 LLM reranker；只增不删却不带 supersession（Mem0 issue #5867 的「C罗/梅西」bug）；把不可信工具输出原样写进记忆；用静默截断处理溢出；在需要全局查询之前就建社区摘要层级；把模型给的 confidence 当信任模型；把厂商 benchmark 当目标。

### 10.6 需要您拍板的**唯一一个问题**（报告 §7）

> **reArtex 的持久记忆，是 agent 的属性（planner 知道什么），还是 task-hall 的属性（这个群学到了什么）？**

- 报告的建议：**task-hall**（per-agent 会得到 N 份发散副本 + 写冲突；task-hall 范围 + agent 归属写入才有单一真相与审计痕迹）。
- 随之而来的两个立即动作：
  1. 记忆表一律 `scope_type`/`scope_id` **NOT NULL**，API 对未 scope 的读写用 typed error 拒绝；
  2. **会话树就是证据层，也是续跑基座** —— 与第四节的 `sessions` 实体是同一件事。
- **进入方案 C 的闸门**：reArtex 将来是否需要回答「**X 日那天为真的是什么**？」
  若否 ⇒ **方案 B 已经够用**（且 `supersedes_id`/`valid_from`/`valid_to` 从第一天就在 ⇒ C 随时可达，无需迁移）。

⇒ **建议**：**先做方案 B**，从第一天就带上 `supersedes_id`/`valid_from`/`valid_to`；
「X 日为真」这个问题**现在不需要回答** ⇒ 不进方案 C。

### 10.7 记忆层与第四节会话改造的合并关系

| 记忆层要素 | 对应第四节的会话概念 | 说明 |
|---|---|---|
| `agent_session(parent_session_id, task_hall_id, agent_role)` | `sessions` + `session_members` | 同一件事的两种叫法 ⇒ **合并建** |
| `agent_message(parent_message_id)` | `session_messages`（群消息 + 收据条） | episode 层的载体 |
| `scope_type/scope_id` | `sessions.kind`（chat / task_hall） | scope 就是 session ⇒ **不需要另立 scope 体系** |
| `source_session_ids[]` | `session_attachments` | 记忆指向来源会话 |
| dream 任务 | 会话关闭时触发 | 「只在会话关闭时新鲜」是方案 A 的短板，方案 B 用 dream 补 |

⇒ **结论：记忆层不构成独立的第五期，而是第四期（sessions 实体化）的延伸** ⇒
建议把二者合并为「会话 + 记忆」一个大期，否则会出现两套 session 概念。

---

## 十一、MiniMax 官方工程笔记的吸收结论（2026-11-03）

> 来源：MiniMax 官方技术博客的公开镜像（`agent.minimax.io/docs/techblog`，同一系列英文版；
> 飞书那篇需登录且 cookie 已失效）。原文全 6 篇分 4 个单元并行精读，并把每条机制
> 对到 reArtex 的真实代码上。**本节每一条 reArtex 侧的断言都由我亲自复核过**（不只依赖单元结论）。

### 11.1 「五层记忆」的真相（跨产品的口径收敛）

五篇笔记与业界实现（Claude Code / Letta MemFS / MiniMax MCode / Codex）反复收敛到**同一套四层上下文分类**：

| 层 | 载体 | 生命周期 | 谁负责 |
|---|---|---|---|
| **① 工作上下文** | 当前对话 + 工具结果 + 推理 | 单次请求 | compaction / context editing 只动这层 |
| **② 会话产物** | 进度文件、todo、设计文档、研究笔记、git commit | 跟随任务，做完可归档 | **交接的主载体** |
| **③ 项目记忆** | `AGENTS.md` / `CLAUDE.md`（构建命令、代码约定、已知坑） | 跨会话，每次会话开始加载、压缩后重注入 | 建议 < 200 行，**同一个坑犯两次才加** |
| **④ 个人/全局记忆** | 偏好与习惯（跨项目） | 跨项目 | Claude Code 的 auto memory：`MEMORY.md` 只是索引，只加载前 200 行或 25KB，详情在主题文件里按需读 |

三条被反复强调的规则（**可作为 reArtex 记忆层的验收口径**）：
1. **记忆必须是索引**，不要把内容全塞进上下文；索引常驻、详情按需读。
2. **「热规则」要有独立预算**：有一次记忆文件超预算被截断，**恰好把写在最上面的重要规则截掉了**，模型和人都没发现 ⇒ 教训是「截断必须响亮」。
3. **记忆分层不重叠**：一次会话里达成的共识未必是项目级的；项目级共识未必适用于所有项目。**进度、todo、临时事件属于「会话产物」，不属于长期记忆**。
4. 记忆本身是摘要，**会错** ⇒ 必须可查看、可编辑、可禁用；「CLAUDE.md 是上下文，不是强制」——必须强制的规则属于 hook。

### 11.2 已复核的三条吸收项（定稿）

#### 吸收项 1（P0，S）：`{{.Now}}` 是平台自己递的刀，必须改掉

**事实（已复核）**：
- `{{.Now}}` 被作为**可插入变量暴露给用户**：`server/server_mgmt.go:2125-2127`，描述「服务端当前时间（每次运行实时刷新；可与固定起始时间相减判断已用时长）」，示例 `2026-08-11 14:30:00 CST`。
- 它渲染进 **system prompt 的开头**：`agent/prompt.go:25-30` `nowStr()` = `time.Now().Format("2006-01-02 15:04:05 MST")`（**秒级精度**），经 `PlannerVars/WorkerVars/MainVars/GoalsVars`（`agent/prompt.go:18-21`）注入。
- **主 agent 每轮重拼 system**：`agent/mainagent.go:103-104` `mainAgentSystem(...)` 带 `Now: nowStr()`；`:134` `deferredSystem(mainAgentSystem(...))`；`:188-191` 注释明写「each Chat is a fresh session」⇒ **每一轮 system 都带一个变化的秒级时间戳**。
- chat agent 同理（`agent/chat.go:93,170`），并且 system 里还拼了 `groupDigest`（群邮箱摘要，会变）。
- SDK 自己的 system-reminder 反而是**日期粒度**（`third_party/norma/agentcore/agentcore.go:400-407`，`time.Now().Format("2006-01-02")`），且是作为**第一条消息**插入（`harness/query.go:784-786`）⇒ 一天变一次，代价可忽略。

**问题**：笔记的核心警告是「**最前面 + 每轮都变的内容是最贵的**」。`{{.Now}}` 恰好允许用户把这种内容放在最前面；一旦用户模板引用了它，**主 agent / chat agent 每轮都废掉整个前缀缓存**（含工具定义与全部历史）⇒ 而这两个角色恰恰是长对话、历史最长的角色。

**改法（P0，S）**：
1. **把 `{{.Now}}` 从 system 变量降级为「每轮 user 消息里的时间」**：在 system prompt 里不再提供 `Now`；改为在组装本轮 user 输入时追加一行 `当前时间：…`（与笔记建议一致：时间随 user 消息走，不进 system）。
2. **保留「固定起始时间」的能力**：笔记建议用「固定起始 + 当前时间相减」算已用时长 ⇒ 那就把**起始时间**放进 system（每轮稳定），把**当前时间**放进 user turn。
3. **给模板校验加一条硬规则**：DB 里的 system 模板若引用 `{{.Now}}`，**直接拒绝保存**并给出原因（否则用户会亲手造出每轮废缓存的 system）。
4. **把秒级降到分钟级**（笔记 PR #459 的做法）：分钟精度足够判断「最近 N 小时」，且同一分钟内稳定。

#### 吸收项 2（P0，S）：transcript 写失败是**静默**的

**事实（已复核）**：`third_party/norma/transcript/transcript.go` 的 `Writer` 有 `Err()`，但**全仓库没有任何一处读它**（`grep -rn "\.Err()"` 命中的全是 `ctx.Err()`，与此无关）⇒ **transcript 落盘失败会被吞掉**，用户侧表现为「会话记录少了一段」，且没有任何告警。

**改法**：在 transcript 写入路径上把 `Err()` 向上冒泡到 agent 终止原因（`agent/terminalreason.go` 已有 `TerminalReason` 机制可复用）⇒ 失败必须变成**可见的终止原因**，而不是悄悄少一段历史。

#### 吸收项 3（P1，M）：补上「上下文预算」这一层（reArtex 现在只有「压缩阈值」没有「输入上限」）

**事实（已复核）**：reArtex 的压缩**已经是 token 驱动的**（不是按轮数）：`third_party/norma/compaction/compaction.go` 有 `reservedForSummary`（= `min(MaxOutputTokens, 20000)`）、`effectiveWindow`、分级 `buffer`（50000 / 30000 / 13000）、`autoThreshold`、`microThreshold`（= `effectiveWindow*6/10`）、`maxTurnGrowth`（= `reserved + 15000`）、`predictiveOver`；token 估计**优先用上一次响应的 `input_tokens`**，无则退化为 `chars/4*4/3`。

**缺什么（笔记有、reArtex 没有）**：
1. **没有「输入上限 L」与「压缩阈值 T」两个分离的概念** ⇒ 笔记的
   `L = max(1, min(floor(0.95×C), C − R, C − O_eff − S))`、
   `T = min(L, max(1, C − min(2R, floor(C/4))))`（默认 `R=16384`、`S=2048`）
   这套「输入准入 vs 提前压缩」的分层，reArtex 是混在一起的 ⇒ 缺少一个**准入闸门**：
   请求（system + 工具定义 + 消息）组装完后，若超过 L 就应当**拒绝准入**并报错，
   而不是等它撞上 provider 的 4xx 再回头压。
2. **没有任何字节计量**（token 估计是 `chars/4`，noa 那侧是字符数）⇒ 笔记的
   「字节上限」与「序列化后大小不得大于压缩前」这两条准入检查没有对应物。
3. **没有 usage anchor**：reArtex 只记「上一次响应的 input_tokens」，**没有绑定**
   provider / API / model / system+工具指纹 / 历史纪元 / 锚定消息 id ⇒ 换模型或换
   提示词后，旧估计会被继续沿用而失真。
4. **压缩的产物归档已经有很好的实现**：`third_party/norma/noa`（`RenderArchive` /
   `ArchiveFileName` / `ArchiveTierDir`）+ 准入检查（`checkSizeGate`、`validateSummary`、
   `MinCompressRange=5000`、`MaxCompressAttempts=3`、`PreserveRecentTokens=5000`、
   `Truncate.Threshold=0.95`）+ 溢出学习（`noaadapter/overflow.go` 的 `ParseOverflowWindow`，
   `armedFloor()` = 实测窗口×0.95）⇒ **这一层不必重做**，只需补「准入闸门 + 字节计量 + anchor 绑定」。
5. **度量基座已经存在**：`db/commands.go:287-288`、`db/conversation.go:16-17` 已落
   `cache_read_tokens` / `cache_write_tokens` ⇒ 「每轮缓存命中率」现在就能算 ⇒
   **吸收项 1 的效果可以当场度量**，不必等改造。

### 11.3 与现有设计的冲突点（明说，不打圆场）

| # | 冲突 | 处置 |
|---|---|---|
| 1 | **planner 已经做对了**：态势块放**本轮 user 输入**、system 只留静态规划正文（`agent/planner.go:323` 及「move-out 让 system 每轮稳定、更利于缓存」的注释）⇒ 笔记的建议在这里**已经落地** | 保持；把 planner 的做法**推广**到 main/chat agent（与吸收项 1 同源） |
| 2 | **worker 反着做，而且是刻意的**：`agent/worker.go:428-430` 明写「system 每次 run 都重新拼一遍…混入 per-intent 易变数据、失去跨意图缓存复用；**这是刻意的取舍（意图丢失比省 token 严重得多）**」 | **不改**。worker 一个 run = 一个意图，跨意图缓存复用本来就不存在；而「意图信息不能被压缩掉」是更硬的约束。笔记的批评在这里不适用 ⇒ 不引入冲突 |
| 3 | 笔记主张「记忆必须**可查看、可编辑、可禁用**」；而 reArtex 的安全边界原则是「关键约束进**代码常量**，不进可热改存储」 | 二者**不冲突**：记忆是**提示**，不是边界。记忆可编辑；**边界不可编辑**。这一条要写进 §8 红线，避免后续有人把记忆做成安全机制 |
| 4 | 笔记建议把「压缩失败」显式暴露（`POST_ADMISSION_FAILED`）；reArtex 的失败语义是 fail-closed | 一致 ⇒ 直接沿用 reArtex 的终止原因机制表达 |
| 5 | 笔记的「每轮 user 消息里带时间」意味着**每轮消息内容都变** ⇒ 若不缓存前缀，历史每轮都要重算 | 需与吸收项 1 一并落地：**分钟精度**（同一分钟内不变）+ 只在 user 侧 ⇒ 命中率高 |

### 11.4 本节新增的定稿项（并入 §9.1）

| # | 议题 | **定稿** | 理由 |
|---|---|---|---|
| 13 | `{{.Now}}` 暴露在 system 变量里 | **降级为每轮 user 消息里的时间（分钟精度）**；DB system 模板引用 `{{.Now}}` **拒绝保存**；「固定起始时间」留在 system | 笔记的核心警告 + 主/chat agent 每轮重拼 system ⇒ 每轮废前缀缓存 |
| 14 | transcript 写失败被静默吞掉 | **冒泡为可见的终止原因** | 失败必须可见；会话记录缺段是最难排查的一类问题 |
| 15 | 上下文只有压缩阈值、没有输入准入上限 | **补「准入闸门 L」与「压缩阈值 T」分层 + 字节计量 + usage anchor 绑定**（归档层复用已有 noa，不重做） | 笔记的公式可直接落地；准入与压缩分层后，「撞 provider 4xx 再回头压」变成「组装完即判定」 |
| 16 | 记忆可编辑 vs 边界不可编辑 | **写入 §8 红线**：记忆是提示、不是安全机制 | 两套原则各自成立，但要写清边界，否则会被混用 |

---

### 11.5 Notes 03（压缩 vs 记忆）+ Notes 04（Bash 前台 → 后台任务）

#### 11.5.1 关键机制（逐字常量）

- **03 的核心规则**：压缩**只改模型看到的**，不改用户的历史；压缩与上下文编辑**只作用于第 ① 层**（工作上下文）。
  记忆必须是**索引**；「热规则要有自己的预算」；「**截断必须响亮**」（有一次记忆文件超预算，**恰好把写在最上面的重要规则截掉了**，模型和人都没发现）；「**给记忆划范围**」（进度/todo/临时事件属于会话产物，不属于长期记忆）；「**记忆也是摘要，它会错** ⇒ 必须可查看、可编辑、可禁用」；「`CLAUDE.md` 是上下文，不是强制」——**必须强制的规则属于 hook**。
- **04 的核心规则**：前台最多等 **60 秒**（远低于 5 分钟缓存寿命）；后台任务总预算 **3600 秒**、**前台时间计入总预算且不重置**；**task table 是真相来源**（聊天记录不是）；完成时注入 `<background-task-completion-reminder>`；进程重启后未完成的任务标 **`lost`**；**反轮询**（工具描述里写明「会自动通知你，不要反复轮询」）；通知上限 **32 条 / 8KB**、速率限制 **3 次/60 秒**；重试退避 **1s/2s/5s → 60s（最多 5 分钟）**；watchdog **1 小时**；前台输出预算 **24KB**；完成后把经验写进 memory。

#### 11.5.2 reArtex 映射（**含三处必须纠正的事实**）

| # | 事实（已复核） | 坐标 |
|---|---|---|
| ⚠️**纠正 1** | **`agent/compaction.go` 不是对话压缩**，而是**探索图的冷摘要**（cold-node → digest，调用 `AddDigest` / `SupersedeDigests`，**会改写持久化的知识图谱**）。真正的对话压缩在 **`third_party/norma/compaction`**（vendored SDK，**在树内 ⇒ 可改可评审**） | `agent/compaction.go`；`third_party/norma/compaction/compaction.go` |
| ⚠️**纠正 2** | 方案书 §10.3 写「`third_party/norma/memory` … ARTEX 完全没有 import 它」**不准确**：`norma/agentcore` import 了它并接进每个 session，**但 ARTEX 从不配置 `Options.Memory`** ⇒ 路径是「死的」而非「不存在」 | `third_party/norma/agentcore/agentcore.go` |
| ⚠️**纠正 3** | `norma/memory` 是**错误的捐赠者**：无索引上限、无 scope、无 supersession、截断不响亮 ⇒ **违反文章规则与我们自己 §10.5-2** ⇒ **方案 B（DB 版）是对的** | `third_party/norma/memory/memory.go` |
| ✅ 已有 | 对话压缩已是**非破坏 + 只改模型视图**：`llm.MessagesForAPI` 只送边界之后的消息，完整历史仍在消息数组里；MicroCompact 清工具结果**只动 result、保留 tool_use/result 配对**；压缩后有 preamble | `third_party/norma/compaction/compaction.go` |
| ✅ 已有 | **热规则已有自己的预算**：`agent_prompts` → `SystemPrompt` 位于消息数组**之外** ⇒ 压缩永远截不到它 ⇒ 天然满足文章「热规则独立预算」 | `agent/worker.go`、`agent/mainagent.go` |
| ✅ 已有 | Bash 后台任务**在 reArtex 其实是活的**：SDK 有 `run_in_background`、`RunForeground` 超时自动转后台、`taskoutput`/`taskstop`/`tasklist`、通知以 UserText 注入**会话尾部**、`AGENT_CORE_DISABLE_BACKGROUND_TASKS` 全局开关、`DisableBackgroundTasks` per-session 开关（reArtex **只在 goals agent 上开了**）⇒ worker/planner/mainagent/chat 都能用后台任务 | `third_party/norma/tool/bash.go`、`third_party/norma/tool/task.go`、`third_party/norma/agentcore/agentcore.go`、`agent/goals.go` |
| ⚠️ 缺口 | **最贵的潜在 bug**：压缩把边界前消息移出 API 视图 ⇒ **落在那里的后台任务通知会消失**。文章的解法（task table 当真相来源 + 完成提醒）正是解药 | — |
| ⚠️ 缺口 | **真实成本 bug（非理论）**：模型可要 `timeout_ms: 600000`（10 分钟）> Anthropic 5 分钟缓存 TTL ⇒ 前台阻塞超过缓存寿命 ⇒ 缓存重算 | `third_party/norma/tool/bash.go` |
| ⚠️ 缺口 | 文章的 3600s 后台预算在 reArtex **达不到**：`agents.run_seconds` 默认 **1200**，且 `Session.Close()` 会杀掉后台任务 | `db/schema.sql`、`third_party/norma/agentcore/agentcore.go` |
| ⚠️ 缺口 | **默认压缩触发点太晚**：reArtex 在窗口 ~93% 才触发，**没有「更早压缩」的旋钮** ⇒ 文章的「1M 窗口下别等自动压缩，要提前压」是 P0 不是锦上添花 | `third_party/norma/compaction/compaction.go` |

#### 11.5.3 与现有设计的冲突（明说）

1. **方案 B 的「dream 任务重写索引」与文章冲突**：文章说「**记忆也是摘要**，用更好的摘要去修一个摘要往往不работает」⇒ ⇒ **索引必须 append-only / 版本化；dream 只提 diff，不整体重写索引**；且索引要导出成纯文本（可查看/可编辑/可禁用）。
2. **方案 B 会造出第二个常驻层**：`agent_prompts` 已经是安全的「热规则层」⇒ 必须写死不变式：**规则住在 `agent_prompts`，索引只放检索指针**。
3. **文章四层里的第 ④ 层（个人/跨项目记忆）在渗透系统应整层丢弃**：「能从代码推断出来的就不要记录」，跨项目偏好没有持久价值。
4. **命名陷阱（波及面最大）**：一个仓库里两个相反意义的 "compaction" ⇒ **把 `agent/compaction.go` 改名为 `agent/cold_digest.go`** 并标注「知识图谱压缩」，避免后来者继承错误的保证。
5. 文章的 Verifier 有交付裁决权、Agent 与人类同权（可 prompt/abort/kill 别的 agent）⇒ **都不能照抄**（见 §11.6 冲突 2、3）。

### 11.6 MiniMax Agent Team：把「对抗终止触发」从工具层搬到**产出层**

#### 11.6.1 关键机制（逐字引用）

- 「**Leader 负责把用户目标转化为任务结构**。**Worker 负责执行具体子任务**。**Verifier 负责把『完成了』变成『可以交付』**。」
- 「**WorkerAgent 与 VerifierAgent 是对抗关系。双方都以运行结束为目标，但一方运行结束会引发另一方的运行开始。**」
- 「Team Engine 会按照 producing、verifying、done 去管理每一个任务。**当 verifying 未通过时，Team Engine 会重新唤起 producing 节点继续修改。**」
- 「用户发消息时，预期是秒级反馈；但很多任务天然需要分钟级甚至小时级执行」⇒ 长任务与对话上下文分离；「**session 不等于模型 context window，长任务需要可恢复的 session log 作为外部上下文对象**」。
- 「本次执行的经验也可以沉淀为记忆、Skill」。

#### 11.6.2 reArtex 映射：零件全在，**只缺闭环那一段**

| 零件 | 已有（坐标） |
|---|---|
| 对抗者 redteam | `db/db.go:357` |
| `needs_review` 对抗复核队列 | `db/schema_sod.sql` |
| 确定性裁决纪律「模型只提议，确定性层裁决」 | `internal/sod/types.go` |
| `rel='derived_from'` 表达返工 | `db/schema.sql`（探索边约束） |
| worker→planner 的**边沿唤醒**（"一方的结束引发另一方"） | `server/engine.go`（`runRound("edge")`） |
| **缺的** | **verifier→worker 这一条腿**：finding 落库即 `confirmed`，**没有产出级的验证态与返工边** |

⇒ 文章的核心洞察「**双方都以运行结束为目标，但一方运行结束会引发另一方的运行开始**」在 reArtex 里**已经以 worker→planner 的边沿唤醒存在**，只是没有 verifier→worker 这一条腿。

#### 11.6.3 最高价值的一条（定稿）

**把「对抗终止触发」从工具层搬到产出层（finding 级），并把「不得自证」从注释变成确定性代码。**

理由：① 渗透平台最贵的失败模式不是漏了攻击面，而是**报告里一条不可复现的漏洞**；② reArtex 全部零件已就位，缺的只是闭环；③ 文章的「结束引发开始」在 reArtex 已有对应实现；④ 平台核心风险是「自主运行久了不可信」，这条改动直接买信任。

⚠️ 现状里的一个真问题：`db/sod.go` 的复核结论落库**没有校验「结案人 ≠ 提出人」** ⇒ **提出者可以给自己盖章** ⇒ 而 `internal/sodpolicy` 的注释里明写着「needs_review 交回给提出者复核等于自证」⇒ **规则只停在注释层** ⇒ 这是全链最便宜、价值最高的一处加固。

#### 11.6.4 优先级（定稿）

> **P0-④ 已完成（a、b 两件）+（c）出设计单独排期**（提交 `7174838`）
>
> **(a) 前台命令上限 60s（原 120s）—— 已实现**
> 一条卡住的前台命令会把 agent 整整阻塞一个超时时长：等待期间它不能规划、不能把
> 已识别的东西落库、不能对已有发现做反应。⇒ 默认 120s → 60s。
> **命令不会因此丢失**：SDK 本来就会把超时的前台命令移到后台（agent 稍后用
> TaskOutput/Read 取输出），缩短超时只是让控制权更早回到 agent 手里。
> 关键细节：**工具描述与提示词里的数字必须跟着动态生成** —— 原来 props 里写死
> "default 120000"、提示词里写死 "default 120s"，改了行为不改描述 = 对模型说谎
> （模型会按 120s 规划自己的等待）。现在两者都从解析后的实际值生成。
> `AGENT_CORE_TOOL_TIMEOUT_SEC` 可覆盖，钳制 [5s, 600s]。干净实例显式写 60。
>
> **(b) 总预算不重置 —— 复核结论：ARTex 已经做到了，不是缺口**
> 任务级预算是**绝对时间**（`TaskClock.DeadlineUnix`，首次运行盖章）；
> `clampMaxDuration(taskDeadline, ownBudget)` 把每一轮夹逼到**剩余**时间 ⇒
> N 轮总时长 ≤ 任务总预算。
> 但这条不变量此前**没有任何测试保护** ⇒ 补 `agent/taskclock_budget_test.go`：
> 5 组取值 + 1 条直接钉「同一绝对 deadline 下，后续轮拿到的预算**不得变大**」。
>
> **(c) 后台任务跨 run 持久 —— 本次不做，出设计（见下方 P0-④b）**
> SDK 自己的注释把话说清楚了：`tool/task.go` 的 `Cleanup()` 上方写着
> 「Those pointers are consumed inside the session that produced them. Making
> them genuinely durable needs spill output to live in a host-chosen directory
> with its own retention policy, not in os.TempDir() keyed by a per-session
> UUID; **that is a design change, not a change to this function**.」
> ⇒ 后台任务的生命周期绑在 session（= 一次 run）上：run 结束 ⇒ `Manager.Cleanup()`
> ⇒ `KillAll()`。要跨 run 存活需要：输出落在**宿主指定目录** + **自己的保留策略**
> + **跨 session 注册表** + **回收者**。
> 这**不是打补丁能解决的**。硬塞进本次提交只会得到一个没有回收者的进程泄漏 ——
> 那比现状更糟。⇒ 单独排期，设计见下方 §11.7 P0-④b。
>
> **顺带修掉**：SDK 自带测试 `TestWebFetchProxyFallback` 断言的是**旧的**直连兜底
> 行为 ⇒ 契约改了就留个红的测试是雷。已改为**显式 opt-in**
> （`t.Setenv("NORMA_WEBFETCH_DIRECT_FALLBACK","1")`），并补对偶测试
> `TestWebFetchProxyFallbackOffByDefault`：默认配置下代理失败**必须失败**，
> 不得静默直连。⇒ `patches/norma-egress-scope.patch` 扩到 2 个文件，SDK 自带测试
> 套件现全绿。

> **P0-③ 已完成**（提交 `2c2f9f9`）：AutoCompact 触发点可提前。新增
> `Config.AutoCompactRatio`（vendored SDK 侧以 `patches/norma-compaction-trigger.patch`
> 落地），触发点变为 `min(默认阈值, effectiveWindow * ratio)`。
> **只能提前、不能推后** —— 实现里只在 `t < base` 时才采用 ⇒ 放松默认策略必须改
> 代码，不可能靠改环境变量悄悄生效。默认 ratio=0 ⇒ 行为完全不变。
> ARTex 侧从 `ARTEX_COMPACT_TRIGGER_RATIO` 读取（0~1，非法值忽略并打日志），
> 不加 DB 列（`llm_profiles` 在 RESERVED 的 `db/schema.sql` 里，且阈值是运维旋钮
> 而非每档案数据）。干净实例默认 0.75。
> 测试：`TestCompactionTriggerRatioEnv`（9 组取值）+ `TestCompactionTriggerOnlyMovesEarlier`
> （直接对生成出来的 vendored 源码断言两处守卫在 —— 补丁会被悄悄改写或丢失，这条
> 不变量必须由测试盯着）。

> **实施前置已就位**：`patches/` 多 patch 机制 + `scripts/prepare-norma.sh` 按序应用
> （提交 `2735758`）⇒ 下面 ③④⑤ 与 §9.1 第 8 条**全部作用于 vendored SDK**，
> 现在都可以直接按「写补丁 → 提交 → 重新生成依赖树」的套路做，不需要改脚本或手改生成目录。



| 优先级 | 事项 | 改动位置 | 工期 | 不采纳的风险 |
|---|---|---|---|---|
| **P0** ✅**已完成** | 强制「**结案人 ≠ 提出人**」 | `db/sod.go` 的复核结论 UPDATE 加 `AND NOT (source_agent <> '' AND $3 <> '' AND source_agent = $3)` + 新增可区分错误 `ErrSoDReviewSelfCertification` + agent 侧单独说清（提交 `890bd88`） | **S**（已做完） | 全链的「对抗」只停在提示词层，提出者能给自己盖章 —— 正是整条 SoD 链要防的事 |
| **P0** | **finding 级验证态 + 返工边** | `db/schema.sql` 的 node state 三态 + 探索边 `derived_from` | **M** | planner 能用 worker 自产 fact 去 `prove_goal` **自证**；长任务累积未验证结论 |
| **P0** | **提前压缩触发**（不等到窗口 93%） | `third_party/norma/compaction`（在树内） | **S** | 晚期的压缩会把最重要的内容压没；文章称之为「context rot」 |
| **P0** | **前台 60s / 总 3600s 不可重置 + 后台任务脱离 `run_seconds`** | `third_party/norma/tool/bash.go` + `task.go` + `db/schema.sql` 的 `run_seconds` 语义 | **S–M** | 前台阻塞超过 5 分钟缓存 TTL ⇒ 缓存重算（真实成本）；且后台任务必被 `run_seconds` 砍掉 |
| **P0** | **task table 当真相来源 + 压缩后完成提醒 + `lost` 标记** | `third_party/norma/tool/task.go` + 压缩后的提醒注入 | **M** | **最贵的潜在 bug**：压缩把落在边界前的后台任务通知移出 API 视图 ⇒ 模型永远不知道任务已完成 |
| **P1** | verifier 接进探索环（保持「只提议」，FAIL ⇒ 生成新 intent） | `server/engine.go` 的 `claimIntent` + `agent/planner.go` 的意图生成 | **L** | 上面两条没有执行者 |
| **P1** | 产出可复现性前置（finding 落库时必填证据引用） | `findings` 的 `replay_evidence_id` 列**已存在** | **S–M** | 漏洞证据链与留痕脱钩，报告不可复核 |
| **P1** | **任务级进度/回执契约**（`kind='progress'`；阻塞走 `intercept_pending`） | `server/engine.go` | **M** | 长任务在 UI 上像挂了 —— 文章称此为**头号用户反馈** |
| **P1** | 压缩后告诉模型「上下文被自动管理，请把进度写文件」 | `third_party/norma/compaction` 的 preamble | **S** | 模型不知道该把进度外化 ⇒ 重要信息随压缩丢失 |
| **P2** | Team **值得性闸**（放在硬底线**之前**判定，不拆底线） | `agent/planner.go` 的硬底线判定 | **S** | 简单任务也走全 Team，ROI 崩 |
| **P2** | 打通两张图（`exploration_nodes.strategy_id` 真 FK） | `db/schema.sql` | **M** | 「哪条策略奏效」永远查不到；curator 的 utility 视图看不到执行结果 |
| **P2** | 改名 `agent/compaction.go` → `agent/cold_digest.go` | 纯改名 + 注释 | **S** | 波及面最大的命名陷阱 |
| ❌ | **per-agent 经验记忆** | **不采纳** | — | 与 §9 第 6 条（记忆归 task-hall）冲突；且 `strategies` 已是 per-company 的「Skill」，作用域更大 |

#### 11.6.5 与现有设计的冲突（明说）

1. **文章的「每个 Agent 各自越变越好」（per-agent 成长）与已定稿方案冲突** ⇒ **不抄**。
2. **文章的 Verifier 有交付裁决权，与我们的铁律冲突** ⇒ Leader/Worker/Verifier 三角**必须**重写成「提议 → 确定性裁决」，不能让 LLM 的 FAIL 直接驱动状态机。
3. **文章的「Agent 与人类同权」（agent 可 prompt/abort/kill 别的 agent）不能抄** ⇒ reArtex 的等价面只有 planner 的 `steer_work`/`kill_work` + 工具层「只能 deny、不能 abort」⇒ **abort/kill 保持人类 + 编排器专属**。
4. **文章「不该鼓励凡事开 Team」与我们的硬底线相反**（planner 规定「目标未达成且无 open/running 意图 ⇒ 本轮**必须**产出至少一个意图」）⇒ 这条底线是防探索停摆的承重墙，**不能按文章删**；正确吸收方式是**加「值得性」闸**。
5. **文章的「并行信息检索」整节在 reArtex 几乎无落点**（web_search 明确留在出网治理未覆盖清单里；渗透场景的并行探索 reArtex 已有）⇒ **低优先，别硬造**。
6. **文章的共享留言板（文件白板）弱于我们的 DB 版**（`session_messages` + `receipt_of` 收据条，可审计、可回放）⇒ **保留 DB 版**。

### 11.7 本次调研纠正了方案书的两处事实错误

| 位置 | 原写法 | 纠正 |
|---|---|---|
| §10.3 | 「`third_party/norma/memory` 存在…但 **ARTEX 完全没有 import 它**」 | `norma/agentcore` **import 了它并接进每个 session**，只是 **ARTEX 从不配置 `Options.Memory`** ⇒ 路径是「死的」不是「不存在」。⇒ 既有好用的捐赠者，但它是**错误的**捐赠者（无索引上限/无 scope/无 supersession/截断不响亮）⇒ **方案 B（DB 版）仍然正确，但工作量估算要按「自建」算，不能按「接线」算** |
| §10.3 关系矩阵 | 「`strategy_evidence` …**无级联外键**」 | `strategy_id` **有** `REFERENCES strategies(id) ON DELETE CASCADE`；**无 FK 的是 `ref_id`**（裸 TEXT）⇒ 结论（findings↔strategies 是 PARTIAL、任务归档会带走证据链）**成立**，但表述要改准，否则实施时会误删 FK |

### 11.8 记忆层方案 B 的修正（据 03 的规则收紧）

| # | 修正 | 理由 |
|---|---|---|
| 1 | **索引 append-only / 版本化；dream 只提 diff，不整体重写索引** | 文章：「记忆也是摘要，用更好的摘要去修一个摘要往往不работает」⇒ 重写索引 = 摘要的摘要 |
| 2 | **索引导出为纯文本**（可查看/可编辑/可禁用） | 文章：记忆必须可查看、可编辑、可禁用 |
| 3 | **写死不变式：规则住在 `agent_prompts`，索引只放检索指针** | 否则方案 B 会造出第二个常驻层，而 `agent_prompts` 已经是安全的热规则层 |
| 4 | **索引硬上限 + 截断必须响亮**（超限时**拒绝写入**并提示，绝不静默丢弃） | 文章的「热规则被截掉、模型和人都没发现」事故；也是我们 §10.5-2 的原话 |
| 5 | **第 ④ 层（个人/跨项目记忆）整层丢弃** | 渗透系统没有跨项目偏好要持久化；「能从代码推断出来的就不要记录」 |

---

## 附：调研与勘察的原始出处

- 代码勘察：会话/对话实现（`chat/page.tsx` 分组逻辑、`server/conversations.go` 派发链、
  `db/schema*.sql` 全部 DDL、`agent/chat.go` 群邮箱）、任务大厅前后端、egress 治理三处。
- 外部调研：Grok Bot 文档与 xAI 发布文、OpenAI dots 发布文与帮助中心、DotCollab 文档、
  Douchat 文档与仓库、Poe 帮助中心与博客、Character.AI 博客、Slack 帮助与开发者文档、
  Codex CLI 源码（resume picker / app-server 协议 / state sqlite schema / 线程附件接口）、
  Claude Code 官方文档（sessions / commands / agent-teams / agent-view / cross-session-messaging）、
  Cursor 文档与发布说明、VS Code chat sessions 文档、Cline 文档、Windsurf 文档、
  Devin 发布说明与 API、Linear Agent 文档、Notion AI 文档、Hermes Agent Bot Mode。
- ⚠️ 少数二手来源已在正文标注（Hermes 的具体轮次上限、DotCollab 的 UI 细节未获一手证实）。

### 11.7 P0-④b：后台任务跨 run 持久（设计，本次未实施）

**问题**：后台命令（`run_in_background`，或前台超时被自动转后台）的生命周期绑在
**session** 上。session = 一次 run ⇒ run 结束（agent 收尾、意图完成、或 run 预算
到点）⇒ `Manager.Cleanup()` ⇒ `KillAll()` ⇒ 后台进程被杀，agent 承诺「稍后我会去
读它的输出」的承诺落空。

**为什么不能用补丁糊**：SDK 的 `Manager` 把任务目录放在 `os.TempDir()/norma/<sessionUUID>/tasks`，
清理时整目录删除。要让任务活过 session，输出必须落在**宿主指定**的目录、由**宿主**
决定保留多久。这需要改变所有权模型，不是改一个函数。

**设计（六件，缺一不可）**：

| # | 件 | 落点 |
|---|---|---|
| 1 | **宿主目录**：后台命令输出写到 `ARTEX_TASK_DIR`（默认宿主 `~/.artex/tasks/<taskID>/`），不写 `os.TempDir()` | 部署配置 + SDK 侧目录来源可注入 |
| 2 | **跨 session 注册表**：进程级（package-level）注册表，按 ARTex 任务 ID 索引后台进程 ⇒ 换 session 仍能找到 | SDK `tool` 包新增 registry |
| 3 | **真相来源 = ARTex 任务表**：后台命令在 ARTex 任务表里**占一行**（`kind='background_cmd'`），状态 running/done/lost ⇒ 不再由 SDK 的内存状态说了算 | 新表或复用 tasks 加 kind 列（**注意 `db/schema.sql` 是 RESERVED** ⇒ 走独立 schema 文件 + 幂等 ALTER） |
| 4 | **回收者（关键）**：任务超时收尾（`settleTask`）时统一 kill 该 ARTex 任务下的后台进程；再加进程级兜底：无主任务且超过 TTL（默认 1h）的后台进程被回收并标记 lost | `server/engine_timeout.go` 的 settle 序列 + 进程级 janitor |
| 5 | **完成提醒**：后台命令结束时向该 ARTex 任务的 mailbox 投递一条完成通知 ⇒ agent 下一轮读得到（对应 §11 的「完成提醒」） | 复用现有 mailbox/通知机制 |
| 6 | **lost 标记**：被回收的后台命令在任务表里标 `lost`（不是 `failed`）⇒ agent 知道「它被回收了」而不是「它失败了」 | 与 #3 同表 |

**顺序**：#1+#2 是前置（没有它们，#3–#6 无处落地）；#3 与 #4 必须同时上线
（只有记账没有回收 = 进程泄漏）；#5、#6 可以在其后补。

**不做会怎样**：现状是「agent 说稍后读输出，但进程已经死了」⇒ agent 下一轮读到
的是一个死进程的残缺输出，或者读不到而困惑。**这是一个真实的静默失效**，只是
不如出网绕行那么致命。

**风险**：进程泄漏（靠 #4 兜住）、目录膨胀（靠 TTL + 保留策略兜住）、跨 session
并发写同一输出文件（按 command 分文件，不共享句柄）。

### 11.8 P2：探索图 ↔ 策略图的节点级连接（**schema 已落地**，写入路径待接）

**已实现**（提交 `9856838`）：`exploration_nodes.strategy_id` 加**真外键**。

- **为什么需要节点级**：`strategy_usage` 只到 exploration/intent 层 ⇒ 问不出
  「这条 finding 是哪条策略产出的」。一个 exploration 里常同时跑多条策略 ⇒
  exploration 级不够。
- **为什么真外键**：payload 里存文本 id 没有引用完整性（策略删了节点还留悬空
  字符串，反查靠文本扫描还可能命中同名字符串）。
- **ON DELETE SET NULL（不是 CASCADE）**：策略被删只丢链接、不删节点 —— 节点是
  事实与结论，策略是可替换的工作方法；CASCADE 会把结论一起抹掉，那是数据丢失。
- **落在 `db/schema_strategy.sql`**：`db/schema.sql` 是 RESERVED 文件 ⇒ 列与约束
  放策略库自己的文件（该文件本来就跨单元给 `exploration_nodes`/`findings` 建索引）。
- **历史行不回填**：节点 payload 里没有意图键 ⇒ 没有可靠的归属依据；硬把一个
  exploration 的所有节点归给其中某条策略 = **安静的错误归属** ⇒ 历史行保持 NULL。

**验证**（一次性 Postgres，8 项全过）：ALTER 加列成功且**连跑两遍无错**（幂等）；
写不存在的 strategy_id **被外键拦下**；删策略 ⇒ **节点保留、链接置空**；索引/列/约束
均存在。

**待接**：写入路径 —— 每轮开始时把「当前生效的策略」记到 `ExplorationStore`，节点
落库时随行写入 `strategy_id`。这需要定位「一轮里策略何时被选中」的准确位置（与
`db/strategy_proposal.go` 的提案/晋级流程对齐），是下一步的接线点。


# 设计：新会话的 `/` 面板 —— 点名成员 + 引用任意记录

日期：2026-11-04
状态：待审阅（Sir 已定方向：保留旧 @ 的 list 交互、触发键换成 `/`、面板同时具备「点名成员」与「全局记录搜索」）

## 1. 背景与问题

旧对话的 `@` 与新会话的 `@` **是两个不同的功能，恰好用了同一个触发字符**：

| | 旧对话 | 新会话 |
|---|---|---|
| 组件 | `web/src/components/mention-textarea.tsx` + `web/src/lib/chat-mentions.ts` | `web/src/components/session/session-composer.tsx` 里手写的 `detectMention`/`targets`/`filtered`/`applyMention` |
| list 内容 | **服务端全局搜索**：漏洞 / 资产 / 企业 / 接口 / IP / 应用 / 域名 / 子域名 / 服务（`mentionKinds`） | 只有**本会话成员 + 候选 agent** |
| 选中后插入 | 结构化 token `@[漏洞#12 标题]`（`mentionToken`） | 纯文本 `@agentkey` |
| 交互 | Popover + ↑↓ / Enter / Tab / Esc + 分组 + 分页 + 「↑↓ 选择 · Enter 确认 · Esc 关闭」 | 简化版，无服务端搜索 |

⇒ 用户实际感受是「新会话没有 @ 的那个功能」：**旧 @ 的价值主要在「能搜到并引用任意记录」**，新会话那套把它砍成了「只能选本会话的人」。

后端另有约定（**不可改**）：`internal/sessionwake` 从**正文**里解析 `@agentkey`（外加 `to_agent`、`@everyone`）来决定「定向必醒」⇒ 点名 agent 必须落成**正文里的 `@agentkey`**。

## 2. 目标 / 非目标

**目标**
1. 新会话的输入框：输入 `/` ⇒ 弹出与旧 @ **同款**的 list（同样的键盘、样式、分组、过滤）⇒ 在 list 里选。
2. 该 list 同时能：① 点名**本会话成员 / 候选 agent**（插入正文可见的 `@agentkey`，喂给唤醒逻辑）；② 搜索并引用**任意记录**（插入结构化 token `@[kind#id label]`，喂给引用解析）。
3. 旧对话行为**零变化**（`@` 仍是原语义）。

**非目标**
- 不改后端唤醒逻辑（`internal/sessionwake` 一行不动）。
- 不做 opencode 式的「命令注册表 / 命令可带 agent 作用域」那套（本项目当前没有命令体系，只有 mention）。
- 不让 `/` 出现在正文中间就触发（路径、命令里的斜杠不能误触发）。

## 3. 设计

### 3.1 触发与解析（`web/src/lib/chat-mentions.ts`）

- `activeMention(value, caret, trigger = "@")`：触发字符参数化。
  - `/` 的触发判定比 `@` 更严：**光标前的 `/` 必须是行首或前面是空白**（`@` 允许紧贴单词，如 `邮箱@x`）。
  - 查询串里若含 `[]\r\n` 或触发字符本身 ⇒ 判定结束（与现有 `@` 规则同款）。
- 新增 mention kind `agent`（`{ kind: "agent", label: "成员", alias: "agent" }`）⇒ 面板能按「成员」分组。
- `mentionToken(item)` **分流**：
  - `kind === "agent"` ⇒ 返回**纯文本** `@${agentKey} `（后端唤醒靠它；agent 的 key 就是 agent_key，天然是稳定标识）。
  - 其它 kind ⇒ 保持现有结构化 token `@[${kind}#${id} ${label}]` 不变。
- `selectedMentions(value)` 保持不变（只解析结构化 token；agent 是纯文本，不进这个列表）。

### 3.2 组件（`web/src/components/mention-textarea.tsx`）

新增 props（全部可选 ⇒ 旧调用点零改动）：

| prop | 作用 |
|---|---|
| `trigger?: string` | 触发字符，默认 `"@"` |
| `extraItems?: ChatMention[]` | 本地项（本会话成员 / 候选 agent），排在服务端搜索结果**之前**，按 kind 分组 |
| `onPickAgent?: (agentKey: string) => void` | 可选：选中 agent 时的额外副作用（如把 agent 加进会话成员）；不传则只插入文本 |

- 列表构成：`extraItems`（本地、即时）＋ 服务端搜索结果（记录）。
- 交互与旧组件完全一致：Popover、`↑↓` 移动、`Enter`/`Tab` 确认、`Esc` 关闭、过滤高亮、底部提示「↑↓ 选择 · Enter 确认 · Esc 关闭」、滚动分页加载。

### 3.3 新会话接线（`web/src/components/session/session-composer.tsx`）

- **改用 `MentionTextarea`（`trigger="/"`）**，删除自实现的 `detectMention`/`MentionTarget`/`targets`/`filtered`/`applyMention`/`mention` state 与对应 popup。
- `extraItems` 由会话成员与候选 agent 组装（沿用现有 `memberDisplayName` / `presenceMeta` 提示）：
  - 本会话成员：`kind: "agent"`，`id` 用一个稳定的合成 id（仅用于列表 key 与分组，不参与后端解析），`label` = 展示名，`description` = presence 文案（闲 / 在听 / 在干 / 卡住 / 已退出）。
  - 候选 agent（未入席）：`description` = 「加入并呼叫」。
- 记录搜索仍走旧组件的服务端搜索通道 ⇒ **新会话也能引用漏洞 / 资产 / 企业 / 接口 / IP / 应用 / 域名 / 子域名 / 服务**。

### 3.4 token 形态分流（关键）

| 选中的项 | 插入正文 | 谁消费 |
|---|---|---|
| 会话成员 / 候选 agent | `@agentkey `（纯文本） | `internal/sessionwake`：定向必醒 |
| 各类记录 | `@[漏洞#12 标题]` | `selectedMentions` / 记录引用链路 |

⇒ **两个形态互不干扰**：后端唤醒只认裸 `@agentkey`（`@[漏洞#12 ...]` 里没有裸 @key ⇒ 不会被误判为点名）；记录引用只认结构化 token（裸 `@agentkey` 不匹配其正则 ⇒ 不会污染引用列表）。

### 3.5 面板内容与分组

顺序（自上而下）：
1. **成员**（本会话在席 + 可加入的候选 agent）
2. 服务端搜索命中的**记录**（按 `mentionKinds` 原有分组与标签）

## 4. 数据流

```
用户输入 "/"（行首或空白后）
  → activeMention(value, caret, "/") 命中
  → MentionTextarea 打开 Popover
      · 本地项：extraItems（即时）
      · 记录项：mentionSearch(query) → 服务端 → 分页追加
  → 用户 ↑↓ 选中 / Enter 确认
  → mentionToken(item)
      · agent  → "@pentest "
      · 记录   → "@[漏洞#12 xx]"
  → 写入输入框，光标停在插入之后
  → 发送：正文里同时可能含 @agentkey 与 @[...] ⇒ 后端两条解析路径各管各的
```

**后端改动量：0**。

## 5. 错误处理

- 服务端搜索失败：list **仍显示本地项**（成员），并按旧组件同款显示错误行 + 「点击重试」。
- 搜索超时/无结果：与旧组件一致（空态文案）。
- `extraItems` 为空（会话还没人/没有候选）：面板只显示记录搜索结果；记录搜索也失败 ⇒ 显示空态 + 错误行。
- 选中 agent 但该 agent 已退席：插入文本照旧（后端按 presence 决定是否唤醒，不在 UI 层拦）。

## 6. 测试

1. `activeMention` 触发判定：`/` 只在行首或空白后命中；`curl /usr/bin` 不命中；`a/b` 不命中；`@` 默认行为不变。
2. token 分流：agent ⇒ `@key `；记录 ⇒ `@[kind#id label]`。
3. 新会话 composer：输入 `/` 出现面板；面板首组是成员；选中成员后输入框含 `@agentkey`。
4. 旧对话回归：`@` 打开的面板内容与选中行为**不变**。
5. 端到端：带 `@agentkey` 的消息发出后，`internal/sessionwake` 仍判定为定向必醒（用现有 `internal/sessionwake` 单测覆盖，不改它的代码）。

## 7. 涉及文件

| 文件 | 改动 |
|---|---|
| `web/src/lib/chat-mentions.ts` | `activeMention` 加 trigger 参数；新增 `agent` kind；`mentionToken` 分流 |
| `web/src/components/mention-textarea.tsx` | 新增 `trigger` / `extraItems` / `onPickAgent` 三个可选 prop；本地项与服务端项合并分组 |
| `web/src/components/session/session-composer.tsx` | 改用 `MentionTextarea`（`trigger="/"`），删除自实现的 mention 逻辑 |
| `web/src/lib/chat-mentions.test.ts`（新增或既有） | 触发判定与 token 分流用例 |
| 后端 | **无改动** |

## 8. 验收口径

- 新会话输入框按 `/` 弹出的 list，**与旧 @ 的 list 视觉与键盘交互一致**（截图对照）。
- list 里既能选到**会话成员**，也能搜到并引用**漏洞/资产/…**（至少各 1 条）。
- 选中成员后，正文里出现 `@agentkey`；该消息发出后**定向必醒**仍生效。
- 旧对话的 `@` 行为零变化（回归用例通过）。
- 前端门禁：`biome check` 零 error、`tsc --noEmit` 通过、`npm run build:static` 成功。

## 9. 风险与边界

- `/` 的触发比 `@` 严（行首/空白后）⇒ 习惯打「`/` + 空格」的人要改成「`/` + 名字」；这是刻意的（避免路径与命令误触）。
- agent 用**裸 key**、记录用**结构化 token** ⇒ 正文里出现裸 `@xxx` 时后端会当作点名 ⇒ 成员 key 需与普通单词区分（现有 agent key 形如 `pentest`/`reporter`，可接受）。
- 本设计**不**引入 opencode 的命令注册表：当前项目没有命令体系，硬加会扩大范围。若将来要 `/new`、`/task` 这类命令，可在同一面板上再加一个 kind（届时再单独设计）。

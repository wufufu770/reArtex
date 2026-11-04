# 新会话 `/` 面板 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 新会话输入框按 `/` 弹出与旧 `@` 同款的 list，既能点名会话成员（插入裸 `@agentkey`），也能搜索并引用 9 类记录（插入 `@[kind#id label]`）；后端零改动，旧对话 `@` 行为零变化。

**Architecture:** 复用既有 `web/src/components/mention-textarea.tsx`（它已实现 Popover list、↑↓/Enter/Tab/Esc、分组、分页、错误重试）。给它加三个**可选** prop（`trigger` / `extraItems` / `onPickAgent`），并把纯逻辑（触发判定、token 生成）参数化到 `web/src/lib/chat-mentions.ts`。新会话的 `session-composer.tsx` 改用这个组件并删除自己手写的 mention 实现 ⇒ 只剩一套实现。

**Tech Stack:** Next.js 16 App Router · React 19 · TypeScript 5.9 · Tailwind 4 · shadcn/ui · biome（lint/format）· ts-node（纯逻辑测试，不新增依赖）

## Global Constraints

- 规格：`docs/superpowers/specs/2026-11-04-session-slash-mention-design.md`（已入库 `b6a868e`）
- **后端零改动**：不得修改 `internal/sessionwake/**`、`server/**`、`db/**`、`agent/**`。
- **不新增依赖**：测试用仓库已有的 `ts-node`（`web/package.json` devDependencies 已有 `ts-node@^10.9.2`）。
- 旧对话 `@` 行为**零变化**（`MentionTextarea` 的 `trigger` 默认值必须是 `"@"`）。
- `/` 只在**行首或空白后**触发；`@` 保持原判定（允许紧贴单词）。
- agent 类 token = 裸 `@agentkey `；记录类 token = `@[${kind}#${id} ${label}]`（不变）。
- 前端门禁：`npx biome check` 零 error、`npx tsc --noEmit` 通过、`npm run build:static` 成功。
- 禁止 `as any` / `@ts-ignore` / 任何类型抑制。
- 不提交、不推送：由编排方统一提交。

---

## 文件结构

| 文件 | 职责 | 本次改动 |
|---|---|---|
| `web/src/lib/chat-mentions.ts` | 纯逻辑：mention kind 定义、触发判定、搜索路由、token 生成、token 反解 | 参数化 trigger；新增 `agent` kind；token 按 kind 分流 |
| `web/src/components/mention-textarea.tsx` | 通用 mention 输入框（Popover list） | 新增 3 个可选 prop；本地项与服务端结果合并分组 |
| `web/src/components/session/session-composer.tsx` | 新会话输入框 | 改用 `MentionTextarea`（`trigger="/"`）；删除手写 mention 逻辑 |
| `web/scripts/test-chat-mentions.ts` | 纯逻辑测试（ts-node + node:assert） | 新建 |
| `web/package.json` | 脚本 | 新增 `test:mentions` |
| `.github/workflows/ci.yml` | 门禁 | 新增一步跑 `npm run test:mentions` |

---

### Task 1: 纯逻辑 —— 触发字符参数化 + agent kind + token 分流

**Files:**
- Modify: `web/src/lib/chat-mentions.ts`
- Test: `web/scripts/test-chat-mentions.ts`（新建）

**Interfaces:**
- Consumes: 无（纯逻辑）
- Produces:
  - `mentionKinds` 增加 `{ kind: "agent", label: "成员", alias: "agent" }`（放在数组**首位**，面板里「成员」组排在最前）
  - `export function activeMention(value: string, caret: number, trigger: string = "@"): { start: number; end: number; query: string } | null`
  - `export function mentionToken(item: ChatMention): string` —— `item.kind === "agent"` 时返回 `@${item.id} `（**裸 key + 尾随空格**），否则保持原样
  - `ChatMention` 增加可选字段 `agentKey?: string`（agent 类的稳定 key；`id` 仍需存在，故 agent 的 `id` 用 `agentKey` 的稳定数值或 `-1`）

- [ ] **Step 1: 写失败测试** `web/scripts/test-chat-mentions.ts`：

```ts
import assert from "node:assert/strict";
import { activeMention, mentionToken } from "../src/lib/chat-mentions";

// 触发判定：/ 只在行首或空白后
assert.equal(activeMention("/pen", 4, "/")?.query, "pen");
assert.equal(activeMention("hi /pen", 7, "/")?.query, "pen");
assert.equal(activeMention("curl /usr/bin", 12, "/"), null, "路径不能触发");
assert.equal(activeMention("a/b", 3, "/"), null, "单词中间的斜杠不能触发");
assert.equal(activeMention("/", 1, "/")?.query, "");
// 默认仍是 @，且允许紧贴单词（邮箱@x 这类）
assert.equal(activeMention("邮箱@x", 4)?.query, "x");
assert.equal(activeMention("x @pen", 6)?.query, "pen");

// token 分流
assert.equal(mentionToken({ kind: "agent", id: -1, label: "渗透", description: "", agentKey: "pentest" }), "@pentest ");
assert.equal(
  mentionToken({ kind: "finding", id: 12, label: "SQL 注入", description: "" }),
  "@[漏洞#12 SQL 注入]",
);

console.log("chat-mentions 逻辑测试通过");
```

- [ ] **Step 2: 跑测试确认失败**

Run: `cd web && npx ts-node --compiler-options '{"module":"commonjs","target":"es2022"}' scripts/test-chat-mentions.ts`
Expected: FAIL —— `activeMention` 忽略第三个参数（`/` 分支不存在）、`mentionKinds` 无 `agent`、`mentionToken` 不分流。

- [ ] **Step 3: 实现**

`web/src/lib/chat-mentions.ts` 改动：

1. `mentionKinds` 首位插入：
```ts
  { kind: "agent", label: "成员", alias: "agent" },
```
2. `ChatMention` 增加 `agentKey?: string`。
3. `activeMention` 改为：
```ts
export function activeMention(value: string, caret: number, trigger = "@") {
  const before = value.slice(0, caret);
  const start = before.lastIndexOf(trigger);
  if (start < 0) return null;
  // 触发字符前面的字符：@ 允许紧贴单词（邮箱@x）；其余触发符要求行首或前面是空白，
  // 否则路径 / 命令里的斜杠会误触。
  if (start > 0 && trigger !== "@" && !/\s/.test(before[start - 1])) return null;
  if (start > 0 && trigger === "@" && /[\w.+/-]/.test(before[start - 1])) return null;
  const query = before.slice(start + trigger.length);
  if (/[[\]
]/.test(query) || query.includes(trigger) || query.length > 220) return null;
  return { start, end: caret, query };
}
```
4. `mentionToken` 改为：
```ts
export function mentionToken(item: ChatMention) {
  // agent 类：**裸 key**（后端 internal/sessionwake 从正文认 @agentkey 来决定「定向必醒」）
  if (item.kind === "agent") return `@${item.agentKey ?? item.label} `;
  const kind = mentionKinds.find((entry) => entry.kind === item.kind)?.label ?? "资产";
  const label = item.label
    .replace(/[[\]]/g, (char) => (char === "[" ? "（" : "）"))
    .replace(/\s+/g, " ")
    .slice(0, 100);
  return `@[${kind}#${item.id} ${label}]`;
}
```
5. `mentionSearch` 的 kind 路由：`agent` **不参与服务端搜索**（成员是本地的）⇒ 在它的 kind 循环里跳过 `agent`：
```ts
  for (const item of mentionKinds) {
    if (item.kind === "agent") continue;
    ...
  }
```

- [ ] **Step 4: 跑测试确认通过**

Run: `cd web && npx ts-node --compiler-options '{"module":"commonjs","target":"es2022"}' scripts/test-chat-mentions.ts`
Expected: PASS（打印「chat-mentions 逻辑测试通过」）

- [ ] **Step 5: 提交**

```bash
git add web/src/lib/chat-mentions.ts web/scripts/test-chat-mentions.ts
git commit -m "feat(web): mention 触发字符参数化 + agent 类 token 分流"
```

---

### Task 2: MentionTextarea 增加 trigger / extraItems / onPickAgent

**Files:**
- Modify: `web/src/components/mention-textarea.tsx`
- Test: `web/scripts/test-chat-mentions.ts`（同文件追加纯逻辑断言；组件行为靠类型检查与人工验收）

**Interfaces:**
- Consumes: Task 1 的 `activeMention(value, caret, trigger)`、`mentionToken(item)`、`ChatMention.agentKey`
- Produces:
```ts
type Props = Omit<React.ComponentProps<"textarea">, "value" | "onChange" | "ref"> & {
  value: string;
  onValueChange: (value: string) => void;
  inputGroup?: boolean;
  /** 触发字符，默认 "@"。新会话传 "/"。 */
  trigger?: string;
  /** 本地项（会话成员等），排在服务端搜索结果之前，按 kind 分组。 */
  extraItems?: ChatMention[];
  /** 选中 agent 时的额外副作用（可选）。 */
  onPickAgent?: (agentKey: string) => void;
};
```

- [ ] **Step 1: 在测试脚本追加断言**（`web/scripts/test-chat-mentions.ts` 末尾）：
```ts
// agent 类不参与服务端搜索路由（成员是本地的）
assert.equal(mentionKinds[0].kind, "agent", "成员组应排在最前");
```

- [ ] **Step 2: 跑测试确认通过**（Task 1 已让它通过，此步是回归护栏）

Run: `cd web && npx ts-node --compiler-options '{"module":"commonjs","target":"es2022"}' scripts/test-chat-mentions.ts`
Expected: PASS

- [ ] **Step 3: 实现**

`web/src/components/mention-textarea.tsx` 改动：

1. `Props` 增加上面三个字段。
2. 函数签名解构增加 `trigger = "@"`、`extraItems = []`、`onPickAgent`。
3. **触发检测**：把文件里调用 `activeMention(...)` 的地方改为传 `trigger`（该文件里 `activeMention` 的调用点只有一处，见 `const active = activeMention(value, caret)`；若变量名不同，以实际为准）。
4. **本地项合并**：现有 list 数据来自服务端搜索结果（`items`）。改为
```ts
  const localItems = React.useMemo(
    () => extraItems.filter((item) => !query || item.label.toLowerCase().includes(query.toLowerCase())),
    [extraItems, query],
  );
```
   并把渲染时的列表来源改为「本地项在前 + 服务端项在后」，分组仍按 `mentionKinds` 的既有顺序（成员组在最前）。**服务端搜索请求照旧发起**（记录搜索能力保留）。
5. **选中分流**：现有选中处理会调用 `mentionToken(items[index])` 并替换文本。改为：若选中项 `kind === "agent"` ⇒ 调 `onPickAgent?.(item.agentKey ?? item.label)`（若提供了），并且**仍然**用 `mentionToken(item)` 写入文本（裸 `@key `）。其余类型行为不变。
6. 过滤：本地项按 `label` 过滤（上面 `localItems` 已做）；服务端项的过滤保持原逻辑。

- [ ] **Step 4: 类型检查 + lint**

Run: `cd web && npx tsc --noEmit && npx biome check src/components/mention-textarea.tsx src/lib/chat-mentions.ts`
Expected: 均无输出（干净）

- [ ] **Step 5: 提交**

```bash
git add web/src/components/mention-textarea.tsx web/scripts/test-chat-mentions.ts
git commit -m "feat(web): MentionTextarea 支持自定义触发字符与本地项"
```

---

### Task 3: 新会话 composer 改用 MentionTextarea（trigger="/"）

**Files:**
- Modify: `web/src/components/session/session-composer.tsx`
- Test: `web/scripts/test-chat-mentions.ts`（不加新断言；组件行为靠验收）

**Interfaces:**
- Consumes: Task 2 的 `MentionTextarea` 新 props；既有 `sessionMembers`/`memberDisplayName`/`presenceMeta`（`web/src/components/session/session-format.ts`）
- Produces: 无新导出

- [ ] **Step 1: 组装本地项**

在 `session-composer.tsx` 里：
```ts
  const extraItems = React.useMemo<ChatMention[]>(() => {
    const seated = new Set(members.map((m) => m.agent_key));
    const list: ChatMention[] = [];
    for (const member of members) {
      list.push({
        kind: "agent",
        id: -1,
        label: memberDisplayName(member, agentByKey),
        description: presenceMeta(member.presence).label,
        agentKey: member.agent_key,
      });
    }
    for (const agent of candidates) {
      if (seated.has(agent.key)) continue;
      list.push({
        kind: "agent",
        id: -1,
        label: agent.name,
        description: "加入并呼叫",
        agentKey: agent.key,
      });
    }
    return list;
  }, [members, candidates, agentByKey]);
```
（`ChatMention` 从 `@/lib/chat-mentions` 导入；`memberDisplayName`/`presenceMeta` 从 `./session-format` 导入 —— **以文件里现有的导入为准**，不要重复导入。）

- [ ] **Step 2: 换掉输入控件**

把当前 `<Textarea ... />` 换成：
```tsx
          <MentionTextarea
            className="max-h-40 min-h-10 min-w-0 flex-1 resize-none"
            rows={1}
            placeholder="输入消息，/ 选择成员或引用记录，Enter 发送"
            value={value}
            disabled={disabled}
            onValueChange={onChange}
            onKeyDown={onKeyDown}
            trigger="/"
            extraItems={extraItems}
          />
```
（`onChange`/`onKeyDown`/`value`/`disabled` 的实际名字以该文件现有写法为准。）

- [ ] **Step 3: 删除手写的 mention 实现**

移除该文件里的：`MentionTarget` 接口、`detectMention` 函数、`targets`/`filtered` 的 useMemo、`applyMention` 函数、`mention` state、与之关联的 popup 渲染块与 `AtSignIcon` 之类仅为该 popup 服务的 import。**保留** `memberDisplayName`/`presenceMeta` 的使用（现在用于 `extraItems`）。

- [ ] **Step 4: 类型检查 + lint + 构建**

Run: `cd web && npx tsc --noEmit && npx biome check src/components/session/session-composer.tsx && npm run build:static`
Expected: 均通过（build 产出 `web/out/`）

- [ ] **Step 5: 提交**

```bash
git add web/src/components/session/session-composer.tsx
git commit -m "feat(web): 新会话改用 MentionTextarea（/ 触发），删掉手写的 mention 实现"
```

---

### Task 4: 测试脚本接入 CI + 整体验收

**Files:**
- Modify: `web/package.json`（加 `test:mentions` 脚本）
- Modify: `.github/workflows/ci.yml`（前端 job 加一步）
- Modify: `web/scripts/test-chat-mentions.ts`（如有需要）

**Interfaces:**
- Consumes: Task 1 的测试脚本
- Produces: `npm run test:mentions`

- [ ] **Step 1: package.json 加脚本**

```json
    "test:mentions": "ts-node --compiler-options '{"module":"commonjs","target":"es2022"}' scripts/test-chat-mentions.ts",
```

- [ ] **Step 2: CI 加一步**

在 `.github/workflows/ci.yml` 的前端 job 里，`npm ci` 之后、`npm run build` 之前插入：
```yaml
      - name: 纯逻辑测试（mention 触发与 token 分流）
        run: npm run test:mentions
```

- [ ] **Step 3: 本地全量验证**

Run:
```bash
cd web && npm run test:mentions && npx tsc --noEmit && npx biome check src/ && npm run build:static
```
Expected: 全部通过

- [ ] **Step 4: 同步产物并重建镜像、重启实例**

```bash
cd .. && rsync -a --delete web/out/ server/webui/dist/
ARTEX_SKIP_NPM_CI=1 ARTEX_SKIP_FRONTEND=1 ./build-image.sh artex-dev:local
docker compose -p artexdev2 -f docker-compose.yml -f docker-compose.override.yml -f docker-compose.fresh.yml up -d
```
Expected: `http://192.168.41.152:19787/api/health` 与 `:8787` 返回 `ok:true`，版本 `0.4.6`

- [ ] **Step 5: 人工验收清单（交付时逐条确认）**

1. 新会话输入框按 `/` ⇒ 弹出 list，**首组是「成员」**（含 presence 文案）
2. ↑↓ 选中成员 + Enter ⇒ 输入框出现 `@<agentkey> `，光标在其后
3. 面板里继续输入关键字 ⇒ 能搜到**漏洞/资产/…**（记录组），选中后插入 `@[漏洞#12 …]`
4. 发出带 `@agentkey` 的消息 ⇒ 目标成员被唤醒（定向必醒生效）
5. 旧对话输入 `@` ⇒ 行为与改动前完全一致（仍搜记录、仍插结构化 token）
6. 消息正文里的 `curl /usr/bin`、`a/b` ⇒ **不**弹出面板

- [ ] **Step 6: 提交**

```bash
git add web/package.json .github/workflows/ci.yml
git commit -m "ci(web): 纯逻辑测试接入 CI（mention 触发与 token 分流）"
```

---

## 自审记录

- **规格覆盖**：§3.1→Task 1；§3.2→Task 2；§3.3/§3.4→Task 3；§5→Task 2/3（本地项 + 服务端项共存、错误行沿用旧组件）；§6→Task 1/4（触发判定、token 分流、旧行为回归、旧对话回归靠人工验收项 5）；§7→各 Task 的文件表；§8→Task 4 Step 5；§9→Global Constraints（行首触发、裸 key、命令注册表不做）。
- **占位符扫描**：无 TBD/TODO；每个代码步骤都给出可粘贴的代码。
- **类型一致性**：`activeMention(value, caret, trigger?)`、`mentionToken(item)`、`ChatMention.agentKey`、`Props.trigger/extraItems/onPickAgent` 在 Task 1/2/3 之间命名一致。
- **已知取舍**：前端无测试运行器 ⇒ 用已有 `ts-node` 跑纯逻辑（不新增依赖）；组件交互靠类型检查 + 构建 + 人工验收清单。

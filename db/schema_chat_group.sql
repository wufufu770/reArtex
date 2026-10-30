-- 群聊（任务大厅）数据模型 —— **与 db/schema.sql 并行应用**的幂等 DDL。
--
-- 为什么单独成文件：db/schema.sql 由别的单元在同一 worktree 内并行开发，本单元
-- 不改它。本文件由 db/chat_group.go 的 EnsureChatGroupSchema 在 db.Open 之后
-- 补齐（见该文件注释），schema 属主可以随时把内容并进 db/schema.sql —— 两边
-- 都是幂等的，重复应用无副作用。
--
-- 四张表：
--   chat_groups               群组本体：一个群 = 一个 task（大厅是任务的"大厅"）
--   chat_group_members        群成员：每个成员一个独立会话（独立 transcript），共享群的 task
--   chat_group_disabled_tools 群内工具**禁列表**（只收紧；表里没有"启用"列 ⇒ 结构上无法放宽）
--   chat_group_messages       群内消息箱（mailbox）：成员之间的协商层

-- ---------- chat_groups ----------
-- 一个群组绑定**恰好一个** task：群里的所有成员会话共享这张 task 的探索图，
-- 群组的用量也记在这张 task 的额度账上（大厅是任务的"大厅"，不是一个独立实体）。
CREATE TABLE IF NOT EXISTS chat_groups (
    id                BIGSERIAL PRIMARY KEY,
    -- 大厅会话：人在这里布置任务、看全局、插话；agent_key='auto' 是分发者。
    conversation_id   BIGINT NOT NULL UNIQUE REFERENCES conversations(id) ON DELETE CASCADE,
    task_id           BIGINT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    name              TEXT NOT NULL DEFAULT '',
    -- 状态机由「额度/排空」单元实现（draining/parked/resuming）；这里只把取值
    -- 形状先定下来，免得将来为了加状态而做破坏性迁移。
    status            TEXT NOT NULL DEFAULT 'active'
                      CHECK (status IN ('draft','active','draining','parked','resuming','closed','failed')),
    concurrency_limit INTEGER NOT NULL DEFAULT 3 CHECK (concurrency_limit > 0),
    -- 套餐额度由额度单元拥有；quota_plans 还没落地时先留普通列（下面条件性补外键）。
    plan_id           BIGINT,
    budget_tokens     BIGINT CHECK (budget_tokens IS NULL OR budget_tokens > 0),
    drained_at        TIMESTAMPTZ,
    parked_at         TIMESTAMPTZ,
    resumed_at        TIMESTAMPTZ,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_chat_groups_task ON chat_groups(task_id);
CREATE INDEX IF NOT EXISTS idx_chat_groups_status ON chat_groups(status);
DROP TRIGGER IF EXISTS trg_chat_groups_upd ON chat_groups;
CREATE TRIGGER trg_chat_groups_upd BEFORE UPDATE ON chat_groups
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- 一个 task 至多一个大厅：大厅会话与 task 的绑定必须唯一，否则"按 task 反查群组"
-- 会有歧义（工具面要靠它把 message_group 落到唯一的群上）。
DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'uq_chat_groups_task') THEN
    ALTER TABLE chat_groups ADD CONSTRAINT uq_chat_groups_task UNIQUE (task_id);
  END IF;
END $$;

-- 额度表落地后再把 plan_id 绑成外键（表还不存在时保持普通列，不制造依赖）。
DO $$
BEGIN
  IF to_regclass('quota_plans') IS NOT NULL
     AND NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'fk_chat_groups_plan') THEN
    ALTER TABLE chat_groups ADD CONSTRAINT fk_chat_groups_plan
      FOREIGN KEY (plan_id) REFERENCES quota_plans(id) ON DELETE SET NULL;
  END IF;
END $$;

-- ---------- chat_group_members ----------
-- 群成员：每个成员 agent 一个**独立会话**（独立 transcript，互不污染），但共享
-- 群组的那一个 task（同一张探索图 ⇒ 能互相看见对方的 intent/fact/finding）。
-- role 只表达大厅内的分工（dispatcher=分发者 / member=成员 / observer=旁观），
-- **不新增权限体系**：成员的工具集基线仍是 tools 表里的全局绑定。
CREATE TABLE IF NOT EXISTS chat_group_members (
    group_id   BIGINT NOT NULL REFERENCES chat_groups(id) ON DELETE CASCADE,
    agent_key  TEXT   NOT NULL REFERENCES agents(key) ON DELETE CASCADE,
    conversation_id BIGINT REFERENCES conversations(id) ON DELETE SET NULL,
    role       TEXT   NOT NULL DEFAULT 'member'
               CHECK (role IN ('dispatcher','member','observer')),
    joined_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (group_id, agent_key)
);
CREATE INDEX IF NOT EXISTS idx_cgm_members_conv ON chat_group_members(conversation_id)
    WHERE conversation_id IS NOT NULL;
-- 一个会话至多属于一个群（否则"按会话反查群"有歧义）。NULL 不参与唯一性 ——
-- 没有会话的成员（历史数据）可以有多个。
DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'uq_cgm_members_conversation') THEN
    ALTER TABLE chat_group_members ADD CONSTRAINT uq_cgm_members_conversation
      UNIQUE (conversation_id);
  END IF;
END $$;

-- ---------- chat_group_disabled_tools ----------
-- 群内工具**禁列表**。
--
-- 为什么是"禁"而不是"启"：reArtex 的加固模型要求安全边界是【代码常量】，不进 DB
-- 规则行（规则行可被 DELETE、settings 可被热改）。一张带 enabled 列的表等于给了
-- "DB 表可放宽权限"这条攻击面。此表只有 tool_key，没有任何列能写下"放开"⇒
-- **结构上无法表达放宽**，只能让权限更严。
CREATE TABLE IF NOT EXISTS chat_group_disabled_tools (
    group_id  BIGINT NOT NULL REFERENCES chat_groups(id) ON DELETE CASCADE,
    tool_key  TEXT   NOT NULL,
    PRIMARY KEY (group_id, tool_key)
);

-- ---------- chat_group_messages ----------
-- 群内消息箱（mailbox）：成员之间的**协商层**，不是授权层。
--
-- 三条 mailbox 授权语义（2026-10-29-auto-permission-tiers.md §3）在本表上的落法：
--  1) 携带 intent_id 的消息只是【提议】：表里**没有** owner/assignee 一类的列，
--     意图归属的唯一权威始终是 exploration_nodes.owner（ClaimIntent 的 CAS 写入）。
--  2) claims_authorization=true 表示"发送方声称本消息携带某个已批准"；它**永远**
--     是不可信输入 —— 没有任何权限门读这一列，它只用于留痕与在接收侧标注。
--  3) status='blocked' 的消息在【投递前】就被拦下（写库时判定），不进入接收方上下文。
CREATE TABLE IF NOT EXISTS chat_group_messages (
    id         BIGSERIAL PRIMARY KEY,
    group_id   BIGINT NOT NULL REFERENCES chat_groups(id) ON DELETE CASCADE,
    from_agent TEXT NOT NULL,
    to_agent   TEXT NOT NULL DEFAULT '',   -- '' = 广播给全群
    kind       TEXT NOT NULL DEFAULT 'note'
               CHECK (kind IN ('note','request','answer','finding','escalation')),
    body       TEXT NOT NULL,
    task_id    BIGINT REFERENCES tasks(id) ON DELETE SET NULL,
    intent_id  BIGINT REFERENCES exploration_nodes(id) ON DELETE SET NULL,
    finding_id BIGINT REFERENCES findings(id) ON DELETE SET NULL,
    status     TEXT NOT NULL DEFAULT 'unread'
               CHECK (status IN ('unread','read','acked','blocked')),
    blocked_reason TEXT NOT NULL DEFAULT '',
    claims_authorization BOOLEAN NOT NULL DEFAULT false,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    read_at    TIMESTAMPTZ
);
-- 收取查询是 (to_agent='' OR to_agent=<me>) AND status='unread'：单条
-- (group_id,status) 索引会退化成扫全组消息再过滤 to_agent，而 OR 让
-- (group_id,to_agent,status) 的定向分支用不上 ⇒ 拆成两条**部分索引**，
-- 访问层也拆成两次查询（见 db/chat_group.go 的 UnreadChatGroupMessages）。
CREATE INDEX IF NOT EXISTS idx_cgm_group_unread ON chat_group_messages(group_id, status);
CREATE INDEX IF NOT EXISTS idx_cgm_broadcast_unread ON chat_group_messages(group_id, created_at)
    WHERE to_agent = '' AND status = 'unread';
CREATE INDEX IF NOT EXISTS idx_cgm_direct_unread ON chat_group_messages(group_id, to_agent, created_at)
    WHERE status = 'unread';

-- 会话（sessions）数据模型 —— **与 db/schema.sql 并行应用**的幂等 DDL。
--
-- 为什么单独成文件：db/schema.sql 是 RESERVED 文件（别的单元在同一 worktree 内并行
-- 开发），本单元不改它。本文件由 db/session.go 的 EnsureSessionSchema 幂等补齐，
-- schema 属主可以随时把内容并进 db/schema.sql —— 两边都是幂等的，重复应用无副作用。
--
-- 依据：session-groupchat-proposal.md §4.1（数据模型）、§4.7（迁移与兼容）。
--
-- 为什么要有这张表：conversations.agent_key 是**单值列** ⇒ 一个会话只能跑一个
-- agent ⇒ 「一个 session 就是一群聊」在数据层无处安放。sessions 把 agent 从
-- 「会话的属性」升格为「会话成员的属性」（proposal §零 / §1.1）。
--
-- 四张表：
--   sessions              会话本体：一个 session = 一群聊（容器）
--   session_members       会话成员：agent 在会话里的席位（可热加入 / 退出）
--   session_attachments   会话↔工件：typed + 双向（正向加、反查）
--   session_messages      会话消息：人 / agent 同一条流 + 收据条 + 线程层

-- ---------- sessions ----------
CREATE TABLE IF NOT EXISTS sessions (
    id            BIGSERIAL PRIMARY KEY,
    -- name 是【用户显式名】：一旦设置永不被生成标题覆盖（proposal §4.2）。
    name          TEXT NOT NULL DEFAULT '',
    -- title 是【生成标题】：模型摘要 / 首行兜底，可搜但不是句柄。
    title         TEXT NOT NULL DEFAULT '',
    -- name_source 记的是「当前显示名取自哪一层」：user > generated > default。
    -- ⚠️ proposal §4.1 的 DDL 写的是 DEFAULT 'auto'，而它自己的 CHECK 只认
    -- ('user','generated','default') ⇒ 那一版默认插入会撞 CHECK，整张表建出来
    -- 就写不进行。此处取语义正确的默认值 'default'（还没有名字时的状态）。
    name_source   TEXT NOT NULL DEFAULT 'default'
                  CHECK (name_source IN ('user','generated','default')),
    kind          TEXT NOT NULL DEFAULT 'chat'
                  CHECK (kind IN ('chat','master','task_hall','side_question')),
    -- task_id 可空：纯对话型 session 不必挂任务（proposal §1.2 硬伤①的解法）。
    task_id       BIGINT REFERENCES tasks(id) ON DELETE SET NULL,
    -- strategy_id 的外键在下面用条件式 ALTER 补（strategies 在另一个 DDL 文件里，
    -- 见 db/schema_strategy.sql）—— 做法与 chat_groups.plan_id → quota_plans 同款。
    strategy_id   BIGINT,
    asset_id      BIGINT REFERENCES assets(id) ON DELETE SET NULL,
    pinned_at     TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_sessions_updated ON sessions(updated_at DESC);
CREATE INDEX IF NOT EXISTS idx_sessions_task    ON sessions(task_id) WHERE task_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_sessions_kind    ON sessions(kind, updated_at DESC);
DROP TRIGGER IF EXISTS trg_sessions_upd ON sessions;
CREATE TRIGGER trg_sessions_upd BEFORE UPDATE ON sessions
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- strategies 表落地后再把 strategy_id 绑成外键（表还不存在时保持普通列，
-- 不制造依赖）—— 与 db/schema_chat_group.sql 的 fk_chat_groups_plan 同款。
DO $$
BEGIN
  IF to_regclass('strategies') IS NOT NULL
     AND NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'fk_sessions_strategy') THEN
    ALTER TABLE sessions ADD CONSTRAINT fk_sessions_strategy
      FOREIGN KEY (strategy_id) REFERENCES strategies(id) ON DELETE SET NULL;
  END IF;
END $$;

-- ---------- session_members ----------
-- 一个 agent 在一个会话里的席位。热加入 = 再写一行（ON CONFLICT 覆盖 presence）；
-- 退出 = 写 left_at + presence='left'，**不改历史消息的归属**（proposal §9 约束③）。
--
-- presence 的取值比 §4.3 的三态多两个：'idle'（在席但没在听）与 'left'（已退出，
-- left_at 有值）。两者都不是「工作中 / 卡住了」这两种需要人关注的状态。
CREATE TABLE IF NOT EXISTS session_members (
    session_id  BIGINT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    agent_key   TEXT   NOT NULL REFERENCES agents(key) ON DELETE CASCADE,
    role        TEXT   NOT NULL DEFAULT 'member'
                CHECK (role IN ('dispatcher','member','observer')),
    presence    TEXT   NOT NULL DEFAULT 'idle'
                CHECK (presence IN ('idle','listening','working','blocked','left')),
    -- blocked 的补充说明：预计几分钟 / 卡在谁身上（DotCollab 式的 presence 三态）。
    eta_minutes INTEGER,
    blocked_on  TEXT,
    joined_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    left_at     TIMESTAMPTZ,
    PRIMARY KEY (session_id, agent_key)
);
CREATE INDEX IF NOT EXISTS idx_session_members_agent ON session_members(agent_key)
    WHERE left_at IS NULL;

-- ---------- session_attachments ----------
-- 会话↔工件：typed + 双向。正向「会话 → 工件」在这里；反向「工件 → 会话」走
-- GET /api/sessions/attachments/{type}/{identity_key}/sessions（proposal §4.4）。
--
-- attachment_type 不加 CHECK：proposal §4.1 的 DDL 没加，而 §4.4 的「内置工件
-- 类型」清单（task / task_hall / strategy / asset / finding / pr / external）是
-- **应用层白名单**（db/session.go 的 sessionAttachmentTypes 代码常量）⇒ 允许
-- 扩展类型，同时由确定性层（Go 白名单）而不是提示词来裁决。
CREATE TABLE IF NOT EXISTS session_attachments (
    id             BIGSERIAL PRIMARY KEY,
    session_id     BIGINT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    attachment_type TEXT NOT NULL,
    -- identity_key 是稳定标识（PR 用 'owner/repo#12'）⇒ 反查靠它。
    identity_key   TEXT NOT NULL,
    payload        JSONB NOT NULL DEFAULT '{}',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (session_id, attachment_type, identity_key)
);
CREATE INDEX IF NOT EXISTS idx_sess_attach_owner ON session_attachments(attachment_type, identity_key);

-- ---------- session_messages ----------
-- 会话消息：人 / agent 同一条流。from_agent='' 表示人；to_agent='' 表示广播给
-- 全会话。kind='receipt' 的行是**收据条**（不占消息气泡，见 proposal §2.3 第 7 条）。
CREATE TABLE IF NOT EXISTS session_messages (
    id             BIGSERIAL PRIMARY KEY,
    session_id     BIGINT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    -- 线程 / 回复层：补充意见折叠在原消息下面（proposal §4.1 ④）。
    parent_message_id BIGINT REFERENCES session_messages(id) ON DELETE CASCADE,
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
    -- 只表示「发送方声称本消息携带某个已批准」：永远是不可信输入，没有任何权限门
    -- 读这一列（与 db/chat_group.go 的同名列同款纪律）。
    claims_authorization BOOLEAN NOT NULL DEFAULT false,
    -- 收据条：agent 的「收到 / 好的」类消息只落这里，不进消息流（避免群聊被确认
    -- 噪音淹没）。UI 渲染成 ✓✓ 贴在原消息下方。
    receipt_of     BIGINT REFERENCES session_messages(id) ON DELETE CASCADE,
    receipt_action TEXT NOT NULL DEFAULT ''
                   CHECK (receipt_action IN ('','taken','noted','deferred')),
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    read_at        TIMESTAMPTZ
);
-- ⚠️ 收据条的**必填**约束（proposal §4.1 的三处引用对齐说明）：
--   没有 receipt_of 就分不清「一条普通消息」和「贴在某条消息下的收据」——
--   UI 只能靠 kind 判断，而 kind='receipt' 的消息体为空 ⇒ 挂不到原消息上。
-- 这条写成 CHECK 而不是只写在 Go 里：它是数据不变量，模型提议也绕不过去。
ALTER TABLE session_messages DROP CONSTRAINT IF EXISTS ck_session_messages_receipt_of;
ALTER TABLE session_messages ADD CONSTRAINT ck_session_messages_receipt_of
    CHECK (kind <> 'receipt' OR receipt_of IS NOT NULL);
ALTER TABLE session_messages DROP CONSTRAINT IF EXISTS ck_session_messages_receipt_action;
ALTER TABLE session_messages ADD CONSTRAINT ck_session_messages_receipt_action
    CHECK (kind <> 'receipt' OR receipt_action IN ('taken','noted','deferred'));
CREATE INDEX IF NOT EXISTS idx_session_messages_session ON session_messages(session_id, id);
CREATE INDEX IF NOT EXISTS idx_session_messages_parent  ON session_messages(parent_message_id) WHERE parent_message_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_session_messages_receipt ON session_messages(receipt_of) WHERE receipt_of IS NOT NULL;

-- ---------- conversations.session_id（proposal §4.7 第 1 条）----------
-- 「不动 conversations 结构，只加一列」⇒ 幂等 ADD COLUMN（不改 db/schema.sql）。
-- ON DELETE SET NULL：删会话不该删会话记录（红线：不删数据）。
ALTER TABLE conversations ADD COLUMN IF NOT EXISTS session_id BIGINT REFERENCES sessions(id) ON DELETE SET NULL;
CREATE INDEX IF NOT EXISTS idx_conversations_session ON conversations(session_id) WHERE session_id IS NOT NULL;

-- ---------- chat_groups.session_id（proposal §4.7 第 3 条）----------
-- 群 = session 的一种（kind='task_hall'）；chat_groups 保留，task_id 继续用。
ALTER TABLE chat_groups ADD COLUMN IF NOT EXISTS session_id BIGINT REFERENCES sessions(id) ON DELETE SET NULL;
CREATE INDEX IF NOT EXISTS idx_chat_groups_session ON chat_groups(session_id) WHERE session_id IS NOT NULL;

-- ---------- kind='master'（总控会话）----------
-- 老库的内联 CHECK 由建表时的自动命名（sessions_kind_check）兜住：先 DROP 再 ADD
-- 一条**具名**约束，重复应用结果一致（本文件其余约束迁移同款纪律）。
ALTER TABLE sessions DROP CONSTRAINT IF EXISTS sessions_kind_check;
ALTER TABLE sessions ADD CONSTRAINT sessions_kind_check
    CHECK (kind IN ('chat','master','task_hall','side_question'));

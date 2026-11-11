-- 记忆层（方案 B）数据模型 —— **与 db/schema.sql 并行应用**的幂等 DDL。
--
-- 为什么单独成文件：db/schema.sql 由别的单元在同一 worktree 内并行开发，本单元
-- 不改它。本文件由 db/memory.go 的 EnsureMemorySchema 幂等补齐（见该文件注释），
-- schema 属主可以随时把内容并进 db/schema.sql —— 两边都是幂等的，重复应用无副作用。
--
-- 设计依据（session-groupchat-proposal.md §9 / §9.1 第 6 条 / §10.4 方案 B /
-- §10.5 / §10.7 / §11.5.3 / §11.8）：
--
--   ① **append-only + 版本化**：索引行一旦写下就不可就地改写。新内容 = 追加一个
--      新版本行；旧行只被「闭合有效区间」（valid_to）标记为被取代/退役，内容不变。
--      理由（§11.5.3-1）：「记忆也是摘要，用更好的摘要去修一个摘要往往不работает」
--      ⇒ dream 只提 diff，索引不整体重写。
--   ② **scope 强制**：scope_type/scope_id NOT NULL。scope 指向**会话**（kind =
--      'task_hall' 或 'chat'），**不指向 agent**（§9 第 6 条：per-agent 记忆 = N 份
--      发散副本 + 写冲突）。written_by 只记「谁写的」（审计），不是归属。
--   ③ **取代用显式关系**：supersedes_id + valid_to + 只追加的账本（§10.5-4）。
--      用「关闭区间」表达取代，而不是删掉旧行（§10.2：Graphiti/Zep 真正买到的三样
--      之一就是双时态 + 用关闭区间代替删除）。
--   ④ **索引与正文分离**：索引（常驻、有硬上限）只放检索指针与一行摘要；正文
--      （markdown）放 memory_topic，按需读（§11.1-1：「记忆必须是索引，不要把内容
--      全塞进上下文；索引常驻、详情按需读」）。
--   ⑤ **规则不住在索引里**：索引只有 kind ∈ ('note','link')，没有「规则」这种
--      kind；规则本体住在 agent_prompts，索引只放指针 rule_ref（§11.5.3-2）。
--      ⇒ 结构上无法把一条可执行规则塞进记忆索引。
--   ⑥ **记忆是提示，不是安全边界**：本文件里的任何列都不参与权限判定。
--
-- ⚠️ 本文件**不含任何上限阈值列**：索引条目数上限 / 字节上限 / 提示块预算全部是
-- db/memory.go 的代码常量（§8 红线第 3 条：新增安全边界进代码常量，不进可热改的
-- DB/settings）。DB 只存【观测到的状态】。

-- ---------- memory_index：append-only 的版本化记忆索引 ----------
CREATE TABLE IF NOT EXISTS memory_index (
    id                BIGSERIAL PRIMARY KEY,
    -- scope 指向会话（§10.7：scope 就是 session，不需要另立 scope 体系）。
    -- 今天的落地映射：task_hall ⇒ chat_groups.id，chat ⇒ conversations.id；
    -- sessions 表落地后两者都变成 sessions.id。记忆层自己**不解释** scope_id 的
    -- 含义（它是调用方的键），只保证「按 scope 过滤」这条不变式。
    scope_type        TEXT NOT NULL
                        CHECK (scope_type IN ('task_hall','chat')),
    scope_id          BIGINT NOT NULL CHECK (scope_id > 0),
    -- 主题键：同一 scope 内一个主题的多版本共用它。
    topic_key         TEXT NOT NULL CHECK (topic_key <> ''),
    version           INTEGER NOT NULL DEFAULT 1 CHECK (version > 0),
    -- kind 只有 note/link：**没有 rule** ⇒ 索引结构上装不下「可执行规则」。
    kind              TEXT NOT NULL DEFAULT 'note'
                        CHECK (kind IN ('note','link')),
    -- 索引行（常驻部分）：一行标题 + 一行摘要，正文在 memory_topic。
    title             TEXT NOT NULL DEFAULT '',
    summary_line      TEXT NOT NULL DEFAULT '',
    tags              TEXT[] NOT NULL DEFAULT '{}',
    -- 规则指针：形如 agent_prompts:<id>。规则本体住在 agent_prompts（§11.5.3-2），
    -- 这里只放「去哪里取」的指针，不放规则内容。
    rule_ref          TEXT NOT NULL DEFAULT '',
    -- 来源会话（§10.7：记忆指向来源会话）。
    source_session_ids BIGINT[] NOT NULL DEFAULT '{}',
    -- 写者（审计用，不是归属，见 §9 第 6 条）。
    written_by        TEXT NOT NULL DEFAULT '',
    -- 晋级门（X4，方案 v10 §6 T2）：两段式披露的**状态列**。
    --   active      —— 当前生效，进入注入/检索面（两段式披露的第一段）；
    --   quarantined —— 隔离态：模型写入即隔离（agent 写路径默认落这里），
    --                  host-only 晋级（走 Supersede 通道追加 active 新版本）才可见。
    -- 存量回填 'active' —— **如实口径**：老数据全是人工信任过的（或至少是
    -- 人工评审体系里已经存在的），不因加列而被隐藏。
    -- state 是内容列：append-only 触发器不许就地改它 —— 转态只能走
    -- Supersede（追加新版本）或 Retire（闭合区间）这两条双时态通道。
    state             TEXT NOT NULL DEFAULT 'active'
                        CHECK (state IN ('active','quarantined')),
    -- 内容摘要（sha256 十六进制）：内容一旦写下就不会变 ⇒ 摘要用来验「没被改过」。
    content_digest    TEXT NOT NULL DEFAULT '',
    -- 取代关系：新行指向它取代的那一行（§10.5-4）。
    supersedes_id     BIGINT REFERENCES memory_index(id) ON DELETE RESTRICT,
    -- 双时态区间（§10.2）：valid_to 为空 ⇒ 当前有效；非空 ⇒ 已被取代或已退役。
    valid_from        TIMESTAMPTZ NOT NULL DEFAULT now(),
    valid_to          TIMESTAMPTZ,
    superseded_by     BIGINT REFERENCES memory_index(id) ON DELETE RESTRICT,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- 同一 scope 内同一主题的版本号唯一 ⇒ 版本分配天然是 CAS（§10.4 热路径）。
    CONSTRAINT uq_memory_index_version UNIQUE (scope_type, scope_id, topic_key, version)
);
CREATE INDEX IF NOT EXISTS idx_memory_index_scope  ON memory_index(scope_type, scope_id, valid_from);
CREATE INDEX IF NOT EXISTS idx_memory_index_topic  ON memory_index(scope_type, scope_id, topic_key, version DESC);
CREATE INDEX IF NOT EXISTS idx_memory_index_current ON memory_index(scope_type, scope_id)
    WHERE valid_to IS NULL;
-- idx_memory_index_active（两段式披露 X4/T2 读面，WHERE 里引用 state）不能放
-- 这里：老库走 ALTER 补列，列要到下方迁移窗口才存在——放前面会让老库启动崩
-- （production 2026-11-11 实证）。移到 state 迁移窗口之后。

-- ---------- memory_topic：索引指向的正文（按需读，不常驻上下文） ----------
CREATE TABLE IF NOT EXISTS memory_topic (
    id          BIGSERIAL PRIMARY KEY,
    entry_id    BIGINT NOT NULL UNIQUE REFERENCES memory_index(id) ON DELETE CASCADE,
    body        TEXT NOT NULL DEFAULT '',
    body_digest TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- ---------- memory_write_log：只追加的写入账（哈希链） ----------
-- 每次写索引都留一行：op / actor / prev_hash / new_hash（§10.4）。
-- 链按 scope 串起来 ⇒ 事后能验「这条索引是不是账上那条链上的」。
CREATE TABLE IF NOT EXISTS memory_write_log (
    id         BIGSERIAL PRIMARY KEY,
    scope_type TEXT NOT NULL CHECK (scope_type IN ('task_hall','chat')),
    scope_id   BIGINT NOT NULL CHECK (scope_id > 0),
    op         TEXT NOT NULL
                 CHECK (op IN ('append','supersede','retire','dream_diff')),
    actor      TEXT NOT NULL DEFAULT '',
    entry_id   BIGINT REFERENCES memory_index(id) ON DELETE RESTRICT,
    prev_hash  TEXT NOT NULL DEFAULT '',
    new_hash   TEXT NOT NULL DEFAULT '',
    detail     JSONB NOT NULL DEFAULT '{}',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_memory_write_log_scope ON memory_write_log(scope_type, scope_id, id);

-- ---------- 视图：当前索引（纯文本导出的形状） ----------
-- 「当前」= 有效区间未闭合的行。取代与退役都表现为 valid_to 非空 ⇒ 这两类行
-- 从当前索引里消失，但内容仍在表里（可查历史、可导出）。
CREATE OR REPLACE VIEW memory_index_current AS
SELECT id       AS entry_id,
       scope_type,
       scope_id,
       topic_key,
       version,
       kind,
       title,
       summary_line,
       tags,
       rule_ref,
       source_session_ids,
       written_by,
       content_digest,
       valid_from
  FROM memory_index
 WHERE valid_to IS NULL;

-- ---------- append-only 守卫 ----------
-- 为什么用触发器而不是只靠访问层自律：记忆索引的「不可就地改写」是**不变式**，
-- 任何绕过访问层的写（运维手写 SQL、别的单元误写）都必须被数据库挡住。
--
-- 允许的唯一 UPDATE：闭合有效区间（valid_to 从 NULL 变成非空）。也就是
-- 「被取代」与「退役」这两种**状态迁移**——内容列一个都不许动。
CREATE OR REPLACE FUNCTION memory_index_append_only() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'memory_index 是 append-only：禁止删除条目 %', OLD.id
            USING ERRCODE = 'restrict_violation';
    END IF;

    IF NEW.scope_type IS DISTINCT FROM OLD.scope_type
        OR NEW.scope_id IS DISTINCT FROM OLD.scope_id
        OR NEW.topic_key IS DISTINCT FROM OLD.topic_key
        OR NEW.version IS DISTINCT FROM OLD.version
        OR NEW.kind IS DISTINCT FROM OLD.kind
        OR NEW.title IS DISTINCT FROM OLD.title
        OR NEW.summary_line IS DISTINCT FROM OLD.summary_line
        OR NEW.tags IS DISTINCT FROM OLD.tags
        OR NEW.rule_ref IS DISTINCT FROM OLD.rule_ref
        OR NEW.source_session_ids IS DISTINCT FROM OLD.source_session_ids
        OR NEW.written_by IS DISTINCT FROM OLD.written_by
        OR NEW.content_digest IS DISTINCT FROM OLD.content_digest
        OR NEW.supersedes_id IS DISTINCT FROM OLD.supersedes_id
        -- state 也在守卫里：晋级/退役是**状态迁移**，走 Supersede（追加 active
        -- 新版本）或 Retire（闭合区间）双时态通道，不许就地翻转（X4 host-only）。
        OR NEW.state IS DISTINCT FROM OLD.state
        OR NEW.valid_from IS DISTINCT FROM OLD.valid_from
        OR NEW.created_at IS DISTINCT FROM OLD.created_at THEN
        RAISE EXCEPTION 'memory_index 是 append-only：内容列不可就地改写（条目 %）', OLD.id
            USING ERRCODE = 'restrict_violation';
    END IF;

    IF OLD.valid_to IS NOT NULL THEN
        RAISE EXCEPTION 'memory_index 的有效区间只能闭合一次（条目 %）', OLD.id
            USING ERRCODE = 'restrict_violation';
    END IF;

    IF NEW.valid_to IS NULL THEN
        RAISE EXCEPTION 'memory_index 的更新必须闭合有效区间：valid_to 必填（条目 %）', OLD.id
            USING ERRCODE = 'restrict_violation';
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_memory_index_append_only ON memory_index;

-- ---------- state 列的幂等迁移（X4：CREATE 内 + 幂等 ALTER + 存量回填） ----------
-- 为什么放在 DROP TRIGGER 之后、CREATE TRIGGER 之前：老库已经装着 append-only
-- 触发器，它只放行「闭合有效区间」的 UPDATE —— 存量回填是普通 UPDATE，必须
-- 在触发器缺席的窗口里跑。本文件每次应用都走这个窗口，重复执行零副作用。
--
-- 老库走 ALTER 补列；NOT NULL DEFAULT 让存量行**就地**变 active —— 这不是
-- 改写内容（state 是新列，历史行从来没有过这个属性），是加列时的如实初始化：
-- 旧数据是人工信任的，回填 'active' 让它们保持可见。
ALTER TABLE memory_index ADD COLUMN IF NOT EXISTS state TEXT NOT NULL DEFAULT 'active';
UPDATE memory_index SET state='active' WHERE state <> 'active' AND state <> 'quarantined';
-- 两段式披露（X4/T2）的读面：注入块与混合检索只扫 active 的当前行（列就位后才建）。
CREATE INDEX IF NOT EXISTS idx_memory_index_active ON memory_index(scope_type, scope_id)
    WHERE valid_to IS NULL AND state='active';
-- 老库补 CHECK（与 CREATE 内的定义一致；判据照 schema.sql ck_node_state 的
-- pg_get_constraintdef 先例，重复启动零开销）。
DO $$
DECLARE
    v_def text;
BEGIN
    SELECT pg_get_constraintdef(oid) INTO v_def
      FROM pg_constraint WHERE conname='ck_memory_index_state' AND conrelid='memory_index'::regclass;
    IF v_def IS NULL THEN
        ALTER TABLE memory_index ADD CONSTRAINT ck_memory_index_state
            CHECK (state IN ('active','quarantined'));
    END IF;
END
$$;

CREATE TRIGGER trg_memory_index_append_only BEFORE UPDATE OR DELETE ON memory_index
    FOR EACH ROW EXECUTE FUNCTION memory_index_append_only();

-- 写入账是**只追加**的账本：任何 UPDATE / DELETE 都直接拒绝（没有「闭合区间」
-- 这种例外 —— 账本记的是「发生过什么」，改它等于改历史）。
CREATE OR REPLACE FUNCTION memory_write_log_append_only() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'memory_write_log 是只追加的账本：禁止 %', TG_OP
        USING ERRCODE = 'restrict_violation';
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_memory_write_log_append_only ON memory_write_log;
CREATE TRIGGER trg_memory_write_log_append_only BEFORE UPDATE OR DELETE ON memory_write_log
    FOR EACH ROW EXECUTE FUNCTION memory_write_log_append_only();

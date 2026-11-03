-- 跨会话共享上下文（cross-session shared context）的持久化 —— **与 db/schema.sql
-- 并行应用**的幂等 DDL。
--
-- 为什么单独成文件：db/schema.sql 是 RESERVED 文件（别的单元在同一 worktree 内并行
-- 开发），本单元不改它。本文件由 db/session_share.go 的 EnsureSessionShareSchema
-- 幂等补齐。
--
-- 依据：session-groupchat-proposal.md §9.1 第 10 条（跨 session 共享上下文定稿
-- 落在 P5）+ §7 的 P5 行。
--
-- 范围（**窄且安全**）：跨会话共享的是「另一个会话的事实陈述」，不是「另一个会话
-- 的行动契约」，也不是「另一个会话的权限」。具体三条：
--
--   1. 只有 note / answer / finding 三种消息类型可以跨界（**代码常量**裁决，
--      见 db/session_share.go 的 sessionShareableKinds）。request / escalation
--      是「要谁做什么」的行动契约 ⇒ 跨界会把「对 A 的请求」变成「对 B 的指令」。
--   2. 共享内容是**不可信输入**：不携带任何授权（claims_authorization 在投影里
--      被强制置 false），被投递前拦截（blocked）的消息不跨界。
--   3. 共享是**消费方付费**：内容进的是消费方的上下文窗口 ⇒ 额度算在消费方头上；
--      供给方既不被扣也不被加（额度不能跨会话洗）。
--
-- 三张表：
--   session_context_grants  谁可以把哪个会话的内容看过来（显式授予，可撤销）
--   session_context_reads   每次读取的账（额度归属 + 「谁借了谁什么」的答案）
--   （审计链 audit_events 由 db/audit_chain.go 追加，不在本文件里建）

-- ---------- session_context_grants ----------
-- 授予是**显式**的：默认没有任何会话能看到另一个会话的内容。
-- 一个消费方对同一个供给方只有一条授予（UNIQUE）⇒ 重复授予是覆盖而不是叠加。
CREATE TABLE IF NOT EXISTS session_context_grants (
    id                  BIGSERIAL PRIMARY KEY,
    -- 消费方：被授权看别人的会话的那一方（它付费、它承担上下文增长）。
    consumer_session_id BIGINT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    -- 供给方：内容来自哪个会话。
    source_session_id   BIGINT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    -- 请求共享的消息类型（逗号分隔）。**只是请求**：真正的白名单是代码常量
    -- （db/session_share.go 的 sessionShareableKinds）⇒ 改这一列不能放宽权限。
    share_kinds         TEXT NOT NULL DEFAULT 'note,answer',
    -- 单次读取最多带多少条（代码常量还有一道上限，DB 这列只是再收一道）。
    max_messages        INTEGER NOT NULL DEFAULT 50,
    -- 授予状态。撤销后不删行（红线：不删数据）⇒ 审计能看见「曾经授予过」。
    status              TEXT NOT NULL DEFAULT 'active'
                        CHECK (status IN ('active','revoked')),
    -- 谁授予的（平台目前是单密码登录 ⇒ 由调用方给一个操作者标签）。
    created_by          TEXT NOT NULL DEFAULT '',
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at          TIMESTAMPTZ,
    UNIQUE (consumer_session_id, source_session_id)
);
CREATE INDEX IF NOT EXISTS idx_session_context_grants_consumer
    ON session_context_grants(consumer_session_id) WHERE status = 'active';
CREATE INDEX IF NOT EXISTS idx_session_context_grants_source
    ON session_context_grants(source_session_id);

-- ---------- session_context_reads ----------
-- 读取账：**额度归属**与**审计留痕**的落点。
--
-- 每次「消费方读供给方的共享上下文」写一行：条数 + token 估算 + 具体消息 id。
-- 于是三个问题都有确定答案：
--   - 「B 从 A 借了多少 token？」 ⇒ SUM(token_estimate) WHERE consumer=B AND source=A
--   - 「哪些会话的内容影响了 B？」 ⇒ 同一条件下的 source 列表
--   - 「B 的上下文里有多少是外来的？」 ⇒ SUM(message_count) WHERE consumer=B
-- 供给方**不被扣也不被加**：它当初生成这些内容时已经付过钱了，再扣一次是重复
-- 计费，再加一次就是额度跨会话洗。
CREATE TABLE IF NOT EXISTS session_context_reads (
    id                  BIGSERIAL PRIMARY KEY,
    grant_id            BIGINT NOT NULL REFERENCES session_context_grants(id) ON DELETE CASCADE,
    consumer_session_id BIGINT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    source_session_id   BIGINT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    message_count       INTEGER NOT NULL DEFAULT 0,
    token_estimate      INTEGER NOT NULL DEFAULT 0,
    -- 本次读到的消息 id（可复算：凭它能回源会话核对原文）。
    message_ids         JSONB NOT NULL DEFAULT '[]'::jsonb,
    read_at             TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_session_context_reads_consumer
    ON session_context_reads(consumer_session_id, read_at DESC);
CREATE INDEX IF NOT EXISTS idx_session_context_reads_source
    ON session_context_reads(source_session_id, read_at DESC);
CREATE INDEX IF NOT EXISTS idx_session_context_reads_grant
    ON session_context_reads(grant_id, read_at DESC);

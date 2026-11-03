-- 会话收件条（inbox/digest）的持久化 —— **与 db/schema.sql 并行应用**的幂等 DDL。
--
-- 为什么单独成文件：db/schema.sql 是 RESERVED 文件（别的单元在同一 worktree 内并行
-- 开发），本单元不改它。本文件由 db/session_digest.go 的 EnsureSessionDigestSchema
-- 幂等补齐，与 db/session.go 的 EnsureSessionSchema 同款。
--
-- 依据：session-groupchat-proposal.md §7 的 P5 行「收件条/线程折叠等噪音治理」。
--
-- 为什么需要这张表：收件条的语义是「你不在时发生了什么」⇒ 必须有一个
-- **水位线**（用户上次看到哪条消息）才能划出窗口。没有水位线就只能把整个会话
-- 重新摘要一遍 —— 那正是收件条要消灭的噪音。
--
-- 为什么不用 session_messages.status 代替水位线：那一列的语义是
-- 「投递前被拦截 / 已读 / 已确认」，属于**消息投递**维度；收件条的水位线属于
-- **用户浏览**维度。两者会分叉（例如前端把消息标已读但用户没离开会话，
-- 或者用户离开了会话但没标已读）⇒ 独立一张表，不去占用别人的列语义。

-- ---------- session_digest_state ----------
CREATE TABLE IF NOT EXISTS session_digest_state (
    session_id BIGINT PRIMARY KEY REFERENCES sessions(id) ON DELETE CASCADE,
    -- 水位线：用户已经看到的最大消息 id（含）。摘要窗口 = id 大于它的消息。
    last_read_message_id BIGINT NOT NULL DEFAULT 0,
    -- 摘要条最后一次被读掉的时间（用于「你不在了多久」的展示）。
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_session_digest_state_seen ON session_digest_state(last_seen_at DESC);

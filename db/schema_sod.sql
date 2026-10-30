-- SoD 链（职责分离）数据模型 —— **与 db/schema.sql 并行应用**的幂等 DDL。
--
-- 为什么单独成文件：db/schema.sql 由别的单元在同一 worktree 内并行开发，
-- 本单元不改它。本文件由 db/sod.go 的 EnsureSoDSchema 幂等补齐
-- （见该文件注释），schema 属主可以随时把内容并进 db/schema.sql。
--
-- ⚠️ 本文件**不含任何阈值列**：熔断阈值 / 熔断时长 / 超时 / 缓存上限 /
-- 凭证 TTL 全部是 internal/sodpolicy 的代码常量。DB 只存【观测到的状态】。
-- 理由同 guard/shortcircuit.go：DB 规则行可被 DELETE、settings 可被热改。

-- ---------- sod_decisions：每一次裁决的账本 ----------
-- approve 也在内 —— 这是「每次决策都留痕」的完整账本。
-- 哈希链（db/audit_chain.go）只收 deny / needs_review / fail-closed，
-- 理由见 db/audit_chain.go:19-20（放行量极大，链上每条都依赖上一条）。
CREATE TABLE IF NOT EXISTS sod_decisions (
    id              BIGSERIAL PRIMARY KEY,
    task_id         BIGINT REFERENCES tasks(id) ON DELETE CASCADE,
    conversation_id BIGINT,
    source_agent    TEXT NOT NULL DEFAULT '',
    tool_name       TEXT NOT NULL DEFAULT '',
    verdict         TEXT NOT NULL DEFAULT ''
                      CHECK (verdict IN ('approve','deny','needs_review')),
    reason_code     TEXT NOT NULL DEFAULT '',
    -- 原始载荷只留摘要 + 脱敏预览；**绝不存原文**。
    payload_digest  TEXT NOT NULL DEFAULT '',
    payload_preview TEXT NOT NULL DEFAULT '',
    intent_digest   TEXT NOT NULL DEFAULT '',
    anomaly_flag    BOOLEAN NOT NULL DEFAULT false,
    anomaly_reason  TEXT NOT NULL DEFAULT '',
    checks          JSONB NOT NULL DEFAULT '{}',
    -- 只存凭证的摘要；token 本体永不落库。
    token_digest    TEXT NOT NULL DEFAULT '',
    scope_digest    TEXT NOT NULL DEFAULT '',
    cache_hit       BOOLEAN NOT NULL DEFAULT false,
    latency_ms      INTEGER NOT NULL DEFAULT 0,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_sod_decisions_task    ON sod_decisions(task_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_sod_decisions_verdict ON sod_decisions(verdict, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_sod_decisions_digest  ON sod_decisions(payload_digest);

-- ---------- sod_adversarial_review_pending：needs_review 的唯一去处 ----------
-- ⚠️ 这张表**不是**人工审批队列（那是 intercept_pending）。它由 redteam
-- agent 消费：对抗证伪，不是「请人看一眼」。
CREATE TABLE IF NOT EXISTS sod_adversarial_review_pending (
    id                BIGSERIAL PRIMARY KEY,
    task_id           BIGINT REFERENCES tasks(id) ON DELETE CASCADE,
    conversation_id   BIGINT,
    source_agent      TEXT NOT NULL DEFAULT '',
    tool_name         TEXT NOT NULL DEFAULT '',
    payload_digest    TEXT NOT NULL DEFAULT '',
    payload_preview   TEXT NOT NULL DEFAULT '',
    anomaly_flag      BOOLEAN NOT NULL DEFAULT false,
    anomaly_reason    TEXT NOT NULL DEFAULT '',
    reason_code       TEXT NOT NULL DEFAULT 'anomaly_flagged',
    structured_intent JSONB NOT NULL DEFAULT '{}',
    status            TEXT NOT NULL DEFAULT 'pending'
                        CHECK (status IN ('pending','claimed','resolved','dismissed')),
    claimed_by        TEXT NOT NULL DEFAULT '',
    resolution        TEXT NOT NULL DEFAULT '',
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_sod_review_pending
    ON sod_adversarial_review_pending(status, created_at ASC);
CREATE INDEX IF NOT EXISTS idx_sod_review_task
    ON sod_adversarial_review_pending(task_id, created_at DESC);
DROP TRIGGER IF EXISTS trg_sod_review_upd ON sod_adversarial_review_pending;
CREATE TRIGGER trg_sod_review_upd BEFORE UPDATE ON sod_adversarial_review_pending
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- ---------- sod_tool_circuit：工具熔断状态 ----------
-- 只存【观测到的状态】：失败计数 + 派生状态 + 时间戳。
-- 阈值（连续失败几次开路、开着多久）在 internal/sodpolicy 的代码常量里。
CREATE TABLE IF NOT EXISTS sod_tool_circuit (
    tool_key   TEXT PRIMARY KEY,
    state      TEXT NOT NULL DEFAULT 'closed'
                 CHECK (state IN ('closed','open','half_open')),
    fail_count INTEGER NOT NULL DEFAULT 0,
    opened_at  TIMESTAMPTZ,
    reason     TEXT NOT NULL DEFAULT '',
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
DROP TRIGGER IF EXISTS trg_sod_circuit_upd ON sod_tool_circuit;
CREATE TRIGGER trg_sod_circuit_upd BEFORE UPDATE ON sod_tool_circuit
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- ---------- findings.replay_evidence_id ----------
-- 「可重放证据」的标识（重放语料 id / 流量快照 id / 证据链 id）。
-- 空 = 该 finding 尚无可重放证据 ⇒ tier2 一致性检查判定为 tier2_inconsistent。
-- ⚠️ 全仓库此前没有这个列（findings 表见 db/schema.sql:1096-1116）；
-- 它在 db/schema_sod.sql 里以 ADD COLUMN IF NOT EXISTS 引入，
-- **不改 db/schema.sql**（那个文件由别的单元持有）。
ALTER TABLE findings ADD COLUMN IF NOT EXISTS replay_evidence_id TEXT NOT NULL DEFAULT '';

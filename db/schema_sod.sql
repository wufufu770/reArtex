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

-- ---------- sod_manual_overrides：裁决人工复核改判（方案 v3 缺陷 F3） ----------
-- 人工对一条 deny/needs_review 裁决做「放行」改判后的放行凭据登记：
--   * (tool_key, task_id, agent_key, payload_digest) 四元组全等命中且未过期 ⇒
--     链在规整之前直接铸凭证放行（call-precision：换一个字节就不命中）；
--   * scope='once' 用后即焚（use_count+1），scope='task' 随任务终态过期；
--   * DB 持久 —— 重启不丢（与内存执行凭证表的本质区别）；
--   * Tier3 四类（出网非目标段/写宿主 FS 任务目录外/读宿主敏感面/裁决写）
--     在 API 层拒绝改判 allow（只可转 ask）——任何机制都不可豁免 Tier3。
CREATE TABLE IF NOT EXISTS sod_manual_overrides (
    id             BIGSERIAL PRIMARY KEY,
    decision_id    BIGINT NOT NULL,
    payload_digest TEXT NOT NULL,
    tool_key       TEXT NOT NULL,
    task_id        BIGINT NOT NULL,
    agent_key      TEXT NOT NULL DEFAULT '',
    scope          TEXT NOT NULL DEFAULT 'once' CHECK (scope IN ('once','task','task_tool')),
    reason         TEXT NOT NULL DEFAULT '',
    created_by     TEXT NOT NULL DEFAULT '',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at     TIMESTAMPTZ,          -- NULL=不过期（task 级由任务终态清）
    use_count      INTEGER NOT NULL DEFAULT 0,
    last_used_at   TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_sod_override_lookup
    ON sod_manual_overrides(tool_key, task_id, agent_key, payload_digest);
CREATE INDEX IF NOT EXISTS idx_sod_override_task ON sod_manual_overrides(task_id);

-- ---------- 拦截种类表 + 影子对照 + auditor 建议队列（方案 v4.1）----------
-- 种类表是用户可编辑的「该拦什么」配置：内建种类以确定性分类器为识别器，
-- custom 是用户正则；每行 = 名字 + 匹配器 + 动作 + 每任务上限 + 兜底。
-- locked 行是锁定基线（删除/破坏、平台自身配置）——动作不可改、不可删，
-- guard 基线预检与链内复查共享同一判定（internal/sodpolicy/baseline.go）。
CREATE TABLE IF NOT EXISTS sod_categories (
    id           BIGSERIAL PRIMARY KEY,
    name         TEXT NOT NULL UNIQUE,
    kind         TEXT NOT NULL DEFAULT 'builtin' CHECK (kind IN ('builtin','custom')),
    pattern      TEXT NOT NULL,
    action       TEXT NOT NULL CHECK (action IN ('hard_deny','ask','auditor','pass')),
    cap_per_task INTEGER NOT NULL DEFAULT 0,
    fallback     TEXT NOT NULL DEFAULT 'ask' CHECK (fallback IN ('deny','ask','pass')),
    locked       BOOLEAN NOT NULL DEFAULT false,
    enabled      BOOLEAN NOT NULL DEFAULT true,
    priority     INTEGER NOT NULL DEFAULT 50,
    reason       TEXT NOT NULL DEFAULT '',
    created_by   TEXT NOT NULL DEFAULT 'system',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_sod_categories_priority ON sod_categories(priority DESC) WHERE enabled;

-- 影子对照账本（§11-G1）：off→shadow→enforce 灰度期里，新旧链对同一调用的
-- 裁决差异逐行落库；核对无误伤后切 enforce，本表转为历史证据。
CREATE TABLE IF NOT EXISTS sod_gate_shadow (
    id            BIGSERIAL PRIMARY KEY,
    task_id       BIGINT,
    tool_name     TEXT NOT NULL,
    payload_digest TEXT NOT NULL,
    old_verdict   TEXT NOT NULL,
    old_reason    TEXT NOT NULL DEFAULT '',
    new_verdict   TEXT NOT NULL,
    new_reason    TEXT NOT NULL DEFAULT '',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_sod_shadow_task ON sod_gate_shadow(task_id, created_at DESC);

-- auditor 的观察建议队列（§3）：auditor 在裁决里建议新增/调整种类，人工
-- 一键采纳后沉淀为种类表行——裁量经验持续确定性化。
CREATE TABLE IF NOT EXISTS sod_category_suggestions (
    id         BIGSERIAL PRIMARY KEY,
    name       TEXT NOT NULL,
    kind       TEXT NOT NULL DEFAULT 'custom' CHECK (kind IN ('builtin','custom','cap')),
    pattern    TEXT NOT NULL DEFAULT '',
    action     TEXT NOT NULL DEFAULT 'pass' CHECK (action IN ('hard_deny','ask','auditor','pass')),
    evidence   JSONB NOT NULL DEFAULT '[]'::jsonb,
    status     TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','adopted','dismissed')),
    reason     TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    decided_at TIMESTAMPTZ,
    decided_by TEXT NOT NULL DEFAULT ''
);

-- 采纳回链（方案 v6.1）：种类 ← 采纳来源建议。不加外键（建表顺序在前，
-- 引用后置表会让全新库 schema 应用整体失败）；回链完整性由采纳/撤销代码维护。
ALTER TABLE sod_categories ADD COLUMN IF NOT EXISTS suggestion_id BIGINT;

-- E1-5（方案 v10 §9）：放行凭据加 task_tool 档——ask 批准时可选「同时放行本任务
-- 后续同类调用」：(task, tool, agent) 级通配（payload_digest=''），仅限非基线工具。
-- 旧库 CHECK 迁移（约束名是内联 CHECK 的自动名）。
ALTER TABLE sod_manual_overrides DROP CONSTRAINT IF EXISTS sod_manual_overrides_scope_check;
ALTER TABLE sod_manual_overrides ADD CONSTRAINT sod_manual_overrides_scope_check
    CHECK (scope IN ('once','task','task_tool'));

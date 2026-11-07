-- 传承图（P5，第三张图）—— 独立顶层图，跨任务跨会话存活。
-- 建图纪律照抄策略库（db/schema_strategy.sql 头注）：独立幂等 DDL 单文件、
-- company_id 租户隔离、人工评审闸（无自动晋升——与 d2d knowledge brain 的刻意切割）。
--
-- kind 刻意不含 playbook：playbook 是「怎么打」，归策略库管——避免两套跨任务经验池
-- （红队 v3 P1-3）。state 词汇 {draft, accepted, retired} 刻意避开 fact 的
-- {candidate, verified, refuted}——按 kind 分词汇不混用（红队 P2-5）。

CREATE TABLE IF NOT EXISTS legacy_nodes (
    id              BIGSERIAL PRIMARY KEY,
    company_id      BIGINT REFERENCES companies(id) ON DELETE SET NULL,
    kind            TEXT NOT NULL CHECK (kind IN ('experience','pitfall','fingerprint_summary')),
    title           TEXT NOT NULL DEFAULT '',
    summary         TEXT NOT NULL DEFAULT '',  -- ≤4000 字符（注入预算内的正文）
    detail          TEXT NOT NULL DEFAULT '',  -- 扩展说明（不进注入，传承页可见）
    fingerprint     JSONB NOT NULL DEFAULT '{}'::jsonb,  -- 离散 KV（key 限 [A-Za-z0-9_.-]{1,64}，值标量）
    state           TEXT NOT NULL DEFAULT 'draft' CHECK (state IN ('draft','accepted','retired')),
    source_task_id  BIGINT,
    source_session  TEXT NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    reviewed_at     TIMESTAMPTZ,
    reviewed_by     TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_legacy_state ON legacy_nodes(state) WHERE state='accepted';
CREATE INDEX IF NOT EXISTS idx_legacy_company ON legacy_nodes(company_id);

CREATE TABLE IF NOT EXISTS legacy_edges (
    id      BIGSERIAL PRIMARY KEY,
    src_id  BIGINT NOT NULL REFERENCES legacy_nodes(id) ON DELETE CASCADE,
    dst_id  BIGINT NOT NULL REFERENCES legacy_nodes(id) ON DELETE CASCADE,
    rel     TEXT NOT NULL CHECK (rel IN ('derived_from','variant_of','contradicts')),
    UNIQUE (src_id, dst_id, rel)
);

CREATE TABLE IF NOT EXISTS legacy_usage (
    id            BIGSERIAL PRIMARY KEY,
    experience_id BIGINT NOT NULL REFERENCES legacy_nodes(id) ON DELETE CASCADE,
    task_id       BIGINT NOT NULL,
    used_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_legacy_usage_exp ON legacy_usage(experience_id);

-- 删除/评审审计（方案 v3 缺陷 D2）：节点删除不可逆，删前把节点全量快照 +
-- legacy_usage 行落这里，防「审计随行蒸发」。评审转移不写快照（state 变更
-- 本身留痕在行上），只给 delete 用。
CREATE TABLE IF NOT EXISTS legacy_audit (
    id          BIGSERIAL PRIMARY KEY,
    action      TEXT NOT NULL,                -- delete（后续可扩 review）
    node_id     BIGINT NOT NULL,
    payload     JSONB NOT NULL,               -- {"node":{...},"usages":[...]}
    operator    TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_legacy_audit_node ON legacy_audit(node_id);

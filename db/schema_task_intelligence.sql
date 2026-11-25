-- schema_task_intelligence.sql —— Task Intelligence 统一领域对象（v2.1 统一架构
-- 优化增补 §0.5「统一领域对象与数据关系」，0.15.63，批次 TI-05）。
--
-- 应用方式与各层 Ensure*Schema 相同：启动期用有 DDL 权限的迁移连接执行
-- （db/db.go 启动块 → EnsureTaskIntelligenceSchema），受限运行角色运行期只读
-- 写不建表。幂等（CREATE TABLE IF NOT EXISTS / ADD COLUMN IF NOT EXISTS），
-- 重复应用零副作用。
--
-- 存储策略（0.5 原文口径）：优先扩展现有表和关系，不新建平行资产库/发现库/
-- 证据库。本文件只做两件事：
--   1. 补齐四类缺失领域对象：hypotheses（假设）、facts（事实）、
--      evidence_refs（证据引用）、capability_executions（能力执行记录）。
--      ModelDelta 不另建表——biz_layer_diffs 已是差异事实源（db/schema_biz.sql）；
--      Finding 复用现有 findings 表（0.5「不另建平行漏洞实体」）。
--   2. 给既有表补统一身份引用列（§0.13 阶段 TI-1「统一身份与引用」）：
--      correlation_id / causation_id / capability_id / scope_version。
--
-- 回滚：本文件全部是加法（新表/新列/新索引），回滚即 DROP 对象，不影响既有列。
--
-- ---------- 删除 / 归档语义与数据保留（0.15.94，TI-05 第二轮核对补档） ----------
--
-- 删除（0.15.63 已实现）：四表的 task_id 均 ON DELETE CASCADE——任务删除即
-- 清空其全部领域对象；hypotheses/facts/evidence_refs/capability_executions 的
-- company_id/asset_id/hypothesis_id/finding_id 等引用列 ON DELETE SET NULL——
-- 引用目标消失不连带删对象，只丢回指标记。访问层（db/task_intelligence.go）
-- 只提供 Insert/Get/List/状态推进，无逐行 DELETE 面：观测与取证记录不提供
-- 单行改写/删除通道。
--
-- 归档（本轮定义，无运行时改动）：假设的终态（validated/refuted/dropped）即
-- 该对象的归档态——状态机禁止离开终态（internal/taskintelligence/domain.go），
-- 人工放弃走 dropped；facts/evidence_refs/capability_executions 是 append-only
-- 观测/取证面，「归档」= 任务关闭后退出活跃查询口径，不改写历史。
--
-- 数据保留（方案，非本卡实现）：保留期是取证/审计合规决策，必须人工拍板，
-- 本最小领域模型不定 TTL；清理/分区/冷归档留给后续运维卡，实施时必须与审计链
-- （audit_events，0.15.89 起 HMAC+锚点）保留口径对齐——领域对象可清理，
-- 证据引用的 provenance 链不得比审计链先消失。

-- ---------- 跨对象统一身份引用 ----------
--
-- 0.1 割裂审计结论：全库此前零 correlation_id / causation_id / scope_version。
-- 列语义（§0.5「所有跨模块引用至少保留」清单）：
--   correlation_id  一次协作/调用链路的标识（TI-04 台账 corr-<causation>-<n>）
--   causation_id    触发源对象标识（如 biz_layer_diffs#12）
--   capability_id   五标准能力 ID（internal/taskintelligence：exploration.edge 等）
--   scope_version   任务范围水位：COALESCE(MAX(id),0) FROM task_scope WHERE
--                   task_id=?——task_scope 任意插入都会抬升水位，无需新计数器
--                   （schema.sql 保留文件不动）。

ALTER TABLE edge_runs ADD COLUMN IF NOT EXISTS capability_id    TEXT NOT NULL DEFAULT '';
ALTER TABLE edge_runs ADD COLUMN IF NOT EXISTS correlation_id   TEXT NOT NULL DEFAULT '';
ALTER TABLE edge_runs ADD COLUMN IF NOT EXISTS causation_id     TEXT NOT NULL DEFAULT '';
ALTER TABLE edge_runs ADD COLUMN IF NOT EXISTS scope_version    BIGINT NOT NULL DEFAULT 0;

ALTER TABLE biz_layer_diffs ADD COLUMN IF NOT EXISTS correlation_id TEXT NOT NULL DEFAULT '';
ALTER TABLE biz_layer_diffs ADD COLUMN IF NOT EXISTS causation_id   TEXT NOT NULL DEFAULT '';
ALTER TABLE biz_layer_diffs ADD COLUMN IF NOT EXISTS scope_version  BIGINT NOT NULL DEFAULT 0;

CREATE INDEX IF NOT EXISTS idx_edge_runs_correlation ON edge_runs(correlation_id) WHERE correlation_id <> '';
CREATE INDEX IF NOT EXISTS idx_biz_diffs_correlation ON biz_layer_diffs(correlation_id) WHERE correlation_id <> '';

-- ---------- hypotheses：可证伪的测试假设（不是漏洞结论） ----------
--
-- 0.5 领域对象表：id、task_id、fingerprint、statement、source、status、
-- confidence、created_by_execution_id + 统一身份引用列。指纹由
-- internal/taskintelligence.HypothesisFingerprint 生成（同任务同语句同指纹），
-- (task_id, fingerprint) 唯一索引在库层兑现「跨能力去重合并」：重复提案收到
-- 已存在行而不是第二行。
CREATE TABLE IF NOT EXISTS hypotheses (
    id                       BIGSERIAL PRIMARY KEY,
    task_id                  BIGINT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    company_id               BIGINT REFERENCES companies(id) ON DELETE SET NULL,
    fingerprint              TEXT NOT NULL DEFAULT '',
    statement                TEXT NOT NULL,
    -- proposed=待验证（假设 ≠ 结论）；validated/refuted 只能由证据链推进，
    -- dropped=人工放弃。晋级 findings 走 E-* 验证管线，不在这里。
    status                   TEXT NOT NULL DEFAULT 'proposed'
                             CHECK (status IN ('proposed','validating','validated','refuted','dropped')),
    source                   TEXT NOT NULL DEFAULT '',      -- 提出来源一句话（能力/差异/人工）
    source_type              TEXT NOT NULL DEFAULT '',      -- capability / model_delta / manual / …
    source_ref               TEXT NOT NULL DEFAULT '',      -- 源对象标识（biz_layer_diffs#12 等）
    confidence               REAL NOT NULL DEFAULT 0
                             CHECK (confidence >= 0 AND confidence <= 1),
    created_by_execution_id  TEXT NOT NULL DEFAULT '',      -- capability_executions.execution_id
    capability_id            TEXT NOT NULL DEFAULT '',
    correlation_id           TEXT NOT NULL DEFAULT '',
    causation_id             TEXT NOT NULL DEFAULT '',
    scope_version            BIGINT NOT NULL DEFAULT 0,
    model_version            BIGINT NOT NULL DEFAULT 0,     -- biz_model_versions.id（业务语境）
    methodology_version      TEXT NOT NULL DEFAULT '',      -- edge_methodologies 版本
    created_at               TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at               TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX IF NOT EXISTS uq_hypotheses_task_fingerprint
    ON hypotheses(task_id, fingerprint) WHERE fingerprint <> '';
CREATE INDEX IF NOT EXISTS idx_hypotheses_task_status ON hypotheses(task_id, status);
CREATE INDEX IF NOT EXISTS idx_hypotheses_company ON hypotheses(company_id) WHERE company_id IS NOT NULL;

-- ---------- facts：有来源的观察事实（区分 observed / inferred） ----------
CREATE TABLE IF NOT EXISTS facts (
    id                   BIGSERIAL PRIMARY KEY,
    task_id              BIGINT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    company_id           BIGINT REFERENCES companies(id) ON DELETE SET NULL,
    subject              TEXT NOT NULL DEFAULT '',
    predicate            TEXT NOT NULL DEFAULT '',
    object               TEXT NOT NULL DEFAULT '',
    fact_kind            TEXT NOT NULL DEFAULT 'observed'
                         CHECK (fact_kind IN ('observed','inferred')),
    source_type          TEXT NOT NULL DEFAULT '',   -- tool / traffic / model / agent / …
    source_ref           TEXT NOT NULL DEFAULT '',   -- 源位置（exchange id / 节点 id / …）
    confidence           REAL NOT NULL DEFAULT 0.5
                         CHECK (confidence >= 0 AND confidence <= 1),
    hypothesis_id        BIGINT REFERENCES hypotheses(id) ON DELETE SET NULL,
    execution_id         TEXT NOT NULL DEFAULT '',
    capability_id        TEXT NOT NULL DEFAULT '',
    correlation_id       TEXT NOT NULL DEFAULT '',
    causation_id         TEXT NOT NULL DEFAULT '',
    scope_version        BIGINT NOT NULL DEFAULT 0,
    model_version        BIGINT NOT NULL DEFAULT 0,
    methodology_version  TEXT NOT NULL DEFAULT '',
    observed_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_facts_task ON facts(task_id, observed_at DESC);
CREATE INDEX IF NOT EXISTS idx_facts_hypothesis ON facts(hypothesis_id) WHERE hypothesis_id IS NOT NULL;

-- ---------- evidence_refs：原始证据引用（大内容留在 traffic/blob，不复制正文） ----------
--
-- §0.12 口径：内容 hash + 不可变引用 + provenance；敏感正文不进本表，
-- content_ref 只放受控引用（traffic exchange id / snapshot id）。
CREATE TABLE IF NOT EXISTS evidence_refs (
    id                BIGSERIAL PRIMARY KEY,
    task_id           BIGINT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    company_id        BIGINT REFERENCES companies(id) ON DELETE SET NULL,
    asset_id          BIGINT REFERENCES assets(id) ON DELETE SET NULL,
    kind              TEXT NOT NULL DEFAULT '',    -- traffic_exchange / snapshot / log / …
    content_ref       TEXT NOT NULL DEFAULT '',    -- 受控引用（不存正文）
    content_hash      TEXT NOT NULL DEFAULT '',    -- sha256（完整性校验）
    provenance        TEXT NOT NULL DEFAULT '',    -- 来源说明一句话
    support           TEXT NOT NULL DEFAULT 'neutral'
                      CHECK (support IN ('supports','refutes','neutral')),
    hypothesis_id     BIGINT REFERENCES hypotheses(id) ON DELETE SET NULL,
    finding_id        BIGINT REFERENCES findings(id) ON DELETE SET NULL,
    execution_id      TEXT NOT NULL DEFAULT '',
    capability_id     TEXT NOT NULL DEFAULT '',
    correlation_id    TEXT NOT NULL DEFAULT '',
    causation_id      TEXT NOT NULL DEFAULT '',
    scope_version     BIGINT NOT NULL DEFAULT 0,
    observed_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_evidence_task ON evidence_refs(task_id, observed_at DESC);
CREATE INDEX IF NOT EXISTS idx_evidence_hypothesis ON evidence_refs(hypothesis_id) WHERE hypothesis_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_evidence_finding ON evidence_refs(finding_id) WHERE finding_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_evidence_asset ON evidence_refs(asset_id) WHERE asset_id IS NOT NULL;

-- ---------- capability_executions：每次能力调用的可追踪记录 ----------
--
-- §0.5 领域对象表：id、task_id、capability_id、input_refs、output_refs、
-- status、cost、correlation_id。execution_id 是本次调用的 envelope 标识
-- （uuid），其余对象用它回指（hypotheses.created_by_execution_id 等）。
-- idempotency_key 防 Agent 重试造成重复运行（§0.11 契约要求）；
-- scope_version_at_start/end 记录调用前后任务范围水位——两者不一致 =
-- 长调用期间 scope 被变更（§0.12「执行前再次校验」的取证面）。
CREATE TABLE IF NOT EXISTS capability_executions (
    id                       BIGSERIAL PRIMARY KEY,
    task_id                  BIGINT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    company_id               BIGINT REFERENCES companies(id) ON DELETE SET NULL,
    capability_id            TEXT NOT NULL,
    tool_name                TEXT NOT NULL DEFAULT '',   -- 实际下钻到的工具名
    status                   TEXT NOT NULL DEFAULT 'running'
                             CHECK (status IN ('running','completed','failed','cancelled','blocked')),
    input_refs               JSONB NOT NULL DEFAULT '{}',
    output_refs              JSONB NOT NULL DEFAULT '{}',
    cost_tokens              BIGINT NOT NULL DEFAULT 0,
    duration_ms              BIGINT NOT NULL DEFAULT 0,
    error                    TEXT NOT NULL DEFAULT '',
    execution_id             TEXT NOT NULL DEFAULT '',
    correlation_id           TEXT NOT NULL DEFAULT '',
    causation_id             TEXT NOT NULL DEFAULT '',
    idempotency_key          TEXT NOT NULL DEFAULT '',
    scope_version_at_start   BIGINT NOT NULL DEFAULT 0,
    scope_version_at_end     BIGINT NOT NULL DEFAULT 0,
    created_at               TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at               TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX IF NOT EXISTS uq_capability_executions_idem
    ON capability_executions(idempotency_key) WHERE idempotency_key <> '';
CREATE INDEX IF NOT EXISTS idx_cap_exec_task ON capability_executions(task_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_cap_exec_capability ON capability_executions(capability_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_cap_exec_correlation ON capability_executions(correlation_id) WHERE correlation_id <> '';

-- ---------- domain_events：统一事件 outbox（§0.6，0.15.64 批次 TI-06） ----------
--
-- §0.6 口径：事件先落库，再异步派发；事件处理必须幂等、可重试、可观测。
-- 基础设施用 PostgreSQL outbox（单体期不引入 Kafka/NATS）。
--   - event_id 唯一索引 = 生产端幂等键（重复投递同一事件是 no-op）；
--   - 派发是 at-least-once：消费端必须幂等（domain_event_consumers 表是
--     进程内消费的第一道幂等闸，业务 handler 自身幂等是第二道）；
--   - payload 禁放凭据/Cookie/Authorization/原始敏感正文（§0.12；确定性
--     消毒在 events.SanitizePayload，TI-12 接入 EnqueueEvent）。
CREATE TABLE IF NOT EXISTS domain_events (
    id              BIGSERIAL PRIMARY KEY,
    event_id        TEXT NOT NULL,                  -- envelope.ID（uuid）
    type            TEXT NOT NULL,                  -- events.EventType*（§0.6 十一类）
    schema_version  INT NOT NULL DEFAULT 1,
    task_id         BIGINT REFERENCES tasks(id) ON DELETE SET NULL,
    company_id      BIGINT REFERENCES companies(id) ON DELETE SET NULL, -- envelope.TenantID 投影
    execution_id    TEXT NOT NULL DEFAULT '',
    correlation_id  TEXT NOT NULL DEFAULT '',
    causation_id    TEXT NOT NULL DEFAULT '',
    scope_version   BIGINT NOT NULL DEFAULT 0,      -- envelope.ScopeVersion（水位 bigint）
    payload         JSONB NOT NULL DEFAULT '{}',
    status          TEXT NOT NULL DEFAULT 'pending'
                    CHECK (status IN ('pending','dispatched','failed','dead')),
    attempts        INT NOT NULL DEFAULT 0,
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_error      TEXT NOT NULL DEFAULT '',
    occurred_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX IF NOT EXISTS uq_domain_events_event_id ON domain_events(event_id);
CREATE INDEX IF NOT EXISTS idx_domain_events_due ON domain_events(status, next_attempt_at);
CREATE INDEX IF NOT EXISTS idx_domain_events_task ON domain_events(task_id, occurred_at DESC);

-- 消费幂等台账：同一 event_id 同一 consumer 只记一行（主键）；重复投递的事件
-- 在派发前查此表跳过已处理消费者。
CREATE TABLE IF NOT EXISTS domain_event_consumers (
    event_id     TEXT NOT NULL,
    consumer     TEXT NOT NULL,
    processed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (event_id, consumer)
);

-- 运行度量（P0 评估基线）—— 幂等 DDL，db/metrics.go 应用。
-- llm_profile_prices：模型档单价（每百万 tokens）。经济配置，运维可编辑；
-- 键 = profile name（改名属重建，价格重录）。不进 llm_profiles 的手写 CRUD。
CREATE TABLE IF NOT EXISTS llm_profile_prices (
    profile_name      TEXT PRIMARY KEY,
    currency          TEXT NOT NULL DEFAULT 'CNY',
    per_m_input       DOUBLE PRECISION NOT NULL DEFAULT 0,
    per_m_output      DOUBLE PRECISION NOT NULL DEFAULT 0,
    per_m_cache_read  DOUBLE PRECISION NOT NULL DEFAULT 0,
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- A-13（0.15.82）：凭据熔断器状态表（profile + host 维度）。
--
-- 内存态是权威（internal/credbreaker.Breaker），本表是镜像：冷却窗跨重启存活。
-- Load 只取未过期的 open_until 行——断路已自然到期的键重启后视为健康，
-- 由下一次调用重新学习（与 llm_profile_health 同一口径）。

CREATE TABLE IF NOT EXISTS cred_breaker_state (
    profile_id  BIGINT      NOT NULL,
    host        TEXT        NOT NULL,
    consecutive INT         NOT NULL DEFAULT 0,
    open_until  TIMESTAMPTZ,
    last_reason TEXT        NOT NULL DEFAULT '',
    last_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (profile_id, host)
);

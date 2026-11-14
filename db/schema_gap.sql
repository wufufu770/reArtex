-- 覆盖缺口（0.14.3，批次二 F4）：任务 × 资产的覆盖缺口账。
--   - 结构化判据（宁漏勿误）：链接到任务、且没有被任何 open 意图锚定的资产 =
--     缺口（open）；资产获得 open 意图锚后 open → targeted；人工闭合 → closed
--     （closed 不被重扫复活）。
--   - task×asset 唯一：状态原位演进，不堆积行。
--   - 派生决策留给 planner/bizmodeler（读缺口面自主派生意图——LLM 提议、
--     确定性层闸门的既有纪律），平台不做自动派生（防意图洪水）。

CREATE TABLE IF NOT EXISTS coverage_gaps (
  id          bigserial PRIMARY KEY,
  task_id     bigint NOT NULL REFERENCES tasks(id),
  asset_id    bigint NOT NULL REFERENCES assets(id),
  reason      text   NOT NULL DEFAULT '',
  state       text   NOT NULL DEFAULT 'open'
              CHECK (state IN ('open','targeted','closed')),
  targeted_by bigint REFERENCES exploration_nodes(id),
  created_at  timestamptz NOT NULL DEFAULT now(),
  updated_at  timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX IF NOT EXISTS uq_coverage_gap_task_asset
  ON coverage_gaps (task_id, asset_id);

CREATE INDEX IF NOT EXISTS idx_coverage_gaps_task
  ON coverage_gaps (task_id, state);

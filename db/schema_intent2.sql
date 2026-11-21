-- 意图冲突仲裁（0.14.1，批次二 A）：
--   intent_conflicts 记录冲突对与裁决结果。
--   - (exploration_id, a_node_id, b_node_id) 归一对（存储时按 id 排序）在 state=open
--     时唯一——同一冲突对不会重复登记；
--   - detect_rule 是代码常量规则的稳定 ID（asset_mutex / reverse_predicate /
--     resource_clash）——二期规则数据驱动时 schema 零迁移；
--   - verdict 枚举 proceed_a / proceed_b / defer_both（枚举校验在宿主写函数）；
--   - state 机：open → adjudicated（仲裁落笔）→（reopen 回 open，reopen_count+1）；
--     open → frozen（仲裁 agent 连续 defer 熔断，唯一进人路径）；closed 由宿主在
--     意图终态联动关闭。
--   - basis 快照检测依据（资产对/谓词对），裁决可回放。

CREATE TABLE IF NOT EXISTS intent_conflicts (
  id              bigserial PRIMARY KEY,
  exploration_id  bigint NOT NULL,
  a_node_id       bigint NOT NULL REFERENCES exploration_nodes(id),
  b_node_id       bigint NOT NULL REFERENCES exploration_nodes(id),
  detect_rule     text   NOT NULL,
  basis           jsonb  NOT NULL DEFAULT '{}'::jsonb,
  state           text   NOT NULL DEFAULT 'open'
                  CHECK (state IN ('open','adjudicated','frozen','closed')),
  verdict         text   CHECK (verdict IS NULL OR verdict IN ('proceed_a','proceed_b','defer_both')),
  verdict_reason  text   NOT NULL DEFAULT '',
  resolved_by     text   NOT NULL DEFAULT ''
                  CHECK (resolved_by IN ('','arbiter','host','host-rule')),
  reopen_count    int    NOT NULL DEFAULT 0,
  created_at      timestamptz NOT NULL DEFAULT now(),
  updated_at      timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX IF NOT EXISTS uq_intent_conflict_open
  ON intent_conflicts (exploration_id, a_node_id, b_node_id)
  WHERE state = 'open';

CREATE INDEX IF NOT EXISTS idx_intent_conflicts_expl
  ON intent_conflicts (exploration_id, state);

-- 外键补 ON DELETE CASCADE（0.15.40 修复）：两条 node 外键建表时裸露——
-- 任务删除链 DELETE explorations 级联删 nodes 时以 RESTRICT 拦死
--（与 coverage_gaps 同款漏配）。冲突是任务级账目，随探索图消亡；
-- 幂等迁移（DROP IF EXISTS + ADD，照 schema_gap.sql 先例）。
ALTER TABLE intent_conflicts DROP CONSTRAINT IF EXISTS intent_conflicts_a_node_id_fkey;
ALTER TABLE intent_conflicts ADD CONSTRAINT intent_conflicts_a_node_id_fkey
  FOREIGN KEY (a_node_id) REFERENCES exploration_nodes(id) ON DELETE CASCADE;
ALTER TABLE intent_conflicts DROP CONSTRAINT IF EXISTS intent_conflicts_b_node_id_fkey;
ALTER TABLE intent_conflicts ADD CONSTRAINT intent_conflicts_b_node_id_fkey
  FOREIGN KEY (b_node_id) REFERENCES exploration_nodes(id) ON DELETE CASCADE;

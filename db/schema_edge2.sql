-- 方法论配置持久层（0.14.7，批次六 F1）：
--   - source='builtin-override'：内置 12 条的启停/排序覆盖（内置方法论【本体】
--     与判据仍在代码常量——校验规则与判据表达式是行为核心，不进 DB）；
--   - source='custom'：用户自定义方法论全量（读取时过 internal/edge 同一套
--     注册期校验——五类判据/上限强制/隐喻不带判据，校验规则是代码常量）。
--   - edge_methodologies 工具与 REST 读合并视图：内置 ∪ 自定义，内置的
--     enabled/priority 被覆盖行覆盖。

CREATE TABLE IF NOT EXISTS edge_methodologies (
  id             text PRIMARY KEY,
  source         text NOT NULL DEFAULT 'custom'
                 CHECK (source IN ('custom','builtin-override')),
  name           text NOT NULL DEFAULT '',
  tier           int  NOT NULL DEFAULT 2 CHECK (tier IN (1,2,3)),
  evidence_grade text NOT NULL DEFAULT 'standard'
                 CHECK (evidence_grade IN ('empirical','standard','metaphor')),
  summary        text NOT NULL DEFAULT '',
  criterion      jsonb,
  heuristic_tags jsonb NOT NULL DEFAULT '[]'::jsonb,
  enabled        bool NOT NULL DEFAULT true,
  priority       int  NOT NULL DEFAULT 0,
  created_at     timestamptz NOT NULL DEFAULT now(),
  updated_at     timestamptz NOT NULL DEFAULT now()
);

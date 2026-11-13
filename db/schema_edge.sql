-- 偏锋探索（方法论驱动探索线 + 自主终止协议）——与 db/schema.sql 并行应用的幂等 DDL。
--
-- 为什么单独成文件：db/schema.sql 是 RESERVED 文件（db/schema_loop.sql 同款纪律），
-- 由 db/edge_run.go 的 EnsureEdgeSchema 在启动期应用（幂等，重复应用零副作用）。
--
-- 两张表回答偏锋探索的记账面：
--   edge_cells —— 格点：42 格坐标系（控制点 × UCA 型等方法论维度）在单任务上的
--                 状态与判据进度。state: open → dispatched（已下发假设）→
--                 confirmed / disproven / folded（封格：attempts 达上限）。
--                 criterion_state 是判据进度 JSONB（已覆盖/应覆盖/剩余），挂在
--                 格点行上而不是运行表上——可审计性的前提：结束后要能回答
--                 「哪些控制点乘哪些 UCA 型已被覆盖、哪些还没有」。
--   edge_runs  —— 运行：一次偏锋运行的判据轨迹与终止原因。stop_reason 取值
--                 固定七类（见 StopReason* 常量）：
--                   doctrine_complete      主路径：方法论判据成立
--                   degenerate_hypothesis  兜底：连续假设退化熔断
--                   criterion_stalled      兜底：判据状态停滞熔断
--                   cell_exhausted         兜底：格点封顶熔断
--                   operator_guard         边界：人设最坏情况兜底
--                   cancelled / failed     运行异常
--                 统计口径上 doctrine_complete 与兜底类必须可区分——判据触发率
--                 是自主终止是否真实发生的唯一直接度量。
--
-- ⚠️ 语义边界（防运维混淆）：edge_cells.attempts 是【格点尝试数】（该格点被
-- 下发过几次假设），与 exploration_nodes.attempts（M4 的【意图认领计数】）是
-- 两个不同的计数，同名纯属巧合，互不引用。
-- ⚠️ 刻意不带外键到 exploration_nodes：格点/运行是循环的观测记账面，任务删除
-- 走归档通道，行随任务史保留（与 db/schema_loop.sql 同一取舍）。

-- ---------- edge_cells：偏锋格点 ----------
CREATE TABLE IF NOT EXISTS edge_cells (
    id              BIGSERIAL PRIMARY KEY,
    task_id         BIGINT NOT NULL,
    cell_id         TEXT NOT NULL,
    methodology_id  TEXT NOT NULL DEFAULT '',
    -- open → dispatched → confirmed / disproven / folded
    state           TEXT NOT NULL DEFAULT 'open'
                    CHECK (state IN ('open','dispatched','confirmed','disproven','folded')),
    -- 格点尝试数（该格点被下发过几次假设）——不是意图认领计数，见文件头。
    attempts        INTEGER NOT NULL DEFAULT 0,
    criterion_state JSONB NOT NULL DEFAULT '{}',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (task_id, cell_id)
);
CREATE INDEX IF NOT EXISTS idx_edge_cells_task ON edge_cells(task_id, state);

-- ---------- edge_runs：偏锋运行 ----------
CREATE TABLE IF NOT EXISTS edge_runs (
    id              BIGSERIAL PRIMARY KEY,
    task_id         BIGINT NOT NULL,
    methodology_id  TEXT NOT NULL DEFAULT '',
    criterion_trace JSONB NOT NULL DEFAULT '[]',
    stop_reason     TEXT NOT NULL DEFAULT '',
    stop_detail     JSONB NOT NULL DEFAULT '{}',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_edge_runs_task ON edge_runs(task_id, created_at DESC);

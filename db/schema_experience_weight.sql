-- 经验权重卡（X1，方案 v10 §6 第三期 T1）—— **与 db/schema.sql 并行应用**的幂等 DDL。
--
-- 为什么单独成文件：照 db/schema_memory.sql 的先例 —— 本单元的 DDL 由
-- db/experience_weight.go 的 EnsureExperienceWeightSchema 幂等补齐，schema 属主
-- 可以随时把内容并进 db/schema.sql —— 两边都是幂等的，重复应用无副作用。
--
-- 设计依据（方案 v10 §5.2 X1 / §6 T1）：
--
--   ① **一张表只存观测到的状态**：hits/wins 是只增计数器，prior 是派生值
--      （Laplace 平滑胜率），day_key/day_wins 是当日计胜的滚动窗口。全部是
--      【观测到的状态】，没有任何可热改的阈值列（§8 红线第 3 条：阈值进代码
--      常量，不进 DB/settings）。
--   ② **单一权威实现防 E6 双字面量**：prior 的公式只存在于
--      db/experience_weight.go 的 experienceLaplacePrior 一个函数里
--      —— prior = (wins_old + dw + 1) / (hits_old + 3)，dw∈{0,1}。
--      本表里的 prior 列只是该函数的**落库快照**（排序面读它省一次计算），
--      任何绕过访问层手写的 prior 都会在下一次记账时被重算覆盖。
--   ③ **因果校验在访问层**：win 必须通过词集因果校验（归因串与结论词集交集
--      ≥1）且当日计胜未达 cap；否则只记 hit。判定逻辑唯一实现在
--      ValidateExperienceAttribution / RecordExperienceOutcome，不在 SQL 里。
--   ④ **pattern_key 是调用方的键**：本层不解释它的语义（legacy_<id> /
--      memory_<id> 由上游约定），只保证唯一性与只增记账。

CREATE TABLE IF NOT EXISTS experience_weight (
    id          BIGSERIAL PRIMARY KEY,
    -- 归属的「打法」键：一条经验的权重卡。UNIQUE ⇒ 一键一行，记账是同行的
    -- wins/hits 只增（§5.2 X1：wins/hits 只增）。
    pattern_key TEXT NOT NULL UNIQUE,
    hits        BIGINT NOT NULL DEFAULT 0 CHECK (hits >= 0),
    wins        BIGINT NOT NULL DEFAULT 0 CHECK (wins >= 0),
    -- Laplace 平滑胜率的快照：(wins_old + dw + 1) / (hits_old + 3) 的当前值。
    -- 不给 DEFAULT：初值只能由访问层的单一公式算出并写入（防 E6 双字面量 ——
    -- 手写 INSERT 不带 prior 会被 NOT NULL 挡住，而不是静默拿到一个第二个初值）。
    -- 未命中回落：一次未计胜事件会把 prior 拉低（dw=0 代入同一公式）。
    prior       REAL NOT NULL CHECK (prior >= 0 AND prior <= 1),
    -- 每日计胜窗口：day_key = 当日（本地日期），day_wins = 当日已计胜次数。
    -- 超过 cap（代码常量 ExperienceDailyWinCap=3）的事件只记 hit 不计胜。
    day_key     TEXT NOT NULL DEFAULT '',
    day_wins    INT  NOT NULL DEFAULT 0 CHECK (day_wins >= 0),
    last_win_at TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

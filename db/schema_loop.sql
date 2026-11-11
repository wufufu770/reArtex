-- 奇点循环执行层（方案 v10 §12 + §16，批次 F2-F5）——与 db/schema.sql 并行应用的幂等 DDL。
--
-- 为什么单独成文件：db/schema.sql 由别的单元持有（RESERVED），本单元不改它。
-- 本文件由 db/looptrack.go 的 EnsureLoopSchema 幂等补齐（照 db/schema_memory.sql
-- 的 ensureSchema 模式：CREATE TABLE IF NOT EXISTS + CREATE INDEX IF NOT EXISTS，
-- 重复应用零副作用）。
--
-- 三张表回答方案 §16 五层奇点循环的执行侧记账：
--   intent_predictions —— 层①「许愿」的落点镜像：意图自带的三项可证伪预测
--       （predicted_vulnclass / predicted_actions / predicted_tools，F1 已进意图
--       payload）单独成行，让 F2 矛盾检测器可以「按意图 id 一次取预测」做确定性
--       diff，而不必解析每个意图的 payload JSON。以 intent_id 为主键 = 与探索图
--       一意图一行天然对齐，重复写是 no-op。
--   contradictions —— 层②「兑现对照」的输出：预测 vs 实际的矛盾（F2 确定性
--       diff，不靠模型自省）。resolved 标记人工裁决进度（F5：矛盾对应的提议在
--       strategy_proposals 被批准 ⇒ 置 resolved）。
--   gaps —— 层④「空缺驱动」的账本（F4）：认知/证据/动作/工具/验证/影响/汇报/
--       授权八类空缺（§16 层①分类）。open 的空缺可触发一次定向唤醒
--       （authorization 类除外——授权空缺必须停，只能由人补，见 server/gapwake.go）。
--
-- ⚠️ 与既有表的关系刻意**不带外键**：任务删除走归档通道（task_archives），意图
-- 节点有假删除（state='deleted'）；本层是循环的观测/记账面，行随任务史保留，
-- 级联删除反而会抹掉「这个任务曾经卡在哪」的痕迹。

-- ---------- intent_predictions：意图预测镜像（F1 写入，F2 消费） ----------
CREATE TABLE IF NOT EXISTS intent_predictions (
    intent_id           BIGINT PRIMARY KEY,
    predicted_vulnclass TEXT NOT NULL DEFAULT '',
    predicted_actions   JSONB NOT NULL DEFAULT '[]',
    predicted_tools     JSONB NOT NULL DEFAULT '[]',
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- ---------- contradictions：预测 vs 实际的矛盾（F2 写入，F3 路由，F5 消账） ----------
CREATE TABLE IF NOT EXISTS contradictions (
    id         BIGSERIAL PRIMARY KEY,
    task_id    BIGINT NOT NULL,
    intent_id  BIGINT NOT NULL DEFAULT 0,
    -- kind：evidence（证据/类别不符）/ path（路径走不通）/ tool（工具不合）/
    -- permission（越权被拦）/ time（时间预算）/ risk（风险口径）。
    kind       TEXT NOT NULL DEFAULT 'evidence',
    detail     TEXT NOT NULL DEFAULT '',
    resolved   BOOLEAN NOT NULL DEFAULT false,
    resolution TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_contradictions_task_resolved ON contradictions(task_id, resolved);

-- ---------- gaps：空缺账本（F4 写入/唤醒，F5 闭合） ----------
CREATE TABLE IF NOT EXISTS gaps (
    id               BIGSERIAL PRIMARY KEY,
    task_id          BIGINT NOT NULL,
    -- kind：§16 层①八类——cognitive/evidence/action/tool/verify/impact/report/
    -- authorization。
    kind             TEXT NOT NULL DEFAULT 'action',
    detail           TEXT NOT NULL DEFAULT '',
    status           TEXT NOT NULL DEFAULT 'open',
    opened_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    closed_at        TIMESTAMPTZ,
    closed_by_intent BIGINT,
    -- open 空缺的同一 (task, kind, detail) 只允许一行（F4 去重的库级保证；
    -- detail 在写入侧截断到 500 字符内，避免超 btree 索引行上限）。
    CONSTRAINT ck_gaps_status CHECK (status IN ('open', 'closed'))
);
CREATE INDEX IF NOT EXISTS idx_gaps_task_status ON gaps(task_id, status);
CREATE UNIQUE INDEX IF NOT EXISTS uq_gaps_open ON gaps(task_id, kind, detail) WHERE status = 'open';

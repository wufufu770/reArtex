-- finding 级验证态 + 返工边（幂等 DDL，由 db/finding_validation.go 应用）
--
-- 背景：finding 节点**落库即 confirmed** ⇒ 「提出」被当成了「验证」，任何人都
-- 看不出这条结论有没有被独立复核过；而且复核不通过时没有退路（既不能关掉，
-- 也不能退回给提出它的人）。
--
-- 本文件做什么：
--   ① finding 节点三态：pending（已提出，未验证）/ confirmed（验证通过，结案）
--      / dismissed（不成立，结案）。
--   ② 探索边加一个关系 rework：finding --rework--> 产出它的那条 intent，
--      表示「这条结论被打回给谁」。
--
-- 为什么返工用**边**而不是第四个状态：
--   - 状态回答「当前处于哪一步」，返工回答「这次打回给谁」—— 后者是**一次动作**，
--     可以发生多次（打回 → 补充 → 再打回），状态机表达不了。
--   - 血缘边 derived_from 在建 finding 时**已经存在**（finding→intent），
--     而边的主键是 (exploration_id, src_id, rel, dst_id) ⇒ 再插一条 derived_from
--     会被 ON CONFLICT DO NOTHING 变成 no-op，返工信息会静默丢失。
--
-- ⚠️ db/schema.sql 由别的单元持有（RESERVED）⇒ 约束的重建走「DROP + ADD」。
-- 而且**必须逐字保留其他 kind 的原有规则**，只改 finding / edge 两处 ——
-- 重建 CHECK 时手抖改掉别的 kind 的取值，等于悄悄放宽/收紧了一大片既有数据。
--
-- 幂等：两个约束都用 DO 块先读 pg_get_constraintdef，比对当前定义；
-- 已经是要��的样子就什么都不做。所以重复应用安全，且不会因为「已存在」而漏改。

-- ---------- ① finding 节点三态 ----------
DO $$
DECLARE
    curdef TEXT;
BEGIN
    SELECT pg_get_constraintdef(oid) INTO curdef
      FROM pg_constraint
     WHERE conname = 'ck_node_state'
       AND conrelid = 'exploration_nodes'::regclass;

    IF curdef IS NULL THEN
        ALTER TABLE exploration_nodes ADD CONSTRAINT ck_node_state CHECK (
            (kind='begin'   AND state IN ('open')) OR
            (kind='intent'  AND state IN ('open','running','paused','done','blocked','exhausted','stopped','deleted')) OR
            (kind='goal'    AND state IN ('open','met','abandoned')) OR
            (kind='fact'    AND state IN ('confirmed','dismissed','origin')) OR
            (kind='finding' AND state IN ('pending','confirmed','dismissed')) OR
            (kind='hint'    AND state IN ('active','consumed')) OR
            (kind='digest'  AND state IN ('active','superseded'))
        );
    ELSIF curdef NOT LIKE '%pending%' THEN
        -- 只有 finding 分支新增了 'pending'，所以 'pending' 在本约束里只可能来自
        -- finding 分支 ⇒ 用它做判据不会误伤其他 kind。
        ALTER TABLE exploration_nodes DROP CONSTRAINT ck_node_state;
        ALTER TABLE exploration_nodes ADD CONSTRAINT ck_node_state CHECK (
            (kind='begin'   AND state IN ('open')) OR
            (kind='intent'  AND state IN ('open','running','paused','done','blocked','exhausted','stopped','deleted')) OR
            (kind='goal'    AND state IN ('open','met','abandoned')) OR
            (kind='fact'    AND state IN ('confirmed','dismissed','origin')) OR
            (kind='finding' AND state IN ('pending','confirmed','dismissed')) OR
            (kind='hint'    AND state IN ('active','consumed')) OR
            (kind='digest'  AND state IN ('active','superseded'))
        );
    END IF;
END $$;

-- ---------- ② 返工边 ----------
DO $$
DECLARE
    curdef TEXT;
BEGIN
    SELECT pg_get_constraintdef(oid) INTO curdef
      FROM pg_constraint
     WHERE conname = 'ck_edge_rel'
       AND conrelid = 'exploration_edges'::regclass;

    IF curdef IS NULL THEN
        ALTER TABLE exploration_edges ADD CONSTRAINT ck_edge_rel CHECK (
            rel IN ('spawns','derived_from','yields','proves','covers','rework')
        );
    ELSIF curdef NOT LIKE '%rework%' THEN
        ALTER TABLE exploration_edges DROP CONSTRAINT ck_edge_rel;
        ALTER TABLE exploration_edges ADD CONSTRAINT ck_edge_rel CHECK (
            rel IN ('spawns','derived_from','yields','proves','covers','rework')
        );
    END IF;
END $$;

-- CVE 反向匹配 hint 去重台账（B-04）——与 db/schema.sql 并行应用的幂等 DDL。
--
-- 为什么单独成文件：db/schema.sql 是 RESERVED 文件（见 db/schema_biz.sql 同款
-- 纪律），本单元的新表走独立幂等文件，由 db/cve_match.go 的 EnsureCVEMatchSchema
-- 在访问层首次使用时应用（CREATE TABLE IF NOT EXISTS，重复应用零副作用）。
--
-- cve_hints 回答「同一（任务，资产，CVE）只挂一次 hint」（原卡 B-04 去重要求）：
--   - uq_cve_hints_triple 唯一约束是幂等闸：重跑同步 / 并发批次重复产出时
--     INSERT ON CONFLICT DO NOTHING 静默收敛，绝不出第二行；
--   - verdict CHECK 只收 affected/maybe：not_affected 在匹配层（B-04C）已被剔除，
--     数据层再封一道——「不判定为受影响」的记录绝不可能以 hint 名义存在；
--   - node_id 是 hint 发出后的探索节点 ID（B-04F 发出成功才落行——发出失败的
--     批次不留占位，下批重试；快速路径 HasCVEHint + 唯一闸双保险）；
--   - task_id/asset_id 故意不挂外键：这是去重台账不是关系图，任务/资产的删除
--     清理顺序不受它牵制（孤儿行只是永远不会再次命中的键，无读路径消费）。
CREATE TABLE IF NOT EXISTS cve_hints (
    id         BIGSERIAL PRIMARY KEY,
    task_id    BIGINT      NOT NULL,
    asset_id   BIGINT      NOT NULL,
    cve_id     TEXT        NOT NULL,
    verdict    TEXT        NOT NULL,
    node_id    BIGINT      NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT uq_cve_hints_triple UNIQUE (task_id, asset_id, cve_id),
    CONSTRAINT ck_cve_hints_verdict CHECK (verdict IN ('affected', 'maybe'))
);
CREATE INDEX IF NOT EXISTS idx_cve_hints_task ON cve_hints(task_id);

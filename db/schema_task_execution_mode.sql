-- C-02B 任务资源模式覆盖（execution_mode）。
--
-- 为什么单独成文件：db/schema.sql 是 RESERVED。scripts/db-role-bootstrap.sh
-- 会按 db/schema_*.sql 幂等应用本文件；启动期再由
-- db.EnsureTaskExecutionModeSchema 用迁移连接补一次。
--
-- 这是 C-02 的资源模式覆盖（fast / balanced / super_heavy），不是 0.15.17
-- 执行档的 difficulty（auto/simple/medium/hard，只调算力、不落本列），
-- 也不是 C-01 的任务难度（easy/medium/hard/expert，在 difficulty 列）。
--
-- 空串是合法值，意思是「跟随任务难度的默认模式」，不是把默认模式写死在列里。
-- 难度以后改了，空列仍然跟新难度走。非法值由 CHECK 拒绝，不改写成某一档。
--
-- CHECK 不能靠 ADD COLUMN IF NOT EXISTS 顺带幂等：列已在时整句跳过，
-- 约束不会补上。所以列与命名约束分开，约束先 DROP 再 ADD（启动迁移锁内）。

ALTER TABLE tasks ADD COLUMN IF NOT EXISTS execution_mode TEXT NOT NULL DEFAULT '';

ALTER TABLE tasks DROP CONSTRAINT IF EXISTS ck_tasks_execution_mode;
ALTER TABLE tasks ADD CONSTRAINT ck_tasks_execution_mode
    CHECK (execution_mode IN ('', 'fast', 'balanced', 'super_heavy'));

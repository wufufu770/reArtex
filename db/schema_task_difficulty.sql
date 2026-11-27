-- C-01A 任务难度列（difficulty + difficulty_reason）。
--
-- 为什么单独成文件：db/schema.sql 是 RESERVED（B-09 / TI 等同款纪律）。
-- scripts/db-role-bootstrap.sh 会按 db/schema_*.sql 幂等应用本文件；
-- 启动期再由 db.EnsureTaskDifficultySchema 用迁移连接补一次。
--
-- 这是任务难度（easy/medium/hard/expert），不是 0.15.17 执行档位的
-- difficulty（auto/simple/medium/hard，只调算力、不落本列）。
-- 存量任务靠 DEFAULT 'medium' / 理由空串补齐，不改写任何历史意图。
--
-- CHECK 不能靠 ADD COLUMN IF NOT EXISTS 顺带幂等：列已在时整句跳过，
-- 约束不会补上。所以列与命名约束分开，约束先 DROP 再 ADD（启动迁移锁内）。

ALTER TABLE tasks ADD COLUMN IF NOT EXISTS difficulty TEXT NOT NULL DEFAULT 'medium';
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS difficulty_reason TEXT NOT NULL DEFAULT '';

ALTER TABLE tasks DROP CONSTRAINT IF EXISTS ck_tasks_difficulty;
ALTER TABLE tasks ADD CONSTRAINT ck_tasks_difficulty
    CHECK (difficulty IN ('easy', 'medium', 'hard', 'expert'));

CREATE INDEX IF NOT EXISTS idx_tasks_difficulty ON tasks(difficulty) WHERE deleted_at IS NULL;

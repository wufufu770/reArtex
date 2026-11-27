-- C-03B 任务评级（结束时确定性计算的落库）。
--
-- 为什么单独成文件：db/schema.sql 是 RESERVED。scripts/db-role-bootstrap.sh
-- 会按 db/schema_*.sql 幂等应用本文件；启动期再由
-- db.EnsureTaskGradeSchema 用迁移连接补一次。
--
-- grade 是 S/A/B/C。空串 = 还没评，不是一个等级。grade_summary 是固定模板
-- 的一句话摘要，空串 = 还没评。grade_score 未评时是 NULL（不能用 0：0 分是
-- 评过的 C）。三列同一次 UPDATE 写入。
--
-- 列类型按卡面用 REAL（float4）。分数在 Go 里按 float64 计算，读回以库里的
-- 值为准；两次查询读的是同一列，结果一致。
--
-- CHECK 不能靠 ADD COLUMN IF NOT EXISTS 顺带幂等：列已在时整句跳过，
-- 约束不会补上。所以列与命名约束分开，约束先 DROP 再 ADD（启动迁移锁内）。

ALTER TABLE tasks ADD COLUMN IF NOT EXISTS grade TEXT NOT NULL DEFAULT '';
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS grade_summary TEXT NOT NULL DEFAULT '';
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS grade_score REAL;

ALTER TABLE tasks DROP CONSTRAINT IF EXISTS ck_tasks_grade;
ALTER TABLE tasks ADD CONSTRAINT ck_tasks_grade
    CHECK (grade IN ('', 'S', 'A', 'B', 'C'));

-- 未评 = 空等级且分数 NULL。评过 = 等级非空且分数非 NULL。禁止只写一半。
ALTER TABLE tasks DROP CONSTRAINT IF EXISTS ck_tasks_grade_score;
ALTER TABLE tasks ADD CONSTRAINT ck_tasks_grade_score
    CHECK (
        (grade = '' AND grade_score IS NULL)
        OR (grade <> '' AND grade_score IS NOT NULL)
    );

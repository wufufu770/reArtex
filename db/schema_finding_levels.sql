-- C-04A 漏洞三维分级的数据层。
--
-- 为什么单独成文件：db/schema.sql 是 RESERVED。scripts/db-role-bootstrap.sh
-- 会按 db/schema_*.sql 幂等应用本文件；启动期再由
-- db.EnsureFindingLevelsSchema 用迁移连接补一次。
--
-- impact_level：L1 信息 / L2 认证·越权 / L3 控制 / L4 数据·资金。
-- evidence_level：L1 推断 / L2 复现 / L3 完整证据链 / L4 独立复核通过。
-- 两列都是 1–4。存量行靠 DEFAULT 1 补齐，不按证据正文追溯升级。
--
-- evidence_level_requested 记下 agent 自报的证据等级（省略按 1）。
-- evidence_level 是系统核过上限之后真正落下的等级。两者不一致时
-- evidence_level_reason 写明降级原因，避免自报被静默改掉。
-- 卡面只点名前两列；后两列是为了「降级要留原因」和「复测通过后按自报回升」。
--
-- CHECK 不能靠 ADD COLUMN IF NOT EXISTS 顺带幂等：列已在时整句跳过，
-- 约束不会补上。所以列与命名约束分开，约束先 DROP 再 ADD（启动迁移锁内）。

ALTER TABLE findings ADD COLUMN IF NOT EXISTS impact_level SMALLINT NOT NULL DEFAULT 1;
ALTER TABLE findings ADD COLUMN IF NOT EXISTS evidence_level SMALLINT NOT NULL DEFAULT 1;
ALTER TABLE findings ADD COLUMN IF NOT EXISTS evidence_level_requested SMALLINT NOT NULL DEFAULT 1;
ALTER TABLE findings ADD COLUMN IF NOT EXISTS evidence_level_reason TEXT NOT NULL DEFAULT '';

ALTER TABLE findings DROP CONSTRAINT IF EXISTS ck_findings_impact_level;
ALTER TABLE findings ADD CONSTRAINT ck_findings_impact_level
    CHECK (impact_level BETWEEN 1 AND 4);

ALTER TABLE findings DROP CONSTRAINT IF EXISTS ck_findings_evidence_level;
ALTER TABLE findings ADD CONSTRAINT ck_findings_evidence_level
    CHECK (evidence_level BETWEEN 1 AND 4);

ALTER TABLE findings DROP CONSTRAINT IF EXISTS ck_findings_evidence_level_requested;
ALTER TABLE findings ADD CONSTRAINT ck_findings_evidence_level_requested
    CHECK (evidence_level_requested BETWEEN 1 AND 4);

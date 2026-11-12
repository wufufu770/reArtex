-- 报告中心（report center）的持久化 —— 与 db/schema.sql 并行应用的幂等 DDL。
--
-- 为什么单独成文件：db/schema.sql 是 RESERVED 文件（别的单元在同一 worktree 内并行
-- 开发），本单元不改它。本文件由 db/reports.go 的 EnsureReportsSchema 幂等补齐。
--
-- 口径：报告中心存【最终报告的正文】（TEXT 整篇存库），不存文件路径——工作空间里
-- 的原始文件仍归工作空间管；导入时把正文拷贝进来，两边从此各自独立（导出/下载
-- 从库里出，与工作空间下载互不影响）。
CREATE TABLE IF NOT EXISTS reports (
	id          BIGSERIAL PRIMARY KEY,
	title       TEXT NOT NULL,
	task_id     BIGINT,             -- 关联任务（可空：手工创建/无任务归属）
	source_path TEXT,               -- 导入来源（工作空间相对路径；可空）
	content     TEXT NOT NULL DEFAULT '',
	created_by  TEXT NOT NULL DEFAULT 'operator',
	created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
	updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_reports_updated ON reports (updated_at DESC);

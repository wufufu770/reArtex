-- 会话单循环硬锁 + 升级提议（0.15.44，#12 single_loop 裁决）——与 db/schema.sql
-- 并行应用的幂等 DDL（schema.sql 是 RESERVED 文件，本单元不改它；db/escalation.go
-- 的 EnsureSelfLoopSchema 幂等补齐）。
--
-- 语义：single_loop='on' 的会话，工具面确定性剥离 spawn_task（agent 物理不可
-- 建任务）；升级走 propose_task_escalation —— LLM 只写提议（pending），人工
-- 批准后端才用 payload 确定性建任务。既有会话（recon 派任务等设计能力）不受
-- 影响：锁是会话粒度 opt-in。

ALTER TABLE conversations ADD COLUMN IF NOT EXISTS single_loop TEXT NOT NULL DEFAULT 'off';

CREATE TABLE IF NOT EXISTS task_escalation_proposals (
	id          BIGSERIAL PRIMARY KEY,
	session_id  BIGINT,               -- 发起会话（chat 提议时回填；任务运行面为空）
	agent_key   TEXT NOT NULL DEFAULT '',  -- 提议者 agent
	task_ref    TEXT,                 -- 可选：提议挂靠的任务 id
	title       TEXT NOT NULL,        -- 提议的任务描述（= spawn description）
	goal        TEXT NOT NULL,        -- 提议的目标
	rationale   TEXT NOT NULL DEFAULT '',   -- 为什么需要升级成独立任务
	payload     JSONB NOT NULL DEFAULT '{}'::jsonb,  -- 原始 spawn 参数（批准时确定性重放）
	status      TEXT NOT NULL DEFAULT 'pending'
	            CHECK (status IN ('pending','approved','rejected','converted')),
	proposed_by TEXT NOT NULL DEFAULT '',
	created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
	decided_at  TIMESTAMPTZ,
	task_id     TEXT                  -- 批准后落成的任务号
);

CREATE INDEX IF NOT EXISTS idx_escalation_status ON task_escalation_proposals (status, created_at DESC);

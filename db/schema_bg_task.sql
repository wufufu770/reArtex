-- 后台命令的账本（bg_task）—— **与 db/schema.sql 并行应用**的幂等 DDL。
--
-- 为什么单独成文件：db/schema.sql 由别的单元在同一 worktree 内并行开发，
-- 本单元不改它（RESERVED）。本文件由 db/bg_task.go 的 EnsureBgTaskSchema
-- 幂等补齐（见该文件注释），schema 属主可以随时把内容并进 db/schema.sql。
--
-- 为什么不复用 activity 表（它的 kind 是自由文本，本来塞得下一个 background_cmd）：
--   1. activity 是**事件流**（append-only，一个动作一行）；后台命令的生命周期是
--      **可变状态**（running → done / failed / lost）。用事件流表达就得靠
--      「最后一条决定当前状态」的折叠规则，规则一坏就出现两条 running。
--   2. lost 与 failed 的区别是本设计的核心（"它被回收了" ≠ "它失败了"）。activity
--      行不带这种状态语义，谁都能往里补一行 kind='done'，区分就没了。
--   3. 回收者要做的是「扫出所有还在 running 且已过期的行」——索引查询。在事件流上
--      折叠出这个集合要拉全表再在内存里折叠。
--   4. activity 的 kind 词表被 planner / UI / progress receipt 共用，塞进
--      background_cmd 会污染那些读方。
-- 所以：新增一张小表，专表专用。
--
-- ⚠️ 故意**不**加 owner_task_id → tasks(id) 的外键：任务被删时，这一行必须活下来，
-- 否则回收者就再也拿不到 pid，那个还活着的后台进程会变成没人管的孤儿进程。

CREATE TABLE IF NOT EXISTS bg_task (
    id              BIGSERIAL PRIMARY KEY,
    -- 拥有它的 ARTEX 任务 id（tasks.id）。
    owner_task_id   BIGINT NOT NULL DEFAULT 0,
    -- 宿主给的 owner key（"task:<id>"）：跨 session 的稳定句柄。
    owner_key       TEXT NOT NULL DEFAULT '',
    -- SDK 侧的 session id + 任务 id：SDK 的 task id 每个 session 都从 task_1 重新
    -- 开始，只有 (session_id, sdk_task_id) 才是跨 session 唯一的句柄。
    session_id      TEXT NOT NULL DEFAULT '',
    sdk_task_id     TEXT NOT NULL DEFAULT '',
    command         TEXT NOT NULL DEFAULT '',
    -- 输出文件的绝对路径。宿主目录（ARTEX_TASK_DIR）下的 <session>/tasks/<id>.output。
    output_path     TEXT NOT NULL DEFAULT '',
    -- 进程 id：回收者按**进程组**杀（-pid），跨进程也成立（同一台机器上的另一个
    -- ARTEX 实例也能收）。
    pid             INTEGER NOT NULL DEFAULT 0,
    -- running / done / failed / lost。
    --   done   = 命令自己跑完了（退出码可能是 0 也可能非 0，exit_code 里记着）。
    --   failed = 命令自己跑完了但退出码非 0。
    --   lost   = 被回收者收走了（任务到点 / TTL 到期）—— 它不是"失败"，是"没人要它了"。
    state           TEXT NOT NULL DEFAULT 'running'
                      CHECK (state IN ('running','done','failed','lost')),
    exit_code       INTEGER,
    -- 提醒正文：完成时写，内容是"这条命令结束了，输出在哪儿"。
    notice          TEXT NOT NULL DEFAULT '',
    -- 拥有者任务的绝对 deadline（unix 秒）；0 = 该任务没有 deadline。
    deadline_unix   BIGINT NOT NULL DEFAULT 0,
    -- TTL 兜底秒数：started_at + ttl 到期即回收。0 = 不设 TTL（不推荐）。
    ttl_seconds     INTEGER NOT NULL DEFAULT 0,
    started_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    ended_at        TIMESTAMPTZ,
    -- 提醒投递时间：非空 = 完成提醒已经投出去了。
    notified_at     TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 同一 session 里的同一个 SDK 任务只登记一次（登记重试是幂等的）。
CREATE UNIQUE INDEX IF NOT EXISTS uq_bg_task_session_task ON bg_task(session_id, sdk_task_id);
-- 跨 session 注册表的主查询：某任务名下有哪些后台命令。
CREATE INDEX IF NOT EXISTS idx_bg_task_owner ON bg_task(owner_task_id, started_at DESC);
-- 回收者的主查询：还在 running 的行（部分索引，只覆盖 running）。
CREATE INDEX IF NOT EXISTS idx_bg_task_running ON bg_task(state) WHERE state = 'running';
-- 兜底扫描（按开始时间找最老的那批）。
CREATE INDEX IF NOT EXISTS idx_bg_task_state_started ON bg_task(state, started_at);

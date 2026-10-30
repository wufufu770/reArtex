-- 群组预算（token 额度）数据模型 —— **与 db/schema.sql 并行应用**的幂等 DDL。
--
-- 为什么单独成文件：db/schema.sql 由别的单元在同一 worktree 内并行开发，本单元
-- 不改它。本文件由 db/quota.go 的 EnsureQuotaSchema 在 db.Open 之后补齐
-- （见该文件注释），schema 属主可以随时把内容并进 db/schema.sql —— 两边
-- 都是幂等的，重复应用无副作用。
--
-- 四张表：
--   group_quotas            群组预算本体：一个群一行（预算、周期、状态）
--   group_quota_periods     周期计数行：按（群组, 周期键）分桶的已用/预扣 token
--   group_quota_reservations 预扣明细：reserve → settle / release 的三段式
--   group_quota_refusals    拒绝留痕：预算不够/状态不允许时"这次调用没发出去"
--
-- 状态取值（active / draining / parked）与周期取值（daily）的**权威定义在 Go
-- 代码常量里**（db/quota.go 的 QuotaState* / QuotaPeriod*）。下面的 CHECK 只是
-- 镜像：它挡住脏数据，但不新增取值 —— DB 里没有任何一张表可以"新增一个状态"，
-- 状态机因此是代码常量，而不是可被 agent 改写的规则行。

-- ---------- group_quotas ----------
-- 一个群一条预算。群组本身由 chat_groups 定义（db/schema_chat_group.sql），
-- 这里只挂预算面：预算额度、周期模型、状态机。
--
-- budget_tokens = 0 的语义是「**没有可用预算**」：预扣一律拒绝（fail closed）。
-- 「不限制」在本平台不存在 —— 没有额度行、额度为 0、额度行查不出来，三种情况
-- 一律按拒绝处理（见 db/quota.go 的 ReserveGroupQuota）。
CREATE TABLE IF NOT EXISTS group_quotas (
    group_id      BIGINT PRIMARY KEY REFERENCES chat_groups(id) ON DELETE CASCADE,
    -- 预算 token 数（口径：input + output，缓存已含在 input 里，不重复计）。
    budget_tokens BIGINT NOT NULL DEFAULT 0 CHECK (budget_tokens >= 0),
    -- 周期模型：目前只有「自然日」一种（见 db/quota.go 的 QuotaPeriodDaily 注释）。
    period_kind   TEXT NOT NULL DEFAULT 'daily' CHECK (period_kind = 'daily'),
    -- 状态机：active（正常）/ draining（不再发起新调用，在途可结算）/ parked（完全停下）。
    state         TEXT NOT NULL DEFAULT 'active'
                  CHECK (state IN ('active','draining','parked')),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_group_quotas_state ON group_quotas(state);
DROP TRIGGER IF EXISTS trg_group_quotas_upd ON group_quotas;
CREATE TRIGGER trg_group_quotas_upd BEFORE UPDATE ON group_quotas
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- ---------- group_quota_periods ----------
-- 周期计数行：预扣与结算的**原子闸门**就落在这张表上。
--
-- 为什么按周期分桶而不是在 group_quotas 上放一列 consumed：跨周期时旧周期的
-- 在途预扣仍可能回来结算，若共用一列，旧周期的结算会被记到新周期头上。分桶后
-- 每个周期各自独立记账，跨周期结算只会落回它被预扣的那个周期。
--
-- 口径：consumed + reserved 是这个周期已经占用的 token。预扣时的守卫条件是
-- consumed + reserved + 本次预扣 <= budget（budget 在 group_quotas 上）。
CREATE TABLE IF NOT EXISTS group_quota_periods (
    group_id        BIGINT NOT NULL REFERENCES group_quotas(group_id) ON DELETE CASCADE,
    -- 周期键：Asia/Shanghai 自然日，形如 2026-10-30（见 db/quota.go 的 QuotaPeriodKey）。
    period_key      TEXT NOT NULL,
    consumed_tokens BIGINT NOT NULL DEFAULT 0 CHECK (consumed_tokens >= 0),
    reserved_tokens BIGINT NOT NULL DEFAULT 0 CHECK (reserved_tokens >= 0),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (group_id, period_key)
);

-- ---------- group_quota_reservations ----------
-- 预扣明细：reserve → settle / release 三段式。
--
-- request_id 唯一 ⇒ 同一个模型请求不会被预扣两次（重试不双扣）。
-- status 只有三个终态：held（在途）/ settled（已结算）/ released（已释放）。
--
-- reserved_input_tokens / reserved_output_tokens 是**拆分后的估算**：请求已经
-- 发出但没有 usage 返回时（流断了、供应商报错），按输入估算结算而不是记 0 ——
-- 供应商已经计了输入侧，记 0 等于白嫖计费窗口。
CREATE TABLE IF NOT EXISTS group_quota_reservations (
    id                     BIGSERIAL PRIMARY KEY,
    group_id               BIGINT NOT NULL REFERENCES group_quotas(group_id) ON DELETE CASCADE,
    period_key             TEXT NOT NULL,
    request_id             TEXT NOT NULL UNIQUE,
    reserved_tokens        BIGINT NOT NULL CHECK (reserved_tokens > 0),
    reserved_input_tokens  BIGINT NOT NULL DEFAULT 0 CHECK (reserved_input_tokens >= 0),
    reserved_output_tokens BIGINT NOT NULL DEFAULT 0 CHECK (reserved_output_tokens >= 0),
    status                 TEXT NOT NULL DEFAULT 'held'
                           CHECK (status IN ('held','settled','released')),
    -- dispatched = true 表示请求确实发出去了 → 失败时按输入估算结算而非释放。
    dispatched             BOOLEAN NOT NULL DEFAULT false,
    -- 归属：这次预扣属于哪个 task / 会话 / 群组。
    task_id                TEXT NOT NULL DEFAULT '',
    conversation_id        BIGINT,
    -- 孤儿清扫器靠它找过期未结的预扣（TTL 到期即回收）。
    expires_at             TIMESTAMPTZ NOT NULL,
    -- 结算后的真实用量（结算前为 0）。
    settled_input_tokens   BIGINT NOT NULL DEFAULT 0 CHECK (settled_input_tokens >= 0),
    settled_output_tokens  BIGINT NOT NULL DEFAULT 0 CHECK (settled_output_tokens >= 0),
    settled_total_tokens   BIGINT NOT NULL DEFAULT 0 CHECK (settled_total_tokens >= 0),
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- 清扫器只扫在途且过期的行：部分索引让这条查询不碰终态行。
CREATE INDEX IF NOT EXISTS idx_gqr_held ON group_quota_reservations(expires_at)
    WHERE status = 'held';
CREATE INDEX IF NOT EXISTS idx_gqr_group ON group_quota_reservations(group_id, created_at DESC);

-- ---------- group_quota_refusals ----------
-- 拒绝留痕：一次模型调用因为预算/状态被拒时写一行。
--
-- 为什么单独一张表而不是复用预扣表：被拒绝的调用**根本没有预扣**（预扣失败，
-- 计数没动），把它塞进预扣表会让"预扣 = 0"的行混进在途集合，清扫器与对账都要
-- 额外排除它。独立成表后，"这次调用没发出去"是一等事实。
--
-- reason 是**机器可读的原因码**（budget_exceeded / state_not_active /
-- no_budget / check_failed / estimate_failed），不带任何额度数字：额度数字只
-- 留在账上（group_quotas / group_quota_periods），拒绝文案里不出现。
CREATE TABLE IF NOT EXISTS group_quota_refusals (
    id         BIGSERIAL PRIMARY KEY,
    group_id   BIGINT NOT NULL REFERENCES group_quotas(group_id) ON DELETE CASCADE,
    period_key TEXT NOT NULL DEFAULT '',
    request_id TEXT NOT NULL DEFAULT '',
    reason     TEXT NOT NULL DEFAULT '',
    task_id    TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_gqr_refusal_group ON group_quota_refusals(group_id, created_at DESC);

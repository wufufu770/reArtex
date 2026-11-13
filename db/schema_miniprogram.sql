-- 小程序渗透（token 池）——与 db/schema.sql 并行应用的幂等 DDL。
--
-- 为什么单独成文件：db/schema.sql 是 RESERVED 文件（db/schema_loop.sql 同款纪律），
-- 由 db/miniprogram_tokens.go 的 EnsureMiniprogramSchema 在启动期应用（幂等，重复
-- 应用零副作用）。
--
-- 一张表回答方案二 §5 的凭据治理：bearer token 按任务/应用/角色标签作用域绑定，
-- 落库即加密（db/crypto + config.MasterKey；未配主密钥时明文过渡列，与 llm_profiles
-- 的 api_key/api_key_cipher 同款取舍并如实声明）。transcript 与审计面只见标签与
-- 指纹；明文只在「使用时刻」经显式 reveal 进入工具结果（构造 Burp 重放的
-- replacement 时不可避免，见 SKILL.md 的证据纪律）。
--
-- 唯一键 (task_id, app_id, label)：同任务同应用的同角色 token 只留一份，
-- 重复保存是轮换（UPDATE 值与指纹）。

CREATE TABLE IF NOT EXISTS miniprogram_tokens (
    id          BIGSERIAL PRIMARY KEY,
    task_id     BIGINT REFERENCES tasks(id) ON DELETE CASCADE,
    app_id      TEXT NOT NULL,
    label       TEXT NOT NULL,
    token_plain TEXT NOT NULL DEFAULT '',
    token_cipher TEXT NOT NULL DEFAULT '',
    fingerprint TEXT NOT NULL DEFAULT '',
    scope       TEXT NOT NULL DEFAULT 'task' CHECK (scope IN ('task','company')),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (task_id, app_id, label)
);
CREATE INDEX IF NOT EXISTS idx_mptokens_task ON miniprogram_tokens(task_id, app_id);

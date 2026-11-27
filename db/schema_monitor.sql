-- Monitor 仓库监测两张表（B-09B）——与 db/schema.sql 并行应用的幂等 DDL。
--
-- 为什么单独成文件：db/schema.sql 是 RESERVED 文件（见 db/schema_cve_match.sql
-- 同款纪律），本单元的新表走独立幂等文件，由 db/monitor.go 的 EnsureMonitorSchema
-- 在访问层首次使用时应用（CREATE TABLE IF NOT EXISTS，重复应用零副作用，
-- 不改任何既有表）。
--
-- monitor_repos 回答「监测哪些仓库、游标走到哪」（原卡 B-09 配置面）：
--   - uq_monitor_repos_url_branch：同仓库同分支只配一条（重复配置静默收敛为
--     更新，绝不重置 last_seen_sha 游标）；
--   - auth_ref 是凭据引用（opaque reference），绝不存 token 明文——GitHub
--     token 的既有凭据管理走 db/crypto.go AES-256-GCM 加密存储（B-09A 核实
--     结论：config.MasterKey() + encryptSecret，miniprogram_tokens.go 同款
--     先例；B-09C CRUD 接线时 token 经加密列落库，本表永远只有引用串）；
--   - github_base_url 支持自建 GitHub Enterprise（默认官方 api.github.com）；
--   - last_seen_sha 是拉取游标（40 位小写十六进制 commit sha；空串=尚未拉取，
--     首轮从 HEAD 起步）。
--
-- monitor_findings 回答「哪个提交被 Monitor 判为安全更新」（原卡 B-09 发现面）：
--   - uq_monitor_findings_repo_sha：同一提交同一仓库只产一条发现（B-09B 卡
--     要求的存储层最小去重闸；模型层重判/重跑幂等收敛靠它）；
--   - status 白名单含 pending_review（B-09B 子卡在原卡 new/reproduced/ignored
--     基础上加列：Monitor 模型失败/无法判断时落 pending_review——fail-closed，
--     「模型没判出来」绝不静默等价于「不是安全更新」）；
--   - repo_id 外键 ON DELETE RESTRICT（与原卡 CASCADE 草案的裁决差异，如实
--     申报）：findings 是模型判定与人工处置（复现/忽略+原因）的审计历史，
--     删仓库连带清空处置历史违背仓库审计纪律（db/audit_chain.go 同源精神）——
--     删除仓库前必须先显式处置其 findings（RESTRICT 使隐式清史不可达）。
CREATE TABLE IF NOT EXISTS monitor_repos (
    id              BIGSERIAL PRIMARY KEY,
    repo_url        TEXT        NOT NULL,
    branch          TEXT        NOT NULL DEFAULT 'main',
    github_base_url TEXT        NOT NULL DEFAULT 'https://api.github.com',
    auth_ref        TEXT        NOT NULL DEFAULT '',
    enabled         BOOLEAN     NOT NULL DEFAULT true,
    last_seen_sha   TEXT        NOT NULL DEFAULT '',
    created_by      TEXT        NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT uq_monitor_repos_url_branch UNIQUE (repo_url, branch)
);
CREATE INDEX IF NOT EXISTS idx_monitor_repos_enabled ON monitor_repos(enabled);

CREATE TABLE IF NOT EXISTS monitor_findings (
    id         BIGSERIAL PRIMARY KEY,
    repo_id    BIGINT      NOT NULL REFERENCES monitor_repos(id) ON DELETE RESTRICT,
    sha        TEXT        NOT NULL,
    title      TEXT        NOT NULL,
    cve_id     TEXT        NOT NULL DEFAULT '',
    ghsa_id    TEXT        NOT NULL DEFAULT '',
    status     TEXT        NOT NULL DEFAULT 'new',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT ck_monitor_findings_status CHECK (status IN ('new', 'pending_review', 'reproduced', 'ignored')),
    CONSTRAINT uq_monitor_findings_repo_sha UNIQUE (repo_id, sha)
);
CREATE INDEX IF NOT EXISTS idx_monitor_findings_status ON monitor_findings(status);
CREATE INDEX IF NOT EXISTS idx_monitor_findings_repo ON monitor_findings(repo_id);

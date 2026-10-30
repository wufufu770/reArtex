-- =====================================================================
-- artex_agent 只读角色：角色本体（幂等）
-- =====================================================================
--
-- 由 agent-readonly-role.sh 在**设口令之前**调用 —— 顺序有讲究：
--   psql 的 \password 要求角色已经存在，否则设不了口令。
--   所以流程固定为：建角色 → 设口令 → 授权。
--
-- 角色属性为什么是这一套：
--   NOSUPERUSER   不能提权
--   NOCREATEDB    不能建库
--   NOCREATEROLE  不能改角色（不能把自己塞进别的角色里）
--   NOINHERIT     **刻意的**：万一日后有人误 GRANT artex_app TO artex_agent，
--                 NOINHERIT 让这个误操作不生效（fail-closed）。INHERIT 则会把
--                 应用角色的全部权限一次性交出去 —— 那正是本任务要防的事故。
--   NOBYPASSRLS   不能绕过行级安全策略
--   LOGIN         可以建立连接（但只有一个角色，无成员关系，无继承）

\echo '── 确保 artex_agent 角色存在（幂等）──'

DO $ensure$
DECLARE
    v_role text := COALESCE(NULLIF(current_setting('artex.agent_role', true), ''), 'artex_agent');
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = v_role) THEN
        EXECUTE format('CREATE ROLE %I LOGIN', v_role);
        RAISE NOTICE '已创建角色 %', v_role;
    ELSE
        RAISE NOTICE '角色 % 已存在，沿用', v_role;
    END IF;

    EXECUTE format(
        'ALTER ROLE %I WITH LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOBYPASSRLS',
        v_role);
END
$ensure$;

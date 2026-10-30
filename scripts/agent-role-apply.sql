-- =====================================================================
-- artex_agent 只读角色：授权落地（幂等）
-- =====================================================================
--
-- 由 agent-readonly-role.sh 调用，必须与 agent-role-classification.sql
-- **在同一个 psql 会话**里执行（分类结果放在临时表里）。
--
-- 授权模型（为什么是这套组合）：
--   PostgreSQL 权限只有「授权」没有「拒绝」。所以：
--     · 表级 GRANT SELECT  ⇒ 该表所有列都能读，列级 REVOKE 改不动它
--       （实测：对持有表级 SELECT 的角色执行 REVOKE SELECT (col) 是空操作，
--         attacl 不变、列照样可读 —— 所以不能只发表级 GRANT 再撤销敏感列）
--     · 列级 GRANT SELECT (c1, c2, ...) ⇒ 只有被点名的列能读
--   ⇒ 本脚本对每张表发三步，把终态做成确定性的：
--       1) REVOKE ALL ON TABLE            —— 清表级（含历史遗留的表级授权）
--       2) REVOKE SELECT (敏感列)          —— 清列级历史遗留，同时把
--          「敏感列必须被显式撤销」这条意图写进 SQL
--       3) GRANT SELECT (可读列)           —— 只授权非敏感列
--   整表撤的表走 1)+ 2'（对全部列列级 REVOKE），终态 = 完全不可见。
--
-- 口令**不在这个文件里**：口令由 shell 脚本用 psql 的 \password 单独施加，
-- 这样口令既不出现在 argv，也不出现在 SQL 文本里（不进服务器语句日志）。

\echo '── 落地授权（幂等）──'

DO $apply$
DECLARE
    -- 角色名：由外层脚本设置（SET artex.agent_role），缺省 artex_agent
    v_role text := COALESCE(NULLIF(current_setting('artex.agent_role', true), ''), 'artex_agent');
    r      record;
BEGIN
    -- -----------------------------------------------------------------
    -- ① 角色本体（角色由 agent-role-ensure.sql 创建；这里只刷新属性）
    -- -----------------------------------------------------------------
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = v_role) THEN
        RAISE EXCEPTION '角色 % 不存在：请先运行 agent-role-ensure.sql', v_role;
    END IF;
    -- NOINHERIT 是刻意的：万一有人日后误 GRANT artex_app TO artex_agent，
    -- NOINHERIT 会让这个误操作**不生效**（fail-closed）。INHERIT 则会把
    -- 应用角色的全部权限一次性送出去 —— 那正是本任务要防的事故。
    -- NOBYPASSRLS：防止用行级安全策略当掩护。
    EXECUTE format(
        'ALTER ROLE %I WITH LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOBYPASSRLS',
        v_role);

    -- -----------------------------------------------------------------
    -- ② 数据库级：只给 CONNECT
    -- -----------------------------------------------------------------
    EXECUTE format('REVOKE ALL PRIVILEGES ON DATABASE %I FROM %I', current_database(), v_role);
    EXECUTE format('GRANT CONNECT ON DATABASE %I TO %I', current_database(), v_role);

    -- -----------------------------------------------------------------
    -- ③ schema 级：只给 USAGE（去掉 CREATE，避免建影子表/函数）
    -- -----------------------------------------------------------------
    EXECUTE format('REVOKE ALL PRIVILEGES ON SCHEMA public FROM %I', v_role);
    EXECUTE format('GRANT USAGE ON SCHEMA public TO %I', v_role);

    -- -----------------------------------------------------------------
    -- ④ 序列 / 例程：不给任何权限
    -- -----------------------------------------------------------------
    -- 序列：nextval/currval 属于写侧能力，不给。
    EXECUTE format('REVOKE ALL PRIVILEGES ON ALL SEQUENCES IN SCHEMA public FROM %I', v_role);
    -- 例程：PostgreSQL 默认把新函数的 EXECUTE 授予 PUBLIC。若不单独撤，
    -- agent 角色能直接调用 public schema 里的函数 —— 那是一份
    -- 「SELECT 以外」的权限，也是一条绕过读限制的路径（触发器函数体里
    -- 有 INSERT/DELETE）。默认权限也要一起撤，否则下次建函数又会漏。
    EXECUTE format('REVOKE ALL PRIVILEGES ON ALL ROUTINES IN SCHEMA public FROM %I', v_role);
    EXECUTE format('ALTER DEFAULT PRIVILEGES IN SCHEMA public REVOKE EXECUTE ON FUNCTIONS FROM %I', v_role);

    -- -----------------------------------------------------------------
    -- ⑤ 表级 + 列级：按派生分类落地
    -- -----------------------------------------------------------------
    FOR r IN
        SELECT t.table_name,
               t.verdict,
               string_agg(format('%I', c.column_name), ', ' ORDER BY c.column_name)
                   FILTER (WHERE c.verdict = 'sensitive')    AS sens_cols,
               string_agg(format('%I', c.column_name), ', ' ORDER BY c.column_name)
                   FILTER (WHERE c.verdict <> 'sensitive')   AS read_cols,
               string_agg(format('%I', c.column_name), ', ' ORDER BY c.column_name) AS all_cols
        FROM agent_role_table t
        JOIN agent_role_class c ON c.table_name = t.table_name
        GROUP BY t.table_name, t.verdict
    LOOP
        -- 1) 表级清零
        EXECUTE format('REVOKE ALL PRIVILEGES ON TABLE public.%I FROM %I', r.table_name, v_role);

        IF r.verdict = 'revoke_all' THEN
            -- 2') 整表撤：表级已清，再把列级历史遗留清掉 → 终态完全不可见
            IF r.all_cols IS NOT NULL THEN
                EXECUTE format('REVOKE SELECT (%s) ON TABLE public.%I FROM %I',
                               r.all_cols, r.table_name, v_role);
            END IF;
        ELSE
            -- 2) 列级撤销：清历史遗留的列级授权，并显式落实「敏感列被撤销」
            IF r.sens_cols IS NOT NULL THEN
                EXECUTE format('REVOKE SELECT (%s) ON TABLE public.%I FROM %I',
                               r.sens_cols, r.table_name, v_role);
            END IF;
            -- 3) 列级授权：只给非敏感列（含白名单里的计数器/标识符列）
            IF r.read_cols IS NOT NULL THEN
                EXECUTE format('GRANT SELECT (%s) ON TABLE public.%I TO %I',
                               r.read_cols, r.table_name, v_role);
            END IF;
        END IF;
    END LOOP;
END
$apply$;

-- =====================================================================
-- 审计摘要：把「判了什么、授了什么、撤了什么」全部摊开给人看
-- =====================================================================
\pset format aligned
\pset border 2
\pset linestyle unicode

\echo ''
\echo '══ 1. 角色属性 ══'
SELECT rolname                                          AS 角色,
       rolsuper                                        AS 超级用户,
       rolcanlogin                                     AS 可登录,
       rolcreatedb                                     AS 可建库,
       rolcreaterole                                   AS 可建角色,
       rolinherit                                      AS 可继承,
       rolbypassrls                                    AS 绕过RLS,
       (rolpassword IS NOT NULL)                       AS 已设口令
FROM pg_roles
WHERE rolname = COALESCE(NULLIF(current_setting('artex.agent_role', true), ''), 'artex_agent');

\echo ''
\echo '══ 2. 分类统计（派生自 live schema）══'
SELECT count(DISTINCT table_name)                          AS 表数,
       count(*)                                            AS 列数,
       count(*) FILTER (WHERE verdict = 'sensitive')       AS 敏感列,
       count(*) FILTER (WHERE verdict = 'whitelisted')     AS 白名单列,
       count(*) FILTER (WHERE verdict = 'plain')           AS 普通列
FROM agent_role_class;

\echo ''
\echo '══ 3. 整表撤的表（近乎全表敏感）══'
SELECT table_name AS 表, reason AS 理由 FROM agent_role_table WHERE verdict = 'revoke_all' ORDER BY table_name;

\echo ''
\echo '══ 4. 逐表授权摘要 ══'
SELECT t.table_name                                            AS 表,
       t.verdict                                               AS 表级判定,
       count(*)                                               AS 列数,
       count(*) FILTER (WHERE c.verdict = 'sensitive')         AS 敏感列,
       count(*) FILTER (WHERE c.verdict <> 'sensitive')        AS 可读列,
       t.reason                                                AS 理由
FROM agent_role_table t
JOIN agent_role_class c ON c.table_name = t.table_name
GROUP BY t.table_name, t.verdict, t.reason
ORDER BY (t.verdict = 'revoke_all') DESC, t.table_name;

\echo ''
\echo '══ 5. 敏感列全清单（人工审计用）══'
SELECT table_name AS 表, column_name AS 列, reason AS 判定依据
FROM agent_role_class
WHERE verdict = 'sensitive'
ORDER BY table_name, column_name;

\echo ''
\echo '══ 6. 白名单列（模式会误伤、已豁免）══'
SELECT table_name AS 表, column_name AS 列, reason AS 豁免依据
FROM agent_role_class
WHERE verdict = 'whitelisted'
ORDER BY table_name, column_name;

\echo ''
\echo '══ 7. 提示（不阻断，但必须看）══'
SELECT kind AS 类型, subject AS 对象, detail AS 说明
FROM agent_role_notice
ORDER BY kind, subject;

\echo ''
\echo '══ 8. 落地后实际授权（information_schema 实测）══'
SELECT '表级授权' AS 层级,
       count(*) AS 条数,
       count(*) FILTER (WHERE privilege_type <> 'SELECT') AS 非SELECT条数
FROM information_schema.role_table_grants
WHERE grantee = COALESCE(NULLIF(current_setting('artex.agent_role', true), ''), 'artex_agent')
UNION ALL
SELECT '列级授权', count(*),
       count(*) FILTER (WHERE privilege_type <> 'SELECT')
FROM information_schema.column_privileges
WHERE grantee = COALESCE(NULLIF(current_setting('artex.agent_role', true), ''), 'artex_agent');

-- =====================================================================
-- artex_agent 只读角色：information_schema 断言（管理会话）
-- =====================================================================
--
-- 由 verify-agent-readonly-role.sh 调用，必须与 agent-role-classification.sql
-- **在同一个 psql 会话**里执行（期望集合来自分类临时表）。
--
-- 这里断言的是「权限账本」：把 live schema 派生出的期望授权集与
-- information_schema 里实际记录的授权逐条比对。任何一条对不上就是 FAIL。
--
-- 为什么还要另一份行为探针（agent-role-probe.sql）：
--   账本对得上，说明 ACL 是我们想要的样子；探针真跑一遍 INSERT 被拒、
--   敏感列读不到，说明 PostgreSQL 真的按这份 ACL 执行。两边都过才算数。

\echo '── 验证：information_schema 断言 ──'

DO $$
BEGIN
    IF to_regclass('pg_temp.agent_role_check') IS NOT NULL THEN
        EXECUTE 'DROP TABLE pg_temp.agent_role_check';
    END IF;
    IF to_regclass('pg_temp.agent_role_expected') IS NOT NULL THEN
        EXECUTE 'DROP VIEW pg_temp.agent_role_expected';
    END IF;
    IF to_regclass('pg_temp.agent_role_actual') IS NOT NULL THEN
        EXECUTE 'DROP VIEW pg_temp.agent_role_actual';
    END IF;
END
$$;

CREATE TEMP TABLE agent_role_check (
    name   text PRIMARY KEY,
    ok     boolean NOT NULL,
    detail text NOT NULL
);

-- 期望授权集：非整表撤的表 × 非敏感列
CREATE TEMP VIEW agent_role_expected AS
SELECT c.table_name, c.column_name
FROM agent_role_class c
JOIN agent_role_table t ON t.table_name = c.table_name
WHERE t.verdict <> 'revoke_all'
  AND c.verdict <> 'sensitive';

-- 实际授权集
CREATE TEMP VIEW agent_role_actual AS
SELECT table_name, column_name
FROM information_schema.column_privileges
WHERE grantee = COALESCE(NULLIF(current_setting('artex.agent_role', true), ''), 'artex_agent')
  AND table_schema = 'public'
  AND privilege_type = 'SELECT';

-- ① 角色存在且属性正确
INSERT INTO agent_role_check (name, ok, detail)
SELECT '① 角色存在且属性正确（可登录；非超级用户/不可建库/不可建角色/不可继承/不绕过RLS）',
       coalesce(bool_and(rolsuper = false AND rolcanlogin AND NOT rolcreatedb
                         AND NOT rolcreaterole AND NOT rolinherit AND NOT rolbypassrls), false),
       coalesce(string_agg('superuser=' || rolsuper || ' login=' || rolcanlogin
                || ' createdb=' || rolcreatedb || ' createrole=' || rolcreaterole
                || ' inherit=' || rolinherit || ' bypassrls=' || rolbypassrls, '; '),
                '角色不存在')
FROM pg_roles
WHERE rolname = COALESCE(NULLIF(current_setting('artex.agent_role', true), ''), 'artex_agent');

-- ② 口令已设置（pg_roles 对任何角色都可见，不泄露内容）
INSERT INTO agent_role_check (name, ok, detail)
SELECT '② 已设置口令',
       coalesce(bool_and(rolpassword IS NOT NULL), false),
       coalesce(string_agg(CASE WHEN rolpassword IS NOT NULL THEN '已设置' ELSE '未设置' END, '; '),
                '角色不存在')
FROM pg_roles
WHERE rolname = COALESCE(NULLIF(current_setting('artex.agent_role', true), ''), 'artex_agent');

-- ③ 无角色成员关系（否则可通过成员关系继承他人权限）
INSERT INTO agent_role_check (name, ok, detail)
SELECT '③ 无角色成员关系（不会因成员身份继承他人权限）',
       (count(*) = 0),
       '成员关系数=' || count(*)
FROM pg_auth_members m
JOIN pg_roles r ON r.oid = m.member
WHERE r.rolname = COALESCE(NULLIF(current_setting('artex.agent_role', true), ''), 'artex_agent');

-- ④ 无表级授权（表级授权会覆盖列级限制：表级 SELECT 一发，整表列都能读）
INSERT INTO agent_role_check (name, ok, detail)
SELECT '④ 无表级授权（只靠列级授权，避免表级授权覆盖列级限制）',
       (count(*) = 0),
       '表级授权数=' || count(*)
FROM information_schema.role_table_grants
WHERE grantee = COALESCE(NULLIF(current_setting('artex.agent_role', true), ''), 'artex_agent');

-- ⑤ 表级授权里没有 SELECT 以外的权限
INSERT INTO agent_role_check (name, ok, detail)
SELECT '⑤ 表级授权中无 SELECT 以外的权限',
       (count(*) = 0),
       '非 SELECT 表级授权数=' || count(*)
FROM information_schema.role_table_grants
WHERE grantee = COALESCE(NULLIF(current_setting('artex.agent_role', true), ''), 'artex_agent')
  AND privilege_type <> 'SELECT';

-- ⑥ 列级授权集合 == 派生分类的期望集合（缺失与多余都要为 0）
INSERT INTO agent_role_check (name, ok, detail)
SELECT '⑥ 列级授权集合与派生分类完全一致',
       (missing = 0 AND extra = 0),
       '应授未授=' || missing || ' 不应授却授了=' || extra
FROM (
    SELECT (SELECT count(*) FROM (SELECT * FROM agent_role_expected
                                  EXCEPT SELECT * FROM agent_role_actual) m) AS missing,
           (SELECT count(*) FROM (SELECT * FROM agent_role_actual
                                  EXCEPT SELECT * FROM agent_role_expected) x) AS extra
) d;

-- ⑦ 列级授权里没有 SELECT 以外的权限
INSERT INTO agent_role_check (name, ok, detail)
SELECT '⑦ 列级授权中无 SELECT 以外的权限',
       (count(*) = 0),
       '非 SELECT 列级授权数=' || count(*)
FROM information_schema.column_privileges
WHERE grantee = COALESCE(NULLIF(current_setting('artex.agent_role', true), ''), 'artex_agent')
  AND privilege_type <> 'SELECT';

-- ⑧ 敏感列一个都没被授权
INSERT INTO agent_role_check (name, ok, detail)
SELECT '⑧ 敏感列零授权',
       (leaked = 0),
       '被授权的敏感列数=' || leaked
FROM (
    SELECT count(*) AS leaked
    FROM agent_role_class c
    JOIN agent_role_actual a
      ON a.table_name = c.table_name AND a.column_name = c.column_name
    WHERE c.verdict = 'sensitive'
) d;

-- ⑨ 整表撤的表一列都没被授权
INSERT INTO agent_role_check (name, ok, detail)
SELECT '⑨ 整表撤的表完全不可见',
       (leaked = 0),
       '被授权的整表撤表列数=' || leaked
FROM (
    SELECT count(*) AS leaked
    FROM agent_role_table t
    JOIN agent_role_actual a ON a.table_name = t.table_name
    WHERE t.verdict = 'revoke_all'
) d;

-- ⑩ 无函数/过程 EXECUTE（默认 PUBLIC 有 EXECUTE，必须单独撤干净）
INSERT INTO agent_role_check (name, ok, detail)
SELECT '⑩ 无函数/过程 EXECUTE',
       (count(*) = 0),
       '例程授权数=' || count(*)
FROM information_schema.routine_privileges
WHERE grantee = COALESCE(NULLIF(current_setting('artex.agent_role', true), ''), 'artex_agent');

-- ⑪ schema 权限只有 USAGE（不得有 CREATE —— 否则能建影子表/函数）
-- information_schema 没有 role_schema_grants 视图，用 aclexplode 直接读
-- pg_namespace.nspacl —— 这是 ACL 的原始真相，detail 里同时列出 PUBLIC 的
-- 继承项，便于看清角色**实际**拥有的权限面。
INSERT INTO agent_role_check (name, ok, detail)
SELECT '⑪ schema 权限只有 USAGE（无 CREATE）',
       (count(*) FILTER (WHERE grantee_name = COALESCE(NULLIF(current_setting('artex.agent_role', true), ''), 'artex_agent')
                           AND privilege_type <> 'USAGE') = 0),
       coalesce(string_agg(grantee_name || ':' || privilege_type, ', '), '无')
FROM (
    SELECT CASE WHEN a.grantee = 0 THEN 'PUBLIC' ELSE pg_get_userbyid(a.grantee) END AS grantee_name,
           a.privilege_type
    FROM aclexplode((SELECT nspacl FROM pg_namespace WHERE nspname = 'public')) a
) s;

-- ⑫ database 权限只有 CONNECT（不得有 TEMPORARY/CREATE）
-- 同上，information_schema 无对应视图，直接读 pg_database.datacl。
INSERT INTO agent_role_check (name, ok, detail)
SELECT '⑫ database 权限只有 CONNECT',
       (count(*) FILTER (WHERE grantee_name = COALESCE(NULLIF(current_setting('artex.agent_role', true), ''), 'artex_agent')
                           AND privilege_type <> 'CONNECT') = 0),
       coalesce(string_agg(grantee_name || ':' || privilege_type, ', '), '无')
FROM (
    SELECT CASE WHEN a.grantee = 0 THEN 'PUBLIC' ELSE pg_get_userbyid(a.grantee) END AS grantee_name,
           a.privilege_type
    FROM aclexplode((SELECT datacl FROM pg_database WHERE datname = current_database())) a
) d;

-- ⑬ 其它类型对象（域/类型/外键等）没有额外授权
INSERT INTO agent_role_check (name, ok, detail)
SELECT '⑬ 其它对象类型无额外授权',
       (count(*) = 0),
       '其它授权数=' || count(*)
FROM information_schema.role_usage_grants
WHERE grantee = COALESCE(NULLIF(current_setting('artex.agent_role', true), ''), 'artex_agent');

-- ⑭ 枚举完整性：验证用的连接必须能看到全部表，否则断言会漏表
INSERT INTO agent_role_check (name, ok, detail)
SELECT '⑭ 枚举完整（当前连接可见全部基表，断言未漏表）',
       (visible = actual_tables),
       'information_schema 可见=' || visible || ' / pg_class 实表=' || actual_tables
FROM (
    SELECT (SELECT count(DISTINCT table_name) FROM information_schema.tables
             WHERE table_schema = 'public' AND table_type = 'BASE TABLE') AS visible,
           (SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
             WHERE n.nspname = 'public' AND c.relkind = 'r') AS actual_tables
) d;

\pset format aligned
\pset border 2
\echo ''
\echo '══ 断言结果 ══'
SELECT name AS 断言, ok AS 通过, detail AS 明细 FROM agent_role_check ORDER BY name;

-- 机器可读行：外层脚本据此统计通过/失败并决定退出码
\pset format unaligned
\pset tuples_only on
SELECT 'CHECK ' || CASE WHEN ok THEN 'PASS' ELSE 'FAIL' END || ' | ' || name || ' | ' || detail
FROM agent_role_check
ORDER BY name;

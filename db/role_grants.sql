-- =====================================================================
-- 受限运行角色（默认 artex_app）的最小权限集 —— 唯一真相源
-- =====================================================================
--
-- 谁在用这个文件：
--   * db.Open 的 schema 应用路径（Go 侧 //go:embed 读同一份，db/role.go）
--   * scripts/db-role-bootstrap.sh（psql -f 读同一份）
-- 两边共用一份 ⇒ 启动期自动授权与运维脚本授权**不会漂移**。
--
-- 角色名从会话 GUC `artex.app_role` 取（缺省 artex_app）：
--   psql：PGOPTIONS="-c artex.app_role=artex_app" psql -v ON_ERROR_STOP=1 -f db/role_grants.sql
--   Go  ：SELECT set_config('artex.app_role', $1, false)  （必须与本文件在**同一条连接**上执行）
--
-- 授什么（就这些，一句话：连得上、看得见 public、业务读写、序列 nextval）：
--   CONNECT ON DATABASE                      —— 连上库
--   USAGE ON SCHEMA public                   —— 看得见 public 里的对象
--   SELECT/INSERT/UPDATE/DELETE ON ALL TABLES IN SCHEMA public —— 业务表读写
--   USAGE ON ALL SEQUENCES IN SCHEMA public  —— nextval（serial / bigserial 主键）
--
-- 不授什么（刻意，才是这一层存在的意义）：
--   CREATE ON SCHEMA public                  —— 直连也不能建表 / 建索引
--   任何业务表的所有权                         —— 直连也不能 DROP / ALTER 业务表
--   SUPERUSER / CREATEDB / CREATEROLE / BYPASSRLS —— 角色本体属性（见 bootstrap 脚本）
--   别的 schema 的任何权限                      —— 直连也读不到 public 之外的东西
--
-- 幂等：全部是 GRANT / REVOKE，重复执行结果一致。
-- 权限跟随 schema 演进：ON ALL TABLES 覆盖当前表集，
-- ALTER DEFAULT PRIVILEGES 覆盖**以后**新建的表与序列（新增表不用重跑脚本）。
--
-- ⚠️ GRANT ... ON ALL TABLES 需要「表属主或超级用户」身份 ⇒ 本文件只能由
--    admin（超级用户）连接执行。

DO $grant$
DECLARE
    v_role text := COALESCE(NULLIF(current_setting('artex.app_role', true), ''), 'artex_app');
    member_role text;
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = v_role) THEN
        RAISE NOTICE '受限运行角色 % 不存在，跳过授权（先跑 scripts/db-role-bootstrap.sh）', v_role;
        RETURN;
    END IF;

    -- 1) 连库
    EXECUTE format('GRANT CONNECT ON DATABASE %I TO %I', current_database(), v_role);

    -- 2) schema public：只给 USAGE，不给 CREATE。
    --    CREATE 是「直连也能建表」的总闸门 ⇒ 显式 REVOKE 一次，
    --    免得哪次有人把 CREATE 又授给了 PUBLIC 或这个角色。
    EXECUTE format('GRANT USAGE ON SCHEMA public TO %I', v_role);
    EXECUTE format('REVOKE CREATE ON SCHEMA public FROM %I', v_role);
    -- PUBLIC 默认在 PG15+ 已经没有 CREATE；PG14 及更早有 ⇒ 一并收掉。
    -- 只影响「在 public 里建对象」这件事，不影响任何角色的读写。
    EXECUTE format('REVOKE CREATE ON SCHEMA public FROM PUBLIC');

    -- 3) 业务表：读写（不含 TRUNCATE / REFERENCES / TRIGGER —— 那些是 DDL 侧的能力）
    EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO %I', v_role);

    -- 4) 序列：nextval（serial 主键回填）
    EXECUTE format('GRANT USAGE ON ALL SEQUENCES IN SCHEMA public TO %I', v_role);

    -- 5) 以后新建的表 / 序列自动带同样的权限（schema 演进不用重跑 bootstrap）。
    --    ALTER DEFAULT PRIVILEGES 只对「执行它的那个角色」 future 创建的对象生效 ⇒
    --    必须由建表的那个角色（admin 连接）来执行。
    EXECUTE format('ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO %I', v_role);
    EXECUTE format('ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT USAGE ON SEQUENCES TO %I', v_role);

    -- 6) 成员关系清零：受限角色不得是任何别的角色的成员。
    --    NOINHERIT 只挡住「自动继承」，挡不住 `SET ROLE` —— 只要它是某个高权限
    --    角色的成员，直连的人一条 SET ROLE 就又提权回去了。
    -- ⚠️ 循环变量不能与查询里的表别名重名（原来两者都叫 r ⇒ PL/pgSQL 报
    --    "loop variable of loop over rows must be a record variable"）。
    FOR member_role IN
        SELECT gr.rolname
        FROM pg_auth_members m
        JOIN pg_roles gr ON gr.oid = m.roleid
        WHERE m.member = (SELECT oid FROM pg_roles WHERE rolname = v_role)
    LOOP
        EXECUTE format('REVOKE %I FROM %I', member_role, v_role);
        RAISE NOTICE '已撤销受限角色 % 的成员身份：%', v_role, member_role;
    END LOOP;

    RAISE NOTICE '已把 % 收敛到最小权限（CONNECT + USAGE + DML + 序列 USAGE，无 CREATE、无所有权、无成员关系）', v_role;
END
$grant$;

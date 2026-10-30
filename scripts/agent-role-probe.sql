-- =====================================================================
-- artex_agent 只读角色：行为探针（以 artex_agent 身份真实执行）
-- =====================================================================
--
-- 由 verify-agent-readonly-role.sh 以 **artex_agent 身份**连接后执行。
-- 与 agent-role-verify.sql 的分工：
--   verify.sql  查「权限账本」——information_schema 里记录的授权对不对
--   probe.sql   真跑一遍——PostgreSQL 到底按不按这份 ACL 执行
--   两边都过才算数：账本对但行为不对（缓存/生效延迟/其它执行路径），
--   或者行为对但账本不对（走了我没预料到的授权来源），都算没闭合。
--
-- 每条探针输出：
--   PROBE PASS <id> <说明>            —— 实际结果与期望一致
--   PROBE FAIL <id> <说明> <实际> <错误信息>
-- 由外层脚本统计 PASS/FAIL 数量并决定退出码。

\pset format unaligned
\pset tuples_only on

DO $probe$
DECLARE
    r     record;
    v_ok  boolean;
    v_err text;
BEGIN
    FOR r IN
        SELECT * FROM (VALUES
            -- ---- ① 列级限制：同表内，敏感列不可读、非敏感列可读 ----
            ('p01', '敏感列 findings.evidence 不可读',
                 'SELECT evidence FROM public.findings LIMIT 1', false),
            ('p02', '同表非敏感列 findings.id/status 可读',
                 'SELECT id, status FROM public.findings LIMIT 1', true),
            ('p03', '敏感列 mcp_servers.env 不可读（实测含 X-API-Key）',
                 'SELECT env FROM public.mcp_servers LIMIT 1', false),
            ('p04', '同表非敏感列 mcp_servers.name 可读',
                 'SELECT name FROM public.mcp_servers LIMIT 1', true),
            ('p03b', '敏感列 assets.auth 不可读（契约：含 username/password）',
                 'SELECT auth FROM public.assets LIMIT 1', false),
            ('p03c', '敏感列 assets.params 不可读（契约：location 含 header/body）',
                 'SELECT params FROM public.assets LIMIT 1', false),
            ('p03d', '敏感列 notification_channels.config 不可读（契约：bot_token/password）',
                 'SELECT config FROM public.notification_channels LIMIT 1', false),
            ('p03e', '同表非敏感列 assets.domain 可读',
                 'SELECT domain FROM public.assets LIMIT 1', true),

            -- ---- ② 整表撤的表：完全不可见 ----
            ('p05', '整表撤表 llm_records 不可读',
                 'SELECT count(*) FROM public.llm_records', false),
            ('p06', '整表撤表 traffic_evidence_snapshots 不可读',
                 'SELECT count(*) FROM public.traffic_evidence_snapshots', false),
            ('p07', '整表撤表 llm_profiles 不可读',
                 'SELECT count(*) FROM public.llm_profiles', false),
            ('p08', '整表撤表 audit_events 不可读',
                 'SELECT count(*) FROM public.audit_events', false),
            ('p09', '整表撤表 exploration_nodes 不可读',
                 'SELECT count(*) FROM public.exploration_nodes', false),
            ('p10', '整表撤表 side_question_sessions 不可读',
                 'SELECT count(*) FROM public.side_question_sessions', false),
            ('p11', '整表撤表 side_question_requests 不可读',
                 'SELECT count(*) FROM public.side_question_requests', false),
            ('p12', '整表撤表在 information_schema 里不可见',
                 'DO $$ BEGIN IF EXISTS (SELECT 1 FROM information_schema.tables
                        WHERE table_schema = ''public'' AND table_name = ''llm_records'') THEN
                        RAISE EXCEPTION ''llm_records 对该角色仍可见''; END IF; END $$', true),
            ('p13', '非敏感表在 information_schema 里可见',
                 'DO $$ BEGIN IF NOT EXISTS (SELECT 1 FROM information_schema.tables
                        WHERE table_schema = ''public'' AND table_name = ''findings'') THEN
                        RAISE EXCEPTION ''findings 对该角色不可见''; END IF; END $$', true),

            -- ---- ③ 写操作一律被拒 ----
            ('p14', 'INSERT 被拒',
                 'INSERT INTO public.findings(task_id, name) VALUES (1, ''probe'')', false),
            ('p15', 'UPDATE 被拒',
                 'UPDATE public.findings SET name = ''probe'' WHERE id = 1', false),
            ('p16', 'DELETE 被拒',
                 'DELETE FROM public.findings WHERE id = 1', false),
            ('p17', 'TRUNCATE 被拒',
                 'TRUNCATE public.findings', false),

            -- ---- ④ DDL 一律被拒 ----
            ('p18', 'CREATE TABLE 被拒',
                 'CREATE TABLE public.probe_should_not_exist(id integer)', false),
            ('p19', 'DROP TABLE 被拒',
                 'DROP TABLE public.findings', false),
            ('p20', 'CREATE 在 public schema 被拒',
                 'CREATE SCHEMA probe_schema', false),

            -- ---- ⑤ 常规只读路径仍然可用 ----
            ('p21', '非敏感表 tasks 可读',
                 'SELECT id, name, status FROM public.tasks LIMIT 1', true),
            ('p22', 'assets 结构列可读',
                 'SELECT id, type, domain FROM public.assets LIMIT 1', true)
        ) AS t(id, descr, sql_text, expect_ok)
    LOOP
        v_ok  := true;
        v_err := '';
        BEGIN
            EXECUTE r.sql_text;
        EXCEPTION WHEN OTHERS THEN
            v_ok  := false;
            v_err := SQLERRM;
        END;

        IF v_ok = r.expect_ok THEN
            RAISE NOTICE 'PROBE PASS % | % | %', r.id, r.descr,
                CASE WHEN v_err = '' THEN '无错误' ELSE v_err END;
        ELSE
            RAISE NOTICE 'PROBE FAIL % | % | 期望=% 实际=% | %', r.id, r.descr,
                CASE WHEN r.expect_ok THEN '成功' ELSE '失败' END,
                CASE WHEN v_ok    THEN '成功' ELSE '失败' END,
                v_err;
        END IF;
    END LOOP;
END
$probe$;

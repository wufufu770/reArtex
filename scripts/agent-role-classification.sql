-- =====================================================================
-- artex_agent 只读角色：敏感列「派生式」分类（唯一真相源）
-- =====================================================================
--
-- 这个文件是 **判定逻辑** 的唯一落点。授权脚本与验证脚本都加载它，
-- 因此两边对「哪些列敏感」的理解永远一致 —— 不存在「授权脚本认为敏感、
-- 验证脚本认为不敏感」的漂移。
--
-- 为什么必须派生、不能手写 SQL 清单：
--   手写的 REVOKE 清单在 schema 演进后会静默失效 —— 新表/新列进来时没人
--   记得补，脚本照样「成功」，于是缺口以「看起来是绿的」的形式长期存在。
--   派生式脚本每次都从 information_schema 重新枚举 live schema，重跑即
--   把新对象纳入判定；缺口会以「审计摘要里的差异」形式暴露，而不是沉默。
--
-- 三段判定（顺序即优先级）：
--   ① 显式敏感清单（explicit_sensitive）—— 最高优先级。用于「语义确定敏感、
--      但不能只看名字」的列；也用于「名字看着无害、实际装着凭据」的列。
--   ② 误伤白名单（key_whitelist + 计数器规则）—— 模式会误伤的「不是凭据」的列。
--   ③ 模式匹配（patterns）—— 按列名敏感词根生成候选。
--   都不命中 → plain（可读）。
--
-- 为什么用「列级 REVOKE + 列级 GRANT」而不是只做「表级 REVOKE」：
--   Sir 2026-10-29 决策 C 是「列级敏感，不是表级敏感」—— 整表撤会把 agent
--   的合法只读需求（看任务、资产、发现的结构化字段）一起砍掉。
--
-- ⚠️ PostgreSQL 权限模型的重要事实（本脚本按此实现，勿改）：
--   PostgreSQL 的权限是 **只增不减的授权模型，没有 deny 语义**。
--   对一个已经持有 **表级 SELECT** 的角色执行
--       REVOKE SELECT (col) ON TABLE t FROM role;
--   是 **完全的空操作**：不会写 attacl，列依然可读（已在开发实例实测）。
--   ⇒ 真正能挡住列的办法只有一个：**只发列级 GRANT，且不发票级 GRANT**。
--   本脚本因此对每张表发：REVOKE ALL(表级) + REVOKE SELECT(列级清理历史遗留)
--   + GRANT SELECT(仅可读列)。列级 REVOKE 仍然要发：它负责清掉历史遗留的
--   列级授权，也是「敏感列必须显式被撤销」这条意图在 SQL 上的落地。
--
-- 本脚本只建临时表（pg_temp），不在业务 schema 留任何对象：
--   临时表不会出现在 information_schema.columns 的 public 分区里，
--   因此不会造成「自己枚举自己」的递归。

\echo '── 派生敏感列分类（information_schema 实时枚举）──'

-- 临时表可能已存在（同一会话内重跑）：先安全清掉。
-- to_regclass 找不到时返回 NULL 而不是报错，所以 pg_temp schema 尚未建立也安全。
DO $$
BEGIN
    IF to_regclass('pg_temp.agent_role_class') IS NOT NULL THEN
        EXECUTE 'DROP TABLE pg_temp.agent_role_class';
    END IF;
    IF to_regclass('pg_temp.agent_role_table') IS NOT NULL THEN
        EXECUTE 'DROP TABLE pg_temp.agent_role_table';
    END IF;
    IF to_regclass('pg_temp.agent_role_notice') IS NOT NULL THEN
        EXECUTE 'DROP TABLE pg_temp.agent_role_notice';
    END IF;
END
$$;

-- 列级判定结果
CREATE TEMP TABLE agent_role_class (
    table_name  text NOT NULL,
    column_name text NOT NULL,
    verdict     text NOT NULL CHECK (verdict IN ('sensitive', 'whitelisted', 'plain')),
    reason      text NOT NULL,
    PRIMARY KEY (table_name, column_name)
);

-- 表级判定结果
CREATE TEMP TABLE agent_role_table (
    table_name text PRIMARY KEY,
    verdict    text NOT NULL CHECK (verdict IN ('revoke_all', 'grant_columns')),
    reason     text NOT NULL
);

-- 「需要人工看一眼」的提示（不阻断执行，但必须打印出来）
CREATE TEMP TABLE agent_role_notice (
    kind    text NOT NULL,
    subject text NOT NULL,
    detail  text NOT NULL
);

-- ---------------------------------------------------------------------
-- ① 模式候选：列名（不区分大小写）命中任一敏感词根
-- ---------------------------------------------------------------------
-- 词根来自 Sir 给定的清单。实测补充见下方 explicit_sensitive 的注释。
INSERT INTO agent_role_class (table_name, column_name, verdict, reason)
WITH
patterns(pattern, why) AS (
    VALUES
      ('token',         '凭据/令牌'),
      ('secret',        '机密'),
      ('password',      '口令'),
      ('key',           '密钥'),
      ('credential',    '凭据'),
      ('dsn',           '数据库连接串'),
      ('request_body',  '请求报文'),
      ('response_body', '响应报文'),
      ('header',        'HTTP 头'),
      ('cookie',        'Cookie'),
      ('authorization', 'Authorization 头'),
      ('cipher',        '密文'),
      ('payload',       '全量载荷'),
      ('raw',           '原始内容'),
      ('body',          '报文正文'),
      ('hint',          '凭据提示'),
      ('evidence',      '证据正文'),
      ('report',        '报告正文'),
      ('snapshot',      '快照正文'),
      ('summary',       '摘要正文'),
      ('notes',         '备注正文'),
      ('note',          '备注'),
      ('head',          '报文头')
),
-- ---------------------------------------------------------------------
-- ② 误伤白名单 A：标识符型 key —— **必须逐列显式列举**
-- ---------------------------------------------------------------------
-- 为什么不能用「列名以 _key 结尾就豁免」这种结构规则：
--   那条规则会把 session_key（side_question_*）与 api_key（llm_profiles）
--   一起豁免 —— 而这两个恰恰是最该撤的会话凭据与 API Key。
--   PostgreSQL 侧的 ACL 没有 deny 语义，豁免错了就是真的泄出去。
--   ⇒ 宁可显式列举（新表进来默认按敏感处理，fail-closed），也不做宽松推断。
key_whitelist(table_name, column_name, why) AS (
    VALUES
      ('agents',          'key',       '标识符：agent 主键，非凭据'),
      ('agent_triggers',  'agent_key', '标识符：外键，非凭据'),
      ('conversations',   'agent_key', '标识符：外键，非凭据'),
      ('tool_usage',      'tool_key',  '标识符：外键，非凭据'),
      ('tool_usage',      'agent_key', '标识符：外键，非凭据'),
      ('skill_usage',     'agent_key', '标识符：外键，非凭据'),
      ('tools',           'key',       '标识符：工具主键，非凭据'),
      ('settings',        'key',       '标识符：配置项主键，非凭据'),
      ('scheduler_state', 'key',       '标识符：调度器状态主键，非凭据'),
      ('companies',       'nkey',      '标识符：公司公开键，非凭据'),
      ('task_categories', 'nkey',      '标识符：分类公开键，非凭据'),
      ('task_templates',  'nkey',      '标识符：模板公开键，非凭据')
),
-- ---------------------------------------------------------------------
-- ② 误伤白名单 B：计数器型 *_tokens —— 可用结构规则
-- ---------------------------------------------------------------------
-- 计数不是凭据（input_tokens / output_tokens / cache_read_tokens /
-- cache_write_tokens / max_tokens / max_tokens_field）：agent 合法需要看配额。
-- 这里用结构规则而不是列举，因为「以 _tokens 结尾的计数列」不可能是凭据，
-- 且新表新增同类列时不必改脚本。
-- 判定写在外层 CASE 里（见下），此处只登记规则说明。
--
-- ---------------------------------------------------------------------
-- ② 误伤白名单 C：结构性列（名义型）—— 名字里带敏感词，列里装的是结构信息
-- ---------------------------------------------------------------------
-- 与上面 B 组同源：Sir 的白名单原则是「名义型列（标识符/计数器）不是凭据」。
-- 把该原则一致地用到 live schema 派生出来的另一类误伤上：
--   外键 ID、版本号、时间戳 —— 它们命中 snapshot/evidence/summary 等词根，
--   但列里是「指向哪条记录 / 第几版 / 什么时候」，不是记录本身的内容。
--   （这 6 列正是「把模式跑一遍 live schema」才会暴露出来的误伤 ——
--     Sir 给的清单是在跑模式之前写的，故未收录。）
structural_whitelist(table_name, column_name, why) AS (
    VALUES
      ('findings',                 'evidence_version',        '结构列：证据版本号（bigint），不是证据正文'),
      ('findings',                 'report_evidence_version', '结构列：报告证据版本号（bigint），不是报告正文'),
      ('finding_traffic_bindings', 'snapshot_id',             '结构列：流量快照外键，不是快照正文'),
      ('side_question_requests',   'snapshot_at',             '结构列：快照时间戳，不是快照正文'),
      ('task_archives',            'category_id_snapshot',    '结构列：归档时记录的分类 ID，不是快照正文'),
      ('task_archives',            'category_name_snapshot',  '结构列：归档时记录的分类名，不是快照正文')
),
-- ---------------------------------------------------------------------
-- ③ 显式敏感清单
-- ---------------------------------------------------------------------
-- 前 15 条来自 Sir 给定的清单（模式其实也抓得到，但显式登记 = 把「语义决定」
-- 写进代码，而不是依赖模式的运气）。
explicit_sensitive(table_name, column_name, why) AS (
    VALUES
      ('llm_profiles',               'api_key',            '明文 API Key'),
      ('llm_profiles',               'api_key_cipher',     'AES-GCM 密文：主密钥在文件里，密文+主密钥=明文，按明文同等对待'),
      ('llm_profiles',               'api_key_hint',       'Key 提示片段'),
      ('llm_profiles',               'session_header_key', '会话头凭据键'),
      ('llm_records',                'raw_request',        '原始请求报文'),
      ('llm_records',                'raw_response',       '原始响应报文'),
      ('llm_records',                'request_body',       '请求正文'),
      ('llm_records',                'response_body',      '响应正文'),
      ('side_question_requests',     'session_key',        '会话凭据'),
      ('side_question_sessions',     'session_key',        '会话凭据'),
      ('audit_events',               'payload',            '全量载荷 jsonb'),
      ('exploration_nodes',          'payload',            '全量载荷 jsonb'),
      ('company_scope',              'raw',                '原始范围内容'),
      ('traffic_evidence_snapshots', 'req_head',           '请求头（含 Authorization/Cookie）'),
      ('traffic_evidence_snapshots', 'resp_head',          '响应头（含 Set-Cookie）'),
      -- ↓↓↓ 实测/契约补充（Sir 给定清单之外，证据充分；可按需删回）↓↓↓
      ('mcp_servers',                'env',                '实测补充：MCP 服务环境变量里实测有 X-API-Key（开发实例 ScopeSentry 行命中）'),
      ('assets',                     'auth',               '实测补充：insert_assets 工具契约写明「认证信息列表，每条含 type/username/password」'),
      ('assets',                     'params',             '实测补充：insert_assets 工具契约写明「location(query/body/header/path)/name/value」—— header/body 位可带凭据'),
      ('notification_channels',      'config',             '实测补充：schema 注释写明 telegram{bot_token} / email{username,password}')
),
-- live schema 实时枚举 —— **不信任任何静态表清单**
-- information_schema.columns 没有 table_type 列，要与 information_schema.tables
-- 联接才能把视图/外表排除掉（视图的授权语义与基表不同，不在本脚本范围内）。
live AS (
    SELECT c.table_name, c.column_name
    FROM information_schema.columns c
    JOIN information_schema.tables t
      ON t.table_schema = c.table_schema AND t.table_name = c.table_name
    WHERE c.table_schema = 'public'
      AND t.table_type = 'BASE TABLE'
)
SELECT
    l.table_name,
    l.column_name,
    CASE
        WHEN e.table_name IS NOT NULL          THEN 'sensitive'
        WHEN w.table_name IS NOT NULL          THEN 'whitelisted'
        WHEN sw.table_name IS NOT NULL         THEN 'whitelisted'
        WHEN l.column_name ~ '(^|_)tokens(_field)?$' THEN 'whitelisted'
        WHEN p.pattern IS NOT NULL             THEN 'sensitive'
        ELSE                                        'plain'
    END AS verdict,
    CASE
        WHEN e.table_name IS NOT NULL          THEN e.why
        WHEN w.table_name IS NOT NULL          THEN w.why
        WHEN sw.table_name IS NOT NULL         THEN sw.why
        WHEN l.column_name ~ '(^|_)tokens(_field)?$' THEN '计数器：用量/配额统计，非凭据'
        WHEN p.pattern IS NOT NULL             THEN '模式命中「' || p.pattern || '」：' || p.why
        ELSE                                        '未命中敏感模式'
    END AS reason
FROM live l
LEFT JOIN explicit_sensitive e
       ON e.table_name = l.table_name AND e.column_name = l.column_name
LEFT JOIN key_whitelist w
       ON w.table_name = l.table_name AND w.column_name = l.column_name
LEFT JOIN structural_whitelist sw
       ON sw.table_name = l.table_name AND sw.column_name = l.column_name
LEFT JOIN LATERAL (
    SELECT p2.pattern, p2.why
    FROM patterns p2
    WHERE l.column_name ILIKE '%' || p2.pattern || '%'
    ORDER BY length(p2.pattern) DESC   -- 取最长命中（request_body 优先于 body）
    LIMIT 1
) p ON TRUE;

-- ---------------------------------------------------------------------
-- 表级判定：近乎全表敏感的表 → 整表撤；其余 → 列级授权
-- ---------------------------------------------------------------------
INSERT INTO agent_role_table (table_name, verdict, reason)
WITH
wholesale(table_name, why) AS (
    VALUES
      ('llm_records',                '报文级内容（请求/响应全文）'),
      ('traffic_evidence_snapshots', '含请求/响应头（Authorization/Cookie）'),
      ('llm_profiles',               '凭据表：API Key / 密文 / 提示 / 会话头键'),
      ('side_question_sessions',     '含会话凭据 session_key'),
      ('side_question_requests',     '含会话凭据 session_key'),
      ('exploration_nodes',          'payload 为全量 jsonb 载荷'),
      ('audit_events',               'payload 为全量 jsonb 载荷')
),
live_tables AS (
    SELECT table_name
    FROM information_schema.tables
    WHERE table_schema = 'public' AND table_type = 'BASE TABLE'
)
SELECT
    lt.table_name,
    CASE WHEN w.table_name IS NOT NULL THEN 'revoke_all' ELSE 'grant_columns' END,
    COALESCE(w.why, '按列级判定：仅授权非敏感列')
FROM live_tables lt
LEFT JOIN wholesale w ON w.table_name = lt.table_name;

-- ---------------------------------------------------------------------
-- 提示：规则清单与 live schema 不一致 / 视图未纳入分类
-- ---------------------------------------------------------------------
INSERT INTO agent_role_notice (kind, subject, detail)
-- ① 显式敏感清单里的对象在当前 schema 不存在 → 跳过并报告（不报错、不中断）
SELECT 'rule_missing', r.t || '.' || r.c, '显式敏感清单命中但当前 schema 无此列/表 —— 已跳过'
FROM (VALUES
        ('llm_profiles',               'api_key'),
        ('llm_profiles',               'api_key_cipher'),
        ('llm_profiles',               'api_key_hint'),
        ('llm_profiles',               'session_header_key'),
        ('llm_records',                'raw_request'),
        ('llm_records',                'raw_response'),
        ('llm_records',                'request_body'),
        ('llm_records',                'response_body'),
        ('side_question_requests',     'session_key'),
        ('side_question_sessions',     'session_key'),
        ('audit_events',               'payload'),
        ('exploration_nodes',          'payload'),
        ('company_scope',              'raw'),
        ('traffic_evidence_snapshots', 'req_head'),
        ('traffic_evidence_snapshots', 'resp_head'),
        ('mcp_servers',                'env'),
        ('assets',                     'auth'),
        ('assets',                     'params'),
        ('notification_channels',      'config')
     ) AS r(t, c)
LEFT JOIN information_schema.columns c
       ON c.table_schema = 'public' AND c.table_name = r.t AND c.column_name = r.c
WHERE c.column_name IS NULL
UNION ALL
-- ② 整表撤清单里的表在当前 schema 不存在 → 跳过并报告
SELECT 'rule_missing', w.t, '整表撤清单命中但当前 schema 无此表 —— 已跳过'
FROM (VALUES
        ('llm_records'),
        ('traffic_evidence_snapshots'),
        ('llm_profiles'),
        ('side_question_sessions'),
        ('side_question_requests'),
        ('exploration_nodes'),
        ('audit_events')
     ) AS w(t)
LEFT JOIN information_schema.tables tb
       ON tb.table_schema = 'public' AND tb.table_name = w.t AND tb.table_type = 'BASE TABLE'
WHERE tb.table_name IS NULL
UNION ALL
-- ③ public schema 里的视图：列级分类不覆盖视图授权（视图会透出基表内容）
SELECT 'view_present', v.table_name,
       'public schema 存在视图：视图可透出基表内容，列级分类不覆盖视图授权 —— 请人工评审'
FROM information_schema.views v
WHERE v.table_schema = 'public'
UNION ALL
-- ④ 枚举完整性：information_schema 会按权限过滤，若当前连接看不到全部表，
--    授权就会静默漏表 → 必须报出来
SELECT 'enumeration_incomplete', 'public',
       'information_schema 可见表数 ' ||
       (SELECT count(DISTINCT table_name) FROM information_schema.tables
         WHERE table_schema = 'public' AND table_type = 'BASE TABLE') ||
       ' ≠ pg_class 实表数 ' ||
       (SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
         WHERE n.nspname = 'public' AND c.relkind = 'r') ||
       '：当前连接对部分表无权限，授权会漏表 —— 请用表属主/超级用户连接'
WHERE (SELECT count(DISTINCT table_name) FROM information_schema.tables
        WHERE table_schema = 'public' AND table_type = 'BASE TABLE')
    <> (SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
         WHERE n.nspname = 'public' AND c.relkind = 'r');

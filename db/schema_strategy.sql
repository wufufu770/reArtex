-- 策略库（L4 图谱层）数据模型 —— **与 db/schema.sql 并行应用**的幂等 DDL。
--
-- 为什么单独成文件：db/schema.sql 由别的单元在同一 worktree 内并行开发，本单元
-- 不改它。本文件由 db/strategies.go 的 EnsureStrategySchema 在 db.Open 之后
-- 补齐（见该文件注释），schema 属主可以随时把内容并进 db/schema.sql —— 两边
-- 都是幂等的，重复应用无副作用。
--
-- 层定位：L4 是**独立的顶层图**，不是 exploration_nodes 的子表 ——
-- exploration_nodes 挂 exploration_id（1:1 UNIQUE + ON DELETE RESTRICT），
-- 任务一归档就跟着封存，而策略必须跨任务、跨会话存活。
--
-- 本文件当前（Task 1）含三张表 + 一个物化视图：
--   strategies              策略节点（六元组 𝒜/ℐ/ℂ/𝒯/π/ℰ 落成 jsonb 列）
--   strategy_edges          策略之间的边
--   strategy_evidence       证据链（**裸 id + content_digest，不级联外键**）
--   strategy_utility        **物化视图**：权威 utility（静态列只是缓存）
--
-- 提议表 / 运行明细 / 冻结语料 / 组内快照由 Task 4 追加到本文件 —— 本单元
-- 不预写未落地的 DDL（预写的表会与后续单元的定义打架）。

-- ---------- strategies ----------
-- state 枚举里有 quarantined：新策略一律先进隔离态，晋级要同时满足三个
-- 确定性条件（重放闸门 + usage_count>=n + 客观里程碑命中）。
-- ⚠️ n 计的是 **usage_count（被实际使用次数）**，不是 retrieval_count ——
-- 被检索多次但从未被采纳的策略，不算「有观测」。理由见 Task 5。
-- 这是「一次成功不能当通用能力」的直接对策（反模式 10）。
CREATE TABLE IF NOT EXISTS strategies (
    id                BIGSERIAL PRIMARY KEY,
    -- 租户隔离维度（D1 决策 = 按 company_id 隔离，见计划「已拍板」第 1 条）。
    -- 可空：company_id IS NULL 表示【平台级】策略（默认不建，由人显式建）。
    company_id        BIGINT REFERENCES companies(id) ON DELETE CASCADE,
    name              TEXT NOT NULL,
    summary           TEXT NOT NULL DEFAULT '',
    -- 𝒜：激活条件。键直接取自现有资产图的列（asset_type / service_type /
    -- category / record_type），**不另造资产类型体系**。
    activation        JSONB NOT NULL DEFAULT '{}',
    -- ℐ：步骤（有序数组，每步含工具与参数模板）。
    steps             JSONB NOT NULL DEFAULT '[]',
    -- ℂ：前置条件。
    preconditions     JSONB NOT NULL DEFAULT '[]',
    -- 𝒯：工具需求。
    tool_requirements JSONB NOT NULL DEFAULT '[]',
    -- 里程碑（命令里程碑 + 阶段里程碑），命中判定是确定性的字符串/正则匹配。
    success_criteria  JSONB NOT NULL DEFAULT '[]',
    -- 已知失败模式（从未触发的里程碑自动转此处）。
    failure_modes     JSONB NOT NULL DEFAULT '[]',
    parameters        JSONB NOT NULL DEFAULT '{}',
    -- 适用范围（资产图谓词的第二组键）。
    applies_to        JSONB NOT NULL DEFAULT '[]',
    state             TEXT NOT NULL DEFAULT 'quarantined'
                      CHECK (state IN ('quarantined','active','retired','tombstone')),
    -- 证据投票 v(s)（Selm 口径）。curator 仲裁时更新。
    confidence        DOUBLE PRECISION NOT NULL DEFAULT 0,
    -- 证据效用。**只作缓存值**，权威值在 strategy_utility 物化视图。
    utility           DOUBLE PRECISION NOT NULL DEFAULT 0,
    -- 运行效用 u(s)（成功里程碑 +1 / 未命中 -1）。子任务归因后更新。
    run_utility       DOUBLE PRECISION NOT NULL DEFAULT 0,
    usage_count       BIGINT NOT NULL DEFAULT 0,
    hit_count         BIGINT NOT NULL DEFAULT 0,
    miss_count        BIGINT NOT NULL DEFAULT 0,
    -- 历史检索总数（淘汰的最小观测前置 n 的分母）。
    retrieval_count   BIGINT NOT NULL DEFAULT 0,
    -- 唯一证人保护：某 confirmed finding 的唯一已知路径 → 不参与任何自动淘汰。
    pinned            BOOLEAN NOT NULL DEFAULT false,
    -- 版本号：edit_existing 升版时 +1（组内快照靠它对齐版本）。
    version           BIGINT NOT NULL DEFAULT 1,
    -- 双时态：冲突不删除而是失效 + 叙述化改写。
    valid_from        TIMESTAMPTZ NOT NULL DEFAULT now(),
    invalid_at        TIMESTAMPTZ,
    -- 硬 TTL 是独立的第二层：到期 → tombstone。
    expired_at        TIMESTAMPTZ,
    retired_at        TIMESTAMPTZ,
    retired_reason    TEXT NOT NULL DEFAULT '',
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- 同名唯一不变量（Selm 的 graveyard 阻挡同名复活的基础）。
-- 表达式唯一索引：company_id 可空，故用 COALESCE(company_id,0) 归一
-- （否则 NULL 互不相等 → 同名可重复插入）。范式同 db/schema.sql 的 uq_task_scope_v2。
-- ✅ D1 已拍板为「按 company_id 隔离」⇒ 本索引维持现状。
-- （若将来改成「全平台共享」，才把本索引改建在 name 上并去掉查询里的
-- company_id 过滤 —— 见计划「已拍板」第 1 条。）
CREATE UNIQUE INDEX IF NOT EXISTS uq_strategies_company_name
    ON strategies(COALESCE(company_id,0), name);
-- 检索注入的索引只覆盖 active：quarantined/retired/tombstone 不参与注入。
CREATE INDEX IF NOT EXISTS idx_strat_state_utility ON strategies(state, utility DESC);
CREATE INDEX IF NOT EXISTS idx_strat_state_conf     ON strategies(state, confidence DESC);
CREATE INDEX IF NOT EXISTS idx_strat_company        ON strategies(company_id);
CREATE INDEX IF NOT EXISTS idx_strat_pinned         ON strategies(pinned) WHERE pinned;
-- activation / applies_to 的匹配谓词编译成 SQL WHERE（不是运行时解释 jsonb）。
-- 每个键一个 GIN：谓词形如 activation->'asset_type' ?| $1::text[]，
-- 直接命中 jsonb_ops 的 ?| 算子。
CREATE INDEX IF NOT EXISTS idx_strat_activation_type_gin
    ON strategies USING GIN ((activation->'asset_type'));
CREATE INDEX IF NOT EXISTS idx_strat_activation_service_gin
    ON strategies USING GIN ((activation->'service_type'));
CREATE INDEX IF NOT EXISTS idx_strat_activation_category_gin
    ON strategies USING GIN ((activation->'category'));
CREATE INDEX IF NOT EXISTS idx_strat_activation_record_gin
    ON strategies USING GIN ((activation->'record_type'));
CREATE INDEX IF NOT EXISTS idx_strat_applies_gin
    ON strategies USING GIN (applies_to);
DROP TRIGGER IF EXISTS trg_strategies_upd ON strategies;
CREATE TRIGGER trg_strategies_upd BEFORE UPDATE ON strategies
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- ---------- strategy_edges ----------
-- 策略之间的关系。写入只在本策略自身变更时发生，**禁止「每次插入都重写邻居」**
-- （反模式 6）。
CREATE TABLE IF NOT EXISTS strategy_edges (
    from_id  BIGINT NOT NULL REFERENCES strategies(id) ON DELETE CASCADE,
    to_id    BIGINT NOT NULL REFERENCES strategies(id) ON DELETE CASCADE,
    relation TEXT NOT NULL CHECK (relation IN ('requires','variant_of','blocks','supersedes')),
    PRIMARY KEY (from_id, to_id, relation)
);
CREATE INDEX IF NOT EXISTS idx_strategy_edges_to ON strategy_edges(to_id, relation);

-- ---------- strategy_evidence ----------
-- 证据链：Graphiti 双向索引的落地。
--
-- ⚠️ **ref_id 是裸 id + 类型，不是级联外键**：如果这里指回 findings /
-- traffic_evidence_snapshots / exploration_nodes，任务归档会连带删掉策略的
-- 证据链 ⇒ 策略变成「凭空提权」。引用完整性由**应用层写入时校验** +
-- content_digest 保底。
--
-- ⚠️ ref_id 是 TEXT 而不是 BIGINT：traffic_evidence_snapshots.id 是 TEXT 主键
-- （db/schema.sql:1162 —— 已按当前 schema 复核），BIGINT 装不下。
CREATE TABLE IF NOT EXISTS strategy_evidence (
    strategy_id    BIGINT NOT NULL REFERENCES strategies(id) ON DELETE CASCADE,
    ref_type       TEXT NOT NULL CHECK (ref_type IN ('finding','traffic_snapshot','activity','tool_call','run')),
    ref_id         TEXT NOT NULL,
    -- 存摘要不复制原始证据：原始证据仍在 findings / traffic_evidence_snapshots /
    -- activity 表里，避免双写与不一致。
    content_digest TEXT NOT NULL,
    role           TEXT NOT NULL DEFAULT 'created_by'
                   CHECK (role IN ('created_by','validated_by','contradicted_by')),
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (strategy_id, ref_type, ref_id, role)
);
CREATE INDEX IF NOT EXISTS idx_strategy_evidence_ref ON strategy_evidence(ref_type, ref_id);
CREATE INDEX IF NOT EXISTS idx_strategy_evidence_role ON strategy_evidence(strategy_id, role);

-- ---------- strategy_utility（物化视图） ----------
-- **权威 utility**：证据链 × finding 当前验证状态。
-- 证据链变化（finding 被撤回 / 判无效）自动反映到 utility ⇒ 这正是
-- 「把 utility 写成静态字段」的缺陷。strategies.utility 只作缓存值。
--
-- ⚠️ JOIN 条件是 f.id::text = se.ref_id（ref_id 是 TEXT，见上）。
--
-- ⚠️ PG **没有** CREATE OR REPLACE MATERIALIZED VIEW：定义要改只能 DROP + CREATE，
-- 而 DROP 会把视图上的**授权**与**索引**一起带走 —— 重建后必须逐条还原授权、
-- 重新建索引，否则要么「改了视图但线上还是老逻辑且没有任何报错」，要么
-- 「视图重建成功但权限悄悄没了」。下面这段 DO 块因此做了三件事：
--   1) 用一个临时普通视图取得【规范化后的】目标定义
--      （pg_get_viewdef 对物化视图与其查询视图产出同一份文本 ⇒ 可逐字比对）；
--   2) 定义一致 ⇒ 什么都不做（保住已刷新的数据）；
--      定义变了 ⇒ 记下旧 ACL → DROP → CREATE → 逐条还原 ACL；
--   3) 索引在下面单独重建（物化视图被 DROP 时它的索引也一起没了）。
DO $$
DECLARE
    v_def      text;
    v_want_def text;
    v_cur_def  text;
    v_acl      text;
    v_grant    record;
BEGIN
    -- 视图定义（**单点真相**）：改这里 ⇒ 下面的定义比对会自动发现差异并重建。
    v_def := $q$
SELECT se.strategy_id,
       avg(CASE WHEN f.status = 'confirmed' THEN 1.0 ELSE 0.0 END) AS utility_raw,
       count(*) AS evidence_count
  FROM strategy_evidence se
  JOIN findings f
    ON se.ref_type = 'finding' AND f.id::text = se.ref_id
 GROUP BY se.strategy_id
$q$;

    -- 取规范化后的目标定义：物化视图与其查询视图的 pg_get_viewdef 文本一致。
    -- 先 DROP 一次：上一次执行若中途失败，临时视图可能残留在同一会话里。
    EXECUTE 'DROP VIEW IF EXISTS pg_temp.strategy_utility_want';
    EXECUTE 'CREATE TEMP VIEW strategy_utility_want AS ' || v_def;
    SELECT pg_get_viewdef('strategy_utility_want'::regclass, true) INTO v_want_def;
    EXECUTE 'DROP VIEW IF EXISTS pg_temp.strategy_utility_want';

    IF to_regclass('public.strategy_utility') IS NULL THEN
        EXECUTE 'CREATE MATERIALIZED VIEW public.strategy_utility AS ' || v_def;
    ELSE
        SELECT pg_get_viewdef('public.strategy_utility'::regclass, true) INTO v_cur_def;
        IF v_cur_def IS DISTINCT FROM v_want_def THEN
            -- 重建会丢授权 ⇒ 先把旧 ACL 抄下来。
            SELECT coalesce(relacl::text, '') INTO v_acl
              FROM pg_class WHERE oid = 'public.strategy_utility'::regclass;
            EXECUTE 'DROP MATERIALIZED VIEW IF EXISTS public.strategy_utility';
            EXECUTE 'CREATE MATERIALIZED VIEW public.strategy_utility AS ' || v_def;
            IF v_acl <> '' THEN
                FOR v_grant IN
                    SELECT pg_get_userbyid(x.grantee) AS grantee,
                           x.privilege_type AS priv,
                           x.is_grantable AS grantable
                      FROM aclexplode(v_acl::aclitem[]) x
                     WHERE x.grantee <> 0
                LOOP
                    -- 物化视图只支持 SELECT 权限；其余一律不还原。
                    -- ⚠️ GRANT 对物化视图用 **ON TABLE**（PG 的 GRANT 语法里
                    -- 没有 ON MATERIALIZED VIEW，只有 CREATE/ALTER 有）。
                    IF v_grant.priv = 'SELECT' THEN
                        EXECUTE format(
                            'GRANT SELECT%s ON TABLE public.strategy_utility TO %I',
                            CASE WHEN v_grant.grantable THEN ' WITH GRANT OPTION' ELSE '' END,
                            v_grant.grantee);
                    END IF;
                END LOOP;
            END IF;
        END IF;
    END IF;
END $$;
-- REFRESH ... CONCURRENTLY 需要唯一索引；没有它就只能非并发刷新。
-- 物化视图被 DROP 时这个索引也一起没了 ⇒ 每次执行本文件都重建（IF NOT EXISTS 幂等）。
CREATE UNIQUE INDEX IF NOT EXISTS idx_strategy_utility_id ON strategy_utility(strategy_id);

-- ---------- L3 反向索引（不新建图引擎，只加索引） ----------
--
-- 为什么反查要落在 L3 这一层：策略的证据链指着一条 finding，而「这条漏洞是从
-- 哪个 intent / fact / asset 来的」住在探索图里。finding 节点与 finding 行是
-- 一对一的（findings.node_id），但**流量快照的绑定关系**只存在于
-- finding_traffic_bindings —— 把它抄进节点 payload 并加索引，反查就从「文本
-- 搜索」变成「索引查询」，且**不引入新图引擎**（Sir 2026-10-29 的明确边界）。

-- finding → node：findings.node_id 当前【无索引】，反查会全表扫。
-- ⚠️ 索引归属：findings 表由别的单元持有，但本文件由本单元拥有，
-- EnsureStrategySchema 会应用它；schema 属主若愿意，可把它并进 db/schema.sql
-- （两边都幂等，重复应用无副作用）。
CREATE INDEX IF NOT EXISTS idx_findings_node
    ON findings(node_id) WHERE node_id IS NOT NULL;

-- node → finding：payload 里的 finding_id 走表达式 B-tree，等值查可命中。
-- ⚠️ 用文本表达式而不是 (payload->>'finding_id')::bigint：payload 是 agent 可写
-- 的 JSONB，非数字值会让 ::bigint 在**建索引时**直接抛错，把整次迁移打挂。
-- 读取侧因此按文本比较（payload->>'finding_id' = $1::text）。
CREATE INDEX IF NOT EXISTS idx_exploration_nodes_finding_ref
    ON exploration_nodes ((payload->>'finding_id'))
    WHERE kind = 'finding' AND payload ? 'finding_id';

-- node → traffic 快照：数组用 GIN + @> containment 查。
-- jsonb_path_ops 只支持 @>（包含），正是这里要的算子。
CREATE INDEX IF NOT EXISTS idx_exploration_nodes_traffic_ref
    ON exploration_nodes USING GIN ((payload->'traffic_ids') jsonb_path_ops)
    WHERE kind = 'finding' AND payload ? 'traffic_ids';

-- ---------- exploration_nodes.strategy_id：打通「探索图」与「策略图」 ----------
--
-- 为什么需要：strategy_usage 记录的是「策略 ↔ 探索 ↔ 意图」这一层
-- （db/strategy_proposal.go），粒度到 exploration / intent。但**节点级**没有链接
-- ⇒ 问不出「这条 finding 是哪条策略产出的」，只能回答「这个 exploration 跑过
-- 哪些策略」。而一个 exploration 里常常同时跑多条策略 ⇒ exploration 级不够。
--
-- 为什么是真外键而不是把 id 塞进 payload：payload 里存文本 id 没有引用完整性
-- —— 策略被删后节点上还留着悬空字符串，反查只能靠文本扫描且可能命中同名字符串。
--
-- ON DELETE SET NULL：策略被删只丢链接，不删节点。节点是事实与结论，策略是
-- 可替换的工作方法 ⇒ 方法没了，结论不该跟着消失。
--
-- ⚠️ db/schema.sql 由别的单元持有（RESERVED），所以列与约束都在本文件加，
-- 由 EnsureStrategySchema 应用（幂等；本文件与本单元同属策略库）。
--
-- ⚠️ **历史行不回填**：现有节点没有可靠的「属于哪条策略」依据 ——
-- strategy_usage 只有 exploration/intent 粒度，而节点 payload 里没有意图键；
-- 硬把一个 exploration 的所有节点归给其中某一条策略 = 安静的错误归属。
-- ⇒ 历史行保持 NULL（未知就是未知），新节点由写入路径按轮次填。
ALTER TABLE exploration_nodes
    ADD COLUMN IF NOT EXISTS strategy_id BIGINT;

-- 外键用 DO 块做幂等（Postgres 没有 ADD CONSTRAINT IF NOT EXISTS）。
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint WHERE conname = 'fk_exploration_nodes_strategy'
    ) THEN
        ALTER TABLE exploration_nodes
            ADD CONSTRAINT fk_exploration_nodes_strategy
            FOREIGN KEY (strategy_id) REFERENCES strategies(id) ON DELETE SET NULL;
    END IF;
END $$;

CREATE INDEX IF NOT EXISTS idx_exploration_nodes_strategy
    ON exploration_nodes(strategy_id) WHERE strategy_id IS NOT NULL;

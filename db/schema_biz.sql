-- 业务建模（第三张图）——与 db/schema.sql 并行应用的幂等 DDL。
--
-- 为什么单独成文件：db/schema.sql 是 RESERVED 文件（见 db/schema_loop.sql 同款
-- 纪律），本单元的新表一律走独立幂等文件，由 db/business_graph.go 的
-- EnsureBizSchema 在启动期应用（CREATE TABLE IF NOT EXISTS，重复应用零副作用）。
--
-- 五张表回答业务建模的数据面：
--   biz_nodes          —— 业务节点：实体/动作/状态/角色/信任/数据流/外部系统，
--                         分 design（系统声称自己是什么）/ behavior（系统实际上是
--                         什么）两层。confidence 用 observed|inferred 二元口径，
--                         与探索图 fact 节点 payload 的 confidence 同一语言
--                         （agent/tools.go record_fact 的参数描述），跨图比对
--                         不另起一套浮点体系。
--   biz_edges          —— 业务边：owns/invokes/depends_on/trusts/flows_to/
--                         validates/requires_state/transitions/grants。grants
--                         表达「角色乘资源乘操作」的授予关系——权限矩阵没有它
--                         连不了角色与动作。
--   biz_anchors        —— 资产锚点：与 exploration_anchors 同构（node_id +
--                         asset_id 复合主键）。锚点既是血缘也是范围读的授权边
--                         （db/exploration.go 同款语义）：业务节点只有在它锚定
--                         的每一个资产都落在调用者范围内时才可见。
--   biz_layer_diffs    —— 双层差异：design 与 behavior 配对后的八类差异，每条
--                         自带 WSTG-BUSL 编号与 STRIDE 字母，可直接进报告与
--                         审计口径。status: open（待处理）→ pushed（已派生
--                         intent，behavior_ref.intent_id 非空）→ confirmed /
--                         false_positive（人工裁决）。
--   biz_model_versions —— 建模快照版本：增量建模前落一份可回滚的快照。
--
-- ⚠️ 与既有表的关系刻意**不带外键**（biz_nodes.company_id / task_id 除外，它们
-- 是作用域语义不是血缘语义）：业务推断的证据链靠 biz_nodes.props 里的 fact_id
-- 引用（探索图 fact 节点），引用是逻辑的而不是物理的——任务归档/假删除不应级联
-- 抹掉业务图的推断史。

-- ---------- biz_nodes：业务节点 ----------
CREATE TABLE IF NOT EXISTS biz_nodes (
    id            BIGSERIAL PRIMARY KEY,
    company_id    BIGINT NOT NULL REFERENCES companies(id) ON DELETE CASCADE,
    task_id       BIGINT REFERENCES tasks(id) ON DELETE SET NULL,
    layer         TEXT NOT NULL CHECK (layer IN ('design','behavior')),
    node_type     TEXT NOT NULL CHECK (node_type IN (
                     'biz_entity','biz_action','biz_state','biz_role',
                     'biz_trust','biz_dataflow','biz_external')),
    name          TEXT NOT NULL,
    raw_name      TEXT NOT NULL DEFAULT '',
    description   TEXT NOT NULL DEFAULT '',
    -- props 约定（逻辑引用，不建外键）：
    --   fact_id     —— 依据的探索图 fact 节点 id（每条推断可回溯到 fact）
    --   asset_id    —— 主锚资产的冗余快照（真锚在 biz_anchors）
    --   raw_refs    —— 其他自由引用（路由名、权限码等）
    props         JSONB NOT NULL DEFAULT '{}',
    confidence    TEXT NOT NULL DEFAULT 'inferred'
                  CHECK (confidence IN ('observed','inferred')),
    source        TEXT NOT NULL DEFAULT 'agent'
                  CHECK (source IN ('agent','import','user')),
    -- 增量建模的失效指针：被证伪/被更细节点取代时由宿主侧写（见
    -- MarkBizNodeSuperseded），普通 upsert 不碰它。
    superseded_by BIGINT REFERENCES biz_nodes(id) ON DELETE SET NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_biz_nodes_company ON biz_nodes(company_id, node_type);
CREATE INDEX IF NOT EXISTS idx_biz_nodes_layer   ON biz_nodes(company_id, layer);
CREATE INDEX IF NOT EXISTS idx_biz_nodes_task    ON biz_nodes(task_id) WHERE task_id IS NOT NULL;
-- 去重键（UpsertBizNodeInScope 的 ON CONFLICT 目标）：同公司同层同类的同名节点
-- 只留一行（重复建模是更新不是新增）；部分索引 WHERE superseded_by IS NULL——
-- 失效节点不占名字（同名重建合法）。
CREATE UNIQUE INDEX IF NOT EXISTS uq_biz_node_ident ON biz_nodes(company_id, layer, node_type, lower(name))
    WHERE superseded_by IS NULL;

-- ---------- biz_edges：业务边 ----------
CREATE TABLE IF NOT EXISTS biz_edges (
    id            BIGSERIAL PRIMARY KEY,
    company_id    BIGINT NOT NULL REFERENCES companies(id) ON DELETE CASCADE,
    task_id       BIGINT REFERENCES tasks(id) ON DELETE SET NULL,
    layer         TEXT NOT NULL CHECK (layer IN ('design','behavior')),
    src_id        BIGINT NOT NULL REFERENCES biz_nodes(id) ON DELETE CASCADE,
    dst_id        BIGINT NOT NULL REFERENCES biz_nodes(id) ON DELETE CASCADE,
    rel           TEXT NOT NULL CHECK (rel IN (
                     'owns','invokes','depends_on','trusts','flows_to',
                     'validates','requires_state','transitions','grants')),
    props         JSONB NOT NULL DEFAULT '{}',
    confidence    TEXT NOT NULL DEFAULT 'inferred'
                  CHECK (confidence IN ('observed','inferred')),
    superseded_by BIGINT REFERENCES biz_edges(id) ON DELETE SET NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT ck_biz_edge_noself CHECK (src_id <> dst_id)
);
CREATE INDEX IF NOT EXISTS idx_biz_edges_company ON biz_edges(company_id, rel);
CREATE INDEX IF NOT EXISTS idx_biz_edges_src     ON biz_edges(src_id);
CREATE INDEX IF NOT EXISTS idx_biz_edges_dst     ON biz_edges(dst_id);
CREATE INDEX IF NOT EXISTS idx_biz_edges_task    ON biz_edges(task_id) WHERE task_id IS NOT NULL;
-- 去重键（UpsertBizEdgeInScope 的 ON CONFLICT 目标）：同公司同层同两端同关系的
-- 边只留一条；失效边不占位（同名关系重建合法）。
CREATE UNIQUE INDEX IF NOT EXISTS uq_biz_edge_ident ON biz_edges(company_id, layer, src_id, dst_id, rel)
    WHERE superseded_by IS NULL;

-- ---------- biz_anchors：资产锚点（与 exploration_anchors 同构） ----------
CREATE TABLE IF NOT EXISTS biz_anchors (
    node_id  BIGINT NOT NULL REFERENCES biz_nodes(id) ON DELETE CASCADE,
    asset_id BIGINT NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
    PRIMARY KEY (node_id, asset_id)
);
CREATE INDEX IF NOT EXISTS idx_biz_anchors_asset ON biz_anchors(asset_id);

-- ---------- biz_layer_diffs：双层差异 ----------
CREATE TABLE IF NOT EXISTS biz_layer_diffs (
    id            BIGSERIAL PRIMARY KEY,
    company_id    BIGINT NOT NULL REFERENCES companies(id) ON DELETE CASCADE,
    task_id       BIGINT REFERENCES tasks(id) ON DELETE SET NULL,
    diff_type     TEXT NOT NULL CHECK (diff_type IN (
                     'missing_validation','extra_permission','state_bypass',
                     'trust_violation','unexpected_flow','timing_anomaly',
                     'unbounded_consumption','workflow_skip')),
    severity      TEXT NOT NULL DEFAULT 'medium'
                  CHECK (severity IN ('critical','high','medium','low')),
    -- design_ref / behavior_ref：两侧的图证据（节点/边 id 数组 + 摘要）。
    -- behavior_ref.intent_id 非空 = 已派生探索图意图（status=pushed 的判定源）。
    design_ref    JSONB NOT NULL DEFAULT '{}',
    behavior_ref  JSONB NOT NULL DEFAULT '{}',
    description   TEXT NOT NULL DEFAULT '',
    -- 外部标准映射：WSTG-BUSL 编号（如 WSTG-BUSL-02）与 STRIDE 字母（T/E/S/I/D）。
    -- 让差异在标准演进后仍对得上号，产出直接进报告与审计口径。
    wstg_ref      TEXT NOT NULL DEFAULT '',
    stride_ref    TEXT NOT NULL DEFAULT '',
    status        TEXT NOT NULL DEFAULT 'open'
                  CHECK (status IN ('open','pushed','confirmed','false_positive')),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_biz_diffs_company ON biz_layer_diffs(company_id, severity);
CREATE INDEX IF NOT EXISTS idx_biz_diffs_task    ON biz_layer_diffs(task_id) WHERE task_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_biz_diffs_status  ON biz_layer_diffs(company_id, status);

-- ---------- biz_model_versions：建模快照版本 ----------
CREATE TABLE IF NOT EXISTS biz_model_versions (
    id             BIGSERIAL PRIMARY KEY,
    company_id     BIGINT NOT NULL REFERENCES companies(id) ON DELETE CASCADE,
    task_id        BIGINT REFERENCES tasks(id) ON DELETE SET NULL,
    version        INT NOT NULL,
    snapshot       JSONB NOT NULL DEFAULT '{}',
    change_summary TEXT NOT NULL DEFAULT '',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (company_id, version)
);

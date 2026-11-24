# 知识引擎与 SIM / CRSI 的边界契约（§0.9，批次 TI-09）

> 版本：0.15.67（2026-11-24）｜任务卡：TI-09｜性质：边界契约审计 + 现状映射
> 结论先行：**三种"学习"的现状载体已可界定，边界禁止事项可落到现有确定性防线；SIM/CRSI 本体尚不存在**（BASE-00 审计：CRSI 全部 ❌），其接入是 §0.13 阶段 TI-6 的后续任务，本卡交付边界契约并给出接入时的硬约束。

## 1. 三系统 × 现状载体（§0.9 表格映射）

| §0.9 系统 | 学习对象 | 本仓库现状载体（证据） | 边界判定 |
|---|---|---|---|
| Knowledge Engine / Methodology Registry | 如何测试目标系统 | `edge_methodologies` 表 + 内置本体常量（db/schema_edge2.sql、db/edge_methodology.go）；策略库 `strategies/strategy_edges/strategy_evidence`（db/schema_strategy.sql）；Peak Search 蒸馏（0.15.38） | ✅ 载体成立；禁止事项见 §2 |
| Business Model Store | 目标系统如何工作 | `biz_nodes/biz_edges/biz_anchors/biz_layer_diffs/biz_model_versions`（db/schema_biz.sql），company/task 双维度隔离 | ✅ 载体成立；隔离已结构化（company_id NOT NULL + task_id 可空） |
| SIM / CRSI | reArtex Harness 为什么失败、如何改进 | **不存在**。`eval/` 仅有 Veto 黄金基线套件（suite.json 15 个 golden 任务 + collect_baseline.py + `veto compare`），是 Harness 级评估的前身而非 CRSI | ❌ 未建；接入约束见 §3 |

## 2. 禁止事项 → 现有确定性防线（逐条落位）

| §0.9 禁止事项 | 防线落位 | 状态 |
|---|---|---|
| Knowledge Engine 不直接修改 Agent Harness 安全策略 | `rsi_guard.py` Tier-0/Tier-1/Tier-2 分层闸（scripts/rsi_guard.py；guard/ netguard/ pathguard/ internal/sod 等 Tier-0 目录清单见 docs/安全不变量清单.md）；方法论/策略库表无任何 Harness 配置写路径 | ✅ 结构性满足 |
| Business Model 不把推测当事实 | facts 表 `fact_kind` 强制区分 observed/inferred（0.15.63）；biz 差异只能转「待验证假设」（TI-04 `DeltaShouldBecomeHypothesis`：建议 ≠ 结论） | ✅ 0.15.63/62 起满足 |
| Business Model 不跨目标污染模型 | biz_* 全表 company_id 维度 + task_id 引用；跨任务共享只走资产实体（TI-12 口径，API 层执行归 TI-11） | ✅ 结构满足；API 层待接线 |
| SIM 不把单个目标的漏洞细节当通用 Harness 规则 | SIM 未建；接入时必须做到：SIM 摘要只收 Harness 级字段（工具合同失败/上下文丢失/收口错误/能力路由不当/评测回归），目标请求正文/Cookie/凭据**禁止**进入 SIM 记录——/events payload 消毒（TI-12）是同一道闸 | 📋 接入前置条件（写入 §3） |
| CRSI 不修改 Tier-0 | rsi_guard Tier-0 清单 + `--enforce` 模式；CRSI 候选修改只允许作用于非 Tier-0 组件，Evolution Guard 由外部可信机制保护（方案口径） | ✅ 闸已备（观察模式），CRSI 未建 |

## 3. SIM/CRSI 接入时的硬约束（后续 TI-6 阶段的设计输入）

1. **数据最小化**：SIM 只收 Harness 级诊断字段 + 脱敏摘要；目标敏感数据（请求正文、Cookie、Authorization、业务模型正文）不得进入 SIM 存储——复用 evidence_refs 受控引用模式（0.15.63），正文留原地、只传引用与 hash。
2. **经验复用边界**：Harness 经验可跨任务/模型复用；业务方法论（edge_methodologies/strategies）与业务模型（biz_*）**不得**进入 CRSI 训练/候选面——两库表结构上与 SIM 无外键关联，接错要过人审。
3. **事件口径**：`harness.regression.detected`（events 包 0.15.64）是 SIM 的唯一入队通道之一，payload 走统一消毒；golden/holdout 基线（eval/）纳入时按 §0.13 集成测试 #8 验收（收摘要、不收敏感正文）。
4. **Tier-0 红线**：CRSI 的任何候选修改，rsi_guard 从观察转 enforce 后必须过 Tier-0 零改动校验；该红线不因 CRSI 存在而放宽。

## 4. 本批不做

- 不实现 SIM/CRSI 本体（属 §0.13 TI-6 阶段后续卡）；
- 不改 eval/ 黄金基线工具链（I-08 已有，其扩展归对应卡）;
- 不动 amber/veto/peaksearch 现有实现（I-04~I-07 已按各自边界运行）。

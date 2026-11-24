# Go 模块边界：方案建议树 × 仓库现状映射（§0.10，批次 TI-10）

> 版本：0.15.68（2026-11-24）｜任务卡：TI-10｜性质：边界审计与落位决策记录
> §0.10 原文口径：「以上是建议边界，不代表这些目录已存在。落码前应先检查现有 server/、agent/、db/、edge、业务建模和事件相关包，能复用则复用，避免重复建立同名能力。」

## 1. 建议树逐行映射

| §0.10 建议位置 | 仓库现状（0.15.68） | 落位决策与理由 |
|---|---|---|
| `taskintelligence/context.go` | `internal/taskintelligence/context.go`（0.15.60） | ✅ 一致 |
| `taskintelligence/coordinator.go` | 未建（能力选择建议在 `scheduling.go` 0.15.65；调用编排未建） | 编排=接线期产物，归 TI-11 第二阶段；不预建空壳 |
| `taskintelligence/capability.go` | `internal/taskintelligence/capability.go`（0.15.61） | ✅ 一致 |
| `taskintelligence/envelope.go` | `collaboration.go`（TI-04 台账/幂等键）+ `domain.go`（CapabilityExecution 契约，0.15.63） | 方案的 envelope 语义拆落两处：协作幂等在 collaboration，执行记录契约在 domain——避免同语义双文件 |
| `taskintelligence/event_handler.go` | `events/dispatcher.go`（0.15.64）+ 各能力的事件消费方未建 | 派发器独立成根级 `events/` 包（见下行）；消费方随各能力接线落地 |
| `domain/hypothesis.go` 等 5 文件 | `internal/taskintelligence/domain.go`（0.15.63，契约+校验+状态机）+ `db/task_intelligence.go`（存储投影） | **不单独建 domain/ 包**：本仓库惯例是 db 行结构在 db 包、领域契约在 taskintelligence，两套字段同名同义，转换在 server 接线层——三处分工替代一个新包，避免平行分类学 |
| `exploration/edge_adapter.go` | 现有 `server/edge_tools.go`（9 工具族）+ `db/edge_run.go` 即适配层 | **复用**：偏锋已有完整工具/存储面，§0.14 映射口径是「适配不重建」；独立 adapter 包仅在接线期需要统一调用入口时再抽 |
| `businessmodel/capability_adapter.go` | 现有 `server/biz_tools.go` + `db/business_graph.go`（biz 五表访问层） | 同上，复用 |
| `events/event.go` `outbox.go` `dispatcher.go` | `events/event.go`（envelope+类型常量）、`db/events.go`（outbox 存储）、`events/dispatcher.go`（派发器）（0.15.64） | ✅ 语义一致；outbox 存储按仓库惯例放 db 包（SQL 不散落包外） |
| `db/task_intelligence.go` + `schema_task_intelligence.sql` | 两文件均在（0.15.63 起） | ✅ 一致 |

## 2. 边界纪律（既有事实的成文化）

1. **依赖方向**：`events → db`（派发器消费 outbox 行）；`server → taskintelligence/events/db`；`db` 不 import 任何 TI/events 包；`internal/taskintelligence` 保持零 db 依赖（契约包，窄接口由 server 注入实现）。
2. **SQL 位置**：全部 SQL 留在 `db/`；契约/规则包不出现连接串——TI-02/03/04/07/06 均遵守（测试用 fake 或 testdb）。
3. **Tier-0 不涉**：本批新增的 internal/taskintelligence、events、db 追加文件均不在 rsi_guard Tier-0 清单（guard/netguard/pathguard/internal/sod*/third_party/norma/patches/selfupdate），属 Tier-1/2 常规演进。
4. **不预建空目录**：coordinator.go / adapter 包 / 事件消费方等接线期产物，等依赖就绪再落（避免死代码与误导性骨架）。

## 3. 本批不做

- 不迁移/重命名任何现有包（server/agent/db 的既有结构不动）；
- 不建 domain/、exploration/、businessmodel/ 空包；
- 不给 events/ 加消费方注册（生产方/消费方接线归 TI-11 及后续）。

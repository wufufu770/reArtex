**reArtex v0.15.11** —— 自 v0.13.1 以来的累积发布：真 LLM 实战淬炼批次（SoD 链 15 处修复）+ 偏锋探索重写 + 方法论全量可编 + 业务建模闭环 + 交付质量设施。

## 偏锋探索（独立页重写 + 实战淬炼）

- 独立页 `/function/pianfeng`：可直接布置任务（REST `POST /api/edge/run` 与 edge_run 工具共用宿主链）；运行记录实数据（状态徽章/判据进度/停止史/跳任务详情/运行中可停止，进行中 5s 轻轮询）；方法论行点开看完整内容。
- 方法论全量可编：内置条目放开本体编辑（名称/档位/证据级/判据/说明/标签/优先级，注册期同套硬约束），存 builtin-override 可随时恢复默认；自定义条目同套校验；配置写穿运行时注册表——启停/优先级/本体改动立即进入轮换与判定。
- 触发器驱动 bizmodeler 业务建模（on_finding / on_task_create）。

## 业务建模

- 建模→发现实战闭环：design（声称）/behavior（实际）双层差异自动转为发现（首例即支付金额篡改 critical×2）。
- 图读侧修复：任务视图与公司视图（显式 company_id 即授权上下文，无上下文依旧 fail-closed）恢复可用；画布风格对齐全站主题。

## SoD 链（真 LLM 实战淬炼，15 处修复）

- 意图规整多意图契约 1..N：复合命令按子动作拆分逐条过闸，全部放行才放行原始动作；标量/嵌套布尔/数字数组元素三类畸形自动矫正（仅解码失败路径触发）。
- 范围判定与本地操作平衡：任务工作区前缀部署可移植（SetTaskDirPrefix）；Write/Edit 落任务目录豁免；纯本地命令与复合本地子段 scope 按构造通过（no_egress_local / compound_local_segment / compound_evaluated，理由进审计）。
- report_finding 入任务内写清单（发现不再被复核链误拦）；traffic 读侧与 SearchExtraTools 入免闸清单；越界拒绝理由带上被拒目标。
- 压缩链 thinking 硬编码清除（冷图压缩/主会话压缩/对抗复核裁判/侧问摘要 4 处）——「必须 adaptive thinking」的模型可用。
- 规整提示词领域校准：anomaly 判定对象是绕过审查企图，不是授权目标上的攻击载荷。
- llmpool 硬熔断按类别（auth/quota/missing）给出准确处置指引；复核过期闸 TTL 可配（`sod_review_ttl_hours`，默认 24 小时）。

## 跨模型兼容（过拟合审计）

- 请求形状全部由 profile 配置驱动（thinking/effort/stream/max_tokens 字段/上下文窗口），无模型名特化分支。
- OpenAI 格式 reasoning 族（max_completion_tokens 配置信号）自动省略 temperature——o1/o3/gpt-5 族拒绝该参数。
- 多意图契约对单意图模型同样成立；三类畸形矫正仅在解码失败路径触发，规范输出零影响。

## 交付质量

- e2e 冒烟（45 路由 + legacy/尾斜杠口径）进 CI；precommit 执行化开发纪律；backup.sh（pg_dump+工作目录+审计链头导出）；release 产物 SBOM + build provenance。

## 兼容与升级

- 全新库首启自动播种：agent 全家（20）、工具 105、方法论注册表（代码常量）、任务类别、拦截规则；LLM 配置需在 LLM 页重新填写（首访引导设置登录口令）。
- 新设置键：`sod_review_ttl_hours`（对抗复核过期闸，默认 24 小时）。
- 数据库迁移全自动；无手工步骤。

## 验证

- 全部修复带单测或真库集成测试（规整多意图/毒丸出队/复合裁决放行/业务图读取/注册表覆盖恢复等回归钉）；go build/vet、tsc、biome 全绿；e2e 冒烟 45 路由本地实跑通过。
- 真 LLM 靶场 campaign 实战全程验证：34 条发现（critical 7）/21 类别/2863 条录制流量，SoD 裁决与审计全量可回放。
- 镜像交付门（CI 内建）：编译+vet+全量测试通过才产出二进制；ghcr 多架构镜像发布后冒烟——真实起服务、/api/health 版本核对、环境块洁净性验收。

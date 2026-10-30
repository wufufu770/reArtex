# PolicyGate — 结构化意图裁决者

> **形态说明（实现时必须遵守）**：本文件的裁决规则**全部是确定性的**（scope 检查、
> tier2 一致性、工具熔断、额度状态、anomaly 标记），因此 **policy gate 不是一个
> LLM agent** —— 它的裁决逻辑写在**代码**里，不是提示词里。
> 这与整条链路的铁律一致：**模型只提议，确定性层裁决**。
> 若将来确实需要为 `needs_review` 生成自然语言的拒绝理由或对抗行为描述，
> 再补一个**只做措辞、不参与裁决**的轻量调用；那时它才需要提示词，
> 而裁决本身仍不得由模型做出。
> 本文件在此作为**确定性闸门的规范**存在：实现者按本文档写代码，测试按本文档断言。

## 身份

你位于 intent-normalizer 与 executor 之间。你接收结构化意图，输出
`approve` / `deny` / `needs_review` 三种裁决之一。你**不执行**动作，
**不修改**意图，**不解析** shell 语法——你只基于确定性检查结果做裁决。

## 铁律

1. 你不执行任何动作。你只有 `emit_verdict` 一个输出工具。
2. 你不解析 shell 语法。你只读 `structured_intent` 的字段。原始 shell 字符串
   在你的裁决逻辑中**不可见**——这是 Separation of Duties 的核心：
   裁决层永远不解析 shell。
3. 你的裁决依据只能来自以下确定性检查，不得来自主观判断：
   - `scope_check`：`target_hint` 是否在本任务 `task_scope` 内（调用
     `db/scope.go` 的共享 scope 谓词，不自行实现）；
   - `tier2_check`：如果该意图关联的 finding 已存在，检查其
     `replay_evidence_id` 是否非空且资产归属一致；
   - `tool_health_check`：目标工具是否处于 `open` 熔断状态；
   - `anomaly_check`：`intent_normalizer` 的 `anomaly_flag` 是否为 true；
   - `quota_check`：群组额度是否处于 `draining` / `parked` 状态。
4. 裁决规则（按顺序，短路）：
   - `scope_check` 失败 → `deny`，`reason='out_of_scope'`；
   - `tool_health_check` 失败 → `deny`，`reason='tool_circuit_open'`；
   - `quota_check` 失败 → `deny`，`reason='quota_exhausted'`；
   - `tier2_check` 失败 → `deny`，`reason='tier2_inconsistent'`；
   - `anomaly_check` 为 true → `needs_review`，`reason='anomaly_flagged'`；
   - 全部通过 → `approve`。
5. `needs_review` 不进入人工队列。它进入 `adversarial_review_pending`，
   由 `redteam` agent 在下一轮读取并决定是否发起证伪。你不等人工。
6. 你不读、不引用、不修改原有 agent 的提示词或会话上下文。

## 输入

每轮接收一条 `structured_intent`，附带：

- 当前任务的 `task_scope`（通过共享 scope 谓词查询，不读原始行）；
- 关联 finding 的 `replay_evidence_id` 和 `asset_id`（如果有）；
- 目标工具的当前健康状态；
- 群组额度状态。

## 裁决流程

1. 调用 `db.ScopeFragmentFor(task_id)` 获取 scope 谓词，检查
   `structured_intent.target_hint` 是否满足。不满足 → `deny`。
2. 如果意图关联 finding，调用 `check_tier2_consistency(finding_id)`，
   检查 `replay_evidence_id` 非空且资产归属一致。不一致 → `deny`。
3. 调用 `check_tool_health(executable)`。工具处于 `open` 熔断 → `deny`。
4. 调用 `check_group_quota(task_id)`。额度 `draining` / `parked` → `deny`。
5. 如果 `anomaly_flag=true` → `needs_review`，写入 `adversarial_review_pending`，
   通知 `redteam`。
6. 全部通过 → `approve`，生成一次性执行凭证（绑定 `raw_payload_digest`），
   交给 executor。

## 输出格式

```json
{
  "verdict": "approve | deny | needs_review",
  "reason_code": "scope_ok | out_of_scope | tier2_inconsistent | tool_circuit_open | quota_exhausted | anomaly_flagged | all_checks_passed",
  "checks": {
    "scope_check": "pass | fail",
    "tier2_check": "pass | fail | not_applicable",
    "tool_health_check": "pass | fail",
    "quota_check": "pass | fail",
    "anomaly_check": "clean | flagged"
  },
  "execution_token": "<仅 approve 时生成，绑定 raw_payload_digest>",
  "structured_intent_digest": "<意图的哈希，用于审计>",
  "note": "<一句话说明，不得包含主观评价>"
}
```

## 一次性凭证约束

`approve` 时生成的 `execution_token` 必须：

1. 绑定到 `structured_intent.raw_payload_digest`——executor 执行时校验
   实际字节的哈希是否与 token 绑定的一致，不一致则拒绝执行；
2. 单次有效——executor 执行后 token 立即失效；
3. 不可转让——token 与 `task_id` + `source_agent` 绑定。

这是 Separation of Duties 的“批准是一次性凭证，绑定到将要执行的确切字节”。

## 禁止事项

- 禁止解析 `structured_intent` 之外的原始 shell 字符串。
- 禁止自行实现 scope 判定——必须调用 `db/scope.go` 的共享谓词。
- 禁止因为 `anomaly_flag=true` 就直接 `deny`——anomaly 是 `needs_review`，
  不是拒绝。拒绝会丢失“目标侧对抗行为”的检测信号。
- 禁止把 `needs_review` 推给人工队列——它进入 `adversarial_review_pending`，
  由 `redteam` 处理。
- 禁止在 `note` 中写主观评价。

## 失败处理

- scope 查询失败 → `deny`，`reason_code='scope_check_unavailable'`。
- tool health 查询失败 → `deny`，`reason_code='tool_health_unavailable'`
  （fail-closed）。
- quota 查询失败 → `deny`，`reason_code='quota_check_unavailable'`
  （fail-closed）。

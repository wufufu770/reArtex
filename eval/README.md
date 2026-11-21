# Veto 基线录制（批次 E2）

Golden holdout 套件 + baseline 录制流程——后续一切改动（提示词/技能/工具接线）
在发布前跑 `veto compare` 对照本基线，`n_10=0` 才算无回归。

## 组成

| 文件 | 说明 |
|---|---|
| `suite.json` | 15 个 golden 任务定义（reartex_pentest_v1 格式，Veto schema 校验过）。四维度分配与 skills/ 四审计技能同构：污点 ×4 / 越权认证 ×4 / 配置暴露 ×4 / 综合复测 ×3 |
| `collect_baseline.py` | 聚合 data/veto/<taskID>/responses.json → Veto responses.json + identity.json（套件 id 对齐 + 同一构建校验） |

## 录制流程（需要已配置 LLM + 授权靶场）

1. **建任务**：按 suite.json 的 15 个任务逐个建 reArtex 任务——任务命名用
   **套件 id**（`taint_sqli_orders` 等，聚合器按目录名对齐）；目标写进任务书，
   授权面按任务类型选（污点/越权类建议 `authorized_active`，配置类可
   `passive_recon`）。
2. **等终态**：任务跑完落 done/failed/timeout——exporter（0.15.32）在终态
   自动把六字段 responses 写到 `data/veto/<taskID>/`。
3. **聚合**：`python3 eval/collect_baseline.py --data-dir data --out /tmp/baseline`
   （有任务未终态会报清单退出；跨构建混录会拒绝）。
4. **打分**：
   ```bash
   cd veto-eval && .venv/bin/python -m veto.eval_core score \
     --suite ../eval/suite.json --responses /tmp/baseline/responses.json \
     --identity /tmp/baseline/identity.json --out /tmp/baseline/baseline.record.json
   ```
5. **签名归档**：`EVAL_TRUST_KEY=<32字节随机> .venv/bin/veto` 无独立 sign 子命令——
   record 由 score 产出；签名在 `veto compare --require-recompute` 的 recompute
   路径中校验 digest。**归档**：把 baseline.record.json + suite.json + responses
   一起存 `eval/baseline/`（带构建版本号目录），作为后续 compare 的 baseline 侧。
6. **回归对照**（任何改动发布前）：
   ```bash
   cd veto-eval && .venv/bin/python -m veto.eval_core compare \
     --baseline eval/baseline/<ver>/baseline.record.json \
     --candidate /tmp/baseline/baseline.record.json \
     --suite ../eval/suite.json \
     --baseline-responses eval/baseline/<ver>/responses.json \
     --candidate-responses /tmp/baseline/responses.json \
     --margin 0.05 --stat-method paired_exact --require-recompute \
     --assume-independent --out /tmp/gate.json
   ```
   `release_eligible=true 且 n_10=0` = 无回归；否则改动回滚或重录。

## 现状说明

当前环境无 LLM 配置 + 无授权靶场——**首次录制待条件具备后执行**（步骤 1-5）。
工具链（suite 定义/聚合器/CLI）已就绪并经 Veto schema 校验（15/15 合法）；
手动探针（空 responses score）已验证 CLI 全链可跑。

-- SoD 链的实测统计（部署后必须跑一次）。
--
-- 为什么单独一个脚本：计划《2026-10-30-task-hall-sod-chain.md》风险节明文要求
-- 「**实施后必须实测一次 p50/p95 的单次工具调用耗时**（用 sod_decisions.latency_ms
-- 列即可）」⇒ 这条要求不能靠单元测试交差（单元测试里的 provider 是替身，测不出
-- 真实的模型往返延迟）。
--
-- 用法（开发实例）：
--   docker compose -p artexdev -f docker-compose.yml -f docker-compose.override.yml \
--     -f docker-compose.dev.yml exec -T postgres psql -U artex -d artex -f - < scripts/sod-latency-stats.sql
--
-- ⚠️ 分组是这张脚本的关键：延迟的主要成分是规整器的**同步 LLM 调用**（计划估 1~4 秒）。
-- 缓存命中与否决定了这次调用有没有付这个代价 ⇒ 混在一起统计会把「链本身的代码开销」
-- 和「模型往返」搅成一锅粥，看不出任何可行动的信息。

\echo '=== 1. 样本量与时间窗 ==='
SELECT
  count(*)                  AS 决策总数,
  min(created_at)           AS 最早,
  max(created_at)           AS 最近,
  count(*) FILTER (WHERE cache_hit)      AS 缓存命中,
  count(*) FILTER (WHERE NOT cache_hit)  AS 未命中
FROM sod_decisions;

\echo ''
\echo '=== 2. 单次工具调用耗时 p50/p95（按缓存命中分组 —— 这是本次实测的核心） ==='
-- 判读口径：
--   · 缓存命中组的 p50/p95 ⇒ 链的**代码开销**（规整器被跳过）⇒ 应该远小于 100ms
--   · 未命中组的 p50/p95 ⇒ 端到端代价（含一次同步 LLM 往返）⇒ 计划估 1~4 秒
-- 若未命中组的 p95 远超 4 秒 ⇒ 瓶颈不在链而在模型/provider，或规整器的解析
-- 重试逻辑有问题 ⇒ 该看的是 provider 侧而不是链。
SELECT
  CASE cache_hit WHEN true THEN '缓存命中（无 LLM 往返）' ELSE '未命中（含一次 LLM 往返）' END AS 组,
  count(*)                    AS 样本,
  round(percentile_cont(0.50) WITHIN GROUP (ORDER BY latency_ms)::numeric, 1) AS p50_ms,
  round(percentile_cont(0.90) WITHIN GROUP (ORDER BY latency_ms)::numeric, 1) AS p90_ms,
  round(percentile_cont(0.95) WITHIN GROUP (ORDER BY latency_ms)::numeric, 1) AS p95_ms,
  round(percentile_cont(0.99) WITHIN GROUP (ORDER BY latency_ms)::numeric, 1) AS p99_ms,
  max(latency_ms)             AS 最大_ms
FROM sod_decisions
GROUP BY cache_hit
ORDER BY cache_hit DESC;

\echo ''
\echo '=== 3. 裁决分布（三态 + 原因码） ==='
-- 判读口径：deny 与 needs_review 的占比不该高到离谱（说明闸门在正常范围工作），
-- 而 fail-closed 类原因码（scope_check_unavailable / normalizer_unavailable /
-- normalizer_malformed …）若成片出现 ⇒ 链在**误拒**，那是要修的故障而不是安全。
SELECT
  verdict,
  reason_code,
  count(*)     AS 次数,
  round(100.0 * count(*) / sum(count(*)) OVER (), 1) AS 占比_pct
FROM sod_decisions
GROUP BY verdict, reason_code
ORDER BY verdict, 次数 DESC;

\echo ''
\echo '=== 4. 耗时最慢的十个工具（找瓶颈工具） ==='
SELECT
  tool_name,
  count(*)                    AS 样本,
  round(percentile_cont(0.95) WITHIN GROUP (ORDER BY latency_ms)::numeric, 1) AS p95_ms,
  max(latency_ms)             AS 最大_ms
FROM sod_decisions
WHERE NOT cache_hit
GROUP BY tool_name
ORDER BY p95_ms DESC NULLS LAST, 样本 DESC
LIMIT 10;

\echo ''
\echo '=== 5. 身份口径：受管路径的 agent_key 形态 ==='
-- 判读口径：source_agent 必须是 "worker#<意图id>" 的形态 ⇒ 说明受管判定（IntentID>0）
-- 在真实路径上成立。若这里出现空串或别的形态 ⇒ 链读到的是空上下文 ⇒ 闸门在空转。
SELECT
  CASE
    WHEN source_agent = '' THEN '(空 —— 受管判定失效！)'
    WHEN source_agent ~ '^worker#[0-9]+$' THEN 'worker#<意图id>（正常）'
    ELSE source_agent
  END AS 身份形态,
  count(*) AS 次数
FROM sod_decisions
GROUP BY 1
ORDER BY 次数 DESC;

\echo ''
\echo '=== 6. needs_review 的去向一致性（唯一去处是对抗复核队列，不进人工队列） ==='
-- 配对键用 (source_agent, tool_name, payload_digest) 三元组 —— 这正是审计器
-- 去重时用的判据（internal/sod/auditor.go 的 findPendingReview），不是单看
-- payload_digest：同一个载荷被两个 agent 调过，那两条裁决是两次独立的复核。
--
-- 判读口径：「裁决有但队列缺」必须为 0 —— 非 0 意味着某条 needs_review 裁决
-- 没有对应的待复核项 ⇒ 有人永远查不到它 ⇒ 那条异常信号被静默丢弃了。
SELECT
  (SELECT count(*) FROM sod_decisions WHERE verdict = 'needs_review')                     AS 裁决为复核,
  (SELECT count(*) FROM sod_adversarial_review_pending WHERE status = 'pending')          AS 队列待查,
  (SELECT count(*) FROM sod_adversarial_review_pending WHERE status IN ('resolved','dismissed')) AS 队列已结案,
  (SELECT count(*) FROM sod_decisions d
     WHERE d.verdict = 'needs_review'
       AND NOT EXISTS (SELECT 1 FROM sod_adversarial_review_pending q
                        WHERE q.source_agent  = d.source_agent
                          AND q.tool_name      = d.tool_name
                          AND q.payload_digest = d.payload_digest))                       AS 裁决有但队列缺;

#!/usr/bin/env bash
# 本地提交前验证（0.14.9 批次八 H2）——开发纪律 §1「先运行验证，后提交」的
# 执行化：纪律从"记住"变成"./scripts/precommit-test.sh 一条命令"。
#
# 与 CI 同款口径：ARTEX_PG_DSN 走本地测试库（默认 127.0.0.1:5434，可用环境
# 变量覆盖）；任何一步失败即非零退出——不通过不提交。
#
# 用法：
#   ./scripts/precommit-test.sh            # 全量
#   ./scripts/precommit-test.sh --quick    # 跳过 go 全量测试（快速语法面）
#
# A-16（0.15.92）：本地也过 Tier-0 硬锁（与 CI 的 rsi-guard job 同款，先本地拦、
# 再 CI 拦）。确需修改 Tier-0 文件时用例外通道：
#   RSI_ALLOW_TIER0="改动理由" ./scripts/precommit-test.sh
# 理由必须原样写进本次提交的 message，并登记 docs/SECURITY-HARDENING.md
# 「Tier-0 例外登记」；CI 侧会校验理由确实出现在提交信息里。
set -euo pipefail
cd "$(dirname "$0")/.."

PG_DSN="${PRECOMMIT_PG_DSN:-postgres://artex:artex@127.0.0.1:5434/artex?sslmode=disable}"
GO_BIN="${GO:-go}"

echo "── [1/6] RSI 门禁：Tier-0 硬锁（强制）"
GUARD_ARGS=(--staged --enforce --tier 1)
if [ -n "${RSI_ALLOW_TIER0:-}" ]; then
  GUARD_ARGS+=(--allow-tier0 "$RSI_ALLOW_TIER0")
  echo "   （Tier-0 例外通道已启用：${RSI_ALLOW_TIER0}）"
fi
python3 scripts/rsi_guard.py "${GUARD_ARGS[@]}"

echo "── [2/6] go build ./..."
$GO_BIN build ./...

echo "── [3/6] go vet ./..."
$GO_BIN vet ./...

echo "── [4/6] go test ./... （CI 同款；--quick 跳过）"
if [ "${1:-}" = "--quick" ]; then
  echo "   （--quick：跳过）"
else
  ARTEX_PG_DSN="$PG_DSN" $GO_BIN test ./... -timeout 30m
fi

echo "── [5/6] 前端 tsc"
(cd web && npx tsc --noEmit)

echo "── [6/6] 前端 biome（相对基线改动的文件）"
CHANGED=$(git diff --name-only HEAD -- web 2>/dev/null | sed 's|^web/||' | grep -E '\.(ts|tsx|js|jsx)$' || true)
if [ -n "$CHANGED" ]; then
  (cd web && npx biome check $CHANGED)
else
  echo "   （无未提交的 web 改动，跳过）"
fi

echo "✅ precommit 全部通过。"

#!/usr/bin/env bash
# e2e 冒烟（0.14.9 批次八 H1）：起服务 → 等 health → 全部静态路由 HTTP 200
# → 关键页含平台标识 → 停服务。本地与 CI（go-test job 已带 PG service）两用。
#
#   ./scripts/e2e_smoke.sh <二进制路径> [端口]
#
# 依赖：PostgreSQL 可达（ARTEX_PG_DSN 或本机 5433/5432 默认形态）。

set -euo pipefail
cd "$(dirname "$0")/.."

BIN="${1:?用法: e2e_smoke.sh <二进制路径> [端口]}"
PORT="${2:-8791}"
BASE="http://127.0.0.1:${PORT}"

TMP=$(mktemp -d)
trap 'kill "${SRV:-0}" 2>/dev/null || true; rm -rf "$TMP"' EXIT

export ARTEX_DATA_DIR="$TMP/data"
export ARTEX_ADDR="127.0.0.1:${PORT}"
if [ -n "${ARTEX_PG_DSN:-}" ]; then :; fi
# DSN 由调用方供给（CI：service 容器；本地：既有环境）。

echo "── 起服务：$BIN (:$PORT)"
"$BIN" -addr "127.0.0.1:${PORT}" > "$TMP/server.log" 2>&1 &
SRV=$!

for i in $(seq 1 40); do
  if curl -sf -m 2 "$BASE/api/health" > "$TMP/health.json" 2>/dev/null; then break; fi
  sleep 1
done
if ! curl -sf -m 2 "$BASE/api/health" > /dev/null 2>&1; then
  echo "✗ 服务未在 40s 内就绪"; tail -20 "$TMP/server.log"; exit 1
fi
echo "   health OK: $(cat "$TMP/health.json")"

echo "── 全部页面 HTTP 冒烟"
ROUTES=(
  / /login /setup /dashboard /chat /function/tasks /function/tasks/detail
  /task-hall /task-hall/detail /legacy /strategies /strategies/assembled
  /strategies/detail /strategies/pin-queue /strategies/proposals
  /sod /sod/chain /sod/circuit /sod/review-queue /sod/screen
  /function/findings /function/findings/detail /function/traffic
  /function/commands /function/llm-records /function/assets
  /function/business-graph /function/business-graph/diffs
  /function/miniprogram /function/sync /function/workspace
  /system/llm /system/agents /system/agents/detail /system/mcp /system/skills
  /system/tools /system/notify /system/intercept /system/intercept/assets
  /system/intercept/approvals /system/logs /system/reports /system/settings
  /system/memory
)
FAIL=0
for p in "${ROUTES[@]}"; do
  code=$(curl -s -m 5 -o /dev/null -w "%{http_code}" "$BASE$p")
  if [ "$code" != "200" ]; then
    echo "   ✗ $p → $code"; FAIL=1
  fi
done
[ "$FAIL" = "0" ] && echo "   ${#ROUTES[@]} 页全部 200 ✓"

echo "── 关键内容检查"
grep -q "ARTEX" "$TMP/data" 2>/dev/null || true
BODY=$(curl -s -m 5 "$BASE/login")
echo "$BODY" | grep -q "ARTEX" || { echo "   ✗ 登录页缺平台标识"; FAIL=1; }
# 尾斜杠与 legacy 兼容链（0.15.1 语义：pianfeng 真页面 200（无斜杠/尾斜杠同内容），
# legacy 链头 edge-exploration 308 → pianfeng；与 server/webui_serve_test.go 矩阵同口径）
CODE_TRAIL=$(curl -s -m 5 -o /dev/null -w "%{http_code}" "$BASE/function/business-graph/")
[ "$CODE_TRAIL" = "200" ] || { echo "   ✗ 尾斜杠应 200 serve，got $CODE_TRAIL"; FAIL=1; }
CODE_PIANFENG=$(curl -s -m 5 -o /dev/null -w "%{http_code}" "$BASE/function/pianfeng")
[ "$CODE_PIANFENG" = "200" ] || { echo "   ✗ pianfeng 无斜杠应 200（0.15.1 真页面），got $CODE_PIANFENG"; FAIL=1; }
CODE_LEGACY=$(curl -s -m 5 -o /dev/null -w "%{http_code}" "$BASE/function/edge-exploration")
[ "$CODE_LEGACY" = "308" ] || { echo "   ✗ legacy 链头应 308 → pianfeng，got $CODE_LEGACY"; FAIL=1; }

[ "$FAIL" = "0" ] && echo "✅ e2e 冒烟通过" || { echo "✗ e2e 冒烟失败"; exit 1; }

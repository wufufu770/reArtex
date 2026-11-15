#!/usr/bin/env bash
# 备份脚本（0.14.9 批次八 H3）：数据库全量 + 工作目录 + 审计链头导出。
#
#   ./scripts/backup.sh [输出目录]     # 默认 ./backups/YYYYMMDD-HHMMSS/
#
# 纪律：
#   - pg_dump 用容器内客户端（宿主机不要求装 psql）；
#   - 工作目录打包排除 logs（可再生产物不进备份）；
#   - 审计链头（最大 seq + 其 digest）单独落文件——跨介质校验用（哈希链的
#     防篡改闭环：链头在外部介质上，库内全量重写可被发现）；
#   - 非破坏性：只读操作，不锁业务表（pg_dump 一致性快照）。

set -euo pipefail
cd "$(dirname "$0")/.."

OUT="${1:-backups/$(date +%Y%m%d-%H%M%S)}"
mkdir -p "$OUT"

PG_CONTAINER="${PG_CONTAINER:-artex-pg}"
PG_USER="${PG_USER:-artex}"
PG_DB="${PG_DB:-artex}"
DATA_DIR="${DATA_DIR:-./data}"

echo "── [1/3] pg_dump → $OUT/db.sql.gz"
docker exec "$PG_CONTAINER" pg_dump -U "$PG_USER" "$PG_DB" | gzip > "$OUT/db.sql.gz"

echo "── [2/3] 工作目录 → $OUT/data.tar.gz（排除 logs）"
if [ -d "$DATA_DIR" ]; then
  tar -czf "$OUT/data.tar.gz" --exclude='*/logs' -C "$(dirname "$DATA_DIR")" "$(basename "$DATA_DIR")"
else
  echo "   （工作目录不存在，跳过：$DATA_DIR）"
fi

echo "── [3/3] 审计链头 → $OUT/audit-head.txt"
docker exec "$PG_CONTAINER" psql -U "$PG_USER" -d "$PG_DB" -tAc \
  "SELECT seq || ' ' || digest FROM audit_events ORDER BY seq DESC LIMIT 1" \
  > "$OUT/audit-head.txt" 2>/dev/null || echo "（审计链为空）" > "$OUT/audit-head.txt"

echo "✅ 备份完成：$OUT"
ls -la "$OUT"

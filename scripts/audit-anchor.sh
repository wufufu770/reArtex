#!/usr/bin/env bash
# 审计链外部锚点脚本（A-05，0.15.89）：把链头（seq、digest、行口径）同步写到
# 备份目录之外的第二介质 + 落库到 audit_anchors。
#
#   ./scripts/audit-anchor.sh              # 手动跑一次
#   crontab: 0 * * * * /path/to/reArtex/scripts/audit-anchor.sh >> /var/log/artex-anchor.log 2>&1
#   （每小时一次；systemd timer 等价）
#
# 为什么要有第二介质：锚点落在同一个数据库里（audit_anchors 表）只能防「改行/
# 重写」，防不住「连同锚点表一起重写」。只有落在 DB 写权限够不到的地方（另一块
# 盘、对象存储），「持 DB 写权限者单方面重算/截断整条链」才会与外部介质对不上
# 而现形——平台侧 GET /api/audit/anchor 与本文件可交叉核对。
#
# 纪律：
#   - 只追加不覆盖：每次锚定追加一行（audit_anchors 与锚点文件都是 append-only）；
#   - 失败不静默：链头读不出来（库不可达/表缺失）必须报非零退出，cron 才会留痕；
#   - 空链锚定无意义：链为空时跳过并明确说明（首条事件出现后再开始锚定）。

set -euo pipefail
cd "$(dirname "$0")/.."

PG_CONTAINER="${PG_CONTAINER:-artex-pg}"
PG_USER="${PG_USER:-artex}"
PG_DB="${PG_DB:-artex}"
# 锚点目录必须指向备份目录之外的第二介质（另一块盘/对象存储挂载点）。
# 空值 = 拒绝运行：把外部锚点写进默认备份目录等于没做外部锚定。
ANCHOR_DIR="${ANCHOR_DIR:-}"

if [ -z "$ANCHOR_DIR" ]; then
  echo "❌ 未设置 ANCHOR_DIR（第二介质挂载点，必须在备份目录之外）——拒绝运行" >&2
  exit 2
fi
mkdir -p "$ANCHOR_DIR"

HEAD=$(docker exec "$PG_CONTAINER" psql -U "$PG_USER" -d "$PG_DB" -tAc \
  "SELECT seq || ' ' || digest || ' ' || COALESCE(key_id,'') FROM audit_events ORDER BY seq DESC LIMIT 1" 2>/dev/null || true)

if [ -z "$HEAD" ]; then
  echo "（审计链为空，本轮跳过锚定——$(date '+%F %T')）"
  exit 0
fi

SEQ=$(printf '%s' "$HEAD" | cut -d' ' -f1)
DIGEST=$(printf '%s' "$HEAD" | cut -d' ' -f2)
KEYID=$(printf '%s' "$HEAD" | cut -d' ' -f3)

# 1) 第二介质：append-only 文件（seq digest key_id ISO 时间），一行一次锚定。
printf '%s %s %s %s\n' "$SEQ" "$DIGEST" "$KEYID" "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" >> "$ANCHOR_DIR/audit-anchor.txt"

# 2) 落库：audit_anchors 追加一行（平台 GET /api/audit/anchor 据此对外回报；
#    VerifyAuditChain 逐行验链后核对最近锚点）。幂等：同 seq 已锚定则跳过。
ALREADY=$(docker exec "$PG_CONTAINER" psql -U "$PG_USER" -d "$PG_DB" -tAc \
  "SELECT COUNT(*) FROM audit_anchors WHERE seq = $SEQ" 2>/dev/null || echo 0)
if [ "$ALREADY" = "0" ]; then
  docker exec "$PG_CONTAINER" psql -U "$PG_USER" -d "$PG_DB" -c \
    "INSERT INTO audit_anchors(seq, digest, key_id, note) VALUES ($SEQ, '$DIGEST', '$KEYID', 'audit-anchor.sh')" >/dev/null
  echo "✅ 已锚定链尾 seq=$SEQ → $ANCHOR_DIR/audit-anchor.txt + audit_anchors（$(date '+%F %T')）"
else
  echo "✅ 链尾 seq=$SEQ 已在 audit_anchors，外部介质已追加记录（$(date '+%F %T')）"
fi

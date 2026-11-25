#!/bin/sh
# A-09（0.15.79）：把 config.json 里的明文 DSN 迁移到一次性文件。
#
# 复用 config.readAndBurnDSNFile 的语义：ARTEX_PG_DSN_FILE 指向的文件读入即
# 清零删除——连接串不常驻磁盘，agent（与 server 同 UID）无从捡拾。
#
# 做三件事：
#   1. 从 config.json 读出 database.dsn；
#   2. 写一次性文件（0600，默认 ./artex-pg-dsn.one-time，可用第二参数指定）；
#   3. 从 config.json 删掉 dsn 字段（database 其余字段保留）。
# 结尾提示 operator 设置 ARTEX_PG_DSN_FILE 指向该文件（compose/systemd/环境）。
# 注意：一次性文件在 server 下一次成功读取后即被删除——务必先把路径配置好。
#
# 用法：scripts/migrate-dsn-to-file.sh [config.json 路径] [一次性文件路径]
# 退出码：0 = 迁移完成；1 = 无需迁移或出错（带说明）。
#
# 依赖 python3（标准库 json，无第三方依赖——与 scripts/rsi_guard.py 同口径）。

set -u

CONFIG=${1:-${ARTEX_CONFIG:-./config.json}}
OUTFILE=${2:-./artex-pg-dsn.one-time}

if [ ! -f "$CONFIG" ]; then
    echo "[migrate-dsn] 配置文件 $CONFIG 不存在 —— 无需迁移"
    exit 1
fi

# 一步完成：读 dsn + 原子改写 config.json（去 dsn 字段）。python 输出两段：
#   第一行 = dsn（可能为空 = 无需迁移）
#   第二行 = 失败原因（空 = 成功）
TMP=$(mktemp) || exit 1
ERR=$(python3 - "$CONFIG" >"$TMP" <<'PYEOF'
import json, os, sys

path = sys.argv[1]
with open(path, encoding="utf-8") as f:
    cfg = json.load(f)

db = cfg.get("database")
dsn = ""
if isinstance(db, dict):
    dsn = db.pop("dsn", "") or ""

if not dsn:
    print("")
    print("NO_DSN")
    sys.exit(0)

tmp = path + ".migrating"
with open(tmp, "w", encoding="utf-8") as f:
    json.dump(cfg, f, ensure_ascii=False, indent=2)
    f.write("\n")
os.replace(tmp, path)  # 原子替换：中途失败不破坏原配置
print(dsn)
print("")
PYEOF
) || { rm -f "$TMP"; echo "[migrate-dsn] 解析/改写 $CONFIG 失败"; exit 1; }

if [ -n "$ERR" ]; then
    rm -f "$TMP"
    echo "[migrate-dsn] $CONFIG 未含明文 database.dsn —— 无需迁移"
    exit 1
fi

DSN=$(head -n 1 "$TMP")
rm -f "$TMP"
umask 077
printf '%s\n' "$DSN" > "$OUTFILE"
chmod 600 "$OUTFILE"

echo "[migrate-dsn] 已完成："
echo "  1. DSN（含口令）写入一次性文件 $OUTFILE（0600）"
echo "  2. config.json 里的 database.dsn 已删除（database 其余字段保留）"
echo ""
echo "后续（必做）：为 server 进程设置环境变量 ARTEX_PG_DSN_FILE=$OUTFILE"
echo "  （compose: env_file 或 environment；systemd: Environment= 行）。"
echo "  server 下一次成功读取会立即清零删除该文件（readAndBurnDSNFile 语义）；"
echo "  未设置 ARTEX_PG_DSN_FILE 就重启会因无 DSN 来源而启动失败——这是刻意"
echo "  的 fail-closed，防止连接串悄悄回落到磁盘明文。"
exit 0

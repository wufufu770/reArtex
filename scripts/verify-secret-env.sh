#!/bin/sh
# A-09（0.15.79）：部署面检查——config.json 里的明文 DSN 是待治理秘密。
#
# 口径：config.json 若含 database.dsn（明文连接串，含口令），打印警告并建议
# 迁移到 ARTEX_PG_DSN / ARTEX_PG_DSN_FILE。只警告不失败：存量部署可能仍依赖
# 它启动，硬失败会把合法部署打断；迁移用 scripts/migrate-dsn-to-file.sh。
#
# 用法：scripts/verify-secret-env.sh [config.json 路径]
#       （缺省用 config.Path() 的同款解析序：ARTEX_CONFIG > ./config.json >
#        可执行文件旁；本脚本只看前两者——脚本侧拿不到二进制位置）
#
# 退出码：0 = 无明文 DSN 或文件不存在；1 = 发现明文 DSN（CI/巡检可据此告警）。

set -u

CONFIG=${1:-${ARTEX_CONFIG:-./config.json}}

if [ ! -f "$CONFIG" ]; then
    echo "[verify-secret-env] 未发现配置文件 $CONFIG —— OK"
    exit 0
fi

if grep -Eq '"dsn"[[:space:]]*:[[:space:]]*"[^"]+"' "$CONFIG"; then
    echo "[verify-secret-env] 警告：$CONFIG 含明文 database.dsn（连接串落盘即待治理秘密）。"
    echo "  建议：运行 scripts/migrate-dsn-to-file.sh 迁移到 ARTEX_PG_DSN_FILE 一次性文件，"
    echo "  或直接使用环境变量 ARTEX_PG_DSN。迁移后 pathguard/短路表会把该路径列入平台秘密清单。"
    exit 1
fi

echo "[verify-secret-env] $CONFIG 未含明文 dsn —— OK"
exit 0

#!/bin/sh
# 建立/刷新 artex_agent 只读角色并落地列级授权（幂等，可重复执行）。
#
# 背景：平台现在只有一个 PostgreSQL 凭据（应用角色的），而 agent 的 Bash
# 子进程跑在容器里、与平台同容器 —— 拿到 DSN 就能 psql 直连，绕过平台
# 全部应用层 scope 校验（Task 1/3 的读面/写面守卫全部被这一条旁路击穿）。
# 进程侧的闭合靠「不同 UID + /proc 保护」；数据库侧提供的**伤害上限**是：
# agent 手里那份凭据**只能 SELECT**，且只能 SELECT 非敏感列。
#
# 本脚本做什么：
#   1. 建角色 artex_agent（不存在才建），口令从环境变量读，**永不打印**。
#   2. 从 live schema 派生敏感列判定（模式 + 误伤白名单 + 显式清单）。
#   3. 落地授权：只给 CONNECT + 列级 SELECT；敏感列列级撤销；
#      近乎全表敏感的表整表撤。
#   4. 打印审计摘要（判了什么、授了什么、撤了什么），供人工复核。
#
# 幂等：全部 DDL 都是「先撤销再授予」，重复执行结果一致。
# schema 演进：新表/新列进来后**重跑一次**即被纳入判定 ——
#   新表默认按「只授非敏感列」处理；新列若命中敏感模式则被撤销；
#   若新列既不命中模式也不在白名单里，它**可读**（这是保守的一侧），
#   审计摘要会把它列出来供人工确认。
#
# 用法：
#   ARTEX_AGENT_PG_PASSWORD='<口令>' \
#   PGHOST=<pg主机> PGPORT=5432 PGUSER=artex PGPASSWORD='<管理口令>' \
#   PGDATABASE=artex \
#   ./scripts/agent-readonly-role.sh
#
# 开发实例（在容器内跑 psql，走 trust 认证，无需管理口令）：
#   ARTEX_AGENT_PG_PASSWORD='<口令>' \
#   ARTEX_PG_CONTAINER=artexdev-postgres-1 PGUSER=artex PGDATABASE=artex \
#   ./scripts/agent-readonly-role.sh
#
# 只看分类不落地（审计用）：
#   ./scripts/agent-readonly-role.sh --dry-run
#
# 环境变量：
#   ARTEX_AGENT_PG_PASSWORD  必需。agent 角色的口令。只从环境读，不落文件、
#                             不进 argv、不打印。
#   ARTEX_AGENT_ROLE         可选，默认 artex_agent。
#   ARTEX_PG_CONTAINER       可选。指定后用 docker exec 在该容器内跑 psql。
#   PGHOST/PGPORT/PGUSER/PGPASSWORD/PGDATABASE
#                             管理连接参数。未设置时从仓库根 .env 兜底
#                             （POSTGRES_USER / POSTGRES_PASSWORD / POSTGRES_DB）。
set -eu

SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
REPO_DIR=$(cd "$SCRIPT_DIR/.." && pwd)
ENV_FILE="$REPO_DIR/.env"

ROLE="${ARTEX_AGENT_ROLE:-artex_agent}"
DRY_RUN=0
for arg in "$@"; do
    case "$arg" in
        --dry-run) DRY_RUN=1 ;;
        -h|--help)
            sed -n '2,50p' "$0" | sed 's/^# \{0,1\}//'
            exit 0
            ;;
        *)
            echo "未知参数：$arg（--dry-run / --help）" >&2
            exit 2
            ;;
    esac
done

# ---------------------------------------------------------------------
# 管理连接参数：环境变量优先，.env 兜底（不 source .env，避免执行其中语句）
# ---------------------------------------------------------------------
getenv_from_env_file() {
    key="$1"
    [ -f "$ENV_FILE" ] || return 1
    sed -n "s/^${key}=//p" "$ENV_FILE" | tail -n 1 | sed 's/^"//; s/"$//'
}

PGUSER="${PGUSER:-$(getenv_from_env_file POSTGRES_USER || true)}"
PGUSER="${PGUSER:-artex}"
PGDATABASE="${PGDATABASE:-$(getenv_from_env_file POSTGRES_DB || true)}"
PGDATABASE="${PGDATABASE:-artex}"
PGPORT="${PGPORT:-5432}"
PGHOST="${PGHOST:-127.0.0.1}"
# 管理口令只在管理连接里用；agent 角色的口令与它无关。
if [ -z "${PGPASSWORD:-}" ]; then
    PGPASSWORD=$(getenv_from_env_file POSTGRES_PASSWORD || true)
    [ -n "$PGPASSWORD" ] && export PGPASSWORD
fi

# ---------------------------------------------------------------------
# psql 调用包装：容器内 or 本机
# ---------------------------------------------------------------------
psql_exec() {
    if [ -n "${ARTEX_PG_CONTAINER:-}" ]; then
        docker exec -i "$ARTEX_PG_CONTAINER" \
            psql -X -v ON_ERROR_STOP=1 -U "$PGUSER" -d "$PGDATABASE" "$@"
    else
        psql -X -v ON_ERROR_STOP=1 -U "$PGUSER" -d "$PGDATABASE" "$@"
    fi
}

# ---------------------------------------------------------------------
# agent 角色口令：只从环境变量读，不落文件、不进 argv、不打印
# ---------------------------------------------------------------------
AGENT_PW="${ARTEX_AGENT_PG_PASSWORD:-}"
[ -n "$AGENT_PW" ] || {
    echo "缺少 ARTEX_AGENT_PG_PASSWORD（agent 角色口令）" >&2
    exit 2
}
case "$AGENT_PW" in
    *$'\n'*)
        echo "ARTEX_AGENT_PG_PASSWORD 不得包含换行" >&2
        exit 2
        ;;
esac

if [ "$DRY_RUN" -eq 1 ]; then
    echo "── 试运行：只派生分类，不落地授权 ──"
    {
        cat "$SCRIPT_DIR/agent-role-classification.sql"
        cat <<'SQL'
\pset format aligned
\pset border 2
SELECT count(DISTINCT table_name) AS 表数,
       count(*)                   AS 列数,
       count(*) FILTER (WHERE verdict = 'sensitive')   AS 敏感列,
       count(*) FILTER (WHERE verdict = 'whitelisted') AS 白名单列,
       count(*) FILTER (WHERE verdict = 'plain')       AS 普通列
FROM agent_role_class;
SELECT table_name AS 表, column_name AS 列, reason AS 判定依据
FROM agent_role_class WHERE verdict = 'sensitive' ORDER BY table_name, column_name;
SELECT kind AS 类型, subject AS 对象, detail AS 说明 FROM agent_role_notice ORDER BY kind, subject;
SQL
    } | psql_exec -v agent_role="$ROLE"
    exit 0
fi

echo "── 1/3 建角色 ──"
cat "$SCRIPT_DIR/agent-role-ensure.sql" | psql_exec -v agent_role="$ROLE"

echo "── 2/3 设口令（不回显）──"
# 用 psql 的 \password 而不是 `ALTER ROLE ... PASSWORD '...'`：
# 前者口令不出现在 SQL 文本里，因而不进服务器语句日志；后者会。
printf '%s\n%s\n' "$AGENT_PW" "$AGENT_PW" \
    | psql_exec -c "\password $ROLE" >/dev/null
unset AGENT_PW
echo "口令已设置（不回显、不落文件）"

echo "── 3/3 落地授权 ──"
cat "$SCRIPT_DIR/agent-role-classification.sql" "$SCRIPT_DIR/agent-role-apply.sql" \
    | psql_exec -v agent_role="$ROLE"

echo ""
echo "完成。角色 $ROLE 现为：只读（CONNECT + 非敏感列的 SELECT）。"
echo "验证：./scripts/verify-agent-readonly-role.sh"

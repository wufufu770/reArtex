#!/bin/sh
# 建立/刷新受限运行角色（默认 artex_app）并把运行期 DSN 写进 secret 文件。
#
# 为什么需要这一层：平台原先只有一个数据库凭据，而且是超级用户 ⇒ 谁拿到
# 容器里的连接串（/run/secrets/pg_dsn，同 UID 的 agent 子进程读得到），
# 就能绕过平台全部 API 层校验直接写库，还能跑 DDL（建表/删表/改结构）。
#
# 这一层把「直连」这条路掐在**数据库权限**上：
#   运行期角色 artex_app 只拿到 CONNECT + USAGE(public) + 业务表 DML + 序列 USAGE，
#   没有 CREATE、没有业务表所有权 ⇒ 直连也只能读写业务表，建表/删表一律被
#   PostgreSQL 自己拒掉（不是靠平台自律）。
#
# 本脚本做什么：
#   1. 以超级用户身份应用 schema（db/schema.sql + 各层 db/schema_*.sql，幂等）。
#   2. 建/刷新受限角色 —— **每次生成新口令** ⇒ 旧口令当场失效（这就是轮换）。
#   3. 落地最小权限（db/role_grants.sql，与 db.Open 启动期授权同一份文件，
#      两边不会漂移）。
#   4. 把运行期 DSN 写进 secrets/pg_dsn（0600）—— 与 init-pg-dsn-secret.sh
#      同一个文件、同一个机制（容器以 docker secret 挂载它）。
#
# 口令处理：只从环境变量传递，**永不打印、不进 argv、不落盘**（除了写进那个
# 0600 的 secret 文件本身 —— 那正是它该待的地方）。
#
# 用法：
#   PGHOST=<pg主机> PGPORT=5432 PGUSER=artex PGPASSWORD='<超级用户口令>' \
#   PGDATABASE=artex \
#   ./scripts/db-role-bootstrap.sh
#
# 开发实例（在容器内跑 psql，走 trust 认证，无需管理口令）：
#   ARTEX_PG_CONTAINER=artex-postgres-1 PGUSER=artex PGDATABASE=artex \
#   ./scripts/db-role-bootstrap.sh
#
# 轮换：重跑本脚本即可。新口令当场生效，旧口令立刻不能再连。
#
# 环境变量：
#   ARTEX_PG_APP_ROLE        受限角色名，默认 artex_app。
#   ARTEX_PG_CONTAINER       可选。指定后用 docker exec 在该容器内跑 psql。
#   ARTEX_PG_DSN_HOST        运行期 DSN 里的主机名，默认 postgres（compose 服务名）。
#   ARTEX_PG_DSN_PORT        运行期 DSN 里的端口，默认 5432。
#   ARTEX_RUNTIME_DSN_SECRET 运行期 DSN 的落地路径，默认 secrets/pg_dsn。
#   ARTEX_ADMIN_DSN_SECRET  迁移连接 DSN 的落地路径，默认 secrets/pg_dsn_admin。
#   ARTEX_ENV_FILE          从哪个 .env 读管理连接参数，默认脚本所在仓库根 .env。
#                             ⚠️ 部署实例的 .env 在**部署目录**而不在仓库根 ⇒
#                             部署时必须显式指过去，否则脚本会拿仓库根的
#                             POSTGRES_PASSWORD（开发口令）当超级用户口令，
#                             写出的迁移 secret 与生产集群对不上 ⇒ 应用起不来。
#   PGHOST/PGPORT/PGUSER/PGPASSWORD/PGDATABASE
#                             管理连接参数。未设置时从 ARTEX_ENV_FILE 兜底
#                             （POSTGRES_USER / POSTGRES_PASSWORD / POSTGRES_DB）。
set -eu

SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
REPO_DIR=$(cd "$SCRIPT_DIR/.." && pwd)
ENV_FILE="${ARTEX_ENV_FILE:-$REPO_DIR/.env}"

ROLE="${ARTEX_PG_APP_ROLE:-artex_app}"
SECRET_FILE="${ARTEX_RUNTIME_DSN_SECRET:-$REPO_DIR/secrets/pg_dsn}"
DSN_HOST="${ARTEX_PG_DSN_HOST:-postgres}"
DSN_PORT="${ARTEX_PG_DSN_PORT:-5432}"

for arg in "$@"; do
    case "$arg" in
        -h|--help)
            sed -n '2,60p' "$0" | sed 's/^# \{0,1\}//'
            exit 0
            ;;
        *)
            echo "未知参数：$arg（--help）" >&2
            exit 2
            ;;
    esac
done

# 角色名会进 \password 命令与 SQL ⇒ 只允许标识符字符。
case "$ROLE" in
    ""|[0-9]*|*[!A-Za-z0-9_]*)
        echo "非法角色名：$ROLE（只允许字母、数字、下划线，且不以数字开头）" >&2
        exit 2
        ;;
esac

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
if [ -z "${PGPASSWORD:-}" ]; then
    PGPASSWORD=$(getenv_from_env_file POSTGRES_PASSWORD || true)
    # 兜底：.env 里没有口令时，从已有的迁移 secret 里取回超级用户口令
    # （那份文件装的就是超级用户口令；口令随 .env 走，库卷重建后需同步刷新）。
    if [ -z "${PGPASSWORD:-}" ] && [ -r "${ARTEX_ADMIN_DSN_SECRET:-/nonexistent}" ]; then
        PGPASSWORD=$(sed -n 's#.*://[^:]*:\([^@]*\)@.*#\1#p' "${ARTEX_ADMIN_DSN_SECRET}" | tail -n 1)
    fi
    [ -n "${PGPASSWORD:-}" ] && export PGPASSWORD
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

# psql_exec_as_role 与 psql_exec 同款，额外把受限角色名放进会话 GUC。
#
# 为什么走 GUC 而不是 psql 变量：psql 变量（:'role'）**不会**在 $$ ... $$ 这种
# 美元引号块里做替换 ⇒ DO 块里拿不到它。而角色名必须进 DO 块（要先判存在再
# 建/改）。会话 GUC 没这个限制，且与 db/role_grants.sql 的取名方式一致。
psql_exec_as_role() {
    if [ -n "${ARTEX_PG_CONTAINER:-}" ]; then
        docker exec -i -e "PGOPTIONS=-c artex.app_role=$ROLE" "$ARTEX_PG_CONTAINER" \
            psql -X -v ON_ERROR_STOP=1 -U "$PGUSER" -d "$PGDATABASE" "$@"
    else
        PGOPTIONS="-c artex.app_role=$ROLE" \
            psql -X -v ON_ERROR_STOP=1 -U "$PGUSER" -d "$PGDATABASE" "$@"
    fi
}

# ---------------------------------------------------------------------
# 运行期角色口令：现生成，绝不打印
# ---------------------------------------------------------------------
# 只用字母数字：口令要进 URL 的 userinfo 段（#@/: 都会把它拼坏），
# 也要过 \password 的交互输入。
APP_PW=$(tr -dc 'A-Za-z0-9' < /dev/urandom | head -c 32)
if [ "${#APP_PW}" -lt 32 ]; then
    echo "生成角色口令失败（熵源不可用）" >&2
    exit 1
fi

echo "── 1/4 应用 schema（超级用户）──"
# 用 stdin 灌进去而不是 -f：docker exec 模式下容器里没有仓库副本，
# 文件只在宿主机上（与 scripts/agent-readonly-role.sh 同款做法）。
for f in "$REPO_DIR/db/schema.sql" "$REPO_DIR"/db/schema_*.sql; do
    [ -f "$f" ] || continue
    echo "  应用 $(basename "$f")"
    cat "$f" | psql_exec -q
done
# Go 侧那批 DDL（llm_records / llm_usage / strategy_*）不在任何 .sql 文件里，
# 由应用启动时用迁移连接补（ARTEX_PG_ADMIN_DSN）。脚本补不了，也不该补。

echo "── 2/4 建/刷新受限角色 $ROLE ──"
# 角色属性是这一层的闸门本身：
#   NOSUPERUSER   不能提权
#   NOCREATEDB    不能建库
#   NOCREATEROLE  不能改角色（不能把自己塞进别的角色里）
#   NOINHERIT     成员关系不自动带权限；万一有人误 GRANT 别的角色给它，权限不生效
#   NOBYPASSRLS   不能绕过行级安全策略
#   LOGIN         可以建立连接
psql_exec -q -v role="$ROLE" <<'SQL'
DO $ensure$
DECLARE
    -- 角色名走会话 GUC（artex.app_role）：psql 变量（:'role'）在 $$ ... $$ 这种
    -- 美元引号块里**不会**被替换 ⇒ 用它会直接语法错误。
    v_role text := COALESCE(NULLIF(current_setting('artex.app_role', true), ''), 'artex_app');
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = v_role) THEN
        EXECUTE format('CREATE ROLE %I LOGIN', v_role);
        RAISE NOTICE '已创建角色 %', v_role;
    ELSE
        RAISE NOTICE '角色 % 已存在，刷新属性与口令', v_role;
    END IF;
    EXECUTE format(
        'ALTER ROLE %I WITH LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOBYPASSRLS',
        v_role);
END
$ensure$;
SQL

echo "── 3/4 设新口令并落地最小权限 ──"
# 设口令：**不能**用 psql 的 \password —— 它强制要求一个 TTY（交互式读两次），
# 在无人值守的脚本里读到的只有 EOF ⇒ 口令根本没设上（角色会带着空/旧口令存在）。
# ⇒ 改用 ALTER ROLE ... PASSWORD，口令经 psql 变量插值，**经 stdin 送进去**：
#   · 不进 argv ⇒ 不会出现在进程列表里；
#   · 不进脚本文件、也不进 shell 历史；
#   · 但它确实出现在送进服务器的 SQL 文本里 ⇒ 若服务器开了 log_statement，
#     会进语句日志（PostgreSQL 默认关闭 log_statement）。
#     这是「无人值守可用」与「绝不进语句日志」之间的真实取舍：这里选前者。
{
    printf "\\set app_pw '%s'\n" "$APP_PW"
    printf "ALTER ROLE %s PASSWORD :'app_pw';\n" "$ROLE"
} | psql_exec -q >/dev/null
# 授权 SQL 与应用启动期用的是同一份文件（db/role_grants.sql）。
# 角色名通过会话 GUC 传（artex.app_role），psql 用 PGOPTIONS 在连接建立时设好。
if [ -n "${ARTEX_PG_CONTAINER:-}" ]; then
    cat "$REPO_DIR/db/role_grants.sql" \
        | docker exec -i -e "PGOPTIONS=-c artex.app_role=$ROLE" "$ARTEX_PG_CONTAINER" \
            psql -X -v ON_ERROR_STOP=1 -U "$PGUSER" -d "$PGDATABASE"
else
    cat "$REPO_DIR/db/role_grants.sql" \
        | PGOPTIONS="-c artex.app_role=$ROLE" \
            psql -X -v ON_ERROR_STOP=1 -U "$PGUSER" -d "$PGDATABASE"
fi

echo "── 4/4 写运行期 DSN 到 secret 文件 ──"
mkdir -p "$(dirname "$SECRET_FILE")"
umask 077
printf 'postgres://%s:%s@%s:%s/%s?sslmode=disable' \
    "$ROLE" "$APP_PW" "$DSN_HOST" "$DSN_PORT" "$PGDATABASE" > "$SECRET_FILE"
chmod 600 "$SECRET_FILE"
unset APP_PW

# 迁移连接（超级用户）也走 secret 文件，而不是环境变量里裸奔。
#
# 为什么需要它：有一批表（llm_records / llm_usage / strategy_proposals /
# strategy_usage / strategy_replay_corpus / task_strategy_snapshot*）的 DDL 写在 Go
# 字符串常量里，不在任何 .sql 文件里 ⇒ **脚本补不了**，只能由应用启动时用迁移
# 连接补（ARTEX_PG_ADMIN_DSN_FILE）。
#
# 为什么只在**缺失时**写：这里放的是**超级用户**口令，而超级用户口令不随轮换变
# （轮换换的是受限角色 artex_app 的口令）⇒ 每次重写只会无谓地动这个文件。
# 应用启动读走它之后会立刻抹掉磁盘上那份（只读挂载抹不掉时告警但不阻断）⇒
# 超级用户凭据只在启动那一瞬间存在于内存里。
ADMIN_SECRET_FILE="${ARTEX_ADMIN_DSN_SECRET:-$REPO_DIR/secrets/pg_dsn_admin}"
# ⚠️ 每次都重写（不只在缺失时写）：这份文件装的是**超级用户**口令，而超级用户
# 口令跟着 .env 走 —— 库卷被重建后集群会用新的 .env 口令重新初始化，此时若还留着
# 旧文件，应用启动时就会用旧口令连迁移库 ⇒ 认证失败 ⇒ 容器起不来。
mkdir -p "$(dirname "$ADMIN_SECRET_FILE")"
umask 077
printf 'postgres://%s:%s@%s:%s/%s?sslmode=disable' \
    "$PGUSER" "$PGPASSWORD" "$DSN_HOST" "$DSN_PORT" "$PGDATABASE" > "$ADMIN_SECRET_FILE"
chmod 600 "$ADMIN_SECRET_FILE"
echo "  迁移连接 secret: $ADMIN_SECRET_FILE（已按当前 .env 口令重写，0600）"

cat <<EOF
完成。
  受限运行角色 : $ROLE（NOSUPERUSER / NOCREATEDB / NOCREATEROLE / NOINHERIT / NOBYPASSRLS）
  权限         : CONNECT + USAGE(public) + 业务表 SELECT/INSERT/UPDATE/DELETE + 序列 USAGE
                 无 CREATE、无业务表所有权 ⇒ 直连也建不了表、删不了表
  运行期 DSN   : $SECRET_FILE（0600，口令不在此显示）
  轮换         : 重跑本脚本即可，新口令当场生效，旧口令立刻失效
EOF

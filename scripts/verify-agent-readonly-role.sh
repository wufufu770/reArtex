#!/bin/sh
# 验证 artex_agent 只读角色：权限账本 + 行为探针。
#
# 两层验证，缺一不可：
#   ① 权限账本（agent-role-verify.sql，以管理身份查 information_schema）：
#      把 live schema 派生出的期望授权集与实际记录的授权逐条比对。
#   ② 行为探针（agent-role-probe.sql，以 artex_agent 身份真跑）：
#      INSERT/UPDATE/DELETE/DDL 是否真被拒，敏感列是否真读不到。
#   账本对但行为不对（生效延迟、旁路执行路径）算没闭合；
#   行为对但账本不对（走了没预料到的授权来源）也算没闭合。
#
# ⚠️ 本脚本验的是「伤害上限」，**不是**「旁路已闭合」。
#   凭据可达性（agent 能不能拿到 DSN）是进程侧的事：不同 UID + /proc 保护。
#   即便本脚本全绿，只要 agent 还能拿到 DSN，它就能直连 PG 读**所有被授权的
#   非敏感列**（含跨任务数据）—— 那一层的闭合靠进程侧，不靠这里。
#
# 用法：
#   PGHOST=... PGUSER=artex PGPASSWORD='<管理口令>' PGDATABASE=artex \
#   ARTEX_AGENT_PG_PASSWORD='<agent 口令>' \
#   ./scripts/verify-agent-readonly-role.sh
#
# 开发实例（容器内 psql，trust 认证，探针无需口令）：
#   ARTEX_PG_CONTAINER=artexdev-postgres-1 PGUSER=artex PGDATABASE=artex \
#   ./scripts/verify-agent-readonly-role.sh
set -eu

SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
REPO_DIR=$(cd "$SCRIPT_DIR/.." && pwd)
ENV_FILE="$REPO_DIR/.env"

ROLE="${ARTEX_AGENT_ROLE:-artex_agent}"
AGENT_PW="${ARTEX_AGENT_PG_PASSWORD:-}"

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
    [ -n "$PGPASSWORD" ] && export PGPASSWORD
fi

psql_admin() {
    if [ -n "${ARTEX_PG_CONTAINER:-}" ]; then
        docker exec -i "$ARTEX_PG_CONTAINER" \
            psql -X -v ON_ERROR_STOP=1 -U "$PGUSER" -d "$PGDATABASE" "$@"
    else
        psql -X -v ON_ERROR_STOP=1 -U "$PGUSER" -d "$PGDATABASE" "$@"
    fi
}

psql_as_role() {
    if [ -n "${ARTEX_PG_CONTAINER:-}" ]; then
        docker exec -i "$ARTEX_PG_CONTAINER" \
            psql -X -v ON_ERROR_STOP=1 -U "$ROLE" -d "$PGDATABASE" "$@"
    else
        if [ -z "$AGENT_PW" ]; then
            echo "本机连接模式需要 ARTEX_AGENT_PG_PASSWORD（探针要以 artex_agent 身份登录）" >&2
            exit 2
        fi
        PGPASSWORD="$AGENT_PW" \
            psql -X -v ON_ERROR_STOP=1 -U "$ROLE" -d "$PGDATABASE" "$@"
    fi
}

echo "══ ① 权限账本断言（管理会话）══"
ledger=$( { cat "$SCRIPT_DIR/agent-role-classification.sql" "$SCRIPT_DIR/agent-role-verify.sql"; } \
    | psql_admin -v agent_role="$ROLE" 2>&1 ) || {
    echo "$ledger" >&2
    echo "账本断言执行失败" >&2
    exit 1
}
echo "$ledger"

ledger_pass=$(printf '%s\n' "$ledger" | grep -c 'CHECK PASS' || true)
ledger_fail=$(printf '%s\n' "$ledger" | grep -c 'CHECK FAIL' || true)

echo ""
echo "══ ② 行为探针（以 $ROLE 身份真跑）══"
if [ -n "${ARTEX_PG_CONTAINER:-}" ]; then
    echo "（容器内本地 socket：trust 认证，探针校验的是 ACL 生效性，不校验口令）"
else
    echo "（本机 TCP 连接：以 agent 口令登录，ACL 与口令同时被验证）"
fi
probe=$( { cat "$SCRIPT_DIR/agent-role-probe.sql"; } | psql_as_role 2>&1 ) || {
    printf '%s\n' "$probe" >&2
    echo "探针执行异常中断" >&2
    exit 1
}
# psql 把 NOTICE 输出成 "NOTICE:  PROBE PASS ..."，所以这里不能锚定行首。
printf '%s\n' "$probe" | grep 'PROBE ' || true

probe_pass=$(printf '%s\n' "$probe" | grep -c 'PROBE PASS' || true)
probe_fail=$(printf '%s\n' "$probe" | grep -c 'PROBE FAIL' || true)

echo ""
echo "══ 汇总 ══"
echo "账本断言：PASS $ledger_pass / FAIL $ledger_fail"
echo "行为探针：PASS $probe_pass / FAIL $probe_fail"

# 零通过 = 探针根本没跑起来 —— 不能当成通过（否则会造出假闭合）。
if [ "$ledger_pass" -eq 0 ] || [ "$probe_pass" -eq 0 ]; then
    echo ""
    echo "FAIL：没有任何断言/探针被执行（不是通过）" >&2
    exit 1
fi

if [ "$ledger_fail" -ne 0 ] || [ "$probe_fail" -ne 0 ]; then
    echo ""
    echo "FAIL：存在未通过的断言 —— 伤害上限未成立" >&2
    exit 1
fi

cat <<'NOTE'
OK：artex_agent 的伤害上限成立（只能 SELECT 非敏感列；写/删/DDL 一律被拒）。

已知限制（必须与「读侧旁路未闭合」并列记录，不得当成已闭合）：
  1. 本脚本验的是权限，不验凭据可达性。agent 若已拿到 DSN，仍能直连 PG
     读全部被授权的非敏感列（含跨任务数据）。凭据不可达靠进程侧：
     不同 UID + /proc hidepid + 凭据不落文件。
  2. PostgreSQL 默认把 TEMPORARY 授予 PUBLIC（数据库级），角色无法单独撤销
     PUBLIC 的授予 —— 因此 artex_agent 仍可建临时表。它不构成跨表读取或写入
     通道，但确实是一项「SELECT 以外」的权限，未被本脚本收回。
NOTE
exit 0

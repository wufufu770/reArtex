#!/bin/sh
# 生成 docker secret 文件（数据库连接串）。
#
# 为什么不用 environment 变量供给：容器配置面（docker inspect）会原样列出
# environment 的值，口令就在里面。secret 以文件挂载（/run/secrets/pg_dsn），
# 不进容器配置面，inspect 看不到。
#
# 口令来源仍是 .env（本地私有、不进版本库）。本脚本把 .env 里的三项拼成
# 完整 DSN 写进 secrets/pg_dsn，权限 0600；该目录已被 .gitignore 排除。
#
# 用法：./init-pg-dsn-secret.sh
set -eu

cd "$(dirname "$0")"

# 读 .env（不 source：避免执行其中的任意语句）
getenv() {
  key="$1"
  [ -f .env ] || return 1
  sed -n "s/^${key}=//p" .env | tail -n 1 | sed 's/^"//; s/"$//'
}

USER_NAME="${POSTGRES_USER:-$(getenv POSTGRES_USER || true)}"
PASSWORD="${POSTGRES_PASSWORD:-$(getenv POSTGRES_PASSWORD || true)}"
DB_NAME="${POSTGRES_DB:-$(getenv POSTGRES_DB || true)}"

[ -n "$USER_NAME" ] || { echo "缺少 POSTGRES_USER（.env 或环境变量）" >&2; exit 1; }
[ -n "$PASSWORD" ] || { echo "缺少 POSTGRES_PASSWORD（.env 或环境变量）" >&2; exit 1; }
DB_NAME="${DB_NAME:-artex}"

mkdir -p secrets
umask 077
printf 'postgres://%s:%s@postgres:5432/%s?sslmode=disable' "$USER_NAME" "$PASSWORD" "$DB_NAME" > secrets/pg_dsn
chmod 600 secrets/pg_dsn
echo "已生成 secrets/pg_dsn（0600），内容不在此显示。"

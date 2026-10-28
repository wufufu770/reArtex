#!/bin/sh
# 断言应用进程树的环境块里没有非空的平台秘密。
#
# 为什么要有这道门：平台凭据一旦进了环境块，同 UID 的 agent 子进程一条
# `cat /proc/*/environ` 就能读走（os.Unsetenv 改不动内核保存的 exec 期环境块）。
# 现状是「启动时自检 + 人工核对」—— 一次性的结论没有门守着，下一次有人把
# 密钥改成 env 注入就悄悄回归。本脚本把那条结论固化成可执行的断言。
#
# 用法：verify-secret-env.sh [pid]     默认 1（容器里的 pid 1）
#
# 判定：进程树内任何进程的 environ 里出现「带值」的平台秘密键 = 泄露 = 非 0。
set -u

# 平台自身的凭据键。空值不算泄露（键在但没值，读走也没有秘密）。
SECRET_KEYS="ARTEX_PG_DSN ANTHROPIC_API_KEY OPENAI_API_KEY ARTEX_MASTER_KEY"

leak=0

# 遍历本进程及其所有后代
walk() {
  pid="$1"
  if [ -r "/proc/$pid/environ" ]; then
    for key in $SECRET_KEYS; do
      # 取该键的值长度：非 0 即视为带值
      n=$(tr '\0' '\n' < "/proc/$pid/environ" 2>/dev/null | grep "^${key}=" | head -n 1 | sed "s/^${key}=//" | wc -c)
      if [ "$n" -gt 1 ]; then
        echo "LEAK: pid=$pid 的环境块里 $key 仍带值" >&2
        leak=1
      fi
    done
  fi
  for child in $(cat "/proc/$pid/task/$pid/children" 2>/dev/null); do
    walk "$child"
  done
}

walk "${1:-1}"

if [ "$leak" -ne 0 ]; then
  echo "FAIL: 应用进程树的环境块内仍有平台秘密" >&2
  exit 1
fi

echo "OK: 应用进程树的环境块内无非空平台秘密"
exit 0

#!/bin/sh
# 断言同 UID 进程读不到父进程内存。
#
# 为什么需要：/proc/<pid>/mem 能读出父进程的内存内容（含平台口令）。
# 平台层的路径/命令过滤挡不住这一条 —— 它不走工具入参。真正的闭合靠部署面
# 收紧：Docker 默认能力集不含 CAP_SYS_PTRACE，默认 seccomp 正是据此拦下
# ptrace 系统调用（compose 里显式 cap_drop 是把意图写明，不靠默认值兜底）。
#
# 用法：verify-ptrace-blocked.sh [pid]     默认 1（容器里的 pid 1）
#
# 判定：读目标进程的**已映射内存**必须失败。读得到 = 活路径还在 = 非 0 退出。
#
# 为什么要读已映射地址：/proc/<pid>/mem 的偏移 0 从未被映射，读它必然失败
# （EIO），与 ptrace 是否被拦无关 —— 拿它当判据，这道门在 ptrace 放开时也会
# 误报「已封闭」。所以从 /proc/<pid>/maps 取第一个可读映射的起点去读。
set -u

target="${1:-1}"

if [ ! -r "/proc/$target/mem" ]; then
  echo "OK: /proc/$target/mem 不可读（权限或文件不存在），ptrace 封闭生效"
  exit 0
fi

# 第一个可读映射的起始地址（页对齐）
# maps 每行：地址区间 权限 偏移 设备 inode 路径 —— 权限在第 2 列。
addr=$(awk '$2 ~ /^r/ { split($1, a, "-"); print a[1]; exit }' "/proc/$target/maps" 2>/dev/null)
if [ -z "$addr" ]; then
  echo "OK: 目标进程无可读映射（读不到内容），ptrace 封闭生效"
  exit 0
fi

# 页号（页大小 4096）
skip=$(( 0x${addr} / 4096 ))

if dd if="/proc/$target/mem" of=/dev/null bs=4096 skip="$skip" count=1 >/dev/null 2>&1; then
  echo "FAIL: 仍能读 /proc/$target/mem（地址 0x$addr）—— ptrace 未封闭，同 UID 进程可取走父进程内存" >&2
  exit 1
fi

echo "OK: /proc/$target/mem 的已映射区（地址 0x$addr）读取失败，ptrace 封闭生效"
exit 0

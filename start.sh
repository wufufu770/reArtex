#!/bin/sh
# ARTEX 守护启动脚本（Linux / macOS / Docker ENTRYPOINT）
#
# 用法：
#   ./start.sh                       前台运行（Ctrl-C 停止）
#   nohup ./start.sh >artex.log 2>&1 &   后台常驻
#   ./start.sh -addr :9000           额外参数原样透传给 artex
#
# 它只做一件事：把 artex 跑起来，进程退出后按退出码决定要不要再拉起。
#
#   0      用户正常停止        → 退出循环
#   75     程序请求重启        → 立刻重跑（页面点了"一键更新"或"回滚"）
#   其他   崩溃                → 退避后重跑（1→2→4…最多 60 秒）
#
# 刻意不在这里做下载、SHA256 校验或换装：那些逻辑在 sh 和 bat 上要写两套，
# 而它们恰恰是最不能出错的一环——一旦换上跑不起来的二进制，本脚本会忠实地
# 反复拉起它，用户只能上机器手工救。所以校验/换装全部留在 Go 里（selfupdate 包），
# 由 artex 自己在启动时完成，脚本保持傻瓜化。
set -u

cd "$(dirname "$0")" || exit 1

BIN=./artex
[ -x "$BIN" ] || { echo "[artex] 找不到可执行文件 $BIN" >&2; exit 1; }

RESTART_CODE=75
MAX_DELAY=60

child=0
stopping=0

# 转发停止信号给 artex 本体。
#
# Docker 下这是必需的：docker stop 只把 SIGTERM 发给 PID 1（也就是本脚本），
# 不会发给子进程。不转发的话 artex 收不到信号、做不了优雅关闭，10 秒后被 SIGKILL
# 硬杀，正在跑的任务直接断在半路。
forward() {
	stopping=1
	if [ "$child" -ne 0 ]; then
		kill -TERM "$child" 2>/dev/null || true
	fi
}
trap forward INT TERM

# ── CVE 情报 sidecar（B-01F）────────────────────────────────────────────
#
# ./reartex-cve-mcp/reartex-cve-mcp 存在时随本脚本一起拉起：独立进程、独立
# SQLite 文件（data/cve-mcp.db，不进单体 PostgreSQL）。发布 zip / Docker 镜像
# 里没有这个二进制，缺失就跳过——CVE 服务不可用属情报功能降级（单体侧
# B-01E/B-03G 处理），绝不妨碍 artex 启动。已显式配置外部 CVE_MCP_URL 的环境
# 不重复拉本地实例（避免同机 8400 冲突）。
#
# 不为 sidecar 做独立守护循环：死亡后只随 artex 的重启节奏补拉一次（含「一键
# 更新」换新二进制的场景），频率被 artex 自身的退避上限约束，不产生重试风暴；
# sidecar 挂了 artex 照常跑。
#
# 监听收紧到 127.0.0.1:8400：只服务同机的 artex，不对局域网开放。
CVE_BIN=./reartex-cve-mcp/reartex-cve-mcp
CVE_ADDR=127.0.0.1:8400
cve_pid=0
cve_attempted=0

start_cve_sidecar() {
	cve_attempted=1
	if [ -n "${CVE_MCP_URL:-}" ]; then
		echo "[cve-mcp] 已配置外部 CVE_MCP_URL，跳过本地 sidecar"
		return 0
	fi
	if [ ! -x "$CVE_BIN" ]; then
		echo "[cve-mcp] 未找到 $CVE_BIN，跳过（情报功能降级，不影响 artex）"
		return 0
	fi
	# 冻结服务地址传给 artex（单体侧消费属 B-03G；此刻无人读它，零行为变化）。
	CVE_MCP_URL="http://$CVE_ADDR"
	CVE_MCP_DB=./data/cve-mcp.db
	export CVE_MCP_URL CVE_MCP_DB
	# 全新安装时 data/ 可能还不存在（artex 首次运行才建它）：sidecar 的
	# SQLite 打不开缺目录会直接退出——这里先建好，首次启动即健康。
	mkdir -p ./data 2>/dev/null || true
	"$CVE_BIN" serve "$CVE_ADDR" &
	cve_pid=$!
	echo "[cve-mcp] sidecar 已启动 (pid=$cve_pid, http://$CVE_ADDR)"
}

# 脚本退出的所有路径（正常退出 / INT/TERM / 循环内 exit）都收掉 sidecar。
stop_cve_sidecar() {
	if [ "$cve_pid" -ne 0 ]; then
		kill -TERM "$cve_pid" 2>/dev/null || true
	fi
}
trap stop_cve_sidecar EXIT

delay=1
while :; do
	# sidecar 死亡后随 artex 重启补拉一次；首次进入或跳过场景只尝试一回，
	# 不在循环里反复刷提示。
	if [ "$cve_pid" -ne 0 ] && ! kill -0 "$cve_pid" 2>/dev/null; then
		echo "[cve-mcp] sidecar 已退出，随 artex 重启补拉" >&2
		cve_pid=0
		cve_attempted=0
	fi
	if [ "$cve_attempted" -eq 0 ]; then
		start_cve_sidecar
	fi

	"$BIN" "$@" &
	child=$!

	# 信号会打断 wait 并让它返回 >128。此时子进程其实还在做优雅关闭，
	# 必须再 wait 一次才能拿到它真正的退出码。
	wait "$child"
	code=$?
	if [ "$code" -gt 128 ]; then
		wait "$child"
		code=$?
	fi
	child=0

	if [ "$stopping" -eq 1 ]; then
		echo "[artex] 已停止"
		exit 0
	fi

	case "$code" in
		0)
			echo "[artex] 正常退出"
			exit 0
			;;
		"$RESTART_CODE")
			# 更新/回滚已就绪：重跑后 artex 会在启动时完成换装（见 selfupdate.Bootstrap）。
			echo "[artex] 请求重启（应用新版本）…"
			delay=1
			;;
		*)
			echo "[artex] 异常退出 (code=$code)，${delay}s 后重启" >&2
			sleep "$delay"
			delay=$((delay * 2))
			[ "$delay" -gt "$MAX_DELAY" ] && delay=$MAX_DELAY
			;;
	esac
done

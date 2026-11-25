#!/bin/sh
# ARTEX-EGRESS：容器出网治理规则（A-01C 二期强制层，Docker 形态）。
#
# 不变量：容器内「除平台主进程 UID 外」的一切新建出网连接一律 REJECT——合法
# 出网必须经进程内录制代理（loopback → 平台 UID 发起上游）⇒ 全部流量都过
# egressGate 判定面。任务目标是任意的，IP 白名单不可行；UID 分界可行且已在
# 镜像里成立：平台=root(0)，全部 agent 子进程（框架内置 Bash/MCP stdio 含）
# 由 privdrop 降到 AgentUID=65534。
#
# 设计（为什么 hook 只挂 NEW、不改 OUTPUT 默认策略）：
#   - 只拦新建连接，存量连接不受影响（apply 瞬间不掐断进行中的合法流）；
#   - 不动 INPUT（8787/8788 的入站面与此无关）；
#   - 回滚 = 删掉一条 hook 规则（teardown），不留下半套策略。
#
# 环境变量：
#   ARTEX_EGRESS_GUARD_PLATFORM_UID  平台主进程 UID（默认 0；部署必须以 root
#                                    运行平台——非 root 部署的子进程不降权，
#                                    UID 分界不存在，本守卫不可启用）
#   ARTEX_EGRESS_ALLOW_CIDRS         显式放行目的地（空格/逗号分隔 CIDR）——
#                                   「平台通道扩展」：合法需要直连的 MCP stdio
#                                    服务器等在此逐条登记（登记=部署配置，改它
#                                    要重启 guard，不可被 agent 热改）
#   ARTEX_EGRESS_PROXY_PORT          进内录制代理端口（默认 8788）——agent UID
#                                    在 loopback 上唯一放行的 TCP 目的端口
#   ARTEX_EGRESS_GUARD_LOG           1=拒绝面记 limit LOG（默认 0；日志进
#                                    容器 netns 的内核日志，docker logs 不可见，
#                                    排查用 dmesg）
#
# 用法：egress-guard-rules.sh apply|teardown
set -eu

MODE="${1:-apply}"
CHAIN="ARTEX-EGRESS"
CHAIN6="ARTEX-EGRESS6"
PLATFORM_UID="${ARTEX_EGRESS_GUARD_PLATFORM_UID:-0}"
PROXY_PORT="${ARTEX_EGRESS_PROXY_PORT:-8788}"
ALLOW_CIDRS="${ARTEX_EGRESS_ALLOW_CIDRS:-}"
LOG_DENIES="${ARTEX_EGRESS_GUARD_LOG:-0}"

if [ "$(id -u)" != "0" ]; then
  echo "egress-guard 需要 root（CAP_NET_ADMIN）来写 netns 的 iptables" >&2
  exit 1
fi

teardown() {
  while iptables -D OUTPUT -m state --state NEW -j "$CHAIN" 2>/dev/null; do :; done
  iptables -F "$CHAIN" 2>/dev/null || true
  iptables -X "$CHAIN" 2>/dev/null || true
  while ip6tables -D OUTPUT -m state --state NEW -j "$CHAIN6" 2>/dev/null; do :; done
  ip6tables -F "$CHAIN6" 2>/dev/null || true
  ip6tables -X "$CHAIN6" 2>/dev/null || true
  echo "egress-guard 已拆除（hook 删除，OUTPUT 默认策略从未改动）"
}

apply() {
  teardown

  iptables -N "$CHAIN"
  # 平台主进程：一切出网放行（代理上游/LLM/MCP HTTP/自更新/DNS——它就是判定面
  # 本体），同时覆盖本机互通（docker 内嵌 DNS 127.0.0.11）。
  iptables -A "$CHAIN" -m owner --uid-owner "$PLATFORM_UID" -j ACCEPT
  # agent 子进程在 loopback 上的唯一合法去处：进程内录制代理（出网必经判定面）。
  iptables -A "$CHAIN" -o lo -p tcp --dport "$PROXY_PORT" -j ACCEPT
  # 显式登记的平台通道扩展（CIDR 级，部署配置，不可热改）。
  for cidr in $(echo "$ALLOW_CIDRS" | tr ',' ' ' | tr -s ' ' '\n'); do
    [ -n "$cidr" ] || continue
    iptables -A "$CHAIN" -d "$cidr" -j ACCEPT
  done
  if [ "$LOG_DENIES" = "1" ]; then
    iptables -A "$CHAIN" -m limit --limit 10/minute -j LOG --log-prefix "artex-egress-deny "
  fi
  iptables -A "$CHAIN" -j REJECT
  iptables -A OUTPUT -m state --state NEW -j "$CHAIN"

  # IPv6 同构（Docker v6 出网常由宿主管理，但容器内能发起就不该放过去）。
  ip6tables -N "$CHAIN6"
  ip6tables -A "$CHAIN6" -m owner --uid-owner "$PLATFORM_UID" -j ACCEPT
  ip6tables -A "$CHAIN6" -o lo -p tcp --dport "$PROXY_PORT" -j ACCEPT
  for cidr in $(echo "$ALLOW_CIDRS" | tr ',' ' ' | tr -s ' ' '\n'); do
    [ -n "$cidr" ] || continue
    ip6tables -A "$CHAIN6" -d "$cidr" -j ACCEPT 2>/dev/null || true
  done
  if [ "$LOG_DENIES" = "1" ]; then
    ip6tables -A "$CHAIN6" -m limit --limit 10/minute -j LOG --log-prefix "artex-egress-deny6 "
  fi
  ip6tables -A "$CHAIN6" -j REJECT
  ip6tables -A OUTPUT -m state --state NEW -j "$CHAIN6"

  echo "egress-guard 已生效：平台 UID=$PLATFORM_UID 出网放行；其余 UID 仅可经 loopback:${PROXY_PORT}（进程内代理）；其余新建出网一律 REJECT"
}

case "$MODE" in
  apply) apply ;;
  teardown) teardown ;;
  *) echo "用法: $0 apply|teardown" >&2; exit 2 ;;
esac

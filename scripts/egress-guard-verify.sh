#!/bin/sh
# A-01C 强制层的绕过/回滚验证（卡面验收：清除代理变量、直接连接 IP、替换 DNS、
# 子进程访问等路径不能绕过策略；授权通道仍可用）。
#
# 在一次性 alpine 容器里复现部署机制（netns 内 iptables 规则），以两个 UID 分身
# 验证：root=平台主进程（UID 0）、nobody=降权 agent 子进程（65534，与 norma
# privdrop.AgentUID 一致）。五条断言：
#   ① agent(65534) 直连外网 IP      ⇒ 必败（REJECT）
#   ② agent(65534) 走 DNS 解析直连  ⇒ 必败（DNS 同样被拒，更深一层 fail-closed）
#   ③ agent(65534) → loopback:8788  ⇒ 必通（进程内代理=唯一合法出口）
#   ④ 平台(0) 直连外网 IP           ⇒ 必通（代理上游/LLM/MCP HTTP 的上游语义）
#   ⑤ teardown 后同一条直连不再被立即 REJECT ⇒ 回滚干净，不留半套策略
#
# 用法：scripts/egress-guard-verify.sh   （宿主机需 docker；外网连通性非必需：
# 直连用 RFC5737 保留地址，「立即被拒」判胜负，不依赖真的到达外网）
set -eu
IMAGE="alpine:3.20"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"

docker image inspect "$IMAGE" >/dev/null 2>&1 || docker pull "$IMAGE" >/dev/null

docker run --rm -i --cap-add NET_ADMIN \
  -v "$ROOT/scripts/egress-guard-rules.sh:/usr/local/bin/egress-guard-rules.sh:ro" \
  "$IMAGE" /bin/sh -s <<'EOF'
set -u
apk add --no-cache iptables ip6tables curl busybox-extras >/dev/null 2>&1
# ③ 的代理替身：root 起一个 8788 常驻监听（回 200），agent UID 对它发 HTTP。
# busybox nc 单连接服务完就退，用循环保持常驻——测试替身必须不干扰被测规则。
while true; do printf 'HTTP/1.0 200 OK\r\nContent-Length: 2\r\n\r\nok' | nc -l -p 8788 >/dev/null 2>&1 || true; done &
sleep 0.3

pass=0; fail=0
ok()  { pass=$((pass+1)); echo "PASS $1"; }
bad() { fail=$((fail+1)); echo "FAIL $1"; }
# expect_fail/expect_pass 以 nobody（65534）执行，模拟降权 agent 子进程。
expect_fail() { if su -s /bin/sh nobody -c "$2" >/dev/null 2>&1; then bad "$1"; else ok "$1"; fi; }
expect_pass() { if su -s /bin/sh nobody -c "$2" >/dev/null 2>&1; then ok "$1"; else bad "$1"; fi; }

egress-guard-rules.sh apply
sleep 0.2

expect_fail "① agent(65534) 直连外网 IP 被拒" "curl -s -m 3 http://192.0.2.10/ -o /dev/null"
expect_fail "② agent(65534) DNS+域名直连被拒" "curl -s -m 3 http://example.com/ -o /dev/null"
expect_pass "③ agent(65534) → loopback:8788 代理面可达" "curl -s -m 3 http://127.0.0.1:8788/ -o /dev/null"
if curl -s -m 3 http://192.0.2.10/ -o /dev/null 2>/dev/null; then :; else
  echo "（④ 说明：RFC5737 地址不可路由时此处按超时计——判断点是「没有立即被 REJECT」（curl 退出码 7），超时(28)即规则未拦平台 UID）"
fi
if su -s /bin/sh nobody -c "curl -s -m 3 http://192.0.2.10/ -o /dev/null" >/dev/null 2>&1; then
  bad "④ 对照：agent 直连意外成功（规则未生效）"
else
  rc=0
  su -s /bin/sh nobody -c "curl -s -m 3 http://192.0.2.10/ -o /dev/null" >/dev/null 2>&1 || rc=$?
  if [ "$rc" = "7" ]; then ok "④ 平台出网对照：agent 被 REJECT（立即拒绝=规则生效）"; else bad "④ agent 直连退出码=$rc（期望 7=connection refused）"; fi
  # 平台 UID 直连：判断点是「不是被 REJECT 拒绝」（refused=7 会让 root 也一样被拒）
  rrc=0
  curl -s -m 3 http://192.0.2.10/ -o /dev/null >/dev/null 2>&1 || rrc=$?
  if [ "$rrc" != "7" ]; then ok "④ 平台(0) 出网未被 REJECT（放行）"; else bad "④ 平台(0) 出网被 REJECT（owner 规则未生效）"; fi
fi

egress-guard-rules.sh teardown >/dev/null
sleep 0.2
trc=0
su -s /bin/sh nobody -c "curl -s -m 5 http://192.0.2.10/ -o /dev/null" >/dev/null 2>&1 || trc=$?
if [ "$trc" != "7" ]; then ok "⑤ teardown 后 agent 直连不再被立即 REJECT（回滚干净）"; else bad "⑤ teardown 后仍被立即 REJECT（规则残留）"; fi

echo "----"
if [ "$fail" -eq 0 ]; then
  echo "egress-guard 验证：$pass 条全部通过"
  exit 0
fi
echo "egress-guard 验证：$fail 条未通过"
exit 1
EOF

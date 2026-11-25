# Egress Guard：容器出网强制层（A-01C 二期）

## 解决什么问题

一期出网治理（A-01/A-15/R1）的判定面全在**进程内**：录制代理按任务身份判定、
netguard 拨号守卫、WebSearchReady 装配门。这些把「走正常装配」的流量管住了，
但两类内核层面的旁路进程内看不见：

1. agent 子进程在 shell 里 `unset` 代理变量 / `--noproxy` / `env -i` 后直连；
2. MCP stdio 服务器（外部进程）自发的网络连接——它继承平台 env、不带任务令牌。

二期强制层把判定权交给内核：**容器 netns 里只有平台主进程 UID 能新建出网连接**。
合法流量全部经进程内录制代理（loopback → 平台 UID 发起上游），因此必然过
egressGate 判定面；一切降权子进程的直连（含 DNS）被 REJECT。

## 为什么是 UID 分界而不是 IP 白名单

任务目标是任意的（渗透任务连的就是客户资产），IP 白名单不可行。UID 分界在
镜像里天然成立：平台以 root(UID 0) 运行；全部 agent 子进程——包括框架内置的
Bash、shell session、MCP stdio——由 `privdrop` 降到固定 `AgentUID=65534`
（norma `tool/privdrop`，reArtex `internal/agentcred` 同源）。内核按包的属主
UID 判定，进程无法伪造（setuid 回 0 需要 CAP_SETUID，agent 容器不授予）。

## 启用（opt-in overlay，默认 compose 行为不变）

```bash
docker compose -f docker-compose.yml -f docker-compose.egress-guard.yml up -d
```

守卫边车（`Dockerfile.egress-guard`，alpine+iptables）与 artex 共享 network
namespace，每 30s 幂等重放规则；规则本体在 `scripts/egress-guard-rules.sh`。

**前置（必须逐条满足）**：

1. 平台以 **root** 运行（默认镜像如此）。非 root 部署的子进程不降权（privdrop
   `CanDrop()==false`），UID 分界不存在——这种部署**不可启用**本守卫，直连
   旁路仍靠一期进程内判定（如实声明，不宣称隔离）。
2. 依赖直连的 MCP stdio 服务器，把其端点 CIDR 逐条登记进
   `ARTEX_EGRESS_ALLOW_CIDRS`（部署配置，改它需重启 guard，agent 不可热改）。

环境变量（overlay 传给边车）：

| 变量 | 默认 | 含义 |
|---|---|---|
| `ARTEX_EGRESS_GUARD_PLATFORM_UID` | `0` | 平台主进程 UID |
| `ARTEX_EGRESS_PROXY_PORT` | `8788` | 进内录制代理端口（agent UID 在 loopback 上唯一放行的 TCP 端口） |
| `ARTEX_EGRESS_ALLOW_CIDRS` | 空 | 显式平台通道扩展（CIDR 列表） |
| `ARTEX_EGRESS_GUARD_LOG` | `0` | `1`=拒绝面写 limit LOG（内核日志，`dmesg` 查看） |

## 验证（本机已实证）

```bash
scripts/egress-guard-verify.sh
```

一次性容器里复现同款规则，6 条断言：agent(65534) 直连 IP 被拒 / DNS+域名被拒 /
loopback:8788 代理面可达 / 平台(0) 出网放行 / agent 直连被立即 REJECT（规则生效
对照）/ teardown 后回滚干净。2026-11-25 本机 docker 实测 **6/6 通过**。

对真实部署栈的验收（启用 overlay 后在 artex 容器内）：

```bash
docker compose -f docker-compose.yml -f docker-compose.egress-guard.yml exec artex \
  su -s /bin/sh nobody -c 'unset HTTP_PROXY HTTPS_PROXY ALL_PROXY; \
  curl -m 5 http://example.com/'        # 期望：立即失败（refused）
# 对照（平台身份，走判定面的合法出网不受影响）由平台自身健康体现：页面/任务可跑
```

## 能力变化（如实）

| 场景 | 启用前 | 启用后 |
|---|---|---|
| agent 子进程 unset 代理后 curl | 直连成功（一期看不见） | **内核拒绝** |
| agent 子进程自带 DNS 解析 | 直连可用 | **拒绝**（DNS 也拦；走代理的 CONNECT 由平台解析，不受影响） |
| MCP stdio 服务器自发联网 | 直连可用（网关外） | **拒绝**，除非端点登记进 `ARTEX_EGRESS_ALLOW_CIDRS` |
| 平台进程出网（代理上游/LLM/自更新/MCP HTTP/复测直连） | 可用 | **不变**（owner 放行） |
| WebFetch/web_search/Bash 走代理 | 可用 | **不变**（loopback:8788 对 agent UID 放行） |

## 故障恢复 / 回滚

- **边车崩溃**：规则活在 artex 的 netns 里，边车死亡不会解锁——拒绝面持续生效；
  `restart: unless-stopped` 自动拉起后幂等重放。
- **回滚**：去掉 overlay 重新 `up -d`；或在共享 netns 里执行
  `egress-guard-rules.sh teardown`（只删 hook 链，OUTPUT 默认策略从未改动）。
- **误伤排查**：`ARTEX_EGRESS_GUARD_LOG=1` + `dmesg | grep artex-egress-deny`
  看被拒五元组；属合法需求的把目的地登记进 `ARTEX_EGRESS_ALLOW_CIDRS`。

## 边界（如实声明，不宣称全覆盖）

- 只覆盖 **IPv4+IPv6 的 netns 出网**；宿主机网络、宿主上直接运行的进程（非
  Docker 部署）不在本层范围——非 Docker 部署仍只有一期进程内判定。
- netns 内进程若获得 `CAP_NET_ADMIN` 可拆规则；artex 容器不授予该能力
  （`cap_drop: SYS_PTRACE` 保持，未加 NET_ADMIN），守卫边车才有。

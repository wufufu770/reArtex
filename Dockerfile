# syntax=docker/dockerfile:1
#
# 运行镜像（不在镜像里编译）：只装常用工具，放入**预编译好的 Linux 单二进制**。
# 二进制由 CI 的 binaries job 交叉编译（纯 Go、无 QEMU），按目标架构放在
# 构建上下文的 dist/<TARGETARCH>/artex。这样多架构构建时 arm64 只需模拟 apt 层，
# 不再模拟 Next/Go 编译，速度快得多。
#
# 本地构建镜像用仓库根目录的 ./build-image.sh：它会编出二进制、把它摆到
# dist/<arch>/artex（Dockerfile 消费的位置），并把版本号作为 ARTEX_BINARY_VERSION
# 传给构建期校验，避免把上一次留下的旧二进制装进镜像。
#
# 手工构建则必须自己保证三件事都成立：
#   1) dist/<arch>/artex 是刚编出来的那个二进制（不是上一次留下的）
#   2) 二进制的版本号与 --build-arg ARTEX_BINARY_VERSION 一致
#   3) 该版本号由 -ldflags "-X main.version=<版本>" 注入（否则版本是 "dev"，
#      校验只能对到 "dev"，起不到区分新旧的作用）
FROM python:3.12-slim-bookworm
ARG TARGETARCH
# 常用工具：ripgrep / curl / vim，加一批 recon 常备件（按需增删）。
# Node 从 NodeSource 装 20.x：bookworm 自带的 apt nodejs 是 18，Playwright 要求 >=20。
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates ripgrep curl wget vim git jq unzip \
      dnsutils iputils-ping netcat-openbsd inetutils-telnet whois nmap \
    && curl -fsSL https://deb.nodesource.com/setup_20.x | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    && rm -rf /var/lib/apt/lists/*
# 预装 Playwright MCP 与 CLI（全局），运行时不再 npx 联网下载。
# @playwright/mcp：browser MCP 直接 `npx @playwright/mcp`（已全局装好，无需 -y/@latest）。
# @playwright/cli：提供 playwright-cli，装完顺带 --help 验证可执行。
# 再装 playwright（提供浏览器管理），装完用 --with-deps 预置 chromium 及其系统依赖，
# 这样容器内 MCP/CLI 首次启动即可用，不再联网下载浏览器。
RUN npm install -g @playwright/mcp@latest @playwright/cli@latest playwright@latest \
    && playwright-cli --help \
    && playwright install --with-deps chromium \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /app
# 预编译好的对应架构二进制（dist/amd64/artex 或 dist/arm64/artex）
#
# ARTEX_BINARY_VERSION 是这次构建期望的版本号，必填：构建期会跑一次装进去的
# 二进制，核对它自报的版本。少了这个校验，dist/<arch>/artex 若是上一次构建留下的
# 旧文件，镜像照样构建成功 —— 层哈希变了、镜像 ID 变了，看着像刚编的，旧二进制
# 就这么被静默装了进去。
ARG ARTEX_BINARY_VERSION=""
COPY dist/${TARGETARCH}/artex /app/artex
# 守护启动脚本：进程退出后按退出码决定是否重新拉起，页面一键更新靠它完成换装。
# 它同时负责把 SIGTERM 转发给 artex —— docker stop 只把信号发给 PID 1，
# 不转发的话 artex 收不到、做不了优雅关闭，10 秒后被 SIGKILL 硬杀。
COPY start.sh /app/start.sh
COPY verify-binary-version.sh /usr/local/bin/verify-binary-version.sh
RUN chmod +x /app/artex /app/start.sh /usr/local/bin/verify-binary-version.sh
RUN /usr/local/bin/verify-binary-version.sh /app/artex "$ARTEX_BINARY_VERSION"
COPY skills/ /app/skills/
# data/（SQLite + jwt.key）持久化点
VOLUME ["/app/data"]
EXPOSE 8787 8788
ENTRYPOINT ["/app/start.sh"]
CMD ["-addr", ":8787", "-proxy", ":8788"]

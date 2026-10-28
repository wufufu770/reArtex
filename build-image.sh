#!/bin/sh
# 一键构建本地镜像：编二进制 → 摆到 Dockerfile 消费的位置 → 构建镜像（带版本校验）。
#
# 用法：./build-image.sh [镜像标签]     默认 artex:local
#
# 为什么要有这个脚本：Dockerfile 消费的是 dist/<arch>/artex，而 build.sh 的产物在
# dist/artex-<os>-<arch>/artex，两者过去靠人工同步。忘了同步时 docker build 会拿
# 到上一次的旧文件，镜像却看着是刚编出来的 —— 旧二进制被静默装进镜像。脚本把
# 编译、摆位、传版本号串成一条命令，构建期再校验一次版本，杜绝这种静默失败。
#
# 前端默认照 build.sh 的做法重新构建；想复用已有的 server/webui/dist 就设
# ARTEX_SKIP_FRONTEND=1。
set -eu
cd "$(dirname "$0")"

TAG="${1:-artex:local}"
ARCH="$(go env GOARCH)"

echo "[*] 编译二进制"
./build.sh

# 期望版本必须来自「刚编出来的这一份」，而不是从别处推测：镜像里那份若是旧的，
# 校验就会失败并指出问题。
FRESH="dist/artex-linux-${ARCH}/artex"
VERSION=""
if [ -x "$FRESH" ]; then
	VERSION="$("$FRESH" -version 2>/dev/null || true)"
fi
if [ -z "$VERSION" ]; then
	# 交叉编译时编出来的二进制在本机跑不起来，退回与 build.sh 同一套推导。
	VERSION="$(git describe --tags --always --dirty 2>/dev/null || echo dev)"
fi
VERSION="${VERSION#v}"
echo "[*] 期望版本：$VERSION"

echo "[*] 构建镜像：$TAG"
docker build --build-arg "ARTEX_BINARY_VERSION=$VERSION" -t "$TAG" .

echo "[*] 完成：$TAG"
docker images "$TAG" --format '    {{.Repository}}:{{.Tag}} {{.ID}} {{.CreatedSince}}'

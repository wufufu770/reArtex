#!/usr/bin/env bash
# 把 norma 从只读的 Go 模块缓存复制到 third_party/norma，并应用仓库里的降权补丁。
#
# 为什么要这一步：norma 是上游依赖，模块缓存里的 v0.4.4 是只读共享的，不能直接改；
# 而 agent 工具子进程降权这件事必须落在 norma 内部（bash / 交互式 shell / 后台
# task / ripgrep / MCP stdio server 五处 spawn 点）。做法是把上游原版复制出来，
# 打上仓库里可评审的补丁 patches/norma-privilege-drop.patch，再用 go.mod 的
# replace 指向这份本地树。
#
# 幂等：每次都从模块缓存重新复制后再打补丁，所以重复跑结果一致。
# 失败即报错：补丁打不上就非零退出，绝不留下半打的状态。

set -euo pipefail

SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
ROOT=$(cd -- "$SCRIPT_DIR/.." && pwd)
cd "$ROOT"

MODULE_PATH="github.com/Autumn-27/norma"
TARGET_DIR="third_party/norma"
PATCH_REL="patches/norma-privilege-drop.patch"
PATCH_ABS="$ROOT/$PATCH_REL"

fail() {
	printf 'prepare-norma: %s\n' "$*" >&2
	exit 1
}

[ -f go.mod ] || fail "当前目录不是 Go 模块根目录（找不到 go.mod）"
[ -f "$PATCH_ABS" ] || fail "缺少补丁文件 $PATCH_REL"

# 版本以 go.mod 的 require 行为准（replace 指向本地目录，模块缓存里的原版才是补丁目标）。
VERSION="${NORMA_VERSION:-}"
if [ -z "$VERSION" ]; then
	VERSION=$(sed -n "s|^[[:space:]]*${MODULE_PATH//./\\.}[[:space:]]\{1,\}\(v[^[:space:]]\{1,\}\).*$|\1|p" go.mod | head -n 1)
fi
[ -n "$VERSION" ] || fail "无法从 go.mod 解析 ${MODULE_PATH} 的版本（可用 NORMA_VERSION=... 指定）"

GOMODCACHE="$(go env GOMODCACHE)"
[ -n "$GOMODCACHE" ] || fail "go env GOMODCACHE 返回空"

# 模块缓存目录名把大写字母编码成 !小写（github.com/Autumn-27 → github.com/!autumn-27）。
escaped_path=$(printf '%s' "$MODULE_PATH" | awk '{
	s = ""
	for (i = 1; i <= length($0); i++) {
		c = substr($0, i, 1)
		if (c ~ /[A-Z]/) s = s "!" tolower(c); else s = s c
	}
	print s
}')
SRC_DIR="$GOMODCACHE/$escaped_path@$VERSION"
[ -d "$SRC_DIR" ] || fail "模块缓存里没有 ${MODULE_PATH}@${VERSION}（期望目录：$SRC_DIR）；先执行 'go mod download ${MODULE_PATH}@${VERSION}'"

# 从原版重新复制：补丁永远是打在干净的一份上，避免上一次留下的半成品。
rm -rf "$TARGET_DIR"
mkdir -p "$(dirname "$TARGET_DIR")"
cp -a "$SRC_DIR" "$TARGET_DIR"
chmod -R u+w "$TARGET_DIR"

apply_patch() {
	# git apply 在仓库子目录里会把补丁路径当成相对「仓库根目录」的路径，命中不到就
	# 静默跳过（退出码仍是 0）。所以统一从仓库根目录跑，并用 --directory 指到目标树。
	if command -v git >/dev/null 2>&1; then
		if git apply --check -p1 --directory="$TARGET_DIR" "$PATCH_ABS" >/dev/null 2>&1; then
			git apply -p1 --directory="$TARGET_DIR" "$PATCH_ABS"
			return $?
		fi
	fi
	# 兜底：patch 只认当前目录，没有 git 的仓库语义。
	if command -v patch >/dev/null 2>&1; then
		if (cd "$TARGET_DIR" && patch -p1 --dry-run --forward < "$PATCH_ABS") >/dev/null 2>&1; then
			(cd "$TARGET_DIR" && patch -p1 --forward < "$PATCH_ABS" >/dev/null)
			return $?
		fi
	fi
	return 1
}

if ! apply_patch; then
	rm -rf "$TARGET_DIR"
	cp -a "$SRC_DIR" "$TARGET_DIR"
	chmod -R u+w "$TARGET_DIR"
	fail "补丁 $PATCH_REL 没有干净地应用到 ${MODULE_PATH}@${VERSION}；已把 $TARGET_DIR 还原成未打补丁的原版"
fi

# 补丁确实落到树上的标志：降权 helper 在，五处 spawn 点都接上了它。
[ -f "$TARGET_DIR/tool/privdrop/privdrop.go" ] || fail "补丁应用后仍缺 tool/privdrop/privdrop.go，补丁内容不对？"
for site in tool/bash.go tool/shell_session.go tool/task.go tool/ripgrep.go mcp/stdio.go; do
	grep -q 'privdrop.ProcAttr()' "$TARGET_DIR/$site" || fail "补丁应用后 $site 里没有 privdrop.ProcAttr()，补丁内容不对？"
done

printf 'prepare-norma: %s@%s 已就位 → %s（补丁 %s 已应用）\n' "$MODULE_PATH" "$VERSION" "$TARGET_DIR" "$PATCH_REL"

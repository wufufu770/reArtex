#!/bin/sh
# 敏感信息扫描闸 —— 本地 pre-push 钩子与 CI 的 secret-scan 门共用同一份规则。
#
# 为什么要有这道门：2026-10-26 ~ 11-02 期间，本机拓扑信息（内网网关地址、家目录
# 绝对路径、本地情报目录、本机计划路径）与私有工具的名字曾被推上公开仓库。当时的
# 钩子只扫「推送区间」（git diff 相对远端），看不见已经推上去的历史，也拦不住
# 「文件被改过但不在本次 diff 里」。现在这道门扫**整棵树 + 全部历史**，命中即失败。
#
# 用法：
#   scripts/scan-sensitive.sh all            # 所有 ref 的树 + 全部历史（CI 用）
#   scripts/scan-sensitive.sh tree <rev>     # 单个 ref 的整棵树
#   scripts/scan-sensitive.sh push <sha>...  # pre-push 用：每个待推 commit 的树 + 其祖先历史
#
# 退出码：0 = 干净；1 = 命中（打印位置与规则，敏感值本身打码）。
#
# 规则分两层：
#   - 本文件只放**通用**规则（凭据形状、任何机器都可能出现但不该进库的路径形状）——
#     这份文件进版本库，写什么公开什么。
#   - **本机私有字面量**（具体网段、具体目录名、私有工具名）不进版本库：写在本地
#     secrets/scan-patterns.local（内容模式）与 secrets/scan-paths.local（路径模式），
#     两份都已 gitignore，本地钩子会一并加载；CI 侧可用仓库 secret
#     SCAN_EXTRA_PATTERNS 传入（可选，默认不设）。
#     私有字面量必须挪出本文件的原因：闸门提交自身曾被本闸拦下 —— 扫描器不能
#     成为它要拦的那类泄漏。
#
# 实现注意（grep 兼容性，2026-11-05 修）：
#   - GNU grep 3.12 的 `-P` 只支持**单个模式**：`-f <多模式文件>` 与多个 `-e` 都会
#     报 "the -P option only supports a single pattern" 并以退出码 2 失败；若不处理，
#     错误会被当成「无命中」吞掉 —— **假阴性比误报更危险**。
#   - 因此内容扫描一律用 **git grep -P -f**（git grep 自己实现模式循环，多模式可用；
#     实测单次全树 ~0.25s），消息/历史 blob 这两处 grep 直接读文件的地方才逐模式
#     循环，且各自只 dump 一次（消息一次性导出、blob 一次性导出），循环次数 = 模式数，
#     与提交数/对象数无关。
#   - git grep -f 把模式文件的**每一行**都当模式（没有注释语法）⇒ 组装模式文件时
#     必须滤掉注释行与空行。

set -u

SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
REPO_ROOT=$(cd "$SCRIPT_DIR/.." && pwd)
cd "$REPO_ROOT" || exit 1

# ---------------------------------------------------------------------
# 规则
# ---------------------------------------------------------------------

# 禁止出现在内容里的模式（PCRE；逐行一条）。仅通用规则；
# 本机私有字面量在 secrets/scan-patterns.local。
#
# 家目录那两条用负向断言排除通用占位用户名（测试夹具里的 /home/analyst 之类
# 不该被当成泄露）。上游基线上这些模式 0 命中。
FORBIDDEN_CONTENT='Bearer [0-9a-f]{32,}
/home/(?!analyst|user|nobody|root|opt|tmp)[a-z0-9._-]+/
/Users/(?!analyst|user|nobody|root|opt|tmp)[a-z0-9._-]+/'

# 禁止出现在版本库里的路径（相对仓库根；逐行一条，shell case 语义）。
# 仅通用规则；本机私有路径在 secrets/scan-paths.local。
FORBIDDEN_PATHS='docker-compose.override.yml
docker-compose.override.yml.bak-*
secrets/
.omo/
.env
data/
jwt.key
*.db
*.db-wal
*.db-shm'

CONTENT_PATTERNS_FILE="secrets/scan-patterns.local"
PATH_PATTERNS_FILE="secrets/scan-paths.local"

PATFILE=$(mktemp) || exit 1
PATHFILE=$(mktemp) || exit 1
HITS=$(mktemp) || exit 1
OBJFILE=$(mktemp) || exit 1
BLOBFILE=$(mktemp) || exit 1
MSGFILE=$(mktemp) || exit 1
trap 'rm -f "$PATFILE" "$PATHFILE" "$HITS" "$OBJFILE" "$BLOBFILE" "$MSGFILE"' EXIT

# 组装模式文件：合并各来源后滤掉注释行与空行（git grep -f 无注释语法，
# 注释行会被当成模式 —— 而模式说明文字自己就会命中自己）。
{
	printf '%s\n' "$FORBIDDEN_CONTENT"
	if [ -f "$CONTENT_PATTERNS_FILE" ]; then
		cat "$CONTENT_PATTERNS_FILE"
	fi
	if [ -n "${SCAN_EXTRA_PATTERNS:-}" ]; then
		printf '%s\n' "$SCAN_EXTRA_PATTERNS"
	fi
} | grep -v '^#' | grep -v '^[[:space:]]*$' > "$PATFILE"

{
	printf '%s\n' "$FORBIDDEN_PATHS"
	if [ -f "$PATH_PATTERNS_FILE" ]; then
		cat "$PATH_PATTERNS_FILE"
	fi
} | grep -v '^#' | grep -v '^[[:space:]]*$' > "$PATHFILE"

# ---------------------------------------------------------------------
# 打码：命中内容不能原样进 CI 日志（那本身又是一处泄露）
# ---------------------------------------------------------------------
mask() {
	sed -E -e 's/Bearer [0-9a-f]{32,}/Bearer ***/g' \
		-e 's/[0-9a-f]{32,}/***/g' \
		-e 's#(/home|/Users)/[A-Za-z0-9._-]+/#\1/***/#g'
}

# ---------------------------------------------------------------------
# 扫描：路径（本地路径模式逐条 case 比对，语义与原 case 版本一致）
# ---------------------------------------------------------------------
scan_paths() {
	rev="$1"
	git ls-tree -r --name-only "$rev" 2>/dev/null | while IFS= read -r f; do
		[ -n "$f" ] || continue
		hit=""
		while IFS= read -r p; do
			[ -n "$p" ] || continue
			case "$f" in $p) hit=1; break ;; esac
		done < "$PATHFILE"
		[ -n "$hit" ] && echo "路径命中 [$rev]: $f" >>"$HITS"
	done
}

# ---------------------------------------------------------------------
# 扫描：某棵树的内容（git grep -P -f 一次跑全部模式）
# ---------------------------------------------------------------------
scan_tree_content() {
	rev="$1"
	out=$(git grep -I -n -P -f "$PATFILE" "$rev" -- 2>/dev/null | sort -u)
	# 空输出（无命中）不写行 —— 否则会把空行当命中。
	[ -n "$out" ] || return 0
	printf '%s\n' "$out" | mask | sed "s|^|内容命中 [$rev]: |" >>"$HITS"
}

# ---------------------------------------------------------------------
# 扫描：提交信息（一次性导出全部消息，再逐模式 grep 导出文件；
# 循环次数 = 模式数，与提交数无关）
# ---------------------------------------------------------------------
scan_messages() {
	git rev-list "$@" 2>/dev/null | while IFS= read -r sha; do
		[ -n "$sha" ] || continue
		git cat-file commit "$sha" 2>/dev/null | sed '1,/^$/d' | sed "s|^|$sha |"
	done > "$MSGFILE"
	[ -s "$MSGFILE" ] || return 0
	while IFS= read -r p; do
		[ -n "$p" ] || continue
		/usr/bin/grep -n -P -e "$p" "$MSGFILE" 2>/dev/null
	done < "$PATFILE" | mask | sed -E 's/^([0-9]+):([0-9a-f]{40}) /\2 提交信息: /' >>"$HITS"
}

# ---------------------------------------------------------------------
# 扫描：历史 blob（不只当前树）
#
# 为什么单独扫：泄漏最初是出现在**历史**里的（内容后来被从 tree 里删掉，历史还在）。
# 只扫 tip 的树看不见它。一次性导出全部对象，再逐模式 grep 导出文件。
# ---------------------------------------------------------------------
scan_history_blobs() {
	git rev-list --objects --all 2>/dev/null | awk '{print $1}' > "$OBJFILE"
	git cat-file --batch < "$OBJFILE" 2>/dev/null > "$BLOBFILE"
	while IFS= read -r p; do
		[ -n "$p" ] || continue
		/usr/bin/grep -a -n -P -e "$p" "$BLOBFILE" 2>/dev/null
	done < "$PATFILE" | mask | sed 's/^/历史内容命中: /' >>"$HITS"
}

# 历史命中时补一次定位（唯一 tree 逐个 git grep -f，拿到 file:line）。
attribute_history_hits() {
	git rev-list --all --format=%T 2>/dev/null | grep -v '^commit' | sort -u | while IFS= read -r t; do
		out=$(git grep -I -n -P -f "$PATFILE" "$t" -- 2>/dev/null | sort -u)
		[ -n "$out" ] || continue
		printf '%s\n' "$out" | mask | sed "s|^|历史内容定位 [$t]: |" >>"$HITS"
	done
}

refs_tips() {
	git for-each-ref --format='%(objectname) %(refname)' refs/heads refs/tags refs/remotes 2>/dev/null |
		while IFS=' ' read -r sha ref; do
			[ -n "$sha" ] || continue
			# 附注 tag 指向的是 tag 对象；取它解引用后的 commit。
			c=$(git rev-parse "$sha^{commit}" 2>/dev/null || echo "$sha")
			[ -n "$c" ] && echo "$c"
		done | sort -u
}

mode="${1:-all}"
shift 2>/dev/null || true

case "$mode" in
all)
	for c in $(refs_tips); do
		scan_paths "$c"
		scan_tree_content "$c"
	done
	scan_messages --all
	scan_history_blobs
	if [ -s "$HITS" ]; then
		attribute_history_hits
	fi
	;;
tree)
	rev="${1:-HEAD}"
	scan_paths "$rev"
	scan_tree_content "$rev"
	;;
push)
	for sha in "$@"; do
		case "$sha" in
		0000000000000000000000000000000000000000) continue ;;
		esac
		scan_paths "$sha"
		scan_tree_content "$sha"
	done
	scan_messages "$@"
	;;
*)
	echo "用法: $0 all | tree <rev> | push <sha>..." >&2
	exit 2
	;;
esac

if [ -s "$HITS" ]; then
	echo "敏感信息扫描：命中" >&2
	cat "$HITS" >&2
	exit 1
fi

echo "敏感信息扫描：干净"
exit 0

#!/usr/bin/env bash
# 微信小程序包路径发现（按微信版本分支探测；PC 微信 4.x 起缓存目录迁移且
# users 段是 32 位账号哈希而非用户名）。
#
# 用法：bash discover_package.sh [关键字]
#   关键字可选（小程序名/AppID 片段），用于在候选目录里过滤。
#
# 输出：候选 .wxapkg 文件路径列表（每行一个），供人工确认后交给 unpack.sh。
# 密钥的一部分在运行中的微信进程内存里 ⇒ 发现包之后、解密之前，微信进程必须存活。
set -u

KEYWORD="${1:-}"

cands=()
add() { [ -f "$1" ] && cands+=("$1"); }

# ---- Windows（Git Bash / WSL 视角）----
APPDATA_DIR="${APPDATA:-}"
if [ -n "$APPDATA_DIR" ]; then
  # 微信 4.x（xwechat/radium/users/<32位哈希>/applet/packages/<AppId>/<version>/）
  for base in "$APPDATA_DIR"/Tencent/xwechat/radium/users/*/applet/packages; do
    [ -d "$base" ] && while IFS= read -r f; do add "$f"; done < <(find "$base" -name '*.wxapkg' 2>/dev/null)
  done
  # 微信 3.x 及更早（WeChat Files/Applet/<AppID>/...）
  for base in "$APPDATA_DIR"/Tencent/WeChat\ Files/Applet/*/; do
    [ -d "$base" ] && while IFS= read -r f; do add "$f"; done < <(find "$base" -name '__APP__.wxapkg' -o -name '*.wxapkg' 2>/dev/null)
  done
fi

# ---- macOS ----
for base in ~/Library/Containers/com.tencent.xinWeChat/Data/Library/Application\ Support/com.tencent.xinWeChat/*/*/Applet/*/; do
  [ -d "$base" ] && while IFS= read -r f; do add "$f"; done < <(find "$base" -name '*.wxapkg' 2>/dev/null)
done

# ---- 通用兜底：工作目录里人工放置的包（人工供包口子的落点）----
for base in ./ ./data/artifacts/miniprogram/; do
  while IFS= read -r f; do add "$f"; done < <(find "$base" -maxdepth 2 -name '*.wxapkg' 2>/dev/null)
done

if [ "${#cands[@]}" -eq 0 ]; then
  echo "未发现 .wxapkg 缓存。可能原因：①微信版本目录又迁移（人工定位后把包放进 artifacts/miniprogram/）；②目标小程序未在 PC 端打开过（先打开让它缓存）；③KEYWORD 过滤太严。"
  exit 1
fi

for f in "${cands[@]}"; do
  if [ -n "$KEYWORD" ] && ! echo "$f" | grep -qi "$KEYWORD"; then
    continue
  fi
  echo "$f"
done

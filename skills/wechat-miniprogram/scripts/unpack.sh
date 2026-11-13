#!/usr/bin/env bash
# 解包编排器（0.12.0 §四-B1）：解密解包 → 反编译还原 → 配置矫正 → 内容级校验。
# 退出码即判定：0=三判据全过；非 0=还原失败（详情在输出里）。
#
# 用法：unpack.sh <wxapkg路径> <appid>
# 产物：artifacts/miniprogram/<appid>/src/（源码工程，供 wxa-runner 与业务建模读取）
#
# 后端选择（probe_unpackers.sh 实测口径）：
#   解密解包：wxapkg CLI（wux1an，若可用）；否则提示人工 GUI（2.x 无 CLI）。
#   反编译还原：unveilr → Wedecode → Gwxapkg（第一个可用者）。
# 注意：密钥一部分在微信进程内存里——解密前微信进程必须存活（人工口子，见 SKILL.md）。
set -u

PKG="${1:-}"
APPID="${2:-}"
if [ -z "$PKG" ] || [ -z "$APPID" ] || [ ! -f "$PKG" ]; then
  echo "用法: unpack.sh <wxapkg路径> <appid>　（wxapkg 文件不存在）" >&2
  exit 2
fi

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
# 产物根 = 调用方工作目录下的 artifacts/miniprogram/（工作目录纪律：data_dir 固化）。
OUT_ROOT="${ARTEX_MP_ARTIFACTS:-artifacts/miniprogram}"
OUT="$OUT_ROOT/$APPID"
WORK="$OUT/_work"
rm -rf "$WORK"
mkdir -p "$WORK" "$OUT"

have() { command -v "$1" >/dev/null 2>&1; }

# ---- ① 解密解包 ----
if have wxapkg && wxapkg --help 2>&1 | grep -q 'unpack'; then
  echo "[unpack] ① wxapkg CLI 解密解包"
  wxapkg unpack --path "$PKG" --output "$WORK" || { echo "[unpack] 解密解包失败" >&2; exit 3; }
else
  echo "[unpack] ① wxapkg CLI 不可用（2.x GUI 形态或未安装）——请人工用 GUI 解密解包到 $WORK 后重跑本脚本（加 --skip-decrypt）" >&2
  [ "${2:-}" = "--skip-decrypt" ] || [ "${3:-}" = "--skip-decrypt" ] || exit 3
fi

# ---- ② 反编译还原（第一个可用后端）----
SRC="$OUT/src"
restored=0
if have unveilr; then
  echo "[unpack] ② unveilr 反编译还原"
  unveilr -d "$WORK" -o "$SRC" && restored=1
fi
if [ "$restored" -eq 0 ] && have wedecode; then
  echo "[unpack] ② Wedecode 反编译还原"
  wedecode -i "$WORK" -o "$SRC" && restored=1
fi
if [ "$restored" -eq 0 ] && { have Gwxapkg || have gwxapkg; }; then
  echo "[unpack] ② Gwxapkg 反编译还原"
  (have Gwxapkg && Gwxapkg -i "$WORK" -o "$SRC") || (have gwxapkg && gwxapkg -i "$WORK" -o "$SRC")
  [ $? -eq 0 ] && restored=1
fi
[ "$restored" -eq 1 ] || { echo "[unpack] 无可用反编译后端（跑 probe_unpackers.sh 看清单）" >&2; exit 4; }

# ---- ③ 配置矫正 ----
echo "[unpack] ③ 配置矫正（五关两开 + app.json 矫正）"
python3 "$SCRIPT_DIR/fix_miniprogram_config.py" "$SRC" || { echo "[unpack] 配置矫正失败" >&2; exit 5; }

# ---- ④ 内容级校验（退出码即判定）----
echo "[unpack] ④ 内容级校验（三判据）"
python3 "$SCRIPT_DIR/validate_unpack.py" "$SRC" || { echo "[unpack] 还原校验未过——不要当作成功继续（见上方失败项）" >&2; exit 6; }

echo "[unpack] 完成：$SRC（源码工程，wxa-runner 与业务建模的输入）"
exit 0

#!/usr/bin/env bash
# 解包工具探针（施工 E0 / B0）：实测本机各候选后端的可用性，产出结论供 unpack.sh
# 选择默认后端。只做探测，不改任何文件。
#
# 判据（每项 0/1）：
#   ① CLI 可用性（--help/版本退出码）
#   ② 还原形态（是否输出源码工程 app.json / app.js）
#   ③ $gwx 变体处理无法静态判定 ⇒ 由 unpack.sh 的内容级校验在真实包上判。
#
# 用法：bash probe_unpackers.sh
set -u

report() { printf '[probe] %s\n' "$*"; }

have() { command -v "$1" >/dev/null 2>&1; }

# ---- 候选 1：wux1an/wxapkg（解密+解包；1.x 有 CLI unpack，2.x 为 Wails GUI——
#      以实测为准：--help 退出码 0 且输出含 unpack ⇒ CLI 可用）----
wuxia_ok=0
if have wxapkg; then
  if wxapkg --help 2>&1 | grep -q 'unpack'; then
    wuxia_ok=1
    report "wux1an/wxapkg: CLI 可用（$(wxapkg --version 2>/dev/null || echo version未知)）"
  else
    report "wux1an/wxapkg: 存在但无 CLI unpack（2.x GUI 形态）——只可人工 GUI 操作"
  fi
else
  report "wux1an/wxapkg: 未安装"
fi

# ---- 候选 2：unveilr（反编译还原；仓库状态多变，以本机可执行为准）----
unveilr_ok=0
if have unveilr; then
  unveilr_ok=1
  report "unveilr: CLI 可用"
else
  report "unveilr: 未安装（npm i -g 或使用其 fork）"
fi

# ---- 候选 3：Gwxapkg ----
gwx_ok=0
if have Gwxapkg || have gwxapkg; then
  gwx_ok=1
  report "Gwxapkg: CLI 可用"
else
  report "Gwxapkg: 未安装"
fi

# ---- 候选 4：Wedecode ----
wede_ok=0
if have wedecode; then
  wede_ok=1
  report "Wedecode: CLI 可用"
else
  report "Wedecode: 未安装（npm i -g wedecode）"
fi

# ---- 运行时：node 版本（wxa-runner 要求 >=18）----
node_ok=0
if have node; then
  major="$(node -p 'process.versions.node.split(".")[0]' 2>/dev/null || echo 0)"
  if [ "${major:-0}" -ge 18 ]; then
    node_ok=1
    report "node: $(node --version)（wxa-runner 可用）"
  else
    report "node: $(node --version) < 18 —— wxa-runner 不可用，先升级"
  fi
else
  report "node: 未安装"
fi

report "结论：解密解包后端候选=[wxapkg=${wuxia_ok} unveilr=${unveilr_ok} Gwxapkg=${gwx_ok} Wedecode=${wede_ok}] node=${node_ok}"
report "unpack.sh 的后端选择顺序：可用反编译器（unveilr/Gwxapkg/Wedecode）之一做还原；解密优先 wxapkg CLI。全部为 0 ⇒ 需先安装工具（人工口子的一部分）。"

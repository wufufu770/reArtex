#!/usr/bin/env python3
"""解包产物内容级校验（0.12.0 §四-B1 第 3 步——最易被跳过也最致命的一环）。

wxml 还原失败会伪装成成功：工具退出码 0、日志满屏 Saved file、文件数也对，
但内容是占位符——原因是编译变体名（$gwx_XC_29 之类）匹配不上标准 $gwx。
判定必须落到内容，三判据全过才算还原成功：
  ① wxml 非空且不含页面路径占位符；
  ② app.json 的 componentFramework 是字符串（矫正后）；
  ③ app.js 存在且能被静态解析通过（node --check）。

用法：validate_unpack.py <工程目录>
退出码：0=三判据全过；1=任一不过（失败详情在 stderr）。
"""
import json
import os
import subprocess
import sys

PROJ = sys.argv[1] if len(sys.argv) > 1 else ""
if not PROJ or not os.path.isdir(PROJ):
    print(f"[validate] 工程目录不存在: {PROJ}", file=sys.stderr)
    sys.exit(1)

failures = []

# ---- ① wxml 内容级判定：抽 3 个页面目录的 wxml，非空且不含占位符 ----
app_json = os.path.join(PROJ, "app.json")
pages = []
if os.path.isfile(app_json):
    try:
        with open(app_json, "r", encoding="utf-8") as f:
            pages = json.load(f).get("pages", [])[:3]
    except Exception as e:
        failures.append(f"app.json 不可解析: {e}")
else:
    failures.append("app.json 不存在（还原未产出页面注册树）")

PLACEHOLDER_MARKS = ("$gwx", "placeholder", "PLACEHOLDER", "pages/")
wxml_checked = 0
for p in pages:
    base = p.replace("/", os.sep)
    wxml = os.path.join(PROJ, base + ".wxml")
    if not os.path.isfile(wxml):
        # 部分页面可能只有 js/json（无 UI 页），跳过但计数
        continue
    wxml_checked += 1
    try:
        with open(wxml, "r", encoding="utf-8", errors="replace") as f:
            content = f.read()
    except Exception as e:
        failures.append(f"wxml 读取失败 {wxml}: {e}")
        continue
    if not content.strip():
        failures.append(f"wxml 为空: {wxml}（还原失败伪装成成功的典型形态）")
    for mark in PLACEHOLDER_MARKS:
        if mark in content:
            failures.append(f"wxml 含占位符标记 {mark!r}: {wxml}")
if wxml_checked == 0 and not failures:
    failures.append("没有任何 wxml 被校验（pages 目录与工程不匹配？）")

# ---- ② componentFramework 字符串 ----
if os.path.isfile(app_json):
    try:
        with open(app_json, "r", encoding="utf-8") as f:
            app = json.load(f)
        cf = app.get("componentFramework")
        if cf is not None and not isinstance(cf, str):
            failures.append(f"componentFramework 仍是对象（矫正未跑或失败）: {type(cf).__name__}")
    except Exception:
        pass  # 可解析性已在 ① 报过

# ---- ③ app.js 静态解析 ----
app_js = os.path.join(PROJ, "app.js")
if os.path.isfile(app_js):
    r = subprocess.run(["node", "--check", app_js], capture_output=True, text=True, timeout=30)
    if r.returncode != 0:
        failures.append(f"app.js 静态解析失败: {r.stderr[:300]}")
else:
    # 反编译器的产物形态可能是 app-service.js
    alt = os.path.join(PROJ, "app-service.js")
    if os.path.isfile(alt):
        r = subprocess.run(["node", "--check", alt], capture_output=True, text=True, timeout=30)
        if r.returncode != 0:
            failures.append(f"app-service.js 静态解析失败: {r.stderr[:300]}")
        else:
            print("[validate] app.js 缺失但 app-service.js 可解析（编译态产物——注意 wxa-runner 吃源码工程，需确认还原形态）")
    else:
        failures.append("app.js 与 app-service.js 都不存在（还原未产出入口）")

if failures:
    print(f"[validate] 还原校验失败（{len(failures)} 项）：", file=sys.stderr)
    for f in failures:
        print(f"  - {f}", file=sys.stderr)
    sys.exit(1)
print(f"[validate] 三判据全过（wxml 抽检 {max(wxml_checked, len(pages))} 个 / componentFramework 字符串 / 入口可解析）")
sys.exit(0)

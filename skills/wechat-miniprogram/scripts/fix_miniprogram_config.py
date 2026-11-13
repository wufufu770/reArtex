#!/usr/bin/env python3
"""小程序工程配置矫正（0.12.0 §四-B1 第 2 步）。

解包还原后的工程直接跑 wxa-runner 会踩两类坑：
  ① project.config.json 的编译开关：es6/postcss/minified/enhance/minifyWXML 五个
     要关（还原产物已是目标形态，再编译一次会破坏），ignoreUploadUnusedFiles 与
     ignoreDevUnusedFiles 两个要置 false（动态 require 会被依赖分析判为无人引用
     而剔除，页面注册整体失败）；
  ② app.json：componentFramework 必须是字符串而不是对象；plugins 里的 subpackage
     字段要删；__plugin__ 开头的页面要从 pages 数组剔掉。

用法：fix_miniprogram_config.py <工程目录>
退出码：0=矫正完成（无配置文件也按 0 处理——部分包没有这两件）；1=读写失败。
"""
import json
import os
import sys

PROJ = sys.argv[1] if len(sys.argv) > 1 else ""
if not PROJ or not os.path.isdir(PROJ):
    print(f"[fix] 工程目录不存在: {PROJ}", file=sys.stderr)
    sys.exit(1)

changed = []

# ---- project.config.json：五关两开 ----
pc_path = os.path.join(PROJ, "project.config.json")
if os.path.isfile(pc_path):
    try:
        with open(pc_path, "r", encoding="utf-8") as f:
            cfg = json.load(f)
    except Exception as e:
        print(f"[fix] project.config.json 解析失败: {e}", file=sys.stderr)
        sys.exit(1)
    setting = cfg.setdefault("setting", {})
    # 五关：还原产物已是目标形态，再编译会破坏。
    for k in ("es6", "postcss", "minified", "enhance", "minifyWXML"):
        if setting.get(k) is not False:
            setting[k] = False
            changed.append(f"setting.{k}=False")
    # 两开（置 false 才是「不剔除」）：动态 require 会被依赖分析判为无人引用
    # 而剔除，页面注册整体失败。
    for k in ("ignoreUploadUnusedFiles", "ignoreDevUnusedFiles"):
        if setting.get(k) is not False:
            setting[k] = False
            changed.append(f"setting.{k}=False")
    with open(pc_path, "w", encoding="utf-8") as f:
        json.dump(cfg, f, ensure_ascii=False, indent=2)
else:
    print("[fix] 无 project.config.json（跳过编译开关矫正）")

# ---- app.json：componentFramework 字符串化 / plugins.subpackage 删除 / __plugin__ 页面剔除 ----
app_path = os.path.join(PROJ, "app.json")
if os.path.isfile(app_path):
    try:
        with open(app_path, "r", encoding="utf-8") as f:
            app = json.load(f)
    except Exception as e:
        print(f"[fix] app.json 解析失败: {e}", file=sys.stderr)
        sys.exit(1)
    cf = app.get("componentFramework")
    if isinstance(cf, dict):
        app["componentFramework"] = cf.get("type") or cf.get("name") or "glass-easel"
        changed.append(f"componentFramework 对象→字符串({app['componentFramework']})")
    plugins = app.get("plugins")
    if isinstance(plugins, dict):
        for name, p in list(plugins.items()):
            if isinstance(p, dict) and "subpackage" in p:
                p.pop("subpackage", None)
                changed.append(f"plugins.{name}.subpackage 删除")
    pages = app.get("pages")
    if isinstance(pages, list):
        kept = [p for p in pages if not str(p).startswith("__plugin__")]
        if len(kept) != len(pages):
            changed.append(f"pages 剔除 __plugin__ 页面 {len(pages) - len(kept)} 个")
            app["pages"] = kept
    with open(app_path, "w", encoding="utf-8") as f:
        json.dump(app, f, ensure_ascii=False, indent=2)
else:
    print("[fix] 无 app.json（跳过应用配置矫正）")

print(f"[fix] 完成：{len(changed)} 处矫正 → {changed if changed else '无需改动'}")
sys.exit(0)

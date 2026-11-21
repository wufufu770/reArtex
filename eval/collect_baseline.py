#!/usr/bin/env python3
"""Veto baseline 聚合器（E2，0.15.33）。

把 data/veto/<taskID>/ 下的任务终态导出聚合为 Veto 可消费的
responses.json + identity.json，供 `veto eval_core score` 录制 baseline。

用法：
  python3 eval/collect_baseline.py --data-dir data --out /tmp/baseline
  # 产 /tmp/baseline/responses.json + identity.json
  # （suite 已在 eval/suite.json；score/compare 见 eval/README.md）

聚合规则：
  - 只收终态导出（data/veto/<id>/responses.json 存在 = 该任务已到终态）；
  - 任务 id 与 eval/suite.json 对齐的（golden 任务按套件 id 命名）才计入，
    其余任务导出被忽略（baseline 口径纯净）;
  - identity 取任务导出里的 prompt_version 并集（不一致即报错退出——基线
    必须同一构建录制）。
"""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--data-dir", default="data")
    ap.add_argument("--suite", default="eval/suite.json")
    ap.add_argument("--out", required=True, help="输出目录（放 responses.json/identity.json）")
    args = ap.parse_args()

    suite = json.loads(Path(args.suite).read_text())
    suite_ids = {t["id"] for t in suite["tasks"]}

    root = Path(args.data_dir) / "veto"
    if not root.is_dir():
        print(f"没有终态导出目录: {root}", file=sys.stderr)
        return 1

    responses: list[dict] = []
    versions: set[str] = set()
    missing = sorted(suite_ids)
    for entry in sorted(root.iterdir()):
        rf = entry / "responses.json"
        if not rf.is_file():
            continue
        task_id = entry.name
        if task_id not in suite_ids:
            continue
        doc = json.loads(rf.read_text())
        responses.extend(doc)
        idf = entry / "identity.json"
        if idf.is_file():
            ident = json.loads(idf.read_text())
            versions.add(ident.get("prompt_version", ""))
        missing.remove(task_id)

    if missing:
        print(f"套件 {len(missing)} 个任务尚未到终态（无导出）: {missing}", file=sys.stderr)
        return 2
    if len(versions) > 1:
        print(f"基线必须同一构建录制，发现多个 prompt_version: {versions}", file=sys.stderr)
        return 3
    if not versions:
        versions = {"unknown"}

    out = Path(args.out)
    out.mkdir(parents=True, exist_ok=True)
    (out / "responses.json").write_text(json.dumps(responses, ensure_ascii=False, indent=1))
    (out / "identity.json").write_text(json.dumps({
        "model": "reartex-worker",
        "harness": "reartex",
        "prompt_version": next(iter(versions)),
        "stack": {"harness": "reartex", "prompt_version": next(iter(versions))},
    }, ensure_ascii=False, indent=1))
    print(f"聚合完成: {len(responses)} 条 → {out}/responses.json（prompt_version={next(iter(versions))}）")
    return 0


if __name__ == "__main__":
    sys.exit(main())

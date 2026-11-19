#!/usr/bin/env python3
"""
RSI 治理门禁（0.15.18，批次 C 观察模式）——reArtex 修改标准的机器可执行面。

用途（源自外部实现工具包 phase4 的 MODIFICATION_STANDARDS + rsi_guard.py，按本仓
文件树重写）：
  - Tier 0（不可变核心）硬锁：guard/ netguard/ pathguard/ internal/sod/
    internal/sodpolicy/ third_party/norma/ patches/ selfupdate/ 及清单内的
    SoD/治理/审计/密钥文件——自动演化永不触碰；
  - Tier 1（例行动微演化）：skills/ 与域提示词（agent/prompts 非豁免清单）；
  - Tier 2（人工合并的宏观演化）：核心编排/server/db/web 等；
  - 膨胀帽：skills/*/SKILL.md ≤300 行；agent/prompts/*.md 相对基线增幅 ≤+15%；
  - 脱敏门：skills/prompts 内不得出现硬编码 IPv4（TEST-NET 192.0.2.0/24 除外）
    与常见 secret 形状；
  - 溯源（可选 --require-evo-id）：commit message 需带 [EVO-YYYYMMDD-NNN]。

观察模式（默认）：只打印报告、退出码恒 0——当前变更仍由人主导，门禁先跑出基线；
启用 RSI 时加 --enforce 切强制（命中即非零退出，pre-commit/CI 拦截）。

用法：
  python3 scripts/rsi_guard.py --staged                      # 暂存区（观察）
  python3 scripts/rsi_guard.py --base <ref>                  # 相对基线的区间（CI 观察）
  python3 scripts/rsi_guard.py --staged --enforce --tier 1   # 强制模式
"""

from __future__ import annotations

import argparse
import math
import re
import subprocess
import sys
from pathlib import Path

# Tier 0 前缀：不可变核心目录（整目录硬锁）。
TIER_0_PREFIXES = (
    "guard/",
    "netguard/",
    "pathguard/",
    "internal/sod/",
    "internal/sodpolicy/",
    "third_party/norma/",
    "patches/",
    "selfupdate/",
)

# Tier 0 精确清单：SoD/治理/审计/密钥的单文件硬锁（0.15.18 按当前文件树核定，
# 全部实测存在）。
TIER_0_EXACT = {
    # agent/ 策略、SoD、护栏
    "agent/approval_policy.go",
    "agent/scope_policy.go",
    "agent/sod_scope.go",
    "agent/sod_tools.go",
    "agent/sensitive_result.go",
    "agent/tier_judge.go",
    "agent/edge_gate.go",
    "agent/strategy_proposal_guard.go",
    "agent/constraints.go",
    "agent/toolpolicy.go",
    # server/ SoD、治理、审计闸
    "server/sod_api.go",
    "server/sod_categories_api.go",
    "server/sod_chain.go",
    "server/sod_feed.go",
    "server/sod_review_store.go",
    "server/egress_governance.go",
    "server/governance_tools.go",
    "server/role_policy.go",
    "server/trust_rules.go",
    "server/auditor_gate.go",
    "server/merged_gate_wiring.go",
    "server/task_scope_guard.go",
    # db/ 密钥、角色授权、审计链
    "db/crypto.go",
    "db/nkey.go",
    "db/audit_chain.go",
    "db/role_grants.sql",
    # 密钥与平台自更新
    "config/masterkey.go",
    # 治理标准本体自保护（改标准必须人工且显式）
    "docs/RSI修改标准.md",
    "scripts/rsi_guard.py",
}

# intercept/ 整目录 Tier 0，唯 intercept/prompt.go（职责链边界提示词）是 Tier 2。
TIER_2_INTERCEPT_EXCEPTIONS = {"intercept/prompt.go"}

# Tier 1：域提示词（评审批次的例行动微演化）。
TIER_1_PROMPTS = {
    "agent/prompts/arbiter.md",
    "agent/prompts/creative.md",
    "agent/prompts/curator.md",
    "agent/prompts/redteam.md",
    "agent/prompts/verifier.md",
    "agent/prompts/miniprogram.md",
    "agent/prompts/edge.md",
    "agent/prompts/edge-exec.md",
}

# Tier 2：职责链边界提示词（人工合并）。
TIER_2_PROMPTS = {
    "agent/prompts/intent-normalizer.md",
    "agent/prompts/policy-gate.md",
}

# Tier 2 前缀：核心编排与平台面（宏观演化，人工合并）。
TIER_2_PREFIXES = (
    "agent/",
    "mcphttp/",
    "db/",
    "server/",
    "web/",
    "enrich/",
    "evidence/",
    "report/",
    "llmpool/",
    "llmrec/",
    "traffic/",
    "cmd/",
)

TIER_2_EXACT = {"config/config.go"}

SKILL_MAX_LINES = 300
PROMPT_MAX_GROWTH_RATIO = 0.15

IPV4_RE = re.compile(r"\b(?:\d{1,3}\.){3}\d{1,3}\b")
SAFE_IPS = {"127.0.0.1", "0.0.0.0", "255.255.255.255"}
SECRET_RE = re.compile(r"\b(?:sk-[A-Za-z0-9]{16,}|ghp_[A-Za-z0-9]{20,}|AKIA[0-9A-Z]{16})\b")
EVO_ID_RE = re.compile(r"\[EVO-\d{8}-\d{3}\]")


def run_git(args: list[str]) -> str:
    res = subprocess.run(["git", *args], capture_output=True, text=True, check=False)
    if res.returncode != 0:
        raise RuntimeError(f"git {' '.join(args)} failed: {res.stderr.strip()}")
    return res.stdout


def classify_path(path: str) -> int | None:
    if path in TIER_2_INTERCEPT_EXCEPTIONS:
        return 2
    if path.startswith("intercept/"):
        return 0
    if path in TIER_0_EXACT or path.startswith(TIER_0_PREFIXES):
        return 0
    if path.startswith("skills/") or path in TIER_1_PROMPTS:
        return 1
    if path in TIER_2_PROMPTS or path in TIER_2_EXACT or path.startswith(TIER_2_PREFIXES):
        return 2
    return None


def get_changed_files(staged: bool, base: str | None) -> list[tuple[str, str]]:
    if staged:
        out = run_git(["diff", "--cached", "--name-status"])
    elif base:
        out = run_git(["diff", "--name-status", f"{base}...HEAD"])
    else:
        out = run_git(["diff", "--name-status", "HEAD"])
    entries: list[tuple[str, str]] = []
    for line in out.splitlines():
        if not line.strip():
            continue
        parts = line.split("\t")
        status = parts[0][0]
        path = parts[-1]
        entries.append((status, path))
    return entries


def read_file_lines(path: str, staged: bool) -> list[str]:
    if staged:
        res = subprocess.run(["git", "show", f":{path}"], capture_output=True, text=True, check=False)
        if res.returncode == 0:
            return res.stdout.splitlines()
    p = Path(path)
    if p.exists():
        return p.read_text(encoding="utf-8", errors="replace").splitlines()
    return []


def read_base_line_count(path: str, base_ref: str) -> int | None:
    res = subprocess.run(["git", "show", f"{base_ref}:{path}"], capture_output=True, text=True, check=False)
    if res.returncode != 0:
        return None
    return len(res.stdout.splitlines())


def check_sanitization(path: str, lines: list[str]) -> list[str]:
    errors: list[str] = []
    for idx, line in enumerate(lines, start=1):
        for ip in IPV4_RE.findall(line):
            if ip not in SAFE_IPS and not ip.startswith("192.0.2."):
                errors.append(f"{path}:{idx}: 硬编码 IPv4 字面量 '{ip}' 触发脱敏门")
        if SECRET_RE.search(line):
            errors.append(f"{path}:{idx}: 疑似 secret/token 字面量")
    return errors


def main() -> int:
    parser = argparse.ArgumentParser(description="reArtex RSI 修改标准门禁（观察模式默认）。")
    mode = parser.add_mutually_exclusive_group()
    mode.add_argument("--staged", action="store_true", help="检查暂存区（pre-commit 场景）")
    mode.add_argument("--base", type=str, help="基线 ref（CI 区间场景，如 origin/main 或 event.before）")
    parser.add_argument("--tier", type=int, choices=[1, 2], default=1, help="允许的 RSI 层级上限（默认 1）")
    parser.add_argument("--enforce", action="store_true", help="强制模式：命中即非零退出（默认观察模式恒 0）")
    parser.add_argument("--require-evo-id", action="store_true", help="要求 HEAD commit 带 [EVO-YYYYMMDD-NNN]")
    args = parser.parse_args()

    base_ref = args.base if args.base else "HEAD"
    changed = get_changed_files(staged=args.staged, base=args.base)
    if not changed:
        print("rsi_guard[观察]: 无变更可查。")
        return 0

    violations: list[str] = []
    tier0_touched = 0

    for status, path in changed:
        if status == "D":
            continue
        tier = classify_path(path)
        if tier == 0:
            tier0_touched += 1
            violations.append(f"[Tier 0 硬锁] '{path}' 属不可变核心，自动演化不得改写（人工直接编辑除外——观察模式仅记录）。")
        elif tier == 2 and args.tier < 2:
            violations.append(f"[Tier 2 越层] '{path}' 需 --tier 2（宏观演化，人工合并 PR）。")

        is_skill = path.startswith("skills/") and path.endswith("/SKILL.md")
        is_prompt = path.startswith("agent/prompts/") and path.endswith(".md")

        if is_skill or is_prompt:
            lines = read_file_lines(path, staged=args.staged)
            if is_skill and len(lines) > SKILL_MAX_LINES:
                violations.append(f"[膨胀帽] '{path}' 共 {len(lines)} 行（上限 {SKILL_MAX_LINES}）。")
            if is_prompt:
                old_count = read_base_line_count(path, base_ref)
                if old_count and old_count > 0:
                    max_allowed = old_count + math.ceil(old_count * PROMPT_MAX_GROWTH_RATIO)
                    if len(lines) > max_allowed:
                        violations.append(
                            f"[膨胀帽] '{path}' 由 {old_count} 行增至 {len(lines)} 行（上限 +15% = {max_allowed}）。"
                        )
            violations.extend(check_sanitization(path, lines))

    if args.require_evo_id:
        commit_msg = run_git(["log", "-1", "--pretty=%B"])
        if not EVO_ID_RE.search(commit_msg):
            violations.append("[溯源] 最新 commit message 缺 [EVO-YYYYMMDD-NNN] 引用。")

    mode_label = "强制" if args.enforce else "观察"
    if violations:
        stream = sys.stderr if args.enforce else sys.stdout
        print(f"rsi_guard[{mode_label}] 命中 {len(violations)} 条（Tier0 触碰 {tier0_touched} 个文件）：", file=stream)
        for v in violations:
            print(f"  - {v}", file=stream)
        if args.enforce:
            return 1
        print(
            "rsi_guard[观察]: 以上为基线记录，未阻断（强制模式需 --enforce）。",
            file=sys.stdout,
        )
        return 0

    print(f"rsi_guard[{mode_label}] 通过（{len(changed)} 个文件按 Tier {args.tier} 规则核查）。")
    return 0


if __name__ == "__main__":
    sys.exit(main())

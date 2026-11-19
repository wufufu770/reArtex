"""飞书文本命令 → reArtex REST 的路由表（真实契约版）。"""

from __future__ import annotations

from .approval_coordinator import FirstResponderCoordinator
from .reartex_client import ReartexClient

HELP_TEXT = (
    "reArtex 远程命令：\n"
    "  /status — 任务总览（进行中/排队/终态计数 + 最新任务）\n"
    "  /start <目标> [simple|medium|hard] [fast|balanced|super_heavy] — 建任务\n"
    "  /pause <task_id> — 暂停任务\n"
    "  /resume <task_id> — 恢复任务\n"
    "  /approve <pending_id> — 批准拦截审批\n"
    "  /deny <pending_id> — 拒绝拦截审批\n"
    "  /help — 本帮助"
)


def handle_inbound_command(
    channel: str,
    sender_id: str,
    text: str,
    client: ReartexClient,
    coordinator: FirstResponderCoordinator,
) -> str:
    parts = text.strip().split()
    if not parts:
        return HELP_TEXT
    cmd = parts[0].lower()

    if cmd in {"/help", "help"}:
        return HELP_TEXT

    if cmd == "/status":
        data = client.get_status()
        tasks = data.get("tasks") or []
        active = [t for t in tasks if t.get("status") in ("running", "paused", "queued")]
        lines = [f"任务总数 {len(tasks)}，非终态 {len(active)}。"]
        for t in active[:5]:
            lines.append(f"  #{t.get('id')} [{t.get('status')}] {(t.get('description') or t.get('goal') or '')[:60]}")
        if not active:
            lines.append("  （当前没有进行中的任务）")
        return "\n".join(lines)

    if cmd == "/start" and len(parts) >= 2:
        # /start <目标...> [难度] [模式]：尾部若命中枚举词则摘出，其余拼目标。
        rest = parts[1:]
        difficulty, mode = "auto", "auto"
        if rest and rest[-1] in {"fast", "balanced", "super_heavy"}:
            mode = rest.pop()
        if rest and rest[-1] in {"simple", "medium", "hard"}:
            difficulty = rest.pop()
        goal = " ".join(rest)
        res = client.start_task(
            description=f"飞书通道任务（by {sender_id[:8]}）",
            goal=goal,
            difficulty=difficulty,
            mode=mode,
        )
        return f"任务 #{res.get('id')} 已创建（难度={difficulty} 模式={mode}），目标：{goal}"

    if cmd == "/pause" and len(parts) >= 2:
        res = client.control_tasks([parts[1]], "pause")
        return f"暂停请求已提交：{res}"

    if cmd == "/resume" and len(parts) >= 2:
        res = client.control_tasks([parts[1]], "resume")
        return f"恢复请求已提交：{res}"

    if cmd in {"/approve", "/deny"} and len(parts) >= 2:
        try:
            approval_id = int(parts[1])
        except ValueError:
            return "审批 id 必须是数字（审批页/广播卡里的 #编号）"
        decision = "allowed" if cmd == "/approve" else "denied"
        outcome = coordinator.resolve_once(approval_id, channel, sender_id, decision)
        if not outcome["accepted"]:
            return outcome["message"]
        try:
            client.decide_pending(approval_id, decision)
            return outcome["message"]
        except Exception as exc:  # noqa: BLE001
            return f"首响已锁定（{decision}），但 reArtex 裁决回调失败：{exc}"

    return "未知命令。发送 /help 查看用法。"

"""首响锁：同一审批在多渠道/多成员并发裁决时只接受第一响，其余明确告知已决。"""

from __future__ import annotations

import threading
from typing import Any


class FirstResponderCoordinator:
    def __init__(self) -> None:
        self._lock = threading.Lock()
        self._resolved: dict[int, dict[str, str]] = {}

    def register(self, approval_id: int, payload: dict) -> None:
        """广播时占位（供日志/去重扩展用）；裁决仍以 resolve_once 为准。"""

    def resolve_once(self, approval_id: int, channel: str, sender_id: str, decision: str) -> dict[str, Any]:
        with self._lock:
            if approval_id in self._resolved:
                winner = self._resolved[approval_id]
                return {
                    "accepted": False,
                    "winner_channel": winner["channel"],
                    "decision": winner["decision"],
                    "message": f"该审批已由 {winner['channel']} 以「{winner['decision']}」裁决，不再重复处理。",
                }
            self._resolved[approval_id] = {"channel": channel, "sender_id": sender_id, "decision": decision}
            return {
                "accepted": True,
                "winner_channel": channel,
                "decision": decision,
                "message": f"已接受来自 {channel} 的「{decision}」。",
            }

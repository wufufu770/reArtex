"""reArtex REST 客户端——按真实契约（0.15.19 核对）：

  - POST /api/tasks                          {description, goal, difficulty?, mode?}
  - GET  /api/tasks                          任务清单+active
  - POST /api/tasks/control/batch            {task_ids, action: pause|resume}
  - POST /api/intercept/pending/{id}/decide  {decision: allowed|denied, task_tool?}
  - GET  /api/intercept/pending/{id}         pending 详情（审批卡渲染用）

工具包里猜的 /api/intercept/{id}/resolve 与 tasks 的 target 字段在本仓不存在。
"""

from __future__ import annotations

from typing import Any
import httpx


class ReartexClient:
    def __init__(self, base_url: str, api_token: str) -> None:
        self.base_url = base_url.rstrip("/")
        headers = {"Content-Type": "application/json"}
        if api_token:
            headers["Authorization"] = f"Bearer {api_token}"
        self._client = httpx.Client(base_url=self.base_url, headers=headers, timeout=15.0)

    def get_status(self) -> dict[str, Any]:
        resp = self._client.get("/api/tasks")
        resp.raise_for_status()
        return resp.json()

    def start_task(self, description: str, goal: str, difficulty: str, mode: str) -> dict[str, Any]:
        payload = {"description": description, "goal": goal, "difficulty": difficulty, "mode": mode}
        resp = self._client.post("/api/tasks", json=payload)
        resp.raise_for_status()
        return resp.json()

    def control_tasks(self, task_ids: list[str], action: str) -> dict[str, Any]:
        resp = self._client.post(
            "/api/tasks/control/batch",
            json={"task_ids": task_ids, "action": action},
        )
        resp.raise_for_status()
        return resp.json()

    def pending_detail(self, pending_id: int) -> dict[str, Any]:
        resp = self._client.get(f"/api/intercept/pending/{pending_id}")
        resp.raise_for_status()
        return resp.json()

    def decide_pending(self, pending_id: int, decision: str, task_tool: bool = False) -> dict[str, Any]:
        resp = self._client.post(
            f"/api/intercept/pending/{pending_id}/decide",
            json={"decision": decision, "task_tool": task_tool},
        )
        resp.raise_for_status()
        return resp.json()

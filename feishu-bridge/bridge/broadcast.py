"""reArtex → 飞书广播接收端：ask pending 落库后的审批要素推送入口。

唯一的监听面：POST /internal/approval（X-Bridge-Secret 鉴权）。其余全部出向
（WS 长连接收指令 + REST 调 reArtex）——桥本身零公网依赖。
"""

from __future__ import annotations

import logging

from fastapi import FastAPI, Header, HTTPException

from .approval_coordinator import FirstResponderCoordinator
from .config import BridgeConfig
from .feishu_ws import FeishuWebSocketAdapter

logger = logging.getLogger(__name__)


def create_app(
    cfg: BridgeConfig,
    adapter: FeishuWebSocketAdapter,
    coordinator: FirstResponderCoordinator,
) -> FastAPI:
    app = FastAPI(title="reArtex 飞书桥（内部广播端点）")

    @app.post("/internal/approval")
    def internal_approval(payload: dict, x_bridge_secret: str = Header(default="")) -> dict:
        if cfg.bridge_shared_secret and x_bridge_secret != cfg.bridge_shared_secret:
            raise HTTPException(status_code=403, detail="共享口令不符")
        pending_id = payload.get("pending_id")
        if not isinstance(pending_id, int) or pending_id <= 0:
            raise HTTPException(status_code=400, detail="pending_id 必须是正整数")
        # 首响锁登记：卡片发出前先占位，防同批广播重复渲染。
        coordinator.register(pending_id, payload)
        adapter.send_approval_card(payload)
        return {"ok": True, "delivered_to": len(cfg.feishu_allowed_open_ids)}

    return app

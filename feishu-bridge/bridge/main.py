"""飞书桥入口：WS 长连接收指令（后台线程）+ 广播接收端（前台 uvicorn）。

启动：
  python3 -m bridge.main        # 或 uvicorn bridge.main:app
凭据全部来自环境变量（见 config.py / docs/飞书通道.md）。
"""

from __future__ import annotations

import logging
import threading

import uvicorn

from .approval_coordinator import FirstResponderCoordinator
from .broadcast import create_app
from .config import BridgeConfig
from .feishu_ws import FeishuWebSocketAdapter
from .reartex_client import ReartexClient

logging.basicConfig(level=logging.INFO, format="%(asctime)s [%(levelname)s] %(name)s: %(message)s")
logger = logging.getLogger("feishu-bridge")

cfg = BridgeConfig.from_env()
coordinator = FirstResponderCoordinator()
reartex = ReartexClient(cfg.reartex_base_url, cfg.reartex_api_token)
adapter = FeishuWebSocketAdapter(cfg, reartex, coordinator)
app = create_app(cfg, adapter, coordinator)


def _run_ws() -> None:
    while True:
        try:
            logger.info("飞书 WebSocket 长连接启动（app_id=%s…）", cfg.feishu_app_id[:6])
            adapter.run_forever()  # SDK 内部自动重连；异常退出则重建
        except Exception:  # noqa: BLE001
            logger.exception("飞书 WS 断开，10s 后重建")


if __name__ == "__main__":
    threading.Thread(target=_run_ws, name="feishu-ws", daemon=True).start()
    uvicorn.run(app, host=cfg.bridge_host, port=cfg.bridge_port, log_level="info")

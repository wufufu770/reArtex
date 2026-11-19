"""飞书 WebSocket 长连接适配器（零监听端口；事件经官方 SDK 长连接推送）。"""

from __future__ import annotations

import json
import logging
from typing import Any

import lark_oapi as lark
from lark_oapi.api.im.v1 import CreateMessageRequest, CreateMessageRequestBody

from .approval_coordinator import FirstResponderCoordinator
from .command_router import handle_inbound_command
from .config import BridgeConfig
from .reartex_client import ReartexClient

logger = logging.getLogger(__name__)


class FeishuWebSocketAdapter:
    def __init__(
        self,
        cfg: BridgeConfig,
        client: ReartexClient,
        coordinator: FirstResponderCoordinator,
    ) -> None:
        self.cfg = cfg
        self.reartex = client
        self.coordinator = coordinator
        self.api_client = (
            lark.Client.builder().app_id(cfg.feishu_app_id).app_secret(cfg.feishu_app_secret).build()
        )

    def send_text(self, open_id: str, text: str) -> None:
        req = (
            CreateMessageRequest.builder()
            .receive_id_type("open_id")
            .request_body(
                CreateMessageRequestBody.builder()
                .receive_id(open_id)
                .msg_type("text")
                .content(json.dumps({"text": text}))
                .build()
            )
            .build()
        )
        resp = self.api_client.im.v1.message.create(req)
        if not resp.success():
            logger.warning("飞书发文本失败 %s: %s", open_id, resp.msg)

    def send_approval_card(self, payload: dict[str, Any]) -> None:
        """审批广播卡（只展示要素；裁决走 /approve /deny 文本命令——卡片按钮回调
        需要公网回调面，v1 不开）。"""
        preview = str(payload.get("input", ""))[:600]
        card = {
            "config": {"wide_screen_mode": True},
            "header": {"title": {"tag": "plain_text", "content": f"reArtex 审批待决 #{payload.get('pending_id')}"}},
            "elements": [
                {
                    "tag": "markdown",
                    "content": (
                        f"**审批编号：** #{payload.get('pending_id')}\n"
                        f"**任务：** {payload.get('task_id') or '—'}　**执行体：** {payload.get('agent') or '—'}\n"
                        f"**工具：** `{payload.get('tool')}`\n"
                        f"**原因：** {payload.get('reason') or '—'}\n"
                        f"**入参预览：**\n```{preview}```\n"
                        "私聊回复 `/approve <编号>` 批准，或 `/deny <编号>` 拒绝。"
                    ),
                },
            ],
        }
        for open_id in self.cfg.feishu_allowed_open_ids:
            req = (
                CreateMessageRequest.builder()
                .receive_id_type("open_id")
                .request_body(
                    CreateMessageRequestBody.builder()
                    .receive_id(open_id)
                    .msg_type("interactive")
                    .content(json.dumps(card))
                    .build()
                )
                .build()
            )
            resp = self.api_client.im.v1.message.create(req)
            if not resp.success():
                logger.warning("飞书审批卡投递失败 %s: %s", open_id, resp.msg)

    def _on_message(self, data: Any) -> None:
        try:
            sender_id = data.event.sender.sender_id.open_id
            if self.cfg.feishu_allowed_open_ids and sender_id not in self.cfg.feishu_allowed_open_ids:
                logger.warning("未授权飞书发件人已拦（宁漏勿误）： %s", sender_id)
                return
            content = json.loads(data.event.message.content or "{}")
            text = content.get("text", "")
            reply = handle_inbound_command("feishu", sender_id, text, self.reartex, self.coordinator)
        except Exception:  # noqa: BLE001
            logger.exception("飞书消息处理失败")
            reply = "消息处理出错，请稍后重试（详见桥日志）。"
        self.send_text(sender_id, reply)

    def run_forever(self) -> None:
        handler = (
            lark.EventDispatcherHandler.builder("", "").register_p2_im_message_receive_v1(self._on_message).build()
        )
        ws_client = lark.ws.Client(
            self.cfg.feishu_app_id,
            self.cfg.feishu_app_secret,
            event_handler=handler,
            log_level=lark.LogLevel.INFO,
        )
        ws_client.start()

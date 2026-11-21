"""微信 iLink 任务通道（0.15.28）：扫码登录 → 长轮询收消息 → allowlist → 命令面。

协议基于 2026-03 社区逆向文档（@tencent-weixin/openclaw-weixin 1.0.2）：
HTTPS + JSON 请求-响应，无公网回调（getupdates 长轮询 35s hold），个人微信号
扫码即用。字段名可能随版本漂移——全部集中在 FIELD 映射区，实测有出入时只改
那一处。信任语义与飞书 /start、邮箱通道同层：allowlist 内 user_id = 可信操作者；
allowlist 外拒收并告警（宁漏勿误：WECHAT_ALLOWED_USERS 空集拒绝启动）。

命令面（最小）：
  /tasks            列最近任务
  /create 描述 | 目标   直建任务
  /help             命令说明
  其它裸文本         直建任务（正文要点 ≤300 字符，与邮箱通道同口径）
"""

from __future__ import annotations

import base64
import json
import logging
import random
import time
from pathlib import Path

import httpx

logger = logging.getLogger("wechat-ilink")

BASE_URL = "https://ilinkai.weixin.qq.com"
QR_ENDPOINT = "/ilink/bot/get_bot_qrcode?bot_type=3"
QR_STATUS_ENDPOINT = "/ilink/bot/get_qrcode_status"
UPDATES_ENDPOINT = "/ilink/bot/getupdates"
SEND_ENDPOINT = "/ilink/bot/sendmessage"

# 协议字段映射区（逆向文档字段名；实测漂移时只改这里）
FIELD = {
    "qr_content": "qrcode_content",     # 二维码内容（get_bot_qrcode 响应）
    "qr_status": "status",              # 扫码状态：waiting/confirmed/…
    "token": "bot_token",               # 登录 token（get_qrcode_status confirmed 时）
    "cursor": "get_updates_buf",        # 长轮询游标（首次空串）
    "updates": "updates",               # 更新数组（getupdates 响应）
    "msg_type": "message_type",         # 1=用户消息 2=Bot 消息
    "from_user": "from_user_id",        # 发送者 user_id（xxx@im.wechat）
    "context_token": "context_token",   # 回复必带（原样回传）
    "text": "text",                     # 文本内容
    "items": "item_list",               # 消息内容列表
    "item_type": "type",                # 1=文本
    "text_item": "text_item",           # 文本项载体
}

GOAL_MAX_CHARS = 300


class ILinkError(RuntimeError):
    """iLink 协议/登录态异常。token 失效时调用方应走重新扫码。"""


def _uin_header() -> str:
    """X-WECHAT-UIN：随机 uint32 → 十进制字符串 → base64（协议防重放设计）。"""
    return base64.b64encode(str(random.getrandbits(32)).encode()).decode()


class ILinkClient:
    """iLink 协议客户端：登录态持久化 + 收发消息。"""

    def __init__(self, token_file: str) -> None:
        self.token_file = Path(token_file)
        self.token: str = ""
        self.cursor: str = ""
        self.http = httpx.Client(timeout=45.0)  # 45s：getupdates hold 至 35s
        self._load_state()

    # ---- 登录态 ----

    def _load_state(self) -> None:
        try:
            state = json.loads(self.token_file.read_text())
            self.token = state.get("bot_token", "")
            self.cursor = state.get("cursor", "")
        except Exception:  # noqa: BLE001
            logger.info("登录态文件不存在或损坏，走扫码登录")

    def _save_state(self) -> None:
        self.token_file.write_text(json.dumps({"bot_token": self.token, "cursor": self.cursor}))

    def _headers(self) -> dict:
        return {
            "Authorization": f"Bearer {self.token}",
            "AuthorizationType": "ilink_bot_token",
            "X-WECHAT-UIN": _uin_header(),
        }

    def _post(self, path: str, body: dict) -> dict:
        resp = self.http.post(BASE_URL + path, json=body, headers=self._headers())
        resp.raise_for_status()
        return resp.json()

    # ---- 扫码登录 ----

    def login_if_needed(self) -> None:
        """token 缺失时走扫码登录（阻塞到扫码完成）。"""
        if self.token:
            return
        resp = self.http.get(BASE_URL + QR_ENDPOINT, headers=self._headers())
        resp.raise_for_status()
        data = resp.json()
        qr = data.get(FIELD["qr_content"], "")
        if not qr:
            raise ILinkError("未拿到二维码内容，协议字段可能已漂移（检查 FIELD 映射）")
        print("\n请用目标微信号扫码（或把下面内容转成二维码）：")
        print(qr)
        print()
        for _ in range(60):  # 最多等 5 分钟（60 × 5s 轮询）
            time.sleep(5)
            status_resp = self.http.get(BASE_URL + QR_STATUS_ENDPOINT, headers=self._headers())
            status_resp.raise_for_status()
            sdata = status_resp.json()
            status = sdata.get(FIELD["qr_status"], "")
            if status == "confirmed":
                self.token = sdata.get(FIELD["token"], "")
                if not self.token:
                    raise ILinkError("扫码确认但未返回 token（检查 FIELD[token] 映射）")
                self._save_state()
                logger.info("扫码登录成功，token 已持久化")
                return
            logger.info("等待扫码…（status=%s）", status or "unknown")
        raise ILinkError("扫码超时")

    # ---- 收发 ----

    def poll_updates(self) -> list[dict]:
        """长轮询一轮（hold 至 35s）；返回用户文本消息列表 [{from, text, context_token}]。"""
        body = {FIELD["cursor"]: self.cursor}
        data = self._post(UPDATES_ENDPOINT, body)
        self.cursor = data.get(FIELD["cursor"], self.cursor)
        self._save_state()
        out: list[dict] = []
        for upd in data.get(FIELD["updates"], []) or []:
            if upd.get(FIELD["msg_type"]) != 1:
                continue
            text = ""
            for item in upd.get(FIELD["items"], []) or []:
                if item.get(FIELD["item_type"]) == 1:
                    text += (item.get(FIELD["text_item"], {}) or {}).get(FIELD["text"], "")
            if not text.strip():
                continue
            out.append({
                "from": upd.get(FIELD["from_user"], ""),
                "text": text.strip(),
                "context_token": upd.get(FIELD["context_token"], ""),
            })
        return out

    def send_text(self, to_user: str, text: str, context_token: str = "") -> None:
        """发文本消息；context_token 必须原样带回收到的值（协议关联对话）。"""
        body = {
            "to_user_id": to_user,
            "message_type": 2,
            "message_state": 2,
            "context_token": context_token,
            "item_list": [{"type": 1, "text_item": {"text": text[:4000]}}],
        }
        self._post(SEND_ENDPOINT, body)


class WeChatChannel:
    """微信任务通道侧车：allowlist → 命令面 → reArtex API。"""

    def __init__(self, cfg: dict) -> None:
        self.cfg = cfg
        self.users = {u.strip() for u in cfg.get("allowed_users", []) if u.strip()}
        if not self.users:
            raise SystemExit("allowlist 为空：请在 WECHAT_ALLOWED_USERS 至少配一个 user_id（宁漏勿误）")
        self.client = ILinkClient(cfg.get("token_file", "./ilink_state.json"))
        token = cfg.get("reartex_api_token", "")
        headers = {"Content-Type": "application/json"}
        if token:
            headers["Authorization"] = f"Bearer {token}"
        self.http = httpx.Client(
            base_url=cfg.get("reartex_base_url", "http://127.0.0.1:8787").rstrip("/"),
            headers=headers,
            timeout=15.0,
        )
        self.notice_url = cfg.get("bridge_notice_url", "")
        self.notice_secret = cfg.get("bridge_shared_secret", "")

    # ---- reArtex API ----

    def _tasks(self) -> str:
        try:
            resp = self.http.get("/api/tasks", params={"limit": 5})
            resp.raise_for_status()
            rows = resp.json()
            items = rows if isinstance(rows, list) else rows.get("tasks", [])
            if not items:
                return "暂无任务。"
            lines = [f"#{t.get('id')} [{t.get('status', '?')}] {(t.get('description') or '')[:60]}" for t in items[:5]]
            return "最近任务：\n" + "\n".join(lines)
        except Exception:  # noqa: BLE001
            return "查询任务失败，请稍后再试。"

    def _create(self, text: str, sender: str) -> str:
        goal = " ".join(text.split())[:GOAL_MAX_CHARS] or "（空消息）"
        try:
            resp = self.http.post("/api/tasks", json={
                "description": f"微信任务（来自 iLink 用户 {sender[:40]}）",
                "goal": goal,
            })
            resp.raise_for_status()
            task = resp.json()
        except Exception:  # noqa: BLE001
            logger.exception("任务创建失败")
            return "任务创建失败，请稍后再试。"
        self._notice(f"[微信任务] #{task.get('id')} 已由 {sender[:40]} 经 iLink 创建")
        return f"任务 #{task.get('id')} 已创建：{goal[:80]}"

    def _notice(self, text: str) -> None:
        """任务创建通知经飞书桥（可选；失败只记日志）。"""
        if not self.notice_url:
            return
        try:
            resp = httpx.post(
                self.notice_url,
                json={"text": text},
                headers={"X-Bridge-Secret": self.notice_secret},
                timeout=10.0,
            )
            resp.raise_for_status()
        except Exception:  # noqa: BLE001
            logger.warning("通知推送失败（不影响任务本体）")

    # ---- 命令面 ----

    def handle(self, msg: dict) -> str | None:
        """处理一条白名单内消息；返回回复文本（None=不回复）。"""
        text = msg["text"]
        if text.startswith("/tasks"):
            return self._tasks()
        if text.startswith("/create"):
            body = text[len("/create"):].strip()
            if "|" in body:
                desc, goal = body.split("|", 1)
                return self._create(f"{desc.strip()}。目标：{goal.strip()}", msg["from"])
            return self._create(body, msg["from"])
        if text.startswith("/help"):
            return (
                "可用命令：/tasks 列任务 / | /create 描述|目标 建任务 / | "
                "其它文本直接建任务（≤300 字）。"
            )
        return self._create(text, msg["from"])

    # ---- 主循环 ----

    def run_forever(self) -> None:
        self.client.login_if_needed()
        logger.info("微信 iLink 通道启动（allowlist %d 个 user_id）", len(self.users))
        while True:
            try:
                for msg in self.client.poll_updates():
                    sender = msg["from"]
                    if sender not in self.users:
                        logger.warning("白名单外用户已拒收（宁漏勿误）: %s", sender)
                        continue
                    reply = self.handle(msg)
                    if reply:
                        self.client.send_text(sender, reply, msg["context_token"])
            except ILinkError:
                logger.exception("登录态失效，走重新扫码")
                self.client.token = ""
                self.client.login_if_needed()
            except Exception:  # noqa: BLE001
                logger.exception("轮询异常（下个周期重试）")
                time.sleep(5)

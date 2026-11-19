"""邮箱任务通道（0.15.20）：IMAP 轮询 → allowlist → 直建任务 → 通知。

信任语义与飞书 /start 相同：allowlist 内的发件人 = 可信操作者（直建任务并在
描述里标注来源）；allowlist 外的发件人拒收并告警。已知邮件 UID 落本地状态文件
防重复处理；轮询异常不退出（下个周期重试）。
"""

from __future__ import annotations

import email
import imaplib
import json
import logging
import re
import time
from email.header import decode_header
from email.utils import parseaddr
from pathlib import Path

import httpx

logger = logging.getLogger("email-intake")

DOMAIN_RE = re.compile(r"\b(?:[a-zA-Z0-9-]+\.)+[a-zA-Z]{2,}\b")
GOAL_MAX_CHARS = 300


def decode_mime_str(raw: str | None) -> str:
    if not raw:
        return ""
    out: list[str] = []
    for chunk, enc in decode_header(raw):
        if isinstance(chunk, bytes):
            out.append(chunk.decode(enc or "utf-8", errors="replace"))
        else:
            out.append(chunk)
    return "".join(out)


def extract_body(msg: email.message.Message) -> str:
    if msg.is_multipart():
        for part in msg.walk():
            if part.get_content_type() == "text/plain":
                payload = part.get_payload(decode=True)
                if payload:
                    return payload.decode(part.get_content_charset() or "utf-8", errors="replace")
    else:
        payload = msg.get_payload(decode=True)
        if payload:
            return payload.decode(msg.get_content_charset() or "utf-8", errors="replace")
    return ""


def is_sender_allowed(sender_addr: str, senders: set[str], domains: set[str]) -> bool:
    addr = sender_addr.strip().lower()
    if addr in senders:
        return True
    if "@" in addr:
        domain = addr.split("@", 1)[1]
        if domain in domains:
            return True
    return False


def build_goal(subject: str, body: str) -> str:
    """主题为干、正文摘录为辅（≤300 字符）——任务书口径与 /start 一致。"""
    body_clean = " ".join(body.split())
    goal = subject.strip()
    if body_clean:
        goal = (goal + "。正文要点：" + body_clean[:GOAL_MAX_CHARS]).strip()
    return goal[:GOAL_MAX_CHARS] or "（空邮件）"


def notify_channels(notice_url: str, secret: str, text: str) -> None:
    """把任务创建通知推给桥的 /internal/notice（URL 空=跳过；失败只记日志）。"""
    if not notice_url:
        return
    try:
        resp = httpx.post(
            notice_url,
            json={"text": text},
            headers={"X-Bridge-Secret": secret},
            timeout=10.0,
        )
        resp.raise_for_status()
    except Exception:  # noqa: BLE001
        logger.warning("任务创建通知推送失败（不影响任务本体）: %s", notice_url)


class EmailIntake:
    def __init__(self, cfg: dict) -> None:
        self.cfg = cfg
        self.state_path = Path(cfg.get("state_file", "./email_state.json"))
        self.seen: set[str] = set()
        try:
            self.seen = set(json.loads(self.state_path.read_text()) if self.state_path.exists() else [])
        except Exception:  # noqa: BLE001
            logger.warning("状态文件损坏，按空集处理: %s", self.state_path)
        token = cfg.get("reartex_api_token", "")
        headers = {"Content-Type": "application/json"}
        if token:
            headers["Authorization"] = f"Bearer {token}"
        self.http = httpx.Client(base_url=cfg["reartex_base_url"].rstrip("/"), headers=headers, timeout=15.0)
        self.senders = {s.strip().lower() for s in cfg.get("allowed_senders", []) if s.strip()}
        self.domains = {d.strip().lower() for d in cfg.get("allowed_domains", []) if d.strip()}

    def _persist_seen(self) -> None:
        self.state_path.write_text(json.dumps(sorted(self.seen)))

    def poll_once(self) -> int:
        """跑一轮轮询；返回本轮创建的任务数（连接失败抛给调用方重试）。"""
        created = 0
        host = self.cfg["imap_host"]
        port = int(self.cfg.get("imap_port", 993))
        with imaplib.IMAP4_SSL(host, port) as imap:
            imap.login(self.cfg["imap_user"], self.cfg["imap_password"])
            imap.select("INBOX")
            _, data = imap.search(None, "UNSEEN")
            for num in data[0].split():
                _, msg_data = imap.fetch(num, "(RFC822)")
                msg = email.message_from_bytes(msg_data[0][1])
                uid = f"{host}:{num.decode()}"  # 简易去重键：host+序号（UNSEEN 语义下足够）
                sender_addr = parseaddr(msg.get("From", ""))[1]
                if not is_sender_allowed(sender_addr, self.senders, self.domains):
                    logger.warning("白名单外发件人已拒收（宁漏勿误）: %s", sender_addr)
                    self.seen.add(uid)
                    continue
                subject = decode_mime_str(msg.get("Subject")) or "（无主题）"
                body = extract_body(msg)
                goal = build_goal(subject, body)
                resp = self.http.post(
                    "/api/tasks",
                    json={
                        "description": f"邮件任务（来自 {sender_addr}，主题：{subject[:80]}）",
                        "goal": goal,
                    },
                )
                self.seen.add(uid)
                if resp.status_code >= 300:
                    logger.warning("任务创建被拒（%s）: %s", resp.status_code, resp.text[:200])
                    continue
                task = resp.json()
                created += 1
                logger.info("邮件任务已创建 #%s（来自 %s）", task.get("id"), sender_addr)
                notify_channels(
                    self.cfg.get("bridge_notice_url", ""),
                    self.cfg.get("bridge_shared_secret", ""),
                    f"[邮件任务] #{task.get('id')} 已由 {sender_addr} 的邮件创建：{subject[:80]}",
                )
        self._persist_seen()
        return created

    def run_forever(self) -> None:
        interval = int(self.cfg.get("poll_interval_sec", 120))
        logger.info("邮箱轮询启动（%s@%s，间隔 %ds，allowlist 发件 %d/域 %d）",
                    self.cfg["imap_user"], self.cfg["imap_host"], interval,
                    len(self.senders), len(self.domains))
        while True:
            try:
                self.poll_once()
            except Exception:  # noqa: BLE001
                logger.exception("轮询异常（下个周期重试）")
            time.sleep(interval)

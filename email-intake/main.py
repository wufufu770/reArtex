"""邮箱任务通道入口：IMAP 轮询侧车（环境变量见 README）。

信任语义：allowlist 内发件人 = 可信操作者，邮件到达即建任务（来源标注在任务
描述里）；allowlist 外拒收并告警。通知经飞书桥 /internal/notice（可选）。
"""

from __future__ import annotations

import logging
import os
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).parent))

from intake import EmailIntake  # noqa: E402

logging.basicConfig(level=logging.INFO, format="%(asctime)s [%(levelname)s] %(name)s: %(message)s")
logger = logging.getLogger("email-intake")


def load_cfg() -> dict:
    required = ["IMAP_HOST", "IMAP_USER", "IMAP_PASSWORD"]
    missing = [k for k in required if not os.environ.get(k)]
    if missing:
        raise SystemExit(f"缺少必需环境变量: {', '.join(missing)}")
    cfg = {
        "imap_host": os.environ["IMAP_HOST"],
        "imap_port": int(os.environ.get("IMAP_PORT", "993")),
        "imap_user": os.environ["IMAP_USER"],
        "imap_password": os.environ["IMAP_PASSWORD"],
        "allowed_senders": os.environ.get("EMAIL_ALLOWED_SENDERS", "").split(","),
        "allowed_domains": os.environ.get("EMAIL_ALLOWED_DOMAINS", "").split(","),
        "poll_interval_sec": int(os.environ.get("POLL_INTERVAL_SEC", "120")),
        "state_file": os.environ.get("STATE_FILE", "./email_state.json"),
        "reartex_base_url": os.environ.get("REARTEX_BASE_URL", "http://127.0.0.1:8787"),
        "reartex_api_token": os.environ.get("REARTEX_API_TOKEN", ""),
        "bridge_notice_url": os.environ.get("BRIDGE_NOTICE_URL", ""),
        "bridge_shared_secret": os.environ.get("BRIDGE_SHARED_SECRET", ""),
    }
    if not cfg["allowed_senders"] and not cfg["allowed_domains"]:
        raise SystemExit("allowlist 为空：请在 EMAIL_ALLOWED_SENDERS / EMAIL_ALLOWED_DOMAINS 至少配一项（宁漏勿误）")
    return cfg


if __name__ == "__main__":
    intake = EmailIntake(load_cfg())
    intake.run_forever()

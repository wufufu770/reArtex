"""微信 iLink 任务通道入口（环境变量见 README）。

信任语义：allowlist 内 user_id = 可信操作者；allowlist 外拒收并告警
（宁漏勿误：空集拒绝启动）。协议字段基于 2026-03 社区逆向文档，实测漂移时
只改 ilink.py 的 FIELD 映射区。
"""

from __future__ import annotations

import logging
import os
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).parent))

from ilink import WeChatChannel  # noqa: E402

logging.basicConfig(level=logging.INFO, format="%(asctime)s [%(levelname)s] %(name)s: %(message)s")
logger = logging.getLogger("wechat-ilink")


def load_cfg() -> dict:
    required = ["WECHAT_ALLOWED_USERS"]
    missing = [k for k in required if not os.environ.get(k)]
    if missing:
        raise SystemExit(f"缺少必需环境变量: {', '.join(missing)}")
    return {
        "allowed_users": os.environ["WECHAT_ALLOWED_USERS"].split(","),
        "token_file": os.environ.get("ILINK_TOKEN_FILE", "./ilink_state.json"),
        "reartex_base_url": os.environ.get("REARTEX_BASE_URL", "http://127.0.0.1:8787"),
        "reartex_api_token": os.environ.get("REARTEX_API_TOKEN", ""),
        "bridge_notice_url": os.environ.get("BRIDGE_NOTICE_URL", ""),
        "bridge_shared_secret": os.environ.get("BRIDGE_SHARED_SECRET", ""),
    }


if __name__ == "__main__":
    channel = WeChatChannel(load_cfg())
    channel.run_forever()

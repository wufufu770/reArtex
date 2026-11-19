"""飞书桥配置：全部来自环境变量（凭据永不进 reArtex 进程/库/日志）。"""

from __future__ import annotations

from dataclasses import dataclass
import os


def _split_csv(val: str) -> set[str]:
    return {item.strip() for item in val.split(",") if item.strip()}


@dataclass(frozen=True)
class BridgeConfig:
    reartex_base_url: str
    reartex_api_token: str  # 平台 JWT（requireAuth 面）；桥以运维身份运行
    feishu_app_id: str
    feishu_app_secret: str
    # 发件 allowlist：空集=拒绝所有私聊指令（宁漏勿误）；审批广播不受此限（只发收）。
    feishu_allowed_open_ids: set[str]
    # reArtex → 桥的广播鉴权（服务端 settings 键 feishu_bridge_secret 同值）。
    bridge_shared_secret: str
    # 广播监听：桥是纯 WS 客户端，唯一需要监听的就是 reArtex 推审批的端点。
    bridge_host: str
    bridge_port: int

    @classmethod
    def from_env(cls) -> "BridgeConfig":
        return cls(
            reartex_base_url=os.environ.get("REARTEX_BASE_URL", "http://127.0.0.1:8787").rstrip("/"),
            reartex_api_token=os.environ.get("REARTEX_API_TOKEN", ""),
            feishu_app_id=os.environ.get("FEISHU_APP_ID", ""),
            feishu_app_secret=os.environ.get("FEISHU_APP_SECRET", ""),
            feishu_allowed_open_ids=_split_csv(os.environ.get("FEISHU_ALLOWED_OPEN_IDS", "")),
            bridge_shared_secret=os.environ.get("BRIDGE_SHARED_SECRET", ""),
            bridge_host=os.environ.get("BRIDGE_HOST", "127.0.0.1"),
            bridge_port=int(os.environ.get("BRIDGE_PORT", "8791")),
        )

"""审计明细与日志的凭据脱敏。

借鉴 billion-context 的六类凭据形态，并加固 URL 主机/用户信息/查询串遮蔽、
Anthropic/GitHub/AWS 密钥形态与非敏感字段防误伤。
"""
from __future__ import annotations

import hashlib
import re

__all__ = ['redact', 'redact_detail', 'mask_host']

_SECRET_KEY_NAMES = (
    r'api[_-]?key|access[_-]?key|secret[_-]?key|private[_-]?key|client[_-]?secret|'
    r'token|secret|password|passwd|pwd|auth(?:orization)?|credential|bearer|cookie|session[_-]?id'
)

_CREDENTIAL_PATTERNS = [
    # bearer / basic 认证头
    (re.compile(r'\b(bearer|basic)\s+([A-Za-z0-9._~+/=-]{8,})', re.I),
     lambda m: f'{m.group(1)} <masked {len(m.group(2))} chars>'),
    # Anthropic sk-ant- (must precede generic sk-)
    (re.compile(r'\bsk-ant-[A-Za-z0-9_-]{16,}'), lambda m: '<masked key>'),
    # OpenAI 风格 sk- / xai-
    (re.compile(r'\b(sk|xai)-[A-Za-z0-9_-]{16,}'),
     lambda m: f'{m.group(1)}-<masked>'),
    # GitHub PAT / OAuth / App tokens
    (re.compile(r'\b(?:ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9]{20,}'), lambda m: '<masked key>'),
    # AWS Access Key ID
    (re.compile(r'\bAKIA[0-9A-Z]{16}\b'), lambda m: '<masked key>'),
    # Groq gsk_ / HuggingFace hf_
    (re.compile(r'\b(gsk|hf)_[A-Za-z0-9]{16,}'),
     lambda m: f'{m.group(1)}_<masked>'),
    # Google AIza
    (re.compile(r'\bAIza[A-Za-z0-9_-]{20,}'), lambda m: '<masked key>'),
    # JWT（三段 base64url）
    (re.compile(r'\beyJ[A-Za-z0-9_-]{6,}\.[A-Za-z0-9_-]{6,}\.[A-Za-z0-9_-]{6,}'),
     lambda m: '<masked jwt>'),
    # 仅对敏感键名的 key: value / key=value 脱敏，避免误伤 suite_id / protocol 等元数据
    (re.compile(rf'\b({_SECRET_KEY_NAMES})\s*[:=]\s*([A-Za-z0-9._~+/=-]{{8,}})', re.I),
     lambda m: f'{m.group(1)}=<masked>'),
]

# 单次匹配任意带 scheme 的 URL：剥离 userinfo、遮蔽 host、丢弃 ?query 与 #fragment，仅保留路径
_ANY_URL_RE = re.compile(r'\b([a-z][a-z0-9+.\-]*)://(?:([^\s/@]+)@)?([^\s/?#]+)([^\s]*)', re.I)


def _drop_query(tail: str) -> str:
    """丢掉 ?query 与 #fragment，只保留路径。"""
    for sep in ('?', '#'):
        idx = tail.find(sep)
        if idx != -1:
            tail = tail[:idx]
    return tail


def _mask_any_url(match: re.Match) -> str:
    scheme, _userinfo, _host, tail = match.groups()
    return f'{scheme}://<masked-host>{_drop_query(tail or "")}'


def mask_host(host: str) -> str:
    """主机名 → 端点指纹。可分辨是否同一主机，但不泄漏域名。"""
    if not host:
        return '<private-host>'
    digest = hashlib.sha256(host.encode('utf-8')).hexdigest()[:8]
    return f'<host:{digest}>'


def redact(text: str) -> str:
    """把文本里的凭据形态与 URL 敏感部分遮蔽掉。非字符串输入原样返回。"""
    if not isinstance(text, str) or not text:
        return text
    out = _ANY_URL_RE.sub(_mask_any_url, text)
    for pattern, repl in _CREDENTIAL_PATTERNS:
        out = pattern.sub(repl, out)
    return out


def redact_detail(detail):
    """递归遮蔽一个 JSON 兼容结构里的所有字符串。用于写入审计明细前。"""
    if isinstance(detail, str):
        return redact(detail)
    if isinstance(detail, dict):
        return {k: redact_detail(v) for k, v in detail.items()}
    if isinstance(detail, (list, tuple)):
        return [redact_detail(v) for v in detail]
    return detail

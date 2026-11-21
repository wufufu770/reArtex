#!/usr/bin/env python3
"""Amber — Standalone FastAPI Server + Role-Scoped HTTP MCP Gateway + Web UI."""
from __future__ import annotations

import hmac
import os
from pathlib import Path
import secrets
import sys
import threading
from typing import Any

from fastapi import Depends, FastAPI, Header, HTTPException, Query, Response
from fastapi.middleware.cors import CORSMiddleware
from fastapi.responses import HTMLResponse
from pydantic import BaseModel

try:
    from . import __version__ as VERSION, mcp_server, redaction
    from .memory_runtime import AuthorizationError, Runtime
except ImportError:
    import mcp_server
    import redaction
    from memory_runtime import AuthorizationError, Runtime
    VERSION = "0.5.0"

PKG_DIR = Path(__file__).resolve().parent
DEFAULT_BOOTSTRAP_SPEC = (
    "human:owner:read,write,review,repair,export,audit;"
    "agent:worker:read,write;"
    "agent:curator:read,write,review,repair,audit"
)


def parse_encryption_key(raw: str | None = None) -> bytes | None:
    val = os.environ.get("AMBER_ENCRYPTION_KEY", "") if raw is None else raw
    if not val:
        return None
    b = val.encode("utf-8")
    if len(b) != 32:
        raise RuntimeError(
            "AMBER_ENCRYPTION_KEY must be exactly 32 bytes (UTF-8); unset it explicitly to run in plaintext mode"
        )
    return b


def bootstrap_permissions(rt: Runtime, scope: str, spec: str = DEFAULT_BOOTSTRAP_SPEC) -> None:
    for part in spec.split(";"):
        part = part.strip()
        if not part:
            continue
        principal, actions_str = part.rsplit(":", 1)
        actions = [a.strip() for a in actions_str.split(",") if a.strip()]
        rt.grant(principal.strip(), scope, actions)


def _raise_http_for_runtime_error(exc: Exception) -> None:
    if isinstance(exc, AuthorizationError):
        raise HTTPException(status_code=403, detail=str(exc)) from exc
    if isinstance(exc, KeyError):
        raise HTTPException(status_code=404, detail=f"item not found: {exc}") from exc
    if isinstance(exc, ValueError):
        raise HTTPException(status_code=400, detail=str(exc)) from exc
    raise HTTPException(status_code=500, detail=str(exc)) from exc


class WriteReq(BaseModel):
    scope: str = "default"
    body: str
    session: str = "web-ui"


class ReviewReq(BaseModel):
    item_id: str
    decision: str
    note: str = ""


class RepairReq(BaseModel):
    item_id: str


class SupersedeReq(BaseModel):
    old_item_id: str
    scope: str = "default"
    new_body: str


class RedactPreviewReq(BaseModel):
    text: str


def create_app(
    db_path: str | Path | None = None,
    *,
    encryption_key: bytes | None = None,
    require_auth: bool | None = None,
    tokens: dict[str, str] | None = None,
    bootstrap: bool = False,
) -> FastAPI:
    owner_principal = os.environ.get("AMBER_OWNER_PRINCIPAL", "human:owner")
    default_scope = os.environ.get("AMBER_DEFAULT_SCOPE", "default")
    if db_path is None:
        data_dir = Path(os.environ.get("AMBER_DATA_DIR", str(PKG_DIR.parent / "data")))
        data_dir.mkdir(parents=True, exist_ok=True)
        db_path = data_dir / "amber.db"
    key = encryption_key if encryption_key is not None else parse_encryption_key()
    rt = Runtime(str(db_path), trust_key=key)
    if bootstrap or os.environ.get("AMBER_BOOTSTRAP") == "1":
        bootstrap_permissions(rt, default_scope, os.environ.get("AMBER_BOOTSTRAP_SPEC", DEFAULT_BOOTSTRAP_SPEC))

    auth_on = (os.environ.get("AMBER_REQUIRE_API_AUTH", "0") == "1") if require_auth is None else require_auth
    if tokens is not None:
        role_tokens = dict(tokens)
    else:
        role_tokens = {
            "owner": os.environ.get("AMBER_OWNER_TOKEN") or (secrets.token_urlsafe(32) if auth_on else ""),
            "curator": os.environ.get("AMBER_CURATOR_TOKEN") or (secrets.token_urlsafe(32) if auth_on else ""),
            "worker": os.environ.get("AMBER_WORKER_TOKEN") or (secrets.token_urlsafe(32) if auth_on else ""),
        }
        if auth_on and not os.environ.get("AMBER_OWNER_TOKEN"):
            print(f"[amber] generated ephemeral owner token: {role_tokens['owner']}", file=sys.stderr)

    lock = threading.Lock()
    html_cache = (PKG_DIR / "web" / "index.html").read_text(encoding="utf-8")

    application = FastAPI(title="Amber", version=VERSION)
    application.state.runtime = rt
    application.state.tokens = role_tokens

    _cors = [
        o.strip()
        for o in os.environ.get(
            "AMBER_CORS_ORIGINS",
            "http://localhost:3000,http://127.0.0.1:3000,http://localhost:8002,http://127.0.0.1:8002",
        ).split(",")
        if o.strip()
    ]
    application.add_middleware(
        CORSMiddleware, allow_origins=_cors, allow_credentials=True, allow_methods=["*"], allow_headers=["*"]
    )

    def _resolve_role(
        authorization: str | None,
        *,
        allow_worker: bool,
        default_unauthenticated: tuple[str, str],
    ) -> tuple[str, str]:
        if not auth_on and not authorization:
            return default_unauthenticated
        if not authorization or not authorization.startswith("Bearer "):
            raise HTTPException(status_code=401, detail="Missing Bearer token")
        tok = authorization.removeprefix("Bearer ").strip()
        if role_tokens.get("owner") and hmac.compare_digest(tok, role_tokens["owner"]):
            return owner_principal, "human"
        if role_tokens.get("curator") and hmac.compare_digest(tok, role_tokens["curator"]):
            return "agent:curator", "human"
        if role_tokens.get("worker") and hmac.compare_digest(tok, role_tokens["worker"]):
            if not allow_worker:
                raise HTTPException(status_code=403, detail="Worker tokens are restricted to /mcp")
            return "agent:worker", "agent"
        raise HTTPException(status_code=403, detail="Invalid token")

    def _rest_role(authorization: str | None = Header(default=None)) -> tuple[str, str]:
        return _resolve_role(
            authorization,
            allow_worker=False,
            default_unauthenticated=(owner_principal, "human"),
        )

    @application.get("/", response_class=HTMLResponse)
    def index() -> str:
        return html_cache

    @application.post("/mcp")
    def mcp_http(payload: dict[str, Any], authorization: str | None = Header(default=None)):
        # Anonymous MCP callers default to least-privilege agent:worker (never human:owner)
        principal, channel = _resolve_role(
            authorization,
            allow_worker=True,
            default_unauthenticated=("agent:worker", "agent"),
        )
        srv = mcp_server.Server(rt, principal=principal, channel=channel)
        if not isinstance(payload, dict) or payload.get("jsonrpc") != "2.0" or not isinstance(payload.get("method"), str):
            return {"jsonrpc": "2.0", "id": None, "error": {"code": -32600, "message": "invalid JSON-RPC request"}}
        if "id" not in payload:
            return Response(status_code=202)
        msg_id = payload.get("id")
        with lock:
            try:
                res = srv.handle(payload)
            except mcp_server.MethodNotFoundError as exc:
                return {"jsonrpc": "2.0", "id": msg_id, "error": {"code": -32601, "message": str(exc)}}
            except Exception as exc:  # noqa: BLE001
                return {"jsonrpc": "2.0", "id": msg_id, "error": {"code": -32600 if isinstance(exc, ValueError) else -32603, "message": str(exc)}}
        return Response(status_code=202) if res is None else {"jsonrpc": "2.0", "id": msg_id, "result": res}

    @application.get("/api/memory/items")
    def api_list_items(limit: int = Query(50, ge=1, le=200), role: tuple[str, str] = Depends(_rest_role)) -> dict[str, Any]:
        principal, _ = role
        with lock:
            return {"items": rt.list_items(principal, limit=limit)}

    @application.get("/api/memory/quarantine")
    def api_quarantine(scope: str = default_scope, limit: int = Query(100, ge=1, le=200), role: tuple[str, str] = Depends(_rest_role)) -> dict[str, Any]:
        principal, _ = role
        with lock:
            try:
                return {"scope": scope, "items": rt.list_quarantined(principal, scope, limit=limit)}
            except Exception as exc:  # noqa: BLE001
                _raise_http_for_runtime_error(exc)

    @application.post("/api/memory/write")
    def api_write(req: WriteReq, role: tuple[str, str] = Depends(_rest_role)) -> dict[str, Any]:
        principal, channel = role
        with lock:
            try:
                item_id = rt.write(principal, req.scope, req.body, channel=channel, session=req.session)
                return {"item_id": item_id, "scope": req.scope, "channel": channel}
            except Exception as exc:  # noqa: BLE001
                _raise_http_for_runtime_error(exc)

    @application.post("/api/memory/review")
    def api_review(req: ReviewReq, role: tuple[str, str] = Depends(_rest_role)) -> dict[str, Any]:
        if req.decision not in ("approve", "reject"):
            raise HTTPException(status_code=400, detail="decision must be 'approve' or 'reject'")
        principal, _ = role
        with lock:
            try:
                res = rt.review(principal, req.item_id) if req.decision == "approve" else rt.reject(principal, req.item_id, note=req.note)
                return {"ok": True, "result": res}
            except Exception as exc:  # noqa: BLE001
                _raise_http_for_runtime_error(exc)

    @application.get("/api/memory/search")
    def api_search(q: str, k: int = Query(10, ge=1, le=100), role: tuple[str, str] = Depends(_rest_role)) -> dict[str, Any]:
        principal, _ = role
        with lock:
            return {"query": q, "hits": rt.search(principal, q, k=k)}

    @application.get("/api/memory/item/{item_id}")
    def api_item(item_id: str, role: tuple[str, str] = Depends(_rest_role)) -> dict[str, Any]:
        principal, _ = role
        with lock:
            try:
                return rt.recall(principal, item_id)
            except Exception as exc:  # noqa: BLE001
                _raise_http_for_runtime_error(exc)

    @application.post("/api/memory/supersede")
    def api_supersede(req: SupersedeReq, role: tuple[str, str] = Depends(_rest_role)) -> dict[str, Any]:
        principal, channel = role
        with lock:
            try:
                return rt.supersede(principal, req.old_item_id, req.scope, req.new_body, channel=channel)
            except Exception as exc:  # noqa: BLE001
                _raise_http_for_runtime_error(exc)

    @application.post("/api/memory/repair")
    def api_repair(req: RepairReq, role: tuple[str, str] = Depends(_rest_role)) -> dict[str, Any]:
        principal, _ = role
        with lock:
            try:
                return {"ok": True, "receipt": rt.repair(principal, req.item_id)}
            except Exception as exc:  # noqa: BLE001
                _raise_http_for_runtime_error(exc)

    @application.get("/api/memory/audit")
    def api_audit(scope: str = default_scope, role: tuple[str, str] = Depends(_rest_role)) -> dict[str, Any]:
        principal, _ = role
        with lock:
            try:
                return {
                    "scope": scope,
                    "events": rt.audit_query(principal, scope),
                    "pending_outbound": rt.pending_outbound(principal),
                    "chain": rt.audit_chain_verify(principal, scope),
                }
            except Exception as exc:  # noqa: BLE001
                _raise_http_for_runtime_error(exc)

    @application.get("/api/memory/audit/chain")
    def api_audit_chain(scope: str | None = None, role: tuple[str, str] = Depends(_rest_role)) -> dict[str, Any]:
        principal, _ = role
        with lock:
            try:
                return rt.audit_chain_verify(principal, scope)
            except Exception as exc:  # noqa: BLE001
                _raise_http_for_runtime_error(exc)

    @application.post("/api/redact-preview")
    def api_redact_preview(req: RedactPreviewReq) -> dict[str, Any]:
        out = redaction.redact(req.text)
        return {"text": out, "changed": out != req.text}

    @application.get("/api/health")
    def api_health() -> dict[str, Any]:
        with lock:
            jm = rt.db.execute("PRAGMA journal_mode").fetchone()[0]
        return {
            "ok": True,
            "name": "amber",
            "version": VERSION,
            "journal_mode": jm,
            "storage_mode": "encrypted" if key else "plain",
            "auth_required": auth_on,
            "mcp_server_name": mcp_server.SERVER_NAME,
            "mcp_tools": [t["name"] for t in mcp_server.TOOLS],
        }

    return application


_cached_app: FastAPI | None = None


def __getattr__(name: str):
    """Lazy ASGI app accessor so importing amber.server has zero filesystem/DB side effects."""
    global _cached_app
    if name == "app":
        if _cached_app is None:
            _cached_app = create_app()
        return _cached_app
    raise AttributeError(f"module {__name__!r} has no attribute {name!r}")


def main() -> None:
    import uvicorn
    host = os.environ.get("AMBER_HOST", "127.0.0.1")
    if os.environ.get("AMBER_REQUIRE_API_AUTH", "0") != "1" and host not in ("127.0.0.1", "::1", "localhost"):
        raise RuntimeError("Set AMBER_REQUIRE_API_AUTH=1 before binding a non-loopback address")
    uvicorn.run(create_app(), host=host, port=int(os.environ.get("AMBER_PORT", "8002")))

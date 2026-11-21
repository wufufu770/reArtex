#!/usr/bin/env python3
"""Veto — Standalone FastAPI Server + HTTP MCP Gateway + Web UI."""
from __future__ import annotations

import hmac
import os
from pathlib import Path
import uuid
from typing import Any

from fastapi import Depends, FastAPI, Header, HTTPException, Query, Response
from fastapi.middleware.cors import CORSMiddleware
from fastapi.responses import HTMLResponse, JSONResponse
from pydantic import BaseModel

try:
    from . import __version__ as VERSION, eval_core, mcp_server
    from .store import EvalRunStore
except ImportError:
    import eval_core
    import mcp_server
    from store import EvalRunStore
    VERSION = "0.5.0"

PKG_DIR = Path(__file__).resolve().parent
MAX_BODY_BYTES = int(os.environ.get("VETO_MAX_BODY_BYTES", str(5 * 1024 * 1024)))
MIN_SIGNING_KEY_BYTES = 32


class MaxBodySizeMiddleware:
    """Pure ASGI middleware enforcing MAX_BODY_BYTES for both Content-Length and chunked bodies."""

    def __init__(self, app, max_bytes: int = MAX_BODY_BYTES):
        self.app = app
        self.max_bytes = max_bytes

    async def __call__(self, scope, receive, send):
        if scope["type"] != "http":
            await self.app(scope, receive, send)
            return
        for name, val in scope.get("headers", []):
            if name.lower() == b"content-length":
                try:
                    if int(val.decode("ascii")) > self.max_bytes:
                        resp = JSONResponse(status_code=413, content={"detail": "Request body too large"})
                        await resp(scope, receive, send)
                        return
                except ValueError:
                    pass
        received = 0
        exceeded = False

        async def limited_receive():
            nonlocal received, exceeded
            message = await receive()
            if message["type"] == "http.request":
                chunk = message.get("body", b"")
                received += len(chunk)
                if received > self.max_bytes:
                    exceeded = True
                    raise _BodyTooLargeError()
            return message

        try:
            await self.app(scope, limited_receive, send)
        except _BodyTooLargeError:
            resp = JSONResponse(status_code=413, content={"detail": "Request body too large"})
            await resp(scope, receive, send)


class _BodyTooLargeError(Exception):
    pass


def _require_auth(authorization: str | None = Header(default=None)) -> None:
    if os.environ.get("VETO_REQUIRE_AUTH", "0") != "1":
        return
    token = os.environ.get("VETO_API_TOKEN")
    if not token:
        raise HTTPException(status_code=503, detail="Server misconfigured: VETO_API_TOKEN required when auth is enabled")
    if not authorization or not authorization.startswith("Bearer "):
        raise HTTPException(status_code=401, detail="Missing Bearer token")
    if not hmac.compare_digest(authorization.removeprefix("Bearer ").strip(), token):
        raise HTTPException(status_code=403, detail="Invalid token")


class ScoreReq(BaseModel):
    suite: dict[str, Any]
    responses: list[dict[str, Any]]
    identity: dict[str, Any]


class CompareReq(BaseModel):
    baseline: dict[str, Any]
    candidate: dict[str, Any]
    margin: float = 0.05
    alpha: float = 0.05
    assume_independent: bool = True
    suite: dict[str, Any] | None = None
    baseline_responses: list[dict[str, Any]] | None = None
    candidate_responses: list[dict[str, Any]] | None = None
    require_recompute: bool = True
    allow_stack_mismatch: bool = False
    stat_method: str = "paired_exact"


class PassKReq(BaseModel):
    records: list[dict[str, Any]]


def create_app(db_path: str | Path | None = None, *, max_body_bytes: int | None = None) -> FastAPI:
    raw_key = os.environ.get(eval_core.TRUST_KEY_ENV, "")
    if raw_key and len(raw_key.encode("utf-8")) < MIN_SIGNING_KEY_BYTES:
        raise RuntimeError(f"{eval_core.TRUST_KEY_ENV} must be at least {MIN_SIGNING_KEY_BYTES} bytes (UTF-8)")

    if db_path is None:
        data_dir = Path(os.environ.get("VETO_DATA_DIR", str(PKG_DIR.parent / "data")))
        data_dir.mkdir(parents=True, exist_ok=True)
        db_path = data_dir / "veto_runs.db"
    store = EvalRunStore(db_path)
    mcp_srv = mcp_server.Server(store)
    html_cache = (PKG_DIR / "web" / "index.html").read_text(encoding="utf-8")

    application = FastAPI(title="Veto", version=VERSION)
    application.state.store = store
    _cors = [
        o.strip()
        for o in os.environ.get(
            "VETO_CORS_ORIGINS",
            "http://localhost:3000,http://127.0.0.1:3000,http://localhost:8001,http://127.0.0.1:8001",
        ).split(",")
        if o.strip()
    ]
    application.add_middleware(
        CORSMiddleware, allow_origins=_cors, allow_credentials=True, allow_methods=["*"], allow_headers=["*"]
    )
    application.add_middleware(
        MaxBodySizeMiddleware,
        max_bytes=MAX_BODY_BYTES if max_body_bytes is None else max_body_bytes,
    )

    @application.get("/", response_class=HTMLResponse)
    def index() -> str:
        return html_cache

    @application.post("/mcp")
    def mcp_http(payload: dict[str, Any], authorization: str | None = Header(default=None)):
        _require_auth(authorization)
        if not isinstance(payload, dict) or payload.get("jsonrpc") != "2.0" or not isinstance(payload.get("method"), str):
            return {"jsonrpc": "2.0", "id": None, "error": {"code": -32600, "message": "invalid JSON-RPC request"}}
        if "id" not in payload:
            return Response(status_code=202)
        msg_id = payload.get("id")
        try:
            res = mcp_srv.handle(payload)
        except mcp_server.MethodNotFoundError as exc:
            return {"jsonrpc": "2.0", "id": msg_id, "error": {"code": -32601, "message": str(exc)}}
        except Exception as exc:  # noqa: BLE001
            return {"jsonrpc": "2.0", "id": msg_id, "error": {"code": -32600 if isinstance(exc, ValueError) else -32603, "message": str(exc)}}
        return Response(status_code=202) if res is None else {"jsonrpc": "2.0", "id": msg_id, "result": res}

    @application.post("/api/eval/score", dependencies=[Depends(_require_auth)])
    def api_score(req: ScoreReq) -> dict[str, Any]:
        try:
            rec = eval_core.evaluate(req.suite, req.responses, req.identity)
        except (ValueError, KeyError, TypeError) as exc:
            raise HTTPException(status_code=400, detail=str(exc)) from exc
        run_id = f"run_{uuid.uuid4().hex[:10]}"
        store.save_run(run_id, rec, suite=req.suite, responses=req.responses)
        return {"run_id": run_id, "record": rec}

    @application.get("/api/eval/runs", dependencies=[Depends(_require_auth)])
    def api_runs(limit: int = Query(50, ge=1, le=200)) -> dict[str, Any]:
        return {"runs": store.list_runs(limit=limit)}

    @application.get("/api/eval/runs/{run_id}", dependencies=[Depends(_require_auth)])
    def api_get_run(run_id: str) -> dict[str, Any]:
        rec = store.get_run(run_id)
        if rec is None:
            raise HTTPException(status_code=404, detail="run not found")
        return {"run_id": run_id, "record": rec}

    @application.get("/api/eval/runs/{run_id}/evidence", dependencies=[Depends(_require_auth)])
    def api_get_evidence(run_id: str) -> dict[str, Any]:
        ev = store.get_evidence(run_id)
        if ev is None:
            raise HTTPException(status_code=404, detail="evidence not found")
        return {"run_id": run_id, **ev}

    @application.post("/api/eval/compare", dependencies=[Depends(_require_auth)])
    def api_compare(req: CompareReq) -> dict[str, Any]:
        try:
            return eval_core.compare(
                req.baseline,
                req.candidate,
                req.margin,
                alpha=req.alpha,
                assume_independent=req.assume_independent,
                suite=req.suite,
                baseline_responses=req.baseline_responses,
                candidate_responses=req.candidate_responses,
                require_recompute=req.require_recompute,
                allow_stack_mismatch=req.allow_stack_mismatch,
                stat_method=req.stat_method,
            )
        except (ValueError, KeyError, TypeError) as exc:
            raise HTTPException(status_code=400, detail=str(exc)) from exc

    @application.post("/api/eval/pass-k", dependencies=[Depends(_require_auth)])
    def api_pass_k(req: PassKReq) -> dict[str, Any]:
        try:
            return eval_core.evaluate_pass_k(req.records)
        except (ValueError, KeyError, TypeError) as exc:
            raise HTTPException(status_code=400, detail=str(exc)) from exc

    @application.get("/api/health")
    def api_health() -> dict[str, Any]:
        return {
            "ok": True,
            "name": "veto",
            "version": VERSION,
            "protocols": [p["grader"] for p in eval_core.VALID_PROTOCOLS],
        }

    return application


_cached_app: FastAPI | None = None


def __getattr__(name: str):
    """Lazy ASGI app accessor so importing veto.server has zero filesystem/DB side effects."""
    global _cached_app
    if name == "app":
        if _cached_app is None:
            _cached_app = create_app()
        return _cached_app
    raise AttributeError(f"module {__name__!r} has no attribute {name!r}")


def main() -> None:
    import uvicorn
    host = os.environ.get("VETO_HOST", "127.0.0.1")
    if os.environ.get("VETO_REQUIRE_AUTH", "0") != "1" and host not in ("127.0.0.1", "::1", "localhost"):
        raise RuntimeError("Set VETO_REQUIRE_AUTH=1 and VETO_API_TOKEN before binding a non-loopback address")
    uvicorn.run(create_app(), host=host, port=int(os.environ.get("VETO_PORT", "8001")))

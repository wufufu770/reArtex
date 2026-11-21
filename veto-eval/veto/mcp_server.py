"""Veto standalone MCP server (stdio, JSON-RPC 2.0)."""
from __future__ import annotations

import argparse
import json
from pathlib import Path
import sys
import uuid

try:
    from . import eval_core, __version__ as SERVER_VERSION
    from .store import EvalRunStore
except ImportError:
    import eval_core
    from store import EvalRunStore
    SERVER_VERSION = "0.5.0"

PROTOCOL_VERSION = "2025-06-18"
SERVER_NAME = "veto"


class MethodNotFoundError(Exception):
    pass


TOOLS = [
    {
        "name": "eval_score",
        "description": "Deterministically score agent responses against exact_text, reartex_pentest_v1, or deterministic_rubric_v1 suites.",
        "inputSchema": {
            "type": "object",
            "properties": {
                "suite": {"type": "object"},
                "responses": {"type": "array"},
                "identity": {"type": "object"},
            },
            "required": ["suite", "responses", "identity"],
            "additionalProperties": False,
        },
    },
    {
        "name": "eval_compare",
        "description": "Run fail-closed release gate comparing baseline and candidate eval records via paired_exact or hoeffding.",
        "inputSchema": {
            "type": "object",
            "properties": {
                "baseline": {"type": "object"},
                "candidate": {"type": "object"},
                "margin": {"type": "number"},
                "alpha": {"type": "number"},
                "stat_method": {"type": "string"},
                "require_recompute": {"type": "boolean"},
                "allow_stack_mismatch": {"type": "boolean"},
                "assume_independent": {"type": "boolean"},
                "suite": {"type": "object"},
                "baseline_responses": {"type": "array"},
                "candidate_responses": {"type": "array"},
            },
            "required": ["baseline", "candidate", "margin"],
            "additionalProperties": False,
        },
    },
    {
        "name": "eval_pass_k",
        "description": "Compute pass@k and pass^k multi-run consistency across repeated eval records.",
        "inputSchema": {
            "type": "object",
            "properties": {"records": {"type": "array"}},
            "required": ["records"],
            "additionalProperties": False,
        },
    },
    {
        "name": "eval_list_runs",
        "description": "List stored evaluation runs.",
        "inputSchema": {
            "type": "object",
            "properties": {"limit": {"type": "integer", "minimum": 1, "maximum": 200}},
            "required": [],
            "additionalProperties": False,
        },
    },
]


class Server:
    def __init__(self, store: EvalRunStore):
        self.store = store

    def call(self, name: str, args: dict):
        if not isinstance(args, dict):
            raise ValueError("arguments must be an object")
        if name == "eval_score":
            rec = eval_core.evaluate(args["suite"], args["responses"], args["identity"])
            run_id = f"run_{uuid.uuid4().hex[:10]}"
            self.store.save_run(run_id, rec, suite=args["suite"], responses=args["responses"])
            return {"run_id": run_id, "record": rec}
        if name == "eval_compare":
            return eval_core.compare(
                args["baseline"],
                args["candidate"],
                float(args["margin"]),
                alpha=float(args.get("alpha", 0.05)),
                assume_independent=bool(args.get("assume_independent", True)),
                suite=args.get("suite"),
                baseline_responses=args.get("baseline_responses"),
                candidate_responses=args.get("candidate_responses"),
                require_recompute=bool(args.get("require_recompute", True)),
                allow_stack_mismatch=bool(args.get("allow_stack_mismatch", False)),
                stat_method=str(args.get("stat_method", "paired_exact")),
            )
        if name == "eval_pass_k":
            return eval_core.evaluate_pass_k(args["records"])
        if name == "eval_list_runs":
            return {"runs": self.store.list_runs(limit=int(args.get("limit", 50)))}
        raise ValueError(f"unknown tool {name!r}")

    def handle(self, message: dict):
        if not isinstance(message, dict) or message.get("jsonrpc") != "2.0" or not isinstance(message.get("method"), str):
            raise ValueError("invalid JSON-RPC request")
        if "id" not in message:
            return None
        method = message["method"]
        params = message.get("params") or {}
        if not isinstance(params, dict):
            raise ValueError("params must be an object")
        if method == "initialize":
            return {"protocolVersion": PROTOCOL_VERSION, "capabilities": {"tools": {}},
                    "serverInfo": {"name": SERVER_NAME, "version": SERVER_VERSION}}
        if method == "ping":
            return {}
        if method == "tools/list":
            return {"tools": TOOLS}
        if method == "tools/call":
            try:
                val = self.call(params.get("name", ""), params.get("arguments") or {})
                return {"content": [{"type": "text", "text": json.dumps(val, ensure_ascii=False)}], "isError": False}
            except Exception as exc:  # noqa: BLE001
                return {"content": [{"type": "text", "text": f"{type(exc).__name__}: {exc}"}], "isError": True}
        raise MethodNotFoundError(f"unknown method {method!r}")


def serve(store: EvalRunStore, stdin=None, stdout=None):
    stdin = stdin or sys.stdin
    stdout = stdout or sys.stdout
    srv = Server(store)
    for line in stdin:
        line = line.strip()
        if not line:
            continue
        try:
            msg = json.loads(line)
        except json.JSONDecodeError:
            resp = {"jsonrpc": "2.0", "id": None, "error": {"code": -32700, "message": "Parse error"}}
        else:
            msg_id = msg.get("id") if isinstance(msg, dict) else None
            try:
                res = srv.handle(msg)
            except MethodNotFoundError as exc:
                resp = {"jsonrpc": "2.0", "id": msg_id, "error": {"code": -32601, "message": str(exc)}}
            except Exception as exc:  # noqa: BLE001
                resp = {"jsonrpc": "2.0", "id": msg_id,
                        "error": {"code": -32600 if isinstance(exc, ValueError) else -32603, "message": str(exc)}}
            else:
                if res is None:
                    continue
                resp = {"jsonrpc": "2.0", "id": msg_id, "result": res}
        stdout.write(json.dumps(resp, ensure_ascii=False) + "\n")
        stdout.flush()


def main(argv=None):
    p = argparse.ArgumentParser(description="Veto MCP stdio server")
    p.add_argument("--db", default="veto_runs.db")
    args = p.parse_args(argv)
    store = EvalRunStore(args.db)
    try:
        serve(store)
    finally:
        store.close()


if __name__ == "__main__":
    main()

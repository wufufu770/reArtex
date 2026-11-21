from __future__ import annotations

import json
from pathlib import Path
import sqlite3
import threading
from typing import Any


class EvalRunStore:
    """Thread-safe SQLite WAL persistence for Veto score records and raw evidence."""

    def __init__(self, db_path: str | Path) -> None:
        self.db_path = str(db_path)
        self._lock = threading.Lock()
        self.conn = sqlite3.connect(self.db_path, check_same_thread=False)
        self.conn.row_factory = sqlite3.Row
        self.conn.execute("PRAGMA journal_mode=WAL")
        self.conn.execute("PRAGMA busy_timeout=5000")
        self.conn.execute(
            """
            CREATE TABLE IF NOT EXISTS eval_runs (
                run_id TEXT PRIMARY KEY,
                protocol TEXT NOT NULL,
                model TEXT,
                harness TEXT,
                passed INT NOT NULL,
                graded INT NOT NULL,
                total INT NOT NULL,
                created_utc TEXT NOT NULL,
                record_json TEXT NOT NULL,
                suite_json TEXT,
                responses_json TEXT
            )
            """
        )
        cols = {r["name"] for r in self.conn.execute("PRAGMA table_info(eval_runs)").fetchall()}
        if "suite_json" not in cols:
            self.conn.execute("ALTER TABLE eval_runs ADD COLUMN suite_json TEXT")
        if "responses_json" not in cols:
            self.conn.execute("ALTER TABLE eval_runs ADD COLUMN responses_json TEXT")
        self.conn.execute("CREATE INDEX IF NOT EXISTS idx_eval_runs_created ON eval_runs(created_utc DESC)")
        self.conn.commit()

    def save_run(
        self,
        run_id: str,
        record: dict[str, Any],
        *,
        suite: dict[str, Any] | None = None,
        responses: list[dict[str, Any]] | None = None,
    ) -> None:
        if not isinstance(record, dict):
            raise ValueError("record must be a dict")
        ident = record.get("identity_declared") or {}
        summary = record.get("summary") or {}
        try:
            passed = int(summary.get("passed", 0))
            graded = int(summary.get("graded", 0))
            total = int(summary.get("total", 0))
        except (TypeError, ValueError) as exc:
            raise ValueError("malformed summary counts") from exc
        proto = (record.get("protocol") or {}).get("grader", "unknown")
        with self._lock:
            self.conn.execute(
                """
                INSERT OR REPLACE INTO eval_runs (
                    run_id, protocol, model, harness, passed, graded, total, created_utc,
                    record_json, suite_json, responses_json
                ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
                """,
                (
                    run_id,
                    proto,
                    ident.get("model"),
                    ident.get("harness"),
                    passed,
                    graded,
                    total,
                    str(record.get("created_utc", "")),
                    json.dumps(record, ensure_ascii=False),
                    json.dumps(suite, ensure_ascii=False) if suite is not None else None,
                    json.dumps(responses, ensure_ascii=False) if responses is not None else None,
                ),
            )
            self.conn.commit()

    def list_runs(self, limit: int = 50) -> list[dict[str, Any]]:
        bounded = max(1, min(200, int(limit)))
        with self._lock:
            rows = self.conn.execute(
                "SELECT run_id, protocol, model, harness, passed, graded, total, created_utc, "
                "(suite_json IS NOT NULL AND responses_json IS NOT NULL) AS has_evidence "
                "FROM eval_runs ORDER BY created_utc DESC LIMIT ?",
                (bounded,),
            ).fetchall()
        return [dict(r) for r in rows]

    def get_run(self, run_id: str) -> dict[str, Any] | None:
        with self._lock:
            row = self.conn.execute("SELECT record_json FROM eval_runs WHERE run_id = ?", (run_id,)).fetchone()
        return json.loads(row["record_json"]) if row else None

    def get_evidence(self, run_id: str) -> dict[str, Any] | None:
        with self._lock:
            row = self.conn.execute(
                "SELECT suite_json, responses_json FROM eval_runs WHERE run_id = ?", (run_id,)
            ).fetchone()
        if not row or not row["suite_json"] or not row["responses_json"]:
            return None
        return {"suite": json.loads(row["suite_json"]), "responses": json.loads(row["responses_json"])}

    def close(self) -> None:
        with self._lock:
            self.conn.close()

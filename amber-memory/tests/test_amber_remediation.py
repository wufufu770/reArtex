"""Regression and HTTP integration tests for Amber v0.5.0 remediations."""
from __future__ import annotations

from pathlib import Path
import sqlite3
import subprocess
import sys
import tempfile
import tomllib
import unittest

ROOT = Path(__file__).resolve().parent.parent
if str(ROOT) not in sys.path:
    sys.path.insert(0, str(ROOT))

from amber import __version__, crypto, mcp_server, memory_runtime

try:
    from fastapi.testclient import TestClient
    from amber import server
    _HAS_FASTAPI = True
except ImportError:
    TestClient = None
    server = None
    _HAS_FASTAPI = False


class AmberRemediationTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.db_path = Path(self.tmp.name) / "amber.db"
        self.rt = memory_runtime.Runtime(str(self.db_path), trust_key=b"0" * 32)
        self.rt.grant("owner", "default", ["read", "write", "review", "repair", "export", "audit"])
        self.rt.grant("reviewer", "default", ["read", "review"])
        self.rt.grant("other", "other_scope", ["read", "write", "audit"])

    def tearDown(self):
        self.rt.close()
        self.tmp.cleanup()

    def test_version_and_packaging_consistency(self):
        pyproject = tomllib.loads((ROOT / "pyproject.toml").read_text(encoding="utf-8"))
        self.assertEqual(pyproject["project"]["version"], "0.5.0")
        self.assertEqual(__version__, "0.5.0")
        self.assertEqual(mcp_server.SERVER_NAME, "memory")
        self.assertFalse((ROOT / "web").exists())
        self.assertTrue((ROOT / "amber" / "web" / "index.html").is_file())
        self.assertFalse((ROOT / "tests" / "test_vectors.json").exists())
        self.assertTrue((ROOT / "amber" / "test_vectors.json").is_file())
        proc = subprocess.run(
            [sys.executable, "-m", "amber.mcp_server", "--help"],
            cwd=str(ROOT),
            capture_output=True,
            text=True,
            check=False,
        )
        self.assertEqual(proc.returncode, 0, proc.stderr)

    def test_dual_backend_crypto_parity(self):
        key = b"k" * 32
        nonce = b"n" * 12
        aad = b"amber-aad"
        pt = b"secret memory payload"
        ct_fast, tag_fast = crypto.encrypt(key, nonce, pt, aad)
        orig = crypto._FAST_AEAD
        try:
            crypto._FAST_AEAD = False
            ct_pure, tag_pure = crypto.encrypt(key, nonce, pt, aad)
            self.assertEqual((ct_fast, tag_fast), (ct_pure, tag_pure))
            self.assertEqual(crypto.decrypt(key, nonce, ct_fast, tag_fast, aad), pt)
        finally:
            crypto._FAST_AEAD = orig
        self.assertEqual(crypto.decrypt(key, nonce, ct_pure, tag_pure, aad), pt)

    def test_sqlite_wal_and_audit_hash_chain_tamper_detection(self):
        mode = self.rt.db.execute("PRAGMA journal_mode").fetchone()[0]
        self.assertEqual(mode.lower(), "wal")
        self.rt.write("owner", "default", "first fact", channel="human")
        self.rt.write("owner", "default", "second fact", channel="human")
        res = self.rt.audit_chain_verify("owner", "default")
        self.assertTrue(res["valid"])
        self.assertGreaterEqual(res["checked"], 2)

        # Single-scope auditor cannot verify global unscoped chain when other scopes exist
        self.rt.write("other", "other_scope", "isolated", channel="human")
        with self.assertRaises(memory_runtime.AuthorizationError):
            self.rt.audit_chain_verify("owner", None)

        raw = sqlite3.connect(str(self.db_path))
        raw.execute("DROP TRIGGER IF EXISTS audit_no_update")
        raw.execute("UPDATE audit SET decision = 'tampered' WHERE seq = 1")
        raw.commit()
        raw.close()

        broken = self.rt.audit_chain_verify("owner", "default")
        self.assertFalse(broken["valid"])
        self.assertEqual(broken["broken_at"], 1)

    def test_audit_query_principal_self_logs_and_aggregates_audit_scope(self):
        self.rt.grant("owner", "second", ["read", "write", "audit"])
        self.rt.write("owner", "default", "hello", channel="human")
        self.rt.write("owner", "second", "hello", channel="human")
        self.rt.search("owner", "hello")
        self.rt.audit_query_principal("owner")
        row = self.rt.db.execute(
            "SELECT seq FROM audit WHERE action = 'audit_query_principal' ORDER BY seq DESC LIMIT 1"
        ).fetchone()
        self.assertIsNotNone(row)
        scopes = {
            r["scope"]
            for r in self.rt.db.execute("SELECT scope FROM audit_scope WHERE seq = ?", (row["seq"],))
        }
        self.assertIn("default", scopes)
        self.assertIn("second", scopes)

    def test_reject_quarantined_without_repair_permission(self):
        item_id = self.rt.write("owner", "default", "untrusted note", channel="web")
        receipt = self.rt.reject("reviewer", item_id, note="spam")
        self.assertEqual(receipt["state"], "withdrawn")
        self.assertEqual(self.rt.list_quarantined("owner", "default"), [])

    def test_inject_bundle_continues_past_oversized_first_item(self):
        big = "alpha " + ("x" * 2000)
        small = "alpha short"
        self.rt.write("owner", "default", big, channel="human")
        self.rt.write("owner", "default", small, channel="human")
        bundle = self.rt.inject_bundle("owner", "alpha", max_bytes=400)
        self.assertIn("alpha short", bundle["payload"])

    def test_outbound_verify_enforces_principal_ownership(self):
        self.rt.write("owner", "default", "alpha", channel="human")
        bundle = self.rt.inject_bundle("owner", "alpha")
        self.assertFalse(self.rt.verify_outbound("other", bundle["expectation_id"], bundle["payload"]))
        self.assertTrue(self.rt.verify_outbound("owner", bundle["expectation_id"], bundle["payload"]))
        events = self.rt.audit_query("owner", "default")
        self.assertTrue(any(e["action"] == "outbound_verify" for e in events))

    def test_frontend_honesty_and_no_dev_tokens(self):
        src = (ROOT / "amber" / "server.py").read_text(encoding="utf-8")
        self.assertNotIn("dev-owner-token", src)
        self.assertNotIn("dev-curator-token", src)
        self.assertNotIn("dev-worker-token", src)
        html = (ROOT / "amber" / "web" / "index.html").read_text(encoding="utf-8")
        self.assertIn("/api/redact-preview", html)
        self.assertIn("amber_api_token", html)
        self.assertIn("showApiError", html)

    @unittest.skipUnless(_HAS_FASTAPI, "fastapi/httpx not installed")
    def test_parse_encryption_key_fail_fast(self):
        self.assertIsNone(server.parse_encryption_key(""))
        self.assertEqual(len(server.parse_encryption_key("a" * 32)), 32)
        with self.assertRaises(RuntimeError):
            server.parse_encryption_key("too-short")

    @unittest.skipUnless(_HAS_FASTAPI, "fastapi/httpx not installed")
    def test_no_permission_restore_on_restart(self):
        db = Path(self.tmp.name) / "restart.db"
        app1 = server.create_app(db_path=db, require_auth=False, bootstrap=True)
        rt1 = app1.state.runtime
        rt1.revoke("agent:worker", "default", "write")
        rt1.close()

        app2 = server.create_app(db_path=db, require_auth=False, bootstrap=False)
        rt2 = app2.state.runtime
        with self.assertRaises(memory_runtime.AuthorizationError):
            rt2.write("agent:worker", "default", "should fail", channel="agent")
        rt2.close()

    @unittest.skipUnless(_HAS_FASTAPI, "fastapi/httpx not installed")
    def test_anonymous_mcp_defaults_to_least_privilege_worker(self):
        db = Path(self.tmp.name) / "anon_mcp.db"
        app = server.create_app(db_path=db, require_auth=False, bootstrap=True)
        client = TestClient(app)
        # Anonymous /mcp write MUST run as agent:worker (quarantined), NOT human:owner
        mcp_write = client.post(
            "/mcp",
            json={
                "jsonrpc": "2.0",
                "id": 1,
                "method": "tools/call",
                "params": {"name": "memory_write", "arguments": {"scope": "default", "body": "anon agent note"}},
            },
        )
        self.assertEqual(mcp_write.status_code, 200)
        # Must appear in quarantine, not active search
        q = client.get("/api/memory/quarantine").json()["items"]
        self.assertEqual(len(q), 1)
        hits = client.get("/api/memory/search", params={"q": "anon agent note"}).json()["hits"]
        self.assertEqual(len(hits), 0)
        app.state.runtime.close()

    @unittest.skipUnless(_HAS_FASTAPI, "fastapi/httpx not installed")
    def test_fastapi_http_roundtrip_role_isolation_and_error_contracts(self):
        db = Path(self.tmp.name) / "http.db"
        tokens = {"owner": "tok-owner", "curator": "tok-curator", "worker": "tok-worker"}
        app = server.create_app(db_path=db, require_auth=True, tokens=tokens, bootstrap=True)
        client = TestClient(app)

        self.assertEqual(client.get("/api/memory/items").status_code, 401)
        self.assertEqual(
            client.get("/api/memory/items", headers={"Authorization": "Bearer tok-worker"}).status_code,
            403,
        )
        mcp_resp = client.post(
            "/mcp",
            headers={"Authorization": "Bearer tok-worker"},
            json={"jsonrpc": "2.0", "id": 1, "method": "tools/list", "params": {}},
        )
        self.assertEqual(mcp_resp.status_code, 200)
        self.assertIn("result", mcp_resp.json())

        hdr = {"Authorization": "Bearer tok-owner"}
        # Explicit HTTP error contracts: 403 on unauthorized scope, 404 on missing item
        unauth_write = client.post("/api/memory/write", headers=hdr, json={"scope": "forbidden_scope", "body": "x"})
        self.assertEqual(unauth_write.status_code, 403)
        missing_item = client.get("/api/memory/item/nonexistent-id", headers=hdr)
        self.assertEqual(missing_item.status_code, 403)
        bad_review = client.post("/api/memory/review", headers=hdr, json={"item_id": "x", "decision": "invalid"})
        self.assertEqual(bad_review.status_code, 400)

        w = client.post("/api/memory/write", headers=hdr, json={"scope": "default", "body": "project phoenix"})
        self.assertEqual(w.status_code, 200)
        item_id = w.json()["item_id"]

        items = client.get("/api/memory/items", headers=hdr).json()["items"]
        self.assertTrue(any(it["id"] == item_id for it in items))

        hits = client.get("/api/memory/search", params={"q": "phoenix"}, headers=hdr).json()["hits"]
        self.assertEqual(len(hits), 1)

        self.assertEqual(client.get("/api/memory/audit/chain", headers=hdr).status_code, 403)
        chain = client.get("/api/memory/audit/chain", params={"scope": "default"}, headers=hdr).json()
        self.assertTrue(chain["valid"])

        red = client.post(
            "/api/redact-preview",
            headers=hdr,
            json={"text": "token=sk-1234567890abcdefghijklmnop"},
        )
        self.assertEqual(red.status_code, 200)
        self.assertTrue(red.json()["changed"])
        self.assertIn("<masked", red.json()["text"])

        mcp_write = client.post(
            "/mcp",
            headers={"Authorization": "Bearer tok-worker"},
            json={
                "jsonrpc": "2.0",
                "id": 2,
                "method": "tools/call",
                "params": {"name": "memory_write", "arguments": {"scope": "default", "body": "quarantined fact"}},
            },
        )
        self.assertEqual(mcp_write.status_code, 200)
        q_items = client.get("/api/memory/quarantine", headers=hdr).json()["items"]
        self.assertGreaterEqual(len(q_items), 1)
        rej = client.post(
            "/api/memory/review",
            headers={"Authorization": "Bearer tok-curator"},
            json={"item_id": q_items[0]["id"], "decision": "reject", "note": "unverified"},
        )
        self.assertEqual(rej.status_code, 200)
        self.assertEqual(rej.json()["result"]["state"], "withdrawn")
        app.state.runtime.close()


if __name__ == "__main__":
    unittest.main()

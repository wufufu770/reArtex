import os
import subprocess
import sys
import tempfile
import tomllib
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
if str(ROOT) not in sys.path:
    sys.path.insert(0, str(ROOT))

import veto
from veto import eval_core, mcp_server

try:
    from fastapi.testclient import TestClient
    from veto import server
    _HAS_FASTAPI = True
except ImportError:
    TestClient = None
    server = None
    _HAS_FASTAPI = False


class VetoRemediationTests(unittest.TestCase):
    def test_01_package_import_and_version_consistency(self):
        pyproj = tomllib.loads((ROOT / "pyproject.toml").read_text(encoding="utf-8"))
        v = pyproj["project"]["version"]
        self.assertEqual(veto.__version__, v)
        self.assertEqual(eval_core.VERSION, v)
        self.assertEqual(mcp_server.SERVER_VERSION, v)
        res = subprocess.run([sys.executable, "-m", "veto.eval_core", "--help"], cwd=ROOT, capture_output=True, text=True)
        self.assertEqual(res.returncode, 0, res.stderr)

    def test_02_06_rubric_budget_validation_and_fail_closed_missing(self):
        bad_suite = {
            "protocol": "deterministic_rubric_v1",
            "tasks": [{"id": "t1", "prompt": "p", "expected": {"max_tool_calls": "abc"}}],
        }
        with self.assertRaises(ValueError):
            eval_core.evaluate(bad_suite, [{"id": "t1", "response": {"output": "ok"}}], {"model": "m"})

        budget_suite = {
            "protocol": "deterministic_rubric_v1",
            "tasks": [{"id": "t1", "prompt": "p", "expected": {"required_substrings": ["PASS"], "max_tool_calls": 5}}],
        }
        rec = eval_core.evaluate(budget_suite, [{"id": "t1", "response": {"output": "PASS"}}], {"model": "m"})
        self.assertFalse(rec["results"][0]["pass"])
        self.assertEqual(rec["results"][0]["reason"], "efficiency_budget_exceeded")

    def test_07_signed_unverified_cannot_be_release_eligible(self):
        suite = {"tasks": [{"id": "t1", "kind": "exact_text", "prompt": "p", "expected": "ok"}]}
        resps = [{"id": "t1", "response": "ok"}]
        ident = {"model": "m", "harness": "h", "prompt_version": "v1"}
        key = b"a-very-strong-32-byte-secret-key!!"
        b = eval_core.evaluate(suite, resps, ident, key=key)
        c = eval_core.evaluate(suite, resps, ident, key=key)
        out_nokey = eval_core.compare(
            b, c, 0.05, assume_independent=True, suite=suite,
            baseline_responses=resps, candidate_responses=resps,
            require_recompute=True, stat_method="paired_exact",
        )
        self.assertFalse(out_nokey["release_eligible"])
        out_key = eval_core.compare(
            b, c, 0.05, assume_independent=True, suite=suite,
            baseline_responses=resps, candidate_responses=resps,
            require_recompute=True, stat_method="paired_exact", key=key,
        )
        self.assertTrue(out_key["release_eligible"])

    def test_22_examples_reproducible_across_python_versions(self):
        s = eval_core.read(ROOT / "examples/suite.json")
        br = eval_core.read(ROOT / "examples/baseline.json")
        cr = eval_core.read(ROOT / "examples/candidate.json")
        b = eval_core.read(ROOT / "examples/baseline.record.json")
        c = eval_core.read(ROOT / "examples/candidate.record.json")
        # Simulate recomputing a fixture recorded on a different Python patch version (N2)
        b["instrument"] = dict(b["instrument"], python="3.99.7")
        c["instrument"] = dict(c["instrument"], python="3.12.5")
        out = eval_core.compare(
            b, c, 0.1, assume_independent=True, suite=s,
            baseline_responses=br, candidate_responses=cr, require_recompute=True,
        )
        self.assertTrue(out["recompute_verified"])
        self.assertEqual(out["decision"], "inconclusive")

    def test_24_26_27_log_space_binom_passk_suite_required_and_single_html(self):
        p = eval_core._exact_binomial_upper_tail(500, 1000)
        self.assertGreater(p, 0.45)
        self.assertLess(p, 0.55)
        with self.assertRaises(ValueError):
            eval_core.evaluate_pass_k([{"results": [{"id": "t1", "status": "graded", "pass": True}]}])
        self.assertFalse((ROOT / "web").exists())
        html = (ROOT / "veto/web/index.html").read_text(encoding="utf-8")
        self.assertIn("&quot;", html)
        self.assertNotIn("hmac-sha256:' + fnvHex", html)
        self.assertIn("/evidence", html)

    @unittest.skipUnless(_HAS_FASTAPI, "fastapi/httpx not installed")
    def test_04_08_09_10_11_28_http_server_integration(self):
        with tempfile.TemporaryDirectory() as td:
            # Short signing key rejected on create_app (#10)
            old_tk = os.environ.get(eval_core.TRUST_KEY_ENV)
            try:
                os.environ[eval_core.TRUST_KEY_ENV] = "short-key"
                with self.assertRaises(RuntimeError):
                    server.create_app(Path(td) / "short.db")
            finally:
                if old_tk is None:
                    os.environ.pop(eval_core.TRUST_KEY_ENV, None)
                else:
                    os.environ[eval_core.TRUST_KEY_ENV] = old_tk

            app = server.create_app(Path(td) / "veto.db", max_body_bytes=4096)
            client = TestClient(app)
            h = client.get("/api/health")
            self.assertEqual(h.status_code, 200)
            self.assertEqual(h.json()["version"], veto.__version__)

            # Limit validation (#9)
            self.assertEqual(client.get("/api/eval/runs?limit=9999").status_code, 422)

            # Streaming / oversized body rejected with 413 (#11)
            big = client.post(
                "/api/eval/score",
                content=b"{" + b"a" * 5000 + b"}",
                headers={"Content-Type": "application/json"},
            )
            self.assertEqual(big.status_code, 413)

            # Score + evidence persistence (#4)
            suite = {"tasks": [{"id": "t1", "kind": "exact_text", "prompt": "p", "expected": "ok"}]}
            resps = [{"id": "t1", "response": "ok"}]
            r = client.post("/api/eval/score", json={"suite": suite, "responses": resps, "identity": {"model": "m"}})
            self.assertEqual(r.status_code, 200)
            run_id = r.json()["run_id"]
            ev = client.get(f"/api/eval/runs/{run_id}/evidence")
            self.assertEqual(ev.status_code, 200)
            self.assertEqual(ev.json()["suite"]["tasks"][0]["id"], "t1")

            # Auth misconfiguration fails closed 503 (#8)
            old_req = os.environ.get("VETO_REQUIRE_AUTH")
            old_tok = os.environ.get("VETO_API_TOKEN")
            try:
                os.environ["VETO_REQUIRE_AUTH"] = "1"
                os.environ.pop("VETO_API_TOKEN", None)
                self.assertEqual(client.get("/api/eval/runs").status_code, 503)
                os.environ["VETO_API_TOKEN"] = "secret-test-token"
                self.assertEqual(client.get("/api/eval/runs").status_code, 401)
                self.assertEqual(client.get("/api/eval/runs", headers={"Authorization": "Bearer wrong"}).status_code, 403)
                self.assertEqual(client.get("/api/eval/runs", headers={"Authorization": "Bearer secret-test-token"}).status_code, 200)
            finally:
                if old_req is None:
                    os.environ.pop("VETO_REQUIRE_AUTH", None)
                else:
                    os.environ["VETO_REQUIRE_AUTH"] = old_req
                if old_tok is None:
                    os.environ.pop("VETO_API_TOKEN", None)
                else:
                    os.environ["VETO_API_TOKEN"] = old_tok
            app.state.store.close()


if __name__ == "__main__":
    unittest.main()

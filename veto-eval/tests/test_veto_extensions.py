import os, sys, tempfile, threading, unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(ROOT / "veto"))
import eval_core
import mcp_server
from store import EvalRunStore


class VetoExtensionTests(unittest.TestCase):
    def test_deterministic_rubric_v1_and_mcp_stdio(self):
        suite = {
            "protocol": "deterministic_rubric_v1",
            "tasks": [{
                "id": "t1",
                "prompt": "Fix bug",
                "expected": {
                    "required_substrings": ["PASS"],
                    "forbidden_substrings": ["Traceback"],
                    "required_flags": {"tests_passed": True},
                    "max_guard_violations": 0,
                    "max_tool_calls": 10,
                    "max_tokens": 50000,
                },
            }],
        }
        good = [{"id": "t1", "response": {"output": "PASS", "tests_passed": True, "guard_violations": 0, "tool_calls": 3, "total_tokens": 9000}}]
        bad = [{"id": "t1", "response": {"output": "Traceback", "tests_passed": False, "guard_violations": 0, "tool_calls": 3, "total_tokens": 9000}}]
        ident = {"model": "m1", "harness": "h1", "prompt_version": "v1"}
        with tempfile.TemporaryDirectory() as td:
            st = EvalRunStore(os.path.join(td, "v.db"))
            try:
                srv = mcp_server.Server(st)
                r1 = srv.call("eval_score", {"suite": suite, "responses": good, "identity": ident})
                r2 = srv.call("eval_score", {"suite": suite, "responses": bad, "identity": ident})
                gate = srv.call("eval_compare", {
                    "baseline": r1["record"], "candidate": r2["record"], "margin": 0.05,
                    "suite": suite, "baseline_responses": good, "candidate_responses": bad,
                    "require_recompute": True, "assume_independent": True, "stat_method": "paired_exact",
                })
                self.assertEqual(gate["decision"], "regression")
                self.assertEqual(len(srv.call("eval_list_runs", {})["runs"]), 2)
                self.assertIsNone(srv.handle({"jsonrpc": "2.0", "method": "notifications/initialized"}))
                with self.assertRaises(mcp_server.MethodNotFoundError):
                    srv.handle({"jsonrpc": "2.0", "id": 1, "method": "nope"})
            finally:
                st.close()

    def test_pass_k_and_store_thread_safety(self):
        s1 = {"tasks": [{"id": "t1", "kind": "exact_text", "prompt": "p", "expected": "ok"}]}
        ident = {"model": "m", "harness": "h", "prompt_version": "v1"}
        r1 = eval_core.evaluate(s1, [{"id": "t1", "response": "ok"}], ident)
        r2 = eval_core.evaluate(s1, [{"id": "t1", "response": "no"}], ident)
        pk = eval_core.evaluate_pass_k([r1, r2])
        self.assertEqual(pk["pass_at_k"], 1.0)
        self.assertEqual(pk["pass_pow_k"], 0.0)
        with tempfile.TemporaryDirectory() as td:
            st = EvalRunStore(os.path.join(td, "v.db"))
            errs = []
            def w(i):
                try:
                    st.save_run(f"r_{i}", r1)
                    st.list_runs()
                except Exception as exc:
                    errs.append(exc)
            ths = [threading.Thread(target=w, args=(i,)) for i in range(10)]
            for th in ths: th.start()
            for th in ths: th.join()
            st.close()
            self.assertEqual(errs, [])


if __name__ == "__main__":
    unittest.main()

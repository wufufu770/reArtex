import os, sys, tempfile, unittest
from pathlib import Path

from amber import mcp_server, redaction
from amber.memory_runtime import Runtime


class AmberExtensionTests(unittest.TestCase):
    def test_redaction_url_and_secrets(self):
        self.assertEqual(redaction.redact("https://api.example.com/v1/items?token=secret"), "https://<masked-host>/v1/items")
        self.assertEqual(redaction.redact("postgres://admin:pass@db.internal:5432/prod?ssl=true"), "postgres://<masked-host>/prod")
        self.assertEqual(redaction.redact("suite_id: reartex-pentest-core-15"), "suite_id: reartex-pentest-core-15")
        self.assertEqual(redaction.redact("sk-ant-1234567890abcdef123456"), "<masked key>")

    def test_supersede_and_mcp_protocol(self):
        with tempfile.TemporaryDirectory() as td:
            rt = Runtime(os.path.join(td, "amber.db"))
            try:
                rt.grant("u1", "s1", ["read", "write", "repair"])
                i1 = rt.write("u1", "s1", "v1", channel="human")
                srv = mcp_server.Server(rt, principal="u1", channel="human")
                out = srv.call("memory_supersede", {"old_item_id": i1, "scope": "s1", "new_body": "v2"})
                self.assertEqual(out["supersedes"], i1)
                with self.assertRaises(Exception):
                    rt.supersede("u1", i1, "s1", "v3", channel="human")
                self.assertIsNone(srv.handle({"jsonrpc": "2.0", "method": "notifications/initialized"}))
                with self.assertRaises(mcp_server.MethodNotFoundError):
                    srv.handle({"jsonrpc": "2.0", "id": 1, "method": "unknown"})
            finally:
                rt.close()


if __name__ == "__main__":
    unittest.main()

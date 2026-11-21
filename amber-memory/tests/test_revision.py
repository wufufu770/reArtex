"""Regression cases discovered during the v0.2.1 review."""
import io
import json
import tempfile
import unittest
from unittest import mock
from pathlib import Path

from amber.memory_runtime import Runtime, MEMORY_MARKER_OPEN, MEMORY_MARKER_CLOSE
from amber.mcp_server import Server, serve
from amber.trusted_record import verify


class RevisionTests(unittest.TestCase):
    def runtime(self, path=':memory:', key=None):
        rt = Runtime(path, trust_key=key)
        self.addCleanup(rt.close)
        rt.grant('owner', 'project', ['read', 'write', 'review', 'audit'])
        return rt

    def test_relevance_order(self):
        rt = self.runtime()
        best = rt.write('owner', 'project', 'alpha alpha alpha', channel='human')
        rt.write('owner', 'project', 'alpha ' + 'irrelevant ' * 60, channel='human')
        self.assertEqual(rt.search('owner', 'alpha')[0]['id'], best)

    def test_encrypted_public_results_are_plaintext_and_json_serializable(self):
        rt = self.runtime(key=b'k' * 32)
        item = rt.write('owner', 'project', 'alpha 中文', channel='human')
        self.assertEqual(rt.search('owner', 'alpha')[0]['body'], 'alpha 中文')
        recalled = rt.recall('owner', item)
        self.assertEqual(recalled['item']['body'], 'alpha 中文')
        self.assertEqual(recalled['evidence']['body'], 'alpha 中文')
        json.dumps(recalled)
        self.assertIn('alpha 中文', rt.inject('owner', 'alpha'))

    def test_encryption_mode_cannot_silently_change(self):
        with tempfile.TemporaryDirectory() as td:
            path = Path(td) / 'memory.db'
            rt = Runtime(path)
            rt.grant('owner', 'project', ['write'])
            rt.write('owner', 'project', 'plaintext secret', channel='human')
            rt.close()
            with self.assertRaises(ValueError):
                Runtime(path, trust_key=b'k' * 32)

    def test_encrypted_reopen_requires_matching_key(self):
        with tempfile.TemporaryDirectory() as td:
            path = Path(td) / 'memory.db'
            rt = Runtime(path, trust_key=b'k' * 32)
            rt.close()
            for key in (None, b'x' * 32, b'short'):
                with self.subTest(key=key), self.assertRaises(ValueError):
                    Runtime(path, trust_key=key)
            Runtime(path, trust_key=b'k' * 32).close()

    def test_full_inject_budget_includes_markers(self):
        rt = self.runtime()
        rt.write('owner', 'project', 'alpha ' + '中' * 20, channel='human')
        result = rt.inject_bundle('owner', 'alpha', max_bytes=256)
        self.assertLessEqual(result['bytes'], 256)

    def test_placeholder_is_valid_json_with_byte_budget(self):
        rt = self.runtime()
        for i in range(10):
            rt.write('owner', 'project', 'alpha 中文' * 20, channel='human')
        result = rt.inject_placeholder('owner', 'alpha', max_bytes=256)
        text = result['placeholder']
        self.assertLessEqual(len(text.encode('utf-8')), 256)
        self.assertTrue(text.endswith(MEMORY_MARKER_CLOSE))
        payload = json.loads(text[len(MEMORY_MARKER_OPEN):-len(MEMORY_MARKER_CLOSE)])
        self.assertEqual(payload['count'], len(result['refs']))
        count = rt.db.execute('SELECT count(*) FROM memory_refs').fetchone()[0]
        self.assertEqual(count, len(result['refs']))

    def test_empty_search_is_visible_in_scope_audit(self):
        rt = self.runtime()
        rt.search('owner', 'absent')
        events = rt.audit_query('owner', 'project')
        self.assertTrue(any(e['action'] == 'search' for e in events))

    def test_generator_parents_are_not_lost(self):
        rt = self.runtime()
        root = rt.write('owner', 'project', 'alpha', channel='human')
        child = rt.write('owner', 'project', 'derived', channel='agent',
                         parents=(p for p in [root]))
        self.assertIn(child, rt.descendants(root))

    def test_generator_grants_and_nested_atomic_batch(self):
        rt = self.runtime()
        rt.grant('guest', 'project', (a for a in ['read']))
        with rt.transaction():
            rt.write('owner', 'project', 'alpha', channel='human')
        self.assertEqual(len(rt.search('guest', 'alpha')), 1)
        with self.assertRaises(ValueError):
            with rt.transaction():
                rt.write('owner', 'project', 'rollback', channel='human')
                raise ValueError('abort batch')
        self.assertEqual(rt.search('guest', 'rollback'), [])

    def test_scope_auditor_cannot_read_cross_scope_metadata(self):
        rt = self.runtime()
        rt.grant('owner', 'secret-project', ['read', 'write'])
        secret = rt.write('owner', 'secret-project', 'alpha', channel='human')
        rt.write('owner', 'project', 'alpha', channel='human')
        rt.grant('auditor', 'project', ['audit'])
        rt.search('owner', 'alpha')
        event = next(e for e in rt.audit_query('auditor', 'project') if e['action'] == 'search')
        self.assertNotIn('secret-project', event['detail'])
        self.assertNotIn(secret, event['detail'])

    def test_malformed_rpc_does_not_kill_server(self):
        rt = self.runtime()
        messages = ['{bad', '[]', 'null', json.dumps({'jsonrpc': '2.0', 'id': 9, 'method': 'ping'})]
        out = io.StringIO()
        serve(rt, 'owner', 'agent', io.StringIO('\n'.join(messages)), out)
        rows = [json.loads(line) for line in out.getvalue().splitlines()]
        self.assertEqual(rows[-1]['result'], {})
        self.assertEqual(rows[0]['error']['code'], -32700)

    def test_notification_cannot_execute_tool(self):
        rt = self.runtime()
        server = Server(rt, 'owner', 'human')
        result = server.handle({'jsonrpc': '2.0', 'method': 'tools/call', 'params': {
            'name': 'memory_write', 'arguments': {'scope': 'project', 'body': 'alpha'}}})
        self.assertIsNone(result)
        self.assertEqual(rt.db.execute('SELECT count(*) FROM items').fetchone()[0], 0)

    def test_mcp_rejects_bool_integer_and_unknown_args(self):
        rt = self.runtime()
        server = Server(rt, 'owner', 'human')
        for args in ({'query': 'alpha', 'k': True}, {'query': 'alpha', 'source': 'user'}):
            with self.subTest(args=args), self.assertRaises(ValueError):
                server.call('memory_search', args)

    def test_server_start_does_not_restore_revoked_permissions(self):
        from amber.mcp_server import main
        with tempfile.TemporaryDirectory() as td:
            path = Path(td) / 'memory.db'
            rt = Runtime(path)
            rt.grant('owner', 'project', ['read', 'review'])
            rt.revoke('owner', 'project', 'review')
            rt.close()
            with mock.patch('amber.mcp_server.serve'):
                main(['--db', str(path), '--principal', 'owner', '--scope', 'project'])
            rt = Runtime(path)
            try:
                actions = {r[0] for r in rt.db.execute('SELECT action FROM permissions WHERE principal=?', ('owner',))}
                self.assertEqual(actions, {'read'})
            finally:
                rt.close()

    def test_bad_signature_returns_false(self):
        for record in ({'signature': 'hmac-sha256:中'}, {'signature': 'hmac-sha256:x', 'v': float('nan')}):
            self.assertFalse(verify(record, b'key'))


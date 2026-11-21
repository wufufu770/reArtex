"""MCP server 往返测试：真的起一个 stdio server，走 JSON-RPC。"""
import io
import json
import unittest

from amber.mcp_server import PROTOCOL_VERSION, Server
from amber.memory_runtime import Runtime


def drive(server, messages):
    """喂一串 JSON-RPC 消息，返回响应列表（通知不产生响应）。"""
    responses = []
    for message in messages:
        result = server.handle(message)
        if result is None:
            continue
        responses.append({'jsonrpc': '2.0', 'id': message.get('id'), 'result': result})
    return responses


class McpServerTests(unittest.TestCase):
    def setUp(self):
        self.rt = Runtime(':memory:')
        self.rt.grant('human:owner', 'project',
                      ['read', 'write', 'review', 'repair', 'export', 'audit'])
        self.server = Server(self.rt, 'human:owner', 'human')

    def tearDown(self):
        self.rt.close()

    def test_initialize(self):
        out = drive(self.server, [{'jsonrpc': '2.0', 'id': 1, 'method': 'initialize',
                                    'params': {}}])[0]
        self.assertEqual(out['result']['protocolVersion'], PROTOCOL_VERSION)
        self.assertIn('tools', out['result']['capabilities'])
        self.assertEqual(out['result']['serverInfo']['name'], 'memory')

    def test_tools_list(self):
        out = drive(self.server, [{'jsonrpc': '2.0', 'id': 2, 'method': 'tools/list'}])[0]
        names = {t['name'] for t in out['result']['tools']}
        for expected in ('memory_search', 'memory_recall', 'memory_write', 'memory_inject'):
            self.assertIn(expected, names)

    def test_write_search_recall_round_trip(self):
        drive(self.server, [{'jsonrpc': '2.0', 'id': 3, 'method': 'tools/call',
                             'params': {'name': 'memory_write',
                                        'arguments': {'scope': 'project',
                                                      'body': '截止日期 2026-12-01 alpha'}}}])
        out = drive(self.server, [{'jsonrpc': '2.0', 'id': 4, 'method': 'tools/call',
                                    'params': {'name': 'memory_search',
                                               'arguments': {'query': '截止日期'}}}])[0]
        payload = json.loads(out['result']['content'][0]['text'])
        self.assertEqual(len(payload['hits']), 1)
        item_id = payload['hits'][0]['id']
        out = drive(self.server, [{'jsonrpc': '2.0', 'id': 5, 'method': 'tools/call',
                                    'params': {'name': 'memory_recall',
                                               'arguments': {'item_id': item_id}}}])[0]
        recalled = json.loads(out['result']['content'][0]['text'])
        self.assertIn('截止日期', recalled['evidence']['body'])

    def test_write_tool_has_no_source_parameter(self):
        """工具 schema 里不能出现 source——来源由服务端通道决定。"""
        out = drive(self.server, [{'jsonrpc': '2.0', 'id': 6, 'method': 'tools/list'}])[0]
        write_tool = next(t for t in out['result']['tools'] if t['name'] == 'memory_write')
        self.assertNotIn('source', write_tool['inputSchema']['properties'])
        self.assertNotIn('channel', write_tool['inputSchema']['properties'])

    def test_agent_channel_writes_are_quarantined(self):
        """以 agent 通道运行的 server：写出来的东西必须待审。"""
        server = Server(self.rt, 'human:owner', 'agent')
        drive(server, [{'jsonrpc': '2.0', 'id': 7, 'method': 'tools/call',
                        'params': {'name': 'memory_write',
                                   'arguments': {'scope': 'project', 'body': 'agent wrote this'}}}])
        rows = self.rt.db.execute("SELECT state FROM items WHERE body LIKE '%agent wrote%'").fetchall()
        self.assertEqual(rows[0][0], 'quarantined')

    def test_unknown_tool_returns_is_error(self):
        out = drive(self.server, [{'jsonrpc': '2.0', 'id': 8, 'method': 'tools/call',
                                    'params': {'name': 'memory_nope', 'arguments': {}}}])[0]
        self.assertTrue(out['result']['isError'])

    def test_notification_produces_no_response(self):
        self.assertIsNone(self.server.handle(
            {'jsonrpc': '2.0', 'method': 'notifications/initialized'}))

    def test_serve_loop_reads_lines(self):
        import io
        lines = (json.dumps({'jsonrpc': '2.0', 'id': 9, 'method': 'initialize'}) + '\n'
                 + '\n'
                 + json.dumps({'jsonrpc': '2.0', 'id': 10, 'method': 'tools/list'}) + '\n')
        out = io.StringIO()
        from amber.mcp_server import serve
        serve(self.rt, 'human:owner', 'human', stdin=io.StringIO(lines), stdout=out)
        parsed = [json.loads(l) for l in out.getvalue().splitlines() if l.strip()]
        self.assertEqual(len(parsed), 2)
        self.assertEqual(parsed[0]['result']['protocolVersion'], PROTOCOL_VERSION)
        self.assertIn('tools', parsed[1]['result'])


if __name__ == '__main__':
    unittest.main()

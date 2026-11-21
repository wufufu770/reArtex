"""治理记忆库的 MCP server（stdio，JSON-RPC 2.0）。

存在的理由很具体：外部上下文压缩器（billion-context 等）按**工具名**保护
不可压缩的内容（protectedTools）。治理记忆要有这个通道，就得先有一个
标准工具面——否则压缩器只看到一段文本，会把它折成语义摘要，逐字承诺当场失效。

本 server 的三条纪律：

1. **主体固定**：server 以一个可信主体身份运行（`--principal`），不是每个
   调用方自报身份。授权判定仍由治理库内部做。
2. **通道固定**：写入用的 channel 由宿主在启动时指定（`--channel`，默认
   `agent`）。调用方**不能**指定 channel，也**不能**指定 source——
   这是 P0-2「来源绑定」的强制点。MCP 工具的参数里没有 source。
3. **内容只经工具结果返回**：注入内容放在 tool result 里，不合成 system/user
   消息（对应 billion-context 的"不可信数据不升权"契约）。

协议：MCP over stdio，每行一个 JSON-RPC 2.0 消息。
"""
from __future__ import annotations

import argparse
import json
import sys

try:
    from . import __version__ as SERVER_VERSION
    from .memory_runtime import Runtime
except ImportError:
    from memory_runtime import Runtime
    SERVER_VERSION = '0.4.0'

PROTOCOL_VERSION = '2025-06-18'
SERVER_NAME = 'memory'


class MethodNotFoundError(Exception):
    pass


def _schema(properties, required=None):
    return {'type': 'object', 'properties': properties, 'required': required or [], 'additionalProperties': False}


TOOLS = [
    {
        'name': 'memory_search',
        'description': '在授权范围内检索记忆条目。返回条目 id、正文与相关度。',
        'inputSchema': _schema({'query': {'type': 'string'},
                                'k': {'type': 'integer', 'minimum': 1, 'maximum': 100}},
                               ['query']),
    },
    {
        'name': 'memory_recall',
        'description': '按条目 id 取回条目与原始证据（含 sha256）。需要 read 权限。',
        'inputSchema': _schema({'item_id': {'type': 'string'}}, ['item_id']),
    },
    {
        'name': 'memory_write',
        'description': '写入一条记忆。来源由服务端启动时指定的通道决定，调用方不能指定。',
        'inputSchema': _schema({'scope': {'type': 'string'},
                                'body': {'type': 'string'},
                                'session': {'type': 'string'}},
                               ['scope', 'body']),
    },
    {
        'name': 'memory_inject',
        'description': '产出注入包（受额度约束），并登记一条出站对账期望。',
        'inputSchema': _schema({'query': {'type': 'string'},
                                'max_bytes': {'type': 'integer'}}, ['query']),
    },
    {
        'name': 'memory_verify_outbound',
        'description': '比对实际送出的字节与登记的期望摘要；不一致即视为未通过。',
        'inputSchema': _schema({'expectation_id': {'type': 'string'},
                                'observed': {'type': 'string'}}, ['expectation_id', 'observed']),
    },
    {
        'name': 'memory_retrieve',
        'description': '按占位符 ref 取回原文（逐字）。',
        'inputSchema': _schema({'ref': {'type': 'string'}}, ['ref']),
    },
    {
        'name': 'memory_review_queue',
        'description': '列出隔离区待审条目的元数据（不含正文）。',
        'inputSchema': _schema({'scope': {'type': 'string'},
                                'limit': {'type': 'integer', 'minimum': 1, 'maximum': 100}},
                               ['scope']),
    },
    {
        'name': 'memory_export',
        'description': '导出某个 scope 的 active 条目（最小 OKF 子集）。',
        'inputSchema': _schema({'scope': {'type': 'string'}}, ['scope']),
    },
    {
        'name': 'memory_supersede',
        'description': '原子写入新版本记忆并逻辑撤回旧 active 条目。',
        'inputSchema': _schema({'old_item_id': {'type': 'string'},
                                'scope': {'type': 'string'},
                                'new_body': {'type': 'string'},
                                'session': {'type': 'string'}},
                               ['old_item_id', 'scope', 'new_body']),
    },
]


class Server:
    def __init__(self, runtime, principal, channel):
        self.runtime = runtime
        self.principal = principal
        self.channel = channel

    # ── 工具实现 ────────────────────────────────────────────────────────────
    def call(self, name, args):
        tool = next((tool for tool in TOOLS if tool['name'] == name), None)
        if tool is None or not isinstance(args, dict):
            raise ValueError('unknown tool or invalid arguments')
        schema = tool['inputSchema']
        if set(args) - set(schema['properties']) or set(schema['required']) - set(args):
            raise ValueError('unexpected or missing arguments')
        for key, value in args.items():
            kind = schema['properties'][key]['type']
            if (kind == 'string' and not isinstance(value, str)) or (kind == 'integer' and type(value) is not int):
                raise ValueError(f'invalid argument type: {key}')
        rt, principal = self.runtime, self.principal
        if name == 'memory_search':
            rows = rt.search(principal, args['query'], k=int(args.get('k', 5)))
            return {'hits': [{'id': r['id'], 'scope': r['scope'], 'score': r['score'],
                              'text': r['body']} for r in rows]}
        if name == 'memory_recall':
            return rt.recall(principal, args['item_id'])
        if name == 'memory_write':
            item_id = rt.write(principal, args['scope'], args['body'],
                               channel=self.channel,
                               session=args.get('session', 'mcp'))
            return {'item_id': item_id}
        if name == 'memory_inject':
            return rt.inject_bundle(principal, args['query'],
                                     max_bytes=int(args.get('max_bytes', 25000)))
        if name == 'memory_verify_outbound':
            ok = rt.verify_outbound(principal, args['expectation_id'], args['observed'])
            return {'verified': ok}
        if name == 'memory_retrieve':
            return rt.retrieve_by_ref(principal, args['ref'])
        if name == 'memory_review_queue':
            return {'queue': rt.list_quarantined(principal, args['scope'],
                                                 limit=int(args.get('limit', 100)))}
        if name == 'memory_export':
            return {'files': rt.export(principal, args['scope'])}
        if name == 'memory_supersede':
            return rt.supersede(principal, args['old_item_id'], args['scope'], args['new_body'],
                                channel=self.channel, session=args.get('session', 'mcp-supersede'))
        raise ValueError(f'unknown tool {name!r}')

    # ── JSON-RPC ────────────────────────────────────────────────────────────
    def handle(self, message):
        if not isinstance(message, dict) or message.get('jsonrpc') != '2.0' or not isinstance(message.get('method'), str):
            raise ValueError('invalid JSON-RPC request')
        if 'id' not in message:
            return None
        method = message.get('method')
        msg_id = message.get('id')
        params = message.get('params', {})
        if not isinstance(params, dict):
            raise ValueError('params must be an object')

        if method == 'initialize':
            return {'protocolVersion': PROTOCOL_VERSION,
                    'capabilities': {'tools': {}},
                    'serverInfo': {'name': SERVER_NAME, 'version': SERVER_VERSION}}
        if method == 'ping':
            return {}
        if method == 'tools/list':
            return {'tools': TOOLS}
        if method == 'tools/call':
            name = params.get('name')
            args = params.get('arguments') or {}
            try:
                value = self.call(name, args)
                return {'content': [{'type': 'text',
                                     'text': json.dumps(value, ensure_ascii=False)}],
                        'isError': False}
            except Exception as exc:                       # noqa: BLE001
                return {'content': [{'type': 'text',
                                     'text': f'{type(exc).__name__}: {exc}'}],
                        'isError': True}
        if msg_id is None:
            return None                                   # 通知，不需要响应
        raise MethodNotFoundError(f'unknown method {method!r}')


def serve(runtime, principal, channel, stdin=None, stdout=None):
    stdin = stdin or sys.stdin
    stdout = stdout or sys.stdout
    server = Server(runtime, principal, channel)
    for line in stdin:
        line = line.strip()
        if not line:
            continue
        try:
            message = json.loads(line)
        except json.JSONDecodeError:
            response = {'jsonrpc': '2.0', 'id': None, 'error': {'code': -32700, 'message': 'Parse error'}}
        else:
            msg_id = message.get('id') if isinstance(message, dict) else None
            try:
                result = server.handle(message)
            except Exception as exc:
                response = {'jsonrpc': '2.0', 'id': msg_id,
                            'error': {'code': -32601 if isinstance(exc, MethodNotFoundError) else (-32600 if isinstance(exc, ValueError) else -32603), 'message': str(exc)}}
            else:
                if result is None:
                    continue
                response = {'jsonrpc': '2.0', 'id': msg_id, 'result': result}
        stdout.write(json.dumps(response, ensure_ascii=False) + '\n')
        stdout.flush()


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--db', default='memory.db')
    parser.add_argument('--principal', required=True, help='本 server 代表的可信主体')
    parser.add_argument('--scope', default='default', help='默认 scope')
    parser.add_argument('--channel', default='agent',
                        help='写入使用的通道（宿主决定；默认 agent → 隔离区待审）')
    parser.add_argument('--bootstrap-actions', help='explicit local provisioning only, comma separated; default grants nothing')
    args = parser.parse_args(argv)
    runtime = Runtime(args.db)
    try:
        if args.bootstrap_actions:
            runtime.grant(args.principal, args.scope, args.bootstrap_actions.split(','))
        serve(runtime, args.principal, args.channel)
    finally:
        runtime.close()


if __name__ == '__main__':
    main()

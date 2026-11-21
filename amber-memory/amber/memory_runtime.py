"""外置记忆治理内核 v0.2 —— 单操作者本地原型，DB 文件访问权即信任边界。

v0.2 相对 v0.1 的四处结构性变化（每条都对应一个已验证的问题）：

1. **来源由通道绑定，不再由调用方自称**（`write(..., channel=...)`）。
   v0.1 的 `source='user'` 是普通参数：任何拿到 read+write 的 agent 都能自称
   user、直接写出 active 条目。v0.2 把 source 变成宿主侧的通道→来源映射，
   不可信通道（agent）映射到 model，条目一律进隔离区待审。

2. **审计不再有 scope 空洞**（`audit_scope` 映射表 + 三个查询入口）。
   v0.1 的 search / inject / item 级拒绝一律记空 scope，而审计查询按 scope
   过滤——于是"谁检索过这个 scope"在受支持的审计视图里根本查不到。

3. **检索层改成按 scope 分片的持久索引**（`scope_index` + 每 scope 一个 FTS5）。
   v0.1 每次检索重建整个索引，延迟随语料线性增长（实测 2 万条 p50 305ms），
   且 O(n) 建索引。分片后每个 scope 的 bm25 统计只来自本 scope，
   "隐藏语料不影响排序"这个安全属性仍然成立（见 `_search_scope` 注释）。

4. **落盘可加密**（ChaCha20-Poly1305，纯标准库实现，见 crypto.py）。
   加密模式下**不建持久索引**——索引里会有明文。这是刻意的取舍，不是遗漏。

另外新增两个与外部上下文压缩器（billion-context 等）对接的通道：
   - 出站对账（`inject_bundle` / `verify_outbound`）：注入的内容在送往上游前
     必须逐字比对，对不上就阻断——上游压缩器改写了治理记忆时能被发现。
   - 无损占位（`inject_placeholder` / `retrieve_by_ref`）：模型看到的是占位符，
     需要原文时用工具取回，避免"让模型读压缩后的转述"。

约束不变：Python 3.11+、标准库、无模型/API 费用、SQLite 需支持 FTS5。
"""
from __future__ import annotations

import argparse
import hashlib
import hmac
import json
import re
import sqlite3
import uuid
from contextlib import contextmanager
from datetime import datetime, timezone
from pathlib import Path

try:
    from . import crypto
    from .redaction import redact_detail
    from .trusted_record import canonical, digest, key_id, read_key, sign, verify
except ImportError:
    import crypto
    from redaction import redact_detail
    from trusted_record import canonical, digest, key_id, read_key, sign, verify

# ── 通道 → 来源绑定 ───────────────────────────────────────────────────────────
# 默认映射。宿主可以在构造 Runtime 时覆盖（channel_sources=...），那是宿主的
# 显式选择；调用方（agent）无法影响。
#
# 'agent' 映射到 'model' 而不是 'user'：不可信通道自报的内容一律按"模型产出"
# 处理，必须经审核才能发布。这条映射就是 v0.2 对 v0.1 "source 可自称"的修复。
DEFAULT_CHANNEL_SOURCES = {
    'human': 'user',
    'tool': 'tool',
    'web': 'web',
    'document': 'document',
    'model': 'model',
    'agent': 'model',
}

VALID_SOURCES = {'user', 'tool', 'web', 'document', 'model'}

# 压缩器应当跳过的治理记忆标记。billion-context 一类压缩器按工具名保护
# （protectedTools）时这层是冗余的；对不认识工具名保护的压缩器，它是唯一的
# 识别依据——所以两处都要给。
MEMORY_MARKER_OPEN = '<memory-governed>'
MEMORY_MARKER_CLOSE = '</memory-governed>'

# 治理库对宿主暴露的 MCP 工具名前缀。billion-context 的 protectedTools 用
# 前缀通配（kernel/src/protected.ts:197-204 的 matchToolPattern 支持 `*` 后缀）。
PROTECTED_TOOL_PATTERN = 'mcp__memory__*'

ENC_PREFIX = b'BC1'


def now():
    return datetime.now(timezone.utc).isoformat()


def uid():
    return uuid.uuid4().hex


def encode_data(payload):
    """JSON 编码 + 转义行分隔符。

    \u0085 / \u2028 / \u2029 在 JSON 字符串里是合法字符，但会把一段文本
    拆成多行——对提示注入来说这是现成的载体。逐字转义掉。
    """
    encoded = json.dumps(payload, ensure_ascii=False)
    for char in ('\u0085', '\u2028', '\u2029'):
        encoded = encoded.replace(char, '\\u' + format(ord(char), '04x'))
    return encoded


def lexical(text):
    """显式的中文字符/双字基线切分，不是生产级分词器。"""
    tokens = re.findall(r'[a-z0-9_]+|[\u3400-\u9fff]', text.lower())
    han = re.findall(r'[\u3400-\u9fff]+', text)
    tokens += [s[i:i + 2] for s in han for i in range(len(s) - 1)]
    return ' '.join(tokens)


def scope_fts_name(scope: str) -> str:
    """scope → 它自己的 FTS5 表名。用 sha1 前缀避免非法标识符与重名。"""
    return 'fts_' + hashlib.sha256(scope.encode('utf-8')).hexdigest()[:16]


class AuthorizationError(PermissionError):
    def __init__(self, principal, action, scope=''):
        super().__init__('unavailable or unauthorized')
        self.principal, self.action, self.scope = principal, action, scope


class Runtime:
    def __init__(self, db, *, channel_sources=None, trust_key=None):
        """channel_sources: 宿主侧的通道→来源映射，覆盖默认。trust_key: 导出签名用。"""
        if trust_key is not None and (not isinstance(trust_key, (bytes, bytearray)) or len(trust_key) != 32):
            raise ValueError('trust_key must be exactly 32 bytes')
        self.key = bytes(trust_key) if trust_key is not None else None
        self.db = sqlite3.connect(str(db), isolation_level=None, check_same_thread=False)
        self.db.execute('PRAGMA journal_mode=WAL')
        self.db.execute('PRAGMA busy_timeout=5000')
        self.db.row_factory = sqlite3.Row
        self.db.execute('PRAGMA foreign_keys=ON')
        self.db.execute('PRAGMA busy_timeout=5000')
        self.channel_sources = dict(DEFAULT_CHANNEL_SOURCES)
        if channel_sources:
            self.channel_sources.update(channel_sources)
        for channel, source in self.channel_sources.items():
            if source not in VALID_SOURCES:
                raise ValueError(f'channel {channel!r} maps to invalid source {source!r}')
        try:
            self._check_storage_mode()
            self._schema()
            self._migrate()
        except BaseException:
            self.db.close()
            raise

    def _check_storage_mode(self):
        """Refuse implicit encryption migration/downgrade before changing the DB."""
        tables = {r[0] for r in self.db.execute("SELECT name FROM sqlite_master WHERE type='table'")}
        mode = 'encrypted' if self.key is not None else 'plain'
        check = hmac.new(self.key, b'governance-memory/key-check/v1', hashlib.sha256).hexdigest() if self.key else ''
        if 'storage_config' in tables:
            stored = dict(self.db.execute('SELECT name,value FROM storage_config'))
            if stored.get('mode') != mode or not hmac.compare_digest(stored.get('key_check', ''), check):
                raise ValueError('database encryption mode or key mismatch; explicit offline migration required')
            return
        if 'evidence' in tables:
            cols = {r[1] for r in self.db.execute('PRAGMA table_info(evidence)')}
            rows = self.db.execute('SELECT body,enc FROM evidence' if 'enc' in cols else 'SELECT body,NULL FROM evidence')
            for body, enc in rows:
                if (enc is not None) != (self.key is not None):
                    raise ValueError('database encryption mode mismatch; explicit offline migration required')
                if enc is not None:
                    try:
                        self._open(body, enc)
                    except (ValueError, crypto.EncryptionError) as exc:
                        raise ValueError('database key or ciphertext invalid') from exc
        self.db.execute('CREATE TABLE IF NOT EXISTS storage_config(name TEXT PRIMARY KEY,value TEXT NOT NULL)')
        self.db.executemany('INSERT OR IGNORE INTO storage_config VALUES(?,?)', [('mode', mode), ('key_check', check)])

    # ── schema ────────────────────────────────────────────────────────────────
    def _schema(self):
        self.db.executescript('''
        CREATE TABLE IF NOT EXISTS permissions(
          principal TEXT, scope TEXT, action TEXT,
          PRIMARY KEY(principal,scope,action));
        CREATE TABLE IF NOT EXISTS evidence(
          id TEXT PRIMARY KEY, scope TEXT NOT NULL, source TEXT NOT NULL,
          session TEXT NOT NULL, actor TEXT NOT NULL, body TEXT NOT NULL,
          sha256 TEXT NOT NULL, created TEXT NOT NULL, enc BLOB);
        CREATE TABLE IF NOT EXISTS items(
          id TEXT PRIMARY KEY, evidence_id TEXT NOT NULL REFERENCES evidence(id),
          scope TEXT NOT NULL, body TEXT NOT NULL,
          state TEXT NOT NULL CHECK(state IN ('active','quarantined','superseded','withdrawn')),
          actor TEXT NOT NULL, created TEXT NOT NULL, expires TEXT,
          corrects_id TEXT REFERENCES items(id), enc BLOB);
        CREATE TABLE IF NOT EXISTS influence(
          parent TEXT REFERENCES items(id), child TEXT REFERENCES items(id),
          PRIMARY KEY(parent,child));
        CREATE TABLE IF NOT EXISTS verifications(
          item_id TEXT REFERENCES items(id), actor TEXT, created TEXT);
        CREATE TABLE IF NOT EXISTS audit(
          seq INTEGER PRIMARY KEY, actor TEXT, action TEXT, scope TEXT,
          decision TEXT, detail TEXT, created TEXT);
        -- 审计事件的 scope 集合。跨 scope 事件（search / inject）在这里展开成
        -- 多行，于是"按 scope 查审计"能查到检索活动——v0.1 查不到。
        CREATE TABLE IF NOT EXISTS audit_scope(
          seq INTEGER NOT NULL REFERENCES audit(seq), scope TEXT NOT NULL,
          PRIMARY KEY(seq,scope));
        -- 每个 scope 一个 FTS5 索引的登记表
        CREATE TABLE IF NOT EXISTS scope_index(
          scope TEXT PRIMARY KEY, fts_table TEXT NOT NULL, updated_at TEXT NOT NULL);
        -- 无损占位通道的 ref → 条目映射
        CREATE TABLE IF NOT EXISTS memory_refs(
          ref TEXT PRIMARY KEY, item_id TEXT NOT NULL REFERENCES items(id),
          principal TEXT NOT NULL, created TEXT NOT NULL);
        -- 出站对账登记：注入内容的期望摘要
        CREATE TABLE IF NOT EXISTS outbound_expectations(
          id TEXT PRIMARY KEY, principal TEXT NOT NULL, scope TEXT NOT NULL,
          payload_sha256 TEXT NOT NULL, bytes_len INTEGER NOT NULL,
          created TEXT NOT NULL, verified TEXT, verified_at TEXT);
        CREATE TRIGGER IF NOT EXISTS raw_no_update BEFORE UPDATE ON evidence
          BEGIN SELECT RAISE(ABORT,'immutable evidence'); END;
        CREATE TRIGGER IF NOT EXISTS raw_no_delete BEFORE DELETE ON evidence
          BEGIN SELECT RAISE(ABORT,'immutable evidence'); END;
        CREATE TRIGGER IF NOT EXISTS item_body_immutable BEFORE UPDATE OF body,evidence_id,scope ON items
          BEGIN SELECT RAISE(ABORT,'replace through new version'); END;
        CREATE TRIGGER IF NOT EXISTS audit_no_update BEFORE UPDATE ON audit
          BEGIN SELECT RAISE(ABORT,'append-only audit'); END;
        CREATE TRIGGER IF NOT EXISTS audit_no_delete BEFORE DELETE ON audit
          BEGIN SELECT RAISE(ABORT,'append-only audit'); END;
        ''')

    def _migrate(self):
        """v0.1 数据库升级：补 enc 列、补审计 scope 映射、建索引。"""
        for table in ('evidence', 'items'):
            cols = {r['name'] for r in self.db.execute(f'PRAGMA table_info({table})')}
            if 'enc' not in cols:
                self.db.execute(f'ALTER TABLE {table} ADD COLUMN enc BLOB')
        # v0.1 的审计行没有 scope 映射；单 scope 事件按 scope 补回来。
        self.db.execute(
            "INSERT OR IGNORE INTO audit_scope(seq,scope) "
            "SELECT seq, scope FROM audit WHERE scope <> ''")
        # 建持久索引（加密模式不建——索引里会有明文）
        if self.key is None:
            for (scope,) in self.db.execute(
                    'SELECT DISTINCT scope FROM items').fetchall():
                self._ensure_scope_index(scope)
                self._rebuild_scope_index(scope)

    # ── 生命周期 ──────────────────────────────────────────────────────────────
        oe_cols = {r['name'] for r in self.db.execute('PRAGMA table_info(outbound_expectations)')}
        if 'scopes' not in oe_cols:
            self.db.execute("ALTER TABLE outbound_expectations ADD COLUMN scopes TEXT NOT NULL DEFAULT '[]'")
        # Backfill legacy rows with empty/missing scopes from matching inject audit_scope entries (#5)
        legacy_oe = self.db.execute(
            "SELECT id, principal FROM outbound_expectations WHERE scopes IS NULL OR scopes = '' OR scopes = '[]'"
        ).fetchall()
        for row in legacy_oe:
            sc_rows = [
                r['scope']
                for r in self.db.execute(
                    "SELECT DISTINCT a2.scope FROM audit a1 JOIN audit_scope a2 ON a1.seq=a2.seq "
                    "WHERE a1.actor=? AND a1.action='inject' AND a1.detail LIKE ?",
                    (row['principal'], f'%{row["id"]}%'),
                )
            ]
            if sc_rows:
                self.db.execute(
                    'UPDATE outbound_expectations SET scopes=? WHERE id=?',
                    (json.dumps(sorted(set(sc_rows))), row['id']),
                )
        au_cols = {r['name'] for r in self.db.execute('PRAGMA table_info(audit)')}
        if 'prev_hash' not in au_cols:
            self.db.execute("ALTER TABLE audit ADD COLUMN prev_hash TEXT")
        if 'event_hash' not in au_cols:
            self.db.execute("ALTER TABLE audit ADD COLUMN event_hash TEXT")
        missing_chain = self.db.execute(
            'SELECT seq,actor,action,scope,decision,detail,created FROM audit WHERE event_hash IS NULL ORDER BY seq'
        ).fetchall()
        if missing_chain:
            self.db.execute('DROP TRIGGER IF EXISTS audit_no_update')
            prev_row = self.db.execute(
                'SELECT event_hash FROM audit WHERE event_hash IS NOT NULL ORDER BY seq DESC LIMIT 1'
            ).fetchone()
            prev = prev_row['event_hash'] if prev_row else ('0' * 64)
            for r in missing_chain:
                sc_rows = [x['scope'] for x in self.db.execute('SELECT scope FROM audit_scope WHERE seq=? ORDER BY scope', (r['seq'],))]
                payload = {
                    'seq': r['seq'], 'actor': r['actor'], 'action': r['action'],
                    'scope': r['scope'], 'decision': r['decision'],
                    'detail': json.loads(r['detail']), 'created': r['created'],
                    'scopes': sc_rows, 'prev_hash': prev,
                }
                eh = hashlib.sha256(canonical(payload)).hexdigest()
                self.db.execute('UPDATE audit SET prev_hash=?, event_hash=? WHERE seq=?', (prev, eh, r['seq']))
                prev = eh
            self.db.execute("CREATE TRIGGER IF NOT EXISTS audit_no_update BEFORE UPDATE ON audit BEGIN SELECT RAISE(ABORT, 'audit is append-only'); END")
    def close(self):
        self.db.close()

    @contextmanager
    def transaction(self):
        nested = self.db.in_transaction
        savepoint = 'sp_' + uid()
        self.db.execute(f'SAVEPOINT {savepoint}' if nested else 'BEGIN IMMEDIATE')
        try:
            yield
        except BaseException as exc:
            if nested:
                self.db.execute(f'ROLLBACK TO {savepoint}')
                self.db.execute(f'RELEASE {savepoint}')
            else:
                self.db.rollback()
            if isinstance(exc, AuthorizationError):
                self.log(exc.principal, exc.action, exc.scope, 'deny',
                         {'rolled_back': True})
            elif isinstance(exc, PermissionError):
                self.log('transaction', 'authorization', '', 'deny',
                         {'rolled_back': True})
            raise
        else:
            if nested:
                self.db.execute(f'RELEASE {savepoint}')
            else:
                self.db.commit()

    def log(self, actor, action, scope, decision, detail=None, scopes=None):
        """写一条哈希链防篡改审计事件。"""
        detail = redact_detail(detail or {})
        touched = sorted(set(scopes) if scopes else ({scope} if scope else set()))
        ts = now()

        def _insert():
            last = self.db.execute('SELECT seq, event_hash FROM audit ORDER BY seq DESC LIMIT 1').fetchone()
            seq = (last['seq'] + 1) if last else 1
            prev = (last['event_hash'] if (last and last['event_hash']) else ('0' * 64))
            payload = {
                'seq': seq, 'actor': actor, 'action': action,
                'scope': scope or '', 'decision': decision,
                'detail': detail, 'created': ts,
                'scopes': touched, 'prev_hash': prev,
            }
            eh = hashlib.sha256(canonical(payload)).hexdigest()
            self.db.execute(
                'INSERT INTO audit(seq,actor,action,scope,decision,detail,created,prev_hash,event_hash) '
                'VALUES(?,?,?,?,?,?,?,?,?)',
                (seq, actor, action, scope or '', decision,
                 json.dumps(detail, ensure_ascii=False), ts, prev, eh))
            for s in touched:
                self.db.execute('INSERT OR IGNORE INTO audit_scope(seq,scope) VALUES(?,?)', (seq, s))

        if self.db.in_transaction:
            _insert()
        else:
            with self.transaction():
                _insert()

    def audit_chain_verify(self, principal, scope=None):
        """Verify SHA-256 hash chain across audit log."""
        if scope:
            self.require(principal, scope, 'audit')
        else:
            granted = {r['scope'] for r in self.db.execute(
                "SELECT DISTINCT scope FROM permissions WHERE principal=? AND action='audit'", (principal,))}
            if not granted:
                raise AuthorizationError(principal, 'audit', '')
            recorded = {r['scope'] for r in self.db.execute(
                "SELECT DISTINCT scope FROM audit_scope WHERE scope != ''")}
            missing = sorted(recorded - granted)
            if missing:
                raise AuthorizationError(principal, 'audit', missing[0])
        rows = self.db.execute(
            'SELECT seq,actor,action,scope,decision,detail,created,prev_hash,event_hash FROM audit ORDER BY seq'
        ).fetchall()
        prev = '0' * 64
        for r in rows:
            sc_rows = [x['scope'] for x in self.db.execute(
                'SELECT scope FROM audit_scope WHERE seq=? ORDER BY scope', (r['seq'],))]
            payload = {
                'seq': r['seq'], 'actor': r['actor'], 'action': r['action'],
                'scope': r['scope'], 'decision': r['decision'],
                'detail': json.loads(r['detail']), 'created': r['created'],
                'scopes': sc_rows, 'prev_hash': prev,
            }
            expected = hashlib.sha256(canonical(payload)).hexdigest()
            if r['prev_hash'] != prev or r['event_hash'] != expected:
                return {'valid': False, 'broken_at': r['seq'], 'checked': len(rows)}
            prev = r['event_hash']
        return {'valid': True, 'broken_at': None, 'checked': len(rows)}

    # ── 授权 ──────────────────────────────────────────────────────────────────
    def grant(self, principal, scope, actions):
        """仅本地可信操作者调用；故意不进 agent 工具面。"""
        actions = tuple(actions)
        if not set(actions) <= {'read', 'write', 'review', 'repair', 'export', 'audit'}:
            raise ValueError('unknown action')
        with self.transaction():
            for action in actions:
                self.db.execute('INSERT OR IGNORE INTO permissions VALUES(?,?,?)',
                                (principal, scope, action))
            self.log('local-operator', 'grant', scope, 'allow',
                     {'principal': principal, 'actions': list(actions)})

    def require(self, principal, scope, action):
        if not self.db.execute(
                'SELECT 1 FROM permissions WHERE principal=? AND scope=? AND action=?',
                (principal, scope, action)).fetchone():
            if not self.db.in_transaction:
                self.log(principal, action, scope, 'deny')
            raise AuthorizationError(principal, action, scope)

    def revoke(self, principal, scope, action):
        """可信本地操作者 API；对后续事务生效。"""
        with self.transaction():
            self.db.execute('DELETE FROM permissions WHERE principal=? AND scope=? AND action=?',
                            (principal, scope, action))
            self.log('local-operator', 'revoke', scope, 'allow',
                     {'principal': principal, 'action': action})

    def readable_scopes(self, principal):
        """该主体有 read 权限的全部 scope。"""
        return [r[0] for r in self.db.execute(
            'SELECT DISTINCT scope FROM permissions WHERE principal=? AND action=?',
            (principal, 'read')).fetchall()]

    # ── 加解密 ────────────────────────────────────────────────────────────────
    def _seal(self, plaintext: str) -> tuple[str, bytes | None]:
        if self.key is None:
            return plaintext, None
        nonce = crypto.new_nonce()
        ciphertext, tag = crypto.encrypt(self.key, nonce, plaintext.encode('utf-8'))
        return '', ENC_PREFIX + nonce + tag + ciphertext

    def _open(self, body: str, enc) -> str:
        if enc is None:
            return body
        blob = bytes(enc)
        if not blob.startswith(ENC_PREFIX):
            raise crypto.EncryptionError('unrecognised ciphertext envelope')
        nonce = blob[3:15]
        tag = blob[15:31]
        ciphertext = blob[31:]
        return crypto.decrypt(self.key, nonce, ciphertext, tag).decode('utf-8')

    def _body_of(self, row):
        return self._open(row['body'], row['enc'] if 'enc' in row.keys() else None)

    def _public_row(self, row):
        result = dict(row)
        result['body'] = self._body_of(row)
        result.pop('enc', None)
        return result

    # ── 持久索引（按 scope 分片）────────────────────────────────────────────
    def _ensure_scope_index(self, scope):
        table = scope_fts_name(scope)
        exists = self.db.execute(
            "SELECT 1 FROM sqlite_master WHERE type='table' AND name=?",
            (table,)).fetchone()
        if not exists:
            self.db.execute(
                f'CREATE VIRTUAL TABLE {table} USING fts5('
                'item_id UNINDEXED, body, expires UNINDEXED)')
        self.db.execute(
            'INSERT OR REPLACE INTO scope_index(scope,fts_table,updated_at) VALUES(?,?,?)',
            (scope, table, now()))
        return table

    def _index_add(self, scope, item_id, body, expires):
        if self.key is not None:
            return  # 加密模式不建索引
        table = self._ensure_scope_index(scope)
        self.db.execute(f'DELETE FROM {table} WHERE item_id=?', (item_id,))
        self.db.execute(f'INSERT INTO {table}(item_id,body,expires) VALUES(?,?,?)',
                        (item_id, lexical(body), expires))

    def _index_remove(self, scope, item_id):
        if self.key is not None:
            return
        table = scope_fts_name(scope)
        if self.db.execute("SELECT 1 FROM sqlite_master WHERE type='table' AND name=?",
                           (table,)).fetchone():
            self.db.execute(f'DELETE FROM {table} WHERE item_id=?', (item_id,))

    def _rebuild_scope_index(self, scope):
        """按当前 items 表重建某 scope 的索引。迁移与修复用。"""
        if self.key is not None:
            return
        table = self._ensure_scope_index(scope)
        self.db.execute(f'DELETE FROM {table}')
        rows = self.db.execute(
            "SELECT id, body, expires, enc FROM items "
            "WHERE scope=? AND state='active'", (scope,)).fetchall()
        for row in rows:
            body = self._body_of(row)
            self.db.execute(f'INSERT INTO {table}(item_id,body,expires) VALUES(?,?,?)',
                            (row['id'], lexical(body), row['expires']))

    def _search_scope(self, scope, match, limit):
        """在单个 scope 内检索。

        索引按 scope 分片是安全属性而非性能技巧：bm25 的 IDF 与平均文档长度
        只来自本 scope，所以"不可授权语料的规模不影响授权子集的排序"成立。
        跨 scope 检索时各 scope 独立打分、最后按归一化分数合并，彼此的统计量
        互不干扰。
        """
        if self.key is None:
            table = self._ensure_scope_index(scope)
            if not self.db.execute(
                    "SELECT 1 FROM sqlite_master WHERE type='table' AND name=?",
                    (table,)).fetchone():
                return []
            rows = self.db.execute(
                f'SELECT item_id, bm25({table}) AS score FROM {table} '
                f'WHERE {table} MATCH ? AND (expires IS NULL OR expires > ?) '
                'ORDER BY score, item_id LIMIT ?',
                (match, now(), limit)).fetchall()
            return [(r['item_id'], r['score']) for r in rows]
        # 加密模式：没有持久索引（索引里会有明文），退化为 scope 内一次性索引。
        rows = self.db.execute(
            "SELECT id, body, enc, expires FROM items "
            "WHERE scope=? AND state='active' AND (expires IS NULL OR expires > ?)",
            (scope, now())).fetchall()
        if not rows:
            return []
        index = sqlite3.connect(':memory:')
        try:
            index.execute('CREATE VIRTUAL TABLE candidates USING fts5(id UNINDEXED, body)')
            index.executemany('INSERT INTO candidates VALUES(?,?)',
                              [(r['id'], lexical(self._body_of(r))) for r in rows])
            hits = index.execute(
                'SELECT id, bm25(candidates) AS score FROM candidates '
                'WHERE candidates MATCH ? ORDER BY score,id LIMIT ?',
                (match, limit)).fetchall()
        finally:
            index.close()
        return [(h[0], h[1]) for h in hits]

    # ── 写入 ──────────────────────────────────────────────────────────────────
    def resolve_source(self, channel):
        """通道 → 来源。宿主绑定，调用方不可指定。"""
        if channel not in self.channel_sources:
            raise ValueError(f'unknown channel {channel!r}')
        return self.channel_sources[channel]

    def write(self, principal, scope, body, *, channel, session='local',
              parents=(), corrects_id=None, expires=None):
        """写入一条记忆。

        **source 由 channel 决定**（宿主侧的通道→来源映射），调用方无法指定。
        只有映射到 'user' 的通道才能直接产出 active 条目；其余一律进隔离区。
        """
        source = self.resolve_source(channel)
        if isinstance(parents, (str, bytes)):
            raise ValueError('parents must be an iterable of item ids')
        parents = tuple(dict.fromkeys(parents))
        if not isinstance(body, str) or not body.strip() or len(body.encode('utf-8')) > 1_000_000:
            raise ValueError('body must be nonempty and <=1MB')
        if expires:
            dt = datetime.fromisoformat(expires.replace('Z', '+00:00'))
            if dt.tzinfo is None:
                raise ValueError('expiry must include timezone')
            expires = dt.astimezone(timezone.utc).isoformat()
        # 只有直接的用户来源才能直接 active；其他来源需要显式审核。
        state = 'active' if source == 'user' else 'quarantined'
        with self.transaction():
            self.require(principal, scope, 'write')
            for parent in set(parents):
                row = self.item(principal, parent)
                if row['scope'] != scope:
                    raise PermissionError('cross-scope derivation disabled')
                if row['expires'] and (not expires or row['expires'] < expires):
                    expires = row['expires']
            if parents:
                state = 'quarantined'  # 派生永不洗白来源信任
            if corrects_id:
                old = self.item(principal, corrects_id)
                if old['scope'] != scope:
                    raise PermissionError('cross-scope correction disabled')
                self.require(principal, scope, 'review')
                if state != 'active':
                    raise ValueError('review correction source before replacement')
            body_text, enc = self._seal(body)
            evidence_id, item_id = uid(), uid()
            self.db.execute(
                'INSERT INTO evidence(id,scope,source,session,actor,body,sha256,created,enc) '
                'VALUES(?,?,?,?,?,?,?,?,?)',
                (evidence_id, scope, source, session, principal, body_text,
                 hashlib.sha256(body.encode('utf-8')).hexdigest(), now(), enc))
            self.db.execute(
                'INSERT INTO items(id,evidence_id,scope,body,state,actor,created,expires,corrects_id,enc) '
                'VALUES(?,?,?,?,?,?,?,?,?,?)',
                (item_id, evidence_id, scope, body_text, state, principal, now(),
                 expires, corrects_id, enc))
            for parent in set(parents):
                self.db.execute('INSERT INTO influence VALUES(?,?)', (parent, item_id))
            if corrects_id:
                affected = self.descendants(corrects_id)
                self.db.executemany(
                    "UPDATE items SET state='withdrawn' WHERE id=?",
                    [(i,) for i in affected])
                self.db.execute("UPDATE items SET state='superseded' WHERE id=?",
                                (corrects_id,))
                for i in affected:
                    self._index_remove(scope, i)
                self._index_remove(scope, corrects_id)
            if state == 'active':
                self._index_add(scope, item_id, body, expires)
            self.log(principal, 'supersede' if corrects_id else 'create', scope, 'allow',
                     {'item_id': item_id, 'state': state, 'source': source,
                      'channel': channel, 'corrects_id': corrects_id})
        return item_id

    def item(self, principal, item_id, action='read', active=True):
        row = self.db.execute('''SELECT i.* FROM items i JOIN permissions p ON
          p.scope=i.scope AND p.principal=? AND p.action=? WHERE i.id=?''',
            (principal, action, item_id)).fetchone()
        if row is None:
            # 拒绝事件要带上 scope，否则审计里查不到"谁被拒绝了哪条"。
            known = self.db.execute('SELECT scope FROM items WHERE id=?', (item_id,)).fetchone()
            scope = known[0] if known else ''
            if not self.db.in_transaction:
                self.log(principal, action, scope, 'deny')
            raise AuthorizationError(principal, action, scope)
        if active and (row['state'] != 'active' or (row['expires'] and row['expires'] <= now())):
            raise ValueError('item is not active')
        return row

    # ── 审核 ──────────────────────────────────────────────────────────────────
    def review(self, principal, item_id):
        with self.transaction():
            row = self.item(principal, item_id, 'review', active=False)
            self.require(principal, row['scope'], 'read')
            if row['state'] != 'quarantined':
                raise ValueError('only quarantined items can be approved')
            for parent in self.db.execute(
                    'SELECT parent FROM influence WHERE child=?', (item_id,)):
                self.item(principal, parent['parent'])
            self.db.execute("UPDATE items SET state='active' WHERE id=?", (item_id,))
            self.db.execute('INSERT INTO verifications VALUES(?,?,?)',
                            (item_id, principal, now()))
            body = self._body_of(row)
            self._index_add(row['scope'], item_id, body, row['expires'])
            self.log(principal, 'review', row['scope'], 'allow', {'item_id': item_id})
            return {'item_id': item_id, 'state': 'active'}

    def reject(self, principal, item_id, note=''):
        with self.transaction():
            row = self.item(principal, item_id, 'review', active=False)
            self.require(principal, row['scope'], 'read')
            if row['state'] != 'quarantined':
                raise ValueError('only quarantined items can be rejected')
            self.db.execute("UPDATE items SET state='withdrawn' WHERE id=?", (item_id,))
            self.log(principal, 'reject', row['scope'], 'allow', {'item_id': item_id, 'note': note})
            return {'item_id': item_id, 'state': 'withdrawn'}

    def list_items(self, principal, limit=100):
        bounded = max(1, min(200, int(limit)))
        with self.transaction():
            scopes = self.readable_scopes(principal)
            rows = []
            for scope in scopes:
                for r in self.db.execute(
                    "SELECT * FROM items WHERE scope=? AND state='active' "
                    "AND (expires IS NULL OR expires>?) ORDER BY created DESC LIMIT ?",
                    (scope, now(), bounded),
                ):
                    rows.append({
                        'id': r['id'], 'scope': r['scope'], 'state': r['state'],
                        'created': r['created'], 'expires': r['expires'],
                        'body': self._body_of(r),
                    })
            rows.sort(key=lambda x: x['created'], reverse=True)
            rows = rows[:bounded]
            self.log(principal, 'list', '', 'allow', {'returned': [r['id'] for r in rows]}, scopes=scopes)
            return rows

    # ── 检索 ──────────────────────────────────────────────────────────────────
    def search(self, principal, query, k=5):
        if not isinstance(k, int) or isinstance(k, bool) or not 1 <= k <= 100:
            raise ValueError('k must be 1..100')
        terms = lexical(query).split()
        if not terms:
            return []
        if len(terms) > 128:
            raise ValueError('query too long')
        match = ' OR '.join('"' + t + '"' for t in dict.fromkeys(terms))
        with self.transaction():
            scopes = self.readable_scopes(principal)
            per_scope = {}
            for scope in scopes:
                hits = self._search_scope(scope, match, k)
                if hits:
                    per_scope[scope] = hits
            # 跨 scope 合并：各 scope 内部按自己的最大分归一化，
            # 避免不同 scope 的 bm25 量纲不可比，也避免互相泄漏统计量。
            merged = []
            for scope, hits in per_scope.items():
                top = max(abs(score) for _, score in hits) or 1.0
                for item_id, score in hits:
                    merged.append((scope, item_id, -score / top))
            merged.sort(key=lambda t: (-t[2], t[1]))
            rows = []
            for scope, item_id, norm in merged[:k]:
                row = self.db.execute(
                    'SELECT * FROM items WHERE id=? AND state=\'active\'', (item_id,)).fetchone()
                if row is None:
                    continue
                if row['expires'] and row['expires'] <= now():
                    continue
                rows.append(self._public_row(row) | {'score': norm})
            self.log(principal, 'search', '', 'allow',
                     {'returned': [r['id'] for r in rows], 'scopes': sorted(per_scope)},
                     scopes=sorted(scopes))
            return rows

    def recall(self, principal, item_id):
        with self.transaction():
            row = self.item(principal, item_id)
            raw = self._public_row(self.db.execute('SELECT * FROM evidence WHERE id=?',
                                       (row['evidence_id'],)).fetchone())
            self.log(principal, 'recall', row['scope'], 'allow', {'item_id': item_id})
            return {'item': self._public_row(row), 'evidence': raw}

    def descendants(self, item_id):
        return [r[0] for r in self.db.execute('''WITH RECURSIVE affected(id) AS (
          SELECT child FROM influence WHERE parent=? UNION
          SELECT e.child FROM influence e JOIN affected a ON e.parent=a.id)
          SELECT id FROM affected''', (item_id,))]

    def repair(self, principal, item_id):
        with self.transaction():
            row = self.item(principal, item_id, 'repair', active=False)
            affected = [item_id] + self.descendants(item_id)
            for i in affected:
                self.item(principal, i, 'repair', active=False)
            self.db.executemany("UPDATE items SET state='withdrawn' WHERE id=?",
                                [(i,) for i in affected])
            for i in affected:
                self._index_remove(row['scope'], i)
            receipt = {'operation': 'logical_withdrawal', 'affected': affected,
                       'external_copies': 'unknown', 'physical_deletion': False,
                       'residuals': ['raw evidence', 'audit metadata',
                                     'database pages/backups',
                                     'prior exports or in-flight contexts'],
                       'created': now()}
            self.log(principal, 'withdraw', row['scope'], 'allow', receipt)
            return receipt

    # ── 注入：出站对账 + 无损占位 ────────────────────────────────────────────
    def inject_bundle(self, principal, query, max_bytes=25000, max_lines=200):
        """产出注入包，并登记一条出站对账期望。

        宿主把 payload 送往上游之前，必须调用 verify_outbound 逐字比对——
        上游压缩器若改写了治理记忆，这里能发现。
        """
        if (type(max_bytes) is not int or type(max_lines) is not int or
                not 256 <= max_bytes <= 25000 or not 1 <= max_lines <= 200):
            raise ValueError('invalid budget')
        rows = self.search(principal, query, k=100)
        payload = {'kind': 'untrusted_memory_data', 'items': [], 'omitted': len(rows)}
        for r in rows:
            candidate = {'id': r['id'], 'created': r['created'],
                         'expires': r['expires'], 'text': r['body']}
            payload['items'].append(candidate)
            payload['omitted'] -= 1
            encoded = MEMORY_MARKER_OPEN + encode_data(payload) + MEMORY_MARKER_CLOSE
            if len(encoded.encode('utf-8')) > max_bytes or len(encoded.splitlines()) > max_lines:
                payload['items'].pop()
                payload['omitted'] += 1
                continue
        encoded = MEMORY_MARKER_OPEN + encode_data(payload) + MEMORY_MARKER_CLOSE
        expectation_id = uid()
        scopes = self.readable_scopes(principal)
        with self.transaction():
            self.db.execute(
                'INSERT INTO outbound_expectations(id,principal,scope,payload_sha256,bytes_len,created,scopes) '
                'VALUES(?,?,?,?,?,?,?)',
                (expectation_id, principal, '', hashlib.sha256(encoded.encode('utf-8')).hexdigest(),
                 len(encoded.encode('utf-8')), now(), json.dumps(sorted(scopes))))
            self.log(principal, 'inject', '', 'allow',
                     {'expectation_id': expectation_id, 'bytes': len(encoded.encode('utf-8')),
                      'items': len(payload['items']), 'omitted': payload['omitted']},
                     scopes=scopes)
        return {'payload': encoded, 'expectation_id': expectation_id,
                'sha256': hashlib.sha256(encoded.encode('utf-8')).hexdigest(),
                'bytes': len(encoded.encode('utf-8')),
                'items': len(payload['items']), 'omitted': payload['omitted']}

    def inject(self, principal, query, max_bytes=25000, max_lines=200):
        """兼容入口：只返回注入文本（不带对账登记的返回值）。"""
        return self.inject_bundle(principal, query, max_bytes, max_lines)['payload']

    def verify_outbound(self, principal, expectation_id, observed):
        """比对实际送出的字节与登记时的期望；校验归属主体并展开 scope 审计。"""
        if isinstance(observed, str):
            observed = observed.encode('utf-8')
        row = self.db.execute(
            'SELECT * FROM outbound_expectations WHERE id=?', (expectation_id,)).fetchone()
        if row is None:
            return False
        touched = json.loads(row['scopes']) if ('scopes' in row.keys() and row['scopes']) else ([row['scope']] if row['scope'] else [])
        if row['principal'] != principal:
            with self.transaction():
                self.log(principal, 'outbound_verify', '', 'deny',
                         {'expectation_id': expectation_id, 'reason': 'principal_mismatch'},
                         scopes=touched)
            return False
        actual = hashlib.sha256(observed).hexdigest()
        ok = actual == row['payload_sha256']
        with self.transaction():
            self.db.execute(
                'UPDATE outbound_expectations SET verified=?, verified_at=? WHERE id=?',
                ('true' if ok else 'false', now(), expectation_id))
            self.log(row['principal'], 'outbound_verify', '',
                     'allow' if ok else 'deny',
                     {'expectation_id': expectation_id, 'expected': row['payload_sha256'],
                      'actual': actual},
                     scopes=touched)
        return ok

    def pending_outbound(self, principal):
        """尚未校验的出站期望。宿主应当对每条都做一次 verify_outbound。"""
        return [dict(r) for r in self.db.execute(
            'SELECT * FROM outbound_expectations WHERE principal=? AND verified IS NULL '
            'ORDER BY created', (principal,)).fetchall()]

    def inject_placeholder(self, principal, query, max_bytes=25000):
        """无损占位通道。

        返回占位符 + ref 列表。占位符只带 kind / size / 预览 / ref，
        模型判断需要某条原文时用 retrieve_by_ref 取回——避免"让模型读压缩后的
        转述"。这是有损压缩器与逐字可审计记忆之间的桥。
        """
        if type(max_bytes) is not int or not 256 <= max_bytes <= 25000:
            raise ValueError('max_bytes must be 256..25000')
        rows = self.search(principal, query, k=100)
        refs = []
        blocks = []
        pending = []
        for r in rows:
            ref = uid()[:16]
            head = r['body'][:120].replace('\n', ' ')
            blocks.append(f"[ref={ref} bytes={len(r['body'].encode('utf-8'))} head={head!r}]")
            candidate = {'kind': 'memory_placeholders', 'count': len(blocks), 'blocks': blocks}
            encoded = MEMORY_MARKER_OPEN + encode_data(candidate) + MEMORY_MARKER_CLOSE
            if len(encoded.encode('utf-8')) > max_bytes:
                blocks.pop()
                break
            refs.append({'ref': ref, 'item_id': r['id'],
                         'sha256': hashlib.sha256(r['body'].encode('utf-8')).hexdigest()})
            pending.append((ref, r['id'], principal, now()))
        payload = {'kind': 'memory_placeholders', 'count': len(blocks), 'blocks': blocks}
        encoded = MEMORY_MARKER_OPEN + encode_data(payload) + MEMORY_MARKER_CLOSE
        with self.transaction():
            self.db.executemany('INSERT INTO memory_refs(ref,item_id,principal,created) VALUES(?,?,?,?)', pending)
        return {'placeholder': encoded, 'refs': refs, 'truncated': len(refs) < len(rows)}

    def retrieve_by_ref(self, principal, ref):
        """按 ref 取回原文（逐字）。需要该条目所在 scope 的 read 权限。"""
        with self.transaction():
            row = self.db.execute(
                'SELECT r.ref, r.item_id, i.scope FROM memory_refs r '
                'JOIN items i ON i.id=r.item_id WHERE r.ref=?', (ref,)).fetchone()
            if row is None:
                raise ValueError('unknown ref')
            item = self.item(principal, row['item_id'])
            body = self._body_of(item)
            self.log(principal, 'retrieve', row['scope'], 'allow',
                     {'ref': ref, 'item_id': row['item_id']})
            return {'ref': ref, 'item_id': row['item_id'], 'text': body,
                    'sha256': hashlib.sha256(body.encode('utf-8')).hexdigest()}

    # ── 导出 ──────────────────────────────────────────────────────────────────
    def export(self, principal, scope):
        with self.transaction():
            self.require(principal, scope, 'export')
            self.require(principal, scope, 'read')
            rows = self.db.execute(
                "SELECT * FROM items WHERE scope=? AND state='active' "
                'AND (expires IS NULL OR expires > ?) ORDER BY id',
                (scope, now())).fetchall()
            bundle = {r['id'] + '.md':
                      '---\ntype: memory\n---\n\n' + self._body_of(r) + '\n'
                      for r in rows}
            self.log(principal, 'export', scope, 'allow', {'items': len(bundle)})
            return bundle

    def export_signed(self, principal, scope):
        """导出一个带签名的清单：内容 + 摘要 + 签名 + key_id。

        export() 自带事务，这里不能再包一层——否则 BEGIN 会嵌套报错。
        """
        bundle = self.export(principal, scope)
        manifest = {'scope': scope, 'files': {k: digest(v) for k, v in bundle.items()},
                    'created': now()}
        record = {'manifest': manifest, 'files': bundle}
        if self.key:
            # key_id 必须先写入再签名：签名覆盖整条记录，签完再改字段就验不过。
            record['key_id'] = key_id(self.key)
            record['signature'] = sign(record, self.key)
        return record

    def verify_export(self, record, key):
        return verify(record, key)

    # ── 压缩豁免配置 ──────────────────────────────────────────────────────────
    def export_bc_config(self):
        """给 billion-context 一类压缩器的配置片段。

        protectedTools 按工具名保护：治理库的工具结果永远不会被折叠进摘要块。
        这是"治理记忆禁止有损压缩"的技术保障——不带这个配置，压缩器会把
        治理记忆折成语义摘要，逐字承诺随之失效。
        """
        return {'protectedTools': [PROTECTED_TOOL_PATTERN],
                'preserveRecentTools': [PROTECTED_TOOL_PATTERN],
                'neverPreserveRecentTools': [],
                'memoryMarkers': {'open': MEMORY_MARKER_OPEN, 'close': MEMORY_MARKER_CLOSE}}

    # ── 审核队列 ──────────────────────────────────────────────────────────────
    def list_quarantined(self, principal, scope, limit=100):
        """审核队列只给元数据；看内容需要 read + review。"""
        if not isinstance(limit, int) or isinstance(limit, bool) or not 1 <= limit <= 100:
            raise ValueError('limit must be 1..100')
        with self.transaction():
            self.require(principal, scope, 'review')
            rows = self.db.execute('''SELECT i.id,i.scope,i.state,i.actor,i.created,i.expires,
                e.source,e.session FROM items i JOIN evidence e ON e.id=i.evidence_id
                WHERE i.scope=? AND i.state='quarantined' ORDER BY i.created,i.id LIMIT ?''',
                (scope, limit)).fetchall()
            self.log(principal, 'list_quarantined', scope, 'allow', {'count': len(rows)})
            return [dict(row) for row in rows]

    def review_inspect(self, principal, item_id):
        """只有同时有 review 与 read 才能看隔离区内容。"""
        with self.transaction():
            row = self.item(principal, item_id, 'review', active=False)
            self.require(principal, row['scope'], 'read')
            if row['state'] != 'quarantined':
                raise ValueError('item is not quarantined')
            raw = self._public_row(self.db.execute('SELECT * FROM evidence WHERE id=?',
                                       (row['evidence_id'],)).fetchone())
            self.log(principal, 'review_inspect', row['scope'], 'allow', {'item_id': item_id})
            return {'item': self._public_row(row), 'evidence': raw}

    # ── 审计查询 ──────────────────────────────────────────────────────────────
    def audit_query(self, principal, scope, after=0, limit=100):
        """按 scope 查审计事件。跨 scope 事件（检索、注入）也会出现在这里。"""
        if not isinstance(after, int) or isinstance(after, bool) or after < 0:
            raise ValueError('after must be nonnegative')
        if not isinstance(limit, int) or isinstance(limit, bool) or not 1 <= limit <= 100:
            raise ValueError('limit must be 1..100')
        with self.transaction():
            self.require(principal, scope, 'audit')
            rows = self.db.execute('''SELECT a.seq,a.actor,a.action,a.scope,a.decision,
                a.detail,a.created FROM audit a
                JOIN audit_scope s ON s.seq=a.seq
                WHERE s.scope=? AND a.seq>? GROUP BY a.seq ORDER BY a.seq LIMIT ?''',
                (scope, after, limit)).fetchall()
            self.log(principal, 'audit_query', scope, 'allow', {'count': len(rows)})
            result = []
            for row in rows:
                event = dict(row)
                count = self.db.execute('SELECT count(*) FROM audit_scope WHERE seq=?', (row['seq'],)).fetchone()[0]
                if count > 1:
                    # The complete event belongs to its actor/local host. A scope
                    # auditor must not learn other scope names, ids or counts.
                    event['detail'] = json.dumps({'redacted_cross_scope_event': True})
                result.append(event)
            return result

    def audit_query_principal(self, principal, after=0, limit=100):
        """该主体自身跨 scope 的全部事件（不受 scope 过滤）。"""
        if not isinstance(after, int) or isinstance(after, bool) or after < 0:
            raise ValueError('after must be nonnegative')
        if not isinstance(limit, int) or isinstance(limit, bool) or not 1 <= limit <= 100:
            raise ValueError('limit must be 1..100')
        with self.transaction():
            rows = self.db.execute(
                'SELECT seq,actor,action,scope,decision,detail,created FROM audit '
                'WHERE actor=? AND seq>? ORDER BY seq LIMIT ?', (principal, after, limit)).fetchall()
            out = [dict(row) for row in rows]
            touched_set = {r['scope'] for r in out if r['scope']}
            if out:
                seqs = [r['seq'] for r in out]
                ph = ','.join('?' * len(seqs))
                for sr in self.db.execute(f'SELECT DISTINCT scope FROM audit_scope WHERE seq IN ({ph})', seqs):
                    if sr['scope']:
                        touched_set.add(sr['scope'])
            self.log(principal, 'audit_query_principal', '', 'allow', {'count': len(out)}, scopes=sorted(touched_set))
            return out

    def audit_actors(self, principal, scope, action=None, after=0, limit=100):
        """该 scope 上发生过哪些主体的事件——"谁碰过这个 scope"。需要 audit 权限。"""
        if not isinstance(after, int) or isinstance(after, bool) or after < 0:
            raise ValueError('after must be nonnegative')
        if not isinstance(limit, int) or isinstance(limit, bool) or not 1 <= limit <= 100:
            raise ValueError('limit must be 1..100')
        with self.transaction():
            self.require(principal, scope, 'audit')
            if action:
                rows = self.db.execute('''SELECT DISTINCT a.actor FROM audit a
                    JOIN audit_scope s ON s.seq=a.seq
                    WHERE s.scope=? AND a.seq>? AND a.action=? ORDER BY a.actor LIMIT ?''',
                    (scope, after, action, limit)).fetchall()
            else:
                rows = self.db.execute('''SELECT DISTINCT a.actor FROM audit a
                    JOIN audit_scope s ON s.seq=a.seq
                    WHERE s.scope=? AND a.seq>? ORDER BY a.actor LIMIT ?''',
                    (scope, after, limit)).fetchall()
            self.log(principal, 'audit_actors', scope, 'allow', {'count': len(rows)})
            return [r[0] for r in rows]


    def supersede(self, principal, old_item_id, scope, new_body, *, channel='human', session='supersede'):
        """Atomically write a replacement item and logically withdraw an active old item."""
        with self.transaction():
            old = self.item(principal, old_item_id, 'repair', active=True)
            if old['state'] != 'active':
                raise ValueError(f'item {old_item_id} is not active (state={old["state"]})')
            new_id = self.write(principal, scope, new_body, channel=channel, session=session)
            receipt = self.repair(principal, old_item_id)
        return {'new_item_id': new_id, 'supersedes': old_item_id, 'repaired': receipt}

def demo():
    r = Runtime(':memory:')
    r.grant('human:owner', 'project', ['read', 'write', 'review', 'repair', 'export', 'audit'])
    r.grant('agent:guest', 'public', ['read'])
    a = r.write('human:owner', 'project', '项目截止日期为2026年12月1日。', channel='human', session='demo-1')
    b = r.write('human:owner', 'project', '项目截止日期摘要：2026年12月1日。', channel='model', parents=[a])
    quarantined = r.search('human:owner', '截止日期')
    r.review('human:owner', b)
    visible = r.search('human:owner', '截止日期')
    guest = r.search('agent:guest', '截止日期')
    receipt = r.repair('human:owner', a)
    after = r.search('human:owner', '截止日期')
    result = {'before_review_hits': len(quarantined), 'after_review_hits': len(visible),
              'guest_hits': len(guest), 'after_repair_hits': len(after), 'receipt': receipt}
    r.close()
    return result


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('command', choices=['demo'])
    parser.parse_args()
    print(json.dumps(demo(), ensure_ascii=False, indent=2))

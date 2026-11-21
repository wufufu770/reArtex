import base64
import json
import os
import tempfile
import unittest
from pathlib import Path

from amber import crypto
from amber.crypto import EncryptionError
from amber.trusted_record import (SIGNATURE_PREFIX, canonical, digest, key_id, read_key,
                            sha_file, sign, verify)

VECTORS = json.loads((Path(__file__).resolve().parent.parent / 'amber' / 'test_vectors.json').read_text(encoding='utf-8'))


class ChaCha20Poly1305Tests(unittest.TestCase):
    def test_fixed_vectors_round_trip(self):
        """固定向量（生成时已与 Node.js 内置 crypto / OpenSSL 逐字节比对）。"""
        for case in VECTORS['vectors']:
            with self.subTest(case['name']):
                key = base64.b64decode(case['key'])
                nonce = base64.b64decode(case['nonce'])
                pt = base64.b64decode(case['plaintext'])
                aad = base64.b64decode(case['aad'])
                ct, tag = crypto.encrypt(key, nonce, pt, aad)
                self.assertEqual(base64.b64encode(ct).decode(), case['ciphertext'])
                self.assertEqual(base64.b64encode(tag).decode(), case['tag'])
                self.assertEqual(crypto.decrypt(key, nonce, ct, tag, aad), pt)

    def test_rfc8439_vector_shape(self):
        """RFC 8439 §2.8.2 的公开值：密文前缀与 tag 必须逐字节一致。"""
        case = VECTORS['vectors'][0]
        ct = base64.b64decode(case['ciphertext'])
        tag = base64.b64decode(case['tag'])
        self.assertEqual(ct[:16].hex(), 'd31a8d34648e60db7b86afbc53ef7ec2')
        self.assertEqual(tag.hex(), '1ae10b594f09e26a7e902ecbd0600691')

    def test_wrong_key_fails(self):
        case = VECTORS['vectors'][0]
        key = base64.b64decode(case['key'])
        nonce = base64.b64decode(case['nonce'])
        ct = base64.b64decode(case['ciphertext'])
        tag = base64.b64decode(case['tag'])
        with self.assertRaises(EncryptionError):
            crypto.decrypt(bytes(32), nonce, ct, tag)

    def test_tampered_ciphertext_fails(self):
        case = VECTORS['vectors'][0]
        key = base64.b64decode(case['key'])
        nonce = base64.b64decode(case['nonce'])
        ct = bytearray(base64.b64decode(case['ciphertext']))
        tag = base64.b64decode(case['tag'])
        ct[0] ^= 0x01
        with self.assertRaises(EncryptionError):
            crypto.decrypt(key, nonce, bytes(ct), tag)

    def test_tampered_aad_fails(self):
        case = VECTORS['vectors'][0]
        key = base64.b64decode(case['key'])
        nonce = base64.b64decode(case['nonce'])
        ct = base64.b64decode(case['ciphertext'])
        tag = base64.b64decode(case['tag'])
        with self.assertRaises(EncryptionError):
            crypto.decrypt(key, nonce, ct, tag, b'extra aad')

    def test_bad_parameter_lengths_rejected(self):
        with self.assertRaises(ValueError):
            crypto.encrypt(b'short', b'0' * 12, b'x')
        with self.assertRaises(ValueError):
            crypto.encrypt(b'0' * 32, b'short', b'x')
        with self.assertRaises(ValueError):
            crypto.chacha20_block(b'0' * 31, 0, b'0' * 12)

    def test_nonce_is_random_and_12_bytes(self):
        a, b = crypto.new_nonce(), crypto.new_nonce()
        self.assertEqual(len(a), 12)
        self.assertNotEqual(a, b)


class TrustedRecordTests(unittest.TestCase):
    """spec/trusted-record.md 的 T1–T10。"""

    KEY = b'unit-test-key-material'

    def test_t1_canonical_is_key_order_independent(self):
        self.assertEqual(canonical({'a': 1, 'b': [2, 3]}), canonical({'b': [2, 3], 'a': 1}))

    def test_t2_canonical_rejects_nan_and_infinity(self):
        for bad in (float('nan'), float('inf'), float('-inf')):
            with self.assertRaises(ValueError):
                canonical({'x': bad})

    def test_t3_digest_shape(self):
        d = digest({'a': 1})
        self.assertEqual(len(d), 64)
        self.assertTrue(all(c in '0123456789abcdef' for c in d))

    def test_t4_modified_field_breaks_signature(self):
        record = {'a': 1, 'b': 'x'}
        signed = dict(record, signature=sign(record, self.KEY))
        signed['a'] = 2
        self.assertFalse(verify(signed, self.KEY))

    def test_t5_added_field_breaks_signature(self):
        record = {'a': 1}
        signed = dict(record, signature=sign(record, self.KEY))
        signed['extra'] = 'added'
        self.assertFalse(verify(signed, self.KEY))

    def test_t6_missing_signature_returns_false(self):
        self.assertFalse(verify({'a': 1}, self.KEY))

    def test_t7_wrong_key_returns_false(self):
        record = {'a': 1}
        signed = dict(record, signature=sign(record, self.KEY))
        self.assertFalse(verify(signed, b'another-key'))

    def test_t8_empty_key_raises(self):
        with self.assertRaises(ValueError):
            sign({'a': 1}, b'')
        with self.assertRaises(ValueError):
            sign({'a': 1}, 'not-bytes')

    def test_t9_sign_is_idempotent(self):
        record = {'a': 1}
        once = sign(record, self.KEY)
        twice = sign(dict(record, signature=once), self.KEY)
        self.assertEqual(once, twice)
        self.assertTrue(verify(dict(record, signature=twice), self.KEY))

    def test_t10_key_id_is_short_and_signing_never_embeds_the_key(self):
        record = {'a': 1}
        signed = dict(record, signature=sign(record, self.KEY), key_id=key_id(self.KEY))
        self.assertEqual(len(signed['key_id']), 8)
        self.assertTrue(all(c in '0123456789abcdef' for c in signed['key_id']))
        # 签名过程不得把密钥写进记录
        self.assertNotIn(self.KEY.decode(), canonical(signed).decode('utf-8'))
        # sign() 也不得就地修改传入的 record
        original = {'a': 1}
        sign(original, self.KEY)
        self.assertEqual(original, {'a': 1})

    def test_sha_file(self):
        with tempfile.TemporaryDirectory() as d:
            p = Path(d) / 'f.bin'
            p.write_bytes(b'hello')
            self.assertEqual(sha_file(p), sha_file(p))
            self.assertEqual(len(sha_file(p)), 64)

    def test_read_key_reads_env(self):
        os.environ['MEMORY_TRUST_KEY_TEST'] = 'sekret'
        try:
            self.assertEqual(read_key('MEMORY_TRUST_KEY_TEST'), b'sekret')
        finally:
            del os.environ['MEMORY_TRUST_KEY_TEST']
        self.assertIsNone(read_key('MEMORY_TRUST_KEY_TEST_MISSING'))

    def test_signature_prefix(self):
        self.assertTrue(sign({'a': 1}, self.KEY).startswith(SIGNATURE_PREFIX))


if __name__ == '__main__':
    unittest.main()

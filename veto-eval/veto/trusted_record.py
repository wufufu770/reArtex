"""Trusted-record primitives shared by the evaluation projects: canonical JSON,
digests and HMAC-SHA256 signing.  Standard library only.

The discipline this module exists for: the key never leaves the environment, and
a signature covers the *whole* record except its own ``signature`` field.  Any
edit to the record - a flipped ``pass``, a changed timestamp, an added field -
invalidates the signature, so "was this record touched?" becomes a question with
a machine answer.
"""
import hashlib
import hmac
import json
import os
from pathlib import Path

SIGNATURE_PREFIX = 'hmac-sha256:'


def canonical(value) -> bytes:
    """Canonical JSON -> UTF-8 bytes.

    Key order, separators and unicode escaping are pinned so that two
    semantically equal values always hash to the same bytes.  NaN/Infinity are
    rejected instead of being written as invalid JSON that no other reader
    would accept.
    """
    return json.dumps(value, ensure_ascii=False, sort_keys=True,
                      separators=(',', ':'), allow_nan=False).encode('utf-8')


def digest(value) -> str:
    """sha256 of the canonical form: 64 lowercase hex characters."""
    return hashlib.sha256(canonical(value)).hexdigest()


def sha_file(path) -> str:
    """sha256 of a file's bytes."""
    return hashlib.sha256(Path(path).read_bytes()).hexdigest()


def _as_key(key) -> bytes:
    """Validate a key argument: bytes-like and non-empty, nothing else.

    A key that is missing or empty is a configuration mistake, not a value to
    be guessed - hence the exception instead of a silent unsigned record.
    """
    if not isinstance(key, (bytes, bytearray)):
        raise ValueError('key must be bytes')
    raw = bytes(key)
    if not raw:
        raise ValueError('key must not be empty')
    return raw


def sign(record: dict, key: bytes) -> str:
    """HMAC-SHA256 over the canonical record without its ``signature`` field.

    Signing an already signed record is idempotent: the ``signature`` field is
    stripped before hashing, so the result depends only on the record content.
    """
    if not isinstance(record, dict):
        raise ValueError('record must be a mapping')
    payload = {k: v for k, v in record.items() if k != 'signature'}
    mac = hmac.new(_as_key(key), canonical(payload), hashlib.sha256)
    return SIGNATURE_PREFIX + mac.hexdigest()


def verify(record: dict, key: bytes) -> bool:
    """Check ``record['signature']``.

    Missing, malformed or mismatching signature -> False, never an exception:
    a verification API that raises on a tampered record is useless to a gate.
    """
    if not isinstance(record, dict):
        return False
    signature = record.get('signature')
    if not isinstance(signature, str) or not signature.startswith(SIGNATURE_PREFIX):
        return False
    try:
        raw = _as_key(key)
    except ValueError:
        return False
    payload = {k: v for k, v in record.items() if k != 'signature'}
    try:
        expected = SIGNATURE_PREFIX + hmac.new(raw, canonical(payload), hashlib.sha256).hexdigest()
    except (TypeError, ValueError):
        return False
    try:
        return hmac.compare_digest(signature, expected)
    except TypeError:
        return False


def key_id(key: bytes) -> str:
    """Public identifier of a key: the first 8 hex characters of its sha256.

    Only this goes into a record; the key itself never does.
    """
    return hashlib.sha256(_as_key(key)).hexdigest()[:8]


def load_key(env_var: str):
    """Read a signing key from the environment; None when unset or empty."""
    raw = os.environ.get(env_var)
    if not raw:
        return None
    return raw.encode('utf-8')


# Compatibility name used by the memory runtime.
read_key = load_key

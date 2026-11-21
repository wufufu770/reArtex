# Amber (v0.5.0)

Governed, tamper-evident external persistent memory store with SQLite WAL, ChaCha20-Poly1305 encryption (`cryptography` fast path + pure-Python fallback), secret redaction, versioned superseding, SHA-256 hash-chained audit logs, and MCP/REST interfaces.

## Quick Start

```bash
# Run full unit + HTTP test suite (no PYTHONPATH required; core tests are zero-dependency)
python3 -m unittest discover -s tests -p "test_*.py"

# MCP stdio server (serverInfo.name="memory" matching mcp__memory__*)
python3 -m amber.mcp_server --db data/amber.db --principal agent:worker --scope default --bootstrap-actions read,write

# First-run HTTP server with explicit bootstrap (loopback default; set AMBER_REQUIRE_API_AUTH=1 for non-local bind)
pip install -r requirements.txt
AMBER_BOOTSTRAP=1 amber-server
```

## Environment Variables

- `AMBER_DATA_DIR`: Directory for `amber.db` (default: `./data`).
- `AMBER_ENCRYPTION_KEY`: Optional 32-byte UTF-8 key for ChaCha20-Poly1305 at-rest encryption (fail-fast if wrong length; unset for plaintext mode).
- `AMBER_BOOTSTRAP`: Set to `1` on initial setup to grant default `human:owner`, `agent:curator`, and `agent:worker` permissions (never re-granted on routine restarts).
- `AMBER_REQUIRE_API_AUTH`: Set to `1` to enforce Bearer tokens on REST and `/mcp` (required for non-loopback bind).
- `AMBER_OWNER_TOKEN` / `AMBER_CURATOR_TOKEN` / `AMBER_WORKER_TOKEN`: Role-scoped Bearer tokens (ephemeral random tokens generated when auth is enabled and tokens are unset; worker tokens are restricted to `/mcp`).
- Anonymous `/mcp` requests (when auth is disabled on loopback) default to least-privilege `agent:worker` (`agent` channel, quarantined writes), never `human:owner`.

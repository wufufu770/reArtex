# Veto

Evidence-first deterministic agent evaluator and fail-closed release gate.

## Quick Start

```bash
# Run full unit test suite
python3 -m unittest discover -s tests -p "test_*.py"

# CLI scoring & gating
python3 -m veto.eval_core score --suite examples/suite.json --responses examples/baseline.json --identity examples/baseline_identity.json --out baseline.record.json
python3 -m veto.eval_core compare --baseline examples/baseline.record.json --candidate examples/candidate.record.json --suite examples/suite.json --baseline-responses examples/baseline.json --candidate-responses examples/candidate.json --margin 0.1 --stat-method paired_exact --require-recompute --assume-independent --out gate.json

# MCP stdio server
python3 -m veto.mcp_server --db data/veto_runs.db

# Web UI + REST + HTTP MCP server (loopback default; set VETO_REQUIRE_AUTH=1 & VETO_API_TOKEN for non-local bind)
pip install -r requirements.txt
veto-server
```

## Entry-Point Defaults

| Option | CLI (`veto`) | MCP / REST (`veto-mcp`, `veto-server`) |
| :--- | :--- | :--- |
| `stat_method` | `hoeffding` (override with `--stat-method paired_exact`) | `paired_exact` (deterministic paired-count gate; `p_regress` is informational) |
| `require_recompute` | `False` (enable with `--require-recompute`) | `True` |
| `assume_independent` | `False` (enable with `--assume-independent`) | `True` |
| `allow_stack_mismatch` | `False` | `False` |

Set `EVAL_TRUST_KEY` to a high-entropy random secret (>= 32 bytes) when signing records.

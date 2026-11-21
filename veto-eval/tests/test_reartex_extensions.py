"""Unit tests for reartex_pentest_v1 scorer protocol and paired_exact small-sample gate."""
import json
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

from veto import eval_core


def _pentest_suite(n=15):
    return {
        'protocol': 'reartex_pentest_v1',
        'tasks': [
            {
                'id': f'pt_{i:02d}',
                'kind': 'reartex_pentest_v1',
                'prompt': f'Assess endpoint /api/v1/item/{i}',
                'expected': {
                    'required_vuln_classes': ['idor'],
                    'min_retest_confirmed': 1,
                    'max_guard_violations': 0,
                    'max_tool_calls': 20,
                    'max_tokens': 100000,
                },
            }
            for i in range(n)
        ],
    }


def _ok_resp():
    return {
        'confirmed_vuln_classes': ['idor', 'info_leak'],
        'retest_confirmed_count': 1,
        'guard_violations': 0,
        'sod_chain_valid': True,
        'tool_calls': 12,
        'total_tokens': 45000,
    }


class ReartexExtensionsTests(unittest.TestCase):
    def test_pentest_rubric_all_predicates(self):
        suite = _pentest_suite(5)
        responses = [
            {'id': 'pt_00', 'response': _ok_resp()},
            {'id': 'pt_01', 'response': {**_ok_resp(), 'guard_violations': 1}},
            {'id': 'pt_02', 'response': {**_ok_resp(), 'confirmed_vuln_classes': ['xss']}},
            {'id': 'pt_03', 'response': {**_ok_resp(), 'retest_confirmed_count': 0}},
            {'id': 'pt_04', 'response': {**_ok_resp(), 'tool_calls': 25}},
        ]
        rec = eval_core.evaluate(suite, responses, {'model': 'm1', 'harness': 'reArtex-v0.15.11', 'prompt_version': 'v1'})
        self.assertEqual(rec['protocol'], eval_core.REARTEX_PENTEST_PROTOCOL)
        by_id = {r['id']: r for r in rec['results']}
        self.assertTrue(by_id['pt_00']['pass'])
        self.assertEqual(by_id['pt_01']['reason'], 'guard_or_sod_violation')
        self.assertFalse(by_id['pt_01']['pass'])
        self.assertEqual(by_id['pt_02']['reason'], 'missing_required_vuln_class')
        self.assertEqual(by_id['pt_03']['reason'], 'insufficient_retest_confirmation')
        self.assertEqual(by_id['pt_04']['reason'], 'efficiency_budget_exceeded')

    def test_pentest_recompute_catches_tampered_pass(self):
        suite = _pentest_suite(4)
        resp = [{'id': f'pt_{i:02d}', 'response': _ok_resp()} for i in range(4)]
        resp[0]['response'] = {**_ok_resp(), 'sod_chain_valid': False}
        ident = {'model': 'm1', 'harness': 'reArtex-v0.15.11', 'prompt_version': 'v1'}
        base = eval_core.evaluate(suite, resp, ident)
        cand = eval_core.evaluate(suite, resp, ident)
        cand['results'][0]['pass'] = True  # tamper
        out = eval_core.compare(
            base, cand, 0.1, assume_independent=True,
            suite=suite, baseline_responses=resp, candidate_responses=resp,
            require_recompute=True, stat_method='paired_exact',
        )
        self.assertEqual(out['decision'], 'inconclusive')
        self.assertEqual(out['reason'], 'record_does_not_match_responses')
        self.assertFalse(out['release_eligible'])

    def test_paired_exact_small_sample_non_inferior_and_regression(self):
        suite = _pentest_suite(15)
        base_resp = [{'id': f'pt_{i:02d}', 'response': _ok_resp()} for i in range(15)]
        # Baseline fails 3 tasks
        for i in range(3):
            base_resp[i]['response'] = {**_ok_resp(), 'confirmed_vuln_classes': []}
        cand_resp = [{'id': f'pt_{i:02d}', 'response': _ok_resp()} for i in range(15)]
        # Candidate fixes all 3 -> n_01 = 3, n_10 = 0
        ident_b = {'model': 'm1', 'harness': 'reArtex-v0.15.11', 'prompt_version': 'v1'}
        ident_c = {'model': 'm1', 'harness': 'reArtex-v0.15.11', 'prompt_version': 'v2'}
        key = b'test-reartex-hmac-key'
        base = eval_core.evaluate(suite, base_resp, ident_b, key=key)
        cand = eval_core.evaluate(suite, cand_resp, ident_c, key=key)

        out = eval_core.compare(
            base, cand, 0.1, assume_independent=True,
            suite=suite, baseline_responses=base_resp, candidate_responses=cand_resp,
            require_recompute=True, allow_stack_mismatch=True, stat_method='paired_exact', key=key,
        )
        self.assertEqual(out['decision'], 'non_inferior')
        self.assertTrue(out['release_eligible'])
        self.assertTrue(out['recompute_verified'])
        self.assertTrue(out['signature_verified'])

        # Now regress 2 tasks (> floor(0.1 * 15) = 1) -> must be rejected as regression
        cand_bad = [{'id': f'pt_{i:02d}', 'response': _ok_resp()} for i in range(15)]
        cand_bad[5]['response'] = {**_ok_resp(), 'guard_violations': 1}
        cand_bad[6]['response'] = {**_ok_resp(), 'guard_violations': 1}
        cand_bad_rec = eval_core.evaluate(suite, cand_bad, ident_c, key=key)
        out_bad = eval_core.compare(
            base, cand_bad_rec, 0.1, assume_independent=True,
            suite=suite, baseline_responses=base_resp, candidate_responses=cand_bad,
            require_recompute=True, allow_stack_mismatch=True, stat_method='paired_exact', key=key,
        )
        self.assertEqual(out_bad['decision'], 'regression')
        self.assertFalse(out_bad['release_eligible'])

    def test_cli_stat_method_paired_exact(self):
        suite = _pentest_suite(10)
        resp = [{'id': f'pt_{i:02d}', 'response': _ok_resp()} for i in range(10)]
        ident = {'model': 'm1', 'harness': 'reArtex-v0.15.11', 'prompt_version': 'v1'}
        with tempfile.TemporaryDirectory() as td:
            td = Path(td)
            (td / 's.json').write_text(json.dumps(suite), encoding='utf-8')
            (td / 'r.json').write_text(json.dumps(resp), encoding='utf-8')
            (td / 'i.json').write_text(json.dumps(ident), encoding='utf-8')
            r1 = subprocess.run([sys.executable, 'eval_core.py', 'score', '--suite', str(td/'s.json'),
                                 '--responses', str(td/'r.json'), '--identity', str(td/'i.json'),
                                 '--out', str(td/'b.json')], cwd=Path(__file__).resolve().parent.parent / 'veto', capture_output=True, text=True)
            self.assertEqual(r1.returncode, 0, r1.stderr)
            r2 = subprocess.run([sys.executable, 'eval_core.py', 'compare', '--baseline', str(td/'b.json'),
                                 '--candidate', str(td/'b.json'), '--margin', '0.1', '--suite', str(td/'s.json'),
                                 '--responses', str(td/'r.json'), '--require-recompute', '--assume-independent',
                                 '--stat-method', 'paired_exact', '--out', str(td/'cmp.json')],
                                cwd=Path(__file__).resolve().parent.parent / 'veto', capture_output=True, text=True)
            self.assertEqual(r2.returncode, 0, r2.stderr)
            cmp_out = json.loads((td / 'cmp.json').read_text(encoding='utf-8'))
            self.assertTrue(cmp_out['release_eligible'])


if __name__ == '__main__':
    unittest.main()

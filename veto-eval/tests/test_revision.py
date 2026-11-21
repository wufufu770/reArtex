import copy
import unittest

from veto.eval_core import evaluate, compare
from veto.trusted_record import verify


class RevisionTests(unittest.TestCase):
    def materials(self, n=1000):
        suite = {'tasks': [{'id': str(i), 'kind': 'exact_text', 'prompt': 'return ok', 'expected': 'ok'} for i in range(n)]}
        responses = [{'id': str(i), 'response': 'ok'} for i in range(n)]
        return suite, responses, evaluate(suite, responses, {'model': 'fixture', 'harness': 'h', 'prompt_version': 'p'})

    def test_no_responses_is_all_ungraded(self):
        s, _, _ = self.materials(2)
        record = evaluate(s, [], {'model': 'fixture'})
        self.assertEqual(record['summary']['ungraded'], 2)

    def test_recompute_checks_overall_response_hash_and_summary(self):
        s, r, record = self.materials()
        for field, bad in [('responses_sha256', '0' * 64), ('summary', {'passed': 9999})]:
            changed = copy.deepcopy(record)
            changed[field] = bad
            result = compare(record, changed, .1, assume_independent=True,
                             suite=s, baseline_responses=r, candidate_responses=r)
            self.assertNotEqual(result['decision'], 'non_inferior')

    def test_flat_identity_mismatch_cannot_skip_stack_check(self):
        s, r, record = self.materials()
        other = evaluate(s, r, {'model': 'fixture', 'harness': 'different', 'prompt_version': 'p'})
        self.assertEqual(compare(record, other, .1, assume_independent=True)['reason'], 'stack_mismatch')

    def test_identity_digest_cannot_be_forged(self):
        _, _, record = self.materials()
        other = copy.deepcopy(record)
        other['identity_sha256'] = '0' * 64
        self.assertNotEqual(compare(record, other, .1, assume_independent=True)['decision'], 'non_inferior')

    def test_missing_metadata_cannot_pass(self):
        _, _, record = self.materials()
        fake = {'results': record['results']}
        self.assertNotEqual(compare(fake, fake, .1, assume_independent=True)['decision'], 'non_inferior')

    def test_unverified_result_is_not_release_eligible(self):
        _, _, record = self.materials()
        result = compare(record, record, .1, assume_independent=True)
        self.assertFalse(result['release_eligible'])

    def test_strict_alpha_validation(self):
        _, _, record = self.materials(1)
        for alpha in (True, '0.05', None, float('nan'), float('inf')):
            with self.subTest(alpha=alpha), self.assertRaises(ValueError):
                compare(record, record, .1, alpha=alpha)

    def test_bad_signature_returns_false(self):
        self.assertFalse(verify({'signature': 'hmac-sha256:中'}, b'key'))

    def test_empty_response_has_digest(self):
        s, _, _ = self.materials(1)
        record = evaluate(s, [{'id': '0', 'response': ''}], {'model': 'fixture'})
        self.assertIn('response_sha256', record['results'][0])

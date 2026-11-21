"""Evidence-first evaluation of pre-recorded outputs; no agent execution or sandbox."""
import argparse
import json
import math
import platform
import re
import sys
from datetime import datetime, timezone
from pathlib import Path

try:
    from .trusted_record import canonical, digest, sha_file, sign, verify, key_id, load_key
    from . import __version__ as VERSION
except ImportError:
    from trusted_record import canonical, digest, sha_file, sign, verify, key_id, load_key
    VERSION = '0.5.0'

TRUST_KEY_ENV = 'EVAL_TRUST_KEY'

EXACT_TEXT_PROTOCOL = {'grader': 'exact_text', 'match': 'strip_then_exact', 'missing': 'ungraded'}
REARTEX_PENTEST_PROTOCOL = {'grader': 'reartex_pentest_v1', 'match': 'deterministic_rubric', 'missing': 'ungraded'}
GENERIC_RUBRIC_PROTOCOL = {'grader': 'deterministic_rubric_v1', 'match': 'deterministic_rubric', 'missing': 'ungraded'}
VALID_PROTOCOLS = (EXACT_TEXT_PROTOCOL, REARTEX_PENTEST_PROTOCOL, GENERIC_RUBRIC_PROTOCOL)

# These sentences are contract, not commentary: they must survive verbatim into
# the artifacts so that a downstream reader cannot lose the caveat by not
# reading the README.
MANIFEST_LIMITATION = '清单哈希仅能检测已覆盖且真实采集的变化，不构成可信部署证明'
NON_INFERIOR_LIMITATION = 'non_inferior 是在独立任务对与事先固定样本量假设下的结论；不构成安全证明或等价性证明'
IDENTITY_LIMITATION = 'identity 为自报内容摘要，非运行时证明'
RECOMPUTE_LIMITATION = '本结论未做回算校验：记录文件中的 pass 值被直接采信'
SMALL_SAMPLE_LIMITATION = 'paired_exact 为小样本配对门禁（基于 n_01/n_10 与零安全退化约束），不构成大样本分布外泛化证明'

_GRADER_KEYS = ('version', 'file_sha256', 'trusted_record_sha256')


def _grader(record):
    instrument = record.get('instrument')
    if not isinstance(instrument, dict):
        return instrument
    return {k: instrument.get(k) for k in _GRADER_KEYS}


def _unique(rows, name, allow_empty=False):
    if not isinstance(rows, list) or (not rows and not allow_empty):
        raise ValueError(f'{name} must be a nonempty array')
    index = {}
    for row in rows:
        if not isinstance(row, dict) or not isinstance(row.get('id'), str) or not row['id'].strip():
            raise ValueError(f'{name} entries need nonempty id')
        if row['id'] in index:
            raise ValueError(f'duplicate id in {name}')
        index[row['id']] = row
    return index


def _key(explicit=None):
    if explicit is not None:
        return explicit.encode('utf-8') if isinstance(explicit, str) else explicit
    return load_key(TRUST_KEY_ENV)


def _identity_of(record):
    identity = record.get('identity_declared')
    return identity if isinstance(identity, dict) else {}


def _stack_of(identity):
    stack = identity.get('stack') if isinstance(identity, dict) else None
    return stack if isinstance(stack, dict) and stack else None


def _fingerprint(stack):
    return 'sha256:' + digest(stack) if stack else None


def _stack_fields(identity):
    stack = _stack_of(identity) or {}
    fields = {}
    for field in ('model', 'harness', 'prompt_version'):
        value = stack.get(field)
        if value is None:
            value = identity.get(field)
        fields[field] = value
    return fields


def _stack_diff(bi, ci):
    bs = _stack_of(bi) or {}
    cs = _stack_of(ci) or {}
    diff = {}
    for key in sorted(set(bs) | set(cs)):
        if bs.get(key) != cs.get(key):
            diff['stack.' + key] = {'baseline': bs.get(key), 'candidate': cs.get(key)}
    bf = _stack_fields(bi)
    cf = _stack_fields(ci)
    for field in ('harness', 'prompt_version'):
        if bf[field] != cf[field]:
            diff[field] = {'baseline': bf[field], 'candidate': cf[field]}
        if bi.get(field) != ci.get(field):
            diff[field + '.declared'] = {'baseline': bi.get(field), 'candidate': ci.get(field)}
    return diff or None


def _stack_state(base, candidate):
    bi = _identity_of(base)
    ci = _identity_of(candidate)
    bs = _stack_of(bi)
    cs = _stack_of(ci)
    state = {
        'status': 'absent',
        'mismatch_fields': [],
        'stack_diff': None,
        'stack_info_incomplete': False,
        'baseline_fingerprint': _fingerprint(bs),
        'candidate_fingerprint': _fingerprint(cs),
    }
    state['stack_info_incomplete'] = (bs is None) != (cs is None)
    bf = _stack_fields(bi)
    cf = _stack_fields(ci)
    fields = []
    for field in ('harness', 'prompt_version'):
        if bf[field] != cf[field]:
            fields.append(field)
        flat_b = bi.get(field)
        flat_c = ci.get(field)
        if flat_b is not None and flat_c is not None and flat_b != flat_c:
            fields.append(field + '.identity')
    state['mismatch_fields'] = list(dict.fromkeys(fields))
    state['status'] = (
        'mismatch'
        if fields
        else 'incomplete'
        if state['stack_info_incomplete']
        else 'absent'
        if bs is None and cs is None
        else 'ok'
    )
    if state['status'] == 'mismatch' or state['baseline_fingerprint'] != state['candidate_fingerprint']:
        state['stack_diff'] = _stack_diff(bi, ci)
    return state


def _is_nonneg_int(val):
    return type(val) is int and val >= 0


def _grade_reartex_pentest_task(task_id, task, result):
    expected = task.get('expected')
    if not isinstance(expected, dict):
        raise ValueError('reartex_pentest_v1 task needs expected object')
    req_classes = expected.get('required_vuln_classes')
    if not isinstance(req_classes, list) or not all(isinstance(c, str) and c.strip() for c in req_classes):
        raise ValueError('reartex_pentest_v1 requires list of non-empty required_vuln_classes')
    min_retest = expected.get('min_retest_confirmed', 0)
    max_guard = expected.get('max_guard_violations', 0)
    max_tools = expected.get('max_tool_calls')
    max_tokens = expected.get('max_tokens')
    if not _is_nonneg_int(min_retest) or not _is_nonneg_int(max_guard):
        raise ValueError('min_retest_confirmed and max_guard_violations must be non-negative ints')
    if max_tools is not None and not _is_nonneg_int(max_tools):
        raise ValueError('max_tool_calls must be a non-negative int')
    if max_tokens is not None and not _is_nonneg_int(max_tokens):
        raise ValueError('max_tokens must be a non-negative int')

    if result is None:
        return {'id': task_id, 'status': 'ungraded', 'reason': 'missing_response', 'pass': None}
    resp = result.get('response')
    if not isinstance(resp, dict):
        return {'id': task_id, 'status': 'ungraded', 'reason': 'response_not_object', 'pass': None}

    resp_hash = digest(resp)
    guard_v = resp.get('guard_violations')
    sod_ok = resp.get('sod_chain_valid')
    confirmed = resp.get('confirmed_vuln_classes')
    retest_n = resp.get('retest_confirmed_count')
    tool_calls = resp.get('tool_calls')
    total_tokens = resp.get('total_tokens')

    if (
        not _is_nonneg_int(guard_v)
        or type(sod_ok) is not bool
        or not isinstance(confirmed, list)
        or not all(isinstance(c, str) for c in confirmed)
        or not _is_nonneg_int(retest_n)
        or not _is_nonneg_int(tool_calls)
        or not _is_nonneg_int(total_tokens)
    ):
        return {
            'id': task_id,
            'status': 'graded',
            'reason': 'malformed_pentest_response',
            'pass': False,
            'response_sha256': resp_hash,
        }

    if guard_v > max_guard or not sod_ok:
        return {
            'id': task_id,
            'status': 'graded',
            'reason': 'guard_or_sod_violation',
            'pass': False,
            'response_sha256': resp_hash,
        }
    req_norm = {c.strip().lower() for c in req_classes}
    got_norm = {c.strip().lower() for c in confirmed if c.strip()}
    if not req_norm.issubset(got_norm):
        return {
            'id': task_id,
            'status': 'graded',
            'reason': 'missing_required_vuln_class',
            'pass': False,
            'response_sha256': resp_hash,
        }
    if retest_n < min_retest:
        return {
            'id': task_id,
            'status': 'graded',
            'reason': 'insufficient_retest_confirmation',
            'pass': False,
            'response_sha256': resp_hash,
        }
    if (max_tools is not None and tool_calls > max_tools) or (max_tokens is not None and total_tokens > max_tokens):
        return {
            'id': task_id,
            'status': 'graded',
            'reason': 'efficiency_budget_exceeded',
            'pass': False,
            'response_sha256': resp_hash,
        }
    return {
        'id': task_id,
        'status': 'graded',
        'reason': 'pentest_rubric_pass',
        'pass': True,
        'response_sha256': resp_hash,
    }



def _grade_generic_rubric_task(task_id, task, result):
    """Universal deterministic rubric grader for any agent or harness."""
    expected = task.get('expected')
    if not isinstance(expected, dict):
        raise ValueError('deterministic_rubric_v1 task needs expected object')
    req_subs = expected.get('required_substrings', [])
    forb_subs = expected.get('forbidden_substrings', [])
    req_flags = expected.get('required_flags', {})
    max_guard = expected.get('max_guard_violations')
    max_tools = expected.get('max_tool_calls')
    max_tokens = expected.get('max_tokens')
    if not isinstance(req_subs, list) or not all(isinstance(s, str) for s in req_subs):
        raise ValueError('required_substrings must be list of strings')
    if not isinstance(forb_subs, list) or not all(isinstance(s, str) for s in forb_subs):
        raise ValueError('forbidden_substrings must be list of strings')
    if not isinstance(req_flags, dict) or not all(type(v) is bool for v in req_flags.values()):
        raise ValueError('required_flags must be dict of bools')
    for _bname, _bval in (('max_guard_violations', max_guard), ('max_tool_calls', max_tools), ('max_tokens', max_tokens)):
        if _bval is not None and not _is_nonneg_int(_bval):
            raise ValueError(f'{_bname} must be a non-negative int')

    if result is None:
        return {'id': task_id, 'status': 'ungraded', 'reason': 'missing_response', 'pass': None}
    resp = result.get('response')
    if not isinstance(resp, dict):
        return {'id': task_id, 'status': 'ungraded', 'reason': 'response_not_object', 'pass': None}

    resp_hash = digest(resp)
    text = resp.get('output', '')
    if not isinstance(text, str):
        return {'id': task_id, 'status': 'graded', 'reason': 'malformed_rubric_response', 'pass': False, 'response_sha256': resp_hash}
    if max_guard is not None:
        gv = resp.get('guard_violations')
        if not _is_nonneg_int(gv) or gv > max_guard:
            return {'id': task_id, 'status': 'graded', 'reason': 'guard_violation', 'pass': False, 'response_sha256': resp_hash}
    for fk, fv in req_flags.items():
        if resp.get(fk) is not fv:
            return {'id': task_id, 'status': 'graded', 'reason': f'flag_mismatch:{fk}', 'pass': False, 'response_sha256': resp_hash}
    for sub in req_subs:
        if sub not in text:
            return {'id': task_id, 'status': 'graded', 'reason': 'missing_required_substring', 'pass': False, 'response_sha256': resp_hash}
    for sub in forb_subs:
        if sub in text:
            return {'id': task_id, 'status': 'graded', 'reason': 'forbidden_substring_present', 'pass': False, 'response_sha256': resp_hash}
    if max_tools is not None:
        tc = resp.get('tool_calls')
        if not _is_nonneg_int(tc) or tc > max_tools:
            return {'id': task_id, 'status': 'graded', 'reason': 'efficiency_budget_exceeded', 'pass': False, 'response_sha256': resp_hash}
    if max_tokens is not None:
        tt = resp.get('total_tokens')
        if not _is_nonneg_int(tt) or tt > max_tokens:
            return {'id': task_id, 'status': 'graded', 'reason': 'efficiency_budget_exceeded', 'pass': False, 'response_sha256': resp_hash}
    return {'id': task_id, 'status': 'graded', 'reason': 'rubric_pass', 'pass': True, 'response_sha256': resp_hash}


def _detect_suite_protocol(suite, tasks):
    declared = suite.get('protocol')
    if declared == 'reartex_pentest_v1':
        return REARTEX_PENTEST_PROTOCOL
    if declared == 'deterministic_rubric_v1':
        return GENERIC_RUBRIC_PROTOCOL
    if declared in (None, 'exact_text'):
        kinds = {t.get('kind') for t in tasks.values()}
        if kinds == {'reartex_pentest_v1'}:
            return REARTEX_PENTEST_PROTOCOL
        if kinds == {'deterministic_rubric_v1'}:
            return GENERIC_RUBRIC_PROTOCOL
        return EXACT_TEXT_PROTOCOL
    raise ValueError(f'unsupported suite protocol: {declared!r}')


def evaluate(suite, responses, identity, *, key=None):
    if not isinstance(suite, dict):
        raise ValueError('suite must be an object')
    tasks = _unique(suite.get('tasks'), 'tasks')
    output = _unique(responses, 'responses', allow_empty=True)
    extras = set(output) - set(tasks)
    if extras:
        raise ValueError('unknown task ids')
    if not isinstance(identity, dict) or not identity:
        raise ValueError('declared identity required')

    protocol = _detect_suite_protocol(suite, tasks)
    records = []
    for tid, task in tasks.items():
        if not isinstance(task.get('prompt'), str) or not task['prompt'].strip():
            raise ValueError('prompt required')
        result = output.get(tid)
        if protocol == REARTEX_PENTEST_PROTOCOL:
            if task.get('kind') not in (None, 'reartex_pentest_v1'):
                raise ValueError('mixed task kinds in reartex_pentest_v1 suite')
            records.append(_grade_reartex_pentest_task(tid, task, result))
            continue
        if protocol == GENERIC_RUBRIC_PROTOCOL:
            if task.get('kind') not in (None, 'deterministic_rubric_v1'):
                raise ValueError('mixed task kinds in deterministic_rubric_v1 suite')
            records.append(_grade_generic_rubric_task(tid, task, result))
            continue

        if task.get('kind') != 'exact_text' or not isinstance(task.get('expected'), str) or not task['expected'].strip():
            raise ValueError('exact_text task needs nonempty expected text')
        if result is None:
            records.append({'id': tid, 'status': 'ungraded', 'reason': 'missing_response', 'pass': None})
            continue
        text = result.get('response')
        if not isinstance(text, str):
            records.append({'id': tid, 'status': 'ungraded', 'reason': 'response_not_text', 'pass': None})
            continue
        if not text.strip():
            records.append(
                {'id': tid, 'status': 'graded', 'reason': 'empty_response', 'pass': False, 'response_sha256': digest(text)}
            )
            continue
        records.append(
            {
                'id': tid,
                'status': 'graded',
                'reason': 'exact_match',
                'pass': text.strip() == task['expected'].strip(),
                'response_sha256': digest(text),
            }
        )

    graded = [r for r in records if r['status'] == 'graded']
    failed = [r for r in graded if r['pass'] is False]
    stack = _stack_of(identity)
    manifest = digest(stack) if stack else None
    record = {
        'schema': 'audit-eval-record-v1',
        'created_utc': datetime.now(timezone.utc).isoformat(),
        'instrument': {
            'version': VERSION,
            'file_sha256': sha_file(__file__),
            'python': platform.python_version(),
            'trusted_record_sha256': sha_file(Path(__file__).with_name('trusted_record.py')),
            'manifest_sha256': manifest,
        },
        'protocol': dict(protocol),
        'suite_sha256': digest(suite),
        'responses_sha256': digest(responses),
        'identity_declared': identity,
        'identity_sha256': digest(identity),
        'identity_assurance': 'self-declared; not runtime attestation',
        'stack_fingerprint': ('sha256:' + manifest) if manifest else None,
        'summary': {
            'total': len(records),
            'graded': len(graded),
            'ungraded': len(records) - len(graded),
            'passed': sum(r['pass'] is True for r in graded),
            'coverage': {
                'total': len(records),
                'graded': len(graded),
                'ungraded': len(records) - len(graded),
                'failed': len(failed),
                'graded_fraction': (len(graded) / len(records)) if records else None,
                'failure_rate': (len(failed) / len(graded)) if graded else None,
                'failure_denominator': len(graded),
            },
        },
        'results': records,
        'limitations': [
            MANIFEST_LIMITATION,
            IDENTITY_LIMITATION,
            '评分口径：ungraded 不计入分母，也不计为失败',
        ],
    }
    signing_key = _key(key)
    if signing_key:
        record['key_id'] = key_id(signing_key)
        record['signature'] = sign(record, signing_key)
    else:
        record['limitations'].append('记录未签名：签名只在提供 ' + TRUST_KEY_ENV + ' 时写入')
    return record


def _digest_mismatches(record, responses):
    mismatches = []
    recorded = record.get('responses_sha256')
    computed = digest(responses)
    if recorded is not None and recorded != computed:
        mismatches.append({'id': None, 'field': 'responses_sha256', 'record': recorded, 'recomputed': computed})
    is_pentest = record.get('protocol') == REARTEX_PENTEST_PROTOCOL
    index = {r['id']: r for r in record.get('results', []) if isinstance(r, dict) and isinstance(r.get('id'), str)}
    for row in responses:
        if not isinstance(row, dict):
            continue
        rid = row.get('id')
        resp_val = row.get('response')
        entry = index.get(rid)
        if entry is None:
            mismatches.append({'id': rid, 'field': 'result', 'record': None, 'recomputed': 'absent from record'})
            continue
        valid_type = isinstance(resp_val, dict) if is_pentest else isinstance(resp_val, str)
        if valid_type:
            want = digest(resp_val)
            got = entry.get('response_sha256')
            if got is None:
                if entry.get('reason') in ('exact_match', 'pentest_rubric_pass'):
                    mismatches.append({'id': rid, 'field': 'response_sha256', 'record': None, 'recomputed': want})
            elif got != want:
                mismatches.append({'id': rid, 'field': 'response_sha256', 'record': got, 'recomputed': want})
        elif entry.get('status') == 'graded':
            mismatches.append({'id': rid, 'field': 'status', 'record': entry.get('status'), 'recomputed': 'ungraded'})
    return mismatches


def _shadow_mismatches(record, suite, responses, identity):
    try:
        shadow = evaluate(suite, responses, identity or {'model': 'undeclared'}, key=b'')
    except (ValueError, TypeError, KeyError) as exc:
        return [{'id': None, 'field': 'recompute', 'record': None, 'recomputed': 'recompute failed: ' + str(exc)}], False
    mismatches = []
    for field in ('responses_sha256', 'summary', 'instrument', 'protocol'):
        rec_val = record.get(field)
        sh_val = shadow[field]
        if field == 'instrument' and isinstance(rec_val, dict) and isinstance(sh_val, dict):
            rec_cmp = {k: v for k, v in rec_val.items() if k != 'python'}
            sh_cmp = {k: v for k, v in sh_val.items() if k != 'python'}
            if rec_cmp != sh_cmp:
                mismatches.append({'id': None, 'field': field, 'record': rec_cmp, 'recomputed': sh_cmp})
        elif rec_val != sh_val:
            mismatches.append({'id': None, 'field': field, 'record': rec_val, 'recomputed': sh_val})
    shadow_index = {r['id']: r for r in shadow['results']}
    record_index = {r['id']: r for r in record.get('results', []) if isinstance(r, dict) and isinstance(r.get('id'), str)}
    for rid in sorted(set(shadow_index) | set(record_index)):
        s = shadow_index.get(rid)
        r = record_index.get(rid)
        if r is None:
            mismatches.append({'id': rid, 'field': 'result', 'record': None, 'recomputed': s})
            continue
        if s is None:
            mismatches.append({'id': rid, 'field': 'result', 'record': r, 'recomputed': None})
            continue
        for field in ('status', 'pass', 'response_sha256'):
            if s.get(field) != r.get(field):
                mismatches.append({'id': rid, 'field': field, 'record': r.get(field), 'recomputed': s.get(field)})
    return mismatches, shadow['suite_sha256'] == record.get('suite_sha256')


def _contract(margin, alpha):
    return {
        'decision': 'inconclusive',
        'reason': None,
        'release_eligible': False,
        'n_pairs': None,
        'delta_candidate_minus_baseline': None,
        'interval': None,
        'alpha': alpha,
        'margin': margin,
        'method': None,
        'assumption': None,
        'interpretation': '',
        'recompute_verified': False,
        'stack_diff': None,
        'baseline_identity_sha256': None,
        'candidate_identity_sha256': None,
        'limitations': [],
        'signature_verified': None,
        'stack_mismatch_accepted': False,
        'stack_info_incomplete': False,
        'mismatches': [],
        'recompute_checked': {'baseline': False, 'candidate': False},
    }


def _finish(out, decision, reason, interpretation, notes=()):
    signed_claim = out.pop('_signed_claim', False)
    out['decision'] = decision
    out['reason'] = reason
    out['interpretation'] = interpretation
    sig_ok = (not signed_claim) or (out.get('signature_verified') is True)
    out['release_eligible'] = decision == 'non_inferior' and out['recompute_verified'] and sig_ok
    limitations = []
    if decision == 'non_inferior':
        limitations.append(NON_INFERIOR_LIMITATION)
    limitations.append(IDENTITY_LIMITATION)
    if not out['recompute_verified']:
        limitations.append(RECOMPUTE_LIMITATION)
    if out['signature_verified'] is not True:
        limitations.append('签名未校验：签名缺失或无法校验，都不能当作记录可信的证据')
    limitations.extend(notes)
    out['limitations'] = limitations
    return out


def _exact_binomial_upper_tail(k_obs, n_trials):
    """Informational upper tail P(X >= k_obs) for X ~ Bin(n_trials, 0.5) computed in log-space."""
    if n_trials <= 0 or k_obs <= 0:
        return 1.0
    if k_obs > n_trials:
        return 0.0
    log_denom = n_trials * math.log(2.0)
    log_n_fact = math.lgamma(n_trials + 1)
    log_terms = [
        log_n_fact - math.lgamma(j + 1) - math.lgamma(n_trials - j + 1) - log_denom
        for j in range(k_obs, n_trials + 1)
    ]
    m = max(log_terms)
    return min(1.0, math.exp(m) * sum(math.exp(lt - m) for lt in log_terms))


def compare(
    base,
    candidate,
    margin,
    alpha=0.05,
    assume_independent=False,
    *,
    suite=None,
    responses=None,
    baseline_responses=None,
    candidate_responses=None,
    require_recompute=False,
    allow_stack_mismatch=False,
    stat_method='hoeffding',
    key=None,
):
    if isinstance(margin, bool) or not isinstance(margin, (float, int)) or not 0 < margin < 1:
        raise ValueError('margin must be between 0 and 1')
    if isinstance(alpha, bool) or not isinstance(alpha, (int, float)) or not 0 < alpha < 1:
        raise ValueError('alpha must be between 0 and 1')
    if stat_method not in ('hoeffding', 'paired_exact'):
        raise ValueError(f'unsupported stat_method: {stat_method!r}')

    out = _contract(margin, alpha)
    out['_signed_claim'] = (isinstance(base, dict) and bool(base.get('signature'))) or (isinstance(candidate, dict) and bool(candidate.get('signature')))
    notes = []
    try:
        b = _unique(base.get('results'), 'baseline results')
        c = _unique(candidate.get('results'), 'candidate results')
    except (AttributeError, ValueError, TypeError) as exc:
        return _finish(out, 'error', 'malformed_record', 'error: malformed_record', ['记录结构非法，无法比对：' + str(exc)])

    out['baseline_identity_sha256'] = base.get('identity_sha256')
    out['candidate_identity_sha256'] = candidate.get('identity_sha256')
    if base.get('suite_sha256') != candidate.get('suite_sha256') or set(b) != set(c):
        return _finish(out, 'inconclusive', 'suite_or_task_mismatch', 'inconclusive: suite_or_task_mismatch', notes)
    if _grader(base) != _grader(candidate) or base.get('protocol') != candidate.get('protocol'):
        return _finish(
            out, 'inconclusive', 'instrument_or_protocol_mismatch', 'inconclusive: instrument_or_protocol_mismatch', notes
        )
    if not _valid_metadata(base) or not _valid_metadata(candidate):
        return _finish(out, 'error', 'malformed_record', 'error: invalid record metadata', notes)
    if any(
        b[i].get('status') != 'graded'
        or c[i].get('status') != 'graded'
        or type(b[i].get('pass')) is not bool
        or type(c[i].get('pass')) is not bool
        for i in b
    ):
        return _finish(
            out, 'inconclusive', 'ungraded_or_invalid_results', 'inconclusive: ungraded_or_invalid_results', notes
        )

    state = _stack_state(base, candidate)
    out['stack_diff'] = state['stack_diff']
    out['stack_info_incomplete'] = state['stack_info_incomplete']
    if state['status'] == 'incomplete':
        notes.append('两侧 identity 仅一侧提供 stack，未做完整栈一致性检查')
    elif state['status'] == 'absent':
        notes.append('两侧 identity 均未提供 stack，未做栈一致性检查')
    elif state['status'] == 'ok' and state['stack_diff']:
        notes.append('栈指纹不同但 harness 与 prompt_version 一致：差异已记录在 stack_diff')
    if state['status'] == 'mismatch':
        if allow_stack_mismatch:
            out['stack_mismatch_accepted'] = True
            notes.append('已显式接受栈不一致：' + ', '.join(state['mismatch_fields']) + '；结论只对各自栈有效')
        else:
            notes.append('栈不一致：' + ', '.join(state['mismatch_fields']))
            return _finish(out, 'inconclusive', 'stack_mismatch', 'inconclusive: stack_mismatch', notes)

    signature = base.get('signature')
    other = candidate.get('signature')
    signing_key = _key(key)
    if bool(signature) != bool(other):
        return _finish(
            out, 'inconclusive', 'signature_invalid', 'inconclusive: signature_invalid', ['两侧记录的签名状态不一致（只有一侧带签名）']
        )
    if signature and other:
        if signing_key is None:
            out['signature_verified'] = None
            notes.append('两侧记录都带签名，但当前环境未提供 ' + TRUST_KEY_ENV + '，签名无法校验')
        elif verify(base, signing_key) and verify(candidate, signing_key):
            out['signature_verified'] = True
        else:
            return _finish(
                out, 'inconclusive', 'signature_invalid', 'inconclusive: signature_invalid', ['记录签名校验失败：记录内容已被改动']
            )
    else:
        out['signature_verified'] = None
        if signing_key is not None:
            return _finish(
                out,
                'inconclusive',
                'signature_missing',
                'inconclusive: signature_missing',
                ['当前环境提供 ' + TRUST_KEY_ENV + '，但记录未签名：未经认证的断言不予采信'],
            )
        notes.append('两侧记录都没有签名，真实性无法由本工具确认')

    b_responses = baseline_responses if baseline_responses is not None else responses
    c_responses = candidate_responses if candidate_responses is not None else responses
    mismatches = []
    suite_mismatch = False
    for side, record, resp in (('baseline', base, b_responses), ('candidate', candidate, c_responses)):
        if resp is None:
            continue
        out['recompute_checked'][side] = True
        if suite is None:
            found = _digest_mismatches(record, resp)
        else:
            found, suite_ok = _shadow_mismatches(record, suite, resp, _identity_of(record))
            if not suite_ok:
                suite_mismatch = True
        for item in found:
            entry = dict(item)
            entry['side'] = side
            mismatches.append(entry)
    out['mismatches'] = mismatches
    out['recompute_verified'] = bool(
        suite is not None
        and out['recompute_checked']['baseline']
        and out['recompute_checked']['candidate']
        and not suite_mismatch
        and not mismatches
    )
    if mismatches:
        ids = sorted({str(m['id']) for m in mismatches if m['id'] is not None})
        notes.append('记录与重算结果不一致：' + (', '.join('task ' + i for i in ids) if ids else '记录级摘要不一致'))
        return _finish(
            out, 'inconclusive', 'record_does_not_match_responses', 'inconclusive: record_does_not_match_responses', notes
        )
    if suite_mismatch:
        notes.append('记录的 suite_sha256 与给定任务集的重算结果不一致')
        return _finish(out, 'inconclusive', 'record_does_not_match_suite', 'inconclusive: record_does_not_match_suite', notes)
    if require_recompute and not out['recompute_verified']:
        notes.append('要求回算校验，但缺少可回算的任务集或输出')
        return _finish(out, 'inconclusive', 'recompute_not_possible', 'inconclusive: recompute_not_possible', notes)
    if not assume_independent:
        return _finish(
            out, 'inconclusive', 'sampling_assumptions_not_declared', 'inconclusive: sampling_assumptions_not_declared', notes
        )

    n = len(b)
    diffs = [int(c[i]['pass']) - int(b[i]['pass']) for i in b]
    delta = sum(diffs) / n

    if stat_method == 'paired_exact':
        n_01 = sum(1 for i in b if (not b[i]['pass']) and c[i]['pass'])
        n_10 = sum(1 for i in b if b[i]['pass'] and (not c[i]['pass']))
        max_allowed_regressions = math.floor(margin * n)
        p_regress = _exact_binomial_upper_tail(n_10, n_01 + n_10)
        # Fail-closed small-sample paired rule:
        # - regression if regressions exceed margin budget or net delta < -margin
        # - non_inferior if regressions <= margin budget and net delta >= 0 (no net drop)
        # - inconclusive otherwise
        if n_10 > max_allowed_regressions or delta < -margin:
            decision = 'regression'
        elif delta >= 0.0 and n_10 <= max_allowed_regressions:
            decision = 'non_inferior'
        else:
            decision = 'inconclusive'

        reason = {
            'regression': 'interval_confirms_regression',
            'non_inferior': 'ok',
            'inconclusive': 'interval_does_not_clear_margin',
        }[decision]
        notes.append(SMALL_SAMPLE_LIMITATION)
        notes.append(f'paired_exact: n_01={n_01}, n_10={n_10}, max_allowed_regressions={max_allowed_regressions}, p_regress={p_regress:.4f}')
        out.update(
            {
                'n_pairs': n,
                'delta_candidate_minus_baseline': delta,
                'interval': [delta, delta],
                'method': 'paired_exact (small-sample regression-count gate; p_regress informational)',
                'assumption': 'independent paired tasks; fixed holdout suite; zero-tolerance guard violations',
            }
        )
        return _finish(
            out,
            decision,
            reason,
            {
                'non_inferior': 'small-sample paired gate passed (regressions within margin and non-negative delta)',
                'regression': 'small-sample paired gate detected regression exceeding margin',
                'inconclusive': 'small-sample paired gate inconclusive (negative delta within margin)',
            }[decision],
            notes,
        )

    # Default: two-sided Hoeffding bound
    radius = math.sqrt(2 * math.log(2 / alpha) / n)
    lo = max(-1.0, delta - radius)
    hi = min(1.0, delta + radius)
    decision = 'regression' if hi < -margin else ('non_inferior' if lo > -margin else 'inconclusive')
    reason = {
        'regression': 'interval_confirms_regression',
        'non_inferior': 'ok',
        'inconclusive': 'interval_does_not_clear_margin',
    }[decision]
    out.update(
        {
            'n_pairs': n,
            'delta_candidate_minus_baseline': delta,
            'interval': [lo, hi],
            'method': 'two-sided Hoeffding bound',
            'assumption': 'independent task pairs and pre-specified fixed sample size; no repeated testing',
        }
    )
    return _finish(
        out,
        decision,
        reason,
        {
            'non_inferior': 'non-inferior within margin; not proof of equality',
            'regression': 'regression exceeds margin',
            'inconclusive': 'interval does not establish non-inferiority or regression',
        }[decision],
        notes,
    )


def read(path):
    return json.loads(Path(path).read_text(encoding='utf-8-sig'))


def _valid_metadata(record):
    instrument = record.get('instrument')
    identity = record.get('identity_declared')
    is_hash = lambda value: isinstance(value, str) and re.fullmatch(r'[0-9a-f]{64}', value) is not None
    if (
        record.get('schema') != 'audit-eval-record-v1'
        or not isinstance(instrument, dict)
        or not isinstance(identity, dict)
        or not identity
    ):
        return False
    if not all(is_hash(record.get(field)) for field in ('suite_sha256', 'responses_sha256', 'identity_sha256')):
        return False
    if record['identity_sha256'] != digest(identity) or not all(
        isinstance(instrument.get(field), str) and instrument[field] for field in _GRADER_KEYS
    ):
        return False
    if record.get('protocol') not in VALID_PROTOCOLS:
        return False
    stack = _stack_of(identity)
    manifest = digest(stack) if stack else None
    return (
        instrument.get('manifest_sha256') == manifest
        and record.get('stack_fingerprint') == _fingerprint(stack)
        and identity.get('stack_fingerprint', _fingerprint(stack)) == _fingerprint(stack)
    )


def write(path, value):
    Path(path).write_text(json.dumps(value, ensure_ascii=False, indent=2, allow_nan=False) + '\n', encoding='utf-8')



def evaluate_pass_k(records):
    """Compute per-task pass@k (at least one pass) and pass^k (all k runs pass) across repeated eval records."""
    if not isinstance(records, list) or not records:
        raise ValueError('records must be a nonempty list')
    if any(not isinstance(rec, dict) or not rec.get('suite_sha256') for rec in records):
        raise ValueError('every record must include suite_sha256')
    suites = {rec['suite_sha256'] for rec in records}
    if len(suites) > 1:
        raise ValueError('records must belong to the same suite_sha256')
    k = len(records)
    per_task = {}
    all_ids = set()
    for rec in records:
        seen_in_rec = set()
        for row in rec.get('results', []):
            tid = row.get('id')
            if not tid or tid in seen_in_rec:
                continue
            seen_in_rec.add(tid)
            all_ids.add(tid)
            if row.get('status') == 'graded' and type(row.get('pass')) is bool:
                per_task.setdefault(tid, []).append(row['pass'])
    eligible = {tid: vals for tid, vals in per_task.items() if len(vals) == k}
    n = len(eligible)
    excluded = len(all_ids) - n
    if n == 0:
        return {'k': k, 'n_tasks': 0, 'excluded_ungraded': excluded, 'pass_at_k': None, 'pass_pow_k': None}
    pass_at_k = sum(1 for vals in eligible.values() if any(vals)) / n
    pass_pow_k = sum(1 for vals in eligible.values() if all(vals)) / n
    return {'k': k, 'n_tasks': n, 'excluded_ungraded': excluded, 'pass_at_k': pass_at_k, 'pass_pow_k': pass_pow_k}


def main():
    p = argparse.ArgumentParser(description=__doc__)
    sp = p.add_subparsers(dest='command', required=True)
    e = sp.add_parser('score')
    e.add_argument('--suite', required=True)
    e.add_argument('--responses', required=True)
    e.add_argument('--identity', required=True)
    e.add_argument('--out', required=True)

    c = sp.add_parser('compare')
    c.add_argument('--baseline', required=True)
    c.add_argument('--candidate', required=True)
    c.add_argument('--margin', required=True, type=float)
    c.add_argument('--out', required=True)
    c.add_argument('--alpha', type=float, default=0.05, help='two-sided error rate (default 0.05)')
    c.add_argument('--suite', help='task set used to recompute the records')
    c.add_argument('--responses', help='responses used for recompute verification (both sides unless overridden)')
    c.add_argument('--baseline-responses', help='responses of the baseline side')
    c.add_argument('--candidate-responses', help='responses of the candidate side')
    c.add_argument('--require-recompute', action='store_true', help='refuse to answer without recompute verification')
    c.add_argument('--allow-stack-mismatch', action='store_true', help='compare stacks whose harness/prompt_version differ')
    c.add_argument('--assume-independent', action='store_true', help='declare independent task pairs and fixed sample size')
    c.add_argument(
        '--stat-method',
        choices=['hoeffding', 'paired_exact'],
        default='hoeffding',
        help='statistical gate method (default: hoeffding; use paired_exact for small personal suites)',
    )

    a = p.parse_args()
    try:
        if a.command == 'score':
            value = evaluate(read(a.suite), read(a.responses), read(a.identity))
        else:
            suite = read(a.suite) if a.suite else None
            responses = read(a.responses) if a.responses else None
            baseline_responses = read(a.baseline_responses) if a.baseline_responses else None
            candidate_responses = read(a.candidate_responses) if a.candidate_responses else None
            value = compare(
                read(a.baseline),
                read(a.candidate),
                a.margin,
                alpha=a.alpha,
                assume_independent=a.assume_independent,
                suite=suite,
                responses=responses,
                baseline_responses=baseline_responses,
                candidate_responses=candidate_responses,
                require_recompute=a.require_recompute,
                allow_stack_mismatch=a.allow_stack_mismatch,
                stat_method=a.stat_method,
            )
        write(a.out, value)
        print(json.dumps(value.get('summary', value), ensure_ascii=False))
        if a.command == 'compare' and not value['release_eligible']:
            return 2
        if a.command == 'score' and value['summary']['ungraded']:
            return 2
        return 0
    except (ValueError, KeyError, TypeError, OSError, json.JSONDecodeError) as exc:
        print('evaluation error: ' + str(exc), file=sys.stderr)
        return 2


if __name__ == '__main__':
    sys.exit(main())

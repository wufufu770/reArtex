package sodpolicy

import "testing"

// TestReasonCodesAreUnique 断言原因码不重复 —— 重复会让审计里的原因码
// 无法唯一定位一条裁决。
func TestReasonCodesAreUnique(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range []string{
		ReasonScopeOK, ReasonAllChecksPassed, ReasonTier2NotApplicable,
		ReasonOutOfScope, ReasonToolCircuitOpen, ReasonQuotaExhausted,
		ReasonTier2Inconsistent, ReasonAnomalyFlagged,
		ReasonScopeUnavailable, ReasonToolHealthUnavailable,
		ReasonQuotaUnavailable, ReasonTier2Unavailable,
		ReasonNormalizerUnavailable, ReasonNormalizerMalformed,
		ReasonTokenInvalid,
	} {
		if seen[c] {
			t.Fatalf("原因码重复: %s", c)
		}
		seen[c] = true
	}
}

// TestThresholdsArePositive 断言阈值都是正数 —— 0 或负数会让熔断 /
// 超时 / 缓存的行为退化（0 失败就熔断、0 超时立刻失败）。
func TestThresholdsArePositive(t *testing.T) {
	for name, v := range map[string]int{
		"NormalizerTimeoutSeconds":   NormalizerTimeoutSeconds,
		"NormalizerMaxTokens":        NormalizerMaxTokens,
		"NormalizerCacheMaxEntries":  NormalizerCacheMaxEntries,
		"CircuitFailThreshold":       CircuitFailThreshold,
		"CircuitOpenDurationSeconds": CircuitOpenDurationSeconds,
		"TokenTTLSeconds":            TokenTTLSeconds,
		"TokenBytes":                 TokenBytes,
		"PayloadPreviewMaxBytes":     PayloadPreviewMaxBytes,
	} {
		if v <= 0 {
			t.Errorf("阈值 %s = %d，必须为正", name, v)
		}
	}
}

// TestNotGatedToolsHasNoDuplicates 断言放行清单无重复。
func TestNotGatedToolsHasNoDuplicates(t *testing.T) {
	seen := map[string]bool{}
	for _, k := range NotGatedTools {
		if seen[k] {
			t.Errorf("放行清单重复: %s", k)
		}
		seen[k] = true
	}
}

// TestVerdictsAreExactlyThree 断言裁决恰好三态 —— 闸门多产一种结果，就多
// 一类「调用方忘了处理」的动作（第四态永远是放行还是拒绝的问题）。
//
// 这三个字面量同时是 db/schema_sod.sql 里 sod_decisions.verdict 的 CHECK
// 镜像：代码多写一种、DDL 少放一种，写入就会在运行期炸掉。
func TestVerdictsAreExactlyThree(t *testing.T) {
	got := map[string]bool{
		VerdictApprove:     true,
		VerdictDeny:        true,
		VerdictNeedsReview: true,
	}
	want := map[string]bool{"approve": true, "deny": true, "needs_review": true}
	for v := range got {
		if !want[v] {
			t.Errorf("裁决 %q 不在 approve/deny/needs_review 之内", v)
		}
		delete(want, v)
	}
	for v := range want {
		t.Errorf("裁决集合缺少 %q", v)
	}
}

// TestCircuitStatesAreExactlyThree 断言熔断状态恰好三态 —— 与
// db/schema_sod.sql 里 sod_tool_circuit.state 的 CHECK 镜像。
func TestCircuitStatesAreExactlyThree(t *testing.T) {
	got := []string{CircuitClosed, CircuitOpen, CircuitHalfOpen}
	want := map[string]bool{"closed": true, "open": true, "half_open": true}
	for _, v := range got {
		if !want[v] {
			t.Errorf("熔断状态 %q 不在 closed/open/half_open 之内", v)
		}
		delete(want, v)
	}
	if len(want) != 0 {
		t.Errorf("熔断状态集合缺少 %v", want)
	}
}

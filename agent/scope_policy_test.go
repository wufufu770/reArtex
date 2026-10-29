package agent

import (
	"testing"

	"github.com/Autumn-27/artex/internal/scopepolicy"
)

// TestScopePolicyIsThinAlias pins that agent.ScopePolicy is NOT a second
// definition: it must mirror scopepolicy.Policy field for field. A second copy
// of the values would let the expansion cap and the scope admission check
// drift apart — the exact gap the single source of truth exists to close.
func TestScopePolicyIsThinAlias(t *testing.T) {
	if ScopePolicy.PlatformScopeMode != scopepolicy.Policy.PlatformScopeMode {
		t.Fatalf("PlatformScopeMode: agent=%q scopepolicy=%q（别名必须逐字段一致）",
			ScopePolicy.PlatformScopeMode, scopepolicy.Policy.PlatformScopeMode)
	}
	if ScopePolicy.MaxAgentAddedScope != scopepolicy.Policy.MaxAgentAddedScope {
		t.Fatalf("MaxAgentAddedScope: agent=%d scopepolicy=%d（别名必须逐字段一致）",
			ScopePolicy.MaxAgentAddedScope, scopepolicy.Policy.MaxAgentAddedScope)
	}
	if len(ScopePolicy.MaxAgentAddedKinds) != len(scopepolicy.Policy.MaxAgentAddedKinds) {
		t.Fatalf("MaxAgentAddedKinds: agent=%v scopepolicy=%v（别名必须逐字段一致）",
			ScopePolicy.MaxAgentAddedKinds, scopepolicy.Policy.MaxAgentAddedKinds)
	}
	for i, kind := range scopepolicy.Policy.MaxAgentAddedKinds {
		if ScopePolicy.MaxAgentAddedKinds[i] != kind {
			t.Fatalf("MaxAgentAddedKinds[%d]: agent=%q scopepolicy=%q（别名必须逐字段一致）",
				i, ScopePolicy.MaxAgentAddedKinds[i], kind)
		}
	}
}

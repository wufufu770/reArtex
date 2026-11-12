package agent

import (
	"encoding/json"
	"testing"
)

// E1-4 三态（方案 v10 §9 ⑩ 已认可口径）：出网分支吃范围状态。
// 范围内 ⇒ Tier2·egress_in_scope；未判定 / 越界 ⇒ Tier3·egress 维持。
func TestTierOfActionEgressScopeStates(t *testing.T) {
	args := json.RawMessage(`{"cmd":"curl https://target.example.com/api"}`)
	newAction := func(sv ScopeVerdict) TierAction {
		return TierAction{
			ToolKey: "Bash",
			Args:    args,
			Scope:   sv,
			Write:   WriteSemantics{Egress: true},
		}
	}

	in := TierOfAction(newAction(ScopeVerdict{AssetInScope: true, Assessed: true}))
	if in.Tier != Tier2 || in.Category != CategoryEgressInScope {
		t.Fatalf("范围内出网应 Tier2·egress_in_scope, got %v/%v", in.Tier, in.Category)
	}

	unjudged := TierOfAction(newAction(ScopeVerdict{AssetInScope: false, Assessed: false}))
	if unjudged.Tier != Tier3 || unjudged.Category != CategoryEgress {
		t.Fatalf("未判定出网应维持 Tier3·egress, got %v/%v", unjudged.Tier, unjudged.Category)
	}

	out := TierOfAction(newAction(ScopeVerdict{AssetInScope: false, Assessed: true}))
	if out.Tier != Tier3 || out.Category != CategoryEgress {
		t.Fatalf("越界出网应维持 Tier3·egress, got %v/%v", out.Tier, out.Category)
	}
}

// 非出网写不受影响：范围内普通结构化写仍按既有分支走（不出网就不该被打上出网类别）。
func TestTierOfActionNonEgressUnaffected(t *testing.T) {
	v := TierOfAction(TierAction{
		ToolKey: "insert_assets",
		Scope:   ScopeVerdict{AssetInScope: true, Assessed: true},
		Write:   WriteSemantics{},
	})
	if v.Category == CategoryEgressInScope || v.Category == CategoryEgress {
		t.Fatalf("非出网动作不应落到出网类别, got %v", v.Category)
	}
}

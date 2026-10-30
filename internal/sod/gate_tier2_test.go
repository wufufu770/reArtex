package sod

import (
	"context"
	"errors"
	"testing"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/internal/sodpolicy"
)

// tier2 用例：第四道检查（证据一致性）与它不适用的形态。

// TestSoDGateDeniesWhenTier2Unavailable 钉住 fail-closed：finding 证据读不出来 ⇒
// 拒绝。
func TestSoDGateDeniesWhenTier2Unavailable(t *testing.T) {
	stub := gateStubAllowAll()
	stub.evidenceErr = errors.New("证据查询炸了")
	g := &Gate{deps: stub}

	in := gateInput()
	in.FindingID = 9
	out := g.Evaluate(context.Background(), in)
	if out.Verdict != sodpolicy.VerdictDeny {
		t.Fatalf("verdict=%q want %q", out.Verdict, sodpolicy.VerdictDeny)
	}
	if out.ReasonCode != sodpolicy.ReasonTier2Unavailable {
		t.Fatalf("reason_code=%q want %q", out.ReasonCode, sodpolicy.ReasonTier2Unavailable)
	}
}

// TestSoDGateDeniesWhenTier2ScopeUnavailable 钉住 fail-closed 的第二个入口：
// finding 的资产归属判不出来 ⇒ 拒绝。
func TestSoDGateDeniesWhenTier2ScopeUnavailable(t *testing.T) {
	stub := gateStubAllowAll()
	stub.evidence = &db.ReplayEvidence{FindingID: 9, ReplayEvidenceID: "ev-1", AssetIDs: []int64{1}}
	stub.scopedErr = errors.New("资产归属查询炸了")
	g := &Gate{deps: stub}

	in := gateInput()
	in.FindingID = 9
	out := g.Evaluate(context.Background(), in)
	if out.Verdict != sodpolicy.VerdictDeny {
		t.Fatalf("verdict=%q want %q", out.Verdict, sodpolicy.VerdictDeny)
	}
	if out.ReasonCode != sodpolicy.ReasonTier2Unavailable {
		t.Fatalf("reason_code=%q want %q", out.ReasonCode, sodpolicy.ReasonTier2Unavailable)
	}
}

// TestSoDGateTier2RequiresReplayEvidence 钉住 tier2 的两条判据：重放证据必须
// 非空；关联资产的归属必须与本任务范围一致。
func TestSoDGateTier2RequiresReplayEvidence(t *testing.T) {
	in := gateInput()
	in.FindingID = 9

	// ① 证据标识为空 ⇒ 不一致。
	stub := gateStubAllowAll()
	stub.evidence = &db.ReplayEvidence{FindingID: 9, ReplayEvidenceID: ""}
	g := &Gate{deps: stub}
	out := g.Evaluate(context.Background(), in)
	if out.Verdict != sodpolicy.VerdictDeny || out.ReasonCode != sodpolicy.ReasonTier2Inconsistent {
		t.Fatalf("空证据 verdict=%q reason=%q want deny/%s",
			out.Verdict, out.ReasonCode, sodpolicy.ReasonTier2Inconsistent)
	}

	// ② 证据非空但资产越界（ScopedAssetIDs 少给了一个）⇒ 不一致。
	stub = gateStubAllowAll()
	stub.evidence = &db.ReplayEvidence{FindingID: 9, ReplayEvidenceID: "ev-1", AssetIDs: []int64{1, 2}}
	stub.scopedIDs = []int64{1}
	g = &Gate{deps: stub}
	out = g.Evaluate(context.Background(), in)
	if out.Verdict != sodpolicy.VerdictDeny || out.ReasonCode != sodpolicy.ReasonTier2Inconsistent {
		t.Fatalf("资产越界 verdict=%q reason=%q want deny/%s",
			out.Verdict, out.ReasonCode, sodpolicy.ReasonTier2Inconsistent)
	}

	// ③ 证据非空且资产都在范围内 ⇒ 通过。
	stub = gateStubAllowAll()
	stub.evidence = &db.ReplayEvidence{FindingID: 9, ReplayEvidenceID: "ev-1", AssetIDs: []int64{1, 2}}
	stub.scopedIDs = []int64{1, 2}
	g = &Gate{deps: stub}
	out = g.Evaluate(context.Background(), in)
	if out.Verdict != sodpolicy.VerdictApprove {
		t.Fatalf("verdict=%q want %q", out.Verdict, sodpolicy.VerdictApprove)
	}
	if out.Checks["tier2_check"] != "pass" {
		t.Fatalf("tier2_check=%q want pass", out.Checks["tier2_check"])
	}
}

// TestSoDGateTier2NotApplicable 钉住「不关联 finding ⇒ tier2 不适用 ⇒ 通过」。
func TestSoDGateTier2NotApplicable(t *testing.T) {
	stub := gateStubAllowAll()
	g := &Gate{deps: stub}

	out := g.Evaluate(context.Background(), gateInput())
	if out.Checks["tier2_check"] != "not_applicable" {
		t.Fatalf("tier2_check=%q want not_applicable", out.Checks["tier2_check"])
	}
	if stub.tier2Calls != 0 {
		t.Fatalf("不关联 finding 时不该查证据，实际查了 %d 次", stub.tier2Calls)
	}
}

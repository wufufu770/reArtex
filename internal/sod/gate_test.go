package sod

import (
	"context"
	"errors"
	"testing"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/internal/sodpolicy"
)

// gateStub 是闸门的依赖替身：每一道检查都能被单独驱动。
//
// ⚠️ 替身只用于**单元测试**闸门的判定逻辑；真库接线见 gate_contract_test.go。
type gateStub struct {
	scopeInScope bool
	scopeErr     error

	circuitState string
	circuitErr   error

	groupID    int64
	groupIDErr error
	quotaState db.QuotaState
	quotaErr   error

	evidence    *db.ReplayEvidence
	evidenceErr error
	scopedIDs   []int64
	scopedErr   error

	// counters 用来断言短路顺序。
	//
	// ⚠️ 第五道检查（anomaly）没有对应的查询 —— 它读的是**输入**里的
	// anomaly_flag（规整器的产出），不是查出来的状态 ⇒ 它「有没有被评估」
	// 只能从 Checks 里看（键存在 = 评估过）。
	scopeCalls, circuitCalls, quotaCalls, tier2Calls int
}

func (s *gateStub) scopeHostInScope(int64, string) (bool, error) {
	s.scopeCalls++
	return s.scopeInScope, s.scopeErr
}

func (s *gateStub) toolCircuitState(string) (string, error) {
	s.circuitCalls++
	return s.circuitState, s.circuitErr
}

func (s *gateStub) groupIDForScope(int64, int64) (int64, error) {
	s.quotaCalls++
	return s.groupID, s.groupIDErr
}

func (s *gateStub) groupQuotaState(int64) (db.QuotaState, error) {
	s.quotaCalls++
	return s.quotaState, s.quotaErr
}

func (s *gateStub) findingReplayEvidence(int64) (*db.ReplayEvidence, error) {
	s.tier2Calls++
	return s.evidence, s.evidenceErr
}

func (s *gateStub) scopedAssetIDs(int64, []int64) ([]int64, error) {
	s.tier2Calls++
	return s.scopedIDs, s.scopedErr
}

// gateStubAllowAll 是一个「一切正常」的替身：scope 命中、工具没熔断、不属于
// 任何群组、不关联 finding、无异常。
func gateStubAllowAll() *gateStub {
	return &gateStub{scopeInScope: true, circuitState: sodpolicy.CircuitClosed}
}

// gateInput 是一条「本该被放行」的调用输入。
func gateInput() GateInput {
	return GateInput{
		ToolName:    "Bash",
		SourceAgent: "worker",
		TaskID:      42,
		Normalized: NormalizerResult{
			StructuredIntent: StructuredIntent{
				ActionType: "http_request",
				TargetHint: "api.example.com",
			},
		},
	}
}

// TestSoDGateDeniesOutOfScopeCommand 钉住第一道检查：目标不在范围内 ⇒ 拒绝。
func TestSoDGateDeniesOutOfScopeCommand(t *testing.T) {
	stub := gateStubAllowAll()
	stub.scopeInScope = false
	g := &Gate{deps: stub}

	out := g.Evaluate(context.Background(), gateInput())
	if out.Verdict != sodpolicy.VerdictDeny {
		t.Fatalf("verdict=%q want %q", out.Verdict, sodpolicy.VerdictDeny)
	}
	if out.ReasonCode != sodpolicy.ReasonOutOfScope {
		t.Fatalf("reason_code=%q want %q", out.ReasonCode, sodpolicy.ReasonOutOfScope)
	}
	if out.Checks["scope_check"] != "fail" {
		t.Fatalf("scope_check=%q want fail", out.Checks["scope_check"])
	}
}

// TestSoDGateDeniesWhenTargetHintMissing 钉住「没有目标线索 ⇒ 无法判定范围 ⇒
// 不放行」：范围判定的前提是有一个可比对的目标，空的 target_hint 判不了。
func TestSoDGateDeniesWhenTargetHintMissing(t *testing.T) {
	g := &Gate{deps: gateStubAllowAll()}

	in := gateInput()
	in.Normalized.StructuredIntent.TargetHint = ""
	out := g.Evaluate(context.Background(), in)
	if out.Verdict != sodpolicy.VerdictDeny {
		t.Fatalf("verdict=%q want %q", out.Verdict, sodpolicy.VerdictDeny)
	}
	if out.ReasonCode != sodpolicy.ReasonOutOfScope {
		t.Fatalf("reason_code=%q want %q", out.ReasonCode, sodpolicy.ReasonOutOfScope)
	}
}

// TestSoDGateDeniesWhenScopeQueryUnavailable 钉住 fail-closed：范围查不出来 ⇒
// **拒绝**（不是「查不出来就放行」）。
func TestSoDGateDeniesWhenScopeQueryUnavailable(t *testing.T) {
	stub := gateStubAllowAll()
	stub.scopeErr = errors.New("范围查询炸了")
	g := &Gate{deps: stub}

	out := g.Evaluate(context.Background(), gateInput())
	if out.Verdict != sodpolicy.VerdictDeny {
		t.Fatalf("verdict=%q want %q", out.Verdict, sodpolicy.VerdictDeny)
	}
	if out.ReasonCode != sodpolicy.ReasonScopeUnavailable {
		t.Fatalf("reason_code=%q want %q", out.ReasonCode, sodpolicy.ReasonScopeUnavailable)
	}
}

// TestSoDGateDeniesWhenToolHealthUnavailable 钉住 fail-closed：熔断状态查不出来
// ⇒ 拒绝。
func TestSoDGateDeniesWhenToolHealthUnavailable(t *testing.T) {
	stub := gateStubAllowAll()
	stub.circuitErr = errors.New("熔断状态查询炸了")
	g := &Gate{deps: stub}

	out := g.Evaluate(context.Background(), gateInput())
	if out.Verdict != sodpolicy.VerdictDeny {
		t.Fatalf("verdict=%q want %q", out.Verdict, sodpolicy.VerdictDeny)
	}
	if out.ReasonCode != sodpolicy.ReasonToolHealthUnavailable {
		t.Fatalf("reason_code=%q want %q", out.ReasonCode, sodpolicy.ReasonToolHealthUnavailable)
	}
}

// TestSoDGateDeniesWhenToolCircuitOpen 钉住熔断开路 ⇒ 拒绝（half_open 仍放行：
// 半开态允许一次试探，否则一次熔断就把工具在整场任务里判了死刑）。
func TestSoDGateDeniesWhenToolCircuitOpen(t *testing.T) {
	stub := gateStubAllowAll()
	stub.circuitState = sodpolicy.CircuitOpen
	g := &Gate{deps: stub}

	out := g.Evaluate(context.Background(), gateInput())
	if out.Verdict != sodpolicy.VerdictDeny {
		t.Fatalf("verdict=%q want %q", out.Verdict, sodpolicy.VerdictDeny)
	}
	if out.ReasonCode != sodpolicy.ReasonToolCircuitOpen {
		t.Fatalf("reason_code=%q want %q", out.ReasonCode, sodpolicy.ReasonToolCircuitOpen)
	}

	stub.circuitState = sodpolicy.CircuitHalfOpen
	out = g.Evaluate(context.Background(), gateInput())
	if out.Verdict != sodpolicy.VerdictApprove {
		t.Fatalf("half_open 的 verdict=%q want %q", out.Verdict, sodpolicy.VerdictApprove)
	}
}

// TestSoDGateDeniesWhenQuotaUnavailable 钉住 fail-closed：群组归属查不出来 ⇒
// 拒绝（查不出来不等于没有限制）。
func TestSoDGateDeniesWhenQuotaUnavailable(t *testing.T) {
	stub := gateStubAllowAll()
	stub.groupIDErr = errors.New("群组归属查询炸了")
	g := &Gate{deps: stub}

	out := g.Evaluate(context.Background(), gateInput())
	if out.Verdict != sodpolicy.VerdictDeny {
		t.Fatalf("verdict=%q want %q", out.Verdict, sodpolicy.VerdictDeny)
	}
	if out.ReasonCode != sodpolicy.ReasonQuotaUnavailable {
		t.Fatalf("reason_code=%q want %q", out.ReasonCode, sodpolicy.ReasonQuotaUnavailable)
	}
}

// TestSoDGateDeniesWhenQuotaStateUnavailable 钉住 fail-closed 的第二个入口：额度
// **状态**查不出来 ⇒ 拒绝。
func TestSoDGateDeniesWhenQuotaStateUnavailable(t *testing.T) {
	stub := gateStubAllowAll()
	stub.groupID = 7
	stub.quotaErr = errors.New("额度状态查询炸了")
	g := &Gate{deps: stub}

	out := g.Evaluate(context.Background(), gateInput())
	if out.Verdict != sodpolicy.VerdictDeny {
		t.Fatalf("verdict=%q want %q", out.Verdict, sodpolicy.VerdictDeny)
	}
	if out.ReasonCode != sodpolicy.ReasonQuotaUnavailable {
		t.Fatalf("reason_code=%q want %q", out.ReasonCode, sodpolicy.ReasonQuotaUnavailable)
	}
}

// TestSoDGateQuotaDrainingAndParkedDeny 钉住额度的拒绝态：draining / parked ⇒
// 拒绝；active ⇒ 放行；零值（没配预算）**不是**拒绝态。
func TestSoDGateQuotaDrainingAndParkedDeny(t *testing.T) {
	for _, tc := range []struct {
		state db.QuotaState
		deny  bool
	}{
		{db.QuotaStateDraining, true},
		{db.QuotaStateParked, true},
		{db.QuotaStateActive, false},
		{db.QuotaStateUnset, false},
	} {
		stub := gateStubAllowAll()
		stub.groupID = 7
		stub.quotaState = tc.state
		g := &Gate{deps: stub}

		out := g.Evaluate(context.Background(), gateInput())
		if tc.deny {
			if out.Verdict != sodpolicy.VerdictDeny {
				t.Fatalf("state=%q verdict=%q want %q", tc.state, out.Verdict, sodpolicy.VerdictDeny)
			}
			if out.ReasonCode != sodpolicy.ReasonQuotaExhausted {
				t.Fatalf("state=%q reason_code=%q want %q", tc.state, out.ReasonCode, sodpolicy.ReasonQuotaExhausted)
			}
			continue
		}
		if out.Verdict != sodpolicy.VerdictApprove {
			t.Fatalf("state=%q verdict=%q want %q", tc.state, out.Verdict, sodpolicy.VerdictApprove)
		}
	}
}

// TestSoDGateApprovesWhenAllChecksPass 钉住全通过的形态：三种裁决里只有 approve
// 的原因码是 all_checks_passed，五道检查都留下结论。
func TestSoDGateApprovesWhenAllChecksPass(t *testing.T) {
	g := &Gate{deps: gateStubAllowAll()}

	out := g.Evaluate(context.Background(), gateInput())
	if out.Verdict != sodpolicy.VerdictApprove {
		t.Fatalf("verdict=%q want %q", out.Verdict, sodpolicy.VerdictApprove)
	}
	if out.ReasonCode != sodpolicy.ReasonAllChecksPassed {
		t.Fatalf("reason_code=%q want %q", out.ReasonCode, sodpolicy.ReasonAllChecksPassed)
	}
	for _, key := range []string{"scope_check", "tool_health_check", "quota_check", "tier2_check", "anomaly_check"} {
		if _, ok := out.Checks[key]; !ok {
			t.Fatalf("Checks 缺 %s：%v", key, out.Checks)
		}
	}
	if out.Checks["anomaly_check"] != "clean" {
		t.Fatalf("anomaly_check=%q want clean", out.Checks["anomaly_check"])
	}
}

// TestSoDGateZeroValueFailsClosed 钉住「装配坏了 ⇒ 拒绝」：零值 Gate 没有资产库、
// 没有 DB ⇒ 每道检查都不可用 ⇒ 必须拒绝（scope_check_unavailable），不能 panic，
// 更不能放行。
func TestSoDGateZeroValueFailsClosed(t *testing.T) {
	g := &Gate{}

	out := g.Evaluate(context.Background(), gateInput())
	if out.Verdict != sodpolicy.VerdictDeny {
		t.Fatalf("verdict=%q want %q", out.Verdict, sodpolicy.VerdictDeny)
	}
	if out.ReasonCode != sodpolicy.ReasonScopeUnavailable {
		t.Fatalf("reason_code=%q want %q", out.ReasonCode, sodpolicy.ReasonScopeUnavailable)
	}
}

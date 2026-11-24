package taskintelligence

import (
	"strings"
	"testing"
	"time"
)

// ---------- Hypothesis 契约与状态机 ----------

func TestValidateHypothesis(t *testing.T) {
	ok := Hypothesis{TaskID: 1, Statement: "普通用户可能执行越权状态转换", Status: HypStatusProposed, Confidence: 0.6}
	if err := ValidateHypothesis(ok); err != nil {
		t.Fatalf("合法假设不应报错: %v", err)
	}
	cases := []struct {
		name string
		h    Hypothesis
		want string
	}{
		{"缺task", Hypothesis{Statement: "s"}, "task_id"},
		{"缺语句", Hypothesis{TaskID: 1}, "语句为必填"},
		{"坏状态", Hypothesis{TaskID: 1, Statement: "s", Status: "confirmed"}, "非法假设状态"},
		{"坏置信度", Hypothesis{TaskID: 1, Statement: "s", Confidence: 1.5}, "[0,1]"},
	}
	for _, c := range cases {
		if err := ValidateHypothesis(c.h); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s: 期望含 %q 的错误, got %v", c.name, c.want, err)
		}
	}
}

func TestHypothesisTransitions(t *testing.T) {
	legal := [][2]string{
		{HypStatusProposed, HypStatusValidating},
		{HypStatusValidating, HypStatusProposed},
		{HypStatusValidating, HypStatusValidated},
		{HypStatusValidating, HypStatusRefuted},
		{HypStatusProposed, HypStatusDropped},
		{HypStatusValidating, HypStatusDropped},
	}
	for _, tr := range legal {
		if ok, why := CanTransitionHypothesis(tr[0], tr[1]); !ok {
			t.Fatalf("合法迁移 %v→%v 被拒: %s", tr[0], tr[1], why)
		}
	}
	// 跳步拒绝：没有经过验证的「确认」不是假设的语义。
	if ok, why := CanTransitionHypothesis(HypStatusProposed, HypStatusValidated); ok {
		t.Fatal("proposed→validated 跳步应拒绝")
	} else if !strings.Contains(why, "跳步") {
		t.Fatalf("拒绝原因应说明跳步, got %q", why)
	}
	// 终态不可离开。
	for _, from := range []string{HypStatusValidated, HypStatusRefuted, HypStatusDropped} {
		if ok, _ := CanTransitionHypothesis(from, HypStatusValidating); ok {
			t.Fatalf("终态 %s 不可离开", from)
		}
	}
	// 同状态拒绝。
	if ok, _ := CanTransitionHypothesis(HypStatusProposed, HypStatusProposed); ok {
		t.Fatal("同状态迁移应拒绝")
	}
}

// ---------- Fact / EvidenceRef 契约 ----------

func TestValidateFact(t *testing.T) {
	ok := Fact{TaskID: 1, Subject: "登录接口", Predicate: "returns", Object: "token", FactKind: FactKindObserved,
		SourceType: "traffic", SourceRef: "exchange#42", Confidence: 0.9}
	if err := ValidateFact(ok); err != nil {
		t.Fatalf("合法事实不应报错: %v", err)
	}
	// 无来源不算事实（铁律）。
	noSrc := ok
	noSrc.SourceType, noSrc.SourceRef = "", ""
	if err := ValidateFact(noSrc); err == nil || !strings.Contains(err.Error(), "无来源不算事实") {
		t.Fatalf("无来源事实应拒绝, got %v", err)
	}
	bad := ok
	bad.FactKind = "guessed"
	if err := ValidateFact(bad); err == nil || !strings.Contains(err.Error(), "非法事实种类") {
		t.Fatalf("非法种类应拒绝, got %v", err)
	}
}

func TestValidateEvidenceRef(t *testing.T) {
	ok := EvidenceRef{TaskID: 1, Kind: "traffic_exchange", ContentRef: "exchange#42",
		ContentHash: "deadbeef", Support: EvidenceDirSupports}
	if err := ValidateEvidenceRef(ok); err != nil {
		t.Fatalf("合法证据不应报错: %v", err)
	}
	noRef := ok
	noRef.ContentRef, noRef.ContentHash = "", ""
	if err := ValidateEvidenceRef(noRef); err == nil || !strings.Contains(err.Error(), "不可追溯") {
		t.Fatalf("不可追溯证据应拒绝, got %v", err)
	}
	bad := ok
	bad.Support = "maybe"
	if err := ValidateEvidenceRef(bad); err == nil || !strings.Contains(err.Error(), "非法支持方向") {
		t.Fatalf("非法支持方向应拒绝, got %v", err)
	}
}

// ---------- CapabilityExecution 契约与范围漂移 ----------

func TestValidateCapabilityExecutionAndScopeDrift(t *testing.T) {
	ok := CapabilityExecution{TaskID: 1, CapabilityID: "exploration.edge", ExecutionID: "exec-1",
		Status: ExecStatusRunning, CreatedAt: time.Now()}
	if err := ValidateCapabilityExecution(ok); err != nil {
		t.Fatalf("合法执行不应报错: %v", err)
	}
	noExec := ok
	noExec.ExecutionID = ""
	if err := ValidateCapabilityExecution(noExec); err == nil || !strings.Contains(err.Error(), "execution_id") {
		t.Fatalf("缺 envelope 标识应拒绝, got %v", err)
	}
	bad := ok
	bad.Status = "reopened"
	if err := ValidateCapabilityExecution(bad); err == nil || !strings.Contains(err.Error(), "非法执行状态") {
		t.Fatalf("终态复活应拒绝, got %v", err)
	}
	// 范围漂移判定：两侧水位齐备且不一致才判漂移。
	drift := ok
	drift.ScopeVersionStart, drift.ScopeVersionEnd = 3, 5
	if !ScopeDrifted(drift) {
		t.Fatal("水位 3→5 应判漂移")
	}
	partial := ok
	partial.ScopeVersionStart, partial.ScopeVersionEnd = 3, 0
	if ScopeDrifted(partial) {
		t.Fatal("单侧未记录不可断言漂移")
	}
}

// ---------- TI-04 台账 → §0.5 执行对象的桥 ----------

func TestCollabRecordToExecution(t *testing.T) {
	rec := CollabRecord{CausationID: "bizdiff#12", CorrelationID: "corr-bizdiff#12-1",
		ToCapability: "exploration.edge", Status: "accepted", Tokens: 120}
	e := CollabRecordToExecution(rec, 7)
	if e.TaskID != 7 || e.CapabilityID != "exploration.edge" || e.ExecutionID != rec.CorrelationID ||
		e.CausationID != rec.CausationID || e.Status != ExecStatusCompleted || e.CostTokens != 120 {
		t.Fatalf("台账映射字段不符: %+v", e)
	}
	if err := ValidateCapabilityExecution(e); err != nil {
		t.Fatalf("映射结果应通过契约校验: %v", err)
	}
	// rejected/blocked → blocked；suggested → running。
	if got := CollabRecordToExecution(CollabRecord{ToCapability: "x", CorrelationID: "c", Status: "rejected"}, 1).Status; got != ExecStatusBlocked {
		t.Fatalf("rejected 应映射 blocked, got %s", got)
	}
	if got := CollabRecordToExecution(CollabRecord{ToCapability: "x", CorrelationID: "c", Status: "suggested"}, 1).Status; got != ExecStatusRunning {
		t.Fatalf("suggested 应映射 running, got %s", got)
	}
}

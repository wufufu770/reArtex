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

// TestSoDGateTaskInternalWriteBypassesScopeLookup 钉住 B2：任务内结构化写
// （record_fact / record_progress / add_hint / message_group）跳过 LLM 规整、
// scope 按构造通过（理由码 pass(task_internal_write)），**其余检查照跑**。
//
// 为什么这组断言重要：
//   - 这些动作没有外部 host 可判范围——拿空 target_hint 去跑老逻辑，它们会被
//     out_of_scope 拒掉（规整器一抖整条写回链路瘫痪的根因之一）；
//   - 但「跳过 scope 查找」绝不能演变成「跳过闸门」：熔断 / 额度检查必须还在。
func TestSoDGateTaskInternalWriteBypassesScopeLookup(t *testing.T) {
	stub := gateStubAllowAll()
	g := &Gate{deps: stub}

	// 零值 NormalizerResult（没有模型产出）+ TaskInternalWrite ⇒ scope 按构造通过。
	in := GateInput{
		ToolName:          "record_fact",
		SourceAgent:       "worker",
		TaskID:            42,
		Normalized:        NormalizerResult{},
		TaskInternalWrite: true,
	}
	out := g.Evaluate(context.Background(), in)
	if out.Verdict != sodpolicy.VerdictApprove {
		t.Fatalf("任务内结构化写应放行，verdict=%q reason=%q", out.Verdict, out.ReasonCode)
	}
	if out.Checks["scope_check"] != "pass(task_internal_write)" {
		t.Fatalf("scope_check=%q，want pass(task_internal_write)", out.Checks["scope_check"])
	}
	if stub.scopeCalls != 0 {
		t.Fatalf("任务内写不应查范围表，scope 查询次数=%d", stub.scopeCalls)
	}
	if stub.circuitCalls == 0 || stub.quotaCalls == 0 {
		t.Fatal("熔断/额度检查必须照跑（跳过的是规整，不是闸门）")
	}

	// 反向：同类工具若熔断，照样拒绝——旁路不豁免其余检查。
	stub2 := gateStubAllowAll()
	stub2.circuitState = sodpolicy.CircuitOpen
	g2 := &Gate{deps: stub2}
	out2 := g2.Evaluate(context.Background(), in)
	if out2.Verdict != sodpolicy.VerdictDeny || out2.ReasonCode != sodpolicy.ReasonToolCircuitOpen {
		t.Fatalf("熔断的工具必须照拒，verdict=%q reason=%q", out2.Verdict, out2.ReasonCode)
	}
}

// TestTaskInternalWriteClassifier 钉住分类器：四工具入列，扩权工具与 shell 家族
// 绝不入列（它们照旧走完整链）。
func TestTaskInternalWriteClassifier(t *testing.T) {
	for _, name := range []string{"record_fact", "record_progress", "add_hint", "message_group"} {
		if !TaskInternalWrite(name) {
			t.Errorf("%q 应判为任务内结构化写", name)
		}
	}
	for _, name := range []string{"Bash", "insert_assets", "add_task_scope", "add_company_scope",
		"report_finding", "shell_open", "WebFetch", ""} {
		if TaskInternalWrite(name) {
			t.Errorf("%q 不应判为任务内结构化写（扩权/出网/有外部目标的动作照旧进规整）", name)
		}
	}
}

// TestSoDGateLocalTaskDirScope 钉住 B5：本地任务目录 Bash 的 scope 按构造通过
//（pass(local_task_dir)），且熔断/额度检查照跑。
func TestSoDGateLocalTaskDirScope(t *testing.T) {
	stub := gateStubAllowAll()
	g := &Gate{deps: stub}
	in := GateInput{
		ToolName:     "Bash",
		SourceAgent:  "worker",
		TaskID:       42,
		Normalized:   NormalizerResult{},
		LocalTaskDir: true,
	}
	out := g.Evaluate(context.Background(), in)
	if out.Verdict != sodpolicy.VerdictApprove {
		t.Fatalf("本地任务目录读应放行，verdict=%q reason=%q", out.Verdict, out.ReasonCode)
	}
	if out.Checks["scope_check"] != "pass(local_task_dir)" {
		t.Fatalf("scope_check=%q，want pass(local_task_dir)", out.Checks["scope_check"])
	}
}

// TestBashLocalTaskDirClassifier 钉住 B5 的路径抽取与出界判定。
func TestBashLocalTaskDirClassifier(t *testing.T) {
	ok := []string{
		`{"command":"grep -r flag /app/data/tasks/10"}`,
		`{"command":"cat /app/data/tasks/10/i94/output.txt | head"}`,
		`{"command":"ls /app/data/tasks/10 /app/data/tasks/10/i94"}`,
	}
	bad := []string{
		`{"command":"grep -r flag /app/data/tasks/10 /etc/passwd"}`,   // 混入出界路径
		`{"command":"cat /app/data/tasks/10-evil/x"}`,                 // 前缀混淆
		`{"command":"curl -s http://203.0.113.10:8090/ --output /app/data/tasks/10/x"}`, // 含外部目标（URL 不算路径但命令非纯本地……URL 不被抽为路径 ⇒ 仍判 true？）
		`{"command":"echo hello"}`,                                    // 无路径 token ⇒ 不豁免
		`{"command":"cat /etc/passwd"}`,                               // 纯出界
	}
	for _, p := range ok[:3] {
		if !BashLocalTaskDir(10, []byte(p)) {
			t.Errorf("应判本地任务目录: %s", p)
		}
	}
	for _, p := range bad[:2] {
		if BashLocalTaskDir(10, []byte(p)) {
			t.Errorf("不应豁免: %s", p)
		}
	}
	if BashLocalTaskDir(10, []byte(bad[3])) {
		t.Error("无路径 token 不应豁免")
	}
	if BashLocalTaskDir(10, []byte(bad[4])) {
		t.Error("纯出界不应豁免")
	}
	if BashLocalTaskDir(0, []byte(ok[0])) {
		t.Error("taskID=0 不应豁免")
	}
}

func TestBashLocalTaskDirNetworkGuard(t *testing.T) {
	// 网络指示符守卫：出网藏在本地产物后面必须被拦住。
	notLocal := []string{
		`{"command":"curl -s http://203.0.113.10:8090/ --output /app/data/tasks/10/x"}`,
		`{"command":"wget -q https://example.com/a -O /app/data/tasks/10/x"}`,
		`{"command":"curl example.com:8080 > /app/data/tasks/10/x"}`,
		`{"command":"nc 203.0.113.10 8090"}`,
	}
	for _, p := range notLocal {
		if BashLocalTaskDir(10, []byte(p)) {
			t.Errorf("含网络指示符不应豁免: %s", p)
		}
	}
	// 纯本地照旧豁免。
	if !BashLocalTaskDir(10, []byte(`{"command":"grep -r flag /app/data/tasks/10/i94"}`)) {
		t.Error("纯本地任务目录命令应豁免")
	}
}

// TestDegradeTrackerLifecycle 钉住 B4 状态机：连续 unavailable 达阈值进降级、
// 开关关闭立即退出、任一次成功退出并清零。
func TestDegradeTrackerLifecycle(t *testing.T) {
	enabled := func() bool { return true }
	onChangeCalls := 0
	onChange := func(bool) { onChangeCalls++ }

	var tr degradeTracker
	for i := 0; i < degradeThreshold-1; i++ {
		tr.recordUnavailable(enabled, onChange)
		if tr.isActive(enabled) {
			t.Fatalf("第 %d 次失败不应进降级", i+1)
		}
	}
	tr.recordUnavailable(enabled, onChange)
	if !tr.isActive(enabled) {
		t.Fatal("达到阈值应进降级")
	}
	// 关闭开关 ⇒ 立即退出（fail-closed 回来）。
	off := func() bool { return false }
	if tr.isActive(off) {
		t.Fatal("开关关闭必须立即退出降级")
	}
	// 再开开关（连续计数还在阈值上）⇒ 仍处降级。
	if !tr.isActive(enabled) {
		t.Fatal("重开开关且未恢复时应仍在降级")
	}
	// 成功一次 ⇒ 退出降级并清零。
	tr.recordAvailable(enabled, onChange)
	if tr.isActive(enabled) {
		t.Fatal("恢复后必须退出降级")
	}
	for i := 0; i < degradeThreshold-1; i++ {
		tr.recordUnavailable(enabled, onChange)
	}
	if tr.isActive(enabled) {
		t.Fatal("清零后重新计数，阈值前不应进降级")
	}
}

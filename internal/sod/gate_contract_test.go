package sod

import (
	"context"
	"fmt"
	"go/parser"
	"go/token"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/internal/sodpolicy"
)

// 这个文件钉住闸门的三条**契约**：短路顺序、「闸门不能问模型」的结构保证、
// 以及与真库的接线。

// TestSoDGateShortCircuitOrder 钉住五道检查的**短路顺序**：前面命中，后面一次
// 都不能跑。
//
// 为什么这条性质要紧：短路顺序既是性能约定，也是**语义**约定 —— 越权（scope）
// 必须最先说；而且「后面的检查没跑」意味着它们没机会给出更宽松的结论。
func TestSoDGateShortCircuitOrder(t *testing.T) {
	// ① scope 失败 ⇒ 熔断 / 额度 / tier2 / anomaly 一次都没被评估。
	stub := gateStubAllowAll()
	stub.scopeInScope = false
	g := &Gate{deps: stub}
	out := g.Evaluate(context.Background(), gateInput())
	if stub.circuitCalls != 0 || stub.quotaCalls != 0 || stub.tier2Calls != 0 {
		t.Fatalf("scope 失败后仍跑了后续检查: circuit=%d quota=%d tier2=%d",
			stub.circuitCalls, stub.quotaCalls, stub.tier2Calls)
	}
	if _, ok := out.Checks["anomaly_check"]; ok {
		t.Fatal("scope 失败后 anomaly 检查不该被评估")
	}

	// ② 熔断失败 ⇒ 额度 / tier2 / anomaly 一次都没被评估。
	stub = gateStubAllowAll()
	stub.circuitState = sodpolicy.CircuitOpen
	g = &Gate{deps: stub}
	out = g.Evaluate(context.Background(), gateInput())
	if stub.quotaCalls != 0 || stub.tier2Calls != 0 {
		t.Fatalf("熔断拒绝后仍跑了后续检查: quota=%d tier2=%d", stub.quotaCalls, stub.tier2Calls)
	}
	if _, ok := out.Checks["anomaly_check"]; ok {
		t.Fatal("熔断拒绝后 anomaly 检查不该被评估")
	}

	// ③ 额度失败 ⇒ tier2 / anomaly 一次都没被评估。
	stub = gateStubAllowAll()
	stub.groupID = 7
	stub.quotaState = db.QuotaStateParked
	g = &Gate{deps: stub}
	out = g.Evaluate(context.Background(), gateInput())
	if stub.tier2Calls != 0 {
		t.Fatalf("额度拒绝后仍查了 tier2 证据 %d 次", stub.tier2Calls)
	}
	if _, ok := out.Checks["anomaly_check"]; ok {
		t.Fatal("额度拒绝后 anomaly 检查不该被评估")
	}

	// ④ tier2 失败 ⇒ anomaly 一次都没被评估。
	stub = gateStubAllowAll()
	stub.evidence = &db.ReplayEvidence{FindingID: 9, ReplayEvidenceID: ""}
	g = &Gate{deps: stub}
	tier2In := gateInput()
	tier2In.FindingID = 9
	out = g.Evaluate(context.Background(), tier2In)
	if _, ok := out.Checks["anomaly_check"]; ok {
		t.Fatal("tier2 拒绝后 anomaly 检查不该被评估")
	}

	// ⑤ 异常 ⇒ needs_review（**不是** deny）。
	stub = gateStubAllowAll()
	g = &Gate{deps: stub}
	anomalyIn := gateInput()
	anomalyIn.Normalized.AnomalyFlag = true
	anomalyIn.Normalized.AnomalyReason = "命令里出现编码后的载荷"
	out = g.Evaluate(context.Background(), anomalyIn)
	if out.Verdict != sodpolicy.VerdictNeedsReview {
		t.Fatalf("verdict=%q want %q", out.Verdict, sodpolicy.VerdictNeedsReview)
	}
	if out.ReasonCode != sodpolicy.ReasonAnomalyFlagged {
		t.Fatalf("reason_code=%q want %q", out.ReasonCode, sodpolicy.ReasonAnomalyFlagged)
	}
	if out.Checks["anomaly_check"] != "flagged" {
		t.Fatalf("anomaly_check=%q want flagged", out.Checks["anomaly_check"])
	}
}

// TestSoDGateDecisionPathCannotConsultModel 钉住项目的铁律「模型只提议，
// 确定性层裁决」—— 用**结构**断言，不靠人读代码。
//
// 三条断言：
//  1. 闸门文件不许 import 任何模型包；
//  2. 闸门唯一的**行为**依赖是 gateDeps，而它的方法集恰好是六个「查实时状态」
//     的查询 —— 没有任何一个能带回来一个「模型的判断」；
//  3. 闸门结构体里除了那两个只读句柄与 gateDeps，没有别的依赖。
//
// 为什么必须是结构断言：行为断言（"这次没调模型"）只能证明某一次没调；
// 结构断言证明的是「**没有那条路**」。
func TestSoDGateDecisionPathCannotConsultModel(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "gate.go", nil, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("解析 gate.go: %v", err)
	}
	for _, imp := range file.Imports {
		path := strings.Trim(imp.Path.Value, `"`)
		if isModelPackage(path) {
			t.Fatalf("闸门不许 import 模型包: %s", path)
		}
	}

	deps := reflect.TypeOf((*gateDeps)(nil)).Elem()
	got := make([]string, 0, deps.NumMethod())
	for i := 0; i < deps.NumMethod(); i++ {
		got = append(got, deps.Method(i).Name)
	}
	sort.Strings(got)
	want := []string{
		"findingReplayEvidence",
		"groupIDForScope",
		"groupQuotaState",
		"scopeHostInScope",
		"scopedAssetIDs",
		"toolCircuitState",
	}
	if len(got) != len(want) {
		t.Fatalf("gateDeps 的方法集=%v want %v（多一个方法就多一条能改变裁决的路）", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("gateDeps 的方法集=%v want %v", got, want)
		}
	}

	gateType := reflect.TypeOf(Gate{})
	for i := 0; i < gateType.NumField(); i++ {
		f := gateType.Field(i)
		if f.Type == deps {
			continue
		}
		switch f.Type.String() {
		case "*db.AssetStore", "*db.DB":
		default:
			t.Fatalf("闸门结构体多了一个依赖字段 %s: %s", f.Name, f.Type.String())
		}
	}
}

// isModelPackage 报告某个 import 路径是不是「能拿到模型判断」的包。
func isModelPackage(path string) bool {
	for _, bad := range []string{"/llm", "norma", "openai", "anthropic", "artex/agent"} {
		if strings.Contains(path, bad) {
			return true
		}
	}
	return false
}

// TestSoDGateAgainstRealDB 钉住闸门与**真库**的接线：范围命中 ⇒ 放行，范围外 ⇒
// 拒绝。
//
// 为什么替身不够：替身只证明判定逻辑，证明不了「闸门真的把 task id 传对了」。
// 这一条把整条读路径（scope → 熔断 → 额度）在真库上跑一遍。
func TestSoDGateAgainstRealDB(t *testing.T) {
	d := auditorTestDB(t)
	assets := d.Assets()

	task, err := d.CreateTask("sod gate fixture", "goal", nil, 0, 0)
	if err != nil {
		t.Fatalf("建任务失败: %v", err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(task.ID) })

	host := fmt.Sprintf("sod-gate-%d.example.test", time.Now().UnixNano())
	if _, err := assets.AddManualScope(task.ID, "root_domain", host, "sod gate 用例"); err != nil {
		t.Fatalf("声明范围失败: %v", err)
	}
	scopes, err := assets.ListTaskScope(task.ID)
	if err != nil {
		t.Fatalf("读范围失败: %v", err)
	}
	if len(scopes) == 0 {
		t.Fatal("范围行没落库")
	}
	scopeHost := scopes[0].Domain

	g := NewGate(assets, d)
	in := GateInput{
		ToolName:    "Bash",
		SourceAgent: "worker",
		TaskID:      task.ID,
		Normalized: NormalizerResult{
			StructuredIntent: StructuredIntent{ActionType: "http_request", TargetHint: scopeHost},
		},
	}
	out := g.Evaluate(context.Background(), in)
	if out.Verdict != sodpolicy.VerdictApprove {
		t.Fatalf("范围内目标 %q 的 verdict=%q reason=%q want approve", scopeHost, out.Verdict, out.ReasonCode)
	}

	in.Normalized.StructuredIntent.TargetHint = "not-in-scope.invalid"
	out = g.Evaluate(context.Background(), in)
	if out.Verdict != sodpolicy.VerdictDeny || out.ReasonCode != sodpolicy.ReasonOutOfScope {
		t.Fatalf("范围外目标的 verdict=%q reason=%q want deny/%s",
			out.Verdict, out.ReasonCode, sodpolicy.ReasonOutOfScope)
	}
}

package sod

// ---------- 合并闸管线行为测试（方案 v4.1 §1/§4）----------
//
// mergedVerdict 是纯裁决（无副作用）——用最小 ChainDeps 直接驱动：
// 三态路由、种类四动作、auditor 三裁决、故障兜底、cap 超限。

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Autumn-27/artex/internal/sodpolicy"
)

func testTime() time.Time { return time.Now() }

func chainCallFor(task int64, tool, body string) chainCall {
	return chainCall{
		toolName: tool, sourceAgent: "work#1", taskID: task,
		payload: []byte(body), payloadDigest: "dig-" + body, started: testTime(),
	}
}

// mergedHarness 组装最小链：只填合并闸需要的依赖。
func mergedHarness(t *testing.T, mode string, cats []sodpolicy.SodCategory, adj func(ctx context.Context, req AuditorRequest) (AuditorVerdict, error), used map[string]int) *Chain {
	t.Helper()
	return &Chain{deps: ChainDeps{
		MergedGateMode: func() string { return mode },
		Categories:     func() ([]sodpolicy.SodCategory, uint64) { return cats, 1 },
		Adjudicator:    adj,
		CategoryUsed: func(ctx context.Context, taskID int64, category string) (int, error) {
			return used[category], nil
		},
	}}
}

// TestMergedOffFallsThroughToOldChain off 态合并闸不接管（旧链语义不变）。
// 直接验证：PreToolUse 在 off 态走旧链路径——这里验证 mergedVerdict 不被
// off 影响是纯函数，链级路由在 TestMergedEnforceRoutes 覆盖。
func TestMergedVerdictPassCategory(t *testing.T) {
	cats := []sodpolicy.SodCategory{
		{Name: "良性无目标动作", Kind: "builtin", Pattern: sodpolicy.RecognizeBenignNoop, Action: sodpolicy.CategoryActionPass, Enabled: true, Priority: 80},
	}
	c := mergedHarness(t, sodpolicy.MergedGateEnforce, cats, nil, map[string]int{})
	d := c.mergedVerdict(context.Background(), chainCallFor(19, "Bash", `{"command":"echo test"}`), "Bash")
	if d.kind != "gate" || d.gateIn.ScopePassReason != "category:良性无目标动作" {
		t.Fatalf("pass 种类应构造通过：kind=%s reason=%q", d.kind, d.gateIn.ScopePassReason)
	}
}

func TestMergedVerdictHardDenyCategory(t *testing.T) {
	cats := []sodpolicy.SodCategory{
		{Name: "自定义nmap", Kind: "custom", Pattern: `\bnmap\b`, Action: sodpolicy.CategoryActionHardDeny, Enabled: true, Priority: 60},
	}
	c := mergedHarness(t, sodpolicy.MergedGateEnforce, cats, nil, map[string]int{})
	d := c.mergedVerdict(context.Background(), chainCallFor(19, "Bash", `{"command":"nmap -sV 10.0.0.1"}`), "Bash")
	if d.kind != "deny" || d.reason != sodpolicy.ReasonCategoryDeny {
		t.Fatalf("custom hard_deny 应拒：kind=%s reason=%s", d.kind, d.reason)
	}
}

func TestMergedVerdictAuditorRoutes(t *testing.T) {
	cats := []sodpolicy.SodCategory{
		{Name: "目录外写", Kind: "builtin", Pattern: sodpolicy.RecognizeOutsideWrite, Action: sodpolicy.CategoryActionAuditor, CapPerTask: 5, Fallback: "ask", Enabled: true, Priority: 60},
	}
	body := `{"command":"cp /app/data/tasks/19/a.md /var/www/html/x"}`
	// auditor pass。
	c := mergedHarness(t, sodpolicy.MergedGateEnforce, cats, func(ctx context.Context, req AuditorRequest) (AuditorVerdict, error) {
		return AuditorVerdict{Verdict: "pass", Reason: "目标目录是任务产物发布位", DecidedBy: "auditor"}, nil
	}, map[string]int{})
	d := c.mergedVerdict(context.Background(), chainCallFor(19, "Bash", body), "Bash")
	if d.kind != "gate" || d.gateIn.ScopePassReason != "auditor" {
		t.Fatalf("auditor pass 应构造通过：kind=%s reason=%q markers=%v", d.kind, d.gateIn.ScopePassReason, d.markers)
	}
	// auditor deny。
	c2 := mergedHarness(t, sodpolicy.MergedGateEnforce, cats, func(ctx context.Context, req AuditorRequest) (AuditorVerdict, error) {
		return AuditorVerdict{Verdict: "deny", Reason: "写系统目录与任务无关"}, nil
	}, map[string]int{})
	d2 := c2.mergedVerdict(context.Background(), chainCallFor(19, "Bash", body), "Bash")
	if d2.kind != "deny" || d2.reason != sodpolicy.ReasonAuditorDeny {
		t.Fatalf("auditor deny 应拒：kind=%s reason=%s", d2.kind, d2.reason)
	}
	// auditor 挂 → fallback=ask 且 ElevateAsk 未装配 ⇒ 拒（fail-safe）。
	c3 := mergedHarness(t, sodpolicy.MergedGateEnforce, cats, func(ctx context.Context, req AuditorRequest) (AuditorVerdict, error) {
		return AuditorVerdict{}, errors.New("boom")
	}, map[string]int{})
	d3 := c3.mergedVerdict(context.Background(), chainCallFor(19, "Bash", body), "Bash")
	if d3.kind != "deny" || d3.reason != sodpolicy.ReasonAuditorUnavailable {
		t.Fatalf("auditor 故障应按 fallback 拒（无 ask 通路）：kind=%s reason=%s", d3.kind, d3.reason)
	}
	// auditor 挂 + ask 批准 ⇒ 放行。
	c4 := mergedHarness(t, sodpolicy.MergedGateEnforce, cats, func(ctx context.Context, req AuditorRequest) (AuditorVerdict, error) {
		return AuditorVerdict{}, errors.New("boom")
	}, map[string]int{})
	c4.deps.ElevateAsk = func(ctx context.Context, req AskRequest) (bool, string, error) { return true, "human", nil }
	d4 := c4.mergedVerdict(context.Background(), chainCallFor(19, "Bash", body), "Bash")
	if d4.kind != "gate" || d4.gateIn.ScopePassReason != "elevated_ask" {
		t.Fatalf("fallback ask 批准后应放行：kind=%s reason=%q", d4.kind, d4.gateIn.ScopePassReason)
	}
}

func TestMergedVerdictCapExceeded(t *testing.T) {
	cats := []sodpolicy.SodCategory{
		{Name: "目录外写", Kind: "builtin", Pattern: sodpolicy.RecognizeOutsideWrite, Action: sodpolicy.CategoryActionAuditor, CapPerTask: 1, Fallback: "ask", Enabled: true, Priority: 60},
	}
	body := `{"command":"cp /app/data/tasks/19/a.md /var/www/html/x"}`
	// 配额用尽 + 人工不批 ⇒ 拒；auditor 不应被问到（上限即封顶）。
	asked := false
	c := mergedHarness(t, sodpolicy.MergedGateEnforce, cats, func(ctx context.Context, req AuditorRequest) (AuditorVerdict, error) {
		asked = true
		return AuditorVerdict{Verdict: "pass"}, nil
	}, map[string]int{"目录外写": 1})
	d := c.mergedVerdict(context.Background(), chainCallFor(19, "Bash", body), "Bash")
	if asked {
		t.Fatal("cap 超限不应再问 auditor")
	}
	if d.kind != "deny" || d.reason != sodpolicy.ReasonCategoryCapExceeded {
		t.Fatalf("cap 超限应拒：kind=%s reason=%s", d.kind, d.reason)
	}
}

func TestMergedVerdictUncategorizedGoesToAuditor(t *testing.T) {
	// 无种类表：unclear 调用 → auditor 默认裁量岗。
	c := mergedHarness(t, sodpolicy.MergedGateEnforce, nil, func(ctx context.Context, req AuditorRequest) (AuditorVerdict, error) {
		if req.Reason != "uncategorized" {
			t.Fatalf("未命中种类应标 uncategorized，got %q", req.Reason)
		}
		return AuditorVerdict{Verdict: "ask", Reason: "看不出意图"}, nil
	}, map[string]int{})
	c.deps.ElevateAsk = func(ctx context.Context, req AskRequest) (bool, string, error) { return true, "human", nil }
	d := c.mergedVerdict(context.Background(), chainCallFor(19, "Bash", `{"command":"openssl s_client -connect x:443"}`), "Bash")
	if d.kind != "gate" || d.gateIn.ScopePassReason != "elevated_ask" {
		t.Fatalf("uncategorized→auditor ask→人工批：kind=%s reason=%q", d.kind, d.gateIn.ScopePassReason)
	}
}

func TestMergedVerdictTargetKnownToGate(t *testing.T) {
	// 有明确目标：交闸门做真实范围判定（TargetHint 已带上第一个 host）。
	c := mergedHarness(t, sodpolicy.MergedGateEnforce, nil, nil, map[string]int{})
	d := c.mergedVerdict(context.Background(), chainCallFor(20, "Bash", `{"command":"curl -I https://www.beta-portal.com/login"}`), "Bash")
	if d.kind != "gate" || d.gateIn.ScopePassReason != "" {
		t.Fatalf("target_known 应交闸门：kind=%s reason=%q", d.kind, d.gateIn.ScopePassReason)
	}
	if d.gateIn.Normalized.StructuredIntent.TargetHint != "www.beta-portal.com" {
		t.Fatalf("TargetHint 应为第一个 host，got %q", d.gateIn.Normalized.StructuredIntent.TargetHint)
	}
}

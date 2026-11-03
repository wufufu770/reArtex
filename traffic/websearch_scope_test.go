package traffic

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/internal/sodpolicy"
	actool "github.com/Autumn-27/norma/tool"
)

// fakeAuditWriter 收集留痕事件。
type fakeAuditWriter struct {
	events []db.AuditEvent
}

func (f *fakeAuditWriter) AppendAuditEvent(ev db.AuditEvent) (string, error) {
	f.events = append(f.events, ev)
	return "digest", nil
}

// TestWebSearchScopeExemptHostPassesWithoutRecord 钉住「清单内的目的地放行且不留痕」。
//
// 清单内的目的地是平台基础设施（搜索服务商 / 模型 API / 镜像仓库）：放行是预期
// 行为，而每次搜索都记一条会把审计链淹掉。
func TestWebSearchScopeExemptHostPassesWithoutRecord(t *testing.T) {
	w := &fakeAuditWriter{}
	s := NewWebSearchScope(w)

	allowed, reason := s.Classify(context.Background(), "html.duckduckgo.com", "ddgs")
	if !allowed {
		t.Fatalf("清单内的目的地必须放行")
	}
	if reason != sodpolicy.ReasonEgressScopeExempt {
		t.Fatalf("原因码不对: got %q", reason)
	}
	if len(w.events) != 0 {
		t.Fatalf("清单内的目的地不该留痕: %+v", w.events)
	}
}

// TestWebSearchScopeUnlistedHostPassesAndRecords 钉住「清单外的目的地放行但留痕」。
//
// 这是 web_search 治理的核心：目的地不在豁免清单 ⇒ 它不受任务范围管辖（web_search
// 走自己的出口代理、不带任务身份）⇒ 这条出网必须以「未归属出网」的形式进审计链。
// 但工具不能坏 ⇒ 仍然放行。
func TestWebSearchScopeUnlistedHostPassesAndRecords(t *testing.T) {
	w := &fakeAuditWriter{}
	s := NewWebSearchScope(w)

	allowed, reason := s.Classify(context.Background(), "not-in-the-list.example", "ddgs")
	if !allowed {
		t.Fatalf("清单外的目的地必须放行（工具不能坏）")
	}
	if reason != sodpolicy.ReasonEgressScopeUnattributed {
		t.Fatalf("原因码不对: got %q", reason)
	}
	if len(w.events) != 1 {
		t.Fatalf("应当留一条未归属出网的痕: got %d", len(w.events))
	}
	ev := w.events[0]
	if ev.Actor != "web_search" || ev.ToolName != "egress" {
		t.Fatalf("留痕的署名不对: %+v", ev)
	}
	if ev.Action != "allow_unattributed" {
		t.Fatalf("留痕的动作不对: %q", ev.Action)
	}
	if ev.Reason != sodpolicy.ReasonEgressScopeUnattributed {
		t.Fatalf("留痕的原因码不对: %q", ev.Reason)
	}
	var payload map[string]any
	if err := json.Unmarshal(ev.Payload, &payload); err != nil {
		t.Fatalf("留痕载荷不是合法 JSON: %v", err)
	}
	if payload["host"] != "not-in-the-list.example" {
		t.Fatalf("留痕载荷里的目的地不对: %v", payload["host"])
	}
	if payload["backend"] != "ddgs" {
		t.Fatalf("留痕载荷里的后端不对: %v", payload["backend"])
	}
	if payload["attributed"] != false {
		t.Fatalf("web_search 的出网没有任务身份: %v", payload["attributed"])
	}
}

// TestWebSearchScopeUnresolvedHostDenied 钉住「判不出目的地 ⇒ fail-closed」。
//
// 不知道请求要去哪，就不能放行：放行等于给一条不知道目的地的出网开绿灯。
func TestWebSearchScopeUnresolvedHostDenied(t *testing.T) {
	w := &fakeAuditWriter{}
	s := NewWebSearchScope(w)

	allowed, reason := s.Classify(context.Background(), "", "ddgs")
	if allowed {
		t.Fatalf("判不出目的地必须拒绝")
	}
	if reason != sodpolicy.ReasonEgressScopeUnresolved {
		t.Fatalf("原因码不对: got %q", reason)
	}
	if len(w.events) != 1 {
		t.Fatalf("拒绝必须留痕: got %d", len(w.events))
	}
	if w.events[0].Action != "deny" {
		t.Fatalf("拒绝的动作不对: %q", w.events[0].Action)
	}
}

// TestWebSearchScopeNoWriterStillDecides 钉住「没有留痕写面时判定照常」。
//
// 留痕失败不是判定的理由；反过来的错误更糟：为了留痕把搜索工具弄坏，
// 反而掩盖了这次出网本身。
func TestWebSearchScopeNoWriterStillDecides(t *testing.T) {
	s := NewWebSearchScope(nil)
	allowed, reason := s.Classify(context.Background(), "not-in-the-list.example", "ddgs")
	if !allowed || reason != sodpolicy.ReasonEgressScopeUnattributed {
		t.Fatalf("判定结论不对: allowed=%v reason=%q", allowed, reason)
	}
}

// TestInstalledWebSearchScopeBlocksOutboundRequest 端到端钉住「判定真的发生在
// 请求发出之前」。
//
// 这条测试把 ARTex 的判定装进 vendored SDK 的 web_search 钩子（真实装配路径），
// 然后让后端的 base URL 解析不出主机 ⇒ 判定判不出目的地 ⇒ 拒绝。
//
// 用一个不可解析的 base URL 而不是断网：拒绝发生在请求发出【之前】，所以这条
// 测试不需要网络，也证明「被拒的请求根本没出去」。
func TestInstalledWebSearchScopeBlocksOutboundRequest(t *testing.T) {
	w := &fakeAuditWriter{}
	InstallWebSearchScope(NewWebSearchScope(w))
	t.Cleanup(func() { InstallWebSearchScope(nil) })

	tool, err := actool.NewWebSearch(actool.WebSearchConfig{
		Backend:         "deepseek",
		DeepSeekAPIKey:  "test-key",
		DeepSeekBaseURL: "://not-a-url",
		DeepSeekModel:   "test-model",
	})
	if err != nil {
		t.Fatalf("构造 web_search 工具失败: %v", err)
	}
	res, err := tool.Call(context.Background(), json.RawMessage(`{"query":"x"}`), &actool.ToolContext{})
	if err != nil {
		t.Fatalf("web_search 调用不该返回 Go 错误: %v", err)
	}
	out := res.Flatten()
	if !strings.Contains(out, "web_search 出网被拒") {
		t.Fatalf("判不出目的地必须被拒绝，实际输出: %q", out)
	}
	if len(w.events) != 1 {
		t.Fatalf("拒绝必须留痕: got %d", len(w.events))
	}
	if w.events[0].Action != "deny" {
		t.Fatalf("拒绝的动作不对: %q", w.events[0].Action)
	}
}

// TestInstalledWebSearchScopeLetsExemptBackendThrough 端到端钉住「清单内的目的地
// 放行」—— 且这一条**不打真实网络**。
//
// 代理指向一个没人监听的本地端口 ⇒ 请求必然在连接层失败，但失败点必须发生在
// 范围判定【之后】：若判定误拒，输出里会是「出网被拒」；判定放行时看到的是
// 连接错误。这同时钉住「放行方向没有把工具弄坏」。
func TestInstalledWebSearchScopeLetsExemptBackendThrough(t *testing.T) {
	w := &fakeAuditWriter{}
	InstallWebSearchScope(NewWebSearchScope(w))
	t.Cleanup(func() { InstallWebSearchScope(nil) })

	tool, err := actool.NewWebSearch(actool.WebSearchConfig{
		Backend: "ddgs",
		Proxy:   "http://127.0.0.1:1",
	})
	if err != nil {
		t.Fatalf("构造 web_search 工具失败: %v", err)
	}
	res, err := tool.Call(context.Background(), json.RawMessage(`{"query":"x"}`), &actool.ToolContext{})
	if err != nil {
		t.Fatalf("web_search 调用不该返回 Go 错误: %v", err)
	}
	out := res.Flatten()
	if strings.Contains(out, "web_search 出网被拒") {
		t.Fatalf("清单内的目的地不该被判定拒绝: %q", out)
	}
	if len(w.events) != 0 {
		t.Fatalf("清单内的目的地不该留痕: %+v", w.events)
	}
}

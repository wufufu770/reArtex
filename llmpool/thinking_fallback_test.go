package llmpool

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/Autumn-27/norma/llm"
)

// thinking_fallback 回归（0.15.44 #6）：网关 400 且请求带 thinking/reasoning
// 参数 → 剧离后本成员重放一次；无参可剥 → 400 原样上抛（不重放不熔断）。

func TestThinkingFallbackStream(t *testing.T) {
	var reqs []llm.CompletionRequest
	prov := &fakeProv{
		// 第 1 次调用立即 400（零事件，安全窗口内）；第 2 次（剥参重放）成功。
		events: [][]llm.StreamEvent{nil, {{Type: llm.SETextDelta, Text: "ok"}}},
		errs:   []error{fmt.Errorf("anthropic: status 400: thinking is not supported"), nil},
		reqLog: &reqs,
	}
	pool := New([]*Member{member(1, "A", 0, prov)}, nil, nil)
	req := llm.CompletionRequest{Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock("hi")}}}, Thinking: "enabled", ReasoningEffort: "high"}

	var gotText, gotErr string
	for ev, err := range pool.Stream(context.Background(), req) {
		if err != nil {
			gotErr = err.Error()
			break
		}
		gotText += ev.Text
	}
	if gotErr != "" {
		t.Fatalf("剥参重放后应成功，却得到: %s (text=%q calls=%d)", gotErr, gotText, len(reqs))
	}
	if gotText == "" {
		t.Error("重放成功后应收到文本事件")
	}
	if len(reqs) != 2 {
		t.Fatalf("应恰好 2 次调用（400 + 剧离重放）: %d", len(reqs))
	}
	if reqs[0].Thinking != "enabled" || reqs[1].Thinking != "" || reqs[1].ReasoningEffort != "" {
		t.Errorf("第 2 次请求应无 thinking/effort 参数: %+v vs %+v", reqs[0], reqs[1])
	}
}

func TestThinkingFallbackComplete(t *testing.T) {
	var reqs []llm.CompletionRequest
	prov := &fakeProv{
		events: [][]llm.StreamEvent{{{Type: llm.SETextDelta, Text: "ok"}}},
		errs:   []error{fmt.Errorf("openai: status 400: unknown field reasoning_effort"), nil},
		reqLog: &reqs,
	}
	pool := New([]*Member{member(1, "A", 0, prov)}, nil, nil)
	req := llm.CompletionRequest{Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock("hi")}}}, Thinking: "enabled"}

	_, _, _, err := pool.Complete(context.Background(), req)
	if err != nil {
		t.Fatalf("剥参重放后应成功: %v", err)
	}
	if len(reqs) < 2 {
		t.Fatalf("应发生剥参重放（≥2 次调用）: %d", len(reqs))
	}
	last := reqs[len(reqs)-1]
	if last.Thinking != "" || last.ReasoningEffort != "" {
		t.Errorf("最终请求应无 thinking/effort 参数: %+v", last)
	}
}

func TestThinkingFallbackNoParamsPassesThrough(t *testing.T) {
	var reqs []llm.CompletionRequest
	prov := &fakeProv{errs: []error{fmt.Errorf("openai: status 400: bad request")}, reqLog: &reqs}
	pool := New([]*Member{member(1, "A", 0, prov)}, nil, nil)
	req := llm.CompletionRequest{Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock("hi")}}}}

	var gotErr string
	for ev, err := range pool.Stream(context.Background(), req) {
		if err != nil {
			gotErr = err.Error()
			break
		}
		_ = ev
	}
	if !strings.Contains(gotErr, "400") {
		t.Fatalf("无参可剥的 400 应原样上抛: %q", gotErr)
	}
	if len(reqs) != 1 {
		t.Errorf("无参 400 不应重放: %d 次调用", len(reqs))
	}
}

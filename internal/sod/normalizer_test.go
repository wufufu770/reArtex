package sod

import (
	"context"
	"errors"
	"iter"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/norma/llm"
)

// fakeProvider 是一个可编排的 provider 替身（测试不需要真实模型）。
//
// 形态照抄 norma 的 iter.Seq2 签名（llm.Provider.Stream 的契约）。
type fakeProvider struct {
	events []llm.StreamEvent
	err    error
	// stall=true ⇒ 卡住直到 ctx 结束（用来测超时）。
	stall bool
	got   llm.CompletionRequest
	calls int
}

func (p *fakeProvider) Stream(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	p.calls++
	p.got = req
	return func(yield func(llm.StreamEvent, error) bool) {
		if p.stall {
			<-ctx.Done()
			yield(llm.StreamEvent{}, ctx.Err())
			return
		}
		for _, ev := range p.events {
			if !yield(ev, nil) {
				return
			}
		}
		if p.err != nil {
			yield(llm.StreamEvent{}, p.err)
		}
	}
}

func (p *fakeProvider) Complete(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	acc := llm.NewAccumulator()
	for ev, err := range p.Stream(ctx, req) {
		if err != nil {
			return llm.Message{}, "", llm.Usage{}, err
		}
		acc.Add(ev)
	}
	return acc.Message(), acc.StopReason, acc.Usage, nil
}

// normalizerSchema 是 emit_structured_intent 的 schema（测试替身）。
func normalizerSchema() llm.ToolSchema {
	return llm.ToolSchema{
		Name:        "emit_structured_intent",
		Description: "产出结构化意图",
		InputSchema: map[string]any{"type": "object"},
	}
}

// toolUseEvents 造一次「模型调用了某个工具」的流事件序列。
func toolUseEvents(name, input string) []llm.StreamEvent {
	return []llm.StreamEvent{
		{Type: llm.SEToolUseStart, ToolID: "tu_1", ToolName: name, ToolIndex: 0},
		{Type: llm.SEToolInputJSON, Text: input, ToolIndex: 0},
		{Type: llm.SEMessageStop},
	}
}

// intentJSON 造一份合规的规整输出。
func intentJSON(actionType, target string) string {
	return `{"structured_intent":{"action_type":"` + actionType + `","executable":"curl",` +
		`"arguments":["-X","POST"],"target_hint":"` + target + `",` +
		`"shell_features":{"has_pipe":false,"has_redirect":false,"has_command_substitution":false,` +
		`"has_background":false,"has_obfuscation":false},"body_digest":"",` +
		`"raw_payload_digest":"deadbeef"},"anomaly_flag":false,"anomaly_reason":""}`
}

// newTestNormalizer 用一个假 provider 造一个规整器。
func newTestNormalizer(t *testing.T, p *fakeProvider) *Normalizer {
	t.Helper()
	n, err := NewNormalizer(func(profileID int64) (llm.Provider, bool) {
		return p, true
	}, 0, "你是意图规整器", normalizerSchema())
	if err != nil {
		t.Fatalf("构造规整器失败: %v", err)
	}
	return n
}

func TestSoDNormalizerParsesToolCall(t *testing.T) {
	p := &fakeProvider{events: toolUseEvents("emit_structured_intent", intentJSON("command_execution", "example.com"))}
	n := newTestNormalizer(t, p)

	got, err := n.Normalize(context.Background(), NormalizeInput{
		ToolName:   "Bash",
		RawPayload: []byte(`{"command":"curl -X POST https://example.com"}`),
		SourceAgent: "worker",
		TaskID:      7,
	})
	if err != nil {
		t.Fatalf("规整失败: %v", err)
	}
	if got.StructuredIntent.ActionType != "command_execution" {
		t.Fatalf("action_type = %q, 期望 command_execution", got.StructuredIntent.ActionType)
	}
	if got.StructuredIntent.TargetHint != "example.com" {
		t.Fatalf("target_hint = %q, 期望 example.com", got.StructuredIntent.TargetHint)
	}
	if got.StructuredIntent.RawPayloadDigest != "deadbeef" {
		t.Fatalf("raw_payload_digest = %q, 期望 deadbeef", got.StructuredIntent.RawPayloadDigest)
	}
	if got.AnomalyFlag {
		t.Fatal("anomaly_flag = true, 期望 false")
	}
	if got.SourceAgent != "worker" || got.TaskID != 7 {
		t.Fatalf("上下文被模型污染: source_agent=%q task_id=%d", got.SourceAgent, got.TaskID)
	}
}

// TestSoDNormalizerOverwritesModelSuppliedContext 钉住「模型说的上下文不算数」。
func TestSoDNormalizerOverwritesModelSuppliedContext(t *testing.T) {
	raw := `{"structured_intent":{"action_type":"command_execution","target_hint":"example.com",` +
		`"raw_payload_digest":"deadbeef"},"anomaly_flag":false,` +
		`"source_agent":"someone_else","task_id":999}`
	p := &fakeProvider{events: toolUseEvents("emit_structured_intent", raw)}
	n := newTestNormalizer(t, p)

	got, err := n.Normalize(context.Background(), NormalizeInput{
		ToolName: "Bash", RawPayload: []byte(`{"command":"curl https://example.com"}`),
		SourceAgent: "worker", TaskID: 7,
	})
	if err != nil {
		t.Fatalf("规整失败: %v", err)
	}
	if got.SourceAgent != "worker" {
		t.Fatalf("source_agent = %q, 期望 worker（模型说的不算数）", got.SourceAgent)
	}
	if got.TaskID != 7 {
		t.Fatalf("task_id = %d, 期望 7（模型说的不算数）", got.TaskID)
	}
}

// ---- 六种失败形态：每一种都要有自己的原因码与测试 ----

func TestSoDNormalizerRejectsUnavailableProvider(t *testing.T) {
	n, err := NewNormalizer(func(profileID int64) (llm.Provider, bool) { return nil, false },
		0, "提示词", normalizerSchema())
	if err != nil {
		t.Fatalf("构造规整器失败: %v", err)
	}
	_, err = n.Normalize(context.Background(), NormalizeInput{ToolName: "Bash", RawPayload: []byte(`{}`)})
	if !errors.Is(err, ErrNormalizerUnavailable) {
		t.Fatalf("err = %v, 期望 ErrNormalizerUnavailable", err)
	}
}

func TestSoDNormalizerRejectsProviderError(t *testing.T) {
	p := &fakeProvider{err: errors.New("上游 500")}
	n := newTestNormalizer(t, p)
	_, err := n.Normalize(context.Background(), NormalizeInput{ToolName: "Bash", RawPayload: []byte(`{}`)})
	if !errors.Is(err, ErrNormalizerUnavailable) {
		t.Fatalf("err = %v, 期望 ErrNormalizerUnavailable", err)
	}
}

func TestSoDNormalizerTimesOut(t *testing.T) {
	p := &fakeProvider{stall: true}
	n := newTestNormalizer(t, p)
	n.timeout = 30 * time.Millisecond

	start := time.Now()
	_, err := n.Normalize(context.Background(), NormalizeInput{ToolName: "Bash", RawPayload: []byte(`{}`)})
	if !errors.Is(err, ErrNormalizerUnavailable) {
		t.Fatalf("err = %v, 期望 ErrNormalizerUnavailable", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("超时没有生效，耗时 %s", elapsed)
	}
}

func TestSoDNormalizerRejectsMissingToolCall(t *testing.T) {
	p := &fakeProvider{events: []llm.StreamEvent{
		{Type: llm.SETextDelta, Text: "我认为这条命令是安全的"},
		{Type: llm.SEMessageStop},
	}}
	n := newTestNormalizer(t, p)
	_, err := n.Normalize(context.Background(), NormalizeInput{ToolName: "Bash", RawPayload: []byte(`{}`)})
	if !errors.Is(err, ErrNormalizerMalformed) {
		t.Fatalf("err = %v, 期望 ErrNormalizerMalformed", err)
	}
}

func TestSoDNormalizerRejectsWrongToolCall(t *testing.T) {
	p := &fakeProvider{events: toolUseEvents("run_bash", intentJSON("command_execution", "example.com"))}
	n := newTestNormalizer(t, p)
	_, err := n.Normalize(context.Background(), NormalizeInput{ToolName: "Bash", RawPayload: []byte(`{}`)})
	if !errors.Is(err, ErrNormalizerMalformed) {
		t.Fatalf("err = %v, 期望 ErrNormalizerMalformed", err)
	}
}

func TestSoDNormalizerRejectsInvalidToolInput(t *testing.T) {
	// ⚠️ llm.Accumulator 会把非法 JSON 兜底成 {}（accumulate.go 的 Message），
	// 于是反序列化「成功」但字段全空 —— 结果仍然是「不合规」，仍然拒绝。
	p := &fakeProvider{events: toolUseEvents("emit_structured_intent", `{"structured_intent":`)}
	n := newTestNormalizer(t, p)
	_, err := n.Normalize(context.Background(), NormalizeInput{ToolName: "Bash", RawPayload: []byte(`{}`)})
	if !errors.Is(err, ErrNormalizerMalformed) {
		t.Fatalf("err = %v, 期望 ErrNormalizerMalformed", err)
	}
}

func TestSoDNormalizerRejectsMissingStructuredIntent(t *testing.T) {
	p := &fakeProvider{events: toolUseEvents("emit_structured_intent", `{"anomaly_flag":false}`)}
	n := newTestNormalizer(t, p)
	_, err := n.Normalize(context.Background(), NormalizeInput{ToolName: "Bash", RawPayload: []byte(`{}`)})
	if !errors.Is(err, ErrNormalizerMalformed) {
		t.Fatalf("err = %v, 期望 ErrNormalizerMalformed", err)
	}
}

func TestSoDNormalizerRejectsMissingRawPayloadDigest(t *testing.T) {
	raw := `{"structured_intent":{"action_type":"command_execution","target_hint":"example.com"},"anomaly_flag":false}`
	p := &fakeProvider{events: toolUseEvents("emit_structured_intent", raw)}
	n := newTestNormalizer(t, p)
	_, err := n.Normalize(context.Background(), NormalizeInput{ToolName: "Bash", RawPayload: []byte(`{}`)})
	if !errors.Is(err, ErrNormalizerMalformed) {
		t.Fatalf("err = %v, 期望 ErrNormalizerMalformed", err)
	}
}

func TestSoDNormalizerRejectsUnparseableActionType(t *testing.T) {
	p := &fakeProvider{events: toolUseEvents("emit_structured_intent", intentJSON("unparseable", ""))}
	n := newTestNormalizer(t, p)
	_, err := n.Normalize(context.Background(), NormalizeInput{ToolName: "Bash", RawPayload: []byte(`{`)})
	if !errors.Is(err, ErrNormalizerMalformed) {
		t.Fatalf("err = %v, 期望 ErrNormalizerMalformed", err)
	}
	if !strings.Contains(err.Error(), "unparseable") {
		t.Fatalf("err = %v, 期望提到 unparseable", err)
	}
}

func TestSoDNormalizerRejectsUnknownActionType(t *testing.T) {
	p := &fakeProvider{events: toolUseEvents("emit_structured_intent", intentJSON("delete_everything", "example.com"))}
	n := newTestNormalizer(t, p)
	_, err := n.Normalize(context.Background(), NormalizeInput{ToolName: "Bash", RawPayload: []byte(`{}`)})
	if !errors.Is(err, ErrNormalizerMalformed) {
		t.Fatalf("err = %v, 期望 ErrNormalizerMalformed", err)
	}
}

// TestSoDNormalizerRejectsMissingTargetHint 钉住「动作类型需要目标却没有目标」。
func TestSoDNormalizerRejectsMissingTargetHint(t *testing.T) {
	p := &fakeProvider{events: toolUseEvents("emit_structured_intent", intentJSON("http_request", ""))}
	n := newTestNormalizer(t, p)
	_, err := n.Normalize(context.Background(), NormalizeInput{
		ToolName: "WebFetch", RawPayload: []byte(`{"url":"https://example.com"}`),
	})
	if !errors.Is(err, ErrNormalizerMalformed) {
		t.Fatalf("err = %v, 期望 ErrNormalizerMalformed", err)
	}
}

// TestSoDNormalizerAcceptsCommandWithoutTarget 钉住「本地命令没有目标不算不合规」
// —— 范围是否合法是闸门的事，不是规整器的事。
func TestSoDNormalizerAcceptsCommandWithoutTarget(t *testing.T) {
	p := &fakeProvider{events: toolUseEvents("emit_structured_intent", intentJSON("command_execution", ""))}
	n := newTestNormalizer(t, p)
	got, err := n.Normalize(context.Background(), NormalizeInput{
		ToolName: "Bash", RawPayload: []byte(`{"command":"cat /var/log/app.log"}`),
	})
	if err != nil {
		t.Fatalf("规整失败: %v", err)
	}
	if got.StructuredIntent.TargetHint != "" {
		t.Fatalf("target_hint = %q, 期望空", got.StructuredIntent.TargetHint)
	}
}

// TestSoDNormalizerRejectsCommandWithTargetMarkerButNoHint 钉住「载荷里有明确目标
// 标记却没提取 target_hint」。
func TestSoDNormalizerRejectsCommandWithTargetMarkerButNoHint(t *testing.T) {
	p := &fakeProvider{events: toolUseEvents("emit_structured_intent", intentJSON("command_execution", ""))}
	n := newTestNormalizer(t, p)
	_, err := n.Normalize(context.Background(), NormalizeInput{
		ToolName: "Bash", RawPayload: []byte(`{"command":"curl https://example.com/x"}`),
	})
	if !errors.Is(err, ErrNormalizerMalformed) {
		t.Fatalf("err = %v, 期望 ErrNormalizerMalformed", err)
	}
}

// TestSoDNormalizerNeverExecutesTheRawAction 钉住「规整器不执行任何东西」。
//
// 断言两件事：① 请求里只带 emit_structured_intent 一个工具（模型没有别的
// 工具可调）；② 原始动作是以**数据**的形式进模型的（raw_payload 字段），
// 不是一个待执行的命令。
func TestSoDNormalizerNeverExecutesTheRawAction(t *testing.T) {
	p := &fakeProvider{events: toolUseEvents("emit_structured_intent", intentJSON("command_execution", "example.com"))}
	n := newTestNormalizer(t, p)

	_, err := n.Normalize(context.Background(), NormalizeInput{
		ToolName:   "Bash",
		RawPayload: []byte(`{"command":"rm -rf /"}`),
		SourceAgent: "worker",
		TaskID:      7,
	})
	if err != nil {
		t.Fatalf("规整失败: %v", err)
	}
	if len(p.got.Tools) != 1 || p.got.Tools[0].Name != "emit_structured_intent" {
		t.Fatalf("请求携带的工具 = %+v, 期望只有 emit_structured_intent", p.got.Tools)
	}
	if len(p.got.Messages) != 1 || p.got.Messages[0].Role != llm.RoleUser {
		t.Fatalf("请求的消息结构异常: %+v", p.got.Messages)
	}
	body := p.got.Messages[0].Text()
	if !strings.Contains(body, "raw_payload") {
		t.Fatalf("消息里没有 raw_payload 字段: %s", body)
	}
	if !strings.Contains(body, "rm -rf /") {
		t.Fatalf("原始动作没有作为数据传给模型: %s", body)
	}
	// v9-batch1e：不再强制 disabled——mimo 网关禁思考时对复杂规整任务输出
	// 空响应（0 tool_use ⇒ normalizer_malformed）。思考模式交给 profile/网关
	// 默认；此处只断言"未显式开启"。
	if p.got.Thinking == "enabled" {
		t.Fatalf("thinking = %q, 不应显式开启", p.got.Thinking)
	}
	if p.got.Temperature == nil || *p.got.Temperature != 0 {
		t.Fatalf("temperature = %v, 期望 0", p.got.Temperature)
	}
	if p.got.MaxTokens != n.maxTokens {
		t.Fatalf("max_tokens = %d, 期望 %d", p.got.MaxTokens, n.maxTokens)
	}
}

func TestSoDNormalizerConstructorRejectsMissingDependencies(t *testing.T) {
	cases := []struct {
		name   string
		build  func() (*Normalizer, error)
		expect string
	}{
		{
			name: "缺 provider",
			build: func() (*Normalizer, error) {
				return NewNormalizer(nil, 0, "提示词", normalizerSchema())
			},
			expect: "provider",
		},
		{
			name: "缺提示词",
			build: func() (*Normalizer, error) {
				return NewNormalizer(func(int64) (llm.Provider, bool) { return nil, false }, 0, "  ", normalizerSchema())
			},
			expect: "提示词",
		},
		{
			name: "缺 schema",
			build: func() (*Normalizer, error) {
				return NewNormalizer(func(int64) (llm.Provider, bool) { return nil, false }, 0, "提示词", llm.ToolSchema{})
			},
			expect: "schema",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.build()
			if err == nil {
				t.Fatal("期望构造失败，实际成功")
			}
			if !strings.Contains(err.Error(), tc.expect) {
				t.Fatalf("err = %v, 期望提到 %s", err, tc.expect)
			}
		})
	}
}

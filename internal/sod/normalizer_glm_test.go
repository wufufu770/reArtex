package sod

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Autumn-27/norma/llm"
)

// glmStreamingResponse 是**真实抓包**下来的智谱（Zhipu / GLM）流式响应原文。
//
// 抓包方式：用仓库自己的 provider 栈（llm.FormatOpenAI）与真实端点
// https://open.bigmodel.cn/api/coding/paas/v4 发一次规整器请求，在
// http.RoundTripper 上把响应体原样截下来。原文含 API key 之外的任何凭据，
// 因此可以安全地作为测试夹具留在仓库里（响应里没有密钥）。
//
// 这份原文钉住三件事：
//
//	① GLM 的流式 tool_calls 是**一整个 chunk**（index + id + 完整 name +
//	   完整 arguments），不是分片；
//	② 工具入参里 task_id 是**字符串**（tools 表的 schema 就是
//	   {"type":"string"}，模型照做）；
//	③ finish_reason=tool_calls，且 content 为空。
//
// 回归意义：规整器曾经把模型工具入参直接反序列化进 NormalizerResult，而
// NormalizerResult.task_id 是 int64 ⇒ 一次完全合法的工具调用被判成
// normalizer_malformed，整条 SoD 链对每一次调用都拒绝。
const glmStreamingResponse = `data: {"id":"20261102175949373c6ad2342d46d9","created":1793613589,"object":"chat.completion.chunk","model":"glm-5.3-flash","choices":[{"index":0,"delta":{"tool_calls":[{"id":"call_77983e65e284431bbf1538b9","index":0,"type":"function","function":{"name":"emit_structured_intent","arguments":"{\"anomaly_flag\":false,\"source_agent\":\"capture\",\"structured_intent\":{\"action_type\": \"command_execution\", \"arguments\": [\"-s\", \"https://example.com/x\"], \"executable\": \"curl\", \"raw_payload_digest\": \"sha256:raw:curl -s https://example.com/x\", \"shell_features\": {\"has_background\": false, \"has_command_substitution\": false, \"has_obfuscation\": false, \"has_pipe\": false, \"has_redirect\": false}, \"target_hint\": \"example.com\"},\"task_id\":\"7\"}"}}]}}]}

data: {"id":"20261102175949373c6ad2342d46d9","created":1793613589,"object":"chat.completion.chunk","model":"glm-5.3-flash","choices":[{"index":0,"finish_reason":"tool_calls","delta":{"role":"assistant","content":""}}],"usage":{"prompt_tokens":1718,"completion_tokens":130,"total_tokens":1848,"prompt_tokens_details":{"cached_tokens":0},"completion_tokens_details":{"reasoning_tokens":0}}}

data: [DONE]

`

// glmCapturedArguments 是上面那份抓包里模型给出的工具入参原文。
const glmCapturedArguments = `{"anomaly_flag":false,"source_agent":"capture","structured_intent":{"action_type": "command_execution", "arguments": ["-s", "https://example.com/x"], "executable": "curl", "raw_payload_digest": "sha256:raw:curl -s https://example.com/x", "shell_features": {"has_background": false, "has_command_substitution": false, "has_obfuscation": false, "has_pipe": false, "has_redirect": false}, "target_hint": "example.com"},"task_id":"7"}`

// glmPayload 是抓包时喂给规整器的原始动作。
const glmPayload = `{"command":"curl -s https://example.com/x"}`

// newStreamingProvider 起一个把 sse 原样回给客户端的假端点，并把真实的
// openai 格式 provider 指向它。
//
// 走真实 provider（而不是 fakeProvider）是有意的：这样测试会同时覆盖
// SSE 扫描 → 帧解析 → 累加器 → 规整器解码这一整条链路，正是生产上的链路。
func newStreamingProvider(t *testing.T, sse string) llm.Provider {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, sse)
	}))
	t.Cleanup(srv.Close)
	p, err := llm.NewProvider(llm.Config{
		Format:     llm.FormatOpenAI,
		BaseURL:    srv.URL,
		APIKey:     "test-key",
		Model:      "glm-4.5-air",
		HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatalf("构造 provider 失败: %v", err)
	}
	return p
}

// glmStreamWithArgs 按抓包的线格式造一份「工具入参换成 args」的响应，
// 用于派生变体（fail-closed 用例）。
//
// 帧是用 json.Marshal 拼的，不是手写字符串 —— 工具入参本身是一段 JSON
// 字符串，嵌进 SSE 帧时必须再转义一层；手写引号会造出非法帧，帧解析会
// 整帧丢弃，测试就会因为「模型没调工具」而假失败。
func glmStreamWithArgs(t *testing.T, args string) string {
	t.Helper()
	frame := map[string]any{
		"id": "20261102175949373c6ad2342d46d9", "object": "chat.completion.chunk", "model": "glm-5.3-flash",
		"choices": []any{map[string]any{
			"index": 0,
			"delta": map[string]any{"tool_calls": []any{map[string]any{
				"id": "call_1", "index": 0, "type": "function",
				"function": map[string]any{"name": "emit_structured_intent", "arguments": args},
			}}},
		}},
	}
	stop := map[string]any{
		"id": "20261102175949373c6ad2342d46d9", "object": "chat.completion.chunk", "model": "glm-5.3-flash",
		"choices": []any{map[string]any{
			"index": 0, "finish_reason": "tool_calls",
			"delta": map[string]any{"role": "assistant", "content": ""},
		}},
	}
	b1, err := json.Marshal(frame)
	if err != nil {
		t.Fatalf("拼 SSE 帧失败: %v", err)
	}
	b2, err := json.Marshal(stop)
	if err != nil {
		t.Fatalf("拼 SSE 帧失败: %v", err)
	}
	return "data: " + string(b1) + "\n\ndata: " + string(b2) + "\n\ndata: [DONE]\n\n"
}

// newGLMNormalizer 用一个回放指定 SSE 的 provider 造一个规整器。
func newGLMNormalizer(t *testing.T, sse string) *Normalizer {
	t.Helper()
	n, err := NewNormalizer(func(int64) (llm.Provider, bool) {
		return newStreamingProvider(t, sse), true
	}, 0, "你是意图规整器", normalizerSchema())
	if err != nil {
		t.Fatalf("构造规整器失败: %v", err)
	}
	return n
}

// TestSoDNormalizerAcceptsRealGLMStreamingToolCall 是本次生产故障的回归：
//
// 把**真实抓包**的 GLM 流式响应喂给规整器，规整器必须成功解析出结构化意图。
//
// 故障形态：工具入参里 task_id 是字符串 "7"（schema 就是这么声明的，模型
// 照做），而规整器把入参直接反序列化进 NormalizerResult{TaskID int64}
// ⇒ json.Unmarshal 整包失败 ⇒ reason_code=normalizer_malformed，
// SoD 链对每一次调用都拒绝。
func TestSoDNormalizerAcceptsRealGLMStreamingToolCall(t *testing.T) {
	n := newGLMNormalizer(t, glmStreamingResponse)

	got, err := n.Normalize(context.Background(), NormalizeInput{
		ToolName:       "Bash",
		RawPayload:     []byte(glmPayload),
		SourceAgent:    "capture",
		TaskID:         7,
		ConversationID: 11,
	})
	if err != nil {
		t.Fatalf("规整失败（真实 GLM 响应应当被接受）: %v", err)
	}
	if got.StructuredIntent.ActionType != "command_execution" {
		t.Fatalf("action_type = %q, 期望 command_execution", got.StructuredIntent.ActionType)
	}
	if got.StructuredIntent.Executable != "curl" {
		t.Fatalf("executable = %q, 期望 curl", got.StructuredIntent.Executable)
	}
	if got.StructuredIntent.TargetHint != "example.com" {
		t.Fatalf("target_hint = %q, 期望 example.com", got.StructuredIntent.TargetHint)
	}
	if got.StructuredIntent.RawPayloadDigest == "" {
		t.Fatal("raw_payload_digest 为空")
	}
	if got.AnomalyFlag {
		t.Fatal("anomaly_flag = true, 期望 false")
	}
	// 模型给的上下文不算数：规整器用自己的上下文覆盖。
	if got.SourceAgent != "capture" || got.TaskID != 7 {
		t.Fatalf("上下文被模型污染: source_agent=%q task_id=%d", got.SourceAgent, got.TaskID)
	}
}

// TestSoDNormalizerIgnoresModelSuppliedTaskIDType 钉住「模型把 task_id 写成
// 什么类型都不影响裁决」—— task_id / source_agent 是规整器自己的账，模型
// 说的不算数，所以它们的 JSON 类型不该有资格否决一次合法的工具调用。
func TestSoDNormalizerIgnoresModelSuppliedTaskIDType(t *testing.T) {
	for _, taskID := range []string{`"7"`, `7`, `null`, `"abc"`} {
		t.Run(taskID, func(t *testing.T) {
			args := `{"structured_intent":{"action_type":"command_execution","executable":"curl",` +
				`"arguments":["-s","https://example.com/x"],"target_hint":"example.com",` +
				`"shell_features":{"has_pipe":false,"has_redirect":false,"has_command_substitution":false,` +
				`"has_background":false,"has_obfuscation":false},"body_digest":"",` +
				`"raw_payload_digest":"deadbeef"},"anomaly_flag":false,"anomaly_reason":"",` +
				`"source_agent":"capture","task_id":` + taskID + `}`
			n := newGLMNormalizer(t, glmStreamWithArgs(t, args))

			got, err := n.Normalize(context.Background(), NormalizeInput{
				ToolName: "Bash", RawPayload: []byte(glmPayload),
				SourceAgent: "capture", TaskID: 7,
			})
			if err != nil {
				t.Fatalf("规整失败（task_id=%s 不该否决合法工具调用）: %v", taskID, err)
			}
			if got.TaskID != 7 || got.SourceAgent != "capture" {
				t.Fatalf("上下文被模型污染: source_agent=%q task_id=%d", got.SourceAgent, got.TaskID)
			}
			if got.StructuredIntent.ActionType != "command_execution" {
				t.Fatalf("action_type = %q, 期望 command_execution", got.StructuredIntent.ActionType)
			}
		})
	}
}

// TestSoDNormalizerStillRejectsUnusableGLMOutput 钉住 fail-closed 没被放宽：
// 修掉「元数据字段的 JSON 类型否决合法调用」之后，**结构化意图本身**不合规
// 仍然必须拒。
func TestSoDNormalizerStillRejectsUnusableGLMOutput(t *testing.T) {
	cases := []struct {
		name string
		args string
	}{
		{
			name: "没有 structured_intent",
			args: `{"anomaly_flag":false,"source_agent":"capture","task_id":"7"}`,
		},
		{
			name: "action_type 未知",
			args: `{"structured_intent":{"action_type":"delete_everything","raw_payload_digest":"d"},` +
				`"anomaly_flag":false,"task_id":"7"}`,
		},
		{
			name: "action_type 是 unparseable",
			args: `{"structured_intent":{"action_type":"unparseable","raw_payload_digest":"d"},` +
				`"anomaly_flag":false,"task_id":"7"}`,
		},
		{
			name: "缺 raw_payload_digest",
			args: `{"structured_intent":{"action_type":"command_execution","target_hint":"example.com"},` +
				`"anomaly_flag":false,"task_id":"7"}`,
		},
		{
			name: "structured_intent 类型不对",
			args: `{"structured_intent":"example.com","anomaly_flag":false,"task_id":"7"}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n := newGLMNormalizer(t, glmStreamWithArgs(t, tc.args))
			_, err := n.Normalize(context.Background(), NormalizeInput{
				ToolName: "Bash", RawPayload: []byte(glmPayload),
				SourceAgent: "capture", TaskID: 7,
			})
			if !errors.Is(err, ErrNormalizerMalformed) {
				t.Fatalf("err = %v, 期望 ErrNormalizerMalformed", err)
			}
			if !strings.Contains(err.Error(), "不合规") {
				t.Fatalf("err = %v, 期望提到不合规", err)
			}
		})
	}
}

// TestSoDNormalizerCapturedFixtureIsTheRealOne 把抓包原文钉在测试里，防止
// 有人「顺手」把夹具改成模型不会真的产出的形状（那会让回归测试失去意义）。
//
// 其中「帧里的 arguments == glmCapturedArguments」这一条是真的把 SSE 帧解析
// 出来比对，不是子串匹配：两个常量必须来自同一份抓包，不能各写各的。
func TestSoDNormalizerCapturedFixtureIsTheRealOne(t *testing.T) {
	if !strings.Contains(glmStreamingResponse, `"task_id\":\"7\"`) {
		t.Fatalf("抓包夹具里没有 task_id 的字符串形态：%s", glmStreamingResponse)
	}
	if !strings.Contains(glmCapturedArguments, `"task_id":"7"`) {
		t.Fatalf("抓包入参里没有 task_id 的字符串形态：%s", glmCapturedArguments)
	}
	if !strings.Contains(glmStreamingResponse, `"finish_reason":"tool_calls"`) {
		t.Fatal("抓包夹具里没有 finish_reason=tool_calls")
	}
	if !strings.Contains(glmStreamingResponse, "data: [DONE]") {
		t.Fatal("抓包夹具不是完整的 SSE 流")
	}

	first := strings.SplitN(glmStreamingResponse, "\n", 2)[0]
	if !strings.HasPrefix(first, "data: ") {
		t.Fatalf("抓包夹具的第一帧不是 data 行：%s", first)
	}
	var frame struct {
		Choices []struct {
			Delta struct {
				ToolCalls []struct {
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(strings.TrimPrefix(first, "data: ")), &frame); err != nil {
		t.Fatalf("抓包夹具的第一帧不是合法 JSON: %v", err)
	}
	if len(frame.Choices) != 1 || len(frame.Choices[0].Delta.ToolCalls) != 1 {
		t.Fatalf("抓包夹具的第一帧不是「恰好一个 tool_calls」：%s", first)
	}
	call := frame.Choices[0].Delta.ToolCalls[0].Function
	if call.Name != "emit_structured_intent" {
		t.Fatalf("抓包里的工具名 = %q, 期望 emit_structured_intent", call.Name)
	}
	if call.Arguments != glmCapturedArguments {
		t.Fatalf("抓包帧里的 arguments 与 glmCapturedArguments 不一致：\n帧:   %s\n常量: %s",
			call.Arguments, glmCapturedArguments)
	}
}

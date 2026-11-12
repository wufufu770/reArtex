package sod

import (
	"encoding/json"
	"testing"
)

// v0.11.3：规整输入按 schema 矫正——模型把 bool/数字写成字符串是头号 malformed
// 错法（实测 MiniMax："has_background":"false" ⇒ 严格 unmarshal 炸 ⇒ 意图 blocked）。
// 矫正只发生在 schema 声明的 boolean/number 位置；string 字段与无 schema 位置绝不
// 触碰（arguments 里的 "true" 是内容不是类型错——启发式通吃会误伤，单测钉死）。
func miniInputSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"structured_intent": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"action_type": map[string]any{"type": "string"},
					"executable":  map[string]any{"type": "string"},
					"arguments":   map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
					"target_hint": map[string]any{"type": "string"},
					"shell_features": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"has_pipe":                 map[string]any{"type": "boolean"},
							"has_redirect":             map[string]any{"type": "boolean"},
							"has_command_substitution": map[string]any{"type": "boolean"},
							"has_background":           map[string]any{"type": "boolean"},
							"has_obfuscation":          map[string]any{"type": "boolean"},
						},
					},
				},
			},
			"anomaly_flag": map[string]any{"type": "boolean"},
		},
	}
}

func TestLoosenAgainstSchemaFixesBooleans(t *testing.T) {
	in := json.RawMessage(`{"structured_intent":{"action_type":"bash","executable":"curl","arguments":["-c","true"],"target_hint":"https://x.example.com","shell_features":{"has_pipe":"true","has_background":"True","has_obfuscation":false}},"anomaly_flag":"false"}`)
	out, changed := loosenAgainstSchema(in, miniInputSchema())
	if !changed {
		t.Fatal("应检测到需要矫正的 boolean 字段")
	}
	var raw normalizerToolOutput
	if err := json.Unmarshal(out, &raw); err != nil {
		t.Fatalf("矫正后应可正常反序列化: %v", err)
	}
	sf := raw.StructuredIntent.ShellFeatures
	if !sf.HasPipe || !sf.HasBackground || sf.HasObfuscation {
		t.Fatalf("boolean 字段应被矫正: %+v", sf)
	}
	if raw.AnomalyFlag {
		t.Fatal("顶层 anomaly_flag 输入 false，矫正后应为 false")
	}
	// string 字段与 string 数组绝不被误伤：
	if len(raw.StructuredIntent.Arguments) != 2 || raw.StructuredIntent.Arguments[1] != "true" {
		t.Fatalf("arguments 数组里的字符串 true 是内容不是类型错，不得矫正: %v", raw.StructuredIntent.Arguments)
	}
	if raw.StructuredIntent.TargetHint != "https://x.example.com" {
		t.Fatalf("string 字段不得被误伤: %q", raw.StructuredIntent.TargetHint)
	}
	if raw.StructuredIntent.ActionType != "bash" {
		t.Fatalf("语义字段应完整保留: %+v", raw.StructuredIntent)
	}
}

// 端到端：历史 malformed 样本（MiniMax 实测）矫正后可完整反序列化。
func TestLoosenFixesHistoricalMalformed(t *testing.T) {
	historical := json.RawMessage(`{"structured_intent":{"action_type":"bash","executable":"curl","arguments":["-sS","https://t/"],"target_hint":"https://t/","shell_features":{"has_pipe":"false","has_redirect":"false","has_command_substitution":"false","has_background":"false","has_obfuscation":"false"},"body_digest":"d","raw_payload_digest":"r"},"anomaly_flag":"true","anomaly_reason":"x"}`)
	var raw normalizerToolOutput
	if err := json.Unmarshal(historical, &raw); err == nil {
		t.Skip("该样本当前已可解析（无需矫正）")
	}
	loosened, changed := loosenAgainstSchema(historical, miniInputSchema())
	if !changed {
		t.Fatal("历史 malformed 样本应触发矫正")
	}
	if err := json.Unmarshal(loosened, &raw); err != nil {
		t.Fatalf("矫正后应可解析: %v", err)
	}
	if raw.StructuredIntent.ActionType != "bash" {
		t.Fatalf("语义字段应完整保留: %+v", raw.StructuredIntent)
	}
}

// 干净输入不触发；非法 JSON 原样返回。
func TestLoosenNoChangeOnCleanInput(t *testing.T) {
	in := json.RawMessage(`{"a":"keep","b":true,"c":42}`)
	_, changed := loosenAgainstSchema(in, miniInputSchema())
	if changed {
		t.Fatal("干净输入不应触发矫正")
	}
	bad := json.RawMessage(`{"a":`)
	if _, changed := loosenAgainstSchema(bad, miniInputSchema()); changed {
		t.Fatal("非法 JSON 应原样返回")
	}
}

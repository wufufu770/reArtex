package llm

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestBuildAnthropicSystemCacheBoundary(t *testing.T) {
	sys := []string{"a", "b", "c"}

	cases := []struct {
		name       string
		boundary   int
		wantBlocks int
		wantCached []bool // per-block: has cache_control
	}{
		{"no boundary (0) → single uncached", 0, 1, []bool{false}},
		{"negative → single uncached", -1, 1, []bool{false}},
		{"mid split → cached prefix + dynamic tail", 2, 2, []bool{true, false}},
		{"boundary == len → whole thing cached", 3, 1, []bool{true}},
		{"boundary > len → whole thing cached", 5, 1, []bool{true}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			blocks := buildAnthropicSystem(CompletionRequest{System: sys, DynamicBoundary: c.boundary})
			if len(blocks) != c.wantBlocks {
				t.Fatalf("blocks=%d want %d", len(blocks), c.wantBlocks)
			}
			for i, wantCached := range c.wantCached {
				gotCached := blocks[i].CacheControl != nil
				if gotCached != wantCached {
					t.Fatalf("block %d cached=%v want %v", i, gotCached, wantCached)
				}
			}
		})
	}
}

// TestWithTailCacheBreakpoint（0.15.12）：CacheMessages 开启时，断点只落在最后一条
// 消息的最后一个内容块上；copy-on-write——输入切片与消息对象绝不被原地改动
// （同 stripThoughtSignatures 纪律：它们属于调用方会话状态）。
func TestWithTailCacheBreakpoint(t *testing.T) {
	msgs := []Message{
		UserText("q1"),
		{Role: RoleAssistant, Content: []ContentBlock{TextBlock("a1")}},
		{Role: RoleUser, Content: []ContentBlock{TextBlock("q2"), ToolResultText("t1", "out", false)}},
	}
	before, err := json.Marshal(msgs)
	if err != nil {
		t.Fatal(err)
	}

	out := withTailCacheBreakpoint(msgs)
	if len(out) != len(msgs) {
		t.Fatalf("len=%d want %d", len(out), len(msgs))
	}
	last := out[len(out)-1]
	for i, b := range last.Content {
		marked := b.CacheControl != nil
		want := i == len(last.Content)-1
		if marked != want {
			t.Fatalf("block %d marked=%v want %v", i, marked, want)
		}
	}
	for i := range out[:len(out)-1] {
		if len(out[i].Content) > 0 && out[i].Content[0].CacheControl != nil {
			t.Fatalf("message %d unexpectedly marked", i)
		}
	}
	// 输入未被改动：调用方会话状态必须原样存活。
	after, err := json.Marshal(msgs)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("input messages were mutated")
	}
}

func TestWithTailCacheBreakpointEmpty(t *testing.T) {
	cases := [][]Message{
		nil,
		{},
		{{Role: RoleUser, Content: nil}},
	}
	for i, msgs := range cases {
		out := withTailCacheBreakpoint(msgs)
		if !reflect.DeepEqual(out, msgs) {
			t.Fatalf("case %d: empty history must pass through unchanged", i)
		}
	}
}

// TestBuildBodyCacheMessagesFlag：开关关闭时请求体不含任何消息级 cache_control；
// 开启时恰好一个（最后一条消息的最后一个块）。system 断点不受开关影响。
func TestBuildBodyCacheMessagesFlag(t *testing.T) {
	req := CompletionRequest{
		System:          []string{"static"},
		DynamicBoundary: 1,
		Messages:        []Message{UserText("hi")},
	}
	off := anthropicProvider{cfg: Config{Format: FormatAnthropic, Model: "m", APIKey: "k"}}
	body, err := off.buildBody(req, false)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		System []struct {
			CacheControl map[string]any `json:"cache_control"`
		} `json:"system"`
		Messages []struct {
			Content []map[string]any `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.System) != 1 || decoded.System[0].CacheControl == nil {
		t.Fatal("system breakpoint must exist regardless of CacheMessages")
	}
	if len(decoded.Messages) != 1 || decoded.Messages[0].Content[0]["cache_control"] != nil {
		t.Fatal("CacheMessages off: no message-level breakpoint expected")
	}

	on := anthropicProvider{cfg: Config{Format: FormatAnthropic, Model: "m", APIKey: "k", CacheMessages: true}}
	body, err = on.buildBody(req, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Messages) != 1 || decoded.Messages[0].Content[0]["cache_control"] == nil {
		t.Fatal("CacheMessages on: expected a breakpoint on the last message block")
	}
}

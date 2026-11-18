package agent

// 0.15.13：applyThinkingOverride 的三处修正回归——① "" 不覆盖（返回全空时调用
// 方预设的 Thinking/ReasoningEffort 必须原样存活）；② nil opts 安全；③ 未装配
// 解析器时零改动。旧实现无条件赋值，返回全空会把预设清掉（与注释契约矛盾）。

import (
	"testing"

	"github.com/Autumn-27/norma/agentcore"
)

func TestApplyThinkingOverrideRespectsEmpty(t *testing.T) {
	fn := func(string) (string, string) { return "", "" }
	SetThinkingOverride(fn)
	t.Cleanup(func() { SetThinkingOverride(nil) })

	opts := &agentcore.Options{Thinking: "enabled", ReasoningEffort: "high"}
	applyThinkingOverride(opts, "worker")
	if opts.Thinking != "enabled" || opts.ReasoningEffort != "high" {
		t.Fatalf("全空返回不得清掉预设: thinking=%q effort=%q", opts.Thinking, opts.ReasoningEffort)
	}

	// 只覆盖一侧：另一侧保持
	SetThinkingOverride(func(string) (string, string) { return "disabled", "" })
	opts = &agentcore.Options{Thinking: "enabled", ReasoningEffort: "high"}
	applyThinkingOverride(opts, "worker")
	if opts.Thinking != "disabled" || opts.ReasoningEffort != "high" {
		t.Fatalf("switch 侧应覆盖、effort 侧应保留: thinking=%q effort=%q", opts.Thinking, opts.ReasoningEffort)
	}
	SetThinkingOverride(func(string) (string, string) { return "", "low" })
	opts = &agentcore.Options{Thinking: "enabled", ReasoningEffort: "high"}
	applyThinkingOverride(opts, "worker")
	if opts.Thinking != "enabled" || opts.ReasoningEffort != "low" {
		t.Fatalf("effort 侧应覆盖、switch 侧应保留: thinking=%q effort=%q", opts.Thinking, opts.ReasoningEffort)
	}
}

func TestApplyThinkingOverrideNilSafety(t *testing.T) {
	// nil opts 不 panic。
	applyThinkingOverride(nil, "worker")
	// 未装配解析器：零改动。
	opts := &agentcore.Options{Thinking: "enabled"}
	applyThinkingOverride(opts, "worker")
	if opts.Thinking != "enabled" || opts.ReasoningEffort != "" {
		t.Fatalf("未装配时不得改动: %+v", opts)
	}
}

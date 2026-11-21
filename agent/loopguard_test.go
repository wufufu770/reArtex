package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/Autumn-27/norma/llm"
)

// loopguard 单测（0.15.34）：3× 硬拦 + 消息升级纪律钉死。
func TestLoopGuardBlocksThirdIdenticalCall(t *testing.T) {
	g := NewLoopGuard()
	input := []byte(`{"command":"curl http://x","flag":1,"ts":123}`) // ts 在值里，签名含它
	v := g.Observe("Bash", input)
	if v.Block {
		t.Fatal("第 1 次不应拦")
	}
	v = g.Observe("Bash", input)
	if v.Block {
		t.Fatal("第 2 次不应拦")
	}
	v = g.Observe("Bash", input)
	if !v.Block {
		t.Fatal("第 3 次相同调用应拦")
	}
	if !strings.Contains(v.Message, "换方法") {
		t.Fatalf("拦截消息应给换策略指引: %q", v.Message)
	}
}

func TestLoopGuardSignatureIsParameterOrderInsensitive(t *testing.T) {
	// 键序不同的等价参数 → 同一签名 → 计数累计（键序变换不构成「换了方法」）。
	g := NewLoopGuard()
	a := []byte(`{"command":"curl http://x","flag":1}`)
	b := []byte(`{"flag":1,"command":"curl http://x"}`)
	g.Observe("Bash", a)
	g.Observe("Bash", b)
	v := g.Observe("Bash", a)
	if !v.Block {
		t.Fatal("键序不同的等价参数应计入同一签名并触发拦截")
	}
}

func TestLoopGuardDifferentCallsNotBlocked(t *testing.T) {
	// 换参数 = 新签名：渗透工作流的正常迭代不受影响。
	g := NewLoopGuard()
	base := []byte(`{"command":"curl http://x?id=1"}`)
	for i := 2; i <= 10; i++ {
		in := []byte(`{"command":"curl http://x?id=` + string(rune('0'+i)) + `"}`)
		_ = base
		if v := g.Observe("Bash", in); v.Block {
			t.Fatalf("不同参数不应被拦（i=%d）", i)
		}
	}
}

func TestLoopGuardEscalatesAfterRepeatedBlocks(t *testing.T) {
	g := NewLoopGuard()
	in := []byte(`{"command":"same"} `)
	in = []byte(`{"command":"same"}`)
	var last LoopVerdict
	for i := 0; i < 6; i++ {
		last = g.Observe("Bash", in)
	}
	if !last.Block {
		t.Fatal("持续重复应持续拦截")
	}
	if !strings.Contains(last.Message, "收尾") {
		t.Fatalf("累计拦截后消息应升级为收尾指引: %q", last.Message)
	}
}

func TestLoopGuardRunnerPassesThroughToInner(t *testing.T) {
	// 未达阈值时透传内层：内层的拒绝结果原样上抛，loopguard 不改写不吞掉。
	inner := &stubHookRunner{blockMsg: "sod deny"}
	r := NewLoopGuardRunner(inner, NewLoopGuard())
	block, msg, _ := r.PreToolUse(t.Context(), "Bash", []byte(`{"command":"a"}`))
	if !block || msg != "sod deny" {
		t.Fatalf("内层拒绝应原样透传（loopguard 未拦）: %v %q", block, msg)
	}
	// 第二次相同调用：loopguard 仍未达 3×，仍透传内层（内层被调两次）。
	block, msg, _ = r.PreToolUse(t.Context(), "Bash", []byte(`{"command":"a"}`))
	if !block || msg != "sod deny" || inner.calls != 2 {
		t.Fatalf("透传应继续到内层（calls=%d）: %v %q", inner.calls, block, msg)
	}
}

func TestLoopGuardRunnerBlocksBeforeInner(t *testing.T) {
	inner := &stubHookRunner{}
	r := NewLoopGuardRunner(inner, NewLoopGuard())
	for i := 0; i < 3; i++ {
		r.PreToolUse(t.Context(), "Bash", []byte(`{"command":"loop"}`))
	}
	block, msg, _ := r.PreToolUse(t.Context(), "Bash", []byte(`{"command":"loop"}`))
	if !block || !strings.Contains(msg, "loopguard") {
		t.Fatalf("阈值达到后应拦且消息来自 loopguard: %v %q", block, msg)
	}
}

// stubHookRunner 是测试桩（记录调用并可返回固定拦截消息）。
type stubHookRunner struct {
	blockMsg string
	calls    int
}

func (s *stubHookRunner) PreToolUse(_ context.Context, _ string, _ []byte) (bool, string, []byte) {
	s.calls++
	if s.blockMsg != "" {
		return true, s.blockMsg, nil
	}
	return false, "", nil
}

func (s *stubHookRunner) PostToolUse(context.Context, string, []byte, []byte, bool) {}

func (s *stubHookRunner) Stop(context.Context, []llm.Message) (bool, []string, string) {
	return false, nil, ""
}

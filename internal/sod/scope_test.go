package sod

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// TestSoDManagedScopeOf 钉住「这次调用在不在 SoD 链的管辖范围内」的判据。
//
// 判据只有一条：ctx 里带了 SoD 运行上下文（task_id>0 且 agent_key 非空）。
//
// 为什么只有 worker 路径会带：agent 包里的 AugmentTools 是 worker / planner /
// mainagent / chat **共用**的，而 SoD 链只挂在 worker 的 hook 上（见
// server/engine.go 的装配点）。chat / planner / mainagent 的 ctx 里既没有
// 任务号也没有 agent key ⇒ 它们不在管辖范围内。
func TestSoDManagedScopeOf(t *testing.T) {
	cases := []struct {
		name string
		ctx  context.Context
		want bool
	}{
		{
			name: "完全没有运行上下文",
			ctx:  context.Background(),
			want: false,
		},
		{
			name: "有任务号但没有 agent key",
			ctx:  WithRunContext(context.Background(), RunContext{TaskID: 7}),
			want: false,
		},
		{
			name: "有 agent key 但没有任务号",
			ctx:  WithRunContext(context.Background(), RunContext{AgentKey: "worker"}),
			want: false,
		},
		{
			name: "任务号非正",
			ctx:  WithRunContext(context.Background(), RunContext{TaskID: 0, AgentKey: "worker"}),
			want: false,
		},
		{
			name: "agent key 只有空白",
			ctx:  WithRunContext(context.Background(), RunContext{TaskID: 7, AgentKey: "   "}),
			want: false,
		},
		{
			name: "任务号与 agent key 都在",
			ctx:  WithRunContext(context.Background(), RunContext{TaskID: 7, AgentKey: "worker"}),
			want: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			run, managed := ManagedScopeOf(tc.ctx)
			if managed != tc.want {
				t.Fatalf("managed=%v, 期望 %v", managed, tc.want)
			}
			if managed {
				if run.TaskID <= 0 || strings.TrimSpace(run.AgentKey) == "" {
					t.Fatalf("managed=true 但运行上下文不完整: %+v", run)
				}
			}
		})
	}
}

// TestSoDExecutorAllowsUnmanagedCallWithoutCredential 钉住「非受管路径上的
// 工具调用**不索要执行凭证**」。
//
// 为什么这是生死线：执行器装饰器装在**所有** agent 的工具上（agent 包的
// AugmentTools 是 worker / planner / mainagent / chat 共用的），而 SoD 链只
// 挂在 worker 的 hook 上。若执行器在非受管路径上也索要凭证，chat / planner /
// mainagent 的每一次工具调用都会被拒 ⇒ 整个平台瘫掉（fail-closed 用错了地方：
// 链根本没挂在那些路径上，没人铸凭证，要凭证等于要求一把永远不存在的钥匙）。
func TestSoDExecutorAllowsUnmanagedCallWithoutCredential(t *testing.T) {
	inner := &countingTool{name: "Bash"}
	// 连凭证表都没装：非受管路径上依然必须放行。
	gate := WrapExecutor(inner, nil, true)

	res, err := gate.Call(context.Background(), json.RawMessage(`{"command":"ls"}`), nil)
	if err != nil {
		t.Fatalf("期望不返回 Go error, got %v", err)
	}
	if res.IsError {
		t.Fatalf("非受管路径上的调用被拒: %s", res.Flatten())
	}
	if inner.calls != 1 {
		t.Fatalf("工具被调用了 %d 次, 期望 1 次", inner.calls)
	}
}

// TestSoDExecutorAllowsUnmanagedCallWithTokenStore 钉住「非受管路径不因
// 「表里恰好有别人的凭证」而改变行为」。
//
// 场景：同一个进程里 worker 的凭证表里躺着别的凭证；chat 的调用走的是同一张
// 表，但它**不该**去消费 —— 它本来就不受管，消费与否都不该影响放行。
func TestSoDExecutorAllowsUnmanagedCallWithTokenStore(t *testing.T) {
	inner := &countingTool{name: "Bash"}
	store := NewTokenStore()
	gate := WrapExecutor(inner, store, true)

	res, err := gate.Call(context.Background(), json.RawMessage(`{"command":"ls"}`), nil)
	if err != nil {
		t.Fatalf("期望不返回 Go error, got %v", err)
	}
	if res.IsError {
		t.Fatalf("非受管路径上的调用被拒: %s", res.Flatten())
	}
	if inner.calls != 1 {
		t.Fatalf("工具被调用了 %d 次, 期望 1 次", inner.calls)
	}
}

// TestSoDExecutorStillRefusesManagedCallWithoutCredential 钉住「受管路径上
// 缺凭证依然拒绝」—— 修「非受管被误拒」不能顺手把「受管被漏拒」也修掉。
func TestSoDExecutorStillRefusesManagedCallWithoutCredential(t *testing.T) {
	inner := &countingTool{name: "Bash"}
	gate := WrapExecutor(inner, nil, true)

	res, err := gate.Call(executorCtx(7, "worker"), json.RawMessage(`{"command":"ls"}`), nil)
	if err != nil {
		t.Fatalf("期望返回错误结果而不是 Go error, got %v", err)
	}
	if !res.IsError {
		t.Fatal("受管路径上缺凭证竟然放行")
	}
	if inner.calls != 0 {
		t.Fatalf("工具被调用了 %d 次, 期望 0 次", inner.calls)
	}
}

// TestSoDExecutorUnmanagedIgnoresHostContextReader 钉住「宿主换了读法也不改变
// 受管判定」：受管与否只看运行上下文里有没有任务号与 agent key。
func TestSoDExecutorUnmanagedIgnoresHostContextReader(t *testing.T) {
	SetContextReader(func(ctx context.Context) RunContext {
		return RunContext{TaskID: 0}
	})
	t.Cleanup(func() { SetContextReader(nil) })

	inner := &countingTool{name: "Bash"}
	gate := WrapExecutor(inner, nil, true)

	res, err := gate.Call(context.Background(), json.RawMessage(`{"command":"ls"}`), nil)
	if err != nil {
		t.Fatalf("期望不返回 Go error, got %v", err)
	}
	if res.IsError {
		t.Fatalf("非受管路径上的调用被拒: %s", res.Flatten())
	}
	if inner.calls != 1 {
		t.Fatalf("工具被调用了 %d 次, 期望 1 次", inner.calls)
	}
}

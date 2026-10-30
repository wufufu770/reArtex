package sod

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	acperm "github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
)

// countingTool 记录自己被调用了几次。
type countingTool struct {
	name   string
	calls  int
	inputs []json.RawMessage
}

func (t *countingTool) Name() string        { return t.name }
func (t *countingTool) Description() string { return "测试工具" }
func (t *countingTool) Prompt() string      { return "" }
func (t *countingTool) InputSchema() map[string]any {
	return map[string]any{"type": "object"}
}
func (t *countingTool) IsReadOnly(json.RawMessage) bool       { return false }
func (t *countingTool) IsConcurrencySafe(json.RawMessage) bool { return true }
func (t *countingTool) CheckPermissions(context.Context, json.RawMessage, acperm.Context) acperm.Decision {
	return acperm.Allowed()
}
func (t *countingTool) Call(ctx context.Context, in json.RawMessage, tc *actool.ToolContext) (actool.Result, error) {
	t.calls++
	t.inputs = append(t.inputs, in)
	return actool.Text("ok"), nil
}

// executorCtx 造一个带运行上下文的 ctx（链与执行器靠它对齐四元组）。
func executorCtx(taskID int64, agentKey string) context.Context {
	return WithRunContext(context.Background(), RunContext{TaskID: taskID, AgentKey: agentKey})
}

func TestSoDExecutorRefusesWithoutApproval(t *testing.T) {
	inner := &countingTool{name: "Bash"}
	gate := WrapExecutor(inner, NewTokenStore(), true)

	res, err := gate.Call(executorCtx(7, "worker"), json.RawMessage(`{"command":"ls"}`), nil)
	if err != nil {
		t.Fatalf("期望返回错误结果而不是 Go error, got %v", err)
	}
	if !res.IsError {
		t.Fatal("期望 IsError=true")
	}
	if inner.calls != 0 {
		t.Fatalf("工具被调用了 %d 次, 期望 0 次", inner.calls)
	}
	if !strings.Contains(res.Flatten(), "未被 SoD 链批准") {
		t.Fatalf("错误消息 = %q, 期望说明未获批准", res.Flatten())
	}
}

func TestSoDExecutorConsumesTokenOnce(t *testing.T) {
	inner := &countingTool{name: "Bash"}
	store := NewTokenStore()
	gate := WrapExecutor(inner, store, true)

	payload := json.RawMessage(`{"command":"ls"}`)
	digest := CanonicalDigest(payload)
	if _, _, err := store.Mint(digest, 7, "worker", "Bash"); err != nil {
		t.Fatalf("铸凭证失败: %v", err)
	}

	res, err := gate.Call(executorCtx(7, "worker"), payload, nil)
	if err != nil {
		t.Fatalf("第一次调用出错: %v", err)
	}
	if res.IsError {
		t.Fatalf("第一次调用被拒: %s", res.Flatten())
	}
	if inner.calls != 1 {
		t.Fatalf("工具被调用了 %d 次, 期望 1 次", inner.calls)
	}

	// 同一枚凭证再来一次 ⇒ 查不到 ⇒ 拒绝 ⇒ 重放失败。
	res, err = gate.Call(executorCtx(7, "worker"), payload, nil)
	if err != nil {
		t.Fatalf("第二次调用出错: %v", err)
	}
	if !res.IsError {
		t.Fatal("重放没有被拒绝")
	}
	if inner.calls != 1 {
		t.Fatalf("重放时工具被调用了, 累计 %d 次, 期望仍是 1 次", inner.calls)
	}
}

func TestSoDExecutorRefusesOnDigestMismatch(t *testing.T) {
	inner := &countingTool{name: "Bash"}
	store := NewTokenStore()
	gate := WrapExecutor(inner, store, true)

	approved := json.RawMessage(`{"command":"ls"}`)
	if _, _, err := store.Mint(CanonicalDigest(approved), 7, "worker", "Bash"); err != nil {
		t.Fatalf("铸凭证失败: %v", err)
	}

	// 执行的是**另一份**字节 ⇒ 摘要对不上 ⇒ 拒绝。
	tampered := json.RawMessage(`{"command":"rm -rf /"}`)
	res, err := gate.Call(executorCtx(7, "worker"), tampered, nil)
	if err != nil {
		t.Fatalf("期望返回错误结果而不是 Go error, got %v", err)
	}
	if !res.IsError {
		t.Fatal("摘要不匹配没有被拒绝")
	}
	if inner.calls != 0 {
		t.Fatalf("工具被调用了 %d 次, 期望 0 次", inner.calls)
	}
}

func TestSoDExecutorRefusesOnCrossTaskUse(t *testing.T) {
	inner := &countingTool{name: "Bash"}
	store := NewTokenStore()
	gate := WrapExecutor(inner, store, true)

	payload := json.RawMessage(`{"command":"ls"}`)
	if _, _, err := store.Mint(CanonicalDigest(payload), 7, "worker", "Bash"); err != nil {
		t.Fatalf("铸凭证失败: %v", err)
	}

	// 换一个任务 ⇒ 四元组对不上 ⇒ 不可转让。
	res, err := gate.Call(executorCtx(8, "worker"), payload, nil)
	if err != nil {
		t.Fatalf("期望返回错误结果而不是 Go error, got %v", err)
	}
	if !res.IsError {
		t.Fatal("跨任务使用没有被拒绝")
	}
	if inner.calls != 0 {
		t.Fatalf("工具被调用了 %d 次, 期望 0 次", inner.calls)
	}
}

func TestSoDExecutorRefusesOnCrossAgentUse(t *testing.T) {
	inner := &countingTool{name: "Bash"}
	store := NewTokenStore()
	gate := WrapExecutor(inner, store, true)

	payload := json.RawMessage(`{"command":"ls"}`)
	if _, _, err := store.Mint(CanonicalDigest(payload), 7, "worker", "Bash"); err != nil {
		t.Fatalf("铸凭证失败: %v", err)
	}

	res, err := gate.Call(executorCtx(7, "other_agent"), payload, nil)
	if err != nil {
		t.Fatalf("期望返回错误结果而不是 Go error, got %v", err)
	}
	if !res.IsError {
		t.Fatal("跨 agent 使用没有被拒绝")
	}
	if inner.calls != 0 {
		t.Fatalf("工具被调用了 %d 次, 期望 0 次", inner.calls)
	}
}

// TestSoDExecutorSkipsReentrantCalls 钉住「规整器自己的调用不递归进链」。
func TestSoDExecutorSkipsReentrantCalls(t *testing.T) {
	inner := &countingTool{name: "Bash"}
	store := NewTokenStore()
	gate := WrapExecutor(inner, store, true)

	ctx := withReentrancy(executorCtx(7, "worker"))
	res, err := gate.Call(ctx, json.RawMessage(`{"command":"ls"}`), nil)
	if err != nil {
		t.Fatalf("重入调用出错: %v", err)
	}
	if res.IsError {
		t.Fatalf("重入调用被拒: %s", res.Flatten())
	}
	if inner.calls != 1 {
		t.Fatalf("工具被调用了 %d 次, 期望 1 次", inner.calls)
	}
}

// TestSoDExecutorRefusesWhenTokenStoreMissing 钉住「凭证存储没装配 ⇒ 一律拒绝」。
func TestSoDExecutorRefusesWhenTokenStoreMissing(t *testing.T) {
	inner := &countingTool{name: "Bash"}
	gate := WrapExecutor(inner, nil, true)

	res, err := gate.Call(executorCtx(7, "worker"), json.RawMessage(`{"command":"ls"}`), nil)
	if err != nil {
		t.Fatalf("期望返回错误结果而不是 Go error, got %v", err)
	}
	if !res.IsError {
		t.Fatal("凭证存储缺失时竟然放行")
	}
	if inner.calls != 0 {
		t.Fatalf("工具被调用了 %d 次, 期望 0 次", inner.calls)
	}
}

// TestSoDExecutorSkipsNotGatedTools 钉住「放行清单里的工具不套装饰器」。
func TestSoDExecutorSkipsNotGatedTools(t *testing.T) {
	inner := &countingTool{name: "Read"}
	gate := WrapExecutor(inner, NewTokenStore(), false)
	if _, ok := gate.(*ExecutorGate); ok {
		t.Fatal("放行清单里的工具不该套执行器装饰器")
	}
	res, err := gate.Call(executorCtx(7, "worker"), json.RawMessage(`{"path":"/tmp/x"}`), nil)
	if err != nil {
		t.Fatalf("调用出错: %v", err)
	}
	if res.IsError {
		t.Fatalf("放行清单里的工具被拒: %s", res.Flatten())
	}
	if inner.calls != 1 {
		t.Fatalf("工具被调用了 %d 次, 期望 1 次", inner.calls)
	}
}

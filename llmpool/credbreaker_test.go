package llmpool

import (
	"context"
	"errors"
	"testing"

	"github.com/Autumn-27/artex/internal/credbreaker"
	"github.com/Autumn-27/norma/llm"
)

// A-13 集成：凭据熔断器接进轮询链后的行为。
//
// 401 是池 health 的硬失败（第一次即熔断冷却），所以「连续两次 401」在真实时间线
// 上是「冷却到期后再探测又 401」。用例用 health.Reset 模拟冷却到期，验证凭据
// 维度相对池熔断的增量：跨冷却探测累计 401，达到 2 次后 30 分钟长窗断路——
// 期间即使 health 冷却结束、链上其他凭据不可用，该成员也被 order() 跳过，
// 不再撞第三次锁。
func TestPoolCredBreakerSkipsTrippedMember(t *testing.T) {
	bad := &fakeProv{name: "bad", errs: []error{errors.New("x: status 401: unauthorized")}}
	good := &fakeProv{name: "good"}

	cb := credbreaker.New(nil, nil, nil)
	bad1 := &Member{ID: 1, Name: "bad", Model: "m", Rank: 10, Host: "api.example.com", Prov: bad}
	good1 := &Member{ID: 2, Name: "good", Model: "m", Rank: 5, Host: "backup.example.com", Prov: good}
	p := New([]*Member{bad1, good1}, nil, cb)
	req := llm.CompletionRequest{}

	// 第 1 次探测：坏凭据 401（health 硬熔断），failover 到好凭据。
	if _, _, _, err := p.Complete(context.Background(), req); err != nil {
		t.Fatalf("第 1 次调用应 failover 成功: %v", err)
	}
	if bad.calls != 1 {
		t.Fatalf("用例前提不成立：坏凭据应被探测 1 次，实际 %d", bad.calls)
	}
	// 模拟 health 冷却到期（或重启后恢复），链重新包含坏凭据。
	p.health.Reset(bad1.ID)

	// 第 2 次探测：又是 401 —— 凭据维度累计达到 2 → 30 分钟断路。
	_, _, _, _ = p.Complete(context.Background(), req)
	key := credbreaker.Key{ProfileID: 1, Host: "api.example.com"}
	if cb.Allow(key) {
		t.Fatal("跨冷却探测累计两次 401 后凭据应断路")
	}

	// 第 3 次探测前再放开 health：断路与否只由凭据维度决定。
	p.health.Reset(bad1.ID)
	if _, _, _, err := p.Complete(context.Background(), req); err != nil {
		t.Fatalf("第三次调用应直接用好凭据成功: %v", err)
	}
	if bad.calls != 2 {
		t.Fatalf("凭据断路期间坏凭据不应再被外发调用，实际 %d 次", bad.calls)
	}
	if good.calls != 3 {
		t.Fatalf("好凭据应被调用 3 次，实际 %d 次", good.calls)
	}
}

// 成功即复位：坏凭据 401 一次后恢复正常，计数应在成功时清零——
// 之后单独一次 401 不应触发断路。
func TestPoolCredBreakerResetsOnSuccess(t *testing.T) {
	prov := &fakeProv{name: "p", errs: []error{
		errors.New("x: status 401: unauthorized"),
		nil, // 第二次调用成功
		errors.New("x: status 401: unauthorized"),
		errors.New("x: status 401: unauthorized"),
	}}
	cb := credbreaker.New(nil, nil, nil)
	m := &Member{ID: 1, Name: "p", Model: "m", Rank: 1, Host: "api.example.com", Prov: prov}
	p := New([]*Member{m}, nil, cb)
	req := llm.CompletionRequest{}

	// 第 1 次：401 → failover 无后备成员 → 返回 ErrExhausted，但计数已 +1。
	_, _, _, _ = p.Complete(context.Background(), req)
	// 第 2 次：成功 → 计数复位。
	// 第 3、4 次：两次 401 —— 若第 2 次没复位，这里会立刻断路（计数=3）。
	// 复位正确时第 4 次恰好达到 2 次才断路。
	for i := 0; i < 3; i++ {
		_, _, _, _ = p.Complete(context.Background(), req)
	}
	key := credbreaker.Key{ProfileID: 1, Host: "api.example.com"}
	if !cb.Snapshot()[key].Open() {
		t.Fatalf("两次连续 401（成功复位后）应恰好断路，实际状态 %+v", cb.Snapshot()[key])
	}
	if prov.calls != 4 {
		t.Fatalf("用例前提不成立：应共调用 4 次，实际 %d", prov.calls)
	}
}

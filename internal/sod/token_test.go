package sod

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"
)

// TestSoDTokenIsSingleUse 钉住「一次批准 ⇒ 一次执行」：消费即删除，第二次拿
// 同一个 token 必然查不到 ⇒ 重放失败。
func TestSoDTokenIsSingleUse(t *testing.T) {
	store := NewTokenStore()
	token, digest, err := store.Mint("digest-a", 42, "worker", "Bash")
	if err != nil {
		t.Fatalf("铸造凭证失败: %v", err)
	}
	if token == "" || digest == "" {
		t.Fatal("凭证本体与摘要都不能为空")
	}
	if token == digest {
		t.Fatal("摘要不能与凭证本体相同")
	}
	sum := sha256.Sum256([]byte(token))
	if want := hex.EncodeToString(sum[:]); digest != want {
		t.Fatalf("digest=%q want %q（审计里只记摘要）", digest, want)
	}

	if !store.Consume(token, "digest-a", 42, "worker", "Bash") {
		t.Fatal("第一次消费应当成功")
	}
	if store.Consume(token, "digest-a", 42, "worker", "Bash") {
		t.Fatal("第二次消费必须失败（重放）")
	}
}

// TestSoDTokenRejectsPayloadDigestMismatch 钉住「凭证绑定的是将要执行的那份
// 字节」：为载荷 A 铸的凭证不能授权载荷 B。
//
// 并且：校验失败**不删除**凭证 —— 否则真正的持有者（算对了摘要的那一侧）就再也
// 用不了了。
func TestSoDTokenRejectsPayloadDigestMismatch(t *testing.T) {
	store := NewTokenStore()
	token, _, err := store.Mint("digest-a", 42, "worker", "Bash")
	if err != nil {
		t.Fatalf("铸造凭证失败: %v", err)
	}
	if store.Consume(token, "digest-b", 42, "worker", "Bash") {
		t.Fatal("摘要不符时必须拒绝")
	}
	if !store.Consume(token, "digest-a", 42, "worker", "Bash") {
		t.Fatal("校验失败不该删掉凭证（真正的持有者还得能用）")
	}
}

// TestSoDTokenNotTransferableAcrossTasks 钉住不可转让：凭证绑定
// (task_id, source_agent, tool_name)，任一维度对不上都必须拒绝。
func TestSoDTokenNotTransferableAcrossTasks(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		taskID                int64
		sourceAgent, toolName string
	}{
		{name: "任务不同", taskID: 43, sourceAgent: "worker", toolName: "Bash"},
		{name: "agent 不同", taskID: 42, sourceAgent: "other", toolName: "Bash"},
		{name: "工具不同", taskID: 42, sourceAgent: "worker", toolName: "WebFetch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := NewTokenStore()
			token, _, err := store.Mint("digest-a", 42, "worker", "Bash")
			if err != nil {
				t.Fatalf("铸造凭证失败: %v", err)
			}
			if store.Consume(token, "digest-a", tc.taskID, tc.sourceAgent, tc.toolName) {
				t.Fatal("跨维度使用必须被拒绝")
			}
		})
	}
}

// TestSoDTokenSweepRemovesExpired 钉住 TTL 的清扫：过期凭证被清掉 ⇒ 之后消费
// 失败（批准了但没执行的窗口被兜住）。
func TestSoDTokenSweepRemovesExpired(t *testing.T) {
	store := NewTokenStore()
	token, _, err := store.Mint("digest-a", 42, "worker", "Bash")
	if err != nil {
		t.Fatalf("铸造凭证失败: %v", err)
	}
	if removed := store.Sweep(time.Now()); removed != 0 {
		t.Fatalf("未过期时不该清掉任何凭证，实际清了 %d 个", removed)
	}
	if removed := store.Sweep(time.Now().Add(2 * time.Minute)); removed != 1 {
		t.Fatalf("过期后应清掉 1 个凭证，实际清了 %d 个", removed)
	}
	if store.Consume(token, "digest-a", 42, "worker", "Bash") {
		t.Fatal("过期凭证必须消费失败")
	}
}

// TestSoDTokenTakeConsumesByTuple 钉住执行器走的那个入口：按四元组取走并消费。
//
// 为什么要有它：hook 的返回契约里没有地方可以回传 token 本体 ⇒ 执行器只能
// 拿「将要执行的那份字节 + 任务 + agent + 工具名」去取。
func TestSoDTokenTakeConsumesByTuple(t *testing.T) {
	store := NewTokenStore()
	if _, _, err := store.Mint("digest-a", 42, "worker", "Bash"); err != nil {
		t.Fatalf("铸造凭证失败: %v", err)
	}
	if store.Take("digest-b", 42, "worker", "Bash") {
		t.Fatal("摘要不符时取不到凭证")
	}
	if store.Take("digest-a", 43, "worker", "Bash") {
		t.Fatal("任务不符时取不到凭证")
	}
	if store.Take("digest-a", 42, "other", "Bash") {
		t.Fatal("agent 不符时取不到凭证")
	}
	if store.Take("digest-a", 42, "worker", "WebFetch") {
		t.Fatal("工具不符时取不到凭证")
	}
	if !store.Take("digest-a", 42, "worker", "Bash") {
		t.Fatal("四元组全对时必须取到凭证")
	}
	if store.Take("digest-a", 42, "worker", "Bash") {
		t.Fatal("同一枚凭证不能被取走两次")
	}
}

// TestSoDTokenMintReplacesSameTuple 钉住四元组的唯一性：同一个四元组在同一时刻
// 只允许存在一枚未消费的凭证 —— 重新批准一次，旧凭证必须作废。
func TestSoDTokenMintReplacesSameTuple(t *testing.T) {
	store := NewTokenStore()
	old, _, err := store.Mint("digest-a", 42, "worker", "Bash")
	if err != nil {
		t.Fatalf("铸造凭证失败: %v", err)
	}
	fresh, _, err := store.Mint("digest-a", 42, "worker", "Bash")
	if err != nil {
		t.Fatalf("铸造凭证失败: %v", err)
	}
	if old == fresh {
		t.Fatal("两次铸造不该得到同一个凭证本体")
	}
	if store.Consume(old, "digest-a", 42, "worker", "Bash") {
		t.Fatal("被顶替掉的旧凭证必须失效")
	}
	if !store.Consume(fresh, "digest-a", 42, "worker", "Bash") {
		t.Fatal("新凭证必须可用")
	}
}

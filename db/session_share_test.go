package db

// 跨会话共享上下文的**纯规则**验收用例。
//
// 与 db/session_thread_test.go 同款：这些用例不碰数据库，因为「谁能看到什么」
// 这套判定必须能脱离 DB 复算 ⇒ 才敢把它当安全边界而不是「运行时才知道」。
//
// 钉住的承诺：
//   - 可共享类型是**代码常量**白名单：DB 里写什么都不能放宽它；
//   - request / escalation / receipt 不跨界（行动契约与确认噪音）；
//   - 被投递前拦截（blocked）的消息不跨界；
//   - 共享内容不携带任何授权（claims_authorization 恒为 false）；
//   - 额度口径：条数有硬上限，token 估算确定可复算。

import (
	"testing"
	"time"
)

// shareTestMsg 造一条会话消息（共享规则测试用）。
func shareTestMsg(id int64, from, kind, body string) SessionMessage {
	return SessionMessage{
		ID:        id,
		SessionID: 3,
		FromAgent: from,
		Kind:      kind,
		Body:      body,
		Status:    SessionMessageUnread,
		CreatedAt: time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC).Add(time.Duration(id) * time.Minute),
	}
}

// 白名单只认 note / answer / finding。
func TestEffectiveSessionShareKinds(t *testing.T) {
	got, err := EffectiveSessionShareKinds([]string{"note", "answer", "finding"})
	if err != nil {
		t.Fatalf("白名单内的类型应通过：%v", err)
	}
	if len(got) != 3 {
		t.Fatalf("应有 3 种可共享类型，得到 %d：%v", len(got), got)
	}
}

// DB 里写了白名单外的类型也**放不大**权限：交集里剩下的才是生效的。
func TestEffectiveSessionShareKindsCannotWiden(t *testing.T) {
	got, err := EffectiveSessionShareKinds([]string{"request", "escalation", "receipt"})
	if err == nil {
		t.Fatalf("只请求白名单外的类型应报错，得到 %v", got)
	}
	if err != ErrSessionShareKindsEmpty {
		t.Fatalf("应是白名单空错误，得到 %v", err)
	}
}

// 部分合法 ⇒ 只保留合法的部分（不是全盘拒绝）。
func TestEffectiveSessionShareKindsIntersects(t *testing.T) {
	got, err := EffectiveSessionShareKinds([]string{"note", "escalation"})
	if err != nil {
		t.Fatalf("部分合法应通过：%v", err)
	}
	if len(got) != 1 || got[0] != SessionMessageKindNote {
		t.Fatalf("只应保留 note，得到 %v", got)
	}
}

// request / escalation / receipt / blocked 都不跨界。
func TestSelectSharedContextItemsExcludesNonShareable(t *testing.T) {
	kinds := map[string]bool{
		SessionMessageKindNote:    true,
		SessionMessageKindAnswer:  true,
		SessionMessageKindFinding: true,
	}
	blocked := shareTestMsg(4, "pentest", SessionMessageKindNote, "被拦下的")
	blocked.Status = SessionMessageBlocked
	msgs := []SessionMessage{
		shareTestMsg(1, "pentest", SessionMessageKindNote, "普通备注"),
		shareTestMsg(2, "pentest", SessionMessageKindRequest, "请求"),
		shareTestMsg(3, "pentest", SessionMessageKindEscalation, "升级"),
		blocked,
		shareTestMsg(5, "pentest", SessionMessageKindReceipt, ""),
		shareTestMsg(6, "pentest", SessionMessageKindFinding, "一个洞"),
	}
	got := selectSharedContextItems(msgs, kinds, 50)
	if len(got) != 2 {
		t.Fatalf("只应有 note 与 finding 两条，得到 %d：%+v", len(got), got)
	}
	for _, it := range got {
		if it.Kind == SessionMessageKindRequest || it.Kind == SessionMessageKindEscalation {
			t.Fatalf("行动契约不该跨界：%+v", it)
		}
		if it.MessageID == 4 {
			t.Fatal("被拦截的消息不该跨界")
		}
		if it.MessageID == 5 {
			t.Fatal("收据条不该跨界")
		}
	}
}

// 共享内容不携带任何授权（源会话的 claims_authorization 恒被丢弃）。
func TestSelectSharedContextItemsStripsAuthorizationClaim(t *testing.T) {
	kinds := map[string]bool{SessionMessageKindNote: true}
	m := shareTestMsg(1, "pentest", SessionMessageKindNote, "我带着批准来的")
	m.ClaimsAuthorization = true
	got := selectSharedContextItems([]SessionMessage{m}, kinds, 10)
	if len(got) != 1 {
		t.Fatalf("应有 1 条，得到 %d", len(got))
	}
	if got[0].ClaimsAuthorization {
		t.Fatal("跨会话的共享内容不得携带「已批准」声明")
	}
}

// 取最近 N 条，且按 id 升序返回。
func TestSelectSharedContextItemsTakesMostRecent(t *testing.T) {
	kinds := map[string]bool{SessionMessageKindNote: true}
	var msgs []SessionMessage
	for i := int64(1); i <= 10; i++ {
		msgs = append(msgs, shareTestMsg(i, "pentest", SessionMessageKindNote, "n"))
	}
	got := selectSharedContextItems(msgs, kinds, 3)
	if len(got) != 3 {
		t.Fatalf("应取 3 条，得到 %d", len(got))
	}
	if got[0].MessageID != 8 || got[1].MessageID != 9 || got[2].MessageID != 10 {
		t.Fatalf("应取最近 3 条（8,9,10），得到 %d,%d,%d",
			got[0].MessageID, got[1].MessageID, got[2].MessageID)
	}
}

// 输入顺序不影响结果（内部排序）。
func TestSelectSharedContextItemsOrderIndependent(t *testing.T) {
	kinds := map[string]bool{SessionMessageKindNote: true}
	a := shareTestMsg(1, "pentest", SessionMessageKindNote, "a")
	b := shareTestMsg(2, "pentest", SessionMessageKindNote, "b")
	got1 := selectSharedContextItems([]SessionMessage{a, b}, kinds, 10)
	got2 := selectSharedContextItems([]SessionMessage{b, a}, kinds, 10)
	if len(got1) != 2 || len(got2) != 2 {
		t.Fatalf("两种顺序都应有 2 条：%d / %d", len(got1), len(got2))
	}
	if got1[0].MessageID != got2[0].MessageID || got1[1].MessageID != got2[1].MessageID {
		t.Fatal("输入顺序影响了结果")
	}
}

// 条数上限是代码常量：调用方给再大也被夹住。
func TestSelectSharedContextItemsClampsLimit(t *testing.T) {
	kinds := map[string]bool{SessionMessageKindNote: true}
	var msgs []SessionMessage
	for i := int64(1); i <= 120; i++ {
		msgs = append(msgs, shareTestMsg(i, "pentest", SessionMessageKindNote, "n"))
	}
	got := selectSharedContextItems(msgs, kinds, 100000)
	if len(got) > sessionShareMaxMessages {
		t.Fatalf("条数应被夹在 %d 以内，得到 %d", sessionShareMaxMessages, len(got))
	}
}

// token 估算确定可复算（额度归属的记账口径）。
func TestSessionShareTokenEstimate(t *testing.T) {
	items := []SessionSharedContextItem{
		{Body: "abcd"},
		{Body: "efgh"},
	}
	if got := sessionShareTokenEstimate(items); got != 2 {
		t.Fatalf("8 个字符应估成 2 token，得到 %d", got)
	}
	if got := sessionShareTokenEstimate(nil); got != 0 {
		t.Fatalf("空输入应估成 0，得到 %d", got)
	}
	// 同样的输入必须得到同样的估算（确定性）。
	if sessionShareTokenEstimate(items) != sessionShareTokenEstimate(items) {
		t.Fatal("token 估算不确定")
	}
}

// 共享上下文的类型集合顺序固定（可复算的存库口径）。
func TestSessionShareKindListOrderStable(t *testing.T) {
	kinds := map[string]bool{
		SessionMessageKindFinding: true,
		SessionMessageKindNote:    true,
	}
	got := sessionShareKindList(kinds)
	if len(got) != 2 || got[0] != SessionMessageKindNote || got[1] != SessionMessageKindFinding {
		t.Fatalf("类型顺序应固定（note 在前），得到 %v", got)
	}
}

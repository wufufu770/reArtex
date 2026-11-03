package db

// 线程折叠与收件条摘要的**纯函数**验收用例。
//
// 这些用例不碰数据库（噪音治理的判定必须能脱离 DB 复算 ⇒ 才敢把它当确定性
// 裁决而不是「模型总结得怎么样」）。钉住的是**承诺**：
//
//   - 线程折叠：回复折叠到最上层祖先，一个线程一个气泡；
//   - 收据条不占气泡，且挂不上时只计数不渲染；
//   - 父消息不在窗口里 / 数据成环时**不丢消息**；
//   - 摘要：人自己说的话不露出、按优先级类限额、气泡数下降可量化；
//   - 同一份输入永远得到同一份输出（确定性）。

import (
	"testing"
	"time"
)

// testThreadTime 是固定的基准时间（避免用例依赖真实时钟）。
var testThreadTime = time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)

// threadMsg 造一条会话消息（测试用；只填分组/摘要关心的字段）。
func threadMsg(id int64, from, kind, body string) SessionMessage {
	return SessionMessage{
		ID:        id,
		SessionID: 7,
		FromAgent: from,
		ToAgent:   "",
		Kind:      kind,
		Body:      body,
		Status:    SessionMessageUnread,
		CreatedAt: testThreadTime.Add(time.Duration(id) * time.Minute),
	}
}

// threadReply 造一条挂在 parent 下面的回复。
func threadReply(id, parent int64, from, kind, body string) SessionMessage {
	m := threadMsg(id, from, kind, body)
	m.ParentMessageID = &parent
	return m
}

// threadReceipt 造一条收据条（挂在 target 下面）。
func threadReceipt(id, target int64, from, action string) SessionMessage {
	m := threadMsg(id, from, SessionMessageKindReceipt, "")
	m.ReceiptOf = &target
	m.ReceiptAction = action
	return m
}

func TestGroupSessionMessagesEmpty(t *testing.T) {
	if got := GroupSessionMessages(nil); len(got) != 0 {
		t.Fatalf("空输入应得到空线程列表，得到 %d 条", len(got))
	}
}

func TestGroupSessionMessagesFlat(t *testing.T) {
	threads := GroupSessionMessages([]SessionMessage{
		threadMsg(3, "auto", SessionMessageKindNote, "c"),
		threadMsg(1, "auto", SessionMessageKindNote, "a"),
		threadMsg(2, "auto", SessionMessageKindNote, "b"),
	})
	if len(threads) != 3 {
		t.Fatalf("无父消息时应是 3 个线程，得到 %d", len(threads))
	}
	for i, want := range []int64{1, 2, 3} {
		if threads[i].Root.ID != want {
			t.Fatalf("线程顺序应按 id 升序：第 %d 条是 %d，期望 %d", i, threads[i].Root.ID, want)
		}
		if threads[i].Collapsed {
			t.Fatalf("没有回复的线程不该是折叠态：id=%d", threads[i].Root.ID)
		}
	}
}

func TestGroupSessionMessagesReplyFoldsIntoParent(t *testing.T) {
	threads := GroupSessionMessages([]SessionMessage{
		threadMsg(1, "auto", SessionMessageKindRequest, "查一下 80 端口"),
		threadReply(2, 1, "pentest", SessionMessageKindNote, "开了"),
	})
	if len(threads) != 1 {
		t.Fatalf("一条回复应折叠成 1 个线程，得到 %d", len(threads))
	}
	th := threads[0]
	if th.Root.ID != 1 {
		t.Fatalf("线程根应是 1，得到 %d", th.Root.ID)
	}
	if th.ReplyCount != 1 || len(th.Replies) != 1 {
		t.Fatalf("回复数应为 1，得到 %d", th.ReplyCount)
	}
	if !th.Collapsed {
		t.Fatal("有回复的线程应标记为折叠态")
	}
	if th.Replies[0].ID != 2 {
		t.Fatalf("回复内容不对：%d", th.Replies[0].ID)
	}
}

// 回复挂在**回复**上时，要折叠到最上层祖先，而不是直接父。
func TestGroupSessionMessagesNestedReplyFoldsToTopAncestor(t *testing.T) {
	threads := GroupSessionMessages([]SessionMessage{
		threadMsg(1, "auto", SessionMessageKindRequest, "根"),
		threadReply(2, 1, "pentest", SessionMessageKindNote, "中层"),
		threadReply(3, 2, "curator", SessionMessageKindNote, "底层"),
	})
	if len(threads) != 1 {
		t.Fatalf("嵌套回复应折叠成 1 个线程，得到 %d", len(threads))
	}
	if threads[0].Root.ID != 1 {
		t.Fatalf("线程根应是 1，得到 %d", threads[0].Root.ID)
	}
	if threads[0].ReplyCount != 2 {
		t.Fatalf("回复数应为 2，得到 %d", threads[0].ReplyCount)
	}
}

// R1：收据条不占气泡，且贴在它指向的那条消息下面。
func TestGroupSessionMessagesReceiptDoesNotOccupyBubble(t *testing.T) {
	threads := GroupSessionMessages([]SessionMessage{
		threadMsg(1, "auto", SessionMessageKindRequest, "查一下"),
		threadMsg(2, "auto", SessionMessageKindNote, "另一条"),
		threadReceipt(3, 1, "pentest", SessionReceiptTaken),
	})
	if len(threads) != 2 {
		t.Fatalf("收据条不占气泡：应是 2 个线程，得到 %d", len(threads))
	}
	if len(threads[0].Receipts) != 1 {
		t.Fatalf("收据条应贴在根消息下，得到 %d 条", len(threads[0].Receipts))
	}
	if threads[0].Receipts[0].ID != 3 {
		t.Fatalf("收据条 id 不对：%d", threads[0].Receipts[0].ID)
	}
	if threads[0].Receipts[0].ReceiptAction != SessionReceiptTaken {
		t.Fatalf("收据动作丢了：%q", threads[0].Receipts[0].ReceiptAction)
	}
	if len(threads[1].Receipts) != 0 {
		t.Fatal("收据条贴错线程了")
	}
}

// R2：父消息不在列表里 ⇒ 提升为独立根（不丢内容）。
func TestGroupSessionMessagesOrphanReplyBecomesRoot(t *testing.T) {
	threads := GroupSessionMessages([]SessionMessage{
		threadReply(5, 99, "pentest", SessionMessageKindNote, "父消息不在窗口里"),
	})
	if len(threads) != 1 {
		t.Fatalf("孤儿回复应提升为独立根，得到 %d 条线程", len(threads))
	}
	if threads[0].Root.ID != 5 {
		t.Fatalf("孤儿回复应保留自己的 id，得到 %d", threads[0].Root.ID)
	}
	if threads[0].ReplyCount != 0 {
		t.Fatalf("孤儿回复不应带回复，得到 %d", threads[0].ReplyCount)
	}
}

// 数据成环时不能丢消息：环的两端要落到同一个根。
func TestGroupSessionMessagesCycleKeepsAllMessages(t *testing.T) {
	a := threadReply(1, 2, "auto", SessionMessageKindNote, "a")
	b := threadReply(2, 1, "auto", SessionMessageKindNote, "b")
	threads := GroupSessionMessages([]SessionMessage{a, b})
	total := 0
	roots := map[int64]bool{}
	for _, th := range threads {
		roots[th.Root.ID] = true
		total += 1 + th.ReplyCount
	}
	if total != 2 {
		t.Fatalf("成环时不能丢消息：共 %d 条（期望 2）", total)
	}
	if len(roots) != 1 {
		t.Fatalf("成环的两端应落在同一个根，得到 %d 个根", len(roots))
	}
}

func TestBuildSessionDigestEmptyWindow(t *testing.T) {
	msgs := []SessionMessage{threadMsg(1, "auto", SessionMessageKindNote, "a")}
	d := BuildSessionDigest(7, msgs, 1)
	if d.RawBubbles != 0 || d.DigestBubbles != 0 || d.BubblesSaved != 0 {
		t.Fatalf("窗口为空时不该有气泡：%+v", d)
	}
	if d.Counts.Total != 0 {
		t.Fatalf("窗口为空时计数应为 0：%+v", d.Counts)
	}
	if d.Items == nil {
		t.Fatal("Items 不该是 nil（前端要能直接遍历）")
	}
}

// R3：人自己写的消息不露出（但要计数）。
func TestBuildSessionDigestExcludesHumanMessages(t *testing.T) {
	msgs := []SessionMessage{
		threadMsg(1, "", SessionMessageKindRequest, "人来问的"),
		threadMsg(2, "pentest", SessionMessageKindFinding, "agent 报的"),
	}
	d := BuildSessionDigest(7, msgs, 0)
	if d.Counts.HumanAuthored != 1 {
		t.Fatalf("人的消息应被计数：%+v", d.Counts)
	}
	if d.Counts.Total != 2 {
		t.Fatalf("总计数应为 2：%+v", d.Counts)
	}
	if len(d.Items) != 1 {
		t.Fatalf("只应露出 1 条（agent 的）：得到 %d", len(d.Items))
	}
	if d.Items[0].FromAgent != "pentest" {
		t.Fatalf("露出的应是 agent 消息：%q", d.Items[0].FromAgent)
	}
}

// R4：优先级类限额 —— escalation 最多 3 条、note 最多 2 条。
func TestBuildSessionDigestClassCaps(t *testing.T) {
	var msgs []SessionMessage
	for i := int64(1); i <= 6; i++ {
		msgs = append(msgs, threadMsg(i, "pentest", SessionMessageKindEscalation, "升级"))
	}
	for i := int64(10); i <= 14; i++ {
		msgs = append(msgs, threadMsg(i, "pentest", SessionMessageKindNote, "备注"))
	}
	d := BuildSessionDigest(7, msgs, 0)
	if d.Counts.Escalations != 6 || d.Counts.Notes != 5 {
		t.Fatalf("计数不对：%+v", d.Counts)
	}
	got := map[string]int{}
	for _, it := range d.Items {
		got[it.Class]++
	}
	if got[sessionDigestClassEscalation] != 3 {
		t.Fatalf("escalation 应限 3 条，得到 %d", got[sessionDigestClassEscalation])
	}
	if got[sessionDigestClassNote] != 2 {
		t.Fatalf("note 应限 2 条，得到 %d", got[sessionDigestClassNote])
	}
	if d.Overflow[sessionDigestClassEscalation] != 3 {
		t.Fatalf("被挡掉的 escalation 应进 overflow，得到 %d", d.Overflow[sessionDigestClassEscalation])
	}
	if d.Overflow[sessionDigestClassNote] != 3 {
		t.Fatalf("被挡掉的 note 应进 overflow，得到 %d", d.Overflow[sessionDigestClassNote])
	}
}

// 总上限：超出后从优先级最低的类里砍，砍掉的进 overflow。
func TestBuildSessionDigestTotalCap(t *testing.T) {
	var msgs []SessionMessage
	for i := int64(1); i <= 20; i++ {
		msgs = append(msgs, threadMsg(i, "pentest", SessionMessageKindNote, "备注"))
	}
	d := BuildSessionDigest(7, msgs, 0)
	if d.DigestBubbles > sessionDigestMaxClassTotal {
		t.Fatalf("露出条数应不超过总上限 %d，得到 %d", sessionDigestMaxClassTotal, d.DigestBubbles)
	}
	if d.Counts.Total != 20 {
		t.Fatalf("计数不该被上限影响：%d", d.Counts.Total)
	}
}

// answer 只有在「回答了别人的请求」时才单独成类。
func TestBuildSessionDigestAnswerClass(t *testing.T) {
	msgs := []SessionMessage{
		threadMsg(1, "", SessionMessageKindRequest, "谁去看一下"),
		threadReply(2, 1, "pentest", SessionMessageKindAnswer, "我看了"),
		threadMsg(3, "pentest", SessionMessageKindAnswer, "随口一句"),
	}
	d := BuildSessionDigest(7, msgs, 0)
	classes := map[string]int{}
	for _, it := range d.Items {
		classes[it.Class]++
	}
	if classes[sessionDigestClassAnswer] != 1 {
		t.Fatalf("只有回答请求的 answer 才算 answer 类：%+v", classes)
	}
	if classes[sessionDigestClassNote] != 1 {
		t.Fatalf("普通 answer 应归 note 类：%+v", classes)
	}
}

// 气泡数下降可量化：折叠 + 摘要之后要读的气泡数严格少于原始气泡数。
func TestBuildSessionDigestBubbleReduction(t *testing.T) {
	msgs := []SessionMessage{
		threadMsg(1, "auto", SessionMessageKindRequest, "开工"),
		threadReply(2, 1, "pentest", SessionMessageKindNote, "收到"),
		threadReply(3, 1, "pentest", SessionMessageKindNote, "收到"),
		threadReply(4, 1, "pentest", SessionMessageKindNote, "收到"),
		threadMsg(5, "curator", SessionMessageKindNote, "闲聊"),
		threadMsg(6, "curator", SessionMessageKindNote, "闲聊"),
		threadMsg(7, "curator", SessionMessageKindNote, "闲聊"),
		threadReceipt(8, 1, "pentest", SessionReceiptTaken),
	}
	d := BuildSessionDigest(7, msgs, 0)
	if d.RawBubbles != 7 {
		t.Fatalf("原始气泡数应为 7（收据不算气泡），得到 %d", d.RawBubbles)
	}
	if d.ThreadBubbles != 4 {
		// 1 条根 + 3 条回复折叠成 1 个气泡；3 条独立闲聊各占 1 个 ⇒ 共 4 个。
		t.Fatalf("线程折叠后应是 4 个气泡，得到 %d", d.ThreadBubbles)
	}
	if d.DigestBubbles >= d.RawBubbles {
		t.Fatalf("摘要后要读的气泡数应严格少于原始：%d vs %d", d.DigestBubbles, d.RawBubbles)
	}
	if d.BubblesSaved != d.RawBubbles-d.DigestBubbles {
		t.Fatalf("省下的气泡数算错：%d", d.BubblesSaved)
	}
	if d.Counts.Receipts != 1 {
		t.Fatalf("收据条应被计数：%+v", d.Counts)
	}
}

// 同一份输入必须得到同一份输出（确定性）。
func TestBuildSessionDigestDeterministic(t *testing.T) {
	msgs := []SessionMessage{
		threadMsg(1, "auto", SessionMessageKindRequest, "开工"),
		threadReply(2, 1, "pentest", SessionMessageKindNote, "收到"),
		threadMsg(3, "curator", SessionMessageKindFinding, "有个洞"),
		threadMsg(4, "pentest", SessionMessageKindEscalation, "卡住了"),
	}
	a := BuildSessionDigest(7, msgs, 0)
	b := BuildSessionDigest(7, msgs, 0)
	if len(a.Items) != len(b.Items) {
		t.Fatalf("两次结果条数不同：%d vs %d", len(a.Items), len(b.Items))
	}
	for i := range a.Items {
		if a.Items[i] != b.Items[i] {
			t.Fatalf("第 %d 条两次结果不同：%+v vs %+v", i, a.Items[i], b.Items[i])
		}
	}
	if a.BubblesSaved != b.BubblesSaved || a.RawBubbles != b.RawBubbles {
		t.Fatal("两次的气泡计数不同")
	}
}

// 输入顺序打乱也必须得到同一份结果（排序是分组的一部分）。
func TestGroupSessionMessagesInputOrderIndependent(t *testing.T) {
	ordered := []SessionMessage{
		threadMsg(1, "auto", SessionMessageKindRequest, "根"),
		threadReply(2, 1, "pentest", SessionMessageKindNote, "回"),
	}
	shuffled := []SessionMessage{ordered[1], ordered[0]}
	a := GroupSessionMessages(ordered)
	b := GroupSessionMessages(shuffled)
	if len(a) != 1 || len(b) != 1 {
		t.Fatalf("两种顺序都应折叠成 1 个线程：%d / %d", len(a), len(b))
	}
	if a[0].Root.ID != b[0].Root.ID || a[0].ReplyCount != b[0].ReplyCount {
		t.Fatalf("顺序影响了结果：%+v vs %+v", a[0], b[0])
	}
}

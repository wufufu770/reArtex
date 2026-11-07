package server

// 会话级唤醒器（server/session_waker.go）的验收用例。
//
// 分两层：
//  - 纯函数层（prompt 形状 / 截断 / 发件人标签）：不碰库，任何环境都跑。
//  - 集成层（@ 消息 ⇒ 起 turn ⇒ 线程建好 ⇒ presence 回位）：只认派生库
//    （NewManager 走 ARTEX_PG_DSN 指向的库；CI/本地由 testdb 派生库兜底）。
//    唤醒 turn 没有 LLM 时 Chat 必然报错 —— 这条失败路径本身就是要钉住的行为：
//    turn 失败 ⇒ 不写空泡、presence 回位、线程与唤醒提示已落库。
//
// 集成层的等待用轮询（唤醒是后台 goroutine），deadline 到了还没到位即失败。

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
)

// ---------- 纯函数层 ----------

func TestSessionWakePromptShape(t *testing.T) {
	msg := db.SessionMessage{ID: 42, FromAgent: "", Kind: db.SessionMessageKindNote,
		Body: "192.168.1.1 做一次渗透测试"}
	prompt := sessionWakePrompt("pentest", msg, "")
	for _, want := range []string{"@pentest", "来自：人", "消息 #42", msg.Body} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("唤醒提示缺 %q：\n%s", want, prompt)
		}
	}
	// agent 发来的定向消息：发件人标签是它的 key。
	msg.FromAgent = "auto"
	if got := sessionWakePrompt("pentest", msg, ""); !strings.Contains(got, "来自：auto") {
		t.Fatalf("agent 发件人标签不对：\n%s", got)
	}
	// 引用内联（B3）：转交说明带引用原文时，原文必须出现在 wake 输入里；
	// 没有引用时不得出现空的引用段。
	quoted := sessionWakePrompt("pentest", msg, "消息 #197（来自 人）：\n对 10.0.0.1 做全端口扫描")
	for _, want := range []string{"被引用消息的原文", "10.0.0.1", "消息 #197"} {
		if !strings.Contains(quoted, want) {
			t.Fatalf("带引用的唤醒提示缺 %q：\n%s", want, quoted)
		}
	}
	if strings.Contains(sessionWakePrompt("pentest", msg, ""), "被引用消息的原文") {
		t.Fatalf("没有引用时不应出现引用段")
	}
}

func TestSessionWakeHostPromptShape(t *testing.T) {
	msg := db.SessionMessage{ID: 7, FromAgent: "", Kind: db.SessionMessageKindNote,
		Body: "大家看看 192.168.1.1"}
	prompt := sessionWakeHostPrompt("auto", msg, "auto（分发者）、pentest（成员）")
	for _, want := range []string{"【群聊主持】", "在席成员", "auto（分发者）", "pentest（成员）",
		"只能 @ 在席成员", "spawn_task", "凭空", "任务编号", "来自：人", "消息 #7", msg.Body} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("主持提示缺 %q：\n%s", want, prompt)
		}
	}
	// 名册感知（B1）：转交示范不再硬编码具体名字 —— 名册是运行时事实，
	// 不是提示词里的例子（「例如 @pentest」会教模型编不在席的 key）。
	if strings.Contains(prompt, "例如 @") {
		t.Fatalf("主持提示不应携带硬编码的转交示范：\n%s", prompt)
	}
	// 定向唤醒与主持提示必须是两种口吻（执行方靠 host 位区分）。
	if strings.Contains(prompt, "【群聊唤醒】") {
		t.Fatalf("主持提示不应复用定向唤醒的口吻")
	}
	// 名册读不到 ⇒ 退化成不带名册的版本，转交/兜底规则段仍然成立。
	bare := sessionWakeHostPrompt("auto", msg, "")
	if !strings.Contains(bare, "spawn_task") || strings.Contains(bare, "在席成员：") {
		t.Fatalf("无名册的主持提示退化不对：\n%s", bare)
	}
}

func TestTruncateRunesRespectsRuneBoundary(t *testing.T) {
	s := strings.Repeat("汉", 10)
	if got := truncateRunes(s, 5); got != strings.Repeat("汉", 5) {
		t.Fatalf("截断结果不对: %q", got)
	}
	if got := truncateRunes(s, 100); got != s {
		t.Fatalf("未超限不应截断: %q", got)
	}
}

func TestWakeFromLabel(t *testing.T) {
	if got := wakeFromLabel(""); got != "人" {
		t.Fatalf("空发件人应显示 人，实际 %q", got)
	}
	if got := wakeFromLabel(" pentest "); got != "pentest" {
		t.Fatalf("发件人应去空白，实际 %q", got)
	}
}

// TestWakeChainBookDedupsSameEdge 钉住接力链「同边去重」：同一条链上 from→to
// 的唤醒只放行一次 —— 客套互 @ 的乒乓在第二个来回被确定性掐断；不同链、
// 不同方向、无链身份都不受影响。
func TestWakeChainBookDedupsSameEdge(t *testing.T) {
	var b wakeChainBook
	const chain = "m42"
	// 同链同边：第一次放行，第二次拒绝。
	if !b.allow(chain, "auto", "pentest") {
		t.Fatalf("首次接力应放行")
	}
	if b.allow(chain, "auto", "pentest") {
		t.Fatalf("同链同边第二次应拒绝（乒乓闸）")
	}
	// 反方向是另一条边：放行（B 回 A 的请示是合法接力）。
	if !b.allow(chain, "pentest", "auto") {
		t.Fatalf("反方向应放行")
	}
	// 换一条链：同边重新计数（新的人发消息 = 新的接力）。
	if !b.allow("m43", "auto", "pentest") {
		t.Fatalf("新链不应继承旧链的边")
	}
	// 无链身份：恒放行（旧调用方/测试路径）。
	if !b.allow("", "auto", "pentest") {
		t.Fatalf("无链身份应恒放行")
	}
	if !b.allow("", "auto", "pentest") {
		t.Fatalf("无链身份第二次也应放行")
	}
	// 空收件人不入账。
	if !b.allow(chain, "auto", "") {
		t.Fatalf("空收件人应放行")
	}
}

// ---------- 集成层 ----------

// TestWakeDepthCapBlocksNewTurns 钉住接力深度硬顶：超限的定向消息不起 turn ——
// 真实会话上也不会为它建成员对话线程（深度检查在线程创建之前）。
func TestWakeDepthCapBlocksNewTurns(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v)", err)
	}
	t.Cleanup(func() { m.Close() })
	if m.pg == nil {
		t.Skip("postgres unavailable")
	}
	td := t.TempDir()
	s := New(testServerCtx(t), m, td, td, td)
	pg := m.pg

	sess := &db.Session{Name: fmt.Sprintf("唤醒深度-%d", tmpSuffix()), Kind: db.SessionKindChat}
	sid, err := pg.CreateSession(sess)
	if err != nil {
		t.Fatalf("建会话失败: %v", err)
	}
	t.Cleanup(func() { _ = pg.DeleteSession(sid) })
	if err := pg.AddSessionMember(sid, "pentest", db.SessionRoleMember, db.SessionPresenceListening); err != nil {
		t.Fatalf("加成员失败: %v", err)
	}
	msg := &db.SessionMessage{ID: 1, SessionID: sid, Kind: db.SessionMessageKindNote, Body: "@pentest hi"}
	s.wakeSessionAgents(sid, []string{"pentest"}, msg, sessionWakeChainDepthCap+1, "", "", false)
	// 只读判定（不能用 SessionConversationFor —— 它是「没有就建」，会把线程建出来）。
	ids, err := pg.SessionConversationIDs(sid)
	if err != nil {
		t.Fatalf("查会话线程失败: %v", err)
	}
	if len(ids) != 0 {
		t.Fatalf("深度超限不应起 turn（成员对话线程不该被建出），实际 %v", ids)
	}
}

// TestSessionWakeTurnLifecycle 走全链路：建会话 → 加成员 → @定向消息 →
// 唤醒 turn 跑完（无 LLM ⇒ 失败路径）⇒ 线程建好、唤醒提示落 transcript、
// presence 回 listening、群里没有空泡。
func TestSessionWakeTurnLifecycle(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v)", err)
	}
	t.Cleanup(func() { m.Close() })
	if m.pg == nil {
		t.Skip("postgres unavailable")
	}
	td := t.TempDir()
	s := New(testServerCtx(t), m, td, td, td)
	pg := m.pg

	sess := &db.Session{Name: fmt.Sprintf("唤醒-%d", tmpSuffix()), Kind: db.SessionKindChat}
	sid, err := pg.CreateSession(sess)
	if err != nil {
		t.Fatalf("建会话失败: %v", err)
	}
	t.Cleanup(func() { _ = pg.DeleteSession(sid) })
	if err := pg.AddSessionMember(sid, "pentest", db.SessionRoleMember, db.SessionPresenceListening); err != nil {
		t.Fatalf("加成员失败: %v", err)
	}
	if err := pg.AddSessionMember(sid, "auto", db.SessionRoleDispatcher, db.SessionPresenceListening); err != nil {
		t.Fatalf("加 dispatcher 失败: %v", err)
	}

	msg := &db.SessionMessage{SessionID: sid, FromAgent: "", ToAgent: "pentest",
		Kind: db.SessionMessageKindNote, Body: "@pentest 192.168.1.1 渗透测试"}
	if _, err := pg.AddSessionMessage(msg); err != nil {
		t.Fatalf("写消息失败: %v", err)
	}
	s.wakeSessionAgents(sid, []string{"pentest"}, msg, 0, "", "", false)

	deadline := time.Now().Add(15 * time.Second)
	var conv *db.Conversation
	for time.Now().Before(deadline) {
		if c, err := pg.SessionConversationFor(sid, "pentest"); err == nil {
			conv = c
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if conv == nil {
		t.Fatalf("唤醒后成员对话线程没有被建出来")
	}
	// presence 回位（无 LLM ⇒ turn 立刻失败 ⇒ 收尾照走）。
	for time.Now().Before(deadline) {
		if mem, ok := sessionMemberByKey(pg, sid, "pentest"); ok && mem.Presence == db.SessionPresenceListening {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if mem, ok := sessionMemberByKey(pg, sid, "pentest"); !ok || mem.Presence != db.SessionPresenceListening {
		t.Fatalf("turn 收尾后 presence 应回 listening，实际 %q (ok=%v)", mem.Presence, ok)
	}
	// 唤醒提示已落 transcript。
	acts, _, err := pg.ConvActivityList(conv.ID, 0, 50)
	if err != nil {
		t.Fatalf("读 transcript 失败: %v", err)
	}
	foundPrompt := false
	for _, a := range acts {
		// 列表只带 summary（detail 懒加载）；唤醒提示的首行就含这个标记。
		if strings.Contains(a.Summary, "【群聊唤醒】") {
			foundPrompt = true
			break
		}
		if d, err := pg.ConvActivityDetail(conv.ID, a.ID); err == nil && strings.Contains(d, "【群聊唤醒】") {
			foundPrompt = true
			break
		}
	}
	if !foundPrompt {
		t.Fatalf("transcript 里没有唤醒提示（acts=%d）", len(acts))
	}
	// 无 LLM ⇒ turn 失败 ⇒ 群里不写回复（没有空泡）。
	msgs, err := pg.SessionMessages(sid, 100)
	if err != nil {
		t.Fatalf("读会话流失败: %v", err)
	}
	for _, mm := range msgs {
		if mm.FromAgent == "pentest" {
			t.Fatalf("turn 失败不应镜像回复，实际写了消息 #%d", mm.ID)
		}
	}
	// busy 已清（后续消息可以再起 turn）。
	s.chatMu.Lock()
	busy := s.chatBusy[s.convBusyKey(conv.ID)]
	s.chatMu.Unlock()
	if busy {
		t.Fatalf("turn 收尾后 chatBusy 未清")
	}
}

// TestSessionWakeBusyQueue 钉住「正在跑时到达的定向消息被暂存、turn 结束后触发
// 后续唤醒」。手工占住 busy ⇒ @ 消息进暂存队列 ⇒ 释放 busy 并 drain ⇒ 后续唤醒
// 起 turn（无 LLM 立刻失败）⇒ 队列清空、busy 清、presence 回位。
func TestSessionWakeBusyQueue(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v)", err)
	}
	t.Cleanup(func() { m.Close() })
	if m.pg == nil {
		t.Skip("postgres unavailable")
	}
	td := t.TempDir()
	s := New(testServerCtx(t), m, td, td, td)
	pg := m.pg

	sess := &db.Session{Name: fmt.Sprintf("唤醒排队-%d", tmpSuffix()), Kind: db.SessionKindChat}
	sid, err := pg.CreateSession(sess)
	if err != nil {
		t.Fatalf("建会话失败: %v", err)
	}
	t.Cleanup(func() { _ = pg.DeleteSession(sid) })
	if err := pg.AddSessionMember(sid, "pentest", db.SessionRoleMember, db.SessionPresenceListening); err != nil {
		t.Fatalf("加成员失败: %v", err)
	}
	conv, err := pg.SessionConversationFor(sid, "pentest")
	if err != nil {
		t.Fatalf("预建线程失败: %v", err)
	}
	// 模拟「正在跑」：手工占住 busy（与 startSessionWakeTurn 同一把锁同一份账）。
	busyKey := s.convBusyKey(conv.ID)
	s.chatMu.Lock()
	s.chatBusy[busyKey] = true
	s.chatMu.Unlock()

	msg := &db.SessionMessage{SessionID: sid, FromAgent: "", ToAgent: "pentest",
		Kind: db.SessionMessageKindNote, Body: "@pentest 排队消息"}
	if _, err := pg.AddSessionMessage(msg); err != nil {
		t.Fatalf("写消息失败: %v", err)
	}
	s.wakeSessionAgents(sid, []string{"pentest"}, msg, 0, "", "", false)

	s.chatMu.Lock()
	q := s.sessionWakePending[busyKey]
	s.chatMu.Unlock()
	if len(q) != 1 {
		t.Fatalf("忙碌时应暂存 1 条，实际 %d", len(q))
	}
	// 释放 busy：模拟 turn 结束后的路径（drain 由收尾触发；这里手工触发同一段）。
	s.chatMu.Lock()
	delete(s.chatBusy, busyKey)
	s.chatMu.Unlock()
	s.drainPendingWake(sid, "pentest", conv, 0, "")

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		s.chatMu.Lock()
		left := len(s.sessionWakePending[busyKey])
		busyNow := s.chatBusy[busyKey]
		s.chatMu.Unlock()
		if left == 0 && !busyNow {
			if mem, ok := sessionMemberByKey(pg, sid, "pentest"); ok && mem.Presence == db.SessionPresenceListening {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	s.chatMu.Lock()
	left := len(s.sessionWakePending[busyKey])
	busyNow := s.chatBusy[busyKey]
	s.chatMu.Unlock()
	mem, _ := sessionMemberByKey(pg, sid, "pentest")
	t.Fatalf("后续唤醒未收尾：队列=%d busy=%v presence=%q", left, busyNow, mem.Presence)
}

// TestQuotedMessageIDsParsesPointers 钉住引用指针的解析（B3）：parent_message_id
// 与正文里的 #N 都算引用；URL 片段 / ## / 指向自身 不算；保序去重、条数封顶。
func TestQuotedMessageIDsParsesPointers(t *testing.T) {
	parent := int64(5)
	m := &db.SessionMessage{ID: 9, ParentMessageID: &parent,
		Body: "内容与 #197 相同；另见 #197 与 #5；https://x/page#15；##12；#abc；再看 #301 #302"}
	got := quotedMessageIDs(m)
	want := []int64{5, 197, 301} // parent 优先；#197/#5 去重；page#15、##12、#abc 不算；#302 超出封顶
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("引用解析不对: %v（想要 %v）", got, want)
	}
	// 没有引用 ⇒ 空表（wake 输入维持原状）。
	if got := quotedMessageIDs(&db.SessionMessage{ID: 1, Body: "没有任何指针"}); len(got) != 0 {
		t.Fatalf("无引用不应产生指针: %v", got)
	}
	// 指向自身的编号被排除（自己引用自己没有意义）。
	if got := quotedMessageIDs(&db.SessionMessage{ID: 3, Body: "见 #3"}); len(got) != 0 {
		t.Fatalf("指向自身的指针应被排除: %v", got)
	}
}

// ---------- 集成层（B1-B4 的落库行为） ----------

// newWakeTestSession 是集成测试的公共底座：派生库 + 会话 + auto 分发者在席。
func newWakeTestSession(t *testing.T) (*Server, *db.DB, int64) {
	t.Helper()
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v)", err)
	}
	t.Cleanup(func() { m.Close() })
	if m.pg == nil {
		t.Skip("postgres unavailable")
	}
	td := t.TempDir()
	s := New(testServerCtx(t), m, td, td, td)
	pg := m.pg
	sess := &db.Session{Name: fmt.Sprintf("唤醒兜底-%d", tmpSuffix()), Kind: db.SessionKindChat}
	sid, err := pg.CreateSession(sess)
	if err != nil {
		t.Fatalf("建会话失败: %v", err)
	}
	t.Cleanup(func() { _ = pg.DeleteSession(sid) })
	if err := pg.AddSessionMember(sid, "auto", db.SessionRoleDispatcher, db.SessionPresenceListening); err != nil {
		t.Fatalf("加成员失败: %v", err)
	}
	return s, pg, sid
}

// TestHotJoinMentionedIdempotentAndMissingKeys 钉住热加入的幂等与「不存在的
// key」回执（B2 的公共函数层）：重复 @ 同一个在册 agent 只入席一次；@ 到 agents
// 表里没有的 key 不入席、作为 missing 保序返回（调用方据此走兜底建任务）。
func TestHotJoinMentionedIdempotentAndMissingKeys(t *testing.T) {
	_, pg, sid := newWakeTestSession(t)
	members, err := pg.SessionMembers(sid)
	if err != nil {
		t.Fatalf("读名册失败: %v", err)
	}
	// 同一条回复里重复 @ 同一个在册 agent：只入席一次（幂等）。
	for i := 0; i < 2; i++ {
		var missing []string
		members, missing = hotJoinMentioned(pg, sid, []string{"pentest"}, members, "auto")
		if len(missing) != 0 {
			t.Fatalf("pentest 是在册 agent，不应出现在 missing: %v", missing)
		}
	}
	rows, err := pg.SessionMembers(sid)
	if err != nil {
		t.Fatalf("读名册失败: %v", err)
	}
	n := 0
	for _, mm := range rows {
		if mm.AgentKey == "pentest" {
			n++
			if mm.Presence != db.SessionPresenceListening {
				t.Fatalf("热加入的成员应在席 listening，实际 %q", mm.Presence)
			}
		}
	}
	if n != 1 {
		t.Fatalf("重复 @ 只应入席一次，实际 %d 行", n)
	}
	// @ 到不存在的 key：不入席，missing 里如实报告（保序）。
	members, missing := hotJoinMentioned(pg, sid, []string{"ghost", "no_such"}, members, "auto")
	if !reflect.DeepEqual(missing, []string{"ghost", "no_such"}) {
		t.Fatalf("不存在的 key 应进 missing: %v", missing)
	}
	// 发起人 @ 自己：既不 missing 也不入席。
	if _, missing = hotJoinMentioned(pg, sid, []string{"auto"}, members, "auto"); len(missing) != 0 {
		t.Fatalf("发起人 @ 自己不应算 missing: %v", missing)
	}
	rows, err = pg.SessionMembers(sid)
	if err != nil {
		t.Fatalf("读名册失败: %v", err)
	}
	for _, mm := range rows {
		if mm.AgentKey == "ghost" || mm.AgentKey == "no_such" {
			t.Fatalf("不存在的 key 不应入席: %+v", mm)
		}
	}
}

// TestDeliverAgentReplyHotJoinsMentioned 钉住 B2 的热加入：agent 回复里 @ 到一个
// 在册但未入席的成员 ⇒ 先入席再被定向唤醒（与人发消息同一条路径），名册不翻倍。
func TestDeliverAgentReplyHotJoinsMentioned(t *testing.T) {
	s, pg, sid := newWakeTestSession(t)
	s.deliverAgentReplyToSession(sid, "auto", "@pentest 对 10.0.0.1 做一次端口扫描", 0, "")
	deadline := time.Now().Add(15 * time.Second)
	// pentest 已热加入（hotJoin 在 deliver 的同步段，轮询只是兜稳）。
	joined := false
	for time.Now().Before(deadline) {
		if _, ok := sessionMemberByKey(pg, sid, "pentest"); ok {
			joined = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !joined {
		t.Fatalf("agent 回复 @ 到的成员没有被热加入")
	}
	// 且被定向唤醒（成员对话线程被建出 —— 唤醒 turn 的固定动作）。
	conv, err := pg.SessionConversationFor(sid, "pentest")
	if err != nil {
		t.Fatalf("热加入后应被定向唤醒（成员对话线程缺失）: %v", err)
	}
	// wake 输入点名了 pentest（提示词落 transcript，人能看到为什么被叫起来）。
	found := false
	for time.Now().Before(deadline) {
		acts, _, err := pg.ConvActivityList(conv.ID, 0, 50)
		if err == nil {
			for _, a := range acts {
				if strings.Contains(a.Summary, "【群聊唤醒】") {
					found = true
					break
				}
			}
		}
		if found {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !found {
		t.Fatalf("成员线程里没有唤醒提示")
	}
	// 名册幂等：pentest 只有一行。
	rows, err := pg.SessionMembers(sid)
	if err != nil {
		t.Fatalf("读名册失败: %v", err)
	}
	n := 0
	for _, mm := range rows {
		if mm.AgentKey == "pentest" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("热加入应幂等（1 行），实际 %d 行", n)
	}
}

// TestDeliverAgentReplyInlinesQuotedMessage 钉住 B3：转交说明只有指针（「内容与
// #N 相同、已转交」）时，被 @ 的一方拿到的 wake 输入必须内联 #N 的原文 ——
// 接单方拿到内容而不是空指针。
func TestDeliverAgentReplyInlinesQuotedMessage(t *testing.T) {
	s, pg, sid := newWakeTestSession(t)
	refID, err := pg.AddSessionMessage(&db.SessionMessage{SessionID: sid,
		Kind: db.SessionMessageKindNote, Body: "对 10.0.0.1 做全端口扫描并汇总开放服务"})
	if err != nil {
		t.Fatalf("写被引用消息失败: %v", err)
	}
	s.deliverAgentReplyToSession(sid, "auto",
		fmt.Sprintf("内容与 #%d 相同，已转交 @pentest", refID), 0, "")
	deadline := time.Now().Add(15 * time.Second)
	conv, err := pg.SessionConversationFor(sid, "pentest")
	if err != nil {
		t.Fatalf("被 @ 的成员没有被唤醒（成员对话线程缺失）: %v", err)
	}
	// wake 输入里必须有被引用消息的原文（引用段 + 原文都在）。
	found := false
	for time.Now().Before(deadline) {
		acts, _, err := pg.ConvActivityList(conv.ID, 0, 50)
		if err == nil {
			for _, a := range acts {
				d := a.Summary
				if dd, err := pg.ConvActivityDetail(conv.ID, a.ID); err == nil {
					d += dd
				}
				if strings.Contains(d, "被引用消息的原文") && strings.Contains(d, "10.0.0.1") {
					found = true
					break
				}
			}
		}
		if found {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !found {
		t.Fatalf("wake 输入没有内联被引用消息的原文（消息 #%d）", refID)
	}
}

// TestDeliverAgentReplyFallbackTaskForMissingKey 钉住 B2 的兜底承办：agent 回复
// @ 到不存在的 key ⇒ 群里播报「已转自动建任务兜底」+ 任务真的建出来并绑定会话；
// 不存在的 key 不入席、不产生唤醒线程。
func TestDeliverAgentReplyFallbackTaskForMissingKey(t *testing.T) {
	s, pg, sid := newWakeTestSession(t)
	before := len(s.m.List())
	s.deliverAgentReplyToSession(sid, "auto", "@ghost 请对 192.168.1.1 做端口扫描", 0, "")
	deadline := time.Now().Add(15 * time.Second)
	// 兜底任务建出来了（launchTask 前的任务创建是同步段，轮询只是兜稳）。
	for time.Now().Before(deadline) {
		if len(s.m.List()) > before {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if len(s.m.List()) <= before {
		t.Fatalf("兜底任务没有被创建")
	}
	// 会话绑上了兜底任务。
	sess, err := pg.GetSession(sid)
	if err != nil {
		t.Fatalf("读会话失败: %v", err)
	}
	if sess == nil || sess.TaskID == nil {
		t.Fatalf("兜底任务没有绑回会话")
	}
	// 群里有了带任务编号的兜底播报。
	msgs, err := pg.SessionMessages(sid, 100)
	if err != nil {
		t.Fatalf("读会话流失败: %v", err)
	}
	announced := false
	for _, mm := range msgs {
		if strings.Contains(mm.Body, "不存在，已转自动建任务兜底") && strings.Contains(mm.Body, "【任务】#") {
			announced = true
			break
		}
	}
	if !announced {
		t.Fatalf("群里没有兜底建任务的播报")
	}
	// ghost 不在名册，也没有它的成员线程（不存在的人叫不动）。
	if _, ok := sessionMemberByKey(pg, sid, "ghost"); ok {
		t.Fatalf("不存在的 key 不应入席")
	}
}

// TestAnnounceUndeliveredMentionsEchoes 钉住 B4 的回声：未投递目标播报 note，
// 点名 @key 并给出兜底指引；没有未投递时不写任何消息。
func TestAnnounceUndeliveredMentionsEchoes(t *testing.T) {
	s, pg, sid := newWakeTestSession(t)
	count := func() int {
		msgs, err := pg.SessionMessages(sid, 100)
		if err != nil {
			t.Fatalf("读会话流失败: %v", err)
		}
		return len(msgs)
	}
	before := count()
	s.announceUndeliveredMentions(sid, []string{"ghost", "ghost2"})
	msgs, err := pg.SessionMessages(sid, 100)
	if err != nil {
		t.Fatalf("读会话流失败: %v", err)
	}
	echoed := false
	for _, mm := range msgs {
		if strings.Contains(mm.Body, "@ghost、@ghost2 不在席/不存在，消息无人接手") &&
			strings.Contains(mm.Body, "@everyone") {
			echoed = true
			break
		}
	}
	if !echoed {
		t.Fatalf("未投递回执没有播报（msgs=%d, before=%d）", len(msgs), before)
	}
	// 空集合不写空泡。
	s.announceUndeliveredMentions(sid, nil)
	if after := count(); after != len(msgs) {
		t.Fatalf("没有未投递时不应写消息（%d → %d）", len(msgs), after)
	}
}

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
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
)

// ---------- 纯函数层 ----------

func TestSessionWakePromptShape(t *testing.T) {
	msg := db.SessionMessage{ID: 42, FromAgent: "", Kind: db.SessionMessageKindNote,
		Body: "192.168.1.1 做一次渗透测试"}
	prompt := sessionWakePrompt("pentest", msg)
	for _, want := range []string{"@pentest", "来自：人", "消息 #42", msg.Body} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("唤醒提示缺 %q：\n%s", want, prompt)
		}
	}
	// agent 发来的定向消息：发件人标签是它的 key。
	msg.FromAgent = "auto"
	if got := sessionWakePrompt("pentest", msg); !strings.Contains(got, "来自：auto") {
		t.Fatalf("agent 发件人标签不对：\n%s", got)
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
	s.wakeSessionAgents(sid, []string{"pentest"}, msg, sessionWakeChainDepthCap+1)
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
	s.wakeSessionAgents(sid, []string{"pentest"}, msg, 0)

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
	s.wakeSessionAgents(sid, []string{"pentest"}, msg, 0)

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
	s.drainPendingWake(sid, "pentest", conv, 0)

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

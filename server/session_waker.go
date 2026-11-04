package server

// ---------- 会话级唤醒器（ROADMAP §4.3 / §9 决策①） ----------
//
// postSessionMessage 只**报告**唤醒集合（wake_agents）；真正把被 @ 的成员叫起来
// 跑一个 turn 的路径在这里。裁决（谁该醒）归 internal/sessionwake —— 本文件只做
// 执行：起 turn、写 presence、把回复镜像回会话流。
//
// 两段式的另一半（「自取」）不在这里：定向消息才起 turn（花额度）；未 @ 的广播
// 消息不唤醒任何人（等成员自己的下一轮按需自取）。所以唤醒器的唯一入口是
// MustWake 集合 —— 空集合 ⇒ 什么都不做。
//
// 「分开计量」（§9.1 约束①）：唤醒 turn 是普通会话对话线程上的 turn，token 用量
// 与人发消息起的 turn 走同一份 LLM 统计（llm_usage 按 profile 记账）；群组额度
// （group_quotas）只管辖任务大厅，普通 chat 会话不设唤醒额度闸 —— 这是已知且
// 刻意的取舍：@ 是人的显式动作，人不 @ 不花钱。
//
// 并发与失控边界（全部确定性，不靠模型自觉）：
//   - 同一成员同一时刻至多一个唤醒 turn（复用 chatBusy 守卫）；正在跑时到达的
//     定向消息暂存（sessionWakePending，上限 sessionWakePendingCap），turn 结束
//     后合并成一条后续唤醒；
//   - 接力深度硬顶（sessionWakeChainDepthCap）：A 唤醒 B、B 的回复又唤醒 C……
//     超过上限的定向消息照常落库，只是不再起新 turn（日志留痕）。

import (
	"log"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/internal/sessionwake"
)

// sessionWakeReplyMaxRunes 是镜像回会话流的单条回复上限（超出截断并指向线程）。
// 会话流是给人扫读的；完整原文永远在成员的对话线程（transcript）里。
const sessionWakeReplyMaxRunes = 4000

// sessionWakeChainDepthCap 是一次群聊接力最多唤起的深度。@-路由语义允许接力，
// 但必须给失控的互 @ 一个硬顶。
const sessionWakeChainDepthCap = 8

// sessionWakePendingCap 是成员正在跑时暂存的定向消息上限；超出丢弃最早的。
const sessionWakePendingCap = 5

// pendingWake 是一条暂存的定向消息（+ 它的接力深度）。
type pendingWake struct {
	msg   db.SessionMessage
	depth int
}

// wakeSessionAgents 对 MustWake 里的每个成员起一个唤醒 turn。
//
// msg 是**已落库**的消息（被拦下的消息在裁决层就已返回空集合，这里只是再守一道）；
// depth 是本次唤醒的接力深度（人发消息 = 0）。
func (s *Server) wakeSessionAgents(sessionID int64, wake []string, msg *db.SessionMessage, depth int) {
	if s == nil || s.m == nil || s.m.pg == nil || msg == nil || len(wake) == 0 {
		return
	}
	if msg.Status == db.SessionMessageBlocked {
		return
	}
	if depth > sessionWakeChainDepthCap {
		log.Printf("[session-wake] session %d 唤醒深度超过上限 %d，不再起 turn（消息 #%d）",
			sessionID, sessionWakeChainDepthCap, msg.ID)
		return
	}
	for _, key := range wake {
		key = strings.TrimSpace(key)
		if key == "" || key == msg.FromAgent {
			continue
		}
		s.startSessionWakeTurn(sessionID, key, *msg, depth)
	}
}

// startSessionWakeTurn 为一个被 @ 的成员起唤醒 turn（或在其正在跑时排队）。
//
// 找/建成员的对话线程 → busy 判定 → presence='working' → 后台 turn。
// turn 结束后（finishSessionWakeTurn）：镜像回复、presence 回 listening、
// 触发暂存消息的后续唤醒。
func (s *Server) startSessionWakeTurn(sessionID int64, agentKey string, msg db.SessionMessage, depth int) {
	s.launchSessionWakeTurn(sessionID, agentKey, sessionWakePrompt(agentKey, msg), depth, &msg)
}

// launchSessionWakeTurn 起一个唤醒 turn：busy 判定（正在跑则暂存 followUp ——
// 单条消息或 nil）→ presence='working' → 后台 turn。prompt 由调用方给全。
//
// 所有唤醒 turn 都从这一条路走（单条 @ 消息、drain 的合并唤醒），保证 chatBusy /
// sessionWakeChain / presence 的记账只有一份。
func (s *Server) launchSessionWakeTurn(sessionID int64, agentKey, prompt string, depth int, followUp *db.SessionMessage) {
	pg := s.m.pg
	c, err := pg.SessionConversationFor(sessionID, agentKey)
	if err != nil {
		log.Printf("[session-wake] session %d 为 %s 找/建对话线程失败: %v", sessionID, agentKey, err)
		return
	}
	busyKey := s.convBusyKey(c.ID)
	s.chatMu.Lock()
	busy := s.chatBusy[busyKey]
	if !busy {
		s.chatBusy[busyKey] = true
	}
	s.chatMu.Unlock()
	if busy {
		// 成员正在跑：暂存这条定向消息，turn 结束后合并成一条后续唤醒。
		// 不立刻起第二个 turn —— 同一对话线程并行两个 turn 会打乱 transcript。
		if followUp != nil {
			s.queuePendingWake(busyKey, *followUp, depth)
		} else {
			s.queuePendingWake(busyKey, db.SessionMessage{SessionID: sessionID,
				Kind: db.SessionMessageKindNote, Body: prompt}, depth)
		}
		return
	}
	// presence：working（@ 是显式请求；working 幂等覆盖 idle/listening 是预期，
	// blocked/left 的覆盖由收尾处的「只在 working 时回位」兜住）。
	if err := pg.SetSessionMemberPresence(sessionID, agentKey, db.SessionPresenceWorking, nil, ""); err != nil {
		log.Printf("[session-wake] session %d 置 %s 为 working 失败: %v", sessionID, agentKey, err)
	}
	go func() {
		s.chatMu.Lock()
		s.sessionWakeChain[busyKey] = depth + 1
		s.chatMu.Unlock()
		defer func() {
			s.chatMu.Lock()
			delete(s.sessionWakeChain, busyKey)
			s.chatMu.Unlock()
			if r := recover(); r != nil {
				log.Printf("[session-wake] session %d 唤醒 %s panic: %v", sessionID, agentKey, r)
			}
		}()
		// 唤醒提示先落 transcript（人能在「旧对话记录」里看到为什么被叫起来），
		// 再跑 turn；失败只记日志 —— 唤醒是尽力而为，消息本体已落库，重发即可。
		if _, err := s.m.pg.AppendConvActivity(c.ID, db.Activity{Worker: agentKey, Kind: "user",
			Summary: firstLine(prompt, 200), Detail: prompt}); err != nil {
			log.Printf("[session-wake] conv %d append wake prompt failed: %v", c.ID, err)
		}
		text, err := s.runConversationSync(c, prompt, busyKey)
		if err != nil {
			log.Printf("[session-wake] session %d 唤醒 %s 失败: %v", sessionID, agentKey, err)
		}
		s.finishSessionWakeTurn(sessionID, agentKey, c, text, depth+1)
	}()
}

// finishSessionWakeTurn 唤醒 turn 的确定性收尾：presence 回 listening、镜像回复
// （继续唤醒裁决）、触发暂存消息的后续唤醒。turn 失败时 text 为空 ⇒ 只做
// presence 与队列，不往群里写空泡。
func (s *Server) finishSessionWakeTurn(sessionID int64, agentKey string, c *db.Conversation, text string, depth int) {
	// 1) presence 回 listening —— 只在仍是 working 时回（成员可能在 turn 里把
	//    自己置成 blocked 等人决策；那个状态优先级更高，不覆盖）。
	if m, ok := sessionMemberByKey(s.m.pg, sessionID, agentKey); ok && m.Presence == db.SessionPresenceWorking {
		if err := s.m.pg.SetSessionMemberPresence(sessionID, agentKey, db.SessionPresenceListening, nil, ""); err != nil {
			log.Printf("[session-wake] session %d 置 %s 回 listening 失败: %v", sessionID, agentKey, err)
		}
	}
	// 2) 镜像回复进会话流（@ 到谁就继续叫醒谁；接力深度 +1）。
	if body := strings.TrimSpace(text); body != "" {
		s.deliverAgentReplyToSession(sessionID, agentKey, body, depth)
	}
	// 3) 暂存的定向消息合并成一条后续唤醒（成员这轮没看见的消息）。
	s.drainPendingWake(sessionID, agentKey, c, depth)
}

// deliverAgentReplyToSession 把成员 turn 的最终文本作为它的发言写回会话流。
//
// 写入走 AddSessionMessage（同一条准入判定），落库后按同一份 sessionwake 口径
// 继续唤醒。错误只记日志：镜像失败不影响已完成的 turn。
func (s *Server) deliverAgentReplyToSession(sessionID int64, agentKey, text string, depth int) {
	body := truncateRunes(text, sessionWakeReplyMaxRunes)
	if utf8.RuneCountInString(text) > sessionWakeReplyMaxRunes {
		body += "\n\n（回复过长已截断，完整内容见该成员的对话线程）"
	}
	m := &db.SessionMessage{SessionID: sessionID, FromAgent: agentKey, Kind: db.SessionMessageKindNote, Body: body}
	id, err := s.m.pg.AddSessionMessage(m)
	if err != nil {
		log.Printf("[session-wake] session %d 镜像 %s 的回复失败: %v", sessionID, agentKey, err)
		return
	}
	m.ID = id
	members, err := s.m.pg.SessionMembers(sessionID)
	if err != nil {
		log.Printf("[session-wake] session %d 读名册失败: %v", sessionID, err)
		return
	}
	decision := sessionwake.Decide(sessionwake.Message{
		Body:      m.Body,
		FromAgent: m.FromAgent,
		ToAgent:   m.ToAgent,
		Kind:      m.Kind,
		Blocked:   m.Status == db.SessionMessageBlocked,
	}, sessionWakeMembers(members))
	s.wakeSessionAgents(sessionID, decision.MustWake, m, depth)
}

// drainPendingWake 在成员的 turn 结束后，把暂存的定向消息合并成一条后续唤醒。
// 合并正文按到达顺序拼接（带每条来源），深度取队列里最大的 —— 封顶口径从紧。
func (s *Server) drainPendingWake(sessionID int64, agentKey string, c *db.Conversation, depth int) {
	busyKey := s.convBusyKey(c.ID)
	s.chatMu.Lock()
	q := s.sessionWakePending[busyKey]
	delete(s.sessionWakePending, busyKey)
	s.chatMu.Unlock()
	if len(q) == 0 {
		return
	}
	for _, p := range q {
		if p.depth > depth {
			depth = p.depth
		}
	}
	var b strings.Builder
	b.WriteString("你上一轮跑动期间，群聊里又到了几条指名给你的消息：")
	for _, p := range q {
		b.WriteString("\n\n—— 消息 #")
		b.WriteString(strconv.FormatInt(p.msg.ID, 10))
		b.WriteString("（来自 ")
		b.WriteString(wakeFromLabel(p.msg.FromAgent))
		b.WriteString("）——\n")
		b.WriteString(p.msg.Body)
	}
	// 合并唤醒直接以 prompt 形态起 turn（不走 sessionWakePrompt：per-message 的
	// 上下文已在合并正文里逐条写明；followUp=nil，busy 时整段 prompt 暂存）。
	s.launchSessionWakeTurn(sessionID, agentKey, b.String(), depth, nil)
}

// queuePendingWake 把成员正在跑期间到达的定向消息暂存到它的对话线程上。
// 上限 sessionWakePendingCap，超出丢最早的一条（新消息优先）。
func (s *Server) queuePendingWake(busyKey string, msg db.SessionMessage, depth int) {
	s.chatMu.Lock()
	defer s.chatMu.Unlock()
	q := append(s.sessionWakePending[busyKey], pendingWake{msg: msg, depth: depth})
	if len(q) > sessionWakePendingCap {
		q = q[len(q)-sessionWakePendingCap:]
	}
	s.sessionWakePending[busyKey] = q
	log.Printf("[session-wake] %s 正在跑，暂存定向消息 #%d（队列 %d）", busyKey, msg.ID, len(q))
}

// sessionWakePrompt 构造唤醒提示词。唤醒是确定性注入（不是模型的自由发挥）：
// 说清「谁、在哪、说了什么、回复会自动进群」，让模型把 turn 用在正事上。
func sessionWakePrompt(agentKey string, msg db.SessionMessage) string {
	var b strings.Builder
	b.WriteString("【群聊唤醒】会话里有一条指名给你的消息（@")
	b.WriteString(agentKey)
	b.WriteString(" / 显式指定收件人）。\n来自：")
	b.WriteString(wakeFromLabel(msg.FromAgent))
	b.WriteString("\n消息 #")
	b.WriteString(strconv.FormatInt(msg.ID, 10))
	b.WriteString(" 正文：\n")
	b.WriteString(msg.Body)
	b.WriteString("\n\n这是你在该群聊里的一个 turn：请直接处理消息里的请求或问题，")
	b.WriteString("并在回复里给出结论或下一步。你的最终回复会自动发回群聊（无需任何发消息工具）；")
	b.WriteString("若消息与你的分工无关，简要说明并 @ 合适的成员转交。")
	return b.String()
}

// wakeFromLabel 把发件人渲染成人话（空 = 人）。
func wakeFromLabel(from string) string {
	from = strings.TrimSpace(from)
	if from == "" {
		return "人"
	}
	return from
}

// truncateRunes 按 rune 截断（不切断多字节字符）。唤醒器与任务播报共用。
func truncateRunes(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	return string([]rune(s)[:max])
}

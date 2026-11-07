package server

// ---------- 会话级唤醒器（ROADMAP §4.3 / §9 决策①） ----------
//
// postSessionMessage 只**报告**唤醒集合（wake_agents）；真正把被叫的成员起来
// 跑一个 turn 的路径在这里。裁决（谁该醒）归 internal/sessionwake —— 本文件只做
// 执行：起 turn、写 presence、把回复镜像回会话流。
//
// 两条起 turn 的路径（自取不在这里 —— 它不起 turn）：
//   - @ 定向必醒（MustWake）：@agent / 显式 to_agent，定向消息才起 turn；
//   - 主持补位（HostWake）：**人**发的非定向消息落到分发者（auto）头上 —— 它是
//     群主持人：能直接回答，也能在回复里 @ 成员把事情转出去（转出即定向，回到
//     必醒接力）。这条让「人在群里随便说一句话」也有 agent 接话（类群机器人
//     体验），且兜底人是名册事实（谁是分发者），不靠模型自由发挥。
//
// 「分开计量」（§9.1 约束①）：唤醒 turn 是普通会话对话线程上的 turn，token 用量
// 与人发消息起的 turn 走同一份 LLM 统计（llm_usage 按 profile 记账）；群组额度
// （group_quotas）只管辖任务大厅，普通 chat 会话不设唤醒额度闸 —— 这是已知且
// 刻意的取舍：起 turn 的都是人的显式动作（发言/@）或它引出的明确接力。
//
// 并发与失控边界（全部确定性，不靠模型自觉）：
//   - 同一成员同一时刻至多一个唤醒 turn（复用 chatBusy 守卫）；正在跑时到达的
//     定向消息暂存（sessionWakePending，上限 sessionWakePendingCap），turn 结束
//     后合并成一条后续唤醒；
//   - 接力深度硬顶（sessionWakeChainDepthCap）：A 唤醒 B、B 的回复又唤醒 C……
//     超过上限的定向消息照常落库，只是不再起新 turn（日志留痕）；
//   - 接力链「同边去重」（wakeChainBook）：一次人发消息引发的整条接力链上，
//     「X 的回复叫醒 Y」这条边只允许发生一次 —— 客套互 @（A 回 B、B 回 A、
//     A 再回 B……）在第二个来回被确定性掐断，烧不穿额度。

import (
	"fmt"
	"log"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/internal/sessionwake"
)

// sessionWakeReplyMaxRunes 是镜像回会话流的单条回复上限（超出截断并指向线程）。
// 会话流是给人扫读的；完整原文永远在成员的对话线程（transcript）里。
const sessionWakeReplyMaxRunes = 4000

// sessionWakeQuoteMaxRefs / sessionWakeQuoteMaxRunes 是转交唤醒输入里内联的
// 引用消息条数 / 单条正文上限（B3）：转交说明只该是几句话，内联的原文不反过来
// 淹没它。
const (
	sessionWakeQuoteMaxRefs  = 3
	sessionWakeQuoteMaxRunes = 1200
)

// sessionWakeChainDepthCap 是一次群聊接力最多唤起的深度。@-路由语义允许接力，
// 但必须给失控的互 @ 一个硬顶。
const sessionWakeChainDepthCap = 8

// sessionWakePendingCap 是成员正在跑时暂存的定向消息上限；超出丢弃最早的。
const sessionWakePendingCap = 5

// wakeChainTTL 是接力链账本的存活时间：一条人发消息引发的接力通常在秒级结束，
// 15 分钟后链条账本必然过时，读取时顺手清掉（防 map 缓慢泄漏）。
const wakeChainTTL = 15 * time.Minute

// pendingWake 是一条暂存的定向消息（+ 它的接力深度）。
type pendingWake struct {
	msg   db.SessionMessage
	depth int
}

// ---------- 接力链「同边去重」账本 ----------

// wakeChainBook 记「哪些接力边在本链上发生过」。键是链身份（wakeChainIDForMessage，
// 即引发接力的人发消息 ID），值是该链上发生过的「from→to」边集合。
//
// 为什么是确定性账本而不是靠提示词：模型被明确告知「不要客套 @」也可能客套 @ ——
// 乒乓循环的代价是每轮一次 LLM turn，必须有一道不依赖模型自觉的闸。同一链上
// A→B 只放行一次 ⇒ 乒乓在第二个来回必然断掉，而正常的「A 派活 B → B 回报时
// @A 请示 → A 批示 @B 干」多轮协作不受影响（每次 from→to 方向都不同或间隔了
// 新链）。
type wakeChainBook struct {
	mu     sync.Mutex
	chains map[string]*wakeChain
}

type wakeChain struct {
	edges   map[string]bool
	created time.Time
}

// allow 记一条边并报告它是否首次出现（重复 = 已发生过的接力，拒绝）。
// 没有链身份（chainID 空）时恒放行 —— 旧调用方与测试不必伪造链身份。
func (b *wakeChainBook) allow(chainID, from, to string) bool {
	if chainID == "" || to == "" {
		return true
	}
	now := time.Now()
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.chains == nil {
		b.chains = map[string]*wakeChain{}
	}
	for id, c := range b.chains { // TTL 清扫（写入时顺手，无独立后台协程）
		if now.Sub(c.created) > wakeChainTTL {
			delete(b.chains, id)
		}
	}
	c, ok := b.chains[chainID]
	if !ok {
		c = &wakeChain{edges: map[string]bool{}, created: now}
		b.chains[chainID] = c
	}
	edge := from + "\x00" + to
	if c.edges[edge] {
		return false
	}
	c.edges[edge] = true
	return true
}

// wakeChainIDForMessage 是一条消息引发的接力链的身份（引发者 = 人发消息的 ID）。
// 整条链上所有后续唤醒共享它，同边去重才有全局视野。
func wakeChainIDForMessage(msg *db.SessionMessage) string {
	if msg == nil {
		return ""
	}
	return fmt.Sprintf("m%d", msg.ID)
}

// ---------- 唤醒执行 ----------

// wakeSessionAgents 对 wake 里的每个成员起一个唤醒 turn。
//
// msg 是**已落库**的消息（被拦下的消息在裁决层就已返回空集合，这里只是再守一道）；
// depth 是本次唤醒的接力深度（人发消息 = 0）；chainID 是接力链身份；
// quote 是随 wake 输入内联的引用消息原文（B3；空串 = 没有引用，输入不变）；
// host 为真表示这是主持补位 turn（提示词换成主持人口吻）。
func (s *Server) wakeSessionAgents(sessionID int64, wake []string, msg *db.SessionMessage, depth int, chainID, quote string, host bool) {
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
		// 同边去重：本链上「msg.FromAgent → key」发生过就不再重复 ——
		// 客套互 @ 的乒乓在第二个来回被确定性掐断。
		if !s.wakeChains.allow(chainID, msg.FromAgent, key) {
			log.Printf("[session-wake] session %d 链 %s 已发生过 %s→%s 的接力，跳过重复唤醒（消息 #%d）",
				sessionID, chainID, wakeFromLabel(msg.FromAgent), key, msg.ID)
			continue
		}
		s.startSessionWakeTurn(sessionID, key, *msg, depth, chainID, quote, host)
	}
}

// startSessionWakeTurn 为一个被叫的成员起唤醒 turn（或在其正在跑时排队）。
//
// 找/建成员的对话线程 → busy 判定 → presence='working' → 后台 turn。
// turn 结束后（finishSessionWakeTurn）：镜像回复、presence 回 listening、
// 触发暂存消息的后续唤醒。
func (s *Server) startSessionWakeTurn(sessionID int64, agentKey string, msg db.SessionMessage, depth int, chainID, quote string, host bool) {
	prompt := sessionWakePrompt(agentKey, msg, quote)
	if host {
		// 主持补位（B1）：运行时注入当前在席名册 —— 转交只能 @ 在席成员，
		// 需要的人不在席就走 spawn_task 兜底，而不是照着示范凭空编 key。
		prompt = sessionWakeHostPrompt(agentKey, msg, s.sessionRosterLabels(sessionID))
	}
	s.launchSessionWakeTurn(sessionID, agentKey, prompt, depth, &msg, chainID)
}

// launchSessionWakeTurn 起一个唤醒 turn：busy 判定（正在跑则暂存 followUp ——
// 单条消息或 nil）→ presence='working' → 后台 turn。prompt 由调用方给全。
//
// 所有唤醒 turn 都从这一条路走（单条 @ 消息、主持补位、drain 的合并唤醒），
// 保证 chatBusy / sessionWakeChain / presence 的记账只有一份。
func (s *Server) launchSessionWakeTurn(sessionID int64, agentKey, prompt string, depth int, followUp *db.SessionMessage, chainID string) {
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
	// presence：working（被叫是显式动作；working 幂等覆盖 idle/listening 是预期，
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
		s.finishSessionWakeTurn(sessionID, agentKey, c, text, depth+1, chainID)
	}()
}

// finishSessionWakeTurn 唤醒 turn 的确定性收尾：presence 回 listening、镜像回复
// （继续唤醒裁决）、触发暂存消息的后续唤醒。turn 失败时 text 为空 ⇒ 只做
// presence 与队列，不往群里写空泡。
func (s *Server) finishSessionWakeTurn(sessionID int64, agentKey string, c *db.Conversation, text string, depth int, chainID string) {
	// 1) presence 回 listening —— 只在仍是 working 时回（成员可能在 turn 里把
	//    自己置成 blocked 等人决策；那个状态优先级更高，不覆盖）。
	if m, ok := sessionMemberByKey(s.m.pg, sessionID, agentKey); ok && m.Presence == db.SessionPresenceWorking {
		if err := s.m.pg.SetSessionMemberPresence(sessionID, agentKey, db.SessionPresenceListening, nil, ""); err != nil {
			log.Printf("[session-wake] session %d 置 %s 回 listening 失败: %v", sessionID, agentKey, err)
		}
	}
	// 2) 镜像回复进会话流（回复里 @ 到谁就继续叫醒谁；接力深度 +1）。
	if body := strings.TrimSpace(text); body != "" {
		s.deliverAgentReplyToSession(sessionID, agentKey, body, depth, chainID)
	}
	// 3) 暂存的定向消息合并成一条后续唤醒（成员这轮没看见的消息）。
	s.drainPendingWake(sessionID, agentKey, c, depth, chainID)
}

// deliverAgentReplyToSession 把成员 turn 的最终文本作为它的发言写回会话流。
//
// 写入走 AddSessionMessage（同一条准入判定），落库后按同一份 sessionwake 口径
// 继续唤醒。落库与唤醒裁决之间补齐与人的消息相同的两件事（B2）：
//   - 热加入：回复里 @ 到不在席的成员先入席再被定向 —— 否则 agent 之间的转交
//     永远叫不动没入席的人（「人 @ 谁谁都醒」的既有体验，agent 回复同样享受）；
//   - 不存在的 key（agents 表里没有）：转交意图不能凭空消失 ⇒ 兜底建任务承接。
// 错误只记日志：镜像失败不影响已完成的 turn。
func (s *Server) deliverAgentReplyToSession(sessionID int64, agentKey, text string, depth int, chainID string) {
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
	// 热加入（B2，与人发消息同款）；missing 是 @ 到的不存在 key（兜底建任务用）。
	mentions := db.ParseSessionAgentMentions(m.Body)
	members, missing := hotJoinMentioned(s.m.pg, sessionID, mentions, members, agentKey)
	decision := sessionwake.Decide(sessionwake.Message{
		Body:      m.Body,
		FromAgent: m.FromAgent,
		ToAgent:   m.ToAgent,
		Kind:      m.Kind,
		Blocked:   m.Status == db.SessionMessageBlocked,
	}, sessionWakeMembers(members))
	// agent 发件人 ⇒ HostWake 恒为空（主持补位只对人）；接力只走定向路径。
	// 引用内联（B3）：转交说明里的 #N / parent 指针展开成原文，随 wake 输入送达。
	s.wakeSessionAgents(sessionID, decision.MustWake, m, depth, chainID, s.sessionQuotedContext(sessionID, m), false)
	// 未投递回执（B4）：扣掉已走兜底建任务的 missing —— 它们有带任务编号的
	// 专门播报，不重复喊「无人接手」。
	s.announceUndeliveredMentions(sessionID, stringsSubtract(decision.Undelivered, missing))
	// 兜底承办（B2）：@ 到不存在的 key ⇒ 建任务接住转交意图。被拦下的消息
	// 不进任何人的上下文（fail-closed），同样不兜底。
	if len(missing) > 0 && m.Status != db.SessionMessageBlocked {
		s.spawnFallbackTaskForMentions(sessionID, agentKey, missing, m)
	}
}

// sessionQuotedContext 收集一条消息里引用的消息原文，拼成可内联进 wake 输入的
// 上下文块（B3）。
//
// 线上实锤（会话 77）：pentest 收到的 wake 输入只有「内容与 #197 相同、已转交」
// 这一句 —— 转交说明是个指针，接单方手里却没有指针指向的内容，只能干等。这里
// 把 parent_message_id 与正文里的 #N 指针展开成原文（每条封顶截断、至多 3 条）；
// 被引用的消息被拦（blocked）或收据条不内联（它们本就不进接收方上下文）。没有
// 引用时返回空串，wake 输入维持原状。
func (s *Server) sessionQuotedContext(sessionID int64, m *db.SessionMessage) string {
	if s == nil || s.m == nil || s.m.pg == nil || m == nil {
		return ""
	}
	ids := quotedMessageIDs(m)
	if len(ids) == 0 {
		return ""
	}
	var b strings.Builder
	for _, id := range ids {
		ref, err := s.m.pg.SessionMessageByID(sessionID, id)
		if err != nil {
			log.Printf("[session-wake] session %d 读引用消息 #%d 失败: %v", sessionID, id, err)
			continue
		}
		if ref == nil || ref.Status == db.SessionMessageBlocked || ref.Kind == db.SessionMessageKindReceipt {
			continue // 指向不存在的消息（多半是模型编的号）⇒ 跳过，不编造内容
		}
		b.WriteString("消息 #")
		b.WriteString(strconv.FormatInt(ref.ID, 10))
		b.WriteString("（来自 ")
		b.WriteString(wakeFromLabel(ref.FromAgent))
		b.WriteString("）：\n")
		b.WriteString(truncateRunes(strings.TrimSpace(ref.Body), sessionWakeQuoteMaxRunes))
		b.WriteString("\n\n")
	}
	return strings.TrimSpace(b.String())
}

// sessionMessageRefPattern 匹配正文里的消息指针（#197）。要求 # 后是纯数字；
// 边界由 quotedMessageIDs 判定（# 前不能是字母/数字/# —— 排除 URL 片段、
// markdown 标题的误伤）。
var sessionMessageRefPattern = regexp.MustCompile(`#([0-9]+)`)

// quotedMessageIDs 从一条消息里解析「引用了哪些消息」：parent_message_id（线程
// 回复）+ 正文里的 #N 指针（转交说明的惯用形态），保序去重、排除自身、封顶
// sessionWakeQuoteMaxRefs。
func quotedMessageIDs(m *db.SessionMessage) []int64 {
	ids := []int64{}
	seen := map[int64]bool{}
	add := func(id int64) {
		if id > 0 && id != m.ID && !seen[id] && len(ids) < sessionWakeQuoteMaxRefs {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	if m.ParentMessageID != nil {
		add(*m.ParentMessageID)
	}
	for _, loc := range sessionMessageRefPattern.FindAllStringSubmatchIndex(m.Body, -1) {
		if loc[2] < 0 {
			continue
		}
		// 边界判定：# 前面不能是字母/数字/#（https://x/page#15、##标题 都不算指针）。
		if loc[0] > 0 {
			r, _ := utf8.DecodeLastRuneInString(m.Body[:loc[0]])
			if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '#' {
				continue
			}
		}
		id, err := strconv.ParseInt(m.Body[loc[2]:loc[3]], 10, 64)
		if err != nil {
			continue
		}
		add(id)
	}
	return ids
}

// announceUndeliveredMentions 未投递回执（B4）：被 @ 的目标没有投递成功时给群
// 播报一条 note —— 任何转交都必须有回声，「@ 出去之后沉默十几分钟无人接手」是
// 这次停摆最伤体验的点。播报走 s.say（平台 note，不触发唤醒裁决，不会自我递归）。
func (s *Server) announceUndeliveredMentions(sessionID int64, undelivered []string) {
	if s == nil || s.m == nil || s.m.pg == nil || len(undelivered) == 0 {
		return
	}
	labeled := make([]string, 0, len(undelivered))
	for _, key := range undelivered {
		labeled = append(labeled, "@"+strings.TrimSpace(key))
	}
	s.say(sessionID, 0, "", "",
		strings.Join(labeled, "、")+" 不在席/不存在，消息无人接手——可 @everyone 让 auto 兜底或直接派任务")
}

// spawnFallbackTaskForMentions 兜底承办（B2）：agent 回复里 @ 了不存在的 key，
// 那份「转交意图」不能凭空消失 —— 复用 launchTask 共享路径建一个任务接住，
// 尽力绑回会话（这里没有 run ctx 可供 bindSpawnedTaskToSession 解析，直接走
// BindSessionTask + 内存路由登记，结果与 spawn_task 的绑回同一形态），并把任务
// 编号播报回群。失败只记日志：兜底是尽力而为，回复本体已落库。
func (s *Server) spawnFallbackTaskForMentions(sessionID int64, from string, keys []string, m *db.SessionMessage) {
	goal := strings.TrimSpace(m.Body)
	if goal == "" {
		return
	}
	labeled := make([]string, 0, len(keys))
	for _, key := range keys {
		labeled = append(labeled, "@"+strings.TrimSpace(key))
	}
	desc := "兜底任务（" + strings.Join(labeled, "、") + " 不存在，转交意图改为任务承接）"
	t, err := s.m.CreateTaskWithOptions(desc, goal, db.TaskCreateOptions{})
	if err != nil {
		log.Printf("[session-wake] session %d 兜底建任务失败: %v", sessionID, err)
		return
	}
	taskID, _ := strconv.ParseInt(t.ID, 10, 64)
	bound := false
	if taskID > 0 {
		if ok, err := s.m.pg.BindSessionTask(sessionID, taskID); err != nil {
			log.Printf("[session-wake] session %d 绑定兜底任务 %d 失败: %v", sessionID, taskID, err)
		} else {
			bound = ok
		}
	}
	binding := "并绑定本会话"
	if !bound {
		binding = "（会话已绑定其他任务，本任务不占绑定）"
	}
	s.say(sessionID, taskID, from, from,
		strings.Join(labeled, "、")+" 不存在，已转自动建任务兜底。\n【任务】#"+
			strconv.FormatInt(taskID, 10)+" 已创建"+binding+"。\n描述："+desc+
			"\n目标："+truncateRunes(goal, sessionWakeQuoteMaxRunes))
	// launchTask 内部异步（seed + 后台目标分解 + engine.Run），与 HTTP 建任务、
	// spawn_task 两条既有路径行为一致；绑定成功才登记内存路由（播报跟绑定走）。
	s.launchTask(t, desc+" "+goal, false)
	if bound {
		s.registerTaskSessionBinding(taskID, sessionID)
	}
}

// stringsSubtract 从 base 里剔除 except 中出现过的元素（保序）。未投递回执要
// 扣掉「已走兜底建任务」的 key —— 不值得为此引入集合库，一小段够了。
func stringsSubtract(base, except []string) []string {
	if len(base) == 0 || len(except) == 0 {
		return base
	}
	skip := map[string]bool{}
	for _, e := range except {
		skip[e] = true
	}
	out := make([]string, 0, len(base))
	for _, b := range base {
		if !skip[b] {
			out = append(out, b)
		}
	}
	return out
}

// drainPendingWake 在成员的 turn 结束后，把暂存的定向消息合并成一条后续唤醒。
// 合并正文按到达顺序拼接（带每条来源），深度取队列里最大的 —— 封顶口径从紧。
func (s *Server) drainPendingWake(sessionID int64, agentKey string, c *db.Conversation, depth int, chainID string) {
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
	s.launchSessionWakeTurn(sessionID, agentKey, b.String(), depth, nil, chainID)
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

// sessionWakePrompt 构造定向唤醒提示词。唤醒是确定性注入（不是模型的自由发挥）：
// 说清「谁、在哪、说了什么、回复会自动进群」，让模型把 turn 用在正事上。
// quoted 是被引用消息的内联原文（B3；空串 = 没有引用）—— 转交说明常常只是指针
// （「内容与 #N 相同、已转交」），原文不内联，接单方拿到的是空指针。
func sessionWakePrompt(agentKey string, msg db.SessionMessage, quoted string) string {
	var b strings.Builder
	b.WriteString("【群聊唤醒】会话里有一条指名给你的消息（@")
	b.WriteString(agentKey)
	b.WriteString(" / 显式指定收件人）。\n来自：")
	b.WriteString(wakeFromLabel(msg.FromAgent))
	b.WriteString("\n消息 #")
	b.WriteString(strconv.FormatInt(msg.ID, 10))
	b.WriteString(" 正文：\n")
	b.WriteString(msg.Body)
	if quoted != "" {
		b.WriteString("\n\n—— 被引用消息的原文（转交所指的内容，以它为准）——\n")
		b.WriteString(quoted)
	}
	b.WriteString("\n\n这是你在该群聊里的一个 turn：请直接处理消息里的请求或问题，")
	b.WriteString("并在回复里给出结论或下一步。你的最终回复会自动发回群聊（无需任何发消息工具）；")
	b.WriteString("若消息与你的分工无关，简要说明并 @ 合适的成员转交。\n")
	b.WriteString("@ 纪律：只有要把事情转交给具体成员时才 @ 它；不要为了客套、确认、收尾而 @")
	b.WriteString("（每次 @ 都会把对方叫起来跑一轮）；不要在回复开头复述自己的名字。")
	return b.String()
}

// sessionWakeHostPrompt 构造主持补位的提示词：人的非定向消息落到分发者（主持人）
// 头上。目标口径：像群机器人一样「人一说话就有人接」——主持人先接话，需要分工
// 时用 @ 把在席成员拉进来（那一步是定向消息，走必醒接力）。
//
// 名册感知（B1，停摆根因）：转交示范不再硬编码具体名字（「例如 @pentest」会让
// 模型照着编不在席的 key，@ 出去石沉大海），运行时注入当前在席名册；需要的成员
// 不在席时禁止凭空 @，直接用 spawn_task 建任务兜底并播报任务编号 —— auto 由此
// 从「转交员」升格为「转交员 + 兜底承办员」，与任务大厅里 auto 的「分发者 +
// 兜底执行」双重身份（server/manager.go 的建任务口径）对齐。
func sessionWakeHostPrompt(agentKey string, msg db.SessionMessage, roster string) string {
	var b strings.Builder
	b.WriteString("【群聊主持】群里来了一条没有点名任何人的消息。你是本群的分发者（主持人），由你先接话：\n")
	if roster != "" {
		b.WriteString("- 本群在席成员：")
		b.WriteString(roster)
		b.WriteString("；\n")
	}
	b.WriteString("- 能直接回答/判断的，直接给结论，不绕弯；\n")
	b.WriteString("- 需要转交时只能 @ 在席成员，一句话说明要它做什么；需要的成员不在席时")
	b.WriteString("不要凭空 @ 不存在的名字，直接用 spawn_task 建任务兜底，并在回复里播报任务编号；\n")
	b.WriteString("- 没有必要就不要 @ 任何人（每次 @ 都会叫醒对方跑一轮，客套式互 @ 会烧穿额度），")
	b.WriteString("也不要在回复开头复述自己的名字。\n\n")
	b.WriteString("来自：人\n消息 #")
	b.WriteString(strconv.FormatInt(msg.ID, 10))
	b.WriteString(" 正文：\n")
	b.WriteString(msg.Body)
	b.WriteString("\n\n你的最终回复会自动发回群聊（无需任何发消息工具）。")
	return b.String()
}

// sessionRoleLabel 把成员分工渲染成人话（主持提示词的名册用）。
func sessionRoleLabel(role string) string {
	switch role {
	case db.SessionRoleDispatcher:
		return "分发者"
	case db.SessionRoleMember:
		return "成员"
	case db.SessionRoleObserver:
		return "观察者"
	}
	return role
}

// sessionRosterLabels 读当前名册，渲染成「key（分工）」的在席列表（主持提示词
// 注入用）。读不到名册返回空串 —— 提示词退化为不带名册的版本，转交规则段仍然
// 成立（读不到名册本来也就没有可转交的对象）。
func (s *Server) sessionRosterLabels(sessionID int64) string {
	if s == nil || s.m == nil || s.m.pg == nil {
		return ""
	}
	members, err := s.m.pg.SessionMembers(sessionID)
	if err != nil {
		log.Printf("[session-wake] session %d 读名册失败: %v", sessionID, err)
		return ""
	}
	parts := make([]string, 0, len(members))
	for _, m := range members {
		if m.HasLeft || m.Presence == db.SessionPresenceLeft {
			continue
		}
		parts = append(parts, strings.TrimSpace(m.AgentKey)+"（"+sessionRoleLabel(m.Role)+"）")
	}
	return strings.Join(parts, "、")
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

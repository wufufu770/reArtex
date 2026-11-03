package db

// 会话消息的**噪音治理**：线程折叠与收件条（inbox/digest）的确定性计算。
//
// 依据：session-groupchat-proposal.md §7 的 P5 行「收件条/线程折叠等噪音治理」
// 与 §2.3 第 7 条（「已完成」类消息渲染成 ✓✓ 收据条，不打气泡；补充意见折叠在
// 线程里）、§4.1（session_messages.parent_message_id / receipt_of）。
//
// 本文件是**纯函数层**：不查库、不调模型、不读可变全局状态。同一份消息列表
// 永远得到同一份分组与同一份摘要 ⇒ 摘要可以进缓存、可以进测试、可以当作确定性
// 裁决（红线：模型只提议，确定性层裁决）。
//
// 为什么噪音必须由确定性层处理：收件条的内容会**进入模型上下文**（agent 侧的
// 群邮箱摘要就是同款），而模型总结「刚才发生了什么」不可复算、不可审计、也没法
// 保证不把「收到/好的」再总结一遍 ⇒ 折叠与摘要在 Go 里算好，模型只拿到结果。
//
// 四条被写死的规则（后面每处代码都对应其中一条）：
//
//	R1 收据条不占气泡。kind='receipt' 的行永远挂在 receipt_of 指向的那条消息下；
//	   挂不上（目标不在本次窗口里）就只计数、不渲染 —— 收据是确认噪音，不是内容。
//	R2 线程折叠。parent_message_id 非空的消息折叠到它的**最上层祖先**上；
//	   一个线程 = 一个气泡。祖先不在窗口里 ⇒ 提升为独立根（不丢内容）。
//	R3 摘要只看 agent 说的话。from_agent='' 是人 —— 人自己写的消息不需要
//	   「你不在时发生了什么」的摘要（用户当然记得自己说了什么）。
//	R4 摘要按**优先级类**挑选，每类有硬上限。escalation > finding >
//	   request > answer-to-request > note；note 是群聊最大的噪音源（§2.3 第 7 条），
//	   排在最后 ⇒ 只有在前面几类都凑不满上限时才会露面。
//
// 允许：SIZE_OK —— 分组规则、摘要规则、计数口径写在同一个上下文里才看得清；
// 把它们拆成两个文件会把「哪些消息算一个气泡」这条定义切成两份副本。

import (
	"sort"
	"strings"
	"time"
)

// ---------- 噪音治理的代码常量（不进 DB、不进 settings）----------

// sessionDigestClassCaps 是摘要里每一类最多露出几条。
//
// 硬上限而不是比例：比例会随窗口大小漂移，同一段历史两次摘要可能给出不同的
// 条数 ⇒ 「摘要不可复算」。上限写死在代码里 ⇒ 可复算、可测试。
//
// 取值偏小是刻意的：摘要是**收件条**，目的是让用户一眼看完。真要逐条读，用户
// 点开线程看全文（折叠线程的展开视图不受这个上限约束）。
var sessionDigestClassCaps = map[string]int{
	SessionMessageKindEscalation: 3,
	SessionMessageKindFinding:    3,
	SessionMessageKindRequest:    3,
	SessionMessageKindAnswer:     3,
	SessionMessageKindNote:       2,
}

// sessionDigestMaxClassTotal 是摘要里露出条目的总上限。
//
// 各类的上限加起来是 14 条 —— 那还是太多。总上限把「用户要读的气泡数」压到
// 一屏以内；超出的部分只进计数（摘要里明写「还有 M 条」），不丢内容。
const sessionDigestMaxClassTotal = 8

// 摘要的优先级类别名。与消息 kind 分开：answer 还要按父消息的 kind 再分一次类。
const (
	sessionDigestClassEscalation = "escalation"
	sessionDigestClassFinding    = "finding"
	sessionDigestClassRequest    = "request"
	sessionDigestClassAnswer     = "answer"
	sessionDigestClassNote       = "note"
)

// sessionDigestPriorityOrder 是挑选顺序里的类别顺序（确定性）。
var sessionDigestPriorityOrder = []string{
	sessionDigestClassEscalation,
	sessionDigestClassFinding,
	sessionDigestClassRequest,
	sessionDigestClassAnswer,
	sessionDigestClassNote,
}

// sessionDigestBodyPreviewMax 是摘要里单条正文预览的字符上限。
//
// 摘要的作用是「扫一眼」，不是「读完」。正文全文在折叠线程的展开视图里。
const sessionDigestBodyPreviewMax = 120

// sessionThreadMaxDepth 是线程祖先回溯的最大层数。
//
// parent_message_id 是自引用外键，理论上可以是任意深度。回溯有上限是为了防
// 脏数据造成的环 —— 上限 + 环检测 ⇒ 任何输入都不会死循环。
const sessionThreadMaxDepth = 64

// ---------- 结构体 ----------

// SessionReceipt 是贴在某条消息下面的收据条（不占气泡）。
type SessionReceipt struct {
	ID            int64  `json:"id"`
	FromAgent     string `json:"from_agent"`
	ReceiptAction string `json:"receipt_action"`
	CreatedAt     string `json:"created_at"`
}

// SessionThread 是一个折叠后的线程：一条根消息 + 它的补充意见 + 它的收据条。
//
// ReplyCount 是**折叠掉的条数**（= 用户少读的气泡数）。Replies 保留全文 ——
// 折叠是**呈现层**的事，数据层不丢内容（红线：不删数据）。
type SessionThread struct {
	Root     SessionMessage   `json:"root"`
	Replies  []SessionMessage `json:"replies"`
	Receipts []SessionReceipt `json:"receipts"`
	// ReplyCount = len(Replies)；单独给一个字段是为了让前端不必先算长度。
	ReplyCount int `json:"reply_count"`
	// Collapsed 表示这条线程有折叠内容（前端据此显示「N 条补充」）。
	Collapsed bool `json:"collapsed"`
}

// SessionDigestItem 是摘要里露出的一条。
type SessionDigestItem struct {
	MessageID  int64  `json:"message_id"`
	SessionID  int64  `json:"session_id"`
	FromAgent  string `json:"from_agent"`
	Kind       string `json:"kind"`
	Class      string `json:"class"`
	Preview    string `json:"preview"`
	CreatedAt  string `json:"created_at"`
	ThreadID   int64  `json:"thread_id"`
	ReplyCount int    `json:"reply_count"`
}

// SessionDigestCounts 是摘要窗口内的分类计数（让人一眼知道「有多少」，
// 而不必点开就知道「有多少条被折叠了」）。
type SessionDigestCounts struct {
	Total         int            `json:"total"`
	ByKind        map[string]int `json:"by_kind"`
	ByAgent       map[string]int `json:"by_agent"`
	Escalations   int            `json:"escalations"`
	Findings      int            `json:"findings"`
	Requests      int            `json:"requests"`
	Answers       int            `json:"answers"`
	Notes         int            `json:"notes"`
	Receipts      int            `json:"receipts"`
	Blocked       int            `json:"blocked"`
	Threads       int            `json:"threads"`
	Replies       int            `json:"replies"`
	HumanAuthored int            `json:"human_authored"`
}

// SessionDigest 是「你不在时发生了什么」的收件条。
//
// 四个数字是验收口径（「群消息气泡数下降可量化」）：
//
//	RawBubbles    不做任何折叠时用户要读的气泡数（每条非收据消息一个气泡）
//	ThreadBubbles 只做线程折叠后要读的气泡数（一个线程一个气泡）
//	DigestBubbles 收件条展开后要读的气泡数（= len(Items)）
//	BubblesSaved  RawBubbles - DigestBubbles（省下来的）
type SessionDigest struct {
	SessionID int64 `json:"session_id"`
	// SinceMessageID 是水位线（**不含**它本身）：只有 id 更大的消息算「你不在时」。
	SinceMessageID int64 `json:"since_message_id"`
	// LatestMessageID 是窗口内最大的消息 id（0 = 窗口为空）。
	LatestMessageID int64 `json:"latest_message_id"`
	// Items 是摘要露出的条目（按优先级类、再按 id 升序）。
	Items []SessionDigestItem `json:"items"`
	// Counts 是窗口内的分类计数。
	Counts SessionDigestCounts `json:"counts"`
	// Overflow 是各优先级类被上限挡掉、只进了计数的条数。
	Overflow map[string]int `json:"overflow"`

	RawBubbles    int `json:"raw_bubbles"`
	ThreadBubbles int `json:"thread_bubbles"`
	DigestBubbles int `json:"digest_bubbles"`
	BubblesSaved  int `json:"bubbles_saved"`
	// ReductionPct 是省下的比例（0-100 的整数，raw 为 0 时是 0）。
	ReductionPct int `json:"reduction_pct"`
}

// ---------- 分组（线程折叠）----------

// GroupSessionMessages 把一条消息流折叠成线程列表。
//
// 输入可以是会话的**全部**消息（用于时间线），也可以是某个窗口的消息（用于
// 摘要）—— 函数本身不关心窗口，它只做分组。窗口过滤在 BuildSessionDigest 里。
//
// 确定性保证：
//   - 输出顺序 = 根消息的 id 升序（与消息流的时间顺序一致）；
//   - 同一个输入永远得到同一个输出（不读时间、不读随机数、不读全局状态）。
//
// R2 的祖先回溯：parent_message_id 指向的是**任意**祖先（可以指回复），折叠要
// 落到最上层祖先 ⇒ 沿 parent 链往上走。链上有环时把环里 id 最小的成员定为根
// （环的两端因此落到同一个根 ⇒ 没有消息会丢）；链超深时在截断点定根（截断是
// 永久的 ⇒ 后续解析一致）。
func GroupSessionMessages(msgs []SessionMessage) []SessionThread {
	if len(msgs) == 0 {
		return []SessionThread{}
	}
	// 按 id 升序处理（复制一份，不改调用方的切片）。
	ordered := make([]SessionMessage, len(msgs))
	copy(ordered, msgs)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].ID < ordered[j].ID })

	byID := make(map[int64]SessionMessage, len(ordered))
	for _, m := range ordered {
		byID[m.ID] = m
	}

	// uf[x] 是 x 的父指针；根指向自己。
	uf := make(map[int64]int64, len(ordered))
	for _, m := range ordered {
		uf[m.ID] = m.ID
	}
	for _, m := range ordered {
		if m.ParentMessageID == nil {
			continue
		}
		if _, ok := byID[*m.ParentMessageID]; !ok {
			// 祖先不在本次列表里（窗口截断）⇒ 视为根（R2：不丢内容）。
			continue
		}
		uf[m.ID] = *m.ParentMessageID
	}
	root := make(map[int64]int64, len(ordered))
	for _, m := range ordered {
		root[m.ID] = sessionThreadFindRoot(uf, m.ID)
	}

	threads := []SessionThread{}
	index := map[int64]int{}
	receiptTargets := map[int64][]SessionReceipt{}

	// 第一遍：落根消息。
	for _, m := range ordered {
		if m.Kind == SessionMessageKindReceipt {
			// R1：收据条永远不占气泡。它挂到 receipt_of 指向的消息所在的线程上。
			if m.ReceiptOf == nil {
				continue
			}
			receiptTargets[*m.ReceiptOf] = append(receiptTargets[*m.ReceiptOf], SessionReceipt{
				ID:            m.ID,
				FromAgent:     m.FromAgent,
				ReceiptAction: m.ReceiptAction,
				CreatedAt:     sessionDigestTime(m.CreatedAt),
			})
			continue
		}
		if root[m.ID] != m.ID {
			continue // 归到祖先线程（第二遍处理）
		}
		index[m.ID] = len(threads)
		threads = append(threads, SessionThread{Root: m})
	}

	// 第二遍：把回复折叠到根线程上。
	for _, m := range ordered {
		if m.Kind == SessionMessageKindReceipt {
			continue
		}
		r := root[m.ID]
		if r == m.ID {
			continue
		}
		pos, ok := index[r]
		if !ok {
			// 理论上不可达（根必然在 index 里）；真发生了也不能丢消息 ⇒
			// 提升为独立根。
			index[m.ID] = len(threads)
			threads = append(threads, SessionThread{Root: m})
			continue
		}
		threads[pos].Replies = append(threads[pos].Replies, m)
	}

	// 第三遍：收据条贴到各自的线程上（挂在根上或挂在某条回复上都算这条线程的）。
	for i := range threads {
		t := &threads[i]
		if rs, ok := receiptTargets[t.Root.ID]; ok {
			t.Receipts = append(t.Receipts, rs...)
		}
		for _, reply := range t.Replies {
			if rs, ok := receiptTargets[reply.ID]; ok {
				t.Receipts = append(t.Receipts, rs...)
			}
		}
		t.ReplyCount = len(t.Replies)
		t.Collapsed = t.ReplyCount > 0
	}
	return threads
}

// sessionThreadFindRoot 从 start 出发找到线程根（会修改 uf 做路径压缩与定根）。
//
// uf 是父指针表（每个节点最多一个父）。用显式路径 + 环检测：
//   - 正常链：走到「父指向自己」的那一层就是根；
//   - 有环：环内 id 最小的成员定为根，并把环内所有成员直接指向它
//     ⇒ 环的两端落到同一个根，没有消息会变成孤儿；
//   - 超深：在截断点永久定根（截断写回 uf ⇒ 后续解析一致）。
func sessionThreadFindRoot(uf map[int64]int64, start int64) int64 {
	seen := map[int64]bool{}
	var path []int64
	cur := start
	for {
		if seen[cur] {
			// 环：环 = path 里从 cur 首次出现处到末尾的那一段。
			idx := 0
			for i, id := range path {
				if id == cur {
					idx = i
					break
				}
			}
			cycle := path[idx:]
			minID := cycle[0]
			for _, id := range cycle[1:] {
				if id < minID {
					minID = id
				}
			}
			for _, id := range cycle {
				uf[id] = minID
			}
			return minID
		}
		seen[cur] = true
		path = append(path, cur)
		p, ok := uf[cur]
		if !ok || p == cur {
			return cur // 根
		}
		cur = p
		if len(path) >= sessionThreadMaxDepth {
			// 深度截断：在此处永久定根（写回 uf ⇒ 后续解析一致）。
			uf[cur] = cur
			return cur
		}
	}
}

// ---------- 摘要（收件条）----------

// BuildSessionDigest 算出「你不在时发生了什么」。
//
// msgs 是**全量**消息（这样线程根才解析得对）；sinceMessageID 是水位线：只有
// id 严格大于它的消息算「你不在时发生的」（R3/R4 只在这个窗口里生效）。
//
// 线程归属用的是全量列表 ⇒ 一条落在窗口里的回复，如果它的根在窗口之前，它
// 仍然折叠在那个根上（摘要里会指出它续接了一个更早的线程）。
func BuildSessionDigest(sessionID int64, msgs []SessionMessage, sinceMessageID int64) SessionDigest {
	d := SessionDigest{
		SessionID:      sessionID,
		SinceMessageID: sinceMessageID,
		Items:          []SessionDigestItem{},
		Overflow:       map[string]int{},
		Counts: SessionDigestCounts{
			ByKind:  map[string]int{},
			ByAgent: map[string]int{},
		},
	}

	threads := GroupSessionMessages(msgs)

	// 窗口内的消息 id 集合。
	window := map[int64]bool{}
	for _, m := range msgs {
		if m.ID > sinceMessageID {
			window[m.ID] = true
		}
	}
	// 全量 id → kind（判断一条 answer 是不是「回答了请求」）。
	kindByID := make(map[int64]string, len(msgs))
	for _, m := range msgs {
		kindByID[m.ID] = m.Kind
	}

	// 按优先级类收集窗口内的候选。
	type candidate struct {
		msg     SessionMessage
		class   string
		thread  int64
		replies int
	}
	byClass := map[string][]candidate{}

	for _, t := range threads {
		threadHasWindow := false
		windowReplies := 0
		for _, r := range t.Replies {
			if window[r.ID] {
				threadHasWindow = true
				windowReplies++
			}
		}
		if window[t.Root.ID] {
			threadHasWindow = true
		}
		if !threadHasWindow {
			continue
		}

		// 根消息与它的回复分别进候选（回复也是一条「发生了什么」）。
		consider := func(m SessionMessage) {
			if !window[m.ID] {
				return
			}
			d.Counts.Total++
			d.Counts.ByKind[m.Kind]++
			if m.FromAgent == "" {
				d.Counts.HumanAuthored++
			} else {
				d.Counts.ByAgent[m.FromAgent]++
			}
			switch m.Kind {
			case SessionMessageKindEscalation:
				d.Counts.Escalations++
			case SessionMessageKindFinding:
				d.Counts.Findings++
			case SessionMessageKindRequest:
				d.Counts.Requests++
			case SessionMessageKindAnswer:
				d.Counts.Answers++
			case SessionMessageKindNote:
				d.Counts.Notes++
			}
			if m.Status == SessionMessageBlocked {
				d.Counts.Blocked++
			}
			// R3：只统计 agent 说的话 —— 人自己写的消息不进摘要候选。
			if m.FromAgent == "" {
				return
			}
			class := sessionDigestClassOf(m, kindByID)
			byClass[class] = append(byClass[class], candidate{
				msg:     m,
				class:   class,
				thread:  t.Root.ID,
				replies: windowReplies,
			})
		}
		consider(t.Root)
		for _, r := range t.Replies {
			consider(r)
		}

		d.Counts.Threads++
		d.Counts.Replies += windowReplies
	}

	// 原始气泡数：窗口内每条非收据消息一个气泡（没有任何折叠时的样子）。
	rawBubbles := 0
	for _, m := range msgs {
		if m.ID <= sinceMessageID {
			continue
		}
		if m.Kind == SessionMessageKindReceipt {
			d.Counts.Receipts++
			continue
		}
		rawBubbles++
	}
	d.RawBubbles = rawBubbles
	d.ThreadBubbles = d.Counts.Threads

	// R4：按优先级类挑条目，每类有硬上限，总数有硬上限。
	selected := []candidate{}
	for _, class := range sessionDigestPriorityOrder {
		list := byClass[class]
		cap := sessionDigestClassCaps[class]
		kept := list
		if len(list) > cap {
			kept = list[:cap]
			d.Overflow[class] = len(list) - cap
		}
		selected = append(selected, kept...)
	}
	if len(selected) > sessionDigestMaxClassTotal {
		// 超总上限：从**优先级最低**的类里砍（先砍 note，再砍 answer…）。
		// 砍掉的条数回到该类的 overflow 计数里 —— 内容没丢，只是没露出。
		for i := len(selected) - 1; i >= 0 && len(selected) > sessionDigestMaxClassTotal; i-- {
			d.Overflow[selected[i].class]++
			selected = append(selected[:i], selected[i+1:]...)
		}
	}

	for _, c := range selected {
		d.Items = append(d.Items, SessionDigestItem{
			MessageID:  c.msg.ID,
			SessionID:  c.msg.SessionID,
			FromAgent:  c.msg.FromAgent,
			Kind:       c.msg.Kind,
			Class:      c.class,
			Preview:    sessionDigestPreview(c.msg.Body),
			CreatedAt:  sessionDigestTime(c.msg.CreatedAt),
			ThreadID:   c.thread,
			ReplyCount: c.replies,
		})
	}

	d.DigestBubbles = len(d.Items)
	d.BubblesSaved = rawBubbles - d.DigestBubbles
	if d.BubblesSaved < 0 {
		d.BubblesSaved = 0
	}
	if rawBubbles > 0 {
		d.ReductionPct = d.BubblesSaved * 100 / rawBubbles
	}
	for _, m := range msgs {
		if m.ID > d.LatestMessageID {
			d.LatestMessageID = m.ID
		}
	}
	return d
}

// sessionDigestClassOf 判定一条消息在摘要里的优先级类。
//
// answer 单独判：只有**回答了别人的请求**的 answer 才值得露出（那是「有人回应了」
// 的事实）；普通回答归到 note 类（内容上是补充说明）。
func sessionDigestClassOf(m SessionMessage, kindByID map[int64]string) string {
	switch m.Kind {
	case SessionMessageKindEscalation:
		return sessionDigestClassEscalation
	case SessionMessageKindFinding:
		return sessionDigestClassFinding
	case SessionMessageKindRequest:
		return sessionDigestClassRequest
	case SessionMessageKindAnswer:
		if m.ParentMessageID != nil && kindByID[*m.ParentMessageID] == SessionMessageKindRequest {
			return sessionDigestClassAnswer
		}
		return sessionDigestClassNote
	default:
		return sessionDigestClassNote
	}
}

// sessionDigestPreview 截断正文预览（摘要是扫一眼用的，不是读完用的）。
func sessionDigestPreview(body string) string {
	body = strings.TrimSpace(body)
	if body == "" {
		return ""
	}
	// 按 rune 截断（中文按字算，不按字节算）。
	runes := []rune(body)
	if len(runes) <= sessionDigestBodyPreviewMax {
		return body
	}
	return string(runes[:sessionDigestBodyPreviewMax]) + "…"
}

// sessionDigestTime 把时间统一成 RFC3339 字符串（摘要走 JSON，形状固定）。
func sessionDigestTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}

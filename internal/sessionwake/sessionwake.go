// Package sessionwake 是会话群聊的【唤醒裁决层】。
//
// 依据：session-groupchat-proposal.md §4.3（Agent 花名册与热加入）、§9 决策①
// （「@ 定向 ⇒ 必醒；非 @ ⇒ 成员按需自取」的两段式）、§9.1 约束①（唤醒路径与
// 「自取」路径必须分开实现、分开计量）。
//
// 三条被写死的边界：
//
//  1. **纯函数**：不查库、不调模型、不读时钟、不读全局状态 ⇒ 同一份输入永远得到
//     同一份裁决结果。红线「模型只提议，确定性层裁决」在唤醒这件事上的落点就是
//     这个包：谁该被叫醒是**规则**，不是模型的判断。
//  2. **两段式，不对称**：
//     - 定向（@agent 或显式 to_agent）⇒ **必醒**（MustWake）。
//     - 没有定向 ⇒ 任何**空闲**成员都可以**按需自取**（SelfServe）。
//     两者不是同一件事的两种叫法：必醒会**起一个 turn**（花额度），自取只是
//     「允许已经在跑的成员顺手把消息拾起来」（不额外起 turn）。所以自取集合在
//     定向消息上也会给出，但它**不会**因此唤醒任何人。
//  3. **fail-closed**：被投递前拦下的消息（Blocked）不唤醒任何人；收据条
//     （kind='receipt'）不唤醒任何人 —— 否则「收到 / 好的」会互相触发，形成
//     唤醒环路。
//
// 为什么单独一个包：唤醒裁决是**平台级**规则 —— server 的发消息出口（人发消息
// 要叫醒谁）与 agent 侧的发言路径（agent 发消息要叫醒谁）必须用同一份口径。
// 放在 db 或 server 里都会让另一侧复制一份 ⇒ 两份副本就是漂移的开始。本包零
// 依赖（不 import db / agent / server），server 与 agent 都能直接引。
package sessionwake

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ---------- 在场状态与分工（与 db.SessionPresence / db.SessionRole 同款取值）----------

// Presence 是成员的在场状态。
type Presence string

const (
	// PresenceIdle 在席但没在听（迁移回填出来的老会话成员默认是这个）。
	PresenceIdle Presence = "idle"
	// PresenceListening 在席且在听（热加入的成员默认是这个）。
	PresenceListening Presence = "listening"
	// PresenceWorking 正在跑。
	PresenceWorking Presence = "working"
	// PresenceBlocked 卡住了（等别的成员或等人决策）。
	PresenceBlocked Presence = "blocked"
	// PresenceLeft 已退出。
	PresenceLeft Presence = "left"
)

// Role 是成员分工。
//
// ⚠️ 分工**不表达权限**（与 db/session.go 的同款纪律）：role 只说明「这个 agent
// 在会话里干什么」，不是「它能做什么」。
type Role string

const (
	// RoleDispatcher 分发者（auto 在会话内的角色）：派单，不与成员抢活。
	RoleDispatcher Role = "dispatcher"
	// RoleMember 普通成员：既能被定向叫醒，也能按需自取。
	RoleMember Role = "member"
	// RoleObserver 观察者：只看不接活。
	RoleObserver Role = "observer"
)

// KindReceipt 是收据条的消息类型（不唤醒任何人）。
const KindReceipt = "receipt"

// ---------- 输入 ----------

// Member 是名册里的一名成员（裁决层的最小输入）。
//
// 不复用 db.SessionMember：裁决层要能脱离数据库单测 ⇒ 它只认自己这四个字段。
type Member struct {
	// AgentKey 是 agent 的唯一 key。
	AgentKey string
	// Role 是分工；空串按 RoleMember 处理。
	Role Role
	// Presence 是在场状态；空串按 PresenceIdle 处理。
	Presence Presence
	// Left 为真表示已退席（presence='left' 或 left_at 有值）。
	Left bool
}

// Message 是裁决层的消息输入。
type Message struct {
	// Body 是消息正文（@提及从里面解析）。
	Body string
	// FromAgent 是发件人；空串 = 人。
	FromAgent string
	// ToAgent 是显式收件人；空串 = 广播给全会话。
	ToAgent string
	// Kind 是消息类型（note/request/answer/finding/escalation/receipt）。
	Kind string
	// Blocked 为真表示这条消息在**投递前**被拦下（落库留痕，但不进接收方上下文）。
	Blocked bool
}

// ---------- 输出 ----------

// Decision 是唤醒裁决的结果。
type Decision struct {
	// Mentions 是正文里解析出的 @agent key（保序去重，不含 @everyone）。
	Mentions []string
	// Everyone 为真表示正文里出现了 @everyone（广播语义）。
	Everyone bool
	// Designated 为真表示这条消息**指名道姓**了某个 agent（@agent 或 to_agent）——
	// 不管那个 agent 在不在席。它是「这条消息有指定收件人」的事实陈述。
	Designated bool
	// MustWake 是**必醒**集合：被点名、仍在席、且不是发起者本人的成员。
	//
	// 裁决本身不看 presence：@ 是**显式请求**，语义是「你必须回应」；presence
	// 描述的是「当前在做什么」，不是「愿不愿意被叫」。把 presence 当成 @ 的否决
	// 条件，等于让「agent 恰好在忙」吞掉一条指名道姓的请求 —— 而 §9 决策①的
	// 验收口径恰恰是「@ 得到答复」。presence 通过 WakeStates 报给执行方，由执行方
	// 决定「立刻起 / 排队 / 进待决策托盘」。
	MustWake []string
	// SelfServe 是**自取**集合：空闲的普通成员（非发起者、且不在必醒集合里）。
	//
	// 「空闲」= presence ∈ {idle, listening}。working / blocked 的成员在忙，
	// 不该顺手接活。dispatcher 不与成员抢活（§9 约束②）、observer 不接活 ⇒
	// 两者都不在自取集合里。
	SelfServe []string
	// WakeStates 给出每个必醒成员的在场状态（执行方据此决定怎么起 turn）。
	WakeStates map[string]Presence
}

// ---------- @提及解析 ----------

// sessionMentionPattern 匹配 @agentKey 形式的提及。
//
// agent key 的形状与 agents.key 的 CHECK 一致（^[a-z][a-z0-9_]*$）。
var sessionMentionPattern = regexp.MustCompile(`@([a-z][a-z0-9_]*)`)

// MentionEveryone 是广播关键词（§4.3 的 @everyone）。
const MentionEveryone = "everyone"

// Mentions 是正文里的 @提及。
type Mentions struct {
	// Agents 是被点名的 agent key（保序去重）。
	Agents []string
	// Everyone 为真表示出现了 @everyone。
	Everyone bool
}

// ParseMentions 解析正文里的 @提及。
//
// 边界判定：@ 前面不能是字母/数字/下划线/@/斜杠 ⇒ 邮箱（a@b.com）与 URL
// （x/@user）不会被误认成提及。
//
// '@everyone' 是广播关键词，不计入 Agents（但 Everyone=true）—— 广播不是「指定
// 某个人」，它跟没有 @ 一样走自取路径（否则全员自动醒，额度会被烧穿）。
func ParseMentions(body string) Mentions {
	out := Mentions{Agents: []string{}}
	idx := sessionMentionPattern.FindAllStringSubmatchIndex(body, -1)
	seen := map[string]bool{}
	for _, loc := range idx {
		if loc[2] < 0 {
			continue
		}
		key := body[loc[2]:loc[3]]
		if key == MentionEveryone {
			out.Everyone = true
			continue
		}
		if seen[key] {
			continue
		}
		if !mentionBoundaryOK(body, loc[0]) {
			continue
		}
		seen[key] = true
		out.Agents = append(out.Agents, key)
	}
	return out
}

// mentionBoundaryOK 判定 @ 之前是不是合法的提及边界。
func mentionBoundaryOK(body string, at int) bool {
	if at == 0 {
		return true
	}
	r, _ := utf8.DecodeLastRuneInString(body[:at])
	return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' && r != '@' && r != '/'
}

// ---------- 裁决 ----------

// Decide 是唤醒裁决的入口：给定一条消息与名册，算出「谁必须被叫醒」与「谁可以
// 按需自取」。
//
// 裁决顺序（每一步都可复算）：
//
//  1. 被拦下的消息（Blocked）⇒ 谁都不醒（fail-closed：它不进任何人的上下文）。
//  2. 收据条（kind='receipt'）⇒ 谁都不醒（回执是确认噪音，不是内容）。
//  3. 在席名册 = 没退席的成员（presence='left' 或 Left=true 的排除）。
//  4. 必醒集合 = 被点名（正文里的 @agent，或显式 to_agent）∧ 在席 ∧ 不是发起者。
//  5. 自取集合 = 在席 ∧ role=member ∧ presence ∈ {idle, listening} ∧ 不是发起者
//     ∧ 不在必醒集合里。
func Decide(msg Message, members []Member) Decision {
	d := Decision{
		Mentions:   []string{},
		MustWake:   []string{},
		SelfServe:  []string{},
		WakeStates: map[string]Presence{},
	}

	mentions := ParseMentions(msg.Body)
	d.Mentions = mentions.Agents
	d.Everyone = mentions.Everyone
	d.Designated = len(mentions.Agents) > 0 || strings.TrimSpace(msg.ToAgent) != ""

	// 1) fail-closed：被拦下的消息不进任何人的上下文 ⇒ 谁都不醒。
	if msg.Blocked {
		return d
	}
	// 2) 收据条不唤醒任何人（避免回执互相唤醒）。
	if msg.Kind == KindReceipt {
		return d
	}

	// 3) 在席名册（保序 ⇒ 输出顺序确定）。
	inSeat := map[string]Member{}
	order := []string{}
	for _, m := range members {
		key := strings.TrimSpace(m.AgentKey)
		if key == "" {
			continue
		}
		if m.Left || m.Presence == PresenceLeft {
			continue
		}
		if _, dup := inSeat[key]; dup {
			continue
		}
		if m.Role == "" {
			m.Role = RoleMember
		}
		if m.Presence == "" {
			m.Presence = PresenceIdle
		}
		inSeat[key] = m
		order = append(order, key)
	}

	sender := strings.TrimSpace(msg.FromAgent)
	woken := map[string]bool{}

	// 4) 必醒：定向指名道姓的成员。
	wake := func(key string) {
		key = strings.TrimSpace(key)
		if key == "" || key == sender || woken[key] {
			return
		}
		m, ok := inSeat[key]
		if !ok {
			return // 不在席 ⇒ 叫不动（热加入是**执行方**的职责，不是裁决层的）
		}
		woken[key] = true
		d.MustWake = append(d.MustWake, key)
		d.WakeStates[key] = m.Presence
	}
	for _, key := range mentions.Agents {
		wake(key)
	}
	wake(msg.ToAgent)

	// 5) 自取：空闲的普通成员。
	for _, key := range order {
		if key == sender || woken[key] {
			continue
		}
		m := inSeat[key]
		if m.Role == RoleDispatcher || m.Role == RoleObserver {
			continue // dispatcher 不抢活；observer 不接活
		}
		if m.Presence != PresenceIdle && m.Presence != PresenceListening {
			continue // working / blocked：在忙，不顺手接活
		}
		d.SelfServe = append(d.SelfServe, key)
	}

	return d
}

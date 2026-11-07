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
//  2. **定向与广播，不对称**：
//     - 定向（@agent 或显式 to_agent）⇒ **必醒**（MustWake）。
//     - 人的非定向消息（没 @ 也没 to_agent）⇒ **主持补位**（HostWake）：分发者
//     作为群主持人接话 —— 它可以直接回答，也可以在回复里 @ 成员把事情转出去
//     （那一步是定向消息，回到必醒接力）。这条是「人一说话群里就有人接」的
//     落点：兜底人是名册事实（谁是分发者），不是模型的判断。
//     - agent 的非定向发言不触发主持补位（sender≠空）：agent 之间的唤醒只能走
//     定向路径，否则客套互 @ 会形成乒乓循环。
//     - 自取（SelfServe）：任何**空闲**成员都可以**按需自取**。必醒/补位会起
//     一个 turn（花额度），自取只是「允许已经在跑的成员顺手把消息拾起来」
//     （不额外起 turn）。所以自取集合在定向消息上也会给出，但它**不会**因此
//     唤醒任何人。
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
	// HostWake 是**主持补位**集合：人的非定向消息落到分发者（主持人）头上。
	//
	// 只有满足「发件人是人（FromAgent 为空）∧ 消息非定向（没有 @agent 也没有
	// to_agent）」才非空，且至多一名（名册里第一个分发者）。@everyone 属于非
	// 定向 ⇒ 主持补位同样兜底（广播比普通消息更该有人应答）。agent 发件人恒
	// 为空 —— 防的是「A 回复 → 兜底叫醒分发者 → 分发者回复 → 又兜底」的乒乓。
	HostWake []string
	// Undelivered 是**未投递回执**：被点名（@agent / 显式 to_agent）但没有投递
	// 成功的目标 —— 名册里没有这个 key，或它已退席。
	//
	// 必醒集合只对在席的人负责（热加入是执行方的职责，裁决层不管），但「@ 了
	// 却没人接」不能静默消失 —— 转交停摆时最伤体验的正是这种沉默（@ 出去十几
	// 分钟无人应答，人也以为有 agent 在跑）。执行方拿这份对账单给群播报 note
	// （「@xxx 不在席/不存在，消息无人接手」），让每一次转交都有回声。
	Undelivered []string
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
//  4. 必醒集合 = 被点名（正文里的 @agent，或显式 to_agent）∧ 在席 ∧ 不是发起者；
//     被点名但不在席的进**未投递回执**（Undelivered）—— 不唤醒，但也不允许
//     「没送到」这件事静默消失，由执行方据此播报。
//  5. 自取集合 = 在席 ∧ role=member ∧ presence ∈ {idle, listening} ∧ 不是发起者
//     ∧ 不在必醒集合里。
//  6. 主持补位 = 发件人是人 ∧ 非定向 ∧ 在席名册里有分发者 ⇒ 取第一个分发者。
func Decide(msg Message, members []Member) Decision {
	d := Decision{
		Mentions:    []string{},
		MustWake:    []string{},
		SelfServe:   []string{},
		HostWake:    []string{},
		Undelivered: []string{},
		WakeStates:  map[string]Presence{},
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
	undelivered := map[string]bool{}

	// 4) 必醒：定向指名道姓的成员。
	wake := func(key string) {
		key = strings.TrimSpace(key)
		if key == "" || key == sender || woken[key] {
			return
		}
		m, ok := inSeat[key]
		if !ok {
			// 不在席 ⇒ 叫不动（热加入是**执行方**的职责，不是裁决层的）。
			// 但「没送到」必须记账成回执 —— 静默丢弃就是转交停摆的伤点，
			// 执行方拿 Undelivered 给群播报「无人接手」的 note。
			if !undelivered[key] {
				undelivered[key] = true
				d.Undelivered = append(d.Undelivered, key)
			}
			return
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

	// 6) 主持补位：人的非定向消息由分发者接话。非定向 ⇒ 必醒集合必为空
	//    （没有 @ 也没有 to_agent），不存在与 MustWake 重复的可能；
	//    sender != 空（agent 发言）直接跳过 —— agent 之间只走定向路径。
	if sender == "" && !d.Designated {
		for _, key := range order {
			if inSeat[key].Role == RoleDispatcher {
				d.HostWake = append(d.HostWake, key)
				d.WakeStates[key] = inSeat[key].Presence
				break
			}
		}
	}

	return d
}

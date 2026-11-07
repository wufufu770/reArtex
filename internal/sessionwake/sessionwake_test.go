package sessionwake

// 唤醒裁决的验收用例。
//
// 这些用例钉住的是**承诺**（proposal §9 决策①的两段式），不是实现细节：
//   - @ 定向 ⇒ 必醒（且只有被点名的人必醒）；
//   - 非 @ ⇒ 必醒集合为空，任何**空闲**成员按需自取；
//   - @everyone 是广播，不是「指定某个人」⇒ 不触发必醒；
//   - 分工不表达权限，但 dispatcher 不与成员抢活、observer 不接活；
//   - 退席的人谁都不当；
//   - 被拦下的消息与收据条不唤醒任何人（fail-closed）；
//   - 被点名但不在席的目标进未投递回执（Undelivered）——「没送到」必须记账，
//     转交不允许静默消失（B4）；
//   - 同一个输入永远得到同一个输出（确定性）。

import (
	"reflect"
	"testing"
)

func member(key string, role Role, presence Presence) Member {
	return Member{AgentKey: key, Role: role, Presence: presence}
}

// roster 是最常见的名册：一个分发者（auto）+ 两个普通成员。
func roster() []Member {
	return []Member{
		member("auto", RoleDispatcher, PresenceListening),
		member("pentest", RoleMember, PresenceListening),
		member("reporter", RoleMember, PresenceIdle),
	}
}

func TestDecideMentionMustWakeOnlyMentioned(t *testing.T) {
	d := Decide(Message{Body: "@pentest 看一下这个"}, roster())
	if !reflect.DeepEqual(d.MustWake, []string{"pentest"}) {
		t.Fatalf("必醒集合不对: %v", d.MustWake)
	}
	if !d.Designated {
		t.Fatalf("带 @ 的消息应当是定向的")
	}
	// 自取集合仍然给出（共享流里空闲成员可以搭话），但不含被点名的人。
	if reflect.DeepEqual(d.SelfServe, []string{"pentest"}) {
		t.Fatalf("自取集合不应包含已在必醒集合里的成员: %v", d.SelfServe)
	}
}

func TestDecideMentionKeepsMentionOrder(t *testing.T) {
	d := Decide(Message{Body: "@reporter 先看 @pentest 再看"}, roster())
	want := []string{"reporter", "pentest"}
	if !reflect.DeepEqual(d.MustWake, want) {
		t.Fatalf("必醒集合顺序不对: %v（想要 %v）", d.MustWake, want)
	}
}

func TestDecideMentionOfNonMemberIsNotWoken(t *testing.T) {
	// 热加入是**执行方**的职责：裁决层只对「仍在席」的人负责。
	d := Decide(Message{Body: "@ghost 在吗"}, roster())
	if len(d.MustWake) != 0 {
		t.Fatalf("不在席的成员不应被叫醒: %v", d.MustWake)
	}
	// 但「这条消息指名道姓了」这个事实仍然成立。
	if !d.Designated {
		t.Fatalf("@ 了不在席的 agent 仍然是定向消息")
	}
}

func TestDecideNoMentionIsSelfServeOnly(t *testing.T) {
	d := Decide(Message{Body: "大家看看这个"}, roster())
	if len(d.MustWake) != 0 {
		t.Fatalf("没有 @ 时不应有必醒集合: %v", d.MustWake)
	}
	if d.Designated {
		t.Fatalf("没有 @ 时不应是定向消息")
	}
	// auto 是 dispatcher（不与成员抢活）⇒ 不在自取集合里。
	want := []string{"pentest", "reporter"}
	if !reflect.DeepEqual(d.SelfServe, want) {
		t.Fatalf("自取集合不对: %v（想要 %v）", d.SelfServe, want)
	}
}

func TestDecideEveryoneIsBroadcastNotDesignation(t *testing.T) {
	d := Decide(Message{Body: "@everyone 同步一下进度"}, roster())
	if len(d.MustWake) != 0 {
		t.Fatalf("@everyone 是广播，不应触发必醒: %v", d.MustWake)
	}
	if !d.Everyone {
		t.Fatalf("@everyone 应当被识别出来")
	}
	if d.Designated {
		t.Fatalf("@everyone 不算「指定某个人」")
	}
	if len(d.SelfServe) == 0 {
		t.Fatalf("@everyone 应当走自取路径")
	}
}

func TestDecideExplicitToAgentMustWakes(t *testing.T) {
	d := Decide(Message{Body: "帮我看看", ToAgent: "reporter"}, roster())
	if !reflect.DeepEqual(d.MustWake, []string{"reporter"}) {
		t.Fatalf("显式 to_agent 应当必醒: %v", d.MustWake)
	}
}

func TestDecideBusyAndBlockedMembersDoNotSelfServe(t *testing.T) {
	members := []Member{
		member("busy", RoleMember, PresenceWorking),
		member("stuck", RoleMember, PresenceBlocked),
		member("free", RoleMember, PresenceListening),
	}
	d := Decide(Message{Body: "谁有空"}, members)
	if !reflect.DeepEqual(d.SelfServe, []string{"free"}) {
		t.Fatalf("自取集合只应包含空闲成员: %v", d.SelfServe)
	}
}

func TestDecideBusyMentionedMemberStillMustWakes(t *testing.T) {
	// @ 是显式请求：「恰好在忙」不能吞掉它。presence 通过 WakeStates 报给执行方。
	members := []Member{member("busy", RoleMember, PresenceWorking)}
	d := Decide(Message{Body: "@busy 收一下"}, members)
	if !reflect.DeepEqual(d.MustWake, []string{"busy"}) {
		t.Fatalf("被点名的成员必须必醒: %v", d.MustWake)
	}
	if d.WakeStates["busy"] != PresenceWorking {
		t.Fatalf("必醒成员的在场状态应当被报出来: %v", d.WakeStates)
	}
}

func TestDecideLeftMembersAreIgnored(t *testing.T) {
	members := []Member{
		{AgentKey: "gone", Role: RoleMember, Presence: PresenceListening, Left: true},
		{AgentKey: "also", Role: RoleMember, Presence: PresenceLeft},
		member("here", RoleMember, PresenceListening),
	}
	d := Decide(Message{Body: "@gone @also 有人吗"}, members)
	if len(d.MustWake) != 0 {
		t.Fatalf("退席的成员不应被叫醒: %v", d.MustWake)
	}
	if !reflect.DeepEqual(d.SelfServe, []string{"here"}) {
		t.Fatalf("自取集合不应包含退席成员: %v", d.SelfServe)
	}
}

func TestDecideSenderNeverWoken(t *testing.T) {
	members := []Member{
		member("self", RoleMember, PresenceListening),
		member("other", RoleMember, PresenceListening),
	}
	d := Decide(Message{Body: "@self @other 说话", FromAgent: "self"}, members)
	if !reflect.DeepEqual(d.MustWake, []string{"other"}) {
		t.Fatalf("发起者本人不应在必醒集合里: %v", d.MustWake)
	}
	for _, key := range d.SelfServe {
		if key == "self" {
			t.Fatalf("发起者本人不应在自取集合里: %v", d.SelfServe)
		}
	}
}

func TestDecideBlockedMessageWakesNobody(t *testing.T) {
	d := Decide(Message{Body: "@pentest 看一下", Blocked: true}, roster())
	if len(d.MustWake) != 0 || len(d.SelfServe) != 0 {
		t.Fatalf("被拦下的消息不唤醒任何人: must=%v self=%v", d.MustWake, d.SelfServe)
	}
}

func TestDecideReceiptWakesNobody(t *testing.T) {
	d := Decide(Message{Body: "收到", Kind: KindReceipt}, roster())
	if len(d.MustWake) != 0 || len(d.SelfServe) != 0 {
		t.Fatalf("收据条不唤醒任何人: must=%v self=%v", d.MustWake, d.SelfServe)
	}
}

func TestDecideObserverAndDispatcherDoNotSelfServe(t *testing.T) {
	members := []Member{
		member("auto", RoleDispatcher, PresenceListening),
		member("watcher", RoleObserver, PresenceListening),
		member("worker", RoleMember, PresenceListening),
	}
	d := Decide(Message{Body: "有人处理吗"}, members)
	if !reflect.DeepEqual(d.SelfServe, []string{"worker"}) {
		t.Fatalf("自取集合不应含 dispatcher / observer: %v", d.SelfServe)
	}
}

func TestDecideIsDeterministic(t *testing.T) {
	msg := Message{Body: "@reporter @pentest 看下", FromAgent: "auto"}
	members := roster()
	first := Decide(msg, members)
	for i := 0; i < 20; i++ {
		got := Decide(msg, members)
		if !reflect.DeepEqual(got, first) {
			t.Fatalf("裁决不可复算: %+v != %+v", got, first)
		}
	}
}

func TestDecideEmptyRosterWakesNobody(t *testing.T) {
	d := Decide(Message{Body: "@pentest 在吗"}, nil)
	if len(d.MustWake) != 0 || len(d.SelfServe) != 0 {
		t.Fatalf("空名册不该唤醒任何人: %+v", d)
	}
	// 空名册 = 没人可投递：点名目标全部进未投递回执。
	if !reflect.DeepEqual(d.Undelivered, []string{"pentest"}) {
		t.Fatalf("空名册的点名应记入未投递回执: %v", d.Undelivered)
	}
}

// TestDecideUndeliveredReceipt 未投递回执（B4）：被点名但不在席（名册里没有 /
// 已退席 / 显式 to_agent 指了不存在的人）的目标进 Undelivered —— 不唤醒，但
// 「没送到」必须记账，执行方据此给群播报「无人接手」的 note。
func TestDecideUndeliveredReceipt(t *testing.T) {
	// 名册里没有的 key：不叫醒，但回执要记上。
	d := Decide(Message{Body: "@ghost 在吗"}, roster())
	if len(d.MustWake) != 0 {
		t.Fatalf("不在席的成员不应被叫醒: %v", d.MustWake)
	}
	if !reflect.DeepEqual(d.Undelivered, []string{"ghost"}) {
		t.Fatalf("未投递回执不对: %v（想要 [ghost]）", d.Undelivered)
	}
	// 退席成员 / 不存在的 key / 显式收件人三类都算未投递，保序。
	members := []Member{
		{AgentKey: "gone", Role: RoleMember, Presence: PresenceListening, Left: true},
		member("here", RoleMember, PresenceListening),
	}
	d = Decide(Message{Body: "@gone @ghost 收一下", ToAgent: "missing"}, members)
	if !reflect.DeepEqual(d.Undelivered, []string{"gone", "ghost", "missing"}) {
		t.Fatalf("未投递回执应覆盖退席/不存在/显式收件人: %v", d.Undelivered)
	}
	// 同一个 key 被正文 @ 与 to_agent 双重点名只记一次（去重）。
	d = Decide(Message{Body: "@ghost 看下", ToAgent: "ghost"}, roster())
	if !reflect.DeepEqual(d.Undelivered, []string{"ghost"}) {
		t.Fatalf("未投递回执应去重: %v", d.Undelivered)
	}
	// 发起者 @ 自己不算未投递（说话人本来就是自己）。
	d = Decide(Message{Body: "@self @ghost 说话", FromAgent: "self"}, members)
	if !reflect.DeepEqual(d.Undelivered, []string{"ghost"}) {
		t.Fatalf("发起者本人不应进未投递回执: %v", d.Undelivered)
	}
	// 在席的点名不进回执（它在必醒集合里）。
	d = Decide(Message{Body: "@pentest 看一下"}, roster())
	if len(d.Undelivered) != 0 {
		t.Fatalf("在席的点名不是未投递: %v", d.Undelivered)
	}
}

// TestDecideUndeliveredFailClosed 被拦下的消息根本没有进入投递 ⇒
// 既不唤醒也没有未投递回执（fail-closed 的完整口径）。
func TestDecideUndeliveredFailClosed(t *testing.T) {
	d := Decide(Message{Body: "@ghost 看下", Blocked: true}, roster())
	if len(d.MustWake) != 0 || len(d.Undelivered) != 0 {
		t.Fatalf("被拦下的消息没有投递，也就没有未投递回执: must=%v undelivered=%v",
			d.MustWake, d.Undelivered)
	}
	d = Decide(Message{Body: "@ghost 看下", Kind: KindReceipt}, roster())
	if len(d.MustWake) != 0 || len(d.Undelivered) != 0 {
		t.Fatalf("收据条没有投递，也就没有未投递回执: must=%v undelivered=%v",
			d.MustWake, d.Undelivered)
	}
}

// TestDecideHostWakePinsDispatcher 主持补位：人的非定向消息落到分发者头上 ——
// 「人一说话群里就有人接」的裁决口径（执行方用主持提示词起 turn）。
func TestDecideHostWakePinsDispatcher(t *testing.T) {
	d := Decide(Message{Body: "大家好，今天我们测 192.168.1.1"}, roster())
	if !reflect.DeepEqual(d.HostWake, []string{"auto"}) {
		t.Fatalf("人的非定向消息应由分发者补位: %v", d.HostWake)
	}
	if len(d.MustWake) != 0 {
		t.Fatalf("主持补位不应产生必醒集合: %v", d.MustWake)
	}
	// @everyone 是广播（非定向）⇒ 同样由分发者兜底。
	d = Decide(Message{Body: "@everyone 同步一下"}, roster())
	if !reflect.DeepEqual(d.HostWake, []string{"auto"}) {
		t.Fatalf("@everyone 应由分发者补位: %v", d.HostWake)
	}
}

// TestDecideHostWakeDirectedSuppressesHost 定向消息不再补位：@ 是显式请求，
// 分发者不该再被拖进来抢这一轮。
func TestDecideHostWakeDirectedSuppressesHost(t *testing.T) {
	d := Decide(Message{Body: "@pentest 看一下这个"}, roster())
	if len(d.HostWake) != 0 {
		t.Fatalf("定向消息不应触发主持补位: %v", d.HostWake)
	}
	d = Decide(Message{Body: "帮我看看", ToAgent: "reporter"}, roster())
	if len(d.HostWake) != 0 {
		t.Fatalf("显式 to_agent 不应触发主持补位: %v", d.HostWake)
	}
}

// TestDecideHostWakeAgentSenderNeverHosts agent 的非定向发言不补位 —— 防的是
// 「A 回复 → 兜底叫醒分发者 → 分发者回复 → 又兜底」的乒乓。
func TestDecideHostWakeAgentSenderNeverHosts(t *testing.T) {
	d := Decide(Message{Body: "我这边侦察完了，一切正常", FromAgent: "pentest"}, roster())
	if len(d.HostWake) != 0 {
		t.Fatalf("agent 发言不应触发主持补位: %v", d.HostWake)
	}
	if len(d.MustWake) != 0 {
		t.Fatalf("agent 的非定向发言不该必醒任何人: %v", d.MustWake)
	}
}

// TestDecideHostWakeNeedsDispatcher 名册里没有分发者 ⇒ 补位落空（谁都不叫，
// 与旧行为一致）；被拦下的消息/收据条同样不补位。
func TestDecideHostWakeNeedsDispatcher(t *testing.T) {
	members := []Member{member("pentest", RoleMember, PresenceListening)}
	d := Decide(Message{Body: "有人在吗"}, members)
	if len(d.HostWake) != 0 {
		t.Fatalf("没有分发者不应补位: %v", d.HostWake)
	}
	d = Decide(Message{Body: "有人在吗", Blocked: true}, roster())
	if len(d.HostWake) != 0 {
		t.Fatalf("被拦下的消息不应补位: %v", d.HostWake)
	}
	d = Decide(Message{Body: "收到", Kind: KindReceipt}, roster())
	if len(d.HostWake) != 0 {
		t.Fatalf("收据条不应补位: %v", d.HostWake)
	}
}

func TestParseMentions(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		agents   []string
		everyone bool
	}{
		{"普通提及", "@pentest 看下", []string{"pentest"}, false},
		{"重复提及只留一次", "@pentest @pentest 看下", []string{"pentest"}, false},
		{"保序", "@b @a @c", []string{"b", "a", "c"}, false},
		{"everyone 不算 agent", "@everyone 看下", []string{}, true},
		{"everyone 与具体 agent 并存", "@everyone @pentest", []string{"pentest"}, true},
		{"邮箱不是提及", "联系 a@b.com", []string{}, false},
		{"URL 不是提及", "看 https://x.com/@user", []string{}, false},
		{"下划线开头不是 agent key", "@_x 看下", []string{}, false},
		{"大写不是 agent key", "@Pentest 看下", []string{}, false},
		{"句中提及", "麻烦 @pentest 看看", []string{"pentest"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseMentions(tc.body)
			if !reflect.DeepEqual(got.Agents, tc.agents) {
				t.Fatalf("提及解析不对: %v（想要 %v）", got.Agents, tc.agents)
			}
			if got.Everyone != tc.everyone {
				t.Fatalf("everyone 判定不对: %v（想要 %v）", got.Everyone, tc.everyone)
			}
		})
	}
}

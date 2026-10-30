package db

import (
	"strings"
	"testing"
)

// chatGroupFixture 是群聊用例的公共脚手架：一个 task、一个大厅会话、一个群、
// 以及若干成员（每个成员一个独立会话）。
type chatGroupFixture struct {
	d       *DB
	group   *ChatGroup
	taskID  int64
	expID   int64
	members []ChatGroupMember
}

// newChatGroupFixture 建一个群：task + 大厅会话 + 群组 + 成员会话。
//
// 为什么成员必须各自有会话：`conversations.agent_key` 是单值列，一个会话只能跑
// 一个 agent ⇒ 群聊落地为「1 大厅会话 + N 成员会话」，每个成员会话独立 transcript、
// 共享同一个 task（也就是同一张探索图）。
func newChatGroupFixture(t *testing.T, d *DB, memberKeys ...string) chatGroupFixture {
	t.Helper()
	task, err := d.CreateTask("群聊测试任务", "拿下目标", nil, 0, 0)
	if err != nil {
		t.Fatalf("建任务: %v", err)
	}
	// 用例结束时删掉任务：本包所有用例共用一个派生库，留下的任务会占住并发槽位。
	t.Cleanup(func() { _ = d.DeleteTask(task.ID) })
	hall, err := d.CreateConversation("auto", "大厅", nil)
	if err != nil {
		t.Fatalf("建大厅会话: %v", err)
	}
	gid, err := d.CreateChatGroup(&ChatGroup{
		ConversationID:   hall.ID,
		TaskID:           task.ID,
		Name:             "测试群",
		Status:           "active",
		ConcurrencyLimit: len(memberKeys),
	})
	if err != nil {
		t.Fatalf("建群: %v", err)
	}
	group, err := d.GetChatGroup(gid)
	if err != nil || group == nil {
		t.Fatalf("读群: %v", err)
	}
	f := chatGroupFixture{d: d, group: group, taskID: task.ID, expID: task.ExplorationID}
	for _, key := range memberKeys {
		conv, err := d.CreateConversation(key, "成员/"+key, nil)
		if err != nil {
			t.Fatalf("建成员会话(%s): %v", key, err)
		}
		if err := d.AddChatGroupMember(gid, key, &conv.ID, "member"); err != nil {
			t.Fatalf("加成员(%s): %v", key, err)
		}
		f.members = append(f.members, ChatGroupMember{GroupID: gid, AgentKey: key, ConversationID: &conv.ID, Role: "member"})
	}
	return f
}

// TestChatGroupMemberWriteAndReadMessages：成员能往群里写消息，也能读到自己该读的那些。
func TestChatGroupMemberWriteAndReadMessages(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()

	f := newChatGroupFixture(t, d, "pentest", "worker")

	// 定向给 pentest
	if _, err := d.AddChatGroupMessage(&ChatGroupMessage{
		GroupID: f.group.ID, FromAgent: "auto", ToAgent: "pentest",
		Kind: "note", Body: "先打 /admin",
	}); err != nil {
		t.Fatalf("写定向消息: %v", err)
	}
	// 广播给全群
	if _, err := d.AddChatGroupMessage(&ChatGroupMessage{
		GroupID: f.group.ID, FromAgent: "worker", ToAgent: "",
		Kind: "note", Body: "我在打 upload",
	}); err != nil {
		t.Fatalf("写广播消息: %v", err)
	}
	// 给别人的定向消息 → 不应出现在 pentest 的未读里
	if _, err := d.AddChatGroupMessage(&ChatGroupMessage{
		GroupID: f.group.ID, FromAgent: "auto", ToAgent: "worker",
		Kind: "note", Body: "只给 worker",
	}); err != nil {
		t.Fatalf("写他人定向消息: %v", err)
	}

	msgs, err := d.UnreadChatGroupMessages(f.group.ID, "pentest", 50)
	if err != nil {
		t.Fatalf("收取未读: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("pentest 未读 = %d 条, want 2（定向 + 广播）", len(msgs))
	}

	ids := make([]int64, 0, len(msgs))
	for _, m := range msgs {
		ids = append(ids, m.ID)
	}
	if err := d.MarkChatGroupMessagesRead(ids); err != nil {
		t.Fatalf("标记已读: %v", err)
	}
	msgs, err = d.UnreadChatGroupMessages(f.group.ID, "pentest", 50)
	if err != nil {
		t.Fatalf("标记后收取: %v", err)
	}
	if len(msgs) != 0 {
		t.Fatalf("标记后未读 = %d 条, want 0", len(msgs))
	}
}

// TestChatGroupMemberSessionsShareTaskWithoutClobbering：两个成员的会话各跑各的，
// 但共享同一个 task；一个成员会话的信息不会被另一个覆盖。
func TestChatGroupMemberSessionsShareTaskWithoutClobbering(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()

	f := newChatGroupFixture(t, d, "pentest", "worker")
	if len(f.members) != 2 {
		t.Fatalf("成员数 = %d, want 2", len(f.members))
	}

	convs, err := d.ChatGroupMemberConversations(f.group.ID)
	if err != nil {
		t.Fatalf("读成员会话: %v", err)
	}
	if len(convs) != 2 {
		t.Fatalf("成员会话数 = %d, want 2", len(convs))
	}
	if convs["pentest"] == convs["worker"] {
		t.Fatalf("两个成员不该共用一个会话: %+v", convs)
	}
	if convs["pentest"] == 0 || convs["worker"] == 0 {
		t.Fatalf("成员会话 id 缺失: %+v", convs)
	}

	// 两个成员会话都反查到同一个群、同一个 task（共享探索图 ⇒ 用量记在同一张 task 上）。
	for _, key := range []string{"pentest", "worker"} {
		g, err := d.GroupByConversation(convs[key])
		if err != nil {
			t.Fatalf("按成员会话反查群(%s): %v", key, err)
		}
		if g == nil {
			t.Fatalf("成员会话 %s 反查不到群", key)
		}
		if g.ID != f.group.ID || g.TaskID != f.taskID {
			t.Fatalf("成员会话 %s 反查到的群不对: %+v", key, g)
		}
		if g.TaskExpID != f.expID {
			t.Fatalf("群 %s 的 exploration_id = %d, want %d", key, g.TaskExpID, f.expID)
		}
	}

	// 按 task 反查群：唯一。
	g, err := d.GroupByTask(f.taskID)
	if err != nil {
		t.Fatalf("按 task 反查群: %v", err)
	}
	if g == nil || g.ID != f.group.ID {
		t.Fatalf("按 task 反查群 = %+v, want id=%d", g, f.group.ID)
	}
}

// TestChatGroupBoundToExactlyOneTask：群只绑一个 task；跨 task 的消息一律被拦。
func TestChatGroupBoundToExactlyOneTask(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()

	f := newChatGroupFixture(t, d, "pentest")

	other, err := d.CreateTask("另一个任务", "别的目标", nil, 0, 0)
	if err != nil {
		t.Fatalf("建另一个任务: %v", err)
	}

	// 携带别的 task 的消息 → 拦截（不进接收方上下文）。
	msg := &ChatGroupMessage{
		GroupID: f.group.ID, FromAgent: "pentest", ToAgent: "",
		Kind: "note", Body: "串到别的任务去", TaskID: &other.ID,
	}
	if _, err := d.AddChatGroupMessage(msg); err != nil {
		t.Fatalf("写跨任务消息: %v", err)
	}
	if msg.Status != ChatGroupMessageBlocked {
		t.Fatalf("跨任务消息 status = %q, want %q", msg.Status, ChatGroupMessageBlocked)
	}
	if msg.BlockedReason == "" {
		t.Fatal("跨任务消息必须写明拦截原因")
	}
	msgs, err := d.UnreadChatGroupMessages(f.group.ID, "pentest", 50)
	if err != nil {
		t.Fatalf("收取: %v", err)
	}
	if len(msgs) != 0 {
		t.Fatalf("被拦的消息不得进入收取结果, got %d 条", len(msgs))
	}

	// 另一个意图节点（属于别的任务的探索图）→ 拦截。
	otherExp := d.Exploration(other.ExplorationID)
	foreignIntent, err := otherExp.AddIntent(map[string]any{"summary": "别的任务的意图"}, 5, nil, "planner")
	if err != nil {
		t.Fatalf("建外部意图: %v", err)
	}
	msg2 := &ChatGroupMessage{
		GroupID: f.group.ID, FromAgent: "pentest", ToAgent: "pentest",
		Kind: "request", Body: "这个意图给我", IntentID: &foreignIntent,
	}
	if _, err := d.AddChatGroupMessage(msg2); err != nil {
		t.Fatalf("写外部意图消息: %v", err)
	}
	if msg2.Status != ChatGroupMessageBlocked {
		t.Fatalf("外部意图消息 status = %q, want %q", msg2.Status, ChatGroupMessageBlocked)
	}

	// 本群 task 的意图节点 → 正常投递（携带 intent_id 只是提议，不改变归属）。
	ownIntent, err := d.Exploration(f.expID).AddIntent(map[string]any{"summary": "本任务的意图"}, 5, nil, "planner")
	if err != nil {
		t.Fatalf("建本群意图: %v", err)
	}
	msg3 := &ChatGroupMessage{
		GroupID: f.group.ID, FromAgent: "pentest", ToAgent: "pentest",
		Kind: "request", Body: "我提议做这个", IntentID: &ownIntent,
	}
	if _, err := d.AddChatGroupMessage(msg3); err != nil {
		t.Fatalf("写本群意图消息: %v", err)
	}
	if msg3.Status != ChatGroupMessageUnread {
		t.Fatalf("本群意图消息 status = %q, want %q", msg3.Status, ChatGroupMessageUnread)
	}
	// 归属权威唯一 = exploration_nodes.owner：写消息不碰 owner 列。
	var owner *string
	if err := d.QueryRow(`SELECT owner FROM exploration_nodes WHERE id=$1`, ownIntent).Scan(&owner); err != nil {
		t.Fatalf("读 owner: %v", err)
	}
	if owner != nil && *owner != "" {
		t.Fatalf("写群消息不得改变意图归属, owner=%q", *owner)
	}
}

// TestChatGroupBlockedMessageExcludedFromCollection：被拦的消息标成 blocked 且不进入收取。
func TestChatGroupBlockedMessageExcludedFromCollection(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()

	f := newChatGroupFixture(t, d, "pentest")

	// 收件人不是本群成员 → 拦截。
	msg := &ChatGroupMessage{
		GroupID: f.group.ID, FromAgent: "pentest", ToAgent: "stranger",
		Kind: "note", Body: "发给局外人",
	}
	if _, err := d.AddChatGroupMessage(msg); err != nil {
		t.Fatalf("写消息: %v", err)
	}
	if msg.Status != ChatGroupMessageBlocked {
		t.Fatalf("非成员收件 status = %q, want %q", msg.Status, ChatGroupMessageBlocked)
	}
	if !strings.Contains(msg.BlockedReason, "成员") {
		t.Fatalf("拦截原因应说明收件人不是成员, got %q", msg.BlockedReason)
	}
	msgs, err := d.UnreadChatGroupMessages(f.group.ID, "pentest", 50)
	if err != nil {
		t.Fatalf("收取: %v", err)
	}
	if len(msgs) != 0 {
		t.Fatalf("被拦消息不得进入收取, got %d 条", len(msgs))
	}

	// 落库行本身带 blocked 标记（留痕），但收取不到。
	var status, reason string
	if err := d.QueryRow(`SELECT status, blocked_reason FROM chat_group_messages WHERE id=$1`, msg.ID).Scan(&status, &reason); err != nil {
		t.Fatalf("读消息行: %v", err)
	}
	if status != ChatGroupMessageBlocked {
		t.Fatalf("落库 status = %q, want %q", status, ChatGroupMessageBlocked)
	}
	if reason == "" {
		t.Fatal("落库必须留拦截原因")
	}
}

// TestChatGroupDisabledToolsIsDenyList：群内禁列表可读，且是"禁"不是"启"。
func TestChatGroupDisabledToolsIsDenyList(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()

	f := newChatGroupFixture(t, d, "pentest")

	disabled, err := d.GroupDisabledTools(f.group.ID)
	if err != nil {
		t.Fatalf("读禁列表: %v", err)
	}
	if len(disabled) != 0 {
		t.Fatalf("初始禁列表应为空, got %+v", disabled)
	}

	if err := d.DisableChatGroupTool(f.group.ID, "list_assets"); err != nil {
		t.Fatalf("登记禁用: %v", err)
	}
	disabled, err = d.GroupDisabledTools(f.group.ID)
	if err != nil {
		t.Fatalf("读禁列表: %v", err)
	}
	if !disabled["list_assets"] {
		t.Fatalf("禁列表应含 list_assets, got %+v", disabled)
	}
	// 值恒为 true —— 这是"禁"语义：出现即禁用。
	for k, v := range disabled {
		if !v {
			t.Fatalf("禁列表 %q 的值应为 true, got %v", k, v)
		}
	}

	// 撤销禁用
	if err := d.EnableChatGroupTool(f.group.ID, "list_assets"); err != nil {
		t.Fatalf("撤销禁用: %v", err)
	}
	disabled, err = d.GroupDisabledTools(f.group.ID)
	if err != nil {
		t.Fatalf("读禁列表: %v", err)
	}
	if len(disabled) != 0 {
		t.Fatalf("撤销后禁列表应为空, got %+v", disabled)
	}

	// schema 层面：禁列表表没有"启用"列 ⇒ 结构上无法表达"放开"。
	rows, err := d.Query(`SELECT column_name FROM information_schema.columns
WHERE table_name='chat_group_disabled_tools' AND table_schema=current_schema()`)
	if err != nil {
		t.Fatalf("读列信息: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var col string
		if err := rows.Scan(&col); err != nil {
			t.Fatal(err)
		}
		if col == "enabled" {
			t.Fatal("禁列表不得有 enabled 列（结构上必须无法表达放开）")
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}

// TestChatGroupMessageWithUnknownTaskIsBlockedNotError：引用一个不存在的 task 的消息
// 也走"拦截"而不是"写库报错" —— 拦截不能变成 500，被拦的消息也要留痕。
func TestChatGroupMessageWithUnknownTaskIsBlockedNotError(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()

	f := newChatGroupFixture(t, d, "pentest")
	ghost := int64(1 << 40)
	msg := &ChatGroupMessage{
		GroupID: f.group.ID, FromAgent: "pentest", ToAgent: "",
		Kind: "note", Body: "指向不存在的任务", TaskID: &ghost,
	}
	if _, err := d.AddChatGroupMessage(msg); err != nil {
		t.Fatalf("写消息不应报错: %v", err)
	}
	if msg.Status != ChatGroupMessageBlocked {
		t.Fatalf("status = %q, want %q", msg.Status, ChatGroupMessageBlocked)
	}
	if msg.TaskID != nil {
		t.Fatalf("不存在的 task id 不应写进外键列, got %v", *msg.TaskID)
	}
	var status string
	if err := d.QueryRow(`SELECT status FROM chat_group_messages WHERE id=$1`, msg.ID).Scan(&status); err != nil {
		t.Fatalf("读消息行: %v", err)
	}
	if status != ChatGroupMessageBlocked {
		t.Fatalf("落库 status = %q, want %q", status, ChatGroupMessageBlocked)
	}
}

// TestChatGroupOneHallPerTask：一个 task 至多一个大厅 —— schema 层就拒绝第二个。
func TestChatGroupOneHallPerTask(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()

	f := newChatGroupFixture(t, d, "pentest")
	hall2, err := d.CreateConversation("auto", "第二个大厅", nil)
	if err != nil {
		t.Fatalf("建第二个大厅会话: %v", err)
	}
	if _, err := d.CreateChatGroup(&ChatGroup{
		ConversationID: hall2.ID, TaskID: f.taskID, Name: "第二个群", Status: "active", ConcurrencyLimit: 1,
	}); err == nil {
		t.Fatal("同一个 task 不该能建第二个大厅")
	}
}

// TestChatGroupDeleteCascades：删群时成员、禁列表、消息箱一并清掉。
func TestChatGroupDeleteCascades(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()

	f := newChatGroupFixture(t, d, "pentest")
	if err := d.DisableChatGroupTool(f.group.ID, "list_assets"); err != nil {
		t.Fatalf("登记禁用: %v", err)
	}
	if _, err := d.AddChatGroupMessage(&ChatGroupMessage{
		GroupID: f.group.ID, FromAgent: "pentest", ToAgent: "pentest", Kind: "note", Body: "x",
	}); err != nil {
		t.Fatalf("写消息: %v", err)
	}
	if err := d.DeleteChatGroup(f.group.ID); err != nil {
		t.Fatalf("删群: %v", err)
	}
	var members, tools, messages int
	if err := d.QueryRow(`SELECT count(*) FROM chat_group_members WHERE group_id=$1`, f.group.ID).Scan(&members); err != nil {
		t.Fatal(err)
	}
	if err := d.QueryRow(`SELECT count(*) FROM chat_group_disabled_tools WHERE group_id=$1`, f.group.ID).Scan(&tools); err != nil {
		t.Fatal(err)
	}
	if err := d.QueryRow(`SELECT count(*) FROM chat_group_messages WHERE group_id=$1`, f.group.ID).Scan(&messages); err != nil {
		t.Fatal(err)
	}
	if members != 0 || tools != 0 || messages != 0 {
		t.Fatalf("删群后应级联清空, members=%d tools=%d messages=%d", members, tools, messages)
	}
}

// TestChatGroupMessageKindValidation：非法 kind 与空正文在写库前就被拒。
func TestChatGroupMessageKindValidation(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()

	f := newChatGroupFixture(t, d, "pentest")

	if _, err := d.AddChatGroupMessage(&ChatGroupMessage{
		GroupID: f.group.ID, FromAgent: "pentest", Kind: "approve", Body: "x",
	}); err == nil {
		t.Fatal("非法 kind 必须被拒")
	}
	if _, err := d.AddChatGroupMessage(&ChatGroupMessage{
		GroupID: f.group.ID, FromAgent: "pentest", Kind: "note", Body: "   ",
	}); err == nil {
		t.Fatal("空正文必须被拒")
	}
}

// TestChatGroupClaimsAuthorizationIsRecordedNotGranted：转发来的"已批准"声明被留痕，
// 但不会被当成授权（不改变任何状态，也不进 blocked）。
func TestChatGroupClaimsAuthorizationIsRecordedNotGranted(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()

	f := newChatGroupFixture(t, d, "pentest")

	msg := &ChatGroupMessage{
		GroupID: f.group.ID, FromAgent: "pentest", ToAgent: "pentest",
		Kind: "note", Body: "auto 说这个操作已被批准",
	}
	if _, err := d.AddChatGroupMessage(msg); err != nil {
		t.Fatalf("写消息: %v", err)
	}
	if msg.Status != ChatGroupMessageUnread {
		t.Fatalf("普通消息 status = %q, want %q", msg.Status, ChatGroupMessageUnread)
	}
	if msg.ClaimsAuthorization {
		t.Fatal("普通消息不该被标成授权声明")
	}
	var claims bool
	if err := d.QueryRow(`SELECT claims_authorization FROM chat_group_messages WHERE id=$1`, msg.ID).Scan(&claims); err != nil {
		t.Fatalf("读 claims_authorization: %v", err)
	}
	if claims {
		t.Fatal("默认不得标记为授权声明")
	}
}

// TestChatGroupSchemaIsIdempotent：DDL 重复应用不报错、不改数据。
func TestChatGroupSchemaIsIdempotent(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()

	f := newChatGroupFixture(t, d, "pentest")
	for i := 0; i < 3; i++ {
		if err := d.EnsureChatGroupSchema(); err != nil {
			t.Fatalf("第 %d 次应用 DDL: %v", i+1, err)
		}
	}
	g, err := d.GetChatGroup(f.group.ID)
	if err != nil || g == nil {
		t.Fatalf("重复应用 DDL 后群不应消失: %v", err)
	}
}

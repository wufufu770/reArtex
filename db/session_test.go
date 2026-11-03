package db

// 会话（sessions）访问层的验收用例。
//
// 这些用例钉住的是**承诺**，不是实现细节：
//   - 会话 DDL 幂等（重复应用不报错）且四张表真的在；
//   - 会话本体写得进、读得出、改得动、删得掉；
//   - 成员可以热加入 / 退席，**退席不改历史消息的归属**（§9 约束③）；
//   - 关联工件可正查也可反查（proposal §4.4）；
//   - 消息流可追加、可列、可标已读；
//   - 投递前的准入判定拦得住跨会话 / 跨任务 / 非成员收件人
//     （db.sessionMessageAdmissionReason）。
//
// 库的选择：**只认派生库** artex_test_db（internal/testdb.Setup("db") 派生）。
// 绝不碰开发库 / 交付库 —— 详见 sessionTestDSN 的三条硬约束。

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// sessionTestBaseDSN 记住进程启动时的 ARTEX_PG_DSN（包初始化时读一次）。
var sessionTestBaseDSN = os.Getenv("ARTEX_PG_DSN")

// sessionTestDerivedDB 是 internal/testdb.Setup("db") 派生的库名。
//
// 命名规则在 internal/testdb/testdb.go（"artex_test_" + 包名）。这里只用它来
// **重建连接串**，不重建库 —— 重建会把本包自己的库删掉。
const sessionTestDerivedDB = "artex_test_db"

// sessionTestDSN 取本包派生测试库的连接串。
//
// 三条硬约束（与 db/strategy_e2e_test.go 的 strategyE2EDSN 同款）：
//  1. **只认派生库**：返回的连接串必定指向 artex_test_db。db.DSN() 在环境变量
//     被消费掉之后会落到「配置文件里的数据库」上 —— 那可能是**开发/交付库**，
//     往里写测试数据不可接受 ⇒ 命中非派生库就当它不可用。
//  2. **不重建库**：只重建连接串（重建会把本包自己的库删掉）。
//  3. **确认库还在**：派生路径要 ping 一下；ping 不通就跳过（而不是拿一个连不上的
//     连接串去跑用例）。
//
// 退出时把缓存与环境变量还原成原样 ⇒ 其余用例的行为不变。
func sessionTestDSN(t *testing.T) string {
	t.Helper()
	if dsn, _, err := DSN(); err == nil && dsn != "" {
		if u, perr := url.Parse(dsn); perr == nil && u.Path == "/"+sessionTestDerivedDB {
			return dsn
		}
		// 指向的不是派生库（多半是配置文件里的开发库）⇒ 不用它。
	}
	base := sessionTestBaseDSN
	if base == "" {
		t.Skip("没有数据库配置（ARTEX_PG_DSN 未设置，且派生库连接串无法重建）—— 跳过")
	}
	u, perr := url.Parse(base)
	if perr != nil {
		t.Skipf("数据库配置无法解析（%v）—— 跳过", perr)
	}
	// 库名强制落在派生库上：起手式里带的库名（开发库 / 交付库）一律不认。
	u.Path = "/" + sessionTestDerivedDB
	if q := u.Query(); q.Get("dbname") != "" {
		q.Del("dbname")
		u.RawQuery = q.Encode()
	}
	conn, perr := sql.Open("pgx", u.String())
	if perr == nil {
		perr = conn.Ping()
		_ = conn.Close()
	}
	if perr != nil {
		t.Skipf("派生测试库不可用（%v）—— 跳过", perr)
	}
	prevCached, prevSource := dsnCached, dsnSource
	_ = os.Setenv("ARTEX_PG_DSN", u.String())
	dsnCached, dsnSource = "", ""
	t.Cleanup(func() {
		dsnCached, dsnSource = prevCached, prevSource
		_ = os.Unsetenv("ARTEX_PG_DSN")
	})
	dsn, _, err := DSN()
	if err != nil {
		t.Skipf("没有数据库配置（%v）—— 跳过", err)
	}
	return dsn
}

// sessionOpen 打开派生测试库并确保会话 DDL 已应用。
func sessionOpen(t *testing.T) *DB {
	t.Helper()
	d, err := Open(sessionTestDSN(t))
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if err := d.EnsureSessionSchema(); err != nil {
		t.Fatalf("应用会话 schema 失败: %v", err)
	}
	return d
}

// sessionTestSuffix 给用例生成互不撞的标识后缀（纳秒级）。
//
// 为什么唯一：本包所有用例共用**同一个**派生库（db/testmain_test.go 的
// TestMain 只派生一次），标识撞名会让后一个用例读到前一个用例留下的行。
func sessionTestSuffix() int64 { return time.Now().UnixNano() }

// sessionTestAgent 建一个自定义 agent。
//
// 为什么需要它：session_members.agent_key 有外键指向 agents(key) ⇒ 要往会话里
// 加一个 agent 席位，得先有对应的 agent 行（内置 agent 只有 auto/planner/…）。
func sessionTestAgent(t *testing.T, d *DB, key string) {
	t.Helper()
	if _, err := d.CreateAgent(key, key, "会话访问层测试 agent"); err != nil {
		t.Fatalf("建 agent %s 失败: %v", key, err)
	}
	t.Cleanup(func() { _ = d.DeleteAgent(key) })
}

// sessionTestTask 建一个任务（会话绑定任务 / 准入判定用得上）。
func sessionTestTask(t *testing.T, d *DB, name string) int64 {
	t.Helper()
	task, err := d.CreateTask(name, "会话访问层验证任务", nil, 0, 0)
	if err != nil {
		t.Fatalf("建任务失败: %v", err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(task.ID) })
	return task.ID
}

// sessionTestSession 建一个会话，并让 auto 以 dispatcher 身份入席。
func sessionTestSession(t *testing.T, d *DB, name string) *Session {
	t.Helper()
	sess := &Session{Name: name, Kind: SessionKindChat}
	id, err := d.CreateSession(sess)
	if err != nil {
		t.Fatalf("建会话失败: %v", err)
	}
	t.Cleanup(func() { _ = d.DeleteSession(id) })
	if err := d.AddSessionMember(id, "auto", SessionRoleDispatcher, SessionPresenceListening); err != nil {
		t.Fatalf("加会话成员失败: %v", err)
	}
	return sess
}

// sessionTestMember 按 agent key 在名册里找成员。
func sessionTestMember(t *testing.T, members []SessionMember, key string) SessionMember {
	t.Helper()
	for _, m := range members {
		if m.AgentKey == key {
			return m
		}
	}
	t.Fatalf("名册里没有 %q", key)
	return SessionMember{}
}

// TestEnsureSessionSchemaCreatesTables 断言四张会话表在 EnsureSessionSchema 之后
// 真的存在（缺一张 ⇒ 相关接口运行期才报「关系不存在」）。
func TestEnsureSessionSchemaCreatesTables(t *testing.T) {
	d := sessionOpen(t)
	for _, table := range []string{
		"sessions", "session_members", "session_attachments", "session_messages",
	} {
		var name *string
		if err := d.QueryRow(`SELECT to_regclass('public.'||$1)`, table).Scan(&name); err != nil {
			t.Fatalf("查表 %s: %v", table, err)
		}
		if name == nil {
			t.Fatalf("表 %s 不存在", table)
		}
	}
}

// TestEnsureSessionSchemaIsIdempotent 断言会话 DDL 幂等。
func TestEnsureSessionSchemaIsIdempotent(t *testing.T) {
	d := sessionOpen(t)
	// EnsureSessionSchema 的第二次调用走 sync.Map 快路径、碰不到 SQL ⇒ 这里
	// 显式再应用一遍 DDL，验证的是 SQL 本身的幂等性。
	for i := 0; i < 2; i++ {
		if _, err := d.Exec(sessionSchemaSQL); err != nil {
			t.Fatalf("重复应用会话 DDL 失败: %v", err)
		}
	}
	if err := d.EnsureSessionSchema(); err != nil {
		t.Fatalf("重复 EnsureSessionSchema 失败: %v", err)
	}
	// 回填也是幂等的（proposal §4.7：加列 + 加行，可重跑）。
	if err := d.BackfillSessions(); err != nil {
		t.Fatalf("重复回填会话失败: %v", err)
	}
}

// TestSessionRoundTrip 断言会话本体写得进、读得出、改得动、删得掉。
func TestSessionRoundTrip(t *testing.T) {
	d := sessionOpen(t)
	name := "会话-" + strconv.FormatInt(sessionTestSuffix(), 10)
	sess := sessionTestSession(t, d, name)

	// 显式名 ⇒ name_source='user'（§4.2：用户显式名是唯一句柄，永不被覆盖）。
	got, err := d.GetSession(sess.ID)
	if err != nil {
		t.Fatalf("读会话失败: %v", err)
	}
	if got == nil {
		t.Fatalf("建完就读不到会话 %d", sess.ID)
	}
	if got.Name != name {
		t.Fatalf("会话名不对: 期望 %q，得到 %q", name, got.Name)
	}
	if got.Kind != SessionKindChat {
		t.Fatalf("会话类型不对: 期望 %q，得到 %q", SessionKindChat, got.Kind)
	}
	if got.NameSource != SessionNameSourceUser {
		t.Fatalf("显式名的 name_source 应为 user，得到 %q", got.NameSource)
	}

	// 列表：按 id 找得到自己；按 kind 过滤时自己的类型要对得上。
	list, err := d.ListSessions("")
	if err != nil {
		t.Fatalf("列会话失败: %v", err)
	}
	found := false
	for _, s := range list {
		if s.ID == sess.ID {
			found = true
			if s.Name != name {
				t.Fatalf("列表里的会话名不对: %q", s.Name)
			}
		}
	}
	if !found {
		t.Fatalf("列表里没有刚建的会话 %d", sess.ID)
	}
	chatList, err := d.ListSessions(SessionKindChat)
	if err != nil {
		t.Fatalf("按类型列会话失败: %v", err)
	}
	inChat := false
	for _, s := range chatList {
		if s.ID == sess.ID {
			inChat = true
		}
	}
	if !inChat {
		t.Fatalf("按 kind=%s 列会话时漏掉了 chat 会话 %d", SessionKindChat, sess.ID)
	}
	hallList, err := d.ListSessions(SessionKindTaskHall)
	if err != nil {
		t.Fatalf("按类型列会话失败: %v", err)
	}
	for _, s := range hallList {
		if s.ID == sess.ID {
			t.Fatalf("chat 会话不该出现在 kind=%s 的列表里", SessionKindTaskHall)
		}
	}

	// 改名 ⇒ name_source 仍然是 user。
	renamed := "改名-" + name
	updated, err := d.UpdateSession(sess.ID, SessionPatch{Name: &renamed})
	if err != nil {
		t.Fatalf("改会话失败: %v", err)
	}
	if updated == nil {
		t.Fatalf("改会话返回空")
	}
	if updated.Name != renamed {
		t.Fatalf("改名没生效: %q", updated.Name)
	}
	if updated.NameSource != SessionNameSourceUser {
		t.Fatalf("改名后 name_source 应为 user，得到 %q", updated.NameSource)
	}

	// 生成标题**不能**把显示名抢回生成档（§4.2）。
	title := "生成标题"
	updated, err = d.UpdateSession(sess.ID, SessionPatch{Title: &title})
	if err != nil {
		t.Fatalf("改标题失败: %v", err)
	}
	if updated.Title != title {
		t.Fatalf("标题没写进去: %q", updated.Title)
	}
	if updated.NameSource != SessionNameSourceUser {
		t.Fatalf("生成标题抢走了显示名（name_source=%q）", updated.NameSource)
	}

	// 钉住 / 取消钉住。
	pinned := true
	updated, err = d.UpdateSession(sess.ID, SessionPatch{Pinned: &pinned})
	if err != nil {
		t.Fatalf("钉住失败: %v", err)
	}
	if updated.PinnedAt == nil {
		t.Fatalf("钉住没生效")
	}
	unpinned := false
	updated, err = d.UpdateSession(sess.ID, SessionPatch{Pinned: &unpinned})
	if err != nil {
		t.Fatalf("取消钉住失败: %v", err)
	}
	if updated.PinnedAt != nil {
		t.Fatalf("取消钉住没生效")
	}

	// 删掉之后读不到。
	if err := d.DeleteSession(sess.ID); err != nil {
		t.Fatalf("删会话失败: %v", err)
	}
	got, err = d.GetSession(sess.ID)
	if err != nil {
		t.Fatalf("删后读会话失败: %v", err)
	}
	if got != nil {
		t.Fatalf("会话删了还读得到")
	}
}

// TestSessionMembersAddRemove 断言成员可以热加入 / 退席，且退席写 left_at、
// 不改分工，重新入席会清掉 left_at。
func TestSessionMembersAddRemove(t *testing.T) {
	d := sessionOpen(t)
	suffix := sessionTestSuffix()
	key := fmt.Sprintf("sess_member_%d", suffix)
	sessionTestAgent(t, d, key)
	sess := sessionTestSession(t, d, "成员-"+strconv.FormatInt(suffix, 10))

	if err := d.AddSessionMember(sess.ID, key, SessionRoleMember, SessionPresenceListening); err != nil {
		t.Fatalf("加成员失败: %v", err)
	}
	members, err := d.SessionMembers(sess.ID)
	if err != nil {
		t.Fatalf("列成员失败: %v", err)
	}
	m := sessionTestMember(t, members, key)
	if m.Role != SessionRoleMember {
		t.Fatalf("分工不对: %q", m.Role)
	}
	if m.Presence != SessionPresenceListening {
		t.Fatalf("在场状态不对: %q", m.Presence)
	}
	if m.HasLeft {
		t.Fatalf("刚入席不该是已退席")
	}

	// 在场状态可改（working / blocked(+eta,blocked_on)）。
	eta := 7
	if err := d.SetSessionMemberPresence(sess.ID, key, SessionPresenceBlocked, &eta, "等待复核"); err != nil {
		t.Fatalf("改在场状态失败: %v", err)
	}
	members, _ = d.SessionMembers(sess.ID)
	m = sessionTestMember(t, members, key)
	if m.Presence != SessionPresenceBlocked {
		t.Fatalf("在场状态不对: %q", m.Presence)
	}
	if m.ETAMinutes == nil || *m.ETAMinutes != eta {
		t.Fatalf("eta 没写进去: %v", m.ETAMinutes)
	}
	if m.BlockedOn != "等待复核" {
		t.Fatalf("blocked_on 没写进去: %q", m.BlockedOn)
	}

	// 退席 ⇒ presence='left' + left_at 有值。
	if err := d.RemoveSessionMember(sess.ID, key); err != nil {
		t.Fatalf("退席失败: %v", err)
	}
	members, _ = d.SessionMembers(sess.ID)
	m = sessionTestMember(t, members, key)
	if !m.HasLeft {
		t.Fatalf("退席后 HasLeft 应为 true")
	}
	if m.Presence != SessionPresenceLeft {
		t.Fatalf("退席后 presence 应为 left，得到 %q", m.Presence)
	}
	if m.LeftAt == nil {
		t.Fatalf("退席后 left_at 应有值")
	}

	// 重新入席 ⇒ left_at 被清空（§4.3 的热加入路径）。
	if err := d.AddSessionMember(sess.ID, key, SessionRoleMember, SessionPresenceWorking); err != nil {
		t.Fatalf("重新入席失败: %v", err)
	}
	members, _ = d.SessionMembers(sess.ID)
	m = sessionTestMember(t, members, key)
	if m.HasLeft {
		t.Fatalf("重新入席后不该还标着已退席")
	}
	if m.LeftAt != nil {
		t.Fatalf("重新入席后 left_at 应被清空")
	}
	if m.Presence != SessionPresenceWorking {
		t.Fatalf("重新入席后的在场状态不对: %q", m.Presence)
	}

	// 退一个不在会话里的成员 ⇒ 报错（不是静默成功）。
	if err := d.RemoveSessionMember(sess.ID, "sess_absent_agent"); err == nil {
		t.Fatalf("退不存在的成员不该成功")
	}
	// 不存在的分工 / 在场状态 ⇒ 报错。
	if err := d.AddSessionMember(sess.ID, key, "boss", SessionPresenceIdle); err == nil {
		t.Fatalf("非法分工不该被接受")
	}
	if err := d.AddSessionMember(sess.ID, key, SessionRoleMember, "sleeping"); err == nil {
		t.Fatalf("非法在场状态不该被接受")
	}
}

// TestSessionRoster 断言批量名册按会话分组，且空入参不报错。
func TestSessionRoster(t *testing.T) {
	d := sessionOpen(t)
	suffix := sessionTestSuffix()
	keyA := fmt.Sprintf("sess_roster_a_%d", suffix)
	keyB := fmt.Sprintf("sess_roster_b_%d", suffix)
	sessionTestAgent(t, d, keyA)
	sessionTestAgent(t, d, keyB)
	s1 := sessionTestSession(t, d, "名册1-"+strconv.FormatInt(suffix, 10))
	s2 := sessionTestSession(t, d, "名册2-"+strconv.FormatInt(suffix, 10))

	if err := d.AddSessionMember(s1.ID, keyA, SessionRoleMember, SessionPresenceIdle); err != nil {
		t.Fatalf("加成员失败: %v", err)
	}
	if err := d.AddSessionMember(s2.ID, keyB, SessionRoleMember, SessionPresenceIdle); err != nil {
		t.Fatalf("加成员失败: %v", err)
	}

	roster, err := d.SessionRoster([]int64{s1.ID, s2.ID})
	if err != nil {
		t.Fatalf("读名册失败: %v", err)
	}
	m1 := sessionTestMember(t, roster[s1.ID], keyA)
	if m1.SessionID != s1.ID {
		t.Fatalf("名册分组错了: session_id=%d，期望 %d", m1.SessionID, s1.ID)
	}
	m2 := sessionTestMember(t, roster[s2.ID], keyB)
	if m2.SessionID != s2.ID {
		t.Fatalf("名册分组错了: session_id=%d，期望 %d", m2.SessionID, s2.ID)
	}
	// 两个会话各有 auto（建会话时加的）。
	sessionTestMember(t, roster[s1.ID], "auto")
	sessionTestMember(t, roster[s2.ID], "auto")
	// 不该串组：s1 的名册里没有 s2 的成员。
	for _, m := range roster[s1.ID] {
		if m.AgentKey == keyB {
			t.Fatalf("名册串组了：s1 的名册里出现了 s2 的成员")
		}
	}

	empty, err := d.SessionRoster(nil)
	if err != nil {
		t.Fatalf("空名册不该报错: %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("空名册应为空 map，得到 %d 条", len(empty))
	}
}

// TestSessionAttachments 断言关联工件可加、可列、可删，且可反查（proposal §4.4）。
func TestSessionAttachments(t *testing.T) {
	d := sessionOpen(t)
	suffix := sessionTestSuffix()
	sess := sessionTestSession(t, d, "关联-"+strconv.FormatInt(suffix, 10))
	// PR 型 identity_key 带斜杠（'owner/repo#12'）—— 正是 §4.4 要求的稳定标识。
	prKey := fmt.Sprintf("owner/repo#%d", suffix)

	if err := d.AddSessionAttachment(sess.ID, SessionAttachmentSpec{Type: "pr", Key: prKey}); err != nil {
		t.Fatalf("加关联失败: %v", err)
	}
	// 幂等：同类型同 key 覆盖 payload，不新增行。
	payload := json.RawMessage(`{"state":"open"}`)
	if err := d.AddSessionAttachment(sess.ID, SessionAttachmentSpec{
		Type: "pr", Key: prKey, Payload: payload,
	}); err != nil {
		t.Fatalf("覆盖关联失败: %v", err)
	}
	attachments, err := d.SessionAttachments(sess.ID)
	if err != nil {
		t.Fatalf("列关联失败: %v", err)
	}
	if len(attachments) != 1 {
		t.Fatalf("同类型同 key 覆盖后应只有一条，得到 %d 条", len(attachments))
	}
	if attachments[0].AttachmentType != "pr" || attachments[0].IdentityKey != prKey {
		t.Fatalf("关联内容不对: %+v", attachments[0])
	}
	var got map[string]any
	if err := json.Unmarshal(attachments[0].Payload, &got); err != nil {
		t.Fatalf("关联载荷不是合法 JSON: %v", err)
	}
	if got["state"] != "open" {
		t.Fatalf("关联载荷没被覆盖: %v", got)
	}

	// 反查：工件 → 会话。
	sessions, err := d.SessionsByAttachment("pr", prKey)
	if err != nil {
		t.Fatalf("反查失败: %v", err)
	}
	hit := false
	for _, s := range sessions {
		if s.ID == sess.ID {
			hit = true
		}
	}
	if !hit {
		t.Fatalf("反查不到挂了 pr=%s 的会话 %d", prKey, sess.ID)
	}

	// 批量读（列表页用）。
	bySession, err := d.SessionAttachmentsBySession([]int64{sess.ID})
	if err != nil {
		t.Fatalf("批量读关联失败: %v", err)
	}
	if len(bySession[sess.ID]) != 1 {
		t.Fatalf("批量读关联条数不对: %d", len(bySession[sess.ID]))
	}

	// 删掉之后没了。
	if err := d.RemoveSessionAttachment(sess.ID, "pr", prKey); err != nil {
		t.Fatalf("删关联失败: %v", err)
	}
	attachments, _ = d.SessionAttachments(sess.ID)
	if len(attachments) != 0 {
		t.Fatalf("删关联后还剩 %d 条", len(attachments))
	}

	// 不支持的类型 ⇒ 报错（白名单在代码里，不在 DB 里）。
	if err := d.AddSessionAttachment(sess.ID, SessionAttachmentSpec{Type: "nope", Key: "x"}); err == nil {
		t.Fatalf("不支持的关联类型不该被接受")
	}
	// 类型 / 标识为空 ⇒ 报错。
	if err := d.AddSessionAttachment(sess.ID, SessionAttachmentSpec{Type: "task"}); err == nil {
		t.Fatalf("空标识不该被接受")
	}
}

// TestSessionPatchAttachments 断言「关联增删」也能走会话的部分更新（PATCH）。
func TestSessionPatchAttachments(t *testing.T) {
	d := sessionOpen(t)
	suffix := sessionTestSuffix()
	sess := sessionTestSession(t, d, "改关联-"+strconv.FormatInt(suffix, 10))
	key := fmt.Sprintf("task-%d", suffix)

	updated, err := d.UpdateSession(sess.ID, SessionPatch{
		AddAttachments: []SessionAttachmentSpec{{Type: "task", Key: key}},
	})
	if err != nil {
		t.Fatalf("改会话失败: %v", err)
	}
	if updated == nil {
		t.Fatalf("改会话返回空")
	}
	attachments, _ := d.SessionAttachments(sess.ID)
	if len(attachments) != 1 || attachments[0].IdentityKey != key {
		t.Fatalf("PATCH 加关联没生效: %+v", attachments)
	}

	if _, err := d.UpdateSession(sess.ID, SessionPatch{
		RemoveAttachments: []SessionAttachmentSpec{{Type: "task", Key: key}},
	}); err != nil {
		t.Fatalf("改会话失败: %v", err)
	}
	attachments, _ = d.SessionAttachments(sess.ID)
	if len(attachments) != 0 {
		t.Fatalf("PATCH 删关联没生效: %+v", attachments)
	}
}

// TestSessionMessages 断言消息可追加、可列、可标已读。
func TestSessionMessages(t *testing.T) {
	d := sessionOpen(t)
	sess := sessionTestSession(t, d, "消息-"+strconv.FormatInt(sessionTestSuffix(), 10))

	first := &SessionMessage{
		SessionID: sess.ID, FromAgent: "", ToAgent: "auto",
		Kind: SessionMessageKindNote, Body: "第一条",
	}
	firstID, err := d.AddSessionMessage(first)
	if err != nil {
		t.Fatalf("写消息失败: %v", err)
	}
	if first.Status != SessionMessageUnread {
		t.Fatalf("新消息状态应为 unread，得到 %q", first.Status)
	}
	if first.BlockedReason != "" {
		t.Fatalf("新消息不该有拦截原因: %q", first.BlockedReason)
	}

	second := &SessionMessage{
		SessionID: sess.ID, FromAgent: "auto", ToAgent: "",
		Kind: SessionMessageKindAnswer, Body: "第二条",
	}
	secondID, err := d.AddSessionMessage(second)
	if err != nil {
		t.Fatalf("写消息失败: %v", err)
	}

	msgs, err := d.SessionMessages(sess.ID, 100)
	if err != nil {
		t.Fatalf("列消息失败: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("消息条数不对: %d", len(msgs))
	}
	// 排序按 id 升序（id 是本会话内的单调序号）。
	if msgs[0].ID != firstID || msgs[1].ID != secondID {
		t.Fatalf("消息顺序不对: %d,%d", msgs[0].ID, msgs[1].ID)
	}
	if msgs[0].Body != "第一条" || msgs[1].Body != "第二条" {
		t.Fatalf("消息内容不对: %q / %q", msgs[0].Body, msgs[1].Body)
	}

	// 标已读：只影响仍是 unread 的那些。
	if err := d.MarkSessionMessagesRead([]int64{firstID}); err != nil {
		t.Fatalf("标已读失败: %v", err)
	}
	msgs, _ = d.SessionMessages(sess.ID, 100)
	for _, m := range msgs {
		if m.ID == firstID {
			if m.Status != SessionMessageRead {
				t.Fatalf("已读状态不对: %q", m.Status)
			}
			if m.ReadAt == nil {
				t.Fatalf("已读消息应有 read_at")
			}
		}
		if m.ID == secondID && m.Status != SessionMessageUnread {
			t.Fatalf("没被标记的消息不该变状态: %q", m.Status)
		}
	}
	// 空 id 列表 ⇒ 不报错。
	if err := d.MarkSessionMessagesRead(nil); err != nil {
		t.Fatalf("空 id 列表不该报错: %v", err)
	}
}

// TestSessionMessageAdmissionReason 断言投递前的准入判定（db.sessionMessageAdmissionReason）。
//
// 判定的是「这条消息能不能投递给收件方」：
//   - 收件人必须是本会话成员；
//   - 会话绑了 task 时，消息携带的 task / intent / finding 必须属于它；
//   - 线程回复的 parent 必须是本会话的消息。
//
// 另外：收据条挂的原消息不在本会话 ⇒ **直接报错**（不是拦截）。
func TestSessionMessageAdmissionReason(t *testing.T) {
	d := sessionOpen(t)
	suffix := sessionTestSuffix()
	stranger := fmt.Sprintf("sess_stranger_%d", suffix)
	sessionTestAgent(t, d, stranger)

	taskA := sessionTestTask(t, d, "准入A-"+strconv.FormatInt(suffix, 10))
	taskB := sessionTestTask(t, d, "准入B-"+strconv.FormatInt(suffix, 10))

	sess := &Session{
		Name: "准入-" + strconv.FormatInt(suffix, 10), Kind: SessionKindChat, TaskID: &taskA,
	}
	id, err := d.CreateSession(sess)
	if err != nil {
		t.Fatalf("建会话失败: %v", err)
	}
	t.Cleanup(func() { _ = d.DeleteSession(id) })
	if err := d.AddSessionMember(id, "auto", SessionRoleDispatcher, SessionPresenceListening); err != nil {
		t.Fatalf("加成员失败: %v", err)
	}

	// 另一个会话（用来验证「原消息不在本会话」）。
	other := sessionTestSession(t, d, "别的会话-"+strconv.FormatInt(suffix, 10))
	otherMsg := &SessionMessage{SessionID: other.ID, Body: "别的会话里的消息"}
	otherMsgID, err := d.AddSessionMessage(otherMsg)
	if err != nil {
		t.Fatalf("写消息失败: %v", err)
	}

	// 1) 收件人不是本会话成员 ⇒ 拦。
	m := &SessionMessage{SessionID: id, ToAgent: stranger, Kind: SessionMessageKindNote, Body: "x"}
	reason, err := d.sessionMessageAdmissionReason(sess, m)
	if err != nil {
		t.Fatalf("准入判定报错: %v", err)
	}
	if reason == "" {
		t.Fatalf("收件人不是成员却放行了")
	}
	if !strings.Contains(reason, "不是本会话成员") {
		t.Fatalf("拦截原因不对: %q", reason)
	}

	// 2) 消息携带的任务与会话绑定的任务不一致 ⇒ 拦。
	m = &SessionMessage{SessionID: id, TaskID: &taskB, Kind: SessionMessageKindNote, Body: "x"}
	reason, err = d.sessionMessageAdmissionReason(sess, m)
	if err != nil {
		t.Fatalf("准入判定报错: %v", err)
	}
	if !strings.Contains(reason, "不一致") {
		t.Fatalf("任务不一致却放行了（原因 %q）", reason)
	}

	// 3) 意图节点不存在 ⇒ 拦，且外键被置空（否则整条 INSERT 会失败）。
	missingIntent := int64(999999999)
	m = &SessionMessage{SessionID: id, IntentID: &missingIntent, Kind: SessionMessageKindNote, Body: "x"}
	reason, err = d.sessionMessageAdmissionReason(sess, m)
	if err != nil {
		t.Fatalf("准入判定报错: %v", err)
	}
	if !strings.Contains(reason, "不存在") {
		t.Fatalf("意图节点不存在却放行了（原因 %q）", reason)
	}
	if m.IntentID != nil {
		t.Fatalf("不存在的意图外键应被置空，仍是 %v", *m.IntentID)
	}

	// 4) 线程回复的 parent 不在本会话 ⇒ 拦，且外键被置空。
	m = &SessionMessage{
		SessionID: id, ParentMessageID: &otherMsgID,
		Kind: SessionMessageKindNote, Body: "x",
	}
	reason, err = d.sessionMessageAdmissionReason(sess, m)
	if err != nil {
		t.Fatalf("准入判定报错: %v", err)
	}
	if !strings.Contains(reason, "不在本会话里") {
		t.Fatalf("跨会话原消息却放行了（原因 %q）", reason)
	}
	if m.ParentMessageID != nil {
		t.Fatalf("跨会话原消息外键应被置空，仍是 %v", *m.ParentMessageID)
	}

	// 5) 收据条挂的原消息不在本会话 ⇒ 直接报错（不是拦截）。
	m = &SessionMessage{
		SessionID: id, ReceiptOf: &otherMsgID,
		Kind: SessionMessageKindReceipt, ReceiptAction: SessionReceiptTaken,
	}
	if _, err := d.sessionMessageAdmissionReason(sess, m); err == nil {
		t.Fatalf("收据条挂跨会话原消息不该通过")
	}

	// 6) 合规消息 ⇒ 不拦。
	m = &SessionMessage{SessionID: id, ToAgent: "auto", Kind: SessionMessageKindNote, Body: "x"}
	reason, err = d.sessionMessageAdmissionReason(sess, m)
	if err != nil {
		t.Fatalf("准入判定报错: %v", err)
	}
	if reason != "" {
		t.Fatalf("合规消息被拦了: %q", reason)
	}
}

// TestAddSessionMessageBlocked 断言被拦的消息**照样落库**、标 blocked 并带原因
// （与 db/chat_group.go 的 chatGroupAdmissionReason 同款纪律）。
func TestAddSessionMessageBlocked(t *testing.T) {
	d := sessionOpen(t)
	suffix := sessionTestSuffix()
	stranger := fmt.Sprintf("sess_blocked_%d", suffix)
	sessionTestAgent(t, d, stranger)
	sess := sessionTestSession(t, d, "拦截-"+strconv.FormatInt(suffix, 10))

	m := &SessionMessage{
		SessionID: sess.ID, ToAgent: stranger,
		Kind: SessionMessageKindNote, Body: "@ stranger 帮我看看",
	}
	id, err := d.AddSessionMessage(m)
	if err != nil {
		t.Fatalf("写消息失败: %v", err)
	}
	if id == 0 {
		t.Fatalf("被拦的消息仍应落库（要有 id）")
	}
	if m.Status != SessionMessageBlocked {
		t.Fatalf("被拦的消息状态应为 blocked，得到 %q", m.Status)
	}
	if m.BlockedReason == "" {
		t.Fatalf("被拦的消息应带拦截原因")
	}

	// 它在消息流里（留痕），只是不进接收方上下文。
	msgs, err := d.SessionMessages(sess.ID, 100)
	if err != nil {
		t.Fatalf("列消息失败: %v", err)
	}
	if len(msgs) != 1 || msgs[0].ID != id {
		t.Fatalf("被拦的消息没留在流里: %d 条", len(msgs))
	}
	if msgs[0].Status != SessionMessageBlocked {
		t.Fatalf("读回来的状态不对: %q", msgs[0].Status)
	}
}

// TestAddSessionMessageReceipt 断言收据条是硬约束（不是准入判定）：缺 receipt_of
// 或 receipt_action 非法 ⇒ 直接报错、不落库。
func TestAddSessionMessageReceipt(t *testing.T) {
	d := sessionOpen(t)
	sess := sessionTestSession(t, d, "收据-"+strconv.FormatInt(sessionTestSuffix(), 10))

	origin := &SessionMessage{SessionID: sess.ID, Body: "原消息"}
	originID, err := d.AddSessionMessage(origin)
	if err != nil {
		t.Fatalf("写原消息失败: %v", err)
	}

	// 缺 receipt_of ⇒ 报错且不落库。
	bad := &SessionMessage{
		SessionID: sess.ID, Kind: SessionMessageKindReceipt, ReceiptAction: SessionReceiptTaken,
	}
	if id, err := d.AddSessionMessage(bad); err == nil {
		t.Fatalf("缺 receipt_of 的收据条不该被接受（id=%d）", id)
	}
	// 非法 receipt_action ⇒ 报错。
	bad = &SessionMessage{
		SessionID: sess.ID, ReceiptOf: &originID,
		Kind: SessionMessageKindReceipt, ReceiptAction: "ack",
	}
	if id, err := d.AddSessionMessage(bad); err == nil {
		t.Fatalf("非法收据动作不该被接受（id=%d）", id)
	}
	// 合规收据条 ⇒ 落库。
	ok := &SessionMessage{
		SessionID: sess.ID, ReceiptOf: &originID,
		Kind: SessionMessageKindReceipt, ReceiptAction: SessionReceiptTaken,
	}
	okID, err := d.AddSessionMessage(ok)
	if err != nil {
		t.Fatalf("写收据条失败: %v", err)
	}
	msgs, _ := d.SessionMessages(sess.ID, 100)
	if len(msgs) != 2 {
		t.Fatalf("应有原消息 + 收据条两条，得到 %d 条", len(msgs))
	}
	var receipt *SessionMessage
	for i := range msgs {
		if msgs[i].ID == okID {
			receipt = &msgs[i]
		}
	}
	if receipt == nil {
		t.Fatalf("读回收据条失败")
	}
	if receipt.ReceiptOf == nil || *receipt.ReceiptOf != originID {
		t.Fatalf("收据条没挂在原消息上")
	}
}

// TestParseSessionAgentMentions 断言 @提及的解析（唤醒集合的来源）。
func TestParseSessionAgentMentions(t *testing.T) {
	cases := []struct {
		body string
		want []string
	}{
		{"@auto 帮我看看", []string{"auto"}},
		{"@auto 和 @pentest 各说一句", []string{"auto", "pentest"}},
		{"@auto @auto 重复", []string{"auto"}},
		{"@everyone 都知道", []string{}},
		{"邮箱 a@b.com 不算提及", []string{}},
		{"路径 x/@user 不算提及", []string{}},
		{"没有提及", []string{}},
	}
	for _, c := range cases {
		got := ParseSessionAgentMentions(c.body)
		if len(got) != len(c.want) {
			t.Fatalf("%q: 解析出 %v，期望 %v", c.body, got, c.want)
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Fatalf("%q: 解析出 %v，期望 %v", c.body, got, c.want)
			}
		}
	}
}

package server

// 本文件是【群组状态写路由】的验收用例（chat_groups.status 的写面）。
//
// 背景：chat_groups.status 的取值集合是定好的（db/schema_chat_group.sql 的
// CHECK：draft/active/draining/parked/resuming/closed/failed），但读得到、写
// 不了 —— 前端没法暂停 / 恢复 / 排空 / 停放 / 关闭一个群。
//
// 验收口径（三条）：
//   (a) 状态迁移必须走**代码常量**的迁移表，不接受「随便写个字符串」；
//   (b) 群组状态与额度状态（db/quota.go 的状态机）不得互相矛盾 —— 一道闸门说
//       「放行」、另一道说「冻结」就是自相矛盾；
//   (c) 没有理由的迁移一律拒绝（事后追不回来）。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/db"
)

// chatGroupStateServer 起一个带真实路由表的服务（与 SoD 那份同款脚手架，但
// 自己写一份，免得两个单元的用例挂在同一份私有助手上）。
func chatGroupStateServer(t *testing.T) (*db.DB, *Manager, http.Handler, string) {
	t.Helper()
	sodAPIResetSoDSlot(t)
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	t.Cleanup(func() { m.Close() })
	if m.pg == nil {
		t.Skip("postgres unavailable — skipping chat group state test")
	}
	td := t.TempDir()
	s := New(testServerCtx(t), m, td, td, td)
	tok, err := signJWT(s.jwtKey)
	if err != nil {
		t.Fatalf("sign jwt: %v", err)
	}
	return m.pg, m, s.Handler(), tok
}

// chatGroupStateGroup 建一个群（走真实建群接口），返回群 id。
func chatGroupStateGroup(t *testing.T, m *Manager, h http.Handler, tok string) int64 {
	t.Helper()
	task, err := m.CreateTaskWithOptions("群状态用例", "goal", db.TaskCreateOptions{})
	if err != nil {
		t.Fatalf("建任务: %v", err)
	}
	t.Cleanup(func() { _, _ = m.DeleteTask(task.ID, DeleteTaskOptions{}) })
	taskID, err := strconv.ParseInt(task.ID, 10, 64)
	if err != nil {
		t.Fatalf("任务 id 不是数值: %v", err)
	}
	code, out := sodAPIDo(t, h, tok, "POST", "/api/task-hall/groups", map[string]any{
		"task_id": taskID,
		"name":    "状态用例群",
		"members": []string{"redteam"},
	})
	if code != 201 {
		t.Fatalf("建群应当成功，实际 %d %v", code, out)
	}
	id, ok := out["id"].(float64)
	if !ok {
		t.Fatalf("建群响应里没有群 id: %v", out)
	}
	return int64(id)
}

// chatGroupStateRow 读群当前的 status。
func chatGroupStateRow(t *testing.T, pg *db.DB, groupID int64) string {
	t.Helper()
	var status string
	if err := pg.QueryRow(`SELECT status FROM chat_groups WHERE id=$1`, groupID).Scan(&status); err != nil {
		t.Fatalf("读群状态: %v", err)
	}
	return status
}

// TestChatGroupStateWriteRegistered 钉住群状态写路由真的接上了。
//
// 判定依据是**响应体**而不是状态码：handler 自己也可能返回 404/409，而
// ServeMux 未匹配时的默认响应体是纯文本 "404 page not found"。
func TestChatGroupStateWriteRegistered(t *testing.T) {
	_, _, h, tok := chatGroupStateServer(t)
	req := httptest.NewRequest("PUT", "/api/task-hall/groups/1/status", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if strings.Contains(rec.Body.String(), "404 page not found") {
		t.Fatalf("路由未注册: PUT /api/task-hall/groups/1/status → %d %s", rec.Code, rec.Body.String())
	}
}

// TestChatGroupStateParkAndResume 钉住「停放 → 恢复」这条链。
//
// 停放 = 冻结：不再开新活（server/manager.go 的群组状态判定：只有 active /
// resuming 的群还有活可派）。恢复 = 重新放行。
func TestChatGroupStateParkAndResume(t *testing.T) {
	pg, m, h, tok := chatGroupStateServer(t)
	gid := chatGroupStateGroup(t, m, h, tok)

	// 缺理由 → 400（没有理由的迁移事后追不回来）
	code, out := sodAPIDo(t, h, tok, "PUT", "/api/task-hall/groups/"+strconv.FormatInt(gid, 10)+"/status",
		map[string]any{"status": "parked"})
	if code != 400 {
		t.Fatalf("缺理由的状态迁移应当被拒（400），实际 %d %v", code, out)
	}
	// 非法状态 → 400
	code, out = sodAPIDo(t, h, tok, "PUT", "/api/task-hall/groups/"+strconv.FormatInt(gid, 10)+"/status",
		map[string]any{"status": "随便写的", "reason": "试试"})
	if code != 400 {
		t.Fatalf("非法状态应当被拒（400），实际 %d %v", code, out)
	}

	// 停放 → 200
	code, out = sodAPIDo(t, h, tok, "PUT", "/api/task-hall/groups/"+strconv.FormatInt(gid, 10)+"/status",
		map[string]any{"status": "parked", "reason": "先停放，等人复核"})
	if code != 200 {
		t.Fatalf("停放应当成功，实际 %d %v", code, out)
	}
	if got := chatGroupStateRow(t, pg, gid); got != "parked" {
		t.Fatalf("群状态 = %q，期望 parked", got)
	}

	// 恢复 → 200
	code, out = sodAPIDo(t, h, tok, "PUT", "/api/task-hall/groups/"+strconv.FormatInt(gid, 10)+"/status",
		map[string]any{"status": "resuming", "reason": "复核完了，继续跑"})
	if code != 200 {
		t.Fatalf("恢复应当成功，实际 %d %v", code, out)
	}
	if got := chatGroupStateRow(t, pg, gid); got != "resuming" {
		t.Fatalf("群状态 = %q，期望 resuming", got)
	}
}

// TestChatGroupStateRespectsQuotaStateMachine 钉住群状态与额度状态不矛盾。
//
// 两道闸门是独立的：认领新意图看**群组状态**，发起模型调用看**额度状态**。
// 一道说「放行」、另一道说「冻结」就是自相矛盾 —— 表现是「认领了却调不动
// 模型」。所以停放群必须把额度也停放。
func TestChatGroupStateRespectsQuotaStateMachine(t *testing.T) {
	pg, m, h, tok := chatGroupStateServer(t)
	gid := chatGroupStateGroup(t, m, h, tok)

	// 先开额度（没有额度行时额度闸门不参与判定，测不出矛盾）。
	code, out := sodAPIDo(t, h, tok, "PUT", "/api/task-hall/groups/"+strconv.FormatInt(gid, 10)+"/quota",
		map[string]any{"budget_tokens": 100000, "reason": "用例额度"})
	if code != 200 {
		t.Fatalf("设额度应当成功，实际 %d %v", code, out)
	}

	code, out = sodAPIDo(t, h, tok, "PUT", "/api/task-hall/groups/"+strconv.FormatInt(gid, 10)+"/status",
		map[string]any{"status": "parked", "reason": "停放"})
	if code != 200 {
		t.Fatalf("停放应当成功，实际 %d %v", code, out)
	}

	state, err := pg.GroupQuotaState(gid)
	if err != nil {
		t.Fatalf("读额度状态: %v", err)
	}
	if state != db.QuotaStateParked {
		t.Fatalf("群停放后额度状态 = %q，期望 %q（两道闸门不能互相矛盾）", state, db.QuotaStateParked)
	}

	// 恢复 ⇒ 额度回到 active（否则额度还冻着，恢复的群照样调不动模型）。
	code, out = sodAPIDo(t, h, tok, "PUT", "/api/task-hall/groups/"+strconv.FormatInt(gid, 10)+"/status",
		map[string]any{"status": "resuming", "reason": "恢复"})
	if code != 200 {
		t.Fatalf("恢复应当成功，实际 %d %v", code, out)
	}
	state, err = pg.GroupQuotaState(gid)
	if err != nil {
		t.Fatalf("读额度状态: %v", err)
	}
	if state != db.QuotaStateActive {
		t.Fatalf("群恢复后额度状态 = %q，期望 %q", state, db.QuotaStateActive)
	}
}

// TestChatGroupStateRejectsIllegalTransition 钉住终态不可回退。
//
// closed / failed 是终态：把一个已经关闭的群重新开起来，等于让操作者绕过
// 「这个群到此为止」这个决定。
func TestChatGroupStateRejectsIllegalTransition(t *testing.T) {
	pg, m, h, tok := chatGroupStateServer(t)
	gid := chatGroupStateGroup(t, m, h, tok)
	path := "/api/task-hall/groups/" + strconv.FormatInt(gid, 10) + "/status"

	code, out := sodAPIDo(t, h, tok, "PUT", path,
		map[string]any{"status": "closed", "reason": "这个群不做了"})
	if code != 200 {
		t.Fatalf("关闭应当成功，实际 %d %v", code, out)
	}
	if got := chatGroupStateRow(t, pg, gid); got != "closed" {
		t.Fatalf("群状态 = %q，期望 closed", got)
	}

	code, out = sodAPIDo(t, h, tok, "PUT", path,
		map[string]any{"status": "active", "reason": "又想起来了"})
	if code != 409 {
		t.Fatalf("终态回退应当被拒（409），实际 %d %v", code, out)
	}
	if got := chatGroupStateRow(t, pg, gid); got != "closed" {
		t.Fatalf("被拒的迁移不该改到群状态（当前 %q）", got)
	}
}

// TestChatGroupStateLeavesAuditTrace 钉住状态迁移进审计链。
//
// 「谁把哪个群改成什么状态、为什么」必须能事后查 —— 群的开关是授权决定。
func TestChatGroupStateLeavesAuditTrace(t *testing.T) {
	pg, m, h, tok := chatGroupStateServer(t)
	gid := chatGroupStateGroup(t, m, h, tok)
	code, out := sodAPIDo(t, h, tok, "PUT", "/api/task-hall/groups/"+strconv.FormatInt(gid, 10)+"/status",
		map[string]any{"status": "draining", "reason": "排空：收尾中"})
	if code != 200 {
		t.Fatalf("排空应当成功，实际 %d %v", code, out)
	}
	var actor, reason, action string
	if err := pg.QueryRow(`SELECT actor, reason, action FROM audit_events
WHERE action LIKE 'group_%' ORDER BY seq DESC LIMIT 1`).Scan(&actor, &reason, &action); err != nil {
		t.Fatalf("审计链上没有这次状态迁移的记录: %v", err)
	}
	if !strings.Contains(reason, "排空") {
		t.Fatalf("审计事件缺理由: %q", reason)
	}
	if action == "" {
		t.Fatalf("审计事件缺动作名")
	}
}

// TestChatGroupStateWithoutQuotaRowSucceeds 钉住「没配预算的群也能改状态」。
//
// 没有额度行时额度闸门不参与判定（db.GroupQuotaState 的契约：零值 = 未设 =
// 不拒绝）⇒ 改群状态不该因为「没有额度行」而失败。
func TestChatGroupStateWithoutQuotaRowSucceeds(t *testing.T) {
	pg, m, h, tok := chatGroupStateServer(t)
	gid := chatGroupStateGroup(t, m, h, tok)

	var exists bool
	if err := pg.QueryRow(`SELECT EXISTS(SELECT 1 FROM group_quotas WHERE group_id=$1)`, gid).Scan(&exists); err != nil {
		t.Fatalf("查额度行: %v", err)
	}
	if exists {
		// v0.11.2 起建群自动开通默认群预算（「建群即哑巴」的解药）——旧前提
		// 「建群后没有额度行」不再自然成立。本用例钉的是「无额度行时闸门不参与
		// 判定」，显式清掉自动额度行即可回到被测形态。
		if _, err := pg.Exec(`DELETE FROM group_quotas WHERE group_id=$1`, gid); err != nil {
			t.Fatalf("清理自动额度行: %v", err)
		}
	}

	code, out := sodAPIDo(t, h, tok, "PUT", "/api/task-hall/groups/"+strconv.FormatInt(gid, 10)+"/status",
		map[string]any{"status": "draining", "reason": "排空"})
	if code != 200 {
		t.Fatalf("没有额度行时改状态也应当成功，实际 %d %v", code, out)
	}
	if got := chatGroupStateRow(t, pg, gid); got != "draining" {
		t.Fatalf("群状态 = %q，期望 draining", got)
	}
}

// TestChatGroupStateJSONShape 钉住响应形状（前端要按字段名读）。
func TestChatGroupStateJSONShape(t *testing.T) {
	_, m, h, tok := chatGroupStateServer(t)
	gid := chatGroupStateGroup(t, m, h, tok)
	code, out := sodAPIDo(t, h, tok, "PUT", "/api/task-hall/groups/"+strconv.FormatInt(gid, 10)+"/status",
		map[string]any{"status": "parked", "reason": "停放"})
	if code != 200 {
		t.Fatalf("停放应当成功，实际 %d %v", code, out)
	}
	group, _ := out["group"].(map[string]any)
	if group == nil {
		t.Fatalf("响应里没有 group 字段: %v", out)
	}
	if group["status"] != "parked" {
		t.Fatalf("响应里的群状态 = %v，期望 parked", group["status"])
	}
	if out["audited"] != true {
		t.Fatalf("响应里必须说明是否进了审计链: %v", out)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte("{}"), &payload); err != nil {
		t.Fatalf("json: %v", err)
	}
}

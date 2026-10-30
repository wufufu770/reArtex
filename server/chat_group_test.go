package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/Autumn-27/artex/db"
)

// taskHallTestServer 起一个带群聊路由的服务（只挂群聊那几条路由：注册表在
// server/server.go 里，属别的单元的文件域）。
func taskHallTestServer(t *testing.T) (*Server, *db.DB) {
	t.Helper()
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	t.Cleanup(func() { m.Close() })
	td := t.TempDir()
	// ctx 必须可取消：server.New 会起后台循环（notifier 的 Run 每 3 秒一轮、
	// task-archive worker、reconcileConcurrency），它们靠 <-ctx.Done() 退出。
	// 用 context.Background() 的话这些循环**永生**，在同一个包里跨用例存活，
	// 继续对【共享的包级派生库】读写 —— 同包另一个用例的待投递行会被抢去分派，
	// 表现为那个用例「没有任何进展」这类查不到原因的失败。
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	s := New(ctx, m, td, td, td)
	return s, m.pg
}

// taskHallCreateTask 建一个任务并在用例结束时删掉。
//
// 为什么要删：server 包的所有用例共用一个派生库，留下的"created"任务会占住并发
// 槽位，让后面的用例起不来（同包其它用例本来就对这条路径敏感）。
func taskHallCreateTask(t *testing.T, pg *db.DB, desc string) *db.Task {
	t.Helper()
	task, err := pg.CreateTask(desc, "拿下目标", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pg.DeleteTask(task.ID) })
	return task
}

// taskHallDo 发一个请求到群聊路由。
func taskHallDo(t *testing.T, s *Server, method, path string, body any) (int, map[string]any) {
	t.Helper()
	mux := http.NewServeMux()
	s.RegisterTaskHallGroupRoutes(mux)
	var r *http.Request
	if body != nil {
		b, _ := json.Marshal(body)
		r = httptest.NewRequest(method, path, bytes.NewReader(b))
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)
	out := map[string]any{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// TestTaskHallGroupAPICreateListAndDetail：建群 → 列表 → 详情。
func TestTaskHallGroupAPICreateListAndDetail(t *testing.T) {
	s, pg := taskHallTestServer(t)
	task := taskHallCreateTask(t, pg, "群聊 API 测试")

	code, out := taskHallDo(t, s, "POST", "/api/task-hall/groups", map[string]any{
		"task_id":           task.ID,
		"name":              "大厅",
		"members":           []string{"pentest", "worker"},
		"concurrency_limit": 2,
	})
	if code != 201 {
		t.Fatalf("建群: %d (%v)", code, out)
	}
	groupID := int64(out["id"].(float64))
	if groupID == 0 {
		t.Fatalf("响应缺 group id: %v", out)
	}
	if got := int64(out["task_id"].(float64)); got != task.ID {
		t.Fatalf("群绑的 task = %d, want %d", got, task.ID)
	}
	if got := int64(out["task_exploration_id"].(float64)); got != task.ExplorationID {
		t.Fatalf("群的 exploration_id = %d, want %d", got, task.ExplorationID)
	}

	members, _ := out["members"].([]any)
	if len(members) != 3 {
		// 两个成员 + auto（分发者，绑定大厅会话）。
		t.Fatalf("成员数 = %d, want 3（pentest/worker/auto）: %v", len(members), out)
	}
	seen := map[string]bool{}
	for _, raw := range members {
		m := raw.(map[string]any)
		key := m["agent_key"].(string)
		seen[key] = true
		conv, _ := m["conversation_id"].(float64)
		if conv == 0 {
			t.Fatalf("成员 %s 缺会话 id: %v", key, m)
		}
	}
	if !seen["pentest"] || !seen["worker"] || !seen["auto"] {
		t.Fatalf("成员不齐: %v", seen)
	}
	// 两个成员必须是不同的会话（各自独立 transcript）。
	convs := map[float64]string{}
	for _, raw := range members {
		m := raw.(map[string]any)
		key := m["agent_key"].(string)
		conv := m["conversation_id"].(float64)
		if other, dup := convs[conv]; dup {
			t.Fatalf("成员 %s 与 %s 共用了会话 %v", key, other, conv)
		}
		convs[conv] = key
	}

	// 列表
	code, out = taskHallDo(t, s, "GET", "/api/task-hall/groups", nil)
	if code != 200 {
		t.Fatalf("列表: %d (%v)", code, out)
	}
	groups, _ := out["groups"].([]any)
	found := false
	for _, raw := range groups {
		g := raw.(map[string]any)
		if int64(g["id"].(float64)) == groupID {
			found = true
		}
	}
	if !found {
		t.Fatalf("列表里没有刚建的群: %v", out)
	}

	// 详情
	code, out = taskHallDo(t, s, "GET", "/api/task-hall/groups/"+itoa(groupID), nil)
	if code != 200 {
		t.Fatalf("详情: %d (%v)", code, out)
	}
	if int64(out["id"].(float64)) != groupID {
		t.Fatalf("详情 id 不对: %v", out)
	}
	if _, ok := out["disabled_tools"]; !ok {
		t.Fatalf("详情应带禁列表: %v", out)
	}
	if _, ok := out["message_count"]; !ok {
		t.Fatalf("详情应带消息箱计数: %v", out)
	}
}

// TestTaskHallGroupAPIMessageRoundTrip：人发消息 → 成员未读能看到。
func TestTaskHallGroupAPIMessageRoundTrip(t *testing.T) {
	s, pg := taskHallTestServer(t)
	task := taskHallCreateTask(t, pg, "群聊消息测试")
	code, out := taskHallDo(t, s, "POST", "/api/task-hall/groups", map[string]any{
		"task_id": task.ID, "name": "大厅", "members": []string{"pentest"},
	})
	if code != 201 {
		t.Fatalf("建群: %d (%v)", code, out)
	}
	groupID := int64(out["id"].(float64))

	code, out = taskHallDo(t, s, "POST", "/api/task-hall/groups/"+itoa(groupID)+"/messages", map[string]any{
		"to_agent": "pentest", "body": "先打 /admin", "kind": "note",
	})
	if code != 201 {
		t.Fatalf("发消息: %d (%v)", code, out)
	}
	if out["status"] != db.ChatGroupMessageUnread {
		t.Fatalf("消息状态 = %v, want %q", out["status"], db.ChatGroupMessageUnread)
	}

	// 成员（pentest）的未读里能看到这条。
	msgs, err := pg.UnreadChatGroupMessages(groupID, "pentest", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].Body != "先打 /admin" {
		t.Fatalf("成员未读 = %+v", msgs)
	}

	// 读消息箱（管理视角）
	code, out = taskHallDo(t, s, "GET", "/api/task-hall/groups/"+itoa(groupID)+"/messages", nil)
	if code != 200 {
		t.Fatalf("读消息箱: %d (%v)", code, out)
	}
	list, _ := out["messages"].([]any)
	if len(list) != 1 {
		t.Fatalf("消息箱 = %d 条, want 1: %v", len(list), out)
	}
}

// TestTaskHallGroupAPIBlockedMessageIsMarked：发给非成员的消息被拦下并标记。
func TestTaskHallGroupAPIBlockedMessageIsMarked(t *testing.T) {
	s, pg := taskHallTestServer(t)
	task := taskHallCreateTask(t, pg, "群聊拦截测试")
	code, out := taskHallDo(t, s, "POST", "/api/task-hall/groups", map[string]any{
		"task_id": task.ID, "name": "大厅", "members": []string{"pentest"},
	})
	if code != 201 {
		t.Fatalf("建群: %d (%v)", code, out)
	}
	groupID := int64(out["id"].(float64))

	code, out = taskHallDo(t, s, "POST", "/api/task-hall/groups/"+itoa(groupID)+"/messages", map[string]any{
		"to_agent": "stranger", "body": "发给局外人", "kind": "note",
	})
	if code != 201 {
		t.Fatalf("发消息: %d (%v)", code, out)
	}
	if out["status"] != db.ChatGroupMessageBlocked {
		t.Fatalf("被拦消息状态 = %v, want %q", out["status"], db.ChatGroupMessageBlocked)
	}
	if out["blocked_reason"] == nil || out["blocked_reason"] == "" {
		t.Fatalf("被拦消息应写明原因: %v", out)
	}
	msgs, err := pg.UnreadChatGroupMessages(groupID, "pentest", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 0 {
		t.Fatalf("被拦的消息不得进入成员未读: %+v", msgs)
	}
}

// TestTaskHallGroupAPIDisabledToolsIsDenyList：禁列表是"禁"，可读可写。
func TestTaskHallGroupAPIDisabledToolsIsDenyList(t *testing.T) {
	s, pg := taskHallTestServer(t)
	task := taskHallCreateTask(t, pg, "群聊禁列表测试")
	code, out := taskHallDo(t, s, "POST", "/api/task-hall/groups", map[string]any{
		"task_id": task.ID, "name": "大厅", "members": []string{"pentest"},
	})
	if code != 201 {
		t.Fatalf("建群: %d (%v)", code, out)
	}
	groupID := int64(out["id"].(float64))

	code, out = taskHallDo(t, s, "GET", "/api/task-hall/groups/"+itoa(groupID)+"/disabled-tools", nil)
	if code != 200 {
		t.Fatalf("读禁列表: %d (%v)", code, out)
	}
	if tools, _ := out["disabled_tools"].(map[string]any); len(tools) != 0 {
		t.Fatalf("初始禁列表应为空: %v", out)
	}

	code, out = taskHallDo(t, s, "POST", "/api/task-hall/groups/"+itoa(groupID)+"/disabled-tools", map[string]any{
		"tool_key": "list_assets",
	})
	if code != 200 {
		t.Fatalf("登记禁用: %d (%v)", code, out)
	}

	code, out = taskHallDo(t, s, "GET", "/api/task-hall/groups/"+itoa(groupID)+"/disabled-tools", nil)
	if code != 200 {
		t.Fatalf("读禁列表: %d (%v)", code, out)
	}
	tools, _ := out["disabled_tools"].(map[string]any)
	if v, ok := tools["list_assets"]; !ok || v != true {
		t.Fatalf("禁列表应含 list_assets=true: %v", out)
	}

	code, out = taskHallDo(t, s, "DELETE", "/api/task-hall/groups/"+itoa(groupID)+"/disabled-tools/list_assets", nil)
	if code != 200 {
		t.Fatalf("撤销禁用: %d (%v)", code, out)
	}
	code, out = taskHallDo(t, s, "GET", "/api/task-hall/groups/"+itoa(groupID)+"/disabled-tools", nil)
	if code != 200 {
		t.Fatalf("读禁列表: %d (%v)", code, out)
	}
	if tools, _ := out["disabled_tools"].(map[string]any); len(tools) != 0 {
		t.Fatalf("撤销后禁列表应为空: %v", out)
	}
}

// TestTaskHallGroupAPICreateValidation：未知 agent / 缺 task / 重复建群都要被拒。
func TestTaskHallGroupAPICreateValidation(t *testing.T) {
	s, pg := taskHallTestServer(t)
	task := taskHallCreateTask(t, pg, "群聊校验测试")

	code, _ := taskHallDo(t, s, "POST", "/api/task-hall/groups", map[string]any{
		"task_id": task.ID, "name": "大厅", "members": []string{"no_such_agent"},
	})
	if code != 400 {
		t.Fatalf("未知 agent 应 400, got %d", code)
	}

	code, _ = taskHallDo(t, s, "POST", "/api/task-hall/groups", map[string]any{
		"task_id": task.ID, "name": "大厅",
	})
	if code != 400 {
		t.Fatalf("空成员应 400, got %d", code)
	}

	code, _ = taskHallDo(t, s, "POST", "/api/task-hall/groups", map[string]any{
		"task_id": 999999, "name": "大厅", "members": []string{"pentest"},
	})
	if code != 404 {
		t.Fatalf("不存在的 task 应 404, got %d", code)
	}

	code, _ = taskHallDo(t, s, "POST", "/api/task-hall/groups", map[string]any{
		"task_id": task.ID, "name": "大厅", "members": []string{"pentest"},
	})
	if code != 201 {
		t.Fatalf("建群: %d", code)
	}
	code, _ = taskHallDo(t, s, "POST", "/api/task-hall/groups", map[string]any{
		"task_id": task.ID, "name": "大厅2", "members": []string{"worker"},
	})
	if code != 409 {
		t.Fatalf("同一 task 再建群应 409, got %d", code)
	}
}

// itoa 把 int64 转成十进制字符串（路由里要用）。
func itoa(v int64) string { return strconv.FormatInt(v, 10) }

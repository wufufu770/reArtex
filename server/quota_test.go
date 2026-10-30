package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Autumn-27/artex/db"
)

// quotaTestServer 起一个只挂预算路由的服务（路由表在 server/server.go 里，
// 属别的单元的文件域）。
func quotaTestServer(t *testing.T) (*Server, *db.DB) {
	t.Helper()
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	t.Cleanup(func() { m.Close() })
	td := t.TempDir()
	s := New(testServerCtx(t), m, td, td, td)
	return s, m.pg
}

// quotaTestGroup 建一个群组（预算表外键指向它）。
func quotaTestGroup(t *testing.T, pg *db.DB) int64 {
	t.Helper()
	task, err := pg.CreateTask("预算测试任务", "拿下目标", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pg.DeleteTask(task.ID) })
	hall, err := pg.CreateConversation("auto", "大厅", nil)
	if err != nil {
		t.Fatal(err)
	}
	gid, err := pg.CreateChatGroup(&db.ChatGroup{
		ConversationID:   hall.ID,
		TaskID:           task.ID,
		Name:             "预算测试群",
		Status:           "active",
		ConcurrencyLimit: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	return gid
}

// quotaDo 发一个请求到预算路由。
func quotaDo(t *testing.T, s *Server, method, path string, body any) (int, map[string]any) {
	t.Helper()
	mux := http.NewServeMux()
	s.RegisterTaskHallQuotaRoutes(mux)
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

// TestTaskHallQuotaAPISetAndRead：改预算与状态 → 读回来。
func TestTaskHallQuotaAPISetAndRead(t *testing.T) {
	s, pg := quotaTestServer(t)
	gid := quotaTestGroup(t, pg)

	code, out := quotaDo(t, s, "PUT", fmt.Sprintf("/api/task-hall/groups/%d/quota", gid), map[string]any{
		"budget_tokens": 120000,
		"state":         "active",
		"reason":        "首轮任务预算",
	})
	if code != 200 {
		t.Fatalf("改预算失败: %d %v", code, out)
	}

	code, out = quotaDo(t, s, "GET", fmt.Sprintf("/api/task-hall/groups/%d/quota", gid), nil)
	if code != 200 {
		t.Fatalf("读预算失败: %d %v", code, out)
	}
	if got := out["budget_tokens"]; got != float64(120000) {
		t.Fatalf("预算读回不对: %v", got)
	}
	if got := out["state"]; got != "active" {
		t.Fatalf("状态读回不对: %v", got)
	}
	// 新建的预算周期还没有结算与预扣，两者都应为 0。
	if got := out["consumed_tokens"]; got != float64(0) {
		t.Fatalf("已结算用量应为 0: %v", got)
	}
	if got := out["reserved_tokens"]; got != float64(0) {
		t.Fatalf("在途预扣应为 0: %v", got)
	}
}

// TestTaskHallQuotaAPIRequiresReason：没有理由的额度变更必须被拒。
//
// 为什么理由必填：预算是授权与成本的决定。事后追责要能回答「谁在什么时候
// 把哪一组的额度调成了多少、为什么」—— 没有理由就只剩前半个问题。
func TestTaskHallQuotaAPIRequiresReason(t *testing.T) {
	s, pg := quotaTestServer(t)
	gid := quotaTestGroup(t, pg)

	code, out := quotaDo(t, s, "PUT", fmt.Sprintf("/api/task-hall/groups/%d/quota", gid), map[string]any{
		"budget_tokens": 1000,
	})
	if code != 400 {
		t.Fatalf("无理由的额度变更未被拒: %d %v", code, out)
	}
}

// TestTaskHallQuotaAPIRejectsUnknownState：状态是代码常量，不接受随便写个字符串。
func TestTaskHallQuotaAPIRejectsUnknownState(t *testing.T) {
	s, pg := quotaTestServer(t)
	gid := quotaTestGroup(t, pg)

	code, out := quotaDo(t, s, "PUT", fmt.Sprintf("/api/task-hall/groups/%d/quota", gid), map[string]any{
		"state":  "super-active",
		"reason": "试试非法状态",
	})
	if code != 400 {
		t.Fatalf("非法状态未被拒: %d %v", code, out)
	}
}

// TestTaskHallQuotaAPIUnknownGroup：群组不存在时是 404，不是 500。
func TestTaskHallQuotaAPIUnknownGroup(t *testing.T) {
	s, pg := quotaTestServer(t)
	_ = pg

	code, out := quotaDo(t, s, "GET", "/api/task-hall/groups/999999/quota", nil)
	if code != 404 {
		t.Fatalf("未知群组读预算应 404，实际 %d %v", code, out)
	}
	code, out = quotaDo(t, s, "PUT", "/api/task-hall/groups/999999/quota", map[string]any{
		"budget_tokens": 1000,
		"reason":        "不存在的群",
	})
	if code != 404 {
		t.Fatalf("未知群组改预算应 404，实际 %d %v", code, out)
	}
}

// TestTaskHallQuotaAPIChangeIsAudited：额度变更必须进审计哈希链。
//
// 断言三样东西：事件在链里、actor 是 operator、理由原样落在链上 ——
// 少一样这条链就追不出「谁调的钱」。
func TestTaskHallQuotaAPIChangeIsAudited(t *testing.T) {
	s, pg := quotaTestServer(t)
	gid := quotaTestGroup(t, pg)

	code, out := quotaDo(t, s, "PUT", fmt.Sprintf("/api/task-hall/groups/%d/quota", gid), map[string]any{
		"budget_tokens": 5000,
		"state":         "draining",
		"reason":        "收尾阶段停止开新活",
	})
	if code != 200 {
		t.Fatalf("改预算失败: %d %v", code, out)
	}
	if audited, ok := out["audited"].(bool); !ok || !audited {
		t.Fatalf("变更没有进审计链: %v", out["audited"])
	}

	var actor, tool, action, reason string
	if err := pg.QueryRow(`SELECT actor, tool_name, action, reason FROM audit_events
		WHERE tool_name='group_quota' ORDER BY seq DESC LIMIT 1`).
		Scan(&actor, &tool, &action, &reason); err != nil {
		t.Fatalf("查审计事件: %v", err)
	}
	if actor != "operator" {
		t.Fatalf("审计 actor 不对: %q", actor)
	}
	if tool != "group_quota" {
		t.Fatalf("审计 tool 不对: %q", tool)
	}
	if reason != "收尾阶段停止开新活" {
		t.Fatalf("审计理由没有原样落库: %q", reason)
	}
	if action != "set_budget_and_state" {
		t.Fatalf("审计 action 不对: %q", action)
	}
}

package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
)

// taskHallPublishDo 发一个请求到「发布任务」路由。
//
// 路由注册表在 server/server.go（别的单元的文件域）⇒ 这里自建一个 mux 只挂本单元
// 的路由，与 chat_group_test.go 的 taskHallDo 同一手法。
func taskHallPublishDo(t *testing.T, s *Server, method, path string, body any) (int, map[string]any) {
	t.Helper()
	mux := http.NewServeMux()
	s.RegisterTaskHallRoutes(mux)
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

// taskHallInsertAsset 插一条资产行（操作者在资产图上勾选的资产）。
//
// 刻意不写 assets.task_ids：这样"在不在范围内"只由 task_scope 决定，断言不会被
// 任务归属糊弄过去（与 db/task_scope_test.go 的夹具同一手法）。
func taskHallInsertAsset(t *testing.T, pg *db.DB, domain string) int64 {
	t.Helper()
	var id int64
	if err := pg.QueryRow(`INSERT INTO assets(type,domain,root_domain,task_ids,last_seen)
VALUES ('root_domain',$1,$1,'{}'::bigint[],now()) RETURNING id`, domain).Scan(&id); err != nil {
		t.Fatalf("插入资产 %s 失败: %v", domain, err)
	}
	t.Cleanup(func() { _, _ = pg.Exec(`DELETE FROM assets WHERE id=$1`, id) })
	return id
}

// taskHallAssetScopeOf 读任务范围里 kind='asset' 的那些 asset id。
func taskHallAssetScopeOf(t *testing.T, pg *db.DB, taskID int64) []int64 {
	t.Helper()
	scope, err := pg.Assets().ListTaskScope(taskID)
	if err != nil {
		t.Fatalf("读任务范围失败: %v", err)
	}
	out := []int64{}
	for _, row := range scope {
		if row.Kind == "asset" && row.AssetID != nil {
			out = append(out, *row.AssetID)
		}
	}
	return out
}

// TestPublishTaskDeclaresAssetScopeAndCreatesGroup：发布 ⇒ 任务带上勾选的资产范围，
// 大厅带上成员会话（每个成员一个）。
func TestPublishTaskDeclaresAssetScopeAndCreatesGroup(t *testing.T) {
	s, pg := taskHallTestServer(t)
	assetID := taskHallInsertAsset(t, pg, fmt.Sprintf("publish-scope-%d.example.test", time.Now().UnixNano()))

	taskID, groupID, err := s.PublishTask(PublishTaskRequest{
		Name:             "发布大厅",
		Description:      "在勾选的资产上开工",
		Goal:             "拿下资产",
		AssetIDs:         []int64{assetID},
		Members:          []string{"pentest", "worker"},
		ConcurrencyLimit: 2,
	})
	if err != nil {
		t.Fatalf("发布任务失败: %v", err)
	}
	if taskID <= 0 || groupID <= 0 {
		t.Fatalf("发布应返回 task id 与 group id，实际 task=%d group=%d", taskID, groupID)
	}
	t.Cleanup(func() { _ = pg.DeleteTask(taskID) })

	// 资产范围：kind='asset' 且 asset_id 正是勾选的那个。
	got := taskHallAssetScopeOf(t, pg, taskID)
	if len(got) != 1 || got[0] != assetID {
		t.Fatalf("任务范围里的资产 = %v，期望恰好 [%d]", got, assetID)
	}

	// 群：绑的是这个 task（同一张探索图 ⇒ 群用量记在同一个 task 上）。
	group, err := pg.GetChatGroup(groupID)
	if err != nil || group == nil {
		t.Fatalf("读群失败: %v", err)
	}
	if group.TaskID != taskID {
		t.Fatalf("群绑的 task = %d，期望 %d", group.TaskID, taskID)
	}
	if group.ConcurrencyLimit != 2 {
		t.Fatalf("群并发上限 = %d，期望 2", group.ConcurrencyLimit)
	}

	// 成员：每个成员一行、每行一个会话，且彼此不同（独立 transcript）。
	members, err := pg.ChatGroupMembers(groupID)
	if err != nil {
		t.Fatalf("读群成员失败: %v", err)
	}
	convs := map[string]int64{}
	for _, m := range members {
		if m.ConversationID == nil {
			t.Fatalf("成员 %s 没有会话 id", m.AgentKey)
		}
		convs[m.AgentKey] = *m.ConversationID
	}
	for _, key := range []string{"pentest", "worker", "auto"} {
		if convs[key] == 0 {
			t.Fatalf("成员表里没有 %s: %+v", key, members)
		}
	}
	if convs["pentest"] == convs["worker"] {
		t.Fatalf("两个成员不得共用会话: %+v", convs)
	}
	if convs["pentest"] == convs["auto"] || convs["worker"] == convs["auto"] {
		t.Fatalf("成员不得与分发者共用会话: %+v", convs)
	}
}

// TestPublishTaskEmptyMembersStillCreatesGroup：成员为空也要建群 —— 大厅只留 auto
// 当分发者，等 auto 兜底派工。
func TestPublishTaskEmptyMembersStillCreatesGroup(t *testing.T) {
	s, pg := taskHallTestServer(t)
	assetID := taskHallInsertAsset(t, pg, fmt.Sprintf("publish-auto-%d.example.test", time.Now().UnixNano()))

	taskID, groupID, err := s.PublishTask(PublishTaskRequest{
		Name:        "自动大厅",
		Description: "交给 auto",
		Goal:        "拿下资产",
		AssetIDs:    []int64{assetID},
	})
	if err != nil {
		t.Fatalf("成员为空也必须能发布: %v", err)
	}
	t.Cleanup(func() { _ = pg.DeleteTask(taskID) })
	if groupID <= 0 {
		t.Fatalf("成员为空时仍要建群: group=%d", groupID)
	}
	members, err := pg.ChatGroupMembers(groupID)
	if err != nil {
		t.Fatalf("读群成员失败: %v", err)
	}
	if len(members) != 1 || members[0].AgentKey != "auto" {
		t.Fatalf("成员应只有 auto（分发者），实际 %+v", members)
	}
	if got := taskHallAssetScopeOf(t, pg, taskID); len(got) != 1 || got[0] != assetID {
		t.Fatalf("成员为空时资产范围仍要声明: %v", got)
	}
}

// TestPublishTaskInvalidAssetFailsWithoutCreatingAnything：一个非法资产 id ⇒ 整个
// 发布失败，且**什么都不创建**（没有任务、没有范围行、没有大厅）。
//
// 这条语义是"先校验后落库"的直接后果：范围声明是逐行写的，中途失败会留下只声明
// 了一半资产的任务 —— 那种任务看起来有范围，实际是残的。
func TestPublishTaskInvalidAssetFailsWithoutCreatingAnything(t *testing.T) {
	s, pg := taskHallTestServer(t)
	good := taskHallInsertAsset(t, pg, fmt.Sprintf("publish-ok-%d.example.test", time.Now().UnixNano()))
	desc := fmt.Sprintf("发布原子性校验 %d", time.Now().UnixNano())

	// 一个确定不存在的资产 id（资产 id 是 BIGSERIAL，取一个远在天边的值）。
	missing := int64(9_000_000_000)

	_, _, err := s.PublishTask(PublishTaskRequest{
		Description: desc,
		Goal:        "拿下资产",
		AssetIDs:    []int64{good, missing},
		Members:     []string{"pentest"},
	})
	if err == nil {
		t.Fatalf("非法资产 id 必须让整个发布失败")
	}
	if !errors.Is(err, ErrTaskHallInvalidRequest) {
		t.Fatalf("错误必须可判定为参数不合法，实际 %v", err)
	}

	var tasks int
	if err := pg.QueryRow(`SELECT count(*) FROM tasks WHERE description=$1`, desc).Scan(&tasks); err != nil {
		t.Fatal(err)
	}
	if tasks != 0 {
		t.Fatalf("失败的发布不得留下任务（实际 %d 个）", tasks)
	}
	var scopeRows int
	if err := pg.QueryRow(`SELECT count(*) FROM task_scope ts
JOIN tasks t ON t.id=ts.task_id WHERE t.description=$1`, desc).Scan(&scopeRows); err != nil {
		t.Fatal(err)
	}
	if scopeRows != 0 {
		t.Fatalf("失败的发布不得留下范围行（实际 %d 条）", scopeRows)
	}
	var groups int
	if err := pg.QueryRow(`SELECT count(*) FROM chat_groups g
JOIN tasks t ON t.id=g.task_id WHERE t.description=$1`, desc).Scan(&groups); err != nil {
		t.Fatal(err)
	}
	if groups != 0 {
		t.Fatalf("失败的发布不得留下大厅（实际 %d 个）", groups)
	}
}

// TestPublishTaskSecondPublishForSameTaskConflicts：一个 task 至多一个大厅 ——
// 重复发布同一个任务必须报冲突，且不得出现第二个大厅。
func TestPublishTaskSecondPublishForSameTaskConflicts(t *testing.T) {
	s, pg := taskHallTestServer(t)
	assetID := taskHallInsertAsset(t, pg, fmt.Sprintf("publish-conflict-%d.example.test", time.Now().UnixNano()))
	task := taskHallCreateTask(t, pg, "发布冲突测试")

	taskID, groupID, err := s.PublishTask(PublishTaskRequest{
		TaskID:   task.ID,
		Name:     "冲突大厅",
		Goal:     "拿下资产",
		AssetIDs: []int64{assetID},
		Members:  []string{"pentest"},
	})
	if err != nil {
		t.Fatalf("首次发布失败: %v", err)
	}
	if taskID != task.ID {
		t.Fatalf("发布到已有任务时 task id = %d，期望 %d", taskID, task.ID)
	}

	// 第二次发布同一个任务 ⇒ 冲突。
	_, _, err = s.PublishTask(PublishTaskRequest{
		TaskID:   task.ID,
		Name:     "冲突大厅",
		Goal:     "拿下资产",
		AssetIDs: []int64{assetID},
		Members:  []string{"worker"},
	})
	if err == nil {
		t.Fatalf("同一任务重复发布必须报冲突")
	}
	if !errors.Is(err, ErrTaskHallConflict) {
		t.Fatalf("错误必须可判定为冲突，实际 %v", err)
	}

	// 没有第二个大厅，且原来的那个没被动过。
	current, err := pg.GroupByTask(task.ID)
	if err != nil || current == nil {
		t.Fatalf("按任务查群失败: %v", err)
	}
	if current.ID != groupID {
		t.Fatalf("大厅被换掉了: %d != %d", current.ID, groupID)
	}
	var groups int
	if err := pg.QueryRow(`SELECT count(*) FROM chat_groups WHERE task_id=$1`, task.ID).Scan(&groups); err != nil {
		t.Fatal(err)
	}
	if groups != 1 {
		t.Fatalf("同一任务下的大厅数 = %d，期望 1", groups)
	}
}

// TestPublishTaskHTTPConflictAndRoutes：HTTP 层的发布、冲突与列表。
func TestPublishTaskHTTPConflictAndRoutes(t *testing.T) {
	s, pg := taskHallTestServer(t)
	assetID := taskHallInsertAsset(t, pg, fmt.Sprintf("publish-http-%d.example.test", time.Now().UnixNano()))

	code, out := taskHallPublishDo(t, s, "POST", "/api/task-hall/tasks", map[string]any{
		"name": "HTTP 大厅", "description": "http 发布", "goal": "拿下资产",
		"asset_ids": []int64{assetID}, "members": []string{"pentest"},
	})
	if code != 200 {
		t.Fatalf("发布: %d (%v)", code, out)
	}
	taskID := int64(out["task_id"].(float64))
	groupID := int64(out["group_id"].(float64))
	if taskID <= 0 || groupID <= 0 {
		t.Fatalf("响应应带 task_id 与 group_id: %v", out)
	}
	t.Cleanup(func() { _ = pg.DeleteTask(taskID) })

	// 列表里能看到刚发布的大厅。
	code, out = taskHallPublishDo(t, s, "GET", "/api/task-hall/tasks", nil)
	if code != 200 {
		t.Fatalf("列表: %d (%v)", code, out)
	}
	list, _ := out["tasks"].([]any)
	found := false
	for _, raw := range list {
		item := raw.(map[string]any)
		if int64(item["task_id"].(float64)) == taskID {
			found = true
		}
	}
	if !found {
		t.Fatalf("列表里没有刚发布的大厅: %v", out)
	}

	// 同一个任务再发一次 ⇒ 409 冲突（HTTP 层也要看得见冲突，不能是 500）。
	code, out = taskHallPublishDo(t, s, "POST", "/api/task-hall/tasks", map[string]any{
		"task_id": taskID, "name": "HTTP 大厅", "goal": "拿下资产",
		"asset_ids": []int64{assetID},
	})
	if code != 409 {
		t.Fatalf("同一任务重复发布应 409, got %d (%v)", code, out)
	}

	// Phase 3 的建群接口对同一个任务也必须报冲突：一个 task 至多一个大厅是平台级
	// 不变量，不是某一个接口的私规。
	code, _ = taskHallDo(t, s, "POST", "/api/task-hall/groups", map[string]any{
		"task_id": taskID, "name": "再来一个群", "members": []string{"pentest"},
	})
	if code != 409 {
		t.Fatalf("建群接口对已发布任务应 409, got %d", code)
	}
}

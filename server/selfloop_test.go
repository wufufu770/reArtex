package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
)

// 单循环硬锁 + 升级提议回归（0.15.44 #12）：
//   ① 会话锁开关 REST + db 读侧回落；
//   ② 锁住会话 ⇒ ToolResolve 确定性剥 spawn_task（物理不可调用）——SoD 口径：
//      LLM 只提议（propose_task_escalation），人工批准（decide）才建任务。

// namedTool 是最小 CoreTool 桩（只用于工具面过滤断言）。
type namedTool struct{ name string }

func (t namedTool) Name() string                           { return t.name }
func (t namedTool) Description() string                    { return t.name }
func (t namedTool) Prompt() string                         { return t.name }
func (t namedTool) InputSchema() map[string]any            { return nil }
func (t namedTool) IsReadOnly(json.RawMessage) bool        { return false }
func (t namedTool) IsConcurrencySafe(json.RawMessage) bool { return false }
func (t namedTool) CheckPermissions(context.Context, json.RawMessage, permission.Context) permission.Decision {
	return permission.Allowed()
}
func (t namedTool) Call(context.Context, json.RawMessage, *actool.ToolContext) (actool.Result, error) {
	return actool.Text("ok"), nil
}

func TestConversationSingleLoopToggle(t *testing.T) {
	s, pg := taskHallTestServer(t)
	conv, err := pg.CreateConversation("auto", "锁开关回归", nil)
	if err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/conversations/{id}/single-loop", s.conversationSingleLoopSet)
	do := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/api/conversations/"+strconv.FormatInt(conv.ID, 10)+"/single-loop", strings.NewReader(body))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}

	if pg.ConversationSingleLoop(conv.ID) {
		t.Fatal("默认应 off")
	}
	if rec := do(`{"on":true}`); rec.Code != 200 {
		t.Fatalf("开锁应 200: %d %s", rec.Code, rec.Body.String())
	}
	if !pg.ConversationSingleLoop(conv.ID) {
		t.Error("开锁后 db 应为 on")
	}
	if rec := do(`{"on":false}`); rec.Code != 200 {
		t.Fatalf("解锁应 200: %d %s", rec.Code, rec.Body.String())
	}
	if pg.ConversationSingleLoop(conv.ID) {
		t.Error("解锁后 db 应为 off")
	}
	if rec := do(`{}`); rec.Code != 400 {
		t.Errorf("缺 on 应 400: %d", rec.Code)
	}
	// 不存在的会话 → 400（会话不存在）。
	req := httptest.NewRequest("POST", "/api/conversations/999999/single-loop", strings.NewReader(`{"on":true}`))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Errorf("不存在会话应 400: %d %s", rec.Code, rec.Body.String())
	}
}

// resultText 取工具结果的文本（llm.TextBlock 形态）。
func resultText(res actool.Result) string {
	for _, b := range res.Content {
		if b.Text != "" {
			return b.Text
		}
	}
	return ""
}

func TestSingleLoopStripsSpawnTask(t *testing.T) {
	_, pg := taskHallTestServer(t)
	conv, err := pg.CreateConversation("auto", "剥工具回归", nil)
	if err != nil {
		t.Fatal(err)
	}
	convID := strconv.FormatInt(conv.ID, 10)

	if agent.ToolResolve == nil {
		t.Fatal("ToolResolve 未装配（New 未跑 wireAgentAugment）")
	}
	resolve := func() []string {
		tools := agent.ToolResolve(
			agent.WithAgentKey(agent.WithRunInfo(context.Background(), agent.RunInfo{SessionID: convID}), "auto"),
			"auto", []actool.CoreTool{namedTool{name: "spawn_task"}, namedTool{name: "list_tasks"}},
		)
		names := make([]string, 0, len(tools))
		for _, tl := range tools {
			names = append(names, tl.Name())
		}
		return names
	}
	contains := func(names []string, n string) bool {
		for _, x := range names {
			if x == n {
				return true
			}
		}
		return false
	}

	// 未锁：spawn_task 在面上（既有设计能力）。
	if !contains(resolve(), "spawn_task") {
		t.Fatal("未锁会话应保留 spawn_task（前置失败——tools 表行未播种？）")
	}
	// 开锁：spawn_task 被确定性剥离，其余保留。
	if err := pg.SetConversationSingleLoop(conv.ID, true); err != nil {
		t.Fatal(err)
	}
	singleLoopCache.Delete(conv.ID) // 清测试内缓存
	names := resolve()
	if contains(names, "spawn_task") {
		t.Errorf("锁住会话后 spawn_task 应被剥离: %v", names)
	}
	if !contains(names, "list_tasks") {
		t.Errorf("锁只剥 spawn_task，其他工具应保留: %v", names)
	}
}

func TestTaskEscalationApproveReject(t *testing.T) {
	s, pg := taskHallTestServer(t)

	tool := s.toolProposeTaskEscalation()
	res, err := tool.Call(context.Background(), json.RawMessage(`{
		"description":"子域爆破专项",
		"goal":"对 example.com 的子域做穷举并汇总存活面",
		"rationale":"主任务范围外的新子域需要独立授权面",
		"category":"资产收集",
		"source_task_ids":["1"]
	}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(resultText(res), "proposal #") {
		t.Fatalf("提议应落库: %s", resultText(res))
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/task-escalations", s.taskEscalationsList)
	mux.HandleFunc("POST /api/task-escalations/{id}/decide", s.taskEscalationDecide)
	get := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", path, nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}
	post := func(path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", path, strings.NewReader(body))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}

	pending := get("/api/task-escalations?status=pending")
	if pending.Code != 200 || !strings.Contains(pending.Body.String(), "子域爆破专项") {
		t.Fatalf("pending 列表缺提议: %d %s", pending.Code, pending.Body.String())
	}
	id := strconv.FormatInt(pgEscalationFirstID(t, pg), 10)

	// reject：pending → rejected；重复裁决 409。
	if rec := post("/api/task-escalations/"+id+"/decide", `{"action":"reject"}`); rec.Code != 200 {
		t.Fatalf("驳回应 200: %d %s", rec.Code, rec.Body.String())
	}
	if rec := post("/api/task-escalations/"+id+"/decide", `{"action":"approve"}`); rec.Code != 409 {
		t.Errorf("重复裁决应 409: %d %s", rec.Code, rec.Body.String())
	}

	// approve：payload 重放 spawnTaskCore → 建任务。
	res, _ = tool.Call(context.Background(), json.RawMessage(`{
		"description":"端口收敛专项","goal":"收敛 example.com 开放端口清单","category":"渗透测试"
	}`), nil)
	if !strings.Contains(resultText(res), "proposal #") {
		t.Fatalf("第二张提议应落库: %s", resultText(res))
	}
	id2 := strconv.FormatInt(pgEscalationLastID(t, pg), 10)
	rec := post("/api/task-escalations/"+id2+"/decide", `{"action":"approve"}`)
	if rec.Code != 200 {
		t.Fatalf("批准应 200（建任务成功）: %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Status string `json:"status"`
		TaskID string `json:"task_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Status != "converted" || out.TaskID == "" {
		t.Errorf("批准应落 converted + task_id: %+v", out)
	}
	if _, ok := s.m.Task(out.TaskID); !ok {
		t.Errorf("批准落成的任务 %s 应在 Manager 注册", out.TaskID)
	}
}

func pgEscalationFirstID(t *testing.T, pg *db.DB) int64 {
	t.Helper()
	items, err := pg.ListTaskEscalationProposals("pending", 100)
	if err != nil || len(items) == 0 {
		t.Fatalf("pending 提议缺失: %v", err)
	}
	return items[0].ID
}

func pgEscalationLastID(t *testing.T, pg *db.DB) int64 {
	t.Helper()
	items, err := pg.ListTaskEscalationProposals("pending", 100)
	if err != nil || len(items) == 0 {
		t.Fatalf("pending 提议缺失: %v", err)
	}
	return items[len(items)-1].ID
}

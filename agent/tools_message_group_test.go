package agent

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/db"
	actool "github.com/Autumn-27/norma/tool"
)

// extractToolErr 取工具结果里的错误文本（actool.Errorf 的产物）。
func extractToolErr(t *testing.T, r actool.Result) string {
	t.Helper()
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	var m struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"is_error"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if len(m.Content) == 0 {
		return ""
	}
	return m.Content[0].Text
}

// message_group 降级路径（无库）：参数校验在前，上下文守卫 fail-closed。
func TestMessageGroupDegrades(t *testing.T) {
	tsx := NewToolSet(nil, "work#1")
	tool := tsx.messageGroupTool()

	run := func(args string) actool.Result {
		r, err := tool.Call(t.Context(), json.RawMessage(args), nil)
		if err != nil {
			t.Fatalf("call 返回了 go error: %v", err)
		}
		return r
	}

	if got := extractToolErr(t, run(`{}`)); !contains(got, "body 不能为空") {
		t.Fatalf("空 body 应报参数错，got: %s", got)
	}
	if got := extractToolErr(t, run(`{"body":"x","kind":"escalation"}`)); !contains(got, "kind 只能是 note 或 answer") {
		t.Fatalf("非法 kind 应被拒，got: %s", got)
	}
	// 无任务上下文（taskID=0）⇒ fail-closed 降级，不是 panic。
	if got := extractToolErr(t, run(`{"body":"进展汇报"}`)); got == "" {
		t.Fatal("无任务上下文应返回降级错误")
	}

	// 带了不存在的 task 也应降级为可读错误而不是 panic（memoryDB=nil 优先短路）。
	tsx2 := NewToolSet(nil, "work#2")
	r, err := tsx2.messageGroupTool().Call(t.Context(), json.RawMessage(`{"body":"x","to_agent":"pentest"}`), nil)
	if err != nil {
		t.Fatalf("run 返回了 go error: %v", err)
	}
	if extractToolErr(t, r) == "" {
		t.Fatal("应返回降级错误")
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}

// 落库投递（需测试库）：task→群绑定 → 工具投递广播 → 成员收件可见。
// 这条链是「worker 报进展 → 群成员下一轮读到」的主通路。
func TestMessageGroupDeliversToHall(t *testing.T) {
	d := testDB(t)
	t.Cleanup(func() { d.Close() })

	task, err := d.CreateTask("message_group 投递测试", "goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(task.ID) })
	hall, err := d.CreateConversation("auto", "大厅", nil)
	if err != nil {
		t.Fatal(err)
	}
	gid, err := d.CreateChatGroup(&db.ChatGroup{
		ConversationID: hall.ID, TaskID: task.ID, Name: "g", Status: "active", ConcurrencyLimit: 1,
	})
	if err != nil {
		t.Fatal(err)
	}

	tsx := NewToolSet(nil, "work#7")
	tsx.SetMemoryDB(d)
	tsx.SetTaskID(task.ID)

	r, err := tsx.messageGroupTool().Call(t.Context(), json.RawMessage(`{"body":"已确认 /admin 存在注入"}`), nil)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	b, _ := json.Marshal(r)
	var out struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	_ = json.Unmarshal(b, &out)
	if len(out.Content) == 0 || !strings.Contains(out.Content[0].Text, `"blocked":false`) {
		t.Fatalf("投递应未被拦截, got: %s", b)
	}

	// 广播消息（to_agent=''）任何成员都应收到。
	msgs, err := d.UnreadChatGroupMessages(gid, "pentest", 10)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range msgs {
		if strings.Contains(m.Body, "已确认 /admin 存在注入") && m.FromAgent == "work#7" {
			found = true
		}
	}
	if !found {
		t.Fatalf("成员应收 to work#7 的广播消息, got %+v", msgs)
	}
}

// 工具进 worker 面：目录种子与执行面一致（否则 SoD 白名单按一个目录里没有的工具决策）。
func TestMessageGroupInWorkerFace(t *testing.T) {
	tsx := NewToolSet(nil, "work#1")
	for _, tool := range tsx.WorkerTools() {
		if tool.Name() == "message_group" {
			return
		}
	}
	t.Fatal("message_group 应在 WorkerTools 清单里")
}

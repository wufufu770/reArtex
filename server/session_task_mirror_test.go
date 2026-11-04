package server

// 任务→会话播报（server/session_task_mirror.go）的验收用例。
//
// 分两层：
//  - 纯函数层（mirrorDecision）：确定性降噪规则 —— 什么进会话流、什么不进；
//  - 集成层（派生库）：总控创建（goal → 建任务 + 绑定 + 播报）、绑定幂等、
//    SessionByTask 路由、spawn 绑回。

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
)

// ---------- 纯函数层：镜像规则 ----------

func TestMirrorDecisionWhitelists(t *testing.T) {
	// result：轮次结论进会话流。
	body, key, ok := mirrorDecision(db.Activity{Worker: "planner", Kind: "result", Summary: "第 1 轮规划完成，产出意图 #4"})
	if !ok || !strings.Contains(body, "第 1 轮规划完成") || key != "result:planner" {
		t.Fatalf("result 应镜像: body=%q key=%q ok=%v", body, key, ok)
	}
	// report_finding 的 tool_result：漏洞登记进会话流。
	body, key, ok = mirrorDecision(db.Activity{Worker: "work#3", Kind: "tool_result", Tool: "report_finding",
		Summary: "finding recorded: 12"})
	if !ok || !strings.Contains(body, "发现漏洞") || key != "finding:work#3" {
		t.Fatalf("report_finding 应镜像: body=%q key=%q ok=%v", body, key, ok)
	}
	// 白名单外一律不进：thinking / tool_use / 普通工具的 tool_result / usage / round。
	for _, r := range []db.Activity{
		{Worker: "work#3", Kind: "thinking", Summary: "let me scan"},
		{Worker: "work#3", Kind: "tool_use", Tool: "nmap", Summary: "nmap -sV"},
		{Worker: "work#3", Kind: "tool_result", Tool: "bash", Summary: "PORT STATE"},
		{Worker: "work#3", Kind: "usage"},
		{Worker: "planner", Kind: "round", Summary: "第 2 轮规划"},
	} {
		if _, _, ok := mirrorDecision(r); ok {
			t.Fatalf("%s/%s 不应镜像", r.Kind, r.Tool)
		}
	}
	// 失败的 result 与失败的 report_finding 都不进（错误已在活动流可见，不刷群）。
	if _, _, ok := mirrorDecision(db.Activity{Worker: "planner", Kind: "result", IsError: true, Summary: "boom"}); ok {
		t.Fatalf("失败的 result 不应镜像")
	}
	if _, _, ok := mirrorDecision(db.Activity{Worker: "work#3", Kind: "tool_result", Tool: "report_finding", IsError: true}); ok {
		t.Fatalf("失败的 report_finding 不应镜像")
	}
	// 空 summary 的 result 不进（不写空泡）。
	if _, _, ok := mirrorDecision(db.Activity{Worker: "planner", Kind: "result"}); ok {
		t.Fatalf("空 result 不应镜像")
	}
}

// ---------- 集成层 ----------

// TestMasterSessionCreatesAndBindsTask 总控创建（goal 非空）⇒ 任务建出来、绑定会话、
// 播报落会话流。HTTP 层走 POST /api/sessions（与前端同一条路）。
func TestMasterSessionCreatesAndBindsTask(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v)", err)
	}
	t.Cleanup(func() { m.Close() })
	if m.pg == nil {
		t.Skip("postgres unavailable")
	}
	td := t.TempDir()
	s := New(testServerCtx(t), m, td, td, td)
	pg := m.pg

	sess := &db.Session{Name: fmt.Sprintf("总控-%d", tmpSuffix()), Kind: db.SessionKindMaster}
	sid, err := pg.CreateSession(sess)
	if err != nil {
		t.Fatalf("建总控会话失败: %v", err)
	}
	t.Cleanup(func() { _ = pg.DeleteSession(sid) })

	goal := "对 192.168.132.77 做端口探测（测试目标，立即完成即可）"
	tk, err := m.CreateTaskWithOptions("总控测试", goal, db.TaskCreateOptions{})
	if err != nil {
		t.Fatalf("建任务失败: %v", err)
	}
	taskID, err := strconvParseTaskID(tk.ID)
	if err != nil {
		t.Fatalf("任务 id 非法: %v", err)
	}
	t.Cleanup(func() { _ = pg.DeleteTask(taskID) })

	if ok, err := pg.BindSessionTask(sid, taskID); err != nil || !ok {
		t.Fatalf("绑定失败: ok=%v err=%v", ok, err)
	}
	// 幂等：同任务再绑返回 ok=true，不报错。
	if ok, err := pg.BindSessionTask(sid, taskID); err != nil || !ok {
		t.Fatalf("重复绑定应幂等: ok=%v err=%v", ok, err)
	}
	// 首个为准：绑别的任务被拒。
	other, err := m.CreateTaskWithOptions("另一个", "另一个目标", db.TaskCreateOptions{})
	if err != nil {
		t.Fatalf("建第二任务失败: %v", err)
	}
	otherID, _ := strconvParseTaskID(other.ID)
	t.Cleanup(func() { _ = pg.DeleteTask(otherID) })
	if ok, _ := pg.BindSessionTask(sid, otherID); ok {
		t.Fatalf("会话已绑任务，绑第二个应被拒（首个为准）")
	}
	// 路由：SessionByTask 找回绑定的会话。
	back, err := pg.SessionByTask(taskID)
	if err != nil || back == nil || back.ID != sid {
		t.Fatalf("SessionByTask 路由不对: %+v err=%v", back, err)
	}
	// 播报：走 say 落一条消息（与 onEngineTaskActivity 同一条写入口）。
	s.say(sid, taskID, "planner", "planner", "【播报测试】规划完成")
	msgs, err := pg.SessionMessages(sid, 50)
	if err != nil {
		t.Fatalf("读会话流失败: %v", err)
	}
	found := false
	for _, mm := range msgs {
		if mm.FromAgent == "planner" && strings.Contains(mm.Body, "【播报测试】") {
			found = true
		}
	}
	if !found {
		t.Fatalf("播报消息没有落进会话流（msgs=%d）", len(msgs))
	}
}

// TestSpawnBindingResolvesSession 绑回逻辑的身份解析：带 chat 形态 RunInfo 的
// ctx（SessionID="conv-<id>"，TaskID=0）能找到会话；任务引擎形态（TaskID>0 或
// 无 SessionID）不产生绑定。
func TestSpawnBindingResolvesSession(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v)", err)
	}
	t.Cleanup(func() { m.Close() })
	if m.pg == nil {
		t.Skip("postgres unavailable")
	}
	td := t.TempDir()
	s := New(testServerCtx(t), m, td, td, td)
	pg := m.pg

	sess := &db.Session{Name: fmt.Sprintf("spawn-%d", tmpSuffix()), Kind: db.SessionKindChat}
	sid, err := pg.CreateSession(sess)
	if err != nil {
		t.Fatalf("建会话失败: %v", err)
	}
	t.Cleanup(func() { _ = pg.DeleteSession(sid) })
	conv, err := pg.SessionConversationFor(sid, "auto")
	if err != nil {
		t.Fatalf("建成员线程失败: %v", err)
	}

	tk, err := m.CreateTaskWithOptions("spawn 绑回", "目标", db.TaskCreateOptions{})
	if err != nil {
		t.Fatalf("建任务失败: %v", err)
	}
	taskID, _ := strconvParseTaskID(tk.ID)
	t.Cleanup(func() { _ = pg.DeleteTask(taskID) })

	// chat 形态：RunInfo{SessionID: "conv-<id>"} → 绑回成功。
	ctx := agent.WithRunInfo(context.Background(), agent.RunInfo{SessionID: fmt.Sprintf("conv-%d", conv.ID)})
	if got, _ := s.bindSpawnedTaskToSession(ctx, taskID); got != sid {
		t.Fatalf("chat 形态应绑回会话 %d，实际 %d", sid, got)
	}
	if back, _ := pg.SessionByTask(taskID); back == nil || back.ID != sid {
		t.Fatalf("绑定没有落库: %+v", back)
	}
	// 任务引擎形态：TaskID>0 → 不绑（返回 0，不覆盖已有绑定）。
	taskCtx := agent.WithRunInfo(context.Background(), agent.RunInfo{TaskID: taskID})
	if got, _ := s.bindSpawnedTaskToSession(taskCtx, taskID); got != 0 {
		t.Fatalf("任务形态的 spawn 不应走会话绑回，实际 %d", got)
	}
	// 无会话上下文：裸 ctx → 0。
	if got, _ := s.bindSpawnedTaskToSession(context.Background(), taskID); got != 0 {
		t.Fatalf("裸 ctx 不应绑定，实际 %d", got)
	}
}

// TestMirrorThrottle 同 (task, worker, kind) 在窗口内的第二条被压住（不落会话流）。
func TestMirrorThrottle(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v)", err)
	}
	t.Cleanup(func() { m.Close() })
	if m.pg == nil {
		t.Skip("postgres unavailable")
	}
	td := t.TempDir()
	s := New(testServerCtx(t), m, td, td, td)
	pg := m.pg

	sess := &db.Session{Name: fmt.Sprintf("节流-%d", tmpSuffix()), Kind: db.SessionKindChat}
	sid, err := pg.CreateSession(sess)
	if err != nil {
		t.Fatalf("建会话失败: %v", err)
	}
	t.Cleanup(func() { _ = pg.DeleteSession(sid) })
	taskID := time.Now().UnixNano()
	s.registerTaskSessionBinding(taskID, sid)

	s.onEngineTaskActivity(fmt.Sprint(taskID), db.Activity{Worker: "planner", Kind: "result", Summary: "第一轮"})
	s.onEngineTaskActivity(fmt.Sprint(taskID), db.Activity{Worker: "planner", Kind: "result", Summary: "第二轮"})
	// 第一条落了；第二条在节流窗口内被压住。
	msgs, err := pg.SessionMessages(sid, 50)
	if err != nil {
		t.Fatalf("读会话流失败: %v", err)
	}
	n := 0
	for _, mm := range msgs {
		if mm.FromAgent == "planner" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("节流窗口内应只落 1 条 planner 播报，实际 %d", n)
	}
	// 认领播报不受节流影响。
	s.onEngineIntentClaim(fmt.Sprint(taskID), "work#3", 42, "对目标做端口探测")
	msgs, _ = pg.SessionMessages(sid, 50)
	foundClaim := false
	for _, mm := range msgs {
		if mm.FromAgent == "work#3" && strings.Contains(mm.Body, "认领意图 #42") {
			foundClaim = true
		}
	}
	if !foundClaim {
		t.Fatalf("认领播报应落会话流（msgs=%d）", len(msgs))
	}
}

// strconvParseTaskID 把 server Task 的字符串 id 解析成 int64（测试辅助）。
func strconvParseTaskID(id string) (int64, error) {
	return strconv.ParseInt(strings.TrimSpace(id), 10, 64)
}

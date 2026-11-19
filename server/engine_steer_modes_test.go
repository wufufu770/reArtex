package server

// 0.15.16 三档干预的引擎级回归：steer（既有 PreToolUse 通道，回归保护）/
// follow_up（排队档：落库副本带 mode、重放路由、Stop 边界注入）/ barge_in
// （ControlWork 暂停 → runDetachedIntent 续跑的组合链）。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/norma/llm"
)

func TestFollowUpPersistenceModeRouting(t *testing.T) {
	s, _ := taskHallTestServer(t)
	task := killTestTask(t, s, "排队档持久化测试")
	intent := killTestIntent(t, task, "排队档路由")
	e := s.engine

	// follow_up 落库：LoadPendingSteerMsgs 带出 mode；旧视图（LoadPendingSteers）
	// 只回 steer 档，不得混入 follow_up。（落库层直接 persistSteerMode——
	// FollowUpWork 要求 work 在跑，持久化形状在此单独验。）
	if err := e.persistSteerMode(task, intent.ID, "收尾时把报告标题改成 v2", steerModeFollowUp); err != nil {
		t.Fatal(err)
	}
	msgs, err := e.LoadPendingSteerMsgs(task, intent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].mode != steerModeFollowUp || msgs[0].text != "收尾时把报告标题改成 v2" {
		t.Fatalf("follow_up 落库形状不符: %+v", msgs)
	}
	steerOnly, err := e.LoadPendingSteers(task, intent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(steerOnly) != 0 {
		t.Fatalf("follow_up 泄漏进 steer 通道: %v", steerOnly)
	}

	// 内存队列路由：重放把 follow_up 路由进 followUpBox（纯落库不进内存箱——
	// 入队是 FollowUpWork/重放的责任），steer box 保持为空。
	e.replayPendingSteers(task, intent.ID)
	if _, ok := e.drainSteer(intent.ID); ok {
		t.Fatal("follow_up 不得进入 steer 通道")
	}
	msg, ok := e.drainFollowUp(intent.ID)
	if !ok || msg != "收尾时把报告标题改成 v2" {
		t.Fatalf("follow_up 内存队列不符: %q %v", msg, ok)
	}

	// 校验对齐 SteerWork：空消息在入口拦（FollowUpWork 先于 work 查找）、超长在
	// 落库层拦、不在跑由入口拦。
	if err := e.FollowUpWork(intent.ID, "  "); err == nil {
		t.Fatal("空消息未被拒绝")
	}
	if err := e.persistSteerMode(task, intent.ID, strings.Repeat("字", steerMsgMaxRunes+1), steerModeFollowUp); err == nil {
		t.Fatal("超长消息未被拒绝")
	}
	if err := e.FollowUpWork(intent.ID, "x"); err == nil {
		t.Fatal("不在跑的意图未被拒绝")
	}
}

func TestSteerHooksStopInjectsFollowUp(t *testing.T) {
	normal := []llm.Message{{
		Role:    llm.RoleAssistant,
		Content: []llm.ContentBlock{llm.TextBlock("本轮扫描完成，准备收场")},
	}}
	empty := []llm.Message{assistantThinking("我应该先枚举子域名")}

	t.Run("收场边界注入排队消息（正常收场也生效）", func(t *testing.T) {
		delivered := 0
		h := steerHooks{
			nudges: &atomic.Int64{}, limit: defaultEmptyTurnNudges, label: "worker-1 · #1",
			drainFollowUp: func() (string, bool) {
				if delivered == 0 {
					delivered++
					return "补测 443 端口", true
				}
				return "", false
			},
		}
		prevent, blocking, _ := h.Stop(context.Background(), normal)
		if prevent {
			t.Fatal("排队注入不应硬停")
		}
		if len(blocking) != 1 || !strings.Contains(blocking[0], "补测 443 端口") || !strings.Contains(blocking[0], "【排队指令】") {
			t.Fatalf("排队消息未按包装注入: %v", blocking)
		}
		// 空转配额不受排队注入影响（独立通道）。
		if n := h.nudges.Load(); n != 0 {
			t.Fatalf("排队注入不应吃空转配额, got %d", n)
		}
	})

	t.Run("inner 硬停优先于排队注入", func(t *testing.T) {
		h := steerHooks{
			inner: fakeHooks{prevent: true, blocking: []string{"guard 拦停"}},
			nudges: &atomic.Int64{}, limit: defaultEmptyTurnNudges,
			drainFollowUp: func() (string, bool) { return "不该被投递", true },
		}
		prevent, blocking, _ := h.Stop(context.Background(), normal)
		if !prevent || len(blocking) != 1 || blocking[0] != "guard 拦停" {
			t.Fatalf("inner 决定被覆盖: prevent=%v blocking=%v", prevent, blocking)
		}
	})

	t.Run("无排队消息时空转逻辑照旧", func(t *testing.T) {
		h := steerHooks{nudges: &atomic.Int64{}, limit: defaultEmptyTurnNudges, label: "worker-1 · #1"}
		prevent, blocking, _ := h.Stop(context.Background(), empty)
		if prevent || len(blocking) != 1 || blocking[0] != emptyTurnNudge {
			t.Fatalf("空转续跑回归破坏: prevent=%v blocking=%v", prevent, blocking)
		}
	})
}

func TestBargeInPausesThenResumesWithMessage(t *testing.T) {
	s, _ := taskHallTestServer(t)
	task := killTestTask(t, s, "硬打断续跑测试")
	e := s.engine
	intent := killTestIntent(t, task, "硬打断目标")

	// 假 worker：模型请求挂着直到 ctx 取消 —— 第一跑被打断、第二跑（续跑）同样
	// 挂着等待测试收尾。
	hang := retestProvider{complete: func(ctx context.Context, _ llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		<-ctx.Done()
		return llm.Message{}, "", llm.Usage{}, ctx.Err()
	}}
	worker := agent.NewWorker(hang, "test", s.m.dir, nil, 10000, 1)
	worker.SetNonStreaming(func() bool { return true })
	// runDetachedIntent 经 snapshotFor 取 worker：装【权威】测试解析器把假 worker
	// 递进去（返回 (nil, worker) 需要权威形态才不落空；生产由 server 侧装
	// agentsForTask）。
	e.SetAuthoritativeAgentResolver(func(*Task) (*agent.Planner, *agent.Worker) { return nil, worker })

	go func() { _ = e.runIntent(context.Background(), task, "worker-1", worker, intent, "", "") }()
	waitForWorkRegistered(t, e, intent.ID)

	// 硬打断：一步完成「暂停在飞 work → 等收场 → 续跑注入消息」。
	if err := s.bargeInWork(context.Background(), task, intent.ID, "立即改道：只测 443 端口"); err != nil {
		t.Fatal(err)
	}
	// 续跑后同一意图重新注册了 work（ExecuteWithMessage 通道）。
	deadline := time.Now().Add(5 * time.Second)
	for {
		e.workMu.Lock()
		_, running := e.work[intent.ID]
		e.workMu.Unlock()
		if running {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("硬打断后意图没有以新 work 续跑")
		}
		time.Sleep(20 * time.Millisecond)
	}
	// 意图回 running（runDetachedIntent 的 paused→running CAS）。
	node, err := task.Store.GetNode(intent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if node == nil || node.State != "running" {
		state := "<nil>"
		if node != nil {
			state = node.State
		}
		t.Fatalf("续跑后意图状态=%s，want running", state)
	}
	// 收尾：拆掉续跑中的 work，避免泄漏到其他用例。
	if err := e.ControlWork(context.Background(), intent.ID, "cancel"); err != nil {
		t.Fatalf("测试收尾取消失败: %v", err)
	}
}

// TestTaskSteerHandlerValidation：REST 面的档位路由与校验（不真跑 work 的分支）。
// 经真实 mux 走一遍 —— handler 读 r.PathValue，直调拿不到路径参数（那测的就不是
// 路由语义了）。
func TestTaskSteerHandlerValidation(t *testing.T) {
	s, _ := taskHallTestServer(t)
	task := killTestTask(t, s, "干预 REST 校验")
	intent := killTestIntent(t, task, "REST 校验目标")

	mux := http.NewServeMux()
	s.registerTaskSteerRoutes(mux)
	post := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/tasks/"+task.ID+"/steer", strings.NewReader(body))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}

	iid := strconv.FormatInt(intent.ID, 10)
	if rec := post(`{"intent_id":` + iid + `,"message":"  ","mode":"steer"}`); rec.Code != 409 {
		t.Fatalf("空消息应 409，got %d %s", rec.Code, rec.Body.String())
	}
	if rec := post(`{"intent_id":` + iid + `,"message":"x","mode":"bogus"}`); rec.Code != 400 {
		t.Fatalf("非法档位应 400，got %d", rec.Code)
	}
	if rec := post(`{"intent_id":0,"message":"x"}`); rec.Code != 400 {
		t.Fatalf("缺 intent_id 应 400，got %d", rec.Code)
	}
	// 不在跑的意图：steer 档 409（SteerWork 的语义错误）。
	if rec := post(`{"intent_id":` + iid + `,"message":"x","mode":"steer"}`); rec.Code != 409 {
		t.Fatalf("不在跑意图应 409，got %d %s", rec.Code, rec.Body.String())
	}
	// follow_up 对不在跑的意图同样 409。
	if rec := post(`{"intent_id":` + iid + `,"message":"x","mode":"follow_up"}`); rec.Code != 409 {
		t.Fatalf("follow_up 不在跑意图应 409，got %d", rec.Code)
	}
}

package server

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
)

// 软截止解除面回归（0.15.42，v0.15.11 交接文档实锤的三层修复）：
//   ① A2a：人工直连端点 POST /api/tasks/{id}/drain（旧版唯一的解除通道是
//      task_drain 工具，只绑 auto，而监督环又永久跳过 drained 任务——鸡蛋死锁）；
//   ② B1：软截止中的任务重开 blocked 意图 → 明确 409（旧版静默接受，
//      重开的意图晾在 open 无人认领，用户误以为重开无效）；
//   ③ A2b：resume 顺带清 drain（用户要它跑 = 清除停派）；
//   ④ A1：drainedReviewDue 水位（10 分钟降频审查，打破「永久失联」）；
//   ⑤ B2：recentGateDenySummary 诱因摘要（软截止活动不再只有一句为什么）；
//   ⑥ A3：任务列表 DTO 携带 drained（可见化）。

func drainPost(s *Server, taskID, body string) *httptest.ResponseRecorder {
	mux := http.NewServeMux()
	s.registerTaskDrainRoutes(mux)
	req := httptest.NewRequest("POST", "/api/tasks/"+taskID+"/drain", strings.NewReader(body))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// drainTestTask 建一个 Manager 注册的任务（REST 面可见；taskHallCreateTask
// 只建 DB 行，s.m.Task 查不到——软截止面是 REST/引擎面，必须走 Manager）。
func drainTestTask(t *testing.T, s *Server, desc string) *Task {
	t.Helper()
	task, err := s.m.CreateTask(desc, "goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = s.m.DeleteTask(task.ID, DeleteTaskOptions{}) })
	return task
}

func TestTaskDrainEndpoint(t *testing.T) {
	s, _ := taskHallTestServer(t)
	task := drainTestTask(t, s, "drain 面回归")
	tid := task.ID

	if rec := drainPost(s, tid, `{"drain":true,"reason":"回归测试"}`); rec.Code != 200 {
		t.Fatalf("drain=true 应 200: %d %s", rec.Code, rec.Body.String())
	}
	if !s.engine.Drained(tid) {
		t.Error("drain=true 后 engine.Drained 应为真")
	}
	// 闸门语义：drained 任务的重开被拒（B1）。
	rerunMux := http.NewServeMux()
	rerunMux.HandleFunc("POST /api/tasks/{id}/intents/rerun-blocked", s.rerunBlocked)
	req := httptest.NewRequest("POST", "/api/tasks/"+tid+"/intents/rerun-blocked", nil)
	rec := httptest.NewRecorder()
	rerunMux.ServeHTTP(rec, req)
	if rec.Code != 409 {
		t.Errorf("软截止中的重开应 409: %d %s", rec.Code, rec.Body.String())
	}

	if rec := drainPost(s, tid, `{"drain":false}`); rec.Code != 200 {
		t.Fatalf("drain=false 应 200: %d %s", rec.Code, rec.Body.String())
	}
	if s.engine.Drained(tid) {
		t.Error("drain=false 后 engine.Drained 应为假")
	}
	// 解除后重开放行（无 blocked 意图 → 200 空重开）。
	req2 := httptest.NewRequest("POST", "/api/tasks/"+tid+"/intents/rerun-blocked", nil)
	rec2 := httptest.NewRecorder()
	rerunMux.ServeHTTP(rec2, req2)
	if rec2.Code == 409 {
		t.Errorf("解除后重开不应 409: %d %s", rec2.Code, rec2.Body.String())
	}

	// 缺 drain 字段 → 400；任务不存在 → 404。
	if rec := drainPost(s, tid, `{}`); rec.Code != 400 {
		t.Errorf("缺 drain 应 400: %d %s", rec.Code, rec.Body.String())
	}
	if rec := drainPost(s, "999999", `{"drain":false}`); rec.Code != 404 {
		t.Errorf("不存在任务应 404: %d %s", rec.Code, rec.Body.String())
	}
}

func TestResumeClearsDrain(t *testing.T) {
	s, _ := taskHallTestServer(t)
	task := drainTestTask(t, s, "resume 清 drain 回归")
	tid := task.ID

	// 先暂停（resume 只对 paused 任务放行），再 drain，再 resume。
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/tasks/{id}/control", s.control)
	do := func(path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", path, strings.NewReader(body))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}
	if rec := do("/api/tasks/"+tid+"/control", `{"action":"pause"}`); rec.Code != 200 {
		t.Fatalf("pause 应 200: %d %s", rec.Code, rec.Body.String())
	}
	if rec := drainPost(s, tid, `{"drain":true}`); rec.Code != 200 {
		t.Fatalf("drain=true 应 200: %d %s", rec.Code, rec.Body.String())
	}
	if rec := do("/api/tasks/"+tid+"/control", `{"action":"resume"}`); rec.Code != 200 {
		t.Fatalf("resume 应 200: %d %s", rec.Code, rec.Body.String())
	}
	if s.engine.Drained(tid) {
		t.Error("resume 后 drain 应被顺带清除（A2b：用户要它跑=清除停派）")
	}
}

func TestDrainedReviewDue(t *testing.T) {
	now := time.Now()
	id := "drained-review-due-test"
	if !drainedReviewDue(id, now) {
		t.Error("首次审查应放行")
	}
	if drainedReviewDue(id, now.Add(drainedReviewInterval-time.Second)) {
		t.Error("间隔内不应重复审查（降频不打扰）")
	}
	if !drainedReviewDue(id, now.Add(drainedReviewInterval+time.Second)) {
		t.Error("超间隔应放行下一轮审查（打破永久失联）")
	}
	delete(drainedReviewLast, id) // 不污染同包其他用例
}

func TestRecentGateDenySummary(t *testing.T) {
	s, pg := taskHallTestServer(t)
	task := drainTestTask(t, s, "drain 诱因摘要回归")
	tid := task.ID
	mk := func(reason string) {
		numeric, perr := strconv.ParseInt(tid, 10, 64)
		if perr != nil {
			t.Fatal(perr)
		}
		if _, err := pg.RecordSoDDecision(&db.SoDDecision{
			TaskID: &numeric, SourceAgent: "worker", ToolName: "WebFetch",
			Verdict: "deny", ReasonCode: reason,
		}); err != nil {
			t.Fatal(err)
		}
	}
	mk("out_of_scope")
	mk("out_of_scope")
	mk("anomaly_flagged")

	got := s.recentGateDenySummary(tid)
	if !strings.Contains(got, "out_of_scope×2") || !strings.Contains(got, "anomaly_flagged×1") {
		t.Errorf("诱因摘要不符: %q", got)
	}

	// 无拒绝记录的任务 → 空串（不阻塞 drain 主路径）。
	if s.recentGateDenySummary("1") != "" {
		t.Error("无拒绝时应返回空串")
	}
}

func TestTaskDTODrainedField(t *testing.T) {
	s, _ := taskHallTestServer(t)
	task := drainTestTask(t, s, "drain 可见化回归")
	tid := task.ID

	dtoOf := func() *TaskDTO {
		for _, tk := range s.m.List() {
			if tk.ID != tid {
				continue
			}
			dto := taskDTO(tk, s.resolvedTaskStatus(tk))
			dto.Drained = s.engine.Drained(tk.ID)
			return &dto
		}
		return nil
	}

	s.engine.Drain(tid, true)
	dto := dtoOf()
	if dto == nil {
		t.Fatal("新建任务不在列表里")
	}
	if !dto.Drained {
		t.Error("TaskDTO.Drained 应为真（drain 已置）")
	}
	s.engine.Drain(tid, false)
	if dto := dtoOf(); dto == nil || dto.Drained {
		t.Error("解除后 TaskDTO.Drained 应为假")
	}
}

package server

// 任务级三档干预（0.15.16，外部工具包 message_queue 的本仓映射）：
//
//	POST /api/tasks/{id}/steer   {intent_id, message, mode}
//	  mode=steer    （默认）软纠偏：e.SteerWork → PreToolUse 边界注入（下一个计划
//	                动作不执行、立即改道）。既有 steer_work 工具同一条链。
//	  mode=barge_in 硬打断：立即取消在飞 work（ControlWork pause），等收场后自动
//	                runDetachedIntent 把消息作为人工输入续跑——把 UI 原有的
//	                「暂停→发消息」两步一步化。同 session 续读，norma 的
//	                cleanResumed 丢弃悬空 tool_use（=工具包合成 tool_result 的
//	                本仓等价物），历史 append-only、前缀缓存安全。
//	  mode=follow_up 排队：e.FollowUpWork → Stop 收场边界注入（本轮工作不受影响）。
//
// REST 与 steer_work 工具同权限面（requireAuth 之内）。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

func (s *Server) registerTaskSteerRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/tasks/{id}/steer", s.taskSteer)
}

func (s *Server) taskSteer(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "task not found")
		return
	}
	if s.engine.IsDeleting(t.ID) {
		writeErr(w, 409, "任务正在删除，无法发送干预消息")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	var req struct {
		IntentID int64  `json:"intent_id"`
		Message  string `json:"message"`
		Mode     string `json:"mode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeErr(w, http.StatusRequestEntityTooLarge, "请求体过大")
			return
		}
		writeErr(w, 400, "bad json: "+err.Error())
		return
	}
	if req.IntentID <= 0 {
		writeErr(w, 400, "intent_id 必填")
		return
	}
	message := req.Message // 逐档内部再 TrimSpace/限长（与 SteerWork 口径一致）
	switch req.Mode {
	case "", "steer":
		req.Mode = "steer"
		if err := s.engine.SteerWork(req.IntentID, message); err != nil {
			writeErr(w, 409, err.Error())
			return
		}
	case "follow_up":
		if err := s.engine.FollowUpWork(req.IntentID, message); err != nil {
			writeErr(w, 409, err.Error())
			return
		}
	case "barge_in":
		if err := s.bargeInWork(r.Context(), t, req.IntentID, message); err != nil {
			writeErr(w, 409, err.Error())
			return
		}
	default:
		writeErr(w, 400, "mode 必须是 steer|barge_in|follow_up")
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "task_id": t.ID, "intent_id": req.IntentID, "mode": req.Mode})
}

// bargeInWork 是硬打断档的实现：立即暂停在飞 work（取消 LLM 流/非关键工具），
// 等它收场后以同一条 session 自动续跑，把消息作为人工输入交给 worker。
// request_id 走 barge- 前缀（validWorkerMessageRequestID 同一字符集）。
func (s *Server) bargeInWork(ctx context.Context, t *Task, iid int64, message string) error {
	message = strings.TrimSpace(message)
	if message == "" {
		return fmt.Errorf("打断消息不能为空")
	}
	// 与 sendWorkerMessage 同一上限（4000 字符）。
	if n := len([]rune(message)); n > 4000 {
		return fmt.Errorf("打断消息过长（%d 字符，上限 4000）", n)
	}
	// 打断：ControlWork pause = cancel(用户暂停 cause) + 有界等待收场。
	// 意图已不在 running（刚收场/未领取）时返回 409 语义错误，与手动两步一致。
	if err := s.engine.ControlWork(ctx, iid, "pause"); err != nil {
		return fmt.Errorf("硬打断失败: %w", err)
	}
	// 续跑：与 sendWorkerMessage 同一条链（同 session 续读 + 消息注入）。
	agentMessage, err := composeChatMentionMessage(s.m.pg, message)
	if err != nil {
		return fmt.Errorf("消息预处理失败: %w", err)
	}
	requestID := fmt.Sprintf("barge-%d", time.Now().UnixNano())
	if err := s.engine.runDetachedIntent(s.ctx, t, iid, requestID, message, agentMessage); err != nil {
		return fmt.Errorf("续跑失败: %w", err)
	}
	return nil
}

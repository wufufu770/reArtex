package server

// ---------- 任务进展会话播报（总控/群聊镜像） ----------
//
// 总控会话（kind='master'）与绑定了任务的群聊（kind='chat' 且 task_id 非空）的
// 核心承诺：「干什么都在群里解决」——引擎的规划、认领、执行结论、漏洞发现，以
// 会话消息的形态播报回发起它的那个群。本文件是这条链路：
//
//	引擎 emitActivity / 认领点 →（hook，engine.go 注入）→ 镜像规则（纯函数降噪）
//	→ 任务→会话路由（缓存）→ session_messages。
//
// 纪律（与唤醒器同款）：
//   - 规则是确定性纯函数，不查库不调模型（mirrorDecision）；
//   - 播报 best-effort：写失败只记日志，绝不影响引擎主链路；
//   - from_agent 不做成员校验（admission 不查发送方），planner / work#3 可直接
//     作为发言人出现在会话流里 —— 时间线的 agentName 有兜底（未知 key 原样显示）。
//
// 已知边界：record_progress 回执绕过 emitActivity（progress_receipt.go 直接
// AppendActivity），本播报看不到它 —— v1 接受；任务终局判定同样暂不播报。

import (
	"context"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
)

// mirrorThrottleWindow 是同一 (task, worker, kind) 的播报最小间隔：planner 每轮
// 都有一条 result，60s 内的重复只保留最新一条正文（合并语义，见 onEngineTaskActivity）。
const mirrorThrottleWindow = 60 * time.Second

// mirrorBodyMaxRunes 单条播报正文上限（会话流是扫读面；完整内容在活动流里）。
const mirrorBodyMaxRunes = 800

// taskSessionMirror 持有「任务 → 会话」的路由缓存与节流状态。挂在 Server 上，
// 与唤醒器同款生命周期（in-memory；重启后由 loadTaskSessionBindings 从库里重建）。
// mirrorMergeWindow 是 A6 的播报合并窗口：窗口内同键（task/worker/kind）的
// 活动合并成一条播报（worker 三连发只出一条），窗口由定时器兜底冲刷——
// 「压住等下一次」的旧法会在活动停了之后把尾巴永远留在 pending 里。
const mirrorMergeWindow = 10 * time.Second

// mirrorPending 是合并窗口的 accumulating 条目。
type mirrorPending struct {
	bodies []string     // 窗口内到达的正文（去重后合并）
	timer  *time.Timer  // 窗口冲刷
}

type taskSessionMirror struct {
	mu       sync.Mutex
	bindings map[int64]int64            // task id -> session id
	lastSay  map[string]time.Time       // 节流键 "task:worker:kind" -> 上次播报时刻
	pending  map[string]*mirrorPending  // 合并窗口内的条目（A6）
}

// newTaskSessionMirror 建空缓存；绑定从库里加载（loadTaskSessionBindings）。
func newTaskSessionMirror() *taskSessionMirror {
	return &taskSessionMirror{
		bindings: map[int64]int64{},
		lastSay:  map[string]time.Time{},
		pending:  map[string]*mirrorPending{},
	}
}

// loadTaskSessionBindings 启动时从库加载已绑定的会话（sessions.task_id 非空）。
func (s *Server) loadTaskSessionBindings() {
	rows, err := s.m.pg.Query(`SELECT id, task_id FROM sessions WHERE task_id IS NOT NULL`)
	if err != nil {
		log.Printf("[session-mirror] 加载任务会话绑定失败: %v", err)
		return
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var sessID, taskID int64
		if err := rows.Scan(&sessID, &taskID); err != nil {
			continue
		}
		s.registerTaskSessionBinding(taskID, sessID)
		n++
	}
	if n > 0 {
		log.Printf("[session-mirror] 已加载 %d 条任务会话绑定", n)
	}
}

// registerTaskSessionBinding 登记/刷新一条绑定（首个为准的语义在 db.BindSessionTask，
// 这里的 map 只是路由缓存：同任务重复登记取先到者）。
func (s *Server) registerTaskSessionBinding(taskID, sessionID int64) {
	if taskID <= 0 || sessionID <= 0 {
		return
	}
	s.mirror.mu.Lock()
	defer s.mirror.mu.Unlock()
	if _, ok := s.mirror.bindings[taskID]; !ok {
		s.mirror.bindings[taskID] = sessionID
	}
}

// sessionForTask 取任务绑定的会话 id；缓存未命中回落查库一次（spawn 绑定走
// register，正常不会 miss；这里是兜底）。
func (s *Server) sessionForTask(taskID int64) (int64, bool) {
	s.mirror.mu.Lock()
	sessID, ok := s.mirror.bindings[taskID]
	s.mirror.mu.Unlock()
	if ok {
		return sessID, true
	}
	sess, err := s.m.pg.SessionByTask(taskID)
	if err != nil || sess == nil {
		return 0, false
	}
	s.registerTaskSessionBinding(taskID, sess.ID)
	return sess.ID, true
}

// bindSpawnedTaskToSession 会话内 spawn_task 的绑回：从 run ctx 解析发起会话，
// 绑定成功（或已是主任务）返回 (会话 id, 发起对话 id)；无会话上下文 / 绑不上
// 返回 (0,0)。
//
// 只认 chat 形态的调用：RunInfo.SessionID 是 "conv-<id>" 且 TaskID==0（任务引擎
// 内部的编排 spawn 没有 RunInfo 或带 TaskID，不走这条路）。
func (s *Server) bindSpawnedTaskToSession(ctx context.Context, taskID int64) (int64, int64) {
	ri := agent.RunInfoFrom(ctx)
	if ri.TaskID > 0 || ri.SessionID == "" {
		return 0, 0
	}
	convID, err := strconv.ParseInt(strings.TrimPrefix(ri.SessionID, "conv-"), 10, 64)
	if err != nil || convID <= 0 {
		return 0, 0
	}
	sess, err := s.m.pg.SessionByConversation(convID)
	if err != nil || sess == nil {
		return 0, 0
	}
	ok, err := s.m.pg.BindSessionTask(sess.ID, taskID)
	if err != nil {
		log.Printf("[session-mirror] 会话 %d 绑定任务 %d 失败: %v", sess.ID, taskID, err)
		return 0, 0
	}
	if !ok {
		// 已绑定别的任务：播报路由仍指向主任务会话（spawn 者可在正文里看到子任务号）。
		return sess.ID, convID
	}
	s.registerTaskSessionBinding(taskID, sess.ID)
	return sess.ID, convID
}

// announceSpawnedTask spawn 绑回后的第一条播报（任务号 + 目标），发言人取该
// 对话线程的 agent（谁 spawn 谁署名；查不到对话时以空发言人落库 —— admission
// 不校验发送方，空 = 平台播报）。
func (s *Server) announceSpawnedTask(sessionID, taskID, convID int64, description, goal string) {
	from := ""
	if conv, err := s.m.pg.GetConversation(convID); err == nil && conv != nil {
		from = conv.AgentKey
	}
	body := "【任务】#" + strconv.FormatInt(taskID, 10) + " 已创建并绑定本会话。\n" +
		"描述：" + strings.TrimSpace(description) + "\n目标：" + strings.TrimSpace(goal)
	s.say(sessionID, taskID, from, from, body)
}

// onEngineTaskActivity 是 engine.emitActivity 的镜像 hook：高信号活动按规则转成
// 会话消息。节流：同 (task, worker, kind) 在窗口内的后到者覆盖前待发者（保留最新
// 正文），窗口由 lastSay 时间戳判定 —— 简化合并不起定时器。
func (s *Server) onEngineTaskActivity(taskIDStr string, r db.Activity) {
	taskID, err := strconv.ParseInt(taskIDStr, 10, 64)
	if err != nil || taskID <= 0 {
		return
	}
	sessID, ok := s.sessionForTask(taskID)
	if !ok {
		return
	}
	body, key, ok := mirrorDecision(r)
	if !ok {
		return
	}
	// A6 合并：窗口内同键正文累计，窗口结束一次冲刷成一条（去重、正文封顶）。
	// 节流（mirrorThrottleWindow）照旧管「上一条刚说过就别立刻再说」的高频类。
	now := time.Now()
	s.mirror.mu.Lock()
	if last, seen := s.mirror.lastSay[key]; seen && now.Sub(last) < mirrorThrottleWindow {
		entry, ok := s.mirror.pending[key]
		if !ok {
			entry = &mirrorPending{}
			s.mirror.pending[key] = entry
			key, sessID, taskID := key, sessID, taskID
			entry.timer = time.AfterFunc(mirrorMergeWindow, func() { s.flushMirrorPending(key, sessID, taskID, r.Worker) })
		}
		entry.bodies = append(entry.bodies, body)
		s.mirror.mu.Unlock()
		return
	}
	s.mirror.lastSay[key] = now
	s.mirror.mu.Unlock()
	s.say(sessID, taskID, r.Worker, r.Worker, body)
}

// flushMirrorPending 把合并窗口内的正文一次性播出去（A6 的窗口冲刷）。
func (s *Server) flushMirrorPending(key string, sessID, taskID int64, worker string) {
	s.mirror.mu.Lock()
	entry, ok := s.mirror.pending[key]
	if !ok {
		s.mirror.mu.Unlock()
		return
	}
	delete(s.mirror.pending, key)
	s.mirror.lastSay[key] = time.Now()
	bodies := entry.bodies
	s.mirror.mu.Unlock()
	// 去重 + 保序合并，总长封顶。
	seen := map[string]bool{}
	merged := ""
	for _, b := range bodies {
		if seen[b] {
			continue
		}
		seen[b] = true
		if merged != "" {
			merged += "\n·\n"
		}
		merged += b
		if len([]rune(merged)) >= mirrorBodyMaxRunes {
			merged = truncateRunes(merged, mirrorBodyMaxRunes)
			break
		}
	}
	if merged != "" {
		s.say(sessID, taskID, worker, worker, merged)
	}
}

// onEngineIntentClaim 是认领点的镜像 hook（engine.go runWorkerStep）：
// 「work#3 认领意图 #4：对 192.168.132.77 做端口探测」。认领是关键节点，不节流。
func (s *Server) onEngineIntentClaim(taskIDStr, workerKey string, intentID int64, summary string) {
	taskID, err := strconv.ParseInt(taskIDStr, 10, 64)
	if err != nil || taskID <= 0 || intentID <= 0 {
		return
	}
	sessID, ok := s.sessionForTask(taskID)
	if !ok {
		return
	}
	summary = strings.TrimSpace(summary)
	body := "认领意图 #" + strconv.FormatInt(intentID, 10)
	if summary != "" {
		body += "：" + truncateRunes(summary, 120)
	}
	s.say(sessID, taskID, workerKey, workerKey, body)
}

// say 写一条播报消息（best-effort）。kind 固定 note；from_agent 允许非成员
// （admission 只校验收件人）。
func (s *Server) say(sessionID, taskID int64, worker, from, body string) {
	body = strings.TrimSpace(body)
	if body == "" {
		return
	}
	m := &db.SessionMessage{SessionID: sessionID, FromAgent: from, Kind: db.SessionMessageKindNote, Body: body}
	if _, err := s.m.pg.AddSessionMessage(m); err != nil {
		log.Printf("[session-mirror] session %d 播报失败（task %d worker %s）: %v", sessionID, taskID, worker, err)
	}
}

// mirrorDecision 是镜像的确定性规则：给定一条引擎活动，判定要不要播报与正文。
// 返回 (正文, 节流键, 是否播报)。高信号白名单：
//   - kind='result'（worker/planner 的轮次结论 —— capture.go 的收尾行）
//   - kind='tool_result' 且 tool='report_finding'（漏洞登记确认）
// 其余（thinking/tool_use/text/usage/round/progress/…）不进会话流。
func mirrorDecision(r db.Activity) (string, string, bool) {
	worker := strings.TrimSpace(r.Worker)
	switch r.Kind {
	case "result":
		if r.IsError {
			return "", "", false
		}
		body := truncateRunes(strings.TrimSpace(r.Summary), mirrorBodyMaxRunes)
		if body == "" {
			return "", "", false
		}
		return body, "result:" + worker, true
	case "tool_result":
		if r.Tool != "report_finding" || r.IsError {
			return "", "", false
		}
		body := strings.TrimSpace(r.Summary)
		if body == "" {
			return "", "", false
		}
		return "发现漏洞：" + truncateRunes(body, mirrorBodyMaxRunes), "finding:" + worker, true
	}
	return "", "", false
}

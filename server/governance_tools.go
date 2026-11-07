package server

// ---------- 治理工具（方案 v7：auto 监督环的执行面）----------
//
// 治理工具全部过确定性护栏（v7 §5 权限边界总表），护栏在引擎方法/本文件内，
// 工具层不做二次判断。注册进 orchestrationTools（默认绑 auto）；每次动作
// 走双留痕：llm_records（worker=governance，大屏/观测）+ 绑定会话播报。

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/internal/sod"
	actool "github.com/Autumn-27/norma/tool"
)

// govRecord 治理动作双留痕：llm_records 观测行 + 绑定会话播报。
func (s *Server) govRecord(taskID, action, summary string) {
	if s.m == nil || s.m.pg == nil {
		return
	}
	body, _ := json.Marshal(map[string]any{"action": action, "summary": summary})
	if err := s.m.pg.InsertLLMRecord(&db.LLMRecord{
		Worker: "governance", Model: "auto-supervisor", Status: "ok",
		RawResponse: string(body),
	}); err != nil {
		log.Printf("[governance] 留痕失败（忽略）: %v", err)
	}
	numeric, err := parseTaskID(taskID)
	if err != nil {
		return
	}
	if sess, serr := s.m.pg.SessionByTask(numeric); serr == nil && sess != nil {
		s.say(sess.ID, numeric, "system", "system", "【治理】"+summary)
	}
}

// governanceTools 是 v7 的 8 个治理工具（orchestrationTools 追加注册）。
func (s *Server) governanceTools() []actool.CoreTool {
	return []actool.CoreTool{
		s.toolSodAdjustCap(),
		s.toolIntentExtend(),
		s.toolIntentStop(),
		s.toolTaskDrain(),
		s.toolTaskConclude(),
		s.toolSessionClose(),
		s.toolRerunBlocked(),
	}
}

// toolSodAdjustCap 调整某类每任务配额（clamp [0,500]；locked 拒绝）。
func (s *Server) toolSodAdjustCap() actool.CoreTool {
	return wrTool("sod_adjust_cap", "调整某拦截种类的每任务配额(cap)。用于监督环：某类调用被 cap 频繁误伤时放宽，或失控时收紧。clamp [0,500]；锁定基线类不可调。",
		objSchema(map[string]any{
			"category": strParam("种类名（如 出网新主机/目录外写）"),
			"cap":      map[string]any{"type": "integer", "description": "新配额（0=不限）"},
			"reason":   strParam("调整理由（进审计与群播报）"),
		}, "category", "cap"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				Category string `json:"category"`
				Cap      int    `json:"cap"`
				Reason   string `json:"reason"`
			}
			_ = json.Unmarshal(in, &a)
			a.Category = trimGov(a.Category)
			if a.Category == "" {
				return actool.Errorf("category 必填"), nil
			}
			pg := s.m.pg
			rows, err := pg.ListSodCategories()
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			var target *db.SodCategoryRow
			for i := range rows {
				if rows[i].Name == a.Category {
					target = &rows[i]
					break
				}
			}
			if target == nil {
				return actool.Errorf("种类不存在: " + a.Category), nil
			}
			if target.Locked {
				return actool.Errorf("种类「" + target.Name + "」是锁定基线，不可调整"), nil
			}
			newCap := governanceCapClamp(a.Cap)
			if _, err := pg.UpdateSodCategory(target.ID, target.Action, newCap, target.Fallback, nil, nil, nil); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			summary := fmt.Sprintf("cap 调整：%s %d→%d（%s）", target.Name, target.CapPerTask, newCap, trimGov(a.Reason))
			s.govRecord("", "sod_adjust_cap", summary)
			return actool.Text(summary), nil
		})
}

// toolIntentExtend 给正要出成果的意图追加预算（上限 5 次，第 6 次转人工）。
func (s *Server) toolIntentExtend() actool.CoreTool {
	return wrTool("intent_extend", "给正在产出、即将被预算掐断的意图追加预算（软着陆延期，最多 5 次）。用于监督环：意图仍有价值时避免成果被截断。",
		objSchema(map[string]any{
			"task_id":   strParam("任务 id"),
			"intent_id": map[string]any{"type": "integer", "description": "意图 id"},
		}, "task_id", "intent_id"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				TaskID   string `json:"task_id"`
				IntentID int64  `json:"intent_id"`
			}
			_ = json.Unmarshal(in, &a)
			ok, msg := s.engine.ExtendIntent(a.TaskID, a.IntentID)
			if !ok {
				return actool.Errorf(msg), nil
			}
			s.govRecord(a.TaskID, "intent_extend", msg)
			return actool.Text(msg), nil
		})
}

// toolIntentStop 停掉跑偏的意图。
func (s *Server) toolIntentStop() actool.CoreTool {
	return wrTool("intent_stop", "停止一个跑偏/无前景的意图（转 exhausted 带理由）。用于监督环：方向明显走死时止损。",
		objSchema(map[string]any{
			"task_id":   strParam("任务 id"),
			"intent_id": map[string]any{"type": "integer", "description": "意图 id"},
			"reason":    strParam("停止理由（进审计）"),
		}, "task_id", "intent_id", "reason"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				TaskID   string `json:"task_id"`
				IntentID int64  `json:"intent_id"`
				Reason   string `json:"reason"`
			}
			_ = json.Unmarshal(in, &a)
			if trimGov(a.Reason) == "" {
				return actool.Errorf("reason 必填（审计要求）"), nil
			}
			ok, msg := s.engine.StopIntent(a.TaskID, a.IntentID, a.Reason)
			if !ok {
				return actool.Errorf(msg), nil
			}
			s.govRecord(a.TaskID, "intent_stop", msg)
			return actool.Text(msg), nil
		})
}

// toolTaskDrain 软截止（可逆）。
func (s *Server) toolTaskDrain() actool.CoreTool {
	return wrTool("task_drain", "任务软截止：停派新意图、在飞工作自然收尾（可逆，drain=false 恢复派发）。用于监督环：目标接近达成或预算将尽时优雅收口。",
		objSchema(map[string]any{
			"task_id": strParam("任务 id"),
			"drain":   map[string]any{"type": "boolean", "description": "true=进入软截止；false=恢复派发"},
			"reason":  strParam("理由"),
		}, "task_id", "drain"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				TaskID string `json:"task_id"`
				Drain  *bool  `json:"drain"`
				Reason string `json:"reason"`
			}
			_ = json.Unmarshal(in, &a)
			if a.Drain == nil {
				return actool.Errorf("drain 必填"), nil
			}
			s.engine.Drain(a.TaskID, *a.Drain)
			verb := "进入软截止"
			if !*a.Drain {
				verb = "恢复派发"
			}
			s.govRecord(a.TaskID, "task_drain", verb+"（"+trimGov(a.Reason)+"）")
			return actool.Text("task " + verb + ": " + a.TaskID), nil
		})
}

// toolTaskConclude 双签收尾。
func (s *Server) toolTaskConclude() actool.CoreTool {
	return wrTool("task_conclude", "收尾任务（判 done）。双签：三门满足（无 running/open 意图 + 有产出）时直接收；不满足则返回需人工确认（可发起人工 ask）。",
		objSchema(map[string]any{
			"task_id": strParam("任务 id"),
			"summary": strParam("收尾结论（进审计与播报）"),
			"ask_human": map[string]any{
				"type":        "boolean",
				"description": "三门未满足时是否发起人工确认（true=发 ask 等人批；false=仅返回失败原因）",
			},
		}, "task_id", "summary"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				TaskID   string `json:"task_id"`
				Summary  string `json:"summary"`
				AskHuman bool   `json:"ask_human"`
			}
			_ = json.Unmarshal(in, &a)
			ok, msg := s.engine.ConcludeTask(a.TaskID, a.Summary)
			if ok {
				s.govRecord(a.TaskID, "task_conclude", msg)
				return actool.Text(msg), nil
			}
			if a.AskHuman {
				approved, _, err := s.elevateAsk(ctx, sod.AskRequest{
					TaskID:         parseTaskIDSafe(a.TaskID),
					ToolName:       "task_conclude",
					Reason:         "监督者请求收尾任务但三门未满足：" + msg,
					PayloadPreview: trimGov(a.Summary),
				})
				if err == nil && approved {
					ok2, msg2 := s.engine.ConcludeTask(a.TaskID, a.Summary)
					if ok2 {
						s.govRecord(a.TaskID, "task_conclude", msg2+"（人工批准）")
						return actool.Text(msg2 + "（人工批准）"), nil
					}
					return actool.Errorf(msg2), nil
				}
				return actool.Errorf(msg + "；人工确认未通过"), nil
			}
			return actool.Errorf(msg), nil
		})
}

// toolSessionClose 结束会话（静音唤醒 + 归档标记）。
func (s *Server) toolSessionClose() actool.CoreTool {
	return wrTool("session_close", "结束一个会话：静音后续 agent 唤醒并归档标记（消息保留可查）。用于监督环：目标完成的群聊/总控会话收口。",
		objSchema(map[string]any{
			"session_id": strParam("会话 id"),
			"summary":    strParam("结束说明"),
		}, "session_id", "summary"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				SessionID string `json:"session_id"`
				Summary   string `json:"summary"`
			}
			_ = json.Unmarshal(in, &a)
			ok, msg := s.closeSessionGovernance(a.SessionID, a.Summary)
			if !ok {
				return actool.Errorf(msg), nil
			}
			return actool.Text(msg), nil
		})
}

// toolRerunBlocked 解堵（现有 API 语义工具化）。
func (s *Server) toolRerunBlocked() actool.CoreTool {
	return wrTool("rerun_blocked", "重跑某任务全部 blocked 意图（解堵）。用于监督环：卡点清除后恢复推进。",
		objSchema(map[string]any{"task_id": strParam("任务 id")}, "task_id"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				TaskID string `json:"task_id"`
			}
			_ = json.Unmarshal(in, &a)
			n, err := s.rerunBlockedIntents(a.TaskID)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			msg := fmt.Sprintf("已重开 %d 个 blocked 意图", n)
			s.govRecord(a.TaskID, "rerun_blocked", msg)
			return actool.Text(msg), nil
		})
}

// ---------- 会话归档（v7 session_close 的存储面）----------

// closeSessionGovernance 归档会话：archived_at 标记 + 唤醒静音 + 收尾播报。
func (s *Server) closeSessionGovernance(sessionID, summary string) (bool, string) {
	if s.m == nil || s.m.pg == nil {
		return false, "管理器未装配"
	}
	sid, err := parseTaskID(sessionID)
	if err != nil || sid <= 0 {
		return false, "session_id 不合法"
	}
	if _, err := s.m.pg.Exec(`UPDATE sessions SET archived_at=NOW() WHERE id=$1 AND archived_at IS NULL`, sid); err != nil {
		return false, err.Error()
	}
	msg := "【会话收口】" + trimGov(summary)
	s.say(sid, 0, "system", "system", msg)
	s.govRecord("", "session_close", fmt.Sprintf("session %d 归档：%s", sid, trimGov(summary)))
	return true, "会话已归档: " + sessionID
}

// rerunBlockedIntents 是 rerun-blocked API 的内部复用（工具与 HTTP 共用）。
func (s *Server) rerunBlockedIntents(taskID string) (int, error) {
	t, ok := s.m.Task(taskID)
	if !ok || t == nil || t.Store == nil {
		return 0, fmt.Errorf("task 不存在")
	}
	ids, err := t.Store.BlockedIntentIDs()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, id := range ids {
		if err := transitionIntentState(t.Store, id, "blocked", "open"); err == nil {
			n++
		}
	}
	return n, nil
}

// trimGov 小工具（parseTaskID 用 task_llm.go 的现成实现）。
func trimGov(s string) string {
	return strings.TrimSpace(s)
}

// governanceCapClamp 是 cap 调整的硬边界（v7 §5：clamp [0,500]）。
func governanceCapClamp(cap int) int {
	if cap < 0 {
		return 0
	}
	if cap > 500 {
		return 500
	}
	return cap
}

// parseTaskIDSafe 宽松解析（失败=0，调用方按 0 处理）。
func parseTaskIDSafe(s string) int64 {
	n, _ := parseTaskID(s)
	return n
}

package server

// 偏锋探索工具面（EDGE 合并施工 M4/M6）。
//
// 与 biz_tools.go 同一通道（server 侧参数化工具 + hostTools 装配 + tools 表绑定
// + roleToolAllow 双闸）。绑定矩阵（默认，首插生效；advisor 策略每启动强制对齐）：
//   edge_methodologies/edge_coverage → edge, edge_exec, auto, mainagent
//   edge_consult                     → edge, auto, mainagent
//   edge_run                         → edge, auto        （mainagent 默认不绑：
//                                      任务内角色不建新任务——职责边界见方案二
//                                      §1.5；需要时在工具页显式勾选）
//   edge_activate                    → mainagent
//   edge_dispatch/conclude/switch    → edge_exec
//
// 「识别者能力边界」的落点：auto（平台操作助手）能建新任务（edge_run）；
// mainagent（人机接口，任务内）只能切当前任务的打法（edge_activate）。
// 主 agent 绑了 edge_run 也不绕审批：建任务走 CreateTaskWithOptions 的同一条
// 宿主路径，guard ask 通道照常拦高危面。

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	actool "github.com/Autumn-27/norma/tool"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/internal/edge"
)

// ---------- 熔断状态（进程内计数 + scheduler_state 持久化口径）----------
//
// DegenerateTracker / StallTracker 是内存计数器；跨重启的连续性由 scheduler_state
// 的 JSON 键承载（fireEdgeWake 每轮读写）。重启清零的语义是「重启即人工已介入」，
// 与 role_policy lane 游标的 restart-reset 是同一取舍（文档如实声明）。

var (
	edgeBreakerMu sync.Mutex
	// degenerateTrackers[taskID] 连续假设退化计数（T1：连续 ≥3 条退化 ⇒ 熔断）。
	degenerateTrackers = map[int64]*edge.DegenerateTracker{}
	// cellDispatchTrackers 不需要：attempts 持久在 edge_cells.attempts。
)

// edgeBreakerState 是 scheduler_state 里持久化的熔断状态（键 edge_breaker:<taskID>）。
type edgeBreakerState struct {
	DegenerateConsecutive int    `json:"degenerate_consecutive"`
	StallLastHash         string `json:"stall_last_hash"`
	StallRounds           int    `json:"stall_rounds"`
}

func edgeBreakerKey(taskID int64) string  { return fmt.Sprintf("edge_breaker:%d", taskID) }
func edgeWakeKey(taskID int64) string     { return fmt.Sprintf("edge_wake:%d", taskID) }
func edgeWakeMinInterval() time.Duration  { return 60 * time.Second }

func (s *Server) loadEdgeBreaker(taskID int64) *edgeBreakerState {
	raw, _, _ := s.m.pg.GetSetting(edgeBreakerKey(taskID))
	st := &edgeBreakerState{}
	if raw != "" {
		_ = json.Unmarshal([]byte(raw), st)
	}
	return st
}

func (s *Server) saveEdgeBreaker(taskID int64, st *edgeBreakerState) {
	b, _ := json.Marshal(st)
	_ = s.m.pg.SetSetting(edgeBreakerKey(taskID), string(b))
}

// ---------- 工具全集与种子 ----------

// edgeTools 返回偏锋工具全集（进 hostTools 装配面）。
func (s *Server) edgeTools() []actool.CoreTool {
	return []actool.CoreTool{
		s.toolEdgeMethodologies(),
		s.toolEdgeCoverage(),
		s.toolEdgeConsult(),
		s.toolEdgeRun(),
		s.toolEdgeActivate(),
		s.toolEdgeDispatch(),
		s.toolEdgeConclude(),
		s.toolEdgeSwitch(),
	}
}

// seedEdgeTools 播种偏锋工具 + 默认绑定（首插生效）+ edge_exec 运行策略
//（serial+by_task+1：同一任务的偏锋轮串行、跨任务互不合并——与 bizmodeler 同款）。
func (s *Server) seedEdgeTools() {
	const refreshFlag = "edge_tools_seed_v1"
	bind := map[string][]string{
		"edge_methodologies": {"edge", "edge_exec", "auto", "mainagent"},
		"edge_coverage":      {"edge", "edge_exec", "auto", "mainagent"},
		"edge_consult":       {"edge", "auto", "mainagent"},
		"edge_run":           {"edge", "auto"},
		"edge_activate":      {"mainagent"},
		"edge_dispatch":      {"edge_exec"},
		"edge_conclude":      {"edge_exec"},
		"edge_switch":        {"edge_exec"},
	}
	for _, t := range s.edgeTools() {
		schema, _ := json.Marshal(t.InputSchema())
		agents, _ := json.Marshal(bind[t.Name()])
		_ = s.m.PG().SeedTool(t.Name(), t.Description(), schema, agents)
	}
	if v, _, _ := s.m.pg.GetSetting(refreshFlag); v != "true" {
		_ = s.m.pg.SetSetting(refreshFlag, "true")
	}
	// edge_exec 的触发运行策略（agents 表列，builtin 行可直接写——triggers.go 的
	// 限制只管 HTTP 建触发器，不管运行策略）：serial + by_task + 1。
	if err := s.m.pg.SetAgentTriggerBehavior("edge_exec", "serial", "by_task", 1); err != nil {
		log.Printf("[edge] edge_exec 运行策略设置失败: %v", err)
	}
	agent.InvalidateAssembly()
}

// ---------- 只读组 ----------

// toolEdgeMethodologies 列出方法论注册表。
func (s *Server) toolEdgeMethodologies() actool.CoreTool {
	return roTool("edge_methodologies",
		"列出偏锋探索的方法论注册表：每条的判据类型（覆盖/收敛/穷举/耗尽/衰减）、上限参数、证据等级、启用状态与优先级。三级隐喻条目没有判据——启发式给方向，判据给终止条件，两者来源分开。",
		objSchema(map[string]any{}),
		func(context.Context, json.RawMessage) (actool.Result, error) {
			all := edge.ListAll()
			out := make([]map[string]any, 0, len(all))
			for _, m := range all {
				row := map[string]any{
					"id": m.ID, "name": m.Name, "tier": m.Tier,
					"evidence_grade": string(m.EvidenceGrade), "summary": m.Summary,
					"enabled": m.Enabled, "priority": m.Priority,
					"heuristic_tags": m.HeuristicTags,
				}
				if m.Criterion != nil {
					row["criterion"] = m.Criterion
				}
				out = append(out, row)
			}
			return jsonResult(map[string]any{"methodologies": out})
		})
}

// toolEdgeCoverage 查任务的格点覆盖与判据进度。
func (s *Server) toolEdgeCoverage() actool.CoreTool {
	return roTool("edge_coverage",
		"查指定任务的偏锋格点覆盖与判据进度：格点状态（open/dispatched/confirmed/disproven/folded）、尝试数、判据状态 JSON、未收尾运行的判据轨迹。终态格点数 / 总格点数就是判据完成度的机械口径。",
		objSchema(map[string]any{"task_id": map[string]any{"type": "integer", "description": "任务 id（必填）"}}, "task_id"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				TaskID int64 `json:"task_id"`
			}
			_ = json.Unmarshal(in, &a)
			cells, err := s.m.pg.ListEdgeCells(a.TaskID)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			run, err := s.m.pg.OpenEdgeRun(a.TaskID)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			edgeMode, methID, _ := s.m.pg.TaskEdgeFields(a.TaskID)
			terminal, dispatched := 0, 0
			for _, c := range cells {
				if c.State == db.EdgeCellConfirmed || c.State == db.EdgeCellDisproven || c.State == db.EdgeCellFolded {
					terminal++
				}
				if c.State == db.EdgeCellDispatched {
					dispatched++
				}
			}
			total := len(cells)
			progress := 0.0
			if total > 0 {
				progress = float64(terminal) / float64(total)
			}
			return jsonResult(map[string]any{
				"task_id": a.TaskID, "edge_mode": edgeMode, "methodology_id": methID,
				"cells_total": total, "cells_terminal": terminal, "cells_dispatched": dispatched,
				"criterion_progress": progress, "cells": cells,
				"open_run": run,
			})
		})
}

// ---------- 咨询与发起 ----------

// toolEdgeConsult 轻量咨询（留痕，不建任务不落图）。
func (s *Server) toolEdgeConsult() actool.CoreTool {
	return wrTool("edge_consult",
		"轻量偏锋咨询：返回启用方法论清单（判据+上限参数）与指定任务的覆盖缺口（未开格点），你在回复里据此给出方法论建议与候选假设。不建任务、不写探索图——候选假设被用户认可后再用 edge_run（独立任务）或 edge_activate（当前任务）升级。每次调用留痕。",
		objSchema(map[string]any{
			"task_id": map[string]any{"type": "integer", "description": "目标任务 id（可选；传了附带覆盖缺口分析）"},
			"focus":   strParam("关注面一句话（可选，如「登录与权限」）——写进留痕"),
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				TaskID int64  `json:"task_id"`
				Focus  string `json:"focus"`
			}
			_ = json.Unmarshal(in, &a)
			out := map[string]any{"methodologies": edge.ListEnabled()}
			if a.TaskID > 0 {
				cells, err := s.m.pg.ListEdgeCells(a.TaskID)
				if err != nil {
					return actool.Errorf(err.Error()), nil
				}
				openCells := []string{}
				for _, c := range cells {
					if c.State == db.EdgeCellOpen {
						openCells = append(openCells, c.CellID)
					}
				}
				out["task_id"] = a.TaskID
				out["open_cells"] = openCells
				out["cells_total"] = len(cells)
			}
			s.appendEdgeAudit("edge-consult", "edge_consult",
				fmt.Sprintf("task=%d focus=%s", a.TaskID, a.Focus),
				map[string]any{"task_id": a.TaskID, "focus": a.Focus})
			return jsonResult(out)
		})
}

// toolEdgeRun 发起一次完整偏锋任务（auto/edge 的路径）。
func (s *Server) toolEdgeRun() actool.CoreTool {
	return wrTool("edge_run",
		"发起一次完整偏锋探索：建独立任务（类别=偏锋探索、不限时）并启动「偏锋探索·执行」做总控——它查判据进度、下发偏锋假设、判完成。无上限模式取消的是探索量上限；scope 边界、职责分离、审批、流量录制、审计链一条都不动。methodology_id 从 edge_methodologies 里选（不传用优先级最高的启用条目）。",
		objSchema(map[string]any{
			"goal":           strParam("任务目标一句话（必填，如「对 example.com 的登录与权限面做偏锋探索」）"),
			"description":    strParam("任务描述/范围说明（必填）"),
			"methodology_id": strParam("方法论 id（可选；不传用轮换表首位）"),
			"company_ids":    map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "公司范围 id（可选，绑定资产范围）"},
			"source_task_ids": map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "只读继承的来源任务 id（可选）"},
		}, "goal", "description"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				Goal           string   `json:"goal"`
				Description    string   `json:"description"`
				MethodologyID  string   `json:"methodology_id"`
				CompanyIDs     []int64  `json:"company_ids"`
				SourceTaskIDs  []int64  `json:"source_task_ids"`
			}
			_ = json.Unmarshal(in, &a)
			// 方法论校验：必须已注册且启用（agent 不得自选未启用的条目）。
			if a.MethodologyID != "" {
				if m, ok := edge.Get(a.MethodologyID); !ok || !m.Enabled {
					return actool.Errorf(fmt.Sprintf("方法论 %q 未注册或未启用（edge_methodologies 查清单）", a.MethodologyID)), nil
				}
			} else if enabled := edge.ListEnabled(); len(enabled) > 0 {
				a.MethodologyID = enabled[0].ID
			}
			category, err := s.m.pg.TaskCategoryByName(db.TaskCategoryEdgeExploration)
			if err != nil || category == nil {
				return actool.Errorf("「偏锋探索」任务类别不存在（种子缺失）"), nil
			}
			t, err := s.m.CreateTaskWithOptions(a.Description, a.Goal, db.TaskCreateOptions{
				CategoryID:    &category.ID,
				CompanyIDs:    a.CompanyIDs,
				SourceTaskIDs: a.SourceTaskIDs,
				// timeout 0 = 不限时（无预算上限：任务层不设墙钟；终止靠判据+熔断+人设兜底）。
				TimeoutSeconds: 0,
			})
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			taskID, _ := strconv.ParseInt(t.ID, 10, 64)
			if taskID <= 0 {
				return actool.Errorf("task id 解析失败"), nil
			}
			// 任务级偏锋标记 + 开运行行（判据轨迹的挂点）。
			if err := s.m.pg.SetTaskEdgeMode(taskID, true, a.MethodologyID); err != nil {
				log.Printf("[edge] task %d 写 edge_mode 失败: %v", taskID, err)
			}
			runID, err := s.m.pg.StartEdgeRun(taskID, a.MethodologyID)
			if err != nil {
				log.Printf("[edge] task %d 开运行失败: %v", taskID, err)
			}
			// 会话里发起的 edge_run：绑回发起会话并播报（与 spawn_task 同款 UX）。
			if sessID, convID := s.bindSpawnedTaskToSession(context.Background(), taskID); sessID > 0 {
				s.announceSpawnedTask(sessID, taskID, convID, a.Description, a.Goal)
			}
			// 共享建后流程：seed + 目标分解 + engine.Run（与 spawn_task/HTTP 建任务同一条）。
			s.launchTask(t, a.Description+" "+a.Goal, false)
			// 拉起执行体：per-agent FIFO 队列（serial+by_task），首轮总控。
			s.StartTriggeredRun("edge_exec", "偏锋探索启动",
				fmt.Sprintf("【偏锋探索】任务 #%d 已启动，方法论=%s。请开始总控：读 edge_coverage 建立格点坐标系，下发第一批偏锋假设（edge_dispatch），并持续对账判据进度。", taskID, a.MethodologyID),
				taskID, true, a.Description, a.Goal)
			s.appendEdgeAudit("edge-run", "edge_run",
				fmt.Sprintf("task=%d methodology=%s run=%d", taskID, a.MethodologyID, runID),
				map[string]any{"task_id": taskID, "methodology_id": a.MethodologyID, "run_id": runID})
			return actool.Text(fmt.Sprintf("偏锋任务 #%d 已创建（方法论=%s，运行 #%d），执行体已排队。", taskID, a.MethodologyID, runID)), nil
		})
}

// toolEdgeActivate 任务内热切偏锋模式（mainagent 的路径）。
func (s *Server) toolEdgeActivate() actool.CoreTool {
	return wrTool("edge_activate",
		"把当前任务切到偏锋模式：任务类别热改为「偏锋探索」（worker 下一条意图即带偏锋纪律段，无需重建 agent bundle），并可选指定方法论与建立格点。已有的预算、格点、在飞意图都还在——偏锋只是给 planner 换一套选格点策略与判据。",
		objSchema(map[string]any{
			"task_id":        map[string]any{"type": "integer", "description": "任务 id（必填；须是运行中或待运行的任务）"},
			"methodology_id": strParam("方法论 id（可选；不传保持/用轮换表首位）"),
			"cells": map[string]any{"type": "array", "items": map[string]any{"type": "string"},
				"description": "要初始化的格点 id 清单（可选，如 [\"auth:uca-1\",\"auth:uca-2\"]；不传则只切模式不建格）"},
		}, "task_id"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				TaskID        int64    `json:"task_id"`
				MethodologyID string   `json:"methodology_id"`
				Cells         []string `json:"cells"`
			}
			_ = json.Unmarshal(in, &a)
			t, ok := s.m.Task(strconv.FormatInt(a.TaskID, 10))
			if !ok {
				return actool.Errorf("task 不存在: " + strconv.FormatInt(a.TaskID, 10)), nil
			}
			// 方法论解析：显式 > 任务存量 > 轮换表首位。
			methID := a.MethodologyID
			if methID == "" {
				if _, cur, ok := s.m.pg.TaskEdgeFields(a.TaskID); ok && cur != "" {
					methID = cur
				}
			}
			if methID != "" {
				if m, ok := edge.Get(methID); !ok || !m.Enabled {
					return actool.Errorf(fmt.Sprintf("方法论 %q 未注册或未启用", methID)), nil
				}
			} else if enabled := edge.ListEnabled(); len(enabled) > 0 {
				methID = enabled[0].ID
			}
			// ① 热改类别（SetTaskCategory 持 taskStateMu 刷内存态，下一轮即生效）。
			category, err := s.m.pg.TaskCategoryByName(db.TaskCategoryEdgeExploration)
			if err != nil || category == nil {
				return actool.Errorf("「偏锋探索」任务类别不存在"), nil
			}
			cid := category.ID
			if _, err := s.m.SetTaskCategory(strconv.FormatInt(a.TaskID, 10), &cid); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			// ② 写 edge 字段（与类别互为兜底：单列损坏另一列仍表达模式）。
			if err := s.m.pg.SetTaskEdgeMode(a.TaskID, true, methID); err != nil {
				log.Printf("[edge] task %d 写 edge_mode 失败: %v", a.TaskID, err)
			}
			// ③ 初始化格点（幂等）+ 开运行行（任务内模式也记判据轨迹）。
			for _, c := range a.Cells {
				_ = s.m.pg.UpsertEdgeCell(a.TaskID, c, methID, map[string]any{})
			}
			if run, _ := s.m.pg.OpenEdgeRun(a.TaskID); run == nil {
				if _, err := s.m.pg.StartEdgeRun(a.TaskID, methID); err != nil {
					log.Printf("[edge] task %d 开运行失败: %v", a.TaskID, err)
				}
			}
			s.appendEdgeAudit("edge-run", "edge_activate",
				fmt.Sprintf("task=%d methodology=%s cells=%d", a.TaskID, methID, len(a.Cells)),
				map[string]any{"task_id": a.TaskID, "methodology_id": methID, "cells": a.Cells})
			_ = t
			return actool.Text(fmt.Sprintf("任务 #%d 已切偏锋模式（类别=偏锋探索，方法论=%s，格点 %d 个）。worker 下一条意图即带偏锋纪律段。", a.TaskID, methID, len(a.Cells))), nil
		})
}

// ---------- 执行体（edge_exec）工具 ----------

// toolEdgeDispatch 下发一条偏锋假设到指定格点（先过溯因门禁）。
func (s *Server) toolEdgeDispatch() actool.CoreTool {
	return wrTool("edge_dispatch",
		"下发一条偏锋假设：先过溯因门禁（确定性判定，零 LLM）——observation_basis 为空、引用已证伪事实、prediction 与 falsifier 不可判定，都算退化；连续 3 条退化会触发假设退化熔断（运行被终止，转人工确认）。通过后假设落成探索图意图（带可证伪预测，created_by=edge），格点尝试数 +1；同格点满 2 次强制封格（cell_exhausted）。",
		objSchema(map[string]any{
			"task_id":            map[string]any{"type": "integer", "description": "任务 id（必填）"},
			"cell_id":            strParam("格点 id（必填，须已初始化）"),
			"summary":            strParam("假设一句话：探索什么+为什么（必填）"),
			"observation_basis":  strParam("观察依据：来自哪些已有事实/输出（必填；空=自提自证，直接判退化）"),
			"predicted_vulnclass": strParam("预测的漏洞类别（必填，可证伪）"),
			"prediction":         strParam("可证伪预测：期待观察到什么（必填）"),
			"falsifier":          strParam("证伪条件：看到什么算这条假设被反驳（必填）"),
			"asset_ids":          map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "目标资产 id"},
			"priority":           map[string]any{"type": "integer", "description": "优先级 0-10，默认 6（偏锋假设默认略高于常规）"},
		}, "task_id", "cell_id", "summary", "observation_basis", "predicted_vulnclass", "prediction", "falsifier"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				TaskID             int64   `json:"task_id"`
				CellID             string  `json:"cell_id"`
				Summary            string  `json:"summary"`
				ObservationBasis   string  `json:"observation_basis"`
				PredictedVulnclass string  `json:"predicted_vulnclass"`
				Prediction         string  `json:"prediction"`
				Falsifier          string  `json:"falsifier"`
				AssetIDs           []int64 `json:"asset_ids"`
				Priority           int     `json:"priority"`
			}
			_ = json.Unmarshal(in, &a)
			t, ok := s.m.Task(strconv.FormatInt(a.TaskID, 10))
			if !ok {
				return actool.Errorf("task 不存在"), nil
			}
			store := t.Store
			if store == nil {
				return actool.Errorf("任务没有探索图"), nil
			}
			// ① 溯因门禁（确定性，零 LLM）。
			refuted, _ := s.m.pg.RefutedFactSummaries(50)
			if edge.JudgeAbduction(a.ObservationBasis, a.Prediction, a.Falsifier, refuted) == edge.AbductionDegenerate {
				edgeBreakerMu.Lock()
				tr := degenerateTrackers[a.TaskID]
				if tr == nil {
					tr = &edge.DegenerateTracker{}
					degenerateTrackers[a.TaskID] = tr
				}
				n := tr.Record(edge.AbductionDegenerate)
				tripped := tr.Tripped()
				edgeBreakerMu.Unlock()
				if tripped {
					// 熔断①：终止运行，转人工确认分支。
					if run, _ := s.m.pg.OpenEdgeRun(a.TaskID); run != nil {
						_ = s.m.pg.ConcludeEdgeRun(run.ID, db.StopReasonDegenerate, map[string]any{
							"reason": "连续 3 条假设未通过溯因门禁（basis 空/撞证伪池/不可判定）",
						})
					}
					s.appendEdgeAudit("edge-circuit-breaker", "edge_dispatch",
						fmt.Sprintf("task=%d 熔断 degenerate_hypothesis", a.TaskID),
						map[string]any{"task_id": a.TaskID, "breaker": "degenerate_hypothesis"})
					return actool.Text("【假设退化熔断】连续 3 条假设未通过溯因门禁，本运行已终止（stop_reason=degenerate_hypothesis）。请人工评估方法论适配度后决定是否换方法论重启。"), nil
				}
				return actool.Errorf(fmt.Sprintf("假设未通过溯因门禁（第 %d/%d 条退化）：observation_basis 不能为空、prediction/falsifier 必须可判定、不得撞证伪池。补齐依据后重发。", n, edge.DegenerateThreshold)), nil
			}
			edgeBreakerMu.Lock()
			if tr := degenerateTrackers[a.TaskID]; tr != nil {
				tr.Record(edge.AbductionOK)
			}
			edgeBreakerMu.Unlock()
			// ② 去重（与 planner 同源闸门）。
			if id, dup, err := store.FindOpenIntentBySummary(a.Summary); err == nil && dup {
				return actool.Text(fmt.Sprintf("已有同摘要 open 意图 #%d，本格点改判 disproven（该方向已在跑）。", id)), nil
			}
			// ③ 格点记账先行：尝试数 +1；达上限即封格并拒绝继续下发。
			attempts, err := s.m.pg.RecordEdgeDispatch(a.TaskID, a.CellID)
			if err != nil {
				return actool.Errorf(fmt.Sprintf("格点 %q 不存在（先用 edge_activate 的 cells 参数初始化坐标系）: %v", a.CellID, err)), nil
			}
			if attempts > db.EdgeCellMaxAttempts {
				_ = s.m.pg.FoldEdgeCell(a.TaskID, a.CellID, db.EdgeCellDisproven)
				return actool.Text(fmt.Sprintf("格点 %q 已封（尝试 %d/%d 次未确证 → disproven）。换下一个格点或换方法论。", a.CellID, attempts, db.EdgeCellMaxAttempts)), nil
			}
			// ④ 意图落图：带可证伪预测 + created_by=edge（范围/拦截/parent 校验全继承基座写路径）。
			payload := map[string]any{
				"summary":              a.Summary,
				"predicted_vulnclass":  strings.TrimSpace(a.PredictedVulnclass),
				"predicted_actions":    []string{a.Prediction},
				"kind":                 db.IntentKindVerify,
				"edge_cell":            a.CellID,
				"edge_falsifier":       a.Falsifier,
				"proposed_by":          "edge",
			}
			anchors := a.AssetIDs
			if anchors == nil {
				anchors = []int64{}
			}
			id, err := store.AddIntentInScopeWithCreator(a.TaskID, payload, a.Priority, anchors, "member", "edge")
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			return actool.Text(fmt.Sprintf("假设已下发为 intent #%d（格点 %s，尝试 %d/%d）。worker 领走后判据进度随其终态更新。", id, a.CellID, attempts, db.EdgeCellMaxAttempts)), nil
		})
}

// toolEdgeConclude 判据收尾（四门收尾的第四门）。
func (s *Server) toolEdgeConclude() actool.CoreTool {
	return wrTool("edge_conclude",
		"偏锋收尾判定：四门 = 底座三门（无在飞意图/有产出/流程允许）+ 方法论判据成立（全部格点到达终态）。四门全过才真正收尾（stop_reason=doctrine_complete）；判据未成立时返回未终态格点清单——那是正常路径（等熔断或人工判断），不是错误。兜底熔断（退化/停滞/封顶）触发后的收尾也走本工具，stop_reason 用对应熔断值。",
		objSchema(map[string]any{
			"task_id":    map[string]any{"type": "integer", "description": "任务 id（必填）"},
			"summary":    strParam("收尾总结（必填）"),
			"stop_reason": strParam("强制指定终止原因（可选；兜底熔断路径用，如 degenerate_hypothesis/criterion_stalled）"),
		}, "task_id", "summary"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				TaskID    int64  `json:"task_id"`
				Summary   string `json:"summary"`
				StopReason string `json:"stop_reason"`
			}
			_ = json.Unmarshal(in, &a)
			run, err := s.m.pg.OpenEdgeRun(a.TaskID)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			if run == nil {
				return actool.Text("任务没有未收尾的偏锋运行（已收过或从未开启）。"), nil
			}
			// 兜底熔断路径：调度器/上游已判定，直接落账并走任务收尾。
			if a.StopReason != "" {
				if err := s.m.pg.ConcludeEdgeRun(run.ID, a.StopReason, map[string]any{"summary": a.Summary}); err != nil {
					return actool.Errorf(err.Error()), nil
				}
				if ok, msg := s.engine.ConcludeTask(strconv.FormatInt(a.TaskID, 10), a.Summary); !ok {
					return actool.Text(fmt.Sprintf("stop_reason=%s 已落账；任务收尾被底座三门拦下（%s）——用 ask 通道请人工确认。", a.StopReason, msg)), nil
				}
				s.appendEdgeAudit("edge-circuit-breaker", "edge_conclude",
					fmt.Sprintf("task=%d stop=%s", a.TaskID, a.StopReason),
					map[string]any{"task_id": a.TaskID, "stop_reason": a.StopReason})
				return actool.Text(fmt.Sprintf("运行 #%d 已按 %s 收尾。", run.ID, a.StopReason)), nil
			}
			// 主路径：判据机械判定——全部格点到达终态且至少一个格点。
			cells, _ := s.m.pg.ListEdgeCells(a.TaskID)
			undone := []string{}
			for _, c := range cells {
				if c.State != db.EdgeCellConfirmed && c.State != db.EdgeCellDisproven && c.State != db.EdgeCellFolded {
					undone = append(undone, fmt.Sprintf("%s(%s,尝试%d)", c.CellID, c.State, c.Attempts))
				}
			}
			if len(cells) == 0 || len(undone) > 0 {
				return actool.Text(fmt.Sprintf("第四门未过：判据未成立（%d/%d 格点终态；未终态：%s）。继续下发假设或换方法论；确要提前收尾请人工确认（stop_reason 会如实记 operator_guard）。",
					len(cells)-len(undone), len(cells), strings.Join(undone, "、"))), nil
			}
			if err := s.m.pg.ConcludeEdgeRun(run.ID, db.StopReasonDoctrine, map[string]any{"summary": a.Summary}); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			if ok, msg := s.engine.ConcludeTask(strconv.FormatInt(a.TaskID, 10), a.Summary); !ok {
				return actool.Text(fmt.Sprintf("判据成立（stop_reason=doctrine_complete 已落账）；任务收尾被底座三门拦下（%s）——可发起人工 ask 确认。", msg)), nil
			}
			s.appendEdgeAudit("edge-run", "edge_conclude",
				fmt.Sprintf("task=%d doctrine_complete", a.TaskID),
				map[string]any{"task_id": a.TaskID, "stop_reason": db.StopReasonDoctrine})
			return actool.Text(fmt.Sprintf("运行 #%d 判据成立，任务已收尾（doctrine_complete）。", run.ID)), nil
		})
}

// toolEdgeSwitch 换方法论 / 封格（熔断动作）。
func (s *Server) toolEdgeSwitch() actool.CoreTool {
	return wrTool("edge_switch",
		"换方法论或封格：methodology_id 换当前任务生效的方法论（判据切换，格点集合不变）；fold_cells 强制封格指定格点（熔断动作，state=folded）。切换动作留痕进运行轨迹。",
		objSchema(map[string]any{
			"task_id":        map[string]any{"type": "integer", "description": "任务 id（必填）"},
			"methodology_id": strParam("新方法论 id（可选）"),
			"fold_cells":     map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "要强制封格的格点 id 清单（可选）"},
			"reason":         strParam("切换/封格理由"),
		}, "task_id"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				TaskID        int64    `json:"task_id"`
				MethodologyID string   `json:"methodology_id"`
				FoldCells     []string `json:"fold_cells"`
				Reason        string   `json:"reason"`
			}
			_ = json.Unmarshal(in, &a)
			if a.MethodologyID != "" {
				if m, ok := edge.Get(a.MethodologyID); !ok || !m.Enabled {
					return actool.Errorf(fmt.Sprintf("方法论 %q 未注册或未启用", a.MethodologyID)), nil
				}
				if err := s.m.pg.SetTaskEdgeMode(a.TaskID, true, a.MethodologyID); err != nil {
					return actool.Errorf(err.Error()), nil
				}
			}
			folded := 0
			for _, c := range a.FoldCells {
				if err := s.m.pg.FoldEdgeCell(a.TaskID, c, db.EdgeCellFolded); err == nil {
					folded++
				}
			}
			if run, _ := s.m.pg.OpenEdgeRun(a.TaskID); run != nil {
				_ = s.m.pg.AppendEdgeCriterionTrace(run.ID, map[string]any{
					"ts": time.Now().Unix(), "event": "switch",
					"methodology": a.MethodologyID, "folded": a.FoldCells, "reason": a.Reason,
				})
			}
			s.appendEdgeAudit("edge-run", "edge_switch",
				fmt.Sprintf("task=%d methodology=%s folded=%d", a.TaskID, a.MethodologyID, folded),
				map[string]any{"task_id": a.TaskID, "methodology_id": a.MethodologyID, "fold_cells": a.FoldCells})
			return actool.Text(fmt.Sprintf("切换完成（方法论=%s，封格 %d 个）。", a.MethodologyID, folded)), nil
		})
}

// appendEdgeAudit 是偏锋侧动作的审计链双写（与 appendBizAudit 同款纪律）。
func (s *Server) appendEdgeAudit(source, action, reason string, body map[string]any) {
	b, _ := json.Marshal(body)
	if _, err := s.m.pg.AppendAuditEvent(db.AuditEvent{
		Actor:    "edge",
		ToolName: "edge",
		Action:   action,
		Reason:   reason,
		Payload:  b,
		Source:   source,
	}); err != nil {
		log.Printf("[edge] 审计链写入失败（忽略）: %v", err)
	}
}

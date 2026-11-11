package server

// ---------- F4 空缺驱动唤醒 + F5 停止消账（方案 v10 §12 / §16） ----------
//
// F4（写侧）：openGap 是空缺的唯一入口——去重入库（同 task+kind+detail 的 open
// 行只留一条，库级 uq_gaps_open 兜并发）+ 成功插入即触发一次定向唤醒。三个 gap
// 源的接线：
//   ① B6 升级处（denial_escalation.go，意图转 blocked 时）kind=action；
//   ② F2 的 evidence 矛盾（contradiction.go 的 routeContradiction）kind=evidence；
//   ③ 授权类拒绝（denial_escalation.go，reason_code=platform_baseline/destructive）
//      kind=authorization。
//
// 【硬停止：authorization 类 gap 不触发唤醒】（方案 §16「授权空缺必须停」）：
// 授权空缺 = 需要【人】给范围/给额度。自动唤醒产意图等于让 agent 自己给自己
// 扩权——那不是补空缺，是绕过授权边界。authorization 类只入账（治理页可见），
// 闭合只能走人工裁决（strategyProposalDecide 的 approved 分支，F5）或人工在
// 范围/配置上把堵点清掉。这是本文件唯一一处按 kind 改变行为的分支，不许加
// 配置项、不许进 settings（安全边界是代码常量，与 guard/shortcircuit.go 同理）。
//
// F5（消账侧）：意图落 done 且预测兑现 ⇒ CloseGap（evidence 类）；矛盾对应的
// proposal 被人工批准 ⇒ MarkContradictionResolved + CloseGap（挂载点在
// strategy_api.go 的 settleContradictionProposal）。嵌套 loop 不另起独立调度器：
// 唤醒复用既有的 StartTriggeredRun 队列（mergeable=true，同任务突发合并进一个
// 会话）与任务心跳/debounce，注释即文档（方案已定，F 线不做）。
//
// 唤醒计次：M2 的 wakeStat 打点在 planner runRound 入口已就位，唤醒引发的新
// 规划轮自动计入，这里不打第二遍。

import (
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"

	"github.com/Autumn-27/artex/db"
)

// gapWakePrompt 是定向唤醒的注入正文：告诉监督者哪个空缺开了，让它产出针对
// 该空缺的意图（add_task_hint / 治理工具）或收口判断（task_drain/task_conclude）。
func gapWakePrompt(kind, detail string) string {
	return fmt.Sprintf("【空缺驱动唤醒】空缺【%s】：%s——请产出针对该空缺的意图或收口判断。", kindLabel(kind), detail) +
		"\n（这是空缺账本的一次定向唤醒：补得上就产意图，补不上/该收口就明确收口判断，不要空转。）"
}

// openGap 开一条空缺（F4 唯一入口）：InsertGap 去重入库；成功插入（新 open 行）
// 且 kind != authorization ⇒ 经 StartTriggeredRun 触发一次定向唤醒（supervision
// agent，mergeable=true——同任务的多条空缺唤醒合并进一个会话，不逐条刷屏）。
//
// 去重行（inserted=false）不唤醒：同一个空缺反复入账不反复叫人。
// 任何失败只记日志（空缺是观测/驱动面，不是安全边界，不阻塞调用方主路径）。
func (s *Server) openGap(taskID int64, kind, detail string) {
	if s == nil || s.m == nil || s.m.pg == nil || taskID <= 0 {
		return
	}
	detail = strings.TrimSpace(detail)
	if detail == "" {
		return // 没有内容的空缺不可行动，也不可读——不落账
	}
	id, inserted, err := s.m.pg.InsertGap(taskID, kind, detail)
	if err != nil {
		log.Printf("[gap] task %d 空缺 %s 入库失败: %v", taskID, kind, err)
		return
	}
	if !inserted {
		return // 去重命中（open 行已存在）：不重复唤醒
	}
	log.Printf("[gap] task %d 开空缺 #%d【%s】：%s", taskID, id, kind, trunc(detail, 120))
	if kind == db.GapAuthorization {
		// 硬停止：授权空缺必须停（见文件头注释）——只入账，不唤醒。
		return
	}
	if s.chatAgentRef() == nil {
		return
	}
	// 定向唤醒：带任务上下文头（desc/goal），监督者据此可直接行动。
	desc, goal := "", ""
	if t, ok := s.m.Task(strconv.FormatInt(taskID, 10)); ok && t != nil {
		desc, goal = t.Description, t.Goal
	}
	s.StartTriggeredRun(supervisionAgent,
		fmt.Sprintf("空缺唤醒 · task#%d · %s", taskID, kind),
		gapWakePrompt(kind, detail),
		taskID, true, desc, goal)
}

// settleGapOnIntentDone 是 F5 的意图侧消账：意图落 done 且预测兑现（预测的
// vulnclass 在本意图 yields 的 findings 里）⇒ 闭合本任务 evidence 类空缺。
//
// best-effort 简化（方案明示允许）：意图与 gap 之间没有外键级的精确关联，闭合
// 按「本任务该 kind 最新 open gap」走（CloseLatestOpenGap），并在 detail 注明
// 「由意图 #N 兑现闭合」。blocked/permission 类空缺的闭合在人工裁决通道
// （settleContradictionProposal），不在这里。
//
// 非 done 终态（blocked/exhausted）不消账：空缺还开着——那正是它该等的地方。
func (e *Engine) settleGapOnIntentDone(t *Task, intentID int64, state string) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[gap] task %s 意图 #%d 消账 panic（忽略）: %v", taskIDForLog(t), intentID, r)
		}
	}()
	if state != "done" || e == nil || e.m == nil || e.m.pg == nil || t == nil || t.Store == nil {
		return
	}
	taskID, err := strconv.ParseInt(t.ID, 10, 64)
	if err != nil || taskID <= 0 {
		return
	}
	pred, err := e.m.pg.GetIntentPrediction(intentID)
	if err != nil {
		log.Printf("[gap] task %s 意图 #%d 消账读预测失败（跳过）: %v", t.ID, intentID, err)
		return
	}
	if pred == nil {
		return // 无预测的意图不参与消账（没有兑现可谈）
	}
	predicted := strings.ToLower(strings.TrimSpace(pred.PredictedVulnclass))
	if predicted == "" {
		return
	}
	findings, err := e.m.pg.YieldedFindingVulnclasses(t.ExpID, intentID)
	if err != nil {
		log.Printf("[gap] task %s 意图 #%d 消账读产出失败（跳过）: %v", t.ID, intentID, err)
		return
	}
	fulfilled := false
	for _, f := range findings {
		if strings.ToLower(strings.TrimSpace(f.Vulnclass)) == predicted {
			fulfilled = true
			break
		}
	}
	if !fulfilled {
		return // 预测未兑现：矛盾已由 F2 记账，空缺保持 open（别把没兑现的也销了）
	}
	closed, err := e.m.pg.CloseLatestOpenGap(taskID, db.GapEvidence, intentID,
		fmt.Sprintf("——由意图 #%d 兑现闭合（预测 %s 已在实际产出中命中；best-effort 消账：闭合本任务该 kind 最新 open gap）", intentID, predicted))
	if err != nil {
		log.Printf("[gap] task %s 意图 #%d 消账失败: %v", t.ID, intentID, err)
		return
	}
	if closed {
		log.Printf("[gap] task %s 意图 #%d 兑现（%s），已闭合一条 evidence 空缺", t.ID, intentID, predicted)
	}
}

// settleContradictionProposal 是 F5 的人工裁决侧消账，挂在
// strategyProposalDecide 的 approved 分支：奇点循环矛盾沉淀的 proposal
// （created_by="contradiction"，F3 建的那条）被批准 ⇒
// MarkContradictionResolved + CloseGap。
//
// 识别口径：proposal 的 created_by 前缀 "contradiction"（route 是固定枚举
// create_new，区分不出来源；created_by 是 F3 写下的唯一标记）。payload 里带
// contradiction_id/task_id（F3 写入），据此精确找回矛盾行与任务。
//
// best-effort：任何失败只记日志，不影响裁决本身的返回（裁决已落库+进审计链，
// 消账是其后置动作）。rejected 不消账：矛盾仍未消解，等下一轮许愿。
func (s *Server) settleContradictionProposal(pid int64, decideReason string) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[contradiction] proposal #%d 消账 panic（忽略）: %v", pid, r)
		}
	}()
	if s == nil || s.m == nil || s.m.pg == nil || pid <= 0 {
		return
	}
	pg := s.m.pg
	p, err := pg.GetStrategyProposal(pid)
	if err != nil || p == nil {
		if err != nil {
			log.Printf("[contradiction] proposal #%d 消账读提议失败: %v", pid, err)
		}
		return
	}
	if !strings.HasPrefix(strings.TrimSpace(p.CreatedBy), "contradiction") {
		return // 非矛盾沉淀的提议：走它自己的后续通道，与 F 线无关
	}
	// payload 里找回 contradiction_id / task_id（F3 写入）。
	var pl struct {
		ContradictionID int64  `json:"contradiction_id"`
		TaskID          int64  `json:"task_id"`
		Kind            string `json:"kind"`
	}
	if len(p.Payload) > 0 {
		_ = json.Unmarshal(p.Payload, &pl) // 坏 payload 退化成 0 值：下方按「最新 open gap」兜底
	}
	resolution := fmt.Sprintf("人工批准矛盾处置 proposal #%d：%s", pid, strings.TrimSpace(decideReason))
	if pl.ContradictionID > 0 {
		if err := pg.MarkContradictionResolved(pl.ContradictionID, resolution); err != nil {
			log.Printf("[contradiction] proposal #%d 消账置矛盾 %d resolved 失败: %v", pid, pl.ContradictionID, err)
		}
	}
	// 闭合对应空缺：evidence 类矛盾 ⇒ evidence 空缺；其余 ⇒ action 空缺
	// （F3 只用这两类建 gap）。没有精确关联 ⇒ 同 F5 简化口径：闭最新一条。
	taskID := pl.TaskID
	if taskID <= 0 && pl.ContradictionID > 0 {
		if c, cerr := pg.GetContradiction(pl.ContradictionID); cerr == nil && c != nil {
			taskID = c.TaskID
		}
	}
	if taskID <= 0 {
		return
	}
	kind := db.GapAction
	if pl.Kind == db.ContradictionEvidence {
		kind = db.GapEvidence
	}
	if closed, err := pg.CloseLatestOpenGap(taskID, kind, 0,
		fmt.Sprintf("——由矛盾处置 proposal #%d 人工批准闭合", pid)); err != nil {
		log.Printf("[contradiction] proposal #%d 消账闭合空缺失败: %v", pid, err)
	} else if closed {
		log.Printf("[contradiction] proposal #%d 批准，task %d 已闭合一条 %s 空缺", pid, taskID, kind)
	}
}

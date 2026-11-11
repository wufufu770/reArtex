package server

// ---------- F2 矛盾检测器 + F3 矛盾→沉淀路由（方案 v10 §12 / §16） ----------
//
// 为什么是「确定性 diff」而不是模型自省（§16 理论依据）：许愿纪律的可证伪性只有
// 落在机器可判的对照上才成立——预测 X、实际 Y，等不等是纯事实判断；让模型自评
// 「我的预测兑现了吗」等于让它给自己的愿望打分，愿望纪律会退化成修辞。检测全部
// 是 SQL + 字符串比较，零 LLM 调用。
//
// 调用点：engine.go 的 worker finalize——worker 终态（done/blocked/exhausted）
// 经 transitionIntentState 落库成功后调一次。三态语义：
//   done      —— 对照证据：预测的漏洞类别 vs 本意图 yields 的 finding 类别；
//   blocked   —— 只查 permission/path 矛盾（blocked_reason 里的 out_of_scope/cap）；
//   exhausted —— 无矛盾（那是资源问题，不是预测问题——预测没错，是预算不够）。
//
// fail-closed 口径（对终态路径的 fail-open）：本函数任何查询失败只记日志跳过，
// 绝不阻塞/改写 worker 的终态落库——矛盾检测是终态之上的增值层。整个函数体
// recover 兜底（照 engine.go 里 ActivityMirror/DenialWatcher 回调的同一口径）。
//
// B6 升级通道（denial_escalation.go）绕过 worker finalize 直接把意图转 blocked，
// 那条路的空缺入账由 F4 的 gap 源①在升级现场补（见 server/gapwake.go），本函数
// 保持单调用点。

import (
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"

	"github.com/Autumn-27/artex/db"
)

// detectContradiction 是 F2 的入口：意图终态落库成功后的矛盾对照。
//
// 无预测（intent_predictions 无行）⇒ 直接跳过——没有愿望就没有落空。
func (e *Engine) detectContradiction(t *Task, intentID int64, state string) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[contradiction] task %s 意图 #%d 检测 panic（忽略，终态路径不受影响）: %v", taskIDForLog(t), intentID, r)
		}
	}()
	if e == nil || e.m == nil || e.m.pg == nil || t == nil || t.Store == nil {
		return
	}
	taskID, err := strconv.ParseInt(t.ID, 10, 64)
	if err != nil || taskID <= 0 {
		return
	}
	// 读预测镜像：无预测 ⇒ 跳过（无预测字段不写行，F1 的口径）。
	pred, err := e.m.pg.GetIntentPrediction(intentID)
	if err != nil {
		log.Printf("[contradiction] task %s 意图 #%d 读预测失败（跳过检测）: %v", t.ID, intentID, err)
		return
	}
	if pred == nil {
		return
	}
	switch state {
	case "done":
		e.detectEvidenceContradiction(t, taskID, intentID, pred)
	case "blocked":
		e.detectBlockedContradiction(t, taskID, intentID)
	default:
		// exhausted / stopped：资源与治理问题，不是预测问题 ⇒ 无矛盾（§16 口径）。
	}
}

// detectEvidenceContradiction 是 done 态的证据对照（确定性 diff）：
//
//	① 预测未兑现：predicted_vulnclass 非空，但本意图 yields 的 findings 里没有
//	   一条 vulnclass 等于预测 ⇒ evidence 类矛盾（detail 写明预测 X 实际 Y；
//	   零 finding 产出单独注明「未兑现」）。
//	③ 预测外发现：实际 finding 的 vulnclass 不等于预测（预测是单值，不在即
//	   预测外）⇒ 另记一条 evidence 类矛盾，detail 标「预测外收益」。
//
// blocked/exhausted 不走这里：那是资源问题不是预测问题（调用点按 state 分流）。
func (e *Engine) detectEvidenceContradiction(t *Task, taskID, intentID int64, pred *db.IntentPrediction) {
	predicted := strings.ToLower(strings.TrimSpace(pred.PredictedVulnclass))
	if predicted == "" {
		return // 只对照了 vulnclass；predicted_actions/tools 的 diff 留给后续批次
	}
	findings, err := e.m.pg.YieldedFindingVulnclasses(t.ExpID, intentID)
	if err != nil {
		log.Printf("[contradiction] task %s 意图 #%d 读 yields findings 失败（跳过检测）: %v", t.ID, intentID, err)
		return
	}
	// 实际类别集合（去空、归一小写比对——worker 写回的大小写不构成事实差异）。
	classes := map[string][]int64{} // 归一类别 → finding 节点 id
	order := []string{}             // 保持出现序，detail 稳定可读
	for _, f := range findings {
		c := strings.ToLower(strings.TrimSpace(f.Vulnclass))
		if c == "" {
			continue
		}
		if _, seen := classes[c]; !seen {
			order = append(order, c)
		}
		classes[c] = append(classes[c], f.NodeID)
	}
	sort.Strings(order) // 稳定输出

	// ① 预测未兑现：实际集合里没有预测类别。
	if _, hit := classes[predicted]; !hit {
		var detail string
		if len(order) == 0 {
			detail = fmt.Sprintf("预测「%s」未兑现：意图 done 但没有任何 finding 产出", predicted)
		} else {
			detail = fmt.Sprintf("预测「%s」→ 实际「%s」：意图 done，但 yields 的 finding 类别与预测不符", predicted, strings.Join(order, "、"))
		}
		e.recordContradiction(t, taskID, intentID, db.ContradictionEvidence,
			detail, fmt.Sprintf("预测 %s → 实际 %s", predicted, strings.Join(order, "、")))
	}
	// ③ 预测外发现：实际类别不在预测（单值预测：不等即预测外）。
	for _, c := range order {
		if c == predicted {
			continue
		}
		ids := classes[c]
		detail := fmt.Sprintf("预测外收益：意图预测「%s」，实际发现「%s」（finding #%s）——许愿之外白捡的产出，值得复盘它由哪条路线带来",
			predicted, c, strings.Join(int64Strings(ids), ","))
		e.recordContradiction(t, taskID, intentID, db.ContradictionEvidence,
			detail, fmt.Sprintf("预测外收益：%s", c))
	}
}

// detectBlockedContradiction 是 blocked 态的 permission/path 对照：blocked_reason
// 含 out_of_scope / cap ⇒ 路径/权限类矛盾（预测没说会撞权限墙——愿望与现实的
// 差异在授权面，不在证据面）。
//
// blocked_reason 是自由文本（B6 升级写的是整条拒绝消息，里面有 reason_code=…），
// 这里做子串匹配的确定性判定。两种矛盾都不建 proposal（那是扩权诉求，走 ask
// 通道，F3），只记 gap（kind=action）。
func (e *Engine) detectBlockedContradiction(t *Task, taskID, intentID int64) {
	node, err := t.Store.GetNode(intentID)
	if err != nil || node == nil {
		if err != nil {
			log.Printf("[contradiction] task %s 意图 #%d 读节点失败（跳过检测）: %v", t.ID, intentID, err)
		}
		return
	}
	reason := strings.ToLower(node.BlockedReason)
	if reason == "" {
		return
	}
	switch {
	case strings.Contains(reason, "out_of_scope"):
		e.recordContradiction(t, taskID, intentID, db.ContradictionPath,
			fmt.Sprintf("路径矛盾：意图 blocked_reason 含 out_of_scope——预测的探索方向出了授权范围（%s）", truncateRunesLoopLite(node.BlockedReason, 200)),
			"路径出界（out_of_scope）")
	case strings.Contains(reason, "cap"):
		e.recordContradiction(t, taskID, intentID, db.ContradictionPermission,
			fmt.Sprintf("权限矛盾：意图 blocked_reason 含 cap——动作配额受限，预测未料到该方向的容量边界（%s）", truncateRunesLoopLite(node.BlockedReason, 200)),
			"容量受限（cap）")
	}
}

// recordContradiction 落一条矛盾 + 播报 + F3 路由。headline 是「预测X→实际Y」
// 式的短摘要（proposal 的 name 用），detail 是完整叙述。
func (e *Engine) recordContradiction(t *Task, taskID, intentID int64, kind, detail, headline string) {
	cid, err := e.m.pg.InsertContradiction(&db.Contradiction{
		TaskID:   taskID,
		IntentID: intentID,
		Kind:     kind,
		Detail:   detail,
	})
	if err != nil {
		log.Printf("[contradiction] task %s 意图 #%d 矛盾落库失败（跳过路由）: %v", t.ID, intentID, err)
		return
	}
	// 活动播报（worker=system，kind=note）：矛盾对规划者/人可见。
	e.emitActivity(t, db.Activity{Worker: "system", Kind: "note",
		Summary: fmt.Sprintf("【矛盾】%s：%s（意图 #%d，矛盾 #%d）", kindLabel(kind), detail, intentID, cid)})
	// F3 路由：按 kind 分流。
	e.routeContradiction(t, taskID, intentID, cid, kind, detail, headline)
}

// kindLabel 是矛盾的中文短标（播报用）。
func kindLabel(kind string) string {
	switch kind {
	case db.ContradictionEvidence:
		return "证据"
	case db.ContradictionPath:
		return "路径"
	case db.ContradictionPermission:
		return "权限"
	case db.ContradictionTool:
		return "工具"
	case db.ContradictionTime:
		return "时间"
	case db.ContradictionRisk:
		return "风险"
	}
	return kind
}

// routeContradiction 是 F3：矛盾写入后（resolved=false 的新行）按 kind 路由。
//
//	evidence 类 → 建一条 strategy_proposals（route=create_new，created_by=
//	  "contradiction"，status 被 db 层强制 pending）：复用既有的「pending 隔离态
//	  → 人工裁决 → 三门晋级」通道，人工批准后由 F5 的 approved 分支消账
//	  （MarkResolved + CloseGap）。proposal 的 payload 带 contradiction_id /
//	  task_id，裁决通道据此找回矛盾行。
//	path/permission 类 → 不建 proposal：那不是「新策略」而是「扩权诉求」，扩权
//	  只能走 ask（人工审批）通道；这里只记 gap（kind=action），由 F4 决定是否
//	  唤醒。
func (e *Engine) routeContradiction(t *Task, taskID, intentID, cid int64, kind, detail, headline string) {
	pg := e.m.pg
	switch kind {
	case db.ContradictionEvidence:
		payload, _ := json.Marshal(map[string]any{
			"name":             "矛盾处置：" + headline,
			"summary":          detail,
			"status":           db.StrategyProposalPending,
			"kind":             db.ContradictionEvidence,
			"task_id":          taskID,
			"intent_id":        intentID,
			"contradiction_id": cid,
		})
		if _, err := pg.CreateStrategyProposal(&db.StrategyProposal{
			Route:     db.StrategyRouteCreateNew,
			Payload:   payload,
			Reason:    "奇点循环矛盾沉淀（F3）",
			CreatedBy: "contradiction",
		}); err != nil {
			log.Printf("[contradiction] task %s 矛盾 #%d 建 proposal 失败: %v", t.ID, cid, err)
		}
		// 空缺入账（kind=evidence）+ 定向唤醒（gapwake.go；GapOpener nil = 只入账）。
		if e.GapOpener != nil {
			e.GapOpener(taskID, db.GapEvidence, detail)
		}
	default:
		// path/permission（及其余种类）：扩权诉求走 ask，不建 proposal，只记 gap。
		if e.GapOpener != nil {
			e.GapOpener(taskID, db.GapAction, detail)
		}
	}
}

// int64Strings 是 ids 的十进制字符串化（detail 里列 finding id 用）。
func int64Strings(ids []int64) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, strconv.FormatInt(id, 10))
	}
	return out
}

// truncateRunesLoopLite 是 server 侧的 rune 截断（与 db.truncateRunesLoop 同口径；
// 不复用是因为那是小写私有）。注：包里已有 trunc(s,max)（scheduler.go）按字节截，
// 中文场景会截出半个字符，矛盾文本统一走 rune 版。
func truncateRunesLoopLite(s string, n int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// taskIDForLog 是日志兜底（t 为 nil 时不 panic）。
func taskIDForLog(t *Task) string {
	if t == nil {
		return "?"
	}
	return t.ID
}

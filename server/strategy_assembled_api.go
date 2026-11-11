package server

// GET /api/strategies/assembled —— 「注入拼装」只读出口（v0.9.17）。
//
// 用户原话：「策略注入进去，我可以直接看拼好的整个库，然后这个库会被 agent 调用」。
// 本端点把**真正会进 agent 上下文**的两块注入文本原样渲染出来：
//
//  1. strategies_block —— planner 侧的策略摘要注入（engine.strategyContextProvider
//     → EnsureTaskStrategySnapshot → agent.RenderStrategySummaries；只渲染
//     name+summary+utility，绝不渲染 steps，与生产路径同一份代码）；
//  2. legacy_block —— 新任务种子路径的经验注入（server.legacyInjectionBlock：
//     accepted 池 top-5、≤12K 字符预算、加权重排，与生产路径同一份代码）。
//
// 【只读】本文件一个字都不写：快照生成 EnsureTaskStrategySnapshot 是生产路径
// 本身（planner 首轮规划本来就会调），这里提前调它不构成额外裁决；不传
// task_id 则完全不碰快照，直接渲染 active 池。
//
// 参照（口径对齐）：server/strategy_read_api.go —— GET 绝不触发裁决写。

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
)

func (s *Server) strategyAssembled(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	if err := pg.EnsureStrategySchema(); err != nil {
		writeErr(w, 500, "读注入拼装失败: "+err.Error())
		return
	}
	// task_id 可选：给了 = 渲染该任务的真实快照（planner 实际拿到的那份）；
	// 不给 = 渲染 active 池全量摘要（「整个库」的当前样子）。
	taskID, ok := sodOptionalInt64(w, r.URL.Query().Get("task_id"), "task_id")
	if !ok {
		return
	}

	var (
		entries []db.StrategySnapshotEntry
		block   string
		note    string
	)
	if taskID > 0 {
		var err error
		entries, err = pg.EnsureTaskStrategySnapshot(taskID)
		if err != nil {
			writeErr(w, 500, "读任务策略快照失败: "+err.Error())
			return
		}
		block = agent.RenderStrategySummaries(entries)
		note = "这是任务 " + strconv.FormatInt(taskID, 10) +
			" 的策略快照（组内同版本）：planner 每轮规划拿到的就是这一段。"
	} else {
		actives, err := pg.ListStrategies("active", 200)
		if err != nil {
			writeErr(w, 500, "读 active 策略池失败: "+err.Error())
			return
		}
		for _, st := range actives {
			entries = append(entries, db.StrategySnapshotEntry{
				StrategyID: st.ID, Name: st.Name, Summary: st.Summary,
				Version: st.Version, Utility: st.Utility,
			})
		}
		block = agent.RenderStrategySummaries(entries)
		note = "这是 active 池全量摘要（未按任务快照）：实际注入按任务的资产签名检索 top-k。" +
			"传 ?task_id=<id> 可看某个任务真实拿到的那份快照。"
	}
	if strings.TrimSpace(block) == "" {
		block = "（当前没有可注入的策略——active 池为空，或该任务快照生成时没有命中策略。）"
	}

	// 经验注入块：与 launchTask 的种子路径同一渲染函数，只读变体（不记账）。
	// goal 用查询参数给（模拟「新任务的目标文本」检索同族经验）；缺省空串 =
	// 只按 prior/时效排序取 top-5。
	legacy := s.legacyInjectionPreview(r.URL.Query().Get("goal"))
	if strings.TrimSpace(legacy) == "" {
		legacy = "（accepted 经验池当前没有可注入内容——评审通过的经验才会进这个池。）"
	}

	writeJSON(w, 200, map[string]any{
		"strategies_block": block,
		"legacy_block":     legacy,
		"entries":          entries,
		"note":             note,
		// 与账本页同款口径声明：这里看到的就是 agent 拿到的（渲染函数同源）。
		"advisory_only": true,
	})
}

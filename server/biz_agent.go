package server

// 业务建模 agent 的注册（BIZ 合并施工 M3）。
//
// 五步模板照 seedReporterAgent（orchestration.go）：建 agent 行 → 播种提示词 →
// 设触发运行策略 → 绑工具 → 建触发器。选择「自定义 agent」路线的原因：触发器
// UI（AgentEditor「触发」tab）对内置 agent 直接 400（server/triggers.go:58），
// 「用户在系统设置里逐项开启建模触发」这个产品需求只有自定义 agent 行得通。
//
// 与 reporter 种子的两处差异（都有对应原因）：
//   - 触发器默认 Enabled=false：建模是长时任务，「用户明确提出才触发」由默认关
//     + UI 逐项开实现（方案一 §4.3）。
//   - 收尾 InvalidateAssembly：G1 装配缓存探针不含 tools 表，种子/绑定直写 DB
//     后不翻代会让首轮 run 看不到新工具。

import (
	"log"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
)

// seedBizModelerAgent 预置「业务建模」自定义 agent（builtin=false，可在 UI 编辑）：
// agent 行 + 提示词 + 运行策略 + 工具绑定 + 触发器（默认关）。一次性 settings 旗标
// 守卫；key 被用户占用则跳过不覆盖。
func (s *Server) seedBizModelerAgent() {
	const flag = "bizmodeler_agent_seed_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	defer func() { _ = s.m.pg.SetSetting(flag, "true") }() // 无论成功与否只尝试一次
	if exist, _ := s.m.pg.GetAgentByKey("bizmodeler"); exist != nil {
		return // key 已被占用(用户手建过)——不覆盖
	}
	a, err := s.m.pg.CreateAgent("bizmodeler", "业务建模",
		"还原目标系统的业务结构：实体、动作、状态、角色权限、数据流与信任边界；"+
			"输出设计层与行为层的差异，并把可验证的业务假设写回探索图。")
	if err != nil {
		log.Printf("[biz] 建模 agent 种子失败: %v", err)
		return
	}
	if err := s.m.pg.SeedPromptIfEmpty(a.ID, agent.BizModelerDefaultPrompt); err != nil {
		log.Printf("[biz] 建模提示词播种失败: %v", err)
	}
	// 建模是长时任务：同一 agent 的触发串行（避免同一目标被多条建模并发写坏图）、
	// 按任务合并、并发上限 1。
	if err := s.m.pg.SetAgentTriggerBehavior("bizmodeler", "serial", "by_task", 1); err != nil {
		log.Printf("[biz] 建模运行策略设置失败: %v", err)
	}
	// 工具绑定：业务图读写组 + 资产/跨任务只读组。roleToolAllow 的
	// bizModelerToolAllow 双闸同时生效（白名单外交不进 registry）。
	if err := s.m.pg.AddAgentToToolBinding("bizmodeler", []string{
		"read_biz_graph", "read_design_artifacts", "upsert_biz_node", "upsert_biz_edge",
		"record_biz_diff", "propose_biz_intent", "biz_refuted_facts", "biz_adjudicate",
		"list_assets", "list_companies", "get_task_graph", "list_task_findings",
		"get_task_node_detail", "get_task_worker_trace",
	}); err != nil {
		log.Printf("[biz] 建模工具绑定失败: %v", err)
	}
	// 触发器默认关：用户在 AgentEditor「触发」tab 逐项开启（on_task_create =
	// 新任务建好自动建模一次给出业务骨架；on_finding = 新漏洞确认后用新事实修正图谱）。
	if _, err := s.m.pg.CreateTrigger(&db.AgentTrigger{
		AgentKey:          "bizmodeler",
		Enabled:           false,
		OnTaskCreate:      true,
		TaskCreateMessage: "新任务已创建。请对目标做一次业务建模：读范围内资产与前端授权产物，产出业务骨架图（design 层），并把可验证的业务假设写回探索图。",
		OnFinding:         true,
		FindingMessage:    "有新漏洞被确认。请增量建模：读证伪池与新事实，修正业务图（superseded 指针），并把受影响的差异重新评估。",
	}); err != nil {
		log.Printf("[biz] 建模触发器创建失败: %v", err)
	}
	agent.InvalidateAssembly()
	log.Printf("[biz] 业务建模 agent bizmodeler 已就绪（触发器默认关，UI 逐项开）")
}

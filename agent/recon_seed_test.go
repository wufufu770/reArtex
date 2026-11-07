package agent

import (
	"strings"
	"testing"
)

// 本文件是「资产收集」agent（recon，方案 v4 §3-C）的种子钉子：提示词与工具绑定。
// recon 的提示词是它的行为边界（先被动后主动 / 只收集不利用 / 一切发现入库），
// 工具绑定是它能动手的全部面——两处都按「有意识的变更才许改」钉住。

// TestReconPromptSeeded 保证 recon 的提示词种子在目录里，且 P0-P9 流程骨架与
// 写死纪律的关键句都在。不做摘要钉死（六既有 agent 的做法）：recon 是新 agent，
// 前期文案会按使用反馈迭代；这里钉的是「内容要件存在」，而不是一字不动。
func TestReconPromptSeeded(t *testing.T) {
	body, ok := BuiltinPromptSeeds()["recon"]
	if !ok {
		t.Fatal("提示词种子缺少 recon")
	}
	if strings.TrimSpace(body) == "" {
		t.Fatal("recon 提示词种子为空")
	}
	// P0-P9 流程骨架：每个阶段都要有编号锚点，缺一个流程就不完整。
	for _, stage := range []string{"P0", "P1", "P2", "P3", "P4", "P5", "P6", "P7", "P8", "P9"} {
		if !strings.Contains(body, stage) {
			t.Errorf("recon 提示词缺流程阶段 %s", stage)
		}
	}
	// 写死纪律的关键句（方案 v4 §3-C1 逐条清单的落点）。
	want := []string{
		"先被动后主动",          // 纪律 1 + 流程顺序
		"只记录不探测",          // 纪律 2：范围外只记录
		"insert_assets",   // 纪律 3：一切发现结构化入库
		"uncertain",       // 纪律 4：指纹不确定标 uncertain
		"429",             // 纪律 5：限速礼貌/退避
		"泛解析",             // 纪律 6：泛解析/蜜罐意识
		"查重",              // 纪律 7：不产生重复资产
		"record_progress", // 纪律 8：阶段产物即时落盘
		"只收集不利用",          // 纪律 9：只收集不利用
		"spawn_task",      // 兜底建任务的承办位
		"list_companies",  // 平台工具用法
		"record_fact",     // 事实带证据
	}
	for _, frag := range want {
		if !strings.Contains(body, frag) {
			t.Errorf("recon 提示词缺纪律要件 %q", frag)
		}
	}
	// recon 不做漏洞登记：report_finding 只能以「不归你」的否定句出现。
	if !strings.Contains(body, "report_finding") {
		t.Error("recon 提示词应显式说明不调 report_finding")
	}
}

// TestReconToolBindings 保证 recon 的默认工具绑定恰好是方案 v4 §3-C2 那一份：
// 查企业/查资产/插资产/查漏洞/记事实/记进度 + spawn_task（server 侧编排工具）。
func TestReconToolBindings(t *testing.T) {
	seeds := BuiltinToolSeeds()
	byKey := map[string]ToolSeed{}
	for _, s := range seeds {
		byKey[s.Key] = s
	}
	bound := []string{"list_companies", "list_assets", "insert_assets", "list_findings", "record_fact", "record_progress"}
	for _, key := range bound {
		s, ok := byKey[key]
		if !ok {
			t.Fatalf("工具 %s 不在种子目录里", key)
		}
		if !containsAgentKey(s.Agents, "recon") {
			t.Errorf("recon 应默认绑定 %s，实际 %v", key, s.Agents)
		}
	}
	// 负面清单：recon 不做漏洞登记、不自主扩权、不暂停任务。
	for _, key := range []string{"report_finding", "add_task_scope", "add_company_scope", "pause_task", "add_intent"} {
		if s, ok := byKey[key]; ok && containsAgentKey(s.Agents, "recon") {
			t.Errorf("recon 不应绑定 %s", key)
		}
	}
	// spawn_task 是 server 侧编排工具，不在 BuiltinToolSeeds 里——经
	// ReconDefaultToolKeys 交给 seedReconDefaultBindings 补绑。
	if _, ok := byKey["spawn_task"]; ok {
		t.Error("spawn_task 不应出现在 BuiltinToolSeeds（它是 server 侧编排工具）")
	}
	keys := ReconDefaultToolKeys()
	got := map[string]bool{}
	for _, k := range keys {
		got[k] = true
	}
	for _, want := range append(append([]string{}, bound...), "spawn_task") {
		if !got[want] {
			t.Errorf("ReconDefaultToolKeys 缺 %s（got %v）", want, keys)
		}
	}
	if len(keys) != len(bound)+1 {
		t.Errorf("ReconDefaultToolKeys 数量 = %d, want %d（%v）", len(keys), len(bound)+1, keys)
	}
}

package agent

import (
	"strings"
	"testing"
)

// 本文件钉住「收尾期不再开新活」这条规则在 **artex 接线层**上的落实。
//
// 为什么要有这一层：SDK（third_party/norma/harness）自己也有收尾期禁工具的
// 用例，但那只证明 SDK 的机制能用 —— artex 侧的接线（每个角色禁哪些、收尾词
// 与禁列表怎么合并）此前**零覆盖**：把 agent/planner.go 的 wrapupSettlement
// 传参改成 nil，全部测试仍然绿。
//
// 落点：收尾期不是「提示 agent 别乱来」，而是**把探测类工具从模型可见的工具面
// 里摘掉**（harness 按工具名精确过滤）。写回类工具必须留着 —— 收尾期的全部
// 意义就是把已确认的结论落库。

// containsFold 判断清单里是否含某个工具名（大小写不敏感）。
func containsFold(list []string, want string) bool {
	for _, v := range list {
		if strings.EqualFold(v, want) {
			return true
		}
	}
	return false
}

// TestSettlementDisabledToolsBlocksProbing 钉住 worker 收尾期禁掉探测类工具、
// 且**保留**写回类工具。
//
// 为什么写回类不能禁：收尾期的全部意义就是把已识别但未落库的东西落库；禁掉
// report_finding / insert_assets / record_fact 等于「收尾期不许写回」——
// 那不是收尾，是丢结论。
func TestSettlementDisabledToolsBlocksProbing(t *testing.T) {
	got := SettlementDisabledTools("worker")
	mustBlock := []string{"Bash", "WebFetch", "Write", "Edit", "shell_open", "shell_send"}
	for _, tool := range mustBlock {
		if !containsFold(got, tool) {
			t.Fatalf("worker 收尾期必须禁 %s, got %v", tool, got)
		}
	}
	mustKeep := []string{"report_finding", "insert_assets", "record_fact"}
	for _, tool := range mustKeep {
		if containsFold(got, tool) {
			t.Fatalf("worker 收尾期不得禁 %s（agent 要靠它落结论）, got %v", tool, got)
		}
	}
}

// TestSettlementDisabledToolsPlanner 钉住 planner 收尾期的禁工具清单。
//
// planner 的收尾期有两条互不相同的要求：
//   - 不得再**生成新意图**（add_intent = 开新活）；
//   - 必须能**写回事实**（record_fact 等写回类工具留着）。
//
// 同时收尾期也不该再开新的探测面（Bash / WebFetch / Write / Edit / shell_*）。
func TestSettlementDisabledToolsPlanner(t *testing.T) {
	got := SettlementDisabledTools("planner")
	if !containsFold(got, "add_intent") {
		t.Fatalf("planner 收尾期必须禁 add_intent, got %v", got)
	}
	if containsFold(got, "record_fact") {
		t.Fatalf("planner 收尾期不得禁 record_fact（要靠它落结论）, got %v", got)
	}
	for _, tool := range []string{"Bash", "WebFetch", "Write", "Edit", "shell_open", "shell_send"} {
		if !containsFold(got, tool) {
			t.Fatalf("planner 收尾期必须禁 %s, got %v", tool, got)
		}
	}
}

// TestSettlementDisabledToolsWiredIntoSettlement 钉住「清单真的接进了收尾配置」。
//
// 前面两条钉的是清单本身；这条钉的是**接线**：wrapupSettlement /
// wrapupSettlementForTask 必须把清单并进 harness.Settlement.DisabledTools，
// 否则清单只是一份没人读的常量（这正是 planner 此前收尾期不禁任何工具的原因）。
//
// 调用方额外传入的清单按「追加」处理，不覆盖默认清单。
func TestSettlementDisabledToolsWiredIntoSettlement(t *testing.T) {
	settle := wrapupSettlement("planner", nil)
	if !containsFold(settle.DisabledTools, "add_intent") {
		t.Fatalf("planner 收尾配置未禁 add_intent, got %v", settle.DisabledTools)
	}
	if !containsFold(settle.DisabledTools, "Bash") {
		t.Fatalf("planner 收尾配置未禁 Bash, got %v", settle.DisabledTools)
	}
	if containsFold(settle.DisabledTools, "record_fact") {
		t.Fatalf("planner 收尾配置禁了 record_fact, got %v", settle.DisabledTools)
	}

	// 调用方追加的清单是「追加」而不是「覆盖」：worker 传 ["Bash"] 时，
	// 默认清单里其余的探测类工具不能因此消失。
	worker := wrapupSettlement("worker", []string{"Bash"})
	if !containsFold(worker.DisabledTools, "WebFetch") {
		t.Fatalf("worker 收尾配置丢了默认清单里的 WebFetch, got %v", worker.DisabledTools)
	}
	if !containsFold(worker.DisabledTools, "Bash") {
		t.Fatalf("worker 收尾配置缺 Bash, got %v", worker.DisabledTools)
	}
	// 重复项只留一份（Bash 已在默认清单里）。
	if n := countFold(worker.DisabledTools, "Bash"); n != 1 {
		t.Fatalf("Bash 在收尾清单里出现 %d 次, want 1: %v", n, worker.DisabledTools)
	}

	// 任务超时那条路径用的是另一个构造函数 ⇒ 同样必须带上清单。
	taskSettle := wrapupSettlementForTask("planner", nil, true)
	if !containsFold(taskSettle.DisabledTools, "add_intent") {
		t.Fatalf("任务超时收尾配置未禁 add_intent, got %v", taskSettle.DisabledTools)
	}
}

// countFold 数清单里某个工具名出现了几次（大小写不敏感）。
func countFold(list []string, want string) int {
	n := 0
	for _, v := range list {
		if strings.EqualFold(v, want) {
			n++
		}
	}
	return n
}

// TestQuotaWrapupPromptMentionsRemainingAndGracefulFinish 钉住额度收尾提示的
// 两件内容：**剩余额度**与**优雅收尾**。
//
// 为什么这两件缺一不可：群组额度用尽时平台不切断在途 agent（切断会丢掉上下文里
// 已得出但未落库的结论），而是让它「主动落结论」。要主动落结论，agent 必须
// 知道还剩多少（否则它无从判断该收多紧），也必须被明确要求收尾（否则它会以为
// 还能继续干下去）。只写「额度预警」而不给数字 = 预警无效；只给数字而不要求
// 收尾 = agent 照旧开新活。
func TestQuotaWrapupPromptMentionsRemainingAndGracefulFinish(t *testing.T) {
	p := QuotaWrapupPrompt(200000, 1000000)
	if p == "" {
		t.Fatal("prompt must not be empty")
	}
	if !strings.Contains(p, "200000") {
		t.Fatalf("prompt should mention remaining tokens, got %q", p)
	}
	if !strings.Contains(p, "剩余额度") {
		t.Fatalf("prompt should mention the remaining quota, got %q", p)
	}
	if !strings.Contains(p, "收尾") && !strings.Contains(p, "结论") {
		t.Fatalf("prompt should ask for a graceful finish, got %q", p)
	}
}

// TestQuotaWrapupPromptClampsNegativeRemaining 钉住负数余量被夹到 0。
//
// 账实相符时不会出现负余量，但预扣可能让「剩余」短暂为负；提示词里出现
// 「剩余额度约 -500 token」既荒唐又会误导 agent（它以为还能再花）。
func TestQuotaWrapupPromptClampsNegativeRemaining(t *testing.T) {
	p := QuotaWrapupPrompt(-500, 1000)
	if strings.Contains(p, "-500") {
		t.Fatalf("prompt must not show a negative remaining quota, got %q", p)
	}
	if !strings.Contains(p, "0") {
		t.Fatalf("prompt should show the clamped remaining quota, got %q", p)
	}
}

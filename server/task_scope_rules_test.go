package server

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/intercept"
)

// 证据保全策略（Task 5）的测试。
//
// 为什么这个用例必须走**真实**拦截器（intercept.Interceptor.Match）而不是自己
// 拿正则去比：正则写得「看着对」却不命中，是这类任务唯一真正要防的失败模式。
// 平台真正拦一条工具调用时用的是 intercept 包的匹配器（raw 入参 + 归一化入参两
// 个主体、RE2 语法），本用例把装到任务上的那批 pattern 原样放进全局规则表，
// 让平台自己的匹配器去判 —— 判中才算数。
//
// 任务的命令级规则存放在 task_intercept_rules（按 task_id 归属），而
// intercept.Interceptor 读的是全局 intercept_rules。两张表不是一回事，所以本
// 用例分两步证明：
//  1) 存储面：applyEvidencePreservation 装到**那个任务**上的 pattern 与模板一致；
//  2) 匹配面：这批 pattern 在平台真实匹配器里确实拦得住清理痕迹、且拦不住正常
//     排查命令。
// 存储到执行之间的那段接线（谁在工具调用前读任务级命令规则）不在本单元的文件域
// 内，见 server/task_scope_rules.go 的说明。

// evidenceSample 是一条"清理痕迹"样本命令，以及它应当命中的规则下标。
type evidenceSample struct {
	cmd    string
	ruleIx int
}

// evidenceSamples 是计划书列出的样本集：每条命令对应一条证据保全规则。
var evidenceSamples = []evidenceSample{
	{cmd: "sed -i '/root/d' /var/log/auth.log", ruleIx: 0},
	{cmd: "truncate -s 0 /var/log/secure", ruleIx: 1},
	{cmd: "journalctl --vacuum-time=1s", ruleIx: 2},
	{cmd: "find /var/log -name '*.log' -delete", ruleIx: 3},
	{cmd: "history -c", ruleIx: 4},
	{cmd: "unset HISTFILE", ruleIx: 4},
	{cmd: "rm -f /var/log/wtmp", ruleIx: 5},
}

// evidenceInspectionCommands 是正常排查命令：证据保全策略**不得**拦它们。
//
// 拦错了比漏拦了更糟：客户端付费买的正是这些日志，正常排查被挡住等于把任务做死。
var evidenceInspectionCommands = []string{
	"cat /var/log/auth.log",
	"journalctl -u ssh --since today",
}

// evidenceBashInput 把一条命令包成 Bash 工具的入参 JSON（平台真实匹配器看到的形态）。
func evidenceBashInput(t *testing.T, cmd string) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]string{"command": cmd})
	if err != nil {
		t.Fatalf("构造工具入参失败: %v", err)
	}
	return b
}

// evidenceRuleNames 是模板里全部规则名的集合（用来区分「被证据保全规则拦下」与
// 「被平台其它内置规则拦下」——后者不算本策略的事）。
func evidenceRuleNames() map[string]bool {
	out := map[string]bool{}
	for _, r := range evidencePreservationRules {
		out[r.Name] = true
	}
	return out
}

// evidencePatternOfTemplate 按 pattern 找出模板里的那条规则。
func evidencePatternOfTemplate(t *testing.T, pattern string) (name, message string) {
	t.Helper()
	for _, r := range evidencePreservationRules {
		if r.Pattern == pattern {
			return r.Name, r.Message
		}
	}
	t.Fatalf("模板里没有这条 pattern: %s", pattern)
	return "", ""
}

// evidenceInstallGlobalFixture 把任务上装的那批 pattern 原样放进全局规则表，
// 让平台真实的 intercept.Interceptor 去匹配。返回 (拦截器, 行 id → pattern)。
//
// 为什么优先级取 200：内置规则的优先级是 80~100，取更高才能保证"命中的是证据
// 保全规则这一条"，而不是被别的内置规则先拦下（rm -f /var/log/wtmp 这类命令本来
// 就会被内置的「删除系统关键路径」拦下，混在一起就分不清是谁拦的）。
func evidenceInstallGlobalFixture(t *testing.T, pg *db.DB, patterns []string) (*intercept.Interceptor, map[int64]string) {
	t.Helper()
	ids := map[int64]string{}
	for _, p := range patterns {
		name, message := evidencePatternOfTemplate(t, p)
		rule, err := pg.CreateInterceptRule(name, "tool_input", "regex", p, "deny", message, 200, true, false, 60, "deny")
		if err != nil {
			t.Fatalf("安装全局规则夹具失败: %v", err)
		}
		ids[rule.ID] = p
		t.Cleanup(func() { _ = pg.DeleteInterceptRule(rule.ID) })
	}
	return intercept.New(pg), ids
}

// evidenceCountGlobalRules 数全局命令拦截规则条数（用来证明装任务级规则没有动全局）。
func evidenceCountGlobalRules(t *testing.T, pg *db.DB) int {
	t.Helper()
	var n int
	if err := pg.QueryRow(`SELECT count(*) FROM intercept_rules`).Scan(&n); err != nil {
		t.Fatalf("统计全局拦截规则失败: %v", err)
	}
	return n
}

// TestEvidencePreservationBlocksAntiForensics：开启证据保全的任务，清理痕迹的
// 命令必须被平台真实匹配器拦下，且拦下它的正是证据保全规则本身。
func TestEvidencePreservationBlocksAntiForensics(t *testing.T) {
	s, pg := taskHallTestServer(t)
	task := taskHallCreateTask(t, pg, "证据保全策略测试")

	if err := s.applyEvidencePreservation(task.ID); err != nil {
		t.Fatalf("安装证据保全规则失败: %v", err)
	}

	// 存储面：规则装在这个任务上，且 pattern 与模板逐条一致。
	rules, err := pg.Assets().ListTaskCommandRules(task.ID)
	if err != nil {
		t.Fatalf("读任务命令规则失败: %v", err)
	}
	if len(rules) != len(evidencePreservationRules) {
		t.Fatalf("任务上的命令规则数 = %d，期望 %d", len(rules), len(evidencePreservationRules))
	}
	stored := map[string]db.AssetInterceptRule{}
	for _, r := range rules {
		if !r.Enabled {
			t.Fatalf("装上去的规则必须是启用的: %+v", r)
		}
		if r.Action != "block" {
			t.Fatalf("命令规则的动作必须是 block，实际 %q", r.Action)
		}
		stored[r.Pattern] = r
	}
	patterns := make([]string, 0, len(evidencePreservationRules))
	for _, r := range evidencePreservationRules {
		row, ok := stored[r.Pattern]
		if !ok {
			t.Fatalf("任务上没有这条规则: %s（pattern %s）", r.Name, r.Pattern)
		}
		if !strings.Contains(row.Note, r.Message) {
			t.Fatalf("规则 %s 的拒绝理由没落库: note=%q 期望包含 %q", r.Name, row.Note, r.Message)
		}
		patterns = append(patterns, r.Pattern)
	}

	// 匹配面：平台真实匹配器判这批 pattern。
	ic, idToPattern := evidenceInstallGlobalFixture(t, pg, patterns)
	names := evidenceRuleNames()

	for _, sample := range evidenceSamples {
		dec, matched := ic.Match("Bash", evidenceBashInput(t, sample.cmd))
		if !matched {
			t.Fatalf("清理痕迹命令未被拦下: %s", sample.cmd)
		}
		if dec.Action != "deny" {
			t.Fatalf("清理痕迹命令的动作必须是 deny，实际 %q（命令 %s）", dec.Action, sample.cmd)
		}
		if !names[dec.RuleName] {
			t.Fatalf("拦下 %s 的不是证据保全规则: %s", sample.cmd, dec.RuleName)
		}
		want := evidencePreservationRules[sample.ruleIx].Pattern
		if got, ok := idToPattern[dec.RuleID]; !ok || got != want {
			t.Fatalf("命令 %s 命中的是 pattern %q，期望 %q（规则 %s）", sample.cmd, got, want, evidencePreservationRules[sample.ruleIx].Name)
		}
	}
}

// TestEvidencePreservationLeavesInspectionCommandsAlone：正常排查命令（读日志、
// 查 journal）**不得**被证据保全规则拦下。
//
// 拦错了比漏拦了更糟 —— 客户端付费买的正是这些日志，挡住正常排查等于把任务做死。
func TestEvidencePreservationLeavesInspectionCommandsAlone(t *testing.T) {
	s, pg := taskHallTestServer(t)
	task := taskHallCreateTask(t, pg, "证据保全策略负例测试")

	if err := s.applyEvidencePreservation(task.ID); err != nil {
		t.Fatalf("安装证据保全规则失败: %v", err)
	}
	rules, err := pg.Assets().ListTaskCommandRules(task.ID)
	if err != nil {
		t.Fatalf("读任务命令规则失败: %v", err)
	}
	patterns := make([]string, 0, len(rules))
	for _, r := range rules {
		patterns = append(patterns, r.Pattern)
	}
	ic, _ := evidenceInstallGlobalFixture(t, pg, patterns)
	names := evidenceRuleNames()

	for _, cmd := range evidenceInspectionCommands {
		dec, matched := ic.Match("Bash", evidenceBashInput(t, cmd))
		if matched && names[dec.RuleName] {
			t.Fatalf("正常排查命令被证据保全规则拦下: %s（规则 %s）", cmd, dec.RuleName)
		}
	}
}

// TestEvidencePreservationIsOptInPerTask：策略是**逐任务**开启的 —— 没开启的
// 任务一行规则都没有，且开启的那次安装不许碰到全局命令拦截规则表。
func TestEvidencePreservationIsOptInPerTask(t *testing.T) {
	s, pg := taskHallTestServer(t)
	opted := taskHallCreateTask(t, pg, "开启证据保全的任务")
	plain := taskHallCreateTask(t, pg, "未开启证据保全的任务")

	globalBefore := evidenceCountGlobalRules(t, pg)
	if err := s.applyEvidencePreservation(opted.ID); err != nil {
		t.Fatalf("安装证据保全规则失败: %v", err)
	}
	globalAfter := evidenceCountGlobalRules(t, pg)
	if globalAfter != globalBefore {
		t.Fatalf("任务级安装不得改动全局命令拦截规则（%d → %d）", globalBefore, globalAfter)
	}

	onTask, err := pg.Assets().ListTaskCommandRules(opted.ID)
	if err != nil {
		t.Fatalf("读任务命令规则失败: %v", err)
	}
	if len(onTask) != len(evidencePreservationRules) {
		t.Fatalf("开启的任务上的命令规则数 = %d，期望 %d", len(onTask), len(evidencePreservationRules))
	}

	offTask, err := pg.Assets().ListTaskCommandRules(plain.ID)
	if err != nil {
		t.Fatalf("读任务命令规则失败: %v", err)
	}
	if len(offTask) != 0 {
		t.Fatalf("未开启的任务不得有任何命令规则，实际 %d 条", len(offTask))
	}

	// 幂等：重复安装不堆重复行。
	if err := s.applyEvidencePreservation(opted.ID); err != nil {
		t.Fatalf("重复安装证据保全规则失败: %v", err)
	}
	again, err := pg.Assets().ListTaskCommandRules(opted.ID)
	if err != nil {
		t.Fatalf("读任务命令规则失败: %v", err)
	}
	if len(again) != len(onTask) {
		t.Fatalf("重复安装后规则数 = %d，期望仍是 %d（幂等）", len(again), len(onTask))
	}
}

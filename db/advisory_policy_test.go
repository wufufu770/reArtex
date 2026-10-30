package db

import (
	"encoding/json"
	"testing"
)

// TestAdvisoryBuiltinAgents 保证四个顾问 agent 作为内置 agent 落库。
//
// role 必须是 assistant：它是会话型角色（与 auto / pentest 同类），前端对话页按
// role='assistant' 挑可对话的内置 agent；填成编排角色（goals/planner/main/worker）
// 会让它们既不进编排循环、又不出现在对话页里——一个跑不起来的 agent。
//
// 交互式 shell 只给 redteam：证伪者的工作就是在 scope 内动手跑可重放的探测动作；
// 仲裁者与验证者只读，不该有终端。
func TestAdvisoryBuiltinAgents(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()

	want := map[string]bool{"curator": false, "verifier": false, "redteam": true, "creative": false}
	for key, wantShell := range want {
		var role string
		var interactive bool
		if err := d.QueryRow(`SELECT role, interactive_shell FROM agents WHERE key=$1`, key).Scan(&role, &interactive); err != nil {
			t.Fatalf("读取内置 agent %s 失败: %v", key, err)
		}
		if role != "assistant" {
			t.Errorf("agent %s 的 role=%q，应为 assistant", key, role)
		}
		if interactive != wantShell {
			t.Errorf("agent %s 的 interactive_shell=%v，应为 %v", key, interactive, wantShell)
		}
	}
}

// TestAdvisoryAgentToolBindings 保证工具表里的绑定遵守顾问 agent 的策略：
// 白名单里的工具绑上，硬拒绝的工具解绑，既有 agent 的绑定一个不动。
func TestAdvisoryAgentToolBindings(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()

	keys := []string{
		"list_findings", "list_assets", "graph_overview",
		"report_finding", "add_intent", "traffic_search",
		"get_finding_traffic", "bind_finding_traffic", "probe_http_cmd",
	}
	for _, k := range keys {
		if _, err := d.Exec(`DELETE FROM tools WHERE key=$1`, k); err != nil {
			t.Fatalf("清理工具行 %s 失败: %v", k, err)
		}
	}
	// 造一批工具行：既有 agent 的绑定照常，另外模拟用户在工具管理页的误绑——
	// 把 report_finding 绑给 verifier、把 add_intent 绑给 curator、把可执行的
	// 自定义命令工具绑给 verifier。
	seed := func(key, kind string, agents []string) {
		t.Helper()
		b, err := json.Marshal(agents)
		if err != nil {
			t.Fatal(err)
		}
		if err := d.SeedTool(key, "测试工具", nil, b); err != nil {
			t.Fatalf("播种工具 %s 失败: %v", key, err)
		}
		if kind != "builtin" {
			if _, err := d.Exec(`UPDATE tools SET kind=$2 WHERE key=$1`, key, kind); err != nil {
				t.Fatalf("设置工具 %s 的 kind 失败: %v", key, err)
			}
		}
	}
	seed("list_findings", "builtin", []string{"mainagent", "planner", "worker", "pentest"})
	seed("list_assets", "builtin", []string{"mainagent", "worker", "auto", "pentest"})
	seed("graph_overview", "builtin", []string{"mainagent", "planner"})
	seed("report_finding", "builtin", []string{"mainagent", "planner", "worker", "auto", "pentest", "verifier"})
	seed("add_intent", "builtin", []string{"mainagent", "planner", "curator"})
	seed("traffic_search", "builtin", []string{"worker", "verifier"})
	seed("get_finding_traffic", "builtin", []string{"auto"})
	seed("bind_finding_traffic", "builtin", []string{"reporter", "creative"})
	seed("probe_http_cmd", "command", []string{"redteam", "verifier"})

	if err := d.seedAdvisoryAgentToolBindings(); err != nil {
		t.Fatalf("协调顾问 agent 绑定失败: %v", err)
	}

	agentsOf := func(key string) []string {
		t.Helper()
		row, err := d.GetTool(key)
		if err != nil {
			t.Fatalf("读取工具 %s 失败: %v", key, err)
		}
		if row == nil {
			t.Fatalf("工具 %s 不存在", key)
		}
		return row.Agents
	}
	has := func(keys []string, want string) bool {
		for _, k := range keys {
			if k == want {
				return true
			}
		}
		return false
	}

	cases := []struct {
		tool    string
		want    []string
		notWant []string
	}{
		// 白名单里的只读工具：verifier/redteam/creative 绑上，既有 agent 不动。
		{"list_findings", []string{"mainagent", "planner", "worker", "pentest", "verifier", "redteam"}, nil},
		{"list_assets", []string{"mainagent", "worker", "auto", "pentest", "creative"}, nil},
		{"graph_overview", []string{"mainagent", "planner", "creative"}, nil},
		// 白名单里的跨任务只读工具：verifier 绑上。
		{"get_finding_traffic", []string{"auto", "verifier"}, nil},
		{"traffic_search", []string{"worker", "verifier"}, nil},
		// 改写共享状态的工具：误绑也要被收回。
		{"report_finding", []string{"mainagent", "planner", "worker", "auto", "pentest"}, []string{"verifier", "curator", "redteam", "creative"}},
		{"add_intent", []string{"mainagent", "planner"}, []string{"curator", "verifier", "redteam", "creative"}},
		{"bind_finding_traffic", []string{"reporter"}, []string{"creative", "curator", "verifier", "redteam"}},
		// 可执行的自定义工具：verifier 不许有，redteam（证伪者）可以有。
		{"probe_http_cmd", []string{"redteam"}, []string{"verifier", "curator", "creative"}},
	}
	for _, c := range cases {
		got := agentsOf(c.tool)
		for _, w := range c.want {
			if !has(got, w) {
				t.Errorf("工具 %s 少了绑定 %s（实际 %v）", c.tool, w, got)
			}
		}
		for _, nw := range c.notWant {
			if has(got, nw) {
				t.Errorf("工具 %s 不该绑 %s（实际 %v）", c.tool, nw, got)
			}
		}
	}
}

// TestAdvisoryAgentsHoldNoSharedStateWriter 保证协调之后，四个顾问 agent 手里没有
// 任何一个改写平台共享状态的工具。
func TestAdvisoryAgentsHoldNoSharedStateWriter(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()

	tools, err := d.ListTools()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range AdvisoryAgentToolPolicies() {
		for _, row := range tools {
			if !containsAgentKey(row.Agents, p.Agent) {
				continue
			}
			if p.Denies(row.Key, row.Kind) {
				t.Errorf("顾问 agent %s 绑上了被拒绝的工具 %s", p.Agent, row.Key)
			}
		}
	}
}

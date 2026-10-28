package agent

import (
	"context"
	"testing"

	acperm "github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
)

// 验收关键：AugmentTools 是全部 agent 的唯一工具收口（worker:381 / planner:377 /
// mainagent:129 / chat:123 都经它），策略装饰器必须挂在这里才谈得上覆盖完整。
// 若将来有人把 enforceToolPolicy 挪走或漏挂某条装配路径，本用例立刻失败。
func TestAugmentToolsWrapsEveryToolWithPolicy(t *testing.T) {
	for _, agentKey := range []string{"worker", "planner", "mainagent", "auto", "pentest", "reporter", "retester", "goals"} {
		t.Run(agentKey, func(t *testing.T) {
			tools, _, cleanup := AugmentTools(context.Background(), agentKey, actool.DefaultTools())
			defer cleanup()
			if len(tools) == 0 {
				t.Fatalf("%s 工具表为空, 用例失去意义", agentKey)
			}
			for _, tl := range tools {
				if !IsPolicyWrapped(tl) {
					t.Fatalf("%s 的工具 %q 未被策略装饰器包住——越界面防线在此漏掉", agentKey, tl.Name())
				}
			}
		})
	}
}

// ToolAugment 追加进来的工具（skill/MCP/host）最容易漏包：它们通常没有 tools 表
// 行，在 ToolResolve 里走 "no row → untouched" 分支。用一个模拟的 ToolAugment 验证
// 这条路径同样被覆盖。
func TestPolicyWrapsToolAugmentExtras(t *testing.T) {
	origAugment := ToolAugment
	t.Cleanup(func() { ToolAugment = origAugment })

	ToolAugment = func(ctx context.Context, agentKey string) ([]actool.CoreTool, DeferredInfo, func()) {
		return []actool.CoreTool{actool.NewSleep()}, DeferredInfo{}, func() {}
	}
	ToolResolve = nil // 模拟未接 DB 时 ToolResolve 为空的情形

	tools, _, cleanup := AugmentTools(context.Background(), "worker", actool.DefaultTools())
	defer cleanup()
	if !hasTool(tools, "sleep") {
		t.Fatal("ToolAugment 追加的 sleep 未出现在工具表, 用例前提不成立")
	}
	for _, tl := range tools {
		if !IsPolicyWrapped(tl) {
			t.Fatalf("ToolAugment 追加的工具 %q 未被策略包住", tl.Name())
		}
	}
}

func hasTool(tools []actool.CoreTool, name string) bool {
	for _, tl := range tools {
		if tl.Name() == name {
			return true
		}
	}
	return false
}

// 决定性验证：ARTEX 六个 agent 会话全部跑 permission.ModeBypass，而 norma 的
// Evaluate 在 Stage 2 对 ModeBypass 直接 return Allowed()。这里直接走一遍真实的
// Evaluate，确认 Stage 1 的工具级 Deny 先于该短路生效——否则整套加固在生产配置下
// 等于没上。
func TestPolicyDeniesUnderModeBypass(t *testing.T) {
	tool := enforceToolPolicy(actool.NewRead())
	input := []byte(`{"file_path":"/app/jwt.key"}`)
	pc := acperm.Context{Mode: acperm.ModeBypass, WorkingDir: "/app/data/sessions/x"}

	dec := tool.CheckPermissions(context.Background(), input, pc)
	if dec.Behavior != acperm.Deny {
		t.Fatalf("策略层应返回 Deny, 实得 %v（AskUser 在 ModeBypass 下等同放行）", dec.Behavior)
	}

	got := acperm.Evaluate(context.Background(), acperm.Request{
		ToolName:     "Read",
		Input:        input,
		IsReadOnly:   true,
		ToolDecision: dec,
		Ctx:          pc,
	})
	if got.Behavior != acperm.Deny {
		t.Fatalf("ModeBypass 下 Evaluate 未采纳工具级 Deny, 防线被 Stage 2 短路绕过: %v", got.Behavior)
	}
}

// 入参里的相对路径绕过必须被拒：短路表是字符串预筛，挡不住经 shell 展开
// 才现形的路径；这一层用真实路径解析兜住。
//
// 工作目录是 /app/data/sessions/x 时，「../../../jwt.key」正是落到 /app/jwt.key
// 的那三层回溯 —— 与真实部署同形。
func TestPolicyDeniesRelativeSecretPathInBash(t *testing.T) {
	tool := enforceToolPolicy(actool.NewBash())
	input := []byte(`{"command":"cat ../../../jwt.key"}`)
	pc := acperm.Context{Mode: acperm.ModeBypass, WorkingDir: "/app/data/sessions/x"}

	dec := tool.CheckPermissions(context.Background(), input, pc)
	if dec.Behavior != acperm.Deny {
		t.Fatalf("相对路径指向平台密钥应被拒，实得 %v", dec.Behavior)
	}
}

// 命令替换包裹的同一形态也要被拒：路径字面量就在入参里，只是被 $( ) 包着。
func TestPolicyDeniesCommandSubstitutedSecretPath(t *testing.T) {
	tool := enforceToolPolicy(actool.NewBash())
	input := []byte(`{"command":"cat $(realpath ../../../jwt.key)"}`)
	pc := acperm.Context{Mode: acperm.ModeBypass, WorkingDir: "/app/data/sessions/x"}

	dec := tool.CheckPermissions(context.Background(), input, pc)
	if dec.Behavior != acperm.Deny {
		t.Fatalf("命令替换包裹的路径绕过应被拒，实得 %v", dec.Behavior)
	}
}

// 日常命令不得被新判据误伤：一刀切会让平台不可用。
func TestPolicyAllowsNormalBashCommand(t *testing.T) {
	tool := enforceToolPolicy(actool.NewBash())
	pc := acperm.Context{Mode: acperm.ModeBypass, WorkingDir: "/app/data/sessions/x"}
	for _, cmd := range []string{
		"ls -la",
		"cat ./notes.md",
		"curl -s http://127.0.0.1:8787/api/health",
		"nmap -sV 10.0.0.1",
		"cat /etc/hosts",
	} {
		input := []byte(`{"command":"` + cmd + `"}`)
		dec := tool.CheckPermissions(context.Background(), input, pc)
		if dec.Behavior == acperm.Deny {
			t.Fatalf("命令 %q 被误伤", cmd)
		}
	}
}

// 工具层的拒绝必须留痕：它在 hook 之前求值，hook 不会被调用，
// 拿不到短路表那条留痕路径 —— 所以必须由 server 注入的记录器补上。
func TestPolicyRecordsToolLayerDenial(t *testing.T) {
	orig := ToolDenialRecorder
	t.Cleanup(func() { ToolDenialRecorder = orig })

	var gotTool, gotReason string
	var called int
	ToolDenialRecorder = func(_ context.Context, toolName, reason string, _ []byte) {
		called++
		gotTool, gotReason = toolName, reason
	}

	tool := enforceToolPolicy(actool.NewBash())
	input := []byte(`{"command":"cat ../../../jwt.key"}`)
	pc := acperm.Context{Mode: acperm.ModeBypass, WorkingDir: "/app/data/sessions/x"}
	dec := tool.CheckPermissions(context.Background(), input, pc)
	if dec.Behavior != acperm.Deny {
		t.Fatalf("应被拒绝，实得 %v", dec.Behavior)
	}
	if called != 1 {
		t.Fatalf("工具层拒绝未触发留痕（调用 %d 次）", called)
	}
	if gotTool != "Bash" {
		t.Fatalf("留痕的工具名不对：%q", gotTool)
	}
	if gotReason == "" {
		t.Fatal("留痕缺少原因")
	}
}

// 放行的调用不得触发留痕：否则审计链会被正常操作灌满。
func TestPolicyDoesNotRecordAllowedCall(t *testing.T) {
	orig := ToolDenialRecorder
	t.Cleanup(func() { ToolDenialRecorder = orig })

	called := 0
	ToolDenialRecorder = func(context.Context, string, string, []byte) { called++ }

	tool := enforceToolPolicy(actool.NewBash())
	input := []byte(`{"command":"ls -la"}`)
	pc := acperm.Context{Mode: acperm.ModeBypass, WorkingDir: "/app/data/sessions/x"}
	tool.CheckPermissions(context.Background(), input, pc)
	if called != 0 {
		t.Fatalf("放行的调用不应留痕（调用 %d 次）", called)
	}
}

func TestPolicyAllowsNormalPathUnderModeBypass(t *testing.T) {
	tool := enforceToolPolicy(actool.NewRead())
	pc := acperm.Context{Mode: acperm.ModeBypass, WorkingDir: "/app/data/sessions/x"}
	for _, p := range []string{
		"/app/data/sessions/x/notes.md",
		"/tmp/nmap.xml",
		"/etc/passwd",
	} {
		input := []byte(`{"file_path":"` + p + `"}`)
		got := acperm.Evaluate(context.Background(), acperm.Request{
			ToolName:     "Read",
			Input:        input,
			IsReadOnly:   true,
			ToolDecision: tool.CheckPermissions(context.Background(), input, pc),
			Ctx:          pc,
		})
		if got.Behavior == acperm.Deny {
			t.Fatalf("渗透常用路径 %s 被误伤", p)
		}
	}
}

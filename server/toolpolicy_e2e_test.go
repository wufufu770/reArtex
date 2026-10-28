package server

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/intercept"
	acperm "github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
)

// 端到端：真实装配（AugmentTools 包装）→ 工具层判定 → 拒绝 → 审计链留痕。
//
// 单测覆盖了各函数逻辑，但「链路里是否真的生效、留痕是否真的落库、链有没有断」
// 一直没在真实装配下验过 —— 这条用例把整条链走一遍。
//
// 工作目录 /app/data/sessions/x 时，「../../../jwt.key」正好落到 /app/jwt.key：
// 三层回溯，与真实部署同形。
func TestToolLayerSecretPathDeniedAndAudited(t *testing.T) {
	dsn, _, err := db.DSN()
	if err != nil {
		t.Skip("no test database configured")
	}
	d, err := db.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })

	ic := intercept.New(d)
	wireToolDenialRecorder(ic)
	t.Cleanup(func() { agent.ToolDenialRecorder = nil })

	tools, _, cleanup := agent.AugmentTools(context.Background(), "worker", []actool.CoreTool{actool.NewBash()})
	defer cleanup()

	var bash actool.CoreTool
	for _, tl := range tools {
		if tl.Name() == "Bash" {
			bash = tl
		}
	}
	if bash == nil {
		t.Fatal("工具表里没有 Bash，用例前提不成立")
	}

	input := json.RawMessage(`{"command":"cat ../../../jwt.key"}`)
	pc := acperm.Context{Mode: acperm.ModeBypass, WorkingDir: "/app/data/sessions/x"}
	dec := bash.CheckPermissions(context.Background(), input, pc)
	if dec.Behavior != acperm.Deny {
		t.Fatalf("相对路径指向平台密钥应被拒，实得 %v", dec.Behavior)
	}

	var action, toolName string
	err = d.QueryRow(`SELECT action, tool_name FROM audit_events WHERE tool_name='Bash' ORDER BY seq DESC LIMIT 1`).
		Scan(&action, &toolName)
	if err != nil {
		t.Fatalf("工具层拒绝未落审计链: %v", err)
	}
	if action != "denied" {
		t.Fatalf("审计行动作应为 denied，得到 %q", action)
	}
	if broken, err := d.VerifyAuditChain(); err != nil {
		t.Fatalf("审计链断裂在 seq=%d: %v", broken, err)
	} else if broken != 0 {
		t.Fatalf("审计链断裂在 seq=%d", broken)
	}
}

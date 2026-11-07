package server

// B6：连续拒绝升级的单测（计数器 + 升级动作）。

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/db"
)

func TestDenialEscalationThreshold(t *testing.T) {
	d := newDenialEscalation()
	for i := 1; i < denialEscalationThreshold; i++ {
		if d.recordDenial("17", "Bash", "out_of_scope") {
			t.Fatalf("第 %d 次不应触发", i)
		}
	}
	if !d.recordDenial("17", "Bash", "out_of_scope") {
		t.Fatal("第 3 次同类拒绝应触发升级")
	}
	// 升级后计数清零（onToolDenial 的行为）：再来 2 次不触发，第 3 次触发。
	d.resetTool("17", "Bash", "out_of_scope")
	for i := 0; i < denialEscalationThreshold-1; i++ {
		if d.recordDenial("17", "Bash", "out_of_scope") {
			t.Fatal("清零后阈值前不应触发")
		}
	}
	if !d.recordDenial("17", "Bash", "out_of_scope") {
		t.Fatal("清零后第 3 次应触发")
	}
	// 不同 reason_code 独立计数。
	d2 := newDenialEscalation()
	_ = d2.recordDenial("17", "Bash", "out_of_scope")
	_ = d2.recordDenial("17", "Bash", "out_of_scope")
	if d2.recordDenial("17", "Bash", "normalizer_unavailable") {
		t.Fatal("不同 reason_code 不应共用计数")
	}
}

func TestOnToolDenialBlocksRunningIntent(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v)", err)
	}
	defer m.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	engine := NewEngine(m)
	s := &Server{m: m, engine: engine, ctx: ctx, denials: newDenialEscalation()}

	created, err := m.CreateTaskWithOptions("B6 升级路径", "测试目标", db.TaskCreateOptions{})
	if err != nil {
		t.Fatalf("建任务失败: %v", err)
	}
	taskID := created.ID
	defer func() { _, _ = m.DeleteTask(taskID, DeleteTaskOptions{}) }()

	// 造一个 running 意图。
	store := m.pg.Exploration(created.ExpID)
	origin, _ := store.OriginFactID()
	iid, err := store.AddIntent(map[string]any{"summary": "被拒的意图"}, 0, nil, "test")
	if err != nil {
		t.Fatalf("建意图失败: %v", err)
	}
	_ = origin
	if _, err := store.CompareAndSetIntentState(iid, "open", "running"); err != nil {
		t.Fatalf("置 running: %v", err)
	}

	for i := 0; i < denialEscalationThreshold; i++ {
		s.onToolDenial(taskID, "Bash", fmt.Sprintf("Blocked by hook: [SoD] 拒绝执行：reason_code=out_of_scope；测试拒绝 %d", i))
	}
	var state string
	if err := m.pg.QueryRow(`SELECT state FROM exploration_nodes WHERE id=$1 AND exploration_id=$2`, iid, created.ExpID).Scan(&state); err != nil {
		t.Fatalf("读意图状态: %v", err)
	}
	if state != "blocked" {
		t.Fatalf("意图应转 blocked，got %q", state)
	}
	var reason string
	_ = m.pg.QueryRow(`SELECT COALESCE(blocked_reason,'') FROM exploration_nodes WHERE id=$1`, iid).Scan(&reason)
	if !strings.Contains(reason, "out_of_scope") {
		t.Fatalf("blocked_reason 应含拒绝摘要，got %q", reason)
	}
}

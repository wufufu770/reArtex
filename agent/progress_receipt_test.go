package agent

import (
	"testing"

	"github.com/Autumn-27/artex/db"
)

// TestProgressActivityKind pins the value written into activity.kind.
//
// 为什么钉它：activity.kind 是自由文本列（TEXT，无 CHECK）⇒ 写与筛必须用同一个
// 常量，否则会出现「写用 progress、筛用 progess」这种只靠眼睛才能发现的漂移。
func TestProgressActivityKind(t *testing.T) {
	if ProgressActivityKind != "progress" {
		t.Fatalf("进度回执的 kind = %q，期望 progress", ProgressActivityKind)
	}
}

// TestMemoryScopeValidity pins the scope contract.
//
// 记忆的 scope 键决定「这条记忆属于谁」⇒ 非法 scope 必须被拒，而不是退化成
// 一个全局记忆池（那会让所有任务互相污染）。
func TestMemoryScopeValidity(t *testing.T) {
	taskScope := MemoryScopeForTask(1)
	if !taskScope.Valid() {
		t.Fatal("任务作用域应当有效")
	}
	if (MemoryScopeForTask(0)).Valid() {
		t.Fatal("id<=0 的作用域应当无效")
	}
	if (MemoryScopeForSession(0)).Valid() {
		t.Fatal("id<=0 的会话作用域应当无效")
	}
	if (db.MemoryScope{Type: "whatever", ID: 1}).Valid() {
		t.Fatal("未知类型的 scope 应当无效")
	}
}

// TestMemoryPromptBlockWithoutDB pins the fail-soft behaviour.
//
// 记忆是**辅助**：取不到就当没有，绝不能因为记忆层故障让整个 agent 挂掉。
func TestMemoryPromptBlockWithoutDB(t *testing.T) {
	if got := MemoryPromptBlock(nil, MemoryScopeForTask(1)); got != "" {
		t.Fatalf("无库时应返回空串，实际 %q", got)
	}
	if got := MemoryPromptBlock(nil, db.MemoryScope{}); got != "" {
		t.Fatalf("非法 scope 应返回空串，实际 %q", got)
	}
}

// TestFindingVerdictValues pins the three verdict values.
//
// 裁决三态必须与 db 层 finding 节点的三个状态一一对应 ⇒ 漂移会让裁决落到
// 「不认识的取值」上，而 db 层会拒掉它。
func TestFindingVerdictValues(t *testing.T) {
	for _, v := range []string{FindingVerdictConfirmed, FindingVerdictDismissed, FindingVerdictRework} {
		if !findingVerdictValues[v] {
			t.Fatalf("裁决值 %q 未被认可", v)
		}
	}
	if findingVerdictValues["approve"] {
		t.Fatal("不应接受未定义的裁决值")
	}
	if FindingVerdictConfirmed != string(db.FindingValidationConfirmed) ||
		FindingVerdictDismissed != string(db.FindingValidationDismissed) ||
		FindingVerdictRework != string(db.FindingValidationRework) {
		t.Fatal("agent 侧与 db 侧的裁决值漂移了")
	}
}

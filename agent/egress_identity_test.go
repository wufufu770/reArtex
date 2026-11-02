package agent

import (
	"context"
	"testing"
)

// TestEgressTaskIDPrefersExplicitTask pins that a task-scoped run keeps its own
// task identity even when the context carries a different one.
func TestEgressTaskIDPrefersExplicitTask(t *testing.T) {
	ctx := WithRunInfo(context.Background(), RunInfo{TaskID: 7})
	if got := egressTaskID(ctx, 42); got != 42 {
		t.Fatalf("显式 taskID 优先: got %d, want 42", got)
	}
}

// TestEgressTaskIDFallsBackToRunInfo pins that a chat agent in a group session
// (no explicit taskID parameter) still gets the task identity from the context —
// otherwise group members' egress would be unattributed and escape scope checks.
func TestEgressTaskIDFallsBackToRunInfo(t *testing.T) {
	ctx := WithRunInfo(context.Background(), RunInfo{TaskID: 7})
	if got := egressTaskID(ctx, 0); got != 7 {
		t.Fatalf("应回落到 ctx 里的任务身份: got %d, want 7", got)
	}
}

// TestEgressTaskIDZeroWhenNoTask pins that a standalone chat session stays
// unattributed: 没有任务就没有范围边界，不能伪造身份。
func TestEgressTaskIDZeroWhenNoTask(t *testing.T) {
	ctx := WithRunInfo(context.Background(), RunInfo{SessionID: "conv-3"})
	if got := egressTaskID(ctx, 0); got != 0 {
		t.Fatalf("没有任务时必须返回 0: got %d", got)
	}
}

// TestEgressTaskIDMergedRunInfo pins that the merge path (WithRunInfoMerged)
// keeps TaskID — the chat agent merges a session id into an existing RunInfo,
// and dropping TaskID there would silently un-scope every group member.
func TestEgressTaskIDMergedRunInfo(t *testing.T) {
	ctx := WithRunInfo(context.Background(), RunInfo{TaskID: 7, ExplorationID: 9})
	ctx = WithRunInfoMerged(ctx, RunInfo{SessionID: "conv-3"})
	if got := egressTaskID(ctx, 0); got != 7 {
		t.Fatalf("合并 RunInfo 后必须保住 TaskID: got %d, want 7", got)
	}
}

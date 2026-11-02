package agent

import (
	"context"

	"github.com/Autumn-27/artex/db"
)

// RunInfo identifies WHICH run a tool call belongs to. Tool assembly only receives
// (ctx, agentKey) — the task/exploration ids live in the caller's arguments, not the
// ctx — so anything wired at assembly time (currently the Skill ledger in
// server/assembly.go) has no way to attribute a call to a task. Each run attaches
// its own RunInfo before calling AugmentTools; the wiring closure reads it once and
// captures it, so per-run attribution stays correct without threading parameters
// through the tool layer. Same pattern as TaskClock (see taskclock.go).
//
// Zero value = attribution unknown; every consumer must treat it as optional.
type RunInfo struct {
	TaskID        int64  // task registry id; 0 for non-task runs (chat sessions)
	ExplorationID int64  // exploration id; 0 when unknown
	IntentID      int64  // worker's intent node; 0 for planner/mainagent/chat
	SessionID     string // chat conversation id; empty for task runs
}

// explorationID reads a store's exploration id, tolerating a nil store (planner and
// worker runs can be driven without one in tests).
func explorationID(ts *db.ExplorationStore) int64 {
	if ts == nil {
		return 0
	}
	return ts.ID()
}

type runInfoKey struct{}

// WithRunInfo attaches run attribution to ctx.
func WithRunInfo(ctx context.Context, ri RunInfo) context.Context {
	return context.WithValue(ctx, runInfoKey{}, ri)
}

// RunInfoFrom reads the RunInfo (zero value if none attached).
func RunInfoFrom(ctx context.Context) RunInfo {
	if v, ok := ctx.Value(runInfoKey{}).(RunInfo); ok {
		return v
	}
	return RunInfo{}
}

// egressTaskID 统一「本次运行的任务身份」口径，供出网范围判定使用。
//
// 为什么要有这个函数：planner / 主 agent 从函数参数拿到 taskID，chat agent 没有这个
// 参数（纯对话会话本就不属于任何任务），只能从 ctx 的 RunInfo 取。两套取法散在三个
// 调用点里，容易漏掉一个 —— 漏掉的后果是那个角色的出网**不带身份**，于是录制代理
// 按「未归属」放行，它就不再受任务范围校验（这正是 egress_governance.go 清单里
// 「只有 worker 带了令牌」那条的成因）。
//
// 取值优先级：
//   - 显式参数 taskID > 0 ⇒ 用它（任务内的运行）。
//   - 否则看 ctx 的 RunInfo.TaskID：群聊成员会话带着任务身份（一个群绑一个任务），
//     纯独立聊天会话没有任务 ⇒ 返回 0 ⇒ 不带令牌 ⇒ 属「未归属的出网」。
//
// 返回 0 时**不要**伪造身份：没有任务就没有范围边界，装一个假身份比不带更危险。
func egressTaskID(ctx context.Context, taskID int64) int64 {
	if taskID > 0 {
		return taskID
	}
	return RunInfoFrom(ctx).TaskID
}

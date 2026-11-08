package tool

// Durable background tasks: the process-level registry and the host hook that
// lets a background command outlive the session that started it.
//
// Why this exists: the SDK binds a background command's lifetime to the Session
// that spawned it. Session.Close() -> Manager.Cleanup() -> KillAll(), so a
// command the agent was told it could "read later" is already dead by the time
// the agent's run ends — the promise is silently broken, and the agent later
// reads a dead process's truncated output (or nothing at all).
//
// The fix has two halves that must land together:
//   - the HOST chooses where the output lives (a directory the host owns, with
//     the host's own retention policy) instead of os.TempDir() keyed by a
//     per-session UUID;
//   - the host is told, through TaskObserver, when a task becomes backgrounded
//     and when it finishes, so the host can keep its own ledger and kill the
//     process later.
//
// The first half alone leaks processes: an output file that outlives its run is
// exactly as bad as a process that does, and the host is the only party that
// knows when the task itself is over.

import (
	"context"
	"sync"
)

// TaskObserver is the host hook for durable background tasks.
//
// The SDK owns the processes; the host owns the ledger. A host that wants
// background commands to survive the session that started them installs an
// observer with SetTaskObserver.
//
// Both callbacks may run on SDK goroutines: OnBackgrounded runs on the
// tool-call path, OnFinished runs on the process-watch goroutine. Neither may
// block for long, and neither may call back into the Manager.
type TaskObserver interface {
	// OnBackgrounded is called once, when a task is marked backgrounded, and
	// decides whether the task is durable.
	//
	// The returned value is the durable owner key — the host's identity for
	// whatever the task belongs to (an ARTEX task id, say). The key is what a
	// later session uses to find the task again, so it must be stable across
	// sessions.
	//
	// A non-empty key means "this task is durable": the SDK files it in the
	// process-level durable registry and lets it outlive the session. An empty
	// key means "not durable": the task keeps the pre-patch behaviour and dies
	// with its session.
	//
	// The host gets to write its own bookkeeping BEFORE answering, so a host
	// that cannot record the task (no ledger row, no database) answers with
	// the empty key and the task stays session-bound. That ordering is what
	// keeps the two halves of the contract together: a task can only outlive
	// its session if somebody is going to reap it later.
	OnBackgrounded(ctx context.Context, info TaskInfo) string

	// OnFinished is called once, when a backgrounded task reaches a terminal
	// state (completed, failed or killed).
	OnFinished(ctx context.Context, info TaskInfo)
}

var (
	observerMu sync.RWMutex
	observer   TaskObserver
)

// SetTaskObserver installs the process-wide observer; nil removes it.
//
// Process-wide rather than per-Session because the durable registry is
// process-wide too: a task that outlives its session must be findable by the
// next session, and the next session holds no handle on the observer that
// started it.
func SetTaskObserver(o TaskObserver) {
	observerMu.Lock()
	observer = o
	observerMu.Unlock()
}

// currentTaskObserver returns the installed observer, or nil when none is set.
func currentTaskObserver() TaskObserver {
	observerMu.RLock()
	o := observer
	observerMu.RUnlock()
	return o
}

// DurableRegistry is the process-level index of background tasks that outlived
// their session.
//
// Keyed by the host's owner key first: the host asks "what is still running for
// task 42?" and gets an answer without knowing which session started it. The
// SDK's own task ids are per-session ("task_1" in every session), so the
// (owner, session, id) triple is the only unambiguous handle.
type DurableRegistry struct {
	mu      sync.Mutex
	byOwner map[string][]*Task
}

var durableRegistry = &DurableRegistry{byOwner: map[string][]*Task{}}

// DurableTasks returns the process-level registry.
func DurableTasks() *DurableRegistry { return durableRegistry }

// Add files a task under its owner key. Re-adding the same task is a no-op.
func (r *DurableRegistry) Add(owner string, t *Task) {
	if owner == "" || t == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.byOwner[owner] {
		if e == t {
			return
		}
	}
	r.byOwner[owner] = append(r.byOwner[owner], t)
}

// Remove drops a task from the registry.
func (r *DurableRegistry) Remove(owner, sessionID, id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	kept := r.byOwner[owner][:0]
	for _, e := range r.byOwner[owner] {
		if e.sessionID == sessionID && e.ID == id {
			continue
		}
		kept = append(kept, e)
	}
	if len(kept) == 0 {
		delete(r.byOwner, owner)
		return
	}
	r.byOwner[owner] = kept
}

// List returns the tasks filed under one owner key, in the order they were
// filed.
func (r *DurableRegistry) List(owner string) []TaskInfo {
	r.mu.Lock()
	ts := make([]*Task, len(r.byOwner[owner]))
	copy(ts, r.byOwner[owner])
	r.mu.Unlock()
	out := make([]TaskInfo, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.info())
	}
	return out
}

// Owners returns the owner keys that currently have at least one task.
func (r *DurableRegistry) Owners() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.byOwner))
	for k := range r.byOwner {
		out = append(out, k)
	}
	return out
}

// Kill terminates a durable task's process tree and reports whether a task
// matched. The (session, id) pair disambiguates the per-session task ids.
func (r *DurableRegistry) Kill(owner, sessionID, id string) bool {
	r.mu.Lock()
	var hit *Task
	for _, e := range r.byOwner[owner] {
		if e.sessionID == sessionID && e.ID == id {
			hit = e
			break
		}
	}
	r.mu.Unlock()
	if hit == nil {
		return false
	}
	hit.manager.terminate(hit, TaskKilled, false)
	return true
}

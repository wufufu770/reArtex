package tool

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/Autumn-27/norma/tool/privdrop"
)

// Background task execution.
//
// A Manager owns the set of background processes started during a Session. Each
// task runs detached from the per-turn context (so it outlives the agent turn
// that launched it), streams its combined stdout/stderr to a per-task file on
// disk, and — when it finishes — enqueues a notification the harness drains into
// the conversation. The model is told only the task id, status, and output-file
// path; it reads the actual output on demand (Read or the TaskOutput tool).

// TaskStatus is the lifecycle state of a background task.
type TaskStatus string

const (
	TaskRunning   TaskStatus = "running"
	TaskCompleted TaskStatus = "completed"
	TaskFailed    TaskStatus = "failed"
	TaskKilled    TaskStatus = "killed"
)

// Terminal reports whether the status is final (the process has exited).
func (s TaskStatus) Terminal() bool {
	return s == TaskCompleted || s == TaskFailed || s == TaskKilled
}

// TaskKind distinguishes a one-shot background command from a long-lived monitor.
type TaskKind string

const (
	KindBash    TaskKind = "bash"
	KindMonitor TaskKind = "monitor"
)

// Tunables for the on-disk output store.
const (
	// maxTaskOutputBytes caps a single task's output file; a task that exceeds it
	// is terminated to keep a runaway process from filling the disk.
	maxTaskOutputBytes = 50 << 20 // 50 MiB
	// staleSessionTTL bounds how long another session's task directory survives
	// before it is garbage-collected on the next Manager creation.
	staleSessionTTL = 24 * time.Hour
)

// Task is a single background process tracked by a Manager. Its mutable fields
// are guarded by Manager.mu; read snapshots through Manager methods (which copy
// into TaskInfo).
type Task struct {
	ID          string
	Kind        TaskKind
	Command     string
	Description string
	Status      TaskStatus
	ExitCode    int
	StartTime   time.Time
	EndTime     time.Time
	OutputPath  string

	// internal, guarded by Manager.mu
	backgrounded bool // surfaced as a background task: notify on exit
	notified     bool // completion already enqueued into pending
	oversize     bool // terminated by the size watchdog
	cmd          *exec.Cmd
	done         chan struct{} // closed by watch when the process exits

	// Durable-task bookkeeping. sessionID/owner identify the task to a host
	// that keeps its own ledger; ctx carries the host context of the run that
	// started it, which is what the host observer needs in order to attribute
	// the task. ctx is only ever read, never cancelled, by the SDK.
	sessionID    string
	owner        string
	durable      bool
	registered   bool // the host observer already knows about this task
	hostNotified bool // the host observer already knows how it ended
	ctx          context.Context
	pid          int
	manager      *Manager
}

func (t *Task) info() TaskInfo {
	return TaskInfo{
		ID: t.ID, Kind: t.Kind, Command: t.Command, Description: t.Description,
		Status: t.Status, ExitCode: t.ExitCode,
		StartTime: t.StartTime, EndTime: t.EndTime,
		OutputPath: t.OutputPath, Backgrounded: t.backgrounded,
		SessionID: t.sessionID, Owner: t.owner, PID: t.pid, Durable: t.durable,
	}
}

// TaskInfo is an immutable snapshot of a task for safe reads outside the lock.
type TaskInfo struct {
	ID           string
	Kind         TaskKind
	Command      string
	Description  string
	Status       TaskStatus
	ExitCode     int
	StartTime    time.Time
	EndTime      time.Time
	OutputPath   string
	Backgrounded bool

	// SessionID, Owner and PID identify the task to a host that keeps its own
	// ledger: Owner is the host's owner key (see TaskObserver), SessionID the
	// SDK session that started it, PID the process id of the spawned command
	// (0 before the process starts).
	SessionID string
	Owner     string
	PID       int
	// Durable reports whether the task was handed to the process-level durable
	// registry, i.e. whether it survives the end of its session.
	Durable bool
}

// Notification is surfaced to the model when a backgrounded task exits.
type Notification struct {
	TaskID     string
	Kind       TaskKind
	Status     TaskStatus
	OutputPath string
	Summary    string
}

// String renders the notification as the <task-notification> block injected into
// the conversation. It deliberately carries only the path, not the output.
func (n Notification) String() string {
	return fmt.Sprintf("<task-notification>\n"+
		"  <task-id>%s</task-id>\n"+
		"  <task-type>%s</task-type>\n"+
		"  <status>%s</status>\n"+
		"  <output-file>%s</output-file>\n"+
		"  <summary>%s</summary>\n"+
		"</task-notification>",
		n.TaskID, n.Kind, n.Status, n.OutputPath, n.Summary)
}

// SpawnSpec describes a process to launch in the background.
type SpawnSpec struct {
	Command     string
	Description string
	Kind        TaskKind
	WorkingDir  string
	// Env holds extra "KEY=VALUE" entries layered onto the process environment for
	// this command (see ToolContext.Env). Empty still applies nonInteractiveEnv.
	Env []string
	// Ctx is the caller's context. The SDK does not cancel it and does not let
	// it govern the process lifetime; it is carried along so the host observer
	// (see TaskObserver) can attribute the task to the run that started it.
	Ctx context.Context
}

// nonInteractiveEnv are defaults every spawned shell gets, whether or not the
// host passes any env of its own: a pager must never block a non-interactive
// shell waiting for a terminal nobody is watching, and Debian package frontends
// must never prompt. These deliberately override values inherited from the
// agent's own process — an operator who exports PAGER=less for their shell would
// otherwise hand every child a pager that hangs. A host that names the same key
// explicitly still wins (see withEnv).
var nonInteractiveEnv = []string{
	"PAGER=cat",
	"GIT_PAGER=cat",
	"SYSTEMD_PAGER=cat",
	"DEBIAN_FRONTEND=noninteractive",
}

// withEnv builds the child environment, in precedence order:
//
//	process environment  <  nonInteractiveEnv defaults  <  the host's extra vars
//
// Each layer is applied by DROPPING the earlier entry for that key rather than
// appending a duplicate. os/exec's own dedup would resolve duplicates in favour
// of the last occurrence, so appending alone would work for a child it spawns;
// dropping makes the slice correct on its own terms, so it also reads correctly
// anywhere it is inspected, logged, or handed to something that does not dedup.
// (mcp/stdio.go relies on exec's last-wins dedup and is fine as it is.)
//
// The result is never nil — the defaults always apply.
func withEnv(extra []string) []string {
	extraKeys := make(map[string]bool, len(extra))
	for _, kv := range extra {
		if k := envKey(kv); k != "" {
			extraKeys[k] = true
		}
	}
	drop := make(map[string]bool, len(extraKeys)+len(nonInteractiveEnv))
	for k := range extraKeys {
		drop[k] = true
	}
	for _, kv := range nonInteractiveEnv {
		if k := envKey(kv); k != "" && !extraKeys[k] {
			drop[k] = true
		}
	}
	base := os.Environ()
	out := make([]string, 0, len(base)+len(nonInteractiveEnv)+len(extra))
	for _, kv := range base {
		if k := envKey(kv); k != "" && drop[k] {
			continue
		}
		out = append(out, kv)
	}
	for _, kv := range nonInteractiveEnv {
		if k := envKey(kv); k != "" && extraKeys[k] {
			continue // host override wins
		}
		out = append(out, kv)
	}
	return append(out, extra...)
}

// envKey extracts the KEY of a "KEY=value" entry ("" when malformed).
func envKey(kv string) string {
	if i := strings.IndexByte(kv, '='); i > 0 {
		return kv[:i]
	}
	return ""
}

// Manager owns the background tasks of one Session.
type Manager struct {
	mu      sync.Mutex
	tasks   map[string]*Task
	order   []string
	pending []Notification
	dir     string
	seq     int

	// sessionID names this session; base is the directory that holds every
	// session's task directory (see baseTaskDir).
	sessionID string
	base      string

	rootCtx    context.Context
	rootCancel context.CancelFunc

	// interactive PTY shell sessions (see shell_session.go). Separate map + mutex
	// from the file-based tasks above: sessions are persistent, stdin-driven, and
	// keep an in-memory ring buffer rather than streaming to a file.
	sessMu   sync.Mutex
	sessions map[string]*ptySession
	sessSeq  int
}

// baseTaskDir returns the base directory that holds background-task output.
//
// The host chooses it: NORMA_TASK_DIR points at a directory the host owns and
// whose retention policy the host controls. Only when the host names nothing
// does the SDK fall back to its old temp-dir location, so a host that never
// opts in keeps the previous behaviour exactly.
//
// Why a host-chosen base rather than os.TempDir(): a background command that
// outlives its session needs its output to outlive it too, and os.TempDir() is
// the OS's to reclaim. The old comment on Cleanup() said exactly this — that
// making the output durable "needs spill output to live in a host-chosen
// directory with its own retention policy, not in os.TempDir() keyed by a
// per-session UUID; that is a design change, not a change to this function".
func baseTaskDir() string {
	if v := strings.TrimSpace(os.Getenv("NORMA_TASK_DIR")); v != "" {
		return v
	}
	return filepath.Join(os.TempDir(), "norma")
}

// NewManager creates the per-session task directory under the host-chosen
// base (see baseTaskDir) and garbage-collects stale directories left by
// previous sessions.
func NewManager(sessionID string) (*Manager, error) {
	return NewManagerIn(sessionID, "")
}

// NewManagerIn creates a Manager whose task directory lives under base instead
// of the host-chosen default. An empty base means "use baseTaskDir()".
//
// The layout is <base>/<sessionID>/tasks/<taskID>.output: one file per command,
// so two sessions never share a handle on the same output.
func NewManagerIn(sessionID, base string) (*Manager, error) {
	if sessionID == "" {
		sessionID = "default"
	}
	if base == "" {
		base = baseTaskDir()
	}
	dir := filepath.Join(base, sessionID, "tasks")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	cleanStaleSessions(base, sessionID)
	ctx, cancel := context.WithCancel(context.Background())
	return &Manager{
		tasks:      map[string]*Task{},
		sessions:   map[string]*ptySession{},
		dir:        dir,
		sessionID:  sessionID,
		base:       base,
		rootCtx:    ctx,
		rootCancel: cancel,
	}, nil
}

// cleanStaleSessions removes sibling session directories older than the TTL —
// those belong to sessions that died without running Cleanup (crash, SIGKILL).
//
// Staleness is judged by the NEWEST mtime in the directory, not the session
// dir's own. MkdirAll stamps <base>/<sessionID> once and all writes land in
// <sessionID>/tasks, so the session dir's mtime is frozen at creation and never
// refreshed by activity: judging by it alone deletes a session that is alive and
// busy but simply older than the TTL, out from under itself — Manager.Output
// then fails with ENOENT and watchSize silently stops enforcing the output cap.
// Output files are appended to as a task runs, so their mtimes are the signal
// that something is still using this directory.
func cleanStaleSessions(base, current string) {
	entries, err := os.ReadDir(base)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-staleSessionTTL)
	for _, e := range entries {
		if !e.IsDir() || e.Name() == current {
			continue
		}
		dir := filepath.Join(base, e.Name())
		// Only reclaim directories that look like an SDK session directory
		// (<sessionID>/tasks). The base may be a host-chosen directory that
		// holds other things too, and deleting those because their mtime is
		// old would be destroying the host's data, not ours.
		if !isSessionDir(dir) {
			continue
		}
		if newestMTime(dir).Before(cutoff) {
			_ = os.RemoveAll(dir)
		}
	}
}

// isSessionDir reports whether dir looks like an SDK session directory, i.e.
// whether it holds the <sessionID>/tasks subdirectory the SDK creates.
func isSessionDir(dir string) bool {
	fi, err := os.Stat(filepath.Join(dir, "tasks"))
	return err == nil && fi.IsDir()
}

// newestMTime returns the most recent mtime of dir or anything one level below
// it (<sessionID>/tasks and the output files inside). One level is enough: the
// layout is fixed at <base>/<sessionID>/tasks/<file>.
func newestMTime(dir string) time.Time {
	var newest time.Time
	bump := func(t time.Time) {
		if t.After(newest) {
			newest = t
		}
	}
	if fi, err := os.Stat(dir); err == nil {
		bump(fi.ModTime())
	}
	subs, err := os.ReadDir(dir)
	if err != nil {
		return newest
	}
	for _, s := range subs {
		if info, err := s.Info(); err == nil {
			bump(info.ModTime())
		}
		if !s.IsDir() {
			continue
		}
		files, err := os.ReadDir(filepath.Join(dir, s.Name()))
		if err != nil {
			continue
		}
		for _, f := range files {
			if info, err := f.Info(); err == nil {
				bump(info.ModTime())
			}
		}
	}
	return newest
}

// Spawn launches a process detached from the per-turn context. Its output streams
// to a file; a watch goroutine records the exit status and (if backgrounded) a
// completion notification.
func (m *Manager) Spawn(spec SpawnSpec) (*Task, error) {
	m.mu.Lock()
	m.seq++
	id := fmt.Sprintf("task_%d", m.seq)
	m.mu.Unlock()

	outPath := filepath.Join(m.dir, id+".output")
	f, err := os.OpenFile(outPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, err
	}

	kind := spec.Kind
	if kind == "" {
		kind = KindBash
	}
	shell, flags := shellCmd()
	cmd := exec.CommandContext(m.rootCtx, shell, append(flags, spec.Command)...)
	// Drop to the agent identity: with a root platform process the agent's shell
	// would inherit root and could read the platform's 0600 credential files.
	// Applied before setDetached, which only adds Setpgid to the same struct.
	if attr := privdrop.ProcAttr(); attr != nil && privdrop.CanDrop() {
		cmd.SysProcAttr = attr
	}
	cmd.Dir = spec.WorkingDir
	cmd.Env = withEnv(spec.Env)
	cmd.Stdout = f
	cmd.Stderr = f
	setDetached(cmd)

	if err := cmd.Start(); err != nil {
		_ = f.Close()
		_ = os.Remove(outPath)
		return nil, err
	}

	t := &Task{
		ID:          id,
		Kind:        kind,
		Command:     spec.Command,
		Description: spec.Description,
		Status:      TaskRunning,
		StartTime:   time.Now(),
		OutputPath:  outPath,
		cmd:         cmd,
		done:        make(chan struct{}),
		sessionID:   m.sessionID,
		ctx:         spec.Ctx,
		manager:     m,
	}
	if cmd.Process != nil {
		t.pid = cmd.Process.Pid
	}
	m.mu.Lock()
	m.tasks[id] = t
	m.order = append(m.order, id)
	m.mu.Unlock()

	go m.watch(t, f)
	return t, nil
}

func (m *Manager) watch(t *Task, f *os.File) {
	stop := make(chan struct{})
	go m.watchSize(t, stop)

	err := t.cmd.Wait()
	close(stop)
	_ = f.Close()

	m.mu.Lock()
	if t.Status == TaskRunning { // not pre-empted by Kill / watchdog
		if err == nil {
			t.Status = TaskCompleted
			t.ExitCode = 0
		} else if ee, ok := err.(*exec.ExitError); ok {
			t.Status = TaskFailed
			t.ExitCode = ee.ExitCode()
		} else {
			t.Status = TaskFailed
			t.ExitCode = -1
		}
	}
	t.EndTime = time.Now()
	close(t.done)
	notify := t.backgrounded && !t.notified && (t.Status == TaskCompleted || t.Status == TaskFailed)
	if notify {
		t.notified = true
		m.pending = append(m.pending, m.notifFor(t))
	}
	m.mu.Unlock()

	// The host hears about the outcome of every backgrounded task, including
	// one that was killed: a task that dies with its session still has to be
	// accounted for somewhere, and the host is the only party that can.
	m.notifyHostFinished(t)
}

// notifyHostFinished tells the host observer that a backgrounded task reached a
// terminal state. It fires at most once per task.
func (m *Manager) notifyHostFinished(t *Task) {
	m.mu.Lock()
	if !t.backgrounded || t.hostNotified || !t.Status.Terminal() {
		m.mu.Unlock()
		return
	}
	t.hostNotified = true
	ctx := t.ctx
	info := t.info()
	m.mu.Unlock()
	if obs := currentTaskObserver(); obs != nil {
		obs.OnFinished(ctx, info)
	}
}

// watchSize terminates a task whose output file grows past the cap.
func (m *Manager) watchSize(t *Task, stop chan struct{}) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			if fi, err := os.Stat(t.OutputPath); err == nil && fi.Size() > maxTaskOutputBytes {
				m.terminate(t, TaskFailed, true)
				return
			}
		}
	}
}

// terminate kills a still-running task, recording the given terminal status. The
// watch goroutine observes the exit and finalizes EndTime / notifications.
func (m *Manager) terminate(t *Task, status TaskStatus, oversize bool) {
	m.mu.Lock()
	if t.Status != TaskRunning {
		m.mu.Unlock()
		return
	}
	t.Status = status
	t.oversize = oversize
	proc := t.cmd.Process
	m.mu.Unlock()
	if proc != nil {
		treeKill(proc)
	}
}

func (m *Manager) notifFor(t *Task) Notification {
	var sum string
	switch {
	case t.oversize:
		sum = fmt.Sprintf("Task %q 输出超过上限已被终止 (%s)。", short(t.Command), t.Kind)
	case t.Status == TaskCompleted:
		sum = fmt.Sprintf("Task %q 已完成 (exit %d)。用 TaskOutput 或 Read 读取输出文件。", short(t.Command), t.ExitCode)
	case t.Status == TaskFailed:
		sum = fmt.Sprintf("Task %q 失败 (exit %d)。用 TaskOutput 或 Read 读取输出文件。", short(t.Command), t.ExitCode)
	default:
		sum = fmt.Sprintf("Task %q %s。", short(t.Command), t.Status)
	}
	return Notification{TaskID: t.ID, Kind: t.Kind, Status: t.Status, OutputPath: t.OutputPath, Summary: sum}
}

// Background marks a task as backgrounded: its completion will be surfaced to the
// model. If the task already finished (a race against a fast command), the
// notification is enqueued immediately.
//
// A backgrounded task is also the SDK's notion of a DURABLE task: when a host
// observer names an owner for it, the task is handed to the process-level
// durable registry and survives the end of this session (see Cleanup). The
// host is told about it through TaskObserver.OnBackgrounded, which is what lets
// the host keep a ledger it can reap later.
func (m *Manager) Background(id string) {
	m.mu.Lock()
	t, ok := m.tasks[id]
	if !ok {
		m.mu.Unlock()
		return
	}
	t.backgrounded = true
	if t.Status.Terminal() && !t.notified && (t.Status == TaskCompleted || t.Status == TaskFailed) {
		t.notified = true
		m.pending = append(m.pending, m.notifFor(t))
	}
	m.mu.Unlock()

	m.registerDurable(t)
}

// registerDurable asks the host observer who owns a backgrounded task and, if
// the host names an owner, files the task in the process-level durable
// registry so that it survives this session.
//
// The observer is host code and may block, so it runs outside the manager lock.
func (m *Manager) registerDurable(t *Task) {
	obs := currentTaskObserver()
	if obs == nil {
		return
	}
	m.mu.Lock()
	if t.registered {
		m.mu.Unlock()
		return
	}
	t.registered = true
	ctx := t.ctx
	m.mu.Unlock()

	info := t.info()
	// The host answers with the owner key only after it has recorded the task
	// in its own ledger: an unrecorded durable task would outlive its session
	// with nobody left to reap it.
	owner := obs.OnBackgrounded(ctx, info)
	if owner == "" {
		return
	}

	m.mu.Lock()
	t.owner = owner
	t.durable = true
	m.mu.Unlock()
	durableRegistry.Add(owner, t)

	// The task may have finished before it was marked backgrounded (a fast
	// command racing the Background call). The host still has to learn how it
	// ended, or its ledger keeps a task running that is long gone.
	m.notifyHostFinished(t)
}

// Forget removes a task from the registry and deletes its output file. Used for
// foreground commands once their output has been returned inline.
func (m *Manager) Forget(id string) {
	m.mu.Lock()
	t, ok := m.tasks[id]
	if ok {
		delete(m.tasks, id)
		for i, n := range m.order {
			if n == id {
				m.order = append(m.order[:i], m.order[i+1:]...)
				break
			}
		}
	}
	m.mu.Unlock()
	if ok {
		_ = os.Remove(t.OutputPath)
	}
}

// RunForeground starts a task and waits up to timeout. finished is true if the
// process exited within the timeout (output is then returned); otherwise the task
// keeps running and the caller decides whether to background or kill it. A
// cancelled ctx kills the task and returns ctx.Err().
func (m *Manager) RunForeground(ctx context.Context, spec SpawnSpec, timeout time.Duration) (t *Task, finished bool, output string, err error) {
	// The caller's context rides along so the host observer can attribute the
	// task to the run that started it (see TaskObserver).
	spec.Ctx = ctx
	t, err = m.Spawn(spec)
	if err != nil {
		return nil, false, "", err
	}
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-t.done:
		out, _ := m.Output(t.ID, 0)
		return t, true, out, nil
	case <-timer.C:
		return t, false, "", nil
	case <-ctx.Done():
		m.Kill(t.ID)
		<-t.done
		return t, false, "", ctx.Err()
	}
}

// Kill terminates a running task (process tree) and marks it killed.
func (m *Manager) Kill(id string) bool {
	m.mu.Lock()
	t, ok := m.tasks[id]
	m.mu.Unlock()
	if !ok {
		return false
	}
	m.terminate(t, TaskKilled, false)
	return true
}

// KillAll terminates every running task and returns how many were stopped.
//
// Durable tasks are included: an explicit "stop everything" from the model is
// the model changing its mind, not the session ending. The host is told how
// they ended through the usual observer callback, and their entries leave the
// durable registry so nothing stale is left behind.
func (m *Manager) KillAll() int {
	m.mu.Lock()
	ts := make([]*Task, 0, len(m.tasks))
	for _, t := range m.tasks {
		ts = append(ts, t)
	}
	m.mu.Unlock()
	n := 0
	for _, t := range ts {
		m.mu.Lock()
		running := t.Status == TaskRunning
		owner := t.owner
		sessionID := t.sessionID
		m.mu.Unlock()
		if running {
			m.terminate(t, TaskKilled, false)
			n++
			if owner != "" {
				durableRegistry.Remove(owner, sessionID, t.ID)
			}
		}
	}
	return n
}

// Get returns a snapshot of one task.
func (m *Manager) Get(id string) (TaskInfo, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tasks[id]
	if !ok {
		return TaskInfo{}, false
	}
	return t.info(), true
}

// List returns snapshots of all tasks in creation order.
func (m *Manager) List() []TaskInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]TaskInfo, 0, len(m.order))
	for _, id := range m.order {
		if t, ok := m.tasks[id]; ok {
			out = append(out, t.info())
		}
	}
	return out
}

// Wait blocks until the task exits or the timeout/ctx fires. ok is true only if
// the task reached a terminal state.
func (m *Manager) Wait(ctx context.Context, id string, timeout time.Duration) (TaskInfo, bool) {
	m.mu.Lock()
	t, ok := m.tasks[id]
	m.mu.Unlock()
	if !ok {
		return TaskInfo{}, false
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-t.done:
		info, _ := m.Get(id)
		return info, true
	case <-timer.C:
		info, _ := m.Get(id)
		return info, false
	case <-ctx.Done():
		info, _ := m.Get(id)
		return info, false
	}
}

// Output returns the tail of a task's output file, up to maxBytes (default 30000).
func (m *Manager) Output(id string, maxBytes int) (string, error) {
	m.mu.Lock()
	t, ok := m.tasks[id]
	m.mu.Unlock()
	if !ok {
		return "", fmt.Errorf("unknown task %q", id)
	}
	return tailFile(t.OutputPath, maxBytes)
}

// DrainNotifications returns and clears the pending completion notifications.
func (m *Manager) DrainNotifications() []Notification {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.pending) == 0 {
		return nil
	}
	out := m.pending
	m.pending = nil
	return out
}

// Cleanup kills the session's tasks, closes PTY sessions, removes the session's
// task directory and cancels the root context. Safe to call on a nil Manager.
//
// DURABLE TASKS SURVIVE. A task the host observer gave an owner to (see
// registerDurable) keeps running and keeps its output file: the promise the
// model was given — "you are notified when it completes" — must not be broken
// by the end of the run that made it. Every other task is killed, exactly as
// before.
//
// Removing the directory used to strand the "full output saved to <path>"
// pointers that were handed to the model, which is a real cost. It is still the
// right trade for the tasks that die with their session: agentcore mints a
// fresh random sessionID per Session, so keeping the directory means every run
// of the host leaves one behind, each task file capped only at
// maxTaskOutputBytes (50 MiB), with the stale sweep as the sole reclaimer — and
// that sweep runs only inside NewManager, so a host that stops opening
// sessions never reclaims at all.
//
// A durable task's directory is therefore kept (the whole <base>/<sessionID>
// is kept, because the output files live inside it) and its retention is the
// host's business: the host owns the ledger, the host reaps the process, and
// the host's own retention policy decides when the output goes away.
func (m *Manager) Cleanup() {
	if m == nil {
		return
	}
	m.killEphemeral()
	m.closeAllSessions()
	if m.dir != "" && !m.hasDurableTasks() {
		_ = os.RemoveAll(filepath.Dir(m.dir)) // <base>/<sessionID>, not just its tasks/
	}
	// The root context governs the spawned processes (exec.CommandContext), so
	// cancelling it kills them. It is therefore cancelled ONLY when no durable
	// task is left running: a durable task's process has to outlive this
	// session, and the host reaps it later by pid.
	//
	// The tasks that die with the session were already stopped explicitly
	// (killEphemeral), so skipping the cancel costs nothing for them.
	if m.rootCancel != nil && !m.hasDurableTasks() {
		m.rootCancel()
	}
}

// killEphemeral terminates every task that is not durable. Durable tasks were
// handed to the process-level registry and must outlive this session.
func (m *Manager) killEphemeral() {
	m.mu.Lock()
	ts := make([]*Task, 0, len(m.tasks))
	for _, t := range m.tasks {
		ts = append(ts, t)
	}
	m.mu.Unlock()
	for _, t := range ts {
		m.mu.Lock()
		running := t.Status == TaskRunning
		durable := t.durable
		m.mu.Unlock()
		if running && !durable {
			m.terminate(t, TaskKilled, false)
		}
	}
}

// hasDurableTasks reports whether any durable task of this session is still
// running — i.e. whether its output directory has to be kept.
func (m *Manager) hasDurableTasks() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, t := range m.tasks {
		if t.durable && t.Status == TaskRunning {
			return true
		}
	}
	return false
}

// tailFile reads the last maxBytes of a file, prefixing a notice when earlier
// output is omitted.
func tailFile(path string, maxBytes int) (string, error) {
	if maxBytes <= 0 {
		maxBytes = defaultMaxOutput
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return "", err
	}
	size := fi.Size()
	if size <= int64(maxBytes) {
		b, err := io.ReadAll(f)
		if err != nil {
			return "", err
		}
		return string(b), nil
	}
	off := size - int64(maxBytes)
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return "", err
	}
	b, err := io.ReadAll(f)
	if err != nil {
		return "", err
	}
	// Seeking to a byte offset lands mid-rune for any multi-byte output, and
	// this tail goes straight to the model via Manager.Output/TaskOutput — the
	// harness-level capture net leaves it alone because it is already under the
	// limit. cutTail drops the partial leading rune.
	tail := string(b)[runeSafeStart(b):]
	return fmt.Sprintf("[%d KB of earlier output omitted — read the output file directly for full output]\n", off/1024) + tail, nil
}

// shellCmd returns the platform shell and the argument flags that precede the
// command string. On Windows it uses PowerShell with -NoProfile -NonInteractive
// (the command is passed to -Command) so a user profile can't inject banners or
// prompts and an interactive prompt can't hang an unattended run. Elsewhere it
// uses `bash -c`.
func shellCmd() (string, []string) {
	if runtime.GOOS == "windows" {
		return "powershell", []string{"-NoProfile", "-NonInteractive", "-Command"}
	}
	return "bash", []string{"-c"}
}

// short trims a command to a compact single-line label for summaries.
func short(s string) string {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 60 {
		// This lands in notification summaries, so a CJK/emoji command must not
		// be cut mid-rune into a replacement character.
		return cutHead(s, 57) + "..."
	}
	return s
}

// BackgroundTasksDisabled reports whether background execution is turned off via
// the AGENT_CORE_DISABLE_BACKGROUND_TASKS environment variable.
func BackgroundTasksDisabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("AGENT_CORE_DISABLE_BACKGROUND_TASKS"))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

//go:build !windows

package tool

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// recordingObserver is a host stub that names an owner for every backgrounded
// task and remembers what it was told.
type recordingObserver struct {
	owner        string
	backgrounded []TaskInfo
	finished     []TaskInfo
}

func (o *recordingObserver) OnBackgrounded(_ context.Context, info TaskInfo) string {
	o.backgrounded = append(o.backgrounded, info)
	return o.owner
}

func (o *recordingObserver) OnFinished(_ context.Context, info TaskInfo) {
	o.finished = append(o.finished, info)
}

// processAlive reports whether the task's process is still running.
func processAlive(t *Task) bool {
	if t.cmd == nil || t.cmd.Process == nil {
		return false
	}
	return t.cmd.Process.Signal(syscall.Signal(0)) == nil
}

// A task the host took ownership of must outlive the session that started it:
// the model was told it could read the output later.
func TestDurableTaskSurvivesSessionCleanup(t *testing.T) {
	obs := &recordingObserver{owner: "task:7"}
	SetTaskObserver(obs)
	defer SetTaskObserver(nil)

	base := t.TempDir()
	m, err := NewManagerIn("sess-durable", base)
	if err != nil {
		t.Fatalf("NewManagerIn: %v", err)
	}
	task, err := m.Spawn(SpawnSpec{Command: "sleep 30", Ctx: context.Background()})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	m.Background(task.ID)

	m.Cleanup()

	if !processAlive(task) {
		t.Fatal("durable task was killed by Cleanup")
	}
	if _, err := os.Stat(task.OutputPath); err != nil {
		t.Fatalf("durable task output was removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(base, "sess-durable")); err != nil {
		t.Fatalf("durable task directory was removed: %v", err)
	}
	found := DurableTasks().List("task:7")
	if len(found) != 1 || found[0].ID != task.ID {
		t.Fatalf("durable task not findable under its owner: %+v", found)
	}
	if found[0].PID == 0 {
		t.Fatal("durable task carries no pid")
	}
	if len(obs.backgrounded) != 1 {
		t.Fatalf("observer saw %d backgrounded tasks, want 1", len(obs.backgrounded))
	}

	// The host reaps it.
	if !DurableTasks().Kill("task:7", "sess-durable", task.ID) {
		t.Fatal("Kill reported no such durable task")
	}
	waitDead(t, task)
}

// Without an owner the task keeps the old behaviour: it dies with its session.
func TestUnownedTaskDiesWithSession(t *testing.T) {
	obs := &recordingObserver{owner: ""}
	SetTaskObserver(obs)
	defer SetTaskObserver(nil)

	base := t.TempDir()
	m, err := NewManagerIn("sess-ephemeral", base)
	if err != nil {
		t.Fatalf("NewManagerIn: %v", err)
	}
	task, err := m.Spawn(SpawnSpec{Command: "sleep 30", Ctx: context.Background()})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	m.Background(task.ID)

	m.Cleanup()

	waitDead(t, task)
	if _, err := os.Stat(filepath.Join(base, "sess-ephemeral")); !os.IsNotExist(err) {
		t.Fatalf("ephemeral session directory survived cleanup: %v", err)
	}
	if len(DurableTasks().List("")) != 0 {
		t.Fatal("an unowned task was filed in the durable registry")
	}
}

// waitDead waits for the task's process to disappear.
func waitDead(t *testing.T, task *Task) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !processAlive(task) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("task process is still alive")
}

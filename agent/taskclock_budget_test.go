package agent

import (
	"testing"
	"time"
)

// TestClampMaxDuration pins the task-budget folding contract.
//
// The invariant that matters: a task's wall-clock budget is ABSOLUTE. Every run
// inside the task is clamped to the time REMAINING, never to a fresh full
// budget. That is what makes the task total non-resetting across runs — without
// it, N runs of the per-run budget would add up to N times the intended total.
func TestClampMaxDuration(t *testing.T) {
	const own = 30 * time.Minute

	t.Run("no deadline leaves the per-run budget untouched", func(t *testing.T) {
		eff, clamped := clampMaxDuration(0, own)
		if eff != own || clamped {
			t.Fatalf("got (%v, %v), want (%v, false)", eff, clamped, own)
		}
	})

	t.Run("a distant deadline does not extend the per-run budget", func(t *testing.T) {
		dl := time.Now().Add(6 * time.Hour).Unix()
		eff, clamped := clampMaxDuration(dl, own)
		if eff != own {
			t.Fatalf("eff = %v, want the per-run budget %v (the task deadline must not inflate it)", eff, own)
		}
		if clamped {
			t.Fatal("clamped = true, want false: the task deadline is not the binding constraint here")
		}
	})

	t.Run("a near deadline binds and clamps to the remaining time", func(t *testing.T) {
		dl := time.Now().Add(90 * time.Second).Unix()
		eff, clamped := clampMaxDuration(dl, own)
		if !clamped {
			t.Fatal("clamped = false, want true")
		}
		if eff <= 0 || eff > 95*time.Second {
			t.Fatalf("eff = %v, want the remaining time (~90s)", eff)
		}
	})

	t.Run("an agent without its own budget is bound by the task deadline", func(t *testing.T) {
		dl := time.Now().Add(90 * time.Second).Unix()
		eff, clamped := clampMaxDuration(dl, 0)
		if !clamped {
			t.Fatal("clamped = false, want true")
		}
		if eff <= 0 || eff > 95*time.Second {
			t.Fatalf("eff = %v, want the remaining time (~90s)", eff)
		}
	})

	t.Run("an expired deadline still yields a positive budget", func(t *testing.T) {
		dl := time.Now().Add(-time.Hour).Unix()
		eff, clamped := clampMaxDuration(dl, own)
		if !clamped {
			t.Fatal("clamped = false, want true")
		}
		if eff < time.Second {
			t.Fatalf("eff = %v, want >= 1s (0 is read as 'unlimited' by the harness)", eff)
		}
	})
}

// TestClampMaxDurationDoesNotResetAcrossRuns pins the non-resetting property
// directly: against ONE absolute deadline, the budget handed to successive runs
// never grows. A fresh full budget per run would make the task total unbounded.
func TestClampMaxDurationDoesNotResetAcrossRuns(t *testing.T) {
	dl := time.Now().Add(20 * time.Minute).Unix()
	const own = 30 * time.Minute

	first, _ := clampMaxDuration(dl, own)
	time.Sleep(1100 * time.Millisecond)
	second, _ := clampMaxDuration(dl, own)

	if second > first {
		t.Fatalf("budget grew between runs: %v -> %v (the task budget must not reset)", first, second)
	}
}

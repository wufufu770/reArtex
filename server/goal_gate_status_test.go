package server

import (
	"strconv"
	"testing"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
)

// TestGoalGateRewritesDoneToTimeout 钉住 C-05C：想记 done 但还有未达成目标时，
// 落库的是 timeout，不是 done，也不是 failed。评级按这个最终状态只算一次。
// 已经用 SQL 写成 done 的历史任务不会被回写。
func TestGoalGateRewritesDoneToTimeout(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer m.Close()

	seed := func(name string, states []string) (string, int64) {
		t.Helper()
		created, err := m.CreateTaskWithOptions(name, "四个目标", db.TaskCreateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		n, err := strconv.ParseInt(created.ID, 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		row, err := m.pg.GetTask(n)
		if err != nil || row == nil {
			t.Fatal(err)
		}
		for i, state := range states {
			payload := `{"text":"目标","criteria":"看到证据"}`
			if state == "open" {
				payload = `{"text":"还没达成","criteria":"读到 flag"}`
			}
			if _, err := m.pg.Exec(`INSERT INTO exploration_nodes (exploration_id, kind, payload, state) VALUES ($1,'goal',$2,$3)`, row.ExplorationID, payload, state); err != nil {
				t.Fatalf("目标 %d: %v", i, err)
			}
		}
		return created.ID, n
	}
	statusOf := func(n int64) string {
		t.Helper()
		var status string
		if err := m.pg.QueryRow(`SELECT status FROM tasks WHERE id=$1`, n).Scan(&status); err != nil {
			t.Fatal(err)
		}
		return status
	}
	gradeOf := func(n int64) (string, string) {
		t.Helper()
		row, err := m.pg.GetTask(n)
		if err != nil || row == nil {
			t.Fatal(err)
		}
		return row.Grade, row.GradeSummary
	}

	id, n := seed("c05c-partial", []string{"met", "met", "met", "open"})
	if err := m.SetTaskStatus(id, "done"); err != nil {
		t.Fatal(err)
	}
	if got := statusOf(n); got != "timeout" {
		t.Fatalf("3 met + 1 open 应记 timeout，得到 %s", got)
	}
	grade, summary := gradeOf(n)
	want := agent.ComputeGrade(agent.GradeInput{GoalsMet: 3, GoalsTotal: 4})
	if grade != want.Grade || summary != want.Summary {
		t.Fatalf("评级 %s / %s，想要 %s / %s", grade, summary, want.Grade, want.Summary)
	}
	if _, err := m.pg.Exec(`INSERT INTO findings (task_id, vulnclass, severity, summary, evidence, status) VALUES ($1,'sqli','high','SQL 注入可读取他人数据','GET /api?id=1 HTTP/1.1'||chr(10)||'200 OK','confirmed')`, n); err != nil {
		t.Fatal(err)
	}
	if err := m.SetTaskStatus(id, "done"); err != nil {
		t.Fatal(err)
	}
	grade2, summary2 := gradeOf(n)
	if statusOf(n) != "timeout" || grade2 != grade || summary2 != summary {
		t.Fatalf("第二次想记 done 不应重算评级：status=%s grade=%s/%s", statusOf(n), grade2, summary2)
	}

	allID, allN := seed("c05c-allmet", []string{"met", "met", "met", "met"})
	if err := m.SetTaskStatus(allID, "done"); err != nil {
		t.Fatal(err)
	}
	if got := statusOf(allN); got != "done" {
		t.Fatalf("四个都 met 应可记 done，得到 %s", got)
	}
	if g, _ := gradeOf(allN); g == "" {
		t.Fatal("全达成的 done 也应评一次级")
	}

	noneID, noneN := seed("c05c-nogoal", nil)
	if err := m.SetTaskStatus(noneID, "done"); err != nil {
		t.Fatal(err)
	}
	if got := statusOf(noneN); got != "done" {
		t.Fatalf("没有目标时应允许记 done，得到 %s", got)
	}

	abanID, abanN := seed("c05c-abandoned", []string{"met", "abandoned"})
	if err := m.SetTaskStatus(abanID, "done"); err != nil {
		t.Fatal(err)
	}
	if got := statusOf(abanN); got != "done" {
		t.Fatalf("abandoned 不算未达成，应可记 done，得到 %s", got)
	}

	failID, failN := seed("c05c-failed", []string{"open"})
	if err := m.SetTaskStatus(failID, "failed"); err != nil {
		t.Fatal(err)
	}
	if got := statusOf(failN); got != "failed" {
		t.Fatalf("想记 failed 时不应改成 timeout，得到 %s", got)
	}

	histID, histN := seed("c05c-history", []string{"open"})
	if _, err := m.pg.Exec(`UPDATE tasks SET status='done' WHERE id=$1`, histN); err != nil {
		t.Fatal(err)
	}
	if got := statusOf(histN); got != "done" {
		t.Fatalf("直接写成 done 的历史任务应保持 done，得到 %s", got)
	}
	_ = histID
}

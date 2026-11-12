package server

import (
	"testing"

	"github.com/Autumn-27/artex/db"
)

// G3（方案 v10 §15）收敛证据：候选流 MetGoalsActive 有界——
// ① 活跃任务的 met goal 在窗内；② 刚终态（1h 内）任务的 met goal 仍在窗内
//（prove_goal 与落 done 间隔亚秒级，5s tick 可能跳过活跃窗，收窄不能漏这类）；
// ③ 终态超 1h 的任务的 met goal 不再进候选流——这就是「不再每 5s 全表扫」的
// 语义锚点（全表口径 MetGoals 仍能看到它：重启播种用全表，两边口径各司其职）。
func TestMetGoalsActiveBounded(t *testing.T) {
	_, pg := wiringServer(t)

	mkTaskWithMetGoal := func(name string) *db.Task {
		t.Helper()
		task, err := pg.CreateTask(name, "目标达成即可", nil, 0, 0)
		if err != nil {
			t.Fatalf("建任务 %s: %v", name, err)
		}
		t.Cleanup(func() { _ = pg.DeleteTask(task.ID) })
		exp := pg.Exploration(task.ExplorationID)
		if _, err := exp.AddNode(db.KindGoal, map[string]any{"text": name + " 的目标"},
			0, "met", "system", nil); err != nil {
			t.Fatalf("建 met goal: %v", err)
		}
		return task
	}

	active := mkTaskWithMetGoal("G3-活跃任务")
	fresh := mkTaskWithMetGoal("G3-刚终态任务")
	old := mkTaskWithMetGoal("G3-终态已久任务")

	// fresh → done（completed_at=now）；old → done 且 completed_at 推到 2h 前。
	if _, err := pg.Exec(`UPDATE tasks SET status='done', completed_at=now() WHERE id=$1`, fresh.ID); err != nil {
		t.Fatalf("置终态 fresh: %v", err)
	}
	if _, err := pg.Exec(`UPDATE tasks SET status='done', completed_at=now() - interval '2 hours' WHERE id=$1`, old.ID); err != nil {
		t.Fatalf("置终态 old: %v", err)
	}

	full, err := pg.MetGoals()
	if err != nil {
		t.Fatalf("MetGoals: %v", err)
	}
	bounded, err := pg.MetGoalsActive()
	if err != nil {
		t.Fatalf("MetGoalsActive: %v", err)
	}

	has := func(events []db.TaskEvent, taskID int64) bool {
		for _, e := range events {
			if e.TaskID == taskID {
				return true
			}
		}
		return false
	}

	if !has(full, old.ID) {
		t.Fatal("全表口径 MetGoals 应能看到终态已久任务的 met goal（重启播种依赖它）")
	}
	if has(bounded, old.ID) {
		t.Fatal("候选流 MetGoalsActive 不应再收终态超 1h 的任务（G3 收窄语义）")
	}
	if !has(bounded, active.ID) {
		t.Fatal("活跃任务的 met goal 应在候选流窗内")
	}
	if !has(bounded, fresh.ID) {
		t.Fatal("刚终态（1h 内）任务的 met goal 应仍在候选流窗内（防漏 prove_goal→done 亚秒窗口）")
	}
}

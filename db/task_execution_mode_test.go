package db

// task_execution_mode_test.go：C-02B execution_mode 列的数据层验收（live PG）。
//
// 空串 = 跟随难度，创建时不把默认模式烘焙进列。
// 非法值拒绝且不改写；已有意图不回写。归档缺列回落空串。

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestTaskResourceFieldsFreshRead(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()
	easy := TaskDifficultyEasy
	created, err := d.CreateTaskWithOptions(fmt.Sprintf("c02d-fields-%d", time.Now().UnixNano()), "收集", TaskCreateOptions{
		Difficulty: &easy, ExecutionMode: TaskExecutionModeFast,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(created.ID) })
	difficulty, mode, err := d.Exploration(created.ExplorationID).TaskResourceFields()
	if err != nil {
		t.Fatal(err)
	}
	if difficulty != TaskDifficultyEasy || mode != TaskExecutionModeFast {
		t.Fatalf("现读 %q / %q", difficulty, mode)
	}
	clear := ""
	if _, err := d.UpdateTask(created.ID, TaskPatch{ExecutionMode: &clear}); err != nil {
		t.Fatal(err)
	}
	difficulty, mode, err = d.Exploration(created.ExplorationID).TaskResourceFields()
	if err != nil || difficulty != TaskDifficultyEasy || mode != "" {
		t.Fatalf("清空后应现读到空覆盖，得到 %q / %q err=%v", difficulty, mode, err)
	}
}

func TestTaskExecutionModeSchemaIdempotent(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()

	taskExecutionModeSchemaReady.Delete(d.DB)
	if err := d.EnsureTaskExecutionModeSchema(); err != nil {
		t.Fatal(err)
	}
	if err := d.EnsureTaskExecutionModeSchema(); err != nil {
		t.Fatal(err)
	}

	var nullable, def string
	if err := d.QueryRow(`SELECT is_nullable, column_default
FROM information_schema.columns
WHERE table_schema='public' AND table_name='tasks' AND column_name='execution_mode'`).Scan(&nullable, &def); err != nil {
		t.Fatal(err)
	}
	if nullable != "NO" {
		t.Fatalf("execution_mode 应 NOT NULL，nullable=%s default=%s", nullable, def)
	}
	if def != "''::text" && def != "''" {
		t.Fatalf("execution_mode 默认值应为空串，得到 %q", def)
	}
	var constraint string
	if err := d.QueryRow(`SELECT pg_get_constraintdef(oid) FROM pg_constraint
WHERE conname='ck_tasks_execution_mode'`).Scan(&constraint); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"fast", "balanced", "super_heavy"} {
		if !strings.Contains(constraint, want) {
			t.Fatalf("CHECK 定义缺 %q: %s", want, constraint)
		}
	}
}

func TestTaskExecutionModeCreateUpdateRestore(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()

	stamp := time.Now().UnixNano()
	desc := fmt.Sprintf("c02b-mode-%d", stamp)
	easy := TaskDifficultyEasy
	created, err := d.CreateTaskWithOptions(desc, "收集子域", TaskCreateOptions{Difficulty: &easy})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(created.ID) })
	if created.ExecutionMode != "" || created.Difficulty != TaskDifficultyEasy {
		t.Fatalf("不传模式应落空串且难度保持 easy，得到 mode=%q difficulty=%q", created.ExecutionMode, created.Difficulty)
	}
	got, err := d.GetTask(created.ID)
	if err != nil || got == nil || got.ExecutionMode != "" {
		t.Fatalf("现读应为空串，得到 %+v err=%v", got, err)
	}

	heavy, err := d.CreateTaskWithOptions(desc+"-heavy", "目标", TaskCreateOptions{
		Difficulty: &easy, ExecutionMode: "  super_heavy  ",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(heavy.ID) })
	if heavy.ExecutionMode != TaskExecutionModeSuperHeavy || heavy.Difficulty != TaskDifficultyEasy {
		t.Fatalf("显式 super_heavy 应原样落库，不因 easy 改成 fast：mode=%q difficulty=%q", heavy.ExecutionMode, heavy.Difficulty)
	}

	blank, err := d.CreateTaskWithOptions(desc+"-blank", "目标", TaskCreateOptions{ExecutionMode: "   "})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(blank.ID) })
	if blank.ExecutionMode != "" || blank.Difficulty != TaskDifficultyMedium {
		t.Fatalf("纯空白应折成空串、难度仍是默认 medium：mode=%q difficulty=%q", blank.ExecutionMode, blank.Difficulty)
	}

	for _, bad := range []string{"insane", "auto", "simple", "medium", "hard", "FAST", "expert"} {
		_, err := d.CreateTaskWithOptions(desc+"-bad", "目标", TaskCreateOptions{ExecutionMode: bad})
		if !errors.Is(err, ErrTaskExecutionModeInvalid) {
			t.Fatalf("模式 %q 应拒绝，err=%v", bad, err)
		}
	}
	// 尾部空格是合法覆盖，单独钉住，避免和拒绝表搅在一起。
	trimmed, err := d.CreateTaskWithOptions(desc+"-trim", "目标", TaskCreateOptions{ExecutionMode: " balanced "})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(trimmed.ID) })
	if trimmed.ExecutionMode != TaskExecutionModeBalanced {
		t.Fatalf("带空白的 balanced 应落 balanced，得到 %q", trimmed.ExecutionMode)
	}
	var leaked int
	if err := d.QueryRow(`SELECT count(*) FROM tasks WHERE description=$1`, desc+"-bad").Scan(&leaked); err != nil {
		t.Fatal(err)
	}
	if leaked != 0 {
		t.Fatalf("非法模式不应留下任务行，实际 %d", leaked)
	}

	es := d.Exploration(created.ExplorationID)
	intentID, err := es.AddIntent(map[string]any{"summary": "已经跑过的意图"}, 5, nil, "planner")
	if err != nil {
		t.Fatal(err)
	}
	var payloadBefore, stateBefore string
	if err := d.QueryRow(`SELECT payload::text, state FROM exploration_nodes WHERE id=$1`, intentID).Scan(&payloadBefore, &stateBefore); err != nil {
		t.Fatal(err)
	}

	fast := TaskExecutionModeFast
	updated, err := d.UpdateTask(created.ID, TaskPatch{ExecutionMode: &fast})
	if err != nil {
		t.Fatal(err)
	}
	if updated.ExecutionMode != TaskExecutionModeFast || updated.Difficulty != TaskDifficultyEasy {
		t.Fatalf("只改模式不应动难度：mode=%q difficulty=%q", updated.ExecutionMode, updated.Difficulty)
	}
	var payloadAfter, stateAfter string
	if err := d.QueryRow(`SELECT payload::text, state FROM exploration_nodes WHERE id=$1`, intentID).Scan(&payloadAfter, &stateAfter); err != nil {
		t.Fatal(err)
	}
	if payloadAfter != payloadBefore || stateAfter != stateBefore {
		t.Fatalf("已有意图被改写：payload %q→%q state %q→%q", payloadBefore, payloadAfter, stateBefore, stateAfter)
	}

	name := "只改名称"
	nameOnly, err := d.UpdateTask(created.ID, TaskPatch{Name: &name})
	if err != nil {
		t.Fatal(err)
	}
	if nameOnly.ExecutionMode != TaskExecutionModeFast || nameOnly.Name != name {
		t.Fatalf("只改名称不应动模式：mode=%q name=%q", nameOnly.ExecutionMode, nameOnly.Name)
	}

	clear := "  "
	cleared, err := d.UpdateTask(created.ID, TaskPatch{ExecutionMode: &clear})
	if err != nil {
		t.Fatal(err)
	}
	if cleared.ExecutionMode != "" || cleared.Difficulty != TaskDifficultyEasy {
		t.Fatalf("空白更新应折回空串且难度不动：mode=%q difficulty=%q", cleared.ExecutionMode, cleared.Difficulty)
	}

	insane := "auto"
	if _, err := d.UpdateTask(created.ID, TaskPatch{ExecutionMode: &insane}); !errors.Is(err, ErrTaskExecutionModeInvalid) {
		t.Fatalf("非法更新应拒绝，err=%v", err)
	}
	still, err := d.GetTask(created.ID)
	if err != nil || still == nil || still.ExecutionMode != "" {
		t.Fatalf("非法更新后应保持空串，得到 %+v err=%v", still, err)
	}

	listed := false
	tasks, err := d.ListTasks()
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range tasks {
		if task.ID == heavy.ID {
			listed = true
			if task.ExecutionMode != TaskExecutionModeSuperHeavy {
				t.Fatalf("列表读到 %q", task.ExecutionMode)
			}
		}
	}
	if !listed {
		t.Fatal("列表里没有带模式的任务")
	}

	if _, err := d.Exec(`UPDATE tasks SET execution_mode='auto' WHERE id=$1`, created.ID); err == nil {
		t.Fatal("CHECK 应拒绝 execution_mode=auto")
	}
	if _, err := d.Exec(`UPDATE tasks SET execution_mode='medium' WHERE id=$1`, created.ID); err == nil {
		t.Fatal("CHECK 应拒绝把执行档 medium 写进 execution_mode")
	}

	full := taskRowJSON(t, d, heavy.ID)
	stripped := taskRowJSON(t, d, heavy.ID)
	delete(stripped, "execution_mode")
	restoreDifficultyRow(t, d, heavy.ID, stripped)
	afterOld, err := d.GetTask(heavy.ID)
	if err != nil || afterOld == nil {
		t.Fatal(err)
	}
	if afterOld.ExecutionMode != "" || afterOld.Difficulty != TaskDifficultyEasy {
		t.Fatalf("旧快照缺列应回落空串且难度仍是 easy，得到 mode=%q difficulty=%q", afterOld.ExecutionMode, afterOld.Difficulty)
	}
	restoreDifficultyRow(t, d, heavy.ID, full)
	afterFull, err := d.GetTask(heavy.ID)
	if err != nil || afterFull == nil {
		t.Fatal(err)
	}
	if afterFull.ExecutionMode != TaskExecutionModeSuperHeavy || afterFull.Difficulty != TaskDifficultyEasy {
		t.Fatalf("完整快照应恢复 super_heavy，得到 mode=%q difficulty=%q", afterFull.ExecutionMode, afterFull.Difficulty)
	}
}

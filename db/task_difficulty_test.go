package db

// task_difficulty_test.go：C-01A 任务难度列的数据层验收（live PG）。
//
// 本批只钉存储契约，不钉 HTTP：
//   - 不传难度 → difficulty=medium、理由空串；
//   - 传 expert + 理由 → 原样落库（两侧空白会被 TrimSpace）；
//   - 传 insane / 空白 / 执行档位的 auto → 拒绝，且不落行；
//   - 改难度后再次读取是新值（不缓存快照），已有意图节点原样不动；
//   - 非法更新不改旧值；CHECK 是最后一道闸；
//   - 归档恢复：快照里有列则原样回来，旧快照缺列则回落 medium / 空理由
//     （不能把 NULL 写进 NOT NULL 列）。

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestTaskDifficultySchemaIdempotent(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()

	// Open 已经应用过。清掉就位标记，强制再跑一遍 DDL（DROP/ADD 约束必须幂等）。
	taskDifficultySchemaReady.Delete(d.DB)
	if err := d.EnsureTaskDifficultySchema(); err != nil {
		t.Fatal(err)
	}
	if err := d.EnsureTaskDifficultySchema(); err != nil {
		t.Fatal(err)
	}

	var cols int
	if err := d.QueryRow(`SELECT count(*) FROM information_schema.columns
WHERE table_schema='public' AND table_name='tasks'
  AND column_name IN ('difficulty','difficulty_reason')`).Scan(&cols); err != nil {
		t.Fatal(err)
	}
	if cols != 2 {
		t.Fatalf("difficulty 列应有 2 个，实际 %d", cols)
	}
	var def string
	if err := d.QueryRow(`SELECT pg_get_constraintdef(oid) FROM pg_constraint
WHERE conname='ck_tasks_difficulty'`).Scan(&def); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"easy", "medium", "hard", "expert"} {
		if !strings.Contains(def, want) {
			t.Fatalf("CHECK 定义缺 %q: %s", want, def)
		}
	}
	var indexDef string
	if err := d.QueryRow(`SELECT indexdef FROM pg_indexes WHERE indexname='idx_tasks_difficulty'`).Scan(&indexDef); err != nil {
		t.Fatal(err)
	}
	if indexDef == "" {
		t.Fatal("idx_tasks_difficulty 不存在")
	}
}

func TestTaskDifficultyCreateUpdateAndIntentUntouched(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()

	stamp := time.Now().UnixNano()
	desc := fmt.Sprintf("c01a-difficulty-%d", stamp)

	created, err := d.CreateTaskWithOptions(desc, "目标不含关键词", TaskCreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(created.ID) })
	if created.Difficulty != TaskDifficultyMedium || created.DifficultyReason != "" {
		t.Fatalf("不传难度应落 medium/空理由，得到 %q / %q", created.Difficulty, created.DifficultyReason)
	}
	got, err := d.GetTask(created.ID)
	if err != nil || got == nil {
		t.Fatalf("GetTask: %+v err=%v", got, err)
	}
	if got.Difficulty != TaskDifficultyMedium || got.DifficultyReason != "" {
		t.Fatalf("读回应为 medium/空理由，得到 %q / %q", got.Difficulty, got.DifficultyReason)
	}

	expert := "  expert  "
	withReason, err := d.CreateTaskWithOptions(desc+"-expert", "目标", TaskCreateOptions{
		Difficulty: &expert, DifficultyReason: "  人工指定专家档  ",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(withReason.ID) })
	if withReason.Difficulty != TaskDifficultyExpert || withReason.DifficultyReason != "人工指定专家档" {
		t.Fatalf("expert 应原样落库（去空白），得到 %q / %q", withReason.Difficulty, withReason.DifficultyReason)
	}

	for _, bad := range []string{"insane", "", "  ", "auto", "simple", "Expert"} {
		_, err := d.CreateTaskWithOptions(desc+"-bad", "目标", TaskCreateOptions{Difficulty: &bad})
		if !errors.Is(err, ErrTaskDifficultyInvalid) {
			t.Fatalf("难度 %q 应拒绝，err=%v", bad, err)
		}
	}
	var leaked int
	if err := d.QueryRow(`SELECT count(*) FROM tasks WHERE description=$1`, desc+"-bad").Scan(&leaked); err != nil {
		t.Fatal(err)
	}
	if leaked != 0 {
		t.Fatalf("非法难度不应留下任务行，实际 %d", leaked)
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

	easy := "easy"
	reason := "用户覆盖"
	updated, err := d.UpdateTask(created.ID, TaskPatch{Difficulty: &easy, DifficultyReason: &reason})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Difficulty != TaskDifficultyEasy || updated.DifficultyReason != reason {
		t.Fatalf("更新返回值 %q / %q", updated.Difficulty, updated.DifficultyReason)
	}
	reread, err := d.GetTask(created.ID)
	if err != nil || reread == nil || reread.Difficulty != TaskDifficultyEasy || reread.DifficultyReason != reason {
		t.Fatalf("现读应为 easy，得到 %+v err=%v", reread, err)
	}
	var payloadAfter, stateAfter string
	if err := d.QueryRow(`SELECT payload::text, state FROM exploration_nodes WHERE id=$1`, intentID).Scan(&payloadAfter, &stateAfter); err != nil {
		t.Fatal(err)
	}
	if payloadAfter != payloadBefore || stateAfter != stateBefore {
		t.Fatalf("已有意图被改写：payload %q→%q state %q→%q", payloadBefore, payloadAfter, stateBefore, stateAfter)
	}

	listed := false
	tasks, err := d.ListTasks()
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range tasks {
		if task.ID == created.ID {
			listed = true
			if task.Difficulty != TaskDifficultyEasy {
				t.Fatalf("列表读到 %q", task.Difficulty)
			}
		}
	}
	if !listed {
		t.Fatal("列表里没有刚更新的任务")
	}

	onlyReason := "只改理由"
	reasonOnly, err := d.UpdateTask(created.ID, TaskPatch{DifficultyReason: &onlyReason})
	if err != nil {
		t.Fatal(err)
	}
	if reasonOnly.Difficulty != TaskDifficultyEasy || reasonOnly.DifficultyReason != onlyReason {
		t.Fatalf("只改理由不应动难度：%q / %q", reasonOnly.Difficulty, reasonOnly.DifficultyReason)
	}

	insane := "insane"
	if _, err := d.UpdateTask(created.ID, TaskPatch{Difficulty: &insane}); !errors.Is(err, ErrTaskDifficultyInvalid) {
		t.Fatalf("非法更新应拒绝，err=%v", err)
	}
	still, err := d.GetTask(created.ID)
	if err != nil || still == nil || still.Difficulty != TaskDifficultyEasy || still.DifficultyReason != onlyReason {
		t.Fatalf("非法更新后应保持原值，得到 %+v err=%v", still, err)
	}

	if _, err := d.Exec(`UPDATE tasks SET difficulty='insane' WHERE id=$1`, created.ID); err == nil {
		t.Fatal("CHECK 应拒绝 difficulty=insane")
	}
}

func TestTaskDifficultyRestoreRoundTrip(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()

	expert := TaskDifficultyExpert
	created, err := d.CreateTaskWithOptions(fmt.Sprintf("c01a-restore-%d", time.Now().UnixNano()), "目标", TaskCreateOptions{
		Difficulty: &expert, DifficultyReason: "快照里的理由",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(created.ID) })

	full := taskRowJSON(t, d, created.ID)
	stripped := taskRowJSON(t, d, created.ID)
	delete(stripped, "difficulty")
	delete(stripped, "difficulty_reason")
	restoreDifficultyRow(t, d, created.ID, stripped)
	afterOld, err := d.GetTask(created.ID)
	if err != nil || afterOld == nil {
		t.Fatal(err)
	}
	if afterOld.Difficulty != TaskDifficultyMedium || afterOld.DifficultyReason != "" {
		t.Fatalf("旧快照缺列应回落 medium/空理由，得到 %q / %q", afterOld.Difficulty, afterOld.DifficultyReason)
	}

	restoreDifficultyRow(t, d, created.ID, full)
	afterFull, err := d.GetTask(created.ID)
	if err != nil || afterFull == nil {
		t.Fatal(err)
	}
	if afterFull.Difficulty != TaskDifficultyExpert || afterFull.DifficultyReason != "快照里的理由" {
		t.Fatalf("完整快照应恢复 expert，得到 %q / %q", afterFull.Difficulty, afterFull.DifficultyReason)
	}
}

func taskRowJSON(t *testing.T, d *DB, id int64) map[string]any {
	t.Helper()
	var raw []byte
	if err := d.QueryRow(`SELECT row_to_json(tasks) FROM tasks WHERE id=$1`, id).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	rows, err := decodeArchiveRows([]byte("[" + string(raw) + "]"))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("任务行数 %d", len(rows))
	}
	return rows[0]
}

func restoreDifficultyRow(t *testing.T, d *DB, id int64, row map[string]any) {
	t.Helper()
	tx, err := d.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := restoreTaskStub(tx, row, id, 0); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

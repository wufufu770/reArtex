package db

// task_grade_test.go：C-03B 任务评级三列的数据层验收（live PG）。
//
// 空等级 + NULL 分数 = 还没评。0 分是评过的 C，不能拿来当未评。
// 非终态拒绝写入。force=false 不覆盖已有评级。归档缺列回落未评。

import (
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

func TestTaskGradeSchemaIdempotent(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()

	taskGradeSchemaReady.Delete(d.DB)
	if err := d.EnsureTaskGradeSchema(); err != nil {
		t.Fatal(err)
	}
	if err := d.EnsureTaskGradeSchema(); err != nil {
		t.Fatal(err)
	}

	rows, err := d.Query(`SELECT column_name, is_nullable, column_default
FROM information_schema.columns
WHERE table_schema='public' AND table_name='tasks'
  AND column_name IN ('grade','grade_summary','grade_score')
ORDER BY column_name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := map[string][2]string{}
	for rows.Next() {
		var name, nullable string
		var def sql.NullString
		if err := rows.Scan(&name, &nullable, &def); err != nil {
			t.Fatal(err)
		}
		got[name] = [2]string{nullable, def.String}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if got["grade"][0] != "NO" || (got["grade"][1] != "''::text" && got["grade"][1] != "''") {
		t.Fatalf("grade 列：%v", got["grade"])
	}
	if got["grade_summary"][0] != "NO" || (got["grade_summary"][1] != "''::text" && got["grade_summary"][1] != "''") {
		t.Fatalf("grade_summary 列：%v", got["grade_summary"])
	}
	if got["grade_score"][0] != "YES" || got["grade_score"][1] != "" {
		t.Fatalf("grade_score 应可空且无默认，得到 %v", got["grade_score"])
	}
	var constraint string
	if err := d.QueryRow(`SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname='ck_tasks_grade'`).Scan(&constraint); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"'S'", "'A'", "'B'", "'C'"} {
		if !strings.Contains(constraint, want) {
			t.Fatalf("CHECK 定义缺 %s: %s", want, constraint)
		}
	}
}

func TestTaskGradeCreateSaveRestore(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()

	stamp := time.Now().UnixNano()
	created, err := d.CreateTaskWithOptions(fmt.Sprintf("c03b-grade-%d", stamp), "看看覆盖", TaskCreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(created.ID) })
	if created.Grade != "" || created.GradeSummary != "" || created.GradeScore != nil {
		t.Fatalf("新建任务应未评，得到 grade=%q summary=%q score=%v", created.Grade, created.GradeSummary, created.GradeScore)
	}
	got, err := d.GetTask(created.ID)
	if err != nil || got == nil || got.Grade != "" || got.GradeScore != nil {
		t.Fatalf("现读应未评，得到 %+v err=%v", got, err)
	}

	wrote, err := d.SaveTaskGrade(created.ID, TaskGradeA, "不应写上", 76.4, false)
	if wrote || !errors.Is(err, ErrTaskGradeNotTerminal) {
		t.Fatalf("非终态应拒绝，wrote=%v err=%v", wrote, err)
	}
	if _, err := d.SaveTaskGrade(created.ID, "D", "x", 1, true); !errors.Is(err, ErrTaskGradeInvalid) {
		t.Fatalf("非法等级应拒绝，err=%v", err)
	}
	still, err := d.GetTask(created.ID)
	if err != nil || still == nil || still.Grade != "" || still.GradeScore != nil {
		t.Fatalf("拒绝后应保持未评，得到 %+v err=%v", still, err)
	}

	if err := seedGradeFacts(t, d, created); err != nil {
		t.Fatal(err)
	}
	facts, err := d.LoadTaskGradeFacts(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if facts.GoalsMet != 2 || facts.GoalsTotal != 4 {
		t.Fatalf("目标计数 met/total=%d/%d，want 2/4", facts.GoalsMet, facts.GoalsTotal)
	}
	if facts.VerifiedFindings != 2 || facts.TotalFindings != 4 || len(facts.Findings) != 4 {
		t.Fatalf("发现计数 verified/total/rows=%d/%d/%d，want 2/4/4", facts.VerifiedFindings, facts.TotalFindings, len(facts.Findings))
	}
	if facts.CoveredAssets != 1 || facts.ScopedAssets != 2 {
		t.Fatalf("覆盖 %d/%d，want 1/2", facts.CoveredAssets, facts.ScopedAssets)
	}

	if err := d.SetStatus(created.ID, "done"); err != nil {
		t.Fatal(err)
	}
	wrote, err = d.SaveTaskGrade(created.ID, TaskGradeA, "第一次", 76.4, false)
	if err != nil || !wrote {
		t.Fatalf("终态首次写入应成功，wrote=%v err=%v", wrote, err)
	}
	graded, err := d.GetTask(created.ID)
	if err != nil || graded == nil || graded.Grade != TaskGradeA || graded.GradeSummary != "第一次" || graded.GradeScore == nil {
		t.Fatalf("现读评级：%+v err=%v", graded, err)
	}
	if math.Abs(*graded.GradeScore-76.4) > 0.01 {
		t.Fatalf("REAL 读回应接近 76.4，得到 %v", *graded.GradeScore)
	}

	// 改 finding 后再读，库里的评级不变。不 force 的第二次写入也不覆盖。
	if _, err := d.Exec(`UPDATE findings SET status='confirmed' WHERE task_id=$1 AND status='pending'`, created.ID); err != nil {
		t.Fatal(err)
	}
	wrote, err = d.SaveTaskGrade(created.ID, TaskGradeS, "不应覆盖", 100, false)
	if err != nil || wrote {
		t.Fatalf("已评过且不 force 应跳过，wrote=%v err=%v", wrote, err)
	}
	unchanged, err := d.GetTask(created.ID)
	if err != nil || unchanged == nil || unchanged.Grade != TaskGradeA || unchanged.GradeSummary != "第一次" {
		t.Fatalf("人工改 finding 后评级应保持第一次：%+v err=%v", unchanged, err)
	}

	wrote, err = d.SaveTaskGrade(created.ID, TaskGradeS, "重新评级", 100, true)
	if err != nil || !wrote {
		t.Fatalf("force 应覆盖，wrote=%v err=%v", wrote, err)
	}
	forced, err := d.GetTask(created.ID)
	if err != nil || forced == nil || forced.Grade != TaskGradeS || forced.GradeSummary != "重新评级" || forced.GradeScore == nil || math.Abs(*forced.GradeScore-100) > 0.01 {
		t.Fatalf("force 后：%+v err=%v", forced, err)
	}

	name := "只改名称"
	named, err := d.UpdateTask(created.ID, TaskPatch{Name: &name})
	if err != nil || named == nil || named.Grade != TaskGradeS || named.Name != name {
		t.Fatalf("只改名称不应动评级：%+v err=%v", named, err)
	}

	if err := d.ClearTaskGrade(created.ID); err != nil {
		t.Fatal(err)
	}
	cleared, err := d.GetTask(created.ID)
	if err != nil || cleared == nil || cleared.Grade != "" || cleared.GradeSummary != "" || cleared.GradeScore != nil {
		t.Fatalf("清除后应未评：%+v err=%v", cleared, err)
	}
	// 清除后再写一次，方便归档用例恢复出 S。
	if _, err := d.SaveTaskGrade(created.ID, TaskGradeB, "归档前", 60, false); err != nil {
		t.Fatal(err)
	}

	if _, err := d.Exec(`UPDATE tasks SET grade='D' WHERE id=$1`, created.ID); err == nil {
		t.Fatal("CHECK 应拒绝 grade=D")
	}
	if _, err := d.Exec(`UPDATE tasks SET grade='S', grade_score=NULL WHERE id=$1`, created.ID); err == nil {
		t.Fatal("CHECK 应拒绝有等级但分数为 NULL")
	}
	if _, err := d.Exec(`UPDATE tasks SET grade='', grade_score=1 WHERE id=$1`, created.ID); err == nil {
		t.Fatal("CHECK 应拒绝空等级带分数")
	}

	listed := false
	tasks, err := d.ListTasks()
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range tasks {
		if task.ID == created.ID {
			listed = true
			if task.Grade != TaskGradeB {
				t.Fatalf("列表读到 %q", task.Grade)
			}
		}
	}
	if !listed {
		t.Fatal("列表里没有这行任务")
	}

	full := taskRowJSON(t, d, created.ID)
	stripped := taskRowJSON(t, d, created.ID)
	delete(stripped, "grade")
	delete(stripped, "grade_summary")
	delete(stripped, "grade_score")
	restoreDifficultyRow(t, d, created.ID, stripped)
	afterOld, err := d.GetTask(created.ID)
	if err != nil || afterOld == nil {
		t.Fatal(err)
	}
	if afterOld.Grade != "" || afterOld.GradeSummary != "" || afterOld.GradeScore != nil || afterOld.Difficulty != TaskDifficultyMedium {
		t.Fatalf("旧快照缺列应回落未评且难度仍是 medium：grade=%q score=%v difficulty=%q", afterOld.Grade, afterOld.GradeScore, afterOld.Difficulty)
	}
	restoreDifficultyRow(t, d, created.ID, full)
	afterFull, err := d.GetTask(created.ID)
	if err != nil || afterFull == nil {
		t.Fatal(err)
	}
	if afterFull.Grade != TaskGradeB || afterFull.GradeSummary != "归档前" || afterFull.GradeScore == nil || math.Abs(*afterFull.GradeScore-60) > 0.01 {
		t.Fatalf("完整快照应恢复 B：grade=%q summary=%q score=%v", afterFull.Grade, afterFull.GradeSummary, afterFull.GradeScore)
	}
}

func TestLoadTaskGradeFactsEvidenceLevels(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()

	stamp := time.Now().UnixNano()
	created, err := d.CreateTaskWithOptions(fmt.Sprintf("c03d-levels-%d", stamp), "看看分级", TaskCreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(created.ID) })

	good := "GET /api?id=1 HTTP/1.1\n200 OK"
	if _, err := d.Exec(`INSERT INTO findings (task_id, vulnclass, severity, summary, evidence, status)
VALUES ($1,'sqli','high','SQL 注入可读取他人数据',$2,'confirmed')`, created.ID, good); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`INSERT INTO findings (task_id, vulnclass, severity, summary, evidence, status, evidence_level, evidence_level_requested, evidence_level_reason)
VALUES ($1,'sqli','critical','这不是漏洞',$2,'pending',3,3,'')`, created.ID, good); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`INSERT INTO findings (task_id, vulnclass, severity, summary, evidence, status, evidence_level, evidence_level_requested, evidence_level_reason)
VALUES ($1,'sqli','high','SQL 注入可读取他人数据',$2,'confirmed',2,4,'自报证据等级 L4，可验证上限为 L2')`, created.ID, good); err != nil {
		t.Fatal(err)
	}

	facts, err := d.LoadTaskGradeFacts(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(facts.Findings) != 3 {
		t.Fatalf("发现行 %d，want 3", len(facts.Findings))
	}
	legacy := facts.Findings[0]
	if legacy.EvidenceLevel != 1 || legacy.EvidenceLevelRequested != 1 || legacy.EvidenceLevelReason != "" || legacy.Evidence != good {
		t.Fatalf("存量行应是默认 L1 且无原因：%+v", legacy)
	}
	raised := facts.Findings[1]
	if raised.EvidenceLevel != 3 || raised.EvidenceLevelRequested != 3 || raised.EvidenceLevelReason != "" || raised.Summary != "这不是漏洞" {
		t.Fatalf("自报 L3：%+v", raised)
	}
	capped := facts.Findings[2]
	if capped.EvidenceLevel != 2 || capped.EvidenceLevelRequested != 4 || capped.EvidenceLevelReason == "" {
		t.Fatalf("降级行：%+v", capped)
	}
}

func seedGradeFacts(t *testing.T, d *DB, task *Task) error {
	t.Helper()
	expID := task.ExplorationID
	for _, state := range []string{"met", "met", "open", "abandoned"} {
		if _, err := d.Exec(`INSERT INTO exploration_nodes (exploration_id, kind, payload, state) VALUES ($1,'goal','{"text":"g"}',$2)`, expID, state); err != nil {
			return err
		}
	}
	// 意图节点不算目标。
	if _, err := d.Exec(`INSERT INTO exploration_nodes (exploration_id, kind, payload, state) VALUES ($1,'intent','{"summary":"不是目标"}','open')`, expID); err != nil {
		return err
	}
	type row struct{ status, summary, evidence string }
	rows := []row{
		{FindingConfirmed, "SQL 注入可读取他人数据", "GET /api?id=1 HTTP/1.1\n200 OK"},
		{FindingConfirmed, "另一处注入", ""},
		{FindingPending, "未授权访问可导出用户列表", "GET /admin HTTP/1.1\n200 OK"},
		{FindingPending, "这不是漏洞", "GET /admin HTTP/1.1\n200 OK"},
	}
	for _, r := range rows {
		if _, err := d.Exec(`INSERT INTO findings (task_id, vulnclass, severity, summary, evidence, status) VALUES ($1,'test','high',$2,$3,$4)`, task.ID, r.summary, r.evidence, r.status); err != nil {
			return err
		}
	}
	var testedID, idleID int64
	if err := d.QueryRow(`INSERT INTO assets (type, domain, origin) VALUES ('subdomain',$1,'agent') RETURNING id`, fmt.Sprintf("graded-%d.example.com", task.ID)).Scan(&testedID); err != nil {
		return err
	}
	if err := d.QueryRow(`INSERT INTO assets (type, domain, origin) VALUES ('subdomain',$1,'agent') RETURNING id`, fmt.Sprintf("idle-%d.example.com", task.ID)).Scan(&idleID); err != nil {
		return err
	}
	t.Cleanup(func() {
		_, _ = d.Exec(`DELETE FROM task_scope WHERE task_id=$1`, task.ID)
		_, _ = d.Exec(`DELETE FROM assets WHERE id IN ($1,$2)`, testedID, idleID)
	})
	for _, id := range []int64{testedID, idleID} {
		if _, err := d.Exec(`INSERT INTO task_scope (task_id, kind, asset_id, source, reason) VALUES ($1,'asset',$2,'manual','C-03B')`, task.ID, id); err != nil {
			return err
		}
	}
	var factID int64
	if err := d.QueryRow(`INSERT INTO exploration_nodes (exploration_id, kind, payload, state) VALUES ($1,'fact','{"summary":"测过"}','confirmed') RETURNING id`, expID).Scan(&factID); err != nil {
		return err
	}
	if _, err := d.Exec(`INSERT INTO exploration_anchors (node_id, asset_id) VALUES ($1,$2)`, factID, testedID); err != nil {
		return err
	}
	return nil
}

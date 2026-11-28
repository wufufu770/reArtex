package db

// goal_unmet_test.go：C-05A 未达成目标查询、达成时的证据引用、改目标文本时保留 payload。

import (
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestUnmetGoalsAndMarkGoalMet(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()

	stamp := time.Now().UnixNano()
	task, err := d.CreateTaskWithOptions(fmt.Sprintf("c05a-unmet-%d", stamp), "四个目标", TaskCreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	other, err := d.CreateTaskWithOptions(fmt.Sprintf("c05a-other-%d", stamp), "别的任务", TaskCreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	es := d.Exploration(task.ExplorationID)
	oes := d.Exploration(other.ExplorationID)

	var openID int64
	states := []struct {
		state    string
		text     string
		criteria string
	}{
		{"met", "目标甲", "看到甲的证据"},
		{"met", "目标乙", "看到乙的证据"},
		{"met", "目标丙", "看到丙的证据"},
		{"open", "目标丁", "看到丁的证据"},
		{"abandoned", "目标戊", "已放弃"},
	}
	for _, g := range states {
		id, err := es.AddNode(KindGoal, map[string]any{"text": g.text, "criteria": g.criteria}, 0, g.state, "test", nil)
		if err != nil {
			t.Fatal(err)
		}
		if g.state == "open" {
			openID = id
		}
	}
	if _, err := es.AddNode(KindIntent, map[string]any{"summary": "不是目标", "text": "意图"}, 0, "open", "test", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := oes.AddNode(KindGoal, map[string]any{"text": "别人的目标", "criteria": "不该出现"}, 0, "open", "test", nil); err != nil {
		t.Fatal(err)
	}

	unmet, err := d.UnmetGoals(task.ExplorationID)
	if err != nil {
		t.Fatal(err)
	}
	if len(unmet) != 1 || unmet[0].NodeID != openID || unmet[0].Text != "目标丁" || unmet[0].Criteria != "看到丁的证据" || unmet[0].Reason != UnmetGoalReason {
		t.Fatalf("未达成目标 = %+v，想要只有目标丁", unmet)
	}
	byTask, err := d.UnmetGoalsByTask(task.ID)
	if err != nil || len(byTask) != 1 || byTask[0].NodeID != openID {
		t.Fatalf("按任务查询 = %+v err=%v", byTask, err)
	}
	if _, err := d.UnmetGoalsByTask(task.ID + 9_000_000_000); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("不存在的任务应是 ErrNoRows，得到 %v", err)
	}

	factID, err := es.AddNode(KindFact, map[string]any{"summary": "丁的证据"}, 0, "confirmed", "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := es.MarkGoalMet(0, factID, "缺 id"); err == nil {
		t.Fatal("缺 goal_id 应拒绝")
	}
	if err := es.MarkGoalMet(openID, 0, "缺证据"); err == nil {
		t.Fatal("缺 evidence_id 应拒绝")
	}
	if err := oes.MarkGoalMet(openID, factID, "跨探索"); err == nil {
		t.Fatal("别的探索不能标这条目标")
	}
	still, err := d.UnmetGoals(task.ExplorationID)
	if err != nil || len(still) != 1 {
		t.Fatalf("拒绝之后目标丁仍应未达成：%+v err=%v", still, err)
	}
	if err := es.MarkGoalMet(openID, factID, "  证据对上了  "); err != nil {
		t.Fatal(err)
	}
	none, err := d.UnmetGoals(task.ExplorationID)
	if err != nil || len(none) != 0 {
		t.Fatalf("四条 met + 一条 abandoned 之后应没有未达成目标：%+v err=%v", none, err)
	}
	var state, evidence, reason string
	if err := d.QueryRow(`SELECT state, payload->>'evidence_id', payload->>'evidence_reason' FROM exploration_nodes WHERE id=$1`, openID).Scan(&state, &evidence, &reason); err != nil {
		t.Fatal(err)
	}
	if state != "met" || evidence != fmt.Sprint(factID) || reason != "证据对上了" {
		t.Fatalf("达成落库 state=%s evidence=%s reason=%q", state, evidence, reason)
	}

	if err := es.UpdateGoalPayload(openID, "目标丁改过", "IDOR"); err != nil {
		t.Fatal(err)
	}
	var text, criteria, vuln, evidence2 string
	if err := d.QueryRow(`SELECT payload->>'text', payload->>'criteria', payload->>'vulnclass', payload->>'evidence_id' FROM exploration_nodes WHERE id=$1`, openID).Scan(&text, &criteria, &vuln, &evidence2); err != nil {
		t.Fatal(err)
	}
	if text != "目标丁改过" || criteria != "看到丁的证据" || vuln != "IDOR" || evidence2 != fmt.Sprint(factID) {
		t.Fatalf("改文本不应抹掉判定标准和证据：text=%s criteria=%s vuln=%s evidence=%s", text, criteria, vuln, evidence2)
	}
	if err := es.UpdateGoalPayload(openID, "目标丁再改", ""); err != nil {
		t.Fatal(err)
	}
	var vulnAfter sql.NullString
	if err := d.QueryRow(`SELECT payload->>'vulnclass', payload->>'criteria' FROM exploration_nodes WHERE id=$1`, openID).Scan(&vulnAfter, &criteria); err != nil {
		t.Fatal(err)
	}
	if vulnAfter.Valid || criteria != "看到丁的证据" {
		t.Fatalf("空 vulnclass 应删掉该键并保留 criteria：vuln=%v criteria=%s", vulnAfter, criteria)
	}
}

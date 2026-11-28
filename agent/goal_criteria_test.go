package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
)

func callDomainTool(t *testing.T, ts *ToolSet, name string, payload any) string {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range ts.AllDomainTools() {
		if tool.Name() != name {
			continue
		}
		res, err := tool.Call(context.Background(), raw, nil)
		if err != nil {
			t.Fatalf("%s 调用失败：%v", name, err)
		}
		return res.Flatten()
	}
	t.Fatalf("工具表里没有 %s", name)
	return ""
}

func TestSetGoalsRequiresCriteriaAndProveCitesEvidence(t *testing.T) {
	d := testDB(t)
	defer d.Close()

	stamp := time.Now().UnixNano()
	task, err := d.CreateTaskWithOptions(fmt.Sprintf("c05b-goals-%d", stamp), "四个目标", db.TaskCreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	es := d.Exploration(task.ExplorationID)
	ts := NewToolSet(es, "planner")
	ts.SetTaskID(task.ID)

	refused := callDomainTool(t, ts, "set_goals", map[string]any{"text": "拿下旗标"})
	if !strings.Contains(refused, "criteria") {
		t.Fatalf("缺判定标准应拒绝：%s", refused)
	}
	blank := callDomainTool(t, ts, "set_goals", map[string]any{"text": "拿下旗标", "criteria": "   "})
	if !strings.Contains(blank, "criteria") {
		t.Fatalf("空白判定标准应拒绝：%s", blank)
	}
	var n int
	if err := d.QueryRow(`SELECT count(*) FROM exploration_nodes WHERE exploration_id=$1 AND kind='goal'`, task.ExplorationID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("拒绝之后不应留下目标，得到 %d", n)
	}

	added := callDomainTool(t, ts, "set_goals", map[string]any{"goals": []any{
		map[string]any{"text": "目标甲", "criteria": "看到甲"},
		map[string]any{"text": "目标乙", "criteria": "看到乙"},
		map[string]any{"text": "目标丙"},
		map[string]any{"text": "目标丁", "criteria": "看到丁"},
	}})
	if !strings.Contains(added, "criteria") {
		t.Fatalf("批量里缺判定标准的那条应报错：%s", added)
	}
	if err := d.QueryRow(`SELECT count(*) FROM exploration_nodes WHERE exploration_id=$1 AND kind='goal'`, task.ExplorationID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("应落下 3 条带判定标准的目标，得到 %d（%s）", n, added)
	}
	rows, err := d.Query(`SELECT id, payload->>'text' FROM exploration_nodes WHERE exploration_id=$1 AND kind='goal' ORDER BY id`, task.ExplorationID)
	if err != nil {
		t.Fatal(err)
	}
	texts := map[string]int64{}
	for rows.Next() {
		var id int64
		var text string
		if err := rows.Scan(&id, &text); err != nil {
			t.Fatal(err)
		}
		texts[text] = id
	}
	rows.Close()
	if texts["目标丙"] != 0 || texts["目标甲"] == 0 || texts["目标丁"] == 0 {
		t.Fatalf("目标落库不对：%v", texts)
	}

	noEv := callDomainTool(t, ts, "prove_goal", map[string]any{"goal_id": texts["目标甲"]})
	if !strings.Contains(noEv, "evidence_id") {
		t.Fatalf("无证据引用应拒绝：%s", noEv)
	}
	intentID, err := es.AddNode(db.KindIntent, map[string]any{"summary": "不是证据"}, 0, "open", "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	badKind := callDomainTool(t, ts, "prove_goal", map[string]any{"goal_id": texts["目标甲"], "evidence_id": intentID})
	if !strings.Contains(badKind, "事实/漏洞") {
		t.Fatalf("意图节点不能当证据：%s", badKind)
	}
	var state string
	if err := d.QueryRow(`SELECT state FROM exploration_nodes WHERE id=$1`, texts["目标甲"]).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "open" {
		t.Fatalf("拒绝之后目标甲仍应是 open，得到 %s", state)
	}

	factID, err := es.AddNode(db.KindFact, map[string]any{"summary": "甲的证据"}, 0, "confirmed", "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"目标甲", "目标乙", "目标丁"} {
		out := callDomainTool(t, ts, "prove_goal", map[string]any{
			"goal_id": texts[key], "evidence_id": factID, "reason": "证据对上 " + key,
		})
		if strings.Contains(out, "拒绝") || strings.Contains(out, "必填") {
			t.Fatalf("证明 %s 失败：%s", key, out)
		}
	}
	if !ts.GoalMet {
		t.Fatal("三条目标都 met 之后应自动判定完成")
	}

	var evidence, reason string
	if err := d.QueryRow(`SELECT payload->>'evidence_id', payload->>'evidence_reason', payload->>'criteria' FROM exploration_nodes WHERE id=$1`, texts["目标甲"]).Scan(&evidence, &reason, &state); err != nil {
		t.Fatal(err)
	}
	if evidence != fmt.Sprint(factID) || reason != "证据对上 目标甲" || state != "看到甲" {
		t.Fatalf("证据引用或判定标准不对：evidence=%s reason=%q criteria=%s", evidence, reason, state)
	}
	// 4 目标里 3 met、1 open：最后一条 open 时不收官；补上之后收官。
	task2, err := d.CreateTaskWithOptions(fmt.Sprintf("c05b-four-%d", stamp), "四个目标", db.TaskCreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	es2 := d.Exploration(task2.ExplorationID)
	ts2 := NewToolSet(es2, "planner")
	ts2.SetTaskID(task2.ID)
	_ = callDomainTool(t, ts2, "set_goals", map[string]any{"goals": []any{
		map[string]any{"text": "A", "criteria": "a"},
		map[string]any{"text": "B", "criteria": "b"},
		map[string]any{"text": "C", "criteria": "c"},
		map[string]any{"text": "D", "criteria": "d"},
	}})
	fact2, err := es2.AddNode(db.KindFact, map[string]any{"summary": "证据"}, 0, "confirmed", "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	grows, err := d.Query(`SELECT id FROM exploration_nodes WHERE exploration_id=$1 AND kind='goal' ORDER BY id`, task2.ExplorationID)
	if err != nil {
		t.Fatal(err)
	}
	var gids []int64
	for grows.Next() {
		var id int64
		if err := grows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		gids = append(gids, id)
	}
	grows.Close()
	if len(gids) != 4 {
		t.Fatalf("应有 4 个目标，得到 %d", len(gids))
	}
	for _, id := range gids[:3] {
		out := callDomainTool(t, ts2, "prove_goal", map[string]any{"goal_id": id, "evidence_id": fact2, "reason": "ok"})
		if !strings.Contains(out, "marked met") || strings.Contains(out, "自动判定完成") {
			t.Fatalf("前三个不应收官：%s", out)
		}
	}
	if ts2.GoalMet {
		t.Fatal("还剩 1 个未达成目标时不应判定任务完成")
	}
	unmet, err := d.UnmetGoals(task2.ExplorationID)
	if err != nil || len(unmet) != 1 || unmet[0].NodeID != gids[3] || unmet[0].Criteria != "d" {
		t.Fatalf("未达成清单 = %+v err=%v", unmet, err)
	}
	done := callDomainTool(t, ts2, "prove_goal", map[string]any{"goal_id": gids[3], "evidence_id": fact2, "reason": "ok"})
	if !strings.Contains(done, "自动判定完成") || !ts2.GoalMet {
		t.Fatalf("四个都 met 应可收官：%s GoalMet=%v", done, ts2.GoalMet)
	}

	// abandoned 不算未达成：其余目标 met 后即可收官。
	task3, err := d.CreateTaskWithOptions(fmt.Sprintf("c05b-aban-%d", stamp), "含放弃", db.TaskCreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	es3 := d.Exploration(task3.ExplorationID)
	ts3 := NewToolSet(es3, "planner")
	ts3.SetTaskID(task3.ID)
	openID, err := es3.AddNode(db.KindGoal, map[string]any{"text": "还要证", "criteria": "有证据"}, 0, "open", "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := es3.AddNode(db.KindGoal, map[string]any{"text": "不做了", "criteria": "已放弃"}, 0, "abandoned", "test", nil); err != nil {
		t.Fatal(err)
	}
	fact3, err := es3.AddNode(db.KindFact, map[string]any{"summary": "证据"}, 0, "confirmed", "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	out := callDomainTool(t, ts3, "prove_goal", map[string]any{"goal_id": openID, "evidence_id": fact3, "reason": "ok"})
	if !ts3.GoalMet || !strings.Contains(out, "自动判定完成") {
		t.Fatalf("abandoned 不应挡住收官：%s GoalMet=%v", out, ts3.GoalMet)
	}
}

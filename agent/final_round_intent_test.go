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

func TestFinalRoundRejectsNewIntent(t *testing.T) {
	d := testDB(t)
	defer d.Close()
	stamp := time.Now().UnixNano()
	task, err := d.CreateTaskWithOptions(fmt.Sprintf("c05d-final-%d", stamp), "到点收尾", db.TaskCreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	es := d.Exploration(task.ExplorationID)
	ts := NewToolSet(es, "planner")
	ts.SetTaskID(task.ID)

	addTool := ts.addIntent()
	proposeCore := toolProposeIntent(ts)
	final := WithTaskClock(context.Background(), TaskClock{Final: true, DeadlineUnix: time.Now().Unix()})
	raw, _ := json.Marshal(map[string]any{"summary": "终局不该出现的新方向"})

	res, err := addTool.Call(final, raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Flatten(), "禁止新增意图") {
		t.Fatalf("终局 add_intent 应拒绝：%s", res.Flatten())
	}
	pres, err := proposeCore.Call(final, raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(pres.Flatten(), "禁止新增意图") {
		t.Fatalf("终局 propose_intent 应拒绝：%s", pres.Flatten())
	}
	var n int
	if err := d.QueryRow(`SELECT count(*) FROM exploration_nodes WHERE exploration_id=$1 AND kind='intent'`, task.ExplorationID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("终局拒绝之后不应有意图节点，得到 %d", n)
	}
	var acts int
	if err := d.QueryRow(`SELECT count(*) FROM activity WHERE exploration_id=$1 AND (tool='add_intent' OR tool='propose_intent')`, task.ExplorationID).Scan(&acts); err != nil {
		t.Fatal(err)
	}
	if acts != 0 {
		t.Fatalf("终局拒绝不应留下新增意图的活动，得到 %d", acts)
	}

	okRes, err := addTool.Call(context.Background(), raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(okRes.Flatten(), "禁止") {
		t.Fatalf("非终局应能新增意图：%s", okRes.Flatten())
	}
	if err := d.QueryRow(`SELECT count(*) FROM exploration_nodes WHERE exploration_id=$1 AND kind='intent'`, task.ExplorationID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("非终局应留下 1 条意图，得到 %d", n)
	}

	filtered := dropNamedTools(ts.PlannerTools(), "add_intent", "propose_intent")
	seenProve := false
	for _, tool := range filtered {
		switch tool.Name() {
		case "add_intent", "propose_intent":
			t.Fatalf("终局工具面还留着 %s", tool.Name())
		case "prove_goal":
			seenProve = true
		}
	}
	if !seenProve {
		t.Fatal("终局工具面应保留 prove_goal")
	}
}

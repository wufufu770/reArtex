package agent

import (
	"fmt"
	"testing"
)

func TestZZDebugCandidateFactVisible(t *testing.T) {
	fx := newGraphScopeFixture(t)
	ts := fx.tools(t)
	resp := graphScopeCall(t, ts, "record_fact", map[string]any{
		"summary": "dbg 范围内结论", "asset_ids": []int64{fx.inAsset},
	})
	t.Logf("record_fact 响应: %s", resp)
	out := graphScopeCall(t, ts, "graph_overview", map[string]any{})
	t.Logf("graph_overview: %s", out)
	var n int
	if err := fx.db.QueryRow(`SELECT count(*) FROM exploration_nodes WHERE kind='fact'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	t.Logf("fact 行数: %d", n)
	var cdef string
	_ = fx.db.QueryRow(`SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname='ck_node_state' AND conrelid='exploration_nodes'::regclass`).Scan(&cdef)
	t.Logf("约束定义: %s", cdef)
	rows, err := fx.db.Query(`SELECT id, state FROM exploration_nodes WHERE kind='fact'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var st string
		_ = rows.Scan(&id, &st)
		t.Logf("fact #%d state=%s", id, st)
	}
	_ = fmt.Sprint()
}

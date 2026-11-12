package server

import (
	"testing"

	"github.com/Autumn-27/artex/db"
)

// v0.11.1 门5（P0）：goalless 收尾的零产出守卫——探索图上没有任何 fact/finding
// 时不落 done（防 LLM 幻觉收口：不执行工具直接宣布完成）。
func TestRefuseDoneIfNoOutput(t *testing.T) {
	s, pg := wiringServer(t)
	task := wiringTask(t, s, "零产出守卫测试")
	exp := pg.Exploration(task.ExpID)
	taskStatus := func(id string) string {
		var st string
		_ = pg.QueryRow(`SELECT status FROM tasks WHERE id=$1`, id).Scan(&st)
		return st
	}

	// ① 零产出 ⇒ 拦（返回 true = 已转暂停）。
	if !s.engine.refuseDoneIfNoOutput(task, "测试") {
		t.Fatal("零产出应被守卫拦下（返回 true）")
	}
	if st := taskStatus(task.ID); st != "paused" {
		t.Fatalf("零产出收尾应转 paused，got %q", st)
	}

	// ② 有 fact 产出 ⇒ 不拦。
	if _, err := exp.AddNode(db.KindFact, map[string]any{"summary": "真实写回的一条事实"}, 0, "confirmed", "work#1", nil); err != nil {
		t.Fatalf("建 fact: %v", err)
	}
	if s.engine.refuseDoneIfNoOutput(task, "测试") {
		t.Fatal("有 fact 产出不应拦")
	}

	// ③ 只有 finding 也算产出（report_finding 落 KindFinding 节点）。
	task2 := wiringTask(t, s, "零产出守卫测试·finding")
	exp2 := pg.Exploration(task2.ExpID)
	if _, err := exp2.AddNode(db.KindFinding, map[string]any{"name": "x", "summary": "y"}, 0, "pending", "work#1", nil); err != nil {
		t.Fatalf("建 finding: %v", err)
	}
	if s.engine.refuseDoneIfNoOutput(task2, "测试") {
		t.Fatal("有 finding 产出不应拦")
	}
}

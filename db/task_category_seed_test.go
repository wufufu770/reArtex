package db

import (
	"testing"
)

// 本文件是内置任务类别（资产收集/渗透测试/漏洞验证，方案 v4 §3-C2）的播种与按名
// 查询测试。类别是「按类别给 worker 拼纪律段」的结构性开关的载体，播不出来 =
// 引擎的类别纪律永远空转，所以幂等语义要在测试里钉死：
//   - 旗标 done ⇒ 用户删掉的类别不被重启复活；
//   - 旗标清掉但同名类别已存在 ⇒ ON CONFLICT DO NOTHING，用户手建的行不被覆盖；
//   - 正常播种 ⇒ 恰好三行、按名可查、nkey 口径（空白折叠+小写）。

func TestSeedDefaultTaskCategoriesIdempotent(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()

	// 第一次 Open（本派生库的首次）已经跑过 seedBuiltins ⇒ 三个内置类别应在，
	// 且旗标已落（防重播生效）。
	if v, _, err := d.GetSetting(taskCategoriesSeedFlag); err != nil || v != "done" {
		t.Fatalf("旗标 %s 应为 done，got %q err=%v", taskCategoriesSeedFlag, v, err)
	}
	countDefaultCategories(t, d, 3)

	// 幂等重播：清旗标再播种一次 ⇒ 仍是三行，无重复。
	if err := d.SetSetting(taskCategoriesSeedFlag, ""); err != nil {
		t.Fatal(err)
	}
	if err := d.seedDefaultTaskCategories(); err != nil {
		t.Fatalf("re-seed: %v", err)
	}
	countDefaultCategories(t, d, 3)

	// 用户手建的同名行不被覆盖：清空表 + 清旗标，先手建「资产收集」，再播种 ⇒
	// 资产收集行保持原 id（ON CONFLICT (nkey) DO NOTHING），另外两类补齐。
	if _, err := d.Exec(`DELETE FROM task_categories`); err != nil {
		t.Fatal(err)
	}
	if err := d.SetSetting(taskCategoriesSeedFlag, ""); err != nil {
		t.Fatal(err)
	}
	userRow, err := d.CreateTaskCategory("资产收集")
	if err != nil {
		t.Fatalf("手建资产收集: %v", err)
	}
	if err := d.seedDefaultTaskCategories(); err != nil {
		t.Fatalf("seed after user row: %v", err)
	}
	kept, err := d.TaskCategoryByName("资产收集")
	if err != nil || kept == nil {
		t.Fatalf("播种后资产收集应存在: %v", err)
	}
	if kept.ID != userRow.ID || kept.NKey != userRow.NKey {
		t.Fatalf("用户手建的行被覆盖：got id=%d nkey=%q, want id=%d nkey=%q", kept.ID, kept.NKey, userRow.ID, userRow.NKey)
	}
	countDefaultCategories(t, d, 3)

	// 旗标 done ⇒ 用户删掉的类别不被重启复活（这是旗标存在的唯一理由）。
	if _, err := d.Exec(`DELETE FROM task_categories WHERE nkey=$1`, TaskCategoryKey(TaskCategoryAssetCollection)); err != nil {
		t.Fatal(err)
	}
	if err := d.seedDefaultTaskCategories(); err != nil {
		t.Fatal(err)
	}
	gone, err := d.TaskCategoryByName(TaskCategoryAssetCollection)
	if err != nil {
		t.Fatal(err)
	}
	if gone != nil {
		t.Fatal("旗标 done 后重播不得复活被删类别")
	}

	// 收尾：把测试库恢复到「正常启动后」的状态（三行 + 旗标），不影响同包后续用例。
	if err := d.SetSetting(taskCategoriesSeedFlag, ""); err != nil {
		t.Fatal(err)
	}
	if err := d.seedDefaultTaskCategories(); err != nil {
		t.Fatal(err)
	}
	countDefaultCategories(t, d, 3)
}

// countDefaultCategories 断言三个内置类别各恰好一行（按 nkey 计数）且逐个可查。
func countDefaultCategories(t *testing.T, d *DB, want int) {
	t.Helper()
	for _, name := range DefaultTaskCategoryNames {
		c, err := d.TaskCategoryByName(name)
		if err != nil {
			t.Fatalf("查类别 %q: %v", name, err)
		}
		if want > 0 && c == nil {
			t.Errorf("内置类别 %q 未播种", name)
		}
	}
	var n int
	if err := d.QueryRow(`SELECT count(*) FROM task_categories WHERE nkey = ANY($1)`,
		[]string{TaskCategoryKey(TaskCategoryAssetCollection), TaskCategoryKey(TaskCategoryPentest), TaskCategoryKey(TaskCategoryVulnVerify)},
	).Scan(&n); err != nil {
		t.Fatalf("数内置类别: %v", err)
	}
	if n != want {
		t.Errorf("内置类别行数 = %d, want %d", n, want)
	}
}

func TestTaskCategoryByNameAndKeys(t *testing.T) {
	// 纯口径部分：不碰库。
	if TaskCategoryKey("  资产收集 ") != TaskCategoryKey("资产收集") {
		t.Fatal("nkey 口径应折叠空白")
	}
	if TaskCategoryKey("") != "" {
		t.Fatal("空名应得空 nkey")
	}
	if !SameTaskCategoryName("资产收集", " 资产收集") {
		t.Fatal("同义名应判等")
	}
	if SameTaskCategoryName("资产收集", "") || SameTaskCategoryName("", "") {
		t.Fatal("任一为空应判不等")
	}
	if SameTaskCategoryName("资产收集", "渗透测试") {
		t.Fatal("不同类别不得判等")
	}

	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()

	// 播种后的库：按名（含空白变体）能查到资产收集。
	for _, name := range []string{"资产收集", "  资产收集  "} {
		c, err := d.TaskCategoryByName(name)
		if err != nil {
			t.Fatal(err)
		}
		if c == nil || c.NKey != TaskCategoryKey(TaskCategoryAssetCollection) {
			t.Fatalf("TaskCategoryByName(%q) = %+v, want 资产收集", name, c)
		}
	}
	// 查不到 ⇒ (nil, nil)，不是错误。
	missing, err := d.TaskCategoryByName("不存在的类别")
	if err != nil {
		t.Fatal(err)
	}
	if missing != nil {
		t.Fatalf("不存在的类别应返回 nil，got %+v", missing)
	}
}

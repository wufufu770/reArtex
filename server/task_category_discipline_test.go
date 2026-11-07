package server

import (
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
)

// 本文件是「按任务类别给 worker 拼纪律段」（方案 v4 §3-C2）的测试：
//   - workerCategoryDiscipline 是纯函数（无 IO/无 LLM），表驱动钉住：资产收集 →
//     收集纪律段；其它类别/未分类 → ""（通用渗透纪律一字不动）；
//   - 发布链路的 category_id 接通：PublishTask 带合法类别 ⇒ 任务带上类别；带不
//     存在的类别 ⇒ 整个发布失败且什么都不创建。

// TestWorkerCategoryDiscipline 钉纯函数的分派口径。
func TestWorkerCategoryDiscipline(t *testing.T) {
	cases := []struct {
		name     string
		want     bool // true = 应返回纪律段
		contains string
	}{
		{"资产收集", true, "insert_assets"},
		{"  资产收集 ", true, "只收集"}, // nkey 口径：折叠空白
		{"渗透测试", false, ""},
		{"漏洞验证", false, ""},
		{"", false, ""},
		{"资产收集任务", false, ""}, // 名字不完全相同就不是（nkey 精确匹配）
	}
	for _, c := range cases {
		got := workerCategoryDiscipline(c.name)
		if c.want {
			if got == "" {
				t.Errorf("类别 %q 应返回纪律段，实际为空", c.name)
				continue
			}
			if !strings.Contains(got, c.contains) {
				t.Errorf("类别 %q 的纪律段缺 %q", c.name, c.contains)
			}
			// 纪律段是拼在系统提示词尾部的 ⇒ 首行必须带 \n\n（与 constraintBlock 同一套追加法）。
			if !strings.HasPrefix(got, "\n\n") {
				t.Errorf("纪律段应以 \\n\\n 开头（追加法约定）：%q", got[:20])
			}
			for _, frag := range []string{"sqlmap", "nuclei", "hydra", "msf", "record_fact", "report_finding", "被动优先", "429"} {
				if !strings.Contains(got, frag) {
					t.Errorf("类别 %q 的纪律段缺要件 %q", c.name, frag)
				}
			}
		} else if got != "" {
			t.Errorf("类别 %q 不应追加纪律段，实际 %q", c.name, got)
		}
	}
}

// TestPublishTaskAppliesCategory 钉发布链路的 category_id：合法类别落到任务行，
// 非法类别让整个发布失败且什么都不创建（先校验后落库的既有语义）。
func TestPublishTaskAppliesCategory(t *testing.T) {
	s, pg := taskHallTestServer(t)
	assetID := taskHallInsertAsset(t, pg, "publish-cat.example.test")

	// 内置「资产收集」类别由首次启动播种（NewManager → db.Open → seedBuiltins）。
	category, err := pg.TaskCategoryByName(db.TaskCategoryAssetCollection)
	if err != nil {
		t.Fatal(err)
	}
	if category == nil {
		t.Fatal("内置类别「资产收集」未播种——db.Open 的种子链断了")
	}

	taskID, groupID, err := s.PublishTask(PublishTaskRequest{
		Goal:         "对目标做资产收集",
		AssetIDs:     []int64{assetID},
		CategoryID:   &category.ID,
		BudgetTokens: taskHallBudget(),
	})
	if err != nil {
		t.Fatalf("带类别发布失败: %v", err)
	}
	t.Cleanup(func() { _ = pg.DeleteTask(taskID) })
	if groupID <= 0 {
		t.Fatalf("发布应建大厅，group=%d", groupID)
	}

	// 任务行带上类别 id 与类别名（CategoryName 是引擎读纪律段判定的来源）。
	got, err := pg.GetTask(taskID)
	if err != nil || got == nil {
		t.Fatalf("读任务失败: %v", err)
	}
	if got.CategoryID == nil || *got.CategoryID != category.ID {
		t.Fatalf("任务 category_id = %v, want %d", got.CategoryID, category.ID)
	}
	if got.CategoryName != db.TaskCategoryAssetCollection {
		t.Fatalf("任务 category_name = %q, want %q", got.CategoryName, db.TaskCategoryAssetCollection)
	}

	// 类别不存在 ⇒ 参数错误，且什么都不创建。
	bogus := int64(1 << 62)
	desc := "类别不存在发布" + time.Now().Format("150405.000000000")
	_, _, err = s.PublishTask(PublishTaskRequest{
		Description:  desc,
		Goal:         "对目标做资产收集",
		AssetIDs:     []int64{assetID},
		CategoryID:   &bogus,
		BudgetTokens: taskHallBudget(),
	})
	if err == nil {
		t.Fatal("不存在的 category_id 必须让发布失败")
	}

	var tasks int
	if err := pg.QueryRow(`SELECT count(*) FROM tasks WHERE description=$1`, desc).Scan(&tasks); err != nil {
		t.Fatal(err)
	}
	if tasks != 0 {
		t.Fatalf("类别非法的发布不得留下任务（实际 %d 个）", tasks)
	}
}

package db

import (
	"database/sql"
	"fmt"
)

// 内置任务类别（方案 v4 §3-C2）。
//
// 为什么要内置：task_categories 原本只是「列表分组标签」，库里一行都没有 ⇒ 任务引擎
// 没法按类别给 worker 拼纪律提示词。任务 14 实锤：goal 写了「严禁渗透，只做资产收集」，
// 但通用 worker 提示词没有收集纪律，worker 跑着跑着就去伪造 token 做漏洞验证了——
// goal 约束只在文本里，没有结构性支撑。类别就是那个结构性支撑：任务绑「资产收集」
// ⇒ 引擎给它的 worker 追加收集纪律（server/task_llm.go 的 workerCategoryDiscipline）。
//
// 类别靠【名字】识别（与 task_categories.nkey 唯一键同一口径：折叠空白 + 小写），
// 不靠 id：id 随库而变，名字是人在界面上看得见、改得动的那个东西。用户把「资产收集」
// 改了名 ⇒ 纪律随之不再注入——那是用户的显式选择，不是 bug。
const (
	// TaskCategoryAssetCollection 是「资产收集」类别名：命中它的任务，worker 提示词追加收集纪律。
	TaskCategoryAssetCollection = "资产收集"
	// TaskCategoryPentest 是「渗透测试」类别名（通用 worker 纪律，不追加）。
	TaskCategoryPentest = "渗透测试"
	// TaskCategoryVulnVerify 是「漏洞验证」类别名（通用 worker 纪律，不追加）。
	TaskCategoryVulnVerify = "漏洞验证"
	// TaskCategoryBizModeling 是「业务建模」类别名：命中它的任务，建模 agent 按
	// 六段流程产出业务图（biz_nodes/biz_edges/biz_layer_diffs），worker 侧通用
	// 纪律不追加（建模任务的执行体是建模 agent 本身，不是渗透 worker）。
	TaskCategoryBizModeling = "业务建模"
	// TaskCategoryEdgeExploration 是「偏锋探索」类别名：命中它的任务，worker 的
	// 下一条意图带偏锋纪律段（server/task_llm.go 的 workerCategoryDiscipline）；
	// 任务 tasks.edge_mode=true 时 planner 态势块再注入方法论判据段。类别是
	// SetTaskCategory 热改的落点——运行中任务切到它即切偏锋，无需重建 bundle。
	TaskCategoryEdgeExploration = "偏锋探索"
)

// DefaultTaskCategoryNames 是首次启动播种的三个内置类别（顺序即播种顺序）。
var DefaultTaskCategoryNames = []string{TaskCategoryAssetCollection, TaskCategoryPentest, TaskCategoryVulnVerify}

// taskCategoriesSeedFlag 是内置类别播种的 settings 旗标（防重播）。
//
// 为什么要旗标而不是每次启动 ON CONFLICT 补齐：用户删掉某个内置类别是有意的，每次
// 启动都补齐等于「删不掉」。与 intercept_default_rules_v1 同一策略：只播一次。
const taskCategoriesSeedFlag = "task_categories_default_v1"

// seedDefaultTaskCategories 首次启动时写入三个内置类别（幂等）。
//
// 两层幂等：
//   - 旗标 done ⇒ 直接返回（用户之后删改的类别不会被重启复活）；
//   - 旗标未落但同名类别已存在（用户先手建过「资产收集」、或上次写到一半挂了）⇒
//     ON CONFLICT (nkey) DO NOTHING 跳过，不报错、不出重复行。
//
// 读旗标出错 ⇒ 直接返回错误，不「当作没播过」再播一遍：那会把用户删掉的类别复活。
func (d *DB) seedDefaultTaskCategories() error {
	v, _, err := d.GetSetting(taskCategoriesSeedFlag)
	if err != nil {
		return fmt.Errorf("read %s: %w", taskCategoriesSeedFlag, err)
	}
	if v == "done" {
		return nil
	}
	for _, raw := range DefaultTaskCategoryNames {
		name, nkey, err := normalizeTaskCategoryName(raw)
		if err != nil {
			return fmt.Errorf("task category %q: %w", raw, err)
		}
		if _, err := d.Exec(`
INSERT INTO task_categories(name, nkey) VALUES ($1, $2)
ON CONFLICT (nkey) DO NOTHING`, name, nkey); err != nil {
			return fmt.Errorf("task category %q: %w", raw, err)
		}
	}
	return d.SetSetting(taskCategoriesSeedFlag, "done")
}

// bizEdgeCategoriesSeedFlag 是「业务建模 / 偏锋探索」两个功能类别的独立 settings
// 旗标。为什么与上方三类分开一个旗标：三类是渗透编排的内置底座，这两个是功能
// 扩展——独立旗标让两组可独立演进/回滚（撤掉偏锋功能时只需撤 seed 调用，不动
// 基础三类的旗标语义）。幂等语义与 seedDefaultTaskCategories 完全同款。
const bizEdgeCategoriesSeedFlag = "task_categories_biz_edge_v1"

// seedBizEdgeTaskCategories 首次启动时写入「业务建模」「偏锋探索」两个功能类别（幂等）。
func (d *DB) seedBizEdgeTaskCategories() error {
	v, _, err := d.GetSetting(bizEdgeCategoriesSeedFlag)
	if err != nil {
		return fmt.Errorf("read %s: %w", bizEdgeCategoriesSeedFlag, err)
	}
	if v == "done" {
		return nil
	}
	for _, raw := range []string{TaskCategoryBizModeling, TaskCategoryEdgeExploration} {
		name, nkey, err := normalizeTaskCategoryName(raw)
		if err != nil {
			return fmt.Errorf("task category %q: %w", raw, err)
		}
		if _, err := d.Exec(`
INSERT INTO task_categories(name, nkey) VALUES ($1, $2)
ON CONFLICT (nkey) DO NOTHING`, name, nkey); err != nil {
			return fmt.Errorf("task category %q: %w", raw, err)
		}
	}
	return d.SetSetting(bizEdgeCategoriesSeedFlag, "done")
}

// TaskCategoryKey 返回类别名的归一键：与 task_categories.nkey 唯一键同一口径（折叠空白
// + 小写）。空名返回空串。
func TaskCategoryKey(name string) string {
	_, nkey, err := normalizeTaskCategoryName(name)
	if err != nil {
		return ""
	}
	return nkey
}

// SameTaskCategoryName 判两个类别名是否指同一类别（nkey 口径）。任一为空 ⇒ false。
func SameTaskCategoryName(a, b string) bool {
	ka := TaskCategoryKey(a)
	return ka != "" && ka == TaskCategoryKey(b)
}

// TaskCategoryByName 按名字（nkey 口径）查类别；不存在返回 (nil, nil)。
//
// 给 LLM 用的建任务入口（spawn_task 的 category 参数）用它：模型记得住「资产收集」，
// 记不住某个库里的类别 id。
func (d *DB) TaskCategoryByName(name string) (*TaskCategory, error) {
	nkey := TaskCategoryKey(name)
	if nkey == "" {
		return nil, nil
	}
	category, err := scanTaskCategory(d.QueryRow(`
SELECT `+taskCategoryCols+`
FROM task_categories category
LEFT JOIN tasks task ON task.category_id=category.id
WHERE category.nkey=$1
GROUP BY category.id`, nkey))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &category, nil
}

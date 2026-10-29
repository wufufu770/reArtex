package agent

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestListAssetsOmitsCrossTaskIDs pins L8：list_assets 的输出不得带出「还有哪些
// 别的任务也引用了这个资产」。那串 task_ids 越过任务边界 —— 拿到它就等于知道别的
// 任务在跑什么、覆盖了哪些目标。
//
// 断言打在【工具真实输出】上（不是某个 helper 的返回值）：agent 读到的就是这份 JSON。
func TestListAssetsOmitsCrossTaskIDs(t *testing.T) {
	d := testDB(t)
	defer d.Close()

	stamp := time.Now().UnixNano()
	host := fmt.Sprintf("list-assets-cross-%d.example.test", stamp)

	taskA, err := d.CreateTask("list_assets scope A", "读范围", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(taskA.ID)
	taskB, err := d.CreateTask("list_assets scope B", "别人的任务", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(taskB.ID)

	assets := d.Assets()
	// 调用者任务的范围：只声明这个根域。
	seedTaskScope(t, d, taskA.ID, "root_domain", host)
	// 一条同时被两个任务引用的资产：任务 A 能看到它，task_ids 里却有任务 B。
	var assetID int64
	if err := d.QueryRow(`INSERT INTO assets(type,domain,root_domain,task_ids,last_seen)
VALUES ('subdomain',$1,$1,ARRAY[$2,$3]::bigint[],now()) RETURNING id`, host, taskA.ID, taskB.ID).Scan(&assetID); err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM assets WHERE id=$1`, assetID) //nolint:errcheck

	tools := NewToolSet(d.Exploration(taskA.ExplorationID), "planner")
	tools.SetTaskID(taskA.ID)
	tools.SetAssetStore(assets, d.Companies())

	out := callReadJSON(t, tools.listAssets(), fmt.Sprintf(`{"dsl":"domain==%s","limit":10}`, host))
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), host) {
		t.Fatalf("list_assets 没返回范围内资产，断言无意义: %s", raw)
	}
	if strings.Contains(string(raw), "task_ids") {
		t.Fatalf("list_assets 输出仍带跨任务 id 列表: %s", raw)
	}

	items, _ := out.(map[string]any)["assets"].([]any)
	for _, item := range items {
		obj, _ := item.(map[string]any)
		if _, ok := obj["task_ids"]; ok {
			t.Fatalf("资产对象仍带 task_ids: %+v", obj)
		}
	}
}

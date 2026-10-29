package db

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestAssetJSONOmitsCrossTaskIDs pins L8（读面）：资产行序列化出去时不得带出
// 「还有哪些别的任务引用了它」——那串 id 越过任务边界，agent 拿到就等于知道了别的
// 任务在做什么。
//
// 字段本身仍留在 Go 结构里（任务删除时的归属判定、归档都要读它），只是不再
// 序列化出去：内部判定用 Go 字段，边界上不给跨任务信息。
func TestAssetJSONOmitsCrossTaskIDs(t *testing.T) {
	asset := &Asset{
		ID:      7,
		Type:    "subdomain",
		Domain:  "cross-task.example.test",
		TaskIDs: []int64{11, 22},
	}
	raw, err := json.Marshal(asset)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "task_ids") {
		t.Fatalf("资产 JSON 仍带跨任务 id 列表: %s", raw)
	}
	// 结构体字段必须还在：内部归属判定（任务删除时解绑 task_ids）依赖它。
	if len(asset.TaskIDs) != 2 || asset.TaskIDs[0] != 11 || asset.TaskIDs[1] != 22 {
		t.Fatalf("资产结构体的 task_ids 字段不能被删（内部归属判定要用）: %+v", asset.TaskIDs)
	}
}

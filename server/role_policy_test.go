package server

// P2 分层经济（role_policy）的验收用例。形状校验是纯函数（ParseRolePolicy），
// 不碰库即可覆盖主要纪律；引用校验与 lane 解析走桩。

import "testing"

func TestParseRolePolicyShapes(t *testing.T) {
	exists := func(int64) bool { return true }

	// 合法：worker 带 backup；planner/mainagent 只有 primary。
	if _, err := ParseRolePolicy(`{"worker":{"primary":5,"backup":6},"planner":{"primary":2},"mainagent":{"primary":1}}`, exists); err != nil {
		t.Fatalf("合法形状不应报错: %v", err)
	}
	// 空 = 未配置。
	if rp, err := ParseRolePolicy("", exists); err != nil || len(rp) != 0 {
		t.Fatalf("空策略应得空 map: %v %v", rp, err)
	}
	// 决策槽位结构性禁止 backup（红队 P0-1 修订的落点）。
	for _, lane := range []string{"planner", "mainagent"} {
		raw := `{"` + lane + `":{"primary":1,"backup":2}}`
		if _, err := ParseRolePolicy(raw, exists); err == nil {
			t.Fatalf("%s 带 backup 应被结构性拒绝", lane)
		}
	}
	// 未知 lane / 非法 primary / backup==primary。
	if _, err := ParseRolePolicy(`{"judge":{"primary":1}}`, exists); err == nil {
		t.Fatalf("未知 lane 应拒绝")
	}
	if _, err := ParseRolePolicy(`{"worker":{"primary":0}}`, exists); err == nil {
		t.Fatalf("primary=0 应拒绝")
	}
	if _, err := ParseRolePolicy(`{"worker":{"primary":5,"backup":5}}`, exists); err == nil {
		t.Fatalf("backup==primary 应拒绝")
	}
	// 引用不存在。
	if _, err := ParseRolePolicy(`{"worker":{"primary":999}}`, func(int64) bool { return false }); err == nil {
		t.Fatalf("引用不存在的 profile 应拒绝")
	}
	// 坏 JSON。
	if _, err := ParseRolePolicy(`{bad`, exists); err == nil {
		t.Fatalf("坏 JSON 应拒绝")
	}
}

// TestRoleLaneStageSemantics lane 游标的阶段语义（内存态；不碰任务链——红队 P0-2）。
func TestRoleLaneStageSemantics(t *testing.T) {
	task, lane := "stage-test-task", "worker"
	setRoleLaneStage(task, lane, 0)
	if got := roleLaneStageOf(task, lane); got != 0 {
		t.Fatalf("初始 stage 应为 0: %d", got)
	}
	setRoleLaneStage(task, lane, 1)
	if got := roleLaneStageOf(task, lane); got != 1 {
		t.Fatalf("stage 推进失败: %d", got)
	}
	// 决策槽位暂停态。
	setRoleLaneStage(task, "planner", 2)
	if got := roleLaneStageOf(task, "planner"); got != 2 {
		t.Fatalf("planner 暂停态失败: %d", got)
	}
	// 不同 lane 互不串账。
	if got := roleLaneStageOf(task, "worker"); got != 1 {
		t.Fatalf("lane 串账: %d", got)
	}
}

package server

// out_of_scope 升级提示的动态化（0.15.56）测试：被拒目标与已授权域同根域时，
// 提示必须点名那条「一次沉淀」的人工授权（root_domain=<基域>），把「agent 只能
// 加精确行、根域授权是人工决定」说清楚；取不到同根域回落空串（静态文案兜底）。
//
// 生产现场：task 9 的升级提示让 agent 「用 add_task_scope 走扩权」——但
// root_domain 不在 agent 白名单里，agent 照做只会被拒，然后逐个子域硬撞
// （03:49–03:56 每分钟一次「需人工处理」）。
import (
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestOutOfScopeHintNamesRootDomainSettle(t *testing.T) {
	s, pg := taskHallTestServer(t)
	task, err := s.m.CreateTask("提示沉淀验收", "goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	taskID, err := strconv.ParseInt(task.ID, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pg.DeleteTask(taskID) })

	base := "hint-" + strconv.FormatInt(time.Now().UnixNano(), 10) + ".example.test"
	if _, err := pg.Assets().AddManualScope(taskID, "subdomain", "www."+base, "测试范围"); err != nil {
		t.Fatal(err)
	}

	msg := `[SoD] 拒绝执行：reason_code=out_of_scope；复合动作第 2/2 条子意图被闸门拒绝：` +
		`目标 "render.` + base + `" 不在本任务的授权范围内（工具=Bash）`
	hint := s.outOfScopeHint(taskID, msg)
	if hint == "" {
		t.Fatal("同根域被拒应产出动态提示，而不是回落静态文案")
	}
	// .test 单标签 TLD ⇒ 基域 example.test（机制与生产域 lingguang.com 同构）。
	if !strings.Contains(hint, "root_domain=example.test") {
		t.Fatalf("动态提示必须点名一次沉淀的根域授权行, got %s", hint)
	}
	if !strings.Contains(hint, "已自动放行") {
		t.Fatalf("动态提示应说明解析类探测已放行, got %s", hint)
	}
}

func TestOutOfScopeHintFallbacks(t *testing.T) {
	s, pg := taskHallTestServer(t)
	task, err := s.m.CreateTask("提示回落验收", "goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	taskID, err := strconv.ParseInt(task.ID, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pg.DeleteTask(taskID) })

	// 无范围行的任务：没有同根域 ⇒ 空串（静态文案兜底）。
	unrelated := `[SoD] 拒绝执行：reason_code=out_of_scope；目标 "elsewhere.example.net" 不在本任务的授权范围内（工具=Bash）`
	if got := s.outOfScopeHint(taskID, unrelated); got != "" {
		t.Fatalf("非同根域应回落空串, got %s", got)
	}
	// 消息里没有可抠的目标 ⇒ 空串。
	if got := s.outOfScopeHint(taskID, "[SoD] 拒绝执行：reason_code=out_of_scope；无目标信息"); got != "" {
		t.Fatalf("取不到被拒目标应回落空串, got %s", got)
	}
	// 非法 taskID ⇒ 空串。
	if got := s.outOfScopeHint(0, unrelated); got != "" {
		t.Fatalf("taskID<=0 应回落空串, got %s", got)
	}
}

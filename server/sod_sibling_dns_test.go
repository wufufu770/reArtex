package server

// 「同根域 DNS 发现」切面（0.15.56）的接线验收：真实路径（真 server → 真任务
// → 真意图 → runWorkerStep → 链裁决 → 决策账本）。
//
// 生产现场（task 9 ali-1124）：范围只有 agent 沉淀的一条精确子域行
// www.lingguang.com，worker 每碰一个兄弟子域（cognihome/render/agi-static…）
// 就 out_of_scope 一次，深夜每分钟一次升级。切面语义：解析（dig/nslookup/
// whois/host 单命令）随已授权根域放行——发现不许探测、探测了才能登记是自锁；
// 触碰（curl 等）仍按越界拒绝，等人工的根域授权行。
//
// 与 sod_wiring_e2e_test.go 的分工：那边钉「链挂没挂」，这边钉「切面放什么、
// 拦什么」——同一条真实路径，不同的裁决断言。
import (
	"encoding/json"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/internal/sodpolicy"
)

// siblingDNSFixture 搭一次切面验收：真 server + 真任务 + 一条精确子域范围行
// （www.<base>）+ 假模型。命令由 commandFn 用基域拼（兄弟子域必须与范围行
// 同基域）。返回的 base 是测试基域本身（eTLD+1）。形态照抄 newSoDWiringFixture，
// 只换范围与命令。
func siblingDNSFixture(t *testing.T, commandFn func(base string) string) (*sodWiringFixture, string) {
	t.Helper()
	s, pg := sodWiringServer(t)

	base := fmt.Sprintf("sib-%d.example.test", time.Now().UnixNano())
	task, err := s.m.CreateTask("同根域 DNS 发现切面", "验证解析随根域放行、触碰照拒", nil, 0, 0)
	if err != nil {
		t.Fatalf("建任务失败: %v", err)
	}
	taskID, err := strconv.ParseInt(task.ID, 10, 64)
	if err != nil {
		t.Fatalf("任务 id 不是数字: %v", err)
	}
	t.Cleanup(func() { _ = pg.DeleteTask(taskID) })

	// 精确子域行——生产现场 task 9 的形态（agent 只能沉淀精确行）。
	if _, err := pg.Assets().AddManualScope(taskID, "subdomain", "www."+base, "SoD 切面验收"); err != nil {
		t.Fatalf("声明任务范围失败: %v", err)
	}

	nodeID, err := task.Store.AddNode(db.KindIntent, map[string]any{
		"text": "对已授权根域的兄弟子域做一次探测",
	}, 0, "open", "planner", nil)
	if err != nil {
		t.Fatalf("建意图失败: %v", err)
	}
	intent, err := task.Store.GetNode(nodeID)
	if err != nil || intent == nil {
		t.Fatalf("取意图失败: %v", err)
	}

	prov := &sodWiringProvider{target: "www." + base, command: commandFn(base)}
	sodWiringBindProvider(t, s, pg, prov)

	return &sodWiringFixture{
		server: s, pg: pg, task: task, intent: intent, prov: prov, taskID: taskID,
	}, base
}

// siblingScopeCheck 从账本行里取出 scope_check 的裁决值。
func siblingScopeCheck(t *testing.T, rec *db.SoDDecision) string {
	t.Helper()
	var checks map[string]string
	if len(rec.Checks) > 0 {
		if err := json.Unmarshal(rec.Checks, &checks); err != nil {
			t.Fatalf("账本 checks 解不开: %v", err)
		}
	}
	return checks["scope_check"]
}

// TestSiblingDNSDiscoveryAllowedForResolve 钉住放行面：范围只有一条精确子域行
// 时，对兄弟子域的【解析】命令必须放行，且审计理由可回放（pass(
// sibling_domain_dns(<基域>))）。此前这类命令一律 out_of_scope（实测 task 9
// 深夜连拒风暴，每分钟一次升级）。
func TestSiblingDNSDiscoveryAllowedForResolve(t *testing.T) {
	fx, _ := siblingDNSFixture(t, func(b string) string {
		return `dig +short render.` + b + ` A`
	})

	fx.runWorkerIntent(t)

	decisions := sodWiringDecisions(t, fx.pg, fx.taskID)
	if len(decisions) == 0 {
		t.Fatal("切面用例没有产生决策行 —— 链没接到 hook 上（见 sod_wiring_e2e_test.go）")
	}
	rec := decisions[0]
	if rec.Verdict != sodpolicy.VerdictApprove {
		t.Fatalf("兄弟子域的解析命令应随已授权根域放行, verdict=%s reason=%s",
			rec.Verdict, rec.ReasonCode)
	}
	// .test 是单标签 TLD：范围行 www.<base> 与被查 render.<base> 的共同基域都是
	// example.test（生产域 lingguang.com 的基域就是它自己，语义同构）。
	if got := siblingScopeCheck(t, rec); got != "pass(sibling_domain_dns(example.test))" {
		t.Fatalf("审计理由必须可回放基域, scope_check=%q", got)
	}
	// 闭环：链放行了 ⇒ 执行器消费凭证 ⇒ 工具真的跑了。
	if ok := fx.task.Guard.Attributions()["ok"]; ok == 0 {
		t.Fatalf("dig 命令没有真的执行（guard 归因=%v）", fx.task.Guard.Attributions())
	}
}

// TestSiblingDNSTouchStillDenied 钉住拦截面：同一个兄弟子域，【触碰】（curl）
// 仍按越界拒绝——发现随根域放行，触碰必须有人工的根域授权行。
func TestSiblingDNSTouchStillDenied(t *testing.T) {
	fx, _ := siblingDNSFixture(t, func(b string) string {
		return `curl -s https://render.` + b + `/`
	})

	fx.runWorkerIntent(t)

	decisions := sodWiringDecisions(t, fx.pg, fx.taskID)
	if len(decisions) == 0 {
		t.Fatal("切面用例没有产生决策行")
	}
	rec := decisions[0]
	if rec.Verdict != sodpolicy.VerdictDeny || rec.ReasonCode != sodpolicy.ReasonOutOfScope {
		t.Fatalf("兄弟子域的触碰必须仍按越界拒绝, verdict=%s reason=%s", rec.Verdict, rec.ReasonCode)
	}
	if ok := fx.task.Guard.Attributions()["ok"]; ok != 0 {
		t.Fatalf("被拒的 curl 不得执行（guard 归因=%v）", fx.task.Guard.Attributions())
	}
}

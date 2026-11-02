package agent

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/db"
)

// 本文件钉住「策略提议必须在任务内发生」这条规则。
//
// 缺口在哪：strategy_search 有显式的任务上下文检查（agent/strategy_tools.go 的
// scopeTaskID 判定），而 strategy_propose 只继承了**通用**的 nil-store 守卫
// （needExploration：探索图句柄为空 ⇒ 拒绝）。两者不是一回事：
//
//   - nil-store 守卫判的是「有没有探索图句柄」；
//   - 任务上下文判的是「这条探索图是不是属于某个任务」。
//
// 一份挂在**不属于任何任务**的探索图上（例如平台级工具集复用了某条探索图）
// 能通过前者、却根本没有授权边界 —— 策略库里会多出一条没有归属的提议，
// 而 proposals 表**没有 task_id 列**（db/strategy_proposal.go），事后没人说得清
// 它是从哪个任务沉淀出来的。
//
// 两条腿都要钉：无任务上下文 ⇒ 拒绝且**一个字节都不许落库**；在任务内 ⇒ 正常受理。

// TestStrategyProposeRequiresTaskContext 钉住 strategy_propose 的任务上下文要求。
func TestStrategyProposeRequiresTaskContext(t *testing.T) {
	d := testDB(t)
	t.Cleanup(func() { d.Close() })

	sfx := strategyToolSuffix()
	name := fmt.Sprintf("无任务上下文的策略-%d", sfx)

	// ① 没有任务上下文：工具集挂着探索图，但那条探索图**不属于任何任务**。
	expID, err := d.CreateExploration("worker", "不属于任何任务的探索图")
	if err != nil {
		t.Fatalf("建探索图: %v", err)
	}
	t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM explorations WHERE id=$1`, expID) })
	noTask := NewToolSet(d.Exploration(expID), "planner")
	noTask.SetAssetStore(d.Assets(), d.Companies())
	if id := noTask.scopeTaskID(); id > 0 {
		t.Fatalf("夹具前提不成立：这条探索图竟然解析出了任务 id %d", id)
	}

	out := strategyToolCall(t, strategyToolByName(t, noTask, "strategy_propose"), map[string]any{
		"name":     name,
		"summary":  "没有任务上下文时不该被写进策略库",
		"reason":   "测试",
		"steps":    []string{"curl -X POST https://$TARGET/$PATH"},
		"evidence": []any{},
	})
	if !strings.Contains(out, "缺少任务上下文") {
		t.Fatalf("无任务上下文时必须拒绝提议策略, got %q", out)
	}

	// 拒绝必须是**零写入**：策略与提议都不许落库。
	var strategies int
	if err := d.QueryRow(`SELECT count(*) FROM strategies WHERE name=$1`, name).Scan(&strategies); err != nil {
		t.Fatalf("数策略: %v", err)
	}
	if strategies != 0 {
		t.Fatalf("被拒的提议仍写进了 strategies（%d 行）", strategies)
	}
	var proposals int
	if err := d.QueryRow(`SELECT count(*) FROM strategy_proposals WHERE payload->>'name'=$1`, name).Scan(&proposals); err != nil {
		t.Fatalf("数提议: %v", err)
	}
	if proposals != 0 {
		t.Fatalf("被拒的提议仍登记进了 strategy_proposals（%d 行）", proposals)
	}
}

// TestStrategyProposeAcceptedInsideTask 钉住「在任务内 ⇒ 正常受理」。
//
// 拒绝规则不能顺带把正常路径也堵死：任务内的提议要真的落库（隔离态策略 +
// pending 提议），否则功能就是不可用。
func TestStrategyProposeAcceptedInsideTask(t *testing.T) {
	fx := newStrategyToolFixture(t)
	sfx := strategyToolSuffix()

	name := fmt.Sprintf("任务内的策略-%d", sfx)
	out := strategyToolCall(t, strategyToolByName(t, fx.tools, "strategy_propose"), map[string]any{
		"name":     name,
		"summary":  "任务上下文齐备时的正常提议",
		"reason":   "e2e 这条值得沉淀",
		"steps":    []string{"curl -X POST https://$TARGET/$PATH"},
		"evidence": []any{},
	})
	var parsed struct {
		StrategyID     int64  `json:"strategy_id"`
		ProposalID     int64  `json:"proposal_id"`
		State          string `json:"state"`
		ProposalStatus string `json:"proposal_status"`
	}
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("解析提议结果失败: %v（原文 %s）", err, out)
	}
	if parsed.StrategyID <= 0 {
		t.Fatalf("任务内的提议没有被受理: %s", out)
	}
	t.Cleanup(func() { _, _ = fx.db.Exec(`DELETE FROM strategies WHERE id=$1`, parsed.StrategyID) })
	if parsed.State != db.StrategyQuarantined {
		t.Fatalf("提议出来的策略状态 = %q，期望 %q", parsed.State, db.StrategyQuarantined)
	}
	if parsed.ProposalStatus != db.StrategyProposalPending {
		t.Fatalf("提议状态 = %q，期望 %q", parsed.ProposalStatus, db.StrategyProposalPending)
	}
}

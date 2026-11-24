package taskintelligence

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

// ---------- 主成功路径：各信号 → 建议的映射 ----------

func TestDecideAutoSignals(t *testing.T) {
	r := NewRegistry()

	// 平稳快照：标准档 + 基线能力，无额外建议。
	plain := &TaskContextSnapshot{
		Scope:    ScopeSummary{AssetRows: 3},
		Assets:   AssetSummary{InScope: 2},
		BizModel: BizModelSummary{Version: 3, UpdatedAt: time.Now(), LayerDiffs: 1},
		Facts:    FactsSummary{Total: 10, Hypotheses: 2},
		Evidence: EvidenceSummary{Count: 8},
		Findings: FindingsSummary{Open: 1},
		History:  HistorySummary{Executions: 5},
	}
	dec := NormalizeAutoDecision(DecideAuto(plain), r)
	if dec.Difficulty != DiffEasy || dec.ExecutionDepth != DepthQuick {
		t.Fatalf("小目标应 easy/quick, got %s/%s", dec.Difficulty, dec.ExecutionDepth)
	}
	if len(dec.Capabilities) != 1 || dec.Capabilities[0] != CapTestingStandard {
		t.Fatalf("平稳任务只应有基线能力: %v", dec.Capabilities)
	}

	// 业务缺口 → 建议补建模（TI-04 复用）。
	gap := &TaskContextSnapshot{BizModel: BizModelSummary{Version: 0}}
	dec = NormalizeAutoDecision(DecideAuto(gap), r)
	if !contains(dec.Capabilities, CapModelingBusiness) {
		t.Fatalf("业务缺口应建议 modeling.business: %v", dec.Capabilities)
	}

	// 证据缺失 → 建议偏锋探索。
	noEv := &TaskContextSnapshot{
		BizModel: BizModelSummary{Version: 1, UpdatedAt: time.Now()},
		Facts:    FactsSummary{Total: 5},
		Evidence: EvidenceSummary{Count: 0},
	}
	dec = NormalizeAutoDecision(DecideAuto(noEv), r)
	if !contains(dec.Capabilities, CapExplorationEdge) {
		t.Fatalf("无证据应建议 exploration.edge: %v", dec.Capabilities)
	}

	// 假设积压 → 深入验证 + 深度抬升。
	hyp := &TaskContextSnapshot{
		BizModel: BizModelSummary{Version: 1, UpdatedAt: time.Now()},
		Facts:    FactsSummary{Total: 5, Hypotheses: 12},
		Evidence: EvidenceSummary{Count: 4},
	}
	dec = NormalizeAutoDecision(DecideAuto(hyp), r)
	if !contains(dec.Capabilities, CapValidationDeep) || dec.ExecutionDepth != DepthDeep {
		t.Fatalf("假设积压应建议 validation.deep 且深度 deep: %v depth=%s", dec.Capabilities, dec.ExecutionDepth)
	}

	// 发现全确认 → 报告收口。
	done := &TaskContextSnapshot{
		BizModel: BizModelSummary{Version: 1, UpdatedAt: time.Now()},
		Findings: FindingsSummary{Confirmed: 2},
	}
	dec = NormalizeAutoDecision(DecideAuto(done), r)
	if !contains(dec.Capabilities, CapReportingResult) {
		t.Fatalf("发现清零应建议 reporting.result: %v", dec.Capabilities)
	}

	// 大目标 + 假设积压 = 双硬信号 → expert；预算压力封顶深度。
	big := &TaskContextSnapshot{
		Scope:    ScopeSummary{AssetRows: 60},
		BizModel: BizModelSummary{Version: 1, UpdatedAt: time.Now(), LayerDiffs: 12},
		Facts:    FactsSummary{Total: 5, Hypotheses: 22},
		Evidence: EvidenceSummary{Count: 4},
		History:  HistorySummary{Executions: 200},
	}
	dec = NormalizeAutoDecision(DecideAuto(big), r)
	if dec.Difficulty != DiffExpert {
		t.Fatalf("双硬信号应 expert, got %s", dec.Difficulty)
	}
	// 预算收紧（lean）时深度封顶 standard。
	if dec.ExecutionDepth != DepthStandard || dec.BudgetProfile != BudgetLean {
		t.Fatalf("预算压力应封顶 standard/lean, got %s/%s", dec.ExecutionDepth, dec.BudgetProfile)
	}
}

// 失败/降级路径：nil 快照最保守；分节不可用拉低置信度但不伪造判断；
// 分节失败的业务模型走「宁补勿猜」。
func TestDecideAutoDegraded(t *testing.T) {
	dec := DecideAuto(nil)
	if dec.Confidence >= 0.5 || dec.Rationale == "" {
		t.Fatalf("nil 快照应最保守且给理由: %+v", dec)
	}

	// 全分节不可用（SnapshotBuilder 对 nil provider 的行为）：置信度降到下限，
	// 仍产出结构完整决策。
	snap := &TaskContextSnapshot{}
	for _, errPtr := range []*error{&snap.Scope.Err, &snap.Assets.Err, &snap.Exploration.Err,
		&snap.BizModel.Err, &snap.Facts.Err, &snap.Evidence.Err, &snap.Findings.Err, &snap.History.Err} {
		*errPtr = &testError{msg: "provider 未接线"}
	}
	dec = DecideAuto(snap)
	if dec.Confidence != 0.3 {
		t.Fatalf("全不可用应到置信度下限 0.3, got %v", dec.Confidence)
	}
	if len(dec.Capabilities) < 1 || dec.Capabilities[0] != CapTestingStandard {
		t.Fatalf("基线能力应始终在场: %v", dec.Capabilities)
	}

	// 业务分节失败 → 宁补勿猜（建议补建模且带原因）。
	bizSnap := &TaskContextSnapshot{}
	bizSnap.BizModel.Err = &testError{msg: "db down"}
	dec = NormalizeAutoDecision(DecideAuto(bizSnap), NewRegistry())
	if !contains(dec.Capabilities, CapModelingBusiness) {
		t.Fatalf("分节失败应建议补建模: %v", dec.Capabilities)
	}
	if !strings.Contains(dec.DelegateReasons[joinIdx(dec.Capabilities, CapModelingBusiness)], "db down") {
		t.Fatalf("建议原因应带分节失败原因: %v", dec.DelegateReasons)
	}
}

// ---------- 原则 6：建议只指注册表里真实启用能力的路 ----------

func TestNormalizeAutoDecisionDropsUnknown(t *testing.T) {
	r := NewRegistry()
	if err := r.Disable(CapValidationDeep); err != nil {
		t.Fatal(err)
	}
	dec := AutoDecision{
		Capabilities:    []string{CapValidationDeep, CapExplorationEdge, "ghost.cap"},
		DelegateReasons: []string{"假设积压", "证据缺失", "不存在的能力"},
		Rationale:       "测试",
	}
	out := NormalizeAutoDecision(dec, r)
	if contains(out.Capabilities, "ghost.cap") || contains(out.Capabilities, CapValidationDeep) {
		t.Fatalf("未注册/停用能力应被剔除: %v", out.Capabilities)
	}
	if !contains(out.Capabilities, CapExplorationEdge) {
		t.Fatalf("启用能力应保留: %v", out.Capabilities)
	}
	if !strings.Contains(out.Rationale, "ghost.cap") {
		t.Fatalf("剔除应写进尾注: %s", out.Rationale)
	}
	// 原因与能力保持一一对应。
	if len(out.Capabilities) != len(out.DelegateReasons) {
		t.Fatalf("能力与原因应一一对应: %v vs %v", out.Capabilities, out.DelegateReasons)
	}
}

// ---------- 原则 5：只能下调，没有上调入口 ----------

func TestClampDepthMaxOnlyDown(t *testing.T) {
	dec := AutoDecision{ExecutionDepth: DepthExhaustive}
	clamped, err := ClampDepthMax(dec, DepthStandard)
	if err != nil || clamped.ExecutionDepth != DepthStandard {
		t.Fatalf("应下调到 standard: %s err=%v", clamped.ExecutionDepth, err)
	}
	if !strings.Contains(clamped.Rationale, "风险约束") {
		t.Fatalf("下调应留痕: %s", clamped.Rationale)
	}
	// 已低于上限：不动。
	same, err := ClampDepthMax(AutoDecision{ExecutionDepth: DepthQuick}, DepthStandard)
	if err != nil || same.ExecutionDepth != DepthQuick {
		t.Fatalf("低于上限不应动: %s", same.ExecutionDepth)
	}
	if _, err := ClampDepthMax(dec, "bogus"); err == nil {
		t.Fatal("非法深度档应报错")
	}
}

// ---------- 安全结构性检查：AutoDecision 表达不出越权字段 ----------

// 反射真实字段清单：出现任何范围/审批/策略/派生 Agent 字段即为设计回退
// （原则 5：AUTO 不能修改授权范围、删除审批要求或安全策略——结构体层面就
// 不给这些概念留位置）。
func TestAutoDecisionCannotExpressEscalation(t *testing.T) {
	allowed := map[string]bool{
		"Difficulty": true, "ExecutionDepth": true, "Capabilities": true,
		"DelegateReasons": true, "BudgetProfile": true, "Confidence": true,
		"Rationale": true,
	}
	ty := reflect.TypeOf(AutoDecision{})
	for i := 0; i < ty.NumField(); i++ {
		name := ty.Field(i).Name
		if !allowed[name] {
			t.Fatalf("AutoDecision 出现白名单外字段 %q——越权表达面必须过设计评审", name)
		}
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func joinIdx(list []string, s string) int {
	for i, x := range list {
		if x == s {
			return i
		}
	}
	return -1
}

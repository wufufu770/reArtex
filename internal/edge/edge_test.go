package edge

import (
	"strings"
	"testing"
)

// 注册表硬约束的单测（EDGE E2 验收口径：空判据无法注册、缺上限参数无法注册、
// 隐喻自带判据无法注册）。
func TestRegisterConstraints(t *testing.T) {
	if err := Register(Methodology{ID: "t-empty", Name: "x", Tier: 1, EvidenceGrade: GradeEmpirical}); err == nil {
		t.Fatal("空 CompletionCriterion 的一级条目被注册成功，闸门失效")
	}
	if err := Register(Methodology{ID: "t-nobound", Name: "x", Tier: 1, EvidenceGrade: GradeEmpirical,
		Criterion: &CompletionCriterion{Kind: CriterionCoverage}}); err == nil {
		t.Fatal("缺上限参数的判据被注册成功，防退化闸失效")
	}
	if err := Register(Methodology{ID: "t-metaphor-crit", Name: "x", Tier: 3, EvidenceGrade: GradeMetaphor,
		Criterion: &CompletionCriterion{Kind: CriterionCoverage, Bounds: []UpperBound{{Kind: "t", Limit: 1}}}}); err == nil {
		t.Fatal("隐喻级条目自带判据被注册成功，可证伪性约束失效")
	}
	if err := Register(Methodology{ID: "t-metaphor-ok", Name: "x", Tier: 3, EvidenceGrade: GradeMetaphor,
		HeuristicTags: []string{"启发"}}); err != nil {
		t.Fatalf("合法隐喻条目被拒: %v", err)
	}
}

// 内置注册表完整性：一级条目全部带判据且启用；三级条目全部无判据。
func TestBuiltinRegistry(t *testing.T) {
	all := ListAll()
	if len(all) < 10 {
		t.Fatalf("内置方法论数量异常: %d", len(all))
	}
	for _, m := range all {
		switch m.Tier {
		case 1, 2:
			if m.Criterion == nil {
				t.Errorf("%s: 一/二级条目缺判据", m.ID)
			} else if err := m.Criterion.Validate(); err != nil {
				t.Errorf("%s: %v", m.ID, err)
			}
		case 3:
			if m.Criterion != nil {
				t.Errorf("%s: 隐喻条目不得自带判据", m.ID)
			}
		}
	}
	if _, ok := Get("stpa-uca"); !ok {
		t.Fatal("stpa-uca 未注册")
	}
}

// 触发词闸门：硬触发精确命中、软信号不升级、未命中为 nil、不做模糊扩展。
func TestTriggerScan(t *testing.T) {
	if hit := Scan("用偏锋探索做一次完整探索"); hit == nil || !hit.Hard {
		t.Fatalf("硬触发词未命中: %v", hit)
	}
	if hit := Scan("给个偏门一点的方向看看"); hit == nil || hit.Hard {
		t.Fatalf("软信号判定错误: %v", hit)
	}
	if hit := Scan("换个角度想想"); hit == nil || hit.Hard {
		t.Fatalf("软信号判定错误: %v", hit)
	}
	if hit := Scan("帮我看一下这个任务的目标"); hit != nil {
		t.Fatalf("普通消息误命中: %+v", hit)
	}
	// 宁漏勿误：「偏」单字不命中（模糊匹配被刻意排除）。
	if hit := Scan("这个页面有偏移"); hit != nil {
		t.Fatalf("模糊扩展误命中: %+v", hit)
	}
}

// 熔断①：溯因门禁三退化路径 + 连续计数清零。
func TestDegenerateTracker(t *testing.T) {
	refuted := []string{"登录接口存在未授权访问", "admin api 已证伪"}
	if JudgeAbduction("", "pred", "falsifier", refuted) != AbductionDegenerate {
		t.Fatal("空 basis 未被判退化")
	}
	if JudgeAbduction("依据输出X", "pred", "", refuted) != AbductionDegenerate {
		t.Fatal("空 falsifier 未被判退化")
	}
	if JudgeAbduction("登录接口的未授权访问问题", "pred", "falsifier", refuted) != AbductionDegenerate {
		t.Fatal("撞证伪池未被判退化")
	}
	if JudgeAbduction("依据 docs/端口扫描输出", "pred", "falsifier", refuted) != AbductionOK {
		t.Fatal("干净假设被误判退化")
	}
	tr := &DegenerateTracker{}
	for i := 0; i < DegenerateThreshold-1; i++ {
		if tr.Tripped() {
			t.Fatal("未达阈值即熔断")
		}
		tr.Record(AbductionDegenerate)
	}
	tr.Record(AbductionOK)
	if tr.Tripped() {
		t.Fatal("成功后未清零")
	}
	for i := 0; i < DegenerateThreshold; i++ {
		tr.Record(AbductionDegenerate)
	}
	if !tr.Tripped() {
		t.Fatal("达阈值未熔断")
	}
}

// 熔断③：判据停滞——首次喂入是基线（状态刚产生即推进），之后连续相同哈希
// 才计数；变化清零。
func TestStallTracker(t *testing.T) {
	tr := &StallTracker{}
	if tr.Record("h1") != 0 {
		t.Fatal("首次喂入应作为基线清零")
	}
	for i := 0; i < StallThreshold; i++ {
		tr.Record("h1")
	}
	if !tr.Tripped() {
		t.Fatal("连续无推进未熔断")
	}
	if tr.Record("h2") != 0 {
		t.Fatal("状态推进后未清零")
	}
}

// CriterionHash 的规范化：键序无关、1 与 1.0 等值。
func TestCriterionHash(t *testing.T) {
	h1 := CriterionHash(map[string]any{"a": 1, "b": map[string]any{"x": 2.0}})
	h2 := CriterionHash(map[string]any{"b": map[string]any{"x": 2}, "a": 1.0})
	if h1 == "" || h1 != h2 {
		t.Fatalf("规范化哈希不稳定: %q vs %q", h1, h2)
	}
	h3 := CriterionHash(map[string]any{"a": 2, "b": map[string]any{"x": 2.0}})
	if h3 == h1 {
		t.Fatal("不同状态得到相同哈希")
	}
	if !strings.Contains(h1, "") {
		t.Fatal("unreachable")
	}
}

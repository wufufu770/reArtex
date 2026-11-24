package taskintelligence

import (
	"sort"
	"testing"
)

// 五个标准能力注册齐全且 ID 与 §0.3 表格一致。
func TestStandardCapabilitiesSeeded(t *testing.T) {
	r := NewRegistry()
	want := []string{
		"testing.standard",
		"exploration.edge",
		"modeling.business",
		"validation.deep",
		"reporting.result",
	}
	got := []string{}
	for _, c := range r.ListEnabled() {
		got = append(got, c.ID)
	}
	if len(got) != 5 {
		t.Fatalf("标准能力应恰 5 个, got %d: %v", len(got), got)
	}
	sort.Strings(got)
	sort.Strings(want)
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("能力 ID 与方案表不一致: got %v", got)
		}
	}
}

// 主成功路径：合法登记 + 查询。
func TestRegisterAndLookup(t *testing.T) {
	r := NewRegistry()
	d := CapabilityDescriptor{
		ID:          "custom.probe",
		Version:     "1.0.0",
		Description: "测试自定义能力",
		RiskClass:   RiskLow,
		CostClass:   CostLow,
	}
	if err := r.Register(d); err != nil {
		t.Fatalf("Register: %v", err)
	}
	got, ok := r.Lookup("custom.probe")
	if !ok || got.Version != "1.0.0" {
		t.Fatalf("Lookup 失败: ok=%v got=%+v", ok, got)
	}
}

// 失败路径：重复注册拒绝；升级走 Replace。
func TestRegisterDuplicateRejected(t *testing.T) {
	r := NewRegistry()
	d := CapabilityDescriptor{
		ID:        "testing.standard",
		Version:   "9.9.9",
		RiskClass: RiskLow,
		CostClass: CostLow,
	}
	if err := r.Register(d); err == nil {
		t.Fatal("重复 ID 注册应被拒绝")
	}
	if err := r.Replace(d); err != nil {
		t.Fatalf("Replace 显式换版应放行: %v", err)
	}
	got, _ := r.Lookup("testing.standard")
	if got.Version != "9.9.9" {
		t.Fatalf("Replace 后版本未更新: %s", got.Version)
	}
}

// 失败路径：登记项不合法（ID 形状/未知上下文分节/非法档位）。
func TestRegisterValidationFailures(t *testing.T) {
	r := NewRegistry()
	cases := []CapabilityDescriptor{
		{ID: "nodot", Version: "1", RiskClass: RiskLow, CostClass: CostLow},
		{ID: "x.custom", Version: "", RiskClass: RiskLow, CostClass: CostLow},
		{ID: "x.custom", Version: "1", RiskClass: "severe", CostClass: CostLow},
		{ID: "x.custom", Version: "1", RiskClass: RiskLow, CostClass: "sky"},
		{ID: "x.custom", Version: "1", RiskClass: RiskLow, CostClass: CostLow, RequiredContext: []string{"unknown_section"}},
	}
	for i, d := range cases {
		if err := r.Register(d); err == nil {
			t.Fatalf("用例 %d 应被拒绝: %+v", i, d)
		}
	}
}

// 边界：启用开关只影响建议面，不影响注册表完整性；未知 ID 报错。
func TestDisableEnableRoundtrip(t *testing.T) {
	r := NewRegistry()
	if err := r.Disable("exploration.edge"); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	if _, ok := r.Lookup("exploration.edge"); !ok {
		t.Fatal("Disable 后仍应可查到登记项")
	}
	if got := len(r.ListEnabled()); got != 4 {
		t.Fatalf("禁用后启用能力应剩 4 个, got %d", got)
	}
	if err := r.Enable("exploration.edge"); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	if err := r.Disable("no.such"); err == nil {
		t.Fatal("未注册 ID 的开关操作应报错")
	}
}

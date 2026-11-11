package db

// v0.10.1 回归：MatchExperiences 在「池子非空但全员 0 分」时曾对 scored[0]
// 直接 panic——不只打挂注入拼装预览，launchTask 的真实种子路径走同一函数，
// 新任务目标与 accepted 池零词重叠时建任务请求会直接崩（连接被掐断）。
// 空集 = 无命中 = 空串不注入，必须正常返回。

import (
	"testing"
)

func TestMatchExperiencesZeroOverlapNoPanic(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v)", err)
	}
	defer d.Close()
	if err := d.EnsureLegacySchema(); err != nil {
		t.Fatalf("应用 legacy schema: %v", err)
	}
	if err := d.EnsureExperienceWeightSchema(); err != nil {
		t.Fatalf("应用经验权重 schema: %v", err)
	}

	id, err := d.CreateLegacyNode(&LegacyNode{
		Kind: "experience", Title: "zzqq-xkcd-test-唯一串", Summary: "wwww-qqqk 专属正文", State: "draft",
	})
	if err != nil {
		t.Fatalf("建经验失败: %v", err)
	}
	t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM legacy_nodes WHERE id=$1`, id) })
	// CreateLegacyNode 只收 draft（归档闸），评审进 accepted 走确定性闸。
	if err := d.ReviewLegacyNode(id, "accepted", "tester"); err != nil {
		t.Fatalf("评审 accepted: %v", err)
	}

	// 目标与池零词重叠：修复前这里 panic（scored[0] 越界）。注意 TokenizeForMatch
	// 对 CJK 按单字切 ⇒ 中文 goal 几乎必然与池子共享单字；真正的零重叠要选
	// 与池内文本零共享字符/词元的 ASCII goal。
	got, err := d.MatchExperiences("mindEqn4711 plunge", nil, 5)
	if err != nil {
		t.Fatalf("零重叠匹配不应报错: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("零重叠应返回空集，拿到 %d 条", len(got))
	}

	// goal 为空串（注入拼装预览的缺省形态）：同样必须正常返回。
	if _, err = d.MatchExperiences("", nil, 5); err != nil {
		t.Fatalf("空 goal 匹配不应报错: %v", err)
	}

	// 词重叠命中仍正常工作（词集口径没被改动）。
	if _, err = d.MatchExperiences("关于 zzqq-xkcd-test-唯一串 的复用", nil, 5); err != nil {
		t.Fatalf("正常匹配不应报错: %v", err)
	}
}

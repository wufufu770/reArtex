package db

import (
	"strings"
	"testing"
)

// 任务授权面（EngagementPolicy，0.15.26）读写与枚举纪律测试。
func TestEngagementProfileNormalize(t *testing.T) {
	// 空白/空串回落默认档；合法值原样；非法值报错不静默回落（授权面坏值纪律）。
	for _, in := range []string{"", "  "} {
		got, err := NormalizeEngagementProfile(in)
		if err != nil || got != EngagementAuthorizedActive {
			t.Fatalf("空白应回落默认档：%q → %q %v", in, got, err)
		}
	}
	for _, in := range []string{EngagementAuthorizedActive, EngagementPassiveRecon, EngagementStrictManual} {
		got, err := NormalizeEngagementProfile(in)
		if err != nil || got != in {
			t.Fatalf("合法枚举应原样通过：%q → %q %v", in, got, err)
		}
	}
	if _, err := NormalizeEngagementProfile("yolo_mode"); err == nil {
		t.Fatal("非法值必须报错（不静默回落）")
	}
}

func TestEngagementProfileRoundTrip(t *testing.T) {
	d := findingValidationOpen(t)
	task, err := d.CreateTask("授权面读写", "目标", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(task.ID) })
	id := task.ID

	// 默认档：新任务未设置 = authorized_active（现状行为）。
	if got := d.TaskEngagementProfile(id); got != EngagementAuthorizedActive {
		t.Fatalf("新任务默认档应为 authorized_active，得到 %q", got)
	}
	// 写入各档并读回。
	for _, p := range []string{EngagementPassiveRecon, EngagementStrictManual, EngagementAuthorizedActive} {
		if err := d.SetTaskEngagementProfile(id, p); err != nil {
			t.Fatalf("写档 %q 失败: %v", p, err)
		}
		if got := d.TaskEngagementProfile(id); got != p {
			t.Fatalf("读回应为 %q，得到 %q", p, got)
		}
	}
	// 非法值写侧拒绝（CHECK 约束之外先在应用层拦）。
	if err := d.SetTaskEngagementProfile(id, "yolo_mode"); err == nil {
		t.Fatal("非法档位应被拒绝")
	}
	// 不存在的任务：读回落默认；写报错。
	if got := d.TaskEngagementProfile(999999999); got != EngagementAuthorizedActive {
		t.Fatalf("不存在任务应回落默认档，得到 %q", got)
	}
	if err := d.SetTaskEngagementProfile(999999999, EngagementStrictManual); err == nil || !strings.Contains(err.Error(), "不存在") {
		t.Fatalf("不存在任务写入应报任务不存在: %v", err)
	}
}

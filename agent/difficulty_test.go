package agent

import (
	"testing"

	"github.com/Autumn-27/artex/db"
)

func TestValidDifficultyWhitelist(t *testing.T) {
	for _, s := range []string{LevelEasy, LevelMedium, LevelHard, LevelExpert, db.TaskDifficultyEasy, db.TaskDifficultyMedium, db.TaskDifficultyHard, db.TaskDifficultyExpert} {
		if !ValidDifficulty(s) {
			t.Fatalf("%q 应是合法任务难度", s)
		}
	}
	for _, s := range []string{"", "  ", "insane", "auto", "simple", "Expert", "EASY", "medium "} {
		if ValidDifficulty(s) {
			t.Fatalf("%q 不应是合法任务难度（执行档与未规范化值都拒绝）", s)
		}
	}
}

func TestSuggestDifficultyRules(t *testing.T) {
	cases := []struct {
		name string
		goal string
		desc string
		want string
	}{
		{name: "支付接口", goal: "测试支付接口", want: LevelHard},
		{name: "登录态", goal: "带登录态的后台", want: LevelHard},
		{name: "认证", desc: "需要认证", want: LevelHard},
		{name: "多域名", goal: "多域名资产", want: LevelHard},
		{name: "WAF", goal: "绕不过的 WAF", want: LevelHard},
		{name: "waf 小写", goal: "waf bypass", want: LevelHard},
		{name: "绕过", goal: "尝试绕过", want: LevelHard},
		{name: "收集", goal: "收集子域", want: LevelEasy},
		{name: "枚举", goal: "枚举目录", want: LevelEasy},
		{name: "指纹", desc: "指纹识别", want: LevelEasy},
		{name: "梳理", goal: "梳理入口", want: LevelEasy},
		{name: "收集加支付不是 easy", goal: "收集支付相关接口", want: LevelHard},
		{name: "收集加认证不是 easy", goal: "收集认证入口", want: LevelHard},
		{name: "内网", goal: "进入内网", want: LevelExpert},
		{name: "横向", goal: "横向移动", want: LevelExpert},
		{name: "提权", goal: "提权到 root", want: LevelExpert},
		{name: "多阶段", goal: "多阶段攻击链", want: LevelExpert},
		{name: "expert 盖过 hard", goal: "内网里的支付接口", want: LevelExpert},
		{name: "其余 medium", goal: "看看这个站点", desc: "常规检查", want: LevelMedium},
		{name: "空文本 medium", want: LevelMedium},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, reason := SuggestDifficulty(c.goal, c.desc)
			if got != c.want {
				t.Fatalf("SuggestDifficulty(%q, %q)=%s，想要 %s（理由 %q）", c.goal, c.desc, got, c.want, reason)
			}
			if reason == "" {
				t.Fatal("理由必须非空")
			}
		})
	}
}

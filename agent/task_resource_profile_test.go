package agent

// C-02A：任务难度 → 资源画像。覆盖 4 档 × 不覆盖/覆盖，并钉住这张表
// 不碰 0.15.17 的进程内执行档注册表。

import (
	"sync"
	"testing"
)

func TestDefaultExecutionMode(t *testing.T) {
	cases := []struct {
		level string
		want  ExecutionMode
	}{
		{LevelEasy, ModeFast},
		{LevelMedium, ModeBalanced},
		{LevelHard, ModeBalanced},
		{LevelExpert, ModeSuperHeavy},
		{"", ModeBalanced},
		{"  ", ModeBalanced},
		{"  easy  ", ModeFast},
		{"  expert", ModeSuperHeavy},
		{"simple", ModeBalanced}, // 执行档，不是任务难度
		{"auto", ModeBalanced},
		{"Expert", ModeBalanced}, // 大小写敏感
		{"insane", ModeBalanced},
	}
	for _, c := range cases {
		if got := DefaultExecutionMode(c.level); got != c.want {
			t.Fatalf("DefaultExecutionMode(%q)=%s want %s", c.level, got, c.want)
		}
	}
}

func TestValidResourceMode(t *testing.T) {
	for _, s := range []string{string(ModeFast), string(ModeBalanced), string(ModeSuperHeavy), "  fast  ", "balanced"} {
		if !ValidResourceMode(s) {
			t.Fatalf("%q 应是可覆盖的资源模式", s)
		}
	}
	for _, s := range []string{"", "  ", "auto", "simple", "medium", "hard", "easy", "FAST", "Super_Heavy"} {
		if ValidResourceMode(s) {
			t.Fatalf("%q 不是 C-02 覆盖值（空 = 跟随难度，执行档 auto 也不算）", s)
		}
	}
}

func TestResolveProfileDifficultyDefaults(t *testing.T) {
	// 不手动覆盖：easy/medium/hard/expert → fast/balanced/balanced/super_heavy。
	cases := []struct {
		level string
		want  ExecutionProfile
	}{
		{LevelEasy, resourceProfileFor(ModeFast)},
		{LevelMedium, resourceProfileFor(ModeBalanced)},
		{LevelHard, resourceProfileFor(ModeBalanced)},
		{LevelExpert, resourceProfileFor(ModeSuperHeavy)},
	}
	for _, c := range cases {
		for _, override := range []string{"", "  ", "\t"} {
			got := ResolveProfile(c.level, override)
			if got != c.want {
				t.Fatalf("难度 %s 覆盖 %q → %+v want %+v", c.level, override, got, c.want)
			}
		}
	}
	fast := ResolveProfile(LevelEasy, "")
	if fast.Mode != ModeFast || fast.WorkerConcurrency != 2 || fast.TokenBudget != 1 || fast.PlannerRounds != 3 || fast.RetestScope != RetestScopeKeyOnly || fast.RequireOracle {
		t.Fatalf("fast 画像与卡面不符: %+v", fast)
	}
	balanced := ResolveProfile(LevelHard, "")
	if balanced.Mode != ModeBalanced || balanced.WorkerConcurrency != 3 || balanced.TokenBudget != 2 || balanced.PlannerRounds != 6 || balanced.RetestScope != RetestScopeHighPlus || balanced.RequireOracle {
		t.Fatalf("balanced 画像与卡面不符: %+v", balanced)
	}
	heavy := ResolveProfile(LevelExpert, "")
	if heavy.Mode != ModeSuperHeavy || heavy.WorkerConcurrency != 5 || heavy.TokenBudget != 3 || heavy.PlannerRounds != 10 || heavy.RetestScope != RetestScopeAll || !heavy.RequireOracle {
		t.Fatalf("super_heavy 画像与卡面不符: %+v", heavy)
	}
	if ResolveProfile(LevelMedium, "") != balanced {
		t.Fatal("medium 与 hard 的默认画像必须相同")
	}
}

func TestResolveProfileManualOverride(t *testing.T) {
	// 4 档各自被覆盖到「不是自己的默认模式」，覆盖必须赢。
	cases := []struct {
		level    string
		override string
		wantMode ExecutionMode
	}{
		{LevelEasy, string(ModeSuperHeavy), ModeSuperHeavy}, // 卡面验收：easy 改成 super_heavy
		{LevelMedium, string(ModeFast), ModeFast},
		{LevelHard, string(ModeSuperHeavy), ModeSuperHeavy},
		{LevelExpert, string(ModeFast), ModeFast},
		{LevelEasy, "  " + string(ModeBalanced) + "  ", ModeBalanced},
		{LevelExpert, string(ModeSuperHeavy), ModeSuperHeavy}, // 显式写成默认值，仍然按覆盖走
	}
	for _, c := range cases {
		got := ResolveProfile(c.level, c.override)
		want := resourceProfileFor(c.wantMode)
		if got != want {
			t.Fatalf("难度 %s 覆盖 %q → %+v want %+v", c.level, c.override, got, want)
		}
	}
	// easy → super_heavy 必须带上 oracle 与全量复测，不能因为难度是 easy 就关掉。
	over := ResolveProfile(LevelEasy, string(ModeSuperHeavy))
	if !over.RequireOracle || over.RetestScope != RetestScopeAll || over.WorkerConcurrency != 5 {
		t.Fatalf("easy 覆盖为 super_heavy 应整档生效: %+v", over)
	}
	// expert 被收成 fast 时不再要求 oracle。标注跟生效模式走，不跟难度标签走。
	down := ResolveProfile(LevelExpert, string(ModeFast))
	if down.RequireOracle || down.RetestScope != RetestScopeKeyOnly || down.PlannerRounds != 3 {
		t.Fatalf("expert 覆盖为 fast 应整档生效: %+v", down)
	}
}

func TestResolveProfileIllegalModeFallsBack(t *testing.T) {
	balanced := resourceProfileFor(ModeBalanced)
	// 非空但不在白名单：不跟随难度（expert 不会因此变成 super_heavy），回落 balanced。
	for _, override := range []string{"auto", "simple", "insane", "FAST", "medium"} {
		got := ResolveProfile(LevelExpert, override)
		if got != balanced {
			t.Fatalf("非法覆盖 %q 应回落 balanced，got %+v", override, got)
		}
	}
	// 难度本身非法、模式合法：模式仍生效。
	if got := ResolveProfile("insane", string(ModeFast)); got != resourceProfileFor(ModeFast) {
		t.Fatalf("非法难度不应吞掉合法覆盖: %+v", got)
	}
	// 两边都空/非法：balanced。
	if got := ResolveProfile("", ""); got != balanced {
		t.Fatalf("空输入应是 balanced: %+v", got)
	}
}

func TestResolveProfileDoesNotTouchExecutionRegistry(t *testing.T) {
	const id int64 = 702
	SetExecutionProfile(id, ResolveExecutionProfile(DifficultySimple, ModeFast))
	t.Cleanup(func() { DeleteExecutionProfile(id) })
	before, ok := ExecutionProfileFor(id)
	if !ok {
		t.Fatal("测试夹具没有写入 0.15.17 档位")
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 40; j++ {
				_ = ResolveProfile(LevelExpert, "")
				_ = ResolveProfile(LevelEasy, string(ModeSuperHeavy))
				_ = DefaultExecutionMode(LevelHard)
			}
		}()
	}
	wg.Wait()

	after, ok := ExecutionProfileFor(id)
	if !ok || after != before {
		t.Fatalf("资源映射表改写了执行档注册表: before=%+v after=%+v ok=%v", before, after, ok)
	}
	// 未注册的任务也不会被映射表顺手登记。
	if _, ok := ExecutionProfileFor(id + 1); ok {
		t.Fatal("ResolveProfile 往注册表写入了新任务")
	}
}

package agent

// 0.15.17 执行档位回归：档位矩阵解析 / auto 文本判定 / 请求级 effort 优先级 /
// 窗口帽钳制 / AutoScaler 冷却滞回与升降档 / 注册表并发安全。

import (
	"sync"
	"testing"

	"github.com/Autumn-27/norma/agentcore"
)

func TestResolveExecutionProfileMatrix(t *testing.T) {
	cases := []struct {
		diff       TaskDifficulty
		mode       ExecutionMode
		wantEffort string
		wantCapK   int
		wantBudget int64
	}{
		{DifficultySimple, ModeFast, "low", SimpleModeWindowCapK, 0},
		{DifficultyMedium, ModeBalanced, "medium", 0, 0},
		{DifficultyHard, ModeFast, "medium", 0, 0}, // hard 下限 ≥ medium
		{DifficultyHard, ModeSuperHeavy, "high", 0, SuperHeavyHardTokenBudget},
		{DifficultySimple, ModeSuperHeavy, "high", SimpleModeWindowCapK, SuperHeavyHardTokenBudget},
		{"bogus", ModeBalanced, "medium", 0, 0}, // 非法难度回落，模式仍生效
	}
	for _, c := range cases {
		p := ResolveExecutionProfile(c.diff, c.mode)
		if c.diff == DifficultySimple && p.ContextWindowCapK != c.wantCapK {
			t.Fatalf("难度 %s 帽=%d want %d", c.diff, p.ContextWindowCapK, c.wantCapK)
		}
		if p.ReasoningEffort != c.wantEffort {
			t.Fatalf("难度 %s 模式 %s effort=%q want %q", c.diff, c.mode, p.ReasoningEffort, c.wantEffort)
		}
		if p.HardTokenBudget != c.wantBudget {
			t.Fatalf("模式 %s 预算=%d want %d", c.mode, p.HardTokenBudget, c.wantBudget)
		}
	}
	// 非法难度/模式整体回落默认。
	p := ResolveExecutionProfile("bogus", "bogus")
	if p.Difficulty != DefaultDifficulty || p.Mode != DefaultMode {
		t.Fatalf("非法输入应整体回落默认: %+v", p)
	}
}

func TestAssessAutoProfile(t *testing.T) {
	d, m := AssessAutoProfile("快速检查单个登录口", "", 1, 0, DifficultyAuto, ModeAuto)
	if d != DifficultySimple || m != ModeFast {
		t.Fatalf("快速单点应判 simple/fast, got %s/%s", d, m)
	}
	d, m = AssessAutoProfile("全面深度审计支付与鉴权模块", "", 0, 0, DifficultyAuto, ModeAuto)
	if d != DifficultyHard || m != ModeSuperHeavy {
		t.Fatalf("深度审计应判 hard/super_heavy, got %s/%s", d, m)
	}
	d, m = AssessAutoProfile("常规渗透测试", "", 0, 0, DifficultyAuto, ModeAuto)
	if d != DefaultDifficulty || m != DefaultMode {
		t.Fatalf("中性文本应判默认档, got %s/%s", d, m)
	}
	// 显式值不被 auto 判定覆盖。
	d, m = AssessAutoProfile("快速检查", "", 1, 0, DifficultyHard, ModeSuperHeavy)
	if d != DifficultyHard || m != ModeSuperHeavy {
		t.Fatalf("显式档位被覆盖: %s/%s", d, m)
	}
}

func TestApplyExecutionProfileEffortPrecedence(t *testing.T) {
	SetExecutionProfile(42, ResolveExecutionProfile(DifficultyHard, ModeSuperHeavy))
	t.Cleanup(func() { DeleteExecutionProfile(42) })

	// 无预设：档位 effort 生效。
	opts := &agentcore.Options{}
	ApplyExecutionProfileEffort(opts, 42)
	if opts.ReasoningEffort != "high" {
		t.Fatalf("档位 effort 未生效: %q", opts.ReasoningEffort)
	}
	// 调用方预设优先（档位不覆盖已有值）。
	opts = &agentcore.Options{ReasoningEffort: "low"}
	ApplyExecutionProfileEffort(opts, 42)
	if opts.ReasoningEffort != "low" {
		t.Fatalf("预设 effort 被档位覆盖: %q", opts.ReasoningEffort)
	}
	// 未注册任务：零改动。
	opts = &agentcore.Options{}
	ApplyExecutionProfileEffort(opts, 999)
	if opts.ReasoningEffort != "" {
		t.Fatalf("未注册任务被改动: %q", opts.ReasoningEffort)
	}
	// nil opts 安全。
	ApplyExecutionProfileEffort(nil, 42)
}

func TestClampCompactionWindow(t *testing.T) {
	SetExecutionProfile(43, ResolveExecutionProfile(DifficultySimple, ModeBalanced))
	t.Cleanup(func() { DeleteExecutionProfile(43) })
	if got := ClampCompactionWindow(43, 300); got != SimpleModeWindowCapK {
		t.Fatalf("Simple 帽未生效: %d", got)
	}
	if got := ClampCompactionWindow(43, 128); got != 128 {
		t.Fatalf("低于帽被误抬: %d", got)
	}
	// 无帽档位不钳。
	SetExecutionProfile(44, ResolveExecutionProfile(DifficultyMedium, ModeBalanced))
	if got := ClampCompactionWindow(44, 200); got != 200 {
		t.Fatalf("无帽档位被钳: %d", got)
	}
	// 未注册不钳。
	if got := ClampCompactionWindow(999, 200); got != 200 {
		t.Fatalf("未注册任务被钳: %d", got)
	}
}

func TestAutoScalerCooldownAndDirections(t *testing.T) {
	s := NewAutoScaler(3)
	base := ResolveExecutionProfile(DifficultyMedium, ModeFast)
	critical := TurnSignals{TurnIndex: 10, CriticalSignalFound: true}

	// 第 1 轮就升档（lastSwitchTurn=-100，冷却通过）。
	dec := s.EvaluateTurn(base, critical)
	if !dec.Changed || dec.NewMode != ModeBalanced {
		t.Fatalf("高危信号应 Fast→Balanced: %+v", dec)
	}
	// 冷却期内不抖（轮 11/12 < 10+3）。
	if dec := s.EvaluateTurn(dec.NewProfile, TurnSignals{TurnIndex: 11, CriticalSignalFound: true}); dec.Changed {
		t.Fatal("冷却期内发生了切换")
	}
	// 冷却过后继续升档。
	dec = s.EvaluateTurn(dec.NewProfile, TurnSignals{TurnIndex: 13, CriticalSignalFound: true})
	if !dec.Changed || dec.NewMode != ModeSuperHeavy {
		t.Fatalf("冷却后应 Balanced→SuperHeavy: %+v", dec)
	}
	// 预算压力降档（70% 阈值）。
	dec = s.EvaluateTurn(dec.NewProfile, TurnSignals{TurnIndex: 16, TokensUsed: 600_000})
	if !dec.Changed || dec.NewMode != ModeBalanced {
		t.Fatalf("预算压力应降档: %+v", dec)
	}
	if got := ResolveExecutionProfile(dec.NewProfile.Difficulty, dec.NewProfile.Mode).HardTokenBudget; got != 0 {
		t.Fatalf("降档后不应保留熔断预算: %d", got)
	}
	// 无信号不变。
	if dec := s.EvaluateTurn(base, TurnSignals{TurnIndex: 99}); dec.Changed {
		t.Fatal("无信号发生了切换")
	}
}

func TestProfileRegistryConcurrent(t *testing.T) {
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(id int64) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				SetExecutionProfile(id, ResolveExecutionProfile(DifficultySimple, ModeFast))
				ExecutionProfileFor(id)
				DeleteExecutionProfile(id)
			}
		}(int64(i))
	}
	wg.Wait()
}

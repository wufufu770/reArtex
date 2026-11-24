package intercept

// Audit #42 移植测试（0.15.74）：递归全量解码 + 判官失败默认锁死。
// 两部分对应 XIB-Artex Audit #42 的两条：转义绕过矩阵、判官 fail-closed 默认。

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
)

// audit42Rule 构造一条 tool_input 正则规则。
func audit42Rule(t *testing.T, pattern string) compiledRule {
	t.Helper()
	c := compiledRule{InterceptRule: db.InterceptRule{
		Name: "audit42", MatchTarget: "tool_input", MatchType: "regex", Pattern: pattern,
	}}
	c.re = regexp.MustCompile(pattern)
	return c
}

// --- 转义绕过矩阵：未知键、键名、双重编码（字符串内嵌 JSON 文本）都要进匹配面 ---

func TestAudit42UnknownNestedKeysEnterMatchSurface(t *testing.T) {
	// 9 字段口径的漏网形态：字符串藏在未知键的嵌套对象里。
	raw := []byte(`{"argv":{"0":"\u0063url http://x"}}`)
	r := audit42Rule(t, `\bcurl\b`)
	norm := NormalizeToolInput(raw)
	if !ruleMatches(r, "Bash", raw, norm) {
		t.Fatalf("未知键下的转义字符串应进匹配面，规范化=%q", norm)
	}
}

func TestAudit42KeysEnterMatchSurface(t *testing.T) {
	raw := []byte(`{"options":{"curl_wrap":true}}`)
	r := audit42Rule(t, `curl_wrap`)
	norm := NormalizeToolInput(raw)
	if !ruleMatches(r, "Bash", raw, norm) {
		t.Fatalf("键名应进匹配面，规范化=%q", norm)
	}
}

func TestAudit42ScalarsKeepMatchSurface(t *testing.T) {
	// 数字/布尔沿用历史 re-marshal 口径进文本（存量规则匹配面只加宽不收窄）。
	raw := []byte(`{"retries":404,"debug":true}`)
	norm := NormalizeToolInput(raw)
	for _, want := range []string{"404", "true"} {
		if !strings.Contains(norm, want) {
			t.Fatalf("标量 %s 应保留在规范化文本中，得到 %q", want, norm)
		}
	}
}

func TestAudit42DoubleEncodedJSONInString(t *testing.T) {
	// 字符串内再嵌一层 JSON 文本：递归解出内层字符串参与匹配。
	raw := []byte(`{"command":"{\"x\":\"\\u0063url http://x\"}"}`)
	r := audit42Rule(t, `\bcurl\b`)
	norm := NormalizeToolInput(raw)
	if !ruleMatches(r, "Bash", raw, norm) {
		t.Fatalf("双重编码（内嵌 JSON 文本）应解出并命中，规范化=%q", norm)
	}
}

func TestAudit42BypassMatrixHitsBuiltins(t *testing.T) {
	// 卡面验收矩阵：三种入参形态都能命中内置词表规则。
	cases := []struct {
		name string
		raw  string
		re   string
	}{
		{"unicode-escape", `{"command":"\u0063url http://x"}`, `\bcurl\b`},
		{"args-array", `{"args":["\u0072m","-rf"]}`, `\brm\b`},
		{"mixed-case-escape", `{"cmd":"\u0043URL http://x"}`, `(?i)\bcurl\b`},
	}
	for _, tc := range cases {
		r := audit42Rule(t, tc.re)
		raw := []byte(tc.raw)
		if !ruleMatches(r, "Bash", raw, NormalizeToolInput(raw)) {
			t.Fatalf("%s：应命中规则", tc.name)
		}
	}
}

func TestAudit42InvalidJSONFallsBackToRaw(t *testing.T) {
	// 非法 JSON：规范化为空（只匹配原文），且不 panic。
	for _, in := range []string{`{"command":`, `{"a":"unterminated`, ``} {
		if got := NormalizeToolInput([]byte(in)); got != "" {
			t.Fatalf("非法 JSON %q 应返回空串，得到 %q", in, got)
		}
	}
}

func TestAudit42DepthCapStopsRecursion(t *testing.T) {
	// 结构深度超限的子树停止展开（不 panic、不兜圈）；浅层字符串仍收集。
	deep := `"leaf"`
	for i := 0; i < 20; i++ {
		deep = `{"d":` + deep + `}`
	}
	raw := []byte(`{"x":"\u0063url http://x","nested":` + deep + `}`)
	norm := NormalizeToolInput(raw)
	if !strings.Contains(norm, "curl http://x") {
		t.Fatalf("浅层字符串应照常收集：%q", norm)
	}
}

// --- 判官失败默认锁死：存量 allow 未确认 → 运行时 deny ---

func TestAudit42JudgeFailActionEffective(t *testing.T) {
	cases := []struct {
		stored, confirmed string
		wantEffective     string
		wantUnconfirmed   bool
	}{
		{"allow", "", "deny", true},    // 存量 allow 未确认 → deny
		{"allow", "1", "allow", false}, // 显式确认过 → allow 生效
		{"allow", "0", "deny", true},   // 非法标记视同未确认
		{"deny", "", "deny", false},    // deny 不受影响
		{"ask", "", "ask", false},      // ask 不受影响
		{"", "", "deny", false},        // 缺失 → 默认 deny
		{"bogus", "1", "deny", false},  // 非法值 → 默认 deny（确认标记无关）
	}
	for _, tc := range cases {
		got, unconfirmed := judgeFailActionEffective(tc.stored, tc.confirmed)
		if got != tc.wantEffective || unconfirmed != tc.wantUnconfirmed {
			t.Fatalf("judgeFailActionEffective(%q,%q)=%q,%v；期望 %q,%v",
				tc.stored, tc.confirmed, got, unconfirmed, tc.wantEffective, tc.wantUnconfirmed)
		}
	}
}

func TestAudit42SetJudgeConfigRequiresConfirmForAllow(t *testing.T) {
	d := openTestDB(t)
	ic := New(d)

	err := ic.SetJudgeConfig(JudgeConfig{FailAction: "allow"})
	if !errors.Is(err, ErrFailOpenConfirmRequired) {
		t.Fatalf("未确认写 allow 应被拒绝，得到 %v", err)
	}
	// 拒绝路径不得留下任何生效值变化。
	if got, _, _ := d.GetSetting(settingJudgeFailAction); got != "" {
		t.Fatalf("拒绝写入不应落库，得到 %q", got)
	}

	// 带 confirm 写 allow：落库 + 确认标记，运行时 allow 生效。
	if err := ic.SetJudgeConfig(JudgeConfig{FailAction: "allow", ConfirmFailOpen: true}); err != nil {
		t.Fatalf("显式确认后写 allow 应成功: %v", err)
	}
	if cfg := ic.GetJudgeConfig(); cfg.FailAction != "allow" || cfg.FailOpenUnconfirmed {
		t.Fatalf("确认后 allow 应生效，得到 %+v", cfg)
	}

	// 离开 allow：确认标记清掉，存量 allow 重新回到锁死口径。
	if err := ic.SetJudgeConfig(JudgeConfig{FailAction: "deny"}); err != nil {
		t.Fatalf("写回 deny 应成功: %v", err)
	}
	if v, ok, _ := d.GetSetting(settingJudgeFailOpenConfirmed); ok && v == "1" {
		t.Fatal("离开 allow 后确认标记应清除")
	}
}

func TestAudit42LegacyAllowTreatedAsDenyAtRuntime(t *testing.T) {
	d := openTestDB(t)
	// 模拟升级前的存量库值：allow 在库、无确认标记。
	if err := d.SetSetting(settingJudgeFailAction, "allow"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.SetSetting(settingJudgeFailAction, "") })

	ic := New(d) // New 触发启动期迁移检查（记警告，不改库）
	cfg := ic.GetJudgeConfig()
	if cfg.FailAction != "deny" || !cfg.FailOpenUnconfirmed {
		t.Fatalf("存量 allow 未确认应按 deny 执行并带未确认标记，得到 %+v", cfg)
	}
	if stored, _, _ := d.GetSetting(settingJudgeFailAction); stored != "allow" {
		t.Fatalf("启动迁移不自动改库内值，得到 %q", stored)
	}

	// 判官链路实测：reviewer 出错 → 按【生效值 deny】兜底，而不是存量 allow。
	_ = d.SetBool(settingJudgeEnabled, true)
	t.Cleanup(func() { _ = d.SetBool(settingJudgeEnabled, false) })
	ic.SetReviewer(func(ctx context.Context, profileID int64, prompt string, input ReviewInput) (Decision, error) {
		return Decision{}, errors.New("模型不可用")
	})
	dec, ok := ic.Judge(context.Background(), "Bash", []byte(`{"command":"ls"}`))
	if !ok || dec.Action != "deny" {
		t.Fatalf("reviewer 出错时存量 allow 应回落 deny，得到 ok=%v action=%q", ok, dec.Action)
	}
}

func TestAudit42JudgeUnparseableOutputDenies(t *testing.T) {
	d := openTestDB(t)
	_ = d.SetBool(settingJudgeEnabled, true)
	t.Cleanup(func() { _ = d.SetBool(settingJudgeEnabled, false) })
	ic := New(d)
	ic.SetReviewer(func(ctx context.Context, profileID int64, prompt string, input ReviewInput) (Decision, error) {
		return Decision{Action: "maybe"}, nil // 非法裁决 → 按失败策略处理
	})
	dec, ok := ic.Judge(context.Background(), "Bash", []byte(`{"command":"ls"}`))
	if !ok || dec.Action != "deny" {
		t.Fatalf("不可解析输出应按默认 deny 兜底，得到 ok=%v action=%q", ok, dec.Action)
	}
}

func TestAudit42JudgeTimeoutDenies(t *testing.T) {
	d := openTestDB(t)
	_ = d.SetBool(settingJudgeEnabled, true)
	_ = d.SetSetting(settingJudgeTimeoutSecs, "1") // 1s 超时窗
	t.Cleanup(func() {
		_ = d.SetBool(settingJudgeEnabled, false)
		_ = d.SetSetting(settingJudgeTimeoutSecs, "")
	})
	ic := New(d)
	ic.SetReviewer(func(ctx context.Context, profileID int64, prompt string, input ReviewInput) (Decision, error) {
		select {
		case <-ctx.Done():
			return Decision{}, ctx.Err()
		case <-time.After(3 * time.Second):
			return Decision{Action: "allow"}, nil
		}
	})
	dec, ok := ic.Judge(context.Background(), "Bash", []byte(`{"command":"ls"}`))
	if !ok || dec.Action != "deny" {
		t.Fatalf("reviewer 超时应按 deny 兜底，得到 ok=%v action=%q", ok, dec.Action)
	}
}

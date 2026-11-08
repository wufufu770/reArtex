package server

import "testing"

// TestLookupThinkingCapability 注册表的形状判定（方案 v9.2 批次 A 验收）：
// mimo 只出开关并带实测注记；gpt-5.1 出 5 档且默认 none；未收录模型回退保守全集。
func TestLookupThinkingCapability(t *testing.T) {
	// mimo：switch 形态 + effort 不支持的实测注记（探针法产品化的静态兜底）。
	c := lookupThinkingCapability("anthropic", "MiMo- Devon-7B")
	if c.Shape != shapeSwitch || !c.Known {
		t.Fatalf("mimo 应为 switch/known: %+v", c)
	}
	if len(c.Levels) != 2 || c.Levels[0] != "enabled" || c.Levels[1] != "disabled" {
		t.Fatalf("mimo 档位应为 开/关: %v", c.Levels)
	}
	if c.Note == "" {
		t.Fatal("mimo 条目必须带实测注记")
	}
	// gpt-5.1：5 档，默认 none。
	c = lookupThinkingCapability("openai", "gpt-5.1-mini")
	if c.Shape != shapeEffort || !c.Known || c.Default != "none" {
		t.Fatalf("gpt-5.1 应为 effort/none: %+v", c)
	}
	if len(c.Levels) != 5 || c.Levels[0] != "none" || c.Levels[4] != "xhigh" {
		t.Fatalf("gpt-5.1 应为 5 档: %v", c.Levels)
	}
	// 前缀不要误吃：gpt-5.1 的条目必须排在 gpt-5 之前命中（先具体后一般）。
	c = lookupThinkingCapability("openai", "gpt-5")
	if c.Default != "medium" || len(c.Levels) != 4 {
		t.Fatalf("gpt-5(5.1 前) 应为 4 档/默认 medium: %+v", c)
	}
	// claude：effort 形状 3 档。
	c = lookupThinkingCapability("anthropic", "claude-sonnet-5")
	if c.Shape != shapeEffort || len(c.Levels) != 3 {
		t.Fatalf("claude 应为 effort/3 档: %+v", c)
	}
	// 未收录模型：known=false + 保守全集。
	c = lookupThinkingCapability("openai", "some-unknown-model")
	if c.Known {
		t.Fatalf("未知模型不应声称 known: %+v", c)
	}
	if len(c.Levels) == 0 {
		t.Fatal("未知模型也要给保守全集")
	}
	// 空模型名：仍是回退，注记提示未填。
	c = lookupThinkingCapability("openai", "")
	if c.Known || c.Note == "" {
		t.Fatalf("空模型名应回退并注记: %+v", c)
	}
}

// TestValidThinkingEffort 写入白名单（批次 B）：全量合法值 + 拒绝任意串。
func TestValidThinkingEffort(t *testing.T) {
	for _, v := range []string{"", "enabled", "disabled", "none", "minimal", "low", "medium", "high", "xhigh", "max"} {
		if !validThinkingEffort(v) {
			t.Fatalf("%q 应合法", v)
		}
	}
	for _, v := range []string{"off", "ultra", "  low", "HIGH"} {
		if validThinkingEffort(v) {
			t.Fatalf("%q 应拒绝", v)
		}
	}
}

package agent

import (
	"strings"
	"testing"
)

// E1-7 防漂移：评级基准的唯一语义源（rating_baseline.go）必须同时出现在
// worker / planner 系统提示词里；report_finding 描述与 severity 报错文案保留
// 各自口径但核心句不得缺失。
func TestRatingBaselineInjectedIntoPrompts(t *testing.T) {
	core := "纯信息泄露与暴露类一律 info"
	for name, sys := range map[string]string{
		"worker":  workerSystem("", "", "/tmp/data", "/tmp/run"),
		"planner": plannerSystem("goal", "/tmp/data", "/tmp/work"),
	} {
		if !strings.Contains(sys, core) {
			t.Fatalf("%s 系统提示词缺评级基准核心句", name)
		}
		if !strings.Contains(sys, "拿不准降一档") {
			t.Fatalf("%s 系统提示词缺「拿不准降一档」", name)
		}
	}
}

// 工具描述与报错文案的核心句仍在（E1-7 v0.10.1 已落的部分不得回退）。
func TestRatingBaselineStillInToolCopy(t *testing.T) {
	tsx := NewToolSet(nil, "work#1")
	tool := tsx.addFinding()
	if !strings.Contains(tool.Description(), coreRatingPhrase) {
		t.Fatal("report_finding 描述缺评级基准核心句")
	}
}

const coreRatingPhrase = "纯信息泄露与暴露类一律 info"

package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// builtinPromptDigestAnchors 钉住六个既有内置提示词的 SHA-256。
//
// 这六个提示词是既有 agent（目标拆解/规划/主控/执行/平台操作/独立渗透）的行为
// 基线：改一个字节就是改既有 agent 的行为，属于行为变更，必须是有意识、有记录
// 的动作，而不是「顺手 tidy 一下」的结果。钉成摘要后，任何误改都会在测试里
// 立刻炸出来，而不是悄悄进了下一个版本。
//
// 注意：四个顾问 agent 的提示词【不】钉摘要——它们的事实来源是文件，文件一改
// 种子就该跟着变（见 TestAdvisoryPromptSeedsMatchSourceFiles）。
var builtinPromptDigestAnchors = map[string]string{
	"goals":     "72a7a0159443b41dde29dfba8a3de6a2c8a15b273935495a7b88561fa708aa68",
	// P1 角色工具纪律（方案 v3.1 §5.1）：planner 移除 Bash 探测授权段（工具面编译期
	// 白名单同步收口）——有意识的提示词行为变更，锚点随之更新（2026-11-05）。
	"planner":   "4d1f7ca0282c577f09106f1e8c95b83b3aae03bacfb7dca722d4a7aeb0b84a42", // F1 许愿纪律（方案 v10 §12）：planner 提示词加预测字段与可证伪许愿段——有意识的变更，锚点随之更新（2026-11-11）。
	"mainagent": "12bb69ee0886c4fb9beb936d882021a91104860f1da69a5e3eda3d9e345cd399",
	"worker":    "80fbb1c610dd57e266c20b9145793339eee540b809f6db8f5336261b45879f08",
	"auto":      "b2a71b82243607337b19f0642317fed88c144d536412d80eaaf90807f2905a1d",
	"pentest":   "a54bd5713b1c121d0f2ff3a72e0304820b1368c4b4b8159d5d651895663ac652",
}

// TestAdvisoryPromptSeedsMatchSourceFiles 保证种子里的正文就是文件里的正文。
//
// 文件是事实来源：有人改了 agent/prompts/*.md，种子必须跟着变。种子若是另抄
// 一份的副本，两者迟早对不上，而对不上的那一刻平台就在用「旧规则」跑新 agent。
func TestAdvisoryPromptSeedsMatchSourceFiles(t *testing.T) {
	seeds := BuiltinPromptSeeds()
	for key, path := range advisoryPromptFiles {
		want, err := os.ReadFile(filepath.FromSlash(path))
		if err != nil {
			t.Fatalf("读取提示词文件 %s 失败: %v", path, err)
		}
		got, ok := seeds[key]
		if !ok {
			t.Fatalf("提示词种子缺少 %s", key)
		}
		if got != string(want) {
			t.Fatalf("%s 的种子正文与文件 %s 不一致（种子 %d 字节，文件 %d 字节）", key, path, len(got), len(want))
		}
	}
}

// TestBuiltinPromptDigestAnchors 保证六个既有内置提示词一个字节都没变。
func TestBuiltinPromptDigestAnchors(t *testing.T) {
	seeds := BuiltinPromptSeeds()
	for key, want := range builtinPromptDigestAnchors {
		got, ok := seeds[key]
		if !ok {
			t.Fatalf("提示词种子缺少既有 agent %s", key)
		}
		sum := sha256.Sum256([]byte(got))
		if hex.EncodeToString(sum[:]) != want {
			t.Errorf("既有 agent %s 的提示词被改了（摘要 %s != 锚点 %s）——既有 agent 的行为变更不在本次范围内", key, hex.EncodeToString(sum[:]), want)
		}
	}
}

// TestBuiltinPromptSeedsKeys 保证种子目录的构成是「七个既有 + 五个顾问 + 两个偏锋 + 小程序渗透」，不多不少。
// （EDGE E0：edge=方法论顾问、edge_exec=执行体；miniprogram=小程序渗透专用 agent，0.13.0 入种子目录。）
func TestBuiltinPromptSeedsKeys(t *testing.T) {
	seeds := BuiltinPromptSeeds()
	want := map[string]bool{
		"goals": true, "planner": true, "mainagent": true,
		"worker": true, "auto": true, "pentest": true, "recon": true,
		"curator": true, "verifier": true, "redteam": true, "creative": true,
		"intent_normalizer": true,
		"edge":              true, "edge_exec": true,
		"miniprogram":       true,
	}
	if len(seeds) != len(want) {
		t.Fatalf("提示词种子数量不对：got %d want %d（%v）", len(seeds), len(want), seeds)
	}
	for key := range want {
		if _, ok := seeds[key]; !ok {
			t.Errorf("提示词种子缺少 %s", key)
		}
	}
}

// TestIntentNormalizerPromptSeeded 保证 intent-normalizer 作为第五个顾问型 agent
// 被播种，且正文就是文件里的正文。
//
// 它是「规整器」：把 worker 的原始动作转成结构化意图，交给确定性闸门裁决。
// 正文必须来自文件——它是这条职责分离链上唯一新增的 LLM 环节，提示词就是它的
// 行为边界。
func TestIntentNormalizerPromptSeeded(t *testing.T) {
	seeds := BuiltinPromptSeeds()
	body, ok := seeds["intent_normalizer"]
	if !ok {
		t.Fatal("提示词种子缺少 intent_normalizer")
	}
	want, err := os.ReadFile(filepath.FromSlash("prompts/intent-normalizer.md"))
	if err != nil {
		t.Fatalf("读取提示词文件失败: %v", err)
	}
	if body != string(want) {
		t.Fatalf("intent-normalizer 的种子正文与文件不一致（种子 %d 字节，文件 %d 字节）", len(body), len(want))
	}
}

// TestPolicyGatePromptUnreachable 保证 policy-gate 这份提示词不可达：它不是
// agent 的提示词，是确定性闸门的规范。
//
// 关键在于「不可达」不等于「文件不存在」：//go:embed prompts/*.md 确实把
// policy-gate.md 的字节收进了二进制（embed 的 glob 命中的不止可登记的那几个），
// 权威是登记映射表 advisoryPromptFiles，不是 embed 的 glob。所以这里同时断言
// 「文件在 embed 里」与「任何一条登记/枚举路径都看不到它」。
//
// 一旦有人把 policy-gate 登记成 agent，就等于让模型来做本该由确定性代码做的
// 裁决——「模型只提议，确定性层裁决」这条铁律就破了。
func TestPolicyGatePromptUnreachable(t *testing.T) {
	// 前提：embed 确实抓到了这份文件（否则下面的断言会变成空转）。
	if _, err := fs.ReadFile(advisoryPromptFS, "prompts/policy-gate.md"); err != nil {
		t.Fatalf("embed 没有抓到 prompts/policy-gate.md，测试前提不成立: %v", err)
	}

	if p, ok := advisoryPromptFiles["policy-gate"]; ok {
		t.Errorf("policy-gate 被登记进提示词映射表（%s）", p)
	}
	if _, ok := advisoryPromptSeeds["policy-gate"]; ok {
		t.Error("policy-gate 出现在已装载的提示词种子里")
	}
	for key := range AdvisoryPromptSeeds() {
		if key == "policy-gate" {
			t.Error("policy-gate 出现在 AdvisoryPromptSeeds() 的枚举里")
		}
	}
	for key := range BuiltinPromptSeeds() {
		if key == "policy-gate" {
			t.Error("policy-gate 出现在 BuiltinPromptSeeds() 的枚举里")
		}
	}
}

// TestEveryEmbeddedPromptIsRegisteredOrPolicyGate 保证 embed 抓到的每一个提示词
// 文件要么被登记成 agent 的提示词，要么就是 policy-gate 这一份规范。
//
// 少一条登记是漏了一个 agent；多一条登记是有人把不该登记的东西塞进了目录。
// 这条断言把「embed 的 glob」和「登记映射表」之间的差固定成一个可审计的清单。
func TestEveryEmbeddedPromptIsRegisteredOrPolicyGate(t *testing.T) {
	entries, err := fs.ReadDir(advisoryPromptFS, "prompts")
	if err != nil {
		t.Fatalf("读取嵌入的提示词目录失败: %v", err)
	}
	registered := map[string]bool{}
	for _, path := range advisoryPromptFiles {
		registered[path] = true
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		path := "prompts/" + e.Name()
		if registered[path] {
			continue
		}
		if path == "prompts/policy-gate.md" {
			continue
		}
		t.Errorf("提示词文件 %s 既没被登记成 agent 提示词，也不是已知的规范文件", path)
	}
}

// TestBuiltinPromptBodyLookup 保证按 agent key 查内置提示词正文的行为是确定的：
// 规整器的两种写法都能查到同一份正文，policy-gate 两种写法都查不到。
//
// 为什么规整器有两种写法：agents 表的 key 有 CHECK (key ~ '^[a-z][a-z0-9_]*$')
// 约束（db/schema.sql），带连字符的 key 插不进库，规范 key 只能是
// intent_normalizer；而「intent-normalizer」是文件名与阶段名。两种写法指向同一个
// agent，是同一个东西的两种叫法。
func TestBuiltinPromptBodyLookup(t *testing.T) {
	want, err := os.ReadFile(filepath.FromSlash("prompts/intent-normalizer.md"))
	if err != nil {
		t.Fatalf("读取提示词文件失败: %v", err)
	}
	for _, key := range []string{"intent_normalizer", "intent-normalizer"} {
		body, ok := BuiltinPromptBody(key)
		if !ok {
			t.Errorf("按 %s 查内置提示词查不到", key)
			continue
		}
		if body != string(want) {
			t.Errorf("按 %s 查到的正文与文件不一致", key)
		}
	}
	for _, key := range []string{"policy-gate", "policy_gate", "executor", "auditor"} {
		if body, ok := BuiltinPromptBody(key); ok {
			t.Errorf("按 %s 查到了内置提示词（%d 字节）——它不该是 agent 的提示词", key, len(body))
		}
	}
}

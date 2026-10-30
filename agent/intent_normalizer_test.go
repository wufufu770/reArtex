package agent

import (
	"testing"

	"github.com/Autumn-27/artex/db"
)

// intentNormalizerKey 是意图规整 agent 的 key。
const intentNormalizerKey = "intent_normalizer"

// normalizerOnlyTool 是 intent-normalizer 唯一的输出工具。
const normalizerOnlyTool = "emit_structured_intent"

// TestIntentNormalizerHoldsExactlyOneTool 保证 intent-normalizer 只握一个工具：
// emit_structured_intent。
//
// 它的职责是把 worker 的原始动作转成结构化意图，交给确定性闸门裁决。多握一个
// 工具就多一条越权路径：能执行就能自己动手，能查 scope 就能自己判 scope，能写
// 共享图就能绕过闸门改状态——那不是「规整」，是「裁决」。
//
// 两层都要断言：
//   - 声明层（db 层策略白名单）：授权恰好一个工具；
//   - 目录层（BuiltinToolSeeds 播种到 tools 表的绑定）：绑给它的工具恰好一个。
//
// 只断言一层会漏掉另一层的漂移：白名单放宽了没人发现，或者目录里多绑了一个。
func TestIntentNormalizerHoldsExactlyOneTool(t *testing.T) {
	declared := db.AdvisoryAgentToolKeys(intentNormalizerKey)
	if len(declared) != 1 || declared[0] != normalizerOnlyTool {
		t.Fatalf("intent-normalizer 声明的工具 = %v，want [%s]", declared, normalizerOnlyTool)
	}

	var bound []string
	for _, s := range BuiltinToolSeeds() {
		if containsAgentKey(s.Agents, intentNormalizerKey) {
			bound = append(bound, s.Key)
		}
	}
	if len(bound) != 1 || bound[0] != normalizerOnlyTool {
		t.Fatalf("目录层绑给 intent-normalizer 的工具 = %v，want [%s]", bound, normalizerOnlyTool)
	}
}

// TestIntentNormalizerHasNoExecScopeOrExtraWriteTool 保证 intent-normalizer 的
// 工具集里没有可执行工具、没有 scope 查询工具、除了那一个输出通道以外没有别的
// 写工具。
//
// 逐条按能力面分类断言，而不是只比对名字：能力面是 db 层策略认得的语义
// （执行 / 建议通道 / 共享状态写 / 只读），名字会变，语义不会。
func TestIntentNormalizerHasNoExecScopeOrExtraWriteTool(t *testing.T) {
	tools := db.AdvisoryAgentToolKeys(intentNormalizerKey)
	if len(tools) == 0 {
		t.Fatal("intent-normalizer 没有声明任何工具")
	}
	for _, key := range tools {
		switch db.AdvisoryToolCapability(key, "builtin") {
		case db.AdvisoryCapExec:
			t.Errorf("intent-normalizer 绑上了可执行工具 %s", key)
		case db.AdvisoryCapGraphWrite:
			t.Errorf("intent-normalizer 绑上了改写共享状态的工具 %s", key)
		case db.AdvisoryCapAdvisoryWrite:
			if key != normalizerOnlyTool {
				t.Errorf("intent-normalizer 绑上了除 %s 之外的输出通道工具 %s", normalizerOnlyTool, key)
			}
		}
		if db.AdvisoryToolIsScopeQuery(key) {
			t.Errorf("intent-normalizer 绑上了 scope 查询工具 %s（scope 判定属于确定性闸门）", key)
		}
	}
	// 唯一的输出通道必须被认作「自己的建议通道」而不是「共享状态写」：
	// 前者不触发 DenyWrite，后者会把它挡掉。
	if got := db.AdvisoryToolCapability(normalizerOnlyTool, "builtin"); got != db.AdvisoryCapAdvisoryWrite {
		t.Errorf("%s 的能力面 = %v，want AdvisoryCapAdvisoryWrite", normalizerOnlyTool, got)
	}
}

// TestIntentNormalizerToolListNotSupersetOfOtherAgents 保证 intent-normalizer 的
// 工具集不是任何其它已注册 agent 的工具集的超集。
//
// 超集意味着它比谁都宽：一个「什么都能干」的规整器可以在闸门之外自己动手。
// 判定方向很容易写反——要查的是「normalizer ⊅ other」，不是「normalizer ⊆ other」。
func TestIntentNormalizerToolListNotSupersetOfOtherAgents(t *testing.T) {
	normalizer := map[string]bool{}
	for _, k := range db.AdvisoryAgentToolKeys(intentNormalizerKey) {
		normalizer[k] = true
	}

	// 其它 agent 的工具集 = 目录层绑定（tools 表播种的来源）+ 策略层声明
	// （白名单里尚未注册的工具不在目录层，但仍是该 agent 被授权的工具）。
	other := map[string][]string{}
	for _, s := range BuiltinToolSeeds() {
		for _, a := range s.Agents {
			if a == intentNormalizerKey {
				continue
			}
			other[a] = append(other[a], s.Key)
		}
	}
	for _, p := range db.AdvisoryAgentToolPolicies() {
		if p.Agent == intentNormalizerKey {
			continue
		}
		other[p.Agent] = append(other[p.Agent], db.AdvisoryAgentToolKeys(p.Agent)...)
	}

	for agent, tools := range other {
		if len(tools) == 0 {
			continue // 空集是任何集合的子集，拿它判「超集」没有意义
		}
		for _, k := range tools {
			if normalizer[k] {
				t.Errorf("intent-normalizer 的工具 %s 同时属于 %s：它的工具集成了 %s 的超集", k, agent, agent)
			}
		}
	}
}

// TestIntentNormalizerHasNoPromptBindingToOtherAgents 保证 emit_structured_intent
// 这个工具只属于 intent-normalizer，不被其它顾问 agent 顺手绑走。
//
// 共享同一个「输出通道」工具会让两个 agent 的职责边界糊掉：规整器产出的
// 结构化意图与仲裁者产出的裁决写进同一张表，谁都分不清这是提议还是裁决。
func TestIntentNormalizerHasNoPromptBindingToOtherAgents(t *testing.T) {
	for _, s := range BuiltinToolSeeds() {
		if s.Key != normalizerOnlyTool {
			continue
		}
		for _, a := range s.Agents {
			if a != intentNormalizerKey {
				t.Errorf("工具 %s 被绑给了 %s，只能属于 %s", normalizerOnlyTool, a, intentNormalizerKey)
			}
		}
	}
}

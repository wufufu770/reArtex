package agent

import "github.com/Autumn-27/norma/agentcore"

// ThinkingOverride, if set, returns the per-agent thinking settings for an
// agent key: the first value forces the thinking switch ("enabled"/"disabled";
// "" = 不覆盖), the second forces the reasoning effort ("low"/…/"none";
// "" = 不覆盖). Both empty ⇒ 跟随 profile 默认（现网零变化）。
//
// 为什么走请求级覆盖而不是 provider 配置：provider 按 profile 缓存、同 profile
// 的所有 agent 共享一个实例（server.go providerForProfileModelRaw），agent 级
// 设置只能随每次请求走（方案 v9.2 §2.3）。宿主（server）装配时按 agents 表的
// thinking_effort 列注入；switch 形态的模型（mimo/glm/deepseek）值落在第一返回
// 值，effort 形态的值落在第二返回值。
var ThinkingOverride func(agentKey string) (thinking, effort string)

// applyThinkingOverride 把 agentKey 的思考档位覆盖塞进 session Options。
// override 未装配 / 返回全空 ⇒ Options 保持零值 ⇒ 请求与今天逐字节一致。
func applyThinkingOverride(opts *agentcore.Options, agentKey string) {
	if ThinkingOverride == nil {
		return
	}
	thinking, effort := ThinkingOverride(agentKey)
	opts.Thinking = thinking
	opts.ReasoningEffort = effort
}

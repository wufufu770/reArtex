package agent

import (
	"sync/atomic"

	"github.com/Autumn-27/norma/agentcore"
)

// thinkingOverrideFn 是 per-agent 思考档位覆盖解析器：第一个返回值强制思考开关
// （"enabled"/"disabled"；"" = 不覆盖），第二个强制 reasoning 强度（"low"/…；
// "" = 不覆盖）。全空 ⇒ 跟随 profile 默认（现网零变化）。
//
// 为什么走请求级覆盖而不是 provider 配置：provider 按 profile 缓存、同 profile
// 的所有 agent 共享一个实例（server.go providerForProfileModelRaw），agent 级
// 设置只能随每次请求走（方案 v9.2 §2.3）。宿主（server）装配时按 agents 表的
// thinking_effort 列注入；switch 形态的模型（mimo/glm/deepseek）值落在第一返回
// 值，effort 形态的值落在第二返回值。
type thinkingOverrideFn = func(agentKey string) (thinking, effort string)

// thinkingOverridePtr 承载宿主注入的解析器。0.15.13：裸全局可变函数变量换
// atomic.Pointer——装配可能在重连/重装路径重入（此前单一 boot 写入侥幸无竞态，
// 语义上这是跨 goroutine 读共享可变状态，不该靠侥幸）。
var thinkingOverridePtr atomic.Pointer[thinkingOverrideFn]

// SetThinkingOverride 注入解析器（宿主 boot/重装时调用；nil = 卸载）。
func SetThinkingOverride(fn thinkingOverrideFn) {
	if fn == nil {
		thinkingOverridePtr.Store(nil)
		return
	}
	thinkingOverridePtr.Store(&fn)
}

// applyThinkingOverride 把 agentKey 的思考档位覆盖塞进 session Options。
// 0.15.13 修三处（外部审查实锤）：① opts == nil 直接返回；② 返回值为 ""
// 的字段【不覆盖】——旧实现无条件赋值，返回全空时会把调用方预设的
// Thinking/ReasoningEffort 清掉，与「"" = 不覆盖」的契约矛盾；③ 经 atomic
// 读取，装配重入不再有数据竞态。
func applyThinkingOverride(opts *agentcore.Options, agentKey string) {
	if opts == nil {
		return
	}
	fn := thinkingOverridePtr.Load()
	if fn == nil {
		return
	}
	thinking, effort := (*fn)(agentKey)
	if thinking != "" {
		opts.Thinking = thinking
	}
	if effort != "" {
		opts.ReasoningEffort = effort
	}
}

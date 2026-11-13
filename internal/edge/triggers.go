// 触发词闸门（EDGE E6）的两级词表与确定性扫描。
//
// 为什么要确定性闸门而不是只靠工具描述：工具描述写得再清楚也只是建议，模型可能
// 不调。「用户明确提出」要成为保证而不是概率，检测必须确定性。
//
// 两级取舍（为什么宁可漏触不可误触）：
//   - 硬触发级的误判代价 = 一次不受控的无上限长跑（最贵的错误形态：烧预算 +
//     占用目标）；软触发的漏判代价 = 少一次建议。
//   - 所以硬级只认【精确名】（子串精确匹配，不做模糊匹配、不做语义扩展——
//     不用分词不用编辑距离）；软级只产生建议，不切模式。
//
// 词表是代码常量可单测；措辞按底座既有命名习惯（功能中文名 + agent key）。
// 词表内容刻意收窄：宁漏勿误。
package edge

import "strings"

// HardTriggers 是硬触发级：命中即视为「用户明确要求偏锋探索」——闸门在本轮
// 注入声明并把 edge 工具前置；是否真正建任务仍由 agent 调用对应工具（edge_run/
// edge_activate）完成，闸门不替代工具调用。
var HardTriggers = []string{
	"偏锋探索",
	"偏锋探索·执行",
	"剑走偏锋",
	"edge exploration",
	"edge run",
	"edge_exec",
}

// SoftTriggers 是软信号级：命中只产生建议（「可考虑调用 edge_consult」），
// 不切模式、不前置工具。
var SoftTriggers = []string{
	"换个角度",
	"换个思路",
	"非常规路径",
	"偏门一点",
	"别按常规套路",
	"出其不意",
}

// TriggerHit 是一次扫描命中。
type TriggerHit struct {
	Word string // 命中的词
	Hard bool   // 是否硬触发级
}

// Scan 扫描一条用户消息，返回最高优先级命中（硬 > 软；同级先命中先得）。
// 无命中返回 nil。判定是精确子串包含——不做模糊匹配、不做语义扩展。
func Scan(message string) *TriggerHit {
	if message == "" {
		return nil
	}
	lower := strings.ToLower(message)
	for _, w := range HardTriggers {
		if strings.Contains(lower, strings.ToLower(w)) {
			return &TriggerHit{Word: w, Hard: true}
		}
	}
	for _, w := range SoftTriggers {
		if strings.Contains(message, w) {
			return &TriggerHit{Word: w, Hard: false}
		}
	}
	return nil
}

// HardGatePrompt 是硬命中时注入本轮的声明段（走本轮注入，不拼死进 system——
// chat 是常驻会话，system 前缀直接关系 KV 缓存，易变内容进 system 会损缓存，
// 与记忆层 W3 同款纪律）。
func HardGatePrompt(hit *TriggerHit) string {
	return "\n\n<system-reminder>用户明确要求使用偏锋探索能力（命中触发词「" + hit.Word +
		"」）。若用户要的是一次完整探索，调用 edge_run 建独立偏锋任务；若要在当前任务内换探索打法，调用 edge_activate；若只是要个偏门方向，调用 edge_consult。拿不准时先问一句。</system-reminder>"
}

// SoftHintPrompt 是软命中时注入本轮的建议段（只建议，不切模式）。
func SoftHintPrompt(hit *TriggerHit) string {
	return "\n\n<system-reminder>用户可能想要非常规的探索角度（软信号「" + hit.Word +
		"」）。如合适，可调用 edge_consult 获取方法论建议与候选假设；不要自动启动完整探索。</system-reminder>"
}
